package importapply

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type commitProxy struct {
	listener net.Listener
	mu       sync.Mutex
	conns    []net.Conn
	wg       sync.WaitGroup
	cut      chan string
	stop     chan struct{}
}

func faultProxy(t *testing.T, f *appendFixture, before bool) (*pgxpool.Pool, *commitProxy) {
	t.Helper()
	cfg := f.Pool.Config()
	target := net.JoinHostPort(cfg.ConnConfig.Host, strconv.Itoa(int(cfg.ConnConfig.Port)))
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	p := &commitProxy{listener: ln, cut: make(chan string, 1), stop: make(chan struct{})}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			client, e := ln.Accept()
			if e != nil {
				return
			}
			backend, e := net.DialTimeout("tcp", target, time.Second)
			if e != nil {
				client.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, client, backend)
			p.mu.Unlock()
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				defer client.Close()
				defer backend.Close()
				armed := make(chan struct{}, 1)
				var once sync.Once
				// PostgreSQL startup packet, followed by actual framed frontend protocol.
				var length [4]byte
				if _, e = io.ReadFull(client, length[:]); e != nil {
					return
				}
				n := binary.BigEndian.Uint32(length[:])
				if n < 8 || n > 10000 {
					return
				}
				payload := make([]byte, n-4)
				if _, e = io.ReadFull(client, payload); e != nil {
					return
				}
				backend.Write(length[:])
				backend.Write(payload)
				responses := make(chan struct{})
				go func() {
					defer close(responses)
					for {
						kind, payload, e := readFrame(backend)
						if e != nil {
							return
						}
						isCommit := kind == 'C' && bytes.Equal(payload, []byte("COMMIT\x00"))
						if isCommit {
							select {
							case <-armed:
								once.Do(func() { p.cut <- "backend_COMMIT_complete_response_dropped" })
								client.Close()
								backend.Close()
								return
							default:
							}
						}
						if e = writeFrame(client, kind, payload); e != nil {
							return
						}
					}
				}()
				for {
					kind, payload, e := readFrame(client)
					if e != nil {
						return
					}
					if kind == 'Q' && strings.EqualFold(strings.TrimSpace(strings.TrimSuffix(string(payload), "\x00")), "commit") {
						if before {
							once.Do(func() { p.cut <- "frontend_COMMIT_dropped_before_backend" })
							return
						}
						armed <- struct{}{}
					}
					if e = writeFrame(backend, kind, payload); e != nil {
						return
					}
				}
			}()
		}
	}()
	cfg.ConnConfig.Host = "127.0.0.1"
	cfg.ConnConfig.Port = uint16(ln.Addr().(*net.TCPAddr).Port)
	cfg.ConnConfig.TLSConfig = nil
	cfg.ConnConfig.Fallbacks = nil
	pool, e := pgxpool.NewWithConfig(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		pool.Close()
		ln.Close()
		p.mu.Lock()
		for _, c := range p.conns {
			c.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
	return pool, p
}
func readFrame(c net.Conn) (byte, []byte, error) {
	var h [5]byte
	if _, e := io.ReadFull(c, h[:]); e != nil {
		return 0, nil, e
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n < 4 || n > 16*1024*1024 {
		return 0, nil, errors.New("invalid proxy frame")
	}
	b := make([]byte, n-4)
	_, e := io.ReadFull(c, b)
	return h[0], b, e
}
func writeFrame(c net.Conn, kind byte, b []byte) error {
	h := make([]byte, 5)
	h[0] = kind
	binary.BigEndian.PutUint32(h[1:], uint32(len(b)+4))
	_, e := c.Write(append(h, b...))
	return e
}

type exitCommitTracer struct{}
type commitKey struct{}

func (exitCommitTracer) TraceQueryStart(c context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return context.WithValue(c, commitKey{}, strings.EqualFold(d.SQL, "commit"))
}
func (exitCommitTracer) TraceQueryEnd(c context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	if yes, _ := c.Value(commitKey{}).(bool); yes && d.Err == nil {
		os.Exit(0)
	}
}
func TestAppendPGCommitOutcome(t *testing.T) {
	if schema := os.Getenv("IM_APPEND_CHILD_SCHEMA"); schema != "" {
		cfg, e := pgxpool.ParseConfig(os.Getenv("IM_IMPORT_APPLY_TEST_DATABASE_URL"))
		if e != nil {
			os.Exit(20)
		}
		cfg.ConnConfig.Tracer = exitCommitTracer{}
		pool, e := pgxpool.NewWithConfig(context.Background(), cfg)
		if e != nil {
			os.Exit(21)
		}
		s, _ := NewService(pool, schema)
		p := access.ImportPrincipal{Identity: access.TrustedIdentity{TenantID: fixtureTenant, UserID: fixtureActor, ActingMembershipID: fixtureMembership}, Issuer: "https://sso.test", Subject: "actor", ExpiresAt: time.Now().Add(time.Hour)}
		_, e = s.Apply(context.Background(), p, fixtureRequest, applyInput(t))
		if e != nil {
			os.Exit(22)
		}
		os.Exit(23)
	}
	for _, scenario := range []string{"beforeCommit", "afterCommit", "cancelBeforeWrite", "processRestart"} {
		t.Run(scenario, func(t *testing.T) {
			f := appendDB(t, 22)
			actor := seedApplyActor(t, f)
			ctx := context.Background()
			plain, _ := NewService(f.Pool, f.Schema)
			if scenario == "processRestart" {
				exe, e := os.Executable()
				if e != nil {
					t.Fatal(e)
				}
				childCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(childCtx, exe, "-test.run=^TestAppendPGCommitOutcome$", "-test.count=1")
				cmd.Env = append(os.Environ(), "IM_APPEND_CHILD_SCHEMA="+f.Schema)
				if e = cmd.Run(); e != nil {
					t.Fatal("child did not exit at successful real COMMIT", e)
				}
				t.Log("child_exit_after_backend_COMMIT_before_service_return")
			} else if scenario == "cancelBeforeWrite" {
				s, pause := pausedService(t, f, "SAVEPOINT import_business", false)
				cancelCtx, cancel := context.WithCancel(ctx)
				done := make(chan error, 1)
				go func() { _, e := s.Apply(cancelCtx, actor, fixtureRequest, applyInput(t)); done <- e }()
				awaitPause(t, pause)
				cancel()
				close(pause.release)
				if e := <-done; !errors.Is(e, ErrRetryable) {
					t.Fatal("cancel before write mislabeled", e)
				}
			} else {
				pool, proxy := faultProxy(t, f, scenario == "beforeCommit")
				s, _ := NewService(pool, f.Schema)
				_, e := s.Apply(ctx, actor, fixtureRequest, applyInput(t))
				if !errors.Is(e, ErrCommitUnknown) {
					t.Fatal("actual COMMIT response failure not unknown", e)
				}
				select {
				case stage := <-proxy.cut:
					t.Log(stage)
				case <-time.After(time.Second):
					t.Fatal("proxy did not intercept actual COMMIT")
				}
			}
			r, e := plain.Apply(ctx, actor, fixtureRequest, applyInput(t))
			if e != nil || r.Receipt.State != Applied || r.Receipt.Counts["total"].Inserted != 6 {
				t.Fatal("recovery", e)
			}
			expectReplay := scenario == "afterCommit" || scenario == "processRestart"
			if r.Replay != expectReplay {
				t.Fatal("wrong commit outcome recovery")
			}
			get, e := plain.Get(ctx, actor, fixtureRequest)
			if e != nil || !bytes.Equal(r.Encoded, get.Encoded) {
				t.Fatal("GET disagrees", e)
			}
			if c := databaseCounts(t, f); c != [8]int{2, 2, 1, 2, 2, 1, 1, 1} {
				t.Fatal("partial or duplicate recovery", c)
			}
			var n int
			if e = f.Admin.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_locks WHERE locktype='advisory' AND pid IN (SELECT pid FROM pg_catalog.pg_stat_activity WHERE usename=$1)", f.Pool.Config().ConnConfig.User).Scan(&n); e != nil || n != 0 {
				t.Fatal("recovery retained session lock", e, n)
			}
		})
	}
}
