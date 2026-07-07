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
	"log/slog"
	"net/http"
	"strings"
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
	// PathPrefix, when set, is applied to every object key this Store
	// touches (Put/Get/Head/Delete/List/PresignPut/PresignGet) — ADR-036 P1
	// (GH #146): an operator-configured s3_compat destination may set a key
	// prefix so multiple sites/tenants can share one customer bucket under
	// separate namespaces. Leading/trailing slashes are normalised; empty
	// (the default) means "bucket root", the pre-existing behaviour for the
	// CP-global Store.
	PathPrefix string
}

// Store is the S3-compatible object-store handle.
type Store struct {
	client     *s3.Client
	presigner  *s3.PresignClient
	bucket     string
	pathPrefix string
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
			// GCS S3-compat (and other non-AWS S3 backends) reject the flexible
			// request checksums that aws-sdk-go-v2 began adding by default
			// (RequestChecksumCalculationWhenSupported) — a live GetObject/PutObject
			// fails with "SignatureDoesNotMatch: Access denied" because the
			// x-amz-sdk-checksum-* / x-amz-checksum-* headers are not part of the
			// signature GCS computes. Presigned URLs were unaffected (no body
			// checksum), which is why backups worked but the manifest read 500'd.
			// Restrict checksums to when the operation genuinely requires them.
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		},
	}
	client := s3.New(s3.Options{}, opts...)
	return &Store{
		client:     client,
		presigner:  s3.NewPresignClient(client),
		bucket:     cfg.Bucket,
		pathPrefix: strings.Trim(cfg.PathPrefix, "/"),
	}, nil
}

// Bucket returns the configured bucket name.
func (s *Store) Bucket() string { return s.bucket }

// PathPrefix returns the configured (normalised, no leading/trailing slash)
// key prefix, or "" when this Store addresses the bucket root.
func (s *Store) PathPrefix() string { return s.pathPrefix }

// prefixedKey returns the key this Store actually addresses in the bucket:
// PathPrefix (if any) + key. EVERY S3 API call this Store makes MUST route
// its key through this method so a destination's configured prefix applies
// consistently across Put/Get/Head/Delete/List/PresignPut/PresignGet — a
// backup PUT and its matching restore GET always agree on the same
// effective key.
func (s *Store) prefixedKey(key string) string {
	if s.pathPrefix == "" {
		return key
	}
	return s.pathPrefix + "/" + strings.TrimPrefix(key, "/")
}

// unprefixedKey strips this Store's PathPrefix (if any) back off a key
// returned by the S3 API (List/ListWithModified), so callers always see the
// same logical/canonical key they would pass to Head/Get/Delete — regardless
// of whether this Store has a configured PathPrefix.
func (s *Store) unprefixedKey(key string) string {
	if s.pathPrefix == "" {
		return key
	}
	return strings.TrimPrefix(key, s.pathPrefix+"/")
}

// EnsureBucket attempts to create the configured bucket if it doesn't exist.
// Best-effort + non-fatal: an error here does NOT abort startup. The bucket
// existence is an operator concern, and a startup-time check is fragile across
// the many S3-compatible backends and proxies we may run behind:
//
//   - SeaweedFS behind a Cloudflare tunnel rewrites/buffers in a way that
//     breaks SigV4 for the CreateBucket payload (real-world failure mode
//     observed during ADR-033 live QA).
//   - Some hosted S3 providers (DO Spaces, Backblaze B2) only let bucket
//     creation happen via a separate API endpoint, not the data plane.
//   - IAM-restricted credentials may have only object-level perms, no
//     bucket-create grant; the bucket exists, the call fails 403.
//
// If the bucket genuinely doesn't exist, downstream operations (PutObject for
// CP writes, presigned PUTs from the agent) will fail loudly with NoSuchBucket
// — those failures are far more visible than a silent startup abort.
func (s *Store) EnsureBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	if err == nil {
		return nil
	}
	// HeadBucket failed — could be 404 (genuinely missing), 403 (no perms but
	// bucket exists), or a tunnel-induced SignatureDoesNotMatch. Try Create
	// anyway, but treat any failure as a warning, not fatal.
	_, cerr := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(s.bucket)})
	if cerr == nil || isAlreadyOwned(cerr) {
		return nil
	}
	slog.Warn("blobstore: EnsureBucket failed — assuming bucket exists and continuing",
		slog.String("bucket", s.bucket),
		slog.String("head_err", err.Error()),
		slog.String("create_err", cerr.Error()),
	)
	return nil
}

// Put uploads an object's bytes. CP-side writes route through a presigned PUT URL
// + plain HTTP (see PutViaPresign): a live SDK PutObject is rejected by GCS's
// S3-compatible API with "SignatureDoesNotMatch: Access denied", exactly like a
// live GetObject (see GetViaPresign) — the agent's presigned chunk PUTs are
// accepted, but a CP-side SDK PutObject 403s. This is why RUCSS's bundle/result
// writes (the only CP-side Puts on GCS) failed silently while presigned backups
// worked. presigned SigV4 is accepted everywhere we run (GCS, SeaweedFS, MinIO).
func (s *Store) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	return s.PutViaPresign(ctx, key, body, size)
}

// presignUploadClient bounds CP-side uploads of objects via presigned PUT URLs.
// Larger timeout than the small-object fetch client (RUCSS source bundles can be
// up to ~16 MiB of page HTML + CSS).
var presignUploadClient = &http.Client{Timeout: 60 * time.Second}

// PutViaPresign uploads an object by minting a short-lived presigned PUT URL and
// PUTting the bytes over plain HTTP, instead of a live SDK PutObject.
//
// aws-sdk-go-v2 signs a live PutObject in a way GCS's S3-compatible API rejects
// with "SignatureDoesNotMatch: Access denied" (the same reason GetViaPresign
// exists for reads), whereas presigned query-param SigV4 is accepted — which is
// why the agent's presigned chunk uploads work but a CP-side PutObject 403s. The
// presigned URL is a bearer credential — never log it.
func (s *Store) PutViaPresign(ctx context.Context, key string, body io.Reader, size int64) error {
	url, err := s.PresignPut(ctx, key, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("blobstore: presign put %q: %w", key, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	resp, err := presignUploadClient.Do(req)
	if err != nil {
		return fmt.Errorf("blobstore: put %q: %w", key, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		// S3 returns an XML error body; surface a bounded snippet for diagnostics
		// (never the URL — it is a bearer credential).
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("blobstore: put %q: unexpected status %d: %s", key, resp.StatusCode, string(snippet))
	}
	return nil
}

// Get downloads an object. The caller MUST close the returned ReadCloser.
//
// Routes through a presigned GET (see GetViaPresign): aws-sdk-go-v2's live
// GetObject is rejected by GCS's S3-compatible API with "SignatureDoesNotMatch:
// Access denied", exactly like live PutObject — which is why the RUCSS worker's
// source-bundle fetch (the only CP-side Get on GCS) 403'd while presigned reads
// worked. Presigned SigV4 is accepted on every backend we run (GCS, SeaweedFS,
// MinIO).
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.GetViaPresign(ctx, key)
}

// presignFetchClient bounds CP-side fetches of small objects via presigned URLs.
var presignFetchClient = &http.Client{Timeout: 15 * time.Second}

// GetViaPresign downloads a (small) object by minting a short-lived presigned
// GET URL and fetching it over plain HTTP, instead of a live SDK GetObject.
//
// aws-sdk-go-v2 signs a live GetObject in a way GCS's S3-compatible API rejects
// with "SignatureDoesNotMatch: Access denied", whereas presigned query-param
// SigV4 is accepted — which is exactly why the agent's presigned chunk
// downloads work but a CP-side GetObject 403s. For the rare CP-side read of a
// small control object (e.g. agent-releases/latest.json) this routes through the
// proven presigned path. Returns ErrNotFound on a 404. The caller MUST close the
// returned ReadCloser. The presigned URL is a bearer credential — never logged.
func (s *Store) GetViaPresign(ctx context.Context, key string) (io.ReadCloser, error) {
	url, err := s.PresignGet(ctx, key, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("blobstore: presign get %q: %w", key, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := presignFetchClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blobstore: fetch %q: %w", key, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("blobstore: fetch %q: unexpected status %d", key, resp.StatusCode)
	}
	return resp.Body, nil
}

// Head reports an object's size and whether it exists. exists is false (with a
// nil error) when the object is absent.
func (s *Store) Head(ctx context.Context, key string) (exists bool, size int64, err error) {
	out, herr := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.prefixedKey(key)),
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
//
// Routes through a presigned DELETE (plain HTTP): aws-sdk-go-v2's live
// DeleteObject is rejected by GCS's S3-compatible API with SignatureDoesNotMatch,
// exactly like Get/Put — which is why the RUCSS source-bundle cleanup logged a
// delete failure after an otherwise-successful compute. A 404/204 are both
// success (idempotent delete). Presigned SigV4 is accepted on every backend.
func (s *Store) Delete(ctx context.Context, key string) error {
	url, err := s.presigner.PresignDeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.prefixedKey(key)),
	}, s3.WithPresignExpires(60*time.Second))
	if err != nil {
		return fmt.Errorf("blobstore: presign delete %q: %w", key, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url.URL, nil)
	if err != nil {
		return err
	}
	resp, err := presignFetchClient.Do(req)
	if err != nil {
		return fmt.Errorf("blobstore: delete %q: %w", key, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// 204 No Content (deleted), 200 OK, and 404 Not Found are all success.
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("blobstore: delete %q: unexpected status %d", key, resp.StatusCode)
	}
	return nil
}

// List returns object keys under a prefix (paginated internally). Used by GC
// reconciliation and tests.
func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(s.prefixedKey(prefix)),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("blobstore: list %q: %w", prefix, err)
		}
		for _, o := range page.Contents {
			if o.Key != nil {
				keys = append(keys, s.unprefixedKey(*o.Key))
			}
		}
	}
	return keys, nil
}

// ObjectInfo is a listed object's key plus its server-recorded last-modified
// time. Used by age-based reapers (e.g. the RUCSS source-bundle backstop
// sweeper) that need to delete objects older than a TTL.
type ObjectInfo struct {
	Key          string
	LastModified time.Time
}

// ListWithModified returns objects under a prefix with their LastModified times
// (paginated internally). An object whose server time is unknown reports the
// zero time. Used by the RUCSS source-bundle backstop sweeper.
func (s *Store) ListWithModified(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(s.prefixedKey(prefix)),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("blobstore: list %q: %w", prefix, err)
		}
		for _, o := range page.Contents {
			if o.Key == nil {
				continue
			}
			info := ObjectInfo{Key: s.unprefixedKey(*o.Key)}
			if o.LastModified != nil {
				info.LastModified = *o.LastModified
			}
			out = append(out, info)
		}
	}
	return out, nil
}

// PresignPut mints a time-bounded presigned PUT URL for key so a client (the
// agent) can upload ciphertext bytes directly to storage. The returned URL is a
// bearer credential — never log it.
func (s *Store) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := s.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.prefixedKey(key)),
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
		Key:    aws.String(s.prefixedKey(key)),
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
