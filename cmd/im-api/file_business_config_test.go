package main

import (
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"strings"
	"testing"
)

func businessSettings(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"IM_FILE_BUSINESS_ENABLED": "true", "IM_DATABASE_URL": "postgres://localhost/test",
		"IM_FILE_S3_ENDPOINT": "http://127.0.0.1:9000", "IM_FILE_S3_REGION": "us-east-1", "IM_FILE_S3_BUCKET": "private-files", "IM_FILE_S3_PATH_STYLE": "true",
		"IM_FILE_DOWNLOAD_S3_ACCESS_KEY": "reader", "IM_FILE_DOWNLOAD_S3_SECRET_KEY": "reader-secret",
		"IM_FILE_DOWNLOAD_SPOOL_DIR": t.TempDir() + "/download", "IM_FILE_DOWNLOAD_OWNER_ID": "11111111-1111-4111-8111-111111111111", "IM_FILE_READ_PROBE_VERSION_ID": "version-1",
	}
}

// Catches accepting incomplete or malformed enabled configurations before listen.
func TestFileBusinessConfigStrict(t *testing.T) {
	valid := businessSettings(t)
	c, e := fileBusinessConfigFromEnv(env(valid), true)
	if e != nil || !c.Enabled || !c.Objects.PathStyle || c.Objects.CredentialSource != "download_environment" || c.OwnerID != valid["IM_FILE_DOWNLOAD_OWNER_ID"] || c.ProbeVersionID != "version-1" || c.SpoolDir != valid["IM_FILE_DOWNLOAD_SPOOL_DIR"] {
		t.Fatal("valid read-only configuration refused", e)
	}
	if _, e = fileBusinessConfigFromEnv(env(valid), false); e == nil {
		t.Fatal("OIDC prerequisite bypassed")
	}
	for _, key := range []string{"IM_DATABASE_URL", "IM_FILE_S3_ENDPOINT", "IM_FILE_S3_REGION", "IM_FILE_S3_BUCKET", "IM_FILE_DOWNLOAD_S3_ACCESS_KEY", "IM_FILE_DOWNLOAD_S3_SECRET_KEY", "IM_FILE_DOWNLOAD_SPOOL_DIR", "IM_FILE_DOWNLOAD_OWNER_ID", "IM_FILE_READ_PROBE_VERSION_ID"} {
		t.Run("missing_"+key, func(t *testing.T) {
			m := businessSettings(t)
			delete(m, key)
			if _, e := fileBusinessConfigFromEnv(env(m), true); e == nil {
				t.Fatal("missing dependency accepted")
			}
		})
	}
	for _, tc := range []struct{ name, key, value string }{
		{"upper_flag", "IM_FILE_BUSINESS_ENABLED", "TRUE"}, {"spaces_flag", "IM_FILE_BUSINESS_ENABLED", " true"}, {"bad_flag", "IM_FILE_BUSINESS_ENABLED", "1"},
		{"relative_spool", "IM_FILE_DOWNLOAD_SPOOL_DIR", "downloads"}, {"zero_owner", "IM_FILE_DOWNLOAD_OWNER_ID", "00000000-0000-0000-0000-000000000000"}, {"uppercase_owner", "IM_FILE_DOWNLOAD_OWNER_ID", "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"}, {"bad_owner", "IM_FILE_DOWNLOAD_OWNER_ID", "node-a"},
		{"null_version", "IM_FILE_READ_PROBE_VERSION_ID", "null"}, {"long_version", "IM_FILE_READ_PROBE_VERSION_ID", strings.Repeat("v", 1025)}, {"invalid_utf8", "IM_FILE_READ_PROBE_VERSION_ID", string([]byte{255})}, {"control_version", "IM_FILE_READ_PROBE_VERSION_ID", "v\n"},
		{"bad_path_style", "IM_FILE_S3_PATH_STYLE", "TRUE"}, {"remote_plaintext", "IM_FILE_S3_ENDPOINT", "http://example.invalid"}, {"endpoint_credentials", "IM_FILE_S3_ENDPOINT", "https://secret@example.invalid"}, {"bad_region", "IM_FILE_S3_REGION", "a b"}, {"bad_bucket", "IM_FILE_S3_BUCKET", "UPPER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := businessSettings(t)
			m[tc.key] = tc.value
			if _, e := fileBusinessConfigFromEnv(env(m), true); e == nil {
				t.Fatal("malformed configuration accepted")
			} else if e.Error() != "invalid_file_business_configuration" {
				t.Fatal("configuration error must be redacted")
			}
		})
	}
}

// Catches even looking up residual reader configuration while the feature is off.
func TestFileBusinessDisabledNoDependencies(t *testing.T) {
	for _, flag := range []string{"", "false"} {
		get := func(k string) string {
			if k != "IM_FILE_BUSINESS_ENABLED" {
				t.Fatalf("disabled feature read dependency: %s", k)
			}
			return flag
		}
		c, e := fileBusinessConfigFromEnv(get, false)
		if e != nil || c.Enabled || c.SpoolDir != "" || c.OwnerID != "" || c.Objects.Endpoint != "" {
			t.Fatal("disabled feature initialized dependencies")
		}
	}
}

// Catches fallback/sharing of uploader or physical cleaner credentials.
func TestFileBusinessCredentialSeparation(t *testing.T) {
	for _, key := range []string{"IM_FILE_S3_ACCESS_KEY", "IM_FILE_CLEANUP_S3_ACCESS_KEY"} {
		t.Run(key, func(t *testing.T) {
			m := businessSettings(t)
			m[key] = "reader"
			if _, e := fileBusinessConfigFromEnv(env(m), true); e == nil {
				t.Fatal("reader credential shared")
			}
		})
	}
	m := businessSettings(t)
	m["IM_FILE_S3_ACCESS_KEY"] = "upload"
	m["IM_FILE_CLEANUP_S3_ACCESS_KEY"] = "cleanup"
	if _, e := fileBusinessConfigFromEnv(func(k string) string {
		if strings.Contains(k, "CLAMD") || strings.Contains(k, "QPDF") || k == "IM_FILE_SPOOL_DIR" || k == "IM_FILE_S3_SECRET_KEY" {
			t.Fatal("reader requires scanner/upload configuration")
		}
		return m[k]
	}, true); e != nil {
		t.Fatal(e)
	}
	delete(m, "IM_FILE_DOWNLOAD_S3_ACCESS_KEY")
	delete(m, "IM_FILE_DOWNLOAD_S3_SECRET_KEY")
	m["IM_FILE_S3_SECRET_KEY"] = "upload-secret"
	if _, e := fileBusinessConfigFromEnv(env(m), true); e == nil {
		t.Fatal("upload credentials substituted for reader")
	}
}

func TestProductionFileCapabilityMatrix(t *testing.T) {
	for _, tc := range []struct {
		u, b bool
		want httpserver.FileCapabilities
	}{
		{false, false, httpserver.FileCapabilities{}},
		{true, false, httpserver.FileCapabilities{UploadEnabled: true}},
		{false, true, httpserver.FileCapabilities{MessageSendEnabled: true, DownloadEnabled: true, FilenameSearchEnabled: true}},
		{true, true, httpserver.FileCapabilities{UploadEnabled: true, MessageSendEnabled: true, DownloadEnabled: true, FilenameSearchEnabled: true}},
	} {
		if got := productionFileCapabilities(tc.u, tc.b); got != tc.want {
			t.Fatal("capability/assembly disagreement", got)
		}
	}
}
