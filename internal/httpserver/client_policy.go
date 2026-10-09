package httpserver

import (
	"net/http"
	"strconv"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type clientPolicyDTO struct {
	Version                string `json:"version"`
	DeviceLeaseDays        int    `json:"device_lease_days"`
	MaxActiveDevices       int    `json:"max_active_devices"`
	OfflineCacheSeconds    int    `json:"offline_cache_seconds"`
	GroupReceiptVisibility string `json:"group_receipt_visibility"`
}

func clientPolicyResponse(p policystore.ClientPolicy) clientPolicyDTO {
	return clientPolicyDTO{Version: strconv.FormatInt(p.Version, 10), DeviceLeaseDays: p.DeviceLeaseDays, MaxActiveDevices: p.MaxActiveDevices, OfflineCacheSeconds: p.OfflineCacheSeconds, GroupReceiptVisibility: p.GroupReceiptVisibility}
}
func canonicalReceiptInt(raw string, positive bool) (int64, bool) {
	n, e := strconv.ParseInt(raw, 10, 64)
	return n, e == nil && n >= 0 && (!positive || n > 0) && strconv.FormatInt(n, 10) == raw
}
func serveClientPolicy(w http.ResponseWriter, r *http.Request, auth Authenticator, svc DeviceService) {
	capabilities := r.URL.Path == "/api/v1/me/client-capabilities"
	if r.Method != "GET" && (capabilities || r.Method != "PUT") {
		allowed := "GET, PUT"
		if capabilities {
			allowed = "GET"
		}
		w.Header().Set("Allow", allowed)
		writeAdminError(w, 405, "method_not_allowed")
		return
	}
	v, status, code := bearerIdentity(r.Context(), r, auth)
	if status != 0 {
		writeDeviceAuthError(w, status, code)
		return
	}
	id, ok := deviceActingIdentity(w, r, v)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeAdminError(w, 400, "invalid_request")
		return
	}
	if r.Method == "GET" {
		if !emptyReceiptBody(r) {
			writeAdminError(w, 400, "invalid_request")
			return
		}
		p, e := svc.GetClientPolicy(r.Context(), id)
		if e != nil {
			writeDeviceError(w, e)
			return
		}
		if capabilities {
			writeAdminJSON(w, 200, struct {
				clientPolicyDTO
				ReceiptsEnabled bool      `json:"receipts_enabled"`
				ServerTime      time.Time `json:"server_time"`
			}{clientPolicyDTO: clientPolicyResponse(p), ReceiptsEnabled: true, ServerTime: time.Now().UTC()})
		} else {
			writeAdminJSON(w, 200, clientPolicyResponse(p))
		}
		return
	}
	var body struct {
		ExpectedVersion        string `json:"expected_version"`
		DeviceLeaseDays        int    `json:"device_lease_days"`
		MaxActiveDevices       int    `json:"max_active_devices"`
		OfflineCacheSeconds    int    `json:"offline_cache_seconds"`
		GroupReceiptVisibility string `json:"group_receipt_visibility"`
	}
	if !decodeReceiptJSON(r, 65536, []string{"expected_version", "device_lease_days", "max_active_devices", "offline_cache_seconds", "group_receipt_visibility"}, &body) {
		writeAdminError(w, 400, "invalid_request")
		return
	}
	expected, ok := canonicalReceiptInt(body.ExpectedVersion, false)
	if !ok {
		writeAdminError(w, 400, "invalid_request")
		return
	}
	p, e := svc.SetClientPolicy(r.Context(), id, expected, policystore.ClientPolicy{DeviceLeaseDays: body.DeviceLeaseDays, MaxActiveDevices: body.MaxActiveDevices, OfflineCacheSeconds: body.OfflineCacheSeconds, GroupReceiptVisibility: body.GroupReceiptVisibility})
	if e != nil {
		writeDeviceError(w, e)
		return
	}
	writeAdminJSON(w, 200, clientPolicyResponse(p))
}
