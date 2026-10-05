package policystore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/leileipei/Enterprise_IM/internal/files"
)

type fileBusinessSample struct{ Filename, MediaType, Path string }

var businessClientCounter atomic.Int64

func businessClientID() string { return clientUUIDv7(time.Now(), int(businessClientCounter.Add(1))) }
func (f *fileBusinessProcessFixture) group(t *testing.T) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"client_request_id": freshFile().ID, "name": "正式进程项目群", "member_membership_ids": []string{targetM2, groupMemberC}})
	code, _, raw := f.request(t, "POST", "api-a", "/api/v1/groups", "admin", adminM, b, "application/json")
	businessStatus(t, code, 201)
	return businessJSON(t, raw)["id"].(string)
}
func (f *fileBusinessProcessFixture) uploadAndAwaitReady(t *testing.T, conversation, kind string, sample fileBusinessSample) files.Metadata {
	t.Helper()
	b, e := os.ReadFile(sample.Path)
	if e != nil {
		t.Fatal(e)
	}
	id := f.upload(t, "api-a", conversation, sample.Filename, sample.MediaType, b, true)
	m := files.Metadata{}
	m.ID = id
	m.ConversationID = conversation
	if e = f.conn.QueryRow(context.Background(), `SELECT original_filename,detected_media_type,object_key,object_version_id,sha256,scan_sha256,scan_engine,scan_definition_version FROM file_objects WHERE id=$1 AND state='ready' AND scan_job_id IS NOT NULL AND scanned_at IS NOT NULL`, id).Scan(&m.OriginalFilename, &m.DetectedMediaType, &m.ObjectKey, &m.ObjectVersionID, &m.SHA256, &m.ScanSHA256, &m.ScanEngine, &m.ScanDefinitionVersion); e != nil {
		t.Fatal("official scan proof absent")
	}
	h := sha256.Sum256(b)
	if !bytes.Equal(h[:], m.SHA256) || !bytes.Equal(m.SHA256, m.ScanSHA256) || m.ScanEngine == "" || m.ScanDefinitionVersion == "" {
		t.Fatal("official scan fingerprint differs")
	}
	return m
}
func (f *fileBusinessProcessFixture) sendFile(t *testing.T, node, cid, kind, id, client string) map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"client_msg_id": client, "message_type": "file", "file_id": id, "caption": "正式附件说明"})
	prefix := "conversations"
	if kind == "group" {
		prefix = "groups"
	}
	code, _, raw := f.request(t, "POST", node, "/api/v1/"+prefix+"/"+cid+"/messages", "admin", adminM, b, "application/json")
	businessStatus(t, code, 200)
	return businessJSON(t, raw)
}
func (f *fileBusinessProcessFixture) assertBinding(t *testing.T, fileID, clientID string) {
	t.Helper()
	var n int
	var sha, scan, sealed []byte
	e := f.conn.QueryRow(context.Background(), `SELECT count(*) FROM message_attachments a JOIN messages m ON m.id=a.message_id JOIN outbox_events o ON o.message_id=m.id WHERE a.file_id=$1 AND m.client_msg_id=$2`, fileID, clientID).Scan(&n)
	if e != nil || n != 1 {
		t.Fatal("attachment/message/outbox is not exactly one", n)
	}
	e = f.conn.QueryRow(context.Background(), `SELECT f.sha256,f.scan_sha256,a.sealed_sha256 FROM file_objects f JOIN message_attachments a ON a.file_id=f.id WHERE f.id=$1`, fileID).Scan(&sha, &scan, &sealed)
	if e != nil || !bytes.Equal(sha, scan) || !bytes.Equal(scan, sealed) {
		t.Fatal("binding changed original fingerprint")
	}
}
func (f *fileBusinessProcessFixture) notificationSocket(t *testing.T) *websocket.Conn {
	t.Helper()
	code, _, b := f.request(t, "POST", "api-a", "/api/v1/realtime/tickets", "admin", adminM, nil, "")
	businessStatus(t, code, 200)
	ticket := businessJSON(t, b)["ticket"].(string)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, e := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.apiURL, "http")+"/api/v1/realtime", &websocket.DialOptions{Subprotocols: []string{"enterprise-im.v1", "ticket." + ticket}})
	if e != nil {
		t.Fatal("official realtime connection failed")
	}
	t.Cleanup(func() { ws.CloseNow() })
	realtimeE2EFrame(t, ws, `{"type":"ready","resync_required":true}`)
	return ws
}
func (f *fileBusinessProcessFixture) waitPublished(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		e := f.conn.QueryRow(context.Background(), "SELECT count(*) FROM outbox_events WHERE published_at IS NOT NULL").Scan(&n)
		if e == nil && n >= want && f.redis.XLen(context.Background(), f.stream).Val() >= int64(want) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("official Outbox publication absent")
}
func (f *fileBusinessProcessFixture) assertDownloaded(t *testing.T, id string, body []byte, name string) {
	t.Helper()
	code, h, b := f.request(t, "GET", "api-a", "/api/v1/files/"+id+"/content", "admin", adminM, nil, "")
	businessStatus(t, code, 200)
	_, params, e := mime.ParseMediaType(h.Get("Content-Disposition"))
	if e != nil || params["filename"] != name || !bytes.Equal(b, body) {
		t.Fatal("download bytes or Unicode filename differ")
	}
}
func (f *fileBusinessProcessFixture) assertTypedSearch(t *testing.T, cid, kind, id, name string) {
	t.Helper()
	prefix := "conversations"
	if kind == "group" {
		prefix = "groups"
	}
	code, _, b := f.request(t, "GET", "api-a", "/api/v1/"+prefix+"/"+cid+"/messages?after_seq=0&limit=100&message_format=typed_v1", "admin", adminM, nil, "")
	businessStatus(t, code, 200)
	if !bytes.Contains(b, []byte(id)) || !bytes.Contains(b, []byte(`"message_type":"file"`)) {
		t.Fatal("ACK was not recovered in typed pull")
	}
	if bytes.Contains(b, []byte(`"download_available":true`)) {
		t.Fatal("typed hint changed")
	}
	code, _, b = f.request(t, "GET", "api-a", "/api/v1/"+prefix+"/"+cid+"/files/search?q="+"%E9%9B%86%E5%9B%A2"+"&limit=50", "admin", adminM, nil, "")
	businessStatus(t, code, 200)
	if !bytes.Contains(b, []byte(id)) {
		t.Fatal("filename search missing accepted attachment")
	}
}
func TestFileBusinessProcessRP03(t *testing.T) {
	f := newFileBusinessProcessFixture(t)
	f.startWorkers(t)
	f.startAPI(t, true, true, "api-a")
	ws := f.notificationSocket(t)
	count := 0
	for _, kind := range []string{"direct", "group"} {
		cid := directA
		if kind == "group" {
			cid = f.group(t)
		}
		for _, s := range (&webFileFixture{}).samples(t, false) {
			if strings.HasSuffix(s.Name, ".txt") {
				s.Name = "\uFEFF" + s.Name
			}
			m := f.uploadAndAwaitReady(t, cid, kind, fileBusinessSample{s.Name, s.MIME, s.Path})
			client := businessClientID()
			ack := f.sendFile(t, "api-a", cid, kind, m.ID, client)
			if ack["duplicate"] != false {
				t.Fatal("first ACK duplicate")
			}
			f.assertBinding(t, m.ID, client)
			f.assertTypedSearch(t, cid, kind, m.ID, s.Name)
			body, _ := os.ReadFile(s.Path)
			f.assertDownloaded(t, m.ID, body, s.Name)
			count++
		}
	}
	f.waitPublished(t, count)
	realtimeE2EFrame(t, ws, `{"type":"sync_required"}`)
	f.assertEvidence(t)
	// Explicit Chrome save is added by Task12; HTTP proof alone is not that claim.
}
func TestFileBusinessProcessRP04(t *testing.T) {
	f := newFileBusinessProcessFixture(t)
	f.startWorkers(t)
	f.startAPI(t, true, true, "api-a")
	secret := []byte("P426_PRIVATE_UNAUTHORIZED_BODY")
	id := f.upload(t, "api-a", directA, "集团权限.txt", "text/plain", secret, true)
	f.sendFile(t, "api-a", directA, "direct", id, businessClientID())
	// A group administrator with no participation also receives no content.
	outsiderAdmin := freshFile().ID
	run(t, f.conn, `INSERT INTO admin_grants(id,tenant_id,membership_id,membership_organization_id,role,effective_from) VALUES($1,$2,$3,$4,'group_admin','2020-01-01')`, outsiderAdmin, tenantA, groupMemberC, orgA)
	for _, c := range []struct{ subject, member string }{{"outsider", groupMemberC}, {"foreign", otherM}} {
		code, _, b := f.request(t, "GET", "api-a", "/api/v1/files/"+id+"/content", c.subject, c.member, nil, "")
		businessStatus(t, code, 404)
		if bytes.Contains(b, secret) {
			t.Fatal("unauthorized content exposed")
		}
	}
	var audits int
	if e := f.conn.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action='file_download' AND outcome='deny'`).Scan(&audits); e != nil || audits < 2 {
		t.Fatal("valid-login denial audits missing", audits)
	}
	run(t, f.conn, `CREATE FUNCTION deny_audit_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='file_download' AND NEW.outcome='deny' THEN RAISE EXCEPTION 'owned denial audit fault'; END IF; RETURN NEW; END $$; CREATE TRIGGER deny_audit_fault BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION deny_audit_fault()`)
	code, _, faultBody := f.request(t, "GET", "api-a", "/api/v1/files/"+id+"/content", "outsider", groupMemberC, nil, "")
	businessStatus(t, code, 503)
	if bytes.Contains(faultBody, secret) {
		t.Fatal("failed denial audit exposed content")
	}
	run(t, f.conn, `DROP TRIGGER deny_audit_fault ON audit_events`)
	code, _, b := f.request(t, "GET", "api-a", "/api/v1/files/"+id+"/content", "", "", nil, "")
	businessStatus(t, code, 401)
	if bytes.Contains(b, secret) {
		t.Fatal("unauthenticated content exposed")
	}
	f.assertEvidence(t)
}
func TestFileBusinessProcessRP05(t *testing.T) {
	f := newFileBusinessProcessFixture(t)
	f.startWorkers(t)
	f.startAPI(t, true, true, "api-a")
	for _, s := range (&webFileFixture{}).samples(t, true) {
		body, _ := os.ReadFile(s.Path)
		if s.Outcome == "select" {
			b, _ := json.Marshal(map[string]string{"upload_request_id": freshFile().ID, "original_filename": s.Name, "declared_media_type": s.MIME, "declared_size_bytes": fmt.Sprint(len(body))})
			code, _, _ := f.request(t, "POST", "api-a", "/api/v1/conversations/"+directA+"/files", "admin", adminM, b, "application/json")
			businessStatus(t, code, 400)
			continue
		}
		id := f.upload(t, "api-a", directA, s.Name, s.MIME, body, false)
		want := "rejected"
		if strings.Contains(s.Name, "损坏") || strings.Contains(s.Name, "伪装") {
			want = "scan_failed"
		}
		f.waitState(t, "api-a", id, want)
		b, _ := json.Marshal(map[string]string{"client_msg_id": businessClientID(), "message_type": "file", "file_id": id})
		code, _, _ := f.request(t, "POST", "api-a", "/api/v1/conversations/"+directA+"/messages", "admin", adminM, b, "application/json")
		if code == 200 {
			t.Fatal("rejected file bound")
		}
	}
	run(t, f.conn, `UPDATE tenant_file_upload_policy SET max_size_bytes=1024,version=version+1 WHERE tenant_id=$1`, tenantA)
	body, _ := json.Marshal(map[string]string{"upload_request_id": freshFile().ID, "original_filename": "集团小上限.txt", "declared_media_type": "text/plain", "declared_size_bytes": "1025"})
	code, _, _ := f.request(t, "POST", "api-a", "/api/v1/conversations/"+directA+"/files", "admin", adminM, body, "application/json")
	if code == 201 {
		t.Fatal("tenant smaller maximum ignored")
	}
	var n int
	if e := f.conn.QueryRow(context.Background(), "SELECT count(*) FROM message_attachments").Scan(&n); e != nil || n != 0 {
		t.Fatal("invalid content attached")
	}
	f.assertEvidence(t)
}
func TestFileBusinessProcessRP06(t *testing.T) {
	f := newFileBusinessProcessFixture(t)
	f.startWorkers(t)
	f.startAPI(t, true, true, "api-a")
	body := []byte("集团停上传后的旧文件")
	id := f.upload(t, "api-a", directA, "集团旧文件.txt", "text/plain", body, true)
	f.sendFile(t, "api-a", directA, "direct", id, businessClientID())
	f.restartAPI(t, "api-a", false, true)
	f.assertDownloaded(t, id, body, "集团旧文件.txt")
	f.assertTypedSearch(t, directA, "direct", id, "集团旧文件.txt")
	code, h, _ := f.request(t, "PUT", "api-a", "/api/v1/files/"+id+"/content", "admin", adminM, body, "application/octet-stream")
	businessStatus(t, code, 405)
	if h.Get("Allow") != "GET" {
		t.Fatal("upload-off methods differ")
	}
	code, _, _ = f.request(t, "POST", "api-a", "/api/v1/conversations/"+directA+"/files", "admin", adminM, []byte(`{}`), "application/json")
	businessStatus(t, code, 503)
	run(t, f.conn, `UPDATE tenant_file_upload_policy SET enabled=false,version=version+1 WHERE tenant_id=$1`, tenantA)
	b, _ := json.Marshal(map[string]string{"client_msg_id": businessClientID(), "message_type": "file", "file_id": id})
	code, _, _ = f.request(t, "POST", "api-a", "/api/v1/conversations/"+directA+"/messages", "admin", adminM, b, "application/json")
	if code == 200 {
		t.Fatal("tenant policy did not constrain resend")
	}
	f.assertEvidence(t)
}
func TestFileBusinessProcessRP10(t *testing.T) {
	f := newFileBusinessProcessFixture(t)
	f.startWorkers(t)
	f.startAPI(t, true, true, "api-a")
	f.startAPI(t, true, true, "api-b")
	ws := f.notificationSocket(t)
	id := f.upload(t, "api-a", directA, "集团双节点.txt", "text/plain", []byte("same original body"), true)
	client := businessClientID()
	a := f.sendFile(t, "api-a", directA, "direct", id, client)
	b := f.sendFile(t, "api-b", directA, "direct", id, client)
	for _, key := range []string{"message_id", "conversation_id", "seq", "server_time"} {
		if a[key] != b[key] {
			t.Fatal("retry ACK origin changed", key)
		}
	}
	if a["duplicate"] != false || b["duplicate"] != true {
		t.Fatal("retry duplicate classification differs")
	}
	f.assertBinding(t, id, client)
	f.waitPublished(t, 1)
	realtimeE2EFrame(t, ws, `{"type":"sync_required"}`)
	ws.CloseNow()
	f.assertTypedSearch(t, directA, "direct", id, "集团双节点.txt")
	f.assertEvidence(t)
}
