package outbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const PublisherPresenceTTL = 10 * time.Second

var ErrPublisherPresenceUnconfigured = errors.New("publisher presence requires Redis and stream")

// PublisherPresenceKey identifies the configured Stream without embedding its
// raw deployment-specific name in the Redis key.
func PublisherPresenceKey(stream string) string {
	digest := sha256.Sum256([]byte(stream))
	return "enterprise-im:outbox:publisher:v1:" + hex.EncodeToString(digest[:])
}

func RefreshPublisherPresence(ctx context.Context, client redis.Cmdable, stream string) error {
	if client == nil || stream == "" {
		return ErrPublisherPresenceUnconfigured
	}
	return client.Set(ctx, PublisherPresenceKey(stream), "active", PublisherPresenceTTL).Err()
}

func PublisherPresent(ctx context.Context, client redis.Cmdable, stream string) (bool, error) {
	if client == nil || stream == "" {
		return false, ErrPublisherPresenceUnconfigured
	}
	count, err := client.Exists(ctx, PublisherPresenceKey(stream)).Result()
	return count == 1, err
}
