package httpserver

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type FileSearchService interface {
	SearchFileMessages(context.Context, access.TrustedIdentity, string, string, string, string, int) (policystore.FileSearchPage, error)
	SearchAllFileMessages(context.Context, access.TrustedIdentity, string, string, string, int) (policystore.FileSearchPage, error)
}

func HandlerWithFileSearch(next http.Handler, auth Authenticator, svc FileSearchService) (http.Handler, error) {
	if next == nil || auth == nil || svc == nil {
		return nil, errors.New("file search requires complete dependencies")
	}
	return fileSearchHandler(next, auth, svc), nil
}

// Kept outside the generic files/{id} router: manual URLs cannot open production search.
func HandlerWithClosedFileSearch(next http.Handler, auth Authenticator) http.Handler {
	return fileSearchHandler(next, auth, nil)
}
func fileSearchRoute(path string) (cid, kind string, cross, matched bool) {
	if path == "/api/v1/files/search" {
		return "", "", true, true
	}
	p := strings.Split(path, "/")
	if len(p) != 7 || p[0] != "" || p[1] != "api" || p[2] != "v1" || (p[3] != "conversations" && p[3] != "groups") || p[5] != "files" || p[6] != "search" {
		return "", "", false, false
	}
	kind = "direct"
	if p[3] == "groups" {
		kind = "group"
	}
	return p[4], kind, false, true
}
func fileSearchHandler(next http.Handler, auth Authenticator, svc FileSearchService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cid, routeKind, cross, matched := fileSearchRoute(r.URL.Path)
		if !matched {
			if next != nil {
				next.ServeHTTP(w, r)
			} else {
				http.NotFound(w, r)
			}
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if auth == nil {
			writeAdminError(w, 503, "file_search_unavailable")
			return
		}
		id, ok := authenticateAdmin(w, r, auth)
		if !ok {
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			rejectAdmin(w, r, 405, "method_not_allowed")
			return
		}
		q, kind, cursor, limit, e := parseFileSearchQuery(r, cross)
		if e != nil || (!cross && !validUUID(cid)) {
			rejectAdmin(w, r, 400, "invalid_file_search")
			return
		}
		if svc == nil {
			writeAdminError(w, 503, "file_search_unavailable")
			return
		}
		var page policystore.FileSearchPage
		if cross {
			page, e = svc.SearchAllFileMessages(r.Context(), id, q, kind, cursor, limit)
		} else {
			page, e = svc.SearchFileMessages(r.Context(), id, strings.ToLower(cid), routeKind, q, cursor, limit)
		}
		if e != nil {
			switch {
			case errors.Is(e, policystore.ErrInvalidFileSearch):
				writeAdminError(w, 400, "invalid_file_search")
			case errors.Is(e, policystore.ErrForbidden):
				writeAdminError(w, 403, "invalid_identity")
			case errors.Is(e, policystore.ErrMessageNotAvailable):
				writeAdminError(w, 404, "not_found")
			default:
				writeAdminError(w, 503, "file_search_unavailable")
			}
			return
		}
		matches := make([]fileSearchMatchDTO, 0, len(page.Matches))
		for _, m := range page.Matches {
			matches = append(matches, fileSearchMatchDTO{m.ConversationID, m.Kind, m.MessageID, strconv.FormatInt(m.Seq, 10), m.SenderUserID, m.ServerTime.UTC().Format(time.RFC3339Nano), m.FileID, m.OriginalFilename, strconv.FormatInt(m.ActualSizeBytes, 10), m.DetectedMediaType})
		}
		writeAdminJSON(w, 200, struct {
			Matches []fileSearchMatchDTO `json:"matches"`
			More    bool                 `json:"has_more"`
			Next    string               `json:"next_cursor"`
		}{matches, page.HasMore, page.NextCursor})
	})
}

type fileSearchMatchDTO struct {
	ConversationID    string `json:"conversation_id"`
	Kind              string `json:"conversation_kind"`
	MessageID         string `json:"message_id"`
	Seq               string `json:"seq"`
	SenderUserID      string `json:"sender_user_id"`
	ServerTime        string `json:"server_time"`
	FileID            string `json:"file_id"`
	OriginalFilename  string `json:"original_filename"`
	ActualSizeBytes   string `json:"actual_size_bytes"`
	DetectedMediaType string `json:"detected_media_type"`
}

func parseFileSearchQuery(r *http.Request, cross bool) (q, kind, cursor string, limit int, err error) {
	bad := func() (string, string, string, int, error) { return "", "", "", 0, policystore.ErrInvalidFileSearch }
	if r.URL.ForceQuery || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		return bad()
	}
	if _, present := r.Header["Content-Encoding"]; present {
		return bad()
	}
	params, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil || len(params["q"]) != 1 {
		return bad()
	}
	for key, values := range params {
		if len(values) != 1 || (key != "q" && key != "limit" && key != "cursor" && !(cross && key == "kind")) {
			return bad()
		}
	}
	q, e = policystore.NormalizeMessageSearchQuery(params.Get("q"))
	if e != nil {
		return bad()
	}
	kind = "all"
	if v, exists := params["kind"]; exists {
		kind = v[0]
		if kind != "all" && kind != "direct" && kind != "group" {
			return bad()
		}
	}
	limit = 20
	if v, exists := params["limit"]; exists {
		limit, e = strconv.Atoi(v[0])
		if e != nil || limit < 1 || limit > 50 || strconv.Itoa(limit) != v[0] {
			return bad()
		}
	}
	if v, exists := params["cursor"]; exists {
		cursor = v[0]
		if cursor == "" || len(cursor) > 2048 {
			return bad()
		}
	}
	if r.Body != nil {
		body, e := io.ReadAll(io.LimitReader(r.Body, 1))
		if e != nil || len(body) > 0 {
			return bad()
		}
	}
	return q, kind, cursor, limit, nil
}
