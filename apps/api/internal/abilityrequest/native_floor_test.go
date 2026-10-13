package abilityrequest

import (
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

func TestNativeDispatchFloor(t *testing.T) {
	for _, op := range []string{"add_native", "set_native", "duplicate"} {
		row := sqlc.AssistantAbilityRequest{AbilityName: "wpmgr/page-edit", InputJson: `{"operations":[{"op":"` + op + `"}]}`}
		if got := agentFloorFor(row); got != "0.61.166" {
			t.Errorf("%s dispatch floor=%s", op, got)
		}
		approved := sqlc.GetApprovedAbilityRequestForDispatchRow{AssistantAbilityRequest: row, EntryEnabled: true, EntryHashCurrent: true, RouteEnabled: true, RouteHashCurrent: true}
		if got := notSentBeforeReserve(approved, sqlc.Site{AgentVersion: "0.61.165"}, true); got != ReasonAgentOutdated {
			t.Errorf("%s dispatch below floor reason=%s", op, got)
		}
		if got := notSentBeforeReserve(approved, sqlc.Site{AgentVersion: "0.61.166"}, true); got != "" {
			t.Errorf("%s dispatch at floor reason=%s", op, got)
		}
		if agentMeetsFloor("0.61.165", agentFloorFor(row)) {
			t.Errorf("%s dispatch permits old agent", op)
		}
	}
	row := sqlc.AssistantAbilityRequest{AbilityName: "wpmgr/page-edit", InputJson: `{"operations":[{"op":"set_text"}]}`}
	if got := agentFloorFor(row); got != agentcmd.MinAgentVersionForBuilderEdit {
		t.Errorf("legacy dispatch floor=%s", got)
	}
	for _, name := range []string{"wpmgr/elementor-widgets", "wpmgr/elementor-widget-schema"} {
		if got := agentFloorFor(sqlc.AssistantAbilityRequest{AbilityName: name}); got != "0.61.166" {
			t.Errorf("%s floor=%s", name, got)
		}
	}
}
