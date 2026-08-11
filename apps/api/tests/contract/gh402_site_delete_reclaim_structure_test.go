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

func TestGH402_ReclaimTableHasNoForeignKeys(t *testing.T) {
	for _, rel := range []string{
		"db/schema.sql",
		"migrations/20260815000000_m113_site_object_reclaim.sql",
	} {
		src := readRepoFile(t, rel)
		block := siteObjectReclaimBlock(t, src)
		if regexp.MustCompile(`(?i)references\s+"?(public"\.")?sites"?`).MatchString(block) {
			t.Errorf("%s: site_object_reclaim has a foreign key to sites. "+
				"That cascade would delete the reclaim record in the same statement as the site, "+
				"which is the entire defect of GH #402", rel)
		}
		if regexp.MustCompile(`(?i)references\s+"?(public"\.")?tenants"?`).MatchString(block) {
			t.Errorf("%s: site_object_reclaim has a foreign key to tenants. "+
				"admin_delete_empty_tenant hard-deletes a tenant row and frees no object storage, "+
				"so that cascade destroys the reclaim record by the very operation that should "+
				"have triggered it, and GH #402 comes back at tenant level", rel)
		}
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

func TestGH402_ReclaimTableHasSiteScopePolicy(t *testing.T) {
	for _, rel := range []string{
		"db/schema.sql",
		"migrations/20260815000000_m113_site_object_reclaim.sql",
	} {
		src := readRepoFile(t, rel)
		if !strings.Contains(src, "site_object_reclaim_site_scope") {
			t.Errorf("%s: site_object_reclaim has no site_object_reclaim_site_scope policy; "+
				"every other site-keyed table carries one (m19, and m112 for the email domain)", rel)
			continue
		}
		idx := strings.Index(src, "site_object_reclaim_site_scope")
		tail := src[idx:]
		if !strings.Contains(tail[:min(len(tail), 800)], "AS RESTRICTIVE") {
			t.Errorf("%s: the site-scope policy is not RESTRICTIVE. A permissive policy is "+
				"OR-combined with the others and grants access rather than removing it", rel)
		}
		if !strings.Contains(tail[:min(len(tail), 800)], "app.allowed_site_ids") {
			t.Errorf("%s: the site-scope policy does not consult app.allowed_site_ids", rel)
		}
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

func siteObjectReclaimBlock(t *testing.T, src string) string {
	t.Helper()
	block, err := extractSiteObjectReclaimBlock(src)
	if err != nil {
		// Fatal, never a silent empty string. A guard that cannot find the table
		// must go red: passing because it read nothing is the failure mode this
		// helper was rewritten to remove.
		t.Fatalf("could not read the site_object_reclaim CREATE TABLE: %v", err)
	}
	return block
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

// balancedParenBody returns the text between src[0], which must be "(", and the
// ")" that matches it. Parens inside a "--" line comment, a "/* */" block
// comment or a single-quoted string do not count.
//
// The comment cases are checked BEFORE the quote case on purpose: this schema is
// heavily commented in English, so an apostrophe inside a comment is far more
// likely than a "--" inside a string literal, and only one of the two orderings
// can be right.
func balancedParenBody(src string) (string, bool) {
	if len(src) == 0 || src[0] != '(' {
		return "", false
	}
	depth := 0
	for i := 0; i < len(src); i++ {
		switch {
		case strings.HasPrefix(src[i:], "--"):
			nl := strings.IndexByte(src[i:], '\n')
			if nl < 0 {
				return "", false
			}
			i += nl
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return "", false
			}
			i += 2 + end + 1
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
				break
			}
			if j >= len(src) {
				return "", false
			}
			i = j
		case src[i] == '(':
			depth++
		case src[i] == ')':
			depth--
			if depth == 0 {
				return src[1:i], true
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
