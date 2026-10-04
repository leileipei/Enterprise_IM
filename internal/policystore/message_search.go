package policystore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/leileipei/Enterprise_IM/internal/access"
)

var ErrInvalidMessageSearch = errors.New("invalid message search")

const messageSearchScanLimit = 500

type MessageSearchPage struct {
	ConversationID string
	Messages       []PulledMessage
	HasMore        bool
	NextCursor     string
}

type messageSearchCursor struct {
	Tenant       string `json:"t"`
	User         string `json:"u"`
	Membership   string `json:"m"`
	Conversation string `json:"c"`
	Kind         string `json:"k"`
	Query        string `json:"q"`
	After        string `json:"a"`
}

// NormalizeMessageSearchQuery defines literal, case-insensitive substring matching.
// SQL wildcard and regexp characters have no special meaning.
func NormalizeMessageSearchQuery(query string) (string, error) {
	if !utf8.ValidString(query) || strings.ContainsRune(query, 0) {
		return "", ErrInvalidMessageSearch
	}
	query = strings.TrimSpace(query)
	if n := utf8.RuneCountInString(query); n < 2 || n > 100 {
		return "", ErrInvalidMessageSearch
	}
	return strings.ToLower(query), nil
}

func (s Service) SearchTextMessages(ctx context.Context, id access.TrustedIdentity, conversationID, query, cursor string, limit int) (MessageSearchPage, error) {
	return s.searchMessages(ctx, id, conversationID, "direct", query, cursor, limit)
}
func (s Service) SearchGroupTextMessages(ctx context.Context, id access.TrustedIdentity, conversationID, query, cursor string, limit int) (MessageSearchPage, error) {
	return s.searchMessages(ctx, id, conversationID, "group", query, cursor, limit)
}

// searchMessages matches only bodies released by the historical authorization
// pipeline. Search progression is based on timeline scanning, never hidden matches.
func (s Service) searchMessages(ctx context.Context, id access.TrustedIdentity, conversationID, kind, query, cursor string, limit int) (MessageSearchPage, error) {
	query, err := NormalizeMessageSearchQuery(query)
	if err != nil || limit < 1 || limit > 50 || !directoryUUIDPattern.MatchString(conversationID) {
		return MessageSearchPage{}, ErrInvalidMessageSearch
	}
	if !directoryUUIDPattern.MatchString(id.TenantID) || !directoryUUIDPattern.MatchString(id.UserID) || !directoryUUIDPattern.MatchString(id.ActingMembershipID) {
		return MessageSearchPage{}, ErrForbidden
	}
	conversationID = strings.ToLower(conversationID)
	binding := messageSearchCursor{Tenant: strings.ToLower(id.TenantID), User: strings.ToLower(id.UserID), Membership: strings.ToLower(id.ActingMembershipID), Conversation: conversationID, Kind: kind, Query: query}
	after := int64(0)
	if cursor != "" {
		if len(cursor) > 2048 {
			return MessageSearchPage{}, ErrInvalidMessageSearch
		}
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil {
			return MessageSearchPage{}, ErrInvalidMessageSearch
		}
		var c messageSearchCursor
		if json.Unmarshal(raw, &c) != nil {
			return MessageSearchPage{}, ErrInvalidMessageSearch
		}
		canonical, _ := json.Marshal(c)
		if base64.RawURLEncoding.EncodeToString(canonical) != cursor {
			return MessageSearchPage{}, ErrInvalidMessageSearch
		}
		after, e = strconv.ParseInt(c.After, 10, 64)
		if e != nil || after < 1 || strconv.FormatInt(after, 10) != c.After {
			return MessageSearchPage{}, ErrInvalidMessageSearch
		}
		binding.After = c.After
		if c != binding {
			return MessageSearchPage{}, ErrInvalidMessageSearch
		}
	}
	var scanned MessagePage
	if kind == "group" {
		scanned, err = s.readGroupTextMessages(ctx, id, conversationID, after, messageSearchScanLimit, "message_search")
	} else {
		scanned, err = s.readTextMessages(ctx, id, conversationID, after, messageSearchScanLimit, "message_search")
	}
	if err != nil {
		return MessageSearchPage{}, err
	}
	page := MessageSearchPage{ConversationID: scanned.ConversationID, Messages: make([]PulledMessage, 0, limit), HasMore: scanned.HasMore}
	next := scanned.NextAfterSeq
	for _, m := range scanned.Messages {
		if m.MessageType != MessageTypeText || m.Redacted || !strings.Contains(strings.ToLower(m.Text), query) {
			continue
		}
		if len(page.Messages) == limit {
			page.HasMore = true
			// Resume after the last delivered match so no undisclosed match is skipped.
			next = page.Messages[len(page.Messages)-1].Seq
			break
		}
		page.Messages = append(page.Messages, m)
	}
	if page.HasMore {
		binding.After = strconv.FormatInt(next, 10)
		raw, _ := json.Marshal(binding)
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}
