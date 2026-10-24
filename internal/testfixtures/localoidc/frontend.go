package localoidc

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func Frontend(provider http.Handler, upstream *url.URL) (http.Handler, error) {
	if provider == nil || upstream == nil || upstream.Scheme != "http" || upstream.User != nil || upstream.Hostname() != "127.0.0.1" || upstream.Port() == "" || (upstream.Path != "" && upstream.Path != "/") || upstream.RawPath != "" || upstream.RawQuery != "" || upstream.ForceQuery || upstream.Fragment != "" {
		return nil, errors.New("explicit loopback API target required")
	}
	port, e := strconv.Atoi(upstream.Port())
	if e != nil || port < 1024 || port > 65535 {
		return nil, errors.New("local API port out of range")
	}
	target := *upstream
	proxy := httputil.NewSingleHostReverseProxy(&target)
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	proxy.Transport = &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ResponseHeaderTimeout: 5 * time.Second, IdleConnTimeout: 30 * time.Second, MaxIdleConns: 16, MaxConnsPerHost: 32}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, e error) { deny(w, 503, "local_api_unavailable") }
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.TLS == nil {
			deny(w, 400, "https_required")
			return
		}
		if r.URL.RawPath != "" || strings.Contains(r.URL.Path, "//") {
			deny(w, 400, "invalid_request")
			return
		}
		for _, part := range strings.Split(r.URL.Path, "/") {
			if part == "." || part == ".." {
				deny(w, 400, "invalid_request")
				return
			}
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/oidc/"):
			provider.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/v1/") || r.URL.Path == "/health/live" || r.URL.Path == "/health/ready":
			proxy.ServeHTTP(w, r)
		case r.URL.Path == "/" && r.Method == "GET":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("Enterprise IM 本地合成联测环境。仅供本机测试。"))
		default:
			deny(w, 404, "not_found")
		}
	}), nil
}
