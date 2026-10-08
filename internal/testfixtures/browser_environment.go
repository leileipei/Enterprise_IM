package testfixtures

import "os"

// BrowserEnvironment conveys only the declared local browser fixture inputs.
// Parent database/storage administrator configuration is never inherited.
func BrowserEnvironment(overrides map[string]string) []string {
	keys := []string{"PATH", "TMPDIR", "LANG", "TZ", "NODE_PATH", "CHROMIUM_EXECUTABLE", "IM_TEST_WEB_URL", "IM_TEST_PRIVATE_CAPTION", "IM_TEST_PRIVATE_NAME", "FILE_UNIT_SOURCE", "WEB_FILE_INPUT", "IM_TEST_AUDIT_SCREENSHOT_DIR", "IM_TEST_CROSS_SEARCH_SCREENSHOT_DIR", "IM_TEST_HOLD_ACTION_SCREENSHOT_DIR", "IM_TEST_LEGAL_HOLD_SCREENSHOT_DIR", "IM_TEST_MESSAGE_SEARCH_SCREENSHOT_DIR", "IM_TEST_HISTORY_SCREENSHOT_DIR", "IM_TEST_POLICY_SCREENSHOT_DIR", "IM_TEST_RETENTION_SCREENSHOT_DIR"}
	values := map[string]string{}
	allowed := map[string]bool{}
	for _, key := range keys {
		allowed[key] = true
		if value := os.Getenv(key); value != "" {
			values[key] = value
		}
	}
	for key, value := range overrides {
		if !allowed[key] {
			panic("unexpected browser fixture configuration key")
		}
		values[key] = value
	}
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	return result
}
