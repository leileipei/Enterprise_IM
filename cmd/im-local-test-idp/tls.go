package main

import "crypto/tls"

func minTLSConfig() *tls.Config { return &tls.Config{MinVersion: tls.VersionTLS12} }
