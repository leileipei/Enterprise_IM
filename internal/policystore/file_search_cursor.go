package policystore

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
)

type fileSearchBinding struct {
	Tenant       string `json:"t"`
	User         string `json:"u"`
	Membership   string `json:"m"`
	Conversation string `json:"c"`
	Kind         string `json:"k"`
	Query        string `json:"q"`
}
type fileSearchPosition struct {
	Conversation, Phase string
	After               int64
}
type fileSearchCursor struct {
	Version      string            `json:"v"`
	Binding      fileSearchBinding `json:"b"`
	Conversation string            `json:"c"`
	Phase        string            `json:"p"`
	After        string            `json:"a"`
}

func validFileSearchPosition(b fileSearchBinding, p fileSearchPosition) bool {
	if !directoryUUIDPattern.MatchString(p.Conversation) || p.Conversation != strings.ToLower(p.Conversation) || p.After < 0 {
		return false
	}
	if b.Conversation != "" && (p.Conversation != b.Conversation || p.Phase != "within" || p.After < 1) {
		return false
	}
	return (p.Phase == "within" && p.After > 0) || (p.Phase == "after" && p.After == 0)
}
func encodeFileSearchCursor(b fileSearchBinding, p fileSearchPosition) (string, error) {
	if !validFileSearchPosition(b, p) {
		return "", ErrInvalidFileSearch
	}
	raw, e := json.Marshal(fileSearchCursor{"file_name_v1", b, p.Conversation, p.Phase, strconv.FormatInt(p.After, 10)})
	if e != nil {
		return "", ErrInvalidFileSearch
	}
	out := base64.RawURLEncoding.EncodeToString(raw)
	if len(out) > 2048 {
		return "", ErrInvalidFileSearch
	}
	return out, nil
}
func decodeFileSearchCursor(raw string, b fileSearchBinding) (fileSearchPosition, error) {
	if raw == "" {
		return fileSearchPosition{}, nil
	}
	bad := func() (fileSearchPosition, error) { return fileSearchPosition{}, ErrInvalidFileSearch }
	if len(raw) > 2048 {
		return bad()
	}
	bytes, e := base64.RawURLEncoding.DecodeString(raw)
	if e != nil {
		return bad()
	}
	var c fileSearchCursor
	if json.Unmarshal(bytes, &c) != nil || c.Version != "file_name_v1" || c.Binding != b {
		return bad()
	}
	n, e := strconv.ParseInt(c.After, 10, 64)
	p := fileSearchPosition{c.Conversation, c.Phase, n}
	if e != nil || strconv.FormatInt(n, 10) != c.After || !validFileSearchPosition(b, p) {
		return bad()
	}
	canonical, e := encodeFileSearchCursor(b, p)
	if e != nil || canonical != raw {
		return bad()
	}
	return p, nil
}
