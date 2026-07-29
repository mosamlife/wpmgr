// openapi_request_body_test.go: the GH #307 structural fix. A full-engine
// contract test that checks every hand-written Gin handler actually READS the
// request-body field names its own OpenAPI schema declares.
//
// Why this file exists. The ogen server is generated but never mounted: all
// 359 live routes are hand-written Gin, and packages/openapi/openapi.yaml is a
// hand-maintained document that only the TypeScript client is generated from.
// Nothing in the Go build ever compares the two. So a handler could bind
// `json:"ids"` while its schema declared `log_ids`, and the field simply never
// bound: the endpoint returned 200 with a zero count, the UI toasted "0 log
// entries deleted", and an audit row recorded the no-op as a success. Two
// endpoints (bulk email log delete and bulk resend) shipped that way and were
// invisible to every existing gate, because route-existence tests only compare
// path+method and never look inside the body.
//
// How it works. It reuses buildFullEngine (openapi_route_coverage_test.go) to
// get the REAL route table, then uses gin's RouteInfo.Handler, the fully
// qualified name of the final handler func (e.g.
// ".../internal/email.(*Handler).bulkDeleteLog-fm"), to find that exact method
// in the source with go/ast, extract the json tags of the struct it binds, and
// diff them against the operation's requestBody schema. No Postgres is needed
// (an empty db.Pool is enough: Register only inspects handler-nilness, and no
// request is ever issued), which is why this gate lives in tests/contract and
// runs in CI on every PR rather than in the container-backed tests package.
//
// Both directions fail the build:
//
//   - a spec field the handler never binds (the #307 class: the caller sends
//     it, the server silently ignores it).
//   - a bound field the spec never declares (the mirror image: no generated
//     client can ever set it, so the feature is unreachable through the
//     documented contract).
//
// A rename in either direction therefore trips BOTH checks.
//
// Comparison is STRUCTURAL and nested: the spec tree and the bound-struct tree
// are walked in parallel, one object level at a time, and a mismatch is
// reported by its dotted path ("core_update.new_version"). The walk descends
// into a property only when BOTH sides still describe it: the spec declares
// sub-properties AND the Go type resolves to a struct with fields. Where either
// side stops (an opaque `type: object` in the spec, or a scalar / map /
// json.RawMessage on the Go side) the comparison stops with it, because there
// is nothing left to compare against. That rule is what makes nesting usable:
// a naive deep diff reports every field of a typed Go struct as "extra" against
// an opaque spec level. See compareLevel for the two real cases this handles.
//
// An earlier version of this header claimed nested coverage the code did not
// have (both extractors stopped at the top level), and nothing detected the
// gap. The gate now asserts a floor on how many compared fields were below the
// top level, so that specific regression fails loudly instead of silently
// shrinking the guard.
//
// Every allowlist entry below carries a reason. Add one only when the drift is
// deliberate; the fix is normally to correct the spec or the handler.
package contract

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
)

const apiModulePrefix = "github.com/mosamlife/wpmgr/apps/api/"

// ---------------------------------------------------------------------------
// Allowlists
// ---------------------------------------------------------------------------

// fieldKey identifies one field of one operation's request body.
type fieldKey struct {
	method string
	path   string
	field  string
}

// allowedSpecFieldNotBound: the spec declares the field, the handler
// deliberately does not read it.
var allowedSpecFieldNotBound = map[fieldKey]string{
	{"POST", "/agent/v1/disconnect", "site_id"}: "the schema itself documents site_id as \"echoed for convenience; auth binds the real identity\", so the handler must trust the signed agent identity, never a body-supplied site id",
	{"POST", "/api/v1/sites", "tags"}:           "bound on the production path: POST /sites dispatches to createWithEnrollment (h.conn != nil), which binds createSiteV2Request including tags. This extractor only reads the dispatching func's own body, where the legacy no-lifecycle fallback binds createSiteRequest. The legacy path does drop tags, but it is a dev/no-SSE fallback and the site-first flow is the shipped one",
}

// allowedBoundFieldNotInSpec: the handler reads the field, the spec
// deliberately does not declare it.
var allowedBoundFieldNotInSpec = map[fieldKey]string{
	{"DELETE", "/api/v1/sites/{siteId}/email/log", "ids"}:                     "GH #307 back-compat alias. log_ids is the contract and what every generated client sends; \"ids\" is the name the broken handler used to read, kept accepted so a hand-rolled caller written against the old handler source does not break. Deliberately NOT advertised in the spec",
	{"POST", "/api/v1/sites/{siteId}/email/log/resend", "ids"}:                "GH #307 back-compat alias; see the DELETE .../email/log entry above",
	{"PUT", "/api/v1/sites/{siteId}/security/login-protection", "updated_at"}: "securityConfigDTO is shared by the GET response and the PUT body; updated_at exists so a client can echo the GET payload straight back. It is server-owned and ignored on write, so declaring it on the update schema would advertise a writable field that is not",
}

// unresolvableHandlers: routes whose bound struct this extractor cannot
// determine statically. Each still has a spec body, so the pairing is checked
// by TestOpenAPIRouteCoverage; only the field-level diff is skipped.
//
// Every reason here has been checked against the handler source. A reason that
// merely sounds plausible is worse than none, because it stops the next reader
// re-checking: the archive and revoke entries used to claim "body is optional
// and ignored" when both in fact read `reason` from the body through the shared
// optionalReason helper, and the heartbeat entry blamed a "cross-package alias"
// when the real cause is that the body is bound into an untyped map. Both are
// resolved rather than re-worded below.
var unresolvableHandlers = map[routeKey]string{
	{"POST", "/agent/v1/diagnostics"}:                   "reads the raw body and hands it to the diagnostics service as json.RawMessage; there is no bound struct in the handler to compare",
	{"POST", "/agent/v1/heartbeat"}:                     "binds the body into a map[string]any, not a struct (internal/agent/handler.go: \"the beat is about liveness, not the payload\"), and forwards the whole map to RecordHeartbeat. An untyped map has no json tags, so there is nothing to diff field-by-field",
	{"POST", "/webhooks/billing/{provider}"}:            "raw provider payload: signature is verified over the exact bytes, so the handler must not decode into a struct first",
	{"POST", "/webhooks/email/{provider}/{routeToken}"}: "raw provider payload, verified over exact bytes, same as the billing webhook",
}

// ---------------------------------------------------------------------------
// Spec side
// ---------------------------------------------------------------------------

type specSchemaNode struct {
	Ref        string               `yaml:"$ref"`
	Required   []string             `yaml:"required"`
	Properties map[string]yaml.Node `yaml:"properties"`
	Items      *yaml.Node           `yaml:"items"`
	AllOf      []yaml.Node          `yaml:"allOf"`
	OneOf      []yaml.Node          `yaml:"oneOf"`
	AnyOf      []yaml.Node          `yaml:"anyOf"`
}

type specDocument struct {
	Paths      map[string]map[string]yaml.Node `yaml:"paths"`
	Components struct {
		Schemas map[string]yaml.Node `yaml:"schemas"`
	} `yaml:"components"`
}

type specOperation struct {
	RequestBody struct {
		Content map[string]struct {
			Schema yaml.Node `yaml:"schema"`
		} `yaml:"content"`
	} `yaml:"requestBody"`
}

type specBodyFields struct {
	schema string
	root   *specNode
}

// specNode is ONE object level of a request-body schema: the properties
// declared at that level, which of them are required, and each property's own
// sub-schema.
//
// A node with an empty props map is "opaque": the spec says `type: object` (or
// a bare scalar) without declaring what is inside. Several agent-facing
// schemas do that deliberately, and it is the reason the comparison never
// descends blindly. See compareLevel.
type specNode struct {
	props    map[string]*specNode
	required map[string]bool
}

func newSpecNode() *specNode {
	return &specNode{props: map[string]*specNode{}, required: map[string]bool{}}
}

// merge folds another level into this one, for allOf composition (which is the
// same object level) and for array items. withRequired is false for
// oneOf/anyOf: those are alternatives, so their names count as declared but
// none of them is required.
func (n *specNode) merge(o *specNode, withRequired bool) {
	if o == nil {
		return
	}
	for k, v := range o.props {
		if existing, ok := n.props[k]; ok && existing != nil && v != nil {
			existing.merge(v, true)
			continue
		}
		n.props[k] = v
	}
	if withRequired {
		for k := range o.required {
			n.required[k] = true
		}
	}
}

// tree builds the full nested schema tree, resolving $ref, merging allOf, and
// unwrapping array items (the Go side unwraps slices to their element type, so
// the spec side must unwrap too or the two would compare different levels).
func (d *specDocument) tree(n yaml.Node, ancestors map[string]bool, depth int) *specNode {
	out := newSpecNode()
	if depth > 12 {
		return out
	}
	var s specSchemaNode
	if err := n.Decode(&s); err != nil {
		return out
	}
	if s.Ref != "" {
		name := strings.TrimPrefix(s.Ref, "#/components/schemas/")
		if ancestors[name] {
			return out // cycle guard
		}
		sub, ok := d.Components.Schemas[name]
		if !ok {
			return out
		}
		next := map[string]bool{name: true}
		for k := range ancestors {
			next[k] = true
		}
		return d.tree(sub, next, depth+1)
	}
	for _, r := range s.Required {
		out.required[r] = true
	}
	for k, v := range s.Properties {
		out.props[k] = d.tree(v, ancestors, depth+1)
	}
	for _, sub := range s.AllOf {
		out.merge(d.tree(sub, ancestors, depth+1), true)
	}
	for _, sub := range append(append([]yaml.Node{}, s.OneOf...), s.AnyOf...) {
		out.merge(d.tree(sub, ancestors, depth+1), false)
	}
	if s.Items != nil {
		out.merge(d.tree(*s.Items, ancestors, depth+1), true)
	}
	return out
}

func specRequestBodies(t *testing.T) map[routeKey]specBodyFields {
	t.Helper()
	raw, err := os.ReadFile(specPath(t))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var doc specDocument
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	out := map[routeKey]specBodyFields{}
	for path, ops := range doc.Paths {
		for verb, node := range ops {
			lv := strings.ToLower(verb)
			switch lv {
			case "get", "post", "put", "patch", "delete", "options":
			default:
				continue
			}
			var op specOperation
			if err := node.Decode(&op); err != nil {
				continue
			}
			ct, ok := op.RequestBody.Content["application/json"]
			if !ok {
				continue
			}
			var top specSchemaNode
			_ = ct.Schema.Decode(&top)
			out[routeKey{strings.ToUpper(lv), path}] = specBodyFields{
				schema: strings.TrimPrefix(top.Ref, "#/components/schemas/"),
				root:   doc.tree(ct.Schema, map[string]bool{}, 0),
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Handler side (go/ast)
// ---------------------------------------------------------------------------

// bodyBindFuncs are the calls that decode a JSON request body in this codebase.
// The destination is always the call's last argument.
var bodyBindFuncs = map[string]bool{
	"bindJSON": true, "policyBindJSON": true, "decode": true, "decodeJSON": true,
	"ShouldBindJSON": true, "BindJSON": true, "ShouldBindWith": true,
	"MustBindWith": true, "ShouldBind": true, "Decode": true, "Unmarshal": true,
}

type sourceIndex struct {
	fset *token.FileSet
	pkgs map[string]map[string]*ast.File
}

func newSourceIndex() *sourceIndex {
	return &sourceIndex{fset: token.NewFileSet(), pkgs: map[string]map[string]*ast.File{}}
}

func (si *sourceIndex) files(importPath string) map[string]*ast.File {
	if f, ok := si.pkgs[importPath]; ok {
		return f
	}
	rel := strings.TrimPrefix(importPath, apiModulePrefix)
	if rel == importPath { // outside this module, no source to read
		si.pkgs[importPath] = nil
		return nil
	}
	// tests/contract/ -> tests/ -> apps/api/, the module root the import path
	// is relative to.
	parsed, err := parser.ParseDir(si.fset, filepath.Join("../..", rel), func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		si.pkgs[importPath] = nil
		return nil
	}
	files := map[string]*ast.File{}
	for _, p := range parsed {
		for name, f := range p.Files {
			files[name] = f
		}
	}
	si.pkgs[importPath] = files
	return files
}

// receiverName returns a method's receiver type name ("" for a plain func).
func receiverName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// findFunc matches on name AND receiver type. Matching the receiver is
// load-bearing: internal/agent alone declares three distinct handlers with a
// method named push, and a name-only match silently reads the wrong body.
func (si *sourceIndex) findFunc(importPath, recv, name string) (*ast.FuncDecl, *ast.File) {
	for _, f := range si.files(importPath) {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if ok && fd.Name.Name == name && receiverName(fd) == recv {
				return fd, f
			}
		}
	}
	return nil, nil
}

func (si *sourceIndex) findType(importPath, name string) (ast.Expr, *ast.File) {
	for _, f := range si.files(importPath) {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.Name == name {
					return ts.Type, f
				}
			}
		}
	}
	return nil, nil
}

func importPathForAlias(f *ast.File, alias string) string {
	for _, im := range f.Imports {
		p, err := strconv.Unquote(im.Path.Value)
		if err != nil {
			continue
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if im.Name != nil {
			name = im.Name.Name
		}
		if name == alias {
			return p
		}
	}
	return ""
}

// goNode is ONE object level of the struct a handler binds: the json tag names
// declared at that level, each mapped to its own sub-tree.
//
// A nil child means the field's type does not resolve to a struct in this
// module's source: a scalar, a map[string]any, a type from outside the api
// module, or a json.RawMessage the handler decodes in a separate later pass.
// A nil child is the signal that the Go side stops describing the shape here,
// and compareLevel stops descending with it.
type goNode struct {
	children map[string]*goNode
}

// withKey returns ancestors plus key, copied so that sibling branches do not
// prune each other. A shared set would let the first branch to reach a type
// suppress every later use of it and silently drop real fields.
func withKey(ancestors map[string]bool, key string) map[string]bool {
	next := make(map[string]bool, len(ancestors)+1)
	for k := range ancestors {
		next[k] = true
	}
	next[key] = true
	return next
}

// structTree resolves a type expression into its nested json-tag tree, or nil
// if the type is not a struct declared in this module.
func (si *sourceIndex) structTree(expr ast.Expr, f *ast.File, importPath string, ancestors map[string]bool, depth int) *goNode {
	if expr == nil || depth > 8 {
		return nil
	}
	switch e := expr.(type) {
	case *ast.StarExpr:
		return si.structTree(e.X, f, importPath, ancestors, depth+1)
	case *ast.ArrayType:
		return si.structTree(e.Elt, f, importPath, ancestors, depth+1)
	case *ast.MapType:
		return si.structTree(e.Value, f, importPath, ancestors, depth+1)
	case *ast.Ident:
		key := importPath + "." + e.Name
		if ancestors[key] {
			return nil
		}
		td, tf := si.findType(importPath, e.Name)
		if td == nil {
			return nil
		}
		return si.structTree(td, tf, importPath, withKey(ancestors, key), depth+1)
	case *ast.SelectorExpr:
		id, ok := e.X.(*ast.Ident)
		if !ok {
			return nil
		}
		ip := importPathForAlias(f, id.Name)
		if ip == "" {
			return nil
		}
		key := ip + "." + e.Sel.Name
		if ancestors[key] {
			return nil
		}
		td, tf := si.findType(ip, e.Sel.Name)
		if td == nil {
			return nil // e.g. encoding/json.RawMessage: outside this module
		}
		return si.structTree(td, tf, ip, withKey(ancestors, key), depth+1)
	case *ast.StructType:
		out := &goNode{children: map[string]*goNode{}}
		for _, fld := range e.Fields.List {
			if fld.Tag != nil {
				tv, err := strconv.Unquote(fld.Tag.Value)
				if err != nil {
					continue
				}
				name := strings.Split(reflect.StructTag(tv).Get("json"), ",")[0]
				if name == "" || name == "-" {
					continue
				}
				out.children[name] = si.structTree(fld.Type, f, importPath, ancestors, depth+1)
				continue
			}
			// An embedded (unnamed) field promotes its own fields to THIS
			// level, so its children merge in rather than nesting. A named
			// field without a tag is not part of the JSON contract.
			if len(fld.Names) == 0 {
				if sub := si.structTree(fld.Type, f, importPath, ancestors, depth+1); sub != nil {
					for k, v := range sub.children {
						if _, exists := out.children[k]; !exists {
							out.children[k] = v
						}
					}
				}
			}
		}
		return out
	}
	return nil
}

// boundBodyTree returns the nested json-tag tree of the struct the handler
// decodes the request body into.
//
// It first looks for a bind call in the handler's own body. If there is none,
// it follows ONE level of delegation into the plain same-package functions the
// handler calls, in source order. Several site-lifecycle handlers (revoke,
// archive) never bind inline; they call a shared optionalReason(c) helper that
// decodes the body for them, and without this step their request body would be
// invisible to this gate for no better reason than where the decode was
// written. One level into the same package is enough for every such handler
// here and keeps the search from wandering off into unrelated call graphs.
func (si *sourceIndex) boundBodyTree(fn *ast.FuncDecl, f *ast.File, importPath string) (*goNode, string) {
	tree, note := si.bindTreeInFunc(fn, f, importPath)
	if tree != nil {
		return tree, ""
	}
	for _, callee := range calledPlainFuncs(fn) {
		cf, cfile := si.findFunc(importPath, "", callee)
		if cf == nil {
			continue
		}
		if sub, _ := si.bindTreeInFunc(cf, cfile, importPath); sub != nil {
			return sub, ""
		}
	}
	return nil, note
}

// calledPlainFuncs lists, in source order and deduplicated, the package-level
// functions a handler calls by bare name. Method calls (h.something) are
// excluded on purpose: a handler that dispatches to another handler method is
// choosing between two whole request paths, not delegating its body decode, and
// following it would silently attribute the wrong struct to this route.
func calledPlainFuncs(fn *ast.FuncDecl) []string {
	if fn.Body == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || seen[id.Name] || bodyBindFuncs[id.Name] {
			return true
		}
		seen[id.Name] = true
		out = append(out, id.Name)
		return true
	})
	return out
}

// bindTreeInFunc finds the first request-body bind call inside ONE function and
// resolves its destination type. Only the FIRST bind counts: a function that
// decodes again (the perf agent re-parses the object_cache sub-object in a
// second pass) is reading a nested level, not another top-level body.
func (si *sourceIndex) bindTreeInFunc(fn *ast.FuncDecl, f *ast.File, importPath string) (*goNode, string) {
	if fn.Body == nil {
		return nil, "handler has no body"
	}
	// Local variable name -> declared type expression.
	locals := map[string]ast.Expr{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.DeclStmt:
			gd, ok := s.Decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				return true
			}
			for _, spec := range gd.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok && vs.Type != nil {
					for _, nm := range vs.Names {
						locals[nm.Name] = vs.Type
					}
				}
			}
		case *ast.AssignStmt:
			if len(s.Lhs) != 1 || len(s.Rhs) != 1 {
				return true
			}
			id, ok := s.Lhs[0].(*ast.Ident)
			if !ok {
				return true
			}
			switch r := s.Rhs[0].(type) {
			case *ast.CompositeLit:
				locals[id.Name] = r.Type
			case *ast.UnaryExpr:
				if cl, ok := r.X.(*ast.CompositeLit); ok {
					locals[id.Name] = cl.Type
				}
			}
		}
		return true
	})

	var tree *goNode
	note := "no request-body bind call found"
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if tree != nil {
			return false // primary body already found
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		var fname string
		switch fe := call.Fun.(type) {
		case *ast.Ident:
			fname = fe.Name
		case *ast.SelectorExpr:
			fname = fe.Sel.Name
		}
		if !bodyBindFuncs[fname] {
			return true
		}
		dst := call.Args[len(call.Args)-1]
		if u, isUnary := dst.(*ast.UnaryExpr); isUnary {
			dst = u.X
		}
		var texpr ast.Expr
		switch de := dst.(type) {
		case *ast.Ident:
			texpr = locals[de.Name]
		case *ast.CompositeLit:
			texpr = de.Type
		}
		if texpr == nil {
			note = "could not resolve the type of the bind destination"
			return true
		}
		got := si.structTree(texpr, f, importPath, map[string]bool{}, 0)
		if got == nil {
			note = "could not resolve the bound struct type"
			return true
		}
		tree = got
		return true
	})
	if tree != nil {
		return tree, ""
	}
	return nil, note
}

// ---------------------------------------------------------------------------
// Parallel walk
// ---------------------------------------------------------------------------

// walkStats counts what the walk actually compared. nested (fields below the
// top level) is asserted against a floor by the gate: if a future refactor
// quietly turns the comparison back into a top-level-only diff, that floor
// fails instead of the gate going green while covering less than it claims.
// This exists because the header once described nested coverage the code did
// not have, and nothing detected the gap.
type walkStats struct {
	fields int
	nested int
}

// compareLevel diffs ONE object level of the spec against the matching level of
// the bound struct, then recurses into each property both sides describe.
//
// The descent rule is the whole design, and it is what keeps nesting free of
// phantom mismatches: recurse into a property only when the spec declares
// sub-properties for it AND the Go type resolves to a struct with fields. If
// either side stops describing the shape, the comparison stops with it, because
// there is nothing left to compare against.
//
// Both directions of "stops describing" are real and intentional:
//
//   - spec side: many agent-facing schemas model a sub-object as an opaque
//     `type: object`. Descending there would diff a fully typed Go struct
//     against a level that declares nothing and report every field as extra.
//   - Go side: AgentCacheStatsReport.object_cache is held as json.RawMessage
//     (internal/perf/agent_handler.go) precisely so a malformed block cannot
//     fail the whole-body Unmarshal; it is decoded in a second pass. The
//     handler genuinely does not bind those names at this level, so its
//     sub-tree is nil and the walk stops. That is a property of the type, not
//     an allowlist entry, so it needs no maintenance and cannot rot into a
//     blanket exemption for that route.
//
// path is the dotted prefix ("" at top level, "core_update." one level in), so
// a nested mismatch is reported and allowlisted by its full path.
func compareLevel(k routeKey, schema string, sp *specNode, gp *goNode, path string, st *walkStats, notBound, notInSpec *[]string) {
	specNames := make([]string, 0, len(sp.props))
	for name := range sp.props {
		specNames = append(specNames, name)
	}
	sort.Strings(specNames)

	for _, name := range specNames {
		full := path + name
		st.fields++
		if path != "" {
			st.nested++
		}
		child, gchild := sp.props[name], gp.children[name]
		if _, bound := gp.children[name]; !bound {
			if _, ok := allowedSpecFieldNotBound[fieldKey{k.method, k.path, full}]; ok {
				continue
			}
			kind := "optional"
			if sp.required[name] {
				kind = "REQUIRED"
			}
			*notBound = append(*notBound, fmt.Sprintf("%s %s [%s] spec declares %s field %q, handler never binds it", k.method, k.path, schema, kind, full))
			continue
		}
		if child != nil && len(child.props) > 0 && gchild != nil && len(gchild.children) > 0 {
			compareLevel(k, schema, child, gchild, full+".", st, notBound, notInSpec)
		}
	}

	goNames := make([]string, 0, len(gp.children))
	for name := range gp.children {
		goNames = append(goNames, name)
	}
	sort.Strings(goNames)

	for _, name := range goNames {
		if _, ok := sp.props[name]; ok {
			continue
		}
		full := path + name
		if _, ok := allowedBoundFieldNotInSpec[fieldKey{k.method, k.path, full}]; ok {
			continue
		}
		*notInSpec = append(*notInSpec, fmt.Sprintf("%s %s [%s] handler binds %q, spec never declares it", k.method, k.path, schema, full))
	}
}

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

func TestOpenAPIRequestBodyFieldCoverage(t *testing.T) {
	engine := buildFullEngine(t, &db.Pool{})

	handlerFor := map[routeKey]string{}
	for _, r := range engine.Routes() {
		handlerFor[routeKey{r.Method, normalizeGinPath(r.Path)}] = r.Handler
	}

	si := newSourceIndex()
	var notBound, notInSpec, unresolved []string
	var stats walkStats
	checked := 0

	bodies := specRequestBodies(t)
	keys := make([]routeKey, 0, len(bodies))
	for k := range bodies {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].path != keys[j].path {
			return keys[i].path < keys[j].path
		}
		return keys[i].method < keys[j].method
	})

	for _, k := range keys {
		sb := bodies[k]
		qualified, live := handlerFor[k]
		if !live {
			continue // route-existence drift is TestOpenAPIRouteCoverage's job
		}
		if _, skip := unresolvableHandlers[k]; skip {
			continue
		}

		// ".../internal/email.(*Handler).bulkDeleteLog-fm" -> pkg, recv, func
		name := strings.TrimSuffix(qualified, "-fm")
		dot := strings.LastIndex(name, ".")
		if dot < 0 {
			continue
		}
		funcName, pkgPart := name[dot+1:], name[:dot]
		recv := ""
		if i := strings.Index(pkgPart, ".("); i >= 0 {
			recv = strings.Trim(pkgPart[i+2:], "()*")
			pkgPart = pkgPart[:i]
		}

		fn, file := si.findFunc(pkgPart, recv, funcName)
		if fn == nil {
			unresolved = append(unresolved, fmt.Sprintf("%s %s -> %s (handler func not found in source)", k.method, k.path, qualified))
			continue
		}
		tree, note := si.boundBodyTree(fn, file, pkgPart)
		if note != "" {
			unresolved = append(unresolved, fmt.Sprintf("%s %s -> %s (%s)", k.method, k.path, qualified, note))
			continue
		}
		checked++

		compareLevel(k, sb.schema, sb.root, tree, "", &stats, &notBound, &notInSpec)
	}

	if checked < 100 {
		t.Fatalf("only %d request bodies were actually compared; the extractor is broken, not the code", checked)
	}
	// Floor on nested coverage. The walk currently reaches 39 fields below the
	// top level (core_update.*, agent_self_update.*, disk.*, host_flags.* on
	// the agent metadata push; report sections.*; perf cdn_credentials.*;
	// security thresholds.*). 30 leaves refactoring room while still failing
	// loudly if the descent ever stops happening.
	if stats.nested < 30 {
		t.Fatalf("only %d of %d compared fields were below the top level; nested comparison has regressed, "+
			"so this gate no longer covers the depth its header claims", stats.nested, stats.fields)
	}
	sort.Strings(notBound)
	sort.Strings(notInSpec)
	sort.Strings(unresolved)

	if len(notBound) > 0 {
		t.Errorf("%d request-body field(s) are declared in packages/openapi/openapi.yaml but never read by the handler.\n"+
			"This is the GH #307 class: the caller sends the field, the server ignores it, and the endpoint\n"+
			"reports success while doing nothing. Fix the handler's json tag, or add a justified\n"+
			"allowedSpecFieldNotBound entry:\n  %s", len(notBound), strings.Join(notBound, "\n  "))
	}
	if len(notInSpec) > 0 {
		t.Errorf("%d request-body field(s) are read by a handler but not declared in packages/openapi/openapi.yaml.\n"+
			"No generated client can ever set these, so the behaviour is unreachable through the documented\n"+
			"contract. Declare them in the spec (and regenerate both clients), or add a justified\n"+
			"allowedBoundFieldNotInSpec entry:\n  %s", len(notInSpec), strings.Join(notInSpec, "\n  "))
	}
	if len(unresolved) > 0 {
		t.Errorf("%d handler(s) with a documented JSON request body could not be analysed. Field drift in these\n"+
			"is invisible to this gate. Either bind the body into a struct this extractor can resolve, or add\n"+
			"a justified unresolvableHandlers entry:\n  %s", len(unresolved), strings.Join(unresolved, "\n  "))
	}
	t.Logf("compared %d request bodies against their OpenAPI schemas (%d fields, %d of them nested)", checked, stats.fields, stats.nested)
}
