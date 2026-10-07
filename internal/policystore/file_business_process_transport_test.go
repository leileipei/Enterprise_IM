package policystore_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A raw TCP relay for actual PostgreSQL wire traffic. Its fault closes existing
// client sockets and new connections, without manufacturing a SQL response.
type processDBRelay struct {
	listener               net.Listener
	target                 string
	blocked                atomic.Bool
	mu                     sync.Mutex
	clients                map[net.Conn]bool
	done                   chan struct{}
	active                 sync.WaitGroup
	queries                atomic.Int64
	serverTLS, upstreamTLS *tls.Config
}

func newProcessDBRelay(t *testing.T, dsn string) *processDBRelay {
	t.Helper()
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal("relay fixture DSN invalid")
	}
	target := u.Host
	if u.Port() == "" {
		target = net.JoinHostPort(u.Hostname(), "5432")
	}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	r := &processDBRelay{listener: l, target: target, clients: map[net.Conn]bool{}, done: make(chan struct{})}
	if u.Query().Get("sslmode") == "verify-full" {
		caPath := u.Query().Get("sslrootcert")
		ca, err := os.ReadFile(caPath)
		if err != nil {
			t.Fatal("relay fixture trusted CA unavailable")
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			t.Fatal("relay fixture trusted CA invalid")
		}
		// The owned integration fixture supplies this certificate beside its CA.
		// It is read only by the test relay and never passed to a product child.
		certificate, err := tls.LoadX509KeyPair(filepath.Join(filepath.Dir(caPath), "server.crt"), filepath.Join(filepath.Dir(caPath), "server.key"))
		if err != nil {
			t.Fatal("relay fixture certificate unavailable")
		}
		r.serverTLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
		r.upstreamTLS = &tls.Config{RootCAs: roots, ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
	}
	go func() {
		defer close(r.done)
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			r.active.Add(1)
			go r.forward(c)
		}
	}()
	return r
}
func (r *processDBRelay) forward(client net.Conn) {
	defer r.active.Done()
	defer client.Close()
	r.mu.Lock()
	if r.blocked.Load() {
		r.mu.Unlock()
		return
	}
	registeredClient := client
	r.clients[registeredClient] = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.clients, registeredClient); r.mu.Unlock() }()
	upstream, e := net.DialTimeout("tcp", r.target, time.Second)
	if e != nil {
		return
	}
	defer upstream.Close()
	if r.serverTLS != nil {
		client, upstream, e = r.secureLinks(client, upstream)
		if e != nil {
			return
		}
	}
	copied := make(chan struct{})
	go func() {
		io.Copy(upstream, &processPGReader{Reader: client, relay: r, startup: true})
		upstream.Close()
		close(copied)
	}()
	io.Copy(client, upstream)
	client.Close()
	<-copied
}

// PostgreSQL negotiates TLS using an SSLRequest before the TLS handshake.
// Both real links retain CA and hostname validation; only decrypted frontend
// frame counts are observed, with no SQL or authentication values recorded.
func (r *processDBRelay) secureLinks(client, upstream net.Conn) (net.Conn, net.Conn, error) {
	deadline := time.Now().Add(5 * time.Second)
	client.SetDeadline(deadline)
	upstream.SetDeadline(deadline)
	request := make([]byte, 8)
	if _, err := io.ReadFull(client, request); err != nil {
		return nil, nil, err
	}
	if binary.BigEndian.Uint32(request[:4]) != 8 || binary.BigEndian.Uint32(request[4:]) != 80877103 {
		return nil, nil, errors.New("TLS relay requires SSLRequest")
	}
	if _, err := upstream.Write(request); err != nil {
		return nil, nil, err
	}
	answer := make([]byte, 1)
	if _, err := io.ReadFull(upstream, answer); err != nil {
		return nil, nil, err
	}
	if answer[0] != 'S' {
		return nil, nil, errors.New("upstream refused TLS")
	}
	if _, err := client.Write(answer); err != nil {
		return nil, nil, err
	}
	securedClient := tls.Server(client, r.serverTLS.Clone())
	securedUpstream := tls.Client(upstream, r.upstreamTLS.Clone())
	if err := securedClient.Handshake(); err != nil {
		return nil, nil, err
	}
	if err := securedUpstream.Handshake(); err != nil {
		return nil, nil, err
	}
	client.SetDeadline(time.Time{})
	upstream.SetDeadline(time.Time{})
	return securedClient, securedUpstream, nil
}

func (r *processDBRelay) setFault(blocked bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blocked.Store(blocked)
	if blocked {
		for c := range r.clients {
			c.Close()
		}
	}
}
func (r *processDBRelay) Close() { r.listener.Close(); <-r.done; r.setFault(true); r.active.Wait() }

// Count actual frontend simple-query/execute frames without recording SQL,
// bind parameters, passwords or results. It never changes forwarded bytes.
type processPGReader struct {
	io.Reader
	relay    *processDBRelay
	startup  bool
	pending  []byte
	disabled bool
}

func (r *processPGReader) Read(b []byte) (int, error) {
	n, e := r.Reader.Read(b)
	if r.disabled {
		return n, e
	}
	r.pending = append(r.pending, b[:n]...)
	for {
		header := 5
		if r.startup {
			header = 4
		}
		if len(r.pending) < header {
			break
		}
		offset := 1
		if r.startup {
			offset = 0
		}
		length := int(binary.BigEndian.Uint32(r.pending[offset : offset+4]))
		total := length + offset
		if length < 4 || total > 1<<20 {
			r.disabled = true
			r.pending = nil
			break
		}
		if len(r.pending) < total {
			break
		}
		if r.startup {
			if length != 8 {
				r.startup = false
			}
		} else if r.pending[0] == 'Q' || r.pending[0] == 'E' {
			r.relay.queries.Add(1)
		}
		r.pending = r.pending[total:]
	}
	return n, e
}
