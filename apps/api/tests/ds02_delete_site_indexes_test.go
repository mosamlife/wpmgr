package tests

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

const ds02IndexMigration = "20261013030000_m181_delete_per_parent_fk_indexes"

var ds02AddedIndexes = []struct{ table, column, name string }{
	{"media_optimization_jobs", "asset_id", "media_optimization_jobs_asset_id_idx"},
	{"rucss_jobs", "result_id", "rucss_jobs_result_id_idx"},
	{"backup_schedule_runs", "snapshot_id", "backup_schedule_runs_snapshot_id_idx"},
	{"generated_reports", "schedule_id", "generated_reports_schedule_id_idx"},
}

func TestDS02PerParentFKIndexDDL(t *testing.T) {
	schema, err := os.ReadFile("../db/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range ds02AddedIndexes {
		pattern := `(?m)^CREATE INDEX(?: IF NOT EXISTS)? ` + index.name + `\s+ON ` + index.table + `\s*\(` + index.column + `\)\s*;`
		if !regexp.MustCompile(pattern).Match(schema) {
			t.Errorf("schema lacks unconditional leading-key index %s(%s)", index.table, index.column)
		}
	}
	body, err := os.ReadFile("../migrations/" + ds02IndexMigration + ".sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range ds02AddedIndexes {
		pattern := `(?m)^CREATE INDEX IF NOT EXISTS ` + index.name + `\s+ON ` + index.table + `\s*\(` + index.column + `\)\s*;`
		if !regexp.MustCompile(pattern).Match(body) {
			t.Errorf("migration lacks idempotent index %s(%s)", index.table, index.column)
		}
	}
}

func TestDS02PerParentFKIndexUpgrade(t *testing.T) {
	ctx := context.Background()
	db := startPostgresAsOwner(t, func(version string) bool { return version < ds02IndexMigration })
	for _, index := range ds02AddedIndexes {
		if ds02HasLeadingIndex(t, db.owner, index.table, index.column) {
			t.Fatalf("before migration unexpectedly covers %s(%s)", index.table, index.column)
		}
	}
	// Seed existing rows through the application role before the startup runner
	// builds the indexes, so the upgrade covers populated tables.
	tenant := seedTenant(t, db.app, "ds02-index-upgrade")
	siteID := ds02SeedSite(t, ctx, db.app, tenant)
	seeds, err := ds02SeedsAtScale(0.0001)
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range seeds {
		if err := ds02SeedTable(ctx, db.app, tenant, siteID, seed); err != nil {
			t.Fatalf("seed %s: %v", seed.table, err)
		}
	}
	if err := db.owner.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, index := range ds02AddedIndexes {
		if !ds02HasLeadingIndex(t, db.owner, index.table, index.column) {
			t.Errorf("after migration lacks %s(%s)", index.table, index.column)
		}
	}
	for _, existing := range []struct{ table, column string }{
		{"backup_file_index", "snapshot_id"},
		{"backup_manifest_entries", "snapshot_id"},
		{"scan_run_hashes", "run_id"},
		{"media_variant_results", "job_id"},
		{"restore_run_events", "restore_run_id"},
		{"update_tasks", "run_id"},
	} {
		if !ds02HasLeadingIndex(t, db.owner, existing.table, existing.column) {
			t.Errorf("existing per-parent index missing: %s(%s)", existing.table, existing.column)
		}
	}
	// Re-running the DDL itself also exercises its IF NOT EXISTS guards.
	body, err := os.ReadFile("../migrations/" + ds02IndexMigration + ".sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, db.owner.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, string(body))
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func ds02HasLeadingIndex(t *testing.T, pool *db.Pool, table, column string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (
		SELECT 1 FROM pg_index i
		JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attname=$2
		JOIN pg_class c ON c.oid=i.indexrelid
		JOIN pg_am am ON am.oid=c.relam AND am.amname='btree'
		WHERE i.indrelid=$1::regclass AND i.indisvalid AND i.indisready
		AND i.indpred IS NULL AND i.indnkeyatts > 0 AND i.indkey[0]=a.attnum
	)`, "public."+strings.TrimSpace(table), column).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}
