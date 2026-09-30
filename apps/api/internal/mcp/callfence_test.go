package mcp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The call fence for this package (design 4.5a). It is checked over the
// package's non-test source with the Go parser, so a new file is covered
// without being listed:
//
//   - a domain.Principal literal appears only in the three functions that
//     build this package's principals;
//   - the creation rail and the status tool never open a tenant transaction
//     themselves: every site read runs under the principal they were handed;
//   - this package imports neither the perf domain nor assistantrequest, which
//     imports it.

var principalLiteralAllowed = map[string]bool{
	"bootstrapTenantPrincipal":  true,
	"connectionScopedPrincipal": true,
	"SingleSitePrincipal":       true,
}

// noTenantTxFiles are the files that may not call a tenant-transaction
// helper at all.
var noTenantTxFiles = map[string]bool{
	"write_rail.go":         true,
	"cache_purge_status.go": true,
}

var tenantTxHelpers = map[string]bool{
	"InTenantTx":       true,
	"InTenantTxAsUser": true,
	"InScopedTenantTx": true,
}

var mcpForbiddenImports = map[string]string{
	"github.com/mosamlife/wpmgr/apps/api/internal/perf":             "the purge is sent by assistantrequest; mcp never reaches the perf domain",
	"github.com/mosamlife/wpmgr/apps/api/internal/assistantrequest": "assistantrequest imports mcp, never the reverse",
}

type mcpFenceViolation struct {
	pos  token.Position
	what string
}

func scanMCPFence(t *testing.T, dir string, wantFiles []string) []mcpFenceViolation {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	seen := map[string]bool{}
	var out []mcpFenceViolation
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		seen[name] = true
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if why, bad := mcpForbiddenImports[path]; bad {
				out = append(out, mcpFenceViolation{fset.Position(imp.Pos()), "imports " + path + ": " + why})
			}
		}
		for _, decl := range f.Decls {
			funcName := ""
			if fn, ok := decl.(*ast.FuncDecl); ok {
				funcName = fn.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				if isPrincipalLiteral(n) && !principalLiteralAllowed[funcName] {
					where := funcName
					if where == "" {
						where = "a package-level declaration"
					}
					out = append(out, mcpFenceViolation{fset.Position(n.Pos()),
						"domain.Principal literal in " + where + ": only bootstrapTenantPrincipal, " +
							"connectionScopedPrincipal and SingleSitePrincipal build a principal"})
				}
				if !noTenantTxFiles[name] {
					return true
				}
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var callee string
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					callee = fn.Sel.Name
				case *ast.Ident:
					callee = fn.Name
				}
				if tenantTxHelpers[callee] {
					out = append(out, mcpFenceViolation{fset.Position(call.Pos()),
						callee + " in " + name + ": the rail and the status tool read only under the principal they were handed"})
				}
				return true
			})
		}
	}
	for _, w := range wantFiles {
		if !seen[w] {
			t.Fatalf("call fence did not scan %s in %s; a fence that reads nothing proves nothing", w, dir)
		}
	}
	return out
}

func isPrincipalLiteral(n ast.Node) bool {
	cl, ok := n.(*ast.CompositeLit)
	if !ok {
		return false
	}
	sel, ok := cl.Type.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "domain" && sel.Sel.Name == "Principal"
}

func TestCallFence_MCP(t *testing.T) {
	for _, v := range scanMCPFence(t, ".", []string{"write_rail.go", "cache_purge_status.go", "kernel.go", "service.go"}) {
		t.Errorf("%s: %s", v.pos, v.what)
	}
}

// TestCallFence_MCP_FiresOnEveryRule plants each violation in a scratch
// package and proves the fence reports it, and that the allowed shapes are
// not reported.
func TestCallFence_MCP_FiresOnEveryRule(t *testing.T) {
	dir := t.TempDir()
	rail := `package mcp

import (
	"context"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	_ "github.com/mosamlife/wpmgr/apps/api/internal/perf"
)

func plantedRail(ctx context.Context, r *Repo) {
	_ = domain.Principal{}
	_ = r.pool.InTenantTx(ctx, [16]byte{}, nil)
	_ = r.pool.InTenantTxAsUser(ctx, [16]byte{}, [16]byte{}, nil)
}
`
	status := `package mcp

import _ "github.com/mosamlife/wpmgr/apps/api/internal/assistantrequest"

func plantedStatus(r *Repo) { InScopedTenantTx(nil) }
`
	elsewhere := `package mcp

import "github.com/mosamlife/wpmgr/apps/api/internal/domain"

var plantedVar = domain.Principal{}

func SingleSitePrincipal() domain.Principal { return domain.Principal{} }

func otherRepo(r *Repo) { _ = r.pool.InTenantTx(nil, [16]byte{}, nil) }
`
	for name, src := range map[string]string{"write_rail.go": rail, "cache_purge_status.go": status, "kernel.go": elsewhere, "service.go": "package mcp\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := scanMCPFence(t, dir, []string{"write_rail.go", "cache_purge_status.go", "kernel.go", "service.go"})
	want := []string{
		"imports github.com/mosamlife/wpmgr/apps/api/internal/perf",
		"imports github.com/mosamlife/wpmgr/apps/api/internal/assistantrequest",
		"domain.Principal literal in plantedRail",
		"domain.Principal literal in a package-level declaration",
		"InTenantTx in write_rail.go",
		"InTenantTxAsUser in write_rail.go",
		"InScopedTenantTx in cache_purge_status.go",
	}
	for _, w := range want {
		found := false
		for _, v := range got {
			if strings.Contains(v.what, w) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the fence did not report %q; got %v", w, got)
		}
	}
	// Not over-firing: the literal inside SingleSitePrincipal and a tenant
	// transaction in a file outside the rail are allowed.
	for _, v := range got {
		if strings.Contains(v.what, "SingleSitePrincipal:") || strings.Contains(v.what, "in kernel.go") ||
			strings.Contains(v.what, "literal in SingleSitePrincipal") {
			t.Errorf("the fence over-fired: %s", v.what)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d violations, want exactly %d: %v", len(got), len(want), got)
	}
}
