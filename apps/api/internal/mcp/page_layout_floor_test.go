package mcp

import (
	"regexp"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// lastAgentReleaseWithoutPageLayout is the newest released agent that does
// not build outline grammar v2. When another release lands in this tree
// before the one that ships the layout builder, raise this to that
// release's number in the same commit that brings it in.
const lastAgentReleaseWithoutPageLayout = "0.61.159"

// pageLayoutBuilderMarkers is code the in-tree agent carries when it builds
// outline grammar v2. Each pattern matches code, not a comment.
var pageLayoutBuilderMarkers = []struct {
	file    string
	what    string
	pattern *regexp.Regexp
}{
	{
		file:    "includes/abilities/class-page-create-builder.php",
		what:    "the builder refuses a classic-editor layout",
		pattern: regexp.MustCompile(`self::bad\(\s*'layout_needs_block_editor'`),
	},
	{
		file:    "includes/commands/class-ability-run-command.php",
		what:    "the command resolves the outline's images",
		pattern: regexp.MustCompile(`\$this->pageCreateMedia\(\s*PageCreateBuilder::mediaIds\(\s*\$spec\s*\)\s*\)`),
	},
}

// TestMinAgentVersionForPageLayout_NotAheadOfShippingAgent holds the layout
// floor to the agent this tree ships. The floor names the release that first
// builds outline grammar v2; once the in-tree agent is newer than the last
// release without it, the in-tree agent is that release or a later one, so
// it must be at or above the floor. An agent that ships the builder below
// the floor would have every layout outline refused as agent_outdated.
//
// The floor, its pin (TestMinAgentVersionForPageLayout_Pinned) and m162's
// usage text (TestPageCreateM162UsageNamesTheLayoutFloor) move together.
func TestMinAgentVersionForPageLayout_NotAheadOfShippingAgent(t *testing.T) {
	floor := agentcmd.MinAgentVersionForPageLayout
	if wpversion.Compare(floor, lastAgentReleaseWithoutPageLayout) <= 0 {
		t.Fatalf("MinAgentVersionForPageLayout %q is not above %q, the last release without the layout builder",
			floor, lastAgentReleaseWithoutPageLayout)
	}
	for _, m := range pageLayoutBuilderMarkers {
		if !m.pattern.Match(agentRepoFile(t, m.file)) {
			t.Errorf("apps/agent/%s: %s is missing (pattern %q); the floor names a builder this tree does not carry",
				m.file, m.what, m.pattern)
		}
	}
	shipping := shippingAgentVersion(t)
	if wpversion.Compare(shipping, lastAgentReleaseWithoutPageLayout) > 0 && wpversion.Compare(shipping, floor) < 0 {
		t.Errorf("apps/agent ships %[1]q with the layout builder, newer than %[2]q, but below MinAgentVersionForPageLayout %[3]q: "+
			"every site on %[1]s would have layout outlines refused as agent_outdated. If %[1]s is the release that ships the "+
			"layout builder, set MinAgentVersionForPageLayout, its pin, m162's usage, schema.sql and atlas.sum to %[1]s together. "+
			"If %[1]s shipped without it, raise lastAgentReleaseWithoutPageLayout to %[1]s.",
			shipping, lastAgentReleaseWithoutPageLayout, floor)
	}
}
