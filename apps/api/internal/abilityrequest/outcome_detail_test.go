package abilityrequest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

func detailStr(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}

// TestOutcomeDetailIsTheClosedConflictDetail: outcome_detail carries the
// conflict a page edit was refused with, from the closed set only.
func TestOutcomeDetailIsTheClosedConflictDetail(t *testing.T) {
	row := func(ability, code, text string) sqlc.AssistantAbilityRequest {
		r := sqlc.AssistantAbilityRequest{AbilityName: ability, State: "failed", Outcome: strp(OutcomeRefused)}
		if code != "" {
			r.OutcomeCode = &code
		}
		if text != "" {
			r.SiteReportedText = &text
		}
		return r
	}
	cases := []struct {
		r    sqlc.AssistantAbilityRequest
		want string
	}{
		{row(mcp.AbilityPageEdit, "conflict", "changed_since_read"), "changed_since_read"},
		{row(mcp.AbilityPageEdit, "conflict", "editor_open"), "editor_open"},
		{row(mcp.AbilityPageEdit, "conflict", "autosave_pending"), "autosave_pending"},
		{row(mcp.AbilityPageEdit, "conflict", "someone is editing this post right now"), "<null>"},
		{row(mcp.AbilityPageEdit, "conflict", "Editor_open"), "<null>"},
		{row(mcp.AbilityPageEdit, "conflict", ""), "<null>"},
		{row(mcp.AbilityPageEdit, "preview_changed", "editor_open"), "<null>"},
		{row(mcp.AbilityPageEdit, "", "editor_open"), "<null>"},
		{row(mcp.AbilityRestWrite, "conflict", "editor_open"), "<null>"},
		{row(mcp.AbilityPageCreate, "conflict", "autosave_pending"), "<null>"},
	}
	for _, c := range cases {
		got := toDTO(c.r, false, "", false, setterNames{}).OutcomeDetail
		if detailStr(got) != c.want {
			t.Fatalf("%s %s %q: outcome_detail %s, want %s", c.r.AbilityName, detailStr(c.r.OutcomeCode),
				detailStr(c.r.SiteReportedText), detailStr(got), c.want)
		}
	}
	b, err := json.Marshal(toDTO(row(mcp.AbilityPageEdit, "preview_changed", ""), false, "", false, setterNames{}))
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if v, present := wire["outcome_detail"]; !present || v != nil {
		t.Fatalf("outcome_detail on the wire is %v (present %v), want null", v, present)
	}
}

// TestPageEditConflictDetailIsKeptByTheOutcomePath: the outcome recording
// keeps the site's conflict detail from a direct answer and from a ledger
// answer after a lost reply, so outcome_detail can name it.
func TestPageEditConflictDetailIsKeptByTheOutcomePath(t *testing.T) {
	now := time.Now()
	for _, d := range []string{DetailChangedSinceRead, DetailEditorOpen, DetailAutosavePending} {
		direct := classifyWrite(mcp.AbilityPageEdit, agentcmd.AbilityRunResponse{},
			&agentcmd.AbilityRunRefusal{Code: "conflict", Detail: d}, now)
		stored, ok := outcomeFromStored(mcp.AbilityPageEdit,
			json.RawMessage(`{"ok":false,"outcome":"refused","code":"conflict","detail":"`+d+`"}`), now)
		if !ok {
			t.Fatalf("%s: the ledger answer was not read", d)
		}
		for name, oc := range map[string]writeOutcome{"direct": direct, "ledger": stored} {
			r := sqlc.AssistantAbilityRequest{AbilityName: mcp.AbilityPageEdit, State: "failed",
				Outcome: &oc.outcome, OutcomeCode: oc.code, SiteReportedText: oc.siteText}
			if got := outcomeDetailFor(r); detailStr(got) != d {
				t.Fatalf("%s %s: recorded code %s text %s gives outcome_detail %s", name, d,
					detailStr(oc.code), detailStr(oc.siteText), detailStr(got))
			}
		}
	}
}
