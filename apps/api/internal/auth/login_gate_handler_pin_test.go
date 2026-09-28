package auth

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestLoginGivesTheChargeBackOnSuccess pins the success give-back in h.login.
//
// Every HTTP-level test in this package runs over a Service with no database,
// so none of them reaches a successful Service.Login, and deleting the call
// would leave them all green. The database-backed proof in apps/api/tests
// (TestLoginAdmissionSuccessfulSignInsAreNeverRefused) exercises it, but CI does
// not run that package, so this pin is what CI sees.
//
// The rule it checks: in h.login's top-level statements, after the one that
// calls h.svc.Login, an unconditional giveBack() call comes before any
// statement that can return, other than the error check. That is "every
// successful sign-in gives its charge back" stated over the source.
func TestLoginGivesTheChargeBackOnSuccess(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "handler.go", nil, 0)
	if err != nil {
		t.Fatalf("parse handler.go: %v", err)
	}
	var login *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "login" && fd.Recv != nil {
			login = fd
		}
	}
	if login == nil {
		t.Fatal("handler.go has no (h *Handler) login; this pin stopped covering it")
	}

	callsMethod := func(n ast.Node, recv, method string) bool {
		found := false
		ast.Inspect(n, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != method {
				return true
			}
			if recv == "" {
				found = true
				return false
			}
			if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == recv {
				found = true
				return false
			}
			return true
		})
		return found
	}
	returns := func(n ast.Node) bool {
		found := false
		ast.Inspect(n, func(n ast.Node) bool {
			if _, ok := n.(*ast.ReturnStmt); ok {
				found = true
			}
			if _, ok := n.(*ast.FuncLit); ok {
				return false
			}
			return !found
		})
		return found
	}
	isErrCheck := func(s ast.Stmt) bool {
		ifs, ok := s.(*ast.IfStmt)
		if !ok || ifs.Init != nil {
			return false
		}
		be, ok := ifs.Cond.(*ast.BinaryExpr)
		if !ok || be.Op != token.NEQ {
			return false
		}
		x, xok := be.X.(*ast.Ident)
		y, yok := be.Y.(*ast.Ident)
		return xok && yok && x.Name == "err" && y.Name == "nil"
	}

	stmts := login.Body.List
	loginAt := -1
	for i, s := range stmts {
		if callsMethod(s, "svc", "Login") {
			loginAt = i
			break
		}
	}
	if loginAt < 0 {
		t.Fatal("h.login no longer calls h.svc.Login at top level; this pin stopped covering it")
	}

	for _, s := range stmts[loginAt+1:] {
		if es, ok := s.(*ast.ExprStmt); ok && callsMethod(es, "", "giveBack") {
			return // reached on every path that got past the error check
		}
		if isErrCheck(s) {
			continue
		}
		if returns(s) {
			t.Fatalf("h.login can return after a successful Service.Login (line %d) without calling giveBack: a successful sign-in would stay charged, and a shared connection would be refused after enough of them",
				fset.Position(s.Pos()).Line)
		}
	}
	t.Fatal("h.login never calls giveBack unconditionally after a successful Service.Login: a successful sign-in would stay charged, and a shared connection would be refused after enough of them")
}
