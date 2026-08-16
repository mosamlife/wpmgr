// monitoring_contract_test.go — GH #414. Keeps the `detail` enum in
// packages/openapi/openapi.yaml honest against what monitoring_handler.go can
// actually put in that field.
//
// A hand-listed comparison ("the handler emits these N values, and here they
// are again") drifts the moment someone adds a refusal code to the handler
// without touching the contract — exactly the gap a review found: the
// handler could already emit site_archived/site_revoked (see
// respondMonitoring's `"site_" + st.ConnectionState`) and the enum only
// declared seven of the nine values. This test instead PARSES
// monitoring_handler.go at test time to find every literal assigned to
// `.Detail`, so it fails the moment the handler's source and the generated
// contract type disagree — no matter which side changed.
package site

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/api/gen"
)

// monitoringHandlerDetailLiterals statically extracts every string literal
// assigned to a `Detail` field or `.Detail` selector inside
// monitoring_handler.go — both the composite-literal form
// (`monitoringResultDTO{..., Detail: "forbidden"}`) and the plain assignment
// form (`res.Detail = "paused"`).
//
// It also recognises the ONE dynamic case in that file,
// `Detail: "site_" + st.ConnectionState` in respondMonitoring, and resolves it
// through monitoringPauseBlockedStates — the actual package-level slice that
// gates which connection states can reach that line (see Pausable()) — rather
// than hand-listing "site_archived"/"site_revoked" a second time. Any other
// dynamic (non-string-literal) value assigned to Detail is unrecognised and
// fails the test loudly instead of being silently skipped.
func monitoringHandlerDetailLiterals(t *testing.T) map[string]bool {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "monitoring_handler.go", nil, 0)
	if err != nil {
		t.Fatalf("parse monitoring_handler.go: %v", err)
	}

	found := make(map[string]bool)
	isDetailKey := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && id.Name == "Detail"
	}
	isDetailSelector := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "Detail"
	}
	// recordValue handles the value side of either a KeyValueExpr or an
	// AssignStmt whose target is Detail.
	recordValue := func(v ast.Expr) {
		switch val := v.(type) {
		case *ast.BasicLit:
			if val.Kind != token.STRING {
				t.Fatalf("Detail assigned a non-string literal %s — this test only understands string literals, update it", val.Value)
			}
			s, err := strconv.Unquote(val.Value)
			if err != nil {
				t.Fatalf("unquote Detail literal %s: %v", val.Value, err)
			}
			found[s] = true
		case *ast.BinaryExpr:
			// The one dynamic shape this file uses: "site_" + <connection state>.
			if val.Op != token.ADD {
				t.Fatalf("Detail assigned an unrecognised binary expression (op %v) — update this test to understand it", val.Op)
			}
			prefixLit, ok := val.X.(*ast.BasicLit)
			if !ok || prefixLit.Kind != token.STRING {
				t.Fatalf("Detail assigned a binary expression whose left side is not a string literal — update this test to understand it")
			}
			prefix, err := strconv.Unquote(prefixLit.Value)
			if err != nil {
				t.Fatalf("unquote Detail prefix literal %s: %v", prefixLit.Value, err)
			}
			// Resolve through the real gate, not a hand-copied list: only
			// these connection states can ever reach a "prefix + state" site.
			for _, blocked := range monitoringPauseBlockedStates {
				found[prefix+blocked] = true
			}
		default:
			t.Fatalf("Detail assigned an unrecognised expression kind %T — update this test to understand it", v)
		}
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.KeyValueExpr:
			if isDetailKey(node.Key) {
				recordValue(node.Value)
			}
		case *ast.AssignStmt:
			for i, lhs := range node.Lhs {
				if isDetailSelector(lhs) && i < len(node.Rhs) {
					recordValue(node.Rhs[i])
				}
			}
		}
		return true
	})

	return found
}

// TestMonitoringResultDetailValuesAreDeclaredInTheContract fails if
// monitoring_handler.go can emit a `detail` value the OpenAPI contract does
// not declare on MonitoringResult.detail. gen.MonitoringResultDetail is
// generated FROM openapi.yaml (see AllValues() in oas_schemas_gen.go), so
// this compares the handler's actual source against the actual contract —
// neither side is hand-copied here, so a later addition to either one that
// is not mirrored in the other turns this test red.
func TestMonitoringResultDetailValuesAreDeclaredInTheContract(t *testing.T) {
	emitted := monitoringHandlerDetailLiterals(t)
	if len(emitted) == 0 {
		// A guard that finds nothing must fail loudly: this would otherwise
		// pass vacuously if the parser or the field name silently stopped
		// matching (e.g. the DTO field was renamed).
		t.Fatal("found zero Detail literals in monitoring_handler.go — the parser is not matching this file any more, this must fail rather than pass vacuously")
	}

	declared := make(map[string]bool)
	for _, v := range gen.MonitoringResultDetail("").AllValues() {
		declared[string(v)] = true
	}

	for detail := range emitted {
		if !declared[detail] {
			t.Errorf("monitoring_handler.go can emit detail %q, but MonitoringResult.detail in packages/openapi/openapi.yaml does not declare it — a generated client cannot type-switch on a value its contract never named", detail)
		}
	}
}
