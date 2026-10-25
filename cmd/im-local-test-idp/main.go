// im-local-test-idp serves a loopback-only synthetic identity source and API proxy.
// It is a local test tool, not a production identity provider.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/testfixtures/localoidc"
)

type RuntimeConfig struct {
	Owner        string `json:"owner"`
	Source       string `json:"source"`
	PrivateDir   string `json:"private_dir"`
	Listen       string `json:"listen"`
	Upstream     string `json:"upstream"`
	LeaseSeconds int    `json:"lease_seconds"`
}
type Ready struct {
	Owner         string    `json:"owner"`
	Source        string    `json:"source"`
	Origin        string    `json:"origin"`
	Issuer        string    `json:"issuer"`
	CACert        string    `json:"ca_cert"`
	CAFingerprint string    `json:"ca_fingerprint"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func readConfig(path string) (RuntimeConfig, error) {
	var cfg RuntimeConfig
	if localoidc.PrivateFile(path) != nil {
		return cfg, errors.New("private configuration required")
	}
	b, e := os.ReadFile(path)
	if e != nil || len(b) > 8192 {
		return cfg, errors.New("local configuration budget")
	}
	if !uniqueConfigKeys(b) {
		return cfg, errors.New("ambiguous local configuration")
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cfg) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return cfg, errors.New("invalid local configuration")
	}
	host, port, e := net.SplitHostPort(cfg.Listen)
	p, pe := strconv.Atoi(port)
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(cfg.Owner) || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(cfg.Source) || localoidc.PrivateDirectory(cfg.PrivateDir) != nil || filepath.Dir(path) != cfg.PrivateDir || filepath.Base(cfg.PrivateDir) != cfg.Owner || e != nil || host != "127.0.0.1" || pe != nil || (p != 0 && (p < 1024 || p > 65535)) || cfg.LeaseSeconds < 1 || cfg.LeaseSeconds > 14400 {
		return cfg, errors.New("local ownership or lease invalid")
	}
	upstream, e := url.Parse(cfg.Upstream)
	if e != nil {
		return cfg, errors.New("invalid local API target")
	}
	if _, e = localoidc.Frontend(http.NotFoundHandler(), upstream); e != nil {
		return cfg, errors.New("invalid local API target")
	}
	return cfg, nil
}
func uniqueConfigKeys(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, e := decoder.Token()
	if e != nil || token != json.Delim('{') {
		return false
	}
	allowed := map[string]bool{"owner": true, "source": true, "private_dir": true, "listen": true, "upstream": true, "lease_seconds": true}
	seen := map[string]bool{}
	for decoder.More() {
		token, e = decoder.Token()
		key, ok := token.(string)
		if e != nil || !ok || !allowed[key] || seen[key] {
			return false
		}
		seen[key] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
	}
	token, e = decoder.Token()
	return e == nil && token == json.Delim('}') && len(seen) == len(allowed) && decoder.Decode(&struct{}{}) == io.EOF
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if e := run(ctx, os.Args[1:], os.Stdout); e != nil {
		fmt.Fprintln(os.Stderr, "local test identity source unavailable")
		os.Exit(1)
	}
}
func run(parent context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("im-local-test-idp", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "", "private local configuration")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *path == "" {
		return errors.New("explicit private configuration required")
	}
	cfg, e := readConfig(*path)
	if e != nil {
		return e
	}
	readyFile := filepath.Join(cfg.PrivateDir, "idp-ready.json")
	if _, e = os.Lstat(readyFile); !os.IsNotExist(e) {
		return errors.New("local ready state already exists")
	}
	tlsDir := filepath.Join(cfg.PrivateDir, "tls")
	if e = os.Mkdir(tlsDir, 0700); e != nil {
		return errors.New("new private TLS directory required")
	}
	files, e := localoidc.GenerateTLS(tlsDir, cfg.Owner, time.Now())
	if e != nil {
		return e
	}
	listener, e := net.Listen("tcp4", cfg.Listen)
	if e != nil {
		return errors.New("local HTTPS listener unavailable")
	}
	defer listener.Close()
	origin := "https://" + listener.Addr().String()
	clients := map[string]localoidc.Client{}
	for _, platform := range []string{"windows", "macos", "android", "ios"} {
		id := "im-" + platform
		c := localoidc.Client{ID: id, RedirectURI: "com.enterpriseim.client:/oauth/callback"}
		if platform == "windows" || platform == "macos" {
			c.Loopback = true
			c.RedirectURI = "http://127.0.0.1/oauth/callback"
		}
		clients[id] = c
	}
	audit := log.New(stdout, "", 0)
	observe := func(event localoidc.SelectionObservation) {
		data, _ := json.Marshal(struct {
			Event string `json:"event"`
			localoidc.SelectionObservation
		}{"local_selection", event})
		audit.Print(string(data))
	}
	provider, e := localoidc.New(localoidc.Config{ObserveSelection: observe, Issuer: origin + "/oidc", Audience: "enterprise-im-api", Clients: clients, Users: []localoidc.User{{Subject: "local-alice", Name: "Alice"}, {Subject: "local-bob", Name: "Bob"}}})
	if e != nil {
		return e
	}
	upstream, _ := url.Parse(cfg.Upstream)
	frontend, e := localoidc.Frontend(provider, upstream)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(cfg.LeaseSeconds)*time.Second)
	defer cancel()
	var mu sync.Mutex
	pendingClose := map[net.Conn]bool{}
	stopping := false
	closeUnmanaged := func() {
		mu.Lock()
		defer mu.Unlock()
		stopping = true
		for c := range pendingClose {
			_ = c.Close()
		}
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != listener.Addr().String() {
			w.WriteHeader(400)
			return
		}
		frontend.ServeHTTP(w, r)
	}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192, ErrorLog: log.New(io.Discard, "", 0), ConnState: func(c net.Conn, state http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		if state == http.StateNew || state == http.StateHijacked {
			if stopping {
				_ = c.Close()
				return
			}
			pendingClose[c] = true
		} else {
			delete(pendingClose, c)
		}
	}}
	server.TLSConfig = minTLSConfig()
	done := make(chan error, 1)
	go func() { done <- server.ServeTLS(listener, files.ServerCert, files.ServerKey) }()
	defer func() {
		closeUnmanaged()
		_ = server.Close()
	}()
	ready := Ready{Owner: cfg.Owner, Source: cfg.Source, Origin: origin, Issuer: origin + "/oidc", CACert: files.CACert, CAFingerprint: files.Fingerprint, ExpiresAt: time.Now().UTC().Add(time.Duration(cfg.LeaseSeconds) * time.Second)}
	data, e := json.Marshal(ready)
	if e != nil {
		return errors.New("local ready encoding unavailable")
	}
	f, e := os.OpenFile(readyFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return errors.New("local ready file unavailable")
	}
	_, we := f.Write(data)
	se := f.Sync()
	ce := f.Close()
	if we != nil || se != nil || ce != nil {
		return errors.New("local ready write failed")
	}
	if _, e = fmt.Fprintln(stdout, string(data)); e != nil {
		return errors.New("local ready output failed")
	}
	select {
	case e = <-done:
		if !errors.Is(e, http.ErrServerClosed) {
			return errors.New("local HTTPS server stopped")
		}
		return nil
	case <-ctx.Done():
	}
	closeUnmanaged()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	_ = server.Shutdown(shutdown)
	e = <-done
	if e != nil && !errors.Is(e, http.ErrServerClosed) {
		return errors.New("local HTTPS shutdown failed")
	}
	return nil
}
