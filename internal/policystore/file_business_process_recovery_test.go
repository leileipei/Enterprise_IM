package policystore_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Gate only reads from the actual S3 response, before the API can authorize
// content output. No object/store/business response is manufactured.
type processObjectGate struct {
	file             string
	entered, release chan struct{}
	once             sync.Once
}
type processGatedBody struct {
	io.ReadCloser
	ctx  context.Context
	gate *processObjectGate
}

func (b *processGatedBody) Read(p []byte) (int, error) {
	b.gate.once.Do(func() { close(b.gate.entered) })
	select {
	case <-b.gate.release:
		return b.ReadCloser.Read(p)
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
}
func (f *fileBusinessProcessFixture) gate(t *testing.T, id string) *processObjectGate {
	t.Helper()
	g := &processObjectGate{id, make(chan struct{}), make(chan struct{}), sync.Once{}}
	f.objectGate.Store(g)
	t.Cleanup(func() {
		f.objectGate.Store(nil)
		select {
		case <-g.release:
		default:
			close(g.release)
		}
	})
	return g
}

type processDownloadReply struct {
	status int
	body   []byte
	err    error
}

func (f *fileBusinessProcessFixture) asyncDownload(t *testing.T, id, subject, member string, expiry time.Time) <-chan processDownloadReply {
	t.Helper()
	token, e := f.sign(subject, expiry)
	if e != nil {
		t.Fatal(e)
	}
	req := downloadRequest(t, f.apiURL, token, member, id)
	out := make(chan processDownloadReply, 1)
	go func() {
		res, e := f.client.Do(req)
		if e != nil {
			out <- processDownloadReply{err: e}
			return
		}
		defer res.Body.Close()
		b, e := io.ReadAll(res.Body)
		out <- processDownloadReply{res.StatusCode, b, e}
	}()
	return out
}
func awaitObjectGate(t *testing.T, g *processObjectGate) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("actual S3 read boundary absent")
	}
}
func awaitDownloadReply(t *testing.T, ch <-chan processDownloadReply) processDownloadReply {
	t.Helper()
	select {
	case x := <-ch:
		return x
	case <-time.After(70 * time.Second):
		t.Fatal("download exceeded original deadline")
	}
	return processDownloadReply{}
}
func (f *fileBusinessProcessFixture) waitTerminal(t *testing.T, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		var phase string
		if f.conn.QueryRow(context.Background(), "SELECT phase FROM file_download_sessions WHERE id=$1", sessionID).Scan(&phase) == nil && (phase == "completed" || phase == "interrupted" || phase == "unknown") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("actual terminal missing")
}
func (f *fileBusinessProcessFixture) restartRepair(t *testing.T, once bool) {
	t.Helper()
	f.stopProcess(t, "repair", false)
	args := []string{"--execute", "--repair-only"}
	if once {
		args = append(args, "--once")
	}
	f.launchProcess(t, "repair", "im-file-cleaner", map[string]string{"IM_DATABASE_URL": f.repairDSN}, once, args...)
	if once {
		select {
		case e := <-f.waits["repair"]:
			if err := f.integrationProcesses["repair"].Exited("exit:0", integrationProcessExit(e)); err != nil {
				t.Error(err)
			}
			delete(f.integrationProcesses, "repair")
			if e != nil {
				t.Fatal("official repair-only failed")
			}
			f.logs["repair"].Close()
			delete(f.processes, "repair")
			delete(f.waits, "repair")
			delete(f.logs, "repair")
		case <-time.After(35 * time.Second):
			t.Fatal("repair-only startup/batch exhausted")
		}
	} else {
		f.await(t, "repair", 35*time.Second, func(b []byte) bool { return bytes.Contains(b, []byte(`"status":"file_download_audit_repair"`)) })
	}
}
func (f *fileBusinessProcessFixture) signalAPI(t *testing.T, node string, signal os.Signal) {
	t.Helper()
	if f.processes[node] == nil || f.processes[node].Process.Signal(signal) != nil {
		t.Fatal("owned API signal failed")
	}
}
func TestFileBusinessProcessRP07(t *testing.T) {
	for _, scenario := range []string{"jwt_expiry", "hard_deny", "membership", "uploader", "rejoin_gap", "ttl", "scheduled_policy"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFileBusinessProcessFixture(t)
			f.startWorkers(t)
			f.startAPI(t, true, true, "api-a")
			cid := directA
			kind := "direct"
			if scenario == "rejoin_gap" {
				cid = f.group(t)
				kind = "group"
			}
			body := []byte("P426_REVOKED_CONTENT")
			id := f.upload(t, "api-a", cid, "集团撤权.txt", "text/plain", body, true)
			f.sendFile(t, "api-a", cid, kind, id, businessClientID())
			// Temporal DB fixture: age only the real, already accepted message. The
			// formal API still reads actual database clock and original policy budget.
			if scenario == "ttl" {
				run(t, f.conn, `UPDATE messages SET accepted_at=clock_timestamp()-interval '2 days' WHERE conversation_id=$1`, cid)
			}
			g := f.gate(t, id)
			expiry := time.Now().Add(time.Minute)
			if scenario == "jwt_expiry" {
				expiry = time.Now().Add(2 * time.Second)
			}
			ch := f.asyncDownload(t, id, "peer", targetM2, expiry)
			awaitObjectGate(t, g)
			switch scenario {
			case "jwt_expiry":
				time.Sleep(time.Until(expiry) + 100*time.Millisecond)
			case "membership":
				run(t, f.conn, `UPDATE user_organizations SET status='suspended' WHERE id=$1`, targetM2)
			case "uploader":
				run(t, f.conn, `UPDATE users SET status='frozen' WHERE id=$1`, adminA)
			case "hard_deny", "scheduled_policy":
				when := time.Now().Add(-time.Second)
				if scenario == "scheduled_policy" {
					when = time.Now().Add(time.Second)
				}
				_, e := (policystore.Service{DB: f.pool}).Publish(context.Background(), publisher(), 0, []policy.Rule{{ID: "p426-download-deny", TenantID: tenantA, Effect: policy.EffectHardDeny, Action: policy.ActionFileDownload, EffectiveFrom: when, Reason: "owned test"}}, "owned process revocation")
				if e != nil {
					t.Fatal("guarded real policy publication failed")
				}
				if scenario == "scheduled_policy" {
					time.Sleep(time.Until(when) + 50*time.Millisecond)
				}
			case "rejoin_gap":
				var interval string
				if f.conn.QueryRow(context.Background(), `SELECT id::text FROM conversation_membership_intervals WHERE conversation_id=$1 AND user_id=$2 AND status='active'`, cid, personA).Scan(&interval) != nil {
					t.Fatal("actual group interval absent")
				}
				code, _, _ := f.request(t, "POST", "api-a", "/api/v1/groups/"+cid+"/leave", "peer", targetM2, []byte(`{"interval_id":"`+interval+`"}`), "application/json")
				businessStatus(t, code, 200)
			case "ttl":
				setDownloadRetention(t, f.conn, 1)
			}
			close(g.release)
			x := awaitDownloadReply(t, ch)
			if x.status == 200 || bytes.Contains(x.body, body) {
				t.Fatal("revocation survived first-output authorization")
			}
			if scenario == "rejoin_gap" {
				gapID := f.upload(t, "api-a", cid, "集团空档.txt", "text/plain", body, true)
				f.sendFile(t, "api-a", cid, "group", gapID, businessClientID())
				code, _, _ := f.request(t, "POST", "api-a", "/api/v1/groups/"+cid+"/invitations", "admin", adminM, []byte(`{"client_request_id":"`+freshFile().ID+`","target_membership_id":"`+targetM2+`"}`), "application/json")
				businessStatus(t, code, 201)
				code, _, gapBody := f.request(t, "GET", "api-a", "/api/v1/files/"+gapID+"/content", "peer", targetM2, nil, "")
				businessStatus(t, code, 404)
				if bytes.Contains(gapBody, body) {
					t.Fatal("rejoin gap exposed body")
				}
			}
			f.assertEvidence(t)
		})
	}
	t.Run("after_first_chunk", func(t *testing.T) {
		f := newFileBusinessProcessFixture(t)
		f.startWorkers(t)
		f.startAPI(t, true, true, "api-a")
		body := bytes.Repeat([]byte("x"), 8<<20)
		id := f.upload(t, "api-a", directA, "集团在途.txt", "text/plain", body, true)
		f.sendFile(t, "api-a", directA, "direct", id, businessClientID())
		address := strings.TrimPrefix(f.apiURL, "http://")
		c, e := net.Dial("tcp", address)
		if e != nil {
			t.Fatal(e)
		}
		defer c.Close()
		c.(*net.TCPConn).SetReadBuffer(1024)
		c.SetDeadline(time.Now().Add(15 * time.Second))
		fmt.Fprintf(c, "GET /api/v1/files/%s/content HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nX-Acting-Membership-ID: %s\r\n\r\n", id, address, f.token(t, "peer"), targetM2)
		res, e := http.ReadResponse(bufio.NewReader(c), nil)
		if e != nil || res.StatusCode != 200 {
			t.Fatal("actual first output absent")
		}
		defer res.Body.Close()
		first := make([]byte, 32768)
		n, e := io.ReadFull(res.Body, first)
		if e != nil || n != len(first) {
			t.Fatal("actual first chunk absent")
		}
		run(t, f.conn, `UPDATE user_organizations SET status='suspended' WHERE id=$1`, targetM2)
		revoked := time.Now()
		var phase string
		var written int64
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			e = f.conn.QueryRow(context.Background(), `SELECT phase,bytes_written FROM file_download_sessions WHERE file_id=$1`, id).Scan(&phase, &written)
			if e == nil && phase == "interrupted" {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if phase != "interrupted" || written >= int64(len(body)) {
			t.Fatal("in-flight output not bounded", phase, written)
		}
		record, _ := json.Marshal(map[string]any{"client_received_before_revocation": n, "server_writer_accepted": written, "revoked_at": revoked.UTC(), "terminal_observed_at": time.Now().UTC(), "kernel_bytes_already_authorized_are_not_revocable": true})
		processPrivateFile(t, filepath.Join(f.privateRoot, "inflight.json"), record)
		f.assertEvidence(t)
	})
}
func TestFileBusinessProcessRP08(t *testing.T) {
	t.Run("slow_client_SIGTERM", func(t *testing.T) {
		f := newFileBusinessProcessFixture(t)
		f.startWorkers(t)
		f.startAPI(t, true, true, "api-a")
		body := bytes.Repeat([]byte("x"), 8<<20)
		id := f.upload(t, "api-a", directA, "集团退出.txt", "text/plain", body, true)
		f.sendFile(t, "api-a", directA, "direct", id, businessClientID())
		address := strings.TrimPrefix(f.apiURL, "http://")
		c, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err = c.(*net.TCPConn).SetReadBuffer(1024); err != nil {
			t.Fatal(err)
		}
		if err = c.SetDeadline(time.Now().Add(25 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err = fmt.Fprintf(c, "GET /api/v1/files/%s/content HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nX-Acting-Membership-ID: %s\r\n\r\n", id, address, f.token(t, "peer"), targetM2); err != nil {
			t.Fatal(err)
		}
		res, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil || res.StatusCode != 200 {
			t.Fatal("actual first output absent", err)
		}
		defer res.Body.Close()
		first := make([]byte, 32768)
		n, err := io.ReadFull(res.Body, first)
		if err != nil || n != len(first) || !bytes.Equal(first, body[:len(first)]) {
			t.Fatal("actual first chunk absent", n, err)
		}
		// Keep the TCP connection open, but consume no further body before SIGTERM.
		var session, phase string
		if err = f.conn.QueryRow(context.Background(), `SELECT id::text,phase FROM file_download_sessions WHERE file_id=$1`, id).Scan(&session, &phase); err != nil || phase != "authorized" {
			t.Fatal("shutdown did not start during actual output", phase, err)
		}
		spool := f.nodes["api-a"].SpoolDir
		if _, err = os.Stat(filepath.Join(spool, session, ".session.json")); err != nil {
			t.Fatal("active prepared spool absent", err)
		}
		signaled := time.Now()
		f.stopProcess(t, "api-a", false)
		exited := time.Now()
		if exited.Sub(signaled) > 20*time.Second {
			t.Fatal("slow-client SIGTERM exceeded 10+10")
		}
		var written int64
		var ack bool
		var terminals, audits int
		err = f.conn.QueryRow(context.Background(), `SELECT s.phase,s.bytes_written,s.audit_acked,(SELECT count(*) FROM file_download_terminal_events WHERE session_id=s.id),(SELECT count(*) FROM file_worker_audit_events WHERE download_session_id=s.id) FROM file_download_sessions s WHERE s.id=$1`, session).Scan(&phase, &written, &ack, &terminals, &audits)
		if err != nil || phase != "interrupted" || ack || terminals != 1 || audits != 0 || written >= int64(len(body)) {
			t.Fatal("slow-client terminal evidence differs before audit repair", phase, written, ack, terminals, audits, err)
		}
		// API settlement persists the terminal fact; the separate official
		// repair-only process acknowledges its machine audit, including retries.
		f.restartRepair(t, true)
		f.restartRepair(t, true)
		err = f.conn.QueryRow(context.Background(), `SELECT s.audit_acked,(SELECT count(*) FROM file_download_terminal_events WHERE session_id=s.id),(SELECT count(*) FROM file_worker_audit_events WHERE download_session_id=s.id) FROM file_download_sessions s WHERE s.id=$1`, session).Scan(&ack, &terminals, &audits)
		if err != nil || !ack || terminals != 1 || audits != 1 {
			t.Fatal("slow-client terminal audit not acknowledged exactly once", ack, terminals, audits, err)
		}
		if _, err = os.Stat(filepath.Join(spool, session)); !os.IsNotExist(err) {
			t.Fatal("prepared session was not released", err)
		}
		// Same owner and spool must reacquire the released process lock.
		f.startAPI(t, false, true, "api-a")
		code, _, _ := f.request(t, "GET", "api-a", "/health/ready", "", "", nil, "")
		businessStatus(t, code, 200)
		f.stopProcess(t, "api-a", false)
		record, _ := json.Marshal(map[string]any{"client_received_before_SIGTERM": n, "server_writer_accepted": written, "phase_at_SIGTERM": "authorized", "terminal_phase": phase, "terminal_events": terminals, "terminal_audits": audits, "signaled_at": signaled.UTC(), "exited_at": exited.UTC(), "same_owner_restart_ready": true, "connection_kept_open_without_further_reads": true})
		processPrivateFile(t, filepath.Join(f.privateRoot, "shutdown-slow-client.json"), record)
		f.assertEvidence(t)
	})
	f := newFileBusinessProcessFixture(t)
	f.startWorkers(t)
	f.startAPI(t, true, true, "api-a")
	f.startAPI(t, false, true, "api-b")
	id := f.upload(t, "api-a", directA, "集团恢复.txt", "text/plain", []byte("owned recovery"), true)
	f.sendFile(t, "api-a", directA, "direct", id, businessClientID())
	g := f.gate(t, id)
	ch := f.asyncDownload(t, id, "admin", adminM, time.Now().Add(time.Minute))
	awaitObjectGate(t, g)
	rootDuring := f.nodes["api-a"].SpoolDir
	heldRoot := rootDuring + "-held-inflight"
	if os.Rename(rootDuring, heldRoot) != nil || os.Mkdir(rootDuring, 0700) != nil {
		t.Fatal("in-flight owned directory replacement failed")
	}
	processPrivateFile(t, filepath.Join(rootDuring, "unknown-inflight"), []byte("preserve in-flight replacement"))
	code, _, _ := f.request(t, "GET", "api-a", "/health/ready", "", "", nil, "")
	businessStatus(t, code, 503)
	f.expectedExitCodes["api-a"] = 1
	start := time.Now()
	f.stopProcess(t, "api-a", false)
	if time.Since(start) > 20*time.Second {
		t.Fatal("SIGTERM exceeded 10+10")
	}
	close(g.release)
	awaitDownloadReply(t, ch)
	f.objectGate.Store(nil)
	replacementEvidence := rootDuring + "-replacement-evidence"
	if os.Rename(rootDuring, replacementEvidence) != nil || os.Rename(heldRoot, rootDuring) != nil {
		t.Fatal("owned spool restoration failed")
	}
	if b, e := os.ReadFile(filepath.Join(replacementEvidence, "unknown-inflight")); e != nil || string(b) != "preserve in-flight replacement" {
		t.Fatal("unknown in-flight evidence cleared")
	}
	f.startAPI(t, false, true, "api-a")
	g = f.gate(t, id)
	ch = f.asyncDownload(t, id, "peer", targetM2, time.Now().Add(time.Minute))
	awaitObjectGate(t, g)
	var session string
	if e := f.conn.QueryRow(context.Background(), `SELECT id::text FROM file_download_sessions WHERE file_id=$1 AND requester_user_id=$2 AND phase='preparing'`, id, personA).Scan(&session); e != nil {
		t.Fatal(e)
	}
	source := filepath.Join(f.nodes["api-a"].SpoolDir, session)
	if _, e := os.Stat(filepath.Join(source, ".session.json")); e != nil {
		t.Fatal("actual orphan source missing")
	}
	f.stopProcess(t, "api-a", true)
	close(g.release)
	awaitDownloadReply(t, ch)
	f.objectGate.Store(nil)
	f.startAPI(t, false, true, "api-a")
	if _, e := os.Stat(source); !os.IsNotExist(e) {
		t.Fatal("proven crash orphan not reclaimed")
	}
	f.stopProcess(t, "api-a", false)
	root := f.nodes["api-a"].SpoolDir
	unknown := filepath.Join(root, "unknown-evidence")
	processPrivateFile(t, unknown, []byte("preserve"))
	f.expectStartupFailure(t, f.apiEnvironment(false, true, "api-a"))
	if b, e := os.ReadFile(unknown); e != nil || string(b) != "preserve" {
		t.Fatal("unknown evidence was cleared")
	}
	os.Remove(unknown)
	settings := f.apiEnvironment(false, true, "api-a")
	settings["IM_FILE_DOWNLOAD_OWNER_ID"] = freshFile().ID
	f.expectStartupFailure(t, settings)
	f.startAPI(t, false, true, "api-a")
	moved := root + "-replaced"
	if os.Rename(root, moved) != nil || os.Mkdir(root, 0700) != nil {
		t.Fatal("owned replacement setup failed")
	}
	processPrivateFile(t, filepath.Join(root, "unknown"), []byte("preserve replacement"))
	code, _, _ = f.request(t, "GET", "api-a", "/health/ready", "", "", nil, "")
	businessStatus(t, code, 503)
	f.stopProcess(t, "api-a", false)
	if b, e := os.ReadFile(filepath.Join(root, "unknown")); e != nil || string(b) != "preserve replacement" {
		t.Fatal("replaced directory evidence cleared")
	}
	f.assertEvidence(t)
}
func TestFileBusinessProcessRP09(t *testing.T) {
	f := newFileBusinessProcessFixture(t)
	f.startWorkers(t)
	f.startAPI(t, true, true, "api-a")
	body := []byte("actual audit recovery")
	id := f.upload(t, "api-a", directA, "集团审计.txt", "text/plain", body, true)
	f.sendFile(t, "api-a", directA, "direct", id, businessClientID())
	run(t, f.conn, `CREATE FUNCTION terminal_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'owned terminal fault'; END $$;CREATE TRIGGER terminal_fault BEFORE INSERT ON file_download_terminal_events FOR EACH ROW EXECUTE FUNCTION terminal_fault()`)
	x := awaitDownloadReply(t, f.asyncDownload(t, id, "admin", adminM, time.Now().Add(3*time.Second)))
	if x.err != nil || x.status != 200 || !bytes.Equal(x.body, body) {
		t.Fatal("actual audit-gap download absent")
	}
	var session string
	var bound time.Time
	if e := f.conn.QueryRow(context.Background(), `SELECT id::text,deadline FROM file_download_sessions WHERE file_id=$1`, id).Scan(&session, &bound); e != nil {
		t.Fatal(e)
	}
	code, _, _ := f.request(t, "GET", "api-a", "/api/v1/files/"+id+"/content", "admin", adminM, nil, "")
	if code == 200 {
		t.Fatal("unsettled download admitted")
	}
	run(t, f.conn, `DROP TRIGGER terminal_fault ON file_download_terminal_events`)
	if d := time.Until(bound) + 20*time.Millisecond; d > 0 {
		time.Sleep(d)
	}
	var jobs int
	f.conn.QueryRow(context.Background(), "SELECT count(*) FROM file_delete_jobs").Scan(&jobs)
	before := f.objectRequests.Load()
	f.restartRepair(t, true)
	f.waitTerminal(t, session)
	var ack bool
	var audits, terminals, afterJobs int
	var reason string
	e := f.conn.QueryRow(context.Background(), `SELECT s.audit_acked,s.reason_code,(SELECT count(*) FROM file_worker_audit_events WHERE download_session_id=s.id),(SELECT count(*) FROM file_download_terminal_events WHERE session_id=s.id),(SELECT count(*) FROM file_delete_jobs) FROM file_download_sessions s WHERE s.id=$1`, session).Scan(&ack, &reason, &audits, &terminals, &afterJobs)
	if e != nil || !ack || reason != "process_lost" || audits != 1 || terminals != 1 || jobs != afterJobs || f.objectRequests.Load() != before {
		t.Fatal("DB-only repair evidence differs", ack, reason, audits, terminals)
	}
	f.restartRepair(t, false)
	f.stopProcess(t, "repair", true)
	f.restartRepair(t, true)
	var again int
	if f.conn.QueryRow(context.Background(), `SELECT count(*) FROM file_worker_audit_events WHERE download_session_id=$1`, session).Scan(&again) != nil || again != 1 {
		t.Fatal("committed audit duplicated after killed repair")
	}
	f.assertDownloaded(t, id, body, "集团审计.txt")
	// Permanent database wire fault after a healthy repair startup must back off
	// and cancel promptly. No storage credential enters the repair process.
	u, _ := url.Parse(f.repairDSN)
	u.Host = f.dbRelay.listener.Addr().String()
	f.launch(t, "repair", "im-file-cleaner", map[string]string{"IM_DATABASE_URL": u.String()}, "--execute", "--repair-only")
	f.await(t, "repair", 35*time.Second, func(b []byte) bool { return bytes.Contains(b, []byte(`"status":"file_download_audit_repair"`)) })
	f.dbRelay.setFault(true)
	f.await(t, "repair", 8*time.Second, func(b []byte) bool { return bytes.Contains(b, []byte(`file_download_audit_repair_unavailable`)) })
	start := time.Now()
	f.stopProcess(t, "repair", false)
	if time.Since(start) > 2*time.Second {
		t.Fatal("repair database failure cancellation unbounded")
	}
	f.dbRelay.setFault(false)
	f.assertEvidence(t)
}
