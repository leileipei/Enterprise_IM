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
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type ConversationService interface {
	StartDirectConversation(context.Context, access.TrustedIdentity, string) (policystore.DirectConversation, error)
	CreateGroup(context.Context, access.TrustedIdentity, policystore.CreateGroupRequest) (policystore.GroupConversation, error)
	GetOwnGroupMembership(context.Context, access.TrustedIdentity, string) (policystore.GroupMembership, error)
	LeaveGroup(context.Context, access.TrustedIdentity, string, string) (policystore.GroupLeaveResult, error)
	InviteGroupMember(context.Context, access.TrustedIdentity, string, policystore.InviteGroupRequest) (policystore.GroupInvitation, error)
	RemoveGroupMember(context.Context, access.TrustedIdentity, string, string) (policystore.GroupRemoveResult, error)
	TransferGroupOwner(context.Context, access.TrustedIdentity, string, policystore.GroupOwnerTransferRequest) (policystore.GroupOwnerTransfer, error)
	ListDirectConversations(context.Context, access.TrustedIdentity, string, int) (policystore.ConversationListPage, error)
	SendTextMessage(context.Context, access.TrustedIdentity, string, string, string) (policystore.MessageACK, error)
	PullTextMessages(context.Context, access.TrustedIdentity, string, int64, int) (policystore.MessagePage, error)
}

// HandlerWithConversations exposes direct conversation and message routes after
// a configured authenticator has verified the caller's access token.
func HandlerWithConversations(base http.Handler, authenticator Authenticator, conversations ConversationService) (http.Handler, error) {
	if base == nil || authenticator == nil || conversations == nil {
		return nil, errors.New("base handler, authentication and conversation service are required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/groups" && !strings.HasPrefix(r.URL.Path, "/api/v1/groups/") && r.URL.Path != "/api/v1/conversations" &&
			!strings.HasPrefix(r.URL.Path, "/api/v1/conversations/") {
			base.ServeHTTP(w, r)
			return
		}
		identity, ok := authenticateAdmin(w, r, authenticator)
		if !ok {
			return
		}
		if r.URL.Path == "/api/v1/groups" {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			createGroup(w, r, identity, conversations)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/groups/") {
			groupMembershipRoute(w, r, identity, conversations)
			return
		}
		if r.URL.Path != "/api/v1/conversations" {
			conversationID, ok := messageConversationID(r.URL.Path)
			if !ok {
				rejectAdmin(w, r, http.StatusNotFound, "not_found")
				return
			}
			switch r.Method {
			case http.MethodGet:
				pullTextMessages(w, r, identity, conversationID, conversations)
			case http.MethodPost:
				sendTextMessage(w, r, identity, conversationID, conversations)
			default:
				w.Header().Set("Allow", "GET, POST")
				rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
			}
			return
		}
		switch r.Method {
		case http.MethodGet:
			listDirectConversations(w, r, identity, conversations)
		case http.MethodPost:
			startDirectConversation(w, r, identity, conversations)
		default:
			w.Header().Set("Allow", "GET, POST")
			rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
		}
	}), nil
}

func listDirectConversations(w http.ResponseWriter, r *http.Request, identity access.TrustedIdentity, service ConversationService) {
	if r.URL.ForceQuery {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) > 2 || len(query["limit"]) > 1 || len(query["cursor"]) > 1 {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	for key := range query {
		if key != "limit" && key != "cursor" {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	limit := 20
	if value, ok := query["limit"]; ok {
		if !decimalDigits(value[0]) {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
		limit, err = strconv.Atoi(value[0])
		if err != nil || limit < 1 || limit > 50 {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	cursor := ""
	if value, ok := query["cursor"]; ok {
		cursor = value[0]
		if cursor == "" || len(cursor) > 256 {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	if r.Body != nil {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
		if err != nil || len(body) != 0 {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	page, err := service.ListDirectConversations(r.Context(), identity, cursor, limit)
	if err != nil {
		switch {
		case errors.Is(err, policystore.ErrInvalidConversationListRequest):
			writeAdminError(w, http.StatusBadRequest, "invalid_request")
		case errors.Is(err, policystore.ErrForbidden):
			writeAdminError(w, http.StatusForbidden, "invalid_identity")
		default:
			slog.ErrorContext(r.Context(), "conversation list unavailable", "error", err)
			writeAdminError(w, http.StatusServiceUnavailable, "unavailable")
		}
		return
	}
	type itemDTO struct {
		ID               string `json:"id"`
		Type             string `json:"type"`
		LastSeq          int64  `json:"last_seq"`
		UpdatedAt        string `json:"updated_at"`
		PeerVisible      bool   `json:"peer_visible"`
		DisplayName      string `json:"display_name,omitempty"`
		OrganizationName string `json:"organization_name,omitempty"`
	}
	items := make([]itemDTO, 0, len(page.Conversations))
	for _, conversation := range page.Conversations {
		item := itemDTO{ID: conversation.ID, Type: "direct", LastSeq: conversation.LastSeq,
			UpdatedAt:   conversation.UpdatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
			PeerVisible: conversation.PeerVisible}
		if conversation.PeerVisible {
			item.DisplayName = conversation.PeerDisplayName
			item.OrganizationName = conversation.PeerOrganizationName
		}
		items = append(items, item)
	}
	writeAdminJSON(w, http.StatusOK, struct {
		Conversations []itemDTO `json:"conversations"`
		HasMore       bool      `json:"has_more"`
		NextCursor    string    `json:"next_cursor,omitempty"`
	}{items, page.HasMore, page.NextCursor})
}

func messageConversationID(path string) (string, bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/api/v1/conversations/"), "/")
	if len(parts) != 2 || parts[1] != "messages" || !validUUID(parts[0]) {
		return "", false
	}
	return parts[0], true
}

func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func pullTextMessages(w http.ResponseWriter, r *http.Request, identity access.TrustedIdentity,
	conversationID string, conversations ConversationService) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) < 1 || len(query) > 2 || len(query["after_seq"]) != 1 ||
		!decimalDigits(query.Get("after_seq")) || len(query["limit"]) > 1 {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	for key := range query {
		if key != "after_seq" && key != "limit" {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	afterSeq, err := strconv.ParseInt(query.Get("after_seq"), 10, 64)
	if err != nil {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	limit := 100
	if value, ok := query["limit"]; ok {
		if !decimalDigits(value[0]) {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
		limit, err = strconv.Atoi(value[0])
		if err != nil || limit < 1 || limit > 500 {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	if r.Body != nil {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
		if err != nil || len(body) != 0 {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	page, err := conversations.PullTextMessages(r.Context(), identity, conversationID, afterSeq, limit)
	if err != nil {
		writeMessageError(w, err)
		return
	}
	type itemDTO struct {
		MessageID    string `json:"message_id,omitempty"`
		Seq          int64  `json:"seq"`
		SenderUserID string `json:"sender_user_id,omitempty"`
		Text         string `json:"text,omitempty"`
		ServerTime   string `json:"server_time,omitempty"`
		Redacted     bool   `json:"redacted,omitempty"`
	}
	items := make([]itemDTO, 0, len(page.Messages))
	for _, message := range page.Messages {
		item := itemDTO{Seq: message.Seq, Redacted: message.Redacted}
		if !message.Redacted {
			item.MessageID = message.MessageID
			item.SenderUserID = message.SenderUserID
			item.Text = message.Text
			item.ServerTime = message.ServerTime.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
		}
		items = append(items, item)
	}
	writeAdminJSON(w, http.StatusOK, struct {
		ConversationID string    `json:"conversation_id"`
		Messages       []itemDTO `json:"messages"`
		NextAfterSeq   int64     `json:"next_after_seq"`
		HasMore        bool      `json:"has_more"`
	}{page.ConversationID, items, page.NextAfterSeq, page.HasMore})
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
