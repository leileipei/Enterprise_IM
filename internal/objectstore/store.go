// Package objectstore provides private fixed-version object operations without SDK types.
package objectstore

import (
	"context"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Location struct{ TenantID, FileID string }
type VersionRef struct {
	Location  Location
	VersionID string
}
type Config struct {
	Endpoint, Region, Bucket, CredentialSource string
	PathStyle                                  bool
}
type Store interface {
	ValidateCapabilities(context.Context) error
	PutVersion(context.Context, Location, string, files.Measurement, io.ReadSeeker) (VersionRef, error)
	ReadVersion(context.Context, VersionRef) (io.ReadCloser, error)
	FindAttemptVersions(context.Context, Location, string, int) ([]VersionRef, error)
}

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (l Location) key() (string, error) {
	if !uuid.MatchString(l.TenantID) || !uuid.MatchString(l.FileID) {
		return "", files.ErrInvalidMetadata
	}
	return "tenants/" + l.TenantID + "/files/" + l.FileID, nil
}
func versionValid(s string) bool {
	return s != "" && s != "null" && len(s) <= 1024 && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}
