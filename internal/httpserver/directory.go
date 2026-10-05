package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type DirectoryService interface {
	GetVisibleMembership(context.Context, access.TrustedIdentity, string) (policystore.DirectoryMembership, error)
	FindVisiblePersonByEmployeeNo(context.Context, access.TrustedIdentity, string) (policystore.DirectoryPerson, error)
	SearchVisiblePeople(context.Context, access.TrustedIdentity, string, int) (policystore.DirectorySearchPage, error)
	ListVisibleOrganizations(context.Context, access.TrustedIdentity) ([]policystore.DirectoryOrganization, error)
	ListVisibleOrganizationMembers(context.Context, access.TrustedIdentity, string, string, int) (policystore.DirectoryMemberPage, error)
}

// HandlerWithDirectory adds ordinary directory lookup and detail routes.
func HandlerWithDirectory(base http.Handler, authenticator Authenticator, directory DirectoryService) (http.Handler, error) {
	if base == nil || authenticator == nil || directory == nil {
		return nil, errors.New("base handler, authentication and directory service are required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/directory" && !strings.HasPrefix(r.URL.Path, "/api/v1/directory/") {
			base.ServeHTTP(w, r)
			return
		}
		identity, ok := authenticateAdmin(w, r, authenticator)
		if !ok {
			return
		}
		if r.URL.Path == "/api/v1/directory/organizations" {
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			listDirectoryOrganizations(w, r, identity, directory)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/directory/organizations/") {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/directory/organizations/"), "/")
			if len(parts) != 2 || parts[1] != "members" {
				rejectAdmin(w, r, http.StatusNotFound, "not_found")
				return
			}
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			listDirectoryOrganizationMembers(w, r, identity, directory, parts[0])
			return
		}
		if r.URL.Path == "/api/v1/directory/users" {
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			params, parseErr := url.ParseQuery(r.URL.RawQuery)
			if parseErr != nil {
				rejectAdmin(w, r, http.StatusBadRequest, "invalid_search")
				return
			}
			if _, nameSearch := params["q"]; nameSearch {
				searchDirectoryPeople(w, r, identity, directory, params)
			} else {
				lookupDirectoryPerson(w, r, identity, directory)
			}
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/v1/directory/memberships/") {
			rejectAdmin(w, r, http.StatusNotFound, "not_found")
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		target := strings.TrimPrefix(r.URL.Path, "/api/v1/directory/memberships/")
		if strings.Contains(target, "/") {
			rejectAdmin(w, r, http.StatusNotFound, "not_found")
			return
		}
		if !validUUID(target) || r.URL.RawQuery != "" {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
		profile, err := directory.GetVisibleMembership(r.Context(), identity, target)
		if err != nil {
			writeDirectoryError(w, err)
			return
		}
		departments := make([]departmentDTO, 0, len(profile.Departments))
		for _, department := range profile.Departments {
			departments = append(departments, departmentDTO{ID: department.ID, Name: department.Name})
		}
		writeAdminJSON(w, http.StatusOK, directoryMembershipDTO{
			UserID: profile.UserID, DisplayName: profile.DisplayName, EmployeeNo: profile.EmployeeNo,
			MembershipID: profile.MembershipID, OrganizationID: profile.OrganizationID,
			OrganizationName: profile.OrganizationName, Title: profile.Title,
			IsPrimary: profile.IsPrimary, Departments: departments,
		})
	}), nil
}

func listDirectoryOrganizationMembers(w http.ResponseWriter, r *http.Request, identity access.TrustedIdentity, directory DirectoryService, orgID string) {
	if !validUUID(orgID) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	params, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(params) > 2 {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	for key, values := range params {
		if (key != "limit" && key != "after") || len(values) != 1 || values[0] == "" {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	limit := 20
	if values, present := params["limit"]; present {
		limit, err = strconv.Atoi(values[0])
		if err != nil || limit < 1 || limit > 20 {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	after := params.Get("after")
	if after != "" && !validUUID(after) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	page, err := directory.ListVisibleOrganizationMembers(r.Context(), identity, orgID, after, limit)
	if err != nil {
		writeDirectoryError(w, err)
		return
	}
	result := directoryMemberPageDTO{People: make([]directoryPersonDTO, 0, len(page.People)), HasMore: page.HasMore}
	for _, person := range page.People {
		result.People = append(result.People, directoryPersonResponse(person))
	}
	if page.NextAfter != "" {
		result.NextAfter = &page.NextAfter
	}
	writeAdminJSON(w, http.StatusOK, result)
}

func listDirectoryOrganizations(w http.ResponseWriter, r *http.Request, identity access.TrustedIdentity, directory DirectoryService) {
	if r.URL.RawQuery != "" {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	nodes, err := directory.ListVisibleOrganizations(r.Context(), identity)
	if err != nil {
		writeDirectoryError(w, err)
		return
	}
	result := directoryOrganizationsDTO{Organizations: make([]directoryOrganizationDTO, 0, len(nodes))}
	for _, node := range nodes {
		item := directoryOrganizationDTO{ID: node.ID, Name: node.Name,
			OrgType: node.OrgType, HasVisibleMembers: node.HasVisibleMembers}
		if node.ParentID != "" {
			parentID := node.ParentID
			item.ParentID = &parentID
		}
		result.Organizations = append(result.Organizations, item)
	}
	writeAdminJSON(w, http.StatusOK, result)
}

func lookupDirectoryPerson(w http.ResponseWriter, r *http.Request, identity access.TrustedIdentity, directory DirectoryService) {
	params, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(params) != 1 || len(params["employee_no"]) != 1 {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_employee_no")
		return
	}
	number := strings.TrimSpace(params.Get("employee_no"))
	if number == "" || !utf8.ValidString(number) || strings.ContainsRune(number, 0) || utf8.RuneCountInString(number) > 128 {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_employee_no")
		return
	}
	person, err := directory.FindVisiblePersonByEmployeeNo(r.Context(), identity, number)
	if err != nil {
		writeDirectoryError(w, err)
		return
	}
	writeAdminJSON(w, http.StatusOK, directoryPersonResponse(person))
}

func searchDirectoryPeople(w http.ResponseWriter, r *http.Request, identity access.TrustedIdentity, directory DirectoryService, params url.Values) {
	if len(params) < 1 || len(params) > 2 || len(params["q"]) != 1 ||
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
	if !utf8.ValidString(query) || strings.ContainsRune(query, 0) ||
		utf8.RuneCountInString(query) < 2 || utf8.RuneCountInString(query) > 100 {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_search")
		return
	}
	limit := 20
	if values, present := params["limit"]; present {
		parsed, err := strconv.Atoi(values[0])
		if err != nil || parsed < 1 || parsed > 20 {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_search")
			return
		}
		limit = parsed
	}
	page, err := directory.SearchVisiblePeople(r.Context(), identity, query, limit)
	if err != nil {
		if errors.Is(err, policystore.ErrInvalidDirectorySearch) {
			writeAdminError(w, http.StatusBadRequest, "invalid_search")
		} else if errors.Is(err, policystore.ErrDirectorySearchTooBroad) {
			writeAdminError(w, http.StatusBadRequest, "refine_search")
		} else {
			writeDirectoryError(w, err)
		}
		return
	}
	result := directorySearchPageDTO{People: make([]directoryPersonDTO, 0, len(page.People)), HasMore: page.HasMore}
	for _, person := range page.People {
		result.People = append(result.People, directoryPersonResponse(person))
	}
	writeAdminJSON(w, http.StatusOK, result)
}

func directoryPersonResponse(person policystore.DirectoryPerson) directoryPersonDTO {
	result := directoryPersonDTO{ID: person.ID, DisplayName: person.DisplayName,
		EmployeeNo: person.EmployeeNo, Memberships: make([]directoryMembershipItemDTO, 0, len(person.Memberships))}
	for _, membership := range person.Memberships {
		departments := make([]departmentDTO, 0, len(membership.Departments))
		for _, department := range membership.Departments {
			departments = append(departments, departmentDTO{ID: department.ID, Name: department.Name})
		}
		result.Memberships = append(result.Memberships, directoryMembershipItemDTO{
			MembershipID: membership.MembershipID, OrganizationID: membership.OrganizationID,
			OrganizationName: membership.OrganizationName, Title: membership.Title,
			IsPrimary: membership.IsPrimary, Departments: departments,
		})
	}
	return result
}

func writeDirectoryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, policystore.ErrInvalidEmployeeNo):
		writeAdminError(w, http.StatusBadRequest, "invalid_employee_no")
	case errors.Is(err, policystore.ErrInvalidDirectoryPage):
		writeAdminError(w, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, policystore.ErrForbidden):
		writeAdminError(w, http.StatusForbidden, "invalid_identity")
	case errors.Is(err, policystore.ErrDirectoryNotVisible):
		writeAdminError(w, http.StatusNotFound, "not_found")
	default:
		slog.Error("directory service unavailable", "error", err)
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable")
	}
}

type directoryMembershipItemDTO struct {
	MembershipID     string          `json:"membership_id"`
	OrganizationID   string          `json:"organization_id"`
	OrganizationName string          `json:"organization_name"`
	Title            string          `json:"title"`
	IsPrimary        bool            `json:"is_primary"`
	Departments      []departmentDTO `json:"departments"`
}

type directoryPersonDTO struct {
	ID          string                       `json:"id"`
	DisplayName string                       `json:"display_name"`
	EmployeeNo  string                       `json:"employee_no"`
	Memberships []directoryMembershipItemDTO `json:"memberships"`
}

type directorySearchPageDTO struct {
	People  []directoryPersonDTO `json:"people"`
	HasMore bool                 `json:"has_more"`
}

type directoryMemberPageDTO struct {
	People    []directoryPersonDTO `json:"people"`
	HasMore   bool                 `json:"has_more"`
	NextAfter *string              `json:"next_after"`
}

type directoryOrganizationDTO struct {
	ID                string  `json:"id"`
	ParentID          *string `json:"parent_id"`
	Name              string  `json:"name"`
	OrgType           string  `json:"org_type"`
	HasVisibleMembers bool    `json:"has_visible_members"`
}

type directoryOrganizationsDTO struct {
	Organizations []directoryOrganizationDTO `json:"organizations"`
}

type directoryMembershipDTO struct {
	UserID           string          `json:"user_id"`
	DisplayName      string          `json:"display_name"`
	EmployeeNo       string          `json:"employee_no"`
	MembershipID     string          `json:"membership_id"`
	OrganizationID   string          `json:"organization_id"`
	OrganizationName string          `json:"organization_name"`
	Title            string          `json:"title"`
	IsPrimary        bool            `json:"is_primary"`
	Departments      []departmentDTO `json:"departments"`
}
