package httpserver

import (
	"bytes"
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

func inviteGroupMember(w http.ResponseWriter, r *http.Request, id access.TrustedIdentity, groupID string, service ConversationService) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.Body == nil {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" ||
		(len(params) > 0 && (len(params) != 1 || !strings.EqualFold(params["charset"], "utf-8"))) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil || !utf8.Valid(raw) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	body, ok := parseGroupInviteBody(raw)
	if !ok {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	result, err := service.InviteGroupMember(r.Context(), id, groupID, body)
	if err != nil {
		writeGroupInviteError(w, err)
		return
	}
	status := http.StatusCreated
	if !result.Created {
		status = http.StatusOK
	}
	writeAdminJSON(w, status, struct {
		IntervalID    string `json:"interval_id"`
		JoinSeq       int64  `json:"join_seq"`
		PolicyVersion int64  `json:"policy_version"`
	}{result.IntervalID, result.JoinSeq, result.PolicyVersion})
}

func parseGroupInviteBody(raw []byte) (policystore.InviteGroupRequest, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return policystore.InviteGroupRequest{}, false
	}
	var body policystore.InviteGroupRequest
	seen := make(map[string]bool, 2)
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok || seen[key] {
			return policystore.InviteGroupRequest{}, false
		}
		seen[key] = true
		switch key {
		case "client_request_id":
			if err := decoder.Decode(&body.ClientRequestID); err != nil {
				return policystore.InviteGroupRequest{}, false
			}
		case "target_membership_id":
			if err := decoder.Decode(&body.TargetMembershipID); err != nil {
				return policystore.InviteGroupRequest{}, false
			}
		default:
			return policystore.InviteGroupRequest{}, false
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || !validUUID(body.ClientRequestID) || !validUUID(body.TargetMembershipID) {
		return policystore.InviteGroupRequest{}, false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return policystore.InviteGroupRequest{}, false
	}
	return body, true
}

func writeGroupInviteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, policystore.ErrInvalidGroupInviteRequest), errors.Is(err, policystore.ErrInvalidGroupMembershipRequest):
		writeAdminError(w, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, policystore.ErrForbidden):
		writeAdminError(w, http.StatusForbidden, "invalid_identity")
	case errors.Is(err, policystore.ErrGroupInvitePermissionDenied):
		writeAdminError(w, http.StatusForbidden, "group_permission_denied")
	case errors.Is(err, policystore.ErrGroupNotAvailable):
		writeAdminError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, policystore.ErrGroupPolicyBlocked):
		writeAdminError(w, http.StatusConflict, "group_policy_blocked")
	case errors.Is(err, policystore.ErrGroupInviteConflict):
		writeAdminError(w, http.StatusConflict, "idempotency_conflict")
	default:
		slog.Error("group invitation unavailable", "error", err)
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable")
	}
}
