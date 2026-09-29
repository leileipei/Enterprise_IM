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
