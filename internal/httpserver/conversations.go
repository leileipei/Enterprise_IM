package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type ConversationService interface {
	StartDirectConversation(context.Context, access.TrustedIdentity, string) (policystore.DirectConversation, error)
	SendTextMessage(context.Context, access.TrustedIdentity, string, string, string) (policystore.MessageACK, error)
}

// HandlerWithConversations exposes direct conversation and message routes after
// a configured authenticator has verified the caller's access token.
func HandlerWithConversations(base http.Handler, authenticator Authenticator, conversations ConversationService) (http.Handler, error) {
	if base == nil || authenticator == nil || conversations == nil {
		return nil, errors.New("base handler, authentication and conversation service are required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/conversations" && !strings.HasPrefix(r.URL.Path, "/api/v1/conversations/") {
			base.ServeHTTP(w, r)
			return
		}
		identity, ok := authenticateAdmin(w, r, authenticator)
		if !ok {
			return
		}
		if r.URL.Path != "/api/v1/conversations" {
			conversationID, ok := messageConversationID(r.URL.Path)
			if !ok {
				rejectAdmin(w, r, http.StatusNotFound, "not_found")
				return
			}
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			sendTextMessage(w, r, identity, conversationID, conversations)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		startDirectConversation(w, r, identity, conversations)
	}), nil
}

func messageConversationID(path string) (string, bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/api/v1/conversations/"), "/")
	if len(parts) != 2 || parts[1] != "messages" || !validUUID(parts[0]) {
		return "", false
	}
	return parts[0], true
}

func sendTextMessage(w http.ResponseWriter, r *http.Request, identity access.TrustedIdentity,
	conversationID string, conversations ConversationService) {
	if r.URL.RawQuery != "" || r.Body == nil {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" ||
		(len(params) > 0 && (len(params) != 1 || !strings.EqualFold(params["charset"], "utf-8"))) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	var body struct {
		ClientMessageID string `json:"client_msg_id"`
		Text            string `json:"text"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128*1024))
	if err != nil || !utf8.Valid(raw) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || !validUUID(body.ClientMessageID) ||
		!utf8.ValidString(body.Text) || strings.TrimSpace(body.Text) == "" ||
		strings.ContainsRune(body.Text, 0) || len(body.Text) > 16*1024 {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	ack, err := conversations.SendTextMessage(r.Context(), identity, conversationID, body.ClientMessageID, body.Text)
	if err != nil {
		writeMessageError(w, err)
		return
	}
	writeAdminJSON(w, http.StatusOK, struct {
		MessageID      string `json:"message_id"`
		ConversationID string `json:"conversation_id"`
		Seq            int64  `json:"seq"`
		ServerTime     string `json:"server_time"`
	}{ack.MessageID, ack.ConversationID, ack.Seq, ack.ServerTime.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")})
}

func writeMessageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, policystore.ErrInvalidMessageRequest),
		errors.Is(err, policystore.ErrInvalidTextMessage),
		errors.Is(err, policystore.ErrInvalidClientMessageID):
		writeAdminError(w, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, policystore.ErrRetryExpired):
		writeAdminError(w, http.StatusGone, "retry_window_expired")
	case errors.Is(err, policystore.ErrForbidden):
		writeAdminError(w, http.StatusForbidden, "invalid_identity")
	case errors.Is(err, policystore.ErrMessageNotAvailable):
		writeAdminError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, policystore.ErrIdempotencyConflict):
		writeAdminError(w, http.StatusConflict, "idempotency_conflict")
	case errors.Is(err, policystore.ErrConversationContextChanged):
		writeAdminError(w, http.StatusConflict, "conversation_context_changed")
	case errors.Is(err, policystore.ErrMessageRateLimited):
		writeAdminError(w, http.StatusTooManyRequests, "rate_limited")
	default:
		slog.Error("message service unavailable", "error", err)
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable")
	}
}

func startDirectConversation(w http.ResponseWriter, r *http.Request, identity access.TrustedIdentity, conversations ConversationService) {
	if r.URL.RawQuery != "" || r.Body == nil {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" ||
		(len(params) > 0 && (len(params) != 1 || !strings.EqualFold(params["charset"], "utf-8"))) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	var body struct {
		TargetMembershipID string `json:"target_membership_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || !validUUID(body.TargetMembershipID) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	chat, err := conversations.StartDirectConversation(r.Context(), identity, body.TargetMembershipID)
	if err != nil {
		writeConversationError(w, err)
		return
	}
	writeAdminJSON(w, http.StatusOK, directConversationDTO{ID: chat.ID, Type: "direct",
		LastSeq: chat.LastSeq, PolicyVersion: chat.PolicyVersion,
		CrossLegal: chat.CrossLegal, DecisionReason: string(chat.DecisionReason)})
}

func writeConversationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, policystore.ErrInvalidChatTarget):
		writeAdminError(w, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, policystore.ErrForbidden):
		writeAdminError(w, http.StatusForbidden, "invalid_identity")
	case errors.Is(err, policystore.ErrChatNotAvailable):
		writeAdminError(w, http.StatusNotFound, "not_found")
	default:
		slog.Error("conversation service unavailable", "error", err)
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable")
	}
}

type directConversationDTO struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	LastSeq        int64  `json:"last_seq"`
	PolicyVersion  int64  `json:"policy_version"`
	CrossLegal     bool   `json:"cross_legal"`
	DecisionReason string `json:"decision_reason"`
}
