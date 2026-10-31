package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// R2Config points at one private Cloudflare R2 bucket. Endpoint overrides the
// account hostname; Region is required by the SDK and is "auto" for R2.
type R2Config struct {
	AccountID string
	AccessKey string
	SecretKey string
	Bucket    string
	Endpoint  string
	Region    string
}

// R2 stores objects with the AWS SDK v2 S3 client. Default CRC32 checksums are
// turned off because R2 rejects that optional trailing checksum. Presigned GET
// is signed locally and is not an R2 round trip.
type R2 struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
}

func NewR2(ctx context.Context, cfg R2Config) (*R2, error) {
	if cfg.AccountID == "" || cfg.AccessKey == "" || cfg.SecretKey == "" || cfg.Bucket == "" {
		return nil, errors.New("R2 account, credentials, and bucket are required")
	}
	if cfg.Region == "" {
		cfg.Region = "auto"
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "https://" + cfg.AccountID + ".r2.cloudflarestorage.com"
	}
	loaded, err := config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")),
		config.WithRegion(cfg.Region),
		config.WithEC2IMDSClientEnableState(imds.ClientDisabled),
	)
	if err != nil {
		return nil, fmt.Errorf("configure R2 client: %w", err)
	}
	client := s3.NewFromConfig(loaded, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(endpoint)
		// service/s3 from v1.73 defaults to a CRC32 body checksum. R2 does not
		// accept that optional header and can stall the upload. WHEN_REQUIRED
		// keeps checksums only for operations that mandate one.
		options.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		options.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return &R2{client: client, presign: s3.NewPresignClient(client), bucket: cfg.Bucket}, nil
}

func (r *R2) Put(ctx context.Context, object Object) error {
	if object.Key == "" || object.ContentType == "" || len(object.Body) == 0 {
		return errors.New("R2 put requires a key, content type, and body")
	}
	_, err := r.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:             aws.String(r.bucket),
		Key:                aws.String(object.Key),
		Body:               bytes.NewReader(object.Body),
		ContentLength:      aws.Int64(int64(len(object.Body))),
		ContentType:        aws.String(object.ContentType),
		ContentDisposition: aws.String(object.ContentDisposition),
		CacheControl:       aws.String("private, no-store"),
	})
	if err != nil {
		return fmt.Errorf("put R2 object: %w", err)
	}
	return nil
}

func (r *R2) PresignGet(ctx context.Context, get Get) (string, error) {
	if get.Key == "" || get.TTL <= 0 {
		return "", errors.New("R2 presign requires a key and a positive ttl")
	}
	signed, err := r.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket:                     aws.String(r.bucket),
		Key:                        aws.String(get.Key),
		ResponseContentType:        aws.String(get.ContentType),
		ResponseContentDisposition: aws.String(get.ContentDisposition),
		ResponseCacheControl:       aws.String("private, no-store"),
	}, s3.WithPresignExpires(get.TTL))
	if err != nil {
		return "", fmt.Errorf("presign R2 get: %w", err)
	}
	return signed.URL, nil
}

func (r *R2) Delete(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("R2 delete requires a key")
	}
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete R2 object: %w", err)
	}
	return nil
}
