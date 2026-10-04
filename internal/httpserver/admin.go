package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/leileipei/Enterprise_IM/internal/access"
)

var (
	// ErrAuthUnavailable lets an authentication adapter distinguish an outage
	// from an invalid credential without exposing adapter details to clients.
	ErrAuthUnavailable = errors.New("authentication unavailable")
	uuidPattern        = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// VerifiedIdentity is issued by an authentication adapter after token
// verification and local user mapping. It must never be built from HTTP fields.
type VerifiedIdentity struct {
	TenantID  string
	UserID    string
	ExpiresAt time.Time
}

type Authenticator interface {
	Authenticate(context.Context, string) (VerifiedIdentity, error)
}

type AdminService interface {
	GetManagedPerson(context.Context, access.TrustedIdentity, string) (access.Person, error)
	SearchManagedPeople(context.Context, access.TrustedIdentity, string, int) (access.SearchPage, error)
	EndMembership(context.Context, access.TrustedIdentity, string) error
}

// HandlerWithAdmin installs management routes only when both dependencies are
// supplied. Production enables it after configuring a real identity source.
func HandlerWithAdmin(database Pinger, authenticator Authenticator, admin AdminService) (http.Handler, error) {
	if authenticator == nil || admin == nil {
		return nil, errors.New("admin authentication and service are required")
	}
	health := newMux(database)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin" && !strings.HasPrefix(r.URL.Path, "/api/v1/admin/") {
			health.ServeHTTP(w, r)
			return
		}
		identity, ok := authenticateAdmin(w, r, authenticator)
		if !ok {
			return
		}
		switch {
		case r.URL.Path == "/api/v1/admin/users":
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			searchAdminPeople(w, r, identity, admin)
		case strings.HasPrefix(r.URL.Path, "/api/v1/admin/users/"):
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			getAdminPerson(w, r, identity, admin)
		case strings.HasPrefix(r.URL.Path, "/api/v1/admin/memberships/"):
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			endAdminMembership(w, r, identity, admin)
		default:
			rejectAdmin(w, r, http.StatusNotFound, "not_found")
		}
	}), nil
}

func searchAdminPeople(w http.ResponseWriter, r *http.Request, identity access.TrustedIdentity, admin AdminService) {
	params, parseErr := url.ParseQuery(r.URL.RawQuery)
	if parseErr != nil || len(params) < 1 || len(params) > 2 || len(params["q"]) != 1 ||
		(len(params) == 2 && len(params["limit"]) != 1) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_search")
		return
	}
	for key := range params {
		if key != "q" && key != "limit" {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_search")
			return
		}
	}
	query := strings.TrimSpace(params.Get("q"))
	if query == "" || !utf8.ValidString(query) || strings.ContainsRune(query, 0) || utf8.RuneCountInString(query) > 100 {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_search")
		return
	}
	limit := 20
	if values, present := params["limit"]; present {
		parsed, err := strconv.Atoi(values[0])
		if err != nil || parsed < 1 || parsed > 50 {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_search")
			return
		}
		limit = parsed
	}
	page, err := admin.SearchManagedPeople(r.Context(), identity, query, limit)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	result := searchPageDTO{People: make([]searchPersonDTO, 0, len(page.People)), HasMore: page.HasMore}
	for _, person := range page.People {
		result.People = append(result.People, searchPersonDTO{ID: person.ID, EmployeeNo: person.EmployeeNo, DisplayName: person.DisplayName})
	}
	writeAdminJSON(w, http.StatusOK, result)
}

func getAdminPerson(w http.ResponseWriter, r *http.Request, identity access.TrustedIdentity, admin AdminService) {
	target := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/users/")
	if strings.Contains(target, "/") {
		rejectAdmin(w, r, http.StatusNotFound, "not_found")
		return
	}
	if !validUUID(target) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_id")
		return
	}
	person, err := admin.GetManagedPerson(r.Context(), identity, target)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeAdminJSON(w, http.StatusOK, personResponse(person))
}

func endAdminMembership(w http.ResponseWriter, r *http.Request, identity access.TrustedIdentity, admin AdminService) {
	segment := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/memberships/")
	if strings.Contains(segment, "/") {
		rejectAdmin(w, r, http.StatusNotFound, "not_found")
		return
	}
	if !strings.HasSuffix(segment, ":end") {
		rejectAdmin(w, r, http.StatusNotFound, "not_found")
		return
	}
	target := strings.TrimSuffix(segment, ":end")
	if !validUUID(target) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_id")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(body) != 0 {
		rejectAdmin(w, r, http.StatusBadRequest, "unexpected_body")
		return
	}
	if err := admin.EndMembership(r.Context(), identity, target); err != nil {
		writeServiceError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func validUUID(value string) bool { return uuidPattern.MatchString(value) }

func authenticateAdmin(w http.ResponseWriter, r *http.Request, authenticator Authenticator) (access.TrustedIdentity, bool) {
	verified, ok := authenticateBearer(w, r, authenticator)
	if !ok {
		return access.TrustedIdentity{}, false
	}
	memberships := r.Header.Values("X-Acting-Membership-ID")
	if len(memberships) != 1 || !validUUID(memberships[0]) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_acting_membership")
		return access.TrustedIdentity{}, false
	}
	return access.TrustedIdentity{TenantID: verified.TenantID, UserID: verified.UserID, ActingMembershipID: memberships[0]}, true
}

func authenticateBearer(w http.ResponseWriter, r *http.Request, authenticator Authenticator) (VerifiedIdentity, bool) {
	w.Header().Set("Cache-Control", "no-store")
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return denyBearer(w, r, http.StatusUnauthorized, "unauthorized")
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n") || len(token) > 8192 {
		return denyBearer(w, r, http.StatusUnauthorized, "unauthorized")
	}
	verified, err := authenticator.Authenticate(r.Context(), token)
	if errors.Is(err, ErrAuthUnavailable) {
		return denyBearer(w, r, http.StatusServiceUnavailable, "unavailable")
	}
	if err != nil || !validUUID(verified.TenantID) || !validUUID(verified.UserID) {
		return denyBearer(w, r, http.StatusUnauthorized, "unauthorized")
	}
	return verified, true
}

func denyBearer(w http.ResponseWriter, r *http.Request, status int, code string) (VerifiedIdentity, bool) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	rejectAdmin(w, r, status, code)
	return VerifiedIdentity{}, false
}

func rejectAdmin(w http.ResponseWriter, r *http.Request, status int, code string) {
	slog.WarnContext(r.Context(), "protected request rejected", "path", r.URL.Path, "error_code", code)
	writeAdminError(w, status, code)
}

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, access.ErrInvalidIdentity):
		writeAdminError(w, http.StatusForbidden, "invalid_identity")
	case errors.Is(err, access.ErrNotFound):
		writeAdminError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, access.ErrConflict):
		writeAdminError(w, http.StatusConflict, "conflict")
	case errors.Is(err, access.ErrInvalidSearch):
		writeAdminError(w, http.StatusBadRequest, "invalid_search")
	default:
		slog.Error("admin service unavailable", "error", err)
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable")
	}
}

func writeAdminError(w http.ResponseWriter, status int, code string) {
	writeAdminJSON(w, status, struct {
		ErrorCode string `json:"error_code"`
		Message   string `json:"message"`
	}{ErrorCode: code, Message: http.StatusText(status)})
}

func writeAdminJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type departmentDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type membershipDTO struct {
	ID               string          `json:"id"`
	OrganizationID   string          `json:"organization_id"`
	OrganizationName string          `json:"organization_name"`
	Title            string          `json:"title"`
	IsPrimary        bool            `json:"is_primary"`
	Departments      []departmentDTO `json:"departments"`
}

type personDTO struct {
	ID          string          `json:"id"`
	DisplayName string          `json:"display_name"`
	Memberships []membershipDTO `json:"memberships"`
}

type searchPersonDTO struct {
	ID          string `json:"id"`
	EmployeeNo  string `json:"employee_no"`
	DisplayName string `json:"display_name"`
}

type searchPageDTO struct {
	People  []searchPersonDTO `json:"people"`
	HasMore bool              `json:"has_more"`
}

func personResponse(person access.Person) personDTO {
	result := personDTO{ID: person.ID, DisplayName: person.DisplayName, Memberships: make([]membershipDTO, 0, len(person.Memberships))}
	for _, membership := range person.Memberships {
		item := membershipDTO{ID: membership.ID, OrganizationID: membership.OrganizationID, OrganizationName: membership.OrganizationName, Title: membership.Title, IsPrimary: membership.IsPrimary, Departments: make([]departmentDTO, 0, len(membership.Departments))}
		for _, department := range membership.Departments {
			item.Departments = append(item.Departments, departmentDTO{ID: department.ID, Name: department.Name})
		}
		result.Memberships = append(result.Memberships, item)
	}
	return result
}
