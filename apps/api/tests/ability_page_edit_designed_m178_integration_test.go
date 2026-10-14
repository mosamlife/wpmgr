// m178 proofs: wpmgr/page-edit's usage after m178 names pattern paths
// (plans.1.price), version 3 insert and replace with the design inherited or
// sent, the designed-pages floor and
// anchor_in_use; describe hands the AI the new usage. A row on m169's usage
// converges: only usage changes, the hash is cleared and the boot stamp
// re-stamps it. A re-run changes nothing. A usage a person rewrote is kept
// with a WARNING.
//
// Catalogue reads and the stamp go through the production paths as wpmgr_app
// (the e2World harness asserts the role). Migration bodies are applied as
// wpmgr_owner, the NOSUPERUSER NOBYPASSRLS migrator.
package tests

import (
	"context"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilities"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

const m178MigrationVersion = "20261012010000_m178_page_edit_designed_usage"

// The usage m178 writes, spelled out so that a change to either side is a
// visible test change.
const (
	m178Usage = "First read the page with wpmgr/page-structure. Send post_id, base_fingerprint (from that read) and " +
		"operations: 1 to 25 changes, applied in order. set_text sets one field (text, url, alt or caption) of a " +
		"node whose editable list names it; in a pattern, name the field by its path, such as plans.1.price or " +
		"faq.3.answer. insert adds outline items after or before a node, or into a node, first or last. replace " +
		"puts outline items in place of a node; to add or remove items in a pattern, replace the pattern. remove " +
		"deletes a node. move puts a node after or before another. Outline items use the wpmgr/page-create " +
		"outline, at most 50 in one change, and all text follows its rules: plain text only, and links that are " +
		"https:// addresses or paths on this site that start with /. On a designed page, insert and replace also " +
		"take version 3 sections and patterns, built with the design the page was created with unless you send " +
		"design, complete, as wpmgr/page-create takes it; they need the WPMgr plugin 0.61.165 or later. A " +
		"#anchor link must name an anchor on the page after the whole request (link_anchor_missing), and a " +
		"section whose anchor is still linked cannot be removed (anchor_in_use). Change each node at most once " +
		"in a request, and never name a node an earlier change in the same request removed or replaced. Locked " +
		"nodes cannot be changed, moved or removed. If the page changed after your read, the request is refused " +
		"with conflict: read the page again and send new changes. Only a draft WPMgr created with " +
		"wpmgr/page-create, still a draft, can be changed; any other page is refused with target_not_eligible."

	m178HumanUsage = "Our own instructions for changing builder drafts."

	m178WarningPersonUsage = "m178: the usage of wpmgr/page-edit was changed by a person; leaving it as it is"
)

func m178Body(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(migrations.FS, m178MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m178 migration body: %v", err)
	}
	return string(b)
}

// m178Row reads wpmgr/page-edit as wpmgr_app through the generated query.
func m178Row(t *testing.T, w *e2World) sqlc.AbilityCatalogue {
	t.Helper()
	var row sqlc.AbilityCatalogue
	if err := w.pool.InTenantTx(context.Background(), w.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (m178 read)")
		row = m157Entry(t, tx, "wpmgr/page-edit")
		return nil
	}); err != nil {
		t.Fatalf("read page-edit: %v", err)
	}
	return row
}

// m178SetUsage puts usage on the page-edit row with no hash, as the migrator
// sees an install that has not run m178, then runs the boot stamp.
func m178SetUsage(t *testing.T, w *e2World, usage string) sqlc.AbilityCatalogue {
	t.Helper()
	tag, err := w.admin.Exec(context.Background(), `UPDATE ability_catalogue
		SET usage = $1, entry_sha256 = NULL, updated_by_user_id = NULL
		WHERE name = 'wpmgr/page-edit' AND source = 'wpmgr'`, usage)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("SETUP: put page-edit on m169's usage: rows=%d err=%v", tag.RowsAffected(), err)
	}
	m162Stamp(t, w)
	r := m178Row(t, w)
	m178AssertStamped(t, r)
	return r
}

func m178AssertStamped(t *testing.T, r sqlc.AbilityCatalogue) string {
	t.Helper()
	_, sum, err := abilities.SendableEntry(r)
	if err != nil {
		t.Fatalf("page-edit is not sendable after the stamp: %v", err)
	}
	if r.EntrySha256 == nil || *r.EntrySha256 != sum {
		t.Fatalf("entry_sha256 = %v, want %s", r.EntrySha256, sum)
	}
	return sum
}

// m178SameOutsideUsage fails unless a and b agree on every column m178 must
// not write: everything but usage, entry_sha256 and updated_at.
func m178SameOutsideUsage(t *testing.T, what string, a, b sqlc.AbilityCatalogue) {
	t.Helper()
	for _, r := range []*sqlc.AbilityCatalogue{&a, &b} {
		r.Usage, r.EntrySha256, r.UpdatedAt = nil, nil, time.Time{}
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("%s: m178 moved a column other than usage:\nbefore %+v\nafter  %+v", what, a, b)
	}
}

// TestPageEditDesignedUsageM178AsAppRole runs every m178 proof on one
// database.
func TestPageEditDesignedUsageM178AsAppRole(t *testing.T) {
	w := newE2World(t, true)

	for _, want := range []string{"plans.1.price", "version 3", "design", "anchor_in_use", "0.61.165 or later"} {
		if !strings.Contains(m178Usage, want) {
			t.Fatalf("m178's usage does not name %q", want)
		}
	}
	if n := len([]rune(m178Usage)); n > 2000 {
		t.Fatalf("m178's usage is %d characters; ability_catalogue_usage_check admits 2000", n)
	}

	// 1. A fresh install: every migration, then the boot stamp. The row has
	// m184's usage over m169's description, limits and floor.
	m162Stamp(t, w)
	fresh := m178Row(t, w)
	m176AssertUsage(t, fresh, m184EditUsage)
	if fresh.Description != m169EditDescription || fresh.Title != m169EditTitle ||
		fresh.MinAgentVersion == nil || *fresh.MinAgentVersion != "0.61.163" {
		t.Fatalf("a fresh install's page-edit copy or floor is not m169's: %+v", fresh)
	}
	m178AssertStamped(t, fresh)

	// 2. An install on m169: m178 writes the usage, clears the hash and
	// touches nothing else; the stamp then fills the hash from the new bytes.
	before := m178SetUsage(t, w, m169EditUsage)
	code, warnings := m176Apply(t, w, m178Body(t))
	if code != "" || len(warnings) != 0 {
		t.Fatalf("m178 on m169's row: code %q, warnings %q; want neither", code, warnings)
	}
	mid := m178Row(t, w)
	m176AssertUsage(t, mid, m178Usage)
	if mid.EntrySha256 != nil {
		t.Fatalf("m178 left entry_sha256 = %s; the boot stamp cannot re-fill a set hash", *mid.EntrySha256)
	}
	m178SameOutsideUsage(t, "m169's row", before, mid)
	m162Stamp(t, w)
	stamped := m178Row(t, w)
	if sum := m178AssertStamped(t, stamped); before.EntrySha256 != nil && sum == *before.EntrySha256 {
		t.Fatalf("the re-stamped hash equals m169's (%s); the new usage did not reach the entry bytes", sum)
	}

	// 3. A second run changes nothing.
	code, warnings = m176Apply(t, w, m178Body(t))
	if code != "" || len(warnings) != 0 {
		t.Fatalf("a second m178: code %q, warnings %q; want neither", code, warnings)
	}
	again := m178Row(t, w)
	if !reflect.DeepEqual(stamped, again) {
		t.Fatalf("a second m178 changed the row:\nbefore %+v\nafter  %+v", stamped, again)
	}

	// 4. A usage a person rewrote: m178 warns, does not stop, and leaves the
	// row exactly as it was.
	edited := m178SetUsage(t, w, m178HumanUsage)
	code, warnings = m176Apply(t, w, m178Body(t))
	if code != "" {
		t.Fatalf("m178 over a person's usage failed with %s; it must warn and carry on", code)
	}
	if len(warnings) != 1 || warnings[0] != m178WarningPersonUsage {
		t.Fatalf("m178 over a person's usage raised warnings %q, want exactly %q", warnings, m178WarningPersonUsage)
	}
	if after := m178Row(t, w); !reflect.DeepEqual(edited, after) {
		t.Fatalf("m178 over a person's usage changed the row:\nbefore %+v\nafter  %+v", edited, after)
	}
}
