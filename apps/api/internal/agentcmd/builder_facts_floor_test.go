package agentcmd

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

var builderFactsAgentVersionDefineRe = regexp.MustCompile(`define\(\s*'WPMGR_AGENT_VERSION',\s*'([0-9]+(?:\.[0-9]+){1,3})'\s*\)`)

func builderFactsAgentTreeFile(t *testing.T, rel string) ([]byte, bool) {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repo root")
	}
	// apps/api/internal/agentcmd/<file>_test.go is four levels below the root.
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..")
	data, err := os.ReadFile(filepath.Join(root, "apps", "agent", filepath.FromSlash(rel)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false
		}
		t.Fatalf("reading apps/agent/%s: %v", rel, err)
	}
	return data, true
}

// TestMinAgentVersionForBuilderFacts_Pinned pins the literal, so moving the
// floor is a deliberate edit to this test and not a quiet change to the
// constant, and checks it is a well-formed version above the last release
// that did not carry builder_facts.
func TestMinAgentVersionForBuilderFacts_Pinned(t *testing.T) {
	const firstVersionWithBuilderFacts = "0.61.159"
	const lastVersionWithoutBuilderFacts = "0.61.158"

	if MinAgentVersionForBuilderFacts != firstVersionWithBuilderFacts {
		t.Fatalf("MinAgentVersionForBuilderFacts = %q, want %q (the agent release that first ships builder_facts); "+
			"if the floor is re-gated to the number that release actually shipped as, update this pin in the same commit",
			MinAgentVersionForBuilderFacts, firstVersionWithBuilderFacts)
	}
	if !regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}$`).MatchString(MinAgentVersionForBuilderFacts) {
		t.Fatalf("floor %q is not a dotted numeric version", MinAgentVersionForBuilderFacts)
	}
	if wpversion.Compare(MinAgentVersionForBuilderFacts, lastVersionWithoutBuilderFacts) <= 0 {
		t.Errorf("floor %q must be above %q, the last release without builder_facts",
			MinAgentVersionForBuilderFacts, lastVersionWithoutBuilderFacts)
	}
}

// TestMinAgentVersionForBuilderFacts_NotAheadOfShippingAgent holds the floor
// to the agent tree once the collector is in it: an agent tree that carries
// the collector must ship at or above the floor. While the collector is not in
// this tree the floor names a release that does not exist yet, and the test
// says so instead of passing quietly.
func TestMinAgentVersionForBuilderFacts_NotAheadOfShippingAgent(t *testing.T) {
	_, has := builderFactsAgentTreeFile(t, "includes/support/class-builder-facts.php")
	if !has {
		t.Skip("the builder_facts collector is not in this apps/agent tree yet; the floor names an unreleased agent and no site can meet it until that release ships")
	}
	raw, ok := builderFactsAgentTreeFile(t, "wpmgr-agent.php")
	if !ok {
		t.Fatal("apps/agent/wpmgr-agent.php not found")
	}
	m := builderFactsAgentVersionDefineRe.FindSubmatch(raw)
	if m == nil {
		t.Fatal("WPMGR_AGENT_VERSION define not found")
	}
	if wpversion.Compare(string(m[1]), MinAgentVersionForBuilderFacts) < 0 {
		t.Errorf("apps/agent carries the builder_facts collector but ships %q, below the floor %q: the release must bump the agent to the floor",
			m[1], MinAgentVersionForBuilderFacts)
	}
}
