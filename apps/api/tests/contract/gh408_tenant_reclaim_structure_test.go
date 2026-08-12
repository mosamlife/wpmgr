package contract

// gh408_tenant_reclaim_structure_test.go: the structural invariants of the
// GH #408 tenant drain, in the cheap CI lane.
//
// m116's header and internal/backup/tenant_reclaim_worker.go both state that
// backup.TenantReclaimKinds is the code-side copy of the
// tenant_object_reclaim_kind_check constraint and that "tests/contract compares
// the two so neither half can move alone". Until this file existed, nothing did:
// the claim was made twice in shipped comments and asserted nowhere.
//
// Everything else about this table is proved against real Postgres in
// apps/api/tests/gh408_tenant_object_reclaim_integration_test.go, which is
// excluded from CI and manual-dispatch only. The two properties below are the
// ones that silently reinstate the bug and cost nothing to check on every pull
// request: a kind set that has drifted, and a foreign key.

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
)

const gh408TenantReclaimMigration = "migrations/20260818000000_m116_tenant_object_reclaim.sql"

// gh408CreateTable anchors on the CREATE TABLE at the start of a line, in both
// dialects this repository writes: bare in db/schema.sql, and quoted,
// schema-qualified and IF NOT EXISTS inside a DO block in the migrations. The
// anchor is what excludes comments, and it is not decoration: the site version
// of this guard read four lines of prose for a while and passed with two
// cascading foreign keys planted in the real table.
func gh408CreateTable(table string) *regexp.Regexp {
	return regexp.MustCompile(
		`(?im)^[ \t]*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:"?public"?\s*\.\s*)?"?` +
			table + `"?\s*\(`)
}

func gh408ExtractTableBlock(src, table string) (string, error) {
	loc := gh408CreateTable(table).FindStringIndex(src)
	if loc == nil {
		return "", errors.New("no `CREATE TABLE " + table + " (` at the start of any line")
	}
	body, ok := balancedParenBody(src[loc[1]-1:]) // loc[1]-1 is the "("
	if !ok {
		return "", errors.New("the column list has no matching close paren")
	}
	return stripSQLLineComments(body), nil
}

// gh408TableBlock is fatal when it finds nothing, never a silent empty string:
// a guard that passes because it read nothing is the failure mode being removed.
func gh408TableBlock(t *testing.T, rel, table string) string {
	t.Helper()
	block, err := gh408ExtractTableBlock(readRepoFile(t, rel), table)
	if err != nil {
		t.Fatalf("%s: could not read the %s CREATE TABLE: %v", rel, table, err)
	}
	return block
}

var gh408KindCheckClause = regexp.MustCompile(
	`(?is)CONSTRAINT\s+"?tenant_object_reclaim_kind_check"?\s+CHECK\s*\(\s*"?kind"?\s+IN\s*\(([^)]*)\)`)

func gh408KindCheckKinds(src string) []string {
	m := gh408KindCheckClause.FindStringSubmatch(src)
	if m == nil {
		return nil
	}
	var out []string
	for _, raw := range strings.Split(m[1], ",") {
		if v := strings.Trim(strings.TrimSpace(raw), "'"); v != "" {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// TestGH408_TenantReclaimKindIsAClosedSetInTheDatabase holds the two copies of
// the kind set together.
//
// A kind in the code alone cannot be inserted. A kind in the database alone is
// accepted and then stranded: the worker refuses to derive a prefix for it, so
// the task reclaims nothing while being the only record naming those objects,
// which is the original defect delivered by its own remedy.
func TestGH408_TenantReclaimKindIsAClosedSetInTheDatabase(t *testing.T) {
	want := append([]string(nil), backup.TenantReclaimKinds...)
	sort.Strings(want)
	if len(want) == 0 {
		t.Fatal("backup.TenantReclaimKinds is empty; the drain accepts nothing")
	}

	for _, rel := range []string{"db/schema.sql", gh408TenantReclaimMigration} {
		block := gh408TableBlock(t, rel, "tenant_object_reclaim")
		got := gh408KindCheckKinds(block)
		if got == nil {
			t.Errorf("%s: tenant_object_reclaim.kind has no tenant_object_reclaim_kind_check "+
				"constraint, so kind is free text and a mistyped backfill is accepted by the "+
				"database and then refused by the drain", rel)
			continue
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: the kind CHECK allows %v but backup.TenantReclaimKinds is %v. A kind in "+
				"only one of the two is either uninsertable or unreclaimable", rel, got, want)
		}
	}

	// And the worker must enforce the same set itself, rather than the constraint
	// being the only thing that does.
	if _, err := backup.TenantObjectPrefixes("tenant_storages", uuid.New()); err == nil {
		t.Error("backup.TenantObjectPrefixes accepted an unknown kind")
	}
	for _, k := range backup.TenantReclaimKinds {
		if _, err := backup.TenantObjectPrefixes(k, uuid.New()); err != nil {
			t.Errorf("backup.TenantObjectPrefixes refuses %q, which the database accepts: %v", k, err)
		}
	}
	// A nil tenant id renders as all-zeroes and yields prefixes that are
	// syntactically valid and semantically wrong, one line before an irreversible
	// delete.
	if _, err := backup.TenantObjectPrefixes(backup.TenantReclaimKindStorage, uuid.Nil); err == nil {
		t.Error("backup.TenantObjectPrefixes derived roots from a nil tenant id")
	}
}

// TestGH408_TenantReclaimTableHasNoForeignKeys is the m113 lesson one level up.
//
// A tenant_id foreign key with ON DELETE CASCADE would destroy this row in the
// exact statement it exists to survive, and that is not hypothetical: m113's
// first version shipped it and adversarial review proved it reinstated the bug.
// The container-lane test asserts the same thing against pg_constraint, but that
// lane is manual-dispatch only, so a foreign key added in a later migration
// would merge green without this.
func TestGH408_TenantReclaimTableHasNoForeignKeys(t *testing.T) {
	for _, rel := range []string{"db/schema.sql", gh408TenantReclaimMigration} {
		block := gh408TableBlock(t, rel, "tenant_object_reclaim")
		var named bool
		if referencesTenants.MatchString(block) {
			named = true
			t.Errorf("%s: tenant_object_reclaim has a foreign key to tenants. That cascade deletes "+
				"the reclaim record in the very statement that creates the orphaned objects, which "+
				"is GH #408 reinstated exactly", rel)
		}
		if m := anyForeignKey.FindString(block); m != "" && !named {
			t.Errorf("%s: tenant_object_reclaim declares a foreign key (%q). This table is "+
				"deliberately parentless: a record of what to clean up has to outlive the deletion "+
				"it describes", rel, m)
		}
	}
}
