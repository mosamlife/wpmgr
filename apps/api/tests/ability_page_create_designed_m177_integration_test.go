// m177 proofs: after every migration, wpmgr/page-create's description and
// usage are m177's, which describe designed pages (outline_version 3, design,
// page_frame, sections and patterns) and name the designed-pages floor,
// 0.61.165; the hash is the hash of the new bytes and describe hands the AI
// the new copy. A row on m166's description and m176's usage converges: only
// description and usage change, the hash is cleared, min_agent_version and
// limits stay, and the boot stamp re-stamps it. A re-run changes nothing. A
// usage a person rewrote is kept with a WARNING while the description still
// moves.
//
// Catalogue reads, describe and the stamp go through the production paths as
// wpmgr_app (the e2World harness asserts the role). Migration bodies are
// applied as wpmgr_owner, the NOSUPERUSER NOBYPASSRLS migrator.
package tests

import (
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

const m177MigrationVersion = "20261012000000_m177_page_create_designed_usage"

// The copy m177 writes, spelled out here rather than read from the migration
// so that a change to either side is a visible test change.
const (
	m177Description = "Creates a new draft page or post from an outline: headings, paragraphs, lists, quotes, tables, " +
		"separators, images already in the site's media library, and, in the block editor or Elementor, buttons, " +
		"spacing, sections and columns. With editor builder:elementor the draft is built in Elementor, from " +
		"Elementor's own layout elements and widgets. With outline_version 3 it is a designed page: sections " +
		"with tones, backgrounds and anchors, and patterns such as a hero, features, stats, pricing, " +
		"testimonials, questions and a call to action, styled from one design of colours, fonts and spacing. " +
		"Nothing is published. Undo moves the draft to the trash."

	m177Usage = "Build the page as an outline. A top-level item can be any block, a group (a section) or columns. A " +
		"group holds blocks or columns; a column holds blocks only. Use 2 to 4 columns; widths are optional " +
		"whole percentages that add up to 100. Images must already be in the media library: find an attachment " +
		"id with wpmgr/rest-read route wp-v2-media-list, and write alt text that describes the picture, or an " +
		"empty alt for a decorative one. Button links are https:// addresses or paths on this site that start " +
		"with /. All text is plain: no HTML, shortcodes or template syntax; square brackets only around a number " +
		"such as [1], and never in alt text. On a site that uses the classic editor, send editor " +
		"wordpress_classic and only headings, paragraphs, lists, quotes, tables, separators and images without " +
		"captions. Layout blocks need the WPMgr plugin 0.61.160 or later on the site. With Elementor, send " +
		"editor builder:elementor: the draft uses Elementor's classic widgets, and elementor_format atomic is " +
		"always refused. In Elementor, a button link cannot contain &, a paragraph cannot be only a web address, " +
		"an image's align can only be none or center, and its alt text must be its media library alt text. " +
		"Elementor pages need the WPMgr plugin 0.61.161 or later and Elementor 3.20 to 4.3 on the site. For a " +
		"designed page, send outline_version 3 with a complete design. Colours are never #000000; ink on ground " +
		"and on surface, and on_accent on accent, need 4.5:1 contrast. Fonts are ones Elementor offers, or " +
		"system. page_frame is full_width, title hidden, when there is a hero, else theme; canvas only on " +
		"request. Use up to 20 sections, an anchor on each and #anchor links to them, one hero holding the only " +
		"level 1 heading, patterns for variety and one accent. Style by tokens, never HTML or CSS. Colours and " +
		"fonts go on the page, not the kit, so Undo covers all. Designed pages need the WPMgr plugin 0.61.165 or " +
		"later and Elementor containers; version 2 buttons cannot use the outline style."

	m177HumanUsage = "Our own page-building instructions, designed pages included."

	m177WarningPersonUsage = "m177: the usage of wpmgr/page-create was changed by a person; leaving it as it is"
)

func m177Body(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(migrations.FS, m177MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m177 migration body: %v", err)
	}
	return string(b)
}

// m176Landed puts page-create where m176 leaves it on an install that has not
// run m177: m166Landed, then m176's own body as the migrator, then the boot
// stamp. It returns the stamped hash.
func m176Landed(t *testing.T, w *e2World) string {
	t.Helper()
	m166Landed(t, w)
	if code, warnings := m176Apply(t, w, m176Body(t)); code != "" || len(warnings) != 0 {
		t.Fatalf("SETUP: m176 on m166's row: code %q, warnings %q", code, warnings)
	}
	m162Stamp(t, w)
	r := m162Row(t, w)
	m176AssertUsage(t, r, m176Usage)
	if r.Description != m166Description {
		t.Fatalf("SETUP: m176's row does not carry m166's description: %q", r.Description)
	}
	return m162AssertStamped(t, r)
}

// m177SameOutsideCopy fails unless a and b agree on every column m177 must
// not write: everything but description, usage, entry_sha256 and updated_at.
func m177SameOutsideCopy(t *testing.T, what string, a, b sqlc.AbilityCatalogue) {
	t.Helper()
	for _, r := range []*sqlc.AbilityCatalogue{&a, &b} {
		r.Description, r.Usage, r.EntrySha256, r.UpdatedAt = "", nil, nil, time.Time{}
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("%s: m177 moved a column other than description and usage:\nbefore %+v\nafter  %+v", what, a, b)
	}
}

func m177AssertCopy(t *testing.T, what string, r sqlc.AbilityCatalogue, wantDescription, wantUsage string) {
	t.Helper()
	if r.Description != wantDescription {
		t.Fatalf("%s: description = %q\nwant:\n%q", what, r.Description, wantDescription)
	}
	m176AssertUsage(t, r, wantUsage)
}

// TestPageCreateDesignedUsageM177AsAppRole runs every m177 proof on one
// database.
func TestPageCreateDesignedUsageM177AsAppRole(t *testing.T) {
	w := newE2World(t, true)

	// 0. The text itself: it names outline version 3, design, page_frame and
	// the designed-pages floor, and fits the usage column's 2000 characters.
	for _, want := range []string{"outline_version 3", "design", "page_frame", "0.61.165 or later"} {
		if !strings.Contains(m177Usage, want) {
			t.Fatalf("m177's usage does not name %q", want)
		}
	}
	if n := len([]rune(m177Usage)); n > 2000 {
		t.Fatalf("m177's usage is %d characters; ability_catalogue_usage_check admits 2000", n)
	}

	// 1. A fresh install: every migration, then the boot stamp. The row has
	// m177's copy, m166's limits and the text-only floor, its hash is the hash
	// of its bytes, and describe hands the AI the new copy.
	fresh := m162Row(t, w)
	m177AssertCopy(t, "a fresh install", fresh, m177Description, m177Usage)
	if !reflect.DeepEqual(m166LimitsOf(t, fresh.Limits), m166LimitsOf(t, []byte(m166Limits))) {
		t.Fatalf("a fresh install's limits are not m166's: %s", fresh.Limits)
	}
	if fresh.MinAgentVersion == nil || *fresh.MinAgentVersion != "0.61.156" {
		t.Fatalf("min_agent_version = %v, want the text-only floor 0.61.156 unchanged", fresh.MinAgentVersion)
	}
	m162AssertStamped(t, fresh)
	res := cpeCall(t, w.eng, w.bearer, mcp.ToolSiteAbilityDescribe, map[string]any{
		"site_id": w.site.String(), "name": mcp.AbilityPageCreate,
	}).wantOK(t, "describe page-create")
	wp, ok := res["wpmgr"].(map[string]any)
	if !ok {
		t.Fatalf("describe has no wpmgr block: %v", res)
	}
	if wp["description"] != m177Description || wp["usage"] != m177Usage {
		t.Fatalf("describe copy: description=%v usage=%v", wp["description"], wp["usage"])
	}

	// 2. An install on m176: m177 writes description and usage, clears the
	// hash and touches nothing else; the stamp then fills the hash from the
	// new bytes.
	old := m176Landed(t, w)
	before := m162Row(t, w)
	code, warnings := m176Apply(t, w, m177Body(t))
	if code != "" || len(warnings) != 0 {
		t.Fatalf("m177 on m176's row: code %q, warnings %q; want neither", code, warnings)
	}
	mid := m162Row(t, w)
	m177AssertCopy(t, "m176's row", mid, m177Description, m177Usage)
	if mid.EntrySha256 != nil {
		t.Fatalf("m177 left entry_sha256 = %s; the boot stamp cannot re-fill a set hash", *mid.EntrySha256)
	}
	m177SameOutsideCopy(t, "m176's row", before, mid)
	m162Stamp(t, w)
	stamped := m162Row(t, w)
	if sum := m162AssertStamped(t, stamped); sum == old {
		t.Fatalf("the re-stamped hash equals m176's (%s); the new copy did not reach the entry bytes", old)
	}

	// 3. A second run changes nothing: not the copy, not the hash, not
	// updated_at, so it cannot close an approval a second time.
	code, warnings = m176Apply(t, w, m177Body(t))
	if code != "" || len(warnings) != 0 {
		t.Fatalf("a second m177: code %q, warnings %q; want neither", code, warnings)
	}
	again := m162Row(t, w)
	if !reflect.DeepEqual(stamped.EntrySha256, again.EntrySha256) || !stamped.UpdatedAt.Equal(again.UpdatedAt) {
		t.Fatalf("a second m177 changed the row:\nbefore %+v\nafter  %+v", stamped, again)
	}
	m177AssertCopy(t, "a second m177", again, m177Description, m177Usage)
	m177SameOutsideCopy(t, "a second m177", stamped, again)

	// 4. A usage a person rewrote: m177 warns, keeps the person's usage, still
	// moves the description it recognises, and clears the hash it changed.
	m176Landed(t, w)
	m162SuperadminWrite(t, w, func(r *sqlc.AbilityCatalogue) {
		s := m177HumanUsage
		r.Usage = &s
	})
	edited := m162Row(t, w)
	if edited.Usage == nil || *edited.Usage != m177HumanUsage || edited.EntrySha256 == nil {
		t.Fatalf("INDETERMINATE: the superadmin usage edit did not land: %+v", edited)
	}
	code, warnings = m176Apply(t, w, m177Body(t))
	if code != "" {
		t.Fatalf("m177 over a person's usage failed with %s; it must warn and carry on", code)
	}
	if len(warnings) != 1 || warnings[0] != m177WarningPersonUsage {
		t.Fatalf("m177 over a person's usage raised warnings %q, want exactly %q", warnings, m177WarningPersonUsage)
	}
	r := m162Row(t, w)
	m177AssertCopy(t, "a person's usage", r, m177Description, m177HumanUsage)
	if r.EntrySha256 != nil {
		t.Fatalf("m177 changed the description but left the hash set: %s", *r.EntrySha256)
	}
	m177SameOutsideCopy(t, "a person's usage", edited, r)
	m162Stamp(t, w)
	m162AssertStamped(t, m162Row(t, w))
}
