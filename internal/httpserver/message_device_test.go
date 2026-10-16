package httpserver

import (
	"context"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMessageDeviceHeadersFailClosedAndInvalidCredentialMaps401(t *testing.T) {
	const cid = "00000000-0000-4000-8000-000000000471"
	const did = "00000000-0000-4000-8000-000000000472"
	for _, kind := range []string{"id only", "token only", "duplicate id", "duplicate token", "bad encoding", "valid shape rejected by service"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			svc := messageStub{send: func(context.Context, access.TrustedIdentity, string, string, string) (policystore.MessageACK, error) {
				calls++
				return policystore.MessageACK{}, policystore.ErrInvalidDevice
			}}
			h, e := HandlerWithConversations(Handler(nil), authFunc(verified), svc)
			if e != nil {
				t.Fatal(e)
			}
			r := messageRequest("/api/v1/conversations/"+cid+"/messages", `{"client_msg_id":"0199f04a-0000-7000-8000-000000000471","text":"message"}`)
			if kind != "token only" {
				r.Header.Set("X-IM-Device-ID", did)
			}
			if kind != "id only" {
				r.Header.Set("X-IM-Device-Token", strings.Repeat("A", 43))
			}
			switch kind {
			case "duplicate id":
				r.Header.Add("X-IM-Device-ID", did)
			case "duplicate token":
				r.Header.Add("X-IM-Device-Token", strings.Repeat("A", 43))
			case "bad encoding":
				r.Header.Set("X-IM-Device-Token", strings.Repeat("!", 43))
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			wantCalls := 0
			if kind == "valid shape rejected by service" {
				wantCalls = 1
			}
			if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid_device") || w.Header().Get("WWW-Authenticate") != "Bearer" || calls != wantCalls {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
		})
	}
}
