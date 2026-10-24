package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func configFixture(t *testing.T) (string, RuntimeConfig) {
	t.Helper()
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	owner := strings.Repeat("a", 32)
	private := filepath.Join(root, owner)
	if e = os.Mkdir(private, 0700); e != nil {
		t.Fatal(e)
	}
	cfg := RuntimeConfig{Owner: owner, Source: strings.Repeat("b", 40), PrivateDir: private, Listen: "127.0.0.1:0", Upstream: "http://127.0.0.1:49152", LeaseSeconds: 1}
	path := filepath.Join(private, "idp.json")
	saveConfig(t, path, cfg)
	return path, cfg
}
func saveConfig(t *testing.T, path string, cfg RuntimeConfig) {
	t.Helper()
	b, e := json.Marshal(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
}
func TestLocalHTTPSCommandLeaseAndPublicReady(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"real_upstream":true}`)) }))
	defer upstream.Close()
	path, cfg := configFixture(t)
	cfg.Upstream = upstream.URL
	saveConfig(t, path, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"--config", path}, io.Discard) }()
	readyPath := filepath.Join(cfg.PrivateDir, "idp-ready.json")
	var ready Ready
	end := time.Now().Add(5 * time.Second)
	for {
		b, e := os.ReadFile(readyPath)
		if e == nil && json.Unmarshal(b, &ready) == nil && ready.Origin != "" {
			break
		}
		select {
		case e := <-done:
			t.Fatal("early stop", e)
		default:
		}
		if time.Now().After(end) {
			t.Fatal("ready deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ready.Owner != cfg.Owner || ready.Source != cfg.Source || len(ready.CAFingerprint) != 64 || ready.Issuer != ready.Origin+"/oidc" {
		t.Fatal("bad public ready")
	}
	if data, _ := os.ReadFile(readyPath); strings.Contains(string(data), "PRIVATE KEY") || strings.Contains(string(data), "server.key") {
		t.Fatal("private material in ready")
	}
	ca, e := os.ReadFile(ready.CACert)
	if e != nil {
		t.Fatal(e)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}, Timeout: 2 * time.Second}
	res, e := client.Get(ready.Origin + "/oidc/keys")
	if e != nil || res.StatusCode != 200 {
		t.Fatal("strict HTTPS unavailable")
	}
	res.Body.Close()
	res, e = client.Get(ready.Origin + "/api/v1/me")
	if e != nil || res.StatusCode != 200 {
		t.Fatal("upstream unavailable")
	}
	res.Body.Close()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		trace := make([]byte, 32000)
		n := runtime.Stack(trace, true)
		t.Errorf("lease did not stop server at original 3s bound; goroutines: %s", trace[:n])
		lateStart := time.Now()
		select {
		case err := <-done:
			t.Logf("shutdown later returned after %s, error=%v", time.Since(lateStart), err)
		case <-time.After(5 * time.Second):
			t.Fatal("shutdown diagnostic still live")
		}
	}
	if res, e = client.Get(ready.Origin + "/oidc/keys"); e == nil {
		res.Body.Close()
		t.Fatal("lease-expired listener still live")
	}
	if e = run(context.Background(), []string{"--config", path}, io.Discard); e == nil {
		t.Fatal("private state reused")
	}
}
func TestRejectConfigBeforeTLSOrListener(t *testing.T) {
	for _, name := range []string{"wide-file", "linked-file", "foreign-owner", "bad-source", "foreign-private", "public-listener", "foreign-upstream", "unbounded-lease"} {
		t.Run(name, func(t *testing.T) {
			path, cfg := configFixture(t)
			switch name {
			case "wide-file":
				os.Chmod(path, 0644)
			case "linked-file":
				link := filepath.Join(cfg.PrivateDir, "link.json")
				os.Symlink(path, link)
				path = link
			case "foreign-owner":
				cfg.Owner = strings.Repeat("c", 32)
				saveConfig(t, path, cfg)
			case "bad-source":
				cfg.Source = "main"
				saveConfig(t, path, cfg)
			case "foreign-private":
				cfg.PrivateDir = filepath.Dir(cfg.PrivateDir)
				saveConfig(t, path, cfg)
			case "public-listener":
				cfg.Listen = "0.0.0.0:18443"
				saveConfig(t, path, cfg)
			case "foreign-upstream":
				cfg.Upstream = "http://customer.invalid:8080"
				saveConfig(t, path, cfg)
			case "unbounded-lease":
				cfg.LeaseSeconds = 86400
				saveConfig(t, path, cfg)
			}
			if e := run(context.Background(), []string{"--config", path}, io.Discard); e == nil {
				t.Fatal("unsafe config accepted")
			}
		})
	}
}

func TestLeaseStopsWithUnfinishedHandshake(t *testing.T) {
	path, cfg := configFixture(t)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- run(ctx, []string{"--config", path}, io.Discard) }()
	var ready Ready
	end := time.Now().Add(5 * time.Second)
	for {
		b, e := os.ReadFile(filepath.Join(cfg.PrivateDir, "idp-ready.json"))
		if e == nil && json.Unmarshal(b, &ready) == nil && ready.Origin != "" {
			break
		}
		if time.Now().After(end) {
			t.Fatal("ready deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
	conn, e := net.DialTimeout("tcp", strings.TrimPrefix(ready.Origin, "https://"), time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unfinished TLS handshake delayed lease shutdown")
	}
	conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, e = conn.Read(make([]byte, 1)); e == nil {
		t.Fatal("unfinished connection remains")
	}
}
func TestRejectDuplicateConfigurationFields(t *testing.T) {
	path, cfg := configFixture(t)
	b, _ := json.Marshal(cfg)
	b = append(b[:len(b)-1], []byte(`,"lease_seconds":1}`)...)
	os.WriteFile(path, b, 0600)
	if e := run(context.Background(), []string{"--config", path}, io.Discard); e == nil {
		t.Fatal("duplicate configuration key accepted")
	}
}
