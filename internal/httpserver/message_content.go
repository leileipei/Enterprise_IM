package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"io"
	"net/http"
	"strconv"
	"unicode/utf8"
)

type FileMessageService interface {
	SendMessage(context.Context, access.TrustedIdentity, string, policystore.MessageSendRequest) (policystore.MessageACK, error)
	SendGroupMessage(context.Context, access.TrustedIdentity, string, policystore.MessageSendRequest) (policystore.MessageACK, error)
}

// This explicit constructor is used by controlled integration assemblies.
// The production constructor never discovers the dependency by type assertion.
func HandlerWithFileMessages(base http.Handler, auth Authenticator, conversations ConversationService, files FileMessageService) (http.Handler, error) {
	if files == nil {
		return nil, errors.New("file message service is required")
	}
	return handlerWithMessageServices(base, auth, conversations, files)
}
func decodeMessageSendRequest(raw []byte) (policystore.MessageSendRequest, error) {
	invalid := policystore.ErrInvalidMessageRequest
	if !utf8.Valid(raw) {
		return policystore.MessageSendRequest{}, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return policystore.MessageSendRequest{}, invalid
	}
	fields := make(map[string]json.RawMessage)
	duplicates := false
	sawFileType := false
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return policystore.MessageSendRequest{}, invalid
		}
		key, ok := token.(string)
		if !ok {
			return policystore.MessageSendRequest{}, invalid
		}
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return policystore.MessageSendRequest{}, invalid
		}
		if _, exists := fields[key]; exists {
			duplicates = true
		}
		if key == "message_type" {
			var kind string
			if json.Unmarshal(value, &kind) == nil && kind == policystore.MessageTypeFile {
				sawFileType = true
			}
		}
		fields[key] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return policystore.MessageSendRequest{}, invalid
	}
	var extra any
	if err = decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return policystore.MessageSendRequest{}, invalid
	}
	if _, typed := fields["message_type"]; !typed {
		// Retain the old text decoder's field matching, duplicate and escaping behavior.
		var legacy struct {
			ClientMessageID string `json:"client_msg_id"`
			Text            string `json:"text"`
		}
		decoder = json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&legacy); err != nil {
			return policystore.MessageSendRequest{}, invalid
		}
		return policystore.ValidateMessageSendRequest(policystore.MessageSendRequest{ClientMessageID: legacy.ClientMessageID, MessageType: policystore.MessageTypeText, Text: legacy.Text})
	}
	if sawFileType && duplicates {
		return policystore.MessageSendRequest{}, invalid
	}
	readString := func(key string, optional bool) (string, error) {
		v, ok := fields[key]
		if !ok && optional {
			return "", nil
		}
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return "", invalid
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return "", invalid
		}
		return s, nil
	}
	req := policystore.MessageSendRequest{}
	req.ClientMessageID, err = readString("client_msg_id", false)
	if err != nil {
		return req, invalid
	}
	req.MessageType, err = readString("message_type", false)
	if err != nil {
		return req, invalid
	}
	allowed := map[string]bool{"client_msg_id": true, "message_type": true}
	switch req.MessageType {
	case policystore.MessageTypeText:
		allowed["text"] = true
		req.Text, err = readString("text", false)
	case policystore.MessageTypeFile:
		if duplicates || !validJSONSurrogates(raw) {
			return req, invalid
		}
		allowed["file_id"] = true
		allowed["caption"] = true
		req.FileID, err = readString("file_id", false)
		if err != nil {
			return req, invalid
		}
		req.Caption, err = readString("caption", true)
	default:
		return req, invalid
	}
	if err != nil {
		return req, invalid
	}
	for key := range fields {
		if !allowed[key] {
			return req, invalid
		}
	}
	return policystore.ValidateMessageSendRequest(req)
}

// encoding/json replaces lone surrogate escapes. Reject them for typed files
// before decoding instead of silently changing caption or field identity.
func validJSONSurrogates(raw []byte) bool {
	inString := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		v, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if v >= 0xdc00 && v <= 0xdfff {
			return false
		}
		if v >= 0xd800 && v <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return !inString
}
