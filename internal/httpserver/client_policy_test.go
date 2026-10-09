package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func TestClientPolicyHTTPPrivilegeAndCAS(t *testing.T) {
	policy := policystore.ClientPolicy{Version: 9007199254740993, DeviceLeaseDays: 30, MaxActiveDevices: 10, OfflineCacheSeconds: 300, GroupReceiptVisibility: "owner_admin"}
	s := deviceStub{getPolicy: func(access.TrustedIdentity) (policystore.ClientPolicy, error) { return policy, nil }, setPolicy: func(id access.TrustedIdentity, v int64, p policystore.ClientPolicy) (policystore.ClientPolicy, error) {
		if id.UserID != actorID || v != policy.Version || p.DeviceLeaseDays != 30 {
			t.Fatal("policy contract")
		}
		return policystore.ClientPolicy{}, policystore.ErrVersionConflict
	}}
	h, e := HandlerWithDevices(http.NotFoundHandler(), authFunc(verified), s)
	if e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, deviceRequest(http.MethodGet, "/api/v1/me/client-capabilities", "", true))
	var b map[string]any
	if e = json.Unmarshal(w.Body.Bytes(), &b); e != nil || b["version"] != "9007199254740993" || b["receipts_enabled"] != true {
		t.Fatal("canonical policy", w.Code, b)
	}
	body := `{"expected_version":"9007199254740993","device_lease_days":30,"max_active_devices":10,"offline_cache_seconds":300,"group_receipt_visibility":"owner_admin"}`
	w = httptest.NewRecorder()
	h.ServeHTTP(w, deviceRequest("PUT", "/api/v1/admin/client-policy", body, true))
	if w.Code != 409 {
		t.Fatal("CAS mapping", w.Code)
	}
	for _, version := range []string{`9007199254740993`, `"09007199254740993"`, `"9223372036854775808"`, `"-1"`} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, deviceRequest("PUT", "/api/v1/admin/client-policy", strings.Replace(body, `"9007199254740993"`, version, 1), true))
		if w.Code != 400 {
			t.Fatal("invalid version", w.Code)
		}
	}
	s.setPolicy = func(access.TrustedIdentity, int64, policystore.ClientPolicy) (policystore.ClientPolicy, error) {
		return policystore.ClientPolicy{}, policystore.ErrForbidden
	}
	h, _ = HandlerWithDevices(http.NotFoundHandler(), authFunc(verified), s)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, deviceRequest("PUT", "/api/v1/admin/client-policy", body, true))
	if w.Code != 403 {
		t.Fatal("admin scope", w.Code)
	}
}
