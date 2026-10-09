package main

import (
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/nativeauth"
	"github.com/leileipei/Enterprise_IM/internal/oidcauth"
	"strings"
)

type NativeRuntimeConfig struct {
	Enabled bool
	Config  nativeauth.Config
}

func loadNativeRuntime(getenv func(string) string, oidcEnabled, receiptsEnabled bool, auth oidcauth.Config) (NativeRuntimeConfig, error) {
	switch getenv("IM_NATIVE_ENABLED") {
	case "", "false":
		return NativeRuntimeConfig{}, nil
	case "true":
	default:
		return NativeRuntimeConfig{}, errors.New("IM_NATIVE_ENABLED must be true or false")
	}
	if !oidcEnabled || !receiptsEnabled {
		return NativeRuntimeConfig{}, errors.New("native clients require OIDC and receipts")
	}
	if e := auth.Validate(); e != nil {
		return NativeRuntimeConfig{}, errors.New("invalid native OIDC prerequisite")
	}
	scope := getenv("IM_NATIVE_SCOPE")
	if scope == "" {
		scope = "openid profile"
	}
	config := nativeauth.Config{Issuer: auth.Issuer, Audience: auth.Audience, AuthorizationURL: getenv("IM_NATIVE_AUTHORIZATION_URL"), TokenURL: getenv("IM_NATIVE_TOKEN_URL"), Scope: scope, Platforms: map[string]nativeauth.PlatformConfig{}}
	for _, platform := range []string{"windows", "macos", "android", "ios"} {
		prefix := "IM_NATIVE_" + strings.ToUpper(platform) + "_"
		refresh := false
		switch getenv(prefix + "REFRESH_ENABLED") {
		case "", "false":
		case "true":
			refresh = true
		default:
			return NativeRuntimeConfig{}, errors.New("invalid native refresh flag")
		}
		client := getenv(prefix + "CLIENT_ID")
		allowed := false
		for _, id := range auth.AllowedClientIDs {
			allowed = allowed || id == client
		}
		if !allowed {
			return NativeRuntimeConfig{}, errors.New("native client is not explicitly allowed")
		}
		config.Platforms[platform] = nativeauth.PlatformConfig{Platform: platform, ClientID: client, RedirectURI: getenv(prefix + "REDIRECT_URI"), Loopback: platform == "macos" || platform == "windows", RefreshEnabled: refresh}
	}
	if e := config.Validate(); e != nil {
		return NativeRuntimeConfig{}, e
	}
	return NativeRuntimeConfig{Enabled: true, Config: config}, nil
}
