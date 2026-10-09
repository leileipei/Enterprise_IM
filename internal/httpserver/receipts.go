package httpserver

import (
	"context"
	"encoding/base64"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type ReceiptService interface {
	SyncDeviceMessages(context.Context, access.TrustedIdentity, policystore.DeviceCredential, string, int64, int) (policystore.DeviceSyncPage, error)
	ConfirmDelivered(context.Context, access.TrustedIdentity, policystore.DeviceCredential, string, string) (policystore.ReceiptState, error)
	ConfirmRead(context.Context, access.TrustedIdentity, policystore.DeviceCredential, string, []int64) (policystore.ReceiptState, error)
	QueryReceipts(context.Context, access.TrustedIdentity, policystore.DeviceCredential, string, []int64) ([]policystore.ReceiptSummary, error)
	ListReceiptUsers(context.Context, access.TrustedIdentity, policystore.DeviceCredential, string, int64, string, int) (policystore.ReceiptUserPage, error)
	GetUnread(context.Context, access.TrustedIdentity, policystore.DeviceCredential, string) (policystore.UnreadState, error)
}

func receiptSeqList(raw []string, max int) ([]int64, bool) {
	if len(raw) < 1 || len(raw) > max {
		return nil, false
	}
	seqs := make([]int64, len(raw))
	last := int64(0)
	for i, s := range raw {
		n, ok := canonicalReceiptInt(s, true)
		if !ok || n <= last {
			return nil, false
		}
		seqs[i] = n
		last = n
	}
	return seqs, true
}
func receiptQueryParams(r *http.Request, keys ...string) (url.Values, bool) {
	if r.URL.ForceQuery || !emptyReceiptBody(r) {
		return nil, false
	}
	p, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil {
		return nil, false
	}
	for k, v := range p {
		allowed := false
		for _, key := range keys {
			allowed = allowed || key == k
		}
		if !allowed || len(v) != 1 {
			return nil, false
		}
	}
	return p, true
}
func HandlerWithReceipts(base http.Handler, auth Authenticator, svc ReceiptService) (http.Handler, error) {
	if base == nil || auth == nil || svc == nil {
		return nil, errors.New("receipt route dependencies required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/conversations/") {
			base.ServeHTTP(w, r)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/conversations/"), "/")
		if len(parts) < 2 {
			base.ServeHTTP(w, r)
			return
		}
		action := parts[1]
		switch action {
		case "sync", "delivered", "read", "receipts", "unread":
		default:
			base.ServeHTTP(w, r)
			return
		}
		users := action == "receipts" && len(parts) == 4 && parts[3] == "users"
		if (!users && len(parts) != 2) || !validUUID(parts[0]) {
			writeAdminError(w, 404, "not_found")
			return
		}
		method := "GET"
		if action == "delivered" || action == "read" {
			method = "PUT"
		}
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeAdminError(w, 405, "method_not_allowed")
			return
		}
		verified, status, code := bearerIdentity(r.Context(), r, auth)
		if status != 0 {
			writeDeviceAuthError(w, status, code)
			return
		}
		id, ok := deviceActingIdentity(w, r, verified)
		if !ok {
			return
		}
		device, ok := receiptDeviceHeader(w, r)
		if !ok {
			return
		}
		cid := strings.ToLower(parts[0])
		if method == "PUT" {
			if r.URL.RawQuery != "" || r.URL.ForceQuery {
				writeAdminError(w, 400, "invalid_request")
				return
			}
			var state policystore.ReceiptState
			var e error
			if action == "delivered" {
				var body struct {
					Proof string `json:"sync_proof"`
				}
				if !decodeReceiptJSON(r, 65536, []string{"sync_proof"}, &body) || !validReceiptProof(body.Proof) {
					writeAdminError(w, 400, "invalid_request")
					return
				}
				state, e = svc.ConfirmDelivered(r.Context(), id, device, cid, body.Proof)
			} else {
				var body struct {
					Seqs []string `json:"seqs"`
				}
				if !decodeReceiptJSON(r, 65536, []string{"seqs"}, &body) {
					writeAdminError(w, 400, "invalid_request")
					return
				}
				seqs, ok := receiptSeqList(body.Seqs, 500)
				if !ok {
					writeAdminError(w, 400, "invalid_request")
					return
				}
				state, e = svc.ConfirmRead(r.Context(), id, device, cid, seqs)
			}
			if e != nil {
				writeReceiptError(w, e)
				return
			}
			writeAdminJSON(w, 200, receiptStateDTO(state))
			return
		}
		switch {
		case action == "sync":
			p, ok := receiptQueryParams(r, "after_seq", "limit", "message_format")
			if !ok || (p.Has("message_format") && p.Get("message_format") != "typed_v1") {
				writeAdminError(w, 400, "invalid_request")
				return
			}
			after := int64(0)
			if p.Has("after_seq") {
				after, ok = canonicalReceiptInt(p.Get("after_seq"), false)
				if !ok {
					writeAdminError(w, 400, "invalid_request")
					return
				}
			}
			limit := 100
			if p.Has("limit") {
				n, ok := canonicalReceiptInt(p.Get("limit"), true)
				if !ok || n > 500 {
					writeAdminError(w, 400, "invalid_request")
					return
				}
				limit = int(n)
			}
			page, e := svc.SyncDeviceMessages(r.Context(), id, device, cid, after, limit)
			if e != nil {
				writeReceiptError(w, e)
				return
			}
			dto, e := deviceSyncDTO(page, verified.ExpiresAt)
			if e != nil {
				writeReceiptError(w, e)
				return
			}
			if !time.Now().Before(verified.ExpiresAt) {
				writeDeviceAuthError(w, 401, "invalid_token")
				return
			}
			writeAdminJSON(w, 200, dto)
		case users:
			seq, ok := canonicalReceiptInt(parts[2], true)
			if !ok {
				writeAdminError(w, 400, "invalid_request")
				return
			}
			if !emptyReceiptBody(r) {
				writeAdminError(w, 400, "invalid_request")
				return
			}
			cursor, limit, ok := receiptPageParams(r, 50)
			if !ok {
				writeAdminError(w, 400, "invalid_request")
				return
			}
			page, e := svc.ListReceiptUsers(r.Context(), id, device, cid, seq, cursor, limit)
			if e != nil {
				writeReceiptError(w, e)
				return
			}
			list := make([]receiptUserDTO, 0, len(page.Users))
			for _, u := range page.Users {
				list = append(list, receiptUserDTO{UserID: u.UserID, DisplayName: u.DisplayName, DeliveredAt: receiptTime(u.DeliveredAt), ReadAt: receiptTime(u.ReadAt)})
			}
			writeAdminJSON(w, 200, struct {
				Users      []receiptUserDTO `json:"users"`
				HasMore    bool             `json:"has_more"`
				NextCursor string           `json:"next_cursor,omitempty"`
			}{list, page.HasMore, page.NextCursor})
		case action == "receipts":
			p, ok := receiptQueryParams(r, "seqs")
			if !ok {
				writeAdminError(w, 400, "invalid_request")
				return
			}
			seqs, ok := receiptSeqList(strings.Split(p.Get("seqs"), ","), 100)
			if !ok {
				writeAdminError(w, 400, "invalid_request")
				return
			}
			summaries, e := svc.QueryReceipts(r.Context(), id, device, cid, seqs)
			if e != nil {
				writeReceiptError(w, e)
				return
			}
			items := make([]receiptSummaryDTO, 0, len(summaries))
			for _, s := range summaries {
				items = append(items, receiptSummaryDTO{Seq: strconv.FormatInt(s.Seq, 10), Status: s.Status, SummaryAvailable: s.SummaryAvailable, VisibleRecipientCount: s.VisibleRecipientCount, VisibleDeliveredCount: s.VisibleDeliveredCount, VisibleReadCount: s.VisibleReadCount, DeliveredAt: receiptTime(s.DeliveredAt), ReadAt: receiptTime(s.ReadAt)})
			}
			writeAdminJSON(w, 200, struct {
				Receipts []receiptSummaryDTO `json:"receipts"`
			}{items})
		default:
			if _, ok := receiptQueryParams(r); !ok {
				writeAdminError(w, 400, "invalid_request")
				return
			}
			u, e := svc.GetUnread(r.Context(), id, device, cid)
			if e != nil {
				writeReceiptError(w, e)
				return
			}
			writeAdminJSON(w, 200, struct {
				UnreadCount     int64  `json:"unread_count"`
				Exact           bool   `json:"exact"`
				HeadSeq         string `json:"head_seq"`
				ReceiptRevision string `json:"receipt_revision"`
				PolicyVersion   string `json:"policy_version"`
			}{u.UnreadCount, u.Exact, strconv.FormatInt(u.HeadSeq, 10), strconv.FormatInt(u.ReceiptRevision, 10), strconv.FormatInt(u.PolicyVersion, 10)})
		}
	}), nil
}
func validReceiptProof(p string) bool {
	if len(p) != 43 {
		return false
	}
	raw, e := base64.RawURLEncoding.Strict().DecodeString(p)
	return e == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == p
}
func writeReceiptError(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, policystore.ErrInvalidReceiptRequest):
		writeAdminError(w, 400, "invalid_request")
	case errors.Is(e, policystore.ErrReceiptNotAvailable), errors.Is(e, policystore.ErrSyncProofNotFound), errors.Is(e, policystore.ErrMessageNotAvailable):
		writeAdminError(w, 404, "not_found")
	case errors.Is(e, policystore.ErrSyncProofExpired):
		writeAdminError(w, 410, "sync_proof_expired")
	case errors.Is(e, policystore.ErrSyncGap):
		writeAdminError(w, 409, "sync_gap")
	case errors.Is(e, policystore.ErrSyncContextChanged):
		writeAdminError(w, 409, "sync_context_changed")
	case errors.Is(e, policystore.ErrSyncProofQuota):
		w.Header().Set("Retry-After", "1")
		writeAdminError(w, 429, "sync_proof_quota")
	default:
		writeDeviceError(w, e)
	}
}
func receiptTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}
func receiptStateDTO(s policystore.ReceiptState) any {
	return struct {
		ScannedSeq      string `json:"scanned_seq"`
		DeliveredSeq    string `json:"delivered_seq"`
		ReadSeq         string `json:"read_seq"`
		ReceiptRevision string `json:"receipt_revision"`
		Duplicate       bool   `json:"duplicate"`
	}{strconv.FormatInt(s.ScannedSeq, 10), strconv.FormatInt(s.DeliveredSeq, 10), strconv.FormatInt(s.ReadSeq, 10), strconv.FormatInt(s.ReceiptRevision, 10), s.Duplicate}
}

type receiptSummaryDTO struct {
	Seq                   string  `json:"seq"`
	Status                string  `json:"status"`
	SummaryAvailable      bool    `json:"summary_available"`
	VisibleRecipientCount int64   `json:"visible_recipient_count"`
	VisibleDeliveredCount int64   `json:"visible_delivered_count"`
	VisibleReadCount      int64   `json:"visible_read_count"`
	DeliveredAt           *string `json:"delivered_at,omitempty"`
	ReadAt                *string `json:"read_at,omitempty"`
}
type receiptUserDTO struct {
	UserID      string  `json:"user_id"`
	DisplayName string  `json:"display_name"`
	DeliveredAt *string `json:"delivered_at,omitempty"`
	ReadAt      *string `json:"read_at,omitempty"`
}
type syncMessageDTO struct {
	messageItemDTO
	Seq              string `json:"seq"`
	ContentExpiresAt string `json:"content_expires_at,omitempty"`
	CacheUntil       string `json:"cache_until,omitempty"`
}

func deviceSyncDTO(p policystore.DeviceSyncPage, jwtExpiry time.Time) (any, error) {
	old := messageItemsDTO(p.Page, true)
	items := make([]syncMessageDTO, 0, len(old))
	for _, m := range old {
		item := syncMessageDTO{messageItemDTO: m, Seq: strconv.FormatInt(m.Seq, 10)}
		if !m.Redacted {
			lease, ok := p.ContentLeases[m.Seq]
			if !ok || lease.ContentExpiresAt.IsZero() || lease.CacheUntil.IsZero() || jwtExpiry.IsZero() {
				return nil, errors.New("content lease unavailable")
			}
			cacheUntil := lease.CacheUntil
			if jwtExpiry.Before(cacheUntil) {
				cacheUntil = jwtExpiry
			}
			item.ContentExpiresAt = lease.ContentExpiresAt.UTC().Format(time.RFC3339Nano)
			item.CacheUntil = cacheUntil.UTC().Format(time.RFC3339Nano)
		}
		items = append(items, item)
	}
	var proof, expiry *string
	if p.SyncProof != "" {
		if !validReceiptProof(p.SyncProof) || p.ProofExpiresAt.IsZero() {
			return nil, errors.New("sync proof unavailable")
		}
		proofValue := p.SyncProof
		proof = &proofValue
		expiryValue := p.ProofExpiresAt.UTC().Format(time.RFC3339Nano)
		expiry = &expiryValue
	}
	return struct {
		ConversationID  string           `json:"conversation_id"`
		Messages        []syncMessageDTO `json:"messages"`
		NextAfterSeq    string           `json:"next_after_seq"`
		HasMore         bool             `json:"has_more"`
		SyncProof       *string          `json:"sync_proof"`
		ProofExpiresAt  *string          `json:"proof_expires_at"`
		ReceiptRevision string           `json:"receipt_revision"`
		ServerTime      string           `json:"server_time"`
	}{p.Page.ConversationID, items, strconv.FormatInt(p.Page.NextAfterSeq, 10), p.Page.HasMore, proof, expiry, strconv.FormatInt(p.ReceiptRevision, 10), p.ServerTime.UTC().Format(time.RFC3339Nano)}, nil
}
