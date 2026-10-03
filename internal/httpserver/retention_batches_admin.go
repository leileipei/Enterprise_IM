package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
)

type RetentionBatchService interface {
	ListRetentionBatches(context.Context, access.TrustedIdentity, string, string, string, int) (access.RetentionBatchPage, error)
}

func HandlerWithRetentionBatches(next http.Handler, authenticator Authenticator, service RetentionBatchService) (http.Handler, error) {
	if next == nil || authenticator == nil || service == nil {
		return nil, errors.New("retention batch route requires handler, authentication and service")
	}
	const prefix = "/api/v1/admin/conversations/"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix) {
			next.ServeHTTP(w, r)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
		if len(parts) != 2 || parts[1] != "retention-batches" {
			next.ServeHTTP(w, r)
			return
		}
		id, ok := authenticateAdmin(w, r, authenticator)
		if !ok {
			return
		}
		if !validUUID(parts[0]) {
			rejectAdmin(w, r, 400, "invalid_id")
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			rejectAdmin(w, r, 405, "method_not_allowed")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil || len(body) != 0 {
			rejectAdmin(w, r, 400, "unexpected_body")
			return
		}
		params, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(params) > 3 {
			rejectAdmin(w, r, 400, "invalid_query")
			return
		}
		kind, cursor, limit := "", "", 100
		for key, values := range params {
			if len(values) != 1 {
				rejectAdmin(w, r, 400, "invalid_query")
				return
			}
			switch key {
			case "kind":
				kind = values[0]
			case "cursor":
				cursor = values[0]
				if cursor == "" || len(cursor) > 1024 {
					rejectAdmin(w, r, 400, "invalid_query")
					return
				}
			case "limit":
				limit, err = strconv.Atoi(values[0])
				if err != nil || limit < 1 || limit > 500 {
					rejectAdmin(w, r, 400, "invalid_query")
					return
				}
			default:
				rejectAdmin(w, r, 400, "invalid_query")
				return
			}
		}
		if kind != "body" && kind != "digest" {
			rejectAdmin(w, r, 400, "invalid_query")
			return
		}
		page, err := service.ListRetentionBatches(r.Context(), id, strings.ToLower(parts[0]), kind, cursor, limit)
		if err != nil {
			switch {
			case errors.Is(err, access.ErrInvalidRetentionQuery):
				writeAdminError(w, 400, "invalid_retention_query")
			case errors.Is(err, access.ErrInvalidIdentity):
				writeAdminError(w, 403, "invalid_identity")
			case errors.Is(err, access.ErrNotFound):
				writeAdminError(w, 404, "not_found")
			default:
				writeAdminError(w, 503, "unavailable")
			}
			return
		}
		batches := make([]retentionBatchDTO, 0, len(page.Batches))
		for _, b := range page.Batches {
			batches = append(batches, retentionBatchDTO{
				ID: b.ID, ConversationID: b.ConversationID, Kind: b.Kind, ProcessedAt: b.ProcessedAt, ProcessedCount: b.ProcessedCount, FirstSeq: b.FirstSeq, LastSeq: b.LastSeq, RetentionDays: b.RetentionDays, CutoffAt: b.CutoffAt, MinExpiresAt: b.MinExpiresAt, MaxExpiresAt: b.MaxExpiresAt,
			})
		}
		writeAdminJSON(w, 200, struct {
			Batches    []retentionBatchDTO `json:"batches"`
			NextCursor string              `json:"next_cursor"`
		}{batches, page.NextCursor})
	}), nil
}

type retentionBatchDTO struct {
	ID             string     `json:"id"`
	ConversationID string     `json:"conversation_id"`
	Kind           string     `json:"kind"`
	ProcessedAt    time.Time  `json:"processed_at"`
	ProcessedCount int        `json:"processed_count"`
	FirstSeq       int64      `json:"first_seq"`
	LastSeq        int64      `json:"last_seq"`
	RetentionDays  *int       `json:"retention_days,omitempty"`
	CutoffAt       *time.Time `json:"cutoff_at,omitempty"`
	MinExpiresAt   *time.Time `json:"min_expires_at,omitempty"`
	MaxExpiresAt   *time.Time `json:"max_expires_at,omitempty"`
}
