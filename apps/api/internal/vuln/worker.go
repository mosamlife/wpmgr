package vuln

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

// ---------------------------------------------------------------------------
// Feed ingester job
// ---------------------------------------------------------------------------

// FeedRefreshQueue is the River queue for the vulnerability feed refresh job.
const FeedRefreshQueue = "vuln_feed_refresh"

// FeedRefreshArgs is the River job payload for the hourly feed refresh.
type FeedRefreshArgs struct{}

// Kind implements river.JobArgs.
func (FeedRefreshArgs) Kind() string { return "vuln_feed_refresh" }

// ---------------------------------------------------------------------------
// Per-site rescan job
// ---------------------------------------------------------------------------

// RescanSiteQueue is the River queue for per-site rescan jobs.
const RescanSiteQueue = "vuln_rescan_site"

// RescanSiteArgs is the River job payload for a per-site vulnerability rescan.
type RescanSiteArgs struct {
	TenantID uuid.UUID `json:"tenant_id"`
	SiteID   uuid.UUID `json:"site_id"`
}

// Kind implements river.JobArgs.
func (RescanSiteArgs) Kind() string { return "vuln_rescan_site" }

// ---------------------------------------------------------------------------
// Wordfence Intelligence V3 feed URLs — Scanner/Production alternation (GH #245)
// ---------------------------------------------------------------------------

// The Scanner feed carries the minimal detection-critical data (affected
// versions, patched, severity) and is the authoritative, superset ID catalog
// — it determines which vulnerabilities EXIST and which software they match.
// The Production feed additionally carries CVSS scores, CVE identifiers,
// remediation text, and a richer copyrights block; its ID set is a proper
// SUBSET of Scanner's (it omits brand-new "being-researched" unrated
// entries), so it is used only to ENRICH rows that Scanner already created —
// never to determine existence.
//
// Wordfence Intelligence v3 enforces ~1 request / 30 minutes GLOBALLY per API
// key. This job runs hourly; fetching BOTH feeds in the same run (even with a
// short inter-fetch delay) deterministically 429'd the second fetch every
// cycle, so Production — and therefore all CVSS/CVE enrichment — never landed
// (GH #245: every finding fell back to a fabricated "low" severity, hiding a
// real CVSS 9.8 core RCE). The fix: alternate feeds across successive runs via
// a persisted cursor (Repo.GetFeedGate / StampFeedMeta) so each run issues
// exactly ONE request, comfortably inside the rate-limit window.
//
// Follow-up (GH #245 post-deploy): the alternation above relied on the FALSE
// assumption that consecutive runs are ~1h apart. In prod, a manually
// triggered sync (admin.VulnFeedKeyService.TriggerSync, deduped on its own
// 5-min ByPeriod window — distinct from the hourly periodic job's 1h ByPeriod
// window, so River does NOT treat them as duplicates of each other) landed
// only 6 minutes after the periodic tick, so the second (Production) request
// still fell inside the 30-min window and 429'd — and because the cursor used
// to advance unconditionally, Production was abandoned for a full cycle
// instead of retried. Fix: Work() now enforces the spacing by WALL-CLOCK
// (Repo.GetFeedGate.LastRequestAt, stamped on every actual request — 200 or
// 429), skipping the entire run (zero requests) when too soon, and the
// cursor advances ONLY inside StampFeedMeta after a successful, non-empty
// ingest — never on a skip, a 429, or any other failure — so an abandoned
// feed is always retried on the next eligible run instead of being skipped
// over.
const (
	ScannerFeedURL    = "https://www.wordfence.com/api/intelligence/v3/vulnerabilities/scanner"
	ProductionFeedURL = "https://www.wordfence.com/api/intelligence/v3/vulnerabilities/production"
)

// Feed kind identifiers — mirror the wordfence_vuln_feed_meta.next_feed_kind
// CHECK constraint values.
const (
	FeedKindScanner    = "scanner"
	FeedKindProduction = "production"
)

// minRequestSpacing is the minimum wall-clock gap Work() enforces between two
// ACTUAL Wordfence HTTP requests, regardless of job/trigger cadence. Wordfence
// enforces ~30 minutes; the extra 1-minute margin absorbs clock/scheduling
// jitter so a request is never dispatched right at the edge of the window.
const minRequestSpacing = 31 * time.Minute

// FeedFetchTimeout bounds a single Wordfence Intelligence HTTP fetch (Scanner
// or Production) via the REQUEST CONTEXT — not the shared httpclient's
// per-request Timeout (cfg.Update.HTTPTimeout, 30s in cmd/wpmgr/main.go),
// which is tuned for unrelated agent/site traffic and far too short here.
//
// Pre-streaming-fix, a large Production response OOM'd the pod ~10s in —
// well before 30s could even matter. Post-streaming-fix it no longer OOMs,
// so it downloads the FULL feed, which for Production (far richer per-record
// than Scanner) can easily exceed 30s; without a dedicated budget the very
// same 30s cap would now fail it a different way (mid-stream timeout instead
// of OOM). FeedFetchTimeout gives the fetch up to 8 minutes, comfortably
// under the River job's 10-minute Timeout (2 minutes of margin for the
// DB flushes — see ingestRecords/BulkUpsertFeedRecordsPool, which typically
// complete in low single-digit seconds even for thousands of records).
//
// cmd/wpmgr/main.go pairs this with a DEDICATED SSRF-hardened httpclient.Client
// (httpclient.New(Config{Timeout: FeedFetchTimeout, ...})) built the SAME way
// as the existing update-apply/backup/media dedicated-client precedent
// (buildUpdateApplyCommander et al.) — never the shared 30s ssrfClient — so
// the client's own Timeout cannot undercut this context deadline either.
// Applying context.WithTimeout here too (in fetchFeed/fetchAndIngestProduction)
// is defense-in-depth AND keeps the ~8m budget self-evident and unit-testable
// from this package without depending on how main.go wires the client.
const FeedFetchTimeout = 8 * time.Minute

// errRateLimited marks a 429 from the Wordfence feed. Wordfence documents no
// Retry-After and no per-window number ("too many requests in a short period"),
// so a 429 must NOT trigger an immediate in-process retry — that just re-hits
// the endpoint and keeps the rate-limit window warm. Instead we stamp the
// status and succeed; the next ELIGIBLE run (wall-clock spaced, see
// minRequestSpacing) is the natural, well-spaced retry of the SAME feed (the
// cursor does not advance on a 429).
var errRateLimited = errors.New("wordfence feed rate limited (429)")

// FeedHTTPDoer is the subset of httpclient.Client the feed worker needs.
type FeedHTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// APIKeyResolver resolves the Wordfence Intelligence API key at job-run time.
// Priority: UI-stored instance setting (encrypted at rest) > WPMGR_WORDFENCE_API_KEY env > "".
// The concrete implementation lives in the admin package (to avoid an import
// cycle: vuln→admin is fine; admin→vuln is already fine).
type APIKeyResolver interface {
	// ResolveAPIKey returns the effective API key and the source ("ui"|"env"|"none").
	// Returns ("", "none") when no key is configured; never returns an error (logs internally).
	ResolveAPIKey(ctx context.Context) (key, source string)
}

// staticKeyResolver satisfies APIKeyResolver with a fixed key (env-only path,
// used when no admin.KeyStore is wired — e.g. unit tests or pre-m80 boot).
type staticKeyResolver struct{ key string }

func (s *staticKeyResolver) ResolveAPIKey(_ context.Context) (string, string) {
	if s.key == "" {
		return "", "none"
	}
	return s.key, "env"
}

// NewStaticKeyResolver wraps a plain API key string in an APIKeyResolver.
// Used in tests and as the fallback path in main before the admin store is wired.
func NewStaticKeyResolver(key string) APIKeyResolver { return &staticKeyResolver{key: key} }

// FeedWorker handles the hourly Wordfence Intelligence feed refresh.
type FeedWorker struct {
	river.WorkerDefaults[FeedRefreshArgs]
	repo     *Repo
	pool     *db.Pool
	svc      *Service
	resolver APIKeyResolver // resolves UI-stored key > env key at runtime
	client   FeedHTTPDoer
	logger   *slog.Logger
}

// NewFeedWorker builds a FeedWorker. resolver must not be nil; use
// NewStaticKeyResolver("") for the no-key case. The worker no-ops cleanly when
// resolver returns ("", "none") so self-hosters without a key do not crash.
func NewFeedWorker(repo *Repo, pool *db.Pool, svc *Service, resolver APIKeyResolver, client FeedHTTPDoer, logger *slog.Logger) *FeedWorker {
	if resolver == nil {
		resolver = &staticKeyResolver{}
	}
	return &FeedWorker{
		repo:     repo,
		pool:     pool,
		svc:      svc,
		resolver: resolver,
		client:   client,
		logger:   logger,
	}
}

// SetService wires the vuln service into the worker after construction. Called
// once at boot after startRiver returns (the service needs the River client).
func (w *FeedWorker) SetService(svc *Service) { w.svc = svc }

// Timeout gives the feed refresh job 10 minutes — comfortably above
// FeedFetchTimeout (8 minutes, the cap on the HTTP fetch itself) with a
// 2-minute margin for the DB flushes that follow (BulkUpsertFeedRecordsPool/
// ingestRecords typically complete in low single-digit seconds even for
// thousands of records — see TestBulkIngest_Scale). A full-dump ingest of
// ~13k records via CopyFrom completes in seconds; the previous per-record
// loop is what originally needed this much headroom, and it still comfortably
// covers the streaming Production path (incremental flushes interleaved with
// the download, rather than one big end-of-run write).
//
// This Timeout bounds the WHOLE River job. It is deliberately independent of
// (and larger than) FeedFetchTimeout, which bounds only the HTTP fetch via
// the request context (see fetchFeed/fetchAndIngestProduction) — and is in
// turn independent of the *http.Client's own per-request Timeout: the vuln
// feed worker is wired in cmd/wpmgr/main.go with a DEDICATED SSRF-hardened
// client (httpclient.New(Config{Timeout: FeedFetchTimeout, ...}), built the
// same way as the update-apply/backup/media dedicated-client precedent — see
// buildUpdateApplyCommander), never the shared 30s ssrfClient used for
// unrelated agent/update traffic. Using that 30s-capped client here would
// have made Production fail by timeout instead of by OOM once the streaming
// fix stopped it from OOM'ing first.
func (w *FeedWorker) Timeout(*river.Job[FeedRefreshArgs]) time.Duration {
	return 10 * time.Minute
}

// Work performs the feed refresh: at most ONE Wordfence Intelligence HTTP
// request per invocation, alternating Scanner/Production across successive
// ELIGIBLE runs (see the package-level doc comment above the feed URL
// constants and Repo.GetFeedGate). Eligibility is gated by WALL-CLOCK spacing
// since the last actual request, not by an assumption about job cadence —
// consecutive runs (periodic + a manually triggered sync, or any other
// clustering) can land far closer together than the nominal hourly interval.
//
// The two feed kinds use DIFFERENT ingest strategies (workScanner /
// workProduction below): Scanner is small enough (~37k MINIMAL records) to
// buffer entirely in memory and ingest in one atomic transaction, and needs
// the full record set anyway for BulkReplaceAllSoftware/PruneMissingVulns.
// Production carries a much richer per-record payload (full CVSS/CVE/CWE/
// copyrights/remediation/raw JSON) and is enrichment-only (no prune), so it
// is STREAMED and flushed in bounded batches — buffering the whole Production
// response in memory once let a request through for the first time (after
// the wall-clock spacing fix) and OOM'd the pod (SIGKILL, prod incident).
func (w *FeedWorker) Work(ctx context.Context, job *river.Job[FeedRefreshArgs]) error {
	// Resolve the key at run-time so a UI-set key takes effect on the next job
	// without requiring a restart. Priority: UI key > env key > no-op.
	apiKey, source := w.resolver.ResolveAPIKey(ctx)
	if apiKey == "" {
		w.logger.Debug("vuln: no API key configured; feed refresh skipped",
			slog.String("source", source))
		return nil
	}
	_ = source // used only for debug logging above

	// Read (never mutate) the alternation cursor + last-request timestamp.
	// Fall back to Scanner/no-prior-request on a read error so a meta-row
	// hiccup never wedges the worker.
	gate, gerr := w.repo.GetFeedGate(ctx)
	feedKind := FeedKindScanner
	if gerr != nil {
		w.logger.Warn("vuln: failed to read feed gate; defaulting to scanner",
			slog.Any("error", gerr))
	} else {
		feedKind = gate.FeedKind
		if gate.LastRequestAt != nil {
			if elapsed := time.Since(*gate.LastRequestAt); elapsed < minRequestSpacing {
				// Make NO Wordfence request this cycle and leave the cursor
				// exactly where it is: the next eligible run retries the SAME
				// feed kind, so nothing is ever abandoned by a too-soon run.
				w.logger.Info("vuln: skipping feed refresh, <31m since last Wordfence request (rate-limit spacing)",
					slog.Float64("elapsed_minutes", elapsed.Minutes()))
				return nil
			}
		}
	}

	w.logger.Info("vuln: starting Wordfence Intelligence feed refresh", slog.String("feed", feedKind))

	if feedKind == FeedKindProduction {
		return w.workProduction(ctx, apiKey)
	}
	return w.workScanner(ctx, apiKey)
}

// stampRequestIfMade records last_request_at when requested is true — a
// response (of any kind: 200, 429, or otherwise) was actually received from
// Wordfence's server, which is what the wall-clock spacing gate in Work()
// needs to see on the next run. A pure transport failure (requested=false)
// never reached Wordfence and must not advance the gate.
func (w *FeedWorker) stampRequestIfMade(ctx context.Context, requested bool) {
	if !requested {
		return
	}
	if serr := w.repo.StampRequestAt(ctx, time.Now().UTC()); serr != nil {
		w.logger.Warn("vuln: failed to stamp last_request_at", slog.Any("error", serr))
	}
}

// handleFetchError applies the shared rate-limit / hard-error handling common
// to both feed paths (including a mid-stream Production failure, which
// surfaces here as a plain non-errRateLimited error). Returns nil for a
// rate-limit (no River retry; the cursor is never touched by either branch —
// StampEnrichmentDegraded/StampFeedError do not reference next_feed_kind) or
// the wrapped error for a hard failure (River retries; the cursor still
// never advances because StampFeedMeta/StampFeedMetaFinal — the only calls
// that flip it — are never reached on this path).
func (w *FeedWorker) handleFetchError(ctx context.Context, feedKind string, err error) error {
	if errors.Is(err, errRateLimited) {
		msg := fmt.Sprintf("%s feed rate limited by Wordfence; will retry on the next eligible refresh", feedKind)
		if feedKind == FeedKindProduction {
			_ = w.repo.StampEnrichmentDegraded(ctx, msg)
		} else {
			_ = w.repo.StampFeedError(ctx, msg)
		}
		w.logger.Warn("vuln: feed rate limited; skipping this cycle (no in-window retry)",
			slog.String("feed", feedKind))
		return nil
	}
	errMsg := fmt.Sprintf("%s feed fetch failed: %v", feedKind, err)
	_ = w.repo.StampFeedError(ctx, errMsg)
	return fmt.Errorf("vuln feed refresh (%s): %w", feedKind, err) // River will retry real failures
}

// handleZeroRecords applies the shared zero-record-response handling. A
// zero-record Production response degrades enrichment_ok only (detection
// freshness is untouched); a zero-record Scanner response means there is no
// usable detection catalog this cycle (ok=false, matching prior behavior).
// Neither advances the cursor.
func (w *FeedWorker) handleZeroRecords(ctx context.Context, feedKind string) {
	msg := fmt.Sprintf("%s feed returned zero records; not applying update", feedKind)
	if feedKind == FeedKindProduction {
		_ = w.repo.StampEnrichmentDegraded(ctx, msg)
	} else {
		_ = w.repo.StampFeedError(ctx, msg)
	}
	w.logger.Warn("vuln: " + msg)
}

// triggerRescanAll fans out per-site rescans after a successful ingest of
// either feed: a Scanner ingest carries new/changed detection data, and a
// Production ingest can land CVSS that upgrades findings out of the
// "unknown" severity bucket — both should become visible promptly.
func (w *FeedWorker) triggerRescanAll(ctx context.Context) {
	if err := w.svc.RescanAll(ctx, uuid.Nil); err != nil {
		w.logger.Warn("vuln: post-feed rescan-all enqueue failed", slog.Any("error", err))
	}
}

// workScanner fetches and ingests the Scanner feed: buffer the whole
// (minimal, ~37k-record) response in memory, then a single atomic transaction
// (BulkUpsertFeedRecords + BulkReplaceAllSoftware + PruneMissingVulns +
// StampFeedMeta — see ingestRecords). Scanner is small enough that buffering
// is fine, and it NEEDS the full record set in one pass for prune to be
// correct (see ingestRecords doc).
func (w *FeedWorker) workScanner(ctx context.Context, apiKey string) error {
	records, defiantNotice, defiantLicense, mitreNotice, requested, err := w.fetchFeed(ctx, ScannerFeedURL, apiKey)
	w.stampRequestIfMade(ctx, requested)
	if err != nil {
		return w.handleFetchError(ctx, FeedKindScanner, err)
	}
	if len(records) == 0 {
		w.handleZeroRecords(ctx, FeedKindScanner)
		return nil
	}

	knownIDs := make([]string, 0, len(records))
	for id := range records {
		knownIDs = append(knownIDs, id)
	}

	meta := FeedMetaUpdate{
		FetchedAt:      time.Now().UTC(),
		OK:             true,
		RecordCount:    len(records),
		DefiantNotice:  defiantNotice,
		DefiantLicense: defiantLicense,
		MitreNotice:    mitreNotice,
	}

	// ingestRecords calls StampFeedMeta, which — ONLY on this successful,
	// non-empty-ingest path — also advances next_feed_kind to production.
	if pgErr := w.ingestRecords(ctx, records, knownIDs, meta); pgErr != nil {
		_ = w.repo.StampFeedError(ctx, pgErr.Error())
		return fmt.Errorf("vuln: ingest records (%s): %w", FeedKindScanner, pgErr)
	}

	w.logger.Info("vuln: feed refresh complete",
		slog.String("feed", FeedKindScanner), slog.Int("records", len(records)))
	w.triggerRescanAll(ctx)
	return nil
}

// workProduction streams and ingests the Production feed in bounded batches
// (fetchAndIngestProduction) rather than buffering the whole response — see
// the package/Work doc comments for why. On success it stamps enrichment_ok +
// last_enrichment_at and advances the cursor (StampFeedMetaFinal); on any
// failure (rate limit, zero records, or a mid-stream error) it falls through
// to the shared handlers, none of which touch the cursor.
func (w *FeedWorker) workProduction(ctx context.Context, apiKey string) error {
	n, defiantNotice, defiantLicense, mitreNotice, requested, err := w.fetchAndIngestProduction(ctx, apiKey)
	w.stampRequestIfMade(ctx, requested)
	if err != nil {
		return w.handleFetchError(ctx, FeedKindProduction, err)
	}
	if n == 0 {
		w.handleZeroRecords(ctx, FeedKindProduction)
		return nil
	}

	now := time.Now().UTC()
	enrichOK := true
	meta := FeedMetaUpdate{
		FetchedAt:        now,
		OK:               true,
		RecordCount:      n,
		DefiantNotice:    defiantNotice,
		DefiantLicense:   defiantLicense,
		MitreNotice:      mitreNotice,
		EnrichmentOK:     &enrichOK,
		LastEnrichmentAt: &now,
	}

	// This is what advances next_feed_kind to scanner — only reached once
	// every batch has already been durably flushed by
	// fetchAndIngestProduction, so a mid-stream failure (returned as err
	// above) can never reach here.
	if serr := w.repo.StampFeedMetaFinal(ctx, meta); serr != nil {
		_ = w.repo.StampFeedError(ctx, serr.Error())
		return fmt.Errorf("vuln: stamp feed meta (%s): %w", FeedKindProduction, serr)
	}

	w.logger.Info("vuln: feed refresh complete",
		slog.String("feed", FeedKindProduction), slog.Int("records", n))
	w.triggerRescanAll(ctx)
	return nil
}

// ingestRecords writes all Scanner records and the meta row in one
// transaction using bulk operations. Scanner-ONLY: Production streams and
// flushes incrementally instead (see fetchAndIngestProduction /
// BulkUpsertFeedRecordsPool / StampFeedMetaFinal) since it is enrichment-only
// and does not need BulkReplaceAllSoftware/PruneMissingVulns, which require
// the FULL Scanner ID set in one pass to be correct. The previous per-record
// loop (13k × DELETE+INSERT round-trips) exceeded the River context deadline;
// the bulk path replaces that with:
//
//  1. A pgx Batch that sends all feed-row upserts in a single round-trip. The
//     upsert is STICKY for the enrichment columns (cvss_score, cvss_rating,
//     cve, cve_link, cwe, reference_urls — see Repo.BulkUpsertFeedRecords): a
//     Scanner run's records typically carry no CVSS data and no references at
//     all, and a blind overwrite would erase whatever a prior Production run
//     had already populated, flapping every finding's severity AND reference
//     link-outs between real values and empty/"unknown" every other hour
//     (GH #245 reintroduced).
//  2. A set-based DELETE of software rows for every vuln_id in the batch,
//     then a single CopyFrom that streams all new software rows to Postgres.
//  3. PruneMissingVulns (single set-based DELETE of retracted vulns) — safe
//     here because Scanner's ID set is the authoritative superset.
//  4. StampFeedMeta (single UPDATE) — advances next_feed_kind to production.
//
// The entire operation is one transaction — atomic, with no partial state.
func (w *FeedWorker) ingestRecords(ctx context.Context, records map[string]FeedRecord, knownIDs []string, meta FeedMetaUpdate) error {
	// Flatten the map into a slice for deterministic ordering (maps in Go are
	// unordered; a slice makes the Batch and CopyFrom reproducible for debugging).
	recs := make([]FeedRecord, 0, len(records))
	for _, r := range records {
		recs = append(recs, r)
	}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Set agent GUC (global tables have no RLS today; this is forward-compatible).
	if _, err := tx.Exec(ctx, "SELECT set_config('app.agent','on',true)"); err != nil {
		return fmt.Errorf("set agent guc: %w", err)
	}

	if err := w.repo.BulkUpsertFeedRecords(ctx, tx, recs); err != nil {
		return err
	}
	if err := w.repo.BulkReplaceAllSoftware(ctx, tx, recs); err != nil {
		return err
	}
	if err := w.repo.PruneMissingVulns(ctx, tx, knownIDs); err != nil {
		return err
	}
	if err := w.repo.StampFeedMeta(ctx, tx, meta); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// fetchFeed downloads and parses a Wordfence Intelligence V3 feed URL.
// The V3 feed is a JSON object keyed by vuln UUID: { "<uuid>": { ... }, ... }.
//
// Returned values: records map, defiant notice, defiant license, mitre
// notice, requested, error. requested reports whether a response was actually
// received from Wordfence's server (true for EVERY status code — 200, 429,
// 401/403, 5xx, or any other — since all of those represent a completed
// network round-trip that counts against the shared ~30-min rate-limit slot).
// requested is false only when the request never reached Wordfence at all
// (building the *http.Request failed, or the transport-level Do() call itself
// errored — DNS/connect/timeout before any response). The caller
// (FeedWorker.Work) uses requested to decide whether to stamp last_request_at.
func (w *FeedWorker) fetchFeed(ctx context.Context, feedURL, apiKey string) (records map[string]FeedRecord, defiantNotice, defiantLicense, mitreNotice string, requested bool, err error) {
	// Bound the fetch (request + full body read/decode, which happens
	// synchronously below before this function returns) to FeedFetchTimeout —
	// NOT the shared httpclient's much shorter per-request Timeout. Applied to
	// Scanner too for consistency, even though Scanner already completes fast.
	fetchCtx, cancel := context.WithTimeout(ctx, FeedFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, feedURL, nil)
	if err != nil {
		return nil, "", "", "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "WPMgr-VulnScanner/1.0")

	resp, doErr := w.client.Do(req)
	if doErr != nil {
		// Transport-level failure: no response ever came back from Wordfence,
		// so this attempt did not consume the shared rate-limit slot.
		return nil, "", "", "", false, fmt.Errorf("http get %s: %w", feedURL, doErr)
	}
	defer func() { _ = resp.Body.Close() }()

	// A response was received — from here on every return path counts as a
	// real request against the rate-limit window, regardless of status code.
	requested = true

	switch resp.StatusCode {
	case http.StatusOK:
		// proceed
	case http.StatusUnauthorized, http.StatusForbidden:
		// Bad or missing API key — surface a clean error without including the key
		// itself in the message (the message is stored in wordfence_vuln_feed_meta
		// and returned to the superadmin via the status endpoint).
		return nil, "", "", "", requested, fmt.Errorf("feed auth failed (HTTP %d): check the Wordfence Intelligence API key in the superadmin settings", resp.StatusCode)
	case http.StatusTooManyRequests:
		// Do NOT retry inside this cycle: Wordfence's rate limit is a ~30-minute
		// GLOBAL window per API key with no documented Retry-After and no fixed
		// per-window count, so an immediate in-process retry cannot escape the
		// window it is already inside — it only burns a second request and
		// keeps the window warm (this was the direct mechanism behind GH #245:
		// the Production fetch was always the second request in the same
		// cycle, so it 429'd every time). The caller stamps degraded status and
		// returns; the next ELIGIBLE run (wall-clock spaced) retries the SAME
		// feed since the cursor does not advance on a 429.
		return nil, "", "", "", requested, fmt.Errorf("rate limited (429) fetching %s: %w", feedURL, errRateLimited)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, "", "", "", requested, fmt.Errorf("unexpected status %d from %s: %s", resp.StatusCode, feedURL, body)
	}

	// Stream-decode the root JSON object so we don't load multi-MB into memory
	// all at once.
	dec := json.NewDecoder(resp.Body)

	// Read opening "{".
	if tok, terr := dec.Token(); terr != nil || tok.(json.Delim) != '{' {
		return nil, "", "", "", requested, fmt.Errorf("expected root object from %s", feedURL)
	}

	recs := make(map[string]FeedRecord)

	for dec.More() {
		// Read the vuln UUID key.
		keyTok, kerr := dec.Token()
		if kerr != nil {
			return nil, "", "", "", requested, fmt.Errorf("read key: %w", kerr)
		}
		vulnID, ok := keyTok.(string)
		if !ok {
			continue
		}

		// Decode the record object as raw JSON first (so we can preserve it in raw).
		var rawMsg json.RawMessage
		if derr := dec.Decode(&rawMsg); derr != nil {
			return nil, "", "", "", requested, fmt.Errorf("decode record %s: %w", vulnID, derr)
		}

		rec, notice, license, mitre, perr := parseFeedRecord(vulnID, rawMsg)
		if perr != nil {
			if errors.Is(perr, errNoUsableSoftware) {
				// A record with no usable software cannot match any site inventory.
				// Skip at Debug level — this is expected for certain informational entries.
				w.logger.Debug("vuln: skipping record with no usable software",
					slog.String("vuln_id", vulnID))
			} else {
				// Defensive catch-all: parseFeedRecord is designed to never return other
				// errors, but guard here anyway.
				w.logger.Warn("vuln: skipping unparseable record",
					slog.String("vuln_id", vulnID), slog.Any("error", perr))
			}
			continue
		}

		recs[vulnID] = rec

		// Capture the first non-empty attribution texts seen.
		if defiantNotice == "" && notice != "" {
			defiantNotice = notice
		}
		if defiantLicense == "" && license != "" {
			defiantLicense = license
		}
		if mitreNotice == "" && mitre != "" {
			mitreNotice = mitre
		}
	}

	return recs, defiantNotice, defiantLicense, mitreNotice, requested, nil
}

// ProductionBatchSize is the number of Production feed records accumulated in
// memory before each flush to the database. The Production feed carries a
// much richer per-record payload than Scanner (full cvss/cve/cwe/copyrights/
// remediation/raw JSON for every analyzed vulnerability), so — unlike
// Scanner, which fetchFeed buffers entirely because its ~37k MINIMAL records
// fit comfortably — Production MUST be streamed in bounded batches. Once the
// wall-clock spacing fix (m102) let a Production request through cleanly for
// the first time, buffering that response whole exceeded the pod's 1Gi
// memory limit and SIGKILL'd it (prod incident, GH #245 third follow-up). 500
// keeps memory bounded to roughly a handful of MB per batch regardless of
// total feed size; 1000 would also be safe — this sits in the middle of the
// requested 500–1000 range.
const ProductionBatchSize = 500

// fetchAndIngestProduction streams the Production feed and flushes it to
// wordfence_vuln_feed in bounded batches of ProductionBatchSize records via
// Repo.BulkUpsertFeedRecordsPool, so memory is O(batch) rather than O(whole
// feed). This differs from fetchFeed (Scanner) in exactly one respect: it
// never accumulates a full records map — the JSON is still decoded
// token-by-token via the SAME json.Decoder approach (dec.Token() for each
// key, dec.Decode(&rawMsg) for each value — see fetchFeed), but each decoded
// record is appended to a small reusable batch slice that gets flushed and
// reset (batch[:0], keeping the same backing array so memory never grows)
// once it reaches ProductionBatchSize, instead of being retained in a
// whole-feed map until the caller returns.
//
// Every batch flush goes through the SAME sticky COALESCE/NULLIF upsert as
// Scanner (Repo.BulkUpsertFeedRecords, wrapped by BulkUpsertFeedRecordsPool
// in its own short transaction): cve/cve_link/cvss_score/cvss_rating/cwe/
// reference_urls are never nulled out. Production never touches software
// rows or prunes (see ingestRecords doc) — each batch's upsert-only write is
// independently safe to commit, so a mid-stream failure on batch K leaves
// batches 1..K-1 durably enriched instead of losing all forward progress (an
// improvement over the one-big-transaction approach, which would have rolled
// back everything on any failure anyway).
//
// Returns: total records successfully ingested (recordCount — used for the
// enrichment_ok=(recordCount>0) decision and the completion log), the
// attribution strings, requested (whether the request reached Wordfence — see
// fetchFeed doc), and error. On a mid-stream failure, recordCount reflects
// however many records were flushed before the failure (already durable) and
// err is non-nil; the caller (workProduction) routes err through
// handleFetchError, which never advances the cursor — StampFeedMetaFinal
// (the only call that does) is only reached on a nil error.
func (w *FeedWorker) fetchAndIngestProduction(ctx context.Context, apiKey string) (recordCount int, defiantNotice, defiantLicense, mitreNotice string, requested bool, err error) {
	// Bound the HTTP request + streamed body read to FeedFetchTimeout — NOT
	// the shared httpclient's much shorter per-request Timeout (that 30s cap
	// is what would make Production fail mid-stream now that the streaming
	// fix means it no longer OOMs first). This deliberately does NOT wrap the
	// DB flush calls below (they keep using the outer job ctx, whose deadline
	// comes from the River job Timeout) — a slow-but-still-flushing batch
	// write should not be cut off just because the HTTP-specific budget is
	// tighter than the overall job budget.
	fetchCtx, cancel := context.WithTimeout(ctx, FeedFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, ProductionFeedURL, nil)
	if err != nil {
		return 0, "", "", "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "WPMgr-VulnScanner/1.0")

	resp, doErr := w.client.Do(req)
	if doErr != nil {
		// Transport-level failure: no response ever came back from Wordfence,
		// so this attempt did not consume the shared rate-limit slot.
		return 0, "", "", "", false, fmt.Errorf("http get %s: %w", ProductionFeedURL, doErr)
	}
	defer func() { _ = resp.Body.Close() }()

	// A response was received — from here on every return path counts as a
	// real request against the rate-limit window, regardless of status code.
	requested = true

	switch resp.StatusCode {
	case http.StatusOK:
		// proceed
	case http.StatusUnauthorized, http.StatusForbidden:
		return 0, "", "", "", requested, fmt.Errorf("feed auth failed (HTTP %d): check the Wordfence Intelligence API key in the superadmin settings", resp.StatusCode)
	case http.StatusTooManyRequests:
		return 0, "", "", "", requested, fmt.Errorf("rate limited (429) fetching %s: %w", ProductionFeedURL, errRateLimited)
	default:
		// Bounded 512-byte diagnostic read ONLY — never the full body. This is
		// the sole non-streaming Read on the production path, and only ever
		// hit on an already-failed non-OK status, before any streaming begins.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, "", "", "", requested, fmt.Errorf("unexpected status %d from %s: %s", resp.StatusCode, ProductionFeedURL, body)
	}

	// Stream-decode the root JSON object token-by-token — identical approach
	// to fetchFeed — but flush in bounded batches (streamProductionRecords)
	// below instead of accumulating a whole-feed map.
	dec := json.NewDecoder(resp.Body)
	if tok, terr := dec.Token(); terr != nil || tok.(json.Delim) != '{' {
		return 0, "", "", "", requested, fmt.Errorf("expected root object from %s", ProductionFeedURL)
	}

	n, defiantNotice, defiantLicense, mitreNotice, serr := w.streamProductionRecords(dec, ProductionBatchSize,
		func(batch []FeedRecord) error { return w.repo.BulkUpsertFeedRecordsPool(ctx, batch) })
	return n, defiantNotice, defiantLicense, mitreNotice, requested, serr
}

// streamProductionRecords decodes key/value pairs from dec — already
// positioned just past the feed's opening '{' — and flushes them via flush in
// batches of at most batchSize records, resetting the batch slice (keeping
// its backing array) after each flush so memory never exceeds batchSize
// records regardless of total feed size. This is the core loop extracted from
// fetchAndIngestProduction so it can be unit-tested without any HTTP/DB
// dependency (see export_test.go's StreamProductionRecords wrapper and
// worker_test.go's TestFeedWorker_StreamProductionRecords_* tests, which
// assert flush is called multiple times and never with more than batchSize
// records).
//
// Returns the total records successfully queued-and-flushed, the attribution
// strings, and an error. On error, the return values reflect whatever was
// flushed via COMPLETED flush calls before the failure — the in-progress
// (not-yet-flushed) batch at the moment of failure is discarded, which is why
// FeedWorker.fetchAndIngestProduction's default ProductionBatchSize keeps
// that discarded slice small regardless of total feed size.
func (w *FeedWorker) streamProductionRecords(dec *json.Decoder, batchSize int, flush func(batch []FeedRecord) error) (n int, defiantNotice, defiantLicense, mitreNotice string, err error) {
	logStreamFail := func(n int, stage string, streamErr error) {
		w.logger.Error(fmt.Sprintf("vuln: production feed stream failed at record %d", n),
			slog.String("stage", stage), slog.Any("error", streamErr))
	}

	batch := make([]FeedRecord, 0, batchSize)
	flushBatch := func() error {
		if len(batch) == 0 {
			return nil
		}
		if ferr := flush(batch); ferr != nil {
			return ferr
		}
		batch = batch[:0] // reuse the backing array — memory never exceeds batchSize records
		return nil
	}

	for dec.More() {
		// Read the vuln UUID key.
		keyTok, kerr := dec.Token()
		if kerr != nil {
			logStreamFail(n, "read_key", kerr)
			return n, defiantNotice, defiantLicense, mitreNotice,
				fmt.Errorf("production feed stream failed at record %d: read key: %w", n, kerr)
		}
		vulnID, ok := keyTok.(string)
		if !ok {
			continue
		}

		// Decode the record object as raw JSON first (so we can preserve it in raw).
		var rawMsg json.RawMessage
		if derr := dec.Decode(&rawMsg); derr != nil {
			logStreamFail(n, "decode_record", derr)
			return n, defiantNotice, defiantLicense, mitreNotice,
				fmt.Errorf("production feed stream failed at record %d (%s): decode: %w", n, vulnID, derr)
		}

		rec, notice, license, mitre, perr := parseFeedRecord(vulnID, rawMsg)
		if perr != nil {
			if errors.Is(perr, errNoUsableSoftware) {
				// A record with no usable software cannot match any site inventory.
				// Skip at Debug level — this is expected for certain informational entries.
				w.logger.Debug("vuln: skipping record with no usable software",
					slog.String("vuln_id", vulnID))
			} else {
				// Defensive catch-all: parseFeedRecord is designed to never return other
				// errors, but guard here anyway.
				w.logger.Warn("vuln: skipping unparseable record",
					slog.String("vuln_id", vulnID), slog.Any("error", perr))
			}
			continue
		}

		batch = append(batch, rec)
		n++

		// Capture the first non-empty attribution texts seen.
		if defiantNotice == "" && notice != "" {
			defiantNotice = notice
		}
		if defiantLicense == "" && license != "" {
			defiantLicense = license
		}
		if mitreNotice == "" && mitre != "" {
			mitreNotice = mitre
		}

		if len(batch) >= batchSize {
			if ferr := flushBatch(); ferr != nil {
				logStreamFail(n, "flush_batch", ferr)
				return n, defiantNotice, defiantLicense, mitreNotice,
					fmt.Errorf("production feed stream failed at record %d: flush batch: %w", n, ferr)
			}
		}
	}

	// Flush the remainder (fewer than batchSize records left over).
	if ferr := flushBatch(); ferr != nil {
		logStreamFail(n, "flush_final_batch", ferr)
		return n, defiantNotice, defiantLicense, mitreNotice,
			fmt.Errorf("production feed stream failed at record %d: flush final batch: %w", n, ferr)
	}

	return n, defiantNotice, defiantLicense, mitreNotice, nil
}

// ---------------------------------------------------------------------------
// Feed record JSON types
// ---------------------------------------------------------------------------

// errNoUsableSoftware is returned by parseFeedRecord when a record carries no
// software entry with a non-empty allow-listed type and non-empty slug. This is
// the ONE legitimate whole-record skip. The caller treats it as Debug-level.
var errNoUsableSoftware = errors.New("no usable software entry")

// wfTimeLayouts are tried in order. The space-separated layout is the real v3
// feed format; date-only and RFC3339 are tolerated as forward-compatible fallbacks.
var wfTimeLayouts = []string{
	"2006-01-02 15:04:05", // real Wordfence v3 format (UTC, space-separated, no T)
	"2006-01-02",          // date-only fallback
	time.RFC3339,          // RFC3339 fallback (forward-tolerant)
}

// wfTime is a Wordfence-feed timestamp. The v3 feed emits UTC datetimes as
// "YYYY-MM-DD HH:MM:SS" (no T, no zone); it never uses RFC3339. UnmarshalJSON
// is intentionally lenient: a null, empty, or unrecognised value yields a nil
// time and never an error, so one bad timestamp field can never drop a record.
type wfTime struct{ t *time.Time }

func (w *wfTime) UnmarshalJSON(b []byte) error {
	w.t = nil
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" || s == `""` {
		return nil // not disclosed → nil, no error
	}
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil // not a JSON string → ignore the field, keep the record
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	for _, layout := range wfTimeLayouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			// Pin to UTC: the space-separated and date-only layouts carry no zone;
			// the feed declares all timestamps UTC.
			u := parsed.UTC()
			w.t = &u
			return nil
		}
	}
	return nil // unrecognised format → nil time, record still ingests
}

// Time returns the parsed *time.Time (nil when absent or unparseable).
func (w wfTime) Time() *time.Time { return w.t }

// wfRecord is the JSON shape of one Wordfence V3 vulnerability record.
// Fields that can carry "odd" shapes are typed as json.RawMessage and decoded
// by extractors so a single malformed field can never fail the whole record.
type wfRecord struct {
	ID            string          `json:"id"`
	Title         string          `json:"title"`
	Informational bool            `json:"informational"`
	Published     wfTime          `json:"published"`
	Updated       wfTime          `json:"updated"`
	CVE           json.RawMessage `json:"cve"`      // string or array in some shapes
	CVELink       string          `json:"cve_link"` // may be absent on Scanner feed
	CVSS          json.RawMessage `json:"cvss"`     // object {vector,score,rating} or null
	CWE           json.RawMessage `json:"cwe"`
	References    json.RawMessage `json:"references"`
	Software      json.RawMessage `json:"software"`   // decoded best-effort
	Copyrights    json.RawMessage `json:"copyrights"` // decoded best-effort
}

// wfCVSS is the object shape inside the "cvss" key of the Production feed.
type wfCVSS struct {
	Score  wfFlexFloat `json:"score"`
	Rating string      `json:"rating"`
}

// wfFlexFloat tolerates a CVSS score encoded as either a JSON number
// (9.8) or a stringified number ("9.8") — the feed has been observed sending
// both shapes. UnmarshalJSON is intentionally lenient, mirroring wfTime: an
// absent, null, empty, or unparseable value yields a nil score and never an
// error, so one odd score field can never drop the whole record or discard a
// sibling field (e.g. rating) that decoded fine.
type wfFlexFloat struct{ v *float64 }

func (f *wfFlexFloat) UnmarshalJSON(b []byte) error {
	f.v = nil
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	// Try a JSON number first (the real v3 shape).
	var num float64
	if err := json.Unmarshal(b, &num); err == nil {
		f.v = &num
		return nil
	}
	// Tolerate a stringified number.
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		str = strings.TrimSpace(str)
		if str == "" {
			return nil
		}
		if parsed, perr := strconv.ParseFloat(str, 64); perr == nil {
			f.v = &parsed
		}
	}
	return nil // unrecognised shape → nil score, record still ingests
}

// wfCopyrightEntry holds one party's attribution data.
type wfCopyrightEntry struct {
	Notice  string `json:"notice"`
	License string `json:"license"`
}

// wfCopyrightsObj is the typed structure for the copyrights block.
type wfCopyrightsObj struct {
	Defiant *wfCopyrightEntry `json:"defiant"`
	MITRE   *wfCopyrightEntry `json:"mitre"`
}

// wfSoftware is one entry in the software[] array.
type wfSoftware struct {
	Type             string          `json:"type"`    // core|plugin|theme
	Name             string          `json:"name"`
	Slug             string          `json:"slug"`
	AffectedVersions json.RawMessage `json:"affected_versions"`
	Patched          bool            `json:"patched"`
	PatchedVersions  json.RawMessage `json:"patched_versions"` // array OR map
	Informational    *bool           `json:"informational"`    // scanner carries this at software level
}

// wfSoftwareTypeAllowList is the set of valid software type values.
var wfSoftwareTypeAllowList = map[string]bool{
	"core":   true,
	"plugin": true,
	"theme":  true,
}

// ---------------------------------------------------------------------------
// Field extractors
// ---------------------------------------------------------------------------

// extractCVE returns a CVE string from a raw JSON value that may be a plain
// string, an array of strings, or null. Returns "" on any odd shape.
func extractCVE(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	// Try plain string first.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Try array; return first element.
	var arr []string
	if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
		return arr[0]
	}
	return ""
}

// extractCVSS decodes a raw "cvss" JSON value into a score and rating.
// The real v3 feed sends an object {vector, score, rating}; for forward
// tolerance, a bare number is also accepted as a score-only value.
// Returns (nil, "") on null/absent/unparseable input.
//
// Two hardening fixes (GH #245 follow-up): (1) a rating is honored even when
// score is absent/null — an object like {"rating":"Critical"} with no score
// field previously fell through this function entirely and lost the rating;
// (2) score tolerates a stringified number ("9.8") via wfFlexFloat, which
// previously made the WHOLE cvss object fail to unmarshal (json.Unmarshal
// aborts a struct decode on any field type mismatch), silently discarding a
// present rating too.
func extractCVSS(raw json.RawMessage) (score *float64, rating string) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, ""
	}
	// Object shape {score, rating} — the real v3 format. This unmarshal only
	// succeeds when raw is structurally a JSON object (a bare number or array
	// fails here and falls through to the tolerance branch below), so it is
	// safe to return whatever fields are present without also requiring
	// Score to be non-nil.
	var obj wfCVSS
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Score.v, obj.Rating
	}
	// Forward-tolerance: bare number (score-only, no object wrapper).
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return &f, ""
	}
	return nil, ""
}

// extractCopyrights decodes the raw copyrights block, returning (defiantNotice,
// defiantLicense, mitreNotice). Returns ("","","") on any odd shape so a
// malformed copyrights block cannot drop the record.
func extractCopyrights(raw json.RawMessage) (defiantNotice, defiantLicense, mitreNotice string) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", "", ""
	}
	var cp wfCopyrightsObj
	if err := json.Unmarshal(raw, &cp); err != nil {
		return "", "", ""
	}
	if cp.Defiant != nil {
		defiantNotice = cp.Defiant.Notice
		defiantLicense = cp.Defiant.License
	}
	if cp.MITRE != nil {
		mitreNotice = cp.MITRE.Notice
	}
	return defiantNotice, defiantLicense, mitreNotice
}

// ---------------------------------------------------------------------------
// Core parser
// ---------------------------------------------------------------------------

// parseFeedRecord decodes one raw Wordfence V3 vulnerability record.
//
// Design: the ONLY non-nil error returned is errNoUsableSoftware (when the
// record has no software entry with a non-empty allow-listed type and non-empty
// slug). Every other field is decoded best-effort: a null/malformed/unexpected
// value defaults to zero/nil and the record still ingests. This means a single
// bad timestamp, cvss object, or copyrights block can never silence the record.
func parseFeedRecord(vulnID string, raw json.RawMessage) (FeedRecord, string, string, string, error) {
	var rec wfRecord
	// The struct uses only safe field types (wfTime, json.RawMessage, string,
	// bool) so this unmarshal cannot fail due to field-level type mismatches.
	if err := json.Unmarshal(raw, &rec); err != nil {
		// Structural failure (not a JSON object at all). Very unlikely on a well-formed
		// feed but guard anyway; return errNoUsableSoftware so the caller skips quietly.
		return FeedRecord{}, "", "", "", errNoUsableSoftware
	}

	// --- cvss: real key is "cvss", shape is {vector,score,rating} or null ---
	cvssScore, cvssRating := extractCVSS(rec.CVSS)

	// --- cve: string or array (decode best-effort) ---
	cve := extractCVE(rec.CVE)

	// --- cve_link: keep only safe http(s) URLs ---
	cveLink := rec.CVELink
	if cveLink != "" && !isSafeURL(cveLink) {
		cveLink = ""
	}

	// --- references: filter to safe URLs ---
	refs := filterReferences(rec.References)
	if len(refs) == 0 {
		refs = []byte("[]")
	}

	// --- cwe: keep raw, nil-safe ---
	cwe := rec.CWE
	if len(cwe) == 0 {
		cwe = nil
	}

	// --- copyrights: best-effort decode ---
	defiantNotice, defiantLicense, mitreNotice := extractCopyrights(rec.Copyrights)

	// --- informational: record-level OR-ed with any software-level true ---
	informational := rec.Informational

	// --- software: decode best-effort; odd shape → nil slice → skip record ---
	var rawSoftware []wfSoftware
	if len(rec.Software) > 0 && string(rec.Software) != "null" {
		// Ignore the error: an odd shape (not an array) leaves rawSoftware nil,
		// which will trigger the errNoUsableSoftware skip below.
		_ = json.Unmarshal(rec.Software, &rawSoftware)
	}

	var software []SoftwareRow
	for _, sw := range rawSoftware {
		// OR-up software-level informational into the record-level flag.
		if sw.Informational != nil && *sw.Informational {
			informational = true
		}

		kind := sw.Type
		if kind == "" || !wfSoftwareTypeAllowList[kind] {
			// Unknown or empty type: skip this software ROW, not the record.
			continue
		}
		slug := normSlug(sw.Slug)
		if slug == "" {
			// A software row with no slug can never match any inventory item.
			continue
		}

		avRaw := sw.AffectedVersions
		if len(avRaw) == 0 || string(avRaw) == "null" {
			// Default to empty object (matching the real v3 object shape) rather
			// than "[]" (an array), so the matcher's object-parser sees the right type.
			avRaw = []byte("{}")
		}

		pvRaw := sw.PatchedVersions
		if len(pvRaw) == 0 || string(pvRaw) == "null" {
			pvRaw = []byte("[]")
		}
		// PatchedVersions may be an array ["1.2","1.3"] or a map {"1.2":true} —
		// normalise to an array.
		pvRaw = normalisePatchedVersions(pvRaw)

		software = append(software, SoftwareRow{
			Kind:             kind,
			Slug:             slug,
			AffectedVersions: avRaw,
			Patched:          sw.Patched,
			PatchedVersions:  pvRaw,
		})
	}

	// Essential-field rule: a record is ingestable iff it has at least one usable
	// software entry (non-empty allow-listed type + non-empty slug).
	if len(software) == 0 {
		return FeedRecord{}, "", "", "", errNoUsableSoftware
	}

	return FeedRecord{
		VulnID:        vulnID,
		Title:         rec.Title,
		CVE:           cve,
		CVELink:       cveLink,
		CVSSScore:     cvssScore,
		CVSSRating:    cvssRating,
		CWE:           cwe,
		Informational: informational,
		References:    refs,
		Published:     rec.Published.Time(),
		Updated:       rec.Updated.Time(),
		Raw:           raw,
		Software:      software,
	}, defiantNotice, defiantLicense, mitreNotice, nil
}

// isSafeURL reports whether a URL has an http:// or https:// scheme.
// F2: used to drop feed-supplied javascript:/data:/etc. references before
// they reach the database, so a malicious feed entry cannot inject a
// non-HTTP URL that would later be rendered as a clickable link in the UI.
func isSafeURL(u string) bool {
	lower := strings.ToLower(strings.TrimSpace(u))
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// filterReferences removes any entry from a Wordfence references JSON array
// whose URL is not an http(s) URL. The feed supports two shapes:
//
//   - array of strings: ["https://example.com", ...]
//   - array of objects: [{"url":"https://example.com"}, ...]
//
// Returns a JSON array of safe URLs in string form, or nil when the input is
// empty/unparseable (callers default to "[]").
func filterReferences(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	// Try array-of-strings first.
	var strs []string
	if json.Unmarshal(raw, &strs) == nil {
		safe := strs[:0]
		for _, u := range strs {
			if isSafeURL(u) {
				safe = append(safe, u)
			}
		}
		b, _ := json.Marshal(safe)
		return b
	}
	// Try array-of-objects with a "url" field.
	var objs []struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &objs) == nil {
		safe := make([]string, 0, len(objs))
		for _, o := range objs {
			if isSafeURL(o.URL) {
				safe = append(safe, o.URL)
			}
		}
		b, _ := json.Marshal(safe)
		return b
	}
	// Unparseable: return nil so the caller substitutes "[]".
	return nil
}

// normalisePatchedVersions converts either a JSON string array or a JSON
// object (map[version]bool/null) to a uniform JSON string array.
func normalisePatchedVersions(raw []byte) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return []byte("[]")
	}
	trimmed := []byte{}
	for _, b := range raw {
		if b != ' ' && b != '\t' && b != '\n' {
			trimmed = append(trimmed, b)
			break
		}
	}
	if len(trimmed) == 0 {
		return []byte("[]")
	}
	if raw[0] == '[' {
		return raw // already an array
	}
	if raw[0] == '{' {
		// It's a map — extract the keys.
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return []byte("[]")
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		b, _ := json.Marshal(keys)
		return b
	}
	return []byte("[]")
}

// ---------------------------------------------------------------------------
// Per-site rescan worker
// ---------------------------------------------------------------------------

// RescanSiteWorker handles per-site rescan jobs enqueued after a feed refresh
// or after an inventory change.
type RescanSiteWorker struct {
	river.WorkerDefaults[RescanSiteArgs]
	svc    *Service
	logger *slog.Logger
	// alertEnqueue is the m103 (GH #247) debounced vuln-alert-dispatch
	// enqueuer, wired post-boot via SetAlertDispatchEnqueuer. Nil is safe
	// (the dispatch hook below is skipped).
	alertEnqueue AlertDispatchEnqueuer
}

// NewRescanSiteWorker builds a RescanSiteWorker.
func NewRescanSiteWorker(svc *Service, logger *slog.Logger) *RescanSiteWorker {
	return &RescanSiteWorker{svc: svc, logger: logger}
}

// SetService wires the vuln service into the worker after construction. Called
// once at boot after startRiver returns (the service needs the River client).
func (w *RescanSiteWorker) SetService(svc *Service) { w.svc = svc }

// SetAlertDispatchEnqueuer wires the debounced batched-alert-dispatch
// enqueuer. Called once at boot after River starts (mirrors SetService).
func (w *RescanSiteWorker) SetAlertDispatchEnqueuer(e AlertDispatchEnqueuer) { w.alertEnqueue = e }

// Work performs the per-site vulnerability rescan, then — on success —
// enqueues a debounced, batched vuln-alert-dispatch job (m103, GH #247). This
// is the ONE hook that covers every rescan trigger (the operator-facing
// "rescan now" route, the post-feed-refresh RescanAll fan-out, and any future
// caller): all of them enqueue a RescanSiteArgs job, which always lands here.
func (w *RescanSiteWorker) Work(ctx context.Context, job *river.Job[RescanSiteArgs]) error {
	args := job.Args
	if err := w.svc.RescanSite(ctx, args.TenantID, args.SiteID); err != nil {
		w.logger.Warn("vuln: site rescan failed",
			slog.String("tenant_id", args.TenantID.String()),
			slog.String("site_id", args.SiteID.String()),
			slog.Any("error", err))
		return err
	}

	// Best-effort: a failure here must never fail the rescan itself (findings
	// are already durably saved) — it just means this tenant's alert waits
	// for the next rescan to re-trigger the debounce.
	if w.alertEnqueue != nil {
		if err := w.alertEnqueue.EnqueueAlertDispatch(ctx); err != nil {
			w.logger.Warn("vuln: enqueue alert dispatch failed",
				slog.String("tenant_id", args.TenantID.String()),
				slog.String("site_id", args.SiteID.String()),
				slog.Any("error", err))
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Vulnerability alert dispatch worker (m103, GH #247)
// ---------------------------------------------------------------------------

// AlertDispatchQueue is the River queue for the batched vuln-alert dispatch job.
const AlertDispatchQueue = "vuln_alert_dispatch"

// AlertDispatchArgs is the (argument-less) River job payload for the
// debounced, cross-tenant vulnerability-alert dispatch. Empty args + the
// UniqueOpts on EnqueueAlertDispatch are what collapse a whole rescan wave
// into a single dispatch job.
type AlertDispatchArgs struct{}

// Kind implements river.JobArgs.
func (AlertDispatchArgs) Kind() string { return "vuln_alert_dispatch" }

// AlertDispatchWorker runs Service.DispatchVulnAlerts.
type AlertDispatchWorker struct {
	river.WorkerDefaults[AlertDispatchArgs]
	svc    *Service
	logger *slog.Logger
}

// NewAlertDispatchWorker builds an AlertDispatchWorker.
func NewAlertDispatchWorker(svc *Service, logger *slog.Logger) *AlertDispatchWorker {
	return &AlertDispatchWorker{svc: svc, logger: logger}
}

// SetService wires the vuln service into the worker after construction.
// Called once at boot after startRiver returns (mirrors RescanSiteWorker).
func (w *AlertDispatchWorker) SetService(svc *Service) { w.svc = svc }

// Work runs the batched dispatch across every tenant with unnotified findings.
func (w *AlertDispatchWorker) Work(ctx context.Context, job *river.Job[AlertDispatchArgs]) error {
	if err := w.svc.DispatchVulnAlerts(ctx); err != nil {
		w.logger.Warn("vuln: alert dispatch job failed", slog.Any("error", err))
		return err
	}
	return nil
}
