package outbox

import (
	"context"
	"errors"
	"strconv"

	"github.com/redis/go-redis/v9"
)

var ErrRedisPublisherUnconfigured = errors.New("Redis publisher requires client and stream")

const DefaultStream = "enterprise-im:message-created:v1"

// RedisPublisher publishes identifier-only notifications. Redis is not the
// source of truth for message content or delivery status.
type RedisPublisher struct {
	Client *redis.Client
	Stream string
}

func (p RedisPublisher) Publish(ctx context.Context, event Event) error {
	if p.Client == nil || p.Stream == "" {
		return ErrRedisPublisherUnconfigured
	}
	return p.Client.XAdd(ctx, &redis.XAddArgs{
		Stream: p.Stream,
		Values: map[string]any{
			"event_id":        event.ID,
			"tenant_id":       event.TenantID,
			"conversation_id": event.ConversationID,
			"message_id":      event.MessageID,
			"seq":             strconv.FormatInt(event.Seq, 10),
			"event_type":      event.EventType,
		},
	}).Err()
}
