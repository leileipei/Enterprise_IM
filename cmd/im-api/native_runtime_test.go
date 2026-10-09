package main

import (
	"github.com/leileipei/Enterprise_IM/internal/oidcauth"
	"testing"
)

func nativeRuntimeTestEnvironment() map[string]string {
	return map[string]string{"IM_NATIVE_ENABLED": "true", "IM_NATIVE_AUTHORIZATION_URL": "https://sso.example.test/group/authorize", "IM_NATIVE_TOKEN_URL": "https://sso.example.test/group/token", "IM_NATIVE_SCOPE": "openid profile", "IM_NATIVE_WINDOWS_CLIENT_ID": "im-windows", "IM_NATIVE_WINDOWS_REDIRECT_URI": "http://127.0.0.1/oauth/callback", "IM_NATIVE_MACOS_CLIENT_ID": "im-macos", "IM_NATIVE_MACOS_REDIRECT_URI": "http://127.0.0.1/oauth/callback", "IM_NATIVE_ANDROID_CLIENT_ID": "im-android", "IM_NATIVE_ANDROID_REDIRECT_URI": "com.enterpriseim.client:/oauth/callback", "IM_NATIVE_IOS_CLIENT_ID": "im-ios", "IM_NATIVE_IOS_REDIRECT_URI": "com.enterpriseim.client:/oauth/callback"}
}
func nativeRuntimeTestAuth() oidcauth.Config {
	return oidcauth.Config{Issuer: "https://sso.example.test/group", Audience: "enterprise-im-api", JWKSURL: "https://sso.example.test/group/keys", AllowedClientIDs: []string{"enterprise-im-web", "im-windows", "im-macos", "im-android", "im-ios"}}
}
func TestNativeRuntimeDisabledOrMissingPrerequisites(t *testing.T) {
	cfg, err := loadNativeRuntime(func(string) string { return "" }, false, false, oidcauth.Config{})
	if err != nil || cfg.Enabled {
		t.Fatal("native must default closed")
	}
	good := nativeRuntimeTestEnvironment()
	getter := func(k string) string { return good[k] }
	cfg, err = loadNativeRuntime(getter, true, true, nativeRuntimeTestAuth())
	if err != nil || !cfg.Enabled || len(cfg.Config.Platforms) != 4 {
		t.Fatal("explicit four-platform configuration rejected")
	}
	for _, tc := range []struct{ oidc, receipts bool }{{false, true}, {true, false}, {false, false}} {
		if _, err := loadNativeRuntime(getter, tc.oidc, tc.receipts, nativeRuntimeTestAuth()); err == nil {
			t.Fatal("native enabled without prerequisites")
		}
	}
	for _, key := range []string{"IM_NATIVE_TOKEN_URL", "IM_NATIVE_MACOS_CLIENT_ID", "IM_NATIVE_IOS_REDIRECT_URI"} {
		copy := nativeRuntimeTestEnvironment()
		delete(copy, key)
		if _, err := loadNativeRuntime(func(k string) string { return copy[k] }, true, true, nativeRuntimeTestAuth()); err == nil {
			t.Fatal("missing configuration accepted", key)
		}
	}
	bad := nativeRuntimeTestEnvironment()
	bad["IM_NATIVE_ENABLED"] = "yes"
	if _, err := loadNativeRuntime(func(k string) string { return bad[k] }, true, true, nativeRuntimeTestAuth()); err == nil {
		t.Fatal("invalid enable flag accepted")
	}
	bad = nativeRuntimeTestEnvironment()
	bad["IM_NATIVE_IOS_REFRESH_ENABLED"] = "auto"
	if _, err := loadNativeRuntime(func(k string) string { return bad[k] }, true, true, nativeRuntimeTestAuth()); err == nil {
		t.Fatal("invalid refresh flag accepted")
	}
	auth := nativeRuntimeTestAuth()
	auth.AllowedClientIDs = auth.AllowedClientIDs[:4]
	if _, err := loadNativeRuntime(getter, true, true, auth); err == nil {
		t.Fatal("native implicitly enlarged allowed client list")
	}
}
