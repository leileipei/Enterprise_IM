package realtime

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/redis/go-redis/v9"
)

var ErrStreamUnconfigured = errors.New("stream fanout requires Redis, stream and recipient resolver")
var ErrPublisherUnavailable = errors.New("matching outbox publisher is unavailable")

const maxRecentEventIDs = 10000
const recipientLookupTimeout = 3 * time.Second

type RecipientResolver interface {
	ResolveMessageEvent(context.Context, string, string, string, string, int64) ([]string, error)
}

type streamEvent struct {
	eventID, tenantID, conversationID, messageID string
	seq                                          int64
}

// Fanout is local to one API process. Every process reads the Redis Stream
// independently and sends a coalesced signal to its own connected devices.
type Fanout struct {
	mu          sync.Mutex
	subscribers map[string]map[chan struct{}]struct{}
	done        chan struct{}
	stopOnce    sync.Once
}

func (f *Fanout) Done() <-chan struct{} { return f.done }

func (f *Fanout) stop() {
	f.stopOnce.Do(func() { close(f.done) })
}

func subscriberKey(tenantID, userID string) string {
	return strings.ToLower(tenantID + ":" + userID)
}

func (f *Fanout) Subscribe(id access.TrustedIdentity) (<-chan struct{}, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.done:
		return nil, func() {}
	default:
	}
	key := subscriberKey(id.TenantID, id.UserID)
	ch := make(chan struct{}, 1)
	if f.subscribers[key] == nil {
		f.subscribers[key] = make(map[chan struct{}]struct{})
	}
	f.subscribers[key][ch] = struct{}{}
	return ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.subscribers[key], ch)
		if len(f.subscribers[key]) == 0 {
			delete(f.subscribers, key)
		}
	}
}

func (f *Fanout) signal(tenantID, userID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subscribers[subscriberKey(tenantID, userID)] {
		select {
		case ch <- struct{}{}:
		default: // one pending signal is sufficient until the client pulls
		}
	}
}

func parseStreamEvent(values map[string]any) (streamEvent, bool) {
	stringField := func(name string) string {
		value, ok := values[name].(string)
		if !ok {
			return ""
		}
		return value
	}
	if stringField("event_type") != "message_created" {
		return streamEvent{}, false
	}
	event := streamEvent{eventID: stringField("event_id"), tenantID: stringField("tenant_id"),
		conversationID: stringField("conversation_id"), messageID: stringField("message_id")}
	if !uuidPattern.MatchString(event.eventID) || !uuidPattern.MatchString(event.tenantID) ||
		!uuidPattern.MatchString(event.conversationID) || !uuidPattern.MatchString(event.messageID) {
		return streamEvent{}, false
	}
	seq, err := strconv.ParseInt(stringField("seq"), 10, 64)
	if err != nil || seq < 1 {
		return streamEvent{}, false
	}
	event.eventID = strings.ToLower(event.eventID)
	event.tenantID = strings.ToLower(event.tenantID)
	event.conversationID = strings.ToLower(event.conversationID)
	event.messageID = strings.ToLower(event.messageID)
	event.seq = seq
	return event, true
}

// StartStreamFanout captures the tail before HTTP starts accepting sockets.
// A restart relies on the ready frame and PostgreSQL pull for recovery.
func StartStreamFanout(ctx context.Context, client *redis.Client, stream string, resolver RecipientResolver) (*Fanout, error) {
	if ctx == nil || client == nil || stream == "" || resolver == nil {
		return nil, ErrStreamUnconfigured
	}
	present, err := outbox.PublisherPresent(ctx, client, stream)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, ErrPublisherUnavailable
	}
	tail, err := client.XRevRangeN(ctx, stream, "+", "-", 1).Result()
	if err != nil {
		return nil, err
	}
	cursor := "0-0"
	if len(tail) != 0 {
		cursor = tail[0].ID
	}
	f := &Fanout{subscribers: make(map[string]map[chan struct{}]struct{}), done: make(chan struct{})}
	go f.read(ctx, client, stream, cursor, resolver)
	return f, nil
}

func (f *Fanout) read(ctx context.Context, client *redis.Client, stream, cursor string, resolver RecipientResolver) {
	defer f.stop()
	seen := make(map[string]struct{})
	order := make([]string, 0, maxRecentEventIDs)
	lastPresenceCheck := time.Time{}
	checkPublisher := func() bool {
		if !lastPresenceCheck.IsZero() && time.Since(lastPresenceCheck) < time.Second {
			return true
		}
		presenceCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		present, err := outbox.PublisherPresent(presenceCtx, client, stream)
		cancel()
		if err != nil || !present {
			if ctx.Err() == nil {
				slog.Error("realtime publisher unavailable", "error", err)
			}
			return false
		}
		lastPresenceCheck = time.Now()
		return true
	}
	for ctx.Err() == nil {
		if !checkPublisher() {
			return
		}
		streams, err := client.XRead(ctx, &redis.XReadArgs{Streams: []string{stream, cursor}, Count: 100,
			Block: time.Second}).Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("realtime stream reader stopped", "error", err)
			}
			return
		}
		for _, result := range streams {
			for _, message := range result.Messages {
				if !checkPublisher() {
					return
				}
				cursor = message.ID
				event, valid := parseStreamEvent(message.Values)
				if !valid {
					slog.Warn("realtime stream event malformed")
					continue
				}
				if _, duplicate := seen[event.eventID]; duplicate {
					continue
				}
				lookupCtx, cancel := context.WithTimeout(ctx, recipientLookupTimeout)
				users, err := resolver.ResolveMessageEvent(lookupCtx, event.tenantID, event.eventID,
					event.conversationID, event.messageID, event.seq)
				cancel()
				if err != nil {
					slog.Error("realtime recipient lookup failed", "error", err)
					return
				}
				if len(users) == 0 {
					continue // an unverified event must not suppress a later valid publish
				}
				seen[event.eventID] = struct{}{}
				order = append(order, event.eventID)
				if len(order) > maxRecentEventIDs {
					delete(seen, order[0])
					order[0] = ""
					order = order[1:]
				}
				for _, userID := range users {
					if uuidPattern.MatchString(userID) {
						f.signal(event.tenantID, userID)
					}
				}
			}
		}
	}
}
