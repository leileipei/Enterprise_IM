package httpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type deviceStub struct {
	register  func(access.TrustedIdentity, policystore.DeviceRegistration) (policystore.DeviceGrant, error)
	list      func(policystore.DeviceOwner, string, int) (policystore.DevicePage, error)
	revoke    func(policystore.DeviceOwner, string) error
	renew     func(access.TrustedIdentity, policystore.DeviceCredential) (policystore.DeviceGrant, error)
	getPolicy func(access.TrustedIdentity) (policystore.ClientPolicy, error)
	setPolicy func(access.TrustedIdentity, int64, policystore.ClientPolicy) (policystore.ClientPolicy, error)
}

func (s deviceStub) RegisterDevice(_ context.Context, id access.TrustedIdentity, r policystore.DeviceRegistration) (policystore.DeviceGrant, error) {
	return s.register(id, r)
}
func (s deviceStub) ListDevices(_ context.Context, o policystore.DeviceOwner, c string, l int) (policystore.DevicePage, error) {
	return s.list(o, c, l)
}
func (s deviceStub) RevokeDevice(_ context.Context, o policystore.DeviceOwner, id string) error {
	return s.revoke(o, id)
}
func (s deviceStub) RenewDevice(_ context.Context, id access.TrustedIdentity, c policystore.DeviceCredential) (policystore.DeviceGrant, error) {
	return s.renew(id, c)
}
func (s deviceStub) GetClientPolicy(_ context.Context, id access.TrustedIdentity) (policystore.ClientPolicy, error) {
	return s.getPolicy(id)
}
func (s deviceStub) SetClientPolicy(_ context.Context, id access.TrustedIdentity, v int64, p policystore.ClientPolicy) (policystore.ClientPolicy, error) {
	return s.setPolicy(id, v, p)
}
func deviceRequest(method, path, body string, acting bool) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer verified-token")
	if acting {
		r.Header.Set("X-Acting-Membership-ID", actingID)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func TestDeviceHTTPVerifiedOwnerAndStrictJSON(t *testing.T) {
	called := 0
	s := deviceStub{register: func(id access.TrustedIdentity, r policystore.DeviceRegistration) (policystore.DeviceGrant, error) {
		called++
		if id.TenantID != tenantID || id.UserID != actorID || id.ActingMembershipID != actingID || r.Name != "浏览器" {
			t.Fatal("untrusted owner")
		}
		return policystore.DeviceGrant{DeviceID: targetUserID, Token: strings.Repeat("A", 43), ExpiresAt: time.Now().Add(time.Hour), ServerTime: time.Now()}, nil
	}}
	h, e := HandlerWithDevices(http.NotFoundHandler(), authFunc(verified), s)
	if e != nil {
		t.Fatal(e)
	}
	body := `{"registration_id":"019a13c0-0000-7000-8000-000000000001","platform":"web","name":"浏览器"}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, deviceRequest("POST", "/api/v1/devices", body, true))
	if w.Code != 201 || called != 1 || !strings.Contains(w.Body.String(), `"device_token"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, bad := range []string{strings.TrimSuffix(body, "}") + `,"tenant_id":"` + tenantID + `"}`, strings.TrimSuffix(body, "}") + `,"user_id":"` + actorID + `"}`, strings.TrimSuffix(body, "}") + `,"token":"secret"}`, strings.TrimSuffix(body, "}") + `,"name":"duplicate"}`, `null`, body + body, strings.Repeat(" ", 2049) + body, strings.Replace(body, `"name"`, `"Name"`, 1)} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, deviceRequest("POST", "/api/v1/devices", bad, true))
		if w.Code != 400 {
			t.Fatal("bad JSON accepted", w.Code)
		}
	}
	if called != 1 {
		t.Fatal("invalid bodies reached service")
	}
	for _, tc := range []struct {
		method, path string
		acting       bool
		code         int
	}{{"PUT", "/api/v1/devices", true, 405}, {"POST", "/api/v1/devices", false, 400}, {"POST", "/api/v1/devices?tenant_id=x", true, 400}, {"GET", "/api/v1/devices/unknown/nested", false, 404}} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, deviceRequest(tc.method, tc.path, body, tc.acting))
		if w.Code != tc.code {
			t.Fatal(tc, w.Code)
		}
	}
}

func TestDeviceHTTPAccountOnlyRevoke(t *testing.T) {
	s := deviceStub{list: func(o policystore.DeviceOwner, c string, l int) (policystore.DevicePage, error) {
		if o != (policystore.DeviceOwner{TenantID: tenantID, UserID: actorID}) || c != "" || l != 20 {
			t.Fatal("owner list")
		}
		return policystore.DevicePage{Devices: []policystore.Device{{ID: targetUserID, Status: "active", Name: "浏览器"}}}, nil
	}, revoke: func(o policystore.DeviceOwner, id string) error {
		if o.UserID != actorID || id != targetUserID {
			t.Fatal("owner revoke")
		}
		return nil
	}}
	h, e := HandlerWithDevices(http.NotFoundHandler(), authFunc(verified), s)
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		method, path string
		code         int
	}{{"GET", "/api/v1/devices", 200}, {"DELETE", "/api/v1/devices/" + targetUserID, 204}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, deviceRequest(tc.method, tc.path, "", false))
		if w.Code != tc.code || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Code)
		}
	}
}

func TestDeviceHTTPRenewAndErrors(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	s := deviceStub{renew: func(id access.TrustedIdentity, c policystore.DeviceCredential) (policystore.DeviceGrant, error) {
		if c.ID != targetUserID || c.Token != token || id.UserID != actorID {
			t.Fatal("credential parsing")
		}
		return policystore.DeviceGrant{DeviceID: c.ID, ExpiresAt: time.Now().Add(time.Hour), ServerTime: time.Now()}, nil
	}}
	h, e := HandlerWithDevices(http.NotFoundHandler(), authFunc(verified), s)
	if e != nil {
		t.Fatal(e)
	}
	path := "/api/v1/devices/" + targetUserID + "/renew"
	r := deviceRequest("POST", path, `{}`, true)
	r.Header.Set("X-IM-Device-ID", targetUserID)
	r.Header.Set("X-IM-Device-Token", token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "device_token") {
		t.Fatal("renew secret", w.Code)
	}
	for _, bad := range []string{token + "=", strings.Repeat("!", 43), token[:42] + "B"} {
		r = deviceRequest("POST", path, `{}`, true)
		r.Header.Set("X-IM-Device-ID", targetUserID)
		r.Header.Set("X-IM-Device-Token", bad)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("invalid token", w.Code)
		}
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{{&policystore.DeviceRegistrationExistsError{DeviceID: targetUserID}, 409, "registration_exists"}, {policystore.ErrDeviceQuotaExceeded, 409, "device_quota"}, {policystore.ErrDeviceRegistrationRateLimited, 429, "rate_limited"}, {policystore.ErrForbidden, 403, "invalid_identity"}, {errors.New("secret db detail"), 503, "unavailable"}} {
		s.register = func(access.TrustedIdentity, policystore.DeviceRegistration) (policystore.DeviceGrant, error) {
			return policystore.DeviceGrant{}, tc.err
		}
		h, _ = HandlerWithDevices(http.NotFoundHandler(), authFunc(verified), s)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, deviceRequest("POST", "/api/v1/devices", `{"registration_id":"019a13c0-0000-7000-8000-000000000001","platform":"web","name":"browser"}`, true))
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) || strings.Contains(w.Body.String(), "secret") {
			t.Fatal("error mapping", w.Code, w.Body.String())
		}
	}
}

func TestDeviceHeaderSecretsNotLogged(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	s := deviceStub{renew: func(access.TrustedIdentity, policystore.DeviceCredential) (policystore.DeviceGrant, error) {
		return policystore.DeviceGrant{}, errors.New("device-token-secret")
	}}
	h, e := HandlerWithDevices(http.NotFoundHandler(), authFunc(verified), s)
	if e != nil {
		t.Fatal(e)
	}
	r := deviceRequest("POST", "/api/v1/devices/"+targetUserID+"/renew", `{}`, true)
	r.Header.Set("X-IM-Device-ID", targetUserID)
	r.Header.Set("X-IM-Device-Token", strings.Repeat("A", 43))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	for _, secret := range []string{strings.Repeat("A", 43), "verified-token", "device-token-secret"} {
		if strings.Contains(logs.String(), secret) || strings.Contains(w.Body.String(), secret) {
			t.Fatal("secret leaked")
		}
	}
	var response map[string]any
	if e = json.Unmarshal(w.Body.Bytes(), &response); e != nil || w.Code != 503 {
		t.Fatal("closed error")
	}
}

type receiptRateStub func(policystore.DeviceOwner) error

func (f receiptRateStub) CheckReceiptWriteRate(_ context.Context, o policystore.DeviceOwner) error {
	return f(o)
}
func TestDeviceHTTPWriteLimiterUsesVerifiedOwner(t *testing.T) {
	called := false
	h, e := HandlerWithReceiptWriteLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }), authFunc(verified), receiptRateStub(func(o policystore.DeviceOwner) error {
		if o.UserID != actorID || o.TenantID != tenantID {
			t.Fatal("untrusted rate key")
		}
		return policystore.ErrReceiptRateLimited
	}))
	if e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, deviceRequest("POST", "/api/v1/devices", `{}`, true))
	if w.Code != 429 || called || w.Header().Get("Retry-After") != "1" {
		t.Fatal("rate bypass")
	}
}
