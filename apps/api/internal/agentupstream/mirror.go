// Package agentupstream MIRRORS the public WPMgr agent release from GitHub
// into THIS install's own object storage (GH #302).
//
// The problem it solves: the release pipeline (scripts/release-agent.sh) writes
// agent-releases/<version>/wpmgr-agent.zip + agent-releases/latest.json into the
// HOSTED service's bucket. A self-hosted control plane has its own storage and
// nothing ever writes there, so its dashboard has no published reference version
// and its sites are never offered an agent upgrade. The documented workaround was
// to build the zip by hand, upload it, and add a package-host override to EVERY
// site's wp-config.php.
//
// The fix is to MIRROR, not to PROXY. This package reads our public GitHub
// release, verifies it end to end, downloads the asset ONCE, and writes the same
// two objects the release pipeline writes into the operator's OWN bucket. From
// that moment every existing path is unchanged and unaware: the signed-manifest
// mint path (internal/agent/update_handler.go), the freshness reader
// (internal/agentrelease), the reference version, and the task planner all keep
// reading exactly the objects they already read, published into that install's
// storage by that install. Nothing in this package is on a request path.
//
// Why not point the signed manifest at a GitHub URL instead: GitHub's asset host
// has already changed once, the redirect target is a short-lived signed URL, and
// it would put a third-party hostname into the agent's permanent trust list and
// require changing the hardened mint path. Mirroring avoids all of that and keeps
// working while GitHub is down.
//
// Security posture, since this is the one job that fetches from the public
// internet and writes a binary into the operator's storage:
//
//   - Every outbound request goes through the shared SSRF-hardened client
//     (internal/httpclient), never http.DefaultClient.
//   - NO URL is ever taken from fetched JSON. The API URL and the asset download
//     URL are both CONSTRUCTED from the configured owner/repo plus the release
//     tag plus a fixed asset name, and every path segment is shape-validated
//     first. The release JSON's own browser_download_url/url fields are not even
//     decoded.
//   - The manifest is validated with the SAME rules the agent-facing reader
//     applies (see validateManifest), including pinning the package object key to
//     the single deterministic form for its version, and shape-checking that
//     version before it is ever built into an object key.
//   - BOTH release assets are digest-verified against the GitHub API's per-asset
//     digest and size, the manifest included. The API response and the asset
//     bytes are independent attestations, and agent-release.json is the document
//     every other check here derives from, so it is verified exactly the way the
//     zip is.
//   - The manifest's sha256 and size are CROSS-CHECKED against the GitHub API's
//     per-asset digest and size before anything is downloaded. A mismatch means
//     the asset was replaced after publish or the upload was partial: this
//     refuses, logs loudly, and leaves the previous mirror exactly where it is.
//   - Publishing is one-way and provenance-checked. The upstream version must be
//     STRICTLY NEWER than what is already mirrored, and the pointer being
//     replaced must be one this mirror wrote (see mayReplace). An install that
//     publishes its own agent builds is never taken over.
//   - The download is streamed under a hard size cap with sha256 computed as it
//     goes, and the computed hash must equal BOTH the manifest's and the API's
//     digest before a single byte reaches storage.
//   - Write order is load-bearing and matches scripts/release-agent.sh: the
//     versioned package object goes up FIRST, the pointer manifest LAST, so
//     latest.json never names an object that is not yet in place.
//
// Failure of any of this degrades to "no new release mirrored". It never breaks
// boot, never touches a request path, and never surfaces as a 500: the existing
// source ladder (own-bucket manifest, then newest-in-fleet, then none) continues
// to apply unchanged.
package agentupstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentplugin"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentrelease"
	"github.com/mosamlife/wpmgr/apps/api/internal/blobstore"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// ManifestKey is the pointer object this mirror writes LAST. It is the exact key
// internal/agent/update_handler.go (updateManifestKey) and internal/agentrelease
// (ManifestKey) already read. Deliberately duplicated here rather than imported,
// for the same reason agentrelease duplicates it: the whole value of mirroring is
// that those packages stay untouched and unaware, so this one must not reach into
// them.
const ManifestKey = "agent-releases/latest.json"

// packagePrefix bounds the object keys this mirror will write. Mirrors
// update_handler.go's updatePackagePrefix.
const packagePrefix = "agent-releases/"

// expectedSlug pins the plugin this channel carries. A release manifest naming
// any other slug is refused. Mirrors update_handler.go's expectedAgentSlug; the
// slug itself lives in agentplugin, the one place the agent's plugin identity is
// defined.
const expectedSlug = agentplugin.SlugSelfHosted

// The two fixed asset names .github/workflows/release.yml publishes on every
// release. They are CONSTANTS, never read from the release JSON: the download URL
// is built from these, and the API response is consulted only for each asset's
// size and digest.
const (
	manifestAssetName = "agent-release.json"
	packageAssetName  = expectedSlug + ".zip"
)

// apiHost / downloadHost are the two hosts this package will talk to, written out
// here so the set is greppable and cannot be widened by a fetched payload.
//
// Note that the download REDIRECTS (once) to GitHub's asset host, which has
// already changed name once and serves from a short-lived signed URL. That
// redirect is deliberately followed without pinning the final host: the sha256 +
// size verification below is the real control over what is downloaded, and
// pinning a host GitHub owns and renames would only make this break silently the
// next time they rename it.
const (
	apiHost      = "api.github.com"
	downloadHost = "github.com"
)

// userAgent identifies this job to GitHub. The API rejects requests without one.
const userAgent = "wpmgr-agent-release-mirror"

// provenanceField / provenanceMarker are the PROVENANCE STAMP this mirror adds
// to every pointer manifest it publishes, and the only evidence it has that a
// latest.json already in the bucket is one of its own.
//
// It exists because the mirror writes the SAME key the release pipeline writes
// (scripts/release-agent.sh, make agent-release). An operator who publishes
// their own agent build owns agent-releases/latest.json, and a mirror that
// overwrote it would take over their release channel on the next tick and undo
// every build they publish afterwards. Refusing anything not strictly newer is
// not enough on its own: an operator build OLDER than upstream would still be
// replaced. So the rule is provenance, not ordering: if the pointer in place
// does not carry this marker, this mirror did not write it, and it stands down
// (see mayReplace).
//
// The marker is an ADDITIVE field on a document that is otherwise passed
// through untouched. Every consumer of latest.json decodes it into a struct
// with the fields it cares about and no DisallowUnknownFields anywhere
// (internal/agent's readLatest, internal/agentrelease's Reader, and this
// package's own validateManifest), so an extra key is inert to all three.
const (
	provenanceField  = "mirrored_by"
	provenanceMarker = "wpmgr-agent-release-mirror"

	// provenanceSourceField records WHICH upstream release the mirrored bytes
	// came from, purely so an operator looking at latest.json can tell. Nothing
	// reads it.
	provenanceSourceField = "mirrored_from"
)

// maxAPIResponseBytes caps the release JSON read. The document is a few KB; 1 MiB
// is a generous ceiling that still refuses an unbounded body.
const maxAPIResponseBytes = 1 << 20

// maxManifestBytes caps the agent-release.json read (it is tiny). Same bound
// update_handler.go applies to latest.json.
const maxManifestBytes = 64 << 10

// MaxPackageBytes is the hard ceiling on the agent zip. The real asset is a
// couple of MB; a manifest declaring more than this is refused BEFORE any
// download starts, which is what keeps the buffered read below bounded.
const MaxPackageBytes = 32 << 20

// minRequestSpacing is the minimum wall-clock gap between two ACTUAL GitHub API
// requests from this process, whatever the job cadence. The unauthenticated API
// allows 60 requests/hour per IP; the schedule (every 6 hours) is nowhere near
// that, so this guard exists only to stop a duplicate or manually re-triggered
// run from spending budget for no new information.
const minRequestSpacing = 30 * time.Minute

// Sentinel errors. Callers (and the worker) distinguish a REFUSAL, which means
// the upstream release failed verification and the previous mirror was
// deliberately left in place, from a degradation, which means this run simply
// learned nothing.
var (
	// ErrNotConfigured: object storage or the HTTP client is not wired, or the
	// configured owner/repo is not a usable path segment.
	ErrNotConfigured = errors.New("agentupstream: not configured")
	// ErrUpstreamUnavailable: GitHub could not be reached or answered with a
	// status this job cannot use. Nothing is wrong with the mirror.
	ErrUpstreamUnavailable = errors.New("agentupstream: upstream unavailable")
	// ErrRateLimited: GitHub applied its rate limit, or this process asked too
	// recently (minRequestSpacing). Quiet, expected, never an alarm.
	ErrRateLimited = errors.New("agentupstream: rate limited")
	// ErrRefused: the upstream release FAILED verification. This is the loud
	// one. The previous mirror is untouched.
	ErrRefused = errors.New("agentupstream: refused upstream release")
	// ErrForeignChannel: agent-releases/latest.json exists and was NOT written
	// by this mirror, so this install publishes its own agent releases and the
	// mirror stands down. Always wrapped together with ErrRefused; it exists so
	// the worker can say that plainly instead of reporting a failed
	// verification, because nothing is wrong with the upstream release.
	ErrForeignChannel = errors.New("agentupstream: latest.json was not published by this mirror")
	// ErrDowngrade: the upstream release is not strictly newer than what is
	// already mirrored. Also always wrapped with ErrRefused. Publishing it would
	// walk this install's published version BACKWARDS, which agents refuse to
	// install (their own downgrade guard) while the dashboard would report the
	// older number as the reference every site is measured against.
	ErrDowngrade = errors.New("agentupstream: upstream release is not newer than the mirrored one")
	// ErrUnorderable: the mirrored version, the upstream version, or both are
	// not a shape this package can order, so this run cannot PROVE upstream is
	// newer. Also always wrapped with ErrRefused, and for the same reason as
	// ErrDowngrade: "cannot compare" is not evidence of "safe to publish", and
	// treating it as such let any version string the comparator could not parse
	// walk straight past the one rule that stops the channel moving backwards.
	ErrUnorderable = errors.New("agentupstream: the mirrored and upstream versions cannot be ordered")
	// ErrPointerUnreadable: the currently published pointer could not be read,
	// and the failure was NOT "no pointer has ever been published". This run
	// therefore cannot tell whether it would be replacing its own pointer or an
	// operator's, so it writes nothing and waits for the next one. A
	// degradation, not a refusal.
	ErrPointerUnreadable = errors.New("agentupstream: cannot read the currently published pointer")
)

// pathSegmentPattern is the shape every value this package interpolates into a
// URL path must match: owner, repo, and the release tag. It admits no "/", no
// "%", and no whitespace, so a hostile tag cannot escape its path segment or
// redirect the fetch at another repository. Length is bounded well past anything
// GitHub itself accepts.
var pathSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,99}$`)

// isPathSegment reports whether s is safe to interpolate into a URL path.
// The explicit ".." rejection is belt-and-braces: the pattern already forbids
// "/", so ".." cannot traverse, but a path segment that is only dots is never
// legitimate here and is cheaper to refuse than to reason about.
func isPathSegment(s string) bool {
	if !pathSegmentPattern.MatchString(s) {
		return false
	}
	return !strings.Contains(s, "..")
}

// Store is the narrow object-storage surface this package needs. Satisfied by
// *blobstore.Store.
//
// Both reads and writes go through the presigned paths (Get/Put are already
// presign-backed in blobstore) because a live SDK GetObject/PutObject is rejected
// by GCS's S3-compatible API.
type Store interface {
	GetViaPresign(ctx context.Context, key string) (io.ReadCloser, error)
	Put(ctx context.Context, key string, body io.Reader, size int64) error
}

// HTTPDoer is the subset of *httpclient.Client this package uses. Mirrors
// vuln.FeedHTTPDoer. It MUST be an SSRF-hardened client.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// githubAsset is the per-asset subset of the releases API response this package
// consumes: a name, a size, and the sha256 digest GitHub now publishes.
//
// The asset's own url/browser_download_url fields are deliberately NOT decoded.
// Nothing this package fetches is ever addressed by a URL that came out of this
// document.
type githubAsset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// Digest is "sha256:<64 lowercase hex>". Absent on releases published before
	// GitHub added the field, which this job treats as unverifiable and refuses.
	Digest string `json:"digest"`
}

// githubRelease is the subset of GET /repos/{owner}/{repo}/releases/latest this
// package consumes.
//
// draft/prerelease are deliberately absent: /releases/latest excludes both BY
// CONTRACT, so hand-filtering them here would only encode a second, weaker copy
// of a rule the endpoint already enforces.
type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

// manifest is the subset of agent-release.json this package validates.
//
// The document is passed through with its values untouched: it is round-tripped
// as raw JSON (see markMirrored), never re-typed, so the fields this struct does
// not name (min_version, requires, requires_php, tested, sections) survive the
// mirror exactly as upstream wrote them. That matters: min_version in particular
// is a real control, and a mirror that re-serialised only the fields it
// understood would silently drop it. The ONE difference between the bytes
// upstream published and the bytes stored here is the provenance stamp this
// mirror adds (provenanceField).
type manifest struct {
	Slug             string `json:"slug"`
	Version          string `json:"version"`
	PackageObjectKey string `json:"package_object_key"`
	PackageSHA256    string `json:"package_sha256"`
	PackageSize      int64  `json:"package_size"`
	// MirroredBy carries the provenance stamp when this manifest was published
	// by a mirror. Empty on anything upstream published and on anything an
	// operator published themselves.
	MirroredBy string `json:"mirrored_by"`
}

// mirrorWrote reports whether this manifest carries this mirror's provenance
// stamp, i.e. whether the pointer currently in place is one of ours to replace.
func (m manifest) mirrorWrote() bool {
	return strings.TrimSpace(m.MirroredBy) == provenanceMarker
}

// pointerFingerprint is the LOCAL half of the publish decision reduced to a
// comparable value: whether a pointer is published into this install's storage
// at all, the version and package digest it names, and whether this mirror
// wrote it.
//
// Those four facts are the COMPLETE local input to the decision, which is what
// makes "the fingerprint is unchanged" a sound reason to reuse a previous run's
// conclusion. Nothing else about the pointer is consulted: the already-current
// check reads the version and the digest, mayReplace reads the version, the
// digest and the provenance stamp, and absence skips both. If a future rule
// starts reading a fifth fact off the pointer, it belongs here too, or the
// conditional-request short-circuit below will start hiding it.
type pointerFingerprint struct {
	present  bool
	version  string
	sha256   string
	mirrored bool
}

// fingerprintOf reduces a pointer read (and whether one was there at all) to the
// value the ETag is banked against.
func fingerprintOf(cur manifest, present bool) pointerFingerprint {
	if !present {
		return pointerFingerprint{}
	}
	return pointerFingerprint{
		present:  true,
		version:  cur.Version,
		sha256:   cur.PackageSHA256,
		mirrored: cur.mirrorWrote(),
	}
}

// publishedFingerprint is the state a pointer this run just WROTE will be read
// back in. It is constructed rather than re-read because it is exactly known:
// markMirrored passes the version and digest through untouched and stamps
// provenance, so the next run reads back this version, this digest, and this
// mirror's marker. Getting it wrong would not be unsafe, only wasteful: the next
// run would see a fingerprint mismatch and re-fetch a full body it did not need.
func publishedFingerprint(man manifest) pointerFingerprint {
	return pointerFingerprint{
		present:  true,
		version:  man.Version,
		sha256:   man.PackageSHA256,
		mirrored: true,
	}
}

// Result reports what a single Run did. Purely informational; the worker logs it.
type Result struct {
	// Mirrored is true when this run actually wrote a new package + manifest.
	Mirrored bool
	// Version is the agent version examined, when one was determined.
	Version string
	// Reason is a short machine-ish token describing a no-op run
	// ("not_modified", "already_current", "too_soon"), empty when Mirrored.
	Reason string
}

// Mirror fetches, verifies and mirrors the upstream agent release. Safe for
// concurrent use; in practice the job runs with MaxWorkers=1.
type Mirror struct {
	store  Store
	client HTTPDoer
	owner  string
	repo   string
	logger *slog.Logger
	// allowRollback is the deliberate escape hatch for the strictly-newer rule
	// (see mayReplace). Off by default.
	allowRollback bool

	mu sync.Mutex
	// etag is the last release-JSON ETag seen, replayed as If-None-Match so a
	// run that learns nothing costs GitHub almost nothing and (on a 304) does
	// not count against the unauthenticated rate limit's body budget.
	etag string
	// etagPointer is the pointer state that ETag was banked against. The ETag is
	// replayed ONLY while the pointer still looks like this, because a 304
	// answers "upstream has not changed" and the publish decision also depends
	// on local state that moves independently of upstream (see Run step 2).
	etagPointer pointerFingerprint
	// lastRequestAt is when an actual API request was last issued from this
	// process (see minRequestSpacing). In-memory and per-replica by design: the
	// periodic job is inserted by the River leader and worked once, so this only
	// ever guards against duplicate or manual triggers on the same process.
	lastRequestAt time.Time
}

// NewMirror builds a Mirror that will only ever move this install's published
// agent version FORWARD. store and client may be nil (object storage or the HTTP
// client not wired); Run then returns ErrNotConfigured and writes nothing. A nil
// logger is replaced with the default.
func NewMirror(store Store, client HTTPDoer, owner, repo string, logger *slog.Logger) *Mirror {
	return NewMirrorWithRollback(store, client, owner, repo, false, logger)
}

// NewMirrorWithRollback builds a Mirror with the rollback escape hatch stated
// explicitly.
//
// allowRollback=false is the normal posture and the one every caller should
// want: an upstream release that is not strictly newer than what is already
// mirrored is refused (see mayReplace). true is for the one case that rule gets
// wrong, a GENUINE upstream rollback: when a bad release is yanked,
// /releases/latest starts answering with an older one, and an operator who wants
// their install to follow it back has to be able to say so. It is a deliberate,
// explicitly configured act (WPMGR_UPDATE_AGENT_MIRROR_ALLOW_ROLLBACK), not a
// default, because the same behaviour left on permanently is what would let a
// yanked-then-restored upstream flap this install's published version.
//
// It does NOT relax the provenance rule: a pointer this mirror did not write is
// never overwritten either way.
func NewMirrorWithRollback(store Store, client HTTPDoer, owner, repo string, allowRollback bool, logger *slog.Logger) *Mirror {
	if logger == nil {
		logger = slog.Default()
	}
	return &Mirror{
		store:         store,
		client:        client,
		owner:         strings.TrimSpace(owner),
		repo:          strings.TrimSpace(repo),
		logger:        logger,
		allowRollback: allowRollback,
	}
}

// Run performs one mirror cycle. It is idempotent: when the mirrored version
// already matches upstream it downloads nothing and writes nothing.
//
// The verification chain, in order, is:
//
//  1. owner/repo are usable URL path segments.
//  2. The currently published pointer is READ, before any conditional request,
//     because the publish decision depends on it and a 304 must not be able to
//     skip it.
//  3. GET the constructed API URL through the SSRF-hardened client, with
//     If-None-Match when, and only when, the pointer is unchanged since that
//     ETag was banked. A 304 ends the run.
//  4. The release tag is a usable URL path segment.
//  5. Both fixed asset names are present on the release.
//  6. Each asset carries a well-formed sha256 digest and a positive size within
//     its cap.
//  7. agent-release.json is downloaded from a CONSTRUCTED URL, its own bytes are
//     digest- and size-verified against the API's account of that asset, and it
//     is validated with the same rules the agent-facing reader applies.
//  8. The manifest's sha256 and size equal the API's digest and size.
//  9. The PUBLISH DECISION against what is already mirrored (see mayReplace):
//     already current stops here with no download; a pointer this mirror did not
//     write, a same-version-different-bytes republish, anything not strictly
//     newer, and anything that cannot be ordered at all are all refused.
//  10. The pointer bytes are stamped with this mirror's provenance marker.
//  11. The package is streamed under a hard cap, hashing as it goes, and the
//     computed digest must equal both the manifest's and the API's.
//  12. The versioned package object is written FIRST, the pointer manifest LAST.
//
// Any error means nothing was half-written: the only two writes happen at the
// very end, package before pointer.
//
// The release-document ETag is committed to this Mirror's state ONLY when the
// run ends in a terminal success (published, or already current), and it is
// replayed only alongside the pointer state it was banked against. Persisting it
// mid-chain is what previously let ONE transient failure wedge the job: the ETag
// was banked the moment the body parsed, so the next run sent If-None-Match, got
// a 304, and that version was never mirrored again for the life of the process.
func (m *Mirror) Run(ctx context.Context) (Result, error) {
	if m.store == nil || m.client == nil {
		return Result{}, fmt.Errorf("%w: object storage or HTTP client not wired", ErrNotConfigured)
	}
	// 1. owner/repo must be usable path segments before either URL is built.
	if !isPathSegment(m.owner) || !isPathSegment(m.repo) {
		return Result{}, fmt.Errorf("%w: upstream owner/repo %q/%q is not a usable path segment", ErrNotConfigured, m.owner, m.repo)
	}

	// 2. The LOCAL half of the publish decision, read BEFORE any conditional
	// request is sent.
	//
	// The ETag answers exactly one question: has the upstream release document
	// changed. The publish decision asks a second, INDEPENDENT one: what is
	// published into this install's storage right now. That half moves on its
	// own, with nothing happening upstream at all. An operator removes
	// agent-releases/latest.json, which is the remedy this package's own
	// stand-down log tells them to use. An operator publishes their own build
	// over it. A bucket is restored, or a storage migration lands without the
	// agent-releases prefix.
	//
	// So the ETag ALONE IS NOT A SUFFICIENT CACHE KEY for this decision. A run
	// that short-circuits on a 304 before reading the pointer answers "nothing
	// to do" to a question it never asked, and because the ETag lives in memory
	// it keeps answering that until the process restarts or upstream cuts a new
	// release, which on a self-hosted control plane can be weeks. That made the
	// documented remedy simply not work: delete the pointer, and the mirror
	// returns not_modified forever instead of republishing.
	//
	// The fix is to make the cache key cover BOTH halves. The pointer is read
	// here, reduced to the facts the decision actually uses
	// (pointerFingerprint), and the banked ETag is replayed only while that
	// value is unchanged. The common no-op path stays cheap: one read against
	// the install's own bucket, then a single 304.
	cur, curPresent, curErr := m.readPointer(ctx)
	if curErr != nil {
		// The pointer could not be read, and it is NOT known to be absent. This
		// run cannot tell whether it would be replacing its own pointer or an
		// operator's, and guessing "mine" is how a release channel gets taken
		// over on the strength of a storage blip. Write nothing, spend no
		// upstream request, and let the next run decide with a readable answer;
		// the ETag is not committed, so that next run re-examines the release.
		m.logger.Warn("agent release mirror: cannot read the currently published pointer, standing down for this run",
			slog.String("key", ManifestKey),
			slog.String("error", curErr.Error()))
		return Result{}, fmt.Errorf("%w: %s: %v", ErrPointerUnreadable, ManifestKey, curErr)
	}
	local := fingerprintOf(cur, curPresent)

	// 3. Fetch the release document. The ETag is carried in a local, not stored:
	// it is committed at the end, and only on a terminal success.
	rel, etag, notModified, err := m.fetchLatestRelease(ctx, local)
	if err != nil {
		return Result{}, err
	}
	if notModified {
		// Sound to stand down: upstream is unchanged, AND (because
		// fetchLatestRelease only sent If-None-Match on a matching fingerprint)
		// the pointer still looks exactly as it did when this ETag was banked.
		// Both inputs to the decision are where they were, so the conclusion
		// that was reached then still holds now.
		return Result{Reason: "not_modified"}, nil
	}

	// 4. The tag goes into a URL path, so it is shape-checked like owner/repo.
	tag := strings.TrimSpace(rel.TagName)
	if !isPathSegment(tag) {
		return Result{}, fmt.Errorf("%w: release tag %q is not a usable path segment", ErrRefused, tag)
	}

	// 5. Both assets must be present. A release missing either one is not a
	// release this channel can mirror.
	pkgAsset, ok := findAsset(rel.Assets, packageAssetName)
	if !ok {
		return Result{}, fmt.Errorf("%w: release %s has no %s asset", ErrRefused, tag, packageAssetName)
	}
	manAsset, ok := findAsset(rel.Assets, manifestAssetName)
	if !ok {
		return Result{}, fmt.Errorf("%w: release %s has no %s asset", ErrRefused, tag, manifestAssetName)
	}

	// 6. The API's own account of the package: digest and size. Both are required.
	// Without a digest there is nothing to cross-check the manifest against, and a
	// mirror that skips the cross-check when the field happens to be missing is a
	// cross-check an attacker can turn off by omission.
	apiSHA, err := sha256FromDigest(pkgAsset.Digest)
	if err != nil {
		return Result{}, fmt.Errorf("%w: release %s asset %s: %v", ErrRefused, tag, packageAssetName, err)
	}
	if pkgAsset.Size <= 0 {
		return Result{}, fmt.Errorf("%w: release %s asset %s has non-positive size %d", ErrRefused, tag, packageAssetName, pkgAsset.Size)
	}
	if pkgAsset.Size > MaxPackageBytes {
		return Result{}, fmt.Errorf("%w: release %s asset %s is %d bytes, over the %d cap", ErrRefused, tag, packageAssetName, pkgAsset.Size, int64(MaxPackageBytes))
	}

	// 6b. The API's account of the MANIFEST asset, held to the same standard.
	// agent-release.json is the document every other check here derives from (the
	// version that becomes an object key, the sha256 the package is held to, the
	// min_version an agent enforces), so trusting its bytes on the strength of a
	// 200 while the zip beside it is digest-verified would leave the checks
	// resting on an unverified input. The API response and the asset bytes are
	// independent attestations for this asset exactly as they are for the zip.
	apiManifestSHA, err := sha256FromDigest(manAsset.Digest)
	if err != nil {
		return Result{}, fmt.Errorf("%w: release %s asset %s: %v", ErrRefused, tag, manifestAssetName, err)
	}
	if manAsset.Size <= 0 {
		return Result{}, fmt.Errorf("%w: release %s asset %s has non-positive size %d", ErrRefused, tag, manifestAssetName, manAsset.Size)
	}
	if manAsset.Size > maxManifestBytes {
		return Result{}, fmt.Errorf("%w: release %s asset %s is %d bytes, over the %d cap", ErrRefused, tag, manifestAssetName, manAsset.Size, int64(maxManifestBytes))
	}

	// 7. Download the release manifest, verify its own bytes, then validate it.
	// The download is bounded by the size the API declares, not by the generous
	// cap, so a body that grew is refused rather than read.
	manifestBytes, gotManifestSHA, err := m.fetchAsset(ctx, tag, manifestAssetName, manAsset.Size)
	if err != nil {
		return Result{}, err
	}
	if int64(len(manifestBytes)) != manAsset.Size {
		m.logger.Error("agent release mirror REFUSED: truncated manifest download",
			slog.String("tag", tag),
			slog.Int("got_bytes", len(manifestBytes)),
			slog.Int64("want_bytes", manAsset.Size))
		return Result{}, fmt.Errorf("%w: downloaded %d bytes of %s, the release declares %d", ErrRefused, len(manifestBytes), manifestAssetName, manAsset.Size)
	}
	if gotManifestSHA != apiManifestSHA {
		m.logger.Error("agent release mirror REFUSED: downloaded manifest sha256 does not match the GitHub asset digest",
			slog.String("tag", tag),
			slog.String("computed_sha256", gotManifestSHA),
			slog.String("api_digest_sha256", apiManifestSHA))
		return Result{}, fmt.Errorf("%w: %s sha256 %s != API asset digest %s", ErrRefused, manifestAssetName, gotManifestSHA, apiManifestSHA)
	}
	man, err := validateManifest(manifestBytes)
	if err != nil {
		return Result{}, err
	}

	// 8. Cross-check the manifest against the API's account of the same asset. A
	// mismatch means the asset was replaced after publish, or the upload was
	// partial. Either way this refuses and the previous mirror stands.
	if man.PackageSHA256 != apiSHA {
		m.logger.Error("agent release mirror REFUSED: manifest sha256 does not match the GitHub asset digest",
			slog.String("tag", tag),
			slog.String("version", man.Version),
			slog.String("manifest_sha256", man.PackageSHA256),
			slog.String("api_digest_sha256", apiSHA))
		return Result{Version: man.Version}, fmt.Errorf("%w: manifest sha256 %s != API asset digest %s", ErrRefused, man.PackageSHA256, apiSHA)
	}
	if man.PackageSize != pkgAsset.Size {
		m.logger.Error("agent release mirror REFUSED: manifest size does not match the GitHub asset size",
			slog.String("tag", tag),
			slog.String("version", man.Version),
			slog.Int64("manifest_size", man.PackageSize),
			slog.Int64("api_asset_size", pkgAsset.Size))
		return Result{Version: man.Version}, fmt.Errorf("%w: manifest size %d != API asset size %d", ErrRefused, man.PackageSize, pkgAsset.Size)
	}

	// 9. The publish decision, against the pointer read in step 2.
	if curPresent {
		// Already current: the same version with the same bytes. Nothing to
		// download, nothing to write, and this is a terminal SUCCESS, so the
		// ETag is committed with the fingerprint of the pointer that made it
		// current. Deliberately settled before any of the refusals: if the
		// pointer in place already names exactly this release, no write is
		// contemplated and there is nothing to protect it from, whoever
		// published it.
		if cur.Version == man.Version && cur.PackageSHA256 == man.PackageSHA256 {
			m.commitETag(etag, local)
			return Result{Version: man.Version, Reason: "already_current"}, nil
		}
		if rerr := m.mayReplace(cur, man); rerr != nil {
			return Result{Version: man.Version}, rerr
		}
	}
	// The other case is curPresent=false: nothing has ever been published into
	// this install's storage. That is the state this whole feature exists for,
	// and there is no pointer to protect. It is also what an operator following
	// the stand-down remedy leaves behind, which is why step 2 has to be able to
	// SEE it rather than being skipped by a 304.

	// 10. Stamp provenance on the pointer bytes BEFORE anything is downloaded, so
	// a document that cannot carry the stamp costs no bandwidth. Every other
	// field is passed through with its raw value untouched (see markMirrored).
	pointerBytes, err := m.markMirrored(manifestBytes, tag)
	if err != nil {
		return Result{Version: man.Version}, err
	}

	// 11. Stream the package under a hard cap, hashing as it goes.
	pkgBytes, gotSHA, err := m.fetchAsset(ctx, tag, packageAssetName, man.PackageSize)
	if err != nil {
		return Result{Version: man.Version}, err
	}
	if int64(len(pkgBytes)) != man.PackageSize {
		// Short read: the connection ended before the declared size. This is the
		// truncated-download case, and it must never reach storage. The sha256
		// check below catches it too; this one exists so the LOG says truncated
		// rather than corrupt, which are different things to go look at.
		m.logger.Error("agent release mirror REFUSED: truncated download",
			slog.String("version", man.Version),
			slog.Int("got_bytes", len(pkgBytes)),
			slog.Int64("want_bytes", man.PackageSize))
		return Result{Version: man.Version}, fmt.Errorf("%w: downloaded %d bytes, manifest declares %d", ErrRefused, len(pkgBytes), man.PackageSize)
	}
	if gotSHA != man.PackageSHA256 || gotSHA != apiSHA {
		m.logger.Error("agent release mirror REFUSED: downloaded package sha256 mismatch",
			slog.String("version", man.Version),
			slog.String("computed_sha256", gotSHA),
			slog.String("manifest_sha256", man.PackageSHA256),
			slog.String("api_digest_sha256", apiSHA))
		return Result{Version: man.Version}, fmt.Errorf("%w: computed sha256 %s does not match the manifest and API digest", ErrRefused, gotSHA)
	}

	// 12. Write order is load-bearing: package FIRST, pointer LAST, so latest.json
	// never names an object that is not yet in place (scripts/release-agent.sh
	// documents the same ordering for the same reason). If the package write
	// fails, the previous mirror is untouched. If the pointer write fails, the new
	// package is simply an orphan object the next run overwrites, and the previous
	// pointer still names a package that is still there.
	pkgKey := packageObjectKey(man.Version)
	if err := m.store.Put(ctx, pkgKey, bytes.NewReader(pkgBytes), int64(len(pkgBytes))); err != nil {
		return Result{Version: man.Version}, fmt.Errorf("agentupstream: write package %s: %w", pkgKey, err)
	}
	if err := m.store.Put(ctx, ManifestKey, bytes.NewReader(pointerBytes), int64(len(pointerBytes))); err != nil {
		return Result{Version: man.Version}, fmt.Errorf("agentupstream: write manifest %s: %w", ManifestKey, err)
	}

	// Terminal success: only now is the ETag worth remembering, and it is banked
	// against the pointer this run just WROTE, not the one it read at step 2.
	// Banking the old fingerprint would leave every subsequent run seeing a
	// mismatch and re-fetching a full body it does not need.
	m.commitETag(etag, publishedFingerprint(man))

	m.logger.Info("agent release mirrored into this install's own storage",
		slog.String("version", man.Version),
		slog.String("tag", tag),
		slog.String("package_key", pkgKey),
		slog.Int64("package_size", man.PackageSize))
	return Result{Mirrored: true, Version: man.Version}, nil
}

// mayReplace decides whether the upstream release may REPLACE the pointer
// currently published into this install's storage. It is called only when a
// pointer was read successfully and does not already name exactly this release.
//
// Three refusals, in the order that produces the truest log line:
//
//  1. PROVENANCE. A latest.json without this mirror's stamp was published by
//     someone else, which on a self-hosted install means the operator's own
//     release pipeline (scripts/release-agent.sh writes this exact key). Taking
//     that over would undo every build they publish, six hours later, forever,
//     and the operator would have no obvious place to look. So the mirror stands
//     down and says so. Recovering deliberately is one action: remove
//     agent-releases/latest.json, and the next run publishes into an empty
//     channel.
//  2. REPUBLISH. The same version with different bytes. A published version is
//     immutable by construction (its object key contains the version), so this
//     is the "replaced after publish" case, and overwriting the mirrored object
//     would rewrite a version some sites have already installed and verified.
//  3. ORDER. The upstream release must be STRICTLY NEWER. Equality is already
//     handled by the caller, so what this catches is upstream going BACKWARDS,
//     which is what a yanked release does: /releases/latest starts answering
//     with the previous one. Repointing at it does not un-install anything (the
//     agent's own downgrade guard refuses to install something older than it is
//     running) but it does make the dashboard lie, because a site AHEAD of the
//     reference classifies as "current" (agentrelease.Classify): every card
//     would read as up to date on a version no site is actually running, and a
//     newly enrolled site would be offered the wrong build.
//
// Ordering uses the repo's one version comparator, guarded by the repo's one
// well-formed check on BOTH sides (see orderableVersion). A version either side
// cannot order does NOT mean "publish": UNORDERABLE IS A REFUSAL. It has to be,
// because the rule this guard exists to enforce is "strictly newer", and a
// version string the comparator cannot parse is not evidence of newer, it is
// the absence of evidence. Accepting it meant an upstream version of "nightly",
// or "0", or an ordinary release tag spelled "v0.61.98", skipped the ordering
// check completely and silently walked the channel backwards, which is the
// exact outcome the check exists to prevent.
//
// The reason it was previously allowed through was that refusing looked like it
// would wedge the channel on a value nothing here could ever make comparable.
// It no longer does, and both ways out are real: the documented remedy (remove
// agent-releases/latest.json, then the next run publishes into an empty
// channel) now genuinely works, because Run no longer short-circuits on a 304
// without looking at the pointer, and the rollback hatch publishes without an
// ordering proof for an operator who wants exactly that.
func (m *Mirror) mayReplace(cur, man manifest) error {
	if !cur.mirrorWrote() {
		m.logger.Warn("agent release mirror STANDING DOWN: this install publishes its own agent releases",
			slog.String("key", ManifestKey),
			slog.String("published_version", cur.Version),
			slog.String("upstream_version", man.Version),
			slog.String("remedy", "the mirror will never overwrite a pointer it did not write; remove "+ManifestKey+" to hand the channel over"))
		return fmt.Errorf("%w: %w: published %q was not mirrored, upstream is %q", ErrRefused, ErrForeignChannel, cur.Version, man.Version)
	}

	if cur.Version == man.Version {
		m.logger.Error("agent release mirror REFUSED: upstream republished the same version with different bytes",
			slog.String("version", man.Version),
			slog.String("mirrored_sha256", cur.PackageSHA256),
			slog.String("upstream_sha256", man.PackageSHA256))
		return fmt.Errorf("%w: version %s already mirrored with a different sha256", ErrRefused, man.Version)
	}

	curOrdered, curOK := orderableVersion(cur.Version)
	upOrdered, upOK := orderableVersion(man.Version)
	if !curOK || !upOK {
		if m.allowRollback {
			m.logger.Warn("agent release mirror: publishing an UNORDERABLE upstream release because rollback is explicitly allowed",
				slog.String("mirrored_version", cur.Version),
				slog.String("upstream_version", man.Version))
			return nil
		}
		m.logger.Warn("agent release mirror REFUSED: the mirrored and upstream versions cannot be ordered, so this run cannot show upstream is newer",
			slog.String("mirrored_version", cur.Version),
			slog.String("upstream_version", man.Version),
			slog.Bool("mirrored_orderable", curOK),
			slog.Bool("upstream_orderable", upOK),
			slog.String("remedy", "remove "+ManifestKey+" to publish into an empty channel, or set WPMGR_UPDATE_AGENT_MIRROR_ALLOW_ROLLBACK=true to publish without an ordering proof"))
		return fmt.Errorf("%w: %w: upstream %q against mirrored %q", ErrRefused, ErrUnorderable, man.Version, cur.Version)
	}
	if wpversion.Compare(upOrdered, curOrdered) > 0 {
		return nil
	}
	if m.allowRollback {
		m.logger.Warn("agent release mirror: publishing an OLDER upstream release because rollback is explicitly allowed",
			slog.String("mirrored_version", cur.Version),
			slog.String("upstream_version", man.Version))
		return nil
	}
	m.logger.Warn("agent release mirror REFUSED: upstream is not newer than the version already mirrored",
		slog.String("mirrored_version", cur.Version),
		slog.String("upstream_version", man.Version),
		slog.String("remedy", "set WPMGR_UPDATE_AGENT_MIRROR_ALLOW_ROLLBACK=true to follow a genuine upstream rollback"))
	return fmt.Errorf("%w: %w: upstream %q is not newer than mirrored %q", ErrRefused, ErrDowngrade, man.Version, cur.Version)
}

// orderableVersion reduces a version string to the one form this package is
// willing to ORDER, reporting false when it cannot be ordered at all.
//
// It is THE ONE PLACE either side of the comparison in mayReplace is
// normalised. Both sides go through it, so the mirrored version and the
// upstream version can never end up measured by different rules, and there is a
// single answer to "what counts as orderable here" to change if that ever needs
// to widen.
//
// The only normalisation is a leading lowercase "v", because that is OUR OWN
// release-tag convention (.github/workflows/release.yml tags vX.Y.Z, and
// isPathSegment accepts exactly that shape for the tag). Treating "v0.61.98" as
// unorderable would stand the channel down over the SPELLING of an ordinary
// release rather than anything about its content, and while unorderable meant
// publish it did worse than that: it downgraded the channel in silence.
// Everything else is left exactly as upstream wrote it and handed to the repo's
// one well-formed check.
//
// Deliberately NOT pushed down into agentrelease.WellFormed, even though that is
// the check it wraps: that predicate also drives the fleet rollup
// (agentrelease.Classify), where widening what counts as a version would
// reclassify every site reporting one. What this mirror is willing to publish
// over is a separate decision from how the dashboard reports a site, and they
// should not be changed by the same edit.
func orderableVersion(v string) (string, bool) {
	s := strings.TrimPrefix(strings.TrimSpace(v), "v")
	if !agentrelease.WellFormed(s) {
		return "", false
	}
	return s, true
}

// markMirrored returns the pointer bytes to publish: the upstream document with
// this mirror's provenance stamp added (see provenanceField).
//
// The document is round-tripped as raw JSON values, never re-typed, so every
// field upstream published survives with its exact value: min_version, requires,
// requires_php, tested and sections all reach latest.json unchanged, and a
// numeric field cannot be reshaped by a decode-encode cycle. Only key order and
// whitespace differ from the bytes upstream served.
func (m *Mirror) markMirrored(body []byte, tag string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("%w: %s is not a JSON object: %v", ErrRefused, manifestAssetName, err)
	}
	marker, err := json.Marshal(provenanceMarker)
	if err != nil {
		return nil, fmt.Errorf("agentupstream: encode provenance marker: %w", err)
	}
	source, err := json.Marshal(m.owner + "/" + m.repo + "@" + tag)
	if err != nil {
		return nil, fmt.Errorf("agentupstream: encode provenance source: %w", err)
	}
	fields[provenanceField] = marker
	fields[provenanceSourceField] = source

	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("%w: %s cannot be re-encoded: %v", ErrRefused, manifestAssetName, err)
	}
	return out, nil
}

// commitETag records the release-document ETag, together with the pointer state
// it was reached against, for replay as If-None-Match on the next run.
//
// Called ONLY from a terminal success path: a run that refused, or failed
// anywhere in the chain, must leave the previous value alone so the next run
// re-fetches the full document and gets another chance at the same release.
//
// The fingerprint travels WITH the ETag and is never recorded on its own,
// because the pair is what the next run's short-circuit rests on: this ETag was
// a sufficient answer while the pointer looked like THAT.
func (m *Mirror) commitETag(etag string, local pointerFingerprint) {
	if etag == "" {
		return
	}
	m.mu.Lock()
	m.etag = etag
	m.etagPointer = local
	m.mu.Unlock()
}

// apiURL builds the releases-latest URL. /releases/latest excludes drafts and
// prereleases by contract.
func apiURL(owner, repo string) string {
	return "https://" + apiHost + "/repos/" + owner + "/" + repo + "/releases/latest"
}

// downloadURL builds the asset download URL from parts this package controls.
//
// The /releases/download/<tag>/<name> form is deliberate: it is ONE redirect to
// the asset host, where /releases/latest/download/<name> is two. Never built from
// anything the fetched JSON supplied except the tag, which is shape-validated
// first (isPathSegment).
func downloadURL(owner, repo, tag, asset string) string {
	return "https://" + downloadHost + "/" + owner + "/" + repo + "/releases/download/" + tag + "/" + asset
}

// packageObjectKey is the single deterministic key a given version's package may
// occupy. Identical to the form update_handler.go pins latest.json's
// package_object_key to.
func packageObjectKey(version string) string {
	return packagePrefix + version + "/" + packageAssetName
}

// fetchLatestRelease GETs the constructed API URL, replaying the last ETag.
// Returns notModified=true on a 304 (nothing new since the last run), and the
// ETag of the response it parsed.
//
// local is the pointer state THIS run observed. The banked ETag is replayed only
// when it matches the state that ETag was banked against, because a 304 ends the
// run and the run's decision also depends on the pointer (see Run step 2). When
// they differ this asks for the full document instead, which is what lets the
// decision be made again from scratch after the pointer was deleted, replaced,
// or lost with its prefix.
//
// It deliberately does NOT store that ETag. Persisting it here, the moment a 200
// body parsed, meant every later failure in the chain (an invalid manifest, a
// mismatched digest, a failed download, a failed write) returned with the ETag
// already banked: the next run sent If-None-Match, got a 304, and the release
// was never mirrored again for the life of the process. A self-hosted control
// plane runs for months, so "only a restart or a newer release recovers it" is
// not a recovery. The caller commits it on terminal success (commitETag).
func (m *Mirror) fetchLatestRelease(ctx context.Context, local pointerFingerprint) (githubRelease, string, bool, error) {
	m.mu.Lock()
	etag := m.etag
	if m.etagPointer != local {
		// The pointer moved under us. The banked ETag is still a true statement
		// about UPSTREAM, but it is no longer a sufficient cache key for this
		// run's decision, so it is not replayed. It is deliberately not cleared
		// either: if the pointer is put back the way it was, it becomes usable
		// again with no extra bookkeeping.
		etag = ""
	}
	last := m.lastRequestAt
	m.mu.Unlock()

	if !last.IsZero() {
		if elapsed := time.Since(last); elapsed < minRequestSpacing {
			return githubRelease{}, "", false, fmt.Errorf("%w: only %s since the last upstream request", ErrRateLimited, elapsed.Round(time.Second))
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL(m.owner, m.repo), nil)
	if err != nil {
		return githubRelease{}, "", false, fmt.Errorf("%w: build request: %v", ErrUpstreamUnavailable, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", userAgent)
	// If-None-Match keeps a run that learns nothing cheap: the unauthenticated
	// API allows 60 requests/hour per IP, and a 304 does not consume that budget
	// the way a full body does.
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return githubRelease{}, "", false, fmt.Errorf("%w: %v", ErrUpstreamUnavailable, err)
	}
	defer drainClose(resp.Body)

	// A response of any kind means a request was actually spent.
	m.mu.Lock()
	m.lastRequestAt = time.Now()
	m.mu.Unlock()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		return githubRelease{}, "", true, nil
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0":
		return githubRelease{}, "", false, fmt.Errorf("%w: GitHub returned %d", ErrRateLimited, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return githubRelease{}, "", false, fmt.Errorf("%w: GitHub returned %d", ErrUpstreamUnavailable, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes))
	if err != nil {
		return githubRelease{}, "", false, fmt.Errorf("%w: read release document: %v", ErrUpstreamUnavailable, err)
	}
	var rel githubRelease
	if jerr := json.Unmarshal(body, &rel); jerr != nil {
		return githubRelease{}, "", false, fmt.Errorf("%w: release document is not valid JSON: %v", ErrRefused, jerr)
	}

	// The ETag is RETURNED, not recorded, and only for a body actually parsed.
	// Whether it is worth remembering depends on how the rest of the run ends,
	// which this function cannot see.
	return rel, resp.Header.Get("ETag"), false, nil
}

// fetchAsset downloads one release asset from a CONSTRUCTED URL, bounded at limit
// bytes, returning the bytes and their lowercase-hex sha256 computed during the
// stream. A body larger than limit is refused rather than truncated silently.
func (m *Mirror) fetchAsset(ctx context.Context, tag, asset string, limit int64) ([]byte, string, error) {
	url := downloadURL(m.owner, m.repo, tag, asset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("%w: build request for %s: %v", ErrUpstreamUnavailable, asset, err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", userAgent)

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: download %s: %v", ErrUpstreamUnavailable, asset, err)
	}
	defer drainClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%w: download %s returned %d", ErrUpstreamUnavailable, asset, resp.StatusCode)
	}

	h := sha256.New()
	var buf bytes.Buffer
	// Read one byte past the cap so an oversize body is detected rather than
	// silently truncated into a hash that would then fail for the wrong reason.
	n, cerr := io.Copy(io.MultiWriter(&buf, h), io.LimitReader(resp.Body, limit+1))
	if cerr != nil {
		return nil, "", fmt.Errorf("%w: read %s: %v", ErrUpstreamUnavailable, asset, cerr)
	}
	if n > limit {
		return nil, "", fmt.Errorf("%w: %s exceeds the %d byte cap", ErrRefused, asset, limit)
	}
	return buf.Bytes(), hex.EncodeToString(h.Sum(nil)), nil
}

// readPointer reads the currently published pointer and splits out the ONE
// failure that is not a failure: no pointer has ever been published here.
//
// present=false with a nil error is the ordinary self-hosted starting state, the
// case this whole feature exists for, and the state an operator leaves behind
// when they follow the stand-down remedy. A non-nil error means a pointer may
// well be there and this process cannot see whose it is, which is a different
// thing entirely and never a licence to publish.
func (m *Mirror) readPointer(ctx context.Context) (manifest, bool, error) {
	cur, err := m.readMirrored(ctx)
	switch {
	case err == nil:
		return cur, true, nil
	case errors.Is(err, blobstore.ErrNotFound):
		return manifest{}, false, nil
	default:
		return manifest{}, false, err
	}
}

// readMirrored reads the manifest currently published into this install's
// storage.
//
// The DISTINCTION between the failures matters to the caller and is preserved:
// blobstore.ErrNotFound means nothing has ever been published here, which is the
// ordinary self-hosted starting state and the case this whole feature exists
// for. Anything else (a storage blip, a malformed document) means a pointer may
// well be there and this process cannot see whose it is, so the caller declines
// to write rather than assuming it owns whatever it cannot read.
func (m *Mirror) readMirrored(ctx context.Context) (manifest, error) {
	rc, err := m.store.GetViaPresign(ctx, ManifestKey)
	if err != nil {
		return manifest{}, err
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes))
	if err != nil {
		return manifest{}, err
	}
	var cur manifest
	if jerr := json.Unmarshal(body, &cur); jerr != nil {
		return manifest{}, jerr
	}
	return cur, nil
}

// validateManifest applies the SAME rules internal/agent/update_handler.go's
// readLatest applies to latest.json, because this is the object that becomes that
// install's latest.json: the slug is pinned, the version is non-empty, the sha256
// is 64 lowercase hex, the size is positive, and the package object key is EXACTLY
// the one deterministic key for that version (a manifest naming any other key in
// the agent-releases/ prefix, such as a stale zip or latest.json itself, is
// rejected).
//
// A failure here STOPS the run. The version is never inferred from the release
// tag as a fallback: the tag is the control plane's release number and no longer
// tracks the agent's own version, so synthesising one would publish the wrong
// number for the code in the zip and the agent's downgrade guard would then
// refuse every later, correctly numbered offer.
func validateManifest(body []byte) (manifest, error) {
	var man manifest
	if err := json.Unmarshal(body, &man); err != nil {
		return manifest{}, fmt.Errorf("%w: %s is not valid JSON: %v", ErrRefused, manifestAssetName, err)
	}
	if man.Slug != expectedSlug {
		return manifest{}, fmt.Errorf("%w: manifest slug %q, want %q", ErrRefused, man.Slug, expectedSlug)
	}
	if man.Version == "" {
		return manifest{}, fmt.Errorf("%w: manifest has no version", ErrRefused)
	}
	// The version is the ONE fetched value that reaches an object key, and this
	// is the package that performs the WRITE. Checking only that
	// package_object_key equals the key built FROM that version proves the
	// document is self-consistent, not that it is safe: both halves come out of
	// the same fetched file, so a hostile pair agrees with itself and a version
	// like "../../backups/tenant-a/2026-07" would pass and then be written
	// outside the release prefix. Shape-check it with the SAME rule the serving
	// route applies to the published version before it rebuilds that key.
	if !agentplugin.IsReleaseVersionSegment(man.Version) {
		return manifest{}, fmt.Errorf("%w: manifest version %q is not a usable object-key segment", ErrRefused, man.Version)
	}
	if !isHex64(man.PackageSHA256) {
		return manifest{}, fmt.Errorf("%w: manifest package_sha256 %q is not 64 lowercase hex", ErrRefused, man.PackageSHA256)
	}
	if man.PackageSize <= 0 {
		return manifest{}, fmt.Errorf("%w: manifest package_size %d is not positive", ErrRefused, man.PackageSize)
	}
	if man.PackageObjectKey != packageObjectKey(man.Version) {
		return manifest{}, fmt.Errorf("%w: manifest package_object_key %q, want %q", ErrRefused, man.PackageObjectKey, packageObjectKey(man.Version))
	}
	if man.PackageSize > MaxPackageBytes {
		return manifest{}, fmt.Errorf("%w: manifest package_size %d is over the %d cap", ErrRefused, man.PackageSize, int64(MaxPackageBytes))
	}
	return man, nil
}

// findAsset returns the release asset with exactly this name.
func findAsset(assets []githubAsset, name string) (githubAsset, bool) {
	for _, a := range assets {
		if a.Name == name {
			return a, true
		}
	}
	return githubAsset{}, false
}

// sha256FromDigest parses GitHub's per-asset "digest" field ("sha256:<64 hex>")
// and returns the bare lowercase hex. An absent, differently-prefixed, or
// malformed digest is an error, never a silent skip of the cross-check.
func sha256FromDigest(digest string) (string, error) {
	d := strings.TrimSpace(digest)
	if d == "" {
		return "", errors.New("asset carries no digest")
	}
	hexPart, ok := strings.CutPrefix(d, "sha256:")
	if !ok {
		return "", fmt.Errorf("asset digest %q is not sha256-prefixed", d)
	}
	if !isHex64(hexPart) {
		return "", fmt.Errorf("asset digest %q is not 64 lowercase hex", d)
	}
	return hexPart, nil
}

// isHex64 reports whether s is exactly 64 lowercase hex chars (a sha256 digest).
// Mirrors update_handler.go's isHex64 so both ends of this channel agree on what
// a well-formed digest is.
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	if _, err := hex.DecodeString(s); err != nil {
		return false
	}
	return s == strings.ToLower(s)
}

// drainClose drains a bounded amount of a response body and closes it, so the
// connection can be reused.
func drainClose(rc io.ReadCloser) {
	if rc == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<16))
	_ = rc.Close()
}
