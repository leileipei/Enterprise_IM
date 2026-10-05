package policystore

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
)

type crossSearchBinding struct {
	Tenant     string `json:"t"`
	User       string `json:"u"`
	Membership string `json:"m"`
	Query      string `json:"q"`
	Kind       string `json:"k"`
}
type crossSearchPosition struct {
	Conversation, Phase string
	After               int64
}
type crossSearchCursor struct {
	Version int `json:"v"`
	crossSearchBinding
	Conversation string `json:"c"`
	Phase        string `json:"p"`
	After        string `json:"a"`
}

func encodeCrossSearchCursor(binding crossSearchBinding, position crossSearchPosition) string {
	raw, _ := json.Marshal(crossSearchCursor{1, binding, position.Conversation, position.Phase, strconv.FormatInt(position.After, 10)})
	return base64.RawURLEncoding.EncodeToString(raw)
}
func decodeCrossSearchCursor(cursor string, binding crossSearchBinding) (crossSearchPosition, error) {
	if cursor == "" {
		return crossSearchPosition{}, nil
	}
	bad := func() (crossSearchPosition, error) { return crossSearchPosition{}, ErrInvalidMessageSearch }
	if len(cursor) > 2048 {
		return bad()
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return bad()
	}
	var c crossSearchCursor
	if json.Unmarshal(raw, &c) != nil || c.Version != 1 || c.crossSearchBinding != binding || !directoryUUIDPattern.MatchString(c.Conversation) || c.Conversation != strings.ToLower(c.Conversation) {
		return bad()
	}
	n, err := strconv.ParseInt(c.After, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != c.After || (c.Phase != "within" && c.Phase != "after") || (c.Phase == "after" && n != 0) {
		return bad()
	}
	canonical, _ := json.Marshal(c)
	if base64.RawURLEncoding.EncodeToString(canonical) != cursor {
		return bad()
	}
	return crossSearchPosition{c.Conversation, c.Phase, n}, nil
}
