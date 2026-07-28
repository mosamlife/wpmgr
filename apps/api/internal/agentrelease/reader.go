// Package agentrelease provides ADDITIVE, read-only visibility into the
// currently published WPMgr agent release version, and classifies each
// site's reported agent_version against it for the fleet "agent freshness"
// dashboard (GET /api/v1/agent/latest, GET /api/v1/fleet/agents).
//
// This package never writes to object storage and never touches the
// self-update path (internal/agent/update_handler.go, ADR-042): it only
// reads the SAME published pointer manifest (agent-releases/latest.json)
// that handler already serves to agents, through the same proven presigned
// read path (see blobstore.Store.GetViaPresign: a live SDK GetObject 403s
// against GCS's S3-compatible API; the presigned-URL path is what works on
// every backend WPMgr runs against).
package agentrelease

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"
)

// ManifestKey is the well-known object key the release pipeline (make
// agent-release) writes the pointer manifest to. Mirrors
// internal/agent/update_handler.go's updateManifestKey; both packages read
// the exact same published object, deliberately duplicated here rather than
// imported so this read-only visibility package carries no dependency on the
// agent-authenticated update-manifest handler or its signing path.
const ManifestKey = "agent-releases/latest.json"

// maxManifestBytes bounds how much of latest.json is read (it is tiny).
const maxManifestBytes = 64 << 10

// defaultTTL is used when NewReader is given a non-positive ttl.
const defaultTTL = 5 * time.Minute

// Store is the narrow object-storage surface this package needs: a read via
// presigned GET. Satisfied by *blobstore.Store.
type Store interface {
	GetViaPresign(ctx context.Context, key string) (io.ReadCloser, error)
}

// manifest is the subset of agent-releases/latest.json this package reads.
type manifest struct {
	Version string `json:"version"`
}

// Reader is a cached, best-effort reader of the currently published agent
// release version. A read failure (no store wired, object missing, storage
// unreachable, malformed JSON) ALWAYS degrades to "" (unknown); this feeds a
// read-only dashboard, and it must never turn into an error response or
// block boot.
type Reader struct {
	store Store
	ttl   time.Duration

	mu          sync.Mutex
	cached      string
	haveFetched bool
	fetchedAt   time.Time
}

// NewReader builds a cached Reader. store may be nil (object storage not
// configured); LatestVersion then always returns "". ttl<=0 uses a 5-minute
// default.
func NewReader(store Store, ttl time.Duration) *Reader {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &Reader{store: store, ttl: ttl}
}

// LatestVersion returns the currently published agent version, or "" when it
// cannot be determined right now. Never returns an error.
func (r *Reader) LatestVersion(ctx context.Context) string {
	r.mu.Lock()
	if r.haveFetched && time.Since(r.fetchedAt) < r.ttl {
		v := r.cached
		r.mu.Unlock()
		return v
	}
	r.mu.Unlock()

	v := r.fetch(ctx)

	r.mu.Lock()
	r.cached = v
	r.haveFetched = true
	r.fetchedAt = time.Now()
	r.mu.Unlock()
	return v
}

// fetch performs the actual read. Any failure (store unwired, object missing:
// blobstore.ErrNotFound when no release has ever been published, a
// transient storage error, malformed JSON) degrades to "" rather than
// propagating an error, matching this package's read-only, never-blocks
// contract.
func (r *Reader) fetch(ctx context.Context) string {
	if r.store == nil {
		return ""
	}
	rc, err := r.store.GetViaPresign(ctx, ManifestKey)
	if err != nil {
		return ""
	}
	defer func() { _ = rc.Close() }()

	body, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes))
	if err != nil {
		return ""
	}
	var m manifest
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	return strings.TrimSpace(m.Version)
}
