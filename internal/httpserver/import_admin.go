package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	a "github.com/leileipei/Enterprise_IM/internal/importapply"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"
)

type ImportService interface {
	Preauthorize(context.Context, access.ImportPrincipal) error
	Apply(context.Context, access.ImportPrincipal, string, []byte) (a.Result, error)
	Get(context.Context, access.ImportPrincipal, string) (a.Result, error)
}

func HandlerWithImports(next http.Handler, auth Authenticator, service ImportService) (http.Handler, error) {
	if next == nil || ((auth == nil) != (service == nil)) {
		return nil, errors.New("invalid import dependencies")
	}
	slots := make(chan struct{}, 1)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		const prefix = "/api/admin/import-batches/"
		if r.URL.Path != "/api/admin/import-batches" && !strings.HasPrefix(r.URL.Path, prefix) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		var randomID [16]byte
		_, _ = rand.Read(randomID[:])
		safeID := hex.EncodeToString(randomID[:])
		r = r.WithContext(context.WithValue(r.Context(), importCorrelationKey{}, safeID))
		w.Header().Set("X-Request-ID", safeID)
		reject := func(status int, code string) { rejectImport(w, r, status, code) }
		if service == nil {
			reject(404, "IMPORT_DISABLED")
			return
		}
		authCtx, authCancel := context.WithDeadline(r.Context(), start.Add(30*time.Second))
		defer authCancel()
		verified, status, _ := bearerIdentity(authCtx, r, auth)
		if status != 0 {
			if status == 401 {
				w.Header().Set("WWW-Authenticate", "Bearer")
			}
			code := "IMPORT_UNAUTHORIZED"
			if status == 503 {
				code = "IMPORT_AUTH_UNAVAILABLE"
			}
			reject(status, code)
			return
		}
		if verified.ExpiresAt.IsZero() || !start.Before(verified.ExpiresAt) || verified.Issuer == "" || verified.Subject == "" {
			reject(401, "IMPORT_UNAUTHORIZED")
			return
		}
		ctx, cancel, e := a.RequestContext(authCtx, start, verified.ExpiresAt)
		if e != nil {
			reject(401, "IMPORT_UNAUTHORIZED")
			return
		}
		defer cancel()
		memberships := r.Header.Values("X-Acting-Membership-ID")
		if len(memberships) != 1 || !validUUID(memberships[0]) {
			reject(400, "IMPORT_INVALID_MEMBERSHIP")
			return
		}
		principal := access.ImportPrincipal{Identity: access.TrustedIdentity{TenantID: verified.TenantID, UserID: verified.UserID, ActingMembershipID: memberships[0]}, Issuer: verified.Issuer, Subject: verified.Subject, ExpiresAt: verified.ExpiresAt}
		if e = service.Preauthorize(ctx, principal); e != nil {
			importServiceError(w, r, e)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, prefix)
		if !validUUID(id) || r.URL.RawQuery != "" {
			reject(400, "IMPORT_INVALID_REQUEST_ID")
			return
		}
		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET, POST")
			reject(405, "IMPORT_METHOD_NOT_ALLOWED")
			return
		}
		var result a.Result
		if r.Method == http.MethodPost {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				reject(409, "IMPORT_BUSY")
				return
			}
			types := r.Header.Values("Content-Type")
			kind := ""
			if len(types) == 1 {
				kind, _, e = mime.ParseMediaType(types[0])
			}
			encodings := r.Header.Values("Content-Encoding")
			encodingOK := len(encodings) == 0 || len(encodings) == 1 && (encodings[0] == "" || strings.EqualFold(encodings[0], "identity"))
			if kind != "application/json" || e != nil || !encodingOK {
				reject(422, "IMPORT_INPUT_INVALID")
				return
			}
			if r.ContentLength > p.MaxInput {
				reject(413, "IMPORT_BODY_TOO_LARGE")
				return
			}
			if end, ok := ctx.Deadline(); ok {
				_ = http.NewResponseController(w).SetReadDeadline(end)
			}
			body := http.MaxBytesReader(w, r.Body, p.MaxInput)
			defer body.Close()
			raw, readErr := io.ReadAll(body)
			if readErr != nil {
				var limit *http.MaxBytesError
				if errors.As(readErr, &limit) {
					reject(413, "IMPORT_BODY_TOO_LARGE")
				} else {
					reject(503, "IMPORT_RETRYABLE")
				}
				return
			}
			if ctx.Err() != nil {
				reject(503, "IMPORT_RETRYABLE")
				return
			}
			result, e = service.Apply(ctx, principal, id, raw)
		} else {
			result, e = service.Get(ctx, principal, id)
		}
		if e != nil {
			importServiceError(w, r, e)
			return
		}
		status = 200
		if r.Method == http.MethodPost {
			if result.Receipt.State == a.Rejected {
				status = 409
			} else if !result.Replay {
				status = 201
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(result.Encoded)
	}), nil
}
func importServiceError(w http.ResponseWriter, r *http.Request, e error) {
	status := 503
	code := "IMPORT_DATABASE_UNAVAILABLE"
	switch {
	case errors.Is(e, a.ErrBusy):
		code = "IMPORT_BUSY"
		status = 409
		if r.Method == http.MethodGet {
			status = 202
		}
	case errors.Is(e, a.ErrNotRecorded):
		status = 404
		code = "IMPORT_NOT_RECORDED"
	case errors.Is(e, a.ErrKeyConflict):
		status = 409
		code = "BATCH_KEY_CONFLICT"
	case errors.Is(e, a.ErrInvalidInput):
		status = 422
		code = "IMPORT_INPUT_INVALID"
	case errors.Is(e, a.ErrForbidden):
		status = 403
		code = "IMPORT_FORBIDDEN"
	case errors.Is(e, a.ErrRetryable):
		code = "IMPORT_RETRYABLE"
	case errors.Is(e, a.ErrAuditUnavailable):
		code = "IMPORT_AUDIT_UNAVAILABLE"
	case errors.Is(e, a.ErrCommitUnknown):
		code = "COMMIT_OUTCOME_UNKNOWN"
	}
	rejectImport(w, r, status, code)
}

type importCorrelationKey struct{}

func rejectImport(w http.ResponseWriter, r *http.Request, status int, code string) {
	id, _ := r.Context().Value(importCorrelationKey{}).(string)
	slog.WarnContext(r.Context(), "controlled import request rejected", "error_code", code, "request_correlation_id", id)
	writeAdminError(w, status, code)
}
