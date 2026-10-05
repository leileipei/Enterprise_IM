package policystore_test

import (
	"encoding/binary"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A raw TCP relay for actual PostgreSQL wire traffic. Its fault closes existing
// client sockets and new connections, without manufacturing a SQL response.
type processDBRelay struct {
	listener net.Listener
	target   string
	blocked  atomic.Bool
	mu       sync.Mutex
	clients  map[net.Conn]bool
	done     chan struct{}
	active   sync.WaitGroup
	queries  atomic.Int64
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
	r.clients[client] = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.clients, client); r.mu.Unlock() }()
	upstream, e := net.DialTimeout("tcp", r.target, time.Second)
	if e != nil {
		return
	}
	defer upstream.Close()
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
