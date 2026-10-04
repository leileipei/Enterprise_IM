package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type FileMetadataService interface {
	ReserveFile(context.Context, access.TrustedIdentity, files.CreateParams) (files.Reservation, error)
	GetOwnFile(context.Context, access.TrustedIdentity, string) (files.Metadata, error)
}
type FileStatusDTO struct {
	FileID            string      `json:"file_id"`
	ConversationID    string      `json:"conversation_id"`
	OriginalFilename  string      `json:"original_filename"`
	DeclaredMediaType string      `json:"declared_media_type"`
	DeclaredSizeBytes string      `json:"declared_size_bytes"`
	State             files.State `json:"state"`
	StateVersion      string      `json:"state_version"`
	CreatedAt         string      `json:"created_at"`
	UploadExpiresAt   string      `json:"upload_expires_at"`
}

func fileStatus(m files.Metadata) FileStatusDTO {
	return FileStatusDTO{m.ID, m.ConversationID, m.OriginalFilename, m.DeclaredMediaType, strconv.FormatInt(m.DeclaredSizeBytes, 10), m.State, strconv.FormatInt(m.StateVersion, 10), m.CreatedAt.UTC().Format(time.RFC3339), m.UploadExpiresAt.UTC().Format(time.RFC3339)}
}
func writeFileError(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, files.ErrInvalidIdentity):
		writeAdminError(w, 403, "invalid_identity")
	case errors.Is(e, files.ErrFileNotFound):
		writeAdminError(w, 404, "file_not_available")
	case errors.Is(e, files.ErrInvalidMetadata):
		writeAdminError(w, 400, "invalid_file_request")
	case errors.Is(e, files.ErrUploadConflict):
		writeAdminError(w, 409, "file_request_conflict")
	case errors.Is(e, files.ErrAlreadyUploaded):
		writeAdminError(w, 409, "file_already_uploaded")
	case errors.Is(e, files.ErrUploadBusy):
		writeAdminError(w, 409, "upload_in_progress")
	case errors.Is(e, files.ErrRecoveryPending):
		writeAdminError(w, 503, "upload_recovery_pending")
	case errors.Is(e, files.ErrUploadExpired):
		writeAdminError(w, 410, "upload_expired")
	case errors.Is(e, files.ErrFileTooLarge):
		writeAdminError(w, 413, "file_too_large")
	case errors.Is(e, files.ErrFileTypeNotAllowed):
		writeAdminError(w, 415, "file_type_not_allowed")
	case errors.Is(e, files.ErrStorageBudgetExceeded):
		writeAdminError(w, 409, "storage_budget_exceeded")
	case errors.Is(e, files.ErrUploadDisabled):
		writeAdminError(w, 409, "file_upload_disabled")
	default:
		writeAdminError(w, 503, "file_service_unavailable")
	}
}
func HandlerWithFileMetadata(next http.Handler, auth Authenticator, svc FileMetadataService) (http.Handler, error) {
	if next == nil || auth == nil || svc == nil {
		return nil, errors.New("file metadata requires handler, authenticator and service")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		reserve := len(parts) == 6 && parts[1] == "api" && parts[2] == "v1" && parts[3] == "conversations" && parts[5] == "files"
		status := len(parts) == 5 && parts[1] == "api" && parts[2] == "v1" && parts[3] == "files"
		if !reserve && !status {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		id, ok := authenticateAdmin(w, r, auth)
		if !ok {
			return
		}
		if r.URL.RawQuery != "" || r.Header.Get("Content-Encoding") != "" {
			rejectAdmin(w, r, 400, "invalid_request")
			return
		}
		want := http.MethodGet
		if reserve {
			want = http.MethodPost
		}
		if r.Method != want {
			w.Header().Set("Allow", want)
			rejectAdmin(w, r, 405, "method_not_allowed")
			return
		}
		if status {
			b, e := io.ReadAll(io.LimitReader(r.Body, 1))
			if e != nil || len(b) != 0 {
				rejectAdmin(w, r, 400, "unexpected_body")
				return
			}
			m, e := svc.GetOwnFile(r.Context(), id, parts[4])
			if e != nil {
				writeFileError(w, e)
				return
			}
			writeAdminJSON(w, 200, fileStatus(m))
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			rejectAdmin(w, r, 400, "invalid_content_type")
			return
		}
		v, e := decodeFileObject(w, r, []string{"upload_request_id", "original_filename", "declared_media_type", "declared_size_bytes"}, 4096)
		if e != nil {
			rejectAdmin(w, r, 400, "invalid_request")
			return
		}
		p := files.CreateParams{TenantID: id.TenantID, UploaderUserID: id.UserID, UploaderMembershipID: id.ActingMembershipID, ConversationID: parts[4]}
		for _, f := range []struct {
			k string
			p *string
		}{{"upload_request_id", &p.UploadRequestID}, {"original_filename", &p.OriginalFilename}, {"declared_media_type", &p.DeclaredMediaType}} {
			if e = json.Unmarshal(v[f.k], f.p); e != nil || strings.ContainsRune(*f.p, utf8.RuneError) {
				rejectAdmin(w, r, 400, "invalid_request")
				return
			}
		}
		p.DeclaredSizeBytes, e = fileDecimal(v["declared_size_bytes"])
		if e != nil {
			rejectAdmin(w, r, 400, "invalid_request")
			return
		}
		p, e = files.NormalizeCreate(p)
		if e != nil {
			rejectAdmin(w, r, 400, "invalid_request")
			return
		}
		result, e := svc.ReserveFile(r.Context(), id, p)
		if e != nil {
			writeFileError(w, e)
			return
		}
		code := 201
		if result.Duplicate {
			code = 200
		}
		writeAdminJSON(w, code, fileStatus(result.File))
	}), nil
}
