package realtime_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/leileipei/Enterprise_IM/internal/realtime"
	"github.com/redis/go-redis/v9"
)

type recipientsStub struct {
	users []string
	err   error
}

type resolverFunc func(context.Context, string, string, string, string, int64) ([]string, error)

func (f resolverFunc) ResolveMessageEvent(ctx context.Context, tenant, event, conversation, message string, seq int64) ([]string, error) {
	return f(ctx, tenant, event, conversation, message, seq)
}

func (s recipientsStub) ResolveMessageEvent(context.Context, string, string, string, string, int64) ([]string, error) {
	return s.users, s.err
}

const streamConversation = "00000000-0000-4000-8000-000000000301"
const streamMessage = "00000000-0000-4000-8000-000000000302"

func publishStreamEvent(t *testing.T, client *redis.Client, stream, eventID, tenant string, seq int64) {
	t.Helper()
	if err := client.XAdd(context.Background(), &redis.XAddArgs{Stream: stream, Values: map[string]any{
		"event_id": eventID, "tenant_id": tenant, "conversation_id": streamConversation,
		"message_id": streamMessage, "event_type": "message_created", "seq": strconv.FormatInt(seq, 10),
	}}).Err(); err != nil {
		t.Fatal(err)
	}
}

func waitSignal(t *testing.T, signals <-chan struct{}) {
	t.Helper()
	select {
	case <-signals:
	case <-time.After(3 * time.Second):
		t.Fatal("online notification not delivered")
	}
}

func markPublisher(t *testing.T, client *redis.Client, stream string) {
	t.Helper()
	if err := outbox.RefreshPublisherPresence(context.Background(), client, stream); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Del(context.Background(), outbox.PublisherPresenceKey(stream)) })
}

func TestStreamFanoutEveryInstanceReceivesAndIsolatesUsers(t *testing.T) {
	client := redisDB(t)
	stream := fmt.Sprintf("enterprise-im:test:fanout:%d", time.Now().UnixNano())
	t.Cleanup(func() { client.Del(context.Background(), stream) })
	markPublisher(t, client, stream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resolver := recipientsStub{users: []string{identity.UserID}}
	first, err := realtime.StartStreamFanout(ctx, client, stream, resolver)
	if err != nil {
		t.Fatal(err)
	}
	second, err := realtime.StartStreamFanout(ctx, client, stream, resolver)
	if err != nil {
		t.Fatal(err)
	}
	firstSignals, unsubscribeFirst := first.Subscribe(identity)
	defer unsubscribeFirst()
	secondSignals, unsubscribeSecond := second.Subscribe(identity)
	defer unsubscribeSecond()
	foreignSignals, unsubscribeForeign := first.Subscribe(access.TrustedIdentity{
		TenantID: "00000000-0000-4000-8000-000000000202", UserID: identity.UserID})
	defer unsubscribeForeign()
	otherSignals, unsubscribeOther := first.Subscribe(access.TrustedIdentity{
		TenantID: identity.TenantID, UserID: "00000000-0000-4000-8000-000000000299"})
	defer unsubscribeOther()
	publishStreamEvent(t, client, stream, "00000000-0000-4000-8000-000000000311", identity.TenantID, 1)
	waitSignal(t, firstSignals)
	waitSignal(t, secondSignals)
	select {
	case <-foreignSignals:
		t.Fatal("cross-tenant notification")
	case <-otherSignals:
		t.Fatal("unrelated user notification")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestStreamFanoutDeduplicatesAndCoalesces(t *testing.T) {
	client := redisDB(t)
	stream := fmt.Sprintf("enterprise-im:test:dedupe:%d", time.Now().UnixNano())
	t.Cleanup(func() { client.Del(context.Background(), stream) })
	markPublisher(t, client, stream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fanout, err := realtime.StartStreamFanout(ctx, client, stream, recipientsStub{users: []string{identity.UserID}})
	if err != nil {
		t.Fatal(err)
	}
	signals, unsubscribe := fanout.Subscribe(identity)
	defer unsubscribe()
	eventID := "00000000-0000-4000-8000-000000000312"
	publishStreamEvent(t, client, stream, eventID, identity.TenantID, 1)
	waitSignal(t, signals)
	publishStreamEvent(t, client, stream, eventID, identity.TenantID, 1)
	select {
	case <-signals:
		t.Fatal("duplicate event notified twice")
	case <-time.After(200 * time.Millisecond):
	}
	for i := 2; i <= 5; i++ {
		publishStreamEvent(t, client, stream, fmt.Sprintf("00000000-0000-4000-8000-%012d", 312+i), identity.TenantID, int64(i))
	}
	time.Sleep(200 * time.Millisecond)
	waitSignal(t, signals)
	select {
	case <-signals:
		t.Fatal("burst was not coalesced")
	default:
	}
}

func TestStreamFanoutFailsClosedOnReaderFailure(t *testing.T) {
	client := redisDB(t)
	stream := fmt.Sprintf("enterprise-im:test:failure:%d", time.Now().UnixNano())
	markPublisher(t, client, stream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fanout, err := realtime.StartStreamFanout(ctx, client, stream, recipientsStub{users: []string{identity.UserID}})
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	select {
	case <-fanout.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("reader failure did not close fanout")
	}
}

func TestStreamFanoutDoesNotDeduplicateUnverifiedEvent(t *testing.T) {
	client := redisDB(t)
	stream := fmt.Sprintf("enterprise-im:test:unverified:%d", time.Now().UnixNano())
	t.Cleanup(func() { client.Del(context.Background(), stream) })
	markPublisher(t, client, stream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstProcessed := make(chan struct{})
	calls := 0
	resolver := resolverFunc(func(context.Context, string, string, string, string, int64) ([]string, error) {
		calls++
		if calls == 1 {
			close(firstProcessed)
			return nil, nil
		}
		return []string{identity.UserID}, nil
	})
	fanout, err := realtime.StartStreamFanout(ctx, client, stream, resolver)
	if err != nil {
		t.Fatal(err)
	}
	signals, unsubscribe := fanout.Subscribe(identity)
	defer unsubscribe()
	eventID := "00000000-0000-4000-8000-000000000321"
	publishStreamEvent(t, client, stream, eventID, identity.TenantID, 1)
	select {
	case <-firstProcessed:
	case <-time.After(3 * time.Second):
		t.Fatal("unverified event not read")
	}
	publishStreamEvent(t, client, stream, eventID, identity.TenantID, 1)
	waitSignal(t, signals)
}

func TestStreamFanoutLookupTimeoutClosesReadiness(t *testing.T) {
	client := redisDB(t)
	stream := fmt.Sprintf("enterprise-im:test:slow-lookup:%d", time.Now().UnixNano())
	t.Cleanup(func() { client.Del(context.Background(), stream) })
	markPublisher(t, client, stream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fanout, err := realtime.StartStreamFanout(ctx, client, stream,
		resolverFunc(func(ctx context.Context, _, _, _, _ string, _ int64) ([]string, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}))
	if err != nil {
		t.Fatal(err)
	}
	publishStreamEvent(t, client, stream, "00000000-0000-4000-8000-000000000322", identity.TenantID, 1)
	select {
	case <-fanout.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("stuck recipient lookup did not fail closed")
	}
}

func TestStreamFanoutRejectsMismatchedOrExpiredPublisher(t *testing.T) {
	client := redisDB(t)
	stream := fmt.Sprintf("enterprise-im:test:publisher-match:%d", time.Now().UnixNano())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	markPublisher(t, client, stream+":other")
	if _, err := realtime.StartStreamFanout(ctx, client, stream, recipientsStub{}); !errors.Is(err, realtime.ErrPublisherUnavailable) {
		t.Fatalf("wrong producer stream accepted: %v", err)
	}
	markPublisher(t, client, stream)
	fanout, err := realtime.StartStreamFanout(ctx, client, stream, recipientsStub{})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Del(ctx, outbox.PublisherPresenceKey(stream)).Err(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fanout.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("expired publisher did not stop notifications")
	}
}

func TestStreamFanoutDetectsPublisherLossInsideSlowBatch(t *testing.T) {
	client := redisDB(t)
	stream := fmt.Sprintf("enterprise-im:test:slow-batch:%d", time.Now().UnixNano())
	t.Cleanup(func() { client.Del(context.Background(), stream) })
	markPublisher(t, client, stream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstLookup := make(chan struct{})
	calls := 0
	resolver := resolverFunc(func(ctx context.Context, _, _, _, _ string, _ int64) ([]string, error) {
		calls++
		if calls == 1 {
			close(firstLookup)
		}
		select {
		case <-time.After(1200 * time.Millisecond):
			return []string{identity.UserID}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	fanout, err := realtime.StartStreamFanout(ctx, client, stream, resolver)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		publishStreamEvent(t, client, stream, fmt.Sprintf("00000000-0000-4000-8000-%012d", 400+i), identity.TenantID, int64(i))
	}
	select {
	case <-firstLookup:
	case <-time.After(3 * time.Second):
		t.Fatal("first event was not read")
	}
	if err := client.Del(ctx, outbox.PublisherPresenceKey(stream)).Err(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fanout.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("publisher loss remained hidden during slow batch")
	}
}
