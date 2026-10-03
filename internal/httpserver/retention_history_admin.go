package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/leileipei/Enterprise_IM/internal/access"
)

type RetentionHistoryService interface {
	ListRetentionPolicyHistory(context.Context, access.TrustedIdentity, string, int) (access.RetentionPolicyHistoryPage, error)
}

func HandlerWithRetentionHistory(next http.Handler, authenticator Authenticator, service RetentionHistoryService) (http.Handler, error) {
	if next == nil || authenticator == nil || service == nil {
		return nil, errors.New("retention history route requires handler, authentication and service")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/retention-policy/history" {
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
		if err != nil || len(params) > 2 {
			rejectAdmin(w, r, 400, "invalid_query")
			return
		}
		limit, cursor := 20, ""
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
		page, err := service.ListRetentionPolicyHistory(r.Context(), id, cursor, limit)
		if err != nil {
			if errors.Is(err, access.ErrInvalidRetentionHistoryQuery) {
				writeAdminError(w, 400, "invalid_retention_history_query")
			} else {
				writeServiceError(w, err)
			}
			return
		}
		history := make([]retentionPolicyDTO, 0, len(page.History))
		for _, p := range page.History {
			history = append(history, retentionDTO(p))
		}
		writeAdminJSON(w, 200, struct {
			History    []retentionPolicyDTO `json:"history"`
			NextCursor string               `json:"next_cursor"`
		}{history, page.NextCursor})
	}), nil
}
