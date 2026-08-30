package main

import (
	"sort"
	"testing"
)

// TestBuildEngine_NoBlindSpotRoutes is the regression test for the defect
// this command exists to fix: tests/contract/openapi_route_coverage_test.go's
// buildFullEngine left MCPDiscoveryH (and, on closer inspection while
// building this command, MCPTransportH, MCPOAuthH and
// BillingSuspensionGate) nil, so server.New's "if deps.X != nil" guards
// silently dropped their routes from engine.Routes(). A url-map coverage
// gate built on that engine would report full coverage while these routes
// stayed invisible to it.
//
// This test asserts the dump contains exactly the routes that nil-handler
// blind spot hid, plus the two system routes, so a future change that
// re-introduces a nil optional field here (or removes one of these routes'
// registration) is caught here rather than discovered as a live 404.
func TestBuildEngine_NoBlindSpotRoutes(t *testing.T) {
	engine, omitted, err := buildEngine()
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	if len(omitted) > 0 {
		t.Errorf("buildEngine left %d server.Deps field(s) nil, so their routes are NOT in this dump: %v", len(omitted), omitted)
	}

	lines := dumpRoutes(engine)
	set := make(map[string]bool, len(lines))
	for _, l := range lines {
		set[l] = true
	}

	required := []string{
		"GET\t/.well-known/oauth-authorization-server",
		"GET\t/.well-known/oauth-protected-resource",
		"GET\t/.well-known/oauth-protected-resource/mcp",
		"POST\t/mcp",
		"GET\t/healthz",
		"GET\t/readyz",
	}
	for _, r := range required {
		if !set[r] {
			t.Errorf("required route missing from dump: %q", r)
		}
	}
}

// TestDumpRoutes_NonEmpty guards against the empty-dump failure mode
// specifically: an engine that builds without error but yields zero routes
// must never be mistaken for "nothing to report" by a consumer.
func TestDumpRoutes_NonEmpty(t *testing.T) {
	engine, _, err := buildEngine()
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	lines := dumpRoutes(engine)
	if len(lines) == 0 {
		t.Fatal("dumpRoutes returned zero routes for a successfully built engine — this must never happen")
	}
}

// TestDumpRoutes_RouteCountRegression is a loud tripwire on the total route
// count: a route count that drops is exactly the kind of silent regression
// (a handler dropped from server.Deps, a Register call deleted) this command
// exists to make visible. minRoutes is a floor, not an exact match — deliberately
// generous — so the test fails loudly on a real drop instead of needing a
// hand-maintained exact count that drifts on every legitimate addition (see
// CLAUDE.md's "never hard-code a count" rule: this asserts a floor, not the
// literal count, for that reason).
func TestDumpRoutes_RouteCountRegression(t *testing.T) {
	engine, _, err := buildEngine()
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	lines := dumpRoutes(engine)

	const minRoutes = 300 // measured at 442 when this test was written; see routes.go's buildEngine
	if len(lines) < minRoutes {
		t.Errorf("route count regression: got %d routes, want at least %d — a handler likely dropped out of server.Deps or lost its Register call", len(lines), minRoutes)
	}
}

// TestDumpRoutes_SortedAndDeduped locks in the output contract a consumer
// gate relies on: sorted, stable, no duplicate lines.
func TestDumpRoutes_SortedAndDeduped(t *testing.T) {
	engine, _, err := buildEngine()
	if err != nil {
		t.Fatalf("buildEngine: %v", err)
	}
	lines := dumpRoutes(engine)

	if !sort.StringsAreSorted(lines) {
		t.Error("dumpRoutes output is not sorted")
	}
	seen := map[string]bool{}
	for _, l := range lines {
		if seen[l] {
			t.Errorf("duplicate route line in dump: %q", l)
		}
		seen[l] = true
	}
}

// TestNormalizeGinPath pins the :param -> {param} rewrite the dump relies on
// to match OpenAPI's path syntax.
func TestNormalizeGinPath(t *testing.T) {
	cases := map[string]string{
		"/api/v1/sites/:siteId":                   "/api/v1/sites/{siteId}",
		"/api/v1/sites/:siteId/backups/:backupId": "/api/v1/sites/{siteId}/backups/{backupId}",
		"/healthz":                              "/healthz",
		"/.well-known/oauth-protected-resource": "/.well-known/oauth-protected-resource",
	}
	for in, want := range cases {
		if got := normalizeGinPath(in); got != want {
			t.Errorf("normalizeGinPath(%q) = %q, want %q", in, got, want)
		}
	}
}
