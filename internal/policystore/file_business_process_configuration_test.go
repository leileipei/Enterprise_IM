package policystore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

func TestFileBusinessProcessRP01(t *testing.T) {
	f := newFileBusinessProcessFixture(t)
	for _, u := range []bool{false, true} {
		for _, b := range []bool{false, true} {
			f.startAPI(t, u, b, "api-a")
			code, _, raw := f.request(t, "GET", "api-a", "/api/v1/file-capabilities", "admin", adminM, nil, "")
			businessStatus(t, code, 200)
			caps := businessJSON(t, raw)
			for key, want := range map[string]bool{"upload_enabled": u, "message_send_enabled": b, "download_enabled": b, "filename_search_enabled": b} {
				if caps[key] != want {
					t.Fatal("capability differs from assembly", key)
				}
			}
			path := "/api/v1/files/" + freshFile().ID + "/content"
			code, _, _ = f.request(t, "GET", "api-a", path, "admin", adminM, nil, "")
			want := 503
			if b {
				want = 404
			}
			businessStatus(t, code, want)
			for _, m := range []string{"HEAD", "OPTIONS", "POST"} {
				code, h, _ := f.request(t, m, "api-a", path, "admin", adminM, nil, "")
				businessStatus(t, code, 405)
				allow := "GET"
				if u {
					allow += ", PUT"
				}
				if h.Get("Allow") != allow {
					t.Fatal("method allow differs from assembly")
				}
			}
			if u {
				f.upload(t, "api-a", directA, "switch.txt", "text/plain", []byte("x"), false)
			} else {
				code, h, _ := f.request(t, "PUT", "api-a", path, "admin", adminM, []byte("x"), "application/octet-stream")
				businessStatus(t, code, 405)
				if h.Get("Allow") != "GET" {
					t.Fatal("closed PUT exposed upload method")
				}
				if b {
					code, _, raw = f.request(t, "POST", "api-a", "/api/v1/conversations/"+directA+"/files", "admin", adminM, []byte("{}"), "application/json")
					businessStatus(t, code, 503)
					if !bytes.Contains(raw, []byte("file_service_unavailable")) {
						t.Fatal("status-only reservation changed original dependency DTO")
					}
				}
			}
			for _, p := range []string{"/api/v1/files/search?q=ab", "/api/v1/conversations/" + directA + "/files/search?q=ab", "/api/v1/groups/" + groupA + "/files/search?q=ab"} {
				code, _, raw = f.request(t, "GET", "api-a", p, "admin", adminM, nil, "")
				if !b {
					businessStatus(t, code, 503)
					if !bytes.Contains(raw, []byte("file_search_unavailable")) {
						t.Fatal("closed search route swallowed")
					}
				} else if code != 200 && code != 404 {
					t.Fatal("open search route swallowed", code)
				}
			}
			body, _ := json.Marshal(map[string]string{"client_msg_id": clientUUIDv7(time.Now(), 12001), "message_type": "file", "file_id": freshFile().ID})
			code, _, raw = f.request(t, "POST", "api-a", "/api/v1/conversations/"+directA+"/messages", "admin", adminM, body, "application/json")
			if !b {
				businessStatus(t, code, 503)
				if !bytes.Contains(raw, []byte("file_message_unavailable")) {
					t.Fatal("closed file send mapping changed")
				}
			} else if code == 503 {
				t.Fatal("business send remained closed")
			}
			f.stopProcess(t, "api-a", false)
		}
	}
	// Malicious disabled download settings must not become dependencies.
	settings := f.apiEnvironment(false, false, "api-a")
	settings["IM_FILE_DOWNLOAD_SPOOL_DIR"] = filepath.Join(f.privateRoot, "disabled-must-not-exist")
	settings["IM_FILE_DOWNLOAD_OWNER_ID"] = "malformed"
	settings["IM_FILE_READ_PROBE_VERSION_ID"] = "null"
	settings["IM_FILE_DOWNLOAD_S3_ACCESS_KEY"] = "bad"
	settings["IM_FILE_DOWNLOAD_S3_SECRET_KEY"] = "bad"
	f.launch(t, "api-a", "im-api", settings)
	f.await(t, "api-a", 5*time.Second, func(b []byte) bool { return productionAPIAddress(b) != "" })
	if _, e := os.Stat(settings["IM_FILE_DOWNLOAD_SPOOL_DIR"]); !os.IsNotExist(e) {
		t.Fatal("disabled business created download directory")
	}
	f.stopProcess(t, "api-a", false)
	f.assertEvidence(t)
}
func TestFileBusinessProcessRP02(t *testing.T) {
	for _, scenario := range []string{"bad_flag", "space_flag", "missing_oidc", "missing_reader", "shared_upload_key", "bad_owner", "bad_probe", "bad_spool", "missing_schema", "bad_constraint", "bad_bucket", "bad_privilege", "symlink", "wide_mode", "wrong_probe_body"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFileBusinessProcessFixture(t)
			e := f.apiEnvironment(false, true, "api-a")
			switch scenario {
			case "bad_flag":
				e["IM_FILE_BUSINESS_ENABLED"] = "TRUE"
			case "space_flag":
				e["IM_FILE_BUSINESS_ENABLED"] = " true"
			case "missing_oidc":
				e["IM_OIDC_ENABLED"] = "false"
			case "missing_reader":
				delete(e, "IM_FILE_DOWNLOAD_S3_SECRET_KEY")
			case "shared_upload_key":
				e["IM_FILE_S3_ACCESS_KEY"] = e["IM_FILE_DOWNLOAD_S3_ACCESS_KEY"]
			case "bad_owner":
				e["IM_FILE_DOWNLOAD_OWNER_ID"] = "00000000-0000-0000-0000-000000000000"
			case "bad_probe":
				e["IM_FILE_READ_PROBE_VERSION_ID"] = "null"
			case "bad_spool":
				e["IM_FILE_DOWNLOAD_SPOOL_DIR"] = "relative"
			case "missing_schema":
				run(t, f.conn, "ALTER TABLE file_download_sessions DROP COLUMN expected_bytes CASCADE")
			case "bad_constraint":
				run(t, f.conn, "ALTER TABLE file_worker_audit_events DROP CONSTRAINT file_worker_download_terminal_origin")
			case "bad_bucket":
				e["IM_FILE_S3_BUCKET"] = os.Getenv("IM_TEST_S3_POLICY_BUCKET")
			case "bad_privilege":
				run(t, f.conn, "REVOKE INSERT ON file_download_sessions FROM "+f.apiRole)
			case "symlink":
				target := filepath.Join(f.privateRoot, "source")
				if os.Mkdir(target, 0700) != nil || os.Symlink(target, e["IM_FILE_DOWNLOAD_SPOOL_DIR"]) != nil {
					t.Fatal("symlink fixture unavailable")
				}
			case "wide_mode":
				if os.Mkdir(e["IM_FILE_DOWNLOAD_SPOOL_DIR"], 0700) != nil || os.Chmod(e["IM_FILE_DOWNLOAD_SPOOL_DIR"], 0755) != nil {
					t.Fatal("wide directory fixture unavailable")
				}
				info, err := os.Stat(e["IM_FILE_DOWNLOAD_SPOOL_DIR"])
				if err != nil || info.Mode().Perm() != 0755 {
					t.Fatal("wide directory fixture mode not applied")
				}
			case "wrong_probe_body":
				client := f.s3Client("BOOTSTRAP", os.Getenv("IM_TEST_S3_ENDPOINT"))
				p, err := client.PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String(os.Getenv("IM_TEST_S3_BUCKET")), Key: aws.String("_im_runtime/read-probe/v1"), Body: bytes.NewReader([]byte("wrong\n"))})
				if err != nil {
					t.Fatal("negative probe fixture failed")
				}
				e["IM_FILE_READ_PROBE_VERSION_ID"] = aws.ToString(p.VersionId)
			}
			f.expectStartupFailure(t, e)
			f.assertEvidence(t)
		})
	}
}
func TestFileBusinessProcessRP12(t *testing.T) {
	f := newFileBusinessProcessFixture(t)
	f.startWorkers(t)
	f.startAPI(t, true, true, "api-a")
	before := f.objectWrites.Load()
	f.objectFault.Store(1)
	started := time.Now()
	code, _, _ := f.request(t, "GET", "api-a", "/health/ready", "", "", nil, "")
	businessStatus(t, code, 503)
	if time.Since(started) > 2500*time.Millisecond {
		t.Fatal("readiness exceeded shared budget")
	}
	code, _, _ = f.request(t, "GET", "api-a", "/health/live", "", "", nil, "")
	businessStatus(t, code, 200)
	f.objectFault.Store(0)
	code, _, _ = f.request(t, "GET", "api-a", "/health/ready", "", "", nil, "")
	businessStatus(t, code, 200)
	if f.objectWrites.Load() != before {
		t.Fatal("readiness mutated objects")
	}
	f.dbRelay.setFault(true)
	started = time.Now()
	code, _, _ = f.request(t, "GET", "api-a", "/health/ready", "", "", nil, "")
	businessStatus(t, code, 503)
	if time.Since(started) > 2500*time.Millisecond {
		t.Fatal("database readiness renewed shared deadline")
	}
	code, _, _ = f.request(t, "GET", "api-a", "/health/live", "", "", nil, "")
	businessStatus(t, code, 200)
	f.dbRelay.setFault(false)
	code, _, _ = f.request(t, "GET", "api-a", "/health/ready", "", "", nil, "")
	businessStatus(t, code, 200)
	code, _, raw := f.request(t, "GET", "api-a", "/api/v1/file-capabilities", "admin", adminM, nil, "")
	businessStatus(t, code, 200)
	if businessJSON(t, raw)["download_enabled"] != true {
		t.Fatal("transient outage changed assembled capability")
	}
	f.stopProcess(t, "scanner", false)
	id := f.upload(t, "api-a", directA, "pending.txt", "text/plain", []byte("pending"), false)
	time.Sleep(1500 * time.Millisecond)
	code, _, raw = f.request(t, "GET", "api-a", "/api/v1/files/"+id, "admin", adminM, nil, "")
	businessStatus(t, code, 200)
	if businessJSON(t, raw)["state"] != "uploaded" {
		t.Fatal("stopped worker produced false ready")
	}
	f.assertEvidence(t)
}
func businessAccessDenied(t *testing.T, e error) {
	t.Helper()
	var api smithy.APIError
	if e == nil || !errors.As(e, &api) || (api.ErrorCode() != "AccessDenied" && api.ErrorCode() != "AllAccessDisabled") {
		t.Fatal("real IAM did not deny forbidden operation")
	}
}
func TestFileBusinessProcessRP13(t *testing.T) {
	f := newFileBusinessProcessFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bucket := aws.String(os.Getenv("IM_TEST_S3_BUCKET"))
	key := aws.String("_im_runtime/read-probe/v1")
	for _, role := range []string{"UPLOAD", "WORKER", "DOWNLOAD", "CLEANUP"} {
		c := f.s3Client(role, os.Getenv("IM_TEST_S3_ENDPOINT"))
		o, e := c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: key, VersionId: aws.String(f.probeVersion)})
		if e != nil {
			t.Fatal("IAM fixed version read denied", role)
		}
		b, e := io.ReadAll(o.Body)
		o.Body.Close()
		if e != nil || string(b) != "enterprise-im-file-read-probe-v1\n" {
			t.Fatal("IAM fixed read differed")
		}
		_, e = c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(os.Getenv("IM_TEST_S3_POLICY_BUCKET"))})
		businessAccessDenied(t, e)
	}
	for _, role := range []string{"WORKER", "DOWNLOAD", "CLEANUP"} {
		_, e := f.s3Client(role, os.Getenv("IM_TEST_S3_ENDPOINT")).PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: aws.String("_im_runtime/deny-" + processRandom(t)), Body: stringsReader("x")})
		businessAccessDenied(t, e)
	}
	for _, role := range []string{"UPLOAD", "WORKER", "DOWNLOAD"} {
		_, e := f.s3Client(role, os.Getenv("IM_TEST_S3_ENDPOINT")).DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: key, VersionId: aws.String(f.probeVersion)})
		businessAccessDenied(t, e)
	}
	_, e := f.s3Client("DOWNLOAD", os.Getenv("IM_TEST_S3_ENDPOINT")).ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: bucket})
	businessAccessDenied(t, e)
	upload := f.s3Client("UPLOAD", os.Getenv("IM_TEST_S3_ENDPOINT"))
	owned := aws.String("_im_runtime/owned-" + processRandom(t))
	put, e := upload.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: owned, Body: stringsReader("owned")})
	if e != nil {
		t.Fatal("upload IAM write denied")
	}
	_, e = f.s3Client("CLEANUP", os.Getenv("IM_TEST_S3_ENDPOINT")).DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: owned, VersionId: put.VersionId})
	if e != nil {
		t.Fatal("cleanup exact owned version denied")
	}
	res, e := http.Get(os.Getenv("IM_TEST_S3_ENDPOINT") + "/" + *bucket + "/" + *key + "?versionId=" + f.probeVersion)
	if e != nil {
		t.Fatal("anonymous IAM probe transport unavailable")
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("anonymous read not denied")
	}
	_, e = f.s3Client("CLEANUP", os.Getenv("IM_TEST_S3_ENDPOINT")).DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: owned})
	businessAccessDenied(t, e)
	f.startAPI(t, false, true, "api-a")
	n := f.nodes["api-b"]
	n.SpoolDir = f.nodes["api-a"].SpoolDir
	n.OwnerID = f.nodes["api-a"].OwnerID
	f.nodes["api-b"] = n
	f.expectStartupFailure(t, f.apiEnvironment(false, true, "api-b"))
	f.proveLinuxDownloadUID(t)
	f.assertEvidence(t)
}
func stringsReader(s string) *bytes.Reader { return bytes.NewReader([]byte(s)) }
