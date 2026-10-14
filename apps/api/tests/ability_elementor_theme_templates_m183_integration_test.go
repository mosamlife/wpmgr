package tests

import (
	"context"
	"encoding/json"
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

const m183MigrationVersion = "20261014190000_m183_elementor_theme_templates_read"
const m183Name = "wpmgr/elementor-theme-templates"
const m183Title = "List Elementor headers and footers"
const m183Description = "Lists the site's Elementor Pro header and footer templates with their type, status, whether WPMgr made each one and where each shows, plus whether Elementor Pro's Theme Builder is ready, the places a template can be set to show, and the site's menus. Changes nothing."
const m183Usage = "Read this before making or changing a header or footer. pro.theme_builder must be true before a header or footer can be made; pro.problem says why it is not. Each template lists shows_on, the display rules Elementor Pro stored, such as include/general for the entire site or exclude/singular/page/42 for everywhere except page 42. shows_on is null when the rules cannot be read, and shows_on_problem says why. places lists the rules this site accepts in this version; takes_id means the rule may end with a page or post id. made_by_wpmgr marks templates WPMgr made; only those, while they are still drafts, can be changed. Lists stop at 25 entries; templates_total and menus_total count everything. Each menu has its id, slug, name and item count."

func m183Body(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(migrations.FS, m183MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m183 migration body: %v", err)
	}
	return string(b)
}

// Seed copy is part of the declarative mirror, which the schema catalog check
// does not compare. This proof needs no database.
func TestM183MigrationSeedMatchesSchema(t *testing.T) {
	body := m183Body(t)
	schema, err := os.ReadFile("../db/schema.sql")
	if err != nil {
		t.Fatalf("read schema mirror: %v", err)
	}
	seeds := strings.Split(body, `INSERT INTO "public"."ability_catalogue"`)
	if len(seeds) != 2 {
		t.Fatalf("want one seed, found %d", len(seeds)-1)
	}
	end := strings.LastIndex(seeds[1], ";")
	if end < 0 {
		t.Fatal("catalogue seed has no statement terminator")
	}
	statement := `INSERT INTO "public"."ability_catalogue"` + seeds[1][:end+1]
	if strings.Count(string(schema), statement) != 1 {
		t.Fatalf("schema mirror must carry this seed exactly once:\n%s", statement)
	}
}

func m183Read(t *testing.T, pool *db.Pool, tenant uuid.UUID) sqlc.AbilityCatalogue {
	t.Helper()
	ctx := context.Background()
	var entry sqlc.AbilityCatalogue
	found := false
	if err := pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m183 catalogue read)")
		rows, err := sqlc.New(tx).ListAbilityCatalogue(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Name != m183Name {
				continue
			}
			if found {
				t.Fatalf("duplicate catalogue row for %s", m183Name)
			}
			entry = row
			found = true
		}
		return nil
	}); err != nil {
		t.Fatalf("read m183 entry: %v", err)
	}
	if !found {
		t.Fatalf("missing catalogue row for %s", m183Name)
	}
	return entry
}

func m183AssertSeed(t *testing.T, r sqlc.AbilityCatalogue) {
	t.Helper()
	if r.Name != m183Name || r.Source != "wpmgr" || r.Class != "read" || r.Status != "admitted" || !r.Enabled ||
		r.ApprovalMode != "none" || r.Snapshot != "none" || r.EffectCopy != "none" ||
		r.PermissionMode != "principal" || r.ChangeClass != "always_ask" ||
		r.Preview != nil || r.OperatorPermission != nil || r.IntegrationID != nil || r.OwnerDir != nil ||
		r.SchemaStructSha256 != nil || r.EntrySha256 != nil || r.UpdatedByUserID.Valid {
		t.Fatalf("theme templates read seed: %+v", r)
	}
	if r.MinAgentVersion == nil || *r.MinAgentVersion != "0.61.167" {
		t.Fatalf("min_agent_version = %v, want 0.61.167", r.MinAgentVersion)
	}
	if r.Title != m183Title || r.Description != m183Description || r.Usage == nil || *r.Usage != m183Usage {
		t.Fatalf("theme templates copy does not match the read contract: %+v", r)
	}
	m169Limits(t, r.Limits, map[string]any{"builders_enabled": []any{"elementor"}, "max_output_bytes": float64(32768)})
}

// Production migrates as wpmgr_owner; the app reads and stamps through its
// transaction helpers. The test grants INSERT only for this seed, then removes
// it, to exercise the migration body as wpmgr_app as well.
func TestM183SeedStampAndRerunKeepsOperatorSettingsAsAppRole(t *testing.T) {
	body := m183Body(t)
	ctx := context.Background()
	pool := startPostgres(t)
	tenant := seedTenant(t, pool, "m183-seed-"+uuid.NewString()[:8])
	m183AssertSeed(t, m183Read(t, pool, tenant))

	apply := func() error {
		return pool.InTenantTx(ctx, tenant, func(tx pgx.Tx) error {
			mcpAssertAndReportRole(t, tx, "InTenantTx (m183 migration body)")
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
			DROP POLICY IF EXISTS m183_test_seed_insert ON ability_catalogue`); err != nil {
			t.Fatalf("remove test seed permission: %v", err)
		}
	}
	defer revoke()
	if err := pgx.BeginFunc(ctx, owner.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM ability_catalogue WHERE name = $1`, m183Name); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `GRANT INSERT ON ability_catalogue TO wpmgr_app;
			CREATE POLICY m183_test_seed_insert ON ability_catalogue FOR INSERT TO wpmgr_app
			WITH CHECK (name = 'wpmgr/elementor-theme-templates'
			            AND source = 'wpmgr' AND class = 'read')`)
		return err
	}); err != nil {
		t.Fatalf("arrange app-role migration seed: %v", err)
	}
	if err := apply(); err != nil {
		t.Fatalf("apply m183 as wpmgr_app: %v", err)
	}
	m183AssertSeed(t, m183Read(t, pool, tenant))
	if _, err := abilities.StampOwnEntryHashes(ctx, pool, slog.Default()); err != nil {
		t.Fatalf("boot stamp: %v", err)
	}
	stamped := m183Read(t, pool, tenant)
	_, sum, err := abilities.EntryBytes(stamped)
	if err != nil || stamped.EntrySha256 == nil || *stamped.EntrySha256 != sum {
		t.Fatalf("boot stamp: hash=%v want=%s err=%v", stamped.EntrySha256, sum, err)
	}
	if err := apply(); err != nil {
		t.Fatalf("rerun m183 as wpmgr_app: %v", err)
	}
	if again := m183Read(t, pool, tenant); !reflect.DeepEqual(stamped, again) {
		t.Fatalf("rerun changed seeded row:\nbefore %+v\nafter %+v", stamped, again)
	}

	admin := connectAdmin(t, pool)
	actor := seedUserRow(t, admin, "m183-operator-"+uuid.NewString()[:8]+"@example.test")
	markSuperadmin(t, admin, actor)
	disabled := false
	status := "detect_only"
	title := "Our header and footer inventory"
	description := "Our template inventory wording."
	usage := "Read our inventory first."
	limits := json.RawMessage(`{"builders_enabled":["elementor"],"max_output_bytes":16384}`)
	edited, err := abilities.NewAdminRepo(pool).Upsert(ctx, actor, &stamped.EntryID, abilities.CatalogueInput{
		Enabled: &disabled, Status: &status, Title: &title, Description: &description, Usage: &usage, Limits: &limits,
	})
	if err != nil {
		t.Fatalf("operator edit through catalogue admin: %v", err)
	}
	if edited.Enabled || edited.Status != status || edited.Title != title || edited.Description != description ||
		edited.Usage == nil || *edited.Usage != usage || !edited.UpdatedByUserID.Valid ||
		uuid.UUID(edited.UpdatedByUserID.Bytes) != actor || edited.EntrySha256 == nil {
		t.Fatalf("operator settings did not land: %+v", edited)
	}
	m169Limits(t, edited.Limits, map[string]any{"builders_enabled": []any{"elementor"}, "max_output_bytes": float64(16384)})
	before := m183Read(t, pool, tenant)
	if err := apply(); err != nil {
		t.Fatalf("rerun m183 over operator settings: %v", err)
	}
	if after := m183Read(t, pool, tenant); !reflect.DeepEqual(before, after) {
		t.Fatalf("rerun changed operator settings:\nbefore %+v\nafter %+v", before, after)
	}
	revoke()
	if err := apply(); m182SQLState(err) != "42501" {
		t.Fatalf("after removing test permission: got %v, want 42501", err)
	}
}
