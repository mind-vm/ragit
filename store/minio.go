package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// MinIOConfig configures MinIOStore. Works against MinIO itself or any
// S3-compatible endpoint (AWS S3, R2, B2, ...).
type MinIOConfig struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool
}

// MinIOStore is an S3-compatible Store implementation.
type MinIOStore struct {
	client *minio.Client
	bucket string
}

var _ Store = (*MinIOStore)(nil)

// NewMinIOStore builds a MinIOStore and ensures its bucket exists.
func NewMinIOStore(ctx context.Context, cfg MinIOConfig) (*MinIOStore, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("store: build minio client: %w", err)
	}

	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("store: check bucket: %w", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("store: create bucket: %w", err)
		}
	}

	return &MinIOStore{client: client, bucket: cfg.Bucket}, nil
}

// Put uploads data under a tenant-prefixed, collision-resistant key and
// returns an s3:// URI identifying it.
func (s *MinIOStore) Put(ctx context.Context, tenantID uuid.UUID, filename string, data []byte, mimeType string) (string, error) {
	key := fmt.Sprintf("%s/%s/%s", tenantID, uuid.NewString(), filename)
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: mimeType})
	if err != nil {
		return "", fmt.Errorf("store: put object: %w", err)
	}
	return "s3://" + s.bucket + "/" + key, nil
}

// Get retrieves the object at uri (as returned by Put).
func (s *MinIOStore) Get(ctx context.Context, uri string) (io.ReadCloser, error) {
	bucket, key, err := parseURI(uri)
	if err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("store: get object: %w", err)
	}
	return obj, nil
}

// Delete removes the object at uri. S3 DELETE is already idempotent — it
// reports success for a key that does not exist — so this needs no
// special-casing to satisfy the interface's idempotency contract.
func (s *MinIOStore) Delete(ctx context.Context, uri string) error {
	bucket, key, err := parseURI(uri)
	if err != nil {
		return err
	}
	if err := s.client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("store: delete object: %w", err)
	}
	return nil
}

func parseURI(uri string) (bucket, key string, err error) {
	if !strings.HasPrefix(uri, "s3://") {
		return "", "", fmt.Errorf("store: not an s3 URI: %s", uri)
	}
	u, err := url.Parse(uri)
	if err != nil {
		return "", "", fmt.Errorf("store: parse URI: %w", err)
	}
	return u.Host, strings.TrimPrefix(u.Path, "/"), nil
}
