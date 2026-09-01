// Package objectstore is the platform's S3-compatible blob store.
//
// It deliberately exposes only what the media flow needs: reserve a key, sign a
// short-lived upload, sign a short-lived read, confirm an object landed, and
// delete. Everything is presigned so bytes travel client↔storage directly and
// never through the Domain API — which is also why the API stays cheap on the
// read-optimised path.
//
// The provider (AWS S3 / Cloudflare R2 / Backblaze B2 / MinIO for dev) is
// configuration: all four speak this API. See ADR-005.
package objectstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ErrNotFound is returned when an object is absent.
var ErrNotFound = errors.New("objectstore: object not found")

// ObjectInfo is the subset of stored-object metadata the platform records.
type ObjectInfo struct {
	Size        int64
	ContentType string
}

// UploadConstraints are the limits the STORAGE SERVER itself enforces on a
// presigned upload. They are part of the signature, so a client cannot raise
// them — an upload that breaks one is refused before any byte is stored.
type UploadConstraints struct {
	// MaxBytes is the largest body the server will accept.
	MaxBytes int64
	// ContentType must match exactly; the server rejects any other value.
	ContentType string
}

// PresignedUpload is a signed multipart/form-data POST. Fields must be sent as
// form values ahead of the file part, which must be named "file".
type PresignedUpload struct {
	URL    string
	Fields map[string]string
}

// Store is the port the content service depends on.
type Store interface {
	// PresignPost returns a signed form the client posts bytes to directly.
	//
	// This is a POST policy rather than a presigned PUT for one reason: a
	// presigned PUT carries no size or type condition, so the server accepts
	// whatever arrives and the only possible check is after the bytes have
	// already landed. A POST policy is signed WITH the constraints, so an
	// oversized or wrong-typed upload is rejected at the edge.
	PresignPost(ctx context.Context, key string, ttl time.Duration, c UploadConstraints) (PresignedUpload, error)
	// PresignGet returns a short-lived read URL. The caller is responsible for
	// having decided the object may be read at all.
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	// Stat confirms an object exists and reports what actually landed — never
	// trust the client's claim about size or content type.
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	Delete(ctx context.Context, key string) error
}

type Config struct {
	Endpoint  string // host[:port], no scheme
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	UseSSL    bool
}

type s3Store struct {
	client *minio.Client
	bucket string
}

// New builds a store against any S3-compatible endpoint.
func New(cfg Config) (Store, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("objectstore: endpoint and bucket are required")
	}
	c, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("objectstore: %w", err)
	}
	return &s3Store{client: c, bucket: cfg.Bucket}, nil
}

func (s *s3Store) PresignPost(ctx context.Context, key string, ttl time.Duration, c UploadConstraints) (PresignedUpload, error) {
	if c.MaxBytes <= 0 || c.ContentType == "" {
		// Refusing here rather than signing an unconstrained form: a policy with
		// no limits is exactly the hole this method exists to close.
		return PresignedUpload{}, errors.New("objectstore: upload constraints require a positive MaxBytes and a content type")
	}
	p := minio.NewPostPolicy()
	if err := p.SetBucket(s.bucket); err != nil {
		return PresignedUpload{}, fmt.Errorf("presign post: %w", err)
	}
	if err := p.SetKey(key); err != nil {
		return PresignedUpload{}, fmt.Errorf("presign post: %w", err)
	}
	if err := p.SetExpires(time.Now().UTC().Add(ttl)); err != nil {
		return PresignedUpload{}, fmt.Errorf("presign post: %w", err)
	}
	if err := p.SetContentType(c.ContentType); err != nil {
		return PresignedUpload{}, fmt.Errorf("presign post: %w", err)
	}
	// Minimum 1: a zero-byte object is never a legitimate asset, and allowing it
	// would let a caller occupy a key with nothing in it.
	if err := p.SetContentLengthRange(1, c.MaxBytes); err != nil {
		return PresignedUpload{}, fmt.Errorf("presign post: %w", err)
	}
	u, fields, err := s.client.PresignedPostPolicy(ctx, p)
	if err != nil {
		return PresignedUpload{}, fmt.Errorf("presign post: %w", err)
	}
	return PresignedUpload{URL: u.String(), Fields: fields}, nil
}

func (s *s3Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := s.client.PresignedGetObject(ctx, s.bucket, key, ttl, url.Values{})
	if err != nil {
		return "", fmt.Errorf("presign get: %w", err)
	}
	return u.String(), nil
}

func (s *s3Store) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).StatusCode == 404 {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, fmt.Errorf("stat: %w", err)
	}
	return ObjectInfo{Size: info.Size, ContentType: info.ContentType}, nil
}

func (s *s3Store) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}
