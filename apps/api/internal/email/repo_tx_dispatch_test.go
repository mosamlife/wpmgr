// repo_tx_dispatch_test.go: the cheap structural guard on the link that makes
// m112 (GH #380) work at all.
//
// WHY A SOURCE-READING TEST IS WORTH HAVING HERE.
//
// The behavioural proof lives in repo_site_scope_integration_test.go, which
// drives real attacks through the real Repo against a real Postgres. That is
// the test that matters, and it is also the test that needs Docker: on a
// machine without it, every case in that file SKIPS, and the skip is silent.
// This file needs nothing but the source tree, so the invariant keeps being
// checked on the developer laptop and in the pre-Docker stage of CI.
//
// It is written in the spirit of the route-coverage contract test this repo
// already runs: it cannot tell you a decision is correct, it can only refuse to
// let a new one be made silently. Both of the escapes it watches for are real
// and both have happened in this codebase:
//
//   - a new repo method written by copying an old one, before scopedTenantTx
//     existed, and so calling r.pool.InTenantTx directly. That method's queries
//     then run with no app.site_scope, and every m112 policy is inert for it.
//     Nothing else in the suite notices, because the policies themselves are
//     still perfectly correct.
//
//   - scopedTenantTx being simplified away in a refactor ("this just calls
//     InTenantTx most of the time"), which turns the whole domain back into its
//     pre-m112 state in one commit.
//
// The allowlist below is the point. Bypassing scopedTenantTx is sometimes
// right, and this test does not forbid it; it forbids doing it WITHOUT saying
// so here, next to the reason.
package email

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// directTenantTxAllowed lists the Repo methods that may call r.pool.InTenantTx
// themselves instead of going through scopedTenantTx, with the reason each one
// is exempt. A method not on this list that calls InTenantTx directly is a
// silent hole in the site-scope boundary.
//
// Both current entries touch email_notify_settings, which is a single per-TENANT
// row with no site_id column at all. It carries no m112 policy because there is
// no site to scope it to, and routing it through scopedTenantTx would set GUCs
// that nothing reads. Adding a site-keyed table to either of these methods means
// removing it from this list, not extending the list.
var directTenantTxAllowed = map[string]string{
	"GetNotifySettings":    "email_notify_settings is per-tenant, not per-site: no site_id column, no m112 policy",
	"UpsertNotifySettings": "email_notify_settings is per-tenant, not per-site: no site_id column, no m112 policy",
}

// TestEveryOperatorPathRunsThroughScopedTenantTx parses repo.go and reports any
// method that reaches for a transaction wrapper directly without being declared
// above.
//
// InAgentTx is deliberately NOT policed. It is the agent and cross-tenant worker
// path; it sets app.agent='on', never sets app.site_scope, and the permissive
// _agent policies exist precisely to let it through. Forcing it onto
// scopedTenantTx would break fleet-wide mail reporting, which
// TestAgentPathThroughTheRepoIsUntouchedByM112 asserts from the other direction.
func TestEveryOperatorPathRunsThroughScopedTenantTx(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "repo.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse repo.go: %v", err)
	}

	var offenders []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		name := fn.Name.Name
		if name == "scopedTenantTx" {
			// The helper itself is the one place InTenantTx belongs.
			continue
		}
		if !callsPoolMethod(fn.Body, "InTenantTx") && !callsPoolMethod(fn.Body, "InTenantTxAsUser") &&
			!callsPoolMethod(fn.Body, "InScopedTenantTx") {
			continue
		}
		if _, allowed := directTenantTxAllowed[name]; allowed {
			continue
		}
		offenders = append(offenders, name)
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("these Repo methods open a tenant transaction directly instead of through "+
			"scopedTenantTx: %s\n\n"+
			"A query that does not go through scopedTenantTx runs with no app.site_scope GUC, "+
			"which makes every m112 RESTRICTIVE policy on site_email_config, "+
			"site_email_connection, site_email_log and email_suppression a no-op for that "+
			"query. A site-scoped collaborator would reach the organisation's row through it, "+
			"which is GH #380 reopened.\n\n"+
			"If the bypass is deliberate, add the method to directTenantTxAllowed in this file "+
			"with the reason it is safe.", strings.Join(offenders, ", "))
	}
}

// TestScopedTenantTxStillDispatchesOnSiteScope is the other half of the guard.
// The list check above passes trivially if scopedTenantTx is gutted to a plain
// InTenantTx call, because then nobody bypasses it and there is nothing left to
// bypass. This asserts the helper still contains the dispatch: a site-scoped
// principal routed onto InScopedTenantTx.
//
// It reads the source rather than calling the function because the dispatch is
// only observable through the GUCs it sets, and asserting on those needs the
// database (which is what the integration file does). Between them the two are
// complete: this one fails without Docker the moment the dispatch is deleted;
// that one fails with Docker the moment the dispatch stops WORKING.
func TestScopedTenantTxStillDispatchesOnSiteScope(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "repo.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse repo.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "scopedTenantTx" && fn.Recv != nil {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("Repo.scopedTenantTx is gone. It is the only thing in this package that sets " +
			"app.site_scope, and without it every m112 policy on the four email tables is " +
			"decoration: a site-scoped collaborator reaches the organisation's config again. " +
			"If it moved, move this test with it.")
	}
	if !callsPoolMethod(body, "InScopedTenantTx") {
		t.Fatal("Repo.scopedTenantTx no longer calls InScopedTenantTx, so nothing sets " +
			"app.site_scope on the email path and the m112 policies never activate")
	}
	if !mentionsIdent(body, "ScopeSite") {
		t.Fatal("Repo.scopedTenantTx no longer tests for domain.ScopeSite, so its dispatch " +
			"cannot be selecting site-scoped principals")
	}
}

// callsPoolMethod reports whether the block contains a call of the form
// <something>.pool.<method>(...).
func callsPoolMethod(body *ast.BlockStmt, method string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		outer, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || outer.Sel.Name != method {
			return true
		}
		// The receiver must be `<x>.pool`, so a same-named method on some other
		// value does not count.
		if inner, ok := outer.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "pool" {
			found = true
			return false
		}
		return true
	})
	return found
}

// mentionsIdent reports whether the block references the given identifier.
func mentionsIdent(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}
