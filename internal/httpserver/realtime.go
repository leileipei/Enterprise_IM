package httpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"github.com/leileipei/Enterprise_IM/internal/realtime"
)

const realtimeSubprotocol = "enterprise-im.v1"
const maxRealtimeConnectionsPerUser = 5
const maxRealtimeConnections = 5000

type RealtimeAuthorizer interface {
	AuthorizeRealtime(context.Context, access.TrustedIdentity, policystore.RealtimeAction) error
	RealtimeIdentityActive(context.Context, access.TrustedIdentity) (bool, error)
}

type RealtimeTicketStore interface {
	Issue(context.Context, access.TrustedIdentity) (string, error)
	Consume(context.Context, string) (access.TrustedIdentity, error)
}

type realtimeHandler struct {
	base          http.Handler
	authenticator Authenticator
	authorizer    RealtimeAuthorizer
	tickets       RealtimeTicketStore
	shutdown      context.Context
	checkInterval time.Duration
	mu            sync.Mutex
	connections   map[string]int
	total         int
}

// HandlerWithRealtime adds one-time ticket issuance and a write-only
// WebSocket connection. Message delivery will be attached in a later slice.
func HandlerWithRealtime(base http.Handler, authenticator Authenticator, authorizer RealtimeAuthorizer,
	tickets RealtimeTicketStore, shutdown context.Context) (http.Handler, error) {
	if base == nil || authenticator == nil || authorizer == nil || tickets == nil || shutdown == nil {
		return nil, errors.New("realtime handler requires base, authentication, authorization, tickets and shutdown context")
	}
	h := &realtimeHandler{base: base, authenticator: authenticator, authorizer: authorizer,
		tickets: tickets, shutdown: shutdown, checkInterval: 5 * time.Second,
		connections: make(map[string]int)}
	return h, nil
}

func (h *realtimeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v1/realtime/tickets":
		h.serveTicket(w, r)
	case "/api/v1/realtime":
		h.serveSocket(w, r)
	default:
		h.base.ServeHTTP(w, r)
	}
}

func (h *realtimeHandler) serveTicket(w http.ResponseWriter, r *http.Request) {
	id, ok := authenticateAdmin(w, r, h.authenticator)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if r.URL.RawQuery != "" || requestHasBody(w, r) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.authorizer.AuthorizeRealtime(r.Context(), id, policystore.RealtimeTicket); err != nil {
		writeRealtimeAuthError(w, err)
		return
	}
	ticket, err := h.tickets.Issue(r.Context(), id)
	if err != nil {
		slog.Error("realtime ticket issue failed", "error", err)
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	writeAdminJSON(w, http.StatusOK, struct {
		Ticket           string `json:"ticket"`
		ExpiresInSeconds int    `json:"expires_in_seconds"`
	}{ticket, int(realtime.TicketTTL / time.Second)})
}

func requestHasBody(w http.ResponseWriter, r *http.Request) bool {
	if r.Body == nil {
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
	return err != nil || len(body) != 0
}

func ticketFromProtocols(r *http.Request) (string, bool) {
	protocols := r.Header.Values("Sec-WebSocket-Protocol")
	if len(protocols) != 1 {
		return "", false
	}
	parts := strings.Split(protocols[0], ",")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) != realtimeSubprotocol {
		return "", false
	}
	secret := strings.TrimSpace(parts[1])
	if !strings.HasPrefix(secret, "ticket.") {
		return "", false
	}
	ticket := strings.TrimPrefix(secret, "ticket.")
	if _, err := realtime.TicketKey(ticket); err != nil {
		return "", false
	}
	return ticket, true
}

func (h *realtimeHandler) serveSocket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" ||
		!strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || requestHasBody(w, r) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	ticket, ok := ticketFromProtocols(r)
	if !ok {
		rejectAdmin(w, r, http.StatusUnauthorized, "invalid_ticket")
		return
	}
	id, err := h.tickets.Consume(r.Context(), ticket)
	if errors.Is(err, realtime.ErrInvalidTicket) {
		rejectAdmin(w, r, http.StatusUnauthorized, "invalid_ticket")
		return
	}
	if err != nil {
		slog.Error("realtime ticket consume failed", "error", err)
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if err := h.authorizer.AuthorizeRealtime(r.Context(), id, policystore.RealtimeConnect); err != nil {
		writeRealtimeAuthError(w, err)
		return
	}
	if !h.claim(id) {
		rejectAdmin(w, r, http.StatusTooManyRequests, "rate_limited")
		return
	}
	defer h.release(id)
	w.Header().Set("Cache-Control", "no-store")
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{realtimeSubprotocol}})
	if err != nil {
		return // Accept already wrote the handshake error.
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1024)
	ctx := conn.CloseRead(h.shutdown)
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = conn.Write(writeCtx, websocket.MessageText, []byte(`{"type":"ready","resync_required":true}`))
	cancel()
	if err != nil {
		return
	}
	interval := h.checkInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	checkTicker := time.NewTicker(interval)
	defer checkTicker.Stop()
	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-checkTicker.C:
			checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			active, err := h.authorizer.RealtimeIdentityActive(checkCtx, id)
			cancel()
			if err != nil {
				conn.Close(websocket.StatusInternalError, "identity check unavailable")
				return
			}
			if !active {
				conn.Close(websocket.StatusPolicyViolation, "identity expired")
				return
			}
		case <-pingTicker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func (h *realtimeHandler) claim(id access.TrustedIdentity) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.connections == nil {
		h.connections = make(map[string]int)
	}
	key := strings.ToLower(id.TenantID + ":" + id.UserID)
	if h.total >= maxRealtimeConnections || h.connections[key] >= maxRealtimeConnectionsPerUser {
		return false
	}
	h.connections[key]++
	h.total++
	return true
}

func (h *realtimeHandler) release(id access.TrustedIdentity) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := strings.ToLower(id.TenantID + ":" + id.UserID)
	if h.connections[key] > 0 {
		h.connections[key]--
		h.total--
		if h.connections[key] == 0 {
			delete(h.connections, key)
		}
	}
}

func writeRealtimeAuthError(w http.ResponseWriter, err error) {
	if errors.Is(err, policystore.ErrForbidden) {
		writeAdminError(w, http.StatusForbidden, "invalid_identity")
		return
	}
	slog.Error("realtime identity verification failed", "error", err)
	writeAdminError(w, http.StatusServiceUnavailable, "unavailable")
}
