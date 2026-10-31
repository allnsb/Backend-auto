package blob

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPresignGetIsLocalAndDoesNotEmbedTheSecret(t *testing.T) {
	const secret = "test-secret-not-in-the-url"
	store, err := NewR2(context.Background(), R2Config{
		AccountID: "abc123",
		AccessKey: "access-key",
		SecretKey: secret,
		Bucket:    "odysseia-attachments",
	})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := store.PresignGet(context.Background(), Get{
		Key:                "conversations/11111111-1111-4111-8111-111111111111/22222222-2222-4222-8222-222222222222",
		ContentType:        "image/jpeg",
		ContentDisposition: `inline; filename="image.jpg"`,
		TTL:                time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(signed, secret) {
		t.Fatal("presigned URL embedded the secret access key")
	}
	for _, part := range []string{"abc123.r2.cloudflarestorage.com", "X-Amz-Expires=60", "X-Amz-Signature=", "response-content-type=", "image.jpg"} {
		if !strings.Contains(signed, part) {
			t.Fatalf("url missing %s: %s", part, signed)
		}
	}
}

func TestMemoryStoreRoundTrip(t *testing.T) {
	store := NewMemory()
	object := Object{Key: "conversations/a/b", ContentType: "image/png", Body: []byte("png")}
	if err := store.Put(context.Background(), object); err != nil {
		t.Fatal(err)
	}
	signed, err := store.PresignGet(context.Background(), Get{Key: object.Key, ContentType: object.ContentType, TTL: 30 * time.Second})
	if err != nil || !strings.Contains(signed, "conversations%2Fa%2Fb") || !strings.HasPrefix(signed, "https://blob.invalid/") {
		t.Fatalf("signed=%s err=%v", signed, err)
	}
	if err := store.Delete(context.Background(), object.Key); err != nil || store.Len() != 0 || store.Deletes != 1 {
		t.Fatalf("delete len=%d deletes=%d err=%v", store.Len(), store.Deletes, err)
	}
	if err := store.Delete(context.Background(), object.Key); err != nil || store.Deletes != 2 {
		t.Fatalf("missing delete deletes=%d err=%v", store.Deletes, err)
	}
}
