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
// 2. site_object_reclaim must NEVER gain a foreign key to sites.
//
// A site_id FK with ON DELETE CASCADE would destroy the row in the very
// statement it exists to survive, restoring the bug exactly. This is the kind
// of thing a later "tidy up the schema" pass does without malice, so it gets an
// explicit test with the reason attached.
// ---------------------------------------------------------------------------

func TestGH402_ReclaimTableHasNoForeignKeyToSites(t *testing.T) {
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
		if !regexp.MustCompile(`(?i)references\s+"?(public"\.")?tenants"?`).MatchString(block) {
			t.Errorf("%s: site_object_reclaim lost its tenant foreign key; "+
				"a hard-purged tenant would leave orphan task rows behind", rel)
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
