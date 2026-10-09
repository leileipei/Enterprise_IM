package main

import (
	"context"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

type receiptAssemblyAuth struct{}

func (receiptAssemblyAuth) Authenticate(context.Context, string) (httpserver.VerifiedIdentity, error) {
	return httpserver.VerifiedIdentity{TenantID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", UserID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

type receiptAssemblyService struct {
	httpserver.ReceiptService
	called *bool
}

func (s receiptAssemblyService) ConfirmRead(_ context.Context, id access.TrustedIdentity, d policystore.DeviceCredential, c string, seqs []int64) (policystore.ReceiptState, error) {
	*s.called = true
	return policystore.ReceiptState{ReadSeq: seqs[0]}, nil
}
func TestReceiptRuntimeQueryAssembly(t *testing.T) {
	called := false
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h, e := assembleReceiptRoutes(fallback, receiptAssemblyAuth{}, receiptAssemblyService{called: &called}, ReceiptRuntimeConfig{Enabled: true})
	if e != nil {
		t.Fatal(e)
	}
	r := httptest.NewRequest("POST", "/api/v1/conversations/cccccccc-cccc-4ccc-8ccc-cccccccccccc/read", strings.NewReader(`{"seqs":["1"]}`))
	r.Header.Set("Authorization", "Bearer verified")
	r.Header.Set("X-Acting-Membership-ID", "dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	r.Header.Set("X-IM-Device-ID", "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee")
	r.Header.Set("X-IM-Device-Token", strings.Repeat("A", 43))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !called {
		t.Fatal("receipt assembly missing", w.Code)
	}
	h, e = assembleReceiptRoutes(fallback, nil, nil, ReceiptRuntimeConfig{})
	if e != nil {
		t.Fatal(e)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal("off route open", w.Code)
	}
}
func TestReceiptRuntimeIndependentStream(t *testing.T) {
	cfg, e := loadReceiptRuntime(func(k string) string {
		if k == "IM_RECEIPTS_ENABLED" {
			return "true"
		}
		return ""
	}, true)
	if e != nil || cfg.Stream != "enterprise-im:receipt-changed:v1" {
		t.Fatal(cfg, e)
	}
	for _, s := range []string{" bad ", "enterprise-im:message-created:v1", "same"} {
		_, e = loadReceiptRuntime(func(k string) string {
			switch k {
			case "IM_RECEIPTS_ENABLED":
				return "true"
			case "IM_RECEIPT_STREAM":
				return s
			case "IM_REALTIME_STREAM":
				return "same"
			}
			return ""
		}, true)
		if e == nil {
			t.Fatal("unsafe receipt stream", s)
		}
	}
}
