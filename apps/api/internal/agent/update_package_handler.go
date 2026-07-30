package agent

// GH #302: serve the mirrored agent package FROM THE CONTROL PLANE.
//
// THE PROBLEM. internal/agentupstream mirrors the public agent release into the
// operator's own object storage, which fixes "a self-hosted install has no
// published agent release". It does not fix the second half: a mirrored object
// presigns onto the operator's OWN storage host, and that hostname differs per
// install, so it can never be an agent default. The documented workaround was to
// add WPMGR_AGENT_PACKAGE_HOST to EVERY site's wp-config.php, which for an
// operator running many sites across several servers is the actual pain.
//
// THE FIX. The agent always trusts the host of the control plane it is enrolled
// with (it already fetches the signed manifest from there, so that origin already
// owns the update channel end to end), and the control plane serves the package
// itself. The agent already knows its control-plane URL, so nothing is configured
// per site.
//
// WHY THIS ROUTE IS UNAUTHENTICATED, AND WHAT REPLACES THE AUTH. The agent
// downloads the package with a bare wp_remote_get on the manifest's package_url:
// no Authorization header, and redirection => 0, so this route must answer 200
// with the bytes and can never redirect to storage. It therefore cannot sit
// behind the agent's Ed25519 signed-request middleware. An unauthenticated route
// that streams a multi-megabyte zip is a bandwidth amplification vector, so the
// URL carries a short-lived signed token minted by the SAME Ed25519 signer that
// signs the manifest (agentcmd.MintPackageToken / VerifyPackageToken), bound to
// one site, one version, and one expiry window.
//
// THE TOKEN BOUNDS WHO, NOT HOW MANY. It is bearer and not single-use, so on its
// own it does not stop the holder of one ordinary manifest fetch from issuing
// unbounded concurrent downloads and pushing that load onto the control plane
// instead of onto object storage. Four bounds in update_package_limits.go close
// that: a per-instance cap on in-flight streams and a small per-token redemption
// budget, both enforced below before any storage work happens, plus two bounds
// on the stream itself, a progress bound so a stream that stops moving gives its
// slot back, and a whole-transfer ceiling so a stream that keeps dripping gives
// it back too. The last one matters because progress alone is trivially
// satisfiable: without the ceiling, holding a slot costs one byte per window.
//
// THE OBJECT KEY IS NOT ADDRESSABLE. Nothing the caller supplies reaches the
// object key. The key is rebuilt from the version in the PUBLISHED manifest, in
// the same pinned agent-releases/<version>/wpmgr-agent.zip form the mirror
// writes and readLatest already enforces; the token's version must equal the
// published one, and the request's site must equal the token's. There is no
// caller-supplied key, no prefix, no traversal and no wildcard on this path.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentplugin"
	"github.com/mosamlife/wpmgr/apps/api/internal/blobstore"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/server/httpx"
)

// ---------------------------------------------------------------------------
// WIRE CONTRACT. These literals are the whole agreement between the mint side
// (the package_url written into the signed manifest) and the download side (this
// route). TestPackageDownloadWireContract pins every one of them against a
// hard-coded string, so renaming any of them fails this package's own suite
// rather than silently producing a package_url nothing can serve.
// ---------------------------------------------------------------------------

const (
	// PackageRoutePrefix is the path the download route is mounted under, in Gin
	// form minus the site parameter.
	PackageRoutePrefix = "/agent/v1/update/package"

	// PackageSiteIDParam is the Gin path-parameter name carrying the site the
	// download is for. It exists so a token minted for site A cannot be redeemed
	// on a URL naming site B: the route compares the two and refuses a mismatch.
	// It never reaches the object key.
	PackageSiteIDParam = "siteId"

	// PackageRoutePath is the full Gin route pattern.
	PackageRoutePath = PackageRoutePrefix + "/:" + PackageSiteIDParam

	// PackageTokenQueryParam is the query parameter carrying the signed
	// download token.
	//
	// The token rides in the QUERY STRING, not the path, deliberately: the
	// request logger (internal/middleware.Logger) records c.Request.URL.Path and
	// never the raw query, so a bearer credential in the query string does not
	// land in the access log, while one in the path would.
	PackageTokenQueryParam = "token"

	// PackageDownloadFilename is the Content-Disposition filename, matching the
	// asset name the release pipeline and the mirror both use.
	PackageDownloadFilename = expectedAgentSlug + ".zip"

	// PackageContentType is the Content-Type served for the zip.
	PackageContentType = "application/zip"
)

// PackageTokenSigner is the narrow token surface this handler needs, satisfied
// by *agentcmd.Signer. Mint is used on the manifest path, Verify on the download
// path; both are the SAME key, so a token can only be redeemed by the control
// plane that issued it.
type PackageTokenSigner interface {
	MintPackageToken(now time.Time, siteID, version string, ttl time.Duration) (token string, jti string, err error)
	VerifyPackageToken(now time.Time, token string) (agentcmd.PackageToken, error)
}

// EnablePackageServing switches this handler's minted package_url from a
// presigned object-storage URL to this control plane's own streaming route, and
// arms the download route to serve it.
//
// baseURL is the control plane's public origin (e.g. "https://cp.example.com").
// When empty, the origin is derived per request from the URL the agent itself
// used to reach the control plane, which is exactly the host the agent trusts,
// and is what makes this need no configuration on a self-hosted install.
//
// Until this is called the handler behaves EXACTLY as before: package_url stays
// a presigned storage URL and the download route refuses everything. That
// default is deliberate. An agent older than the build that trusts its own
// control-plane host will reject a CP-hosted package_url on its host allowlist,
// so flipping this on for a fleet whose agents predate that change would stop
// their self-update rather than start it.
func (h *UpdateHandler) EnablePackageServing(tokens PackageTokenSigner, baseURL string) {
	h.tokens = tokens
	h.packageBase = strings.TrimRight(strings.TrimSpace(baseURL), "/")
}

// RegisterPublic mounts the UNAUTHENTICATED package download route. It must be
// mounted on the root engine, never on the agent-authenticated group: the agent
// sends no credentials on this request (see the file header). The signed token
// in the query string is the entire authorisation.
func (h *UpdateHandler) RegisterPublic(r gin.IRouter) {
	r.GET(PackageRoutePath, h.packageDownload)
}

// packageDownload streams the published agent package to a caller holding a
// valid, unexpired, site- and version-bound token.
func (h *UpdateHandler) packageDownload(c *gin.Context) {
	ctx := c.Request.Context()

	if h.store == nil || h.tokens == nil {
		// Serving is not enabled on this install. Nothing has ever minted a token
		// pointing here, so any request is either a probe or a stale URL.
		httpx.Error(c, domain.Unavailable("update_package_serving_disabled", "control-plane package serving is not enabled"))
		return
	}

	// The site in the path is parsed as a UUID purely to reject junk early and to
	// normalise it for the comparison below. It is NEVER used to address storage.
	siteID, err := uuid.Parse(strings.TrimSpace(c.Param(PackageSiteIDParam)))
	if err != nil {
		httpx.Error(c, errPackageUnauthorized())
		return
	}

	claims, err := h.tokens.VerifyPackageToken(time.Now(), c.Query(PackageTokenQueryParam))
	if err != nil {
		// Log the DISTINCT reason; return one opaque 401. Telling an
		// unauthenticated caller which check failed is free oracle access to the
		// token format.
		slog.WarnContext(ctx, "GH #302 package download token refused",
			slog.String("reason", packageTokenReason(err)),
			slog.String("site_id", siteID.String()))
		httpx.Error(c, errPackageUnauthorized())
		return
	}

	// Site binding: a token minted for another site cannot be redeemed here.
	if !strings.EqualFold(claims.SiteID, siteID.String()) {
		slog.WarnContext(ctx, "GH #302 package download token is for a different site",
			slog.String("token_jti", claims.JTI),
			slog.String("request_site_id", siteID.String()))
		httpx.Error(c, errPackageUnauthorized())
		return
	}

	// CONCURRENCY BOUND (H3a). Per instance, not fleet-wide. Above the cap this
	// answers 429 immediately instead of queueing, because a queued request still
	// pins a goroutine and a storage connection, which is the amplification we
	// are refusing. Held across the manifest read as well as the stream, so ALL
	// the storage work this route can cause is inside the bound.
	//
	// THIS RUNS BEFORE THE REDEMPTION COUNT, DELIBERATELY. A busy refusal is the
	// CONTROL PLANE saying "not now"; it serves zero bytes and it is not the
	// caller's fault. Counting it against the token's budget spent the caller's
	// retries on refusals, so five 429s during a busy patch left a site holding a
	// token that a completely idle instance would then reject as exhausted, which
	// is precisely the retry the budget is deliberately not single-use for. The
	// ordering costs nothing: an exhausted token now holds a slot for one map
	// lookup and still reaches no storage.
	if !h.streams.tryAcquire() {
		slog.WarnContext(ctx, "GH #302 package download refused: instance stream cap reached",
			slog.String("site_id", siteID.String()),
			slog.String("token_jti", claims.JTI),
			slog.Int("in_flight", h.streams.inFlight()),
			slog.Int("limit", h.streams.capacity()))
		c.Header("Retry-After", "30")
		httpx.Error(c, domain.RateLimited("update_package_busy",
			"too many package downloads are in flight; retry shortly"))
		return
	}
	defer h.streams.release()

	// REDEMPTION BOUND (H3b). The token is bearer and deliberately not
	// single-use, so without this one captured token is an unlimited supply of
	// package-sized responses for its whole five-minute life. Counted BEFORE any
	// storage work so an exhausted token costs one signature verification and
	// nothing else. See update_package_limits.go for the budget and why it cannot
	// interfere with a legitimate download or its retries.
	red := h.redemptions.redeem(time.Now(), claims.JTI, claims.ExpiresAt)
	if !red.Allowed {
		slog.WarnContext(ctx, "GH #302 package download token redeemed too many times",
			slog.String("token_jti", claims.JTI),
			slog.String("site_id", siteID.String()),
			slog.Int("redemptions", red.Count),
			slog.Int("limit", maxPackageTokenRedemptions))
		c.Header("Retry-After", "300")
		httpx.Error(c, domain.RateLimited("update_package_token_exhausted",
			"this download token has been redeemed too many times"))
		return
	}
	// Throttled, because this fires once per REQUEST for as long as the table
	// stays full, and the condition that fills it is a flood of freshly minted
	// tokens. Warning on every one of them turns a token flood into a log flood
	// on the instance least able to afford it. See packageTableFullLogEvery.
	if !red.Tracked && warnPackageTableFull.allow(time.Now(), packageTableFullLogEvery) {
		slog.WarnContext(ctx, "GH #302 package token redemption table is full; redemption limit not enforced for these requests",
			slog.Int("tracked_limit", maxTrackedPackageTokens),
			slog.Duration("next_report_after", packageTableFullLogEvery))
	}

	rel, err := h.readLatest(ctx)
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			httpx.Error(c, domain.NotFound("update_package_not_published", "no agent release is published"))
			return
		}
		if errors.Is(err, errInvalidManifest) {
			httpx.Error(c, domain.Internal("update_manifest_invalid", "published release manifest is malformed"))
			return
		}
		slog.ErrorContext(ctx, "GH #302 package download manifest read failed", slog.String("err", err.Error()))
		httpx.Error(c, domain.Internal("update_manifest_read_failed", "failed to read release manifest").WithCause(err))
		return
	}

	// Version binding. The token authorises ONE version; only the version that is
	// published RIGHT NOW is servable. A token minted before a newer release
	// landed is refused rather than silently upgraded, because the agent verifies
	// the sha256 from the manifest it was handed and would reject different bytes
	// anyway, and because a token must never widen to cover a build it did not
	// name.
	if claims.Version != rel.Version {
		slog.WarnContext(ctx, "GH #302 package download token names a version that is not published",
			slog.String("token_jti", claims.JTI),
			slog.String("token_version", claims.Version),
			slog.String("published_version", rel.Version))
		httpx.Error(c, domain.NotFound("update_package_version_unavailable", "the requested agent release is not the published one"))
		return
	}

	// Belt and braces on the one value that reaches the key. readLatest already
	// pins package_object_key to exactly this form, so a version that is not a
	// clean path segment cannot arrive here from a well-formed manifest; refusing
	// it explicitly means no malformed published manifest can ever aim this route
	// at another object.
	if !agentplugin.IsReleaseVersionSegment(rel.Version) {
		slog.ErrorContext(ctx, "GH #302 published version is not a usable path segment", slog.String("version", rel.Version))
		httpx.Error(c, domain.Internal("update_manifest_invalid", "published release manifest is malformed"))
		return
	}

	// THE KEY. Rebuilt from constants plus the published version. Not read from
	// the manifest's package_object_key, not derived from anything in the
	// request. There is exactly one object this route can ever read.
	key := updatePackagePrefix + rel.Version + "/" + PackageDownloadFilename

	// THE STREAMING READ (H2). Deliberately GetStreamViaPresign and never
	// GetViaPresign: the latter runs on a client with a 15s WHOLE-REQUEST
	// timeout, and http.Client.Timeout covers reading the response body. Because
	// the body below is copied straight onto the site's connection, the SITE's
	// download speed decides how long the storage read stays open, so a slow
	// consumer would kill the storage side. At a few MiB that capped path needs a
	// sustained ~200 KB/s from the site, which plenty of shared hosting misses,
	// and by then this handler has already written 200 plus a Content-Length: the
	// site gets a short body, refuses it on its size check, and never self-updates
	// again, retrying every six hours forever with only a CP log line to show for
	// it.
	//
	// The streaming client has no whole-request cap. PROGRESS is what bounds it,
	// on BOTH halves and with one implementation: blobstore's stall guard tears
	// the storage read down after blobstore.StreamStallTimeout with no bytes
	// moving, and streamPackage below runs that same guard over the write to the
	// site, restarted by every partial write the socket accepts. A disconnected
	// site cancels streamCtx and ends both immediately.
	//
	// WHAT THE CONTROL PLANE NOW DEMANDS OF A SLOW SITE: no rate at all, up to a
	// whole-transfer ceiling. A site that accepts even one byte inside every 20s
	// window is carried on, whatever size it reads in, for as long as the ceiling
	// allows. The earlier per-call write deadline did impose a rate, and not the
	// 3.28 KB/s its chunk size reasoned to: measured at a fixed 25.6 KB/s it
	// completed at a 1280 B read size and truncated at 4096 B and 16 KiB, so the
	// effective floor sat between 25 and 40 KB/s and moved with the consumer's
	// read granularity. The table and the mechanism are on streamPackage.
	//
	// "CARRIED ON HOWEVER LONG THAT TAKES" IS ALSO THE ATTACK, WHICH IS WHY THERE
	// IS A CEILING. Read as a pure feature it is a denial of service: the minimum
	// a consumer must accept to restart the window is whatever its TCP stack
	// registers as progress, so one that drips holds its slot indefinitely.
	// Measured over net.Pipe at one byte per 60% of the window, 12.028s and 60
	// windows moved 99 of 4194304 bytes, 8.2 B/s, slot still held. Sixteen of
	// those, mintable from four unrate-limited manifest fetches, are the whole
	// update channel for every site on the instance. packageStreamTotalLimit
	// bounds the TOTAL for that reason, derived from the package size at 1 KB/s
	// and clamped to [30 min, 2 h], which is two orders of magnitude clear of the
	// slowest legitimate consumer and far clear of what the agent's own download
	// budget demands of it. The arithmetic, and why the token's expiry is
	// deliberately not revalidated mid-stream instead, are on that function.
	//
	// THE END-TO-END FLOOR THAT REMAINS IS THE AGENT'S. The agent downloads with
	// wp_remote_get in apps/agent/includes/support/class-update-checker.php using
	// a WHOLE-operation cap, raised from 60s to 300s in agent 0.61.105 precisely
	// because the old value was the binding constraint once the control plane
	// stopped being one. At 60s a 3 MiB package demanded about 55 KB/s, which sat
	// ABOVE the 25 to 40 KB/s band this whole path exists to carry: those sites
	// downloaded for a full minute, were cut off, failed the size check, and
	// retried the identical failure forever. At 300s the same package demands
	// about 11 KB/s. Below that the download still fails, but on the AGENT side as
	// a WP_Error that retries on the next check rather than arriving truncated.
	// Anyone moving the agent's download cap moves this floor, and it is the ONLY
	// floor left on the path.
	//
	// streamCtx is this request's context plus a cancel that fires the moment this
	// handler gives up, so when the WRITE side is the one that timed out the
	// storage read is torn down with it instead of being left to its own window.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	rc, err := h.store.GetStreamViaPresign(streamCtx, key)
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			httpx.Error(c, domain.NotFound("update_package_not_published", "the published agent package object is missing"))
			return
		}
		slog.ErrorContext(ctx, "GH #302 package download read failed",
			slog.String("err", err.Error()),
			slog.String("key", key))
		httpx.Error(c, domain.Internal("update_package_read_failed", "failed to read the release package").WithCause(err))
		return
	}
	defer func() { _ = rc.Close() }()

	c.Header("Content-Type", PackageContentType)
	c.Header("Content-Length", strconv.FormatInt(rel.PackageSize, 10))
	c.Header("Content-Disposition", `attachment; filename="`+PackageDownloadFilename+`"`)
	c.Header("X-Content-Type-Options", "nosniff")
	// The URL is a bearer credential, so no shared cache may keep the body.
	c.Header("Cache-Control", "private, no-store")
	c.Status(http.StatusOK)

	// Bounded copy on BOTH axes: never more bytes than the manifest declares
	// (whatever the object turns out to hold), and never more than
	// packageStreamStall of wall clock with nothing moving. A short read leaves
	// the agent with fewer bytes than Content-Length promised, which its own size
	// and sha256 checks reject.
	written, cerr := streamPackage(c.Writer, rc, rel.PackageSize, packageStreamStall)
	if cerr != nil && !errors.Is(cerr, io.EOF) {
		// The status and headers are already flushed, so this cannot become an
		// error response. Log it; the agent's integrity checks fail the install.
		slog.ErrorContext(ctx, "GH #302 package download stream aborted",
			slog.String("err", cerr.Error()),
			slog.String("key", key),
			slog.Int64("written", written),
			slog.Int64("want", rel.PackageSize))
		return
	}

	// A SHORT BODY IS A FAILURE, NOT A SUCCESS. streamPackage reports io.EOF when
	// the object held fewer bytes than the manifest declares, and that used to
	// fall through to the INFO line below: a truncated serve was logged as
	// "served", at INFO, with a byte count under the Content-Length this handler
	// had already promised. The site refuses it on its size check and retries
	// forever, so this is precisely the condition that must be loud. It means the
	// published manifest and the published object disagree, which no request can
	// cause and no retry can fix.
	if written < rel.PackageSize {
		slog.ErrorContext(ctx, "GH #302 package download served a short body: the published object is smaller than the manifest declares",
			slog.String("site_id", siteID.String()),
			slog.String("key", key),
			slog.String("version", rel.Version),
			slog.Int64("written", written),
			slog.Int64("want", rel.PackageSize))
		return
	}

	slog.InfoContext(ctx, "GH #302 served agent package from the control plane",
		slog.String("site_id", siteID.String()),
		slog.String("version", rel.Version),
		slog.String("token_jti", claims.JTI),
		slog.Int64("bytes", written))
}

// PackageStreamsInFlight reports how many package downloads this instance is
// currently serving. Exported for the shutdown path and for operational logging.
func (h *UpdateHandler) PackageStreamsInFlight() int {
	if h == nil || h.streams == nil {
		return 0
	}
	return h.streams.inFlight()
}

// WaitForPackageStreams blocks until every in-flight package download has
// finished or ctx is done, and reports how many were still running when it
// returned. Zero means the route drained cleanly.
//
// The server calls this on SIGTERM because http.Server.Shutdown does not:
// Shutdown explicitly does not wait for HIJACKED connections, and the write half
// of this route hijacks (see packageStreamSemaphore.waitIdle for the measurement
// and the reasoning).
func (h *UpdateHandler) WaitForPackageStreams(ctx context.Context) int {
	if h == nil || h.streams == nil {
		return 0
	}
	return h.streams.waitIdle(ctx)
}

// packageURL returns the URL to embed in the signed manifest as package_url.
//
// With control-plane serving enabled this is this route, carrying a freshly
// minted token whose lifetime is h.presignTTL: the SAME window the presigned
// object-storage URL used, so the manifest's own exp semantics are unchanged and
// nothing downstream needs to know the origin moved. With it disabled this is
// the presigned storage URL, byte for byte the previous behaviour.
func (h *UpdateHandler) packageURL(c *gin.Context, siteID uuid.UUID, rel releaseManifest) (string, error) {
	if h.tokens == nil {
		return h.store.PresignGet(c.Request.Context(), rel.PackageObjectKey, h.presignTTL)
	}
	token, _, err := h.tokens.MintPackageToken(time.Now(), siteID.String(), rel.Version, h.presignTTL)
	if err != nil {
		return "", err
	}
	return h.packageOrigin(c) + PackageRoutePrefix + "/" + siteID.String() +
		"?" + PackageTokenQueryParam + "=" + url.QueryEscape(token), nil
}

// packageOrigin resolves the scheme+host to build package_url against.
//
// A configured base URL always wins. Absent one, the origin is derived from the
// request the AGENT just made, which is by construction the control-plane URL
// that agent was enrolled with and therefore the host it trusts; that fallback
// is what lets a self-hosted install work with nothing configured. The scheme is
// forced to https because the agent refuses any package_url that is not https,
// so emitting http here would only produce a URL it is guaranteed to reject.
func (h *UpdateHandler) packageOrigin(c *gin.Context) string {
	if h.packageBase != "" {
		return h.packageBase
	}
	host := strings.TrimSpace(c.Request.Host)
	if host == "" {
		return ""
	}
	return "https://" + host
}

// errPackageUnauthorized is the single opaque refusal every token failure maps
// to. One code for all of them, on purpose: a caller with no credential learns
// only that it has no credential.
func errPackageUnauthorized() *domain.Error {
	return domain.Unauthorized("update_package_unauthorized", "a valid package download token is required")
}

// packageTokenReason maps a verification error to a short log token. Never
// returned to the caller.
func packageTokenReason(err error) string {
	switch {
	case errors.Is(err, agentcmd.ErrPackageTokenExpired):
		return "expired"
	case errors.Is(err, agentcmd.ErrPackageTokenSignature):
		return "bad_signature"
	case errors.Is(err, agentcmd.ErrPackageTokenClaims):
		return "bad_claims"
	case errors.Is(err, agentcmd.ErrPackageTokenMalformed):
		return "malformed"
	default:
		return "invalid"
	}
}
