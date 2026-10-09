package httpserver

import (
	"bytes"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/nativeauth"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func HandlerWithNativeAuth(base http.Handler, broker *nativeauth.Broker) (http.Handler, error) {
	if base == nil || broker == nil {
		return nil, errors.New("native route dependencies required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/native" && !strings.HasPrefix(r.URL.Path, "/api/v1/native/") {
			base.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		bodyConsumed := r.ContentLength == 0 && len(r.TransferEncoding) == 0
		controller := http.NewResponseController(w)
		if e := controller.SetReadDeadline(time.Now().Add(5 * time.Second)); e != nil && !errors.Is(e, http.ErrNotSupported) {
			nativeAuthError(w, nativeauth.ErrUnavailable, 0)
			return
		}
		if !bodyConsumed {
			w.Header().Set("Connection", "close")
		}
		defer func() {
			if bodyConsumed {
				_ = controller.SetReadDeadline(time.Time{})
			}
		}()

		if r.URL.RawPath != "" {
			writeAdminError(w, 404, "not_found")
			return
		}
		if r.URL.Path == "/api/v1/native/config" {
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				writeAdminError(w, 405, "method_not_allowed")
				return
			}
			params, e := url.ParseQuery(r.URL.RawQuery)
			if e != nil || r.URL.ForceQuery || r.ContentLength != 0 || len(r.TransferEncoding) > 0 || len(params) != 1 || len(params["platform"]) != 1 {
				writeAdminError(w, 400, "invalid_request")
				return
			}
			cfg, e := broker.ConfigFor(params.Get("platform"))
			if e != nil {
				writeAdminError(w, 400, "invalid_request")
				return
			}
			writeAdminJSON(w, 200, cfg)
			return
		}
		if r.URL.Path != "/api/v1/native/oauth/token" && r.URL.Path != "/api/v1/native/oauth/refresh" {
			writeAdminError(w, 404, "not_found")
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAdminError(w, 405, "method_not_allowed")
			return
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			writeAdminError(w, 400, "invalid_request")
			return
		}
		ctx, release, e := broker.BeginRequest(r.Context())
		if e != nil {
			nativeAuthError(w, e, 0)
			return
		}
		defer release()
		host, _, e := net.SplitHostPort(r.RemoteAddr)
		if e != nil {
			nativeAuthError(w, nativeauth.ErrUnavailable, 0)
			return
		}
		addr, e := netip.ParseAddr(host)
		if e != nil || addr.IsUnspecified() || addr.Zone() != "" {
			nativeAuthError(w, nativeauth.ErrUnavailable, 0)
			return
		}
		delay, e := broker.AllowRequest(ctx, addr.Unmap().String())
		if e != nil {
			nativeAuthError(w, e, delay)
			return
		}
		originalBody := r.Body
		finished := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				if originalBody != nil {
					_ = originalBody.Close()
				}
			case <-finished:
			}
		}()
		defer close(finished)
		r = r.WithContext(ctx)
		var tokens nativeauth.Tokens
		if r.URL.Path == "/api/v1/native/oauth/token" {
			var request nativeauth.ExchangeRequest
			if !decodeNativeJSON(w, r, []string{"platform", "code", "code_verifier", "redirect_uri", "nonce"}, &request, &bodyConsumed) {
				return
			}
			w.Header().Del("Connection")
			tokens, e = broker.Exchange(ctx, request)
		} else {
			var request nativeauth.RefreshRequest
			if !decodeNativeJSON(w, r, []string{"platform", "refresh_token"}, &request, &bodyConsumed) {
				return
			}
			w.Header().Del("Connection")
			tokens, e = broker.Refresh(ctx, request)
		}
		if e != nil {
			nativeAuthError(w, e, 0)
			return
		}
		writeAdminJSON(w, 200, tokens)
	}), nil
}
func decodeNativeJSON(w http.ResponseWriter, r *http.Request, fields []string, target any, bodyConsumed *bool) bool {
	if r.ContentLength > 4096 {
		writeAdminError(w, 413, "request_too_large")
		return false
	}
	raw, e := io.ReadAll(io.LimitReader(r.Body, 4097))
	if e != nil {
		var timeout net.Error
		if r.Context().Err() != nil || (errors.As(e, &timeout) && timeout.Timeout()) {
			nativeAuthError(w, nativeauth.ErrUnavailable, 0)
		} else {
			writeAdminError(w, 400, "invalid_request")
		}
		return false
	}
	if len(raw) > 4096 {
		writeAdminError(w, 413, "request_too_large")
		return false
	}
	*bodyConsumed = true
	copy := r.Clone(r.Context())
	copy.Body = io.NopCloser(bytes.NewReader(raw))
	if !decodeReceiptJSON(copy, 4096, fields, target) {
		writeAdminError(w, 400, "invalid_request")
		return false
	}
	return true
}
func nativeAuthError(w http.ResponseWriter, e error, delay time.Duration) {
	switch {
	case errors.Is(e, nativeauth.ErrRateLimited):
		seconds := int(math.Ceil(delay.Seconds()))
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		writeAdminError(w, 429, "native_rate_limit")
	case errors.Is(e, nativeauth.ErrInvalidGrant):
		writeAdminError(w, 401, "invalid_grant")
	default:
		writeAdminError(w, 503, "auth_unavailable")
	}
}
func HandlerWithClosedNativeAuth(base http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/native" || strings.HasPrefix(r.URL.Path, "/api/v1/native/") {
			w.Header().Set("Cache-Control", "no-store")
			writeAdminError(w, 404, "not_found")
			return
		}
		base.ServeHTTP(w, r)
	})
}
