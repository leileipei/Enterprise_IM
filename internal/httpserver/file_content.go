package httpserver

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/filetransfer"
	"io"
	"net/http"
	"strings"
	"time"
)

type FileContentService interface {
	Upload(context.Context, access.TrustedIdentity, string, io.Reader) (files.Metadata, error)
}

func HandlerWithFileContent(next http.Handler, auth Authenticator, svc FileContentService) (http.Handler, error) {
	return fileContentHandler(next, auth, svc, filetransfer.ReceiveTimeout, filetransfer.UploadTimeout)
}
func fileContentHandler(next http.Handler, auth Authenticator, svc FileContentService, receive, total time.Duration) (http.Handler, error) {
	if next == nil || auth == nil || svc == nil || receive <= 0 || total < receive {
		return nil, errors.New("file content requires complete bounded dependencies")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) != 6 || parts[1] != "api" || parts[2] != "v1" || parts[3] != "files" || parts[5] != "content" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		id, ok := authenticateAdmin(w, r, auth)
		if !ok {
			return
		}
		if r.Method != http.MethodPut {
			w.Header().Set("Allow", "PUT")
			rejectAdmin(w, r, 405, "method_not_allowed")
			return
		}
		if r.URL.RawQuery != "" || r.Header.Get("Content-Encoding") != "" || r.Header.Get("Content-Type") != "application/octet-stream" {
			rejectAdmin(w, r, 400, "invalid_request")
			return
		}
		controller := http.NewResponseController(w)
		if e := controller.SetReadDeadline(start.Add(receive)); e != nil {
			writeAdminError(w, 503, "upload_deadline_unavailable")
			return
		}
		defer controller.SetReadDeadline(time.Time{})
		if e := controller.SetWriteDeadline(start.Add(total)); e != nil {
			writeAdminError(w, 503, "upload_deadline_unavailable")
			return
		}
		defer func() {
			_ = controller.Flush()
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		}()
		ctx, cancel := context.WithDeadline(r.Context(), start.Add(total))
		defer cancel()
		stop := context.AfterFunc(ctx, func() { controller.SetReadDeadline(time.Now()); r.Body.Close() })
		defer stop()
		m, e := svc.Upload(ctx, id, parts[4], r.Body)
		if e != nil {
			w.Header().Set("Connection", "close")
			if errors.Is(e, filetransfer.ErrNodeBusy) {
				w.Header().Set("Retry-After", "1")
				w.Header().Set("Connection", "close")
				writeAdminError(w, 429, "upload_node_busy")
			} else {
				writeFileError(w, e)
			}
			return
		}
		writeAdminJSON(w, 200, fileStatus(m))
	}), nil
}
