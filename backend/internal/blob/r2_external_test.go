//go:build external

package blob

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Explicit opt-in only. These checks write and delete one random synthetic
// object per bucket. They never list buckets, touch existing keys, print
// credentials/presigned URLs, or count a skipped check as external acceptance.
func TestExternalR2PrivateRoundTrip(t *testing.T) {
	cfg := externalR2Config(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, err := NewR2(ctx, cfg)
	if err != nil {
		t.Fatal("cannot initialize the configured private R2 bucket")
	}
	key := "conversations/p4-acceptance/" + uuid.NewString() + "/pixel.png"
	cleanupExternalR2(t, store, key)
	body := externalPixel(t)
	if err := store.Put(ctx, Object{Key: key, ContentType: "image/png", ContentDisposition: `inline; filename="pixel.png"`, Body: body}); err != nil {
		t.Fatal("private R2 PutObject failed; credentials and signed URLs are omitted")
	}
	signed, err := store.PresignGet(ctx, Get{Key: key, ContentType: "image/png", ContentDisposition: `inline; filename="pixel.png"`, TTL: time.Minute})
	if err != nil {
		t.Fatal("private R2 presigning failed")
	}
	response := externalR2Get(t, ctx, signed)
	data, err := io.ReadAll(io.LimitReader(response.Body, 8192))
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || !bytes.Equal(data, body) || response.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("private R2 signed round-trip or cache policy failed: status=%d", response.StatusCode)
	}
	unsigned, err := url.Parse(signed)
	if err != nil {
		t.Fatal("invalid signed URL returned")
	}
	unsigned.RawQuery = ""
	denied := externalR2Get(t, ctx, unsigned.String())
	denied.Body.Close()
	if denied.StatusCode != 401 && denied.StatusCode != 403 && denied.StatusCode != 404 {
		t.Fatalf("unsigned S3 API access was not denied: status=%d", denied.StatusCode)
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Fatal("private R2 DeleteObject failed")
	}
	missing := externalR2Get(t, ctx, signed)
	missing.Body.Close()
	if missing.StatusCode != 404 {
		t.Fatalf("deleted private object remains available: status=%d", missing.StatusCode)
	}
}

func TestExternalR2PublicRoundTrip(t *testing.T) {
	cfg := externalR2Config(t, true)
	base := os.Getenv("ODYSSEIA_EMOJI_PUBLIC_BASE_URL")
	if base == "" {
		t.Fatal("ODYSSEIA_EMOJI_PUBLIC_BASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	public, err := NewPublicR2(ctx, cfg, base)
	if err != nil {
		t.Fatal("cannot initialize the configured public R2 bucket")
	}
	deleter, err := NewR2(ctx, cfg)
	if err != nil {
		t.Fatal("cannot initialize cleanup for the public test object")
	}
	// A random non-catalog key avoids collisions with shared emoji hashes.
	key := "acceptance/p4/" + uuid.NewString() + "/pixel.png"
	cleanupExternalR2(t, deleter, key)
	body := externalPixel(t)
	if err := public.Put(ctx, PublicObject{Key: key, ContentType: "image/png", Body: body}); err != nil {
		t.Fatal("public R2 PutObject failed")
	}
	response := externalR2Get(t, ctx, public.PublicURL(key))
	data, err := io.ReadAll(io.LimitReader(response.Body, 8192))
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || !bytes.Equal(data, body) || response.Header.Get("Cache-Control") != ImmutableCacheControl {
		t.Fatalf("public R2 round-trip or immutable cache policy failed: status=%d", response.StatusCode)
	}
	// Deletion cleans the origin. A public immutable CDN may retain this
	// harmless synthetic pixel, so a post-delete CDN 404 is not asserted.
}

func externalR2Config(t *testing.T, public bool) R2Config {
	t.Helper()
	if os.Getenv("ODYSSEIA_TEST_EXTERNAL_R2") != "1" {
		t.Skip("real R2 acceptance requires explicit ODYSSEIA_TEST_EXTERNAL_R2=1")
	}
	get := func(name string) string {
		if public {
			if value := os.Getenv("ODYSSEIA_EMOJI_R2_" + name); value != "" {
				return value
			}
		}
		return os.Getenv("ODYSSEIA_R2_" + name)
	}
	bucket := os.Getenv("ODYSSEIA_R2_BUCKET")
	if public {
		bucket = os.Getenv("ODYSSEIA_EMOJI_R2_BUCKET")
		if bucket == os.Getenv("ODYSSEIA_R2_BUCKET") {
			t.Fatal("public emoji and private attachment buckets must differ")
		}
	}
	cfg := R2Config{AccountID: get("ACCOUNT_ID"), AccessKey: get("ACCESS_KEY_ID"), SecretKey: get("SECRET_ACCESS_KEY"), Bucket: bucket, Endpoint: get("ENDPOINT"), Region: "auto"}
	if cfg.AccountID == "" || cfg.AccessKey == "" || cfg.SecretKey == "" || cfg.Bucket == "" {
		t.Fatal("real R2 acceptance requires account, key, secret and bucket configuration")
	}
	if cfg.Endpoint != "" {
		endpoint, err := url.Parse(cfg.Endpoint)
		if err != nil || endpoint.Scheme != "https" || !strings.HasSuffix(endpoint.Hostname(), ".r2.cloudflarestorage.com") {
			t.Fatal("real R2 acceptance requires a verified Cloudflare R2 HTTPS endpoint")
		}
	}
	return cfg
}

func cleanupExternalR2(t *testing.T, store *R2, key string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := store.Delete(ctx, key); err != nil {
			t.Errorf("cleanup failed for synthetic object %s; delete that exact key manually", key)
		}
	})
}

func externalR2Get(t *testing.T, ctx context.Context, address string) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		t.Fatal("cannot construct R2 acceptance request")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("R2 HTTPS request failed; signed URL is omitted")
	}
	return response
}

func externalPixel(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
