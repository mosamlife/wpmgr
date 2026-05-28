// Package blobstore is a thin S3-compatible object-storage client over
// aws-sdk-go-v2 (ADR-010). It targets either managed AWS S3 or a self-hosted
// S3-compatible endpoint (SeaweedFS / MinIO) via a custom endpoint plus
// path-style addressing.
//
// WPMgr stores ONLY ciphertext here: backup chunks are age-encrypted client-
// side on the agent before they ever reach object storage, and the agent
// transfers bytes DIRECTLY to/from S3 using presigned PUT/GET URLs this package
// mints. The control plane proxies no chunk bytes and holds no decryption key.
//
// NEVER log a presigned URL: it is a bearer credential granting time-bounded
// access to the object.
package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Config configures the S3 client. Endpoint is the custom S3 endpoint (e.g.
// http://localhost:8333 for SeaweedFS); empty uses the AWS default resolver.
// ForcePathStyle must be true for SeaweedFS/MinIO (no virtual-host buckets).
type Config struct {
	Endpoint       string
	Region         string
	Bucket         string
	AccessKey      string
	SecretKey      string
	ForcePathStyle bool
}

// Store is the S3-compatible object-store handle.
type Store struct {
	client    *s3.Client
	presigner *s3.PresignClient
	bucket    string
}

// New builds a Store from static credentials and a (possibly custom) endpoint.
// It does not perform any network I/O; use EnsureBucket to create the bucket.
func New(cfg Config) (*Store, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("blobstore: bucket is required")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	opts := []func(*s3.Options){
		func(o *s3.Options) {
			o.Region = region
			o.Credentials = credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")
			o.UsePathStyle = cfg.ForcePathStyle
			if cfg.Endpoint != "" {
				o.BaseEndpoint = aws.String(cfg.Endpoint)
			}
		},
	}
	client := s3.New(s3.Options{}, opts...)
	return &Store{
		client:    client,
		presigner: s3.NewPresignClient(client),
		bucket:    cfg.Bucket,
	}, nil
}

// Bucket returns the configured bucket name.
func (s *Store) Bucket() string { return s.bucket }

// EnsureBucket creates the configured bucket if it does not already exist. Safe
// to call repeatedly (idempotent): an already-owned bucket is not an error.
func (s *Store) EnsureBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	if err == nil {
		return nil
	}
	_, cerr := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(s.bucket)})
	if cerr != nil && !isAlreadyOwned(cerr) {
		return fmt.Errorf("blobstore: create bucket %q: %w", s.bucket, cerr)
	}
	return nil
}

// Put uploads an object's bytes. Used in tests and for any CP-side writes; the
// production chunk-upload path uses presigned PUT URLs so the agent writes
// directly.
func (s *Store) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
	})
	if err != nil {
		return fmt.Errorf("blobstore: put %q: %w", key, err)
	}
	return nil
}

// Get downloads an object. The caller MUST close the returned ReadCloser.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("blobstore: get %q: %w", key, err)
	}
	return out.Body, nil
}

// Head reports an object's size and whether it exists. exists is false (with a
// nil error) when the object is absent.
func (s *Store) Head(ctx context.Context, key string) (exists bool, size int64, err error) {
	out, herr := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if herr != nil {
		if isNotFound(herr) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("blobstore: head %q: %w", key, herr)
	}
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return true, size, nil
}

// Delete removes an object. Deleting a missing key is not an error (S3 idempotent
// delete semantics).
func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("blobstore: delete %q: %w", key, err)
	}
	return nil
}

// List returns object keys under a prefix (paginated internally). Used by GC
// reconciliation and tests.
func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("blobstore: list %q: %w", prefix, err)
		}
		for _, o := range page.Contents {
			if o.Key != nil {
				keys = append(keys, *o.Key)
			}
		}
	}
	return keys, nil
}

// PresignPut mints a time-bounded presigned PUT URL for key so a client (the
// agent) can upload ciphertext bytes directly to storage. The returned URL is a
// bearer credential — never log it.
func (s *Store) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := s.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("blobstore: presign put %q: %w", key, err)
	}
	return req.URL, nil
}

// PresignGet mints a time-bounded presigned GET URL for key so a client (the
// agent) can download ciphertext bytes directly from storage. Bearer credential
// — never log it.
func (s *Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := s.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("blobstore: presign get %q: %w", key, err)
	}
	return req.URL, nil
}

// ErrNotFound is returned by Get when the key is absent.
var ErrNotFound = errors.New("blobstore: object not found")

func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}

func isAlreadyOwned(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
			return true
		}
	}
	return false
}
