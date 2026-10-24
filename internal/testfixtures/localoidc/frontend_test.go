package localoidc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestHTTPSFrontendPreservesRealAPIAndWebSocket(t *testing.T) {
	seen := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		if r.URL.Path == "/api/v1/stream" {
			c, e := websocket.Accept(w, r, nil)
			if e != nil {
				return
			}
			defer c.CloseNow()
			ctx, cancel := context.WithTimeout(r.Context(), time.Second)
			defer cancel()
			kind, b, e := c.Read(ctx)
			if e == nil {
				_ = c.Write(ctx, kind, b)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"actual_upstream":true}`))
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	provider := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("local-provider")) })
	handler, e := Frontend(provider, u)
	if e != nil {
		t.Fatal(e)
	}
	files := privateTLS(t, strings.Repeat("c", 32))
	cert, e := tls.LoadX509KeyPair(files.ServerCert, files.ServerKey)
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	ca, _ := os.ReadFile(files.CACert)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	req, _ := http.NewRequest("GET", server.URL+"/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer synthetic-test-only")
	res, e := client.Do(req)
	if e != nil || res.StatusCode != 200 || !strings.Contains(string(body(t, res)), "actual_upstream") {
		t.Fatal("API not proxied")
	}
	if <-seen != "Bearer synthetic-test-only" {
		t.Fatal("Authorization lost")
	}
	res, e = client.Get(server.URL + "/oidc/keys")
	if e != nil || string(body(t, res)) != "local-provider" {
		t.Fatal("OIDC route not isolated")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, e := websocket.Dial(ctx, "wss"+strings.TrimPrefix(server.URL, "https")+"/api/v1/stream", &websocket.DialOptions{HTTPClient: client})
	if e != nil {
		t.Fatal(e)
	}
	defer c.CloseNow()
	if e = c.Write(ctx, websocket.MessageText, []byte("probe")); e != nil {
		t.Fatal(e)
	}
	_, msg, e := c.Read(ctx)
	if e != nil || string(msg) != "probe" {
		t.Fatal("WebSocket proxy failed")
	}
	for _, path := range []string{"/private", "/oidc-other", "/api/v1/../private", "/api/v1//me"} {
		res, e = client.Get(server.URL + path)
		if e != nil {
			t.Fatal(e)
		}
		body(t, res)
		if res.StatusCode != 404 && res.StatusCode != 400 {
			t.Fatal("unexpected upstream route", path)
		}
	}
}
func TestFrontendRejectsExternalTargetsAndCleartext(t *testing.T) {
	provider := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	for _, target := range []string{"http://customer.invalid:8080", "https://127.0.0.1:8080", "http://user:password@127.0.0.1:8080", "http://127.0.0.1:8080/path", "http://127.0.0.1:8080?x=1", "http://127.0.0.1:80"} {
		u, _ := url.Parse(target)
		if _, e := Frontend(provider, u); e == nil {
			t.Fatal("external or ambiguous target accepted")
		}
	}
	u, _ := url.Parse("http://127.0.0.1:49152")
	h, e := Frontend(provider, u)
	if e != nil {
		t.Fatal(e)
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest("GET", "http://127.0.0.1/oidc/keys", nil))
	if res.Code != 400 {
		t.Fatal("cleartext frontend accepted")
	}
}
