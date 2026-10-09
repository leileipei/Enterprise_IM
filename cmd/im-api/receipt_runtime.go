package main

import (
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/receiptoutbox"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/leileipei/Enterprise_IM/internal/httpserver"
)

type ReceiptRuntimeConfig struct {
	Enabled                                        bool
	Stream                                         string
	DeviceRegistrationsPerHour, WriteRatePerSecond int
}

func loadReceiptRuntime(getenv func(string) string, oidcEnabled bool) (ReceiptRuntimeConfig, error) {
	cfg := ReceiptRuntimeConfig{}
	switch getenv("IM_RECEIPTS_ENABLED") {
	case "", "false":
		return cfg, nil
	case "true":
		cfg.Enabled = true
	default:
		return cfg, errors.New("invalid receipt switch")
	}
	if !oidcEnabled {
		return ReceiptRuntimeConfig{}, errors.New("receipts require OIDC")
	}
	cfg.Stream = getenv("IM_RECEIPT_STREAM")
	if cfg.Stream == "" {
		cfg.Stream = receiptoutbox.DefaultStream
	}
	if len(cfg.Stream) > 128 || strings.IndexFunc(cfg.Stream, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 || cfg.Stream == "enterprise-im:message-created:v1" || (getenv("IM_REALTIME_STREAM") != "" && cfg.Stream == getenv("IM_REALTIME_STREAM")) {
		return ReceiptRuntimeConfig{}, errors.New("invalid independent receipt stream")
	}
	var e error
	cfg.DeviceRegistrationsPerHour, e = receiptLimit(getenv("IM_DEVICE_REGISTRATIONS_PER_HOUR"), 10, 1000)
	if e != nil {
		return ReceiptRuntimeConfig{}, e
	}
	cfg.WriteRatePerSecond, e = receiptLimit(getenv("IM_RECEIPT_WRITES_PER_SECOND"), 10, 10000)
	if e != nil {
		return ReceiptRuntimeConfig{}, e
	}
	return cfg, nil
}
func receiptLimit(raw string, fallback, max int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	n, e := strconv.Atoi(raw)
	if e != nil || n < 1 || n > max || strconv.Itoa(n) != raw {
		return 0, errors.New("invalid receipt limit")
	}
	return n, nil
}
func assembleDeviceRoutes(base http.Handler, auth httpserver.Authenticator, svc httpserver.DeviceService, cfg ReceiptRuntimeConfig) (http.Handler, error) {
	if !cfg.Enabled {
		return httpserver.HandlerWithClosedReceipts(base), nil
	}
	return httpserver.HandlerWithDevices(base, auth, svc)
}

func assembleReceiptRoutes(base http.Handler, auth httpserver.Authenticator, svc httpserver.ReceiptService, cfg ReceiptRuntimeConfig) (http.Handler, error) {
	if !cfg.Enabled {
		return httpserver.HandlerWithClosedReceipts(base), nil
	}
	return httpserver.HandlerWithReceipts(base, auth, svc)
}
