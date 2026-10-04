package policystore

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestFileSearchCursorBinding(t *testing.T) {
	b := fileSearchBinding{"10000000-0000-0000-0000-000000000001", "10000000-0000-0000-0000-000000000002", "10000000-0000-0000-0000-000000000003", "10000000-0000-0000-0000-000000000004", "direct", "abc"}
	p := fileSearchPosition{b.Conversation, "within", 9007199254740993}
	raw, e := encodeFileSearchCursor(b, p)
	if e != nil {
		t.Fatal(e)
	}
	got, e := decodeFileSearchCursor(raw, b)
	if e != nil || got != p {
		t.Fatal(got, e)
	}
	for _, change := range []func(*fileSearchBinding){func(b *fileSearchBinding) { b.User = "10000000-0000-0000-0000-000000000099" }, func(b *fileSearchBinding) { b.Membership = b.User }, func(b *fileSearchBinding) { b.Tenant = b.User }, func(b *fileSearchBinding) { b.Query = "different" }, func(b *fileSearchBinding) { b.Kind = "group" }, func(b *fileSearchBinding) { b.Conversation = b.User }} {
		changed := b
		change(&changed)
		if _, e := decodeFileSearchCursor(raw, changed); !errors.Is(e, ErrInvalidFileSearch) {
			t.Fatal("changed binding accepted", e)
		}
	}
	text, _ := json.Marshal(messageSearchCursor{Tenant: b.Tenant, User: b.User, Membership: b.Membership, Conversation: b.Conversation, Kind: b.Kind, Query: b.Query, After: "1"})
	for _, bad := range []string{base64.RawURLEncoding.EncodeToString(text), raw + "=", strings.Repeat("a", 2049), base64.RawURLEncoding.EncodeToString([]byte(`{"v":"file_name_v1"}`))} {
		if _, e := decodeFileSearchCursor(bad, b); !errors.Is(e, ErrInvalidFileSearch) {
			t.Fatal("bad cursor", e)
		}
	}
}
