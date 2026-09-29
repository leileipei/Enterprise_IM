package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"github.com/leileipei/Enterprise_IM/internal/realtime"
)

const testTicket = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type realtimeAuthStub struct {
	mu       sync.Mutex
	actions  []policystore.RealtimeAction
	active   bool
	authErr  error
	checkErr error
}

func (s *realtimeAuthStub) AuthorizeRealtime(_ context.Context, id access.TrustedIdentity, action policystore.RealtimeAction) error {
	if id.TenantID != tenantID || id.UserID != actorID || id.ActingMembershipID != actingID {
		return policystore.ErrForbidden
	}
	s.mu.Lock()
	s.actions = append(s.actions, action)
	s.mu.Unlock()
	return s.authErr
}

func (s *realtimeAuthStub) RealtimeIdentityActive(context.Context, access.TrustedIdentity) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.checkErr
}

type ticketStub struct {
	mu       sync.Mutex
	issued   bool
	issueErr error
	used     bool
}

func (s *ticketStub) Issue(_ context.Context, id access.TrustedIdentity) (string, error) {
	if id.TenantID != tenantID || id.UserID != actorID || id.ActingMembershipID != actingID {
		return "", realtime.ErrInvalidTicket
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.issueErr != nil {
		return "", s.issueErr
	}
	s.issued = true
	return testTicket, nil
}

func (s *ticketStub) Consume(_ context.Context, ticket string) (access.TrustedIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ticket != testTicket || !s.issued || s.used {
		return access.TrustedIdentity{}, realtime.ErrInvalidTicket
	}
	s.used = true
	return access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}, nil
}

func TestRealtimeTicketRouteValidatesIdentityAndRequest(t *testing.T) {
	auth := &realtimeAuthStub{active: true}
	tickets := &ticketStub{}
	handler, err := HandlerWithRealtime(Handler(nil), authFunc(verified), auth, tickets, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, adminRequest(http.MethodPost, "/api/v1/realtime/tickets"))
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"ticket":"`+testTicket+`"`) ||
		!strings.Contains(res.Body.String(), `"expires_in_seconds":30`) || res.Header().Get("Cache-Control") != "no-store" ||
		len(auth.actions) != 1 || auth.actions[0] != policystore.RealtimeTicket {
		t.Fatalf("ticket response: %d %s actions=%v", res.Code, res.Body.String(), auth.actions)
	}
	for _, request := range []*http.Request{
		adminRequest(http.MethodGet, "/api/v1/realtime/tickets"),
		adminRequest(http.MethodPost, "/api/v1/realtime/tickets?tenant_id="+tenantID),
		httpTestRequestWithBody("/api/v1/realtime/tickets", "{}"),
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, request)
		if request.Method == http.MethodGet && res.Code != 405 || request.Method == http.MethodPost && res.Code != 400 {
			t.Fatalf("invalid ticket request: %d %s", res.Code, res.Body.String())
		}
	}
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/api/v1/realtime/tickets", nil))
	if res.Code != 401 {
		t.Fatalf("anonymous ticket: %d %s", res.Code, res.Body.String())
	}
	auth.authErr = policystore.ErrForbidden
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, adminRequest(http.MethodPost, "/api/v1/realtime/tickets"))
	if res.Code != 403 {
		t.Fatalf("inactive ticket: %d %s", res.Code, res.Body.String())
	}
	auth.authErr = nil
	tickets.issueErr = errors.New("private Redis detail")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, adminRequest(http.MethodPost, "/api/v1/realtime/tickets"))
	if res.Code != 503 || strings.Contains(res.Body.String(), "private Redis detail") {
		t.Fatalf("Redis failure: %d %s", res.Code, res.Body.String())
	}
}

func httpTestRequestWithBody(path, body string) *http.Request {
	req := adminRequest(http.MethodPost, path)
	req.Body = http.NoBody
	if body != "" {
		req = adminRequest(http.MethodPost, path)
		req.Body = io.NopCloser(strings.NewReader(body))
	}
	return req
}

func TestRealtimeWebSocketReadyReplayAndOrigin(t *testing.T) {
	auth := &realtimeAuthStub{active: true}
	tickets := &ticketStub{issued: true}
	handler, err := HandlerWithRealtime(Handler(nil), authFunc(verified), auth, tickets, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/realtime"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"enterprise-im.v1", "ticket." + testTicket},
	})
	if err != nil {
		t.Fatalf("valid websocket: %v status=%v", err, response)
	}
	defer conn.CloseNow()
	if conn.Subprotocol() != "enterprise-im.v1" || strings.Contains(response.Header.Get("Sec-WebSocket-Protocol"), testTicket) {
		t.Fatalf("ticket echoed in subprotocol: %q", conn.Subprotocol())
	}
	_, body, err := conn.Read(ctx)
	if err != nil || string(body) != `{"type":"ready","resync_required":true}` {
		t.Fatalf("ready frame: %s %v", body, err)
	}
	_, response, err = websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"enterprise-im.v1", "ticket." + testTicket},
	})
	if err == nil || response == nil || response.StatusCode != 401 {
		t.Fatalf("replayed ticket: response=%v err=%v", response, err)
	}
	wrongOriginTickets := &ticketStub{issued: true}
	wrongOriginHandler, _ := HandlerWithRealtime(Handler(nil), authFunc(verified), auth, wrongOriginTickets, context.Background())
	wrongOriginServer := httptest.NewServer(wrongOriginHandler)
	defer wrongOriginServer.Close()
	wrongURL := "ws" + strings.TrimPrefix(wrongOriginServer.URL, "http") + "/api/v1/realtime"
	_, response, err = websocket.Dial(ctx, wrongURL, &websocket.DialOptions{
		Subprotocols: []string{"enterprise-im.v1", "ticket." + testTicket},
		HTTPHeader:   http.Header{"Origin": []string{"https://evil.example"}},
	})
	if err == nil || response == nil || response.StatusCode != 403 {
		t.Fatalf("cross-origin websocket: response=%v err=%v", response, err)
	}
}

func TestRealtimeWebSocketRejectsInvalidProtocolAndRevokedTicket(t *testing.T) {
	auth := &realtimeAuthStub{active: true}
	tickets := &ticketStub{issued: true}
	handler, err := HandlerWithRealtime(Handler(nil), authFunc(verified), auth, tickets, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/realtime"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, protocols := range [][]string{
		{"enterprise-im.v1"},
		{"ticket." + testTicket},
		{"enterprise-im.v1", "ticket." + testTicket, "extra"},
	} {
		_, response, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{Subprotocols: protocols})
		if err == nil || response == nil || response.StatusCode != 401 || tickets.used {
			t.Fatalf("bad protocols %v: response=%v err=%v used=%v", protocols, response, err, tickets.used)
		}
	}
	auth.authErr = policystore.ErrForbidden
	_, response, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"enterprise-im.v1", "ticket." + testTicket},
	})
	if err == nil || response == nil || response.StatusCode != 403 || !tickets.used {
		t.Fatalf("revoked ticket: response=%v err=%v", response, err)
	}
	_, response, err = websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"enterprise-im.v1", "ticket." + testTicket},
	})
	if err == nil || response == nil || response.StatusCode != 401 {
		t.Fatalf("revoked ticket reused: response=%v err=%v", response, err)
	}
}

func TestRealtimeConnectionLimitAndRevocation(t *testing.T) {
	auth := &realtimeAuthStub{active: true}
	tickets := &ticketStub{issued: true}
	h := &realtimeHandler{authenticator: authFunc(verified), authorizer: auth, tickets: tickets,
		shutdown: context.Background(), checkInterval: 20 * time.Millisecond}
	for i := 0; i < 5; i++ {
		if !h.claim(access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) {
			t.Fatalf("connection %d rejected", i)
		}
	}
	if h.claim(access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) {
		t.Fatal("sixth connection accepted")
	}
	h.release(access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID})
	if !h.claim(access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) {
		t.Fatal("released slot not reusable")
	}
	connectionTickets := &ticketStub{issued: true}
	socketHandler := &realtimeHandler{base: Handler(nil), authenticator: authFunc(verified), authorizer: auth,
		tickets: connectionTickets, shutdown: context.Background(), checkInterval: 20 * time.Millisecond,
		connections: make(map[string]int)}
	server := httptest.NewServer(socketHandler)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/realtime"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"enterprise-im.v1", "ticket." + testTicket},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatalf("ready: %v", err)
	}
	auth.mu.Lock()
	auth.active = false
	auth.mu.Unlock()
	if _, _, err := conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("revoked connection status: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		socketHandler.mu.Lock()
		count := socketHandler.total
		socketHandler.mu.Unlock()
		if count == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("connection slot leaked after revocation")
}

func TestRealtimeWebSocketSurvivesHTTPWriteTimeout(t *testing.T) {
	auth := &realtimeAuthStub{active: true}
	tickets := &ticketStub{issued: true}
	handler, err := HandlerWithRealtime(Handler(nil), authFunc(verified), auth, tickets, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config.WriteTimeout = 100 * time.Millisecond
	server.Start()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/realtime"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"enterprise-im.v1", "ticket." + testTicket},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatalf("ready: %v", err)
	}
	conn.CloseRead(ctx)
	time.Sleep(200 * time.Millisecond)
	if err := conn.Ping(ctx); err != nil {
		t.Fatalf("websocket closed by HTTP write timeout: %v", err)
	}
}

func TestRealtimeShutdownClosesConnectionAndReleasesSlot(t *testing.T) {
	auth := &realtimeAuthStub{active: true}
	tickets := &ticketStub{issued: true}
	shutdown, stop := context.WithCancel(context.Background())
	handler, err := HandlerWithRealtime(Handler(nil), authFunc(verified), auth, tickets, shutdown)
	if err != nil {
		t.Fatal(err)
	}
	h := handler.(*realtimeHandler)
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/realtime"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"enterprise-im.v1", "ticket." + testTicket},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatalf("ready: %v", err)
	}
	stop()
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("connection remained open after shutdown")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		count := h.total
		h.mu.Unlock()
		if count == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("connection slot leaked after shutdown")
}
