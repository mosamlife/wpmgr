package abilityrequest

import (
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// A stored wpmgr/page-edit input as the run path stores it: the operations the
// AI sent, and the design when it sent one or the draft gave it.
const (
	editFingerprint = "7787514f2667f5577151c00759c33e4553edc1d75f96e0dc1ddcdc23989a5ea7"
	editDesign      = `{"colors":{"ground":"#F6F5F2","surface":"#FFFFFF","ink":"#15171C","muted":"#5A606C",` +
		`"accent":"#0D7A63","on_accent":"#FFFFFF"},"fonts":{"heading":"Manrope","body":"Source Sans 3"},` +
		`"scale":{"base_px":17,"ratio":"major_third"},"radius":"md","density":"normal"}`
	editParagraph = `{"op":"insert","after":"3c4d5e6","outline":[{"type":"paragraph","text":"Added"}]}`
	editSection   = `{"op":"insert","after":"3c4d5e6","outline":[{"type":"section","anchor":"press",` +
		`"children":[{"type":"paragraph","text":"Press"}]}]}`
	editPattern = `{"op":"insert","after":"3c4d5e6","outline":[{"type":"pattern","pattern":"faq",` +
		`"variant":"accordion","faq":[{"question":"Q?","answer":"A."}]}]}`
	editPathText  = `{"op":"set_text","ref":"3c4d5e6","field":"plans.1.price","text":"49"}`
	editPlainText = `{"op":"set_text","ref":"3c4d5e6","field":"text","text":"One"}`
)

func storedEdit(operation, extra string) string {
	in := `{"post_id":418,"base_fingerprint":"` + editFingerprint + `","operations":[` + operation + `]`
	if extra != "" {
		in += "," + extra
	}
	return in + "}"
}

// TestPageEditDesignedFloorRefusesOlderAgent is the dispatch half of the
// per-input floor of wpmgr/page-edit: an approved edit that carries a design,
// adds a section or a pattern, or sets text by a pattern path, whose site's
// plugin is below MinAgentVersionForDesignedPages, closes
// not_sent/agent_outdated and is never sent; at the floor it goes on. An edit
// with none of these keeps the floor of the entry. The run half is the test of
// the same name in internal/mcp.
func TestPageEditDesignedFloorRefusesOlderAgent(t *testing.T) {
	edit := func(input string) sqlc.AssistantAbilityRequest {
		return sqlc.AssistantAbilityRequest{AbilityName: mcp.AbilityPageEdit, InputJson: input}
	}
	approved := func(input string) sqlc.GetApprovedAbilityRequestForDispatchRow {
		return sqlc.GetApprovedAbilityRequestForDispatchRow{
			AssistantAbilityRequest: edit(input),
			EntryEnabled:            true, EntryHashCurrent: true, RouteEnabled: true, RouteHashCurrent: true,
		}
	}
	on := func(version string) sqlc.Site { return sqlc.Site{AgentVersion: version} }
	own, inherited := storedEdit(editSection, `"design":`+editDesign), storedEdit(editParagraph, `"design":`+editDesign)
	cases := []struct {
		name  string
		input string
		floor string
	}{
		{"a design the AI sent", own, agentcmd.MinAgentVersionForDesignedPages},
		{"a design the draft gave", inherited, agentcmd.MinAgentVersionForDesignedPages},
		{"a pattern", storedEdit(editPattern, ""), agentcmd.MinAgentVersionForDesignedPages},
		{"a pattern path", storedEdit(editPathText, ""), agentcmd.MinAgentVersionForDesignedPages},
		{"a paragraph", storedEdit(editParagraph, ""), agentcmd.MinAgentVersionForBuilderEdit},
		{"a plain set_text", storedEdit(editPlainText, ""), agentcmd.MinAgentVersionForBuilderEdit},
		{"no operations", `{"post_id":418}`, agentcmd.MinAgentVersionForBuilderEdit},
	}
	for _, c := range cases {
		if got := agentFloorFor(edit(c.input)); got != c.floor {
			t.Errorf("%s: dispatch floor %s, want %s", c.name, got, c.floor)
		}
	}
	for _, c := range []struct {
		name  string
		input string
		site  sqlc.Site
		want  string
	}{
		{"designed edit on the last release without it", own, on("0.61.164"), ReasonAgentOutdated},
		{"designed edit on the builder edit floor", own, on(agentcmd.MinAgentVersionForBuilderEdit), ReasonAgentOutdated},
		{"designed edit on no reported version", own, on(""), ReasonAgentOutdated},
		{"inherited design on the last release without it", inherited, on("0.61.164"), ReasonAgentOutdated},
		{"pattern path on the last release without it", storedEdit(editPathText, ""), on("0.61.164"), ReasonAgentOutdated},
		{"designed edit at its floor", own, on(agentcmd.MinAgentVersionForDesignedPages), ""},
		{"designed edit above its floor", own, on("0.61.170"), ""},
		{"plain edit on the builder edit floor", storedEdit(editParagraph, ""), on(agentcmd.MinAgentVersionForBuilderEdit), ""},
		{"plain edit below the builder edit floor", storedEdit(editParagraph, ""), on("0.61.162"), ReasonAgentOutdated},
	} {
		if got := notSentBeforeReserve(approved(c.input), c.site, true); got != c.want {
			t.Errorf("%s: reason %q, want %q", c.name, got, c.want)
		}
	}
}
