package abilityrequest

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// An edit outcome may report its rules. Only a checked identity is compared
// with approval; display text stays in the request's existing card facts.
func appliedTemplateEditOutcome(hash string, raw []byte, now time.Time) writeOutcome {
	oc := appliedEditOutcome(hash, now)
	if len(raw) == 0 {
		return oc
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return writeOutcome{}
	}
	rules, has := top["shows_on"]
	if !has {
		if _, has := top["template"]; has {
			return writeOutcome{}
		}
		return oc
	}
	checked, ok := mcp.CheckThemeTemplateReadRules(rules)
	if !ok {
		return writeOutcome{}
	}
	typ := ""
	if raw, has := top["template"]; has {
		if json.Unmarshal(raw, &typ) != nil || !mcp.ThemeTemplateTypeKnown(typ) {
			return writeOutcome{}
		}
	}
	oc.template = &createdTemplateIdentity{Type: typ, ShowsOn: checked}
	return oc
}

func editOutcomeMatchingApproval(row sqlc.AssistantAbilityRequest, oc writeOutcome) writeOutcome {
	if oc.outcome != OutcomeApplied || oc.template == nil {
		return oc
	}
	card, ok := mcp.ReadPageEditCardFacts(row.CardFacts)
	if !ok || card.Template == nil || (oc.template.Type != "" && oc.template.Type != card.Template.Type) {
		return writeOutcome{}
	}
	approved, reported := slices.Clone(card.Template.ShowsOn.After), slices.Clone(oc.template.ShowsOn)
	slices.Sort(approved)
	slices.Sort(reported)
	if !slices.Equal(approved, reported) {
		return writeOutcome{}
	}
	oc.template = nil
	return oc
}

func undoTemplateRulesValid(raw []byte) bool {
	if len(raw) == 0 {
		return true
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return false
	}
	if typ, has := top["template"]; has {
		var name string
		if json.Unmarshal(typ, &name) != nil || !mcp.ThemeTemplateTypeKnown(name) {
			return false
		}
	}
	if rules, has := top["shows_on"]; has {
		_, ok := mcp.CheckThemeTemplateReadRules(rules)
		return ok
	}
	return true
}
