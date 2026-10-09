package webclient

import "testing"

func TestWebReceiptPageAndVisibility(t *testing.T) { runFileUnit(t, "receipts", "e2e/receipts.cjs") }
func TestWebDeviceMemoryAndQuota(t *testing.T) {
	runFileUnit(t, "device-session", "e2e/device_session.cjs")
}
