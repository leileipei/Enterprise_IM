package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type CrossMessageSearchService interface {
	SearchAllTextMessages(context.Context, access.TrustedIdentity, string, string, string, int) (policystore.CrossConversationSearchPage, error)
}

// HandlerWithCrossMessageSearch searches only the caller's participating history.
func HandlerWithCrossMessageSearch(base http.Handler, auth Authenticator, service CrossMessageSearchService) (http.Handler, error) {
	if base == nil || auth == nil || service == nil {
		return nil, errors.New("base handler, authentication and message search service are required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/messages/search" {
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
		q, kind, cursor, limit, ok := parseCrossMessageSearchQuery(w, r)
		if !ok {
			return
		}
		page, err := service.SearchAllTextMessages(r.Context(), id, q, kind, cursor, limit)
		if err != nil {
			switch {
			case errors.Is(err, policystore.ErrInvalidMessageSearch):
				writeAdminError(w, 400, "invalid_search")
			case errors.Is(err, policystore.ErrForbidden):
				writeAdminError(w, 403, "invalid_identity")
			default:
				writeAdminError(w, 503, "unavailable")
			}
			return
		}
		type matchDTO struct {
			Conversation string `json:"conversation_id"`
			Kind         string `json:"conversation_kind"`
			ID           string `json:"id"`
			Seq          string `json:"seq"`
			Sender       string `json:"sender_user_id"`
			Text         string `json:"text"`
			Time         string `json:"server_time"`
		}
		matches := make([]matchDTO, 0, len(page.Messages))
		for _, m := range page.Messages {
			matches = append(matches, matchDTO{m.ConversationID, m.Kind, m.Message.MessageID, strconv.FormatInt(m.Message.Seq, 10), m.Message.SenderUserID, m.Message.Text, m.Message.ServerTime.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")})
		}
		writeAdminJSON(w, 200, struct {
			Messages []matchDTO `json:"messages"`
			More     bool       `json:"has_more"`
			Next     string     `json:"next_cursor"`
		}{matches, page.HasMore, page.NextCursor})
	}), nil
}

func parseCrossMessageSearchQuery(w http.ResponseWriter, r *http.Request) (string, string, string, int, bool) {
	bad := func() (string, string, string, int, bool) {
		rejectAdmin(w, r, 400, "invalid_search")
		return "", "", "", 0, false
	}
	params, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || r.URL.ForceQuery || len(params) < 1 || len(params) > 4 || len(params["q"]) != 1 {
		return bad()
	}
	for key, values := range params {
		if (key != "q" && key != "kind" && key != "limit" && key != "cursor") || len(values) != 1 {
			return bad()
		}
	}
	q, err := policystore.NormalizeMessageSearchQuery(params.Get("q"))
	if err != nil {
		return bad()
	}
	kind := "all"
	if values, exists := params["kind"]; exists {
		kind = values[0]
		if kind != "all" && kind != "direct" && kind != "group" {
			return bad()
		}
	}
	limit := 20
	if values, ok := params["limit"]; ok {
		if !decimalDigits(values[0]) {
			return bad()
		}
		limit, err = strconv.Atoi(values[0])
		if err != nil || limit < 1 || limit > 50 || strconv.Itoa(limit) != values[0] {
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
	return q, kind, cursor, limit, true
}
