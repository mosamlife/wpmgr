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
	"net"
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
	// StreamStallTimeout overrides how long a LARGE-object transfer
	// (GetStreamViaPresign, PutViaPresign) may make no progress before it is
	// torn down. Zero, the normal setting, uses the StreamStallTimeout default.
	// It exists so a backend with unusual latency can be accommodated without
	// reintroducing a whole-request cap, and so tests can compress the window
	// instead of sleeping through it.
	StreamStallTimeout time.Duration
}

// Store is the S3-compatible object-store handle.
type Store struct {
	client     *s3.Client
	presigner  *s3.PresignClient
	bucket     string
	pathPrefix string
	// stallTimeout is the inter-byte bound applied to every transfer on
	// presignStreamClient. Never zero: New defaults it.
	stallTimeout time.Duration
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
	stall := cfg.StreamStallTimeout
	if stall <= 0 {
		stall = StreamStallTimeout
	}
	client := s3.New(s3.Options{}, opts...)
	return &Store{
		client:       client,
		presigner:    s3.NewPresignClient(client),
		bucket:       cfg.Bucket,
		pathPrefix:   strings.Trim(cfg.PathPrefix, "/"),
		stallTimeout: stall,
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

// streamTransport bounds the phases of a LARGE-object transfer that the control
// plane can actually predict (connect, TLS, and the wait for response headers)
// while deliberately leaving the BODY unbounded in wall-clock terms.
//
// It is shared by the streaming read (GetStreamViaPresign) and the presigned
// upload (PutViaPresign) because both carry a multi-megabyte body whose duration
// is set by the peer's throughput, not by anything knowable at dial time.
//
// ResponseHeaderTimeout is the piece that keeps this honest: on a read it caps
// how long storage may sit silent before the first byte, and on a write the Go
// transport measures it only AFTER the request body has been fully written, so a
// slow upload is never killed by it.
//
// NOTHING HERE BOUNDS THE BODY. ResponseHeaderTimeout is spent by the time the
// first byte arrives and does not apply mid-body, and the Dialer keepalive only
// notices a socket whose peer has vanished, not one that is alive and silent.
// That is what StreamStallTimeout and the stall guard in stall.go exist for, and
// why BOTH transfer methods on this client wrap their body in one: every
// transfer on presignStreamClient carries a progress bound.
var streamTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          100,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
}

// presignStreamClient carries LARGE objects to and from storage: the agent
// package the control plane streams to a site (GetStreamViaPresign) and CP-side
// uploads (PutViaPresign).
//
// It has NO whole-request Timeout, on purpose. http.Client.Timeout covers
// READING THE RESPONSE BODY as well as sending the request, so any fixed value
// turns "the consumer on the far end is slow" into "the storage transfer died
// mid-body", i.e. a truncated object rather than a slow one. On the download
// path that truncation reaches a WordPress site as a short zip it refuses to
// install; on the upload path it aborts a package the control plane had already
// spent the bandwidth on.
//
// What bounds a transfer instead, in order of what usually fires first: the
// PROGRESS WATCHDOG (StreamStallTimeout, see stall.go) which tears down any
// transfer that stops moving bytes; the CALLER'S CONTEXT (a disconnected
// consumer or a cancelled job cancels the transfer immediately); and
// streamTransport's dial/TLS/response-header timeouts. The watchdog is the one
// that does not depend on the caller getting anything right, which is why it is
// applied inside this package rather than left to call sites.
var presignStreamClient = &http.Client{Transport: streamTransport}

// presignStreamTTL is the presign window for a large-object transfer. S3
// evaluates a presigned URL's expiry when the request ARRIVES, not while the
// body is streaming, so this only has to cover the gap between minting the URL
// and issuing the request; the extra headroom over the small-object window is
// for a busy control plane, not for the transfer itself.
const presignStreamTTL = 5 * time.Minute

// PutViaPresign uploads an object by minting a short-lived presigned PUT URL and
// PUTting the bytes over plain HTTP, instead of a live SDK PutObject.
//
// aws-sdk-go-v2 signs a live PutObject in a way GCS's S3-compatible API rejects
// with "SignatureDoesNotMatch: Access denied" (the same reason GetViaPresign
// exists for reads), whereas presigned query-param SigV4 is accepted — which is
// why the agent's presigned chunk uploads work but a CP-side PutObject 403s. The
// presigned URL is a bearer credential — never log it.
//
// TIMEOUT POSTURE (H1b). This used to run on a client with a 60s whole-request
// Timeout, which covered the body: an object near the size cap needed a sustained
// upload rate the control plane cannot promise itself, and missing it aborted the
// write. It now runs on presignStreamClient, which has no whole-request cap.
//
// THAT MOVE CHANGED EVERY CALLER, so the replacement bound is applied HERE
// rather than left to them. Four callers (the release mirror, the screenshot
// worker, the report worker and the backup index writer) run under River jobs
// that carry a job Timeout, but the RUCSS ingest stash (internal/perf/rucss.go)
// runs on the agent's REQUEST path, where dropping the whole-request cap would
// have left the calling agent hanging up as the only bound. The progress
// watchdog below gives all six the same deadline: an upload that keeps moving is
// never cut off, and one that stops moving for StreamStallTimeout is torn down.
// Pass a cancellable ctx as well; the two bounds are independent.
func (s *Store) PutViaPresign(ctx context.Context, key string, body io.Reader, size int64) error {
	url, err := s.PresignPut(ctx, key, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("blobstore: presign put %q: %w", key, err)
	}

	// The derived context is what the watchdog cancels; cancelling it is the only
	// thing that unblocks a transport parked on a socket write to a silent peer.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	guard := NewStallGuard(cancel, s.stallTimeout)
	defer guard.Stop()

	req, err := http.NewRequestWithContext(streamCtx, http.MethodPut, url, body)
	if err != nil {
		return err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	guardRequestBody(req, guard)
	resp, err := presignStreamClient.Do(req)
	if err != nil && guard.Stalled() {
		return fmt.Errorf("blobstore: put %q: %w: no progress for %s", key, ErrStreamStalled, guard.Window())
	}
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
//
// This 15s cap covers reading the response body, so it is only ever correct for
// an object the control plane consumes IMMEDIATELY and in full (latest.json, a
// RUCSS bundle). Do NOT raise it to accommodate a large or slowly-consumed
// object: use GetStreamViaPresign, which is built for that. Callers that want a
// short, decisive bound depend on this staying short.
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

// GetStreamViaPresign downloads a LARGE object by minting a short-lived
// presigned GET URL and handing the caller the live response body to stream
// from. Returns ErrNotFound on a 404. The caller MUST close the returned
// ReadCloser. The presigned URL is a bearer credential and is never logged.
//
// Use this, not GetViaPresign, whenever the bytes are (a) large or (b) consumed
// at someone else's pace, e.g. proxied onward to a client. GetViaPresign's client
// has a 15s whole-request Timeout that covers reading the body, so a consumer
// slower than object_size/15s makes the STORAGE read fail mid-body with "context
// deadline exceeded while reading body". At a few MiB that floor lands around
// 200 KB/s, which plenty of shared hosting misses, and the caller has usually
// already committed a 200 and a Content-Length by then, so the failure surfaces
// as a silently truncated download rather than an error.
//
// This path has no whole-request timeout at all (see presignStreamClient). What
// bounds it instead is PROGRESS: the returned body is wrapped in a stall guard
// (stall.go) that tears the transfer down once StreamStallTimeout passes with no
// bytes moving, so a backend that sends headers and one chunk and then goes
// quiet cannot hold the caller open. Duration was never the right dimension; a
// large read is allowed to take as long as it needs while it is moving.
//
// Pass the consumer's request context too, so a disconnect tears the storage
// read down immediately rather than waiting out the stall window. The caller
// MUST Close the returned body: Close is what releases the derived context and
// the underlying connection.
func (s *Store) GetStreamViaPresign(ctx context.Context, key string) (io.ReadCloser, error) {
	url, err := s.PresignGet(ctx, key, presignStreamTTL)
	if err != nil {
		return nil, fmt.Errorf("blobstore: presign get %q: %w", key, err)
	}
	streamCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := presignStreamClient.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("blobstore: stream %q: %w", key, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		cancel()
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("blobstore: stream %q: unexpected status %d", key, resp.StatusCode)
	}
	return newGuardedBody(resp.Body, cancel, s.stallTimeout), nil
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
