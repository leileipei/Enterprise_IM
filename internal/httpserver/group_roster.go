package httpserver

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func listGroupMembers(w http.ResponseWriter, r *http.Request, id access.TrustedIdentity, groupID string, service ConversationService) {
	cursor, limit, ok := parseConversationListQuery(w, r)
	if !ok {
		return
	}
	page, err := service.ListGroupMembers(r.Context(), id, groupID, cursor, limit)
	if err != nil {
		switch {
		case errors.Is(err, policystore.ErrInvalidGroupRosterRequest):
			writeAdminError(w, http.StatusBadRequest, "invalid_request")
		case errors.Is(err, policystore.ErrForbidden):
			writeAdminError(w, http.StatusForbidden, "invalid_identity")
		case errors.Is(err, policystore.ErrGroupRosterPermissionDenied):
			writeAdminError(w, http.StatusForbidden, "group_permission_denied")
		case errors.Is(err, policystore.ErrGroupNotAvailable):
			writeAdminError(w, http.StatusNotFound, "not_found")
		default:
			slog.ErrorContext(r.Context(), "group roster unavailable", "error", err)
			writeAdminError(w, http.StatusServiceUnavailable, "unavailable")
		}
		return
	}
	type memberDTO struct {
		IntervalID       string `json:"interval_id"`
		DisplayName      string `json:"display_name"`
		Role             string `json:"role"`
		OrganizationName string `json:"organization_name"`
	}
	members := make([]memberDTO, 0, len(page.Members))
	for _, member := range page.Members {
		members = append(members, memberDTO{member.IntervalID, member.DisplayName, member.Role, member.OrganizationName})
	}
	writeAdminJSON(w, http.StatusOK, struct {
		Members    []memberDTO `json:"members"`
		HasMore    bool        `json:"has_more"`
		NextCursor string      `json:"next_cursor,omitempty"`
	}{members, page.HasMore, page.NextCursor})
}
