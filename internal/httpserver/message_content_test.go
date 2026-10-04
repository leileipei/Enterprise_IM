package httpserver

import (
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"strings"
	"testing"
)

const typedClient = "0199f04a-0000-7000-8000-000000000471"
const typedFile = "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"

func TestFileMessageHTTPParsing(t *testing.T) {
	prefix := `{"client_msg_id":"` + typedClient + `",`
	file := prefix + `"message_type":"file","file_id":"` + typedFile + `"`
	for _, tc := range []struct {
		name, body, kind, caption string
		valid                     bool
	}{
		{"legacy", prefix + `"text":"text"}`, "text", "", true},
		{"typed text", prefix + `"message_type":"text","text":"text"}`, "text", "", true},
		{"omitted caption", file + `}`, "file", "", true}, {"empty caption", file + `,"caption":""}`, "file", "", true},
		{"blank caption", file + `,"caption":" \r\n"}`, "file", " \r\n", true},
		{"surrogate pair", file + `,"caption":"\ud83d\ude00"}`, "file", "😀", true},
		{"literal escape", file + `,"caption":"\\uD800"}`, "file", `\uD800`, true},
		{"limit", file + `,"caption":"` + strings.Repeat("a", 16384) + `"}`, "file", strings.Repeat("a", 16384), true},
		{"over", file + `,"caption":"` + strings.Repeat("a", 16385) + `"}`, "", "", false},
		{"null caption", file + `,"caption":null}`, "", "", false}, {"number caption", file + `,"caption":1}`, "", "", false},
		{"high surrogate", file + `,"caption":"\uD800"}`, "", "", false}, {"low surrogate", file + `,"caption":"\uDC00"}`, "", "", false},
		{"broken pair", file + `,"caption":"\uD800\u0041"}`, "", "", false},
		{"duplicate caption", file + `,"caption":"a","caption":"b"}`, "", "", false},
		{"duplicate type", file + `,"message_type":"file"}`, "", "", false},
		{"duplicate type switches file to text", prefix + `"message_type":"file","message_type":"text","text":"x"}`, "", "", false},
		{"duplicate ID", file + `,"file_id":"` + typedFile + `"}`, "", "", false},
		{"duplicate client", file + `,"client_msg_id":"` + typedClient + `"}`, "", "", false},
		{"unknown", file + `,"foo":"bar"}`, "", "", false}, {"extra JSON", file + `} {}`, "", "", false},
		{"null type", prefix + `"message_type":null,"text":"x"}`, "", "", false},
		{"empty type", prefix + `"message_type":"","text":"x"}`, "", "", false},
		{"unknown type", prefix + `"message_type":"image","file_id":"` + typedFile + `"}`, "", "", false},
		{"missing file", prefix + `"message_type":"file"}`, "", "", false},
		{"null file", prefix + `"message_type":"file","file_id":null}`, "", "", false},
		{"file text empty", file + `,"text":""}`, "", "", false},
		{"text caption empty", prefix + `"text":"x","caption":""}`, "", "", false},
		{"text file null", prefix + `"text":"x","file_id":null}`, "", "", false},
		{"null text", prefix + `"text":null}`, "", "", false},
		{"nul", file + `,"caption":"\u0000"}`, "", "", false},
		{"invalid utf8", file + `,"caption":"` + string([]byte{0xff}) + `"}`, "", "", false},
		{"array", "[]", "", "", false}, {"null", "null", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, e := decodeMessageSendRequest([]byte(tc.body))
			if (e == nil) != tc.valid {
				t.Fatal(tc.name, req, e)
			}
			if e == nil && (req.MessageType != tc.kind || req.Caption != tc.caption || req.ClientMessageID != typedClient || (req.MessageType == policystore.MessageTypeFile && req.FileID != strings.ToLower(typedFile))) {
				t.Fatal(req)
			}
		})
	}
}
