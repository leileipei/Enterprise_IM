package localoidc

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func privateTLS(t *testing.T, owner string) TLSFiles {
	t.Helper()
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	files, e := GenerateTLS(dir, owner, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	return files
}
func TestFreshCAExactTrustAndPrivateFiles(t *testing.T) {
	a := privateTLS(t, strings.Repeat("a", 32))
	b := privateTLS(t, strings.Repeat("b", 32))
	if a.Fingerprint == b.Fingerprint {
		t.Fatal("reused local CA")
	}
	root, e := os.ReadFile(a.CACert)
	if e != nil {
		t.Fatal(e)
	}
	block, _ := pem.Decode(root)
	if block == nil {
		t.Fatal("invalid CA")
	}
	cert, e := x509.ParseCertificate(block.Bytes)
	if e != nil || !cert.IsCA || !cert.NotAfter.Before(time.Now().Add(25*time.Hour)) {
		t.Fatal("invalid local CA lifetime")
	}
	for _, p := range []string{a.CACert, a.CAKey, a.ServerCert, a.ServerKey} {
		info, e := os.Lstat(p)
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			t.Fatal("unsafe TLS file")
		}
	}
	key, e := tls.LoadX509KeyPair(a.ServerCert, a.ServerKey)
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{key}}
	server.StartTLS()
	t.Cleanup(server.Close)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(root)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	res, e := client.Get(server.URL)
	if e != nil || res.StatusCode != 204 {
		t.Fatal("exact CA rejected")
	}
	res.Body.Close()
	wrong, _ := os.ReadFile(b.CACert)
	pool = x509.NewCertPool()
	pool.AppendCertsFromPEM(wrong)
	client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	if res, e = client.Get(server.URL); e == nil {
		res.Body.Close()
		t.Fatal("wrong CA trusted")
	}
	serverCertRaw, _ := os.ReadFile(a.ServerCert)
	block, _ = pem.Decode(serverCertRaw)
	leaf, _ := x509.ParseCertificate(block.Bytes)
	if leaf.VerifyHostname("127.0.0.1") != nil || leaf.VerifyHostname("localhost") != nil || leaf.VerifyHostname("customer.invalid") == nil {
		t.Fatal("wrong certificate SAN")
	}
}
func TestTLSRejectsLinksReuseAndWideDirectory(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	owner := strings.Repeat("a", 32)
	os.Chmod(dir, 0755)
	if _, e := GenerateTLS(dir, owner, time.Now()); e == nil {
		t.Fatal("wide directory accepted")
	}
	os.Chmod(dir, 0700)
	link := dir + "-link"
	if e := os.Symlink(dir, link); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.Remove(link) })
	if _, e := GenerateTLS(link, owner, time.Now()); e == nil {
		t.Fatal("linked directory accepted")
	}
	sentinel := filepath.Join(dir, "server.key")
	os.WriteFile(sentinel, []byte("keep"), 0600)
	if _, e := GenerateTLS(dir, owner, time.Now()); e == nil {
		t.Fatal("existing private file accepted")
	}
	data, _ := os.ReadFile(sentinel)
	if string(data) != "keep" {
		t.Fatal("existing file overwritten")
	}
	os.Remove(sentinel)
	if _, e := GenerateTLS(dir, "not-owner", time.Now()); e == nil {
		t.Fatal("invalid owner accepted")
	}
}
