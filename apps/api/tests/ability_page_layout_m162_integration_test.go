// m162 proofs (slice GUT): after the migration and the boot stamp,
// wpmgr/page-create carries the layout description, usage and limits, its
// hash is the hash of its new bytes, and describe shows the copy to the AI; a
// request approved before the re-stamp closes not_sent / entry_changed; a
// re-run is a no-op that closes nothing; a row whose copy a person changed
// stops the migration; a row a person only disabled takes the new copy and
// stays disabled.
//
// Every catalogue read, request and dispatch goes through the production
// dispatch as wpmgr_app (the e2World harness, which asserts the role). The
// migration body is re-applied as wpmgr_owner, the NOSUPERUSER NOBYPASSRLS
// migrator. The superuser connection only puts the row back into the state
// m157 left it in, standing in for a database that has not yet run m162.
//
// Each guard has a mutation test that applies the migration with the guard
// stripped and requires the defect the guard prevents.
package tests

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilities"
	"github.com/mosamlife/wpmgr/apps/api/internal/abilityrequest"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

const m162MigrationVersion = "20261009000000_m162_page_create_layout_copy"

// The copy m162 writes, and the copy m157 seeded. They are spelled out here
// rather than read from the migration so that a change to either side is a
// visible test change.
const (
	m162SeedDescription = "Creates a new draft page or post from a text outline. Nothing is published. Undo moves the draft to the trash."

	m162Description = "Creates a new draft page or post from an outline: headings, paragraphs, lists, quotes, tables, separators, " +
		"images already in the site's media library, and, in the block editor, buttons, spacing, sections and " +
		"columns. Nothing is published. Undo moves the draft to the trash."

	m162Usage = "Build the page as an outline. A top-level item can be any block, a group (a section) or columns. A group " +
		"holds blocks or columns; a column holds blocks only. Use 2 to 4 columns; widths are optional whole " +
		"percentages that add up to 100. Images must already be in the media library: find an attachment id with " +
		"wpmgr/rest-read route wp-v2-media-list, and write alt text that describes the picture, or an empty alt for " +
		"a decorative one. Button links are https:// addresses or paths on this site that start with /. All text is " +
		"plain: no HTML, shortcodes or template syntax; square brackets only around a number such as [1], and never " +
		"in alt text. On a site that uses the classic editor, send editor wordpress_classic and only headings, " +
		"paragraphs, lists, quotes, tables, separators and images without captions. Layout blocks need the WPMgr " +
		"plugin 0.61.160 or later on the site."

	m162Limits = `{"max_top_level_nodes":200,"max_nodes":400,"max_columns":4,"max_children":50,"max_images":20,` +
		`"max_buttons":12,"max_tables":10,"max_table_rows":50,"max_table_columns":6,"max_title_chars":200,` +
		`"max_text_chars":5000,"max_total_chars":60000,"max_input_bytes":65536}`
)

// The guard texts the mutation tests strip. stripOnce fails loudly when one
// is not found, so a reworded guard cannot turn a mutation test vacuous.
const (
	m162NeedleHashClear = "           \"entry_sha256\" = NULL,\n"
	m162NeedleNoOp      = "    IF (v_desc, v_usage_now, v_limits_now) IS NOT DISTINCT FROM (v_description, v_usage, v_limits) THEN\n" +
		"        RETURN;\n" +
		"    END IF;\n"
	m162NeedleRefusal = "        RAISE EXCEPTION 'm162: the copy of % was changed by a person; refusing to overwrite it', v_name\n" +
		"            USING ERRCODE = '55000',\n" +
		"                  HINT = 'Set its description, usage and limits back to the m157 values, or to the m162 values, ' ||\n" +
		"                         'through the superadmin catalogue write, then start the new revision again.';\n"
)

func m162Body(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(migrations.FS, m162MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m162 migration body: %v", err)
	}
	return string(b)
}

// m162Apply runs a migration body as wpmgr_owner in one transaction, the way
// internal/db's runner applies a file, and returns the SQLSTATE ("" on
// success). The version row is already present, so it is not re-inserted.
func m162Apply(t *testing.T, w *e2World, body string) string {
	t.Helper()
	ctx := context.Background()
	owner := connectOwner(t, w.pool)
	defer owner.Close()
	err := pgx.BeginFunc(ctx, owner.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, body)
		return err
	})
	if err == nil {
		return ""
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("applying m162 returned a non-database error: %v", err)
	}
	return pgErr.Code
}

// m162Row reads wpmgr/page-create as wpmgr_app through the generated query.
func m162Row(t *testing.T, w *e2World) sqlc.AbilityCatalogue {
	t.Helper()
	var row sqlc.AbilityCatalogue
	if err := w.pool.InTenantTx(context.Background(), w.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m162 read)")
		row = m157Entry(t, tx, mcp.AbilityPageCreate)
		return nil
	}); err != nil {
		t.Fatalf("read page-create: %v", err)
	}
	return row
}

// m162ResetToSeed puts the row back exactly as m157 left it: the database
// state m162 meets on an install that has not run it yet.
func m162ResetToSeed(t *testing.T, w *e2World) {
	t.Helper()
	tag, err := w.admin.Exec(context.Background(), `UPDATE ability_catalogue
		SET description = $2, usage = NULL, limits = '{}'::jsonb, entry_sha256 = NULL,
		    enabled = true, updated_by_user_id = NULL
		WHERE name = $1 AND source = 'wpmgr'`, mcp.AbilityPageCreate, m162SeedDescription)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("SETUP: reset page-create to the m157 seed: rows=%d err=%v", tag.RowsAffected(), err)
	}
}

func m162Stamp(t *testing.T, w *e2World) {
	t.Helper()
	if _, err := abilities.StampOwnEntryHashes(context.Background(), w.pool, slog.Default()); err != nil {
		t.Fatalf("boot stamp: %v", err)
	}
}

func m162LimitsOf(t *testing.T, raw []byte) map[string]int64 {
	t.Helper()
	var m map[string]int64
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("limits %s are not an object of integers: %v", raw, err)
	}
	return m
}

// m162AssertCopy fails unless the row carries m162's copy and nothing else
// m162 must leave alone moved.
func m162AssertCopy(t *testing.T, r sqlc.AbilityCatalogue, wantEnabled bool) {
	t.Helper()
	if r.Description != m162Description {
		t.Fatalf("description = %q, want m162's", r.Description)
	}
	if r.Usage == nil || *r.Usage != m162Usage {
		t.Fatalf("usage = %v, want m162's", r.Usage)
	}
	if got, want := m162LimitsOf(t, r.Limits), m162LimitsOf(t, []byte(m162Limits)); !reflect.DeepEqual(got, want) {
		t.Fatalf("limits = %v, want %v", got, want)
	}
	if r.MinAgentVersion == nil || *r.MinAgentVersion != "0.61.156" {
		t.Fatalf("min_agent_version = %v, want the text-only floor 0.61.156 unchanged", r.MinAgentVersion)
	}
	if r.Title != "Create a draft page" || r.Source != "wpmgr" || r.Class != "write" || r.Status != "admitted" ||
		r.Enabled != wantEnabled {
		t.Fatalf("a column m162 must not touch moved: %+v", r)
	}
}

// m162AssertStamped fails unless the stored hash is the hash of the row's
// own canonical bytes, so the entry is sendable.
func m162AssertStamped(t *testing.T, r sqlc.AbilityCatalogue) string {
	t.Helper()
	_, sum, err := abilities.SendableEntry(r)
	if err != nil {
		t.Fatalf("page-create is not sendable after the stamp: %v", err)
	}
	if r.EntrySha256 == nil || *r.EntrySha256 != sum {
		t.Fatalf("entry_sha256 = %v, want %s", r.EntrySha256, sum)
	}
	return sum
}

func (w *e2World) approve(t *testing.T, id uuid.UUID) {
	t.Helper()
	if _, err := w.svc.Approve(context.Background(), w.person, w.site, id, w.digest(t, id)); err != nil {
		t.Fatalf("approve: %v", err)
	}
}

// TestPageLayoutM162CopyStampedAndDescribedAsAppRole: on an install that has
// not run m166, m162 and the boot stamp leave page-create with m162's copy
// and a hash of its new bytes, and describe hands the AI that copy and every
// limit.
func TestPageLayoutM162CopyStampedAndDescribedAsAppRole(t *testing.T) {
	w := newE2World(t, true)
	m162Landed(t, w)
	r := m162Row(t, w)
	m162AssertCopy(t, r, true)
	m162AssertStamped(t, r)

	res := cpeCall(t, w.eng, w.bearer, mcp.ToolSiteAbilityDescribe, map[string]any{
		"site_id": w.site.String(), "name": mcp.AbilityPageCreate,
	}).wantOK(t, "describe page-create")
	wp, ok := res["wpmgr"].(map[string]any)
	if !ok {
		t.Fatalf("describe has no wpmgr block: %v", res)
	}
	if wp["description"] != m162Description || wp["usage"] != m162Usage {
		t.Fatalf("describe copy: description=%v usage=%v", wp["description"], wp["usage"])
	}
	limits, err := json.Marshal(wp["limits"])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := m162LimitsOf(t, limits), m162LimitsOf(t, []byte(m162Limits)); !reflect.DeepEqual(got, want) {
		t.Fatalf("describe limits = %v, want every m162 limit %v", got, want)
	}
}

// TestPageLayoutM162ClosesRequestApprovedBeforeRestampAsAppRole: a request
// approved against the m157 entry and dispatched after m162 and the re-stamp
// sends nothing and closes not_sent / entry_changed; a request asked after
// the re-stamp runs.
func TestPageLayoutM162ClosesRequestApprovedBeforeRestampAsAppRole(t *testing.T) {
	w := newE2World(t, true)
	m162ResetToSeed(t, w)
	m162Stamp(t, w)
	old := m162AssertStamped(t, m162Row(t, w))

	id := w.ask(t)
	w.approve(t, id)

	if code := m162Apply(t, w, m162Body(t)); code != "" {
		t.Fatalf("m162 on the m157 row failed with %s", code)
	}
	mid := m162Row(t, w)
	m162AssertCopy(t, mid, true)
	if mid.EntrySha256 != nil {
		t.Fatalf("m162 left entry_sha256 = %s; the boot stamp cannot re-fill a set hash", *mid.EntrySha256)
	}
	m162Stamp(t, w)
	if now := m162AssertStamped(t, m162Row(t, w)); now == old {
		t.Fatalf("the re-stamped hash equals the m157 one (%s); the copy did not reach the entry bytes", old)
	}

	w.dispatch(t, id)
	if n := w.agent.sent(agentcmd.AbilityRunModeWrite); n != 0 {
		t.Fatalf("%d writes sent against an entry that changed after approval", n)
	}
	state, _, notSent, _, _ := w.row(t, id)
	if state != "not_sent" || notSent == nil || *notSent != abilityrequest.ReasonEntryChanged {
		t.Fatalf("row: state=%s not_sent_reason=%v, want not_sent/entry_changed", state, notSent)
	}

	fresh := w.ask(t)
	w.approve(t, fresh)
	w.dispatch(t, fresh)
	if n := w.agent.sent(agentcmd.AbilityRunModeWrite); n != 1 {
		t.Fatalf("a request asked after the re-stamp sent %d writes, want 1", n)
	}
	if state, _, _, _, _ := w.row(t, fresh); state != "done" {
		t.Fatalf("a request asked after the re-stamp ended %s, want done", state)
	}
}

// TestPageLayoutM162MutationKeepsHashBlocksEverySend: without the hash clear
// the copy changes under the old hash, the stamp cannot touch a set hash, and
// every send of page-create is refused as a changed entry.
func TestPageLayoutM162MutationKeepsHashBlocksEverySend(t *testing.T) {
	w := newE2World(t, true)
	m162ResetToSeed(t, w)
	m162Stamp(t, w)
	m162AssertStamped(t, m162Row(t, w))

	mutated := stripOnce(t, m162Body(t), m162NeedleHashClear)
	if code := m162Apply(t, w, mutated); code != "" {
		t.Fatalf("mutated m162 failed with %s", code)
	}
	m162Stamp(t, w)
	r := m162Row(t, w)
	if _, _, err := abilities.SendableEntry(r); !errors.Is(err, abilities.ErrEntryChanged) {
		t.Fatalf("mutation did not fire: with the hash clear stripped, page-create should be unsendable "+
			"(ErrEntryChanged), got err=%v; the clear is not proven load-bearing", err)
	}
}

// TestPageLayoutM162RerunIsNoOpAsAppRole: m162 applied a second time changes
// nothing, so a request approved after the first run and its stamp still
// runs.
func TestPageLayoutM162RerunIsNoOpAsAppRole(t *testing.T) {
	w := newE2World(t, true)
	m162Landed(t, w)
	before := m162Row(t, w)
	m162AssertCopy(t, before, true)
	sum := m162AssertStamped(t, before)

	id := w.ask(t)
	w.approve(t, id)

	if code := m162Apply(t, w, m162Body(t)); code != "" {
		t.Fatalf("a second m162 failed with %s", code)
	}
	after := m162Row(t, w)
	if after.EntrySha256 == nil || *after.EntrySha256 != sum || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("a second m162 changed the row: hash %v -> %v, updated_at %v -> %v",
			before.EntrySha256, after.EntrySha256, before.UpdatedAt, after.UpdatedAt)
	}

	w.dispatch(t, id)
	if n := w.agent.sent(agentcmd.AbilityRunModeWrite); n != 1 {
		t.Fatalf("a request approved before the re-run sent %d writes, want 1", n)
	}
	if state, _, _, _, _ := w.row(t, id); state != "done" {
		t.Fatalf("a request approved before the re-run ended %s, want done", state)
	}
}

// TestPageLayoutM162MutationNoOpBranchStopsBoot: without the already-applied
// branch, a re-run meets a row that is no longer the m157 seed and stops the
// boot.
func TestPageLayoutM162MutationNoOpBranchStopsBoot(t *testing.T) {
	w := newE2World(t, true)
	sum := m162Landed(t, w)

	mutated := stripOnce(t, m162Body(t), m162NeedleNoOp)
	if code := m162Apply(t, w, mutated); code != "55000" {
		t.Fatalf("mutation did not fire: a re-run without the already-applied branch should stop with 55000, "+
			"got %q; the branch is not proven load-bearing", code)
	}
	if r := m162Row(t, w); r.EntrySha256 == nil || *r.EntrySha256 != sum {
		t.Fatalf("a refused re-run changed the hash to %v", r.EntrySha256)
	}
}

// m162SuperadminWrite writes page-create through the one write path, as
// wpmgr_app with a superadmin actor, keeping every column except the ones
// edit changes. The hash is stamped from the would-be row, as the Go admin
// path does.
func m162SuperadminWrite(t *testing.T, w *e2World, edit func(*sqlc.AbilityCatalogue)) {
	t.Helper()
	ctx := context.Background()
	admin := uuid.New()
	if _, err := w.admin.Exec(ctx, `INSERT INTO users (id, email, is_superadmin) VALUES ($1, $2, true)`,
		admin, "m162-"+admin.String()[:8]+"@example.test"); err != nil {
		t.Fatalf("SETUP: seed superadmin: %v", err)
	}
	r := m162Row(t, w)
	edit(&r)
	_, sum, err := abilities.EntryBytes(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.pool.InTenantTx(ctx, w.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m162 superadmin write)")
		_, err := sqlc.New(tx).AdminUpsertAbilityCatalogueEntry(ctx, sqlc.AdminUpsertAbilityCatalogueEntryParams{
			ActorUserID: admin, EntryID: pgtype.UUID{Bytes: r.EntryID, Valid: true},
			Name: r.Name, Source: r.Source, Class: r.Class, Status: r.Status, Enabled: r.Enabled,
			ApprovalMode: r.ApprovalMode, PermissionMode: r.PermissionMode, IntegrationID: r.IntegrationID,
			OwnerDir: r.OwnerDir, VersionMin: r.VersionMin, VersionMaxTested: r.VersionMaxTested,
			MinWpVersion: r.MinWpVersion, MinAgentVersion: r.MinAgentVersion, SchemaStructSha256: r.SchemaStructSha256,
			DynamicEnumPaths: r.DynamicEnumPaths, Title: r.Title, Description: r.Description, Usage: r.Usage,
			OperatorPermission: r.OperatorPermission, Target: r.Target, Snapshot: r.Snapshot, Preview: r.Preview,
			ArgRender: r.ArgRender, EffectCopy: r.EffectCopy, Limits: r.Limits, NestedAllow: r.NestedAllow,
			GlobalOptionKeys: r.GlobalOptionKeys, IntegrationBlock: r.IntegrationBlock, Admission: r.Admission,
			EntrySha256: &sum, OutputFields: r.OutputFields,
		})
		return err
	}); err != nil {
		t.Fatalf("SETUP: superadmin write: %v", err)
	}
}

const m162HumanDescription = "Our own wording for page creation."

// TestPageLayoutM162RefusesHumanCopyKeepsKillSwitchAsAppRole: on a row whose
// description a superadmin rewrote, m162 stops with 55000 and changes
// nothing; on a row a superadmin only disabled, m162 writes the copy, leaves
// it disabled, and the stamp re-fills the hash.
func TestPageLayoutM162RefusesHumanCopyKeepsKillSwitchAsAppRole(t *testing.T) {
	w := newE2World(t, true)

	m162ResetToSeed(t, w)
	m162SuperadminWrite(t, w, func(r *sqlc.AbilityCatalogue) { r.Description = m162HumanDescription })
	edited := m162Row(t, w)
	if edited.Description != m162HumanDescription || !edited.UpdatedByUserID.Valid {
		t.Fatalf("INDETERMINATE: the superadmin edit did not land: %+v", edited)
	}
	if code := m162Apply(t, w, m162Body(t)); code != "55000" {
		t.Fatalf("m162 over a person's copy returned %q, want 55000", code)
	}
	after := m162Row(t, w)
	if after.Description != m162HumanDescription || after.Usage != nil ||
		!reflect.DeepEqual(after.EntrySha256, edited.EntrySha256) {
		t.Fatalf("a refused m162 changed the row: %+v", after)
	}

	m162ResetToSeed(t, w)
	m162SuperadminWrite(t, w, func(r *sqlc.AbilityCatalogue) { r.Enabled = false })
	disabled := m162Row(t, w)
	if disabled.Enabled || !disabled.UpdatedByUserID.Valid || disabled.Description != m162SeedDescription {
		t.Fatalf("INDETERMINATE: the kill switch did not land as a superadmin write: %+v", disabled)
	}
	if code := m162Apply(t, w, m162Body(t)); code != "" {
		t.Fatalf("m162 over a disabled, otherwise seeded row failed with %s", code)
	}
	r := m162Row(t, w)
	m162AssertCopy(t, r, false)
	if r.EntrySha256 != nil {
		t.Fatalf("m162 left the disabled row's hash set: %s", *r.EntrySha256)
	}
	m162Stamp(t, w)
	m162AssertStamped(t, m162Row(t, w))
}

// TestPageLayoutM162MutationOverwritesHumanCopy: without the refusal, m162
// overwrites the copy a person wrote.
func TestPageLayoutM162MutationOverwritesHumanCopy(t *testing.T) {
	w := newE2World(t, true)
	m162ResetToSeed(t, w)
	m162SuperadminWrite(t, w, func(r *sqlc.AbilityCatalogue) { r.Description = m162HumanDescription })

	mutated := stripOnce(t, m162Body(t), m162NeedleRefusal)
	_ = m162Apply(t, w, mutated)
	if r := m162Row(t, w); r.Description != m162Description {
		t.Fatalf("mutation did not fire: without the refusal, m162 should overwrite the person's copy, "+
			"description is %q; the refusal is not proven load-bearing", r.Description)
	}
}
