package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ImmutableCacheControl is the RFC 8246 header for a content-addressed object.
// The key contains the hash, so the bytes at that URL do not change.
const ImmutableCacheControl = "public, max-age=31536000, immutable"

// PublicObject is one CDN object. Key is chosen by the caller and includes a
// content hash. Body is the sanitized bytes.
type PublicObject struct {
	Key         string
	ContentType string
	Body        []byte
}

// PublicStore is a bucket that is safe to publish. It is not the conversation
// attachment store: no presign, no no-store, and a different bucket.
type PublicStore interface {
	Put(ctx context.Context, object PublicObject) error
	PublicURL(key string) string
}

// PublicR2 writes immutable objects and returns a stable public URL.
type PublicR2 struct {
	client *s3.Client
	bucket string
	base   string
}

func NewPublicR2(ctx context.Context, cfg R2Config, publicBase string) (*PublicR2, error) {
	base, err := CleanPublicBase(publicBase)
	if err != nil {
		return nil, err
	}
	client, err := newS3Client(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &PublicR2{client: client, bucket: cfg.Bucket, base: base}, nil
}

func (p *PublicR2) Put(ctx context.Context, object PublicObject) error {
	input, err := PublicPutInput(p.bucket, object)
	if err != nil {
		return err
	}
	if _, err := p.client.PutObject(ctx, input); err != nil {
		return fmt.Errorf("put public R2 object: %w", err)
	}
	return nil
}

func (p *PublicR2) PublicURL(key string) string {
	return p.base + "/" + key
}

// PublicPutInput is the PutObject request for a public bucket. Tests check the
// cache header without calling R2.
func PublicPutInput(bucket string, object PublicObject) (*s3.PutObjectInput, error) {
	if err := validPublicKey(object.Key); err != nil || object.ContentType == "" || len(object.Body) == 0 || bucket == "" {
		return nil, errors.New("public put requires a bucket, key, content type, and body")
	}
	return &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(object.Key),
		Body:          bytes.NewReader(object.Body),
		ContentLength: aws.Int64(int64(len(object.Body))),
		ContentType:   aws.String(object.ContentType),
		CacheControl:  aws.String(ImmutableCacheControl),
	}, nil
}

// CleanPublicBase accepts an https origin, optionally with a path, and no
// trailing slash, user info, query, or fragment.
func CleanPublicBase(raw string) (string, error) {
	if strings.HasSuffix(raw, "/") || strings.Contains(raw, "\\") {
		return "", errors.New("public base URL must be https and must not end with a slash")
	}
	if !strings.HasPrefix(raw, "https://") || strings.Contains(raw, "?") || strings.Contains(raw, "#") || strings.Contains(raw, "@") {
		return "", errors.New("public base URL must be https and must not end with a slash")
	}
	rest := strings.TrimPrefix(raw, "https://")
	host, _, _ := strings.Cut(rest, "/")
	if host == "" || strings.Contains(host, " ") {
		return "", errors.New("public base URL must be https and must not end with a slash")
	}
	return raw, nil
}

func validPublicKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") || strings.Contains(key, "\\") {
		return errors.New("public object key is invalid")
	}
	return nil
}

// MemoryPublic is the in-process public bucket used when tests have no R2 account.
type MemoryPublic struct {
	base    string
	mu      sync.Mutex
	objects map[string]PublicObject
	Puts    int
}

func NewMemoryPublic(publicBase string) (*MemoryPublic, error) {
	base, err := CleanPublicBase(publicBase)
	if err != nil {
		return nil, err
	}
	return &MemoryPublic{base: base, objects: map[string]PublicObject{}}, nil
}

func (m *MemoryPublic) Put(_ context.Context, object PublicObject) error {
	if err := validPublicKey(object.Key); err != nil || object.ContentType == "" || len(object.Body) == 0 {
		return errors.New("public put requires a key, content type, and body")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.objects[object.Key]; ok && !bytes.Equal(existing.Body, object.Body) {
		return errors.New("public object key already stores different bytes")
	}
	copied := object
	copied.Body = append([]byte(nil), object.Body...)
	m.objects[object.Key] = copied
	m.Puts++
	return nil
}

func (m *MemoryPublic) PublicURL(key string) string {
	return m.base + "/" + key
}

// Len reports how many objects are currently stored.
func (m *MemoryPublic) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objects)
}
