package aireadiness

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The readiness checklist is advice to a person. It must never gate anything:
// not a tool call, an approval, a dispatch, an agent command. The way that
// property is kept is structural: only the wiring layer (the server's route
// table and the binary's main) may import this package, so nothing on the
// execution path can read a Result, directly or through another package.
//
// A site can forge what feeds the checklist (register an ability under a
// builder's namespace, send a made-up builder_facts). That is harmless exactly
// because the worst it can do is mislead a display.

const apiModulePrefix = "github.com/mosamlife/wpmgr/apps/api/"

const readinessPkg = "internal/aireadiness"

// wiringPackages are the only packages allowed to import the readiness
// package: they mount its routes and construct it.
var wiringPackages = map[string]bool{
	"internal/server": true,
	"cmd/wpmgr":       true,
	"cmd/dump-routes": true, // builds the same route table to list it; serves nothing
}

// apiRoot is apps/api, found from this file's location.
func apiRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate apps/api")
	}
	// apps/api/internal/aireadiness/advisory_test.go
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// importGraph maps every package under internal/ and cmd/ (non-test files
// only) to the in-module packages it imports.
func importGraph(t *testing.T, root string) map[string][]string {
	t.Helper()
	graph := map[string][]string{}
	fset := token.NewFileSet()
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			pkg := filepath.ToSlash(rel)
			f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			if _, ok := graph[pkg]; !ok {
				graph[pkg] = nil
			}
			for _, imp := range f.Imports {
				p, err := strconv.Unquote(imp.Path.Value)
				if err != nil || !strings.HasPrefix(p, apiModulePrefix) {
					continue
				}
				graph[pkg] = append(graph[pkg], strings.TrimPrefix(p, apiModulePrefix))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scanning %s: %v", top, err)
		}
	}
	return graph
}

// violations returns one line per way a package outside allowed reaches
// target, directly or through other packages.
func violations(graph map[string][]string, target string, allowed map[string]bool) []string {
	var out []string
	pkgs := make([]string, 0, len(graph))
	for p := range graph {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, start := range pkgs {
		if start == target || allowed[start] {
			continue
		}
		// Breadth-first, remembering each package's parent for the report.
		parent := map[string]string{start: ""}
		queue := []string{start}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, next := range graph[cur] {
				if _, seen := parent[next]; seen {
					continue
				}
				parent[next] = cur
				if next == target {
					chain := []string{next}
					for p := cur; p != ""; p = parent[p] {
						chain = append([]string{p}, chain...)
					}
					out = append(out, strings.Join(chain, " -> "))
					continue
				}
				queue = append(queue, next)
			}
		}
	}
	return out
}

func TestNothingOnTheExecutionPathImportsAIReadiness(t *testing.T) {
	graph := importGraph(t, apiRoot(t))

	// Positive controls: a scan that finds nothing must fail, not pass. The
	// wiring layer does import the package, and the package does import the
	// contract constants it compares against.
	if len(graph) < 20 {
		t.Fatalf("the import scan found only %d packages; it is not reading the tree", len(graph))
	}
	imports := func(from, to string) bool {
		for _, p := range graph[from] {
			if p == to {
				return true
			}
		}
		return false
	}
	if !imports("internal/server", readinessPkg) {
		t.Fatal("positive control failed: internal/server must import the readiness package; the scan is not seeing imports")
	}
	if !imports(readinessPkg, "internal/agentcmd") {
		t.Fatal("positive control failed: the readiness package imports agentcmd for its floors; the scan is not seeing imports")
	}

	if v := violations(graph, readinessPkg, wiringPackages); len(v) > 0 {
		t.Fatalf("the AI readiness checklist is advisory and must not be reachable from anything but the wiring layer; found:\n  %s",
			strings.Join(v, "\n  "))
	}

	// The execution-path packages are named too, so a failure reads as what it
	// is, and so renaming one of them cannot quietly drop it from the check.
	for _, name := range []string{
		"internal/mcp", "internal/abilityrequest", "internal/abilities", "internal/agentcmd",
		"internal/assistantrequest", "internal/agent", "internal/update",
	} {
		if _, ok := graph[name]; !ok {
			t.Errorf("%s is not in the import scan; it moved or the scan is incomplete", name)
		}
	}
}

// The scan above is only worth anything if it can fail, so it is run against
// graphs that contain each way of breaking the rule.
func TestViolationsFindsEveryWayToReachTheTarget(t *testing.T) {
	allowed := map[string]bool{"wiring": true}
	cases := []struct {
		name  string
		graph map[string][]string
		want  int
	}{
		{"clean", map[string][]string{"mcp": {"agentcmd"}, "agentcmd": nil, "target": {"agentcmd"}, "wiring": {"target"}}, 0},
		{"direct import", map[string][]string{"mcp": {"target"}, "target": nil, "wiring": {"target"}}, 1},
		{"through another package", map[string][]string{"mcp": {"helper"}, "helper": {"target"}, "target": nil}, 2},
		{"a long chain", map[string][]string{"a": {"b"}, "b": {"c"}, "c": {"target"}, "target": nil}, 3},
		{"the wiring layer may import it", map[string][]string{"wiring": {"target"}, "target": nil}, 0},
		{"a package reaching it only through the wiring layer is a violation", map[string][]string{"mcp": {"wiring"}, "wiring": {"target"}, "target": nil}, 1},
		{"an import cycle does not hang", map[string][]string{"a": {"b"}, "b": {"a", "target"}, "target": nil}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := violations(c.graph, "target", allowed); len(got) != c.want {
				t.Fatalf("got %d violations %v, want %d", len(got), got, c.want)
			}
		})
	}
}
