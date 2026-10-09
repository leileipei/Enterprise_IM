package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReceiptRuntimeDefaultOff(t *testing.T) {
	cfg, e := loadReceiptRuntime(func(string) string { return "" }, false)
	if e != nil || cfg.Enabled {
		t.Fatal("default must be off", e)
	}
	for _, values := range []map[string]string{{"IM_RECEIPTS_ENABLED": "true"}, {"IM_RECEIPTS_ENABLED": "maybe"}} {
		if _, e = loadReceiptRuntime(func(k string) string { return values[k] }, false); e == nil {
			t.Fatal("unsafe runtime enabled")
		}
	}
	cfg, e = loadReceiptRuntime(func(k string) string {
		if k == "IM_RECEIPTS_ENABLED" {
			return "true"
		}
		return ""
	}, true)
	if e != nil || !cfg.Enabled || cfg.DeviceRegistrationsPerHour != 10 || cfg.WriteRatePerSecond != 10 {
		t.Fatal("defaults", e)
	}
	for _, key := range []string{"IM_DEVICE_REGISTRATIONS_PER_HOUR", "IM_RECEIPT_WRITES_PER_SECOND"} {
		for _, value := range []string{"0", "-1", "invalid", "10001"} {
			if _, e = loadReceiptRuntime(func(k string) string {
				if k == "IM_RECEIPTS_ENABLED" {
					return "true"
				}
				if k == key {
					return value
				}
				return ""
			}, true); e == nil {
				t.Fatal("invalid runtime limit", key, value)
			}
		}
	}
}

func TestReceiptRuntimeClosedAssembly(t *testing.T) {
	h, e := assembleDeviceRoutes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }), nil, nil, ReceiptRuntimeConfig{})
	if e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"/api/v1/devices", "/api/v1/devices/" + "11111111-1111-4111-8111-111111111111", "/api/v1/admin/client-policy", "/api/v1/me/client-capabilities", "/api/v1/conversations/11111111-1111-4111-8111-111111111111/sync", "/api/v1/conversations/11111111-1111-4111-8111-111111111111/read"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatal("closed route", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/conversations/11111111-1111-4111-8111-111111111111/messages", nil))
	if w.Code != 200 {
		t.Fatal("legacy route intercepted")
	}
}
