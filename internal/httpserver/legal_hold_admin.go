package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/leileipei/Enterprise_IM/internal/access"
)

type LegalHoldService interface {
	ListLegalHolds(context.Context, access.TrustedIdentity, string, string, int) (access.LegalHoldPage, error)
	PlaceLegalHold(context.Context, access.TrustedIdentity, string, string, string) (access.LegalHold, bool, error)
	ReleaseLegalHold(context.Context, access.TrustedIdentity, string, string, string, string) (access.LegalHold, error)
}

type legalHoldDTO struct {
	ID                       string     `json:"id"`
	ConversationID           string     `json:"conversation_id"`
	CaseReference            string     `json:"case_reference"`
	PlacedByUserID           string     `json:"placed_by_user_id"`
	PlacedByMembershipID     string     `json:"placed_by_membership_id"`
	PlacedAt                 time.Time  `json:"placed_at"`
	ReleaseApprovalReference string     `json:"release_approval_reference,omitempty"`
	ReleasedByUserID         string     `json:"released_by_user_id,omitempty"`
	ReleasedByMembershipID   string     `json:"released_by_membership_id,omitempty"`
	ReleasedAt               *time.Time `json:"released_at,omitempty"`
}

func legalHoldResponse(hold access.LegalHold) legalHoldDTO {
	return legalHoldDTO{ID: hold.ID, ConversationID: hold.ConversationID,
		CaseReference: hold.CaseReference, PlacedByUserID: hold.PlacedByUserID,
		PlacedByMembershipID: hold.PlacedByMembershipID, PlacedAt: hold.PlacedAt,
		ReleaseApprovalReference: hold.ReleaseApprovalReference,
		ReleasedByUserID:         hold.ReleasedByUserID,
		ReleasedByMembershipID:   hold.ReleasedByMembershipID,
		ReleasedAt:               hold.ReleasedAt}
}

func decodeLegalHoldInput(w http.ResponseWriter, r *http.Request, referenceKey string) (string, string, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil || !utf8.Valid(body) {
		return "", "", access.ErrInvalidLegalHold
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return "", "", access.ErrInvalidLegalHold
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return "", "", access.ErrInvalidLegalHold
		}
		key, ok := token.(string)
		if !ok || (key != "request_id" && key != referenceKey) {
			return "", "", access.ErrInvalidLegalHold
		}
		if _, exists := fields[key]; exists {
			return "", "", access.ErrInvalidLegalHold
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return "", "", access.ErrInvalidLegalHold
		}
		fields[key] = value
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || len(fields) != 2 {
		return "", "", access.ErrInvalidLegalHold
	}
	if _, err := decoder.Token(); err != io.EOF {
		return "", "", access.ErrInvalidLegalHold
	}
	if string(fields["request_id"]) == "null" || string(fields[referenceKey]) == "null" {
		return "", "", access.ErrInvalidLegalHold
	}
	var requestID, reference string
	if json.Unmarshal(fields["request_id"], &requestID) != nil ||
		json.Unmarshal(fields[referenceKey], &reference) != nil ||
		strings.ContainsRune(requestID, utf8.RuneError) ||
		strings.ContainsRune(reference, utf8.RuneError) ||
		!validUUID(requestID) || !utf8.ValidString(reference) {
		return "", "", access.ErrInvalidLegalHold
	}
	return requestID, reference, nil
}

func writeLegalHoldError(w http.ResponseWriter, err error) {
	if errors.Is(err, access.ErrFileCleanupInProgress) {
		writeAdminError(w, http.StatusConflict, "file_cleanup_in_progress")
	} else if errors.Is(err, access.ErrInvalidLegalHold) {
		writeAdminError(w, http.StatusBadRequest, "invalid_legal_hold")
	} else {
		writeServiceError(w, err)
	}
}

func HandlerWithLegalHolds(next http.Handler, authenticator Authenticator,
	service LegalHoldService) (http.Handler, error) {
	if next == nil || authenticator == nil || service == nil {
		return nil, errors.New("legal hold route requires handler, authentication and service")
	}
	const prefix = "/api/v1/admin/conversations/"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix) {
			next.ServeHTTP(w, r)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		parts := strings.Split(rest, "/")
		list := len(parts) == 2 && parts[1] == "legal-holds"
		release := len(parts) == 4 && parts[1] == "legal-holds" && parts[3] == "release"
		if !list && !release {
			next.ServeHTTP(w, r)
			return
		}
		id, ok := authenticateAdmin(w, r, authenticator)
		if !ok {
			return
		}
		if !validUUID(parts[0]) || (release && !validUUID(parts[2])) {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_id")
			return
		}
		conversationID := strings.ToLower(parts[0])
		if list && r.Method == http.MethodGet {
			body, err := io.ReadAll(io.LimitReader(r.Body, 1))
			if err != nil || len(body) != 0 {
				rejectAdmin(w, r, http.StatusBadRequest, "unexpected_body")
				return
			}
			params, err := url.ParseQuery(r.URL.RawQuery)
			if err != nil || len(params) > 2 {
				rejectAdmin(w, r, http.StatusBadRequest, "invalid_query")
				return
			}
			limit, cursor := 100, ""
			for key, values := range params {
				if len(values) != 1 || (key != "limit" && key != "cursor") {
					rejectAdmin(w, r, http.StatusBadRequest, "invalid_query")
					return
				}
				if key == "limit" {
					limit, err = strconv.Atoi(values[0])
					if err != nil || limit < 1 || limit > 500 {
						rejectAdmin(w, r, http.StatusBadRequest, "invalid_query")
						return
					}
				} else {
					cursor = values[0]
					if cursor == "" || len(cursor) > 1024 {
						rejectAdmin(w, r, http.StatusBadRequest, "invalid_query")
						return
					}
				}
			}
			page, err := service.ListLegalHolds(r.Context(), id, conversationID, cursor, limit)
			if err != nil {
				writeLegalHoldError(w, err)
				return
			}
			holds := make([]legalHoldDTO, 0, len(page.Holds))
			for _, hold := range page.Holds {
				holds = append(holds, legalHoldResponse(hold))
			}
			writeAdminJSON(w, http.StatusOK, struct {
				Holds      []legalHoldDTO `json:"holds"`
				NextCursor string         `json:"next_cursor"`
			}{Holds: holds, NextCursor: page.NextCursor})
			return
		}
		if r.Method != http.MethodPost {
			if list {
				w.Header().Set("Allow", "GET, POST")
			} else {
				w.Header().Set("Allow", "POST")
			}
			rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		if r.URL.RawQuery != "" {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_query")
			return
		}
		mediaType, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
		if strings.TrimSpace(mediaType) != "application/json" {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_content_type")
			return
		}
		referenceKey := "case_reference"
		if release {
			referenceKey = "approval_reference"
		}
		requestID, reference, err := decodeLegalHoldInput(w, r, referenceKey)
		if err != nil {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
		if list {
			hold, created, err := service.PlaceLegalHold(r.Context(), id, conversationID, requestID, reference)
			if err != nil {
				writeLegalHoldError(w, err)
				return
			}
			status := http.StatusOK
			if created {
				status = http.StatusCreated
			}
			writeAdminJSON(w, status, legalHoldResponse(hold))
			return
		}
		hold, err := service.ReleaseLegalHold(r.Context(), id, conversationID,
			strings.ToLower(parts[2]), requestID, reference)
		if err != nil {
			writeLegalHoldError(w, err)
			return
		}
		writeAdminJSON(w, http.StatusOK, legalHoldResponse(hold))
	}), nil
}
