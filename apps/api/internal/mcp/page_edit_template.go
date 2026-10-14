package mcp

import (
	"encoding/json"
	"slices"
)

// PageEditCardTemplate is the display evidence approved with a template edit.
// Rule titles are cleaned site text and must be rendered as plain text.
type PageEditCardTemplate struct {
	Type       string              `json:"type"`
	ShowsOn    TemplateEditPlaces  `json:"shows_on"`
	RuleTitles []TemplateRuleTitle `json:"rule_titles"`
}

type TemplateEditPlaces struct {
	Before []string `json:"before"`
	After  []string `json:"after"`
}

func checkPageEditTemplate(pv pageEditPreview, f pageEditFacts) (*PageEditCardTemplate, bool) {
	var asked []string
	for _, op := range f.ops {
		if op.Op == "set_shows_on" {
			asked = op.Rules
		}
	}
	if len(pv.Template) == 0 {
		return nil, len(pv.ShowsOn) == 0 && len(pv.RuleTitles) == 0 && asked == nil
	}
	typ, ok := jsonStringOf(pv.Template)
	if !ok || !ThemeTemplateTypeKnown(typ) {
		return nil, false
	}
	shows, ok := jsonObjectOf(pv.ShowsOn)
	if !ok || !keysExactly(shows, []string{"before", "after"}, "before", "after") {
		return nil, false
	}
	before, ok := CheckThemeTemplateReadRules(shows["before"])
	if !ok {
		return nil, false
	}
	after, ok := CheckThemeTemplateReadRules(shows["after"])
	if !ok {
		return nil, false
	}
	want := before
	if asked != nil {
		want = asked
	}
	if !slices.Equal(want, after) {
		return nil, false
	}
	titles, _, ok := checkTemplateDisplayFacts(append(slices.Clone(before), after...), pv.RuleTitles, []byte("null"))
	if !ok {
		return nil, false
	}
	return &PageEditCardTemplate{Type: typ, ShowsOn: TemplateEditPlaces{Before: before, After: after}, RuleTitles: titles}, true
}

func templateEditDigestParts(parts phpList, tpl *PageEditCardTemplate) phpList {
	if tpl == nil {
		return parts
	}
	before, after := phpList{}, phpList{}
	for _, r := range tpl.ShowsOn.Before {
		before = append(before, r)
	}
	for _, r := range tpl.ShowsOn.After {
		after = append(after, r)
	}
	return append(parts, tpl.Type, before, after)
}

// Display operations do not change the tree. Their echoes are checked before
// the remaining operations enter the classic tree verifier.
func pageEditTreeChanges(raw []byte, f pageEditFacts) ([]byte, pageEditFacts, bool) {
	changes, ok := jsonArrayOf(raw)
	if !ok || len(changes) != len(f.ops) {
		return nil, f, false
	}
	out := make([]json.RawMessage, 0, len(changes))
	ops := make([]pageEditOp, 0, len(f.ops))
	for i, op := range f.ops {
		if op.Op != "set_shows_on" {
			out = append(out, changes[i])
			ops = append(ops, op)
			continue
		}
		c, ok := jsonObjectOf(changes[i])
		if !ok || !keysExactly(c, []string{"op", "after"}, "op", "after") {
			return nil, f, false
		}
		name, ok := jsonStringOf(c["op"])
		rules, problem := CheckThemeTemplateRules(c["after"])
		if !ok || name != op.Op || problem != "" || !slices.Equal(rules, op.Rules) {
			return nil, f, false
		}
	}
	f.ops = ops
	b, err := json.Marshal(out)
	return b, f, err == nil
}
