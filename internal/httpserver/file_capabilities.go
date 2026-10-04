package httpserver

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"io"
	"net/http"
)

type FileCapabilities struct {
	UploadEnabled         bool `json:"upload_enabled"`
	MessageSendEnabled    bool `json:"message_send_enabled"`
	DownloadEnabled       bool `json:"download_enabled"`
	FilenameSearchEnabled bool `json:"filename_search_enabled"`
}
type FileIdentityValidator interface {
	ValidateFileIdentity(context.Context, access.TrustedIdentity) error
}

// Capabilities describe actual handler assembly. They are never a file grant.
func HandlerWithFileCapabilities(next http.Handler, auth Authenticator, identity FileIdentityValidator, caps FileCapabilities) (http.Handler, error) {
	if next == nil || auth == nil || identity == nil {
		return nil, errors.New("file capabilities require complete dependencies")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/file-capabilities" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		id, ok := authenticateAdmin(w, r, auth)
		if !ok {
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			rejectAdmin(w, r, 405, "method_not_allowed")
			return
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery || r.Header.Get("Content-Encoding") != "" {
			rejectAdmin(w, r, 400, "invalid_request")
			return
		}
		if r.Body != nil {
			b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
			if e != nil || len(b) != 0 {
				rejectAdmin(w, r, 400, "invalid_request")
				return
			}
		}
		if e := identity.ValidateFileIdentity(r.Context(), id); e != nil {
			if errors.Is(e, access.ErrInvalidIdentity) {
				writeAdminError(w, 403, "invalid_identity")
			} else {
				writeAdminError(w, 503, "file_service_unavailable")
			}
			return
		}
		writeAdminJSON(w, 200, caps)
	}), nil
}
