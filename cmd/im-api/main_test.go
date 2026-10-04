package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"github.com/leileipei/Enterprise_IM/internal/oidcauth"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
)

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestWebConfigRequiresOIDCAndAllowedPublicClient(t *testing.T) {
	base := oidcauth.Config{Issuer: "https://sso.example.test/group", Audience: "enterprise-im-api",
		JWKSURL: "https://sso.example.test/keys", AllowedClientIDs: []string{"enterprise-im-web"}}
	if enabled, _, err := webConfigFromEnv(env(nil), true, base); err != nil || enabled {
		t.Fatalf("web should default off: %v %v", enabled, err)
	}
	settings := map[string]string{"IM_WEB_ENABLED": "true", "IM_WEB_AUTHORIZATION_URL": "https://sso.example.test/authorize",
		"IM_WEB_TOKEN_URL": "https://sso.example.test/token", "IM_WEB_CLIENT_ID": "enterprise-im-web",
		"IM_WEB_REDIRECT_URL": "https://im.example.test/web/"}
	if _, _, err := webConfigFromEnv(env(settings), false, base); err == nil {
		t.Fatal("web without OIDC accepted")
	}
	enabled, cfg, err := webConfigFromEnv(env(settings), true, base)
	if err != nil || !enabled || cfg.ClientID != "enterprise-im-web" || cfg.Scope != "openid profile" || cfg.Issuer != base.Issuer {
		t.Fatalf("valid web config rejected: %v %+v %v", enabled, cfg, err)
	}
	settings["IM_WEB_CLIENT_ID"] = "unknown-client"
	if _, _, err := webConfigFromEnv(env(settings), true, base); err == nil {
		t.Fatal("client outside access-token allowlist accepted")
	}
	settings["IM_WEB_CLIENT_ID"] = "enterprise-im-web"
	settings["IM_WEB_REDIRECT_URL"] = "http://im.example.test/web/"
	if _, _, err := webConfigFromEnv(env(settings), true, base); err == nil {
		t.Fatal("insecure web callback accepted")
	}
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

func TestRealtimeRedisRequiresOIDCAndValidURL(t *testing.T) {
	if options, err := realtimeRedisOptionsFromEnv(env(nil), false); err != nil || options != nil {
		t.Fatalf("realtime default should be off: %v %v", options, err)
	}
	for _, tc := range []struct {
		url     string
		enabled bool
	}{
		{"redis://127.0.0.1:16379/0", false},
		{"not-a-url", true},
		{"http://127.0.0.1:16379", true},
		{"redis://", true},
	} {
		if _, err := realtimeRedisOptionsFromEnv(env(map[string]string{"IM_REALTIME_REDIS_URL": tc.url}), tc.enabled); err == nil {
			t.Fatalf("invalid realtime config accepted: %+v", tc)
		}
	}
	options, err := realtimeRedisOptionsFromEnv(env(map[string]string{"IM_REALTIME_REDIS_URL": "redis://127.0.0.1:16379/0"}), true)
	if err != nil || options == nil || options.Addr != "127.0.0.1:16379" || !options.ContextTimeoutEnabled {
		t.Fatalf("valid realtime Redis config: %+v %v", options, err)
	}
}

func TestRealtimeStreamConfiguration(t *testing.T) {
	stream, err := realtimeStreamFromEnv(env(nil), true)
	if err != nil || stream != outbox.DefaultStream {
		t.Fatalf("default stream: %q %v", stream, err)
	}
	stream, err = realtimeStreamFromEnv(env(map[string]string{"IM_REALTIME_STREAM": "enterprise-im:staging:message-created:v1"}), true)
	if err != nil || stream != "enterprise-im:staging:message-created:v1" {
		t.Fatalf("custom stream: %q %v", stream, err)
	}
	for _, tc := range []struct {
		stream  string
		enabled bool
	}{
		{" bad ", true}, {"bad\nname", true}, {strings.Repeat("x", 129), true},
		{"enterprise-im:staging:message-created:v1", false},
	} {
		if _, err := realtimeStreamFromEnv(env(map[string]string{"IM_REALTIME_STREAM": tc.stream}), tc.enabled); err == nil {
			t.Fatalf("invalid stream accepted: %+v", tc)
		}
	}
}

func TestFileAPIAssemblyDisabled(t *testing.T) {
	enabled, _, dir, e := fileUploadConfigFromEnv(env(nil), false)
	if e != nil || enabled || dir != "" {
		t.Fatal(enabled, dir, e)
	}
	if _, _, _, e = fileUploadConfigFromEnv(env(map[string]string{"IM_FILE_UPLOAD_ENABLED": "yes"}), true); e == nil {
		t.Fatal("invalid bool accepted")
	}
}
func TestFileAPIAssemblyEnabledPreflight(t *testing.T) {
	settings := map[string]string{"IM_FILE_UPLOAD_ENABLED": "true", "IM_DATABASE_URL": "postgres://localhost/test", "IM_FILE_S3_ENDPOINT": "http://127.0.0.1:9000", "IM_FILE_S3_REGION": "us-east-1", "IM_FILE_S3_BUCKET": "private-files", "IM_FILE_S3_CREDENTIAL_SOURCE": "environment", "IM_FILE_S3_ACCESS_KEY": "fixture", "IM_FILE_S3_SECRET_KEY": "fixture", "IM_FILE_S3_PATH_STYLE": "true", "IM_FILE_SPOOL_DIR": t.TempDir() + "/private"}
	if _, _, _, e := fileUploadConfigFromEnv(env(settings), false); e == nil {
		t.Fatal("enabled without oidc")
	}
	enabled, c, dir, e := fileUploadConfigFromEnv(env(settings), true)
	if e != nil || !enabled || !c.PathStyle || dir != settings["IM_FILE_SPOOL_DIR"] {
		t.Fatal(enabled, c, dir, e)
	}
	for _, k := range []string{"IM_DATABASE_URL", "IM_FILE_S3_ENDPOINT", "IM_FILE_S3_BUCKET", "IM_FILE_S3_ACCESS_KEY", "IM_FILE_S3_SECRET_KEY", "IM_FILE_SPOOL_DIR"} {
		v := settings[k]
		delete(settings, k)
		if _, _, _, e := fileUploadConfigFromEnv(env(settings), true); e == nil {
			t.Fatal("missing config", k)
		}
		settings[k] = v
	}
	settings["IM_FILE_S3_PATH_STYLE"] = "invalid"
	if _, _, _, e := fileUploadConfigFromEnv(env(settings), true); e == nil {
		t.Fatal("invalid path style")
	}
}

func TestFileMessageProductionClosedConfiguration(t *testing.T) {
	// The unknown flag must not activate even the upload dependency path.
	enabled, _, _, err := fileUploadConfigFromEnv(env(map[string]string{"IM_FILE_MESSAGE_ENABLED": "true"}), true)
	if err != nil || enabled {
		t.Fatal("unknown message flag activated upload", enabled, err)
	}
}

func TestFileDownloadProductionAssemblyClosed(t *testing.T) {
	for _, upload := range []bool{false, true} {
		h := productionFileDownloadHandler(http.NotFoundHandler(), upload)
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/v1/files/00000000-0000-4000-8000-000000000001/content", nil)
		h.ServeHTTP(w, req)
		if w.Code != 503 || !strings.Contains(w.Body.String(), "file_download_unavailable") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestProductionFileCapabilitiesClosed(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		got := productionFileCapabilities(enabled)
		if got != (httpserver.FileCapabilities{UploadEnabled: enabled}) {
			t.Fatal("production file sharing capability opened", got)
		}
	}
}
