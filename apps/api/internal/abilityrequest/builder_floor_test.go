package abilityrequest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

const elementorInput = `{"post_type":"page","editor":"builder:elementor","title":"T","outline":[{"type":"paragraph","text":"x"}],"elementor_format":"classic"}`

// The dispatch floor of a page built with a page builder is the builder
// floor, whatever its outline: an approved Elementor request whose site's
// plugin is below it closes not_sent/agent_outdated and is never sent, while
// a block-editor layout on the same plugin goes on.
func TestWorkerBuilderRequestOldAgentNotSent(t *testing.T) {
	page := func(input string) sqlc.AssistantAbilityRequest {
		return sqlc.AssistantAbilityRequest{AbilityName: mcp.AbilityPageCreate, InputJson: input}
	}
	if got := agentFloorFor(page(elementorInput)); got != agentcmd.MinAgentVersionForBuilderAdapters {
		t.Fatalf("Elementor page floor %s, want %s", got, agentcmd.MinAgentVersionForBuilderAdapters)
	}
	approved := func(input string) sqlc.GetApprovedAbilityRequestForDispatchRow {
		return sqlc.GetApprovedAbilityRequestForDispatchRow{
			AssistantAbilityRequest: page(input),
			EntryEnabled:            true, EntryHashCurrent: true, RouteEnabled: true, RouteHashCurrent: true,
		}
	}
	on := func(version string) sqlc.Site { return sqlc.Site{AgentVersion: version} }
	layoutFloor := agentcmd.MinAgentVersionForPageLayout
	cases := []struct {
		name  string
		input string
		site  sqlc.Site
		want  string
	}{
		{"Elementor page at the layout floor", elementorInput, on(layoutFloor), ReasonAgentOutdated},
		{"Elementor page on no reported version", elementorInput, on(""), ReasonAgentOutdated},
		{"Elementor page at the builder floor", elementorInput, on(agentcmd.MinAgentVersionForBuilderAdapters), ""},
		{"block-editor layout at the layout floor", layoutInput, on(layoutFloor), ""},
	}
	for _, c := range cases {
		if got := notSentBeforeReserve(approved(c.input), c.site, true); got != c.want {
			t.Errorf("%s: reason %q, want %q", c.name, got, c.want)
		}
	}
}

// A builder write the site refused, read back from the ledger, keeps the
// agent's code, so the card shows that code and not unknown.
func TestOutcomeFromStored_BuilderCodes(t *testing.T) {
	for _, code := range []string{"builder_not_enabled", "builder_not_available", "node_not_supported_by_builder",
		"image_alt_from_library", "builder_save_refused", "builder_crashed"} {
		raw := json.RawMessage(`{"ok":false,"outcome":"refused","code":"` + code + `","detail":"outline[0]"}`)
		oc, ok := outcomeFromStored("", raw, time.Now())
		if !ok || oc.outcome != OutcomeRefused || oc.code == nil || *oc.code != code {
			t.Errorf("%s: outcome %+v (ok %v)", code, oc, ok)
		}
	}
}
