package policystore_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/filetransfer"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFileAPIRealOIDC(t *testing.T) {
	c, _, _ := uploadFixture(t)
	ctx := context.Background()
	var schema string
	c.QueryRow(ctx, "SELECT current_schema()").Scan(&schema)
	dsn := processDatabaseURL(t, schema)
	env, token := fileOIDCIssuer(t, c)
	baseEnv := fileRuntimeEnv(t, dsn, t.TempDir()+"/api")
	baseEnv = append(baseEnv, env...)
	baseEnv = append(baseEnv, "IM_HTTP_ADDR=127.0.0.1:0", "IM_WEB_ENABLED=false", "IM_REALTIME_REDIS_URL=", "IM_REALTIME_STREAM=")
	binary := productionTestBinary(t, "im-api")
	disabled := startProductionAPI(t, binary, append(baseEnv, "IM_FILE_UPLOAD_ENABLED=false"))
	filePublicRequest(t, "GET", disabled, "/api/v1/file-upload-policy", token("ordinary"), targetM2, "", "", 404)
	api := startProductionAPI(t, binary, append(baseEnv, "IM_FILE_UPLOAD_ENABLED=true"))
	filePublicRequest(t, "GET", api, "/api/v1/file-upload-policy", "invalid", targetM2, "", "", 401)
	filePublicRequest(t, "GET", api, "/api/v1/file-upload-policy", token("unmapped"), targetM2, "", "", 401)
	policy := filePublicRequest(t, "GET", api, "/api/v1/file-upload-policy", token("ordinary"), targetM2, "", "", 200)
	if policy["enabled"] != true {
		t.Fatal(policy)
	}
	filePublicRequest(t, "GET", api, "/api/v1/admin/file-upload-policy", token("ordinary"), targetM2, "", "", 404)
	workerEnv := fileRuntimeEnv(t, dsn, t.TempDir()+"/worker")
	workerEnv = append(workerEnv, "IM_FILE_WORKER_ENABLED=true", "IM_FILE_WORKER_ID="+uploadOwner, "IM_FILE_QPDF_PATH="+os.Getenv("IM_TEST_QPDF_PATH"), "IM_FILE_CLAMD_SOCKET="+os.Getenv("IM_TEST_CLAMD_SOCKET"), "IM_FILE_SCANNER_MANIFEST="+os.Getenv("IM_TEST_SCANNER_MANIFEST"), "IM_FILE_S3_ACCESS_KEY="+os.Getenv("IM_TEST_FILE_WORKER_ACCESS_KEY"), "IM_FILE_S3_SECRET_KEY="+os.Getenv("IM_TEST_FILE_WORKER_SECRET_KEY"))
	worker := startFileWorkerProcess(t, productionTestBinary(t, "im-file-worker"), workerEnv)
	for index, body := range []string{"valid UTF-8 text", "X5O!P%@AP[4\\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*"} {
		request := fmt.Sprintf("00000000-0000-4000-8000-%012d", index+990)
		b, _ := json.Marshal(map[string]string{"upload_request_id": request, "original_filename": "测试.txt", "declared_media_type": "text/plain", "declared_size_bytes": fmt.Sprint(len(body))})
		out := filePublicRequest(t, "POST", api, "/api/v1/conversations/"+directA+"/files", token("ordinary"), targetM2, string(b), "application/json", 201)
		id := out["file_id"].(string)
		filePublicRequest(t, "GET", api, "/api/v1/files/"+id, token("admin"), adminM, "", "", 404)
		filePublicRequest(t, "GET", api, "/api/v1/files/"+id, token("ordinary"), targetM, "", "", 404)
		filePublicRequest(t, "GET", api, "/api/v1/files/"+id+"/content", token("ordinary"), targetM2, "", "", 405)
		filePublicRequest(t, "PUT", api, "/api/v1/files/"+id+"/content", token("ordinary"), targetM2, body, "application/octet-stream", 200)
		filePublicRequest(t, "PUT", api, "/api/v1/files/"+id+"/content", token("ordinary"), targetM2, "replacement", "application/octet-stream", 409)
		want := "ready"
		if index == 1 {
			want = "rejected"
		}
		deadline := time.Now().Add(20 * time.Second)
		for {
			out = filePublicRequest(t, "GET", api, "/api/v1/files/"+id, token("ordinary"), targetM2, "", "", 200)
			if out["state"] == want {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("actual worker result missing", out)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if len(out) != 9 {
			t.Fatal("unsafe status DTO", out)
		}
	}
	// A graceful stop must complete without leaving local spool files.
	if e := worker.Process.Signal(os.Interrupt); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- worker.Wait() }()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal("graceful worker exit", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("worker shutdown timeout")
	}
	for _, entry := range workerEnv {
		if strings.HasPrefix(entry, "IM_FILE_SPOOL_DIR=") {
			entries, e := os.ReadDir(strings.TrimPrefix(entry, "IM_FILE_SPOOL_DIR="))
			if e != nil || len(entries) != 0 {
				t.Fatal("worker spool not cleaned", entries, e)
			}
		}
	}
}
func TestFileAPIRealWorkerReadOnlyCredentials(t *testing.T) {
	objects := realTransferObjects(t)
	_ = objects
	t.Setenv("IM_FILE_S3_ACCESS_KEY", os.Getenv("IM_TEST_FILE_WORKER_ACCESS_KEY"))
	t.Setenv("IM_FILE_S3_SECRET_KEY", os.Getenv("IM_TEST_FILE_WORKER_SECRET_KEY"))
	s, e := objectstore.NewS3(objectstore.Config{Endpoint: os.Getenv("IM_TEST_S3_ENDPOINT"), Region: "us-east-1", Bucket: os.Getenv("IM_TEST_S3_BUCKET"), CredentialSource: "environment", PathStyle: true})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ValidateCapabilities(context.Background()); e != nil {
		t.Fatal("read-only capabilities failed", e)
	}
	m := files.Measurement{SizeBytes: 1, DetectedMediaType: "text/plain", SHA256: sha256.Sum256([]byte("x"))}
	if _, e = s.PutVersion(context.Background(), objectstore.Location{TenantID: tenantA, FileID: clientA}, clientA, m, strings.NewReader("x")); e == nil {
		t.Fatal("worker fixture credentials unexpectedly allow writes")
	}
}

func TestFileAPIRealWorkerCrashTakeover(t *testing.T) {
	c, repo, r := uploadFixture(t)
	objects := realTransferObjects(t)
	ctx := context.Background()
	var schema string
	c.QueryRow(ctx, "SELECT current_schema()").Scan(&schema)
	dsn := processDatabaseURL(t, schema)
	uploader, e := filetransfer.NewService(repo, objects, t.TempDir()+"/upload", uploadOwner)
	if e != nil {
		t.Fatal(e)
	}
	m, e := uploader.Upload(ctx, publisher(), r.File.ID, strings.NewReader("x"))
	if e != nil {
		t.Fatal(e)
	}
	target, _ := url.Parse(os.Getenv("IM_TEST_S3_ENDPOINT"))
	blocked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Query().Get("versionId") != "" {
			once.Do(func() { close(blocked) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		r.URL.Scheme = target.Scheme
		r.URL.Host = target.Host
		r.RequestURI = ""
		res, e := http.DefaultTransport.RoundTrip(r)
		if e != nil {
			w.WriteHeader(503)
			return
		}
		defer res.Body.Close()
		for k, v := range res.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(res.StatusCode)
		io.Copy(w, res.Body)
	}))
	defer proxy.Close()
	defer close(release)
	env := fileRuntimeEnv(t, dsn, t.TempDir()+"/worker")
	env = append(env, "IM_FILE_WORKER_ENABLED=true", "IM_FILE_WORKER_ID="+uploadOwner, "IM_FILE_QPDF_PATH="+os.Getenv("IM_TEST_QPDF_PATH"), "IM_FILE_CLAMD_SOCKET="+os.Getenv("IM_TEST_CLAMD_SOCKET"), "IM_FILE_SCANNER_MANIFEST="+os.Getenv("IM_TEST_SCANNER_MANIFEST"), "IM_FILE_S3_ACCESS_KEY="+os.Getenv("IM_TEST_FILE_WORKER_ACCESS_KEY"), "IM_FILE_S3_SECRET_KEY="+os.Getenv("IM_TEST_FILE_WORKER_SECRET_KEY"))
	binary := productionTestBinary(t, "im-file-worker")
	old := startFileWorkerProcess(t, binary, append(env, "IM_FILE_S3_ENDPOINT="+proxy.URL))
	select {
	case <-blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("actual worker never reached object read")
	}
	scanState(t, c, m.ID, files.StateScanning, 2)
	var first string
	c.QueryRow(ctx, "SELECT scan_job_id::text FROM file_objects WHERE id=$1", m.ID).Scan(&first)
	if e = old.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = old.Wait()
	run(t, c, "UPDATE file_scan_jobs SET lease_expires_at=clock_timestamp()+interval '1 second' WHERE id=$1", first)
	next := startFileWorkerProcess(t, binary, append(env, "IM_FILE_WORKER_ID="+clientB))
	deadline := time.Now().Add(25 * time.Second)
	for {
		var state files.State
		c.QueryRow(ctx, "SELECT state FROM file_objects WHERE id=$1", m.ID).Scan(&state)
		if state == files.StateReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("crash recovery did not ready", state)
		}
		time.Sleep(100 * time.Millisecond)
	}
	scanState(t, c, m.ID, files.StateReady, 5)
	var count int
	var latest string
	c.QueryRow(ctx, "SELECT count(*) FROM file_scan_jobs WHERE file_id=$1", m.ID).Scan(&count)
	c.QueryRow(ctx, "SELECT scan_job_id::text FROM file_objects WHERE id=$1", m.ID).Scan(&latest)
	if count != 2 || first == latest || objects.puts.Load() != 1 {
		t.Fatal(count, first, latest, objects.puts.Load())
	}
	next.Process.Signal(os.Interrupt)
	if e = next.Wait(); e != nil {
		t.Fatal(e)
	}
}
