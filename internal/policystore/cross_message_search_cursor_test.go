package policystore

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestCrossSearchCursorStrictBinding(t *testing.T) {
	b := crossSearchBinding{Tenant: "00000000-0000-4000-8000-000000000001", User: "00000000-0000-4000-8000-000000000002", Membership: "00000000-0000-4000-8000-000000000003", Query: "工单", Kind: "all"}
	p := crossSearchPosition{Conversation: "abcdefab-cdef-4abc-8abc-abcdefabcdef", Phase: "within", After: 9007199254740993}
	c := encodeCrossSearchCursor(b, p)
	got, err := decodeCrossSearchCursor(c, b)
	if err != nil || got != p {
		t.Fatalf("roundtrip %+v %v", got, err)
	}
	changes := []crossSearchBinding{b, b, b, b, b}
	changes[0].Tenant = b.User
	changes[1].User = b.Tenant
	changes[2].Membership = b.User
	changes[3].Query = "其他"
	changes[4].Kind = "direct"
	for _, other := range changes {
		if _, err := decodeCrossSearchCursor(c, other); !errors.Is(err, ErrInvalidMessageSearch) {
			t.Fatalf("binding accepted %+v %v", other, err)
		}
	}
	raw, _ := base64.RawURLEncoding.DecodeString(c)
	invalid := []string{"bad", strings.Repeat("a", 2049), "e30"}
	for _, r := range []string{strings.Replace(string(raw), `"v":1`, `"v":2`, 1), strings.Replace(string(raw), `"a":"9007199254740993"`, `"a":"-1"`, 1), strings.Replace(string(raw), `"a":"9007199254740993"`, `"a":"01"`, 1), strings.Replace(string(raw), `"a":"9007199254740993"`, `"a":"9223372036854775808"`, 1), strings.Replace(string(raw), `"p":"within"`, `"p":"after"`, 1), strings.Replace(string(raw), `"p":"within"`, `"p":"wrong"`, 1), strings.Replace(string(raw), p.Conversation, strings.ToUpper(p.Conversation), 1), strings.Replace(string(raw), `"v":1`, `"v":1,"v":1`, 1), strings.Replace(string(raw), `"v":1`, `"v":1,"extra":1`, 1), strings.Replace(string(raw), `"v":1`, `"v":"1"`, 1)} {
		invalid = append(invalid, base64.RawURLEncoding.EncodeToString([]byte(r)))
	}
	for _, bad := range invalid {
		if _, err := decodeCrossSearchCursor(bad, b); !errors.Is(err, ErrInvalidMessageSearch) {
			t.Fatalf("invalid cursor accepted %q %v", bad, err)
		}
	}
	p.Phase = "after"
	p.After = 0
	if got, err := decodeCrossSearchCursor(encodeCrossSearchCursor(b, p), b); err != nil || got != p {
		t.Fatalf("after %+v %v", got, err)
	}
}
