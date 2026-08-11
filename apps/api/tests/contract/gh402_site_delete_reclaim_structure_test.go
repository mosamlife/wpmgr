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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

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
	converge := readRepoFile(t, gh402ConvergeMigration)
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
	m113 := readRepoFile(t, "migrations/20260815000000_m113_site_object_reclaim.sql")
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

// siteObjectReclaimBlock extracts the SQL text from the site_object_reclaim
// CREATE TABLE up to the end of that statement.
func siteObjectReclaimBlock(t *testing.T, src string) string {
	t.Helper()
	idx := strings.Index(src, "site_object_reclaim")
	if idx < 0 {
		t.Fatal("site_object_reclaim is not declared at all")
	}
	start := strings.Index(src[idx:], "(")
	if start < 0 {
		t.Fatal("malformed site_object_reclaim declaration")
	}
	rest := src[idx+start:]
	// Read to the end of the CREATE TABLE statement.
	end := strings.Index(rest, ");")
	if end < 0 {
		t.Fatal("malformed site_object_reclaim declaration")
	}
	return rest[:end]
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
