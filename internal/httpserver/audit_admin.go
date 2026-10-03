package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
)

type AuditQueryService interface {
	ListAuditEvents(context.Context, access.TrustedIdentity, access.AuditEventFilter, string, int) (access.AuditEventPage, error)
}

func HandlerWithAuditQuery(next http.Handler, authenticator Authenticator, service AuditQueryService) (http.Handler, error) {
	if next == nil || authenticator == nil || service == nil {
		return nil, errors.New("audit query route requires handler, authentication and service")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/audit-events" {
			next.ServeHTTP(w, r)
			return
		}
		id, ok := authenticateAdmin(w, r, authenticator)
		if !ok {
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			rejectAdmin(w, r, 405, "method_not_allowed")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil || len(body) > 0 {
			rejectAdmin(w, r, 400, "unexpected_body")
			return
		}
		params, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(params) > 5 {
			rejectAdmin(w, r, 400, "invalid_query")
			return
		}
		limit, cursor := 20, ""
		action, outcome, actorUserID := "", "", ""
		for key, values := range params {
			if len(values) != 1 {
				rejectAdmin(w, r, 400, "invalid_query")
				return
			}
			switch key {
			case "limit":
				limit, err = strconv.Atoi(values[0])
				if err != nil || limit < 1 || limit > 100 || strconv.Itoa(limit) != values[0] {
					rejectAdmin(w, r, 400, "invalid_query")
					return
				}
			case "action":
				action = values[0]
				if !adminAuditActionPattern.MatchString(action) {
					rejectAdmin(w, r, 400, "invalid_query")
					return
				}
			case "actor_user_id":
				actorUserID = strings.ToLower(values[0])
				if !validUUID(actorUserID) {
					rejectAdmin(w, r, 400, "invalid_query")
					return
				}
			case "outcome":
				outcome = values[0]
				if outcome != "allow" && outcome != "deny" {
					rejectAdmin(w, r, 400, "invalid_query")
					return
				}
			case "cursor":
				cursor = values[0]
				if cursor == "" || len(cursor) > 1024 {
					rejectAdmin(w, r, 400, "invalid_query")
					return
				}
			default:
				rejectAdmin(w, r, 400, "invalid_query")
				return
			}
		}
		page, err := service.ListAuditEvents(r.Context(), id, access.AuditEventFilter{Action: action, Outcome: outcome, ActorUserID: actorUserID}, cursor, limit)
		if err != nil {
			if errors.Is(err, access.ErrInvalidAuditQuery) {
				writeAdminError(w, 400, "invalid_audit_query")
			} else {
				writeServiceError(w, err)
			}
			return
		}

		events := make([]auditEventDTO, 0, len(page.Events))
		for _, e := range page.Events {
			events = append(events, auditEventDTO{ID: e.ID, ActorUserID: e.ActorUserID, ActingMembershipID: e.ActingMembershipID, Action: e.Action, ResourceType: e.ResourceType, ResourceID: e.ResourceID, Outcome: e.Outcome, Reason: e.Reason, OccurredAt: e.OccurredAt})
		}
		writeAdminJSON(w, 200, struct {
			Events     []auditEventDTO `json:"events"`
			NextCursor string          `json:"next_cursor"`
		}{events, page.NextCursor})
	}), nil
}

var adminAuditActionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

type auditEventDTO struct {
	ID                 string    `json:"id"`
	ActorUserID        string    `json:"actor_user_id"`
	ActingMembershipID string    `json:"acting_membership_id"`
	Action             string    `json:"action"`
	ResourceType       string    `json:"resource_type"`
	ResourceID         *string   `json:"resource_id"`
	Outcome            string    `json:"outcome"`
	Reason             string    `json:"reason"`
	OccurredAt         time.Time `json:"occurred_at"`
}
