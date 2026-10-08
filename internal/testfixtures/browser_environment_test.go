package testfixtures

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestBrowserEnvironmentIsolation(t *testing.T) {
	t.Setenv("IM_TEST_S3_ADMIN_SECRET_KEY", "ambient-management-secret")
	t.Setenv("PGPASSWORD", "ambient-password")
	t.Setenv("IM_FILE_BUSINESS_ENABLED", "ambient-switch")
	cmd := exec.Command("/usr/bin/env")
	cmd.Env = BrowserEnvironment(map[string]string{"WEB_FILE_INPUT": "/controlled/private/input.json"})
	output, err := cmd.Output()
	if err != nil {
		t.Fatal("controlled browser environment probe failed")
	}
	for _, value := range []string{"ambient-management-secret", "ambient-password", "ambient-switch"} {
		if bytes.Contains(output, []byte(value)) {
			t.Fatal("browser child inherits management configuration")
		}
	}
	if !bytes.Contains(output, []byte("WEB_FILE_INPUT=/controlled/private/input.json")) {
		t.Fatal("explicit browser input missing")
	}
	t.Run("reject_management_override", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("management override allowed")
			}
		}()
		BrowserEnvironment(map[string]string{"PGPASSWORD": "forbidden"})
	})
}
