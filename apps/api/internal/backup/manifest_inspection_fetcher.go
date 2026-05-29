package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// ErrNoInspectionEntry is the sentinel returned by ManifestInspectionFetcherAdapter.Fetch
// when the manifest carries no entry classified as the sql-inspection artifact.
// Surfaces the "old snapshot" path — a snapshot taken by a pre-inspection-aware
// agent that never emitted a `sql-inspection.json` manifest entry.
//
// The sqlinspect handler currently locates the entry itself (via
// findInspectionEntry) before invoking Fetch, so today this sentinel is mostly
// belt-and-braces: it fires only when the entry was found at handler time but
// has since disappeared (a deletion racing with the GET). Defining it here
// keeps the adapter's contract honest and lets a future handler rev map it to
// 404 with `code: "no_inspection_artifact"` without re-discovering the case.
var ErrNoInspectionEntry = errors.New("backup: snapshot has no sql-inspection manifest entry")

// inspectionChunkStore mints a presigned GET URL for an object-store key. The
// adapter then fetches via plain HTTP — sidesteps the aws-sdk-go-v2 direct
// GetObject path which fails SigV4 verification on GCS S3-interop when the
// configured region is "auto" (presigned URLs are lenient about the region
// scope, direct SDK calls are not — they recompute SigV4 live and GCS rejects
// `auto` as a credential-scope region). Satisfied by *blobstore.Store.PresignGet.
//
// History: this interface used to expose `Get(ctx, key)` which dispatched the
// SDK's direct GetObject. That worked on local SeaweedFS in dev but 403'd in
// prod on GCS for every snapshot taken since the inspection feature shipped,
// surfacing as "503 inspection_unwired" in the operator-facing restore dialog.
// Fixed 2026-05-29 by routing through the same presigned-URL minting path the
// agent already uses for chunk PUT and the restore engine uses for chunk GET.
type inspectionChunkStore interface {
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// inspectionChunkTTL is the lifetime of the presigned GET URL minted per chunk.
// Short enough that a leaked URL is useless within a meaningful window; long
// enough that a slow Cloud SQL read or a brief throttling event doesn't blow
// the URL before we finish streaming it.
const inspectionChunkTTL = 60 * time.Second

// inspectionChunkResolver looks up the stored Chunks for a set of BLAKE3 hashes
// in a tenant — returns each chunk's s3 key (so the adapter can fetch via the
// content-addressed, tenant-namespaced path). Satisfied by backup.Repo.
type inspectionChunkResolver interface {
	ExistingChunkHashes(ctx context.Context, tenantID uuid.UUID, hashes []string) (map[string]Chunk, error)
}

// ManifestInspectionFetcherAdapter is the production implementation of the
// ManifestInspectionFetcher interface declared in sqlinspect_handler.go.
//
// It streams the ordered ciphertext chunks of an agent-supplied inspection
// manifest entry from object storage, concatenates them in manifest order,
// and returns the resulting bytes.
//
// V0 NOTE: agents that pre-date V1-SaaS ship the inspection artifact as
// PLAINTEXT chunks (ENCRYPT_CHUNKS=false in class-encrypt-and-upload.php).
// The CP fetches the bytes verbatim — no age decryption is performed here
// because the CP holds no agent identity. When V1-SaaS flips ENCRYPT_CHUNKS
// to true, this adapter will need a decryption hook; today the JSON validates
// directly so an accidentally-encrypted artifact surfaces as a parse error
// (the handler then falls through to the cache/enqueue tiers).
type ManifestInspectionFetcherAdapter struct {
	store    inspectionChunkStore
	resolver inspectionChunkResolver
}

// NewManifestInspectionFetcher builds the adapter. `store` is the blobstore
// the agent uploaded ciphertext chunks to (typically *blobstore.Store); `repo`
// is the backup repo used to resolve chunk hashes to their tenant-namespaced
// s3 keys.
func NewManifestInspectionFetcher(store inspectionChunkStore, repo inspectionChunkResolver) *ManifestInspectionFetcherAdapter {
	return &ManifestInspectionFetcherAdapter{store: store, resolver: repo}
}

// Fetch implements ManifestInspectionFetcher. Given a snapshot + the manifest
// entry the handler located, it:
//
//  1. Confirms the entry carries at least one chunk hash (defense in depth —
//     a zero-chunk inspection entry is malformed).
//  2. Resolves the chunk hashes to s3 keys via the tenant-scoped repo. This
//     also serves as the tenancy check: a hash that does not belong to this
//     tenant is invisible (RLS + the WHERE filter) and the adapter refuses
//     to fetch unknown chunks.
//  3. Streams each chunk from the blobstore in manifest order and writes it
//     to an in-memory buffer (the inspection report is bounded — typically
//     tens of KB, max a few hundred KB even for very wide schemas).
//  4. Validates the concatenated bytes parse as JSON (a generic
//     map[string]any; the handler does the typed Report decode).
//
// Returns ErrNoInspectionEntry if the manifest entry has no chunks — the
// "old snapshot" sentinel (today only reachable on a delete-race, since the
// handler's findInspectionEntry guards entry presence).
func (f *ManifestInspectionFetcherAdapter) Fetch(ctx context.Context, tenantID uuid.UUID, snap Snapshot, entry ManifestEntry) ([]byte, error) {
	if f == nil || f.store == nil || f.resolver == nil {
		return nil, fmt.Errorf("manifest inspection fetcher: not configured")
	}
	if len(entry.ChunkHashes) == 0 {
		return nil, ErrNoInspectionEntry
	}

	chunks, err := f.resolver.ExistingChunkHashes(ctx, tenantID, entry.ChunkHashes)
	if err != nil {
		return nil, fmt.Errorf("manifest inspection fetcher: resolve chunks: %w", err)
	}

	var buf bytes.Buffer
	for _, hash := range entry.ChunkHashes {
		c, ok := chunks[hash]
		if !ok {
			return nil, fmt.Errorf("manifest inspection fetcher: chunk %s not stored for tenant %s", hash, tenantID)
		}
		if err := f.appendChunk(ctx, &buf, c.S3Key); err != nil {
			return nil, fmt.Errorf("manifest inspection fetcher: fetch chunk %s: %w", hash, err)
		}
	}

	raw := buf.Bytes()
	// Lightweight content check — the agent ships JSON; if the bytes don't
	// parse, surfacing it here (rather than letting the handler's typed
	// decode fail) gives the operator a clearer error code.
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("manifest inspection fetcher: artifact is not valid JSON: %w", err)
	}
	return raw, nil
}

// appendChunk streams one chunk from object storage into buf. Mints a short-
// lived presigned GET URL then fetches via plain HTTP — the same path that
// works for agent PUT and restore-engine GET. Direct SDK GetObject was tried
// originally but fails on GCS S3-interop with region=auto (the SDK recomputes
// SigV4 live and GCS rejects `auto` as a credential-scope region).
func (f *ManifestInspectionFetcherAdapter) appendChunk(ctx context.Context, buf *bytes.Buffer, key string) error {
	url, err := f.store.PresignGet(ctx, key, inspectionChunkTTL)
	if err != nil {
		return fmt.Errorf("presign inspection chunk: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build inspection chunk request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch inspection chunk: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch inspection chunk %q: status %d", key, resp.StatusCode)
	}
	if _, err := io.Copy(buf, resp.Body); err != nil {
		return fmt.Errorf("read inspection chunk body: %w", err)
	}
	return nil
}
