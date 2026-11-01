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

	"github.com/google/uuid"
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
	EmojiR2AccountID           string
	EmojiR2AccessKeyID         string
	EmojiR2SecretAccessKey     string
	EmojiR2Bucket              string
	EmojiR2Endpoint            string
	EmojiR2Region              string
	EmojiPublicBaseURL         string
	EmojiMaxBytes              int64
	EmojiMaxEdge               int
	EmojiSiteOwnerIDs          []string
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
	if err := loadEmojiConfig(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func loadEmojiConfig(cfg *Config) error {
	cfg.EmojiR2Bucket = os.Getenv("ODYSSEIA_EMOJI_R2_BUCKET")
	if !bucketName(cfg.EmojiR2Bucket) {
		return errors.New("ODYSSEIA_EMOJI_R2_BUCKET must be a lowercase DNS label of 3 to 63 characters")
	}
	if cfg.EmojiR2Bucket == cfg.R2Bucket {
		return errors.New("ODYSSEIA_EMOJI_R2_BUCKET must differ from ODYSSEIA_R2_BUCKET")
	}
	account := os.Getenv("ODYSSEIA_EMOJI_R2_ACCOUNT_ID")
	key := os.Getenv("ODYSSEIA_EMOJI_R2_ACCESS_KEY_ID")
	secret := os.Getenv("ODYSSEIA_EMOJI_R2_SECRET_ACCESS_KEY")
	endpoint := os.Getenv("ODYSSEIA_EMOJI_R2_ENDPOINT")
	region := os.Getenv("ODYSSEIA_EMOJI_R2_REGION")
	if account == "" && key == "" && secret == "" && endpoint == "" && region == "" {
		account, key, secret, endpoint, region = cfg.R2AccountID, cfg.R2AccessKeyID, cfg.R2SecretAccessKey, cfg.R2Endpoint, cfg.R2Region
	} else if !asciiToken(account, 1, 64) || strings.ContainsAny(account, ".-") || !asciiToken(key, 1, 128) || strings.ContainsAny(secret, "\r\n") || secret == "" || len(secret) > 256 {
		return errors.New("ODYSSEIA_EMOJI_R2_ACCOUNT_ID, ODYSSEIA_EMOJI_R2_ACCESS_KEY_ID and ODYSSEIA_EMOJI_R2_SECRET_ACCESS_KEY are required together")
	} else if endpoint != "" {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return errors.New("ODYSSEIA_EMOJI_R2_ENDPOINT must be an https origin without user info, query, or fragment")
		}
	}
	if region == "" {
		region = "auto"
	}
	if !asciiToken(region, 1, 32) {
		return errors.New("ODYSSEIA_EMOJI_R2_REGION must be 1 to 32 letters, digits, or hyphens")
	}
	base := os.Getenv("ODYSSEIA_EMOJI_PUBLIC_BASE_URL")
	if strings.HasSuffix(base, "/") || !strings.HasPrefix(base, "https://") || strings.ContainsAny(base, "?#@") {
		return errors.New("ODYSSEIA_EMOJI_PUBLIC_BASE_URL must be an https URL without user info, query, fragment, or a trailing slash")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("ODYSSEIA_EMOJI_PUBLIC_BASE_URL must be an https URL without user info, query, fragment, or a trailing slash")
	}
	cfg.EmojiR2AccountID = account
	cfg.EmojiR2AccessKeyID = key
	cfg.EmojiR2SecretAccessKey = secret
	cfg.EmojiR2Endpoint = endpoint
	cfg.EmojiR2Region = region
	cfg.EmojiPublicBaseURL = base
	cfg.EmojiMaxBytes = 256 * 1024
	cfg.EmojiMaxEdge = 128
	if cfg.EmojiMaxBytes, err = optionalInt64("ODYSSEIA_EMOJI_MAX_BYTES", cfg.EmojiMaxBytes, 1, 256*1024); err != nil {
		return err
	}
	if cfg.EmojiMaxEdge, err = optionalInt("ODYSSEIA_EMOJI_MAX_EDGE", cfg.EmojiMaxEdge, 1, 256); err != nil {
		return err
	}
	rawOwners := os.Getenv("ODYSSEIA_EMOJI_SITE_OWNER_IDS")
	if rawOwners == "" {
		return nil
	}
	seen := map[string]struct{}{}
	for _, part := range strings.Split(rawOwners, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return errors.New("ODYSSEIA_EMOJI_SITE_OWNER_IDS must be canonical UUIDs separated by commas")
		}
		parsed, err := uuid.Parse(part)
		if err != nil || parsed == uuid.Nil || parsed.String() != part {
			return errors.New("ODYSSEIA_EMOJI_SITE_OWNER_IDS must be canonical UUIDs separated by commas")
		}
		if _, ok := seen[part]; ok {
			return errors.New("ODYSSEIA_EMOJI_SITE_OWNER_IDS must be canonical UUIDs separated by commas")
		}
		seen[part] = struct{}{}
		cfg.EmojiSiteOwnerIDs = append(cfg.EmojiSiteOwnerIDs, part)
	}
	return nil
}

// ObjectStore is the R2 (or other S3-compatible) target. The API and the
// attachment sweep both read it; the sweep does not load the HTTPS listener.
type ObjectStore struct {
	AccountID       string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
	Endpoint        string
	Region          string
}

func LoadObjectStore() (ObjectStore, error) {
	cfg := ObjectStore{
		AccountID:       os.Getenv("ODYSSEIA_R2_ACCOUNT_ID"),
		AccessKeyID:     os.Getenv("ODYSSEIA_R2_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("ODYSSEIA_R2_SECRET_ACCESS_KEY"),
		Bucket:          os.Getenv("ODYSSEIA_R2_BUCKET"),
		Endpoint:        os.Getenv("ODYSSEIA_R2_ENDPOINT"),
		Region:          os.Getenv("ODYSSEIA_R2_REGION"),
	}
	if cfg.Region == "" {
		cfg.Region = "auto"
	}
	if !asciiToken(cfg.AccountID, 1, 64) || strings.ContainsAny(cfg.AccountID, ".-") {
		return ObjectStore{}, errors.New("ODYSSEIA_R2_ACCOUNT_ID must be 1 to 64 letters or digits")
	}
	if !asciiToken(cfg.AccessKeyID, 1, 128) || strings.ContainsAny(cfg.SecretAccessKey, "\r\n") || cfg.SecretAccessKey == "" || len(cfg.SecretAccessKey) > 256 {
		return ObjectStore{}, errors.New("ODYSSEIA_R2_ACCESS_KEY_ID and ODYSSEIA_R2_SECRET_ACCESS_KEY are required")
	}
	if !bucketName(cfg.Bucket) {
		return ObjectStore{}, errors.New("ODYSSEIA_R2_BUCKET must be a lowercase DNS label of 3 to 63 characters")
	}
	if !asciiToken(cfg.Region, 1, 32) {
		return ObjectStore{}, errors.New("ODYSSEIA_R2_REGION must be 1 to 32 letters, digits, or hyphens")
	}
	if cfg.Endpoint != "" {
		parsed, err := url.Parse(cfg.Endpoint)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return ObjectStore{}, errors.New("ODYSSEIA_R2_ENDPOINT must be an https origin without user info, query, or fragment")
		}
	}
	return cfg, nil
}

// DefaultAttachmentRetention is about 7 days from attachments.created_at.
// Go durations have no day unit; 168h is the unset value.
const DefaultAttachmentRetention = 168 * time.Hour

// AttachmentRetention reads ODYSSEIA_ATTACHMENT_RETENTION. The sweep command
// uses it. The API process does not delete objects.
func AttachmentRetention() (time.Duration, error) {
	raw := os.Getenv("ODYSSEIA_ATTACHMENT_RETENTION")
	if raw == "" {
		return DefaultAttachmentRetention, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed < time.Hour || parsed > 366*24*time.Hour {
		return 0, errors.New("ODYSSEIA_ATTACHMENT_RETENTION must be a Go duration from 1h to 8784h; the default is 168h")
	}
	return parsed, nil
}

func loadAttachmentConfig(cfg *Config) error {
	store, err := LoadObjectStore()
	if err != nil {
		return err
	}
	cfg.R2AccountID = store.AccountID
	cfg.R2AccessKeyID = store.AccessKeyID
	cfg.R2SecretAccessKey = store.SecretAccessKey
	cfg.R2Bucket = store.Bucket
	cfg.R2Endpoint = store.Endpoint
	cfg.R2Region = store.Region
	cfg.AttachmentMaxBytes = 10 << 20
	cfg.AttachmentMaxEdge = 8192
	cfg.AttachmentMaxPixels = 16_777_216
	cfg.AttachmentMaxVoiceMS = 300_000
	cfg.AttachmentUploadsPerMinute = 10
	cfg.AttachmentQuotaBytes = 100 << 20
	cfg.AttachmentPresignTTL = time.Minute
	cfg.AttachmentMaxPerMessage = 4
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
