package policystore_test

// This is a transport protocol fixture, not a successful PostgreSQL response.
// RP11 separately proves the actual database query and bounded search.
import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileBusinessRelayTLSProtocol(t *testing.T) {
	template := httptest.NewTLSServer(nil)
	certificate := template.TLS.Certificates[0]
	template.Close()
	root := t.TempDir()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})
	keyDER, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"ca.crt": certPEM, "server.crt": certPEM, "server.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})} {
		if err := os.WriteFile(filepath.Join(root, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	sslRequest := []byte{0, 0, 0, 8, 4, 210, 22, 47}
	payload := []byte{0, 0, 0, 12, 0, 3, 0, 0, 0, 0, 0, 0, 'Q', 0, 0, 0, 5, 0}
	result := make(chan error, 1)
	go func() {
		conn, e := upstream.Accept()
		if e != nil {
			result <- e
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		request := make([]byte, 8)
		if _, e = io.ReadFull(conn, request); e != nil {
			result <- e
			return
		}
		if !bytes.Equal(request, sslRequest) {
			result <- io.ErrUnexpectedEOF
			return
		}
		if _, e = conn.Write([]byte{'S'}); e != nil {
			result <- e
			return
		}
		secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
		actual := make([]byte, len(payload))
		_, e = io.ReadFull(secure, actual)
		if e == nil && !bytes.Equal(actual, payload) {
			e = io.ErrUnexpectedEOF
		}
		if e == nil {
			_, e = secure.Write([]byte("observed"))
		}
		result <- e
	}()
	relay := newProcessDBRelay(t, "postgresql://fixture:fixture@"+upstream.Addr().String()+"/fixture?sslmode=verify-full&sslrootcert="+url.QueryEscape(filepath.Join(root, "ca.crt")))
	defer relay.Close()
	conn, err := net.DialTimeout("tcp", relay.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = conn.Write(sslRequest); err != nil {
		t.Fatal(err)
	}
	answer := make([]byte, 1)
	if _, err = io.ReadFull(conn, answer); err != nil || answer[0] != 'S' {
		t.Fatal("SSL negotiation unavailable")
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	secure := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	if _, err = secure.Write(payload); err != nil {
		t.Fatal(err)
	}
	proof := make([]byte, len("observed"))
	if _, err = io.ReadFull(secure, proof); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err != nil {
		t.Fatal(err)
	}
	if relay.queries.Load() != 1 {
		t.Fatal("TLS relay did not observe actual frontend query frame")
	}
	conn.Close()
	relay.active.Wait()
	relay.mu.Lock()
	retained := len(relay.clients)
	relay.mu.Unlock()
	if retained != 0 {
		t.Fatal("completed TLS relay retained client identity")
	}
}
