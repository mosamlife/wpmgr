// m176 proofs (#890): after every migration, wpmgr/page-create's usage is
// m176's, which names the Elementor versions the agent builds (3.20 to 4.3),
// the image alignments and the button link rule it enforces, and that Atomic
// is always refused; the hash is the hash of the new bytes and describe hands
// the AI the new usage. A row on m166's usage converges: only usage changes,
// the hash is cleared and the boot stamp re-stamps it. A re-run changes
// nothing. A usage a person rewrote is left exactly as it is, with a WARNING
// and without stopping the boot. A person's own description or limits do not
// block the correction and are kept.
//
// One database for every step. Catalogue reads, describe and the stamp go
// through the production paths as wpmgr_app (the e2World harness asserts the
// role). Migration bodies are applied as wpmgr_owner, the NOSUPERUSER
// NOBYPASSRLS migrator, on a connection that records the notices the body
// raises.
//
// Each guard has a mutation that strips it and requires the defect it
// prevents.
package tests

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilities"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

const m176MigrationVersion = "20261010000000_m176_page_create_elementor_usage"

// The usage m176 writes, spelled out here rather than read from the migration
// so that a change to either side is a visible test change: m162's usage, the
// Elementor sentences m166 added, with the rules and the version range the
// agent enforces.
const (
	m176Usage = m162Usage + " On a site with Elementor, send editor builder:elementor to build the " +
		"draft in Elementor from the same outline. The draft is built with Elementor's classic widgets: leave " +
		"elementor_format out or send classic; site_default, the default, builds classic widgets too. Atomic is not " +
		"available yet: elementor_format atomic is always refused, whatever the site runs. In Elementor, buttons " +
		"cannot use the outline style, a button link cannot contain &, a paragraph cannot be only a web address, " +
		"an image's align can only be none or center, and an image's alt text must be exactly the alt text it has " +
		"in the media library. Elementor pages need the WPMgr plugin 0.61.161 or later and Elementor 3.20 to 4.3 on " +
		"the site; a later Elementor is refused until WPMgr verifies it."

	m176HumanUsage = "Our own instructions for building pages, Elementor included."

	m176WarningPersonUsage = "m176: the usage of wpmgr/page-create was changed by a person; leaving it as it is"
)

// The guard texts the mutations strip. stripOnce fails loudly when one is not
// found, so a reworded guard cannot turn a mutation vacuous.
const (
	m176NeedleGuard = "    IF v_usage_now IS DISTINCT FROM v_m166_usage THEN\n" +
		"        RAISE WARNING 'm176: the usage of % was changed by a person; leaving it as it is', v_name\n" +
		"            USING HINT = 'To describe the Elementor versions and limits WPMgr builds, set its usage to the m176 text ' ||\n" +
		"                         'through the superadmin catalogue write.';\n" +
		"        RETURN;\n" +
		"    END IF;\n"
	m176NeedleUpdateOnlyM166 = "\n       AND \"usage\" IS NOT DISTINCT FROM v_m166_usage"
)

func m176Body(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(migrations.FS, m176MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m176 migration body: %v", err)
	}
	return string(b)
}

// m176Apply runs a migration body as wpmgr_owner in one transaction, the way
// internal/db's runner applies a file, and returns the SQLSTATE ("" on
// success) and every WARNING the body raised. The connection is
// connectOwner's, with a notice handler added.
func m176Apply(t *testing.T, w *e2World, body string) (string, []string) {
	t.Helper()
	ctx := context.Background()
	owner := connectOwner(t, w.pool)
	defer owner.Close()
	cfg := owner.Config().ConnConfig.Copy()
	var warnings []string
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		if n.Severity == "WARNING" {
			warnings = append(warnings, n.Message)
		}
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("SETUP: connect as the migrator: %v", err)
	}
	defer conn.Close(ctx)

	var role string
	var super, bypass bool
	if err := conn.QueryRow(ctx,
		`SELECT rolname, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&role, &super, &bypass); err != nil {
		t.Fatalf("SETUP: read the migrator role: %v", err)
	}
	if role != "wpmgr_owner" || super || bypass {
		t.Fatalf("SETUP: m176 would run as %s (superuser %v, bypassrls %v), want wpmgr_owner with neither", role, super, bypass)
	}

	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, body)
		return err
	})
	if err == nil {
		return "", warnings
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("applying m176 returned a non-database error: %v", err)
	}
	return pgErr.Code, warnings
}

// m166Landed puts page-create where m166 leaves it on an install that has not
// run m176: m162Landed, then m166's own body as the migrator, then the boot
// stamp. It returns the stamped hash.
func m166Landed(t *testing.T, w *e2World) string {
	t.Helper()
	m162Landed(t, w)
	if code := m162Apply(t, w, m166Body(t)); code != "" {
		t.Fatalf("SETUP: m166 on m162's row failed with %s", code)
	}
	m162Stamp(t, w)
	r := m162Row(t, w)
	m166AssertCopy(t, r, true)
	return m162AssertStamped(t, r)
}

// m176SameOutsideUsage fails unless a and b agree on every column m176 must
// not write: everything but usage, entry_sha256 and updated_at.
func m176SameOutsideUsage(t *testing.T, what string, a, b sqlc.AbilityCatalogue) {
	t.Helper()
	for _, r := range []*sqlc.AbilityCatalogue{&a, &b} {
		r.Usage, r.EntrySha256, r.UpdatedAt = nil, nil, time.Time{}
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("%s: m176 moved a column other than usage:\nbefore %+v\nafter  %+v", what, a, b)
	}
}

// m176AssertUnchanged fails unless after is before, column for column.
func m176AssertUnchanged(t *testing.T, what string, before, after sqlc.AbilityCatalogue) {
	t.Helper()
	if !reflect.DeepEqual(before.Usage, after.Usage) || !reflect.DeepEqual(before.EntrySha256, after.EntrySha256) ||
		!before.UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatalf("%s: the row changed:\nbefore %+v\nafter  %+v", what, before, after)
	}
	m176SameOutsideUsage(t, what, before, after)
}

func m176AssertUsage(t *testing.T, r sqlc.AbilityCatalogue, want string) {
	t.Helper()
	if r.Usage == nil || *r.Usage != want {
		stored := "(NULL)"
		if r.Usage != nil {
			stored = *r.Usage
		}
		t.Fatalf("usage = %q\nwant:\n%q", stored, want)
	}
}

// TestBuilderPageCreateM176AsAppRole runs every m176 proof on one database.
func TestBuilderPageCreateM176AsAppRole(t *testing.T) {
	w := newE2World(t, true)

	// 1. A fresh install: every migration, then the boot stamp. The row has
	// m176's usage over m166's description and limits, its hash is the hash
	// of its bytes, and describe hands the AI the new usage.
	fresh := m162Row(t, w)
	m176AssertUsage(t, fresh, m176Usage)
	if fresh.Description != m166Description ||
		!reflect.DeepEqual(m166LimitsOf(t, fresh.Limits), m166LimitsOf(t, []byte(m166Limits))) {
		t.Fatalf("a fresh install's description or limits are not m166's: %+v", fresh)
	}
	m162AssertStamped(t, fresh)
	res := cpeCall(t, w.eng, w.bearer, mcp.ToolSiteAbilityDescribe, map[string]any{
		"site_id": w.site.String(), "name": mcp.AbilityPageCreate,
	}).wantOK(t, "describe page-create")
	wp, ok := res["wpmgr"].(map[string]any)
	if !ok {
		t.Fatalf("describe has no wpmgr block: %v", res)
	}
	if wp["usage"] != m176Usage {
		t.Fatalf("describe usage = %v, want m176's", wp["usage"])
	}

	// 2. An install on m166: m176 writes the usage, clears the hash and
	// touches nothing else; the stamp then fills the hash from the new bytes.
	old := m166Landed(t, w)
	before := m162Row(t, w)
	code, warnings := m176Apply(t, w, m176Body(t))
	if code != "" || len(warnings) != 0 {
		t.Fatalf("m176 on m166's row: code %q, warnings %q; want neither", code, warnings)
	}
	mid := m162Row(t, w)
	m176AssertUsage(t, mid, m176Usage)
	if mid.EntrySha256 != nil {
		t.Fatalf("m176 left entry_sha256 = %s; the boot stamp cannot re-fill a set hash", *mid.EntrySha256)
	}
	m176SameOutsideUsage(t, "m166's row", before, mid)
	m162Stamp(t, w)
	stamped := m162Row(t, w)
	sum := m162AssertStamped(t, stamped)
	if sum == old {
		t.Fatalf("the re-stamped hash equals m166's (%s); the new usage did not reach the entry bytes", old)
	}

	// 3. A second run changes nothing: not the usage, not the hash, not
	// updated_at, so it cannot close an approval a second time.
	code, warnings = m176Apply(t, w, m176Body(t))
	if code != "" || len(warnings) != 0 {
		t.Fatalf("a second m176: code %q, warnings %q; want neither", code, warnings)
	}
	m176AssertUnchanged(t, "a second m176", stamped, m162Row(t, w))

	// 4. A usage a person rewrote through the superadmin write: m176 warns,
	// does not stop, and leaves the row exactly as the person left it.
	m166Landed(t, w)
	m162SuperadminWrite(t, w, func(r *sqlc.AbilityCatalogue) {
		s := m176HumanUsage
		r.Usage = &s
	})
	edited := m162Row(t, w)
	if edited.Usage == nil || *edited.Usage != m176HumanUsage || !edited.UpdatedByUserID.Valid || edited.EntrySha256 == nil {
		t.Fatalf("INDETERMINATE: the superadmin usage edit did not land: %+v", edited)
	}
	code, warnings = m176Apply(t, w, m176Body(t))
	if code != "" {
		t.Fatalf("m176 over a person's usage failed with %s; it must warn and carry on", code)
	}
	if len(warnings) != 1 || warnings[0] != m176WarningPersonUsage {
		t.Fatalf("m176 over a person's usage raised warnings %q, want exactly %q", warnings, m176WarningPersonUsage)
	}
	m176AssertUnchanged(t, "m176 over a person's usage", edited, m162Row(t, w))

	// 5. Mutations on that row. Without the warning branch the boot stops
	// (the UPDATE, which only ever moves m166's text, hits no row); without
	// it and the UPDATE's own predicate, the person's usage is overwritten.
	if code, _ := m176Apply(t, w, stripOnce(t, m176Body(t), m176NeedleGuard)); code != "P0002" {
		t.Fatalf("mutation did not fire: without the warning branch, m176 over a person's usage returned %q, "+
			"want P0002; the branch is not proven load-bearing", code)
	}
	m176AssertUnchanged(t, "a refused mutated m176", edited, m162Row(t, w))
	unguarded := stripOnce(t, stripOnce(t, m176Body(t), m176NeedleGuard), m176NeedleUpdateOnlyM166)
	if code, _ := m176Apply(t, w, unguarded); code != "" {
		t.Fatalf("the unguarded m176 failed with %s; the mutation did not run", code)
	}
	if r := m162Row(t, w); r.Usage == nil || *r.Usage != m176Usage {
		t.Fatalf("mutation did not fire: without both guards m176 should overwrite the person's usage, usage is %v; "+
			"the guards are not proven load-bearing", r.Usage)
	}

	// 6. A person changed the description and the limits but not the usage:
	// m176 corrects the usage and keeps the person's description and limits.
	m166Landed(t, w)
	var tightened map[string]any
	if err := json.Unmarshal([]byte(m166Limits), &tightened); err != nil {
		t.Fatal(err)
	}
	tightened["max_images"] = 5
	tightenedRaw, err := json.Marshal(tightened)
	if err != nil {
		t.Fatal(err)
	}
	m162SuperadminWrite(t, w, func(r *sqlc.AbilityCatalogue) {
		r.Description = m166HumanDescription
		r.Limits = tightenedRaw
	})
	edited = m162Row(t, w)
	if edited.Description != m166HumanDescription || !edited.UpdatedByUserID.Valid {
		t.Fatalf("INDETERMINATE: the superadmin description and limits edit did not land: %+v", edited)
	}
	code, warnings = m176Apply(t, w, m176Body(t))
	if code != "" || len(warnings) != 0 {
		t.Fatalf("m176 over a person's description and limits: code %q, warnings %q; want neither", code, warnings)
	}
	r := m162Row(t, w)
	m176AssertUsage(t, r, m176Usage)
	if r.EntrySha256 != nil {
		t.Fatalf("m176 left the hash set on a row whose usage it changed: %s", *r.EntrySha256)
	}
	m176SameOutsideUsage(t, "a person's description and limits", edited, r)
	if got := m166LimitsOf(t, r.Limits)["max_images"]; got != float64(5) {
		t.Fatalf("the person's max_images became %v, want 5", got)
	}
	m162Stamp(t, w)
	m162AssertStamped(t, m162Row(t, w))

	// 7. Mutation: without the hash clear, the entry m176 rewrote is never
	// sent again.
	m166Landed(t, w)
	if code, _ := m176Apply(t, w, stripOnce(t, m176Body(t), m162NeedleHashClear)); code != "" {
		t.Fatalf("the m176 without its hash clear failed with %s; the mutation did not run", code)
	}
	m162Stamp(t, w)
	r = m162Row(t, w)
	if r.Usage == nil || *r.Usage != m176Usage {
		t.Fatalf("INDETERMINATE: the mutated m176 did not write its usage: %v", r.Usage)
	}
	if _, _, err := abilities.SendableEntry(r); !errors.Is(err, abilities.ErrEntryChanged) {
		t.Fatalf("mutation did not fire: with the hash clear stripped, page-create should be unsendable "+
			"(ErrEntryChanged), got err=%v; the clear is not proven load-bearing", err)
	}
}
