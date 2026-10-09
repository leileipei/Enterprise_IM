package nativeauth

import "testing"

func nativeConfigForTest(tokenURL string) Config {
	return Config{Issuer: "https://sso.example.test/group", AuthorizationURL: "https://sso.example.test/group/authorize", TokenURL: tokenURL, Audience: "enterprise-im-api", Scope: "openid profile", Platforms: map[string]PlatformConfig{
		"windows": {Platform: "windows", ClientID: "im-windows", RedirectURI: "http://127.0.0.1/oauth/callback", Loopback: true, RefreshEnabled: true},
		"macos":   {Platform: "macos", ClientID: "im-macos", RedirectURI: "http://127.0.0.1/oauth/callback", Loopback: true, RefreshEnabled: true},
		"android": {Platform: "android", ClientID: "im-android", RedirectURI: "com.enterpriseim.client:/oauth/callback", RefreshEnabled: true},
		"ios":     {Platform: "ios", ClientID: "im-ios", RedirectURI: "com.enterpriseim.client:/oauth/callback", RefreshEnabled: true},
	}}
}

func TestNativeConfigRejectsUnsafePlatformAndEndpoint(t *testing.T) {
	valid := nativeConfigForTest("https://sso.example.test/group/token")
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	cases := []struct {
		name   string
		change func(*Config)
	}{
		{"http_issuer", func(c *Config) { c.Issuer = "http://sso.example.test/group" }},
		{"token_userinfo", func(c *Config) { c.TokenURL = "https://user:secret@sso.example.test/group/token" }},
		{"token_query", func(c *Config) { c.TokenURL += "?endpoint=other" }},
		{"authorization_fragment", func(c *Config) { c.AuthorizationURL += "#fragment" }},
		{"empty_audience", func(c *Config) { c.Audience = "" }},
		{"no_openid", func(c *Config) { c.Scope = "profile" }},
		{"missing_platform", func(c *Config) { delete(c.Platforms, "ios") }},
		{"extra_platform", func(c *Config) { c.Platforms["linux"] = c.Platforms["macos"] }},
		{"duplicate_client", func(c *Config) {
			p := c.Platforms["ios"]
			p.ClientID = c.Platforms["android"].ClientID
			c.Platforms["ios"] = p
		}},
		{"mismatched_platform", func(c *Config) { p := c.Platforms["macos"]; p.Platform = "windows"; c.Platforms["macos"] = p }},
		{"desktop_external_redirect", func(c *Config) {
			p := c.Platforms["macos"]
			p.RedirectURI = "http://example.test/oauth/callback"
			c.Platforms["macos"] = p
		}},
		{"desktop_fixed_port", func(c *Config) {
			p := c.Platforms["macos"]
			p.RedirectURI = "http://127.0.0.1:8080/oauth/callback"
			c.Platforms["macos"] = p
		}},
		{"mobile_loopback", func(c *Config) { p := c.Platforms["ios"]; p.Loopback = true; c.Platforms["ios"] = p }},
		{"mobile_other_scheme", func(c *Config) {
			p := c.Platforms["ios"]
			p.RedirectURI = "arbitrary:/oauth/callback"
			c.Platforms["ios"] = p
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := nativeConfigForTest(valid.TokenURL)
			tc.change(&c)
			if c.Validate() == nil {
				t.Fatal("unsafe native configuration accepted")
			}
		})
	}
}
