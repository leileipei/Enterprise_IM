package policystore_test

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type deviceSendAuth struct{}

func (deviceSendAuth) Authenticate(context.Context, string) (httpserver.VerifiedIdentity, error) {
	return httpserver.VerifiedIdentity{TenantID: tenantA, UserID: adminA}, nil
}
func deviceSendHandler(t *testing.T, s policystore.Service) http.Handler {
	t.Helper()
	h, e := httpserver.HandlerWithFileMessages(httpserver.Handler(nil), deviceSendAuth{}, s, s)
	if e != nil {
		t.Fatal(e)
	}
	return h
}
func deviceSendRequest(h http.Handler, path string, body map[string]string, device *policystore.DeviceCredential) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", path, strings.NewReader(string(b)))
	r.Header.Set("Authorization", "Bearer owned-device-test")
	r.Header.Set("X-Acting-Membership-ID", adminM)
	r.Header.Set("Content-Type", "application/json")
	if device != nil {
		r.Header.Set("X-IM-Device-ID", device.ID)
		r.Header.Set("X-IM-Device-Token", device.Token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func deviceSendSetup(t *testing.T, kind string) (*pgx.Conn, policystore.Service, string, map[string]string) {
	t.Helper()
	path := "/api/v1/conversations/" + directA + "/messages"
	var c *pgx.Conn
	var s policystore.Service
	body := map[string]string{"client_msg_id": clientUUIDv7(at, 9211), "message_type": "text", "text": "device message"}
	switch kind {
	case "direct text":
		c = db(t)
		seedDirectConversation(t, c)
		s = deviceService(c)
	case "group text":
		c = db(t)
		seed(t, c)
		s = deviceService(c)
		g, e := s.CreateGroup(context.Background(), publisher(), createGroupRequest(targetM2))
		if e != nil {
			t.Fatal(e)
		}
		path = "/api/v1/groups/" + g.ID + "/messages"
	case "direct file":
		var req policystore.MessageSendRequest
		c, s, _, req = directFileFixture(t)
		body = map[string]string{"client_msg_id": req.ClientMessageID, "message_type": "file", "file_id": req.FileID, "caption": req.Caption}
	case "group file":
		var cid string
		var req policystore.MessageSendRequest
		c, s, cid, _, req = groupFileFixture(t)
		path = "/api/v1/groups/" + cid + "/messages"
		body = map[string]string{"client_msg_id": req.ClientMessageID, "message_type": "file", "file_id": req.FileID, "caption": req.Caption}
	default:
		t.Fatal("unknown case")
	}
	return c, s, path, body
}
func TestMessageDeviceRevocationRejectsNewAndReplayForEveryMessageKind(t *testing.T) {
	for _, kind := range []string{"direct text", "group text", "direct file", "group file"} {
		t.Run(kind, func(t *testing.T) {
			c, s, path, body := deviceSendSetup(t, kind)
			h := deviceSendHandler(t, s)
			grant, e := s.RegisterDevice(context.Background(), publisher(), deviceRegistration(1))
			if e != nil {
				t.Fatal(e)
			}
			device := policystore.DeviceCredential{ID: grant.DeviceID, Token: grant.Token}
			if w := deviceSendRequest(h, path, body, &device); w.Code != 200 {
				t.Fatalf("active device: %d %s", w.Code, w.Body.String())
			}
			if e = s.RevokeDevice(context.Background(), policystore.DeviceOwner{TenantID: tenantA, UserID: adminA}, device.ID); e != nil {
				t.Fatal(e)
			}
			for _, replay := range []bool{true, false} {
				if !replay {
					body["client_msg_id"] = clientUUIDv7(at, 9212)
				}
				w := deviceSendRequest(h, path, body, &device)
				if w.Code != 401 || !strings.Contains(w.Body.String(), "invalid_device") || w.Header().Get("WWW-Authenticate") != "Bearer" {
					t.Fatalf("revoked replay=%t: %d %s", replay, w.Code, w.Body.String())
				}
			}
			for _, table := range []string{"messages", "message_idempotency", "outbox_events"} {
				var n int
				if e = c.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); e != nil || n != 1 {
					t.Fatal(table, n, e)
				}
			}
		})
	}
}
func TestMessageDeviceWrongOwnerTokenExpiryAndLegacyCompatibility(t *testing.T) {
	c, s, path, body := deviceSendSetup(t, "direct text")
	clock := at
	s.Now = func() time.Time { return clock }
	h := deviceSendHandler(t, s)
	grant, e := s.RegisterDevice(context.Background(), publisher(), deviceRegistration(1))
	if e != nil {
		t.Fatal(e)
	}
	device := policystore.DeviceCredential{ID: grant.DeviceID, Token: grant.Token}
	other, e := s.RegisterDevice(context.Background(), groupMemberIdentity(), deviceRegistration(2))
	if e != nil {
		t.Fatal(e)
	}
	for _, d := range []policystore.DeviceCredential{{ID: device.ID, Token: strings.Repeat("A", 43)}, {ID: other.DeviceID, Token: other.Token}} {
		if w := deviceSendRequest(h, path, body, &d); w.Code != 401 {
			t.Fatalf("foreign credential: %d %s", w.Code, w.Body.String())
		}
	}
	clock = grant.ExpiresAt
	if w := deviceSendRequest(h, path, body, &device); w.Code != 401 {
		t.Fatalf("expired credential: %d %s", w.Code, w.Body.String())
	}
	clock = at
	if w := deviceSendRequest(h, path, body, nil); w.Code != 200 {
		t.Fatalf("legacy no-device request: %d %s", w.Code, w.Body.String())
	}
	var count int
	if e = c.QueryRow(context.Background(), "SELECT count(*) FROM messages").Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
}
func TestMessageDeviceExpiryAfterSuccessAuditRollsBack(t *testing.T) {
	c, s, path, body := deviceSendSetup(t, "direct text")
	clock := at
	s.Now = func() time.Time { return clock }
	grant, e := s.RegisterDevice(context.Background(), publisher(), deviceRegistration(1))
	if e != nil {
		t.Fatal(e)
	}
	device := policystore.DeviceCredential{ID: grant.DeviceID, Token: grant.Token}
	s.DB = afterDeviceAuditDB{Beginner: c, after: func() { clock = grant.ExpiresAt }}
	w := deviceSendRequest(deviceSendHandler(t, s), path, body, &device)
	if w.Code != 401 {
		t.Fatalf("lease expired after audit: %d %s", w.Code, w.Body.String())
	}
	for _, table := range []string{"messages", "message_idempotency", "outbox_events"} {
		var n int
		if e = c.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); e != nil || n != 0 {
			t.Fatal(table, n, e)
		}
	}
}
func TestMessageDeviceRevocationSerializedWithSend(t *testing.T) {
	c, s, path, body := deviceSendSetup(t, "direct text")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	grant, e := s.RegisterDevice(ctx, publisher(), deviceRegistration(1))
	if e != nil {
		t.Fatal(e)
	}
	device := policystore.DeviceCredential{ID: grant.DeviceID, Token: grant.Token}
	tx, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "UPDATE user_devices SET revoked_at=$2 WHERE id=$1", device.ID, at); e != nil {
		t.Fatal(e)
	}
	peer, observer := filePeer(t, c), filePeer(t, c)
	done := make(chan *httptest.ResponseRecorder, 1)
	h := deviceSendHandler(t, deviceService(peer))
	go func() { done <- deviceSendRequest(h, path, body, &device) }()
	waitFileSendLock(t, observer, peer.PgConn().PID())
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	w := <-done
	if w.Code != 401 {
		t.Fatalf("concurrent revocation: %d %s", w.Code, w.Body.String())
	}
	var n int
	if e = c.QueryRow(ctx, "SELECT count(*) FROM messages").Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}

// A final file-context query can itself cross the lease boundary after the
// device row was locked. Keep the last clock check free of further I/O.
func TestMessageDeviceExpiryDuringFinalBusinessRecheck(t *testing.T) {
	c, s, path, body := deviceSendSetup(t, "direct file")
	clock := at
	s.Now = func() time.Time { return clock }
	grant, e := s.RegisterDevice(context.Background(), publisher(), deviceRegistration(1))
	if e != nil {
		t.Fatal(e)
	}
	triggered := false
	s.DB = deviceRecheckClockDB{Beginner: c, after: func() { clock = grant.ExpiresAt; triggered = true }}
	w := deviceSendRequest(deviceSendHandler(t, s), path, body, &policystore.DeviceCredential{ID: grant.DeviceID, Token: grant.Token})
	if !triggered || w.Code != 401 {
		t.Fatalf("post-lock recheck lease boundary: triggered=%v status=%d body=%s", triggered, w.Code, w.Body.String())
	}
	for _, table := range []string{"messages", "message_idempotency", "outbox_events"} {
		var n int
		if e = c.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); e != nil || n != 0 {
			t.Fatal(table, n, e)
		}
	}
}

type deviceRecheckClockDB struct {
	access.Beginner
	after func()
}

func (d deviceRecheckClockDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, e := d.Beginner.Begin(ctx)
	if e != nil {
		return nil, e
	}
	return &deviceRecheckClockTx{Tx: tx, after: d.after}, nil
}

type deviceRecheckClockTx struct {
	pgx.Tx
	locked bool
	after  func()
}

func (tx *deviceRecheckClockTx) QueryRow(ctx context.Context, q string, args ...any) pgx.Row {
	row := tx.Tx.QueryRow(ctx, q, args...)
	return deviceRecheckClockRow{Row: row, after: func() {
		if strings.Contains(q, "user_devices") && strings.Contains(q, "FOR SHARE") {
			tx.locked = true
			return
		}
		if tx.locked && strings.Contains(q, "FROM conversations WHERE") && !strings.Contains(q, "FOR UPDATE") {
			tx.after()
		}
	}}
}

type deviceRecheckClockRow struct {
	pgx.Row
	after func()
}

func (r deviceRecheckClockRow) Scan(v ...any) error {
	e := r.Row.Scan(v...)
	if e == nil {
		r.after()
	}
	return e
}
