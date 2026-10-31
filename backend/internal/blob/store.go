// Package blob is the object-storage seam.
// Store is the private conversation bucket: short-lived presigned GET, no-store.
// PublicStore is a separate bucket for content-addressed emoji: stable URLs and
// immutable cache headers. A later presigned PUT can satisfy Store without
// changing message binding.
package blob

import (
	"context"
	"time"
)

// Object is one stored blob. Key is chosen by the server. Body is the sanitized bytes.
type Object struct {
	Key                string
	ContentType        string
	ContentDisposition string
	Body               []byte
}

// Get describes one short-lived download. The signed URL is a bearer token.
type Get struct {
	Key                string
	ContentType        string
	ContentDisposition string
	TTL                time.Duration
}

// Store hides the provider. Put and Delete run only on the server side.
// PresignGet does not transfer the object. Delete of a missing key succeeds
// so the retention sweep can retry. One key per Delete: R2 rejects the CRC32
// checksum that aws-sdk-go-v2 attaches to DeleteObjects.
type Store interface {
	Put(ctx context.Context, object Object) error
	PresignGet(ctx context.Context, get Get) (string, error)
	Delete(ctx context.Context, key string) error
}
