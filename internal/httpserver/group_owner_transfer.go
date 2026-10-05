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

func transferGroupOwner(w http.ResponseWriter, r *http.Request, id access.TrustedIdentity, groupID string, service ConversationService) {
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
	request, ok := parseGroupOwnerTransferBody(raw)
	if !ok {
		rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	result, err := service.TransferGroupOwner(r.Context(), id, groupID, request)
	if err != nil {
		writeGroupOwnerTransferError(w, err)
		return
	}
	status := http.StatusCreated
	if !result.Created {
		status = http.StatusOK
	}
	writeAdminJSON(w, status, struct {
		SourceIntervalID string `json:"source_interval_id"`
		TargetIntervalID string `json:"target_interval_id"`
	}{result.SourceIntervalID, result.TargetIntervalID})
}

func parseGroupOwnerTransferBody(raw []byte) (policystore.GroupOwnerTransferRequest, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return policystore.GroupOwnerTransferRequest{}, false
	}
	var body policystore.GroupOwnerTransferRequest
	seen := make(map[string]bool, 3)
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok || seen[key] {
			return policystore.GroupOwnerTransferRequest{}, false
		}
		seen[key] = true
		switch key {
		case "client_request_id":
			err = decoder.Decode(&body.ClientRequestID)
		case "source_interval_id":
			err = decoder.Decode(&body.SourceIntervalID)
		case "target_interval_id":
			err = decoder.Decode(&body.TargetIntervalID)
		default:
			return policystore.GroupOwnerTransferRequest{}, false
		}
		if err != nil {
			return policystore.GroupOwnerTransferRequest{}, false
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || !validUUID(body.ClientRequestID) ||
		!validUUID(body.SourceIntervalID) || !validUUID(body.TargetIntervalID) {
		return policystore.GroupOwnerTransferRequest{}, false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return policystore.GroupOwnerTransferRequest{}, false
	}
	return body, true
}

func writeGroupOwnerTransferError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, policystore.ErrInvalidGroupOwnerTransferRequest),
		errors.Is(err, policystore.ErrInvalidGroupMembershipRequest):
		writeAdminError(w, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, policystore.ErrForbidden):
		writeAdminError(w, http.StatusForbidden, "invalid_identity")
	case errors.Is(err, policystore.ErrGroupOwnerTransferPermissionDenied):
		writeAdminError(w, http.StatusForbidden, "group_permission_denied")
	case errors.Is(err, policystore.ErrGroupNotAvailable):
		writeAdminError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, policystore.ErrGroupOwnerTransferConflict):
		writeAdminError(w, http.StatusConflict, "idempotency_conflict")
	default:
		slog.Error("group owner transfer unavailable", "error", err)
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable")
	}
}
