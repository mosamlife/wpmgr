package mcp

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/mosamlife/wpmgr/apps/api/internal/humantext"
)

var themeTemplateRule = regexp.MustCompile(`^(include|exclude)(/[a-z0-9_-]{1,64}){1,2}(/[1-9][0-9]{0,19})?$`)
var themeTemplateID = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)

// ThemeTemplateTypeKnown names the template types page-create can draft.
func ThemeTemplateTypeKnown(t string) bool { return t == "header" || t == "footer" }

// CheckThemeTemplateRules checks the curated places and preserves request order.
func CheckThemeTemplateRules(raw []byte) ([]string, string) {
	list, ok := jsonArrayOf(raw)
	if !ok || len(list) > 20 {
		return nil, "must be a list of at most 20 unique rules"
	}
	out := make([]string, 0, len(list))
	seen := map[string]bool{}
	for i, value := range list {
		rule, ok := jsonStringOf(value)
		if !ok || !themeTemplateRule.MatchString(rule) || seen[rule] {
			return nil, fmt.Sprintf("shows_on[%d] must be a unique include/exclude rule using a v1 place and an optional positive decimal id", i)
		}
		parts := strings.Split(rule, "/")[1:]
		hasID := themeTemplateID.MatchString(parts[len(parts)-1])
		if hasID {
			parts = parts[:len(parts)-1]
		}
		place := strings.Join(parts, "/")
		switch place {
		case "singular/page", "singular/post":
		case "general", "singular/front_page", "singular/not_found404", "archive":
			if hasID {
				return nil, fmt.Sprintf("shows_on[%d] allows an id only on singular/page or singular/post", i)
			}
		default:
			return nil, fmt.Sprintf("shows_on[%d] place must be general, singular/front_page, singular/page, singular/post, singular/not_found404 or archive", i)
		}
		seen[rule] = true
		out = append(out, rule)
	}
	return out, ""
}

// PageCardTemplate binds the template type and requested rules into approval.
type PageCardTemplate struct {
	Type       string              `json:"type"`
	ShowsOn    []string            `json:"shows_on"`
	RuleTitles []TemplateRuleTitle `json:"rule_titles"`
	AlsoLive   []TemplateLive      `json:"also_live"`
}

func readPageCardTemplate(raw []byte) (PageCardTemplate, bool) {
	m, ok := jsonObjectOf(raw)
	if !ok || len(m) != 4 {
		return PageCardTemplate{}, false
	}
	typ, ok := jsonStringOf(m["type"])
	rules, problem := CheckThemeTemplateRules(m["shows_on"])
	if !ok || !ThemeTemplateTypeKnown(typ) || problem != "" {
		return PageCardTemplate{}, false
	}
	titles, live, ok := checkTemplateDisplayFacts(rules, m["rule_titles"], m["also_live"])
	return PageCardTemplate{Type: typ, ShowsOn: rules, RuleTitles: titles, AlsoLive: live}, ok
}

// ReadPageCardTemplate reads the checked template approval facts.
func ReadPageCardTemplate(raw []byte) (PageCardTemplate, bool) {
	top, ok := readPageCardTop(raw)
	if !ok {
		return PageCardTemplate{}, false
	}
	return readPageCardTemplate(top["template"])
}

// CheckThemeTemplateReadRules accepts the site's display-rule read grammar.
// Existing published templates may use registered places beyond the write list.
func CheckThemeTemplateReadRules(raw []byte) ([]string, bool) {
	list, ok := jsonArrayOf(raw)
	if !ok || len(list) > 20 {
		return nil, false
	}
	rules := make([]string, 0, len(list))
	for _, value := range list {
		rule, ok := jsonStringOf(value)
		if !ok || !themeTemplateRule.MatchString(rule) {
			return nil, false
		}
		rules = append(rules, rule)
	}
	return rules, true
}

// TemplateRuleTitle is cleaned site text for a target named by a display rule.
type TemplateRuleTitle struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

// TemplateLive is a published template reported during precheck. Its title is site text.
type TemplateLive struct {
	ID             int64    `json:"id"`
	Title          string   `json:"title"`
	ShowsOn        []string `json:"shows_on"`
	ShowsOnProblem *string  `json:"shows_on_problem"`
}

func templateSiteTitle(raw []byte) (string, bool) {
	title, ok := jsonStringOf(raw)
	return humantext.CapRunes(humantext.Clean(title), abilityTitleExcerptRunes), ok
}

func checkTemplateDisplayFacts(rules []string, rawTitles, rawLive []byte) ([]TemplateRuleTitle, []TemplateLive, bool) {
	titles, ok := jsonArrayOf(rawTitles)
	if !ok || len(titles) > 20 {
		return nil, nil, false
	}
	targets := map[string]bool{}
	for _, rule := range rules {
		parts := strings.Split(rule, "/")
		if len(parts) == 4 {
			targets[parts[3]] = true
		}
	}
	checkedTitles := make([]TemplateRuleTitle, 0, len(titles))
	seen := map[int64]bool{}
	for _, value := range titles {
		m, ok := jsonObjectOf(value)
		if !ok || len(m) != 2 {
			return nil, nil, false
		}
		id, ok := jsonIntOf(m["id"])
		title, textOK := templateSiteTitle(m["title"])
		if !ok || id <= 0 || !textOK || seen[id] || !targets[strconv.FormatInt(id, 10)] {
			return nil, nil, false
		}
		seen[id] = true
		checkedTitles = append(checkedTitles, TemplateRuleTitle{ID: id, Title: title})
	}
	if bytes.Equal(bytes.TrimSpace(rawLive), []byte("null")) {
		return checkedTitles, nil, true
	}
	live, ok := jsonArrayOf(rawLive)
	if !ok || len(live) > 5 {
		return nil, nil, false
	}
	checkedLive := make([]TemplateLive, 0, len(live))
	seen = map[int64]bool{}
	for _, value := range live {
		m, ok := jsonObjectOf(value)
		if !ok || len(m) != 4 {
			return nil, nil, false
		}
		id, ok := jsonIntOf(m["id"])
		title, textOK := templateSiteTitle(m["title"])
		if !ok || id <= 0 || !textOK || seen[id] {
			return nil, nil, false
		}
		seen[id] = true
		row := TemplateLive{ID: id, Title: title}
		if bytes.Equal(bytes.TrimSpace(m["shows_on"]), []byte("null")) {
			problem, ok := jsonStringOf(m["shows_on_problem"])
			if !ok || (problem != "unreadable" && problem != "too_many") {
				return nil, nil, false
			}
			row.ShowsOnProblem = &problem
		} else {
			var valid bool
			row.ShowsOn, valid = CheckThemeTemplateReadRules(m["shows_on"])
			if !valid || !bytes.Equal(bytes.TrimSpace(m["shows_on_problem"]), []byte("null")) {
				return nil, nil, false
			}
		}
		checkedLive = append(checkedLive, row)
	}
	return checkedTitles, checkedLive, true
}

func checkTemplatePrecheckFacts(preview builderPreview, f pageCreateFacts) (*PageCardTemplate, bool) {
	titles, live, ok := checkTemplateDisplayFacts(f.showsOn, preview.RuleTitles, preview.AlsoLive)
	if !ok {
		return nil, false
	}
	return &PageCardTemplate{Type: f.templateType, ShowsOn: f.showsOn, RuleTitles: titles, AlsoLive: live}, true
}
