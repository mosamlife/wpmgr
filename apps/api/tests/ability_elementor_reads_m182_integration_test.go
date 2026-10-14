package tests

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilities"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

const m182MigrationVersion = "20261013040000_m182_elementor_read_abilities"

var m182Names = []string{"wpmgr/elementor-widgets", "wpmgr/elementor-widget-schema"}

func m182Body(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(migrations.FS, m182MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m182 migration body: %v", err)
	}
	return string(b)
}

// Seed copy is part of the declarative mirror, which the schema catalog check
// does not compare. This proof needs no database.
func TestM182MigrationSeedsMatchSchema(t *testing.T) {
	body := m182Body(t)
	schema, err := os.ReadFile("../db/schema.sql")
	if err != nil {
		t.Fatalf("read schema mirror: %v", err)
	}
	seeds := strings.Split(body, `INSERT INTO "public"."ability_catalogue"`)
	if len(seeds) != len(m182Names)+1 {
		t.Fatalf("want one seed per ability, found %d", len(seeds)-1)
	}
	for _, seed := range seeds[1:] {
		end := strings.Index(seed, ";")
		if end < 0 {
			t.Fatal("catalogue seed has no statement terminator")
		}
		statement := `INSERT INTO "public"."ability_catalogue"` + seed[:end+1]
		if strings.Count(string(schema), statement) != 1 {
			t.Fatalf("schema mirror must carry this seed exactly once:\n%s", statement)
		}
	}
}

func m182Read(t *testing.T, pool *db.Pool, tenant uuid.UUID) map[string]sqlc.AbilityCatalogue {
	t.Helper()
	ctx := context.Background()
	entries := make(map[string]sqlc.AbilityCatalogue)
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m182 catalogue read)")
		rows, err := sqlc.New(tx).ListAbilityCatalogue(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Name != m182Names[0] && row.Name != m182Names[1] {
				continue
			}
			if _, exists := entries[row.Name]; exists {
				t.Fatalf("duplicate catalogue row for %s", row.Name)
			}
			entries[row.Name] = row
		}
		return nil
	}); err != nil {
		t.Fatalf("read m182 entries: %v", err)
	}
	if len(entries) != len(m182Names) {
		t.Fatalf("want both m182 entries, got %v", entries)
	}
	return entries
}

func m182AssertSeeds(t *testing.T, entries map[string]sqlc.AbilityCatalogue) {
	t.Helper()
	for _, name := range m182Names {
		r := entries[name]
		if r.Source != "wpmgr" || r.Class != "read" || r.Status != "admitted" || !r.Enabled ||
			r.ApprovalMode != "none" || r.Snapshot != "none" || r.EffectCopy != "none" ||
			r.PermissionMode != "principal" || r.ChangeClass != "always_ask" ||
			r.Preview != nil || r.OperatorPermission != nil || r.IntegrationID != nil || r.OwnerDir != nil ||
			r.SchemaStructSha256 != nil || r.EntrySha256 != nil || r.UpdatedByUserID.Valid {
			t.Fatalf("%s read seed: %+v", name, r)
		}
		if r.MinAgentVersion == nil || *r.MinAgentVersion != "0.61.166" {
			t.Fatalf("%s min_agent_version = %v, want 0.61.166", name, r.MinAgentVersion)
		}
		if r.Title == "" || !strings.Contains(r.Description, "Changes nothing.") || r.Usage == nil ||
			!strings.Contains(*r.Usage, "Atomic widgets are listed but cannot be used yet.") {
			t.Fatalf("%s plain copy is incomplete: %+v", name, r)
		}
	}
	widgets := entries[m182Names[0]]
	if widgets.Title != "List Elementor widgets" || !strings.Contains(*widgets.Usage, "name, title, categories, pro and atomic") {
		t.Fatalf("widget catalogue copy: %+v", widgets)
	}
	m169Limits(t, widgets.Limits, map[string]any{"builders_enabled": []any{"elementor"}})
	schema := entries[m182Names[1]]
	if schema.Title != "Read an Elementor widget's controls" {
		t.Fatalf("widget schema title: %q", schema.Title)
	}
	for _, phrase := range []string{"compact", "sections and control ids", "option keys", "default", "responsive", "group family", "activator", "repeater fields", "32 KiB"} {
		if !strings.Contains(*schema.Usage, phrase) {
			t.Fatalf("widget schema usage missing %q: %q", phrase, *schema.Usage)
		}
	}
	m169Limits(t, schema.Limits, map[string]any{"builders_enabled": []any{"elementor"}, "max_output_bytes": float64(32768)})
}

// Production migrates as wpmgr_owner; the app reads and stamps through its
// transaction helpers. To exercise the INSERT body as wpmgr_app too, the test
// grants INSERT with a policy restricted to these seed names, then removes it.
func TestM182SeedsStampAndRerunAsAppRole(t *testing.T) {
	body := m182Body(t)
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m182-seed-"+uuid.NewString()[:8])
	m182AssertSeeds(t, m182Read(t, pool, tenant))

	apply := func() error {
		return pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "InTenantTx (m182 migration body)")
			_, err := tx.Exec(ctx, body)
			return err
		})
	}
	if err := apply(); m182SQLState(err) != "42501" {
		t.Fatalf("without migration-time INSERT permission: got %v, want 42501", err)
	}

	owner := connectOwner(t, pool)
	defer owner.Close()
	revoke := func() {
		t.Helper()
		if _, err := owner.Exec(ctx, `REVOKE INSERT ON ability_catalogue FROM wpmgr_app;
			DROP POLICY IF EXISTS m182_test_seed_insert ON ability_catalogue`); err != nil {
			t.Fatalf("remove test seed permission: %v", err)
		}
	}
	defer revoke()
	if err := pgx.BeginFunc(ctx, owner.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM ability_catalogue WHERE name = ANY($1)`, m182Names); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `GRANT INSERT ON ability_catalogue TO wpmgr_app;
			CREATE POLICY m182_test_seed_insert ON ability_catalogue FOR INSERT TO wpmgr_app
			WITH CHECK (name IN ('wpmgr/elementor-widgets', 'wpmgr/elementor-widget-schema')
			            AND source = 'wpmgr' AND class = 'read')`)
		return err
	}); err != nil {
		t.Fatalf("arrange app-role migration seed: %v", err)
	}
	if err := apply(); err != nil {
		t.Fatalf("apply m182 as wpmgr_app: %v", err)
	}
	m182AssertSeeds(t, m182Read(t, pool, tenant))
	if _, err := abilities.StampOwnEntryHashes(ctx, pool, slog.Default()); err != nil {
		t.Fatalf("boot stamp: %v", err)
	}
	stamped := m182Read(t, pool, tenant)
	for _, r := range stamped {
		_, sum, err := abilities.EntryBytes(r)
		if err != nil || r.EntrySha256 == nil || *r.EntrySha256 != sum {
			t.Fatalf("%s boot stamp: hash=%v want=%s err=%v", r.Name, r.EntrySha256, sum, err)
		}
	}
	if err := apply(); err != nil {
		t.Fatalf("rerun m182 as wpmgr_app: %v", err)
	}
	if again := m182Read(t, pool, tenant); !reflect.DeepEqual(stamped, again) {
		t.Fatalf("rerun changed seeded rows:\nbefore %+v\nafter %+v", stamped, again)
	}
	revoke()
	if err := apply(); m182SQLState(err) != "42501" {
		t.Fatalf("after removing test permission: got %v, want 42501", err)
	}
}

func m182SQLState(err error) string {
	code, _ := m169Refusal(err)
	return code
}
