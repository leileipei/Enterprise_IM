package receiptoutbox

import (
	"context"
	"errors"
	"github.com/redis/go-redis/v9"
	"regexp"
	"strconv"
	"strings"
)

const DefaultStream = "enterprise-im:receipt-changed:v1"

var ErrRedisPublisherUnconfigured = errors.New("receipt publisher requires Redis and stream")
var ErrInvalidEvent = errors.New("invalid receipt event")
var uuid = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type RedisPublisher struct {
	Client *redis.Client
	Stream string
}

func (p RedisPublisher) Publish(ctx context.Context, e Event) error {
	if p.Client == nil || p.Stream == "" {
		return ErrRedisPublisherUnconfigured
	}
	if e.Revision < 1 || !uuid.MatchString(e.ID) || !uuid.MatchString(e.TenantID) || !uuid.MatchString(e.ConversationID) {
		return ErrInvalidEvent
	}
	return p.Client.XAdd(ctx, &redis.XAddArgs{Stream: p.Stream, Values: map[string]any{"event_id": strings.ToLower(e.ID), "tenant_id": strings.ToLower(e.TenantID), "conversation_id": strings.ToLower(e.ConversationID), "revision": strconv.FormatInt(e.Revision, 10), "event_type": "receipt_changed"}}).Err()
}
