//go:build nativeclientintegration && (darwin || linux)

package policystore_test

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"github.com/leileipei/Enterprise_IM/internal/receiptoutbox"
	"github.com/redis/go-redis/v9"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCrossGroupFilesDeviceFixture(t *testing.T) {
	for _, key := range []string{"IM_TEST_DATABASE_URL", "IM_TEST_REDIS_URL", "IM_TEST_MOBILE_CONFIG", "IM_TEST_MOBILE_FACTS", "IM_TEST_MOBILE_STOP"} {
		if os.Getenv(key) == "" {
			t.Fatalf("explicit cross-process fixture requires %s", key)
		}
	}
	ctx := context.Background()
	conn := db(t)
	seedDirectConversation(t, conn)
	runtimePolicyEdit(t, conn)
	grantPublisher(t, conn)
	rules := []policy.Rule{}
	for _, action := range []policy.Action{policy.ActionDirectoryView, policy.ActionStartChat, policy.ActionCreateGroup, policy.ActionSendMessage, policy.ActionFileDownload} {
		rules = append(rules, policy.Rule{ID: "group-native-" + string(action), TenantID: tenantA, Effect: policy.EffectAllow, Action: action, SourceOrganizationID: orgA, TargetOrganizationID: orgA2, Bidirectional: true, RequestedBy: adminA, ApprovedBy: adminA, Reason: "owned cross-process group", EffectiveFrom: time.Now().Add(-time.Hour), EffectiveTo: time.Now().Add(time.Hour)})
	}
	if _, e := (policystore.Service{DB: conn}).Publish(ctx, publisher(), 0, rules, "owned native group fixture"); e != nil {
		t.Fatal(e)
	}
	for _, key := range []string{"IM_TEST_S3_ENDPOINT", "IM_TEST_S3_BUCKET", "IM_FILE_S3_ACCESS_KEY", "IM_FILE_S3_SECRET_KEY", "IM_TEST_FILE_WORKER_ACCESS_KEY", "IM_TEST_FILE_WORKER_SECRET_KEY", "IM_TEST_FILE_DOWNLOAD_ACCESS_KEY", "IM_TEST_FILE_DOWNLOAD_SECRET_KEY", "IM_TEST_SCANNER_MANIFEST", "IM_TEST_CLAMD_SOCKET", "IM_TEST_QPDF_PATH", "IM_TEST_INTEGRATION_PROBE_VERSION"} {
		if os.Getenv(key) == "" {
			t.Fatal("missing mandatory real file dependency", key)
		}
	}
	for _, n := range []string{"000004_external_identities", "000026_native_oauth_rate"} {
		b, e := os.ReadFile("../../db/migrations/" + n + ".up.sql")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = conn.PgConn().Exec(ctx, string(b)).ReadAll(); e != nil {
			t.Fatal(e)
		}
	}
	var schema string
	if e := conn.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); e != nil {
		t.Fatal(e)
	}
	dsn := processDatabaseURL(t, schema)
	parsed, _ := url.Parse(dsn)
	run(t, conn, "GRANT DELETE ON native_oauth_request_windows TO "+pgx.Identifier{parsed.User.Username()}.Sanitize())
	options, e := redis.ParseURL(os.Getenv("IM_TEST_REDIS_URL"))
	if e != nil {
		t.Fatal(e)
	}
	cache := redis.NewClient(options)
	t.Cleanup(func() { _ = cache.Close() })
	stream := "enterprise-im:test:mobile:" + schema
	receipts := stream + ":receipts"
	t.Cleanup(func() {
		if e := cache.Del(ctx, stream, receipts, outbox.PublisherPresenceKey(stream), receiptoutbox.PublisherPresenceKey(receipts)).Err(); e != nil {
			t.Error("owned Redis cleanup failed")
		}
	})
	worker := fileProductCommand(productionTestBinary(t, "im-outbox-worker"), []string{"IM_DATABASE_URL=" + dsn, "IM_OUTBOX_REDIS_URL=" + os.Getenv("IM_TEST_REDIS_URL"), "IM_OUTBOX_STREAM=" + stream})
	crossRetainedRealtime(t, worker, func() bool { return cache.Exists(ctx, outbox.PublisherPresenceKey(stream)).Val() == 1 })
	receiptWorker := fileProductCommand(productionTestBinary(t, "im-receipt-worker"), []string{"IM_DATABASE_URL=" + dsn, "IM_RECEIPT_REDIS_URL=" + os.Getenv("IM_TEST_REDIS_URL"), "IM_RECEIPT_STREAM=" + receipts})
	crossRetainedRealtime(t, receiptWorker, func() bool { return cache.Exists(ctx, receiptoutbox.PublisherPresenceKey(receipts)).Val() == 1 })
	// Real broker refresh uses the platform-bound synthetic identity issuer.
	env, refresh := crossRefreshIssuer(t, conn)
	upstream, _ := url.Parse(crossRetainedAPI(t, productionTestBinary(t, "im-api"), append(append(fileRuntimeEnv(t, dsn, t.TempDir()+"/upload"), env...), "IM_HTTP_ADDR=127.0.0.1:0", "IM_RECEIPTS_ENABLED=true", "IM_RECEIPT_WRITES_PER_SECOND=100", "IM_REALTIME_REDIS_URL="+os.Getenv("IM_TEST_REDIS_URL"), "IM_REALTIME_STREAM="+stream, "IM_RECEIPT_STREAM="+receipts, "IM_FILE_UPLOAD_ENABLED=true", "IM_FILE_BUSINESS_ENABLED=true", "IM_FILE_DOWNLOAD_S3_ACCESS_KEY="+os.Getenv("IM_TEST_FILE_DOWNLOAD_ACCESS_KEY"), "IM_FILE_DOWNLOAD_S3_SECRET_KEY="+os.Getenv("IM_TEST_FILE_DOWNLOAD_SECRET_KEY"), "IM_FILE_DOWNLOAD_SPOOL_DIR="+t.TempDir()+"/download", "IM_FILE_DOWNLOAD_OWNER_ID="+freshFile().ID, "IM_FILE_READ_PROBE_VERSION_ID="+os.Getenv("IM_TEST_INTEGRATION_PROBE_VERSION"))))

	crossRetainedFileWorker(t, productionTestBinary(t, "im-file-worker"), append(fileRuntimeEnv(t, dsn, t.TempDir()+"/worker"), "IM_FILE_WORKER_ENABLED=true", "IM_FILE_WORKER_ID="+freshFile().ID, "IM_FILE_QPDF_PATH="+os.Getenv("IM_TEST_QPDF_PATH"), "IM_FILE_CLAMD_SOCKET="+os.Getenv("IM_TEST_CLAMD_SOCKET"), "IM_FILE_SCANNER_MANIFEST="+os.Getenv("IM_TEST_SCANNER_MANIFEST"), "IM_FILE_S3_ACCESS_KEY="+os.Getenv("IM_TEST_FILE_WORKER_ACCESS_KEY"), "IM_FILE_S3_SECRET_KEY="+os.Getenv("IM_TEST_FILE_WORKER_SECRET_KEY")))

	repair := fileProductCommand(productionTestBinary(t, "im-file-cleaner"), []string{"IM_DATABASE_URL=" + dsn})
	repair.Args = append(repair.Args, "--execute", "--repair-only")
	crossRetainedStart(t, "repair", repair, func(log []byte) bool { return strings.Contains(string(log), "file_download_audit_repair") })

	var mu sync.Mutex
	events := []map[string]any{}
	eventPath := filepath.Join(filepath.Dir(os.Getenv("IM_TEST_MOBILE_FACTS")), "backend-api-events.json")
	saveEvents := func() {
		b, e := json.MarshalIndent(events, "", "  ")
		if e == nil {
			e = os.WriteFile(eventPath+".new", b, 0600)
		}
		if e == nil {
			e = os.Rename(eventPath+".new", eventPath)
		}
		if e != nil {
			t.Error("cross event persistence failed", e)
		}
	}
	reverse := httputil.NewSingleHostReverseProxy(upstream)
	reverse.ModifyResponse = func(r *http.Response) error {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, map[string]any{"n": len(events) + 1, "method": r.Request.Method, "path": r.Request.URL.Path, "status": r.StatusCode, "device": r.Request.Header.Get("X-IM-Device-ID"), "device_headers": r.Request.Header.Get("X-IM-Device-ID") != "" && r.Request.Header.Get("X-IM-Device-Token") != ""})
		saveEvents()
		return nil
	}
	proxy, ca := nativeProbeTLS(t, reverse)
	actors := map[string]any{}
	for role, platform := range map[string]string{"alice": "android", "bob_ios": "ios", "bob_android": "android"} {
		user, membership := personA, targetM
		if role == "alice" {
			user, membership = adminA, adminM
		}
		actors[role] = map[string]any{"platform": platform, "user": user, "membership": membership, "refresh": refresh[role]}
	}
	input := map[string]any{"origin": proxy.URL, "ca_pem": string(ca), "tenant": tenantA, "conversation": directA, "schema": schema, "actors": actors}
	b, e := json.Marshal(input)
	if e != nil {
		t.Fatal(e)
	}
	path := os.Getenv("IM_TEST_MOBILE_CONFIG")
	if e = os.WriteFile(path+".tmp", b, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(path+".tmp", path); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(18 * time.Minute)
	for {
		if _, e = os.Stat(os.Getenv("IM_TEST_MOBILE_STOP")); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cross-process scenario deadline")
		}
		time.Sleep(100 * time.Millisecond)
	}

	var messages, devices, delivered, reads, userReads, groups, members, organizations, readyFiles, attachments, scans, downloads, acked, revoked, minSeq, maxSeq, clients int
	queries := []struct {
		sql  string
		dest []any
	}{
		{"SELECT count(*),COALESCE(min(seq),0),COALESCE(max(seq),0),count(DISTINCT client_msg_id) FROM messages", []any{&messages, &minSeq, &maxSeq, &clients}},
		{"SELECT count(*),count(*) FILTER(WHERE revoked_at IS NOT NULL) FROM user_devices", []any{&devices, &revoked}},
		{"SELECT count(*),count(*) FILTER(WHERE read_at IS NOT NULL) FROM message_device_receipts", []any{&delivered, &reads}},
		{"SELECT count(*) FROM message_user_receipts WHERE read_at IS NOT NULL", []any{&userReads}},
		{"SELECT count(*) FROM conversations WHERE kind='group'", []any{&groups}},
		{"SELECT count(*),count(DISTINCT source_organization_id) FROM conversation_membership_intervals WHERE leave_seq IS NULL", []any{&members, &organizations}},
		{"SELECT count(*) FROM file_objects WHERE state='ready'", []any{&readyFiles}},
		{"SELECT count(*) FROM message_attachments", []any{&attachments}},
		{"SELECT count(*) FROM file_scan_jobs WHERE status='completed'", []any{&scans}},
		{"SELECT count(*),count(*) FILTER(WHERE audit_acked) FROM file_download_sessions WHERE phase='completed'", []any{&downloads, &acked}},
	}
	for _, q := range queries {
		if e = conn.QueryRow(ctx, q.sql).Scan(q.dest...); e != nil {
			t.Fatal(e)
		}
	}
	facts := map[string]int{"messages": messages, "devices": devices, "delivered": delivered, "read": reads, "user_read": userReads, "groups": groups, "memberships": members, "organizations": organizations, "files_ready": readyFiles, "attachments": attachments, "scan_completed": scans, "downloads_completed": downloads, "downloads_acked": acked, "revoked": revoked, "min_seq": minSeq, "max_seq": maxSeq, "distinct_client_ids": clients}
	b, _ = json.MarshalIndent(facts, "", "  ")
	if e = os.WriteFile(os.Getenv("IM_TEST_MOBILE_FACTS"), b, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(filepath.Dir(path), "scenario-done")); e != nil {
		t.Fatal("group scenario incomplete")
	}
	expectedDownloads, expectedRevoked, expectedDelivered, expectedRead := 2, 1, 6, 6
	if os.Getenv("IM_TEST_CROSS_SCENARIO") == "retention-restart" {
		expectedDownloads, expectedRevoked, expectedDelivered, expectedRead = 4, 0, 7, 7
	}
	if messages != 4 || devices != 3 || delivered != expectedDelivered || reads != expectedRead || userReads != 4 || groups != 1 || members != 2 || organizations != 2 || readyFiles != 1 || attachments != 1 || scans != 1 || downloads != expectedDownloads || acked != expectedDownloads || revoked != expectedRevoked || minSeq != 1 || maxSeq != 4 || clients != 4 {
		t.Fatalf("group/file/device final facts mismatch %s", b)
	}
	t.Log("CROSS_GROUP_FILES_DEVICES_FINAL independent database facts passed")
}

func TestCrossRetainedProductLogSurvivesTestCleanup(t *testing.T) {
	root := t.TempDir()
	resolved, e := filepath.EvalSymlinks(root)
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("IM_TEST_MOBILE_FACTS", filepath.Join(resolved, "facts.json"))
	t.Run("owned-writer", func(t *testing.T) {
		f := crossRetainedLog(t, "diagnostic")
		t.Cleanup(func() { f.Close() })
		if _, e := f.WriteString("actual product diagnostic\n"); e != nil {
			t.Fatal(e)
		}
	})
	data, e := os.ReadFile(filepath.Join(resolved, "product", "diagnostic.log"))
	if e != nil || string(data) != "actual product diagnostic\n" {
		t.Fatalf("diagnostic lost after test cleanup: %q %v", data, e)
	}
}

// The fixture output is outside Go's automatic temporary-directory cleanup.
// All callers use an owned, fresh output; names never contain credentials.
func crossRetainedLog(t *testing.T, role string) *os.File {
	t.Helper()
	if !regexp.MustCompile(`^[a-z-]+$`).MatchString(role) {
		t.Fatal("invalid retained log role")
	}
	parent := filepath.Dir(os.Getenv("IM_TEST_MOBILE_FACTS"))
	real, e := filepath.EvalSymlinks(parent)
	if e != nil || !filepath.IsAbs(parent) || real != parent {
		t.Fatal("unverified diagnostic parent")
	}
	dir := filepath.Join(parent, "product")
	if e = os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	info, e := os.Lstat(dir)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		t.Fatal("unsafe diagnostic directory")
	}
	f, e := os.OpenFile(filepath.Join(dir, role+".log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	return f
}
func crossRetainedStart(t *testing.T, role string, cmd *exec.Cmd, ready func([]byte) bool) {
	t.Helper()
	log := crossRetainedLog(t, role)
	t.Cleanup(func() { log.Close() })
	cmd.Stdout = log
	cmd.Stderr = log
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	proof := trackFileProductProcess(t, cmd)
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		data, e := os.ReadFile(log.Name())
		if e == nil && ready(data) {
			if e = proof.Ready(); e != nil {
				t.Fatal(e)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	data, _ := os.ReadFile(log.Name())
	t.Fatalf("product %s readiness failed; retained log: %s", role, data)
}
func crossRetainedRealtime(t *testing.T, cmd *exec.Cmd, ready func() bool) {
	t.Helper()
	crossRetainedStart(t, filepath.Base(cmd.Path), cmd, func([]byte) bool { return ready() })
}
func crossRetainedFileWorker(t *testing.T, binary string, env []string) {
	t.Helper()
	crossRetainedStart(t, "file-worker", fileProductCommand(binary, env), func(log []byte) bool { return strings.Contains(string(log), "file worker started") })
}
func crossRetainedAPI(t *testing.T, binary string, env []string) string {
	t.Helper()
	address := ""
	client := http.Client{Timeout: time.Second}
	crossRetainedStart(t, "api", fileProductCommand(binary, env), func(log []byte) bool {
		a := productionAPIAddress(log)
		if a == "" {
			return false
		}
		response, e := client.Get("http://" + a + "/health/ready")
		if e != nil {
			return false
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			return false
		}
		address = "http://" + a
		return true
	})
	return address
}
