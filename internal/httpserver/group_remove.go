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

func removeGroupMember(w http.ResponseWriter, r *http.Request, id access.TrustedIdentity, groupID string, service ConversationService) {
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
	intervalID, ok := parseGroupRemoveBody(raw)
	if !ok {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	result, err := service.RemoveGroupMember(r.Context(), id, groupID, intervalID)
	if err != nil {
		writeGroupRemoveError(w, err)
		return
	}
	writeAdminJSON(w, http.StatusOK, struct {
		IntervalID string `json:"interval_id"`
		Status     string `json:"status"`
		LeaveSeq   int64  `json:"leave_seq"`
	}{result.IntervalID, result.Status, result.LeaveSeq})
}

func parseGroupRemoveBody(raw []byte) (string, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') || !decoder.More() {
		return "", false
	}
	keyToken, err := decoder.Token()
	key, ok := keyToken.(string)
	if err != nil || !ok || key != "interval_id" {
		return "", false
	}
	var intervalID string
	if err := decoder.Decode(&intervalID); err != nil || !validUUID(intervalID) || decoder.More() {
		return "", false
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return "", false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", false
	}
	return intervalID, true
}

func writeGroupRemoveError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, policystore.ErrInvalidGroupMembershipRequest):
		writeAdminError(w, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, policystore.ErrForbidden):
		writeAdminError(w, http.StatusForbidden, "invalid_identity")
	case errors.Is(err, policystore.ErrGroupRemovePermissionDenied):
		writeAdminError(w, http.StatusForbidden, "group_permission_denied")
	case errors.Is(err, policystore.ErrGroupNotAvailable):
		writeAdminError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, policystore.ErrGroupOwnerTransferRequired):
		writeAdminError(w, http.StatusConflict, "owner_transfer_required")
	default:
		slog.Error("group member removal unavailable", "error", err)
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable")
	}
}
