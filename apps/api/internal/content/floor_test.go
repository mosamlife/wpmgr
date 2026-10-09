package content

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

var agentVersionDefineRe = regexp.MustCompile(`define\(\s*'WPMGR_AGENT_VERSION',\s*'([0-9]+(?:\.[0-9]+){1,3})'\s*\)`)

func agentTreeFile(t *testing.T, rel string) ([]byte, bool) {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repo root")
	}
	// apps/api/internal/content/floor_test.go is four levels below the root.
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

// TestMinAgentVersionForContentProbe_Pinned pins the literal, so moving the
// floor is a deliberate edit to this test and not a quiet change to the
// constant, and checks it is a well-formed version above the last release that
// did not carry the command.
func TestMinAgentVersionForContentProbe_Pinned(t *testing.T) {
	const firstVersionWithContentProbe = "0.61.154"
	const lastVersionWithoutContentProbe = "0.61.153"

	if MinAgentVersionForContentProbe != firstVersionWithContentProbe {
		t.Fatalf("MinAgentVersionForContentProbe = %q, want %q (the agent release that first ships content_probe); "+
			"if the floor is re-gated, update this pin in the same commit", MinAgentVersionForContentProbe, firstVersionWithContentProbe)
	}
	if !agentVersionShape.MatchString(MinAgentVersionForContentProbe) {
		t.Fatalf("floor %q is not a dotted numeric version", MinAgentVersionForContentProbe)
	}
	if wpversion.Compare(MinAgentVersionForContentProbe, lastVersionWithoutContentProbe) <= 0 {
		t.Errorf("floor %q must be above %q, the last release without the command", MinAgentVersionForContentProbe, lastVersionWithoutContentProbe)
	}
}

// TestMinAgentVersionForContentProbe_NotAheadOfShippingAgent holds the floor to
// the agent tree once the command is in it: an agent tree that carries
// content_probe must ship at or above the floor. While the command is not in
// this tree the floor names a release that does not exist yet, and the test
// says so instead of passing quietly.
func TestMinAgentVersionForContentProbe_NotAheadOfShippingAgent(t *testing.T) {
	_, has := agentTreeFile(t, "includes/commands/class-content-probe-command.php")
	if !has {
		t.Skip("content_probe is not in this apps/agent tree yet; the floor names an unreleased agent and no site can meet it until that release ships")
	}
	raw, ok := agentTreeFile(t, "wpmgr-agent.php")
	if !ok {
		t.Fatal("apps/agent/wpmgr-agent.php not found")
	}
	m := agentVersionDefineRe.FindSubmatch(raw)
	if m == nil {
		t.Fatal("WPMGR_AGENT_VERSION define not found")
	}
	if wpversion.Compare(string(m[1]), MinAgentVersionForContentProbe) < 0 {
		t.Errorf("apps/agent carries content_probe but ships %q, below the floor %q: the release must bump the agent to the floor",
			m[1], MinAgentVersionForContentProbe)
	}
}

func TestAgentMeetsFloor(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{MinAgentVersionForContentProbe, true},
		{"0.61.153", false},
		{"0.61.155", true},
		{"0.62.0", true},
		{"1.0.0", true},
		{"", false},
		{"garbage", false},
		{"0.61.154-beta", false},
		{" 0.61.154 ", true},
	}
	for _, c := range cases {
		if got := AgentMeetsFloor(c.v); got != c.want {
			t.Errorf("AgentMeetsFloor(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}
