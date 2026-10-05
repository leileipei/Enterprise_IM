package main

import (
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/leileipei/Enterprise_IM/internal/objectstore"
)

type fileBusinessConfig struct {
	Enabled                           bool
	Objects                           objectstore.Config
	SpoolDir, OwnerID, ProbeVersionID string
}

var fileBusinessOwnerPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func fileBusinessConfigFromEnv(getenv func(string) string, oidcEnabled bool) (fileBusinessConfig, error) {
	var zero fileBusinessConfig
	invalid := errors.New("invalid_file_business_configuration")
	switch getenv("IM_FILE_BUSINESS_ENABLED") {
	case "", "false":
		return zero, nil
	case "true":
	default:
		return zero, invalid
	}
	c := fileBusinessConfig{Enabled: true, Objects: objectstore.Config{
		Endpoint: getenv("IM_FILE_S3_ENDPOINT"), Region: getenv("IM_FILE_S3_REGION"), Bucket: getenv("IM_FILE_S3_BUCKET"), CredentialSource: "download_environment",
	}, SpoolDir: getenv("IM_FILE_DOWNLOAD_SPOOL_DIR"), OwnerID: getenv("IM_FILE_DOWNLOAD_OWNER_ID"), ProbeVersionID: getenv("IM_FILE_READ_PROBE_VERSION_ID")}
	switch getenv("IM_FILE_S3_PATH_STYLE") {
	case "", "false":
	case "true":
		c.Objects.PathStyle = true
	default:
		return zero, invalid
	}
	u, e := url.Parse(c.Objects.Endpoint)
	if e != nil || u.User != nil || u.Host == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && (u.Scheme != "http" || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1"))) {
		return zero, invalid
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`).MatchString(c.Objects.Bucket) || !regexp.MustCompile(`^[a-z0-9-]{1,64}$`).MatchString(c.Objects.Region) {
		return zero, invalid
	}
	key := getenv("IM_FILE_DOWNLOAD_S3_ACCESS_KEY")
	if !oidcEnabled || getenv("IM_DATABASE_URL") == "" || key == "" || getenv("IM_FILE_DOWNLOAD_S3_SECRET_KEY") == "" || !filepath.IsAbs(c.SpoolDir) || !fileBusinessOwnerPattern.MatchString(c.OwnerID) || c.OwnerID == "00000000-0000-0000-0000-000000000000" || c.ProbeVersionID == "" || c.ProbeVersionID == "null" || len(c.ProbeVersionID) > 1024 || !utf8.ValidString(c.ProbeVersionID) || strings.IndexFunc(c.ProbeVersionID, unicode.IsControl) >= 0 {
		return zero, invalid
	}
	if key == getenv("IM_FILE_S3_ACCESS_KEY") || key == getenv("IM_FILE_CLEANUP_S3_ACCESS_KEY") {
		return zero, invalid
	}
	return c, nil
}
