package main

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/leileipei/Enterprise_IM/internal/httpserver"
)

type ReceiptRuntimeConfig struct {
	Enabled                                        bool
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
