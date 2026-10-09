// m166 proofs (slice BF-C): after the migration, wpmgr/page-create's copy and
// usage name editor builder:elementor and elementor_format, its limits keep
// m162's integers and add builders_enabled ["elementor"], and its hash is
// cleared, so the boot stamp stamps the new bytes and the signed entry
// carries builders_enabled to the agent. A re-run changes nothing, so a
// request approved before it still runs. A row whose copy a person changed
// stops the migration with 55000 and keeps the person's copy; with that guard
// stripped, the migration overwrites it. A row a person only disabled takes
// the new copy and stays disabled.
//
// One database for every step. Catalogue reads, describe, requests and the
// stamp go through the production paths as wpmgr_app (the e2World harness
// asserts the role). Migration bodies are applied as wpmgr_owner, the
// NOSUPERUSER NOBYPASSRLS migrator. The superuser connection only puts the
// row back into the state m157 seeded, from which m162's own body, applied
// as wpmgr_owner, brings it to the state m166 meets on an install that has
// not run it.
package tests

import (
	"encoding/json"
	"io/fs"
	"reflect"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilities"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

const m166MigrationVersion = "20261009060000_m166_builder_page_create"

// The copy m166 writes. It is spelled out here rather than read from the
// migration so that a change to either side is a visible test change; the
// usage is m162's usage, unchanged, followed by the Elementor text.
const (
	m166Description = "Creates a new draft page or post from an outline: headings, paragraphs, lists, quotes, tables, separators, " +
		"images already in the site's media library, and, in the block editor or Elementor, buttons, spacing, " +
		"sections and columns. With editor builder:elementor the draft is built in Elementor, from Elementor's own " +
		"layout elements and widgets. Nothing is published. Undo moves the draft to the trash."

	m166Usage = m162Usage + " On a site with Elementor, send editor builder:elementor to build the " +
		"draft in Elementor from the same outline. The draft is built with Elementor's classic widgets: leave " +
		"elementor_format out or send classic; site_default, the default, builds classic widgets too. Atomic is not " +
		"available yet: elementor_format atomic is always refused, whatever the site runs. In Elementor, buttons " +
		"cannot use the outline style, a " +
		"paragraph cannot be only a web address, and an image's alt text must be exactly the alt text it has in the " +
		"media library. Elementor pages need the WPMgr plugin 0.61.161 or later and Elementor 3.20 or later on the " +
		"site."

	m166Limits = `{"max_top_level_nodes":200,"max_nodes":400,"max_columns":4,"max_children":50,"max_images":20,` +
		`"max_buttons":12,"max_tables":10,"max_table_rows":50,"max_table_columns":6,"max_title_chars":200,` +
		`"max_text_chars":5000,"max_total_chars":60000,"max_input_bytes":65536,"builders_enabled":["elementor"]}`

	m166HumanDescription = "Our own wording for page creation, Elementor included."
)

// m166NeedleGuard is the guard the mutation strips: the refusal of a row
// whose copy is neither m162's nor m166's. stripOnce fails loudly when it is
// not found, so a reworded guard cannot turn the mutation vacuous.
const m166NeedleGuard = "    IF (v_desc, v_usage_now, v_limits_now) IS DISTINCT FROM (v_m162_description, v_m162_usage, v_m162_limits) THEN\n" +
	"        RAISE EXCEPTION 'm166: the copy of % was changed by a person; refusing to overwrite it', v_name\n" +
	"            USING ERRCODE = '55000',\n" +
	"                  HINT = 'Set its description, usage and limits back to the m162 values, or to the m166 values, ' ||\n" +
	"                         'through the superadmin catalogue write, then start the new revision again.';\n" +
	"    END IF;\n"

func m166Body(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(migrations.FS, m166MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m166 migration body: %v", err)
	}
	return string(b)
}

// m166LimitsOf decodes limits whole, arrays included.
func m166LimitsOf(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("limits %s are not a JSON object: %v", raw, err)
	}
	return m
}

// m162Landed puts page-create where m162 leaves it on an install that has
// not run m166: the m157 seed, then m162's own body as the migrator, then the
// boot stamp. It returns the stamped hash.
func m162Landed(t *testing.T, w *e2World) string {
	t.Helper()
	m162ResetToSeed(t, w)
	if code := m162Apply(t, w, m162Body(t)); code != "" {
		t.Fatalf("SETUP: m162 on the m157 seed failed with %s", code)
	}
	m162Stamp(t, w)
	r := m162Row(t, w)
	m162AssertCopy(t, r, true)
	return m162AssertStamped(t, r)
}

// m166AssertCopy fails unless the row carries m166's copy and limits, with
// builders_enabled exactly ["elementor"], and nothing m166 must leave alone
// moved.
func m166AssertCopy(t *testing.T, r sqlc.AbilityCatalogue, wantEnabled bool) {
	t.Helper()
	m166AssertCopyWithUsage(t, r, wantEnabled, m166Usage)
}

// m166AssertCopyWithUsage is m166AssertCopy with the usage a later migration
// wrote over m166's: on a fresh install, m176's (#890).
func m166AssertCopyWithUsage(t *testing.T, r sqlc.AbilityCatalogue, wantEnabled bool, wantUsage string) {
	t.Helper()
	if r.Description != m166Description {
		t.Fatalf("description = %q, want m166's", r.Description)
	}
	if r.Usage == nil || *r.Usage != wantUsage {
		stored := "(NULL)"
		if r.Usage != nil {
			stored = *r.Usage
		}
		t.Fatalf("usage = %q\nwant:\n%q", stored, wantUsage)
	}
	got := m166LimitsOf(t, r.Limits)
	if want := m166LimitsOf(t, []byte(m166Limits)); !reflect.DeepEqual(got, want) {
		t.Fatalf("limits = %v, want %v", got, want)
	}
	if b, ok := got["builders_enabled"].([]any); !ok || len(b) != 1 || b[0] != "elementor" {
		t.Fatalf("limits.builders_enabled = %v, want [\"elementor\"]", got["builders_enabled"])
	}
	if r.MinAgentVersion == nil || *r.MinAgentVersion != "0.61.156" {
		t.Fatalf("min_agent_version = %v, want the text-only floor 0.61.156 unchanged", r.MinAgentVersion)
	}
	if r.Title != "Create a draft page" || r.Source != "wpmgr" || r.Class != "write" || r.Status != "admitted" ||
		r.Enabled != wantEnabled {
		t.Fatalf("a column m166 must not touch moved: %+v", r)
	}
}

// m166AssertSignedEntryCarriesBuilders fails unless the entry bytes the
// control plane signs and sends name builders_enabled ["elementor"].
func m166AssertSignedEntryCarriesBuilders(t *testing.T, r sqlc.AbilityCatalogue) {
	t.Helper()
	b, sum, err := abilities.SendableEntry(r)
	if err != nil {
		t.Fatalf("page-create is not sendable: %v", err)
	}
	var e struct {
		Limits struct {
			BuildersEnabled []string `json:"builders_enabled"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("entry bytes: %v", err)
	}
	if !reflect.DeepEqual(e.Limits.BuildersEnabled, []string{"elementor"}) {
		t.Fatalf("the signed entry (sha256 %s) carries builders_enabled %v, want [elementor]", sum, e.Limits.BuildersEnabled)
	}
}

// TestBuilderPageCreateM166AsAppRole runs every m166 proof on one database.
func TestBuilderPageCreateM166AsAppRole(t *testing.T) {
	w := newE2World(t, true)

	// 1. A fresh install: every migration, then the boot stamp. The row has
	// m166's copy and limits, with the usage m176 corrected after it (#890),
	// its hash is the hash of its new bytes, the signed entry carries
	// builders_enabled, and describe hands the AI the copy and every integer
	// limit.
	fresh := m162Row(t, w)
	m166AssertCopyWithUsage(t, fresh, true, m176Usage)
	m162AssertStamped(t, fresh)
	m166AssertSignedEntryCarriesBuilders(t, fresh)
	res := cpeCall(t, w.eng, w.bearer, mcp.ToolSiteAbilityDescribe, map[string]any{
		"site_id": w.site.String(), "name": mcp.AbilityPageCreate,
	}).wantOK(t, "describe page-create")
	wp, ok := res["wpmgr"].(map[string]any)
	if !ok {
		t.Fatalf("describe has no wpmgr block: %v", res)
	}
	if wp["description"] != m166Description || wp["usage"] != m176Usage {
		t.Fatalf("describe copy: description=%v usage=%v", wp["description"], wp["usage"])
	}
	described, ok := wp["limits"].(map[string]any)
	if !ok {
		t.Fatalf("describe limits: %v", wp["limits"])
	}
	for k, v := range m166LimitsOf(t, []byte(m166Limits)) {
		if _, isNumber := v.(float64); isNumber && described[k] != v {
			t.Fatalf("describe limit %s = %v, want %v", k, described[k], v)
		}
	}

	// 2. An install on m162: m166 writes the copy and builders_enabled and
	// clears the hash; the stamp then fills it from the new bytes.
	old := m162Landed(t, w)
	if code := m162Apply(t, w, m166Body(t)); code != "" {
		t.Fatalf("m166 on m162's row failed with %s", code)
	}
	mid := m162Row(t, w)
	m166AssertCopy(t, mid, true)
	if mid.EntrySha256 != nil {
		t.Fatalf("m166 left entry_sha256 = %s; the boot stamp cannot re-fill a set hash", *mid.EntrySha256)
	}
	m162Stamp(t, w)
	stamped := m162Row(t, w)
	sum := m162AssertStamped(t, stamped)
	if sum == old {
		t.Fatalf("the re-stamped hash equals m162's (%s); the new copy did not reach the entry bytes", old)
	}
	m166AssertSignedEntryCarriesBuilders(t, stamped)

	// 3. A second run changes nothing, so a request approved after the first
	// run and its stamp still runs.
	id := w.ask(t)
	w.approve(t, id)
	if code := m162Apply(t, w, m166Body(t)); code != "" {
		t.Fatalf("a second m166 failed with %s", code)
	}
	again := m162Row(t, w)
	if again.EntrySha256 == nil || *again.EntrySha256 != sum || !again.UpdatedAt.Equal(stamped.UpdatedAt) {
		t.Fatalf("a second m166 changed the row: hash %v -> %v, updated_at %v -> %v",
			stamped.EntrySha256, again.EntrySha256, stamped.UpdatedAt, again.UpdatedAt)
	}
	m166AssertCopy(t, again, true)
	w.dispatch(t, id)
	if n := w.agent.sent(agentcmd.AbilityRunModeWrite); n != 1 {
		t.Fatalf("a request approved before the re-run sent %d writes, want 1", n)
	}
	if state, _, _, _, _ := w.row(t, id); state != "done" {
		t.Fatalf("a request approved before the re-run ended %s, want done", state)
	}

	// 4. A row whose copy a person changed through the superadmin write,
	// in its description or in its limits: m166 stops with 55000 and the
	// person's row stays exactly as it was.
	var tightened map[string]any
	if err := json.Unmarshal([]byte(m162Limits), &tightened); err != nil {
		t.Fatal(err)
	}
	tightened["max_images"] = 5
	tightenedRaw, err := json.Marshal(tightened)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		what string
		edit func(*sqlc.AbilityCatalogue)
	}{
		{"description", func(r *sqlc.AbilityCatalogue) { r.Description = m166HumanDescription }},
		{"limits", func(r *sqlc.AbilityCatalogue) { r.Limits = tightenedRaw }},
	} {
		m162Landed(t, w)
		m162SuperadminWrite(t, w, tc.edit)
		edited := m162Row(t, w)
		if !edited.UpdatedByUserID.Valid || edited.EntrySha256 == nil {
			t.Fatalf("INDETERMINATE: the superadmin %s edit did not land: %+v", tc.what, edited)
		}
		if code := m162Apply(t, w, m166Body(t)); code != "55000" {
			t.Fatalf("m166 over a person's %s returned %q, want 55000", tc.what, code)
		}
		after := m162Row(t, w)
		if after.Description != edited.Description || !reflect.DeepEqual(after.Usage, edited.Usage) ||
			!reflect.DeepEqual(m166LimitsOf(t, after.Limits), m166LimitsOf(t, edited.Limits)) ||
			!reflect.DeepEqual(after.EntrySha256, edited.EntrySha256) || !after.UpdatedAt.Equal(edited.UpdatedAt) {
			t.Fatalf("a refused m166 changed the person's %s row:\nbefore %+v\nafter  %+v", tc.what, edited, after)
		}
	}

	// 5. Mutation: with the guard stripped, m166 overwrites the copy the
	// person wrote (the limits edit left by step 4).
	mutated := stripOnce(t, m166Body(t), m166NeedleGuard)
	if code := m162Apply(t, w, mutated); code != "" {
		t.Fatalf("the mutated m166 failed with %s; the mutation did not run", code)
	}
	if r := m162Row(t, w); r.Description != m166Description || m166LimitsOf(t, r.Limits)["max_images"] != float64(20) {
		t.Fatalf("mutation did not fire: without the guard, m166 should overwrite the person's limits, "+
			"limits are %s; the guard is not proven load-bearing", r.Limits)
	}

	// 6. A row a person only disabled takes the new copy, stays disabled,
	// and is stamped again.
	m162Landed(t, w)
	m162SuperadminWrite(t, w, func(r *sqlc.AbilityCatalogue) { r.Enabled = false })
	disabled := m162Row(t, w)
	if disabled.Enabled || !disabled.UpdatedByUserID.Valid || disabled.Description != m162Description {
		t.Fatalf("INDETERMINATE: the kill switch did not land as a superadmin write: %+v", disabled)
	}
	if code := m162Apply(t, w, m166Body(t)); code != "" {
		t.Fatalf("m166 over a disabled, otherwise m162 row failed with %s", code)
	}
	r := m162Row(t, w)
	m166AssertCopy(t, r, false)
	if r.EntrySha256 != nil {
		t.Fatalf("m166 left the disabled row's hash set: %s", *r.EntrySha256)
	}
	m162Stamp(t, w)
	m162AssertStamped(t, m162Row(t, w))
}
