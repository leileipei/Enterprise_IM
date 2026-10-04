package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
)

type FileUploadPolicyService interface {
	GetFileUploadPolicy(context.Context, access.TrustedIdentity) (access.FileUploadPolicyRecord, error)
	SetFileUploadPolicy(context.Context, access.TrustedIdentity, access.FileUploadPolicyChange) (access.FileUploadPolicyRecord, error)
	ListFileUploadPolicyHistory(context.Context, access.TrustedIdentity, string, int) (access.FileUploadPolicyHistoryPage, error)
	GetEffectiveFileUploadPolicy(context.Context, access.TrustedIdentity) (files.UploadPolicy, error)
}

func filePolicyDTO(p access.FileUploadPolicyRecord, admin bool) map[string]any {
	v := map[string]any{"enabled": p.Policy.Enabled, "max_size_bytes": strconv.FormatInt(p.Policy.MaxSizeBytes, 10), "allowed_media_types": p.Policy.AllowedMediaTypes, "upload_ttl_seconds": strconv.FormatInt(p.Policy.UploadTTLSeconds, 10)}
	if admin {
		v["tenant_storage_budget_bytes"] = strconv.FormatInt(p.Policy.TenantStorageBudgetBytes, 10)
		v["version"] = strconv.FormatInt(p.Policy.Version, 10)
		v["approval_reference"] = p.ApprovalReference
		v["actor_user_id"] = p.ActorUserID
		v["acting_membership_id"] = p.ActorMembershipID
		if !p.UpdatedAt.IsZero() {
			v["updated_at"] = p.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
	}
	return v
}

// Strict field decoding is shared with file reservation routes; it never accepts duplicate keys.
func decodeFileObject(w http.ResponseWriter, r *http.Request, keys []string, max int64) (map[string]json.RawMessage, error) {
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if e != nil || !utf8.Valid(b) {
		return nil, errors.New("invalid file request")
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	first, e := d.Token()
	if e != nil || first != json.Delim('{') {
		return nil, errors.New("invalid file object")
	}
	allowed := make(map[string]bool, len(keys))
	for _, k := range keys {
		allowed[k] = true
	}
	values := make(map[string]json.RawMessage, len(keys))
	for d.More() {
		k, e := d.Token()
		if e != nil {
			return nil, e
		}
		name, ok := k.(string)
		if !ok || !allowed[name] || values[name] != nil {
			return nil, errors.New("invalid file fields")
		}
		var v json.RawMessage
		if e = d.Decode(&v); e != nil || string(v) == "null" {
			return nil, errors.New("invalid file value")
		}
		values[name] = v
	}
	last, e := d.Token()
	if e != nil || last != json.Delim('}') || len(values) != len(keys) {
		return nil, errors.New("incomplete file request")
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, errors.New("trailing file request")
	}
	return values, nil
}
func fileDecimal(v json.RawMessage) (int64, error) {
	var s string
	if json.Unmarshal(v, &s) != nil {
		return 0, errors.New("file integer must be string")
	}
	n, e := strconv.ParseInt(s, 10, 64)
	if e != nil || n < 0 || strconv.FormatInt(n, 10) != s {
		return 0, errors.New("noncanonical file integer")
	}
	return n, nil
}
func decodeFilePolicy(w http.ResponseWriter, r *http.Request) (access.FileUploadPolicyChange, error) {
	var c access.FileUploadPolicyChange
	keys := []string{"enabled", "max_size_bytes", "allowed_media_types", "upload_ttl_seconds", "tenant_storage_budget_bytes", "expected_version", "approval_reference"}
	v, e := decodeFileObject(w, r, keys, 4096)
	if e != nil {
		return c, e
	}
	if e = json.Unmarshal(v["enabled"], &c.Policy.Enabled); e != nil {
		return c, e
	}
	if e = json.Unmarshal(v["allowed_media_types"], &c.Policy.AllowedMediaTypes); e != nil {
		return c, e
	}
	for _, typ := range c.Policy.AllowedMediaTypes {
		if typ == "" || strings.ContainsRune(typ, utf8.RuneError) {
			return c, files.ErrInvalidUploadPolicy
		}
	}
	for _, field := range []struct {
		k string
		p *int64
	}{{"max_size_bytes", &c.Policy.MaxSizeBytes}, {"upload_ttl_seconds", &c.Policy.UploadTTLSeconds}, {"tenant_storage_budget_bytes", &c.Policy.TenantStorageBudgetBytes}, {"expected_version", &c.ExpectedVersion}} {
		n, e := fileDecimal(v[field.k])
		if e != nil {
			return c, e
		}
		*field.p = n
	}
	if e = json.Unmarshal(v["approval_reference"], &c.ApprovalReference); e != nil || strings.ContainsRune(c.ApprovalReference, utf8.RuneError) {
		return c, files.ErrInvalidUploadPolicy
	}
	if _, e = files.NormalizeUploadPolicy(c.Policy); e != nil {
		return c, e
	}
	return c, nil
}
func writeFilePolicyError(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, files.ErrInvalidUploadPolicy):
		writeAdminError(w, 400, "invalid_file_upload_policy")
	case errors.Is(e, files.ErrStorageBudgetExceeded):
		writeAdminError(w, 409, "storage_budget_exceeded")
	default:
		writeServiceError(w, e)
	}
}
func filePolicyHistoryQuery(r *http.Request) (string, int, error) {
	q, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil || len(q) > 2 {
		return "", 0, errors.New("invalid query")
	}
	limit, cursor := 20, ""
	for k, v := range q {
		if len(v) != 1 {
			return "", 0, errors.New("duplicate query")
		}
		switch k {
		case "limit":
			limit, e = strconv.Atoi(v[0])
			if e != nil || limit < 1 || limit > 100 || strconv.Itoa(limit) != v[0] {
				return "", 0, errors.New("invalid limit")
			}
		case "cursor":
			cursor = v[0]
			if cursor == "" || len(cursor) > 1024 {
				return "", 0, errors.New("invalid cursor")
			}
		default:
			return "", 0, errors.New("unknown query")
		}
	}
	return cursor, limit, nil
}
func HandlerWithFileUploadPolicy(next http.Handler, auth Authenticator, svc FileUploadPolicyService) (http.Handler, error) {
	if next == nil || auth == nil || svc == nil {
		return nil, errors.New("file policy requires handler, authenticator and service")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path != "/api/v1/file-upload-policy" && path != "/api/v1/admin/file-upload-policy" && path != "/api/v1/admin/file-upload-policy/history" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		id, ok := authenticateAdmin(w, r, auth)
		if !ok {
			return
		}
		public := path == "/api/v1/file-upload-policy"
		history := strings.HasSuffix(path, "/history")
		if r.Header.Get("Content-Encoding") != "" || (!history && r.URL.RawQuery != "") {
			rejectAdmin(w, r, 400, "invalid_request")
			return
		}
		if r.Method == http.MethodGet {
			b, e := io.ReadAll(io.LimitReader(r.Body, 1))
			if e != nil || len(b) != 0 {
				rejectAdmin(w, r, 400, "unexpected_body")
				return
			}
			if history {
				cursor, limit, e := filePolicyHistoryQuery(r)
				if e != nil {
					rejectAdmin(w, r, 400, "invalid_query")
					return
				}
				page, e := svc.ListFileUploadPolicyHistory(r.Context(), id, cursor, limit)
				if e != nil {
					writeFilePolicyError(w, e)
					return
				}
				items := make([]map[string]any, 0, len(page.History))
				for _, p := range page.History {
					items = append(items, filePolicyDTO(p, true))
				}
				writeAdminJSON(w, 200, map[string]any{"history": items, "next_cursor": page.NextCursor})
				return
			}
			var p access.FileUploadPolicyRecord
			if public {
				p.Policy, e = svc.GetEffectiveFileUploadPolicy(r.Context(), id)
			} else {
				p, e = svc.GetFileUploadPolicy(r.Context(), id)
			}
			if e != nil {
				writeFilePolicyError(w, e)
				return
			}
			writeAdminJSON(w, 200, filePolicyDTO(p, !public))
			return
		}
		if r.Method == http.MethodPut && !public && !history {
			if r.Header.Get("Content-Type") != "application/json" {
				rejectAdmin(w, r, 400, "invalid_content_type")
				return
			}
			c, e := decodeFilePolicy(w, r)
			if e != nil {
				rejectAdmin(w, r, 400, "invalid_request")
				return
			}
			p, e := svc.SetFileUploadPolicy(r.Context(), id, c)
			if e != nil {
				writeFilePolicyError(w, e)
				return
			}
			writeAdminJSON(w, 200, filePolicyDTO(p, true))
			return
		}
		allowed := "GET"
		if !public && !history {
			allowed = "GET, PUT"
		}
		w.Header().Set("Allow", allowed)
		rejectAdmin(w, r, 405, "method_not_allowed")
	}), nil
}
