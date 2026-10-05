package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type MessageSearchService interface {
	SearchTextMessages(context.Context, access.TrustedIdentity, string, string, string, int) (policystore.MessageSearchPage, error)
	SearchGroupTextMessages(context.Context, access.TrustedIdentity, string, string, string, int) (policystore.MessageSearchPage, error)
}

// HandlerWithMessageSearch adds searches within one authorized conversation.
func HandlerWithMessageSearch(base http.Handler, auth Authenticator, service MessageSearchService) (http.Handler, error) {
	if base == nil || auth == nil || service == nil {
		return nil, errors.New("base handler, authentication and message search service are required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) != 6 || parts[0] != "api" || parts[1] != "v1" || (parts[2] != "conversations" && parts[2] != "groups") || parts[4] != "messages" || parts[5] != "search" {
			base.ServeHTTP(w, r)
			return
		}
		id, ok := authenticateAdmin(w, r, auth)
		if !ok {
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			rejectAdmin(w, r, 405, "method_not_allowed")
			return
		}
		if !validUUID(parts[3]) {
			rejectAdmin(w, r, 400, "invalid_search")
			return
		}
		q, cursor, limit, ok := parseMessageSearchQuery(w, r)
		if !ok {
			return
		}
		search := service.SearchTextMessages
		if parts[2] == "groups" {
			search = service.SearchGroupTextMessages
		}
		page, err := search(r.Context(), id, strings.ToLower(parts[3]), q, cursor, limit)
		if err != nil {
			switch {
			case errors.Is(err, policystore.ErrInvalidMessageSearch):
				writeAdminError(w, 400, "invalid_search")
			case errors.Is(err, policystore.ErrForbidden):
				writeAdminError(w, 403, "invalid_identity")
			case errors.Is(err, policystore.ErrMessageNotAvailable):
				writeAdminError(w, 404, "not_found")
			default:
				writeAdminError(w, 503, "unavailable")
			}
			return
		}
		type matchDTO struct {
			ID     string `json:"id"`
			Seq    string `json:"seq"`
			Sender string `json:"sender_user_id"`
			Text   string `json:"text"`
			Time   string `json:"server_time"`
		}
		matches := make([]matchDTO, 0, len(page.Messages))
		for _, m := range page.Messages {
			matches = append(matches, matchDTO{m.MessageID, strconv.FormatInt(m.Seq, 10), m.SenderUserID, m.Text, m.ServerTime.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")})
		}
		writeAdminJSON(w, 200, struct {
			Conversation string     `json:"conversation_id"`
			Messages     []matchDTO `json:"messages"`
			More         bool       `json:"has_more"`
			Next         string     `json:"next_cursor"`
		}{page.ConversationID, matches, page.HasMore, page.NextCursor})
	}), nil
}

func parseMessageSearchQuery(w http.ResponseWriter, r *http.Request) (string, string, int, bool) {
	bad := func() (string, string, int, bool) { rejectAdmin(w, r, 400, "invalid_search"); return "", "", 0, false }
	params, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || r.URL.ForceQuery || len(params) < 1 || len(params) > 3 || len(params["q"]) != 1 {
		return bad()
	}
	for key, values := range params {
		if (key != "q" && key != "limit" && key != "cursor") || len(values) != 1 {
			return bad()
		}
	}
	q, err := policystore.NormalizeMessageSearchQuery(params.Get("q"))
	if err != nil {
		return bad()
	}
	limit := 20
	if values, ok := params["limit"]; ok {
		if !decimalDigits(values[0]) {
			return bad()
		}
		limit, err = strconv.Atoi(values[0])
		if err != nil || limit < 1 || limit > 50 {
			return bad()
		}
	}
	cursor := ""
	if values, ok := params["cursor"]; ok {
		cursor = values[0]
		if cursor == "" || len(cursor) > 2048 {
			return bad()
		}
	}
	if r.Body != nil {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
		if err != nil || len(body) != 0 {
			return bad()
		}
	}
	return q, cursor, limit, true
}
