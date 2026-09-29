package realtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/redis/go-redis/v9"
)

const TicketTTL = 30 * time.Second
const ticketPrefix = "enterprise-im:realtime:ticket:v1:"

var (
	ErrInvalidTicket     = errors.New("invalid or expired realtime ticket")
	ErrTicketUnavailable = errors.New("realtime ticket store unavailable")
	uuidPattern          = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

type RedisTickets struct {
	Client redis.Cmdable
}

type ticketIdentity struct {
	TenantID           string `json:"tenant_id"`
	UserID             string `json:"user_id"`
	ActingMembershipID string `json:"acting_membership_id"`
}

func validIdentity(id access.TrustedIdentity) bool {
	return uuidPattern.MatchString(id.TenantID) && uuidPattern.MatchString(id.UserID) &&
		uuidPattern.MatchString(id.ActingMembershipID)
}

// TicketKey validates the opaque ticket and returns a Redis key containing
// only its digest. The raw ticket is never persisted or logged.
func TicketKey(ticket string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(ticket)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != ticket {
		return "", ErrInvalidTicket
	}
	digest := sha256.Sum256(decoded)
	return ticketPrefix + hex.EncodeToString(digest[:]), nil
}

func (s RedisTickets) Issue(ctx context.Context, id access.TrustedIdentity) (string, error) {
	if s.Client == nil {
		return "", ErrTicketUnavailable
	}
	if !validIdentity(id) {
		return "", ErrInvalidTicket
	}
	value, err := json.Marshal(ticketIdentity{TenantID: strings.ToLower(id.TenantID),
		UserID: strings.ToLower(id.UserID), ActingMembershipID: strings.ToLower(id.ActingMembershipID)})
	if err != nil {
		return "", ErrTicketUnavailable
	}
	for attempt := 0; attempt < 3; attempt++ {
		secret := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, secret); err != nil {
			return "", errors.Join(ErrTicketUnavailable, err)
		}
		ticket := base64.RawURLEncoding.EncodeToString(secret)
		key, _ := TicketKey(ticket)
		created, err := s.Client.SetNX(ctx, key, value, TicketTTL).Result()
		if err != nil {
			return "", errors.Join(ErrTicketUnavailable, err)
		}
		if created {
			return ticket, nil
		}
	}
	return "", ErrTicketUnavailable
}

func (s RedisTickets) Consume(ctx context.Context, ticket string) (access.TrustedIdentity, error) {
	key, err := TicketKey(ticket)
	if err != nil {
		return access.TrustedIdentity{}, err
	}
	if s.Client == nil {
		return access.TrustedIdentity{}, ErrTicketUnavailable
	}
	value, err := s.Client.GetDel(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return access.TrustedIdentity{}, ErrInvalidTicket
	}
	if err != nil {
		return access.TrustedIdentity{}, errors.Join(ErrTicketUnavailable, err)
	}
	var stored ticketIdentity
	if err := json.Unmarshal([]byte(value), &stored); err != nil {
		return access.TrustedIdentity{}, ErrInvalidTicket
	}
	id := access.TrustedIdentity{TenantID: stored.TenantID, UserID: stored.UserID,
		ActingMembershipID: stored.ActingMembershipID}
	if !validIdentity(id) {
		return access.TrustedIdentity{}, ErrInvalidTicket
	}
	return id, nil
}
