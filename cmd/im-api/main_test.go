package main

import "testing"

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestOIDCAdminConfigRequiresExplicitEnableAndCompleteSettings(t *testing.T) {
	enabled, _, err := adminConfigFromEnv(env(nil))
	if err != nil || enabled {
		t.Fatalf("default must remain health-only: enabled=%v err=%v", enabled, err)
	}
	valid := map[string]string{
		"IM_OIDC_ENABLED":            "true",
		"IM_OIDC_ISSUER":             "https://sso.example.test/group",
		"IM_OIDC_AUDIENCE":           "enterprise-im-api",
		"IM_OIDC_JWKS_URL":           "https://sso.example.test/group/keys",
		"IM_OIDC_ALLOWED_CLIENT_IDS": "web,desktop",
	}
	enabled, cfg, err := adminConfigFromEnv(env(valid))
	if err != nil || !enabled || len(cfg.AllowedClientIDs) != 2 || cfg.AllowedClientIDs[1] != "desktop" {
		t.Fatalf("valid config rejected: enabled=%v cfg=%+v err=%v", enabled, cfg, err)
	}
	for _, tc := range []map[string]string{
		{"IM_OIDC_ENABLED": "true"},
		{"IM_OIDC_ENABLED": "unexpected"},
		{"IM_OIDC_ENABLED": "true", "IM_OIDC_ISSUER": "http://insecure.example.test", "IM_OIDC_AUDIENCE": "api", "IM_OIDC_JWKS_URL": "https://sso.example.test/keys", "IM_OIDC_ALLOWED_CLIENT_IDS": "web"},
	} {
		if _, _, err := adminConfigFromEnv(env(tc)); err == nil {
			t.Fatalf("unsafe configuration accepted: %v", tc)
		}
	}
}

func TestMessageRateConfigRequiresPositiveInteger(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		want  int
		valid bool
	}{
		{"", 10, true}, {"1", 1, true}, {"25", 25, true},
		{"0", 0, false}, {"-1", 0, false}, {"1.5", 0, false}, {"abc", 0, false},
	} {
		got, err := messageRateFromEnv(env(map[string]string{"IM_MESSAGE_RATE_PER_SECOND": tc.raw}))
		if (err == nil) != tc.valid || (tc.valid && got != tc.want) {
			t.Fatalf("rate %q: %d %v", tc.raw, got, err)
		}
	}
}
