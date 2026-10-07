package policystore_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"github.com/leileipei/Enterprise_IM/internal/realtime"
	"github.com/leileipei/Enterprise_IM/internal/testfixtures"
	"github.com/redis/go-redis/v9"
)

// This child runs the production HTTP handlers and Redis fanout in its own OS
// process. A fixed identity is used because customer IdP integration is separate.
func TestRealtimeAPIChild(t *testing.T) {
	if os.Getenv("IM_TEST_REALTIME_API_CHILD") != "1" {
		t.Skip("API process helper")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("IM_TEST_PROCESS_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	options, err := redis.ParseURL(os.Getenv("IM_TEST_REDIS_URL"))
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(options)
	defer client.Close()
	service := policystore.Service{DB: pool, Now: func() time.Time { return at }}
	fanout, err := realtime.StartStreamFanout(ctx, client, os.Getenv("IM_TEST_PROCESS_STREAM"), service)
	if err != nil {
		t.Fatal(err)
	}
	auth := realtimeE2EAuth{}
	base, err := httpserver.HandlerWithConversations(httpserver.Handler(nil), auth, service)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpserver.HandlerWithRealtimeNotifications(base, auth, service,
		realtime.RedisTickets{Client: client}, ctx, fanout)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", os.Getenv("IM_TEST_PROCESS_LISTEN"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.WriteFile(os.Getenv("IM_TEST_PROCESS_READY_FILE"), []byte(listener.Addr().String()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := http.Serve(listener, handler); err != nil {
		t.Fatal(err)
	}
}

func processDatabaseURL(t *testing.T, schema string) string {
	t.Helper()
	parsed, err := url.Parse(os.Getenv("IM_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema+",public")
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func integrationProcessExit(err error) string {
	if err == nil {
		return "exit:0"
	}
	if exit, ok := err.(*exec.ExitError); ok {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return "signal:" + status.Signal().String()
		}
		return fmt.Sprintf("exit:%d", exit.ExitCode())
	}
	return "wait_error"
}

func startRealtimeProcess(t *testing.T, cmd *exec.Cmd, ready func() bool) {
	t.Helper()
	logFile, err := os.CreateTemp(t.TempDir(), "process-*.log")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		t.Fatal(err)
	}
	var registration *testfixtures.IntegrationProcess
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		waitErr := cmd.Wait()
		if registration != nil {
			if err := registration.Exited("signal:killed", integrationProcessExit(waitErr)); err != nil {
				t.Error(err)
			}
		}
		_ = logFile.Close()
	})
	registration, err = testfixtures.RegisterIntegrationProcess(cmd, os.Getenv("IM_TEST_INTEGRATION_GATE"), t.Name())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			if err := registration.Ready(); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	output, _ := os.ReadFile(logFile.Name())
	t.Fatalf("process did not become ready: %s", output)
}

func TestMultiProcessRealtimeWorkerFanoutAndReconnect(t *testing.T) {
	if os.Getenv("IM_TEST_REDIS_URL") == "" {
		t.Skip("set IM_TEST_REDIS_URL for multi-process integration test")
	}
	conn := db(t)
	seedDirectConversation(t, conn)
	var schema string
	if err := conn.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	databaseURL := processDatabaseURL(t, schema)
	options, err := redis.ParseURL(os.Getenv("IM_TEST_REDIS_URL"))
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	stream := fmt.Sprintf("enterprise-im:test:process:%d", time.Now().UnixNano())
	t.Cleanup(func() {
		if err := client.Del(context.Background(), stream, outbox.PublisherPresenceKey(stream)).Err(); err != nil {
			t.Errorf("clean Redis test keys: %v", err)
		}
	})

	workerBinary := filepath.Join(t.TempDir(), "im-outbox-worker")
	buildCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", workerBinary, "../../cmd/im-outbox-worker")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build production worker: %v: %s", err, output)
	}
	worker := exec.Command(workerBinary)
	worker.Env = append(os.Environ(), "IM_DATABASE_URL="+databaseURL,
		"IM_OUTBOX_REDIS_URL="+os.Getenv("IM_TEST_REDIS_URL"), "IM_OUTBOX_STREAM="+stream)
	startRealtimeProcess(t, worker, func() bool {
		return client.Exists(context.Background(), outbox.PublisherPresenceKey(stream)).Val() == 1
	})

	addresses := make([]string, 2)
	for index := range addresses {
		readyFile := filepath.Join(t.TempDir(), "api-ready")
		api := exec.Command(os.Args[0], "-test.run=^TestRealtimeAPIChild$")
		api.Env = append(os.Environ(), "IM_TEST_REALTIME_API_CHILD=1",
			"IM_TEST_PROCESS_DATABASE_URL="+databaseURL, "IM_TEST_PROCESS_STREAM="+stream,
			"IM_TEST_PROCESS_LISTEN=127.0.0.1:0", "IM_TEST_PROCESS_READY_FILE="+readyFile)
		startRealtimeProcess(t, api, func() bool {
			data, err := os.ReadFile(readyFile)
			if err != nil {
				return false
			}
			addresses[index] = strings.TrimSpace(string(data))
			return addresses[index] != ""
		})
	}
	if addresses[0] == addresses[1] {
		t.Fatalf("API processes reported the same address: %s", addresses[0])
	}
	firstURL, secondURL := "http://"+addresses[0], "http://"+addresses[1]
	httpClient := &http.Client{Timeout: 5 * time.Second}
	first := realtimeE2EConnect(t, firstURL, realtimeE2ETicket(t, httpClient, firstURL))
	second := realtimeE2EConnect(t, secondURL, realtimeE2ETicket(t, httpClient, secondURL))

	realtimeE2ESend(t, httpClient, firstURL, clientUUIDv7(at, 1301), "跨进程消息一")
	realtimeE2EFrame(t, first, `{"type":"sync_required"}`)
	realtimeE2EFrame(t, second, `{"type":"sync_required"}`)
	realtimeE2EPull(t, httpClient, firstURL, 0, 1, "跨进程消息一")
	realtimeE2EPull(t, httpClient, secondURL, 0, 1, "跨进程消息一")

	first.CloseNow()
	realtimeE2ESend(t, httpClient, secondURL, clientUUIDv7(at, 1302), "跨进程消息二")
	realtimeE2EFrame(t, second, `{"type":"sync_required"}`)
	reconnected := realtimeE2EConnect(t, firstURL, realtimeE2ETicket(t, httpClient, firstURL))
	realtimeE2EPull(t, httpClient, firstURL, 1, 2, "跨进程消息二")
	reconnected.CloseNow()
}
