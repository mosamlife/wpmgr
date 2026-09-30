package mcp

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// originOnlyAgentVersionDefineRe extracts WPMGR_AGENT_VERSION from
// apps/agent/wpmgr-agent.php, e.g. `define('WPMGR_AGENT_VERSION', '0.61.153');`.
// This package's own copy of the pattern internal/email pins its floor with;
// the helper there lives in a _test.go file and cannot be imported.
var originOnlyAgentVersionDefineRe = regexp.MustCompile(`define\(\s*'WPMGR_AGENT_VERSION',\s*'([0-9]+(?:\.[0-9]+){1,3})'\s*\)`)

// agentRepoFile reads a file under apps/agent in this checkout.
func agentRepoFile(t *testing.T, rel string) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repo root")
	}
	// thisFile: apps/api/internal/mcp/origin_only_floor_test.go, four levels
	// below the repo root.
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..")
	path := filepath.Join(repoRoot, "apps", "agent", filepath.FromSlash(rel))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return data
}

// shippingAgentVersion is the version apps/agent ships in this checkout, read
// from the plugin header rather than repeated as a literal here.
func shippingAgentVersion(t *testing.T) string {
	t.Helper()
	m := originOnlyAgentVersionDefineRe.FindSubmatch(agentRepoFile(t, "wpmgr-agent.php"))
	if m == nil {
		t.Fatal("WPMGR_AGENT_VERSION define not found in apps/agent/wpmgr-agent.php")
	}
	return string(m[1])
}

// originOnlyHandlerMarkers are the code the in-tree agent must carry for an
// origin-only clear to mean what the control plane sends it to mean: the
// command reads the option, the reach gate honours it, and the result reports
// that it was honoured. Each pattern matches code, not a comment: every one of
// them names a PHP variable.
var originOnlyHandlerMarkers = []struct {
	file    string
	what    string
	pattern *regexp.Regexp
}{
	{
		file:    "includes/commands/class-cache-purge-command.php",
		what:    "the cache_purge command reads origin_only from its params",
		pattern: regexp.MustCompile(`array_key_exists\(\s*'origin_only'\s*,\s*\$params\s*\)`),
	},
	{
		file:    "includes/integrations/class-integration.php",
		what:    "the integration reach gate reads origin_only from the purge options",
		pattern: regexp.MustCompile(`\$options\['origin_only'\]\s*\?\?\s*false\)\s*===\s*true`),
	},
	{
		file:    "includes/cache/class-cache-manager.php",
		what:    "the cache manager reports origin_only_honoured",
		pattern: regexp.MustCompile(`\$result\['origin_only_honoured'\]\s*=\s*true`),
	},
}

// TestMinAgentVersionForOriginOnlyPurge_NotAheadOfShippingAgent pins the floor
// the creation path, approval and dispatch all compare a site's agent against.
//
// The floor names the release that first ships the origin-only clear. An
// agent below it ignores the option, so the clear it runs is not limited to
// the one site a person approved it for.
//
// Two properties are checked against this checkout's apps/agent:
//   - the shipping agent version is at or above the floor, so no site is
//     admitted on a version number this repository has not released with the
//     handler in it;
//   - the shipping agent carries the handler itself.
//
// The floor literal is pinned too, so moving it is a deliberate edit to this
// test rather than a quiet change to the constant.
func TestMinAgentVersionForOriginOnlyPurge_NotAheadOfShippingAgent(t *testing.T) {
	const firstVersionWithOriginOnlyPurge = "0.61.153"

	if MinAgentVersionForOriginOnlyPurge != firstVersionWithOriginOnlyPurge {
		t.Fatalf("MinAgentVersionForOriginOnlyPurge = %q, want %q (the agent release that first ships the origin-only clear); "+
			"if the floor is being re-gated to a later release, update this pin in the same commit",
			MinAgentVersionForOriginOnlyPurge, firstVersionWithOriginOnlyPurge)
	}

	shipping := shippingAgentVersion(t)
	if wpversion.Compare(shipping, MinAgentVersionForOriginOnlyPurge) < 0 {
		t.Errorf("apps/agent ships %q, below MinAgentVersionForOriginOnlyPurge %q: the release carrying the origin-only clear "+
			"must land in this tree, as exactly %q, before this floor can be relied on",
			shipping, MinAgentVersionForOriginOnlyPurge, firstVersionWithOriginOnlyPurge)
	}

	for _, m := range originOnlyHandlerMarkers {
		if !m.pattern.Match(agentRepoFile(t, m.file)) {
			t.Errorf("apps/agent/%s: %s is missing (pattern %q); an agent at or above the floor must carry the origin-only handler",
				m.file, m.what, m.pattern)
		}
	}
}
