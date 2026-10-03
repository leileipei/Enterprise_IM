// Package files defines file metadata invariants. Validation does not authenticate
// identities, authorize resources, measure content, or perform malware scanning.
package files

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"mime"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxFileSizeBytes int64 = 26214400

var ErrInvalidMetadata = errors.New("invalid file metadata")
var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var mediaTypePattern = regexp.MustCompile("^[a-z0-9!#$%&'*+.^_`|~-]+/[a-z0-9!#$%&'*+.^_`|~-]+$")

type CreateParams struct {
	TenantID             string
	ConversationID       string
	UploaderUserID       string
	UploaderMembershipID string
	UploadRequestID      string
	OriginalFilename     string
	DeclaredMediaType    string
	DeclaredSizeBytes    int64
}

func validFilename(s string) bool {
	return utf8.ValidString(s) && len(s) >= 1 && len(s) <= 255 && s != "." && s != ".." &&
		strings.TrimSpace(s) == s && strings.IndexFunc(s, unicode.IsControl) < 0 && !strings.ContainsAny(s, "/\\:")
}
func validMediaType(s string) bool {
	if len(s) < 1 || len(s) > 127 || !mediaTypePattern.MatchString(s) {
		return false
	}
	typ, params, err := mime.ParseMediaType(s)
	return err == nil && typ == s && len(params) == 0
}
func canonicalUUID(s string) bool { return uuidPattern.MatchString(s) && s == strings.ToLower(s) }

func NormalizeCreate(p CreateParams) (CreateParams, error) {
	ids := []*string{&p.TenantID, &p.ConversationID, &p.UploaderUserID, &p.UploaderMembershipID, &p.UploadRequestID}
	for _, id := range ids {
		if !uuidPattern.MatchString(*id) {
			return CreateParams{}, ErrInvalidMetadata
		}
		*id = strings.ToLower(*id)
	}
	if !validFilename(p.OriginalFilename) || !validMediaType(p.DeclaredMediaType) || p.DeclaredSizeBytes < 1 || p.DeclaredSizeBytes > MaxFileSizeBytes {
		return CreateParams{}, ErrInvalidMetadata
	}
	return p, nil
}

// CreationDigest binds canonical creation parameters, not the idempotency key.
func CreationDigest(p CreateParams) ([32]byte, error) {
	p, err := NormalizeCreate(p)
	if err != nil {
		return [32]byte{}, err
	}
	raw, err := json.Marshal([]string{"v1", p.TenantID, p.ConversationID, p.UploaderUserID, p.UploaderMembershipID, p.OriginalFilename, p.DeclaredMediaType, strconv.FormatInt(p.DeclaredSizeBytes, 10)})
	if err != nil {
		return [32]byte{}, ErrInvalidMetadata
	}
	return sha256.Sum256(raw), nil
}
