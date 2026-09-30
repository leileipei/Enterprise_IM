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
	"unicode"
	"unicode/utf8"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func createGroup(w http.ResponseWriter, r *http.Request, identity access.TrustedIdentity, service ConversationService) {
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
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil || !utf8.Valid(raw) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	var body struct {
		ClientRequestID     string   `json:"client_request_id"`
		Name                string   `json:"name"`
		MemberMembershipIDs []string `json:"member_membership_ids"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || !validUUID(body.ClientRequestID) ||
		len(body.MemberMembershipIDs) < 1 || len(body.MemberMembershipIDs) > 20 {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || utf8.RuneCountInString(name) > 120 {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	seen := map[string]bool{strings.ToLower(identity.ActingMembershipID): true}
	for _, membershipID := range body.MemberMembershipIDs {
		if !validUUID(membershipID) || seen[strings.ToLower(membershipID)] {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
		seen[strings.ToLower(membershipID)] = true
	}
	group, err := service.CreateGroup(r.Context(), identity, policystore.CreateGroupRequest{
		ClientRequestID: body.ClientRequestID, Name: name,
		MemberMembershipIDs: body.MemberMembershipIDs,
	})
	if err != nil {
		writeGroupCreateError(w, err)
		return
	}
	status := http.StatusCreated
	if !group.Created {
		status = http.StatusOK
	}
	writeAdminJSON(w, status, struct {
		ID            string `json:"id"`
		Type          string `json:"type"`
		LastSeq       int64  `json:"last_seq"`
		PolicyVersion int64  `json:"policy_version"`
		MemberCount   int    `json:"member_count"`
	}{group.ID, "group", group.LastSeq, group.PolicyVersion, group.MemberCount})
}

func writeGroupCreateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, policystore.ErrInvalidGroupRequest):
		writeAdminError(w, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, policystore.ErrForbidden):
		writeAdminError(w, http.StatusForbidden, "invalid_identity")
	case errors.Is(err, policystore.ErrGroupNotAvailable):
		writeAdminError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, policystore.ErrGroupRequestConflict):
		writeAdminError(w, http.StatusConflict, "idempotency_conflict")
	default:
		slog.Error("group creation unavailable", "error", err)
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable")
	}
}

func groupMembershipRoute(w http.ResponseWriter, r *http.Request, identity access.TrustedIdentity, service ConversationService) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/groups/"), "/")
	if len(parts) != 2 || !validUUID(parts[0]) {
		rejectAdmin(w, r, http.StatusNotFound, "not_found")
		return
	}
	switch parts[1] {
	case "membership":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		getOwnGroupMembership(w, r, identity, parts[0], service)
	case "leave":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		leaveGroup(w, r, identity, parts[0], service)
	case "invitations":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		inviteGroupMember(w, r, identity, parts[0], service)
	default:
		rejectAdmin(w, r, http.StatusNotFound, "not_found")
	}
}

func getOwnGroupMembership(w http.ResponseWriter, r *http.Request, identity access.TrustedIdentity, groupID string, service ConversationService) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	if r.Body != nil {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
		if err != nil || len(body) != 0 {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	result, err := service.GetOwnGroupMembership(r.Context(), identity, groupID)
	if err != nil {
		writeGroupMembershipError(w, err)
		return
	}
	writeAdminJSON(w, http.StatusOK, struct {
		IntervalID  string `json:"interval_id"`
		Role        string `json:"role"`
		JoinSeq     int64  `json:"join_seq"`
		GroupStatus string `json:"group_status"`
	}{result.IntervalID, result.Role, result.JoinSeq, result.GroupStatus})
}

func leaveGroup(w http.ResponseWriter, r *http.Request, identity access.TrustedIdentity, groupID string, service ConversationService) {
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
	var body struct {
		IntervalID string `json:"interval_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || !validUUID(body.IntervalID) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	result, err := service.LeaveGroup(r.Context(), identity, groupID, body.IntervalID)
	if err != nil {
		writeGroupMembershipError(w, err)
		return
	}
	writeAdminJSON(w, http.StatusOK, struct {
		IntervalID string `json:"interval_id"`
		Status     string `json:"status"`
		LeaveSeq   int64  `json:"leave_seq"`
	}{result.IntervalID, result.Status, result.LeaveSeq})
}

func writeGroupMembershipError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, policystore.ErrInvalidGroupMembershipRequest):
		writeAdminError(w, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, policystore.ErrForbidden):
		writeAdminError(w, http.StatusForbidden, "invalid_identity")
	case errors.Is(err, policystore.ErrGroupNotAvailable):
		writeAdminError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, policystore.ErrGroupOwnerTransferRequired):
		writeAdminError(w, http.StatusConflict, "owner_transfer_required")
	default:
		slog.Error("group membership unavailable", "error", err)
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable")
	}
}
