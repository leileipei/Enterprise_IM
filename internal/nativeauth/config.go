// Package nativeauth provides a fixed, public-client OAuth token broker.
package nativeauth

import (
	"encoding/base64"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type PlatformConfig struct {
	Platform, ClientID, RedirectURI string
	Loopback, RefreshEnabled        bool
}
type Config struct {
	Issuer, AuthorizationURL, TokenURL, Audience, Scope string
	Platforms                                           map[string]PlatformConfig
}
type PublicConfig struct {
	Platform         string    `json:"platform"`
	Issuer           string    `json:"issuer"`
	AuthorizationURL string    `json:"authorization_url"`
	ClientID         string    `json:"client_id"`
	Scope            string    `json:"scope"`
	RedirectURI      string    `json:"redirect_uri"`
	Loopback         bool      `json:"loopback"`
	RefreshEnabled   bool      `json:"refresh_enabled"`
	ServerTime       time.Time `json:"server_time"`
}

var platforms = []string{"windows", "macos", "android", "ios"}

const loopbackCallback = "http://127.0.0.1/oauth/callback"
const mobileCallback = "com.enterpriseim.client:/oauth/callback"

var verifierPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

func safeText(value string, max int) bool {
	return value != "" && len(value) <= max && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}
func httpsEndpoint(value string) bool {
	u, e := url.Parse(value)
	return e == nil && safeText(value, 2048) && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.String() == value
}
func (c Config) Validate() error {
	if !httpsEndpoint(c.Issuer) || !httpsEndpoint(c.AuthorizationURL) || !httpsEndpoint(c.TokenURL) || !safeText(c.Audience, 2048) || !safeText(c.Scope, 512) || len(c.Platforms) != 4 {
		return errors.New("invalid native OAuth configuration")
	}
	openid := false
	for _, scope := range strings.Fields(c.Scope) {
		openid = openid || scope == "openid"
	}
	if !openid {
		return errors.New("native scope requires openid")
	}
	ids := map[string]bool{}
	for _, name := range platforms {
		p, ok := c.Platforms[name]
		desktop := name == "windows" || name == "macos"
		redirect := mobileCallback
		if desktop {
			redirect = loopbackCallback
		}
		if !ok || p.Platform != name || !safeText(p.ClientID, 256) || ids[p.ClientID] || p.Loopback != desktop || p.RedirectURI != redirect {
			return errors.New("invalid native platform configuration")
		}
		ids[p.ClientID] = true
	}
	return nil
}
func (p PlatformConfig) allowsRedirect(raw string) bool {
	if !p.Loopback {
		return raw == p.RedirectURI
	}
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Path != "/oauth/callback" || u.RawPath != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.String() != raw {
		return false
	}
	port, e := strconv.Atoi(u.Port())
	return e == nil && port > 0 && port <= 65535 && u.Host == "127.0.0.1:"+strconv.Itoa(port)
}
func validNonce(raw string) bool {
	b, e := base64.RawURLEncoding.DecodeString(raw)
	return e == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == raw
}
