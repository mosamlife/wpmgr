// Integration tests for GH #245 — the Wordfence Intelligence feed fetch fix.
//
// Root cause: Wordfence Intelligence v3 enforces ~1 request / 30 min GLOBALLY
// per API key. The feed worker used to fetch Scanner then, after only a 2s
// sleep, fetch Production in the SAME run — Production was deterministically
// 429'd every cycle, so CVSS/CVE enrichment never landed and every finding
// fell back to a fabricated "low" severity (SeverityFromRating's unconditional
// no-data fallback), hiding a real CVSS 9.8 core RCE.
//
// These tests drive vuln.FeedWorker.Work end-to-end against a real Postgres
// (migrations + RLS applied) with a fake HTTP client standing in for the
// Wordfence API, proving:
//   - each Work() invocation issues at most one HTTP request (alternation);
//   - a Production fetch that lands CVSS data actually persists it in
//     wordfence_vuln_feed (not just that an internal merge function ran);
//   - a subsequent Scanner-only run does NOT null out that CVSS data
//     (sticky enrichment — get this wrong and severities flap
//     Unknown<->Critical every other hour, reintroducing #245 intermittently);
//   - a 429 on the Production leg degrades enrichment_ok without an in-window
//     retry and WITHOUT affecting the detection-feed ok flag (which gates
//     RescanSite).
//
// Post-deploy follow-up (m102): the alternation above relied on the FALSE
// assumption that consecutive runs are ~1h apart. Prod logs showed a
// manually-triggered sync landing only ~6 minutes after the periodic tick, so
// the Production request still fell inside Wordfence's ~30-min window and
// 429'd — and because the cursor used to advance unconditionally, Production
// was abandoned for a full cycle. These tests additionally prove:
//   - a run inside the 31-min wall-clock spacing window makes NO request and
//     leaves the alternation cursor untouched (nothing is ever abandoned);
//   - a run that IS eligible (spacing satisfied) fetches the feed the cursor
//     still points at and advances it only on success;
//   - last_request_at advances on both a 200 and a 429 (either consumes the
//     shared rate-limit slot), but the cursor never advances on a 429.
//
// Because the spacing gate is wall-clock (time.Since, not injectable), tests
// that drive more than one Work() call use backdateLastRequest to rewind
// wordfence_vuln_feed_meta.last_request_at directly rather than sleeping for
// 31+ real minutes.
package tests

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/vuln"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeFeedHTTPClient implements vuln.FeedHTTPDoer, returning a canned response
// per URL and recording every request it receives (in order) so tests can
// assert the exact number and target of HTTP calls made per Work() cycle.
type fakeFeedHTTPClient struct {
	mu        sync.Mutex
	calls     []string
	responses map[string]func() (int, string) // URL -> (status, body) thunk
}

func newFakeFeedHTTPClient() *fakeFeedHTTPClient {
	return &fakeFeedHTTPClient{responses: map[string]func() (int, string){}}
}

func (c *fakeFeedHTTPClient) setResponse(url string, status int, body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.responses[url] = func() (int, string) { return status, body }
}

func (c *fakeFeedHTTPClient) Do(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	url := req.URL.String()
	c.calls = append(c.calls, url)
	thunk, ok := c.responses[url]
	c.mu.Unlock()

	status, body := http.StatusOK, `{}`
	if ok {
		status, body = thunk()
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

func (c *fakeFeedHTTPClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *fakeFeedHTTPClient) lastCall() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		return ""
	}
	return c.calls[len(c.calls)-1]
}

// noopSiteLoader satisfies vuln.SiteLoader. It is never actually exercised by
// these tests: RescanAll(ctx, uuid.Nil) enumerates tenants first and a fresh
// migrated test DB has none, so the per-tenant ListAllSiteIDs fan-out never
// runs. It exists only to satisfy vuln.NewService's constructor.
type noopSiteLoader struct{}

func (noopSiteLoader) GetSiteForVuln(_ context.Context, _, _ uuid.UUID) (vuln.SiteSnapshot, error) {
	return vuln.SiteSnapshot{}, nil
}

func (noopSiteLoader) ListAllSiteIDs(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

// noopRescanEnqueuer satisfies vuln.RescanEnqueuer for the same reason.
type noopRescanEnqueuer struct{}

func (noopRescanEnqueuer) EnqueueRescanSite(_ context.Context, _ vuln.RescanSiteArgs) error {
	return nil
}

// buildFeedWorker wires a *vuln.FeedWorker against the real pool + a fake
// HTTP client, mirroring the production wiring in cmd/wpmgr/main.go.
func buildFeedWorker(pool *db.Pool, client *fakeFeedHTTPClient) *vuln.FeedWorker {
	logger := slog.Default()
	repo := vuln.NewRepo(pool)
	svc := vuln.NewService(repo, pool, noopSiteLoader{}, nil, noopRescanEnqueuer{}, logger)
	resolver := vuln.NewStaticKeyResolver("test-wordfence-key-1234")
	return vuln.NewFeedWorker(repo, pool, svc, resolver, client, logger)
}

func runFeedWork(t *testing.T, w *vuln.FeedWorker) error {
	t.Helper()
	return w.Work(context.Background(), &river.Job[vuln.FeedRefreshArgs]{})
}

// backdateLastRequest rewinds wordfence_vuln_feed_meta.last_request_at so the
// NEXT Work() call is immediately eligible to make a request, without a test
// waiting the real 31-minute wall-clock spacing the gate enforces.
func backdateLastRequest(t *testing.T, pool *db.Pool, ago time.Duration) {
	t.Helper()
	target := time.Now().UTC().Add(-ago)
	if _, err := pool.Exec(context.Background(),
		`UPDATE wordfence_vuln_feed_meta SET last_request_at = $1 WHERE id = 1`,
		target,
	); err != nil {
		t.Fatalf("backdate last_request_at: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Fixture feed bodies
// ---------------------------------------------------------------------------

// scannerBodyNoCVSS is a Scanner-shaped response: a single vuln record with
// software but NO cvss key at all (the real Scanner feed never carries CVSS).
func scannerBodyNoCVSS(vulnID string) string {
	return `{"` + vulnID + `": {
		"title": "Test Plugin XSS",
		"software": [
			{"type":"plugin","slug":"test-plugin","affected_versions":{"* - 2.0.0":{"from_version":"*","from_inclusive":true,"to_version":"2.0.0","to_inclusive":false}},"patched":true,"patched_versions":["2.0.1"]}
		]
	}}`
}

// productionBodyReferenceURL is the reference URL carried by
// productionBodyWithCVSS below, used by tests that assert on reference_urls.
const productionBodyReferenceURL = "https://www.wordfence.com/threat-intel/vulnerabilities/test"

// productionBodyWithCVSS is a Production-shaped response for the SAME
// vuln_id, carrying a full cvss object + cve + a reference URL — the
// enrichment data Scanner never sends (scannerBodyNoCVSS above carries
// neither a "cvss" key nor a "references" key, matching the real feeds).
func productionBodyWithCVSS(vulnID string) string {
	return `{"` + vulnID + `": {
		"title": "Test Plugin XSS",
		"software": [
			{"type":"plugin","slug":"test-plugin","affected_versions":{"* - 2.0.0":{"from_version":"*","from_inclusive":true,"to_version":"2.0.0","to_inclusive":false}},"patched":true,"patched_versions":["2.0.1"]}
		],
		"cvss": {"vector":"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H","score":9.8,"rating":"Critical"},
		"cve": "CVE-2026-24500",
		"references": ["` + productionBodyReferenceURL + `"]
	}}`
}

func feedRow(t *testing.T, pool *db.Pool, vulnID string) (cvssScore *float64, cvssRating, cve string) {
	t.Helper()
	var score *float64
	var rating, cveVal *string
	err := pool.QueryRow(context.Background(),
		`SELECT cvss_score, cvss_rating, cve FROM wordfence_vuln_feed WHERE vuln_id = $1`,
		vulnID,
	).Scan(&score, &rating, &cveVal)
	if err != nil {
		t.Fatalf("select wordfence_vuln_feed row: %v", err)
	}
	r, c := "", ""
	if rating != nil {
		r = *rating
	}
	if cveVal != nil {
		c = *cveVal
	}
	return score, r, c
}

// feedReferenceURLs reads and decodes wordfence_vuln_feed.reference_urls for vulnID.
func feedReferenceURLs(t *testing.T, pool *db.Pool, vulnID string) []string {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT reference_urls FROM wordfence_vuln_feed WHERE vuln_id = $1`, vulnID,
	).Scan(&raw); err != nil {
		t.Fatalf("select reference_urls: %v", err)
	}
	var urls []string
	if err := json.Unmarshal(raw, &urls); err != nil {
		t.Fatalf("unmarshal reference_urls (%s): %v", raw, err)
	}
	return urls
}

// ---------------------------------------------------------------------------
// Test 1: alternation — at most one HTTP request per Work() cycle, and
// successive cycles target the OTHER feed.
// ---------------------------------------------------------------------------

func TestFeedWorker_AlternatesFeedsOneRequestPerRun(t *testing.T) {
	pool := startPostgres(t)
	client := newFakeFeedHTTPClient()
	client.setResponse(vuln.ScannerFeedURL, http.StatusOK, scannerBodyNoCVSS("245-alt-vuln-1"))
	client.setResponse(vuln.ProductionFeedURL, http.StatusOK, productionBodyWithCVSS("245-alt-vuln-1"))

	w := buildFeedWorker(pool, client)

	// The alternation cursor defaults to 'scanner' (schema default), so run 1
	// must hit the Scanner URL and run 2 must hit Production, run 3 back to
	// Scanner — and each run must issue EXACTLY one HTTP request. Runs after
	// the first are backdated to be immediately eligible under the wall-clock
	// spacing gate (m102) — this test is about alternation, not spacing.
	wantSequence := []string{vuln.ScannerFeedURL, vuln.ProductionFeedURL, vuln.ScannerFeedURL}
	for i, wantURL := range wantSequence {
		if i > 0 {
			backdateLastRequest(t, pool, 32*time.Minute)
		}
		before := client.callCount()
		if err := runFeedWork(t, w); err != nil {
			t.Fatalf("run %d: Work returned error: %v", i+1, err)
		}
		after := client.callCount()
		if after-before != 1 {
			t.Errorf("run %d: made %d HTTP request(s); want exactly 1 (Wordfence enforces ~1 request/30min globally)", i+1, after-before)
		}
		if got := client.lastCall(); got != wantURL {
			t.Errorf("run %d: fetched %q; want %q", i+1, got, wantURL)
		}
	}
}

// ---------------------------------------------------------------------------
// Test 2: a successful Production fetch actually lands CVSS end-to-end (not
// just that some internal merge function ran).
// ---------------------------------------------------------------------------

func TestFeedWorker_ProductionFetch_CVSSLandsEndToEnd(t *testing.T) {
	pool := startPostgres(t)
	const vulnID = "245-e2e-vuln-1"
	client := newFakeFeedHTTPClient()
	client.setResponse(vuln.ScannerFeedURL, http.StatusOK, scannerBodyNoCVSS(vulnID))
	client.setResponse(vuln.ProductionFeedURL, http.StatusOK, productionBodyWithCVSS(vulnID))

	w := buildFeedWorker(pool, client)

	// Run 1: Scanner creates the detection row with NO cvss data yet.
	if err := runFeedWork(t, w); err != nil {
		t.Fatalf("run 1 (scanner): %v", err)
	}
	score, rating, cve := feedRow(t, pool, vulnID)
	if score != nil || rating != "" || cve != "" {
		t.Fatalf("after scanner-only run: score=%v rating=%q cve=%q; want all empty (scanner never carries CVSS)", score, rating, cve)
	}

	// Run 2: Production enriches the SAME row. Backdate to be immediately
	// eligible under the wall-clock spacing gate (m102).
	backdateLastRequest(t, pool, 32*time.Minute)
	if err := runFeedWork(t, w); err != nil {
		t.Fatalf("run 2 (production): %v", err)
	}
	score, rating, cve = feedRow(t, pool, vulnID)
	if score == nil {
		t.Fatal("after production run: cvss_score is nil; want 9.8 (CVSS must land end-to-end, not just merge-called)")
	}
	if *score != 9.8 {
		t.Errorf("cvss_score = %v; want 9.8", *score)
	}
	if rating != "Critical" {
		t.Errorf("cvss_rating = %q; want %q", rating, "Critical")
	}
	if cve != "CVE-2026-24500" {
		t.Errorf("cve = %q; want %q", cve, "CVE-2026-24500")
	}

	// Feed meta must reflect a successful enrichment.
	meta, err := vuln.NewRepo(pool).GetFeedMeta(context.Background())
	if err != nil {
		t.Fatalf("GetFeedMeta: %v", err)
	}
	if !meta.EnrichmentOK {
		t.Error("meta.EnrichmentOK = false; want true after a successful production ingest")
	}
	if meta.LastEnrichmentAt == nil {
		t.Error("meta.LastEnrichmentAt is nil; want a timestamp after a successful production ingest")
	}
}

// ---------------------------------------------------------------------------
// Test 3: sticky enrichment — a Scanner run AFTER a Production run must NOT
// null out the CVSS data Production already landed.
// ---------------------------------------------------------------------------

func TestFeedWorker_StickyEnrichment_ScannerRunPreservesCVSS(t *testing.T) {
	pool := startPostgres(t)
	const vulnID = "245-sticky-vuln-1"
	client := newFakeFeedHTTPClient()
	client.setResponse(vuln.ScannerFeedURL, http.StatusOK, scannerBodyNoCVSS(vulnID))
	client.setResponse(vuln.ProductionFeedURL, http.StatusOK, productionBodyWithCVSS(vulnID))

	w := buildFeedWorker(pool, client)

	// Run 1: scanner (no cvss). Run 2: production (cvss lands). Runs after the
	// first are backdated to be immediately eligible under the wall-clock
	// spacing gate (m102).
	if err := runFeedWork(t, w); err != nil {
		t.Fatalf("run 1 (scanner): %v", err)
	}
	backdateLastRequest(t, pool, 32*time.Minute)
	if err := runFeedWork(t, w); err != nil {
		t.Fatalf("run 2 (production): %v", err)
	}
	score, rating, _ := feedRow(t, pool, vulnID)
	if score == nil || rating != "Critical" {
		t.Fatalf("precondition failed: score=%v rating=%q after production run; want 9.8/Critical", score, rating)
	}

	// Run 3: scanner AGAIN, re-sending the SAME record with no cvss key. This
	// is exactly the flap scenario GH #245's follow-up warns about: a naive
	// blind-overwrite upsert would null cvss_score/cvss_rating right back out.
	backdateLastRequest(t, pool, 32*time.Minute)
	if err := runFeedWork(t, w); err != nil {
		t.Fatalf("run 3 (scanner again): %v", err)
	}
	score, rating, _ = feedRow(t, pool, vulnID)
	if score == nil {
		t.Fatal("after a subsequent scanner-only run: cvss_score is nil; want 9.8 preserved (sticky enrichment)")
	}
	if *score != 9.8 {
		t.Errorf("cvss_score = %v; want 9.8 preserved across the scanner run", *score)
	}
	if rating != "Critical" {
		t.Errorf("cvss_rating = %q; want %q preserved across the scanner run", rating, "Critical")
	}

	// enrichment_ok must ALSO remain sticky-true (a scanner run must not touch it).
	meta, err := vuln.NewRepo(pool).GetFeedMeta(context.Background())
	if err != nil {
		t.Fatalf("GetFeedMeta: %v", err)
	}
	if !meta.EnrichmentOK {
		t.Error("meta.EnrichmentOK = false after a scanner run; want it to stay sticky-true from the prior production run")
	}
}

// TestFeedWorker_StickyEnrichment_ScannerRunPreservesReferences mirrors the
// CVSS sticky test above for reference_urls — the ONE enrichment column that
// was still a blind overwrite (adversarial-verify follow-up to GH #245):
// parseFeedRecord defaults an absent references[] to the non-NULL '[]' jsonb
// sentinel (never SQL NULL), and the Scanner feed never carries references,
// so a plain COALESCE against the existing row does nothing — the empty-array
// sentinel needs its own NULLIF guard. A subsequent Scanner run must NOT blank
// out reference URLs a prior Production run already landed.
func TestFeedWorker_StickyEnrichment_ScannerRunPreservesReferences(t *testing.T) {
	pool := startPostgres(t)
	const vulnID = "245-sticky-refs-vuln-1"
	client := newFakeFeedHTTPClient()
	client.setResponse(vuln.ScannerFeedURL, http.StatusOK, scannerBodyNoCVSS(vulnID))
	client.setResponse(vuln.ProductionFeedURL, http.StatusOK, productionBodyWithCVSS(vulnID))

	w := buildFeedWorker(pool, client)

	// Run 1: scanner — scannerBodyNoCVSS carries no "references" key at all,
	// so the stored reference_urls must be the empty-array default.
	if err := runFeedWork(t, w); err != nil {
		t.Fatalf("run 1 (scanner): %v", err)
	}
	if refs := feedReferenceURLs(t, pool, vulnID); len(refs) != 0 {
		t.Fatalf("after scanner-only run: reference_urls = %v; want empty", refs)
	}

	// Run 2: production lands a real reference URL. Backdate to be
	// immediately eligible under the wall-clock spacing gate (m102).
	backdateLastRequest(t, pool, 32*time.Minute)
	if err := runFeedWork(t, w); err != nil {
		t.Fatalf("run 2 (production): %v", err)
	}
	refs := feedReferenceURLs(t, pool, vulnID)
	if len(refs) != 1 || refs[0] != productionBodyReferenceURL {
		t.Fatalf("after production run: reference_urls = %v; want [%s]", refs, productionBodyReferenceURL)
	}

	// Run 3: scanner AGAIN, re-sending the SAME record with no "references"
	// key — parseFeedRecord defaults this to a non-NULL '[]' jsonb, which is
	// exactly the value a naive COALESCE(EXCLUDED.x, existing.x) upsert cannot
	// distinguish from "no update"; a blind overwrite would erase the URL
	// Production just landed, making advisory link-outs oscillate every hour.
	backdateLastRequest(t, pool, 32*time.Minute)
	if err := runFeedWork(t, w); err != nil {
		t.Fatalf("run 3 (scanner again): %v", err)
	}
	refs = feedReferenceURLs(t, pool, vulnID)
	if len(refs) != 1 || refs[0] != productionBodyReferenceURL {
		t.Errorf("after a subsequent scanner-only run: reference_urls = %v; want [%s] preserved (sticky enrichment)", refs, productionBodyReferenceURL)
	}
}

// ---------------------------------------------------------------------------
// Test 4: a 429 on the Production leg degrades enrichment status WITHOUT an
// in-window retry and WITHOUT affecting the detection ok flag.
// ---------------------------------------------------------------------------

func TestFeedWorker_ProductionRateLimited_NoInWindowRetry_DetectionUnaffected(t *testing.T) {
	pool := startPostgres(t)
	const vulnID = "245-429-vuln-1"
	client := newFakeFeedHTTPClient()
	client.setResponse(vuln.ScannerFeedURL, http.StatusOK, scannerBodyNoCVSS(vulnID))
	client.setResponse(vuln.ProductionFeedURL, http.StatusTooManyRequests, `{"error":"rate limited"}`)

	w := buildFeedWorker(pool, client)

	// Run 1: scanner succeeds, establishing a healthy detection feed.
	if err := runFeedWork(t, w); err != nil {
		t.Fatalf("run 1 (scanner): %v", err)
	}
	repo := vuln.NewRepo(pool)
	meta, err := repo.GetFeedMeta(context.Background())
	if err != nil {
		t.Fatalf("GetFeedMeta after run 1: %v", err)
	}
	if !meta.OK {
		t.Fatal("precondition failed: meta.OK = false after a successful scanner run")
	}

	// Run 2: production is rate-limited (429). Must return nil (no River
	// retry) and must issue EXACTLY one HTTP request this cycle — no
	// synchronous in-window retry loop. Backdate to be immediately eligible
	// under the wall-clock spacing gate (m102) so this run actually attempts
	// the request (rather than being spacing-skipped, which would also make
	// zero requests but for a different reason).
	backdateLastRequest(t, pool, 32*time.Minute)
	before := client.callCount()
	if err := runFeedWork(t, w); err != nil {
		t.Fatalf("run 2 (production 429): Work returned an error; want nil (no River retry on a rate limit): %v", err)
	}
	after := client.callCount()
	if after-before != 1 {
		t.Errorf("run 2: made %d HTTP request(s) on a 429; want exactly 1 (no in-window retry)", after-before)
	}

	meta, err = repo.GetFeedMeta(context.Background())
	if err != nil {
		t.Fatalf("GetFeedMeta after run 2: %v", err)
	}
	// Detection freshness (ok) must be UNAFFECTED by a Production hiccup —
	// otherwise RescanSite would wrongly skip every other hour once
	// alternation is in play.
	if !meta.OK {
		t.Error("meta.OK = false after a Production 429; want true (Scanner-driven detection data is unaffected)")
	}
	// Enrichment status must reflect the degradation.
	if meta.EnrichmentOK {
		t.Error("meta.EnrichmentOK = true after a Production 429; want false")
	}
	if meta.LastError == "" || !strings.Contains(strings.ToLower(meta.LastError), "rate limit") {
		t.Errorf("meta.LastError = %q; want it to mention the rate limit", meta.LastError)
	}

	// Belt-and-suspenders: a 429 must NOT advance the cursor — the same feed
	// (production) is retried on the next eligible run rather than abandoned.
	gate, err := repo.GetFeedGate(context.Background())
	if err != nil {
		t.Fatalf("GetFeedGate after run 2: %v", err)
	}
	if gate.FeedKind != vuln.FeedKindProduction {
		t.Errorf("cursor after a 429 = %q; want still %q (429 must not advance the cursor)", gate.FeedKind, vuln.FeedKindProduction)
	}
}

// ---------------------------------------------------------------------------
// m102 follow-up: wall-clock request spacing, independent of job cadence.
// ---------------------------------------------------------------------------

// TestFeedWorker_SpacingSkip_NoRequestNoCursorAdvance covers (a)+(d): a run
// that lands inside the 31-min wall-clock spacing window — regardless of how
// long it's actually been since the periodic job last fired — makes NO
// Wordfence request and leaves the alternation cursor untouched, so the feed
// it intended to fetch is retried, never abandoned, on the next eligible run.
// This is the direct regression guard for the prod incident: a scanner run at
// 04:47 followed by a second run only 6 minutes later must now skip entirely
// instead of 429ing on production and losing that cycle.
func TestFeedWorker_SpacingSkip_NoRequestNoCursorAdvance(t *testing.T) {
	pool := startPostgres(t)
	const vulnID = "245-spacing-skip-vuln-1"
	client := newFakeFeedHTTPClient()
	client.setResponse(vuln.ScannerFeedURL, http.StatusOK, scannerBodyNoCVSS(vulnID))
	client.setResponse(vuln.ProductionFeedURL, http.StatusOK, productionBodyWithCVSS(vulnID))

	w := buildFeedWorker(pool, client)
	repo := vuln.NewRepo(pool)

	// Run 1: scanner succeeds — establishes last_request_at ~now, cursor -> production.
	if err := runFeedWork(t, w); err != nil {
		t.Fatalf("run 1 (scanner): %v", err)
	}
	if got := client.callCount(); got != 1 {
		t.Fatalf("after run 1: callCount = %d; want 1", got)
	}
	gate, err := repo.GetFeedGate(context.Background())
	if err != nil {
		t.Fatalf("GetFeedGate after run 1: %v", err)
	}
	if gate.FeedKind != vuln.FeedKindProduction {
		t.Fatalf("cursor after run 1 = %q; want %q", gate.FeedKind, vuln.FeedKindProduction)
	}

	// Run 2: immediately after (real elapsed time in a test is milliseconds,
	// nowhere near 31 minutes) — must skip entirely: no request, no error,
	// cursor unchanged.
	if err := runFeedWork(t, w); err != nil {
		t.Fatalf("run 2 (spacing skip): Work returned an error; want nil: %v", err)
	}
	if got := client.callCount(); got != 1 {
		t.Errorf("after run 2 (spacing-skip): callCount = %d; want still 1 (no new request)", got)
	}
	gate, err = repo.GetFeedGate(context.Background())
	if err != nil {
		t.Fatalf("GetFeedGate after run 2: %v", err)
	}
	if gate.FeedKind != vuln.FeedKindProduction {
		t.Errorf("cursor after a spacing-skip = %q; want still %q (unchanged — never abandon a feed)", gate.FeedKind, vuln.FeedKindProduction)
	}
}

// TestFeedWorker_EligibleAfterSpacing_FetchesIntendedFeedAndAdvances covers
// (b): once >=31 minutes have elapsed since the last actual Wordfence request
// (simulated here via backdateLastRequest rather than a real 31-minute
// sleep), the next run fetches the feed the cursor still points at (the one
// abandoned by the earlier spacing-skip scenario) and, on success, advances
// the cursor — proving production is fetched on the first eligible run
// instead of being skipped over indefinitely.
func TestFeedWorker_EligibleAfterSpacing_FetchesIntendedFeedAndAdvances(t *testing.T) {
	pool := startPostgres(t)
	const vulnID = "245-eligible-vuln-1"
	client := newFakeFeedHTTPClient()
	client.setResponse(vuln.ScannerFeedURL, http.StatusOK, scannerBodyNoCVSS(vulnID))
	client.setResponse(vuln.ProductionFeedURL, http.StatusOK, productionBodyWithCVSS(vulnID))

	w := buildFeedWorker(pool, client)
	repo := vuln.NewRepo(pool)

	if err := runFeedWork(t, w); err != nil {
		t.Fatalf("run 1 (scanner): %v", err)
	}
	backdateLastRequest(t, pool, 32*time.Minute)

	before := client.callCount()
	if err := runFeedWork(t, w); err != nil {
		t.Fatalf("run 2 (eligible production): %v", err)
	}
	after := client.callCount()
	if after-before != 1 {
		t.Errorf("run 2: made %d HTTP request(s); want exactly 1 (now eligible)", after-before)
	}
	if got := client.lastCall(); got != vuln.ProductionFeedURL {
		t.Errorf("run 2: fetched %q; want %q (cursor pointed at production)", got, vuln.ProductionFeedURL)
	}

	gate, err := repo.GetFeedGate(context.Background())
	if err != nil {
		t.Fatalf("GetFeedGate: %v", err)
	}
	if gate.FeedKind != vuln.FeedKindScanner {
		t.Errorf("cursor after a successful eligible production ingest = %q; want %q (advanced)", gate.FeedKind, vuln.FeedKindScanner)
	}
}

// TestFeedWorker_LastRequestAt_UpdatesOn200And429 covers (c): last_request_at
// must advance on EVERY actual Wordfence request, whether it succeeds (200)
// or is rate-limited (429) — both consume the shared rate-limit slot, so both
// must feed the wall-clock spacing gate (otherwise a 429 would look like "no
// request happened" and the very next run could immediately re-hit the same
// still-limited window).
func TestFeedWorker_LastRequestAt_UpdatesOn200And429(t *testing.T) {
	pool := startPostgres(t)
	const vulnID = "245-last-req-vuln-1"
	client := newFakeFeedHTTPClient()
	client.setResponse(vuln.ScannerFeedURL, http.StatusOK, scannerBodyNoCVSS(vulnID))
	client.setResponse(vuln.ProductionFeedURL, http.StatusTooManyRequests, `{"error":"rate limited"}`)

	w := buildFeedWorker(pool, client)
	repo := vuln.NewRepo(pool)

	// Run 1: scanner succeeds (200) — last_request_at must be set.
	if err := runFeedWork(t, w); err != nil {
		t.Fatalf("run 1 (scanner 200): %v", err)
	}
	gate, err := repo.GetFeedGate(context.Background())
	if err != nil {
		t.Fatalf("GetFeedGate after run 1: %v", err)
	}
	if gate.LastRequestAt == nil {
		t.Fatal("LastRequestAt is nil after a successful (200) request; want non-nil")
	}
	firstStamp := *gate.LastRequestAt

	// Make run 2 eligible, then hit the 429 on production.
	backdateLastRequest(t, pool, 32*time.Minute)
	if err := runFeedWork(t, w); err != nil {
		t.Fatalf("run 2 (production 429): %v", err)
	}
	gate, err = repo.GetFeedGate(context.Background())
	if err != nil {
		t.Fatalf("GetFeedGate after run 2: %v", err)
	}
	if gate.LastRequestAt == nil {
		t.Fatal("LastRequestAt is nil after a 429; want non-nil (a 429 still consumes the rate-limit slot)")
	}
	if !gate.LastRequestAt.After(firstStamp) {
		t.Errorf("LastRequestAt after the 429 (%v) did not advance past the first stamp (%v)", gate.LastRequestAt, firstStamp)
	}
	// Cursor must NOT have advanced on the 429 (same feed retried next eligible run).
	if gate.FeedKind != vuln.FeedKindProduction {
		t.Errorf("cursor after a 429 = %q; want still %q (429 does not advance the cursor)", gate.FeedKind, vuln.FeedKindProduction)
	}
}

// ---------------------------------------------------------------------------
// Repo-level: severity 'unknown' bucket — CHECK constraint, counts, ordering.
// ---------------------------------------------------------------------------

// TestSeverityUnknown_StorageCountsAndOrdering exercises the full storage
// path for the new 'unknown' severity bucket (GH #245 WP1):
//   - the widened CHECK constraint accepts 'unknown' (a pre-migration schema
//     would 500 on this insert — this is the direct regression guard for
//     "the widening migration must land before any code writes it");
//   - FleetOpenCounts tallies it separately;
//   - the severity ORDER BY ranks it between 'high' and 'medium'.
func TestSeverityUnknown_StorageCountsAndOrdering(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	repo := vuln.NewRepo(pool)

	tenantID := uuid.New()
	siteID := uuid.New()
	insertTenantAndSite(t, pool, tenantID, siteID)

	// One finding per severity bucket, including the new 'unknown' one.
	severities := []string{
		vuln.SeverityCritical, vuln.SeverityHigh, vuln.SeverityUnknown,
		vuln.SeverityMedium, vuln.SeverityLow,
	}
	err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		for i, sev := range severities {
			if err := repo.UpsertFinding(ctx, tx, vuln.FindingUpsert{
				TenantID:         tenantID,
				SiteID:           siteID,
				VulnID:           uuid.NewString(),
				Kind:             "plugin",
				Slug:             "plug",
				Name:             "Plug",
				InstalledVersion: "1.0",
				Severity:         sev,
				Title:            "finding " + sev,
			}); err != nil {
				return err
			}
			_ = i
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed findings (severity CHECK constraint must accept 'unknown'): %v", err)
	}

	// FleetOpenCounts must tally 'unknown' separately.
	critical, high, medium, low, unknown, err := repo.FleetOpenCounts(ctx, tenantID)
	if err != nil {
		t.Fatalf("FleetOpenCounts: %v", err)
	}
	if critical != 1 || high != 1 || medium != 1 || low != 1 || unknown != 1 {
		t.Errorf("counts = critical=%d high=%d medium=%d low=%d unknown=%d; want 1 each",
			critical, high, medium, low, unknown)
	}

	// ORDER BY must rank: critical(1) < high(2) < unknown(3) < medium(4) < low(5).
	findings, err := repo.ListOpenFindings(ctx, tenantID, siteID)
	if err != nil {
		t.Fatalf("ListOpenFindings: %v", err)
	}
	if len(findings) != 5 {
		t.Fatalf("len(findings) = %d; want 5", len(findings))
	}
	gotOrder := make([]string, len(findings))
	for i, f := range findings {
		gotOrder[i] = f.Severity
	}
	wantOrder := []string{
		vuln.SeverityCritical, vuln.SeverityHigh, vuln.SeverityUnknown,
		vuln.SeverityMedium, vuln.SeverityLow,
	}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Errorf("severity order[%d] = %q; want %q (full order: %v)", i, gotOrder[i], wantOrder[i], gotOrder)
			break
		}
	}
}

// insertTenantAndSite creates the minimal tenant + site rows needed for a
// site_vulnerabilities FK-satisfying insert.
func insertTenantAndSite(t *testing.T, pool *db.Pool, tenantID, siteID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, name, slug) VALUES ($1, 'Test Tenant', $2)`,
			tenantID, "test-tenant-"+tenantID.String(),
		); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO sites (id, tenant_id, name, url) VALUES ($1, $2, 'Test Site', 'https://example.com')`,
			siteID, tenantID,
		)
		return err
	}); err != nil {
		t.Fatalf("seed tenant/site: %v", err)
	}
}
