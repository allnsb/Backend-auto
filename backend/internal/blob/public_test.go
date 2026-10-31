package blob

import (
	"context"
	"testing"
)

func TestPublicObjectIsImmutableAndSeparate(t *testing.T) {
	if ImmutableCacheControl != "public, max-age=31536000, immutable" {
		t.Fatal(ImmutableCacheControl)
	}
	input, err := PublicPutInput("odysseia-emoji", PublicObject{
		Key: "emoji/v1/ab/abcd.png", ContentType: "image/png", Body: []byte{1, 2, 3},
	})
	if err != nil || input.CacheControl == nil || *input.CacheControl != ImmutableCacheControl {
		t.Fatalf("cache = %v err=%v", input, err)
	}
	if _, err := PublicPutInput("odysseia-emoji", PublicObject{Key: "../x", ContentType: "image/png", Body: []byte{1}}); err == nil {
		t.Fatal("path escape accepted")
	}
	store, err := NewMemoryPublic("https://emoji.invalid/custom")
	if err != nil {
		t.Fatal(err)
	}
	object := PublicObject{Key: "emoji/v1/ab/abcd.png", ContentType: "image/png", Body: []byte{1, 2, 3}}
	if err := store.Put(context.Background(), object); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), object); err != nil {
		t.Fatal(err)
	}
	object.Body = []byte{9}
	if err := store.Put(context.Background(), object); err == nil {
		t.Fatal("different bytes reused a content key")
	}
	if store.PublicURL(object.Key) != "https://emoji.invalid/custom/emoji/v1/ab/abcd.png" {
		t.Fatal(store.PublicURL(object.Key))
	}
	if _, err := NewMemoryPublic("http://emoji.invalid"); err == nil {
		t.Fatal("http base accepted")
	}
}
