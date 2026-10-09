package realtime

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/receiptoutbox"
	"github.com/redis/go-redis/v9"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

type RealtimeReceiptResolver interface {
	ResolveReceiptEvent(context.Context, string, string, string, int64) ([]string, error)
}
type receiptStreamEvent struct {
	eventID, tenantID, conversationID string
	revision                          int64
}

func parseReceiptStreamEvent(v map[string]any) (receiptStreamEvent, bool) {
	if len(v) != 5 {
		return receiptStreamEvent{}, false
	}
	field := func(k string) string { s, _ := v[k].(string); return s }
	if field("event_type") != "receipt_changed" {
		return receiptStreamEvent{}, false
	}
	e := receiptStreamEvent{eventID: field("event_id"), tenantID: field("tenant_id"), conversationID: field("conversation_id")}
	if !uuidPattern.MatchString(e.eventID) || !uuidPattern.MatchString(e.tenantID) || !uuidPattern.MatchString(e.conversationID) {
		return receiptStreamEvent{}, false
	}
	n, err := strconv.ParseInt(field("revision"), 10, 64)
	if err != nil || n < 1 || strconv.FormatInt(n, 10) != field("revision") {
		return receiptStreamEvent{}, false
	}
	e.eventID = strings.ToLower(e.eventID)
	e.tenantID = strings.ToLower(e.tenantID)
	e.conversationID = strings.ToLower(e.conversationID)
	e.revision = n
	return e, true
}

// AttachReceiptStream adds coalesced receipt signals to the same local sockets.
// Its independent presence never closes legacy message fanout/readiness. If the
// producer is absent, the reader waits for recovery and HTTP polling still works.
func (f *Fanout) AttachReceiptStream(ctx context.Context, client *redis.Client, stream string, resolver RealtimeReceiptResolver) (bool, error) {
	if f == nil || ctx == nil || client == nil || stream == "" || resolver == nil {
		return false, ErrStreamUnconfigured
	}
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	present, e := receiptoutbox.PublisherPresent(checkCtx, client, stream)
	if e != nil {
		return false, e
	}
	tail, e := client.XRevRangeN(checkCtx, stream, "+", "-", 1).Result()
	if e != nil {
		return false, e
	}
	cursor := "0-0"
	if len(tail) > 0 {
		cursor = tail[0].ID
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.done:
		return false, ErrStreamUnconfigured
	default:
	}
	if f.receiptAttached {
		return false, errors.New("receipt stream already attached")
	}
	f.receiptAttached = true
	go f.readReceipts(ctx, client, stream, cursor, resolver)
	return present, nil
}
func (f *Fanout) receiptPause(ctx context.Context) bool {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-f.done:
		return false
	case <-timer.C:
		return true
	}
}
func (f *Fanout) readReceipts(ctx context.Context, client *redis.Client, stream, cursor string, resolver RealtimeReceiptResolver) {
	seen := map[string]bool{}
	order := make([]string, 0, maxRecentEventIDs)
	degraded := false
	for ctx.Err() == nil {
		select {
		case <-f.done:
			return
		default:
		}
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		present, e := receiptoutbox.PublisherPresent(checkCtx, client, stream)
		cancel()
		if e != nil || !present {
			if !degraded && ctx.Err() == nil {
				slog.Warn("receipt notifications degraded; HTTP polling remains available", "error_code", "receipt_publisher_unavailable")
			}
			degraded = true
			if !f.receiptPause(ctx) {
				return
			}
			continue
		}
		degraded = false
		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		streams, e := client.XRead(readCtx, &redis.XReadArgs{Streams: []string{stream, cursor}, Count: 100, Block: time.Second}).Result()
		cancel()
		if errors.Is(e, redis.Nil) {
			continue
		}
		if e != nil {
			if ctx.Err() != nil {
				return
			}
			if !f.receiptPause(ctx) {
				return
			}
			continue
		}
		for _, result := range streams {
			for _, message := range result.Messages {
				cursor = message.ID
				event, valid := parseReceiptStreamEvent(message.Values)
				if !valid || seen[event.eventID] {
					continue
				}
				lookupCtx, cancel := context.WithTimeout(ctx, recipientLookupTimeout)
				users, e := resolver.ResolveReceiptEvent(lookupCtx, event.tenantID, event.conversationID, event.eventID, event.revision)
				cancel()
				if e != nil {
					if ctx.Err() == nil {
						slog.Warn("receipt notifications degraded; HTTP polling remains available", "error_code", "receipt_lookup_unavailable")
					}
					continue
				}
				if len(users) == 0 {
					continue
				}
				// Recheck independent presence after a slow durable-target lookup.
				checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				present, e := receiptoutbox.PublisherPresent(checkCtx, client, stream)
				cancel()
				if e != nil || !present {
					continue
				}
				seen[event.eventID] = true
				order = append(order, event.eventID)
				if len(order) > maxRecentEventIDs {
					delete(seen, order[0])
					order[0] = ""
					order = order[1:]
				}
				for _, user := range users {
					if uuidPattern.MatchString(user) {
						f.signal(event.tenantID, user)
					}
				}
			}
		}
	}
}
