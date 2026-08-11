package contract

// gh402_site_delete_reclaim_structure_test.go: the structural invariants of
// the GH #402 fix, in the cheap CI lane.
//
// The behavioural proofs live in internal/backup (fakes) and apps/api/tests
// (real Postgres). What is left over are three properties that are neither: a
// duplicated constant, a foreign key that must NOT exist, and the fact that one
// statement sits inside one transaction with another. Each of them is invisible
// to an ordinary test and each of them silently reintroduces the bug if a later
// refactor gets it wrong, so they are asserted here, against the source itself,
// in the lane every pull request pays for.

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/backup"
)

// repoFile resolves a path relative to apps/api (this package sits at
// apps/api/tests/contract).
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join("..", "..", rel)
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(repoFile(t, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// 1. The reclaim kind literal is duplicated on purpose. It must not drift.
// ---------------------------------------------------------------------------

func TestGH402_ReclaimKindConstantsAgree(t *testing.T) {
	// internal/site cannot import internal/backup for one string without
	// coupling the two domains, so it holds an unexported copy. This test is
	// the thing that stops the copy drifting: a mismatch would make DELETE
	// /sites/{id} write tasks the worker never picks up, which fails silently
	// and looks exactly like the original bug.
	src := readRepoFile(t, "internal/site/repo.go")
	re := regexp.MustCompile(`backupManifestReclaimKind\s*=\s*"([^"]+)"`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatal("internal/site/repo.go no longer declares backupManifestReclaimKind")
	}
	if m[1] != backup.ReclaimKindBackupManifest {
		t.Fatalf("site kind %q does not match backup.ReclaimKindBackupManifest %q",
			m[1], backup.ReclaimKindBackupManifest)
	}

	// The database default must agree too: the column defaults to this value,
	// and a task inserted without an explicit kind has to be one the worker
	// recognises.
	schema := readRepoFile(t, "db/schema.sql")
	block := siteObjectReclaimBlock(t, schema)
	want := `kind             text        NOT NULL DEFAULT '` + backup.ReclaimKindBackupManifest + `'`
	if !strings.Contains(block, want) {
		t.Errorf("db/schema.sql site_object_reclaim.kind default does not match %q",
			backup.ReclaimKindBackupManifest)
	}
}

// ---------------------------------------------------------------------------
// 1b. kind is a CLOSED set, in the database, and both halves of it move
// together.
//
// The worker can derive a storage prefix for exactly the kinds it knows, and a
// task carrying any other value reclaims nothing while being the only record
// that those objects exist. Rows do not only arrive from DELETE /sites/{id},
// which writes a code constant: the m113 header and the CHANGELOG both instruct
// an operator to hand a known-deleted site to the sweep with a hand-written
// INSERT, because that is the only route to the objects orphaned before m113.
// A typo there used to be accepted by the database and then refused by the
// worker, which is the original defect delivered by the remedy for it.
//
// So the set is written down three times, and this test is what stops the
// copies drifting: backup.ReclaimKinds in code, the CHECK constraint in
// db/schema.sql and m113 for fresh databases, and m115 for databases that
// already have the table. A kind added to the code alone cannot be inserted; a
// kind added to the schema alone is accepted and then stranded.
// ---------------------------------------------------------------------------

const gh402KindCheckMigration = "migrations/20260817000000_m115_site_object_reclaim_kind_check.sql"

// kindCheckKinds pulls the quoted values out of a
// CHECK (kind IN ('a', 'b')) clause, in either quoting dialect.
var kindCheckClause = regexp.MustCompile(
	`(?is)CONSTRAINT\s+"?site_object_reclaim_kind_check"?\s+CHECK\s*\(\s*"?kind"?\s+IN\s*\(([^)]*)\)`)

// stripSQLLineComments removes "--" line comments.
//
// Every migration here carries a long explanatory header that names the exact
// statements being asserted on, so an assertion about what a file DOES is
// otherwise satisfied by what its header SAYS. That is the same class of defect
// as the unanchored block extractor above, and it bit immediately: the first
// version of the "must not VALIDATE CONSTRAINT" check below went red against
// m115, whose header explains that it deliberately does not.
//
// A "--" inside a string literal would be truncated too. No migration here has
// one, and the alternative is a SQL lexer in a contract test.
func stripSQLLineComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func kindCheckKinds(t *testing.T, src string) []string {
	t.Helper()
	m := kindCheckClause.FindStringSubmatch(src)
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

func TestGH402_ReclaimKindIsAClosedSetInTheDatabase(t *testing.T) {
	want := append([]string(nil), backup.ReclaimKinds...)
	sort.Strings(want)
	if len(want) == 0 {
		t.Fatal("backup.ReclaimKinds is empty; the worker accepts nothing")
	}

	// The constraint is declared inline in the CREATE TABLE for a fresh
	// database, so it is read out of the table block, which is anchored.
	for _, rel := range []string{
		"db/schema.sql",
		"migrations/20260815000000_m113_site_object_reclaim.sql",
	} {
		block := siteObjectReclaimBlock(t, readRepoFile(t, rel))
		got := kindCheckKinds(t, block)
		if got == nil {
			t.Errorf("%s: site_object_reclaim.kind has no site_object_reclaim_kind_check "+
				"constraint. kind is then free text, and an operator backfill with a mistyped "+
				"kind is accepted by the database and refused by the worker, which strands the "+
				"objects it was written to reclaim (GH #402 through its own remedy)", rel)
			continue
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: the kind CHECK allows %v but backup.ReclaimKinds is %v. "+
				"A kind in only one of the two is either uninsertable or unreclaimable", rel, got, want)
		}
	}

	// And the same set has to reach a database that already has the table.
	// migrate.go never re-reads an applied version, so this cannot live in m113
	// or m114: both are applied on exactly the databases that need it.
	converge := stripSQLLineComments(readRepoFile(t, gh402KindCheckMigration))
	got := kindCheckKinds(t, converge)
	if got == nil {
		t.Fatalf("%s does not add site_object_reclaim_kind_check. A database that ran an earlier "+
			"m113 keeps a free-text kind column forever", gh402KindCheckMigration)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s allows %v but backup.ReclaimKinds is %v", gh402KindCheckMigration, got, want)
	}
	if !strings.Contains(converge, "NOT VALID") {
		t.Error(gh402KindCheckMigration + " adds the constraint without NOT VALID. ADD CONSTRAINT " +
			"validates every existing row, and migrate.go applies migrations on boot, so a " +
			"database already holding a bad row would fail to start. The row that fails " +
			"validation is also the one that must not be lost: it names objects nothing else names")
	}
	if strings.Contains(converge, "VALIDATE CONSTRAINT") {
		t.Error(gh402KindCheckMigration + " validates the constraint, which fails the boot " +
			"migration on exactly the databases that hold a bad row")
	}
	if !(gh402KindCheckMigration > gh402ConvergeMigration) {
		t.Error("the kind-check migration does not sort after m114; migrations apply in lexical " +
			"filename order")
	}

	// The worker must actually enforce the same set, rather than the constraint
	// being the only thing that does. Rows predating m115 exist.
	if _, err := backup.SiteObjectPrefix("backup_manifesto", uuid.New(), uuid.New()); err == nil {
		t.Error("backup.SiteObjectPrefix accepted an unknown kind")
	}
}

// ---------------------------------------------------------------------------
// 2. site_object_reclaim must NEVER gain a foreign key to EITHER parent.
//
// A site_id FK with ON DELETE CASCADE would destroy the row in the very
// statement it exists to survive, restoring the bug exactly.
//
// A tenant_id FK with ON DELETE CASCADE is the same mistake one level up, and
// the first version of m113 shipped it. admin_delete_empty_tenant (org delete
// Lane A, and the superadmin orphan cleanup) hard-deletes a tenant row while
// freeing ZERO object storage, so the cascade destroyed the reclaim record by
// exactly the operation that should have triggered it. Only Lane B, the
// grace-window org.PurgeWorker, sweeps the tenant object roots first.
//
// Both are the kind of thing a later "add the missing foreign key" pass does
// without malice, so both get an explicit test with the reason attached.
// ---------------------------------------------------------------------------

// sqlIdent is one SQL identifier as Postgres accepts it and as this repository
// writes it: double-quoted, or bare.
const sqlIdent = `(?:"[^"]+"|[A-Za-z_][A-Za-z_0-9$]*)`

// referencesTable matches a REFERENCES clause naming the given parent table.
//
// THE FIRST TWO VERSIONS OF THIS MATCHER WERE THE REAL HOLE, AND ANCHORING THE
// EXTRACTOR DID NOT CLOSE IT. Round 3 fixed the extractor, reported that both
// cascading foreign keys planted in m113's real CREATE TABLE now failed the
// guard, and that did not reproduce: an independent reviewer planted exactly
//
//	"tenant_id" uuid NOT NULL REFERENCES public.tenants (id) ON DELETE CASCADE
//
// and the guard stayed green, because the pattern it used,
// `references\s+"?(public"\.")?tenants"?`, can only read a schema qualifier that
// is QUOTED. Six of the twelve spellings below walked straight past it. So the
// grammar is written out properly here:
//
//   - quoted or bare table name,
//   - schema qualified or not, in any mix of quoting, with or without spaces
//     around the dot,
//   - with or without a space before the parenthesised column list, and with no
//     column list at all (Postgres then targets the primary key),
//   - split across a line break anywhere whitespace is legal, and
//   - written as a table-level FOREIGN KEY constraint rather than on the column.
//
// TestGH402_ForeignKeyGuardCatchesEverySpelling plants every one of them and
// requires a match. \s covers a newline in Go's regexp, which is what makes the
// line-break case fall out rather than need its own pattern.
func referencesTable(table string) *regexp.Regexp {
	return regexp.MustCompile(
		`(?i)\bREFERENCES\s+(?:` + sqlIdent + `\s*\.\s*)?(?:"` + table + `"|\b` + table + `\b)`)
}

var (
	referencesSites   = referencesTable("sites")
	referencesTenants = referencesTable("tenants")

	// anyForeignKey is the backstop. The two matchers above are only as good as
	// the spellings someone thought of, and the cost of missing one is the whole
	// bug back. This table is supposed to have NO foreign key to anything at
	// all, so any REFERENCES or FOREIGN KEY surviving in it is reportable even
	// if it names neither parent: the next reviewer reads the message and
	// decides, instead of the guard deciding silently by staying green.
	anyForeignKey = regexp.MustCompile(`(?i)\b(?:REFERENCES|FOREIGN\s+KEY)\b`)
)

func TestGH402_ReclaimTableHasNoForeignKeys(t *testing.T) {
	for _, rel := range []string{
		"db/schema.sql",
		"migrations/20260815000000_m113_site_object_reclaim.sql",
	} {
		block := siteObjectReclaimBlock(t, readRepoFile(t, rel))
		var named bool
		if referencesSites.MatchString(block) {
			named = true
			t.Errorf("%s: site_object_reclaim has a foreign key to sites. "+
				"That cascade would delete the reclaim record in the same statement as the site, "+
				"which is the entire defect of GH #402", rel)
		}
		if referencesTenants.MatchString(block) {
			named = true
			t.Errorf("%s: site_object_reclaim has a foreign key to tenants. "+
				"admin_delete_empty_tenant hard-deletes a tenant row and frees no object storage, "+
				"so that cascade destroys the reclaim record by the very operation that should "+
				"have triggered it, and GH #402 comes back at tenant level", rel)
		}
		if m := anyForeignKey.FindString(block); m != "" && !named {
			t.Errorf("%s: site_object_reclaim declares a foreign key (%q). This table is "+
				"deliberately parentless: a record of what to clean up has to outlive the "+
				"deletion it describes, so any cascade onto it is a contradiction in terms "+
				"(GH #402)", rel, m)
		}
	}
}

// gh402SyntheticTable renders a CREATE TABLE in the shape m113 really has,
// including a comment that says the word REFERENCES, with the two given column
// lines spliced in. The comment is not decoration: it is the line
// db/schema.sql actually carries ("-- No REFERENCES. See the header"), and it is
// why the block has to reach the matchers with its comments stripped.
func gh402SyntheticTable(tenantLine, siteLine string) string {
	return `
-- CREATE TABLE site_object_reclaim (tenant_id uuid REFERENCES tenants (id));
CREATE TABLE IF NOT EXISTS "public"."site_object_reclaim" (
    "id"               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- No REFERENCES. See the header: a cascade from either parent destroys the
    -- record in the operation it is supposed to survive.
` + tenantLine + `
` + siteLine + `
    "kind"             text        NOT NULL DEFAULT 'backup_manifest',
    "created_at"       timestamptz NOT NULL DEFAULT now()
);
`
}

// gh402FKSpellings is every way a foreign key to these two parents can be
// written. Each is planted into the real m113 CREATE TABLE by hand as well; the
// pasted output of those runs is in the pull request. This test is what keeps
// them covered after that.
var gh402FKSpellings = []struct{ name, tenant, site string }{
	{
		"bare, space before the column list",
		`    "tenant_id" uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,`,
		`    "site_id"   uuid NOT NULL REFERENCES sites (id) ON DELETE CASCADE,`,
	},
	{
		"bare, no space",
		`    "tenant_id" uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,`,
		`    "site_id"   uuid NOT NULL REFERENCES sites(id) ON DELETE CASCADE,`,
	},
	{
		// The one the round-3 report claimed to have caught and had not.
		"schema qualified, unquoted, space before the column list",
		`    "tenant_id" uuid NOT NULL REFERENCES public.tenants (id) ON DELETE CASCADE,`,
		`    "site_id"   uuid NOT NULL REFERENCES public.sites (id) ON DELETE CASCADE,`,
	},
	{
		"schema qualified, unquoted, no space",
		`    "tenant_id" uuid NOT NULL REFERENCES public.tenants(id) ON DELETE CASCADE,`,
		`    "site_id"   uuid NOT NULL REFERENCES public.sites(id) ON DELETE CASCADE,`,
	},
	{
		// The dialect m4 uses for backup_chunks, so it is not hypothetical.
		"fully quoted and schema qualified",
		`    "tenant_id" uuid NOT NULL REFERENCES "public"."tenants" ("id") ON DELETE CASCADE,`,
		`    "site_id"   uuid NOT NULL REFERENCES "public"."sites" ("id") ON DELETE CASCADE,`,
	},
	{
		"quoted table, bare schema",
		`    "tenant_id" uuid NOT NULL REFERENCES public."tenants" (id) ON DELETE CASCADE,`,
		`    "site_id"   uuid NOT NULL REFERENCES public."sites" (id) ON DELETE CASCADE,`,
	},
	{
		"quoted schema, bare table",
		`    "tenant_id" uuid NOT NULL REFERENCES "public".tenants (id) ON DELETE CASCADE,`,
		`    "site_id"   uuid NOT NULL REFERENCES "public".sites (id) ON DELETE CASCADE,`,
	},
	{
		"quoted, no schema, no space",
		`    "tenant_id" uuid NOT NULL REFERENCES "tenants"("id") ON DELETE CASCADE,`,
		`    "site_id"   uuid NOT NULL REFERENCES "sites"("id") ON DELETE CASCADE,`,
	},
	{
		"spaces around the dot",
		`    "tenant_id" uuid NOT NULL REFERENCES "public" . "tenants" (id) ON DELETE CASCADE,`,
		`    "site_id"   uuid NOT NULL REFERENCES "public" . "sites" (id) ON DELETE CASCADE,`,
	},
	{
		// A pgFormatter-style wrap, or simply a long line someone broke.
		"across a line break",
		"    \"tenant_id\" uuid NOT NULL\n        REFERENCES\n        public.tenants (id) ON DELETE CASCADE,",
		"    \"site_id\"   uuid NOT NULL\n        REFERENCES\n        public.sites (id) ON DELETE CASCADE,",
	},
	{
		// Legal Postgres: with no column list the reference targets the primary
		// key, and it still cascades.
		"no column list at all",
		`    "tenant_id" uuid NOT NULL REFERENCES public.tenants ON DELETE CASCADE,`,
		`    "site_id"   uuid NOT NULL REFERENCES sites ON DELETE CASCADE,`,
	},
	{
		// The exact shape m4 wrote for backup_chunks, and the shape an
		// "add the missing constraint" pass produces.
		"table-level FOREIGN KEY constraint",
		"    \"tenant_id\"        uuid        NOT NULL,\n" +
			"    CONSTRAINT \"site_object_reclaim_tenant_id_fkey\"\n" +
			"        FOREIGN KEY (\"tenant_id\") REFERENCES public.tenants (id) ON DELETE CASCADE,",
		"    \"site_id\"          uuid        NOT NULL,\n" +
			"    CONSTRAINT \"site_object_reclaim_site_id_fkey\"\n" +
			"        FOREIGN KEY (\"site_id\") REFERENCES public.sites (id) ON DELETE CASCADE,",
	},
	{
		"table-level FOREIGN KEY, unnamed and bare",
		"    \"tenant_id\"        uuid        NOT NULL,\n" +
			"    FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE,",
		"    \"site_id\"          uuid        NOT NULL,\n" +
			"    FOREIGN KEY (site_id) REFERENCES sites(id) ON DELETE CASCADE,",
	},
}

func TestGH402_ForeignKeyGuardCatchesEverySpelling(t *testing.T) {
	for _, s := range gh402FKSpellings {
		t.Run(s.name, func(t *testing.T) {
			block := siteObjectReclaimBlock(t, gh402SyntheticTable(s.tenant, s.site))
			if !referencesTenants.MatchString(block) {
				t.Errorf("the tenants foreign-key guard does not see this spelling, so it could be "+
					"reintroduced with the guard green:\n%s", s.tenant)
			}
			if !referencesSites.MatchString(block) {
				t.Errorf("the sites foreign-key guard does not see this spelling, so it could be "+
					"reintroduced with the guard green:\n%s", s.site)
			}
			if !anyForeignKey.MatchString(block) {
				t.Errorf("the catch-all does not see this spelling either:\n%s", s.tenant)
			}
		})
	}

	// The other half of a guard worth having: it must not fire on correct code.
	// The comment line here is the one db/schema.sql really carries, and it says
	// the word REFERENCES, so a guard that read the block with its prose still
	// in it would fail the repository as it stands.
	clean := siteObjectReclaimBlock(t, gh402SyntheticTable(
		`    "tenant_id"        uuid        NOT NULL,`,
		`    "site_id"          uuid        NOT NULL,`))
	if referencesTenants.MatchString(clean) || referencesSites.MatchString(clean) ||
		anyForeignKey.MatchString(clean) {
		t.Errorf("the guard fires on a table with no foreign key at all. A guard that fails "+
			"correct work gets switched off, and then it guards nothing.\nblock: %q", clean)
	}

	// A neighbouring table name must not be mistaken for a parent.
	near := siteObjectReclaimBlock(t, gh402SyntheticTable(
		`    "tenant_id" uuid NOT NULL REFERENCES tenants_archive (id),`,
		`    "site_id"   uuid NOT NULL REFERENCES sites_archive (id),`))
	if referencesTenants.MatchString(near) || referencesSites.MatchString(near) {
		t.Error("a foreign key to tenants_archive or sites_archive was reported as one to " +
			"tenants or sites; the message would name the wrong parent")
	}
	if !anyForeignKey.MatchString(near) {
		t.Error("the catch-all missed a foreign key to a table neither named matcher covers, " +
			"which is the case it exists for")
	}
}

// ---------------------------------------------------------------------------
// 2b. Converging an already-applied database must live in a LATER migration.
//
// internal/db/migrate.go skips any version already in schema_migrations, so a
// database that applied the pre-review m113 (tenant foreign key present, no
// site-scope policy) never reads m113 again however that file is edited. The
// first draft of this fix put the DROP CONSTRAINT inside m113, where it could
// only ever run on the databases that did not need it. The convergence
// therefore has to be m114, a version nothing has applied yet.
//
// This test fails if someone moves it back, or renumbers it under m113.
// ---------------------------------------------------------------------------

const gh402ConvergeMigration = "migrations/20260816000000_m114_site_object_reclaim_converge.sql"

func TestGH402_ConvergenceLivesInAMigrationAnOldDatabaseWillRun(t *testing.T) {
	// Comments stripped throughout: these files explain themselves at length and
	// name the statements in question, so an unstripped Contains asserts on the
	// prose rather than on the SQL.
	converge := stripSQLLineComments(readRepoFile(t, gh402ConvergeMigration))
	if !strings.Contains(converge, `DROP CONSTRAINT IF EXISTS "site_object_reclaim_tenant_id_fkey"`) {
		t.Error(gh402ConvergeMigration + " no longer drops a pre-existing " +
			"site_object_reclaim_tenant_id_fkey. A database that applied the first version of " +
			"m113 keeps the cascade that GH #402's second review round proved destroys the " +
			"reclaim record")
	}
	if !strings.Contains(converge, "site_object_reclaim_site_scope") {
		t.Error(gh402ConvergeMigration + " does not create the site-scope policy. The " +
			"pre-review m113 shipped without it, and m113 will never run again on that database")
	}

	// The convergence must sort AFTER m113, or the runner applies it first and
	// it converges a table that does not exist yet.
	if !(gh402ConvergeMigration > "migrations/20260815000000_m113_site_object_reclaim.sql") {
		t.Error("the convergence migration does not sort after m113; migrations are applied in " +
			"lexical filename order")
	}

	// And m113 must NOT try to do this work itself, which is the mistake being
	// guarded against.
	m113 := stripSQLLineComments(readRepoFile(t, "migrations/20260815000000_m113_site_object_reclaim.sql"))
	if strings.Contains(m113, "DROP CONSTRAINT") {
		t.Error("m113 contains a DROP CONSTRAINT. A database that already applied m113 will " +
			"never read it again, so convergence placed there runs only where it is not needed. " +
			"It belongs in " + gh402ConvergeMigration)
	}
}

// ---------------------------------------------------------------------------
// 2c. A site-keyed table ships with the m19 site-scope policy.
//
// site_object_reclaim.site_id is a site key, so tenant isolation alone leaves a
// collaborator invited to ONE site able to reach rows naming every other site
// in the organisation. m112 shipped one migration earlier for exactly this
// reason in the email domain, after seven privilege-escalation doors kept
// appearing because the database had no opinion.
// ---------------------------------------------------------------------------

// siteObjectReclaimSiteScopeCreate anchors on the CREATE POLICY statement, at
// the start of a line, which a "--" comment cannot be. The name may be quoted
// (the migrations) or bare (db/schema.sql), and the character after it must be
// whitespace so that a longer policy name is not mistaken for this one.
var siteObjectReclaimSiteScopeCreate = regexp.MustCompile(
	`(?im)^[ \t]*CREATE\s+POLICY\s+"?site_object_reclaim_site_scope"?\s`)

// siteScopePolicyStatement returns the CREATE POLICY statement itself, comments
// stripped.
//
// THE FIXED-WINDOW VERSION OF THIS WAS THE SAME DEFECT THE EXTRACTOR ABOVE WAS
// REWRITTEN TO REMOVE, LEFT IN PLACE ONE ASSERTION LATER. It took
// strings.Index(src, "site_object_reclaim_site_scope") and read 800 characters
// from the first substring match, so any prose naming the policy ahead of the
// real statement satisfied the AS RESTRICTIVE and app.allowed_site_ids checks
// out of the documentation. Proven, not supposed: three lines of ordinary
// comment above the policy plus a real policy rewritten to
// `FOR ALL USING (true)` passed. A permissive site-scope policy is OR-combined
// with site_object_reclaim_tenant_isolation, so it GRANTS instead of
// subtracting and the site boundary disappears entirely.
func siteScopePolicyStatement(t *testing.T, src string) string {
	t.Helper()
	stmt, err := extractSiteScopePolicy(src)
	if err != nil {
		// Fatal, never a silent empty string, for the same reason as the table
		// block: passing because it read nothing is the failure being removed.
		t.Fatalf("could not read the site_object_reclaim_site_scope CREATE POLICY: %v", err)
	}
	return stmt
}

func extractSiteScopePolicy(src string) (string, error) {
	loc := siteObjectReclaimSiteScopeCreate.FindStringIndex(src)
	if loc == nil {
		return "", errors.New("no `CREATE POLICY site_object_reclaim_site_scope` at the start of " +
			"any line; every site-keyed table carries one (m19, and m112 for the email domain)")
	}
	stmt, ok := statementFrom(src[loc[0]:])
	if !ok {
		return "", errors.New("the CREATE POLICY statement is not terminated by a semicolon")
	}
	return stripSQLLineComments(stmt), nil
}

func TestGH402_ReclaimTableHasSiteScopePolicy(t *testing.T) {
	// m114 is in this loop deliberately. It is the ONLY route by which a
	// database that ran the pre-review m113 gets this policy, its text is meant
	// to be identical to m113's, and every RLS proof that runs against a fresh
	// database never sees it at all.
	for _, rel := range []string{
		"db/schema.sql",
		"migrations/20260815000000_m113_site_object_reclaim.sql",
		gh402ConvergeMigration,
	} {
		stmt := siteScopePolicyStatement(t, readRepoFile(t, rel))
		upper := strings.ToUpper(stmt)
		if !strings.Contains(upper, "AS RESTRICTIVE") {
			t.Errorf("%s: the site-scope policy is not RESTRICTIVE. A permissive policy is "+
				"OR-combined with site_object_reclaim_tenant_isolation, so it GRANTS rather than "+
				"subtracts and the site boundary disappears.\nstatement: %s", rel, stmt)
		}
		if !strings.Contains(stmt, "app.allowed_site_ids") {
			t.Errorf("%s: the site-scope policy does not consult app.allowed_site_ids.\n"+
				"statement: %s", rel, stmt)
		}
		if !strings.Contains(upper, "WITH CHECK") {
			t.Errorf("%s: the site-scope policy has no WITH CHECK, so it constrains reads and "+
				"not writes. Forging or closing another site's reclaim row is the half that "+
				"costs something: it strands that site's manifests (GH #402 through a different "+
				"door).\nstatement: %s", rel, stmt)
		}
	}
}

// The policy extractor's own test, with the decoy the fixed window fell for.
func TestGH402_SiteScopePolicyExtractorIsAnchored(t *testing.T) {
	const good = `
CREATE POLICY site_object_reclaim_site_scope ON site_object_reclaim
    AS RESTRICTIVE FOR ALL
    USING (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    )
    WITH CHECK (
        coalesce(current_setting('app.site_scope', true), '') <> 'on'
        OR site_id = ANY (
            string_to_array(nullif(current_setting('app.allowed_site_ids', true), ''), ',')::uuid[]
        )
    );
`
	// The same policy after the mutation that matters: permissive, and blind to
	// the allow-list. Nothing about the weakened text is exotic; it is what a
	// "simplify this policy" pass produces.
	const weak = `
CREATE POLICY site_object_reclaim_site_scope ON site_object_reclaim
    FOR ALL
    USING (
        true
    );
`
	// Every decoy names the policy and carries both phrases the assertions look
	// for, and every one of them sits ABOVE the statement.
	decoys := map[string]string{
		"a header paragraph": `
-- site_object_reclaim_site_scope is declared AS RESTRICTIVE and consults
-- app.allowed_site_ids, so it can only ever subtract from the permissive
-- policies above.
`,
		"the pg_policies existence guard the migrations wrap every policy in": `
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'site_object_reclaim'
          AND policyname = 'site_object_reclaim_site_scope' -- AS RESTRICTIVE, app.allowed_site_ids
    ) THEN
`,
		"a commented-out earlier draft": `
-- CREATE POLICY site_object_reclaim_site_scope ON site_object_reclaim
--     AS RESTRICTIVE FOR ALL
--     USING (site_id = ANY (current_setting('app.allowed_site_ids', true)::uuid[]));
`,
	}

	for name, decoy := range decoys {
		t.Run(name, func(t *testing.T) {
			stmt, err := extractSiteScopePolicy(decoy + good)
			if err != nil {
				t.Fatalf("extractor failed on a file containing a real CREATE POLICY: %v", err)
			}
			if !strings.Contains(stmt, "WITH CHECK") {
				t.Fatalf("the extractor returned something other than the whole policy statement, "+
					"so the assertions would read prose.\ngot: %q", stmt)
			}

			// The load-bearing direction: with the decoy still above it, a
			// WEAKENED policy must not satisfy the assertions.
			stmt, err = extractSiteScopePolicy(decoy + weak)
			if err != nil {
				t.Fatalf("extractor failed on the weakened policy: %v", err)
			}
			if strings.Contains(strings.ToUpper(stmt), "AS RESTRICTIVE") {
				t.Errorf("the extractor read AS RESTRICTIVE out of the decoy while the real policy "+
					"is permissive. That is exactly how the fixed 800-character window passed.\n"+
					"got: %q", stmt)
			}
			if strings.Contains(stmt, "app.allowed_site_ids") {
				t.Errorf("the extractor read app.allowed_site_ids out of the decoy while the real "+
					"policy ignores it.\ngot: %q", stmt)
			}
		})
	}

	// A file with no CREATE POLICY at all must error rather than return "".
	if _, err := extractSiteScopePolicy(decoys["a header paragraph"]); err == nil {
		t.Error("a file whose only mention of the policy is prose produced no error; the guard " +
			"would read an empty statement and pass")
	}

	// A longer policy name is a different policy.
	if _, err := extractSiteScopePolicy(
		"CREATE POLICY site_object_reclaim_site_scope_read ON site_object_reclaim USING (true);\n",
	); err == nil {
		t.Error("site_object_reclaim_site_scope_read was accepted as site_object_reclaim_site_scope")
	}
}

// ---------------------------------------------------------------------------
// siteObjectReclaimBlock: the column list of the site_object_reclaim CREATE
// TABLE, and nothing else.
//
// THIS HELPER IS LOAD BEARING, AND ITS FIRST VERSION WAS NOT. It took the first
// occurrence of the substring "site_object_reclaim" anywhere in the file and
// read from the next "(" to the next ");". In m113 the first occurrence is the
// operator backfill statement quoted in the header comment,
//
//	--   INSERT INTO site_object_reclaim (tenant_id, site_id, kind)
//	--   ... SET completed_at = NULL, attempts = 0, next_attempt_at = now();
//
// whose "(" opens the column name list and whose "now();" supplies the ");".
// The foreign-key guard below was therefore reading four lines of prose, and
// passed with BOTH cascading foreign keys planted in the real CREATE TABLE.
// The single worst defect this branch has produced could have come back into
// m113 with the guard against it still green.
//
// So it is anchored on CREATE TABLE, at the start of a line, which a "--"
// comment line cannot be, and the end is the paren that matches the opening
// one rather than the first ");" that happens along.
// ---------------------------------------------------------------------------

// siteObjectReclaimCreateTable matches the CREATE TABLE header in both dialects
// this repo writes: the bare declarative form in db/schema.sql, and the quoted,
// schema-qualified, IF NOT EXISTS form the migrations use inside a DO block.
// The ^ anchor with (?m) is what excludes comments: every SQL comment line here
// starts with "--", so no comment can satisfy "start of line, then CREATE".
var siteObjectReclaimCreateTable = regexp.MustCompile(
	`(?im)^[ \t]*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:"?public"?\s*\.\s*)?"?site_object_reclaim"?\s*\(`)

// The returned block has its "--" comments stripped, so every assertion made
// about it is an assertion about SQL. That matters in both directions here:
// db/schema.sql's column list carries the line "-- No REFERENCES. See the
// header", which would make the foreign-key backstop fail correct code, and a
// planted foreign key hidden behind a comment on the same line would otherwise
// be invisible. Prose inside the declaration can neither satisfy nor defeat a
// check on the declaration.
func siteObjectReclaimBlock(t *testing.T, src string) string {
	t.Helper()
	block, err := extractSiteObjectReclaimBlock(src)
	if err != nil {
		// Fatal, never a silent empty string. A guard that cannot find the table
		// must go red: passing because it read nothing is the failure mode this
		// helper was rewritten to remove.
		t.Fatalf("could not read the site_object_reclaim CREATE TABLE: %v", err)
	}
	return stripSQLLineComments(block)
}

// extractSiteObjectReclaimBlock is the testable half, so the anti-vacuity
// property can itself be tested (see TestGH402_ReclaimBlockExtractorIsAnchored).
func extractSiteObjectReclaimBlock(src string) (string, error) {
	loc := siteObjectReclaimCreateTable.FindStringIndex(src)
	if loc == nil {
		return "", errors.New("no `CREATE TABLE site_object_reclaim (` at the start of any line")
	}
	body, ok := balancedParenBody(src[loc[1]-1:]) // loc[1]-1 is the "("
	if !ok {
		return "", errors.New("the column list has no matching close paren")
	}
	return body, nil
}

// skipNonCode advances past whatever non-code construct begins at src[i]: a
// "--" line comment, a "/* */" block comment, or a single-quoted string. It
// returns the index of that construct's last byte, i unchanged if src[i] begins
// none of them, and -1 if the construct is unterminated.
//
// The comment cases are checked BEFORE the quote case on purpose: this schema is
// heavily commented in English, so an apostrophe inside a comment is far more
// likely than a "--" inside a string literal, and only one of the two orderings
// can be right.
func skipNonCode(src string, i int) int {
	switch {
	case strings.HasPrefix(src[i:], "--"):
		nl := strings.IndexByte(src[i:], '\n')
		if nl < 0 {
			return -1
		}
		return i + nl
	case strings.HasPrefix(src[i:], "/*"):
		end := strings.Index(src[i+2:], "*/")
		if end < 0 {
			return -1
		}
		return i + 2 + end + 1
	case src[i] == '\'':
		j := i + 1
		for j < len(src) {
			if src[j] != '\'' {
				j++
				continue
			}
			if j+1 < len(src) && src[j+1] == '\'' { // '' is an escaped quote
				j += 2
				continue
			}
			return j
		}
		return -1
	}
	return i
}

// balancedParenBody returns the text between src[0], which must be "(", and the
// ")" that matches it. Parens inside a comment or a string do not count.
func balancedParenBody(src string) (string, bool) {
	if len(src) == 0 || src[0] != '(' {
		return "", false
	}
	depth := 0
	for i := 0; i < len(src); i++ {
		if j := skipNonCode(src, i); j != i {
			if j < 0 {
				return "", false
			}
			i = j
			continue
		}
		switch src[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return src[1:i], true
			}
		}
	}
	return "", false
}

// statementFrom returns the SQL statement starting at src[0], up to and
// including the ";" that ends it. A ";" inside a comment, inside a string, or
// inside parentheses is not a terminator: m113 documents an operator INSERT
// ending in "now();" a few lines above the code, and the policy bodies are one
// long parenthesised expression each.
func statementFrom(src string) (string, bool) {
	depth := 0
	for i := 0; i < len(src); i++ {
		if j := skipNonCode(src, i); j != i {
			if j < 0 {
				return "", false
			}
			i = j
			continue
		}
		switch src[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ';':
			if depth == 0 {
				return src[:i+1], true
			}
		}
	}
	return "", false
}

// The extractor's own test. The foreign-key guard is only as good as this, and
// the previous extractor passed every test in this file while reading a
// comment, so the property gets asserted directly: the block must come from the
// CREATE TABLE, and decoys that mention the table anywhere else must not be
// mistaken for it.
func TestGH402_ReclaimBlockExtractorIsAnchored(t *testing.T) {
	const table = `
CREATE TABLE site_object_reclaim (
    id        uuid PRIMARY KEY,
    tenant_id uuid NOT NULL,
    kind      text NOT NULL DEFAULT 'backup_manifest'
);
`
	// Every one of these mentions the table, offers a "(" and ends in ");", and
	// every one of them sits ABOVE the real declaration. The old extractor
	// returned the decoy for the first of them, which is the shape m113
	// actually has.
	decoys := map[string]string{
		"the operator backfill quoted in a comment": `
--   INSERT INTO site_object_reclaim (tenant_id, site_id, kind)
--   VALUES ('<tenant_id>', '<site_id>', 'backup_manifest')
--   ON CONFLICT (tenant_id, site_id, kind) DO UPDATE
--     SET tenant_id = tenant_id REFERENCES tenants (id) ON DELETE CASCADE, next_attempt_at = now();
`,
		"a banner comment": `
-- ---------------------------------------------------------------------------
-- site_object_reclaim  (m113 / GH #402), REFERENCES tenants (id) is forbidden;
-- ---------------------------------------------------------------------------
`,
		"an index name": `
CREATE UNIQUE INDEX site_object_reclaim_site_kind_key
    ON site_object_reclaim (tenant_id, site_id, kind);
`,
		"a policy name": `
CREATE POLICY site_object_reclaim_tenant_isolation ON site_object_reclaim
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
`,
		"a commented-out CREATE TABLE": `
-- CREATE TABLE site_object_reclaim (
--     tenant_id uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE
-- );
`,
	}

	for name, decoy := range decoys {
		t.Run(name, func(t *testing.T) {
			block, err := extractSiteObjectReclaimBlock(decoy + table)
			if err != nil {
				t.Fatalf("extractor failed on a file containing a real CREATE TABLE: %v", err)
			}
			if !strings.Contains(block, "id        uuid PRIMARY KEY") {
				t.Fatalf("extractor returned something other than the CREATE TABLE column list.\n"+
					"That is how the foreign-key guard went vacuous, so it is fatal.\ngot: %q", block)
			}
			if strings.Contains(block, "REFERENCES") {
				t.Fatalf("extractor pulled text from the decoy above the table; the guard would "+
					"now FALSE-POSITIVE on correct code.\ngot: %q", block)
			}
		})
	}

	// A file with no CREATE TABLE at all must error, not silently return "".
	if _, err := extractSiteObjectReclaimBlock(decoys["a policy name"]); err == nil {
		t.Error("a file with no CREATE TABLE produced no error; the guard would read an empty " +
			"block and pass")
	}

	// A close paren inside a comment or a string must not end the block early.
	tricky := `
CREATE TABLE IF NOT EXISTS "public"."site_object_reclaim" (
    -- 'cp' | 'local' (s3_compat) | NULL, and a stray ); in prose
    "destination_kind" text DEFAULT 'a)string;with(parens',
    "tenant_id" uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE
);
`
	block, err := extractSiteObjectReclaimBlock(tricky)
	if err != nil {
		t.Fatalf("extractor failed on the quoted migration dialect: %v", err)
	}
	if !strings.Contains(block, `"tenant_id" uuid NOT NULL REFERENCES tenants`) {
		t.Errorf("the block was truncated by a paren inside a comment or a string literal, so a "+
			"foreign key declared after one would be invisible to the guard.\ngot: %q", block)
	}
}

// ---------------------------------------------------------------------------
// 3. The enqueue must sit in the SAME transaction closure as the DELETE.
//
// Splitting it into an earlier separate transaction is the reporter's own
// suggested shape and it is unsafe: if that commits and the delete then rolls
// back, the database holds a durable instruction to delete the manifests of a
// site that is still live. Whether two statements share a transaction is not
// something a unit test can see, so it is asserted against the AST.
// ---------------------------------------------------------------------------

func TestGH402_SiteDeleteEnqueuesInsideTheSameTransaction(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, repoFile(t, "internal/site/repo.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse internal/site/repo.go: %v", err)
	}

	var deleteFn *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Delete" || fn.Recv == nil {
			return true
		}
		deleteFn = fn
		return false
	})
	if deleteFn == nil {
		t.Fatal("internal/site/repo.go has no Delete method")
	}

	// Find the closure passed to InTenantTx and require BOTH calls inside it.
	var checked bool
	ast.Inspect(deleteFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "InTenantTx" {
			return true
		}
		var closure *ast.FuncLit
		for _, arg := range call.Args {
			if fl, ok := arg.(*ast.FuncLit); ok {
				closure = fl
			}
		}
		if closure == nil {
			t.Fatal("Delete's InTenantTx call has no closure argument")
		}
		checked = true

		calls := map[string]bool{}
		ast.Inspect(closure, func(m ast.Node) bool {
			c, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			if s, ok := c.Fun.(*ast.SelectorExpr); ok {
				calls[s.Sel.Name] = true
			}
			return true
		})
		if !calls["DeleteSite"] {
			t.Error("DeleteSite is not called inside Delete's InTenantTx closure")
		}
		if !calls["EnqueueSiteObjectReclaim"] {
			t.Error("EnqueueSiteObjectReclaim is not called inside the SAME InTenantTx closure as DeleteSite. " +
				"A separate transaction can commit while the delete rolls back, leaving a durable " +
				"instruction to delete a LIVE site's manifests (GH #402)")
		}
		return false
	})
	if !checked {
		t.Fatal("Delete no longer wraps its work in InTenantTx")
	}
}

// ---------------------------------------------------------------------------
// 4. The manifest root and the chunk root must stay disjoint.
//
// The whole safety argument for the reclaimer is structural: it cannot delete a
// chunk because chunks are not under the prefix it walks. If a future change
// moved chunk keys under tenant/<id>/, that argument evaporates silently and
// the reclaimer would start deleting deduplicated chunks that live sites need.
// ---------------------------------------------------------------------------

func TestGH402_ManifestAndChunkRootsAreDisjoint(t *testing.T) {
	src := readRepoFile(t, "internal/backup/model.go")
	if !strings.Contains(src, `"chunks/" + tenantID.String() + "/" + blake3`) {
		t.Fatal("chunkS3Key no longer builds chunks/<tenantID>/<blake3>; " +
			"the reclaimer's safety argument is that chunk keys are NOT under the manifest root, " +
			"so this change needs the reclaimer re-reviewed (GH #402)")
	}
	svc := readRepoFile(t, "internal/backup/service.go")
	if !strings.Contains(svc, `"tenant/%s/site/%s/backup/%s/manifest.json"`) {
		t.Fatal("manifestIndexKey no longer builds tenant/<t>/site/<s>/backup/<snap>/manifest.json; " +
			"the reclaim worker derives exactly that prefix (GH #402)")
	}
}
