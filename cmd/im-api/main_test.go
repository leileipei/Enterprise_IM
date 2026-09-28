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
