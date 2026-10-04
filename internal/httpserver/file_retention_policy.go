package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
)

type FileRetentionPolicyService interface {
	GetFileRetentionPolicy(context.Context, access.TrustedIdentity) (access.FileRetentionPolicyRecord, error)
	SetFileRetentionPolicy(context.Context, access.TrustedIdentity, access.FileRetentionPolicyChange) (access.FileRetentionPolicyRecord, error)
	ListFileRetentionPolicyHistory(context.Context, access.TrustedIdentity, string, int) (access.FileRetentionPolicyHistoryPage, error)
}

func fileRetentionPolicyDTO(p access.FileRetentionPolicyRecord) map[string]any {
	v := map[string]any{"file_retention_days": strconv.FormatInt(p.Policy.Days, 10), "cleanup_enabled": p.Policy.CleanupEnabled, "version": strconv.FormatInt(p.Policy.Version, 10), "approval_reference": p.ApprovalReference, "actor_user_id": p.ActorUserID, "acting_membership_id": p.ActorMembershipID, "current_policy_applies_to_existing_files": true}
	if !p.UpdatedAt.IsZero() {
		v["updated_at"] = p.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return v
}
func decodeFileRetentionPolicy(w http.ResponseWriter, r *http.Request) (access.FileRetentionPolicyChange, error) {
	var c access.FileRetentionPolicyChange
	v, e := decodeFileObject(w, r, []string{"file_retention_days", "cleanup_enabled", "expected_version", "approval_reference"}, 4096)
	if e != nil {
		return c, e
	}
	if e = json.Unmarshal(v["cleanup_enabled"], &c.Policy.CleanupEnabled); e != nil {
		return c, e
	}
	if e = json.Unmarshal(v["approval_reference"], &c.ApprovalReference); e != nil || strings.ContainsRune(c.ApprovalReference, utf8.RuneError) {
		return c, files.ErrInvalidRetentionPolicy
	}
	if c.Policy.Days, e = fileDecimal(v["file_retention_days"]); e != nil {
		return c, e
	}
	if c.ExpectedVersion, e = fileDecimal(v["expected_version"]); e != nil {
		return c, e
	}
	if _, e = files.NormalizeRetentionPolicy(c.Policy); e != nil {
		return c, e
	}
	return c, nil
}
func writeFileRetentionPolicyError(w http.ResponseWriter, e error) {
	if errors.Is(e, files.ErrInvalidRetentionPolicy) {
		writeAdminError(w, 400, "invalid_file_retention_policy")
		return
	}
	writeServiceError(w, e)
}
func HandlerWithFileRetentionPolicy(next http.Handler, auth Authenticator, svc FileRetentionPolicyService) (http.Handler, error) {
	if next == nil || auth == nil || svc == nil {
		return nil, errors.New("file policy requires handler, authenticator and service")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path != "/api/v1/admin/file-retention-policy" && path != "/api/v1/admin/file-retention-policy/history" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		id, ok := authenticateAdmin(w, r, auth)
		if !ok {
			return
		}
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
				page, e := svc.ListFileRetentionPolicyHistory(r.Context(), id, cursor, limit)
				if e != nil {
					writeFileRetentionPolicyError(w, e)
					return
				}
				items := make([]map[string]any, 0, len(page.History))
				for _, p := range page.History {
					items = append(items, fileRetentionPolicyDTO(p))
				}
				writeAdminJSON(w, 200, map[string]any{"history": items, "next_cursor": page.NextCursor})
				return
			}
			var p access.FileRetentionPolicyRecord
			p, e = svc.GetFileRetentionPolicy(r.Context(), id)
			if e != nil {
				writeFileRetentionPolicyError(w, e)
				return
			}
			writeAdminJSON(w, 200, fileRetentionPolicyDTO(p))
			return
		}
		if r.Method == http.MethodPut && !history {
			if r.Header.Get("Content-Type") != "application/json" {
				rejectAdmin(w, r, 400, "invalid_content_type")
				return
			}
			c, e := decodeFileRetentionPolicy(w, r)
			if e != nil {
				rejectAdmin(w, r, 400, "invalid_request")
				return
			}
			p, e := svc.SetFileRetentionPolicy(r.Context(), id, c)
			if e != nil {
				writeFileRetentionPolicyError(w, e)
				return
			}
			writeAdminJSON(w, 200, fileRetentionPolicyDTO(p))
			return
		}
		allowed := "GET"
		if !history {
			allowed = "GET, PUT"
		}
		w.Header().Set("Allow", allowed)
		rejectAdmin(w, r, 405, "method_not_allowed")
	}), nil
}
