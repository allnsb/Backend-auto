package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type Config struct {
	HTTPSAddr                  string
	TLSCertFile                string
	TLSKeyFile                 string
	DatabaseURL                string
	AuthIssuer                 string
	AuthAudience               string
	AuthJWKSURL                string
	AuthCAFile                 string
	RealtimeConnectionSecret   string
	RealtimeProxySecret        string
	MaxWriteTransactions       int64
	ConversationMemberLimit    int
	R2AccountID                string
	R2AccessKeyID              string
	R2SecretAccessKey          string
	R2Bucket                   string
	R2Endpoint                 string
	R2Region                   string
	AttachmentMaxBytes         int64
	AttachmentMaxEdge          int
	AttachmentMaxPixels        int64
	AttachmentMaxVoiceMS       int
	AttachmentUploadsPerMinute int
	AttachmentQuotaBytes       int64
	AttachmentPresignTTL       time.Duration
	AttachmentMaxPerMessage    int
}

func Load() (Config, error) {
	cfg := Config{
		HTTPSAddr:                os.Getenv("ODYSSEIA_HTTPS_ADDR"),
		TLSCertFile:              os.Getenv("ODYSSEIA_TLS_CERT_FILE"),
		TLSKeyFile:               os.Getenv("ODYSSEIA_TLS_KEY_FILE"),
		DatabaseURL:              os.Getenv("ODYSSEIA_DATABASE_URL"),
		AuthIssuer:               os.Getenv("ODYSSEIA_AUTH_ISSUER"),
		AuthAudience:             os.Getenv("ODYSSEIA_AUTH_AUDIENCE"),
		AuthJWKSURL:              os.Getenv("ODYSSEIA_AUTH_JWKS_URL"),
		AuthCAFile:               os.Getenv("ODYSSEIA_AUTH_CA_FILE"),
		RealtimeConnectionSecret: os.Getenv("ODYSSEIA_REALTIME_CONNECTION_SECRET"),
		RealtimeProxySecret:      os.Getenv("ODYSSEIA_REALTIME_PROXY_SECRET"),
	}
	if cfg.HTTPSAddr == "" {
		cfg.HTTPSAddr = "127.0.0.1:8443"
	}
	if _, _, err := net.SplitHostPort(cfg.HTTPSAddr); err != nil {
		return Config{}, fmt.Errorf("ODYSSEIA_HTTPS_ADDR: %w", err)
	}
	if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
		return Config{}, errors.New("ODYSSEIA_TLS_CERT_FILE and ODYSSEIA_TLS_KEY_FILE are required")
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("ODYSSEIA_DATABASE_URL is required")
	}
	if cfg.AuthIssuer == "" || cfg.AuthAudience == "" || cfg.AuthJWKSURL == "" {
		return Config{}, errors.New("ODYSSEIA_AUTH_ISSUER, ODYSSEIA_AUTH_AUDIENCE and ODYSSEIA_AUTH_JWKS_URL are required")
	}
	if cfg.RealtimeConnectionSecret == "" || cfg.RealtimeProxySecret == "" {
		return Config{}, errors.New("ODYSSEIA_REALTIME_CONNECTION_SECRET and ODYSSEIA_REALTIME_PROXY_SECRET are required")
	}
	if cfg.RealtimeConnectionSecret == cfg.RealtimeProxySecret {
		return Config{}, errors.New("connection signing and internal proxy secrets must differ")
	}
	if raw := os.Getenv("ODYSSEIA_MAX_WRITE_TRANSACTIONS"); raw != "" {
		limit, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || limit < 0 {
			return Config{}, errors.New("ODYSSEIA_MAX_WRITE_TRANSACTIONS must be a nonnegative integer; zero selects the automatic budget")
		}
		cfg.MaxWriteTransactions = limit
	}
	cfg.ConversationMemberLimit = 50
	if raw := os.Getenv("ODYSSEIA_CONVERSATION_MEMBER_LIMIT"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 50 {
			return Config{}, errors.New("ODYSSEIA_CONVERSATION_MEMBER_LIMIT must be an integer from 1 to 50")
		}
		cfg.ConversationMemberLimit = limit
	}
	if err := loadAttachmentConfig(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func loadAttachmentConfig(cfg *Config) error {
	cfg.R2AccountID = os.Getenv("ODYSSEIA_R2_ACCOUNT_ID")
	cfg.R2AccessKeyID = os.Getenv("ODYSSEIA_R2_ACCESS_KEY_ID")
	cfg.R2SecretAccessKey = os.Getenv("ODYSSEIA_R2_SECRET_ACCESS_KEY")
	cfg.R2Bucket = os.Getenv("ODYSSEIA_R2_BUCKET")
	cfg.R2Endpoint = os.Getenv("ODYSSEIA_R2_ENDPOINT")
	cfg.R2Region = os.Getenv("ODYSSEIA_R2_REGION")
	if cfg.R2Region == "" {
		cfg.R2Region = "auto"
	}
	if !asciiToken(cfg.R2AccountID, 1, 64) || strings.ContainsAny(cfg.R2AccountID, ".-") {
		return errors.New("ODYSSEIA_R2_ACCOUNT_ID must be 1 to 64 letters or digits")
	}
	if !asciiToken(cfg.R2AccessKeyID, 1, 128) || strings.ContainsAny(cfg.R2SecretAccessKey, "\r\n") || cfg.R2SecretAccessKey == "" || len(cfg.R2SecretAccessKey) > 256 {
		return errors.New("ODYSSEIA_R2_ACCESS_KEY_ID and ODYSSEIA_R2_SECRET_ACCESS_KEY are required")
	}
	if !bucketName(cfg.R2Bucket) {
		return errors.New("ODYSSEIA_R2_BUCKET must be a lowercase DNS label of 3 to 63 characters")
	}
	if !asciiToken(cfg.R2Region, 1, 32) {
		return errors.New("ODYSSEIA_R2_REGION must be 1 to 32 letters, digits, or hyphens")
	}
	if cfg.R2Endpoint != "" {
		parsed, err := url.Parse(cfg.R2Endpoint)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return errors.New("ODYSSEIA_R2_ENDPOINT must be an https origin without user info, query, or fragment")
		}
	}
	cfg.AttachmentMaxBytes = 10 << 20
	cfg.AttachmentMaxEdge = 8192
	cfg.AttachmentMaxPixels = 16_777_216
	cfg.AttachmentMaxVoiceMS = 300_000
	cfg.AttachmentUploadsPerMinute = 10
	cfg.AttachmentQuotaBytes = 100 << 20
	cfg.AttachmentPresignTTL = time.Minute
	cfg.AttachmentMaxPerMessage = 4
	var err error
	if cfg.AttachmentMaxBytes, err = optionalInt64("ODYSSEIA_ATTACHMENT_MAX_BYTES", cfg.AttachmentMaxBytes, 1, 10<<20); err != nil {
		return err
	}
	if cfg.AttachmentMaxEdge, err = optionalInt("ODYSSEIA_ATTACHMENT_MAX_EDGE", cfg.AttachmentMaxEdge, 1, 32768); err != nil {
		return err
	}
	if cfg.AttachmentMaxPixels, err = optionalInt64("ODYSSEIA_ATTACHMENT_MAX_PIXELS", cfg.AttachmentMaxPixels, 1, 67_108_864); err != nil {
		return err
	}
	if cfg.AttachmentMaxVoiceMS, err = optionalInt("ODYSSEIA_ATTACHMENT_MAX_VOICE_MS", cfg.AttachmentMaxVoiceMS, 1, 300_000); err != nil {
		return err
	}
	if cfg.AttachmentUploadsPerMinute, err = optionalInt("ODYSSEIA_ATTACHMENT_UPLOADS_PER_MINUTE", cfg.AttachmentUploadsPerMinute, 1, 60); err != nil {
		return err
	}
	if cfg.AttachmentQuotaBytes, err = optionalInt64("ODYSSEIA_ATTACHMENT_USER_QUOTA_BYTES", cfg.AttachmentQuotaBytes, 1, 1<<30); err != nil {
		return err
	}
	if cfg.AttachmentMaxPerMessage, err = optionalInt("ODYSSEIA_ATTACHMENT_MAX_PER_MESSAGE", cfg.AttachmentMaxPerMessage, 1, 4); err != nil {
		return err
	}
	if raw := os.Getenv("ODYSSEIA_ATTACHMENT_PRESIGN_TTL"); raw != "" {
		ttl, err := time.ParseDuration(raw)
		if err != nil || ttl < time.Second || ttl > 5*time.Minute {
			return errors.New("ODYSSEIA_ATTACHMENT_PRESIGN_TTL must be a duration from 1s to 5m")
		}
		cfg.AttachmentPresignTTL = ttl
	}
	return nil
}

func optionalInt(name string, current, min, max int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return current, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < min || parsed > max {
		return 0, fmt.Errorf("%s must be an integer from %d to %d", name, min, max)
	}
	return parsed, nil
}

func optionalInt64(name string, current, min, max int64) (int64, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return current, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed < min || parsed > max {
		return 0, fmt.Errorf("%s must be an integer from %d to %d", name, min, max)
	}
	return parsed, nil
}

func asciiToken(value string, min, max int) bool {
	if len(value) < min || len(value) > max {
		return false
	}
	for _, r := range value {
		if r > unicode.MaxASCII || !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' {
			return false
		}
	}
	return true
}

func bucketName(value string) bool {
	if len(value) < 3 || len(value) > 63 || strings.HasPrefix(value, "-") || strings.HasSuffix(value, "-") || strings.Contains(value, "..") {
		return false
	}
	for _, r := range value {
		if r > unicode.MaxASCII || !unicode.IsLower(r) && !unicode.IsDigit(r) && r != '-' {
			return false
		}
	}
	return true
}
