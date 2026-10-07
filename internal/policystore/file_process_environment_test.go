package policystore_test

import (
	"bytes"
	"testing"
)

// A real env child probes transport of configuration, not business behavior.
func TestFileProcessEnvironmentIsolation(t *testing.T) {
	t.Setenv("IM_TEST_S3_ADMIN_ACCESS_KEY", "untrusted-parent-admin")
	t.Setenv("IM_TEST_S3_ADMIN_SECRET_KEY", "parent-secret-for-test")
	t.Setenv("PGPASSWORD", "ambient-database-password")
	t.Setenv("IM_FILE_BUSINESS_ENABLED", "ambient-startup-switch")
	child := fileProductCommand("/usr/bin/env", []string{"IM_FILE_S3_ACCESS_KEY=ordinary-role", "IM_FILE_BUSINESS_ENABLED=false"})
	output, err := child.Output()
	if err != nil {
		t.Fatal("controlled environment child failed")
	}
	for _, forbidden := range [][]byte{[]byte("untrusted-parent-admin"), []byte("parent-secret-for-test"), []byte("ambient-database-password"), []byte("ambient-startup-switch")} {
		if bytes.Contains(output, forbidden) {
			t.Fatal("official product child inherited parent management configuration")
		}
	}
	if !bytes.Contains(output, []byte("IM_FILE_S3_ACCESS_KEY=ordinary-role")) || !bytes.Contains(output, []byte("IM_FILE_BUSINESS_ENABLED=false")) {
		t.Fatal("explicit product configuration lost")
	}
}
