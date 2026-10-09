package httpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type DeviceService interface {
	RegisterDevice(context.Context, access.TrustedIdentity, policystore.DeviceRegistration) (policystore.DeviceGrant, error)
	ListDevices(context.Context, policystore.DeviceOwner, string, int) (policystore.DevicePage, error)
	RevokeDevice(context.Context, policystore.DeviceOwner, string) error
	RenewDevice(context.Context, access.TrustedIdentity, policystore.DeviceCredential) (policystore.DeviceGrant, error)
	GetClientPolicy(context.Context, access.TrustedIdentity) (policystore.ClientPolicy, error)
	SetClientPolicy(context.Context, access.TrustedIdentity, int64, policystore.ClientPolicy) (policystore.ClientPolicy, error)
}

func isReceiptRoute(path string) bool {
	if path == "/api/v1/devices" || strings.HasPrefix(path, "/api/v1/devices/") || path == "/api/v1/admin/client-policy" || path == "/api/v1/me/client-capabilities" {
		return true
	}
	if !strings.HasPrefix(path, "/api/v1/conversations/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/api/v1/conversations/"), "/")
	if len(parts) < 2 {
		return false
	}
	switch parts[1] {
	case "sync", "delivered", "read", "receipts", "unread":
		return true
	}
	return false
}
func HandlerWithClosedReceipts(base http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isReceiptRoute(r.URL.Path) {
			writeAdminError(w, 404, "not_found")
			return
		}
		base.ServeHTTP(w, r)
	})
}

func HandlerWithDevices(base http.Handler, auth Authenticator, svc DeviceService) (http.Handler, error) {
	if base == nil || auth == nil || svc == nil {
		return nil, errors.New("device route dependencies required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/api/v1/admin/client-policy" || path == "/api/v1/me/client-capabilities" {
			serveClientPolicy(w, r, auth, svc)
			return
		}
		if path != "/api/v1/devices" && !strings.HasPrefix(path, "/api/v1/devices/") {
			base.ServeHTTP(w, r)
			return
		}
		suffix := strings.TrimPrefix(path, "/api/v1/devices")
		parts := strings.Split(strings.TrimPrefix(suffix, "/"), "/")
		allowed := "GET, POST"
		id := ""
		renew := false
		if suffix != "" {
			if len(parts) == 1 && parts[0] != "" {
				id = parts[0]
				allowed = "DELETE"
			} else if len(parts) == 2 && parts[1] == "renew" {
				id = parts[0]
				renew = true
				allowed = "POST"
			} else {
				writeAdminError(w, 404, "not_found")
				return
			}
			if !validUUID(id) {
				writeAdminError(w, 400, "invalid_id")
				return
			}
			id = strings.ToLower(id)
		}
		if !(suffix == "" && (r.Method == "GET" || r.Method == "POST")) && r.Method != allowed {
			w.Header().Set("Allow", allowed)
			writeAdminError(w, 405, "method_not_allowed")
			return
		}
		verified, status, code := bearerIdentity(r.Context(), r, auth)
		if status != 0 {
			writeDeviceAuthError(w, status, code)
			return
		}
		owner := policystore.DeviceOwner{TenantID: strings.ToLower(verified.TenantID), UserID: strings.ToLower(verified.UserID)}
		if suffix == "" && r.Method == "GET" {
			serveDeviceList(w, r, owner, svc)
			return
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			writeAdminError(w, 400, "invalid_request")
			return
		}
		if !renew && suffix != "" {
			if !emptyReceiptBody(r) {
				writeAdminError(w, 400, "invalid_request")
				return
			}
			if e := svc.RevokeDevice(r.Context(), owner, id); e != nil {
				writeDeviceError(w, e)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(204)
			return
		}
		identity, ok := deviceActingIdentity(w, r, verified)
		if !ok {
			return
		}
		if renew {
			credential, ok := receiptDeviceHeader(w, r)
			if !ok {
				return
			}
			if credential.ID != id {
				writeDeviceAuthError(w, 401, "invalid_device")
				return
			}
			raw, e := io.ReadAll(io.LimitReader(r.Body, 2049))
			if e != nil || len(raw) > 2048 {
				writeAdminError(w, 400, "invalid_request")
				return
			}
			if len(raw) > 0 {
				r.Body = io.NopCloser(bytes.NewReader(raw))
				if !decodeReceiptJSON(r, 2048, nil, &struct{}{}) {
					writeAdminError(w, 400, "invalid_request")
					return
				}
			}
			grant, e := svc.RenewDevice(r.Context(), identity, credential)
			if e != nil {
				writeDeviceError(w, e)
				return
			}
			writeAdminJSON(w, 200, deviceGrantDTO{DeviceID: grant.DeviceID, ExpiresAt: grant.ExpiresAt, ServerTime: grant.ServerTime})
			return
		}
		var body struct {
			RegistrationID string `json:"registration_id"`
			Platform       string `json:"platform"`
			Name           string `json:"name"`
		}
		if !decodeReceiptJSON(r, 2048, []string{"registration_id", "platform", "name"}, &body) {
			writeAdminError(w, 400, "invalid_request")
			return
		}
		grant, e := svc.RegisterDevice(r.Context(), identity, policystore.DeviceRegistration{RegistrationID: body.RegistrationID, Platform: body.Platform, Name: body.Name})
		if e != nil {
			writeDeviceError(w, e)
			return
		}
		writeAdminJSON(w, 201, deviceGrantDTO{DeviceID: grant.DeviceID, Token: grant.Token, ExpiresAt: grant.ExpiresAt, ServerTime: grant.ServerTime})
	}), nil
}

type deviceGrantDTO struct {
	DeviceID   string    `json:"device_id"`
	Token      string    `json:"device_token,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
	ServerTime time.Time `json:"server_time"`
}

func deviceActingIdentity(w http.ResponseWriter, r *http.Request, v VerifiedIdentity) (access.TrustedIdentity, bool) {
	values := r.Header.Values("X-Acting-Membership-ID")
	if len(values) != 1 || !validUUID(values[0]) {
		writeAdminError(w, 400, "invalid_acting_membership")
		return access.TrustedIdentity{}, false
	}
	return access.TrustedIdentity{TenantID: strings.ToLower(v.TenantID), UserID: strings.ToLower(v.UserID), ActingMembershipID: strings.ToLower(values[0])}, true
}
func receiptDeviceHeader(w http.ResponseWriter, r *http.Request) (policystore.DeviceCredential, bool) {
	ids, tokens := r.Header.Values("X-IM-Device-ID"), r.Header.Values("X-IM-Device-Token")
	if len(ids) != 1 || !validUUID(ids[0]) || len(tokens) != 1 || len(tokens[0]) != 43 {
		writeDeviceAuthError(w, 401, "invalid_device")
		return policystore.DeviceCredential{}, false
	}
	raw, e := base64.RawURLEncoding.Strict().DecodeString(tokens[0])
	if e != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != tokens[0] {
		writeDeviceAuthError(w, 401, "invalid_device")
		return policystore.DeviceCredential{}, false
	}
	return policystore.DeviceCredential{ID: strings.ToLower(ids[0]), Token: tokens[0]}, true
}
func writeDeviceAuthError(w http.ResponseWriter, status int, code string) {
	if status == 401 {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	writeAdminError(w, status, code)
}
func writeDeviceError(w http.ResponseWriter, err error) {
	var duplicate *policystore.DeviceRegistrationExistsError
	switch {
	case errors.As(err, &duplicate):
		writeAdminJSON(w, 409, struct {
			ErrorCode string `json:"error_code"`
			DeviceID  string `json:"device_id"`
		}{ErrorCode: "registration_exists", DeviceID: duplicate.DeviceID})
	case errors.Is(err, policystore.ErrInvalidDevice):
		writeDeviceAuthError(w, 401, "invalid_device")
	case errors.Is(err, policystore.ErrInvalidDeviceRequest), errors.Is(err, policystore.ErrInvalidClientPolicy):
		writeAdminError(w, 400, "invalid_request")
	case errors.Is(err, policystore.ErrForbidden):
		writeAdminError(w, 403, "invalid_identity")
	case errors.Is(err, policystore.ErrDeviceNotFound):
		writeAdminError(w, 404, "not_found")
	case errors.Is(err, policystore.ErrDeviceQuotaExceeded):
		writeAdminError(w, 409, "device_quota")
	case errors.Is(err, policystore.ErrVersionConflict):
		writeAdminError(w, 409, "version_conflict")
	case errors.Is(err, policystore.ErrDeviceRegistrationRateLimited), errors.Is(err, policystore.ErrReceiptRateLimited):
		w.Header().Set("Retry-After", "1")
		writeAdminError(w, 429, "rate_limited")
	default:
		writeAdminError(w, 503, "unavailable")
	}
}

// Decode exact top-level keys, disallow duplicates/case aliases and cap bytes
// before decoding typed values. Unknown or null fields never reach services.
func decodeReceiptJSON(r *http.Request, max int, fields []string, out any) bool {
	contentType, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || contentType != "application/json" {
		return false
	}
	raw, e := io.ReadAll(io.LimitReader(r.Body, int64(max+1)))
	if e != nil || len(raw) > max || !utf8.Valid(raw) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	tok, e := d.Token()
	if e != nil || tok != json.Delim('{') {
		return false
	}
	allowed := map[string]bool{}
	for _, f := range fields {
		allowed[f] = true
	}
	seen := map[string]json.RawMessage{}
	for d.More() {
		key, e := d.Token()
		if e != nil {
			return false
		}
		name, ok := key.(string)
		if !ok || !allowed[name] {
			return false
		}
		if _, ok = seen[name]; ok {
			return false
		}
		var value json.RawMessage
		if e = d.Decode(&value); e != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
		seen[name] = value
	}
	if tok, e = d.Token(); e != nil || tok != json.Delim('}') || len(seen) != len(fields) {
		return false
	}
	if _, e = d.Token(); e != io.EOF {
		return false
	}
	return json.Unmarshal(raw, out) == nil
}
func emptyReceiptBody(r *http.Request) bool {
	raw, e := io.ReadAll(io.LimitReader(r.Body, 1))
	return e == nil && len(raw) == 0
}
func receiptPageParams(r *http.Request, maximum int) (string, int, bool) {
	params, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil || r.URL.ForceQuery {
		return "", 0, false
	}
	for k, v := range params {
		if (k != "limit" && k != "cursor") || len(v) != 1 {
			return "", 0, false
		}
	}
	limit := 20
	if v, ok := params["limit"]; ok {
		n, e := strconv.Atoi(v[0])
		if e != nil || n < 1 || n > maximum || strconv.Itoa(n) != v[0] {
			return "", 0, false
		}
		limit = n
	}
	return params.Get("cursor"), limit, true
}
func serveDeviceList(w http.ResponseWriter, r *http.Request, owner policystore.DeviceOwner, svc DeviceService) {
	cursor, limit, ok := receiptPageParams(r, 100)
	if !ok || !emptyReceiptBody(r) {
		writeAdminError(w, 400, "invalid_request")
		return
	}
	page, e := svc.ListDevices(r.Context(), owner, cursor, limit)
	if e != nil {
		writeDeviceError(w, e)
		return
	}
	type deviceDTO struct {
		ID             string     `json:"device_id"`
		RegistrationID string     `json:"registration_id"`
		Platform       string     `json:"platform"`
		Name           string     `json:"name"`
		Status         string     `json:"status"`
		CreatedAt      time.Time  `json:"created_at"`
		ExpiresAt      time.Time  `json:"expires_at"`
		RevokedAt      *time.Time `json:"revoked_at,omitempty"`
		LastSeenAt     *time.Time `json:"last_seen_at,omitempty"`
	}
	result := struct {
		Devices    []deviceDTO `json:"devices"`
		HasMore    bool        `json:"has_more"`
		NextCursor string      `json:"next_cursor,omitempty"`
	}{Devices: make([]deviceDTO, 0, len(page.Devices)), HasMore: page.HasMore, NextCursor: page.NextCursor}
	utc := func(value *time.Time) *time.Time {
		if value == nil {
			return nil
		}
		at := value.UTC()
		return &at
	}
	for _, d := range page.Devices {
		result.Devices = append(result.Devices, deviceDTO{ID: d.ID, RegistrationID: d.RegistrationID, Platform: d.Platform, Name: d.Name, Status: d.Status, CreatedAt: d.CreatedAt.UTC(), ExpiresAt: d.ExpiresAt.UTC(), RevokedAt: utc(d.RevokedAt), LastSeenAt: utc(d.LastSeenAt)})
	}
	writeAdminJSON(w, 200, result)
}

type ReceiptWriteLimiter interface {
	CheckReceiptWriteRate(context.Context, policystore.DeviceOwner) error
}

func HandlerWithReceiptWriteLimit(base http.Handler, auth Authenticator, limiter ReceiptWriteLimiter) (http.Handler, error) {
	if base == nil || auth == nil || limiter == nil {
		return nil, errors.New("receipt write limiter dependencies required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isReceiptRoute(r.URL.Path) || (r.Method != "POST" && r.Method != "PUT" && r.Method != "DELETE") {
			base.ServeHTTP(w, r)
			return
		}
		v, status, code := bearerIdentity(r.Context(), r, auth)
		if status != 0 {
			writeDeviceAuthError(w, status, code)
			return
		}
		if e := limiter.CheckReceiptWriteRate(r.Context(), policystore.DeviceOwner{TenantID: v.TenantID, UserID: v.UserID}); e != nil {
			writeDeviceError(w, e)
			return
		}
		base.ServeHTTP(w, r)
	}), nil
}
