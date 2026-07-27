package vuln

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Repo is the data-access layer for the vuln domain.  All tenant-scoped
// operations run under pool.InTenantTx or pool.InAgentTx as appropriate.
// The global feed tables (wordfence_vuln_*) are written by the ingester via
// pool.InAgentTx and read without a tenant GUC.
type Repo struct {
	pool *db.Pool
}

// NewRepo builds a Repo.
func NewRepo(pool *db.Pool) *Repo {
	return &Repo{pool: pool}
}

// ---------------------------------------------------------------------------
// Feed ingestion (global, no RLS)
// ---------------------------------------------------------------------------

// UpsertFeedRecord inserts or replaces one vulnerability record and its
// associated software rows inside the provided transaction.  Called once per
// record during the feed import batch.
//
// cve/cve_link/cvss_score/cvss_rating/cwe/reference_urls are written STICKY
// against the existing row: under the Scanner/Production alternation (GH
// #245), a Scanner run's records typically carry none of these fields at all
// (nilString turns "" into NULL, CVSSScore is already a nil-able *float64,
// and parseFeedRecord defaults an absent references[] to a non-NULL '[]'
// jsonb — never NULL), and a blind overwrite would erase enrichment a prior
// Production run had already populated — flapping severity AND reference
// link-outs between real values and empty every other hour. A Production
// run's non-empty values always win: cve/cve_link/cvss_score/cvss_rating/cwe
// via COALESCE (NULL loses to any non-NULL), reference_urls via
// COALESCE(NULLIF(...,'[]'::jsonb), ...) since its "no data" sentinel is the
// literal empty array, not SQL NULL.
func (r *Repo) UpsertFeedRecord(ctx context.Context, tx pgx.Tx, rec FeedRecord) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO wordfence_vuln_feed
			(vuln_id, title, cve, cve_link, cvss_score, cvss_rating, cwe,
			 informational, reference_urls, published, updated, raw)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (vuln_id) DO UPDATE SET
			title          = EXCLUDED.title,
			cve            = COALESCE(EXCLUDED.cve, wordfence_vuln_feed.cve),
			cve_link       = COALESCE(EXCLUDED.cve_link, wordfence_vuln_feed.cve_link),
			cvss_score     = COALESCE(EXCLUDED.cvss_score, wordfence_vuln_feed.cvss_score),
			cvss_rating    = COALESCE(EXCLUDED.cvss_rating, wordfence_vuln_feed.cvss_rating),
			cwe            = COALESCE(EXCLUDED.cwe, wordfence_vuln_feed.cwe),
			informational  = EXCLUDED.informational,
			reference_urls = COALESCE(NULLIF(EXCLUDED.reference_urls, '[]'::jsonb), wordfence_vuln_feed.reference_urls),
			published      = EXCLUDED.published,
			updated        = EXCLUDED.updated,
			raw            = EXCLUDED.raw`,
		rec.VulnID, rec.Title, nilString(rec.CVE), nilString(rec.CVELink),
		rec.CVSSScore, nilString(rec.CVSSRating), rec.CWE,
		rec.Informational, rec.References, rec.Published, rec.Updated, rec.Raw,
	)
	if err != nil {
		return fmt.Errorf("upsert vuln feed record %s: %w", rec.VulnID, err)
	}

	// Delete stale software rows for this vuln (re-insert below ensures freshness).
	if _, err := tx.Exec(ctx,
		`DELETE FROM wordfence_vuln_software WHERE vuln_id = $1`, rec.VulnID,
	); err != nil {
		return fmt.Errorf("delete stale software rows for %s: %w", rec.VulnID, err)
	}

	// Re-insert software rows.
	// normSlug canonicalises (case + directory form) on ingest so the feed
	// canonical slug and an agent inventory slug always compare equal (see
	// normSlug's doc comment below for the full rationale). The original
	// mixed-case/raw slug is NOT stored because the conflict key
	// (vuln_id, kind, slug) must be stable, and slug is matched in
	// LookupSoftware using the same normalised value derived from the agent
	// inventory.
	for _, sw := range rec.Software {
		slug := normSlug(sw.Slug)
		if slug == "" {
			// A software row whose slug normalises to empty can never be
			// looked up (LookupSoftware refuses an empty normalised query
			// key too; see its guard below) and would only ever occupy an
			// unreachable row. BulkReplaceAllSoftware already carries this
			// same guard for the bulk ingest path; this is the equivalent
			// for the single-record path.
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO wordfence_vuln_software
				(vuln_id, kind, slug, affected_versions, patched, patched_versions)
			VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (vuln_id, kind, slug) DO UPDATE SET
				affected_versions = EXCLUDED.affected_versions,
				patched           = EXCLUDED.patched,
				patched_versions  = EXCLUDED.patched_versions`,
			rec.VulnID, sw.Kind, slug, sw.AffectedVersions, sw.Patched, sw.PatchedVersions,
		); err != nil {
			return fmt.Errorf("upsert software row vuln=%s kind=%s slug=%s: %w",
				rec.VulnID, sw.Kind, slug, err)
		}
	}
	return nil
}

// BulkUpsertFeedRecords upserts all feed records in a single pgx Batch, sending
// every INSERT ... ON CONFLICT DO UPDATE in one round-trip instead of one per record.
// This replaces the O(N) per-record path that timed out on 13k-record full-dump ingest.
//
// Sticky enrichment: see the identical rationale on UpsertFeedRecord — the
// cve/cve_link/cvss_score/cvss_rating/cwe/reference_urls columns are all
// preserved (COALESCE, or COALESCE+NULLIF for reference_urls whose "no data"
// sentinel is the literal '[]' jsonb rather than NULL) so a Scanner run's
// records (which normally carry none of these) can never blank out a prior
// Production run's enrichment — including its reference link-outs.
func (r *Repo) BulkUpsertFeedRecords(ctx context.Context, tx pgx.Tx, recs []FeedRecord) error {
	if len(recs) == 0 {
		return nil
	}

	const upsertSQL = `
		INSERT INTO wordfence_vuln_feed
			(vuln_id, title, cve, cve_link, cvss_score, cvss_rating, cwe,
			 informational, reference_urls, published, updated, raw)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (vuln_id) DO UPDATE SET
			title          = EXCLUDED.title,
			cve            = COALESCE(EXCLUDED.cve, wordfence_vuln_feed.cve),
			cve_link       = COALESCE(EXCLUDED.cve_link, wordfence_vuln_feed.cve_link),
			cvss_score     = COALESCE(EXCLUDED.cvss_score, wordfence_vuln_feed.cvss_score),
			cvss_rating    = COALESCE(EXCLUDED.cvss_rating, wordfence_vuln_feed.cvss_rating),
			cwe            = COALESCE(EXCLUDED.cwe, wordfence_vuln_feed.cwe),
			informational  = EXCLUDED.informational,
			reference_urls = COALESCE(NULLIF(EXCLUDED.reference_urls, '[]'::jsonb), wordfence_vuln_feed.reference_urls),
			published      = EXCLUDED.published,
			updated        = EXCLUDED.updated,
			raw            = EXCLUDED.raw`

	batch := &pgx.Batch{}
	for _, rec := range recs {
		batch.Queue(upsertSQL,
			rec.VulnID, rec.Title, nilString(rec.CVE), nilString(rec.CVELink),
			rec.CVSSScore, nilString(rec.CVSSRating), rec.CWE,
			rec.Informational, rec.References, rec.Published, rec.Updated, rec.Raw,
		)
	}

	br := tx.SendBatch(ctx, batch)
	defer func() { _ = br.Close() }()

	for i := range recs {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("bulk upsert feed record %d (%s): %w", i, recs[i].VulnID, err)
		}
	}
	return br.Close()
}

// BulkUpsertFeedRecordsPool wraps BulkUpsertFeedRecords in its own short-lived
// transaction — used by the streaming Production ingest path
// (FeedWorker.fetchAndIngestProduction), which flushes bounded batches as the
// feed is decoded rather than holding one long transaction open for the
// entire (potentially very large) response. Production only ever upserts
// existing/enrichment data (never BulkReplaceAllSoftware/PruneMissingVulns —
// see ingestRecords), so each batch committing independently is safe: a
// mid-stream failure on a later batch leaves earlier batches durably
// enriched rather than losing all forward progress.
func (r *Repo) BulkUpsertFeedRecordsPool(ctx context.Context, recs []FeedRecord) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.agent','on',true)"); err != nil {
		return fmt.Errorf("set agent guc: %w", err)
	}
	if err := r.BulkUpsertFeedRecords(ctx, tx, recs); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// BulkReplaceAllSoftware replaces the software rows for every vuln_id in recs
// using two set-based operations instead of per-record DELETE + INSERT loops:
//
//  1. A single DELETE that removes all existing software rows whose vuln_id is in
//     the batch. This is one network round-trip regardless of how many vulns or
//     software rows exist.
//  2. A pgx CopyFrom that streams all new software rows to Postgres in a single
//     COPY protocol frame.
//
// Together these replace the previous O(N×M) approach (13k vulns × per-vuln DELETE
// + per-software-row INSERT) that triggered a context deadline on full-dump ingest.
//
// Duplicate handling: CopyFrom uses the COPY protocol which cannot express
// ON CONFLICT DO UPDATE. Any duplicate (vuln_id, kind, slug) tuples in the
// input batch would therefore violate the PK and abort the entire COPY.
// Two real sources produce duplicates:
//
//   - A single feed record's software[] array listing the same (kind, slug) more
//     than once (e.g. a plugin entry that appears with two different affected-version
//     ranges).
//   - A future feed revision that lists the same (kind, slug) with a split
//     affected-version range across two entries.
//
// Both are eliminated by dedupSoftwareRows, which merges rows sharing the same
// normalised (vuln_id, kind, slug) key. A final map-based guard prevents any
// residual duplicate from reaching CopyFrom regardless of feed quirks.
func (r *Repo) BulkReplaceAllSoftware(ctx context.Context, tx pgx.Tx, recs []FeedRecord) error {
	if len(recs) == 0 {
		return nil
	}

	// Collect all vuln_ids being replaced.
	vulnIDs := make([]string, len(recs))
	for i, rec := range recs {
		vulnIDs[i] = rec.VulnID
	}

	// Step 1: delete all software rows for these vulns in one statement.
	if _, err := tx.Exec(ctx,
		`DELETE FROM wordfence_vuln_software WHERE vuln_id = ANY($1::text[])`,
		vulnIDs,
	); err != nil {
		return fmt.Errorf("bulk delete software rows: %w", err)
	}

	// Step 2: collect all new software rows and stream them via CopyFrom.
	// Preallocate a reasonable capacity (average ~2 software rows per vuln).
	// Apply per-record dedup first (mergeSoftwareRows), then a final PK-level
	// guard map to prevent any residual duplicate from reaching CopyFrom.
	type pkKey struct{ vulnID, kind, slug string }
	seen := make(map[pkKey]struct{}, len(recs)*2)

	rows := make([][]any, 0, len(recs)*2)
	for _, rec := range recs {
		for _, sw := range dedupSoftwareRows(rec.Software) {
			slug := normSlug(sw.Slug)
			if slug == "" {
				continue
			}
			pk := pkKey{rec.VulnID, sw.Kind, slug}
			if _, dup := seen[pk]; dup {
				// Should not reach here after dedupSoftwareRows, but guard anyway.
				continue
			}
			seen[pk] = struct{}{}
			rows = append(rows, []any{
				rec.VulnID,
				sw.Kind,
				slug,
				sw.AffectedVersions,
				sw.Patched,
				sw.PatchedVersions,
			})
		}
	}

	if len(rows) == 0 {
		return nil // nothing to copy (all vulns had no software — should not occur in practice)
	}

	cols := []string{"vuln_id", "kind", "slug", "affected_versions", "patched", "patched_versions"}
	if _, err := tx.CopyFrom(ctx,
		pgx.Identifier{"wordfence_vuln_software"},
		cols,
		pgx.CopyFromRows(rows),
	); err != nil {
		return fmt.Errorf("bulk copy software rows: %w", err)
	}
	return nil
}

// dedupSoftwareRows collapses any SoftwareRow entries that share the same
// normalised (kind, slug) key by merging their version data.  It is called
// per-record inside BulkReplaceAllSoftware before building the CopyFrom batch.
//
// Merge semantics:
//   - affected_versions: union of JSON object keys from all duplicate entries.
//     Both the feed and the real Wordfence schema represent affected_versions as
//     a JSON object keyed by a human-readable range label (e.g. "1.0.0 - 1.2.3").
//     Merging their keys ensures the matcher sees every affected range.
//   - patched: OR — true if any duplicate entry says patched=true (a fix exists
//     even if only one feed variant lists patched_versions).
//   - patched_versions: union of the two arrays, deduplicated.
//
// The function is a no-op (O(n) scan, no allocation) when every (kind, slug)
// key is already unique, which is the common case.
func dedupSoftwareRows(rows []SoftwareRow) []SoftwareRow {
	if len(rows) <= 1 {
		return rows
	}

	// Fast path: check whether there are any duplicates at all. If the set of
	// normalised (kind, slug) keys has the same cardinality as the slice, return
	// the slice unchanged (no allocation, no merge).
	type key struct{ kind, slug string }
	keySet := make(map[key]int, len(rows)) // maps key → first-seen index in result
	hasDup := false
	for _, sw := range rows {
		k := key{sw.Kind, normSlug(sw.Slug)}
		if _, exists := keySet[k]; exists {
			hasDup = true
			break
		}
		keySet[k] = 0
	}
	if !hasDup {
		return rows
	}

	// Slow path: rebuild the slice, merging duplicates as we go.
	type entry struct {
		idx int // position in result slice
		sw  SoftwareRow
	}
	result := make([]SoftwareRow, 0, len(rows))
	indexByKey := make(map[key]int, len(rows)) // key → index in result

	for _, sw := range rows {
		k := key{sw.Kind, normSlug(sw.Slug)}
		if idx, exists := indexByKey[k]; !exists {
			indexByKey[k] = len(result)
			result = append(result, sw)
		} else {
			// Merge into the existing entry.
			existing := &result[idx]
			existing.AffectedVersions = mergeAffectedVersions(existing.AffectedVersions, sw.AffectedVersions)
			existing.Patched = existing.Patched || sw.Patched
			existing.PatchedVersions = mergePatchedVersions(existing.PatchedVersions, sw.PatchedVersions)
		}
	}
	return result
}

// mergeAffectedVersions unions two JSON objects keyed by range labels.
// Both inputs are expected to be JSON objects (the Wordfence v3 schema).
// If either is empty/null/not-an-object, the other is returned unchanged.
// On any parse error the first input is returned as-is (defensive: don't lose data).
func mergeAffectedVersions(a, b []byte) []byte {
	if len(a) == 0 || string(a) == "null" || string(a) == "{}" {
		if len(b) > 0 {
			return b
		}
		return a
	}
	if len(b) == 0 || string(b) == "null" || string(b) == "{}" {
		return a
	}
	// Both are non-empty: unmarshal as maps and union their keys.
	var ma, mb map[string]json.RawMessage
	if err := json.Unmarshal(a, &ma); err != nil {
		return a // a is not an object; keep a and discard b
	}
	if err := json.Unmarshal(b, &mb); err != nil {
		return a // b is not an object; keep a
	}
	for k, v := range mb {
		if _, exists := ma[k]; !exists {
			ma[k] = v
		}
	}
	merged, err := json.Marshal(ma)
	if err != nil {
		return a
	}
	return merged
}

// mergePatchedVersions unions two JSON arrays of version strings, deduplicating.
// Both inputs are expected to be JSON arrays.
// If either is empty/null, the other is returned unchanged.
func mergePatchedVersions(a, b []byte) []byte {
	if len(a) == 0 || string(a) == "null" || string(a) == "[]" {
		if len(b) > 0 {
			return b
		}
		return a
	}
	if len(b) == 0 || string(b) == "null" || string(b) == "[]" {
		return a
	}
	var va, vb []string
	if err := json.Unmarshal(a, &va); err != nil {
		return a
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		return a
	}
	seen := make(map[string]struct{}, len(va)+len(vb))
	merged := make([]string, 0, len(va)+len(vb))
	for _, v := range va {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			merged = append(merged, v)
		}
	}
	for _, v := range vb {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			merged = append(merged, v)
		}
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return a
	}
	return out
}

// PruneMissingVulns deletes feed rows whose vuln_id is not in the provided set.
// Called after a full-dump ingest to remove retracted vulnerabilities.
func (r *Repo) PruneMissingVulns(ctx context.Context, tx pgx.Tx, knownIDs []string) error {
	if len(knownIDs) == 0 {
		// Safety: if the ingested set is empty (e.g. feed returned nothing) do NOT
		// prune — something went wrong upstream.
		return nil
	}
	// pgx parameterized ANY($1::text[]): deletes every row NOT in the full set.
	if _, err := tx.Exec(ctx,
		`DELETE FROM wordfence_vuln_feed WHERE vuln_id != ALL($1::text[])`,
		knownIDs,
	); err != nil {
		return fmt.Errorf("prune missing vulns: %w", err)
	}
	return nil
}

// StampFeedMeta writes the freshness + attribution sentinel row after a
// successful ingest of either feed, and ADVANCES the Scanner/Production
// alternation cursor (next_feed_kind) to the other value.
//
// StampFeedMeta is called ONLY on a successful, non-empty ingest (see
// FeedWorker.ingestRecords) — every other outcome (spacing-skip, 429,
// zero-record response, hard fetch/ingest error) leaves the cursor untouched,
// so a feed that hasn't yet been successfully fetched is never abandoned: the
// next eligible (>=31min-spaced) run retries the SAME feed kind until it
// finally lands (GH #245 wall-clock-spacing follow-up).
//
// defiant_notice/defiant_license/mitre_notice are written via COALESCE so a
// run whose records happened not to carry a copyrights block (odd shape,
// truncated response, etc.) never blanks out previously-captured attribution
// text — that text is a standing legal display obligation, not a per-run
// value.
//
// enrichment_ok/last_enrichment_at are ALSO COALESCE-preserved: meta.EnrichmentOK
// and meta.LastEnrichmentAt are nil-able pointers, and nil means "this run did
// not touch enrichment" (the Scanner-run case) — leaving the last Production
// run's enrichment status sticky rather than flapping it false every other
// hour purely because Scanner doesn't carry CVSS.
func (r *Repo) StampFeedMeta(ctx context.Context, tx pgx.Tx, meta FeedMetaUpdate) error {
	_, err := tx.Exec(ctx, `
		UPDATE wordfence_vuln_feed_meta SET
			fetched_at         = $1,
			ok                 = $2,
			record_count       = $3,
			defiant_notice     = COALESCE($4, defiant_notice),
			defiant_license    = COALESCE($5, defiant_license),
			mitre_notice       = COALESCE($6, mitre_notice),
			last_error         = $7,
			enrichment_ok      = COALESCE($8, enrichment_ok),
			last_enrichment_at = COALESCE($9, last_enrichment_at),
			next_feed_kind     = CASE next_feed_kind
				WHEN 'scanner' THEN 'production'
				ELSE 'scanner'
			END
		WHERE id = 1`,
		meta.FetchedAt, meta.OK, meta.RecordCount,
		nilString(meta.DefiantNotice), nilString(meta.DefiantLicense),
		nilString(meta.MitreNotice), nilString(meta.LastError),
		meta.EnrichmentOK, meta.LastEnrichmentAt,
	)
	return err
}

// StampFeedMetaFinal opens its own transaction to call StampFeedMeta directly
// against the pool — used by the streaming Production ingest path
// (FeedWorker.fetchAndIngestProduction / workProduction), whose record
// batches are already committed incrementally (BulkUpsertFeedRecordsPool)
// rather than atomically with the meta stamp. Calling this is what advances
// next_feed_kind (via the CASE in StampFeedMeta's SQL), so it must only be
// called once the ENTIRE stream has completed successfully — never on a
// mid-stream failure, which returns an error before reaching this call.
func (r *Repo) StampFeedMetaFinal(ctx context.Context, meta FeedMetaUpdate) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := r.StampFeedMeta(ctx, tx, meta); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// StampRequestAt records the wall-clock time of an ACTUAL Wordfence HTTP
// request — called whenever fetchFeed got a response from Wordfence's server,
// regardless of status code (200 OR 429 both consume the shared ~30-min
// rate-limit slot; a transport-level failure that never reached Wordfence
// does not). This is deliberately separate from fetched_at, which is
// success-only: last_request_at is the wall-clock input to Work()'s spacing
// gate (GetFeedGate), so the gate is accurate even across a run that 429'd.
func (r *Repo) StampRequestAt(ctx context.Context, at time.Time) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE wordfence_vuln_feed_meta SET last_request_at = $1 WHERE id = 1`,
		at,
	)
	return err
}

// StampFeedError records a DETECTION-feed error: it sets ok = false (the flag
// RescanSite gates on) and last_error. It must only be called for a Scanner
// fetch failure or a hard/non-rate-limit failure that affects the feed as a
// whole — never for a routine Production-only hiccup (use
// StampEnrichmentDegraded for that), or detection rescans would wrongly skip
// every other hour once Scanner/Production alternation is in play.
func (r *Repo) StampFeedError(ctx context.Context, lastError string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE wordfence_vuln_feed_meta SET ok = false, last_error = $1 WHERE id = 1`,
		lastError,
	)
	return err
}

// StampEnrichmentDegraded records a Production-fetch failure or degradation
// (rate-limited, zero records, transient error) WITHOUT touching the
// detection-feed ok/fetched_at/record_count fields. Those track Scanner-driven
// detection freshness and must stay exactly as the last successful Scanner run
// left them — a Production hiccup is real (surfaced via enrichment_ok=false +
// last_error) but must never make RescanSite skip a cycle.
func (r *Repo) StampEnrichmentDegraded(ctx context.Context, lastError string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE wordfence_vuln_feed_meta SET enrichment_ok = false, last_error = $1 WHERE id = 1`,
		lastError,
	)
	return err
}

// GetFeedMeta returns the current feed sentinel row.
func (r *Repo) GetFeedMeta(ctx context.Context) (FeedMeta, error) {
	var m FeedMeta
	var fetchedAt, lastEnrichmentAt pgtype.Timestamptz
	var defiantNotice, defiantLicense, mitreNotice, lastError pgtype.Text
	err := r.pool.QueryRow(ctx, `
		SELECT fetched_at, ok, record_count,
		       defiant_notice, defiant_license, mitre_notice, last_error,
		       enrichment_ok, last_enrichment_at
		FROM wordfence_vuln_feed_meta WHERE id = 1`,
	).Scan(&fetchedAt, &m.OK, &m.RecordCount,
		&defiantNotice, &defiantLicense, &mitreNotice, &lastError,
		&m.EnrichmentOK, &lastEnrichmentAt)
	if err != nil {
		return m, fmt.Errorf("get feed meta: %w", err)
	}
	if fetchedAt.Valid {
		t := fetchedAt.Time
		m.FetchedAt = &t
	}
	if lastEnrichmentAt.Valid {
		t := lastEnrichmentAt.Time
		m.LastEnrichmentAt = &t
	}
	m.DefiantNotice = defiantNotice.String
	m.DefiantLicense = defiantLicense.String
	m.MitreNotice = mitreNotice.String
	m.LastError = lastError.String
	return m, nil
}

// GetFeedMetaStatus returns the condensed feed meta fields needed by the
// superadmin status endpoint. This satisfies admin.VulnFeedMetaReader.
// Returns zero values with nil error when the meta row has never been written
// (fetched_at IS NULL) — i.e. the feed has not yet run.
func (r *Repo) GetFeedMetaStatus(ctx context.Context) (ok bool, recordCount int, lastSynced *time.Time, lastError string, enrichmentOK bool, lastEnrichmentAt *time.Time, err error) {
	meta, ferr := r.GetFeedMeta(ctx)
	if ferr != nil {
		return false, 0, nil, "", false, nil, ferr
	}
	return meta.OK, meta.RecordCount, meta.FetchedAt, meta.LastError, meta.EnrichmentOK, meta.LastEnrichmentAt, nil
}

// FeedGate is the worker's fetch-eligibility + alternation-cursor state, read
// once at the top of Work(). Internal worker bookkeeping only — never
// exposed via the API (mirrors next_feed_kind's existing internal-only status).
type FeedGate struct {
	// FeedKind is the feed ("scanner"|"production") this cycle should fetch
	// IF eligible. It only changes when StampFeedMeta successfully advances it
	// after a completed ingest — never on a read.
	FeedKind string
	// LastRequestAt is the wall-clock time of the last ACTUAL Wordfence HTTP
	// request (any status code), or nil if none has ever been made. Work()
	// uses this — NOT an assumption about job cadence — to enforce the
	// ~30-min global rate-limit spacing (GH #245 follow-up: consecutive
	// periodic + manually-triggered runs can land far closer together than
	// the nominal hourly cadence).
	LastRequestAt *time.Time
}

// GetFeedGate reads the current alternation cursor and last-request timestamp
// WITHOUT mutating anything — a pure read. Advancing next_feed_kind (via
// StampFeedMeta) and stamping last_request_at (via StampRequestAt) are
// separate, explicit steps the caller invokes only after it has decided to
// actually make a request and, for the cursor, only after that request's
// ingest succeeds.
func (r *Repo) GetFeedGate(ctx context.Context) (FeedGate, error) {
	var g FeedGate
	var lastRequestAt pgtype.Timestamptz
	err := r.pool.QueryRow(ctx, `
		SELECT next_feed_kind, last_request_at FROM wordfence_vuln_feed_meta WHERE id = 1`,
	).Scan(&g.FeedKind, &lastRequestAt)
	if err != nil {
		return g, fmt.Errorf("get feed gate: %w", err)
	}
	if lastRequestAt.Valid {
		t := lastRequestAt.Time
		g.LastRequestAt = &t
	}
	return g, nil
}

// LookupSoftware returns all vulnerability software rows for the given (kind, slug).
// Reads without a tenant GUC (global public table).
// The slug is canonicalised (normSlug: directory form + lower-case) before
// comparison to match the normalisation applied on ingest (see
// UpsertFeedRecord). This is what lets a plugin's agent-reported inventory
// slug, a get_plugins() array-key FILE PATH such as
// "woocommerce/woocommerce.php", match the Wordfence feed's bare canonical
// directory slug ("woocommerce"). See normSlug's doc comment for the full
// rationale and known limitations.
func (r *Repo) LookupSoftware(ctx context.Context, kind, slug string) ([]VulnSoftwareRow, error) {
	querySlug := normSlug(slug)
	if querySlug == "" {
		// An empty normalised slug must never be used as a lookup key. A raw
		// slug like "/x" (a malicious or malformed agent-reported value)
		// normalises to "" because strings.Cut's directory component before
		// the first "/" is empty; every real stored row's slug is guaranteed
		// non-empty (see the empty-slug guards on the ingest side in
		// UpsertFeedRecord/BulkReplaceAllSoftware/parseFeedRecord), so an
		// empty query key can only ever be an attempt to match rows that do
		// not exist. Refuse outright rather than relying on that invariant
		// never being violated on either side.
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT s.vuln_id, s.kind, s.slug, s.affected_versions, s.patched, s.patched_versions,
		       f.title, f.cve, f.cve_link, f.cvss_score, f.cvss_rating, f.reference_urls
		FROM wordfence_vuln_software s
		JOIN wordfence_vuln_feed f USING (vuln_id)
		WHERE s.kind = $1 AND s.slug = $2`, kind, querySlug)
	if err != nil {
		return nil, fmt.Errorf("lookup software %s/%s: %w", kind, slug, err)
	}
	defer rows.Close()

	var result []VulnSoftwareRow
	for rows.Next() {
		var row VulnSoftwareRow
		var cve, cveLink, cvssRating pgtype.Text
		var cvssScore pgtype.Numeric
		if err := rows.Scan(
			&row.VulnID, &row.Kind, &row.Slug,
			&row.AffectedVersions, &row.Patched, &row.PatchedVersions,
			&row.Title, &cve, &cveLink, &cvssScore, &cvssRating,
			&row.References,
		); err != nil {
			return nil, fmt.Errorf("scan software row: %w", err)
		}
		row.CVE = cve.String
		row.CVELink = cveLink.String
		row.CVSSRating = cvssRating.String
		if cvssScore.Valid {
			f, _ := cvssScore.Float64Value()
			if f.Valid {
				v := f.Float64
				row.CVSSScore = &v
			}
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Findings (tenant-scoped, RLS-enforced)
// ---------------------------------------------------------------------------

// UpsertFinding inserts or refreshes a matched finding for a site.
// Dismissed findings are only updated (last_seen + installed_version) if the
// installed version has changed, preserving the dismiss decision otherwise.
func (r *Repo) UpsertFinding(ctx context.Context, tx pgx.Tx, f FindingUpsert) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO site_vulnerabilities
			(tenant_id, site_id, vuln_id, kind, slug, name,
			 installed_version, fixed_version, severity, cvss_score,
			 cve, title, status, first_seen, last_seen)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'open',now(),now())
		ON CONFLICT (site_id, vuln_id, kind, slug) DO UPDATE SET
			last_seen         = now(),
			installed_version = EXCLUDED.installed_version,
			fixed_version     = EXCLUDED.fixed_version,
			severity          = EXCLUDED.severity,
			cvss_score        = EXCLUDED.cvss_score,
			cve               = EXCLUDED.cve,
			title             = EXCLUDED.title,
			name              = EXCLUDED.name,
			-- Re-open a resolved finding when the same vuln re-appears.
			status = CASE
				WHEN site_vulnerabilities.status = 'resolved' THEN 'open'
				ELSE site_vulnerabilities.status
			END,
			resolved_at = CASE
				WHEN site_vulnerabilities.status = 'resolved' THEN NULL
				ELSE site_vulnerabilities.resolved_at
			END,
			-- m103 (GH #247): a reappearing vulnerability alerts again. Every
			-- OTHER branch of this upsert (a routine re-match of an already-open
			-- finding, e.g. on every rescan) must NEVER touch notified_at — that
			-- is the sole responsibility of the alert dispatcher's claim
			-- (Repo.ClaimUnnotifiedFindings) and an enrichment-only DO UPDATE
			-- (e.g. the Production feed later filling in a CVSS score for an
			-- already-claimed 'unknown' finding) must not silently re-arm it.
			notified_at = CASE
				WHEN site_vulnerabilities.status = 'resolved' THEN NULL
				ELSE site_vulnerabilities.notified_at
			END`,
		f.TenantID, f.SiteID, f.VulnID, f.Kind, f.Slug, f.Name,
		f.InstalledVersion, nilString(f.FixedVersion), f.Severity,
		f.CVSSScore, nilString(f.CVE), f.Title,
	)
	if err != nil {
		return fmt.Errorf("upsert finding vuln=%s site=%s: %w", f.VulnID, f.SiteID, err)
	}
	return nil
}

// ResolveStaleFindings marks open findings for the site as resolved when their
// vuln_id is NOT in the current matched set (i.e. the vulnerability no longer
// applies after an update or the item was removed).
func (r *Repo) ResolveStaleFindings(ctx context.Context, tx pgx.Tx, tenantID, siteID uuid.UUID, matchedVulnIDs []string) error {
	if len(matchedVulnIDs) == 0 {
		// Resolve ALL open findings — the site has no vulnerabilities.
		_, err := tx.Exec(ctx, `
			UPDATE site_vulnerabilities SET
				status      = 'resolved',
				resolved_at = now()
			WHERE tenant_id = $1 AND site_id = $2 AND status = 'open'`,
			tenantID, siteID,
		)
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE site_vulnerabilities SET
			status      = 'resolved',
			resolved_at = now()
		WHERE tenant_id = $1 AND site_id = $2
		  AND status = 'open'
		  AND vuln_id != ALL($3::text[])`,
		tenantID, siteID, matchedVulnIDs,
	)
	return err
}

// ListOpenFindings returns the open findings for a site, severity-sorted.
func (r *Repo) ListOpenFindings(ctx context.Context, tenantID, siteID uuid.UUID) ([]Finding, error) {
	var findings []Finding
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT v.id, v.tenant_id, v.site_id, v.vuln_id, v.kind, v.slug, v.name,
			       v.installed_version, v.fixed_version, v.severity, v.cvss_score,
			       v.cve, v.title, v.status, v.first_seen, v.last_seen,
			       v.resolved_at, v.dismissed_at, v.dismissed_by,
			       f.cve_link, f.reference_urls
			FROM site_vulnerabilities v
			LEFT JOIN wordfence_vuln_feed f USING (vuln_id)
			WHERE v.tenant_id = $1 AND v.site_id = $2 AND v.status = 'open'
			ORDER BY
				CASE v.severity
					WHEN 'critical' THEN 1
					WHEN 'high'     THEN 2
					WHEN 'unknown'  THEN 3
					WHEN 'medium'   THEN 4
					WHEN 'low'      THEN 5
					ELSE 6
				END,
				v.cvss_score DESC NULLS LAST,
				v.first_seen DESC`,
			tenantID, siteID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f Finding
			if err := scanFinding(rows, &f); err != nil {
				return err
			}
			findings = append(findings, f)
		}
		return rows.Err()
	})
	return findings, err
}

// GetFinding returns a single finding by ID, tenant-scoped.
func (r *Repo) GetFinding(ctx context.Context, tenantID, siteID, findingID uuid.UUID) (Finding, error) {
	var f Finding
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT v.id, v.tenant_id, v.site_id, v.vuln_id, v.kind, v.slug, v.name,
			       v.installed_version, v.fixed_version, v.severity, v.cvss_score,
			       v.cve, v.title, v.status, v.first_seen, v.last_seen,
			       v.resolved_at, v.dismissed_at, v.dismissed_by,
			       COALESCE(fd.cve_link,''), COALESCE(fd.reference_urls,'[]'::jsonb)
			FROM site_vulnerabilities v
			LEFT JOIN wordfence_vuln_feed fd USING (vuln_id)
			WHERE v.tenant_id = $1 AND v.site_id = $2 AND v.id = $3`,
			tenantID, siteID, findingID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		if !rows.Next() {
			return domain.NotFound("finding_not_found", "vulnerability finding not found")
		}
		return scanFinding(rows, &f)
	})
	return f, err
}

// DismissFinding marks a finding as dismissed by the given user.
func (r *Repo) DismissFinding(ctx context.Context, tenantID, siteID, findingID, userID uuid.UUID) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE site_vulnerabilities SET
				status       = 'dismissed',
				dismissed_at = now(),
				dismissed_by = $4
			WHERE tenant_id = $1 AND site_id = $2 AND id = $3 AND status = 'open'`,
			tenantID, siteID, findingID, userID,
		)
		if err != nil {
			return domain.Internal("dismiss_finding_failed", "failed to dismiss finding").WithCause(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.NotFound("finding_not_found", "vulnerability finding not found or not in open state")
		}
		return nil
	})
}

// RestoreFinding re-opens a dismissed finding.
func (r *Repo) RestoreFinding(ctx context.Context, tenantID, siteID, findingID uuid.UUID) error {
	return r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE site_vulnerabilities SET
				status       = 'open',
				dismissed_at = NULL,
				dismissed_by = NULL
			WHERE tenant_id = $1 AND site_id = $2 AND id = $3 AND status = 'dismissed'`,
			tenantID, siteID, findingID,
		)
		if err != nil {
			return domain.Internal("restore_finding_failed", "failed to restore finding").WithCause(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.NotFound("finding_not_found", "vulnerability finding not found or not in dismissed state")
		}
		return nil
	})
}

// FleetOpenCounts returns the open finding counts per severity across all
// sites for a tenant.
func (r *Repo) FleetOpenCounts(ctx context.Context, tenantID uuid.UUID) (critical, high, medium, low, unknown int, err error) {
	err = r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT severity, count(*)
			FROM site_vulnerabilities
			WHERE tenant_id = $1 AND status = 'open'
			GROUP BY severity`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sev string
			var cnt int
			if err := rows.Scan(&sev, &cnt); err != nil {
				return err
			}
			switch sev {
			case SeverityCritical:
				critical = cnt
			case SeverityHigh:
				high = cnt
			case SeverityMedium:
				medium = cnt
			case SeverityLow:
				low = cnt
			case SeverityUnknown:
				unknown = cnt
			}
		}
		return rows.Err()
	})
	return
}

// FleetOpenFindings returns the cross-site open findings list for the tenant,
// ordered by severity then cvss_score then first_seen.
func (r *Repo) FleetOpenFindings(ctx context.Context, tenantID uuid.UUID, limit int) ([]FleetFindingRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var result []FleetFindingRow
	err := r.pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT v.id, v.tenant_id, v.site_id, v.vuln_id, v.kind, v.slug, v.name,
			       v.installed_version, v.fixed_version, v.severity, v.cvss_score,
			       v.cve, v.title, v.status, v.first_seen, v.last_seen,
			       v.resolved_at, v.dismissed_at, v.dismissed_by,
			       COALESCE(f.cve_link,''), COALESCE(f.reference_urls,'[]'::jsonb),
			       s.name AS site_name, s.url AS site_url
			FROM site_vulnerabilities v
			LEFT JOIN wordfence_vuln_feed f USING (vuln_id)
			JOIN sites s ON s.id = v.site_id
			WHERE v.tenant_id = $1 AND v.status = 'open'
			ORDER BY
				CASE v.severity
					WHEN 'critical' THEN 1
					WHEN 'high'     THEN 2
					WHEN 'unknown'  THEN 3
					WHEN 'medium'   THEN 4
					WHEN 'low'      THEN 5
					ELSE 6
				END,
				v.cvss_score DESC NULLS LAST,
				v.first_seen DESC
			LIMIT $2`,
			tenantID, limit,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row FleetFindingRow
			if err := scanFleetFindingRow(rows, &row); err != nil {
				return err
			}
			result = append(result, row)
		}
		return rows.Err()
	})
	return result, err
}

// ---------------------------------------------------------------------------
// Vulnerability alerting (m103, GH #247)
// ---------------------------------------------------------------------------

// ClaimedFinding is one site_vulnerabilities row claimed by
// ClaimUnnotifiedFindings, joined with its site's name/url so the alert
// dispatcher can build the batched email/webhook payload without a second
// round-trip.
type ClaimedFinding struct {
	Finding  Finding
	SiteName string
	SiteURL  string
}

// ListTenantsWithUnnotifiedFindings returns the distinct set of tenants that
// currently have at least one open, not-yet-notified finding
// (status='open' AND notified_at IS NULL). Runs under InAgentTx (cross-tenant
// enumeration, mirrors the feed worker's cross-tenant tenant listing) — the
// caller (Service.DispatchVulnAlerts) fans out to dispatchTenant per tenant,
// each of which claims and gates independently under its own InTenantTx.
func (r *Repo) ListTenantsWithUnnotifiedFindings(ctx context.Context) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := r.pool.InAgentTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT tenant_id FROM site_vulnerabilities
			WHERE status = 'open' AND notified_at IS NULL`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	return out, err
}

// ClaimUnnotifiedFindings atomically claims every open, not-yet-notified
// finding for a tenant by stamping notified_at = now(), and returns the
// claimed rows joined with their site's name/url. MUST run inside the
// caller's transaction (tx) — the claim and any resulting email-enqueue
// happen in the SAME tx so they commit or roll back together (transactional
// outbox; see Service.dispatchTenant). The UPDATE's row locks make concurrent
// dispatchers safe: a second dispatcher racing on the same tenant claims zero
// rows once the first has committed (or blocks until it does, then also
// claims zero).
//
// ALL matching rows are claimed regardless of severity, and regardless of
// whether alerting is even enabled for the tenant — the caller applies the
// notify_vulns/severity gate AFTER claiming. Stamping unconditionally means
// enabling alerts later (or lowering the threshold) never floods the tenant
// with a backlog of old findings that predate the change.
func (r *Repo) ClaimUnnotifiedFindings(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]ClaimedFinding, error) {
	rows, err := tx.Query(ctx, `
		WITH claimed AS (
			UPDATE site_vulnerabilities
			SET notified_at = now()
			WHERE tenant_id = $1 AND status = 'open' AND notified_at IS NULL
			RETURNING id, tenant_id, site_id, vuln_id, kind, slug, name,
			          installed_version, fixed_version, severity, cvss_score,
			          cve, title, status, first_seen, last_seen,
			          resolved_at, dismissed_at, dismissed_by
		)
		SELECT c.id, c.tenant_id, c.site_id, c.vuln_id, c.kind, c.slug, c.name,
		       c.installed_version, c.fixed_version, c.severity, c.cvss_score,
		       c.cve, c.title, c.status, c.first_seen, c.last_seen,
		       c.resolved_at, c.dismissed_at, c.dismissed_by,
		       s.name AS site_name, s.url AS site_url
		FROM claimed c
		JOIN sites s ON s.id = c.site_id`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("claim unnotified findings for tenant %s: %w", tenantID, err)
	}
	defer rows.Close()

	var out []ClaimedFinding
	for rows.Next() {
		var cf ClaimedFinding
		var (
			cvssScore   pgtype.Numeric
			resolvedAt  pgtype.Timestamptz
			dismissedAt pgtype.Timestamptz
			dismissedBy pgtype.UUID
			fixedVer    pgtype.Text
			cve         pgtype.Text
		)
		f := &cf.Finding
		if err := rows.Scan(
			&f.ID, &f.TenantID, &f.SiteID, &f.VulnID, &f.Kind, &f.Slug, &f.Name,
			&f.InstalledVersion, &fixedVer, &f.Severity, &cvssScore,
			&cve, &f.Title, &f.Status, &f.FirstSeen, &f.LastSeen,
			&resolvedAt, &dismissedAt, &dismissedBy,
			&cf.SiteName, &cf.SiteURL,
		); err != nil {
			return nil, fmt.Errorf("scan claimed finding: %w", err)
		}
		f.FixedVersion = fixedVer.String
		f.CVE = cve.String
		if cvssScore.Valid {
			fv, _ := cvssScore.Float64Value()
			if fv.Valid {
				v := fv.Float64
				f.CVSSScore = &v
			}
		}
		if resolvedAt.Valid {
			t := resolvedAt.Time
			f.ResolvedAt = &t
		}
		if dismissedAt.Valid {
			t := dismissedAt.Time
			f.DismissedAt = &t
		}
		if dismissedBy.Valid {
			uid := uuid.UUID(dismissedBy.Bytes)
			f.DismissedBy = &uid
		}
		out = append(out, cf)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Internal types used between repo and service
// ---------------------------------------------------------------------------

// FeedRecord is the parsed representation of one vulnerability from the
// Wordfence feed, ready for database ingestion.
type FeedRecord struct {
	VulnID        string
	Title         string
	CVE           string
	CVELink       string
	CVSSScore     *float64
	CVSSRating    string
	CWE           []byte // JSONB
	Informational bool
	References    []byte // JSONB
	Published     *time.Time
	Updated       *time.Time
	Raw           []byte // full record JSONB
	Software      []SoftwareRow
}

// SoftwareRow is one entry in the software[] array of a feed record.
type SoftwareRow struct {
	Kind             string // core|plugin|theme
	Slug             string
	AffectedVersions []byte // JSONB
	Patched          bool
	PatchedVersions  []byte // JSONB
}

// FeedMetaUpdate is the data written to the sentinel row after ingestion.
//
// EnrichmentOK/LastEnrichmentAt are nil-able: nil means "this run did not
// touch enrichment status" (a Scanner run), preserving whatever the last
// Production run wrote (see StampFeedMeta doc). A non-nil pointer means this
// run WAS a Production run and its outcome should replace the stored value.
type FeedMetaUpdate struct {
	FetchedAt      time.Time
	OK             bool
	RecordCount    int
	DefiantNotice  string
	DefiantLicense string
	MitreNotice    string
	LastError      string

	EnrichmentOK     *bool
	LastEnrichmentAt *time.Time
}

// VulnSoftwareRow is the projection returned by LookupSoftware: all the
// columns the matcher needs from the software + feed join.
type VulnSoftwareRow struct {
	VulnID           string
	Kind             string
	Slug             string
	AffectedVersions []byte
	Patched          bool
	PatchedVersions  []byte
	Title            string
	CVE              string
	CVELink          string
	CVSSScore        *float64
	CVSSRating       string
	References       []byte
}

// FindingUpsert is the data the service hands to UpsertFinding.
type FindingUpsert struct {
	TenantID         uuid.UUID
	SiteID           uuid.UUID
	VulnID           string
	Kind             string
	Slug             string
	Name             string
	InstalledVersion string
	FixedVersion     string
	Severity         string
	CVSSScore        *float64
	CVE              string
	Title            string
}

// FleetFindingRow is the row shape returned by FleetOpenFindings.
type FleetFindingRow struct {
	Finding  Finding
	SiteName string
	SiteURL  string
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// normSlug canonicalises a software slug so the wordfence_vuln_software
// storage key (ingest) and the lookup key (matching) always compare equal.
// Two normalisations are applied, in order:
//
//  1. Directory form. A WordPress plugin's installed-inventory slug is the
//     get_plugins() ARRAY KEY, which is the plugin's main FILE PATH relative
//     to wp-content/plugins, e.g. "woocommerce/woocommerce.php" for an
//     ordinary "directory" plugin (apps/agent/includes/commands/
//     class-metadata-command.php stores this raw, unmodified, as the "slug"
//     field of every plugin entry). Wordfence's own canonical slug for the
//     same plugin is the DIRECTORY component only, "woocommerce", so
//     normSlug takes everything before the first "/" (strings.Cut) and
//     discards the rest. Before this, LookupSoftware compared the untouched
//     "woocommerce/woocommerce.php" against the stored canonical
//     "woocommerce" and NEVER matched, so no plugin vulnerability ever
//     matched in production (themes/core were unaffected, see below).
//
//     KNOWN PRE-EXISTING LIMITATION, not solved here: a single-file plugin's
//     get_plugins() key has no "/" at all (e.g. "hello.php" for Hello
//     Dolly), so the directory cut is a no-op and the value is passed
//     through unchanged. If that plugin's Wordfence canonical slug is NOT
//     simply its file stem (Hello Dolly's canonical slug is "hello-dolly",
//     not "hello"), it still will not match. Closing that gap needs an
//     explicit slug-alias table, which is a separate, larger change.
//
//  2. Case. The result of step 1 is lower-cased so a mixed-case feed slug
//     (e.g. "Akismet") or inventory slug always compares equal regardless of
//     casing.
//
// Applied identically on BOTH the ingest path (UpsertFeedRecord,
// BulkUpsertFeedRecords/BulkReplaceAllSoftware, dedupSoftwareRows) and the
// lookup path (LookupSoftware) so the stored key and the query key are
// always the same value.
//
// Ingest-side no-op guarantee: the Wordfence feed already sends bare,
// directory-form canonical slugs with no "/" (e.g. "woocommerce",
// "akismet"); parseFeedRecord normalises them before they ever reach
// UpsertFeedRecord/BulkReplaceAllSoftware, so for every real feed record the
// directory cut has nothing to cut and this is exactly the old
// lower-case-only behaviour. Themes (the agent sends the theme's stylesheet
// DIRECTORY, already canonical) and core (hard-coded "wordpress") never
// contain a "/" either, so their normalised form is unchanged.
func normSlug(slug string) string {
	dir, _, _ := strings.Cut(slug, "/")
	return strings.ToLower(dir)
}

func nilString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func scanFinding(rows pgx.Rows, f *Finding) error {
	var (
		cvssScore   pgtype.Numeric
		resolvedAt  pgtype.Timestamptz
		dismissedAt pgtype.Timestamptz
		dismissedBy pgtype.UUID
		cveLink     pgtype.Text
		fixedVer    pgtype.Text
		cve         pgtype.Text
		refs        []byte
	)
	err := rows.Scan(
		&f.ID, &f.TenantID, &f.SiteID, &f.VulnID, &f.Kind, &f.Slug, &f.Name,
		&f.InstalledVersion, &fixedVer, &f.Severity, &cvssScore,
		&cve, &f.Title, &f.Status, &f.FirstSeen, &f.LastSeen,
		&resolvedAt, &dismissedAt, &dismissedBy,
		&cveLink, &refs,
	)
	if err != nil {
		return err
	}
	f.FixedVersion = fixedVer.String
	f.CVE = cve.String
	f.CVELink = cveLink.String
	f.References = refs
	if cvssScore.Valid {
		fv, _ := cvssScore.Float64Value()
		if fv.Valid {
			v := fv.Float64
			f.CVSSScore = &v
		}
	}
	if resolvedAt.Valid {
		t := resolvedAt.Time
		f.ResolvedAt = &t
	}
	if dismissedAt.Valid {
		t := dismissedAt.Time
		f.DismissedAt = &t
	}
	if dismissedBy.Valid {
		uid := uuid.UUID(dismissedBy.Bytes)
		f.DismissedBy = &uid
	}
	return nil
}

func scanFleetFindingRow(rows pgx.Rows, row *FleetFindingRow) error {
	var (
		cvssScore   pgtype.Numeric
		resolvedAt  pgtype.Timestamptz
		dismissedAt pgtype.Timestamptz
		dismissedBy pgtype.UUID
		cveLink     pgtype.Text
		fixedVer    pgtype.Text
		cve         pgtype.Text
		refs        []byte
	)
	f := &row.Finding
	err := rows.Scan(
		&f.ID, &f.TenantID, &f.SiteID, &f.VulnID, &f.Kind, &f.Slug, &f.Name,
		&f.InstalledVersion, &fixedVer, &f.Severity, &cvssScore,
		&cve, &f.Title, &f.Status, &f.FirstSeen, &f.LastSeen,
		&resolvedAt, &dismissedAt, &dismissedBy,
		&cveLink, &refs,
		&row.SiteName, &row.SiteURL,
	)
	if err != nil {
		return err
	}
	f.FixedVersion = fixedVer.String
	f.CVE = cve.String
	f.CVELink = cveLink.String
	f.References = refs
	if cvssScore.Valid {
		fv, _ := cvssScore.Float64Value()
		if fv.Valid {
			v := fv.Float64
			f.CVSSScore = &v
		}
	}
	if resolvedAt.Valid {
		t := resolvedAt.Time
		f.ResolvedAt = &t
	}
	if dismissedAt.Valid {
		t := dismissedAt.Time
		f.DismissedAt = &t
	}
	if dismissedBy.Valid {
		uid := uuid.UUID(dismissedBy.Bytes)
		f.DismissedBy = &uid
	}
	return nil
}

// marshalJSON marshals v to JSON bytes, returning nil on error (callers handle
// nil gracefully by defaulting to "[]" or "{}" in the DB column default).
func marshalJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// Ensure marshalJSON is used (suppress unused warning — called from worker).
var _ = marshalJSON
