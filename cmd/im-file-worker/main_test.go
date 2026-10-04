package main

import (
	"context"
	"testing"
)

func workerEnv(v map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { s, ok := v[k]; return s, ok }
}
func TestFileWorkerConfig(t *testing.T) {
	c, e := ConfigFromEnv(workerEnv(nil))
	if e != nil || c.Enabled || c.ScanConcurrency != 1 {
		t.Fatal(c, e)
	}
	if e = run(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	values := map[string]string{"IM_FILE_WORKER_ENABLED": "true", "IM_DATABASE_URL": "postgres://localhost/test", "IM_FILE_WORKER_ID": "00000000-0000-4000-8000-000000000001", "IM_FILE_SPOOL_DIR": t.TempDir() + "/private", "IM_FILE_S3_ENDPOINT": "http://127.0.0.1:9000", "IM_FILE_S3_REGION": "us-east-1", "IM_FILE_S3_BUCKET": "private-files", "IM_FILE_S3_CREDENTIAL_SOURCE": "environment", "IM_FILE_S3_ACCESS_KEY": "fixture", "IM_FILE_S3_SECRET_KEY": "fixture", "IM_FILE_QPDF_PATH": "/controlled/qpdf", "IM_FILE_CLAMD_SOCKET": "/controlled/clamd.sock", "IM_FILE_SCANNER_MANIFEST": "/controlled/manifest.json"}
	c, e = ConfigFromEnv(workerEnv(values))
	if e != nil || !c.Enabled || c.ScanConcurrency != 1 {
		t.Fatal(c, e)
	}
	for _, key := range []string{"IM_DATABASE_URL", "IM_FILE_WORKER_ID", "IM_FILE_SPOOL_DIR", "IM_FILE_S3_ENDPOINT", "IM_FILE_S3_ACCESS_KEY", "IM_FILE_QPDF_PATH", "IM_FILE_CLAMD_SOCKET", "IM_FILE_SCANNER_MANIFEST"} {
		old := values[key]
		delete(values, key)
		if _, e = ConfigFromEnv(workerEnv(values)); e == nil {
			t.Fatal("missing config", key)
		}
		values[key] = old
	}
	for _, bad := range []struct{ k, v string }{{"IM_FILE_WORKER_ENABLED", "yes"}, {"IM_FILE_WORKER_ID", "not-uuid"}, {"IM_FILE_SCAN_CONCURRENCY", "2"}, {"IM_FILE_S3_PATH_STYLE", "yes"}, {"IM_FILE_SPOOL_DIR", "relative"}} {
		old := values[bad.k]
		values[bad.k] = bad.v
		if _, e = ConfigFromEnv(workerEnv(values)); e == nil {
			t.Fatal(bad)
		}
		values[bad.k] = old
	}
}
