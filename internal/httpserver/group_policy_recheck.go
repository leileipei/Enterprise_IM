package httpserver

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func recheckGroupPolicy(w http.ResponseWriter, r *http.Request, id access.TrustedIdentity,
	groupID string, service ConversationService) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength > 0 {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	if r.Body != nil {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
		if err != nil || len(body) != 0 {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	result, err := service.RecheckGroupPolicy(r.Context(), id, groupID)
	if err != nil {
		writeGroupPolicyRecheckError(w, err)
		return
	}
	writeAdminJSON(w, http.StatusOK, struct {
		Status        string `json:"status"`
		PolicyVersion int64  `json:"policy_version"`
	}{result.Status, result.PolicyVersion})
}

func writeGroupPolicyRecheckError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, policystore.ErrInvalidGroupMembershipRequest):
		writeAdminError(w, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, policystore.ErrForbidden):
		writeAdminError(w, http.StatusForbidden, "invalid_identity")
	case errors.Is(err, policystore.ErrGroupRecheckPermissionDenied):
		writeAdminError(w, http.StatusForbidden, "group_permission_denied")
	case errors.Is(err, policystore.ErrGroupNotAvailable):
		writeAdminError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, policystore.ErrGroupPolicyBlocked):
		writeAdminError(w, http.StatusConflict, "group_policy_blocked")
	default:
		slog.Error("group policy recheck unavailable", "error", err)
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable")
	}
}
