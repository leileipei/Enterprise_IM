package policystore_test

import (
	"context"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Observe the device row itself. RenewDevice also takes identity locks and
// therefore cannot distinguish an early device lock from unrelated contention.
func TestMessageDeviceConversationWaitDoesNotHoldDeviceLock(t *testing.T) {
	c, s, path, body := deviceSendSetup(t, "direct text")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	grant, e := s.RegisterDevice(ctx, publisher(), deviceRegistration(1))
	if e != nil {
		t.Fatal(e)
	}
	device := policystore.DeviceCredential{ID: grant.DeviceID, Token: grant.Token}
	block, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = block.Exec(ctx, "SELECT id FROM conversations WHERE id=$1 FOR SHARE", directA); e != nil {
		t.Fatal(e)
	}
	peer, observer := filePeer(t, c), filePeer(t, c)
	sendDone := make(chan *httptest.ResponseRecorder, 1)
	sent := false
	defer func() {
		_ = block.Rollback(context.Background())
		cancel()
		// Drain the request before filePeer's cleanup closes its connection.
		if !sent {
			<-sendDone
		}
	}()
	real := deviceSendHandler(t, deviceService(peer))
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { real.ServeHTTP(w, r.WithContext(ctx)) })
	go func() { sendDone <- deviceSendRequest(h, path, body, &device) }()
	waitFileSendLock(t, observer, peer.PgConn().PID())
	probe, e := observer.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	_, e = probe.Exec(ctx, "SELECT id FROM user_devices WHERE id=$1 FOR UPDATE NOWAIT", device.ID)
	_ = probe.Rollback(context.Background())
	if e != nil {
		t.Fatalf("sender holds device before acquiring conversation: %v", e)
	}
	if e = block.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	w := <-sendDone
	sent = true
	if w.Code != 200 {
		t.Fatalf("sender after wait: %d %s", w.Code, w.Body.String())
	}
}

// The device lock is deliberately the last row lock. Its wait must precede
// fresh business authorization, for both provisional writes and durable replay.
func TestMessageDeviceWaitRechecksAuthorization(t *testing.T) {
	for _, kind := range []string{"direct text", "group text", "direct file", "group file"} {
		for _, boundary := range []string{"actor_expires", "target_expires", "hard_deny_activates", "exception_expires", "actor_replay"} {
			// The group-file fixture has three members; the two-person temporary
			// exception intentionally does not authorize its third member.
			if kind == "group file" && boundary == "exception_expires" {
				continue
			}
			t.Run(kind+"/"+boundary, func(t *testing.T) {
				c, s, path, body := deviceSendSetup(t, kind)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				grant, e := s.RegisterDevice(ctx, publisher(), deviceRegistration(1))
				if e != nil {
					t.Fatal(e)
				}
				device := policystore.DeviceCredential{ID: grant.DeviceID, Token: grant.Token}
				replay := boundary == "actor_replay"
				if replay {
					if w := deviceSendRequest(deviceSendHandler(t, s), path, body, &device); w.Code != 200 {
						t.Fatal(w.Code, w.Body.String())
					}
				}
				setupBoundary := boundary
				if replay {
					setupBoundary = "actor_expires"
				}
				textCommitBoundary(t, c, s, setupBoundary)
				blocker, e := c.Begin(ctx)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = blocker.Exec(ctx, "SELECT id FROM user_devices WHERE id=$1 FOR UPDATE", device.ID); e != nil {
					t.Fatal(e)
				}
				peer, observer := filePeer(t, c), filePeer(t, c)
				var later atomic.Bool
				s.DB = peer
				s.Now = func() time.Time {
					if later.Load() {
						return at.Add(2 * time.Second)
					}
					return at
				}
				real := deviceSendHandler(t, s)
				h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { real.ServeHTTP(w, r.WithContext(ctx)) })
				done := make(chan *httptest.ResponseRecorder, 1)
				drained := false
				defer func() {
					_ = blocker.Rollback(context.Background())
					cancel()
					if !drained {
						<-done
					}
				}()
				go func() { done <- deviceSendRequest(h, path, body, &device) }()
				waitFileSendLock(t, observer, peer.PgConn().PID())
				later.Store(true)
				if e = blocker.Commit(ctx); e != nil {
					t.Fatal(e)
				}
				w := <-done
				drained = true
				want := 404
				if strings.HasPrefix(kind, "group") {
					want = 409
				}
				if setupBoundary == "actor_expires" {
					want = 403
				}
				if w.Code != want {
					t.Fatalf("authorization after device wait: got %d %s want %d", w.Code, w.Body.String(), want)
				}
				cid := strings.Split(path, "/")[4]
				count := 0
				if replay {
					count = 1
				}
				assertTextCommitState(t, c, cid, count, count)
				var allowed, denied int
				if e = c.QueryRow(ctx, "SELECT count(*) FILTER(WHERE outcome='allow'),count(*) FILTER(WHERE outcome='deny') FROM audit_events WHERE action='message_send'").Scan(&allowed, &denied); e != nil || allowed != count || denied != 1 {
					t.Fatal("audits", allowed, denied, e)
				}
			})
		}
	}
}
