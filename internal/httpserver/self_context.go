package httpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type SelfContextService interface {
	GetSelfContext(context.Context, string, string) (policystore.SelfContext, error)
}

// HandlerWithSelfContext adds the Bearer-only identity bootstrap route.
func HandlerWithSelfContext(base http.Handler, authenticator Authenticator, service SelfContextService) (http.Handler, error) {
	if base == nil || authenticator == nil || service == nil {
		return nil, errors.New("base handler, authentication and self context service are required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me" {
			base.ServeHTTP(w, r)
			return
		}
		identity, ok := authenticateBearer(w, r, authenticator)
		if !ok {
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery || hasHeader(r.Header, "X-Acting-Membership-ID") {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil || len(body) != 0 {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
		self, err := service.GetSelfContext(r.Context(), identity.TenantID, identity.UserID)
		if errors.Is(err, policystore.ErrForbidden) {
			writeAdminError(w, http.StatusForbidden, "invalid_identity")
			return
		}
		if err != nil {
			slog.ErrorContext(r.Context(), "self context unavailable", "error", err)
			writeAdminError(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		writeAdminJSON(w, http.StatusOK, self)
	}), nil
}

func hasHeader(headers http.Header, name string) bool {
	for key := range headers {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}
