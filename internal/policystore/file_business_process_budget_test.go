package policystore_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"net/url"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestFileBusinessProcessRP11(t *testing.T) {
	f := newFileBusinessProcessFixture(t)
	f.startAPI(t, false, true, "api-a")
	// Deliberately a database-boundary workload, not 1002 claimed real scans.
	// Guarded normal send service binds all schema-valid scanner fingerprints.
	s := policystore.Service{DB: f.pool, MessageRatePerSecond: 100000}
	ordinal := 0
	appendFile := func(cid, kind, name string) {
		ordinal++
		m := namedSearchFile(t, f.conn, cid, name)
		req := policystore.MessageSendRequest{ClientMessageID: businessClientID(), MessageType: "file", FileID: m.ID}
		var e error
		if kind == "group" {
			_, e = s.SendGroupMessage(context.Background(), publisher(), cid, req)
		} else {
			_, e = s.SendMessage(context.Background(), publisher(), cid, req)
		}
		if e != nil {
			t.Fatal("normal database boundary binding failed", ordinal)
		}
	}
	for i := 0; i < 500; i++ {
		appendFile(directA, "direct", "ordinary.pdf")
	}
	appendFile(directA, "direct", "needle.pdf")
	query := func(path string) map[string]any {
		start := time.Now()
		before := f.dbRelay.queries.Load()
		code, _, b := f.request(t, "GET", "api-a", path, "admin", adminM, nil, "")
		elapsed := time.Since(start)
		businessStatus(t, code, 200)
		if elapsed >= 5*time.Second {
			t.Fatal("original search budget exceeded")
		}
		if f.dbRelay.queries.Load() <= before {
			t.Fatal("actual SQL observation missing")
		}
		page := businessJSON(t, b)
		facts, _ := json.Marshal(map[string]any{"path_class": "filename_search", "duration_ms": elapsed.Milliseconds(), "actual_sql_execute_frames": f.dbRelay.queries.Load() - before, "returned_matches": len(page["matches"].([]any)), "has_more": page["has_more"]})
		processPrivateFile(t, filepath.Join(f.privateRoot, fmt.Sprintf("query-%d.json", time.Now().UnixNano())), facts)
		return page
	}
	path := "/api/v1/conversations/" + directA + "/files/search?q=needle&limit=20"
	first := query(path)
	if len(first["matches"].([]any)) != 0 || first["has_more"] != true {
		t.Fatal("501 candidates were not bounded")
	}
	cursor := first["next_cursor"].(string)
	raw, e := base64.RawURLEncoding.DecodeString(cursor)
	if e != nil {
		t.Fatal(e)
	}
	var pos map[string]any
	if json.Unmarshal(raw, &pos) != nil || pos["a"] != "500" {
		t.Fatal("candidate boundary is not exactly 500")
	}
	next := query(path + "&cursor=" + url.QueryEscape(cursor))
	if len(next["matches"].([]any)) != 1 || next["has_more"] != false {
		t.Fatal("bounded continuation lost 501st binding")
	}
	ids := []string{}
	for i := 0; i < 21; i++ {
		ids = append(ids, f.group(t))
	}
	sort.Strings(ids)
	for _, cid := range ids[:20] {
		for i := 0; i < 25; i++ {
			appendFile(cid, "group", "ordinary.pdf")
		}
	}
	appendFile(ids[20], "group", "needle.pdf")
	first = query("/api/v1/files/search?q=needle&kind=group&limit=20")
	if len(first["matches"].([]any)) != 0 || first["has_more"] != true {
		t.Fatal("21 conversations not bounded")
	}
	cursor = first["next_cursor"].(string)
	raw, e = base64.RawURLEncoding.DecodeString(cursor)
	if e != nil {
		t.Fatal(e)
	}
	if json.Unmarshal(raw, &pos) != nil || pos["c"] != ids[19] {
		t.Fatal("conversation boundary not 20")
	}
	next = query("/api/v1/files/search?q=needle&kind=group&limit=20&cursor=" + url.QueryEscape(cursor))
	if len(next["matches"].([]any)) != 1 || next["has_more"] != false {
		t.Fatal("21st conversation lost")
	}
	one := query("/api/v1/files/search?q=ordinary&kind=group&limit=1")
	if len(one["matches"].([]any)) != 1 || one["has_more"] != true {
		t.Fatal("limit1 did not progress")
	}
	var n int
	if e := f.conn.QueryRow(context.Background(), `SELECT count(*) FROM message_attachments a JOIN file_objects f ON f.id=a.file_id WHERE f.state='ready' AND f.sha256=f.scan_sha256 AND f.sha256=a.sealed_sha256 AND f.scan_engine IS NOT NULL AND f.scan_definition_version IS NOT NULL`).Scan(&n); e != nil || n != 1002 {
		t.Fatal("normal proof-boundary bindings absent")
	}
	f.assertEvidence(t)
	t.Run("legacy_damaged_500", TestFileSearchBoundedProgress)
	t.Run("legacy_unicode_literal", TestFileSearchLiteral)
}
