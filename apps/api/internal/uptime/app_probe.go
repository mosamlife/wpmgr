// GH #291 Phase 2: the application-health probe.
//
// The FROZEN reachability probe (probe.go) requests the cacheable homepage,
// so a site whose PHP is completely dead but still served a cached 200
// reports "up" forever - the literal incident this phase exists to close.
// Phase 1 already fixed the case where the AGENT can tell us (a disconnected
// agent now derives Degraded, see deriveFleetStatus). This file adds a
// direct measurement so the control plane does not depend on the agent being
// enrolled and disconnected to notice.
//
// Two-signal model (locked, see docs/security/uptime-app-health-design-
// 2026-07-27.md section 1): "up" keeps its exact current meaning forever -
// nothing in this file ever touches site_uptime_probes.up, sites.
// health_status, site_incidents, uptime percentages, or anything probe.go
// computes. "app_up" is a new, orthogonal, three-valued signal (true / false
// / unknown) that this file alone produces.
package uptime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/httpclient"
)

// appProbeMaxBody bounds how much of the app-probe response body is read.
// The WP REST index (/wp-json/) can run to hundreds of KB; only a small
// prefix is needed to tell JSON from an HTML fatal-error page.
const appProbeMaxBody = 16 << 10

// appProbeBusterParam is the query parameter added to every app-probe
// request so a query-string-keyed cache treats it as a new object. Same name
// the design specifies and the Phase 4 post-update probe already uses
// (internal/agentcmd/client.go cacheBusterParam) - NOT a security token, and
// deliberately not utm-adjacent (some managed hosts strip utm_* server-side
// before PHP ever sees them).
const appProbeBusterParam = "wpmgr_hc"

// appProbeUserAgent identifies the app-health probe in a site's access logs,
// distinct from the reachability probe and the cron-kicker.
const appProbeUserAgent = "WPMgr-AppHealthProbe/1.0"

// Machine-readable reasons for an AppProbeResult, so the UI (and a later
// alerting phase) can explain a verdict instead of rendering a bare chip.
const (
	// AppProbeReasonAgentFresh is B0: sites.last_seen_at is fresher than the
	// app-probe interval, so a heartbeat already proves PHP booted. Zero
	// network requests made.
	AppProbeReasonAgentFresh = "agent_fresh"
	// AppProbeReasonRestOK is a 2xx response whose body is a JSON object -
	// the WP REST index (or the operator's B3 override, held to the same
	// bar). app_up=true.
	AppProbeReasonRestOK = "rest_ok"
	// AppProbeReasonRest5xx is HTTP 500 specifically - PHP itself returned an
	// error, which IS the positive evidence of the application running and
	// failing that this signal exists to capture. app_up=false. See the 5xx
	// switch case in fetchAndClassify for the full reasoning on why every
	// OTHER 5xx status (502, 503, 504, and the unenumerated remainder)
	// defaults to UNKNOWN instead.
	AppProbeReasonRest5xx = "rest_5xx"
	// AppProbeReasonRestMaintenance is HTTP 503. UNKNOWN, never false - 503
	// is exactly what WordPress core's own maintenance mode emits (the
	// .maintenance file dropped during ANY core/plugin/theme update, update
	// windows this product itself schedules) and is also the conventional
	// response from a rate limiter or an under-attack challenge page. A
	// fleet-wide managed core-update window would otherwise paint every site
	// application-down purely because this product is doing its job.
	AppProbeReasonRestMaintenance = "rest_maintenance"
	// AppProbeReasonRestBadGateway is HTTP 502. UNKNOWN, never false - a 502
	// means the reverse proxy in front of the site could not reach its
	// upstream at all. That is the SAME condition AppProbeReasonUnreachable
	// already covers when this file's own HTTP client observes a connection
	// refused directly; learning about it secondhand, via the site's own
	// proxy, is not stronger evidence that the application ran and failed -
	// if anything the application never got the chance to run.
	AppProbeReasonRestBadGateway = "rest_bad_gateway"
	// AppProbeReasonRestGatewayTimeout is HTTP 504. UNKNOWN, never false - a
	// 504 is literally a timeout, just reported by the proxy instead of
	// observed directly by this probe's own client. See AppProbeReasonTimeout
	// for why a timeout is never conclusive (a merely slow site is common and
	// healthy); a timeout reported secondhand by the proxy is no more
	// conclusive than one this probe times out on itself.
	AppProbeReasonRestGatewayTimeout = "rest_gateway_timeout"
	// AppProbeReasonRest5xxOther is any 5xx status other than 500 (conclusive
	// false), 502, 503, or 504 (each UNKNOWN with its own reason above).
	// UNKNOWN, never false - see the 5xx switch case in fetchAndClassify for
	// why the default for the rest of the 5xx space, including codes this
	// comment does not enumerate by number (505, 507-509, and Cloudflare's
	// 520-527 origin-error range among them), is UNKNOWN rather than
	// conclusive.
	AppProbeReasonRest5xxOther = "rest_5xx_other"
	// AppProbeReasonWPFatalError is a 2xx response whose body carries the
	// same WordPress fatal-error page signature probe.go's scanFatal
	// detects for the reachability probe (a wp_die()/db-error screen served
	// with HTTP 200). app_up=false.
	AppProbeReasonWPFatalError = "wp_fatal_error"
	// AppProbeReasonCacheHit is a detected cache HIT on the probe response -
	// the bypass was defeated. UNKNOWN, never healthy: silently reporting
	// healthy when the bypass failed would be worse than the bug this phase
	// fixes.
	AppProbeReasonCacheHit = "cache_hit"
	// AppProbeReasonRestForbidden is a 401/403. Extremely common (security
	// plugins, including WPMgr's own suite, commonly gate the REST API).
	// UNKNOWN, never false - a fleet-wide false-alarm storm is worse than an
	// unclassified site.
	AppProbeReasonRestForbidden = "rest_forbidden"
	// AppProbeReasonRestAbsent is a 404 on the default /wp-json/ target -
	// normal on a permalinks-off / REST-disabled install. Triggers the B2
	// fallback; only the FINAL request's 404 (after B2 also 404s, or B2 does
	// not apply - an overridden B3 target) settles as this reason. UNKNOWN,
	// never false.
	AppProbeReasonRestAbsent = "rest_absent"
	// AppProbeReasonRest4xx is any other 4xx status. UNKNOWN, never false -
	// mirrors the 401/404 rule for the long tail of other client-error codes
	// a WAF/security plugin might return.
	AppProbeReasonRest4xx = "rest_4xx"
	// AppProbeReasonRestNonJSON is a 2xx response whose body is not a JSON
	// object. Triggers the B2 fallback; only the FINAL request's non-JSON
	// 200 settles as this reason. UNKNOWN, never false.
	AppProbeReasonRestNonJSON = "rest_non_json"
	// AppProbeReasonTimeout is a request that did not complete within the
	// probe's own budget (the context deadline NewAppProber's timeout sets,
	// or a lower-level dial/TLS timeout that self-reports as one). UNKNOWN,
	// never false: a timeout proves the response did not arrive in time, not
	// that the application ran and failed - a merely SLOW site (a heavy
	// plugin, a cold PHP-FPM worker, a slow upstream) is common and healthy,
	// and reporting it as application-down would be a false positive.
	AppProbeReasonTimeout = "timeout"
	// AppProbeReasonUnreachable is any OTHER transport-level failure
	// (connection refused, DNS failure, TLS handshake failure, an SSRF
	// block, or a request that could not even be built) - see
	// AppProbeReasonTimeout for the timeout case, classified separately.
	// UNKNOWN, never false. This is a deliberate departure from the
	// reachability probe's own posture: a connection refused is arguably
	// conclusive for the SITE as a whole, but that judgment already belongs
	// to probe.go, which independently records the site DOWN. This signal
	// answers a narrower question - did the APPLICATION itself execute and
	// fail? - and a transport failure never proves the application ran at
	// all, so it cannot supply evidence for that question either way. The
	// same false-alarm-storm reasoning that governs 401/403/404 above
	// applies here: prefer UNKNOWN whenever conclusive evidence of the
	// application actually running and failing is unavailable.
	AppProbeReasonUnreachable = "unreachable"
	// AppProbeReasonRestUnexpected covers a response classified by none of
	// the rules above (e.g. an unfollowed 3xx). UNKNOWN, never false.
	AppProbeReasonRestUnexpected = "rest_unexpected"
	// AppProbeReasonInvalidSiteURL is buildURL failing to construct a probe
	// target from the site's stored URL (not http(s), no host, or otherwise
	// unparseable). UNKNOWN, never false - a malformed stored site URL is a
	// configuration problem, not evidence the application ran and failed;
	// naming it distinctly from AppProbeReasonUnreachable also lets an
	// operator see at a glance that this is a config issue to fix on the
	// site record, not an outage to investigate.
	AppProbeReasonInvalidSiteURL = "invalid_site_url"
)

// AppProbeResult is the tri-state verdict from one application-health probe.
// Up is nil for UNKNOWN - never coerced to true or false. Reason is always
// populated (one of the AppProbeReason* constants) whenever a probe actually
// ran, including an unknown verdict; it is the sentinel package metrics'
// UpsertRollup/InsertChecks use to tell "attempted, unknown" apart from "not
// attempted this check" (see metrics.Check.AppProbeReason).
type AppProbeResult struct {
	Up     *bool
	Reason string
}

// AppProber performs the GH #291 Phase 2 application-health probe: B0 (agent
// ground truth, zero network requests) falling through to B1 (/wp-json/),
// B2 (the ?rest_route=/ fallback, only on 404 or non-JSON 200), or B3 (a
// per-site override path that replaces B1/B2 entirely). It reuses the same
// SSRF-hardened *httpclient.Client the reachability prober uses (ADR-009).
type AppProber struct {
	client  *httpclient.Client
	timeout time.Duration
}

// NewAppProber builds an AppProber. timeout bounds a single probe attempt
// (B1 or B2 or the B3 override - never both B1 and B2 combined into one
// deadline; each gets its own context), defaulting to 10s when non-positive.
func NewAppProber(client *httpclient.Client, timeout time.Duration) *AppProber {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &AppProber{client: client, timeout: timeout}
}

// Probe runs B0-B3 for one site, stopping at the first conclusive result.
//
//   - lastSeenAt / appProbeInterval implement B0: when lastSeenAt is fresher
//     than appProbeInterval, a heartbeat already proves PHP booted and
//     WordPress loaded - zero network requests are made. lastSeenAt nil (the
//     site has never heartbeated) always falls through to B1/B2/B3.
//   - overridePath implements B3: when non-empty, it REPLACES the default
//     target entirely (no B2 fallback) - an operator who configured a custom
//     health-check path presumably did so because the default targets are
//     blocked/unreliable for this site, so falling back to them would defeat
//     the point of the override.
func (p *AppProber) Probe(ctx context.Context, siteURL string, lastSeenAt *time.Time, appProbeInterval time.Duration, overridePath string) AppProbeResult {
	// B0: agent ground truth.
	if lastSeenAt != nil && !lastSeenAt.IsZero() && appProbeInterval > 0 && time.Since(*lastSeenAt) < appProbeInterval {
		t := true
		return AppProbeResult{Up: &t, Reason: AppProbeReasonAgentFresh}
	}

	if overridePath != "" {
		target, err := p.buildURL(siteURL, normalizeOverridePath(overridePath), nil)
		if err != nil {
			// A malformed/unparseable stored site URL is a configuration
			// problem, not evidence the application ran and failed - see
			// AppProbeReasonInvalidSiteURL. UNKNOWN, never a conclusive false.
			return AppProbeResult{Up: nil, Reason: AppProbeReasonInvalidSiteURL}
		}
		up, reason, _ := p.fetchAndClassifyWithTimeout(ctx, target)
		return AppProbeResult{Up: up, Reason: reason}
	}

	// B1.
	b1URL, err := p.buildURL(siteURL, "/wp-json/", nil)
	if err != nil {
		// Same reasoning as the B3 override branch above: a bad site URL is
		// a config problem, never conclusive evidence of app failure.
		return AppProbeResult{Up: nil, Reason: AppProbeReasonInvalidSiteURL}
	}
	up, reason, tryFallback := p.fetchAndClassifyWithTimeout(ctx, b1URL)
	if !tryFallback {
		return AppProbeResult{Up: up, Reason: reason}
	}

	// B2 fallback: only on 404 or a non-JSON 200 from B1 (see
	// fetchAndClassify's tryFallback return). Gets its OWN fresh timeout
	// budget below (fetchAndClassifyWithTimeout), independent of however
	// long B1 already took.
	b2URL, err := p.buildURL(siteURL, "/", map[string]string{"rest_route": "/"})
	if err != nil {
		// Could not build the fallback URL: report B1's own (inconclusive)
		// verdict rather than discarding it.
		return AppProbeResult{Up: up, Reason: reason}
	}
	up2, reason2, _ := p.fetchAndClassifyWithTimeout(ctx, b2URL)
	return AppProbeResult{Up: up2, Reason: reason2}
}

// fetchAndClassifyWithTimeout wraps fetchAndClassify with a FRESH
// context.WithTimeout derived from ctx (the caller's own context, NEVER
// chained off a previous attempt's already-partially-consumed deadline), so
// B1, B2, and the B3 override each get the FULL p.timeout budget for their
// own single attempt - exactly the contract NewAppProber's doc comment
// states. Before this, Probe built ONE shared context up front and reused it
// for every attempt, so a slow B1 could leave B2 with almost no time
// remaining and produce a misleading verdict from a starved request rather
// than a genuine one. The tradeoff: Probe's worst-case wall-clock duration is
// now bounded by (attempts made) * p.timeout, not by p.timeout alone - at
// most 2*p.timeout today, since a sweep tick that reaches B2 makes exactly
// two attempts, and the B3 override path makes exactly one.
func (p *AppProber) fetchAndClassifyWithTimeout(ctx context.Context, targetURL string) (up *bool, reason string, tryFallback bool) {
	attemptCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	return p.fetchAndClassify(attemptCtx, targetURL)
}

// normalizeOverridePath ensures a per-site override path (sites.
// app_probe_path) is treated as a path, not a bare filename - a leading
// slash is required by buildURL's join.
func normalizeOverridePath(p string) string {
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

// buildURL joins extraPath (which must start with "/") onto siteURL's
// origin, merges query (if any), and appends a fresh appProbeBusterParam
// value. siteURL must be http(s) with a host - the same validation
// joinCommandURL applies to agent command URLs.
//
// Deliberately NOT path.Join: that function calls path.Clean, which strips a
// trailing slash - turning "/wp-json/" into "/wp-json". WP's REST index only
// answers reliably at the trailing-slash form the design specifies
// (GET <site>/wp-json/?wpmgr_hc=...); a bare "/wp-json" is a different route
// on some configurations. Plain concatenation onto a trailing-slash-trimmed
// base path preserves extraPath's own trailing slash exactly as given.
func (p *AppProber) buildURL(siteURL, extraPath string, query map[string]string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(siteURL))
	if err != nil {
		return "", fmt.Errorf("invalid site url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid site url scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("site url has no host")
	}
	u.Path = strings.TrimRight(u.Path, "/") + extraPath
	q := u.Query()
	for k, v := range query {
		q.Set(k, v)
	}
	q.Set(appProbeBusterParam, randomBusterToken())
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String(), nil
}

// randomBusterToken returns a fresh, unpredictable-enough token for the
// cache-buster query parameter - it is not a security token, it only needs
// to vary per call so a query-string-keyed cache treats each probe as a new
// object. Falls back to a fixed value only if crypto/rand is somehow
// unavailable (never observed in practice; keeps buildURL infallible).
func randomBusterToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "wpmgrhc"
	}
	return hex.EncodeToString(b[:])
}

// fetchAndClassify issues one GET to targetURL and classifies the result.
// tryFallback is true only when the caller should retry via B2 - a 404, or a
// 2xx whose body is not JSON (see the AppProbeReason* doc comments for the
// full classification table).
func (p *AppProber) fetchAndClassify(ctx context.Context, targetURL string) (up *bool, reason string, tryFallback bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		// A request that cannot even be constructed is a local/config
		// problem (targetURL already passed buildURL's own validation, so
		// this is effectively unreachable in practice) - never evidence the
		// application ran and failed. UNKNOWN, same reasoning as the
		// client.Do() branch below.
		return nil, AppProbeReasonUnreachable, false
	}
	req.Header.Set("User-Agent", appProbeUserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")

	resp, err := p.client.HTTPClient().Do(req)
	if err != nil {
		if isTimeoutError(err) {
			// A timeout proves only that the response did not arrive within
			// the probe's budget - see AppProbeReasonTimeout. A merely SLOW
			// site is common and healthy; reporting it as application-down
			// would be a false positive the moment Phase 3 turns verdicts
			// into pages.
			return nil, AppProbeReasonTimeout, false
		}
		// Every other transport-level failure (DNS failure, TLS handshake
		// failure, connection refused, an SSRF block) lands here - see
		// AppProbeReasonUnreachable's doc comment for why a connection
		// refused is deliberately UNKNOWN rather than conclusive false: it
		// is the reachability prober's job (probe.go) to declare the site
		// unreachable, and a transport failure never proves the application
		// itself ran and failed.
		return nil, AppProbeReasonUnreachable, false
	}
	defer func() { _ = resp.Body.Close() }()

	// A detected cache HIT means UNKNOWN, never healthy - the bypass was
	// defeated, and silently reporting healthy in that case is worse than
	// the bug this phase exists to fix. Checked before status/body
	// classification: an HTTP 200 that is actually a stale cached object
	// proves nothing about the current backend.
	if hit, _ := agentcmd.DetectCacheHit(resp.Header); hit {
		return nil, AppProbeReasonCacheHit, false
	}

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, io.LimitReader(resp.Body, appProbeMaxBody))
	body := buf.Bytes()

	switch {
	case resp.StatusCode == http.StatusServiceUnavailable:
		// 503 is exactly what WordPress core's own maintenance mode emits
		// (the .maintenance file, dropped during ANY core/plugin/theme
		// update - including an update window this product itself
		// schedules), and is also the conventional response from a rate
		// limiter or an under-attack challenge page. Must be checked BEFORE
		// the generic >=500 case below, or a fleet-wide managed update
		// window would paint every site application-down purely because
		// this product is doing its job. A Retry-After header (RFC 9110) or
		// a challenge-page marker in the body would be even stronger
		// evidence of a deliberate temporary state, but the safe default -
		// UNKNOWN on every 503, regardless of headers/body - is sufficient
		// on its own and keeps this branch simple.
		return nil, AppProbeReasonRestMaintenance, false
	case resp.StatusCode == http.StatusBadGateway:
		// 502: the reverse proxy in front of the site could not reach its
		// upstream at all - see AppProbeReasonRestBadGateway. That is the
		// SAME condition this file's own HTTP client classifies as
		// AppProbeReasonUnreachable when it observes a connection refused
		// directly (below); observing it secondhand via the site's own proxy
		// is not stronger evidence. UNKNOWN.
		return nil, AppProbeReasonRestBadGateway, false
	case resp.StatusCode == http.StatusGatewayTimeout:
		// 504 is literally a timeout, reported by the proxy - see
		// AppProbeReasonRestGatewayTimeout / AppProbeReasonTimeout. A
		// merely SLOW site is common and healthy; this client's own timeout
		// on the same request is UNKNOWN, so a proxy reporting the identical
		// condition cannot be treated as stronger evidence. UNKNOWN.
		return nil, AppProbeReasonRestGatewayTimeout, false
	case resp.StatusCode == http.StatusInternalServerError:
		// 500: PHP itself returned an error. This IS the positive evidence
		// that the application ran and failed that this whole signal exists
		// to capture, so it is the one 5xx status that stays conclusive.
		f := false
		return &f, AppProbeReasonRest5xx, false
	case resp.StatusCode >= 500:
		// Every 5xx status NOT already handled above (501, 505, 507-509, and
		// Cloudflare's 520-527 origin-error range among them - this default
		// deliberately does not hand-enumerate every vendor-specific 5xx a
		// proxy/CDN/WAF in front of a site might emit, since that list would
		// inevitably miss one) defaults to UNKNOWN rather than conclusive.
		// Cloudflare's 520-527 range in particular documents itself as an
		// ORIGIN-unreachable/misbehaving condition at the network layer, the
		// same category as 502/504 above, not a PHP error like 500. The cost
		// asymmetry favours under-alarming here: an unrecognised 5xx staying
		// UNKNOWN costs at most a missed page for a rare code, while reading
		// a network-layer proxy error as "the application ran and failed"
		// when it may never have gotten the chance to run risks the false
		// positive this whole file exists to eliminate.
		return nil, AppProbeReasonRest5xxOther, false
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// 4xx is NOT app-down: a 401 rest_not_logged_in is extremely common
		// (security plugins, including WPMgr's own suite, gate the REST
		// API) and treating it as down would cause a fleet-wide
		// false-alarm storm.
		return nil, AppProbeReasonRestForbidden, false
	case resp.StatusCode == http.StatusNotFound:
		// Normal on a REST-disabled/permalinks-off install; try B2 before
		// settling.
		return nil, AppProbeReasonRestAbsent, true
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if looksLikeJSONObject(body) {
			t := true
			return &t, AppProbeReasonRestOK, false
		}
		// A WordPress fatal-error/db-error screen only reaches the client as
		// HTTP 200 once headers have already been sent - reuse the SAME
		// structural scan the (frozen) reachability probe uses, rather than
		// a second, looser detector that could disagree with it (probe.go
		// is same-package; this does not modify it).
		if down, _ := scanFatal(body, resp.Header.Get("Content-Type")); down {
			f := false
			return &f, AppProbeReasonWPFatalError, false
		}
		return nil, AppProbeReasonRestNonJSON, true
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return nil, AppProbeReasonRest4xx, false
	default:
		return nil, AppProbeReasonRestUnexpected, false
	}
}

// isTimeoutError reports whether err represents a request that did not
// complete within its deadline - the probe's own context.WithTimeout
// expiring (surfaces as context.DeadlineExceeded, possibly wrapped in a
// *url.Error by net/http) or a lower-level net.Error that self-reports
// Timeout()==true (e.g. a dial timeout on a slow/black-holed host) - as
// opposed to a definite connection-level failure (refused, DNS, TLS, an SSRF
// block), which is NOT a timeout even though it is equally a client.Do()
// error.
func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

// looksLikeJSONObject reports whether body's PREFIX (it may be truncated at
// appProbeMaxBody - the WP REST index can run to hundreds of KB) begins a
// well-formed JSON object. json.Decoder.Token only needs to see the opening
// "{" to succeed; it does not require the rest of the document to be
// present, so this is safe to call on a deliberately truncated read.
func looksLikeJSONObject(body []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return false
	}
	d, ok := tok.(json.Delim)
	return ok && d == '{'
}

// appProbeDue is the stateless, restart-safe cadence check that lets the app
// probe piggyback on the reachability sweep's existing 60s tick without
// keeping any cadence state in memory or in the database. Given the sweep's
// own timestamp (now - the same value uptime.ProbeWorker.Sweep already
// receives and uses for every site in the sweep) and the two configured
// intervals, it deterministically decides which sites are "due" THIS tick:
//
//   - ratio = round(appProbeInterval / probeInterval) - at the documented
//     defaults (300s / 60s) this is 5, matching the design's "roughly 4 of
//     every 5 rollup upserts carry app_up = NULL" arithmetic exactly: 1 of
//     every `ratio` sweeps performs an app probe for a given site, the other
//     `ratio`-1 do not.
//   - seq is the sweep's own tick number (now truncated to whole
//     probeInterval units) - every process computes the SAME seq from the
//     SAME now, with no shared state and no coordination required.
//   - offset is derived from the site's own UUID, spreading sites evenly
//     across the `ratio` buckets instead of every site coming due on the
//     exact same tick (which would turn the app probe into a periodic
//     thundering herd instead of a steady trickle).
//
// A process restart loses no state: the next tick's seq is still correctly
// derived from wall-clock time, so at worst a site's app probe is delayed by
// up to one ratio-worth of ticks - an acceptable, self-healing imprecision
// for a "roughly" cadence (see the design doc), not a correctness bug.
func appProbeDue(siteID uuid.UUID, now time.Time, probeInterval, appProbeInterval time.Duration) bool {
	if probeInterval <= 0 {
		probeInterval = time.Minute
	}
	if appProbeInterval <= 0 {
		appProbeInterval = 5 * time.Minute
	}
	ratio := int64(appProbeInterval / probeInterval)
	if ratio < 1 {
		ratio = 1
	}
	// probeInterval.Seconds() truncates to whole seconds via the int64
	// conversion below, so any sub-second probeInterval (already floored to
	// time.Minute above only when it was <= 0, not when it is merely small)
	// would otherwise make this divisor 0 and panic. Floor it at 1 second -
	// the same self-healing "roughly" cadence tolerance appProbeDue's own doc
	// comment already accepts for a process restart.
	divisor := int64(probeInterval.Seconds())
	if divisor < 1 {
		divisor = 1
	}
	seq := now.Unix() / divisor

	var h uint64
	for i := 0; i < len(siteID) && i < 8; i++ {
		h = h<<8 | uint64(siteID[i])
	}
	offset := int64(h % uint64(ratio))

	return (seq+offset)%ratio == 0
}
