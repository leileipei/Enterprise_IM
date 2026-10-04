package httpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/filedownload"
	"github.com/leileipei/Enterprise_IM/internal/files"
)

type FileDownloadService interface {
	Prepare(context.Context, access.TrustedIdentity, string) (*filedownload.Prepared, error)
	Authorize(context.Context, access.TrustedIdentity, *filedownload.Prepared) error
	Check(context.Context, access.TrustedIdentity, *filedownload.Prepared) error
	Finish(context.Context, *filedownload.Prepared, filedownload.Result) error
}

func downloadContentPath(r *http.Request) (string, bool) {
	p := strings.Split(r.URL.Path, "/")
	if len(p) != 6 || p[1] != "api" || p[2] != "v1" || p[3] != "files" || p[5] != "content" {
		return "", false
	}
	return p[4], true
}

// The production route stays closed independently of the upload switch.
func HandlerWithClosedFileDownload(next http.Handler, uploadEnabled bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, matched := downloadContentPath(r); !matched {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodGet {
			writeAdminError(w, 503, "file_download_unavailable")
			return
		}
		if r.Method == http.MethodPut && uploadEnabled {
			next.ServeHTTP(w, r)
			return
		}
		allow := "GET"
		if uploadEnabled {
			allow += ", PUT"
		}
		w.Header().Set("Allow", allow)
		writeAdminError(w, 405, "method_not_allowed")
	})
}

func HandlerWithFileDownload(next http.Handler, auth Authenticator, svc FileDownloadService) (http.Handler, error) {
	if next == nil || auth == nil || svc == nil {
		return nil, errors.New("file download requires complete dependencies")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fid, matched := downloadContentPath(r)
		if !matched || r.Method == http.MethodPut {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET, PUT")
			writeAdminError(w, 405, "method_not_allowed")
			return
		}
		if !validUUID(fid) || r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength != 0 || len(r.TransferEncoding) != 0 || (r.Body != nil && r.Body != http.NoBody) {
			writeAdminError(w, 400, "invalid_request")
			return
		}
		for _, h := range []string{"Range", "If-Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "Content-Encoding"} {
			if _, present := r.Header[http.CanonicalHeaderKey(h)]; present {
				writeAdminError(w, 400, "invalid_request")
				return
			}
		}
		controller := http.NewResponseController(w)
		if controller.SetWriteDeadline(start.Add(filedownload.DownloadTimeout)) != nil {
			writeAdminError(w, 503, "download_deadline_unavailable")
			return
		}
		authCtx, stop := context.WithTimeout(r.Context(), time.Second)
		verified, ok := authenticateBearer(w, r.WithContext(authCtx), auth)
		authTimedOut := authCtx.Err() != nil
		stop()
		if !ok {
			return
		}
		if authTimedOut {
			writeAdminError(w, 503, "file_download_unavailable")
			return
		}
		if !downloadExpiryValid(verified.ExpiresAt) {
			writeAdminError(w, 503, "download_expiry_unavailable")
			return
		}
		if !time.Now().Before(verified.ExpiresAt) {
			writeAdminError(w, 401, "unauthorized")
			return
		}
		memberships := r.Header.Values("X-Acting-Membership-ID")
		if len(memberships) != 1 || !validUUID(memberships[0]) {
			writeAdminError(w, 400, "invalid_identity")
			return
		}
		id := access.TrustedIdentity{TenantID: strings.ToLower(verified.TenantID), UserID: strings.ToLower(verified.UserID), ActingMembershipID: strings.ToLower(memberships[0])}
		_, bearer, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		total := downloadEarlier(start.Add(filedownload.DownloadTimeout), verified.ExpiresAt)
		if controller.SetWriteDeadline(total) != nil {
			writeAdminError(w, 503, "download_deadline_unavailable")
			return
		}
		ctx, cancel := context.WithDeadline(r.Context(), total)
		defer cancel()
		p, e := svc.Prepare(ctx, id, strings.ToLower(fid))
		if e != nil {
			writeDownloadError(w, e)
			return
		}
		defer p.Close()
		result := filedownload.Result{Outcome: "interrupted", Reason: "dependency_unavailable"}
		finished := false
		settle := func() {
			if finished {
				return
			}
			finished = true
			settleCtx, done := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			defer done()
			if svc.Finish(settleCtx, p, result) != nil {
				slog.ErrorContext(settleCtx, "file download settlement pending")
			}
		}
		defer settle()
		sent := false
		fail := func(err error, reason string) {
			result.Reason = reason
			if sent {
				settle()
				panic(http.ErrAbortHandler)
			}
			writeDownloadError(w, err)
		}
		if e = svc.Authorize(ctx, id, p); e != nil {
			reason := "audit_unavailable"
			if errors.Is(e, filedownload.ErrNotFound) || errors.Is(e, filedownload.ErrInvalidIdentity) {
				reason = "authorization_revoked"
			}
			fail(e, reason)
			return
		}
		name, size := p.Filename(), p.SizeBytes()
		if !downloadFilenameValid(name) || size < 1 || size > files.MaxFileSizeBytes {
			fail(filedownload.ErrUnavailable, "dependency_unavailable")
			return
		}
		disposition := mime.FormatMediaType("attachment", map[string]string{"filename": name})
		if disposition == "" {
			fail(filedownload.ErrUnavailable, "dependency_unavailable")
			return
		}
		buf := make([]byte, 32768)
		// Authorization covers the full spool; the first output still requires a
		// fresh bearer and database check. Later checks are bounded by the preceding one.
		lastValid := time.Now()
		for result.BytesWritten < size {
			remaining := size - result.BytesWritten
			chunk := buf
			if remaining < int64(len(chunk)) {
				chunk = chunk[:remaining]
			}
			n, readErr := io.ReadFull(p, chunk)
			if readErr != nil || n != len(chunk) {
				fail(filedownload.ErrUnavailable, downloadStopReason(ctx, verified.ExpiresAt))
				return
			}
			checkDeadline := downloadEarlier(total, lastValid.Add(time.Second))
			checkCtx, done := context.WithDeadline(ctx, checkDeadline)
			fresh, authErr := auth.Authenticate(checkCtx, bearer)
			reason := "authorization_revoked"
			if errors.Is(authErr, ErrAuthUnavailable) {
				reason = "dependency_unavailable"
			}
			if authErr == nil && (!downloadExpiryValid(fresh.ExpiresAt) || !time.Now().Before(fresh.ExpiresAt)) {
				authErr = filedownload.ErrInvalidIdentity
				reason = "token_expired"
			}
			if authErr == nil && (strings.ToLower(fresh.TenantID) != id.TenantID || strings.ToLower(fresh.UserID) != id.UserID) {
				authErr = filedownload.ErrInvalidIdentity
			}
			// Never extend the original verified expiration, even if an adapter changes it.
			if authErr == nil {
				checkDeadline = downloadEarlier(checkDeadline, fresh.ExpiresAt)
				freshCtx, freshDone := context.WithDeadline(checkCtx, checkDeadline)
				authErr = svc.Check(freshCtx, id, p)
				if freshCtx.Err() != nil && authErr == nil {
					authErr = freshCtx.Err()
				}
				freshDone()
			}
			if checkCtx.Err() != nil && authErr == nil {
				authErr = checkCtx.Err()
			}
			done()
			if !time.Now().Before(verified.ExpiresAt) {
				authErr = filedownload.ErrInvalidIdentity
				reason = "token_expired"
			}
			if authErr != nil {
				fail(authErr, reason)
				return
			}
			lastValid = time.Now()
			writeDeadline := downloadEarlier(downloadEarlier(total, fresh.ExpiresAt), lastValid.Add(time.Second))
			if !time.Now().Before(writeDeadline) || controller.SetWriteDeadline(writeDeadline) != nil {
				fail(filedownload.ErrUnavailable, downloadStopReason(ctx, verified.ExpiresAt))
				return
			}
			if !sent {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
				w.Header().Set("Content-Disposition", disposition)
				w.WriteHeader(http.StatusOK)
				sent = true
			}
			written, writeErr := w.Write(chunk)
			if written < 0 || written > len(chunk) {
				written = 0
				writeErr = io.ErrShortWrite
			}
			result.BytesWritten += int64(written)
			if writeErr != nil || written != len(chunk) {
				fail(filedownload.ErrUnavailable, downloadStopReason(ctx, verified.ExpiresAt))
				return
			}
			if controller.Flush() != nil {
				fail(filedownload.ErrUnavailable, downloadStopReason(ctx, verified.ExpiresAt))
				return
			}
			if !time.Now().Before(writeDeadline) {
				fail(filedownload.ErrUnavailable, downloadStopReason(ctx, verified.ExpiresAt))
				return
			}
		}
		result.Outcome, result.Reason = "completed", "completed"
		settle()
		// Output is already flushed. Restore the normal server budget for reuse.
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
	}), nil
}
func downloadEarlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
func downloadExpiryValid(t time.Time) bool {
	return !t.IsZero() && t.UTC().Year() >= 1 && t.UTC().Year() <= 9999
}
func downloadFilenameValid(s string) bool {
	return utf8.ValidString(s) && len(s) >= 1 && len(s) <= 255 && s != "." && s != ".." && strings.TrimSpace(s) == s && strings.IndexFunc(s, unicode.IsControl) < 0 && !strings.ContainsAny(s, "/\\:")
}
func downloadStopReason(ctx context.Context, expiry time.Time) string {
	if !time.Now().Before(expiry) {
		return "token_expired"
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	return "client_disconnected"
}
func writeDownloadError(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, filedownload.ErrNotFound):
		writeAdminError(w, 404, "not_found")
	case errors.Is(e, filedownload.ErrInvalidIdentity):
		writeAdminError(w, 403, "invalid_identity")
	case errors.Is(e, filedownload.ErrBusy):
		writeAdminError(w, 409, "download_in_progress")
	case errors.Is(e, filedownload.ErrAuditPending):
		writeAdminError(w, 409, "download_audit_pending")
	case errors.Is(e, filedownload.ErrLimit):
		w.Header().Set("Retry-After", "1")
		writeAdminError(w, 429, "download_busy")
	default:
		writeAdminError(w, 503, "file_download_unavailable")
	}
}
