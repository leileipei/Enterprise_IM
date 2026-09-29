package webclient

import (
	"errors"
	"net/url"
	"strings"
	"unicode"
)

type Config struct {
	Issuer           string
	AuthorizationURL string
	TokenURL         string
	ClientID         string
	RedirectURL      string
	Scope            string
}

func secureEndpoint(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	return u, err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil &&
		u.RawQuery == "" && u.Fragment == "" && u.String() == raw
}

func (c Config) Validate() error {
	for _, raw := range []string{c.Issuer, c.AuthorizationURL, c.TokenURL, c.RedirectURL} {
		if _, valid := secureEndpoint(raw); !valid {
			return errors.New("web OIDC endpoints and redirect must be HTTPS URLs")
		}
	}
	redirect, _ := url.Parse(c.RedirectURL)
	if redirect.Path != "/web/" || redirect.RawPath != "" {
		return errors.New("web redirect must be the public /web/ URL")
	}
	if c.ClientID == "" || len(c.ClientID) > 256 || strings.TrimSpace(c.ClientID) != c.ClientID ||
		strings.IndexFunc(c.ClientID, unicode.IsControl) >= 0 {
		return errors.New("invalid web OIDC client ID")
	}
	if c.Scope == "" || len(c.Scope) > 256 || strings.IndexFunc(c.Scope, unicode.IsControl) >= 0 {
		return errors.New("invalid web OIDC scope")
	}
	scopes := strings.Fields(c.Scope)
	if len(scopes) == 0 || !contains(scopes, "openid") || strings.Join(scopes, " ") != c.Scope {
		return errors.New("web OIDC scope must include openid")
	}
	return nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
