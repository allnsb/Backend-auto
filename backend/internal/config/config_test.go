package config

import (
	"strings"
	"testing"
)

// Configuration errors must fail startup instead of silently disabling the
// budget; the pool-specific upper bound is checked by the storage constructor.
func TestWriteTransactionConfiguration(t *testing.T) {
	for key, value := range map[string]string{
		"ODYSSEIA_HTTPS_ADDR":                 "127.0.0.1:8443",
		"ODYSSEIA_TLS_CERT_FILE":              "fixture.crt",
		"ODYSSEIA_TLS_KEY_FILE":               "fixture.key",
		"ODYSSEIA_DATABASE_URL":               "postgresql://fixture.invalid/db",
		"ODYSSEIA_AUTH_ISSUER":                "https://issuer.invalid",
		"ODYSSEIA_AUTH_AUDIENCE":              "forum",
		"ODYSSEIA_AUTH_JWKS_URL":              "https://issuer.invalid/keys",
		"ODYSSEIA_REALTIME_CONNECTION_SECRET": "connection-test-secret",
		"ODYSSEIA_REALTIME_PROXY_SECRET":      "proxy-test-secret",
	} {
		t.Setenv(key, value)
	}
	for _, tc := range []struct {
		name, raw string
		want      int64
		invalid   bool
	}{
		{name: "unset"},
		{name: "automatic", raw: "0"},
		{name: "explicit", raw: "10", want: 10},
		{name: "negative", raw: "-1", invalid: true},
		{name: "fraction", raw: "1.5", invalid: true},
		{name: "text", raw: "secret-not-an-integer", invalid: true},
		{name: "overflow", raw: "9223372036854775808", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ODYSSEIA_MAX_WRITE_TRANSACTIONS", tc.raw)
			cfg, err := Load()
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "ODYSSEIA_MAX_WRITE_TRANSACTIONS") || strings.Contains(err.Error(), tc.raw) {
					t.Fatalf("invalid budget must fail without echoing its value: %v", err)
				}
				return
			}
			if err != nil || cfg.MaxWriteTransactions != tc.want {
				t.Fatalf("budget=%d, want=%d: %v", cfg.MaxWriteTransactions, tc.want, err)
			}
		})
	}
}
