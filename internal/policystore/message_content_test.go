package policystore

import (
	"crypto/sha256"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"strings"
	"testing"
)

const contentClient = "0199f04a-0000-7000-8000-000000000501"
const contentFile = "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"

func TestMessageContentBounds(t *testing.T) {
	for _, kind := range []string{"text", "file"} {
		for _, tc := range []struct {
			name, body string
			valid      bool
		}{
			{"empty", "", kind == "file"}, {"blank", " \r\n", kind == "file"}, {"limit", strings.Repeat("界", 5461) + "a", true},
			{"over", strings.Repeat("a", 16385), false}, {"nul", "a\x00b", false}, {"utf8", string([]byte{0xff}), false},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				req := MessageSendRequest{ClientMessageID: strings.ToUpper(contentClient), MessageType: kind}
				if kind == "file" {
					req.FileID = contentFile
					req.Caption = tc.body
				} else {
					req.Text = tc.body
				}
				got, err := validateMessageSendRequest(req)
				if (err == nil) != tc.valid {
					t.Fatalf("valid=%v, got %v", tc.valid, err)
				}
				if err == nil && (got.ClientMessageID != contentClient || (kind == "file" && (got.FileID != strings.ToLower(contentFile) || got.Caption != tc.body))) {
					t.Fatal("normalization changed content", got)
				}
			})
		}
	}
	for _, req := range []MessageSendRequest{
		{ClientMessageID: contentClient, MessageType: ""}, {ClientMessageID: contentClient, MessageType: "other"},
		{ClientMessageID: "invalid", MessageType: "text", Text: "a"}, {ClientMessageID: contentClient, MessageType: "file", FileID: "invalid"},
		{ClientMessageID: contentClient, MessageType: "text", Text: "a", FileID: contentFile},
		{ClientMessageID: contentClient, MessageType: "text", Text: "a", Caption: "a"},
		{ClientMessageID: contentClient, MessageType: "file", FileID: contentFile, Text: "a"},
	} {
		if _, e := validateMessageSendRequest(req); e == nil {
			t.Fatal("mixed/invalid request accepted", req)
		}
	}
}
func TestFileMessageDigestCanonical(t *testing.T) {
	id := access.TrustedIdentity{TenantID: "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAA01", UserID: "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAA02", ActingMembershipID: "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAA03"}
	cid := "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAA04"
	req := MessageSendRequest{ClientMessageID: contentClient, MessageType: "file", FileID: contentFile, Caption: "<&\r\ne\u0301"}
	var sealed [32]byte
	for i := range sealed {
		sealed[i] = 0xab
	}
	canonical := `["file-message-v1","file","aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa01","aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa04","aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa02","aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa03","aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","abababababababababababababababababababababababababababababababab","\u003c\u0026\r\né"]`
	want := sha256.Sum256([]byte(canonical))
	got, e := fileMessageDigest(id, cid, req, sealed)
	if e != nil || got != want {
		t.Fatalf("canonical digest got %x want %x: %v", got, want, e)
	}
	id.TenantID = strings.ToLower(id.TenantID)
	id.UserID = strings.ToLower(id.UserID)
	id.ActingMembershipID = strings.ToLower(id.ActingMembershipID)
	req.FileID = strings.ToLower(req.FileID)
	lower, e := fileMessageDigest(id, strings.ToLower(cid), req, sealed)
	if e != nil || lower != got {
		t.Fatal("UUID alias changed logical request", e)
	}
	for _, caption := range []string{"<&\ne\u0301", "<&\r\né", "<&\r\ne\u0301 "} {
		req.Caption = caption
		changed, e := fileMessageDigest(id, cid, req, sealed)
		if e != nil || changed == got {
			t.Fatal("caption not preserved", e)
		}
	}
}
