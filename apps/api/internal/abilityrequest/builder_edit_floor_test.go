package abilityrequest

import (
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// An approved wpmgr/page-edit request is dispatched only to a plugin at the
// builder edit floor: below it the request closes not_sent/agent_outdated.
func TestWorkerPageEditFloor(t *testing.T) {
	row := sqlc.AssistantAbilityRequest{AbilityName: mcp.AbilityPageEdit, InputJson: `{"post_id":418}`}
	if got := agentFloorFor(row); got != agentcmd.MinAgentVersionForBuilderEdit {
		t.Fatalf("page-edit dispatch floor %s, want %s", got, agentcmd.MinAgentVersionForBuilderEdit)
	}
	if agentMeetsFloor(agentcmd.MinAgentVersionForBuilderAdapters, agentFloorFor(row)) {
		t.Fatal("a plugin below the builder edit floor would be sent a page edit")
	}
	if !agentMeetsFloor(agentcmd.MinAgentVersionForBuilderEdit, agentFloorFor(row)) {
		t.Fatal("a plugin at the builder edit floor would not be sent a page edit")
	}
}
