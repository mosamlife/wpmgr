package abilityrequest

import (
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// designedInput is a stored designed-page input (outline version 3).
const designedInput = `{"post_type":"page","editor":"builder:elementor","elementor_format":"classic","title":"Launch page",` +
	`"outline_version":3,"design":{"colors":{"ground":"#F6F5F2","surface":"#FFFFFF","ink":"#15171C","muted":"#5A606C",` +
	`"accent":"#0D7A63","on_accent":"#FFFFFF"},"fonts":{"heading":"Manrope","body":"Source Sans 3"},` +
	`"scale":{"base_px":17,"ratio":"major_third"},"radius":"md","density":"normal"},"page_frame":"full_width",` +
	`"outline":[{"type":"section","anchor":"top","children":[{"type":"heading","level":2,"text":"Hi"}]}]}`

// TestDesignedPagesFloorRefusesOlderAgent is the dispatch half: an approved
// designed page whose site's plugin is below MinAgentVersionForDesignedPages
// closes not_sent/agent_outdated and is never sent, even on a plugin that
// builds version 2 pages with a page builder; at the floor it goes on. The run
// half is the test of the same name in internal/mcp.
func TestDesignedPagesFloorRefusesOlderAgent(t *testing.T) {
	page := func(input string) sqlc.AssistantAbilityRequest {
		return sqlc.AssistantAbilityRequest{AbilityName: mcp.AbilityPageCreate, InputJson: input}
	}
	if got := agentFloorFor(page(designedInput)); got != agentcmd.MinAgentVersionForDesignedPages {
		t.Fatalf("a designed page's dispatch floor is %s, want %s", got, agentcmd.MinAgentVersionForDesignedPages)
	}
	approved := func(input string) sqlc.GetApprovedAbilityRequestForDispatchRow {
		return sqlc.GetApprovedAbilityRequestForDispatchRow{
			AssistantAbilityRequest: page(input),
			EntryEnabled:            true, EntryHashCurrent: true, RouteEnabled: true, RouteHashCurrent: true,
		}
	}
	on := func(version string) sqlc.Site { return sqlc.Site{AgentVersion: version} }
	builderV2 := `{"post_type":"page","editor":"builder:elementor","title":"T","outline":[{"type":"paragraph","text":"x"}]}`
	cases := []struct {
		name  string
		input string
		site  sqlc.Site
		want  string
	}{
		{"designed page on the last release without it", designedInput, on("0.61.164"), ReasonAgentOutdated},
		{"designed page on the builder floor", designedInput, on(agentcmd.MinAgentVersionForBuilderAdapters), ReasonAgentOutdated},
		{"designed page on no reported version", designedInput, on(""), ReasonAgentOutdated},
		{"designed page at its floor", designedInput, on(agentcmd.MinAgentVersionForDesignedPages), ""},
		{"designed page above its floor", designedInput, on("0.61.170"), ""},
		{"version 2 builder page on the last release without designed pages", builderV2, on("0.61.164"), ""},
	}
	for _, c := range cases {
		if got := notSentBeforeReserve(approved(c.input), c.site, true); got != c.want {
			t.Errorf("%s: reason %q, want %q", c.name, got, c.want)
		}
	}
}
