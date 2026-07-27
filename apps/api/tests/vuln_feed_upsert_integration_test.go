// Integration test for the vulnerability feed upsert (m79 + m81 fix) and the
// bulk ingest path (timeout fix).
//
// TestUpsertFeedRecord_RoundTrip reproduces the original prod failure: UpsertFeedRecord
// failing with SQLSTATE 42601 ("syntax error at or near 'references'") because the
// column was named after a PostgreSQL reserved keyword.
//
// TestBulkIngest_Scale proves the bulk ingest path (BulkUpsertFeedRecords +
// BulkReplaceAllSoftware) that replaced the per-record loop scales to thousands
// of records without timing out and upserts idempotently on a second run.
//
// TestBulkIngest_DuplicateSoftware reproduces the prod blocker: when a single
// feed record's software[] array contains two entries with the same (kind, slug),
// the BulkReplaceAllSoftware CopyFrom path was throwing SQLSTATE 23505
// (duplicate key violates unique constraint "wordfence_vuln_software_pkey").
// The fix: dedupSoftwareRows merges the duplicate entries before CopyFrom, unioning
// affected_versions, OR-ing patched, and unioning patched_versions.
package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/vuln"
)

func TestUpsertFeedRecord_RoundTrip(t *testing.T) {
	pool := startPostgres(t) // skips if Docker unavailable
	ctx := context.Background()

	score := 6.5
	pub := time.Date(1998, 1, 9, 0, 0, 0, 0, time.UTC)
	upd := time.Date(2022, 8, 5, 20, 14, 5, 0, time.UTC)

	affectedVersions, _ := json.Marshal(map[string]any{
		"1.0.0 - 1.2.3": map[string]any{
			"from_version":  "1.0.0",
			"from_inclusive": true,
			"to_version":    "1.2.3",
			"to_inclusive":  true,
		},
	})
	patchedVersions, _ := json.Marshal([]string{"1.2.4"})
	refURLs, _ := json.Marshal([]string{"https://www.wordfence.com/threat-intel/vulnerabilities/example"})
	cwe, _ := json.Marshal(map[string]any{"id": 80, "name": "Basic XSS"})
	raw, _ := json.Marshal(map[string]any{"_test": true})

	rec := vuln.FeedRecord{
		VulnID:        "848ccbdc-c6f1-480f-a272-cd459e706713",
		Title:         "Example Plugin <= 1.2.3 - Stored XSS",
		CVE:           "CVE-1998-1000",
		CVELink:       "https://www.cve.org/CVERecord?id=CVE-1998-1000",
		CVSSScore:     &score,
		CVSSRating:    "Medium",
		CWE:           cwe,
		Informational: false,
		References:    refURLs, // stored in reference_urls column after m81 rename
		Published:     &pub,
		Updated:       &upd,
		Raw:           raw,
		Software: []vuln.SoftwareRow{
			{
				Kind:             "plugin",
				Slug:             "example",
				AffectedVersions: affectedVersions,
				Patched:          true,
				PatchedVersions:  patchedVersions,
			},
		},
	}

	repo := vuln.NewRepo(pool)

	// Step 1: INSERT path — this is the statement that was failing with SQLSTATE
	// 42601 ("syntax error at or near 'references'") before the m81 rename.
	t.Run("insert_succeeds", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SELECT set_config('app.agent','on',true)"); err != nil {
			t.Fatalf("set agent guc: %v", err)
		}
		if err := repo.UpsertFeedRecord(ctx, tx, rec); err != nil {
			t.Fatalf("UpsertFeedRecord (INSERT path) failed: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	})

	// Step 2: Verify the row persisted and reference_urls survived.
	t.Run("row_persisted_with_reference_urls", func(t *testing.T) {
		var title string
		var refURLsOut []byte
		err := pool.QueryRow(ctx,
			`SELECT title, reference_urls FROM wordfence_vuln_feed WHERE vuln_id = $1`,
			rec.VulnID,
		).Scan(&title, &refURLsOut)
		if err != nil {
			t.Fatalf("SELECT after upsert: %v", err)
		}
		if title != rec.Title {
			t.Errorf("title = %q; want %q", title, rec.Title)
		}
		var urls []string
		if err := json.Unmarshal(refURLsOut, &urls); err != nil {
			t.Fatalf("unmarshal reference_urls: %v", err)
		}
		if len(urls) != 1 || urls[0] != "https://www.wordfence.com/threat-intel/vulnerabilities/example" {
			t.Errorf("reference_urls = %v; want [https://www.wordfence.com/...]", urls)
		}
	})

	// Step 3: ON CONFLICT DO UPDATE path — the second query site of the original
	// error was in the ON CONFLICT SET clause which also referenced the
	// (unquoted) reserved keyword.
	t.Run("on_conflict_update_succeeds", func(t *testing.T) {
		updated := rec
		updated.Title = "Example Plugin <= 1.2.3 - Stored XSS (updated)"

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SELECT set_config('app.agent','on',true)"); err != nil {
			t.Fatalf("set agent guc: %v", err)
		}
		if err := repo.UpsertFeedRecord(ctx, tx, updated); err != nil {
			t.Fatalf("UpsertFeedRecord (ON CONFLICT path) failed: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		// Verify the title was updated.
		var gotTitle string
		if err := pool.QueryRow(ctx,
			`SELECT title FROM wordfence_vuln_feed WHERE vuln_id = $1`, rec.VulnID,
		).Scan(&gotTitle); err != nil {
			t.Fatalf("SELECT after ON CONFLICT update: %v", err)
		}
		if gotTitle != updated.Title {
			t.Errorf("title after update = %q; want %q", gotTitle, updated.Title)
		}
	})

	// Step 4: LookupSoftware — exercises the JOIN SELECT that uses reference_urls.
	t.Run("lookup_software_returns_reference_urls", func(t *testing.T) {
		rows, err := repo.LookupSoftware(ctx, "plugin", "example")
		if err != nil {
			t.Fatalf("LookupSoftware: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("LookupSoftware returned %d rows; want 1", len(rows))
		}
		r := rows[0]
		if r.VulnID != rec.VulnID {
			t.Errorf("VulnID = %q; want %q", r.VulnID, rec.VulnID)
		}
		if r.CVE != rec.CVE {
			t.Errorf("CVE = %q; want %q", r.CVE, rec.CVE)
		}
		var urls []string
		if err := json.Unmarshal(r.References, &urls); err != nil {
			t.Fatalf("unmarshal References from LookupSoftware: %v", err)
		}
		if len(urls) != 1 {
			t.Errorf("References len = %d; want 1", len(urls))
		}
	})
}

// TestUpsertFeedRecord_EmptyNormalizedSlugSkipped proves the single-record
// ingest path (UpsertFeedRecord) carries the same empty-normalised-slug guard
// BulkReplaceAllSoftware already had: a software row whose slug normalises to
// the empty string (here "/", which strings.Cut splits into an empty
// directory component before the first "/") must be silently skipped rather
// than stored, so it can never later be matched by an equally-empty
// normalised lookup key.
func TestUpsertFeedRecord_EmptyNormalizedSlugSkipped(t *testing.T) {
	pool := startPostgres(t) // skips if Docker unavailable
	ctx := context.Background()
	repo := vuln.NewRepo(pool)

	affectedVersions, _ := json.Marshal(map[string]any{})
	patchedVersions, _ := json.Marshal([]string{})
	refURLs, _ := json.Marshal([]string{"https://example.com/vuln"})
	raw, _ := json.Marshal(map[string]any{"_test": true})

	rec := vuln.FeedRecord{
		VulnID:     "empty-slug-test-0000-0000-000000000001",
		Title:      "Empty slug guard test",
		References: refURLs,
		Raw:        raw,
		Software: []vuln.SoftwareRow{
			// A slug that normalises to "" and must be skipped.
			{Kind: "plugin", Slug: "/", AffectedVersions: affectedVersions, Patched: false, PatchedVersions: patchedVersions},
			// A normal slug in the same record, which must still land.
			{Kind: "plugin", Slug: "real-plugin", AffectedVersions: affectedVersions, Patched: false, PatchedVersions: patchedVersions},
		},
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.agent','on',true)"); err != nil {
		t.Fatalf("set agent guc: %v", err)
	}
	if err := repo.UpsertFeedRecord(ctx, tx, rec); err != nil {
		t.Fatalf("UpsertFeedRecord: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	t.Run("empty_slug_row_not_stored", func(t *testing.T) {
		var count int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM wordfence_vuln_software WHERE vuln_id = $1 AND slug = ''`,
			rec.VulnID,
		).Scan(&count); err != nil {
			t.Fatalf("count query: %v", err)
		}
		if count != 0 {
			t.Errorf("wordfence_vuln_software rows with empty slug = %d; want 0 (must be skipped, not stored)", count)
		}
	})

	t.Run("normal_slug_row_still_stored", func(t *testing.T) {
		var count int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM wordfence_vuln_software WHERE vuln_id = $1 AND slug = 'real-plugin'`,
			rec.VulnID,
		).Scan(&count); err != nil {
			t.Fatalf("count query: %v", err)
		}
		if count != 1 {
			t.Errorf("wordfence_vuln_software rows for real-plugin = %d; want 1", count)
		}
	})
}

// TestBulkIngest_Scale exercises the bulk ingest path that fixed the prod timeout.
//
// The per-record loop (UpsertFeedRecord called 13k times in one tx) blew the
// River job context deadline. BulkUpsertFeedRecords + BulkReplaceAllSoftware
// replaced it with a single pgx Batch round-trip + one DELETE + one CopyFrom.
//
// This test:
//   (a) generates 2,000 synthetic records with 2 software rows each (4,000 total),
//   (b) runs the full bulk path (the same operations ingestRecords calls),
//   (c) asserts row counts in both tables,
//   (d) asserts timing is well under 10 seconds,
//   (e) runs a second ingest with an overlapping set (1,800 of the original +
//       200 new) and asserts the ON CONFLICT upsert is idempotent (no dup-key),
//       the 200 new records landed, and the 200 removed ones were pruned by
//       PruneMissingVulns.
func TestBulkIngest_Scale(t *testing.T) {
	pool := startPostgres(t) // skips if Docker unavailable
	ctx := context.Background()
	repo := vuln.NewRepo(pool)

	const N = 2000 // number of synthetic records per run
	const softwarePerRec = 2

	// buildRecords generates n synthetic feed records. Each has two software rows
	// (one plugin, one theme) so we exercise the CopyFrom path non-trivially.
	buildRecords := func(n int, titlePrefix string) []vuln.FeedRecord {
		avJSON, _ := json.Marshal(map[string]any{
			"* - 1.0.0": map[string]any{
				"from_version": "*", "from_inclusive": true,
				"to_version": "1.0.0", "to_inclusive": true,
			},
		})
		pvJSON, _ := json.Marshal([]string{"1.0.1"})
		refJSON, _ := json.Marshal([]string{"https://example.com/vuln"})
		rawJSON, _ := json.Marshal(map[string]any{"_test": true})
		score := 7.5

		recs := make([]vuln.FeedRecord, n)
		for i := 0; i < n; i++ {
			vulnID := fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
			recs[i] = vuln.FeedRecord{
				VulnID:        vulnID,
				Title:         fmt.Sprintf("%s record %d", titlePrefix, i),
				CVE:           fmt.Sprintf("CVE-2026-%04d", i),
				CVSSScore:     &score,
				CVSSRating:    "High",
				Informational: false,
				References:    refJSON,
				Raw:           rawJSON,
				Software: []vuln.SoftwareRow{
					{
						Kind:             "plugin",
						Slug:             fmt.Sprintf("plugin-%d", i),
						AffectedVersions: avJSON,
						Patched:          true,
						PatchedVersions:  pvJSON,
					},
					{
						Kind:             "theme",
						Slug:             fmt.Sprintf("theme-%d", i),
						AffectedVersions: avJSON,
						Patched:          false,
						PatchedVersions:  pvJSON,
					},
				},
			}
		}
		return recs
	}

	// bulkIngest runs the same three operations that ingestRecords uses.
	bulkIngest := func(t *testing.T, recs []vuln.FeedRecord) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SELECT set_config('app.agent','on',true)"); err != nil {
			t.Fatalf("set agent guc: %v", err)
		}
		if err := repo.BulkUpsertFeedRecords(ctx, tx, recs); err != nil {
			t.Fatalf("BulkUpsertFeedRecords: %v", err)
		}
		if err := repo.BulkReplaceAllSoftware(ctx, tx, recs); err != nil {
			t.Fatalf("BulkReplaceAllSoftware: %v", err)
		}
		knownIDs := make([]string, len(recs))
		for i, r := range recs {
			knownIDs[i] = r.VulnID
		}
		if err := repo.PruneMissingVulns(ctx, tx, knownIDs); err != nil {
			t.Fatalf("PruneMissingVulns: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	countRows := func(t *testing.T, table string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}

	// --- First ingest: 2,000 records ---
	t.Run("first_ingest_2000_records", func(t *testing.T) {
		recs := buildRecords(N, "first")
		start := time.Now()
		bulkIngest(t, recs)
		elapsed := time.Since(start)

		feedCount := countRows(t, "wordfence_vuln_feed")
		// The round-trip test may have left one extra row; allow for it.
		if feedCount < N {
			t.Errorf("wordfence_vuln_feed: got %d rows, want >= %d", feedCount, N)
		}

		swCount := countRows(t, "wordfence_vuln_software")
		if swCount < N*softwarePerRec {
			t.Errorf("wordfence_vuln_software: got %d rows, want >= %d", swCount, N*softwarePerRec)
		}

		// The bulk path should comfortably finish in under 10 seconds even on a
		// container with limited I/O. The per-record path took 13k+ round-trips;
		// the bulk path is 3 SQL operations.
		if elapsed > 10*time.Second {
			t.Errorf("bulk ingest of %d records took %v; want < 10s (bulk path should be fast)", N, elapsed)
		}
		t.Logf("first ingest: %d feed rows, %d software rows in %v", feedCount, swCount, elapsed)
	})

	// --- Second ingest: 1,800 overlapping + 200 new; 200 original removed ---
	t.Run("second_ingest_overlap_and_prune", func(t *testing.T) {
		const keep = 1800
		const newCount = 200
		// Overlap: first 1,800 records from the first batch (updated titles).
		overlap := buildRecords(keep, "second-update")
		// New: 200 brand-new records with IDs in the range [N, N+newCount).
		newRecs := make([]vuln.FeedRecord, newCount)
		avJSON, _ := json.Marshal(map[string]any{})
		pvJSON, _ := json.Marshal([]string{})
		refJSON, _ := json.Marshal([]string{"https://example.com/new"})
		rawJSON, _ := json.Marshal(map[string]any{"_test": true})
		score := 5.0
		for i := 0; i < newCount; i++ {
			idx := N + i
			vulnID := fmt.Sprintf("00000000-0000-0000-0000-%012d", idx)
			newRecs[i] = vuln.FeedRecord{
				VulnID:        vulnID,
				Title:         fmt.Sprintf("new record %d", idx),
				CVSSScore:     &score,
				CVSSRating:    "Medium",
				References:    refJSON,
				Raw:           rawJSON,
				Software: []vuln.SoftwareRow{
					{
						Kind: "plugin", Slug: fmt.Sprintf("new-plugin-%d", idx),
						AffectedVersions: avJSON, Patched: true, PatchedVersions: pvJSON,
					},
				},
			}
		}
		recs := append(overlap, newRecs...)

		bulkIngest(t, recs)

		feedCount := countRows(t, "wordfence_vuln_feed")
		wantFeed := keep + newCount
		if feedCount != wantFeed {
			t.Errorf("after second ingest: wordfence_vuln_feed = %d rows, want %d (keep=%d new=%d)",
				feedCount, wantFeed, keep, newCount)
		}

		// Verify updated title for one of the overlap records.
		var gotTitle string
		if err := pool.QueryRow(ctx,
			`SELECT title FROM wordfence_vuln_feed WHERE vuln_id = $1`,
			fmt.Sprintf("00000000-0000-0000-0000-%012d", 0),
		).Scan(&gotTitle); err != nil {
			t.Fatalf("SELECT overlap title: %v", err)
		}
		if gotTitle != "second-update record 0" {
			t.Errorf("overlap title = %q; want %q", gotTitle, "second-update record 0")
		}

		// Verify one of the new records landed.
		var newTitle string
		if err := pool.QueryRow(ctx,
			`SELECT title FROM wordfence_vuln_feed WHERE vuln_id = $1`,
			fmt.Sprintf("00000000-0000-0000-0000-%012d", N),
		).Scan(&newTitle); err != nil {
			t.Fatalf("SELECT new record title: %v", err)
		}
		if newTitle != fmt.Sprintf("new record %d", N) {
			t.Errorf("new record title = %q; want %q", newTitle, fmt.Sprintf("new record %d", N))
		}

		// Verify one of the pruned records is gone (IDs [keep..N-1]).
		var prunedCount int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM wordfence_vuln_feed WHERE vuln_id = $1`,
			fmt.Sprintf("00000000-0000-0000-0000-%012d", keep),
		).Scan(&prunedCount); err != nil {
			t.Fatalf("SELECT pruned check: %v", err)
		}
		if prunedCount != 0 {
			t.Errorf("pruned record still present after second ingest")
		}

		t.Logf("second ingest: %d feed rows (keep=%d new=%d pruned=%d)",
			feedCount, keep, newCount, N-keep)
	})
}

// TestBulkIngest_DuplicateSoftware reproduces the prod blocker
// (SQLSTATE 23505 on CopyFrom) and verifies the fix: when a single feed
// record's software[] array contains two entries with the same (kind, slug),
// the dedup+merge path produces exactly one row in wordfence_vuln_software
// with the union of both entries' affected_versions and patched_versions.
//
// Additionally, two DIFFERENT vulns sharing the same (kind, slug) must each
// produce their own row (different vuln_id) and must NOT be collapsed.
func TestBulkIngest_DuplicateSoftware(t *testing.T) {
	pool := startPostgres(t) // skips if Docker unavailable
	ctx := context.Background()
	repo := vuln.NewRepo(pool)

	// --- Case A: one record whose software[] has duplicate (kind, slug) ---
	//
	// Two software entries for (plugin, "akismet") with different affected_versions:
	//   range1: "1.0.0 - 1.2.3"
	//   range2: "1.5.0 - 1.6.0"
	// Only range2 is patched; range2 also has patched_versions ["1.6.1"].
	// After ingest we expect:
	//   (a) NO error (no 23505)
	//   (b) exactly ONE row for (vuln-dup-A, plugin, akismet)
	//   (c) affected_versions contains BOTH range keys (no data lost)
	//   (d) patched = true  (OR of false, true)
	//   (e) patched_versions = ["1.6.1"] (union)

	av1, _ := json.Marshal(map[string]any{
		"1.0.0 - 1.2.3": map[string]any{
			"from_version": "1.0.0", "from_inclusive": true,
			"to_version": "1.2.3", "to_inclusive": true,
		},
	})
	av2, _ := json.Marshal(map[string]any{
		"1.5.0 - 1.6.0": map[string]any{
			"from_version": "1.5.0", "from_inclusive": true,
			"to_version": "1.6.0", "to_inclusive": true,
		},
	})
	pv2, _ := json.Marshal([]string{"1.6.1"})
	pvEmpty, _ := json.Marshal([]string{})
	refJSON, _ := json.Marshal([]string{"https://example.com/vuln"})
	rawJSON, _ := json.Marshal(map[string]any{"_test": true})
	score := 6.5

	dupRec := vuln.FeedRecord{
		VulnID:        "dup-test-0000-0000-0000-000000000001",
		Title:         "Akismet dup-software test",
		CVE:           "CVE-2026-9001",
		CVSSScore:     &score,
		CVSSRating:    "Medium",
		Informational: false,
		References:    refJSON,
		Raw:           rawJSON,
		Software: []vuln.SoftwareRow{
			// First entry: range1, not patched.
			{Kind: "plugin", Slug: "akismet", AffectedVersions: av1, Patched: false, PatchedVersions: pvEmpty},
			// Duplicate entry: same (kind, slug), different range, patched.
			{Kind: "plugin", Slug: "akismet", AffectedVersions: av2, Patched: true, PatchedVersions: pv2},
		},
	}

	// --- Case B: two DIFFERENT vulns sharing the same (kind, slug) ---
	//
	// Both affect "akismet" plugin but have different vuln_ids. The PK includes
	// vuln_id, so both rows must survive — the dedup must NOT collapse across
	// different vulns.
	avB, _ := json.Marshal(map[string]any{
		"* - 2.0.0": map[string]any{"from_version": "*", "from_inclusive": true, "to_version": "2.0.0", "to_inclusive": false},
	})
	pvB, _ := json.Marshal([]string{"2.0.1"})
	scoreB := 8.0

	recB1 := vuln.FeedRecord{
		VulnID:        "dup-test-0000-0000-0000-000000000002",
		Title:         "Akismet cross-vuln test 1",
		CVSSScore:     &scoreB,
		CVSSRating:    "High",
		References:    refJSON,
		Raw:           rawJSON,
		Software: []vuln.SoftwareRow{
			{Kind: "plugin", Slug: "akismet", AffectedVersions: avB, Patched: true, PatchedVersions: pvB},
		},
	}
	recB2 := vuln.FeedRecord{
		VulnID:        "dup-test-0000-0000-0000-000000000003",
		Title:         "Akismet cross-vuln test 2",
		CVSSScore:     &scoreB,
		CVSSRating:    "High",
		References:    refJSON,
		Raw:           rawJSON,
		Software: []vuln.SoftwareRow{
			{Kind: "plugin", Slug: "akismet", AffectedVersions: avB, Patched: true, PatchedVersions: pvB},
		},
	}

	allRecs := []vuln.FeedRecord{dupRec, recB1, recB2}

	// Run the bulk ingest (the same operations ingestRecords uses).
	t.Run("no_duplicate_key_error", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SELECT set_config('app.agent','on',true)"); err != nil {
			t.Fatalf("set agent guc: %v", err)
		}
		if err := repo.BulkUpsertFeedRecords(ctx, tx, allRecs); err != nil {
			t.Fatalf("BulkUpsertFeedRecords: %v", err)
		}
		// This is the call that previously threw SQLSTATE 23505.
		if err := repo.BulkReplaceAllSoftware(ctx, tx, allRecs); err != nil {
			t.Fatalf("BulkReplaceAllSoftware (duplicate software): %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	})

	// (b) Exactly one row for Case A's duplicate (vuln_id, kind, slug).
	t.Run("exactly_one_software_row_for_dup", func(t *testing.T) {
		var count int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM wordfence_vuln_software WHERE vuln_id = $1 AND kind = 'plugin' AND slug = 'akismet'`,
			dupRec.VulnID,
		).Scan(&count); err != nil {
			t.Fatalf("count query: %v", err)
		}
		if count != 1 {
			t.Errorf("wordfence_vuln_software rows for dup record = %d; want 1 (dedup must collapse to one)", count)
		}
	})

	// (c) Merged affected_versions contains BOTH range keys — no data lost.
	t.Run("merged_affected_versions_has_both_ranges", func(t *testing.T) {
		var avOut []byte
		if err := pool.QueryRow(ctx,
			`SELECT affected_versions FROM wordfence_vuln_software WHERE vuln_id = $1 AND kind = 'plugin' AND slug = 'akismet'`,
			dupRec.VulnID,
		).Scan(&avOut); err != nil {
			t.Fatalf("SELECT affected_versions: %v", err)
		}
		var avMap map[string]json.RawMessage
		if err := json.Unmarshal(avOut, &avMap); err != nil {
			t.Fatalf("unmarshal affected_versions: %v (raw: %s)", err, avOut)
		}
		if _, ok := avMap["1.0.0 - 1.2.3"]; !ok {
			t.Errorf("affected_versions missing range key %q; full map: %v", "1.0.0 - 1.2.3", avMap)
		}
		if _, ok := avMap["1.5.0 - 1.6.0"]; !ok {
			t.Errorf("affected_versions missing range key %q; full map: %v", "1.5.0 - 1.6.0", avMap)
		}
	})

	// (d) patched = true (OR of false and true).
	t.Run("merged_patched_is_true", func(t *testing.T) {
		var patched bool
		if err := pool.QueryRow(ctx,
			`SELECT patched FROM wordfence_vuln_software WHERE vuln_id = $1 AND kind = 'plugin' AND slug = 'akismet'`,
			dupRec.VulnID,
		).Scan(&patched); err != nil {
			t.Fatalf("SELECT patched: %v", err)
		}
		if !patched {
			t.Error("patched = false; want true (OR of false and true from the two dup entries)")
		}
	})

	// (e) patched_versions contains the merged version string.
	t.Run("merged_patched_versions_union", func(t *testing.T) {
		var pvOut []byte
		if err := pool.QueryRow(ctx,
			`SELECT patched_versions FROM wordfence_vuln_software WHERE vuln_id = $1 AND kind = 'plugin' AND slug = 'akismet'`,
			dupRec.VulnID,
		).Scan(&pvOut); err != nil {
			t.Fatalf("SELECT patched_versions: %v", err)
		}
		var pvSlice []string
		if err := json.Unmarshal(pvOut, &pvSlice); err != nil {
			t.Fatalf("unmarshal patched_versions: %v (raw: %s)", err, pvOut)
		}
		found := false
		for _, v := range pvSlice {
			if v == "1.6.1" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("patched_versions = %v; want to contain %q", pvSlice, "1.6.1")
		}
	})

	// Case B: two different vulns sharing (kind=plugin, slug=akismet) must
	// produce TWO rows (one per vuln_id) — the dedup must NOT collapse across vulns.
	t.Run("two_different_vulns_same_slug_produce_two_rows", func(t *testing.T) {
		var count int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM wordfence_vuln_software WHERE vuln_id IN ($1, $2) AND kind = 'plugin' AND slug = 'akismet'`,
			recB1.VulnID, recB2.VulnID,
		).Scan(&count); err != nil {
			t.Fatalf("count query: %v", err)
		}
		if count != 2 {
			t.Errorf("software rows for two distinct vulns = %d; want 2 (dedup must NOT collapse across vuln_ids)", count)
		}
	})

	// Idempotency: running the same ingest a second time must succeed (no errors,
	// same row counts — the DELETE+CopyFrom pattern is inherently idempotent).
	t.Run("second_ingest_is_idempotent", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SELECT set_config('app.agent','on',true)"); err != nil {
			t.Fatalf("set agent guc: %v", err)
		}
		if err := repo.BulkUpsertFeedRecords(ctx, tx, allRecs); err != nil {
			t.Fatalf("second BulkUpsertFeedRecords: %v", err)
		}
		if err := repo.BulkReplaceAllSoftware(ctx, tx, allRecs); err != nil {
			t.Fatalf("second BulkReplaceAllSoftware: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("second commit: %v", err)
		}

		// Row counts must be identical after the second ingest.
		var count int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM wordfence_vuln_software WHERE vuln_id = $1 AND kind = 'plugin' AND slug = 'akismet'`,
			dupRec.VulnID,
		).Scan(&count); err != nil {
			t.Fatalf("count query after second ingest: %v", err)
		}
		if count != 1 {
			t.Errorf("after second ingest: rows = %d; want 1", count)
		}
	})
}
