package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type ConversationService interface {
	StartDirectConversation(context.Context, access.TrustedIdentity, string) (policystore.DirectConversation, error)
}

// HandlerWithConversations exposes direct conversation creation only after
// a configured authenticator has verified the caller's access token.
func HandlerWithConversations(base http.Handler, authenticator Authenticator, conversations ConversationService) (http.Handler, error) {
	if base == nil || authenticator == nil || conversations == nil {
		return nil, errors.New("base handler, authentication and conversation service are required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/conversations" {
			base.ServeHTTP(w, r)
			return
		}
		identity, ok := authenticateAdmin(w, r, authenticator)
		if !ok {
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
