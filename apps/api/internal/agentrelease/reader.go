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

// defaultNegativeTTL bounds how long a FAILED read is allowed to stand before
// the next call retries. A failure is cheap to retry and expensive to believe
// (see Reader's doc comment), so it must never inherit the full success TTL:
// one transient presign or storage blip would otherwise pin "unknown" for five
// minutes across every replica that saw it.
const defaultNegativeTTL = 30 * time.Second

// maxLastKnownGoodAge bounds how OLD the last known good version may get before
// this Reader stops offering it in place of a live read.
//
// Serving the last good value across a blip is correct and must stay (see
// Reader): a rollup silently reclassified against something else is a plausible
// wrong answer, which is worse than a visibly stale one. Serving it forever is
// not correct. With storage broken permanently after a single success, an
// unbounded last known good reports that version as the "published" reference
// indefinitely, so a release landing during a sustained outage is never seen
// and the dashboard shows a confident all-clear attributed to a channel it can
// no longer read. Past this age the honest answer is that no reference can be
// determined right now.
//
// One hour is twelve times the 5-minute success TTL: far longer than any
// realistic presign blip, storage failover or rolling redeploy, and short
// enough that a stale reference cannot outlive a release.
const maxLastKnownGoodAge = time.Hour

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
// unreachable, malformed JSON) degrades to the LAST KNOWN GOOD version, or to
// "" (unknown) when this process has never read one; this feeds a read-only
// dashboard, and it must never turn into an error response or block boot.
//
// Three properties of the cache are load-bearing, all learned the hard way:
//
//   - A failure NEVER overwrites a good value. The published version is the
//     reference the whole fleet rollup is classified against, and a rollup
//     silently reclassified against something else is a plausible wrong answer,
//     which is worse than a visibly stale one. Stale but true beats derived.
//   - A failure is cached for a much shorter negative TTL than a success, so a
//     single blip cannot hold the degraded answer for the full success TTL.
//     Each API replica holds its own Reader, so a long-held failure also makes
//     the dashboard disagree with itself between replicas on refresh.
//   - That good value is nonetheless AGE BOUNDED (maxLastKnownGoodAge). Stale
//     but true stops being true at some point: a value read an hour ago is no
//     longer evidence about what is published now, and standing behind it
//     indefinitely turns a sustained outage into a confident all-clear
//     attributed to a channel this process can no longer read. Past the bound
//     LatestVersion returns "" and the caller reports that no reference can be
//     determined.
type Reader struct {
	store       Store
	ttl         time.Duration
	negativeTTL time.Duration
	// maxAge bounds how old cached may get before it stops being served at all
	// (see maxLastKnownGoodAge).
	maxAge time.Duration

	mu sync.Mutex
	// cached is the last SUCCESSFULLY read version, retained across failures.
	cached string
	// cachedAt is when cached was read, i.e. the age the maxAge bound applies
	// to. It advances only on a successful read, never on a failure.
	cachedAt time.Time
	// everPublished records whether a well-formed version has ever been read
	// by this process. It is the honest discriminator between "this install
	// has a release channel that is momentarily unreadable" and "this install
	// has no release channel at all", which is the self-hosted steady state
	// the fleet-derived fallback exists for (see Service.referenceVersion).
	everPublished bool
	haveFetched   bool
	lastFetchOK   bool
	fetchedAt     time.Time
}

// NewReader builds a cached Reader. store may be nil (object storage not
// configured); LatestVersion then always returns "". ttl<=0 uses a 5-minute
// default, and failures are cached for defaultNegativeTTL (capped at ttl, so a
// deliberately short ttl stays short).
func NewReader(store Store, ttl time.Duration) *Reader {
	return NewReaderWithLimits(store, ttl, defaultNegativeTTL, maxLastKnownGoodAge)
}

// NewReaderWithNegativeTTL builds a cached Reader with an explicit negative
// TTL: how long a failed read is cached before the next call retries. Both
// non-positive values take their defaults, and negativeTTL is capped at ttl (a
// failure is never cached longer than a success). The last known good version
// is bounded at the default maxLastKnownGoodAge.
func NewReaderWithNegativeTTL(store Store, ttl, negativeTTL time.Duration) *Reader {
	return NewReaderWithLimits(store, ttl, negativeTTL, maxLastKnownGoodAge)
}

// NewReaderWithLimits builds a cached Reader with all three bounds stated
// explicitly:
//
//   - ttl: how long a SUCCESSFUL read is served before it is read again.
//   - negativeTTL: how long a FAILED read is held before the next call retries.
//   - maxLastKnownGood: how old the last successfully read version may get
//     before the Reader stops serving it and reports unknown instead.
//
// Non-positive values take their defaults. negativeTTL is capped at ttl (a
// failure is never cached longer than a success), and ttl is capped at
// maxLastKnownGood, so no value is ever served past the age bound whatever the
// TTL says.
func NewReaderWithLimits(store Store, ttl, negativeTTL, maxLastKnownGood time.Duration) *Reader {
	if maxLastKnownGood <= 0 {
		maxLastKnownGood = maxLastKnownGoodAge
	}
	if ttl <= 0 {
		ttl = defaultTTL
	}
	if ttl > maxLastKnownGood {
		ttl = maxLastKnownGood
	}
	if negativeTTL <= 0 {
		negativeTTL = defaultNegativeTTL
	}
	if negativeTTL > ttl {
		negativeTTL = ttl
	}
	return &Reader{store: store, ttl: ttl, negativeTTL: negativeTTL, maxAge: maxLastKnownGood}
}

// LatestVersion returns the currently published agent version, or "" when it
// cannot be determined right now. A read that fails after an earlier success
// returns that earlier version rather than "", up to maxLastKnownGoodAge; past
// that bound it returns "" so a sustained outage is reported as unknown instead
// of as a stale published claim. Never returns an error.
func (r *Reader) LatestVersion(ctx context.Context) string {
	r.mu.Lock()
	if r.haveFetched && time.Since(r.fetchedAt) < r.currentTTLLocked() {
		v := r.lastKnownGoodLocked()
		r.mu.Unlock()
		return v
	}
	r.mu.Unlock()

	v := r.fetch(ctx)
	// A fetch counts as successful only when it yields a version this package
	// is willing to order. A manifest that reads but carries garbage is no
	// more usable than an absent one, and must not be promoted to last known
	// good nor recorded as proof that a release channel exists here.
	ok := isWellFormedVersion(v)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.haveFetched = true
	r.lastFetchOK = ok
	r.fetchedAt = time.Now()
	if ok {
		r.cached = strings.TrimSpace(v)
		r.cachedAt = r.fetchedAt
		r.everPublished = true
	}
	// On failure r.cached is deliberately left untouched: the last known good
	// version survives the blip, and stays "" when there has never been one.
	// It is served only while still inside the age bound.
	return r.lastKnownGoodLocked()
}

// lastKnownGoodLocked returns the last successfully read version while it is
// still young enough to stand in for a live read, and "" once it is not (or
// when there has never been one). Caller must hold r.mu.
//
// Deliberately does NOT clear r.cached or r.everPublished. A bounded-out value
// is withheld, not forgotten: the next successful read refreshes it in place,
// and the proof that this install HAS a release channel is what keeps the fleet
// rollup on "none" instead of falling through to a fleet-derived reference
// (see Service.referenceVersion).
func (r *Reader) lastKnownGoodLocked() string {
	if r.cached == "" || time.Since(r.cachedAt) >= r.maxAge {
		return ""
	}
	return r.cached
}

// EverPublished reports whether this Reader has ever successfully read a
// well-formed published version. False is the self-hosted steady state (no
// release pipeline ever writes agent-releases/latest.json into that install's
// own bucket); it is deliberately NOT the same fact as "the last read failed".
func (r *Reader) EverPublished() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.everPublished
}

// currentTTLLocked returns the TTL that applies to the entry currently held:
// the full ttl after a successful read, the much shorter negativeTTL after a
// failed one. Caller must hold r.mu.
func (r *Reader) currentTTLLocked() time.Duration {
	if r.lastFetchOK {
		return r.ttl
	}
	return r.negativeTTL
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
