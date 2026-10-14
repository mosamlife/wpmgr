package abilityrequest

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// PageTemplateDTO is the template evidence checked at precheck and retained in the card.
// Titles are cleaned site text, to be rendered as plain text.
type PageTemplateDTO struct {
	Type           string                  `json:"type"`
	ShowsOn        []string                `json:"shows_on"`
	RuleTitles     []mcp.TemplateRuleTitle `json:"rule_titles"`
	AlsoLive       []mcp.TemplateLive      `json:"also_live"`
	RefusalMessage *string                 `json:"refusal_message"`
}

func pageTemplateFor(r sqlc.AssistantAbilityRequest) *PageTemplateDTO {
	if r.AbilityName != mcp.AbilityPageCreate || r.ContentClearedAt.Valid {
		return nil
	}
	template, ok := mcp.ReadPageCardTemplate(r.CardFacts)
	if !ok {
		return nil
	}
	out := &PageTemplateDTO{Type: template.Type, ShowsOn: template.ShowsOn, RuleTitles: template.RuleTitles, AlsoLive: template.AlsoLive}
	if r.OutcomeCode != nil && r.Outcome != nil {
		detail := ""
		if r.SiteReportedText != nil {
			detail = *r.SiteReportedText
		}
		if message := mcp.ThemeTemplateRefusalMessage(*r.OutcomeCode, detail, r.SiteLabel); message != "" {
			out.RefusalMessage = &message
		}
	}
	return out
}

// createdTemplateIdentity is used only to compare the outcome with its approval.
type createdTemplateIdentity struct {
	Type    string
	ShowsOn []string
}

func createdTemplateOutcome(postID int64, raw []byte, now time.Time) writeOutcome {
	oc := createdOutcome(postID, now)
	if len(raw) == 0 {
		return oc
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return writeOutcome{}
	}
	if _, has := top["template"]; !has {
		if _, has := top["shows_on"]; has {
			return writeOutcome{}
		}
		return oc
	}
	var typ string
	if json.Unmarshal(top["template"], &typ) != nil || !mcp.ThemeTemplateTypeKnown(typ) {
		return writeOutcome{}
	}
	rules, problem := mcp.CheckThemeTemplateRules(top["shows_on"])
	if problem != "" {
		return writeOutcome{}
	}
	oc.template = &createdTemplateIdentity{Type: typ, ShowsOn: rules}
	return oc
}

func outcomeMatchingApproval(row sqlc.AssistantAbilityRequest, oc writeOutcome) writeOutcome {
	if row.AbilityName == mcp.AbilityPageEdit {
		return editOutcomeMatchingApproval(row, oc)
	}
	if row.AbilityName != mcp.AbilityPageCreate || oc.outcome != OutcomeCreated {
		return oc
	}
	approved, hasTemplate := mcp.ReadPageCardTemplate(row.CardFacts)
	if !hasTemplate && oc.template == nil {
		return oc
	}
	if !hasTemplate || oc.template == nil || approved.Type != oc.template.Type {
		return writeOutcome{}
	}
	asked, stored := slices.Clone(approved.ShowsOn), slices.Clone(oc.template.ShowsOn)
	slices.Sort(asked)
	slices.Sort(stored)
	if !slices.Equal(asked, stored) {
		return writeOutcome{}
	}
	oc.template = nil
	return oc
}

// Only a ledger outcome matching its recorded approval is a decided result.
func ledgerVerdictForRequest(row sqlc.AssistantAbilityRequest, resp agentcmd.AbilityRunResponse, unknownFor time.Duration, now time.Time) (writeOutcome, bool) {
	oc, decided := ledgerVerdict(row.AbilityName, resp, unknownFor, now)
	if !decided {
		return oc, false
	}
	oc = outcomeMatchingApproval(row, oc)
	return oc, oc.outcome != ""
}
