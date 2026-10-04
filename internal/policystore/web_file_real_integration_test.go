package policystore_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestWebFileRealLifecycle(t *testing.T) {
	for _, kind := range []string{"direct", "group"} {
		t.Run(kind, func(t *testing.T) {
			f := newWebFileFixture(t)
			cid := directA
			if kind == "group" {
				cid = f.group(t)
			}
			f.browser(t, "file_lifecycle", map[string]any{"conversation": cid, "kind": kind, "samples": f.samples(t, false)})
			for _, table := range []string{"messages", "message_attachments", "outbox_events"} {
				var count int
				if e := f.real.conn.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&count); e != nil || count != 4 {
					t.Fatal("four browser sends not persisted exactly once", table, count, e)
				}
			}
			if f.real.logins.Load() != 2 || f.real.exchanges.Load() != 2 {
				t.Fatal("PKCE login and reload not verified")
			}
			f.assertPrivate(t)
		})
	}
}
func TestWebFileRealRejectedScan(t *testing.T) {
	f := newWebFileFixture(t)
	f.browser(t, "file_lifecycle", map[string]any{"conversation": directA, "kind": "direct", "rejected": true, "samples": f.samples(t, true)})
	var count int
	if e := f.real.conn.QueryRow(context.Background(), "SELECT count(*) FROM message_attachments").Scan(&count); e != nil || count != 0 {
		t.Fatal("rejected content bound", count, e)
	}
	f.assertPrivate(t)
}
func TestWebFileRealSettings(t *testing.T) {
	f := newWebFileFixture(t)
	f.browser(t, "file_settings", map[string]any{})
	var version, days int64
	if e := f.real.conn.QueryRow(context.Background(), "SELECT version,file_retention_days FROM tenant_file_retention_policy WHERE tenant_id=$1", tenantA).Scan(&version, &days); e != nil || version != 1 || days != 30 {
		t.Fatal("real policy write absent", version, days, e)
	}
	f.assertPrivate(t)
}
func TestWebFileRealProductionClosed(t *testing.T) {
	f := newWebFileFixture(t)
	var caps map[string]bool
	if e := json.Unmarshal(f.real.request(t, f.real.api, "GET", "/api/v1/file-capabilities", nil, 200), &caps); e != nil || caps["message_send_enabled"] || caps["download_enabled"] || caps["filename_search_enabled"] {
		t.Fatal("production capabilities open")
	}
	for _, path := range []string{"/api/v1/files/" + targetM2 + "/content", "/api/v1/files/search?q=report", "/api/v1/conversations/" + directA + "/files/search?q=report"} {
		f.real.request(t, f.real.api, "GET", path, nil, 503)
	}
	f.real.request(t, f.real.api, "POST", "/api/v1/conversations/"+directA+"/messages", map[string]string{"client_msg_id": clientUUIDv7(time.Now(), 9930), "message_type": "file", "file_id": targetM2, "caption": ""}, 503)
	f.assertPrivate(t)
}
