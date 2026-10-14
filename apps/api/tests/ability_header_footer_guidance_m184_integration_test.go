// m184 changes only the recognised catalogue guidance, clears the changed
// entry hashes for the boot stamp, and preserves operator copy on reruns.
// Reads and stamps use the app's transaction helpers; migration bodies run
// as the non-superuser migrator, matching production.
package tests

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

const m184MigrationVersion = "20261014200000_m184_header_footer_guidance"

const (
	m184CreateDescription = m177Description + " With template header or footer instead of post_type it makes an Elementor Pro header or footer draft; read wpmgr/elementor-theme-templates first."
	m184EditUsage         = m178Usage + " set_native, add_native and duplicate change Elementor widgets: read wpmgr/elementor-widget-schema and follow its write_rules, and send them apart from text or outline changes. A WPMgr header or footer draft is changed the same way; set_shows_on {rules} replaces where it shows, and without it the places stay."
)

func m184Body(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(migrations.FS, m184MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m184 migration body: %v", err)
	}
	return string(b)
}

// Read only complete literal expressions, including SQL's escaped quotes.
// Missing or ambiguous copy is a failure, never an empty successful match.
func m184LiteralCopy(t *testing.T, src, prefix string) string {
	t.Helper()
	var code strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			code.WriteString(line + "\n")
		}
	}
	src = code.String()
	literal := `'(?:[^']|'')*'`
	expressions := regexp.MustCompile(literal + `(?:\s*\|\|\s*` + literal + `)*`)
	quotes := regexp.MustCompile(literal)
	var matches []string
	for _, expression := range expressions.FindAllString(src, -1) {
		var value strings.Builder
		for _, part := range quotes.FindAllString(expression, -1) {
			value.WriteString(strings.ReplaceAll(part[1:len(part)-1], "''", "'"))
		}
		if strings.HasPrefix(value.String(), prefix) {
			matches = append(matches, value.String())
		}
	}
	if len(matches) != 1 {
		t.Fatalf("want exactly one copy expression beginning %q, found %d", prefix, len(matches))
	}
	return matches[0]
}

// The catalogue mirror is seed data, outside the schema catalog comparison.
// This test checks exact copy and bounds without needing a database.
func TestM184GuidanceTextAndSchemaMatches(t *testing.T) {
	body := m184Body(t)
	schema, err := os.ReadFile("../db/schema.sql")
	if err != nil {
		t.Fatalf("read schema mirror: %v", err)
	}
	for _, tc := range []struct {
		declaration, want string
		limit             int
	}{
		{"v_m177_description", m177Description, 1000},
		{"v_description", m184CreateDescription, 1000},
		{"v_m178_usage", m178Usage, 2000},
		{"v_edit_usage", m184EditUsage, 2000},
	} {
		t.Run(tc.declaration, func(t *testing.T) {
			declaration := regexp.MustCompile(`(?s)\b` + tc.declaration + `\s+constant\s+text\s*:=\s*('(?:[^']|'')*'(?:\s*\|\|\s*'(?:[^']|'')*')*)\s*;`)
			parts := declaration.FindStringSubmatch(body)
			if parts == nil {
				t.Fatalf("missing literal declaration %s", tc.declaration)
			}
			if got := m184LiteralCopy(t, parts[1], tc.want[:30]); got != tc.want {
				t.Fatalf("%s copy = %q\nwant %q", tc.declaration, got, tc.want)
			}
			if n := len([]rune(tc.want)); n > tc.limit {
				t.Fatalf("%s is %d characters, limit %d", tc.declaration, n, tc.limit)
			}
		})
	}
	for _, want := range []string{m184CreateDescription, m184EditUsage, m177Usage} {
		if got := m184LiteralCopy(t, string(schema), want[:30]); got != want {
			t.Fatalf("schema copy = %q\nwant %q", got, want)
		}
	}
}

func m184PreviousCopy(t *testing.T, w *e2World) map[string]sqlc.AbilityCatalogue {
	t.Helper()
	tag, err := w.admin.Exec(context.Background(), `UPDATE ability_catalogue
		SET description = $1, entry_sha256 = NULL, updated_by_user_id = NULL
		WHERE name = 'wpmgr/page-create' AND source = 'wpmgr'`, m177Description)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("SETUP: restore m177 description: rows=%d err=%v", tag.RowsAffected(), err)
	}
	m178SetUsage(t, w, m178Usage)
	return m177M178Catalogue(t, w)
}

func m184SameOutsideGuidance(t *testing.T, name string, a, b sqlc.AbilityCatalogue) {
	t.Helper()
	for _, row := range []*sqlc.AbilityCatalogue{&a, &b} {
		if name == "wpmgr/page-create" {
			row.Description = ""
		} else {
			row.Usage = nil
		}
		row.EntrySha256, row.UpdatedAt = nil, time.Time{}
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("%s: m184 changed another column:\nbefore %+v\nafter %+v", name, a, b)
	}
}

func TestHeaderFooterGuidanceM184AsAppRole(t *testing.T) {
	w := newE2World(t, true)
	body := m184Body(t)

	fresh := m177M178Catalogue(t, w)
	m177AssertCopy(t, "fresh install", fresh["wpmgr/page-create"], m184CreateDescription, m177Usage)
	m176AssertUsage(t, fresh["wpmgr/page-edit"], m184EditUsage)
	m162AssertStamped(t, fresh["wpmgr/page-create"])
	m178AssertStamped(t, fresh["wpmgr/page-edit"])

	before := m184PreviousCopy(t, w)
	if code, warnings := m176Apply(t, w, body); code != "" || len(warnings) != 0 {
		t.Fatalf("m184 on predecessor copy: code %q, warnings %q", code, warnings)
	}
	mid := m177M178Catalogue(t, w)
	m177AssertCopy(t, "updated predecessor", mid["wpmgr/page-create"], m184CreateDescription, m177Usage)
	m176AssertUsage(t, mid["wpmgr/page-edit"], m184EditUsage)
	for name, old := range before {
		row := mid[name]
		if name != "wpmgr/page-create" && name != "wpmgr/page-edit" {
			if !reflect.DeepEqual(old, row) {
				t.Fatalf("m184 changed unrelated row %s", name)
			}
			continue
		}
		if old.EntrySha256 == nil || row.EntrySha256 != nil {
			t.Fatalf("%s: m184 must clear a stamped hash: before %v after %v", name, old.EntrySha256, row.EntrySha256)
		}
		if !row.UpdatedAt.After(old.UpdatedAt) {
			t.Fatalf("%s: m184 did not advance updated_at", name)
		}
		m184SameOutsideGuidance(t, name, old, row)
	}
	m162Stamp(t, w)
	stamped := m177M178Catalogue(t, w)
	for _, name := range m177M178Names {
		sum := m162AssertStamped(t, stamped[name])
		if sum == *before[name].EntrySha256 {
			t.Fatalf("%s: new guidance did not change the canonical entry hash", name)
		}
	}
	if code, warnings := m176Apply(t, w, body); code != "" || len(warnings) != 0 {
		t.Fatalf("rerun m184: code %q, warnings %q", code, warnings)
	}
	if again := m177M178Catalogue(t, w); !reflect.DeepEqual(stamped, again) {
		t.Fatal("rerun m184 changed the catalogue, timestamp or stamped hash")
	}

	// One operator-owned row does not prevent the other row from converging.
	for _, name := range m177M178Names {
		t.Run("operator_"+name, func(t *testing.T) {
			m184PreviousCopy(t, w)
			column := "description"
			if name == "wpmgr/page-edit" {
				column = "usage"
			}
			tag, err := w.admin.Exec(context.Background(), `UPDATE ability_catalogue SET `+column+` = $1,
				entry_sha256 = NULL WHERE name = $2 AND source = 'wpmgr'`, "Our own draft guidance.", name)
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("SETUP: operator copy: rows=%d err=%v", tag.RowsAffected(), err)
			}
			m162Stamp(t, w)
			edited := m177M178Catalogue(t, w)[name]
			code, warnings := m176Apply(t, w, body)
			wantWarning := fmt.Sprintf("m184: the %s of %s was changed by a person; leaving it as it is", column, name)
			if code != "" || !reflect.DeepEqual(warnings, []string{wantWarning}) {
				t.Fatalf("operator copy: code %q, warnings %q, want %q", code, warnings, wantWarning)
			}
			after := m177M178Catalogue(t, w)
			if !reflect.DeepEqual(edited, after[name]) {
				t.Fatalf("m184 changed the operator's row %s", name)
			}
			other := "wpmgr/page-create"
			if name == other {
				other = "wpmgr/page-edit"
				m176AssertUsage(t, after[other], m184EditUsage)
			} else {
				m177AssertCopy(t, "other row", after[other], m184CreateDescription, m177Usage)
			}
			if after[other].EntrySha256 != nil {
				t.Fatalf("m184 did not clear the other row's hash: %s", other)
			}
		})
	}

	// Missing rows warn independently; each probe restores its deletion.
	m184PreviousCopy(t, w)
	if code, warnings := m176Apply(t, w, body); code != "" || len(warnings) != 0 {
		t.Fatalf("SETUP: restore m184 copy: code %q, warnings %q", code, warnings)
	}
	m162Stamp(t, w)
	complete := m177M178Catalogue(t, w)
	for _, name := range m177M178Names {
		t.Run("missing_"+name, func(t *testing.T) {
			code, warnings := m176Apply(t, w, "SAVEPOINT m184_missing;\nDELETE FROM ability_catalogue WHERE name = '"+name+"';\n"+body+"\nROLLBACK TO SAVEPOINT m184_missing;")
			wantWarning := fmt.Sprintf("m184: expected exactly one wpmgr catalogue row named %s, found 0; its copy was not changed", name)
			if code != "" || !reflect.DeepEqual(warnings, []string{wantWarning}) {
				t.Fatalf("missing row: code %q, warnings %q, want %q", code, warnings, wantWarning)
			}
			if after := m177M178Catalogue(t, w); !reflect.DeepEqual(complete, after) {
				t.Fatal("missing-row probe did not restore the catalogue")
			}
		})
	}
}
