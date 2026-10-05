package objectstore

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func testVersionDeleter(t *testing.T, h http.HandlerFunc) VersionDeleter {
	t.Helper()
	t.Setenv("IM_FILE_CLEANUP_S3_ACCESS_KEY", "cleanup-test-access")
	t.Setenv("IM_FILE_CLEANUP_S3_SECRET_KEY", "cleanup-test-secret")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	d, e := NewS3VersionDeleter(Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "test-files", CredentialSource: "cleanup_environment", PathStyle: true})
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func deleteXML(w http.ResponseWriter, code string, status int) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<Error><Code>%s</Code><Message>opaque fixture</Message></Error>`, code)
}
func listXML(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprint(w, `<ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`+content+`</ListVersionsResult>`)
}
func versionXML(key, version string) string {
	return `<Version><Key>` + key + `</Key><VersionId>` + version + `</VersionId><Size>1</Size></Version>`
}
func versionHead(w http.ResponseWriter, version, attempt string) {
	w.Header().Set("X-Amz-Version-Id", version)
	w.Header().Set("X-Amz-Meta-Attempt-Id", attempt)
	w.Header().Set("Content-Length", "1")
}
func TestFileDeleteVersionExact(t *testing.T) {
	var calls atomic.Int32
	d := testVersionDeleter(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "DELETE" || r.URL.Query().Get("versionId") != "sealed/+ opaque" || r.URL.Path != "/test-files/tenants/"+location.TenantID+"/files/"+location.FileID {
			t.Error("wrong fixed deletion", r.Method, r.URL)
		}
		w.WriteHeader(204)
	})
	for _, v := range []string{"", "null", "bad\nversion"} {
		if d.DeleteVersion(context.Background(), VersionRef{Location: location, VersionID: v}) == nil {
			t.Fatal("invalid delete accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid delete sent")
	}
	if e := d.DeleteVersion(context.Background(), VersionRef{Location: location, VersionID: "sealed/+ opaque"}); e != nil || calls.Load() != 1 {
		t.Fatal(e, calls.Load())
	}
}
func TestFileDeleteInventoryIsolation(t *testing.T) {
	key, _ := location.key()
	d := testVersionDeleter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			if r.URL.Path != "/test-files/"+key || r.URL.Query().Get("versionId") != "v1" {
				t.Error("neighbor or marker probed", r.URL)
			}
			versionHead(w, "v1", attemptID)
			return
		}
		if r.Method != "GET" || r.URL.Query().Get("prefix") != key || r.URL.Query().Get("max-keys") != "4" {
			t.Error(r.Method, r.URL)
		}
		listXML(w, `<IsTruncated>false</IsTruncated>`+versionXML(key, "v1")+versionXML(key+"-neighbor", "null")+`<DeleteMarker><Key>`+key+`</Key><VersionId>marker-1</VersionId></DeleteMarker>`)
	})
	p, e := d.ListVersions(context.Background(), location, VersionCursor{}, 4)
	if e != nil || !p.Exhausted || len(p.Versions) != 2 || p.Versions[0].AttemptID != attemptID || !p.Versions[1].DeleteMarker {
		t.Fatal(p, e)
	}
}
func TestFileDeletePagination(t *testing.T) {
	key, _ := location.key()
	t.Run("filtered page still advances", func(t *testing.T) {
		d := testVersionDeleter(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("key-marker") == "" {
				listXML(w, `<IsTruncated>true</IsTruncated><NextKeyMarker>`+key+`-neighbor</NextKeyMarker><NextVersionIdMarker>n1</NextVersionIdMarker>`+versionXML(key+"-neighbor", "n1"))
				return
			}
			if r.URL.Query().Get("key-marker") != key+"-neighbor" || r.URL.Query().Get("version-id-marker") != "n1" {
				t.Error(r.URL)
			}
			listXML(w, `<IsTruncated>false</IsTruncated>`)
		})
		p, e := d.ListVersions(context.Background(), location, VersionCursor{}, 1)
		if e != nil || p.Exhausted || len(p.Versions) != 0 || p.Next.KeyMarker != key+"-neighbor" {
			t.Fatal(p, e)
		}
		p, e = d.ListVersions(context.Background(), location, p.Next, 1)
		if e != nil || !p.Exhausted || p.Next != (VersionCursor{}) {
			t.Fatal(p, e)
		}
	})
	for _, kind := range []string{"repeat", "backward", "missing", "over-limit"} {
		t.Run(kind, func(t *testing.T) {
			cursor := VersionCursor{KeyMarker: key, VersionMarker: "v1"}
			d := testVersionDeleter(t, func(w http.ResponseWriter, r *http.Request) {
				xml := `<IsTruncated>true</IsTruncated><NextKeyMarker>` + key + `</NextKeyMarker><NextVersionIdMarker>v1</NextVersionIdMarker>`
				if kind == "backward" {
					xml = `<IsTruncated>true</IsTruncated><NextKeyMarker>other-key</NextKeyMarker><NextVersionIdMarker>v2</NextVersionIdMarker>`
				}
				if kind == "missing" {
					xml = `<IsTruncated>true</IsTruncated>`
				}
				if kind == "over-limit" {
					xml = `<IsTruncated>false</IsTruncated>` + versionXML(key+"-neighbor", "v1") + versionXML(key+"-neighbor", "null")
				}
				listXML(w, xml)
			})
			if _, e := d.ListVersions(context.Background(), location, cursor, 1); e == nil {
				t.Fatal("unsafe page accepted")
			}
		})
	}
}
func TestFileDeletePresenceFailures(t *testing.T) {
	key, _ := location.key()
	for _, kind := range []string{"403", "403NoSuchVersion", "NoSuchBucket", "NoSuchKey", "generic404", "marker", "mismatch", "NoSuchVersion", "inventoryFailure", "inventoryPresent"} {
		t.Run(kind, func(t *testing.T) {
			d := testVersionDeleter(t, func(w http.ResponseWriter, r *http.Request) {
				if _, ok := r.URL.Query()["versions"]; ok {
					if kind == "inventoryFailure" {
						deleteXML(w, "AccessDenied", 403)
						return
					}
					xml := `<IsTruncated>false</IsTruncated>`
					if kind == "inventoryPresent" {
						xml += versionXML(key, "v1")
					}
					listXML(w, xml)
					return
				}
				if r.URL.Query().Get("versionId") != "v1" {
					t.Error("missing version", r.URL)
				}
				if r.Method == "HEAD" {
					versionHead(w, "v1", attemptID)
					return
				}
				switch kind {
				case "403":
					deleteXML(w, "AccessDenied", 403)
				case "403NoSuchVersion":
					deleteXML(w, "NoSuchVersion", 403)
				case "NoSuchBucket":
					deleteXML(w, "NoSuchBucket", 404)
				case "NoSuchKey":
					deleteXML(w, "NoSuchKey", 404)
				case "generic404":
					w.WriteHeader(404)
				case "marker":
					w.Header().Set("X-Amz-Delete-Marker", "true")
					deleteXML(w, "MethodNotAllowed", 405)
				case "mismatch":
					versionHead(w, "other", attemptID)
					io.WriteString(w, "x")
				default:
					deleteXML(w, "NoSuchVersion", 404)
				}
			})
			p, e := d.ProbeVersion(context.Background(), VersionRef{Location: location, VersionID: "v1"})
			if kind == "NoSuchVersion" {
				if e != nil || p != VersionAbsent {
					t.Fatal(p, e)
				}
			} else if p != VersionUnknown || e == nil {
				t.Fatal("uncertain result became absence", p, e)
			}
		})
	}
}
func TestFileDeleteNoAutoRetry(t *testing.T) {
	for _, op := range []string{"list", "probe", "delete"} {
		t.Run(op, func(t *testing.T) {
			var calls atomic.Int32
			d := testVersionDeleter(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); deleteXML(w, "SlowDown", 503) })
			var e error
			switch op {
			case "list":
				_, e = d.ListVersions(context.Background(), location, VersionCursor{}, 100)
			case "probe":
				_, e = d.ProbeVersion(context.Background(), VersionRef{Location: location, VersionID: "v1"})
			case "delete":
				e = d.DeleteVersion(context.Background(), VersionRef{Location: location, VersionID: "v1"})
			}
			if e == nil || calls.Load() != 1 {
				t.Fatal(e, calls.Load())
			}
		})
	}
}
func TestFileDeleteCredentialsAreIndependent(t *testing.T) {
	t.Setenv("IM_FILE_S3_ACCESS_KEY", "upload-only")
	t.Setenv("IM_FILE_S3_SECRET_KEY", "upload-only")
	t.Setenv("IM_FILE_CLEANUP_S3_ACCESS_KEY", "")
	t.Setenv("IM_FILE_CLEANUP_S3_SECRET_KEY", "")
	c := Config{Endpoint: "http://127.0.0.1:1", Region: "us-east-1", Bucket: "test-files", CredentialSource: "cleanup_environment", PathStyle: true}
	if _, e := NewS3VersionDeleter(c); e == nil {
		t.Fatal("cleanup fell back to upload credentials")
	}
	t.Setenv("IM_FILE_CLEANUP_S3_ACCESS_KEY", "cleanup")
	t.Setenv("IM_FILE_CLEANUP_S3_SECRET_KEY", "cleanup")
	c.CredentialSource = "environment"
	if _, e := NewS3VersionDeleter(c); e == nil {
		t.Fatal("upload credential source accepted")
	}
}
func TestFileDeleteVersionPresenceAndMetadata(t *testing.T) {
	for _, attempt := range []string{attemptID, "invalid", ""} {
		t.Run(attempt, func(t *testing.T) {
			d := testVersionDeleter(t, func(w http.ResponseWriter, r *http.Request) {
				versionHead(w, "v1", attempt)
				if r.Header.Get("Range") != "bytes=0-0" {
					t.Error("probe downloaded whole object")
				}
				io.WriteString(w, "x")
			})
			p, e := d.ProbeVersion(context.Background(), VersionRef{Location: location, VersionID: "v1"})
			if attempt == attemptID {
				if e != nil || p != VersionPresent {
					t.Fatal(p, e)
				}
			} else if e == nil || p != VersionUnknown {
				t.Fatal(p, e)
			}
		})
	}
}
