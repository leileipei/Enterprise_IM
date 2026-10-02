package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/leileipei/Enterprise_IM/internal/access"
)

type RetentionPolicyService interface {
	GetRetentionPolicy(context.Context, access.TrustedIdentity) (access.RetentionPolicy, error)
	SetRetentionPolicy(context.Context, access.TrustedIdentity, int64, int, string) (access.RetentionPolicy, error)
}

type retentionPolicyDTO struct {
	MessageBodyDays   int        `json:"message_body_days"`
	Version           int64      `json:"version"`
	ApprovalReference string     `json:"approval_reference"`
	ApprovedByUserID  string     `json:"approved_by_user_id"`
	ApprovedAt        *time.Time `json:"approved_at"`
}

type retentionPolicyInput struct {
	MessageBodyDays   int
	ExpectedVersion   int64
	ApprovalReference string
}

func decodeRetentionPolicyInput(w http.ResponseWriter, r *http.Request) (retentionPolicyInput, error) {
	var input retentionPolicyInput
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil || !utf8.Valid(body) {
		return input, errors.New("invalid retention request body")
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return input, errors.New("retention request must be an object")
	}
	fields := make(map[string]json.RawMessage, 3)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return input, err
		}
		key, ok := token.(string)
		if !ok || (key != "message_body_days" && key != "expected_version" && key != "approval_reference") {
			return input, errors.New("unknown retention request field")
		}
		if _, exists := fields[key]; exists {
			return input, errors.New("duplicate retention request field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return input, err
		}
		fields[key] = value
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || len(fields) != 3 {
		return input, errors.New("incomplete retention request")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return input, errors.New("trailing retention request data")
	}
	for _, key := range []string{"message_body_days", "expected_version", "approval_reference"} {
		if string(fields[key]) == "null" {
			return input, errors.New("null retention request field")
		}
	}
	if err := json.Unmarshal(fields["message_body_days"], &input.MessageBodyDays); err != nil {
		return input, err
	}
	if err := json.Unmarshal(fields["expected_version"], &input.ExpectedVersion); err != nil {
		return input, err
	}
	if err := json.Unmarshal(fields["approval_reference"], &input.ApprovalReference); err != nil ||
		strings.ContainsRune(input.ApprovalReference, utf8.RuneError) {
		return input, errors.New("invalid approval reference")
	}
	return input, nil
}

func retentionDTO(policy access.RetentionPolicy) retentionPolicyDTO {
	return retentionPolicyDTO{MessageBodyDays: policy.MessageBodyDays, Version: policy.Version,
		ApprovalReference: policy.ApprovalReference, ApprovedByUserID: policy.ApprovedByUserID,
		ApprovedAt: policy.ApprovedAt}
}

func HandlerWithRetentionPolicy(next http.Handler, authenticator Authenticator,
	service RetentionPolicyService) (http.Handler, error) {
	if next == nil || authenticator == nil || service == nil {
		return nil, errors.New("retention policy route requires handler, authentication and service")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/retention-policy" {
			next.ServeHTTP(w, r)
			return
		}
		id, ok := authenticateAdmin(w, r, authenticator)
		if !ok {
			return
		}
		if r.URL.RawQuery != "" {
			rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
		switch r.Method {
		case http.MethodGet:
			body, err := io.ReadAll(io.LimitReader(r.Body, 1))
			if err != nil || len(body) > 0 {
				rejectAdmin(w, r, http.StatusBadRequest, "unexpected_body")
				return
			}
			policy, err := service.GetRetentionPolicy(r.Context(), id)
			if err != nil {
				writeServiceError(w, err)
				return
			}
			writeAdminJSON(w, http.StatusOK, retentionDTO(policy))
		case http.MethodPut:
			mediaType, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
			if strings.TrimSpace(mediaType) != "application/json" {
				rejectAdmin(w, r, http.StatusBadRequest, "invalid_content_type")
				return
			}
			input, err := decodeRetentionPolicyInput(w, r)
			if err != nil {
				rejectAdmin(w, r, http.StatusBadRequest, "invalid_request")
				return
			}
			policy, err := service.SetRetentionPolicy(r.Context(), id, input.ExpectedVersion,
				input.MessageBodyDays, input.ApprovalReference)
			if err != nil {
				if errors.Is(err, access.ErrInvalidRetentionPolicy) {
					rejectAdmin(w, r, http.StatusBadRequest, "invalid_retention_policy")
				} else {
					writeServiceError(w, err)
				}
				return
			}
			writeAdminJSON(w, http.StatusOK, retentionDTO(policy))
		default:
			w.Header().Set("Allow", "GET, PUT")
			rejectAdmin(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
		}
	}), nil
}
