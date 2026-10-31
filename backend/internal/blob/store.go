// Package blob is the object-storage seam for conversation attachments.
// The first implementation streams bytes through the API into R2. A later
// presigned PUT can satisfy the same interface without changing message binding.
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

// Store hides the provider. Put and Delete run only on the API side.
// PresignGet does not transfer the object.
type Store interface {
	Put(ctx context.Context, object Object) error
	PresignGet(ctx context.Context, get Get) (string, error)
	Delete(ctx context.Context, key string) error
}
