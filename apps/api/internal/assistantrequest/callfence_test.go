package assistantrequest

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

// The call fence: this package must never open a transaction that bypasses
// the single-site principal, never build a principal by hand, and never call
// a helper that opens its own transaction on site data.
//
// Every rule below is checked over the package's non-test source with the Go
// parser, so a new file is covered without being listed.

// inTenantTxAllowed names the only functions that may open a tenant
// transaction: exception 11 and the sweeper's and reconciler's per-row
// compare-and-sets. None of them touches site data.
var inTenantTxAllowed = map[string]bool{
	"closeWithoutSite":  true,
	"sweepExpired":      true,
	"sweepPastDeadline": true,
	"reconcile":         true,
}

// forbiddenSelectors are calls that open their own transaction on site data,
// or that belong to the dashboard purge path.
var forbiddenSelectors = map[string]string{
	"MarkCachePurged":             "use MarkCachePurgedTx on the outcome transaction",
	"GetCDNCredentialsCiphertext": "use GetCDNCredentialsCiphertextTx on the reservation transaction",
	"RecordPurge":                 "the reservation inserts the attempt record itself",
	"Purge":                       "the dashboard purge records, sends and stamps in its own transactions",
	"GetSiteURL":                  "the site URL comes from the scoped site read",
	"InTenantTxAsUser":            "no tenant transaction as a user on this path",
	"InScopedTenantTx":            "site-scoped transactions come from RunTenantTx with a single-site principal",
}

// forbiddenImports are packages this one must not import.
var forbiddenImports = map[string]string{
	"github.com/mosamlife/wpmgr/apps/api/internal/site": "site data is read through the scoped statement, never a site repo",
}

type fenceViolation struct {
	pos  token.Position
	what string
}

func scanFence(t *testing.T, dir string) []fenceViolation {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var out []fenceViolation
	files := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if why, bad := forbiddenImports[path]; bad {
				out = append(out, fenceViolation{fset.Position(imp.Pos()), "imports " + path + ": " + why})
			}
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				// Package-level vars can hold literals too.
				ast.Inspect(decl, func(n ast.Node) bool {
					if v := principalLiteral(n); v != "" {
						out = append(out, fenceViolation{fset.Position(n.Pos()), v})
					}
					return true
				})
				continue
			}
			funcName := fn.Name.Name
			ast.Inspect(fn, func(n ast.Node) bool {
				if v := principalLiteral(n); v != "" {
					out = append(out, fenceViolation{fset.Position(n.Pos()), v})
				}
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch name := sel.Sel.Name; {
				case name == "InTenantTx":
					if !inTenantTxAllowed[funcName] {
						out = append(out, fenceViolation{fset.Position(call.Pos()),
							"InTenantTx in " + funcName + ": only closeWithoutSite, the sweeper and the reconciler may open a tenant transaction"})
					}
				default:
					if why, bad := forbiddenSelectors[name]; bad {
						out = append(out, fenceViolation{fset.Position(call.Pos()), name + " called in " + funcName + ": " + why})
					}
				}
				return true
			})
		}
	}
	if files == 0 {
		t.Fatalf("call fence scanned no source files in %s; a fence that reads nothing proves nothing", dir)
	}
	return out
}

// principalLiteral reports a domain.Principal composite literal. Every
// principal on this path comes from mcp.SingleSitePrincipal or from the
// request context.
func principalLiteral(n ast.Node) string {
	cl, ok := n.(*ast.CompositeLit)
	if !ok {
		return ""
	}
	sel, ok := cl.Type.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if x, ok := sel.X.(*ast.Ident); ok && x.Name == "domain" && sel.Sel.Name == "Principal" {
		return "domain.Principal literal: build a site principal only with mcp.SingleSitePrincipal"
	}
	return ""
}

func TestCallFence_AssistantRequest(t *testing.T) {
	violations := scanFence(t, ".")
	for _, v := range violations {
		t.Errorf("%s: %s", v.pos, v.what)
	}
}

// TestCallFence_FiresOnEveryRule plants one of each violation in a scratch
// copy of the package and proves the fence reports each, so a fence that
// silently matches nothing cannot pass.
func TestCallFence_FiresOnEveryRule(t *testing.T) {
	dir := t.TempDir()
	src := `package assistantrequest

import (
	"context"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	_ "github.com/mosamlife/wpmgr/apps/api/internal/site"
)

func plantedDispatch(ctx context.Context, s *Service) {
	_ = domain.Principal{}
	_ = s.repo.pool.InTenantTx(ctx, [16]byte{}, nil)
	var r interface{ MarkCachePurged(); GetCDNCredentialsCiphertext(); RecordPurge(); Purge(); GetSiteURL() }
	r.MarkCachePurged()
	r.GetCDNCredentialsCiphertext()
	r.RecordPurge()
	r.Purge()
	r.GetSiteURL()
}

func closeWithoutSite(ctx context.Context, s *Service) {
	_ = s.repo.pool.InTenantTx(ctx, [16]byte{}, nil)
}
`
	if err := os.WriteFile(filepath.Join(dir, "planted.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	got := scanFence(t, dir)
	want := []string{
		"imports github.com/mosamlife/wpmgr/apps/api/internal/site",
		"domain.Principal literal",
		"InTenantTx in plantedDispatch",
		"MarkCachePurged called",
		"GetCDNCredentialsCiphertext called",
		"RecordPurge called",
		"Purge called",
		"GetSiteURL called",
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
			t.Errorf("planted violation %q was not reported; got %d violations", w, len(got))
		}
	}
	for _, v := range got {
		if strings.Contains(v.what, "InTenantTx in closeWithoutSite") {
			t.Errorf("fence over-fired on the allowed closeWithoutSite: %s", v.what)
		}
	}
}
