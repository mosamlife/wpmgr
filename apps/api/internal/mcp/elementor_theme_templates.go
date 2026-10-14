package mcp

import (
	"encoding/json"
	"regexp"
)

var elementorThemeTemplatesShape = readObject(map[string]*readShape{
	"pro": readObject(map[string]*readShape{
		"active":        readBool,
		"version":       {token: regexp.MustCompile(`^[0-9][0-9A-Za-z._+-]{0,31}$`), nullable: true},
		"theme_builder": readBool,
		"problem":       {token: regexp.MustCompile(`^(?:pro_inactive|theme_builder_unavailable)$`), nullable: true},
	}),
	"templates": {maxItems: 25, items: readObject(map[string]*readShape{
		"id":            {scalar: "integer", minimum: 1},
		"type":          {token: regexp.MustCompile(`^(?:header|footer)$`)},
		"status":        {token: regexp.MustCompile(`^(?:publish|draft|pending|private|future|other)$`)},
		"made_by_wpmgr": readBool, "title": readText, "modified_gmt": readText,
		"shows_on":         {nullable: true, maxItems: 20, items: &readShape{token: regexp.MustCompile(`^(include|exclude)(/[a-z0-9_-]{1,64}){1,2}(/[1-9][0-9]{0,19})?$`)}},
		"shows_on_problem": {token: regexp.MustCompile(`^(?:pro_unavailable|unreadable|too_many)$`), nullable: true},
	})},
	"templates_total": {scalar: "integer"},
	"places": {maxItems: 6, items: readObject(map[string]*readShape{
		"rule":     {token: regexp.MustCompile(`^(?:general|singular/front_page|singular/page|singular/post|singular/not_found404|archive)$`)},
		"takes_id": readBool,
	})},
	"menus": {maxItems: 25, items: readObject(map[string]*readShape{
		"id":   {scalar: "integer", minimum: 1},
		"slug": {token: regexp.MustCompile(`^[a-z0-9_-]{1,200}$`), nullable: true},
		"name": readText, "items": {scalar: "integer"},
	})},
	"menus_total": {scalar: "integer"},
})

// The closed shape validates types before these cross-field checks run.
func validThemeTemplatesRelations(m map[string]any) bool {
	pro := m["pro"].(map[string]any)
	if pro["theme_builder"] == true && pro["active"] == false {
		return false
	}
	places := m["places"].([]any)
	if pro["theme_builder"] == false && len(places) != 0 {
		return false
	}
	seen := map[string]bool{}
	for _, value := range places {
		place := value.(map[string]any)
		rule := place["rule"].(string)
		takesID := rule == "singular/page" || rule == "singular/post"
		if seen[rule] || place["takes_id"] != takesID {
			return false
		}
		seen[rule] = true
	}
	templates := m["templates"].([]any)
	templatesTotal, _ := m["templates_total"].(json.Number).Int64()
	menusTotal, _ := m["menus_total"].(json.Number).Int64()
	if int64(len(templates)) > templatesTotal || int64(len(m["menus"].([]any))) > menusTotal {
		return false
	}
	for _, value := range templates {
		template := value.(map[string]any)
		if (template["shows_on"] == nil) == (template["shows_on_problem"] == nil) {
			return false
		}
	}
	return true
}
