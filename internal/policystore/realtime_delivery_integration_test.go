package policystore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"github.com/leileipei/Enterprise_IM/internal/realtime"
	"github.com/redis/go-redis/v9"
)

type realtimeE2EAuth struct{}

func (realtimeE2EAuth) Authenticate(_ context.Context, token string) (httpserver.VerifiedIdentity, error) {
	if token != "integration-token" {
		return httpserver.VerifiedIdentity{}, errors.New("invalid test token")
	}
	return httpserver.VerifiedIdentity{TenantID: tenantA, UserID: adminA}, nil
}

func realtimeE2ERequest(t *testing.T, method, url string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer integration-token")
	req.Header.Set("X-Acting-Membership-ID", adminM)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func realtimeE2ETicket(t *testing.T, client *http.Client, base string) string {
	t.Helper()
	res, err := client.Do(realtimeE2ERequest(t, http.MethodPost, base+"/api/v1/realtime/tickets", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil || res.StatusCode != http.StatusOK || len(body.Ticket) != 43 {
		t.Fatalf("issue ticket: status=%d body=%+v err=%v", res.StatusCode, body, err)
	}
	return body.Ticket
}

func realtimeE2EConnect(t *testing.T, base, ticket string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/api/v1/realtime",
		&websocket.DialOptions{Subprotocols: []string{"enterprise-im.v1", "ticket." + ticket}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	realtimeE2EFrame(t, conn, `{"type":"ready","resync_required":true}`)
	return conn
}

func realtimeE2EFrame(t *testing.T, conn *websocket.Conn, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, frame, err := conn.Read(ctx)
	if err != nil || string(frame) != want {
		t.Fatalf("websocket frame: got=%q want=%q err=%v", frame, want, err)
	}
}

func realtimeE2ESend(t *testing.T, client *http.Client, base string, requestID string, text string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"client_msg_id": requestID, "text": text})
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(realtimeE2ERequest(t, http.MethodPost,
		base+"/api/v1/conversations/"+directA+"/messages", body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var ack struct {
		Seq int64 `json:"seq"`
	}
	if err := json.NewDecoder(res.Body).Decode(&ack); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("send message: status=%d ack=%+v err=%v", res.StatusCode, ack, err)
	}
}

func realtimeE2EPull(t *testing.T, client *http.Client, base string, afterSeq int64, wantSeq int64, wantText string) {
	t.Helper()
	res, err := client.Do(realtimeE2ERequest(t, http.MethodGet,
		fmt.Sprintf("%s/api/v1/conversations/%s/messages?after_seq=%d&limit=10", base, directA, afterSeq), nil))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var page struct {
		Messages []struct {
			Seq  int64  `json:"seq"`
			Text string `json:"text"`
		} `json:"messages"`
		NextAfterSeq int64 `json:"next_after_seq"`
		HasMore      bool  `json:"has_more"`
	}
	if err := json.NewDecoder(res.Body).Decode(&page); err != nil || res.StatusCode != http.StatusOK ||
		len(page.Messages) != 1 || page.Messages[0].Seq != wantSeq || page.Messages[0].Text != wantText ||
		page.NextAfterSeq != wantSeq || page.HasMore {
		t.Fatalf("pull after %d: status=%d page=%+v err=%v", afterSeq, res.StatusCode, page, err)
	}
}

func TestTwoDeviceRealtimeFromCommittedMessageThroughRedisAndReconnect(t *testing.T) {
	rawRedis := os.Getenv("IM_TEST_REDIS_URL")
	if rawRedis == "" {
		t.Skip("set IM_TEST_REDIS_URL for real-time integration test")
	}
	conn := db(t)
	seedDirectConversation(t, conn)
	var schema string
	if err := conn.QueryRow(context.Background(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(os.Getenv("IM_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	redisOptions, err := redis.ParseURL(rawRedis)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(redisOptions)
	t.Cleanup(func() { client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	stream := fmt.Sprintf("enterprise-im:test:two-device:%d", time.Now().UnixNano())
	t.Cleanup(func() {
		if err := client.Del(context.Background(), stream, outbox.PublisherPresenceKey(stream)).Err(); err != nil {
			t.Errorf("clean Redis test keys: %v", err)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	presenceStopped := make(chan struct{})
	if err := outbox.RefreshPublisherPresence(context.Background(), client, stream); err != nil {
		cancel()
		t.Fatal(err)
	}
	presenceErrors := make(chan error, 1)
	go func() {
		defer close(presenceStopped)
		ticker := time.NewTicker(outbox.PublisherPresenceTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := outbox.RefreshPublisherPresence(ctx, client, stream); err != nil {
					presenceErrors <- err
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-presenceStopped
	}()
	service := policystore.Service{DB: pool, Now: func() time.Time { return at }}
	auth := realtimeE2EAuth{}
	servers := make([]*httptest.Server, 2)
	for index := range servers {
		fanout, err := realtime.StartStreamFanout(ctx, client, stream, service)
		if err != nil {
			t.Fatal(err)
		}
		base, err := httpserver.HandlerWithConversations(httpserver.Handler(nil), auth, service)
		if err != nil {
			t.Fatal(err)
		}
		handler, err := httpserver.HandlerWithRealtimeNotifications(base, auth, service,
			realtime.RedisTickets{Client: client}, ctx, fanout)
		if err != nil {
			t.Fatal(err)
		}
		servers[index] = httptest.NewServer(handler)
		t.Cleanup(servers[index].Close)
	}
	httpClient := &http.Client{Timeout: 5 * time.Second}
	first := realtimeE2EConnect(t, servers[0].URL, realtimeE2ETicket(t, httpClient, servers[0].URL))
	second := realtimeE2EConnect(t, servers[1].URL, realtimeE2ETicket(t, httpClient, servers[1].URL))

	realtimeE2ESend(t, httpClient, servers[0].URL, clientUUIDv7(at, 1201), "设备一发出")
	worker := outbox.Worker{DB: pool, Publisher: outbox.RedisPublisher{Client: client, Stream: stream}}
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("publish committed outbox: processed=%t err=%v", processed, err)
	}
	realtimeE2EFrame(t, first, `{"type":"sync_required"}`)
	realtimeE2EFrame(t, second, `{"type":"sync_required"}`)
	realtimeE2EPull(t, httpClient, servers[0].URL, 0, 1, "设备一发出")
	realtimeE2EPull(t, httpClient, servers[1].URL, 0, 1, "设备一发出")

	first.CloseNow()
	realtimeE2ESend(t, httpClient, servers[1].URL, clientUUIDv7(at, 1202), "断线期间发出")
	if processed, err := worker.ProcessOne(context.Background()); err != nil || !processed {
		t.Fatalf("publish second outbox: processed=%t err=%v", processed, err)
	}
	realtimeE2EFrame(t, second, `{"type":"sync_required"}`)
	reconnected := realtimeE2EConnect(t, servers[0].URL, realtimeE2ETicket(t, httpClient, servers[0].URL))
	realtimeE2EPull(t, httpClient, servers[0].URL, 1, 2, "断线期间发出")
	reconnected.CloseNow()
	select {
	case err := <-presenceErrors:
		t.Fatalf("refresh Redis publisher presence: %v", err)
	default:
	}
}
