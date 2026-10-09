package httpserver

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type receiptStub struct {
	sync      func(access.TrustedIdentity, policystore.DeviceCredential, string, int64, int) (policystore.DeviceSyncPage, error)
	delivered func(string) (policystore.ReceiptState, error)
	read      func([]int64) (policystore.ReceiptState, error)
	query     func([]int64) ([]policystore.ReceiptSummary, error)
	users     func(int64, string, int) (policystore.ReceiptUserPage, error)
	unread    func() (policystore.UnreadState, error)
}

func (s receiptStub) SyncDeviceMessages(_ context.Context, id access.TrustedIdentity, d policystore.DeviceCredential, c string, a int64, l int) (policystore.DeviceSyncPage, error) {
	return s.sync(id, d, c, a, l)
}
func (s receiptStub) ConfirmDelivered(_ context.Context, _ access.TrustedIdentity, _ policystore.DeviceCredential, _ string, p string) (policystore.ReceiptState, error) {
	return s.delivered(p)
}
func (s receiptStub) ConfirmRead(_ context.Context, _ access.TrustedIdentity, _ policystore.DeviceCredential, _ string, seqs []int64) (policystore.ReceiptState, error) {
	return s.read(seqs)
}
func (s receiptStub) QueryReceipts(_ context.Context, _ access.TrustedIdentity, _ policystore.DeviceCredential, _ string, seqs []int64) ([]policystore.ReceiptSummary, error) {
	return s.query(seqs)
}
func (s receiptStub) ListReceiptUsers(_ context.Context, _ access.TrustedIdentity, _ policystore.DeviceCredential, _ string, seq int64, cursor string, limit int) (policystore.ReceiptUserPage, error) {
	return s.users(seq, cursor, limit)
}
func (s receiptStub) GetUnread(_ context.Context, _ access.TrustedIdentity, _ policystore.DeviceCredential, _ string) (policystore.UnreadState, error) {
	return s.unread()
}
func receiptRequest(method, path, body string) *http.Request {
	r := deviceRequest(method, path, body, true)
	r.Header.Set("X-IM-Device-ID", targetUserID)
	r.Header.Set("X-IM-Device-Token", strings.Repeat("A", 43))
	return r
}
func TestReceiptHTTPCanonicalInt64AndStrictBody(t *testing.T) {
	const big int64 = 9007199254740993
	calls := 0
	svc := receiptStub{read: func(seqs []int64) (policystore.ReceiptState, error) {
		calls++
		if len(seqs) != 1 || seqs[0] != big {
			t.Fatal(seqs)
		}
		return policystore.ReceiptState{ReadSeq: big, ReceiptRevision: big}, nil
	}, query: func(seqs []int64) ([]policystore.ReceiptSummary, error) {
		if len(seqs) != 1 || seqs[0] != big {
			t.Fatal(seqs)
		}
		return []policystore.ReceiptSummary{{Seq: big, Status: "sent"}}, nil
	}, unread: func() (policystore.UnreadState, error) {
		return policystore.UnreadState{UnreadCount: 1, HeadSeq: big, ReceiptRevision: big, PolicyVersion: big, Exact: false}, nil
	}}
	h, e := HandlerWithReceipts(http.NotFoundHandler(), authFunc(verified), svc)
	if e != nil {
		t.Fatal(e)
	}
	base := "/api/v1/conversations/" + targetUserID
	w := httptest.NewRecorder()
	h.ServeHTTP(w, receiptRequest("POST", base+"/read", `{"seqs":["9007199254740993"]}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"read_seq":"9007199254740993"`) || !strings.Contains(w.Body.String(), `"receipt_revision":"9007199254740993"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, body := range []string{`{"seqs":[9007199254740993]}`, `{"seqs":["9223372036854775808"]}`, `{"seqs":["01"]}`, `{"seqs":["+1"]}`, `{"seqs":["0"]}`, `{"seqs":["1","1"]}`, `{"seqs":["2","1"]}`, `{"seqs":[null]}`, `{"seqs":null}`, `{"seqs":[]}`, `{"seqs":["1"],"seqs":["2"]}`, `{"Seqs":["1"]}`, `{"seqs":["1"],"user_id":"x"}`, `{"seqs":["1"]}{}`, strings.Repeat(" ", 65537) + `{"seqs":["1"]}`} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, receiptRequest("POST", base+"/read", body))
		if w.Code != 400 {
			t.Fatal("bad read accepted", body[:min(len(body), 80)], w.Code)
		}
	}
	if calls != 1 {
		t.Fatal("bad read reached service", calls)
	}
	for _, path := range []string{base + "/receipts?seqs=9007199254740993", base + "/unread"} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, receiptRequest("GET", path, ""))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"9007199254740993"`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, q := range []string{"seqs=1&seqs=2", "seqs=01", "seqs=1,0", "seqs=1,1", "seqs=9223372036854775808", "seqs=1&unknown=2"} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, receiptRequest("GET", base+"/receipts?"+q, ""))
		if w.Code != 400 {
			t.Fatal(q, w.Code)
		}
	}
}
func TestReceiptHTTPErrorMappingAndClosedRoutes(t *testing.T) {
	base := "/api/v1/conversations/" + targetUserID
	for _, tc := range []struct {
		e      error
		status int
		code   string
	}{{policystore.ErrInvalidReceiptRequest, 400, "invalid_request"}, {policystore.ErrInvalidDevice, 401, "invalid_device"}, {policystore.ErrForbidden, 403, "invalid_identity"}, {policystore.ErrSyncProofNotFound, 404, "not_found"}, {policystore.ErrReceiptNotAvailable, 404, "not_found"}, {policystore.ErrSyncProofExpired, 410, "sync_proof_expired"}, {policystore.ErrSyncGap, 409, "sync_gap"}, {policystore.ErrSyncContextChanged, 409, "sync_context_changed"}, {policystore.ErrSyncProofQuota, 429, "sync_proof_quota"}, {errors.New("sensitive-driver-detail"), 503, "unavailable"}} {
		h, e := HandlerWithReceipts(http.NotFoundHandler(), authFunc(verified), receiptStub{delivered: func(string) (policystore.ReceiptState, error) { return policystore.ReceiptState{}, tc.e }})
		if e != nil {
			t.Fatal(e)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, receiptRequest("POST", base+"/delivered", `{"sync_proof":"`+strings.Repeat("A", 43)+`"}`))
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) || strings.Contains(w.Body.String(), "sensitive") {
			t.Fatal(tc, w.Code, w.Body.String())
		}
		if tc.status == 429 && w.Header().Get("Retry-After") == "" {
			t.Fatal("quota retry header missing")
		}
	}
	closed := HandlerWithClosedReceipts(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for _, suffix := range []string{"/sync", "/delivered", "/read", "/receipts", "/receipts/1/users", "/unread"} {
		w := httptest.NewRecorder()
		closed.ServeHTTP(w, httptest.NewRequest("GET", base+suffix, nil))
		if w.Code != 404 {
			t.Fatal(suffix, w.Code)
		}
	}
}
func TestReceiptHTTPSyncStringSequencesAndJWTCacheClamp(t *testing.T) {
	now := time.Now().UTC()
	expiry := now.Add(30 * time.Second)
	const big int64 = 9007199254740993
	auth := authFunc(func(context.Context, string) (VerifiedIdentity, error) {
		return VerifiedIdentity{TenantID: tenantID, UserID: actorID, ExpiresAt: expiry}, nil
	})
	svc := receiptStub{sync: func(id access.TrustedIdentity, d policystore.DeviceCredential, c string, a int64, l int) (policystore.DeviceSyncPage, error) {
		if id.UserID != actorID || d.ID != targetUserID || c != targetUserID || a != big || l != 500 {
			t.Fatal(id, d.ID, c, a, l)
		}
		return policystore.DeviceSyncPage{Page: policystore.MessagePage{ConversationID: c, NextAfterSeq: big + 1, Messages: []policystore.PulledMessage{{Seq: big + 1, MessageID: targetMemID, SenderUserID: actingID, Text: "消息", MessageType: "text", ServerTime: now}}}, ServerTime: now, ProofExpiresAt: now.Add(300 * time.Second), SyncProof: strings.Repeat("A", 43), ReceiptRevision: big, ContentLeases: map[int64]policystore.ContentLease{big + 1: {ContentExpiresAt: now.Add(time.Hour), CacheUntil: now.Add(300 * time.Second)}}}, nil
	}}
	h, e := HandlerWithReceipts(http.NotFoundHandler(), auth, svc)
	if e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, receiptRequest("GET", "/api/v1/conversations/"+targetUserID+"/sync?after_seq=9007199254740993&limit=500", ""))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"seq":"9007199254740994"`) || !strings.Contains(w.Body.String(), `"cache_until":"`+expiry.Format(time.RFC3339Nano)+`"`) || strings.Contains(w.Body.String(), `"seq":900719`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
