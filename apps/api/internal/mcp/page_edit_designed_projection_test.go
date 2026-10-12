package mcp

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

type designedProjectionPage struct {
	BeforeTree      json.RawMessage  `json:"before_tree"`
	BaseFingerprint string           `json:"base_fingerprint"`
	Structure       map[string]any   `json:"structure"`
	Checks          []map[string]any `json:"checks"`
}

func designedProjectionPages(t *testing.T) map[string]designedProjectionPage {
	t.Helper()
	raw, err := os.ReadFile(agentAbilityFixtures + "page-edit-designed-previews.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Pages map[string]designedProjectionPage `json:"pages"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Pages) == 0 {
		t.Fatal("the designed golden has no pages")
	}
	return golden.Pages
}

func designedProjectionJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDesignedPageStructureIsTheAgentGolden(t *testing.T) {
	for name, page := range designedProjectionPages(t) {
		t.Run(name, func(t *testing.T) {
			tree, ok := phpDecodeDocument(page.BeforeTree, pageEditTreeMaxDepth, true)
			if !ok {
				t.Fatal("the before tree does not decode")
			}
			p, err := elementorProject(tree)
			if err != nil {
				t.Fatal(err)
			}
			answer, ok := p.answer(pageStructureMaxNodes, pageStructureMaxBytes)
			if !ok {
				t.Fatal("the structure cannot be encoded")
			}
			for _, key := range []string{"post_id", "builder", "builder_version", "format", "status", "editable", "open_state", "element_cache_active"} {
				answer[key] = page.Structure[key]
			}
			answer["base_fingerprint"] = page.BaseFingerprint
			got, want := designedProjectionJSON(t, answer), designedProjectionJSON(t, page.Structure)
			if !bytes.Equal(got, want) {
				t.Fatalf("structure differs from the agent golden: %s", designedTreeDifference(got, want))
			}
			checks := make([]map[string]any, 0)
			for _, node := range p.nodes {
				facts := map[string]any{"ref": node.ref}
				if node.anchor != "" {
					facts["anchor"] = node.anchor
				}
				if node.pattern != "" {
					facts["pattern"] = node.pattern
				}
				if len(node.links) > 0 {
					facts["links"] = node.links
				}
				if len(facts) > 1 {
					checks = append(checks, facts)
				}
			}
			if got, want := designedProjectionJSON(t, checks), designedProjectionJSON(t, page.Checks); !bytes.Equal(got, want) {
				t.Fatalf("internal checks differ from the agent golden: %s", designedTreeDifference(got, want))
			}
		})
	}
}

func TestDesignedPageStructureOutputKeepsPathsAndFencesText(t *testing.T) {
	for name, page := range designedProjectionPages(t) {
		t.Run(name, func(t *testing.T) {
			got, truncated := fenceAbilityOutput(AbilityPageStructure, &sqlc.AbilityCatalogue{Source: "wpmgr"},
				designedProjectionJSON(t, page.Structure), abilityRunDefaultOutputBytes)
			if truncated {
				t.Fatal("a small structure was withheld")
			}
			want := page.Structure
			for _, key := range []string{"builder_version", "status"} {
				want[key] = fenceSiteText(want[key].(string))
			}
			for _, raw := range want["nodes"].([]any) {
				node := raw.(map[string]any)
				if label, ok := node["label"].(string); ok {
					node["label"] = fenceSiteText(label)
				}
				if fields, ok := node["from_the_site"].(map[string]any); ok {
					for path, text := range fields {
						fields[path] = fenceSiteText(text.(string))
					}
				}
			}
			if !bytes.Equal(got, designedProjectionJSON(t, want)) {
				t.Fatalf("filtered structure drops contract fields: %s", designedTreeDifference(got, designedProjectionJSON(t, want)))
			}
		})
	}
}

func TestDesignedPageStructurePatternShapes(t *testing.T) {
	for _, page := range readDesignedGolden(t) {
		t.Run(page.Name, func(t *testing.T) {
			tree, ok := phpDecodeDocument(page.Tree, pageEditTreeMaxDepth, true)
			if !ok {
				t.Fatal("the compiler golden tree does not decode")
			}
			p, err := elementorProject(tree)
			if err != nil {
				t.Fatal(err)
			}
			var input struct {
				Outline []struct {
					Children []struct {
						Type    string `json:"type"`
						Pattern string `json:"pattern"`
						Variant string `json:"variant"`
					} `json:"children"`
				} `json:"outline"`
			}
			if err := json.Unmarshal([]byte(page.Input), &input); err != nil {
				t.Fatal(err)
			}
			var want []string
			for _, section := range input.Outline {
				for _, child := range section.Children {
					if child.Type == "pattern" {
						want = append(want, child.Pattern+":"+child.Variant)
					}
				}
			}
			var got []string
			for _, node := range p.nodes {
				if node.kind == "pattern" {
					got = append(got, node.pattern+":"+node.variant)
				}
			}
			if !bytes.Equal(designedProjectionJSON(t, got), designedProjectionJSON(t, want)) {
				t.Fatalf("patterns read %v, want %v", got, want)
			}
		})
	}
}

func TestDesignedPageStructureOutputDropsUnknownFields(t *testing.T) {
	raw := json.RawMessage(`{"element_cache_active":true,"secret":"hidden","nodes":[{"ref":"abc","parent":"root","kind":"pattern","pattern":"pricing","variant":"three-plans","editable":["plans.0.price","plans.0.cta.url","owner","plans.01.price"],"anchor":"private","links":["#private"],"from_the_site":{"plans.0.price":"Tom & Jerry's café","plans.0.cta.url":"#signup","plans.0.features.7":"Last feature","owner":"hidden","plans.01.price":"hidden","plans.4.price":"hidden"}}]}`)
	out, truncated := fenceAbilityOutput(AbilityPageStructure, &sqlc.AbilityCatalogue{Source: "wpmgr"}, raw, abilityRunDefaultOutputBytes)
	if truncated {
		t.Fatal("a small structure was withheld")
	}
	for _, want := range []string{`"element_cache_active":true`, `"pattern":"pricing"`, `"variant":"three-plans"`, `"editable":["plans.0.price","plans.0.cta.url",null,null]`, `"plans.0.price":"` + siteTextMarker + `Tom \u0026 Jerry's café"`, `"plans.0.features.7":"` + siteTextMarker + `Last feature"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %s: %s", want, out)
		}
	}
	for _, never := range []string{"secret", "hidden", "owner", "plans.01.price", "plans.4.price", "private", "anchor", "links"} {
		if strings.Contains(string(out), never) {
			t.Errorf("output carries %s: %s", never, out)
		}
	}
}

func TestDesignedPageStructureOffersOnlyReadablePatterns(t *testing.T) {
	page := designedProjectionPages(t)["landing"]
	if len(page.BeforeTree) == 0 {
		t.Fatal("the landing golden is missing")
	}
	cases := []struct {
		name, ref, kind, label, absent string
		edit                           func(node map[string]any)
	}{
		{name: "styling and rewording", ref: "8737c66", kind: "pattern", edit: func(node map[string]any) {
			box := node["elements"].([]any)[0].(map[string]any)["elements"].([]any)[0].(map[string]any)
			box["settings"].(map[string]any)["title_text"] = "Tom &amp; Jerry&#39;s café"
			box["settings"].(map[string]any)["title_color"] = "#ABCDEF"
		}},
		{name: "added feature", ref: "8737c66", kind: "locked", label: elementorLockedLabels[elementorLabelPatternEdited], edit: func(node map[string]any) {
			column := node["elements"].([]any)[0].(map[string]any)
			parts := column["elements"].([]any)
			column["elements"] = append(parts, parts[0])
		}},
		{name: "missing widget id", ref: "8737c66", kind: "locked", label: elementorLockedLabels[elementorLabelPatternEdited], edit: func(node map[string]any) {
			box := node["elements"].([]any)[0].(map[string]any)["elements"].([]any)[0].(map[string]any)
			delete(box, "id")
		}},
		{name: "dynamic item", ref: "8737c66", kind: "locked", label: elementorLockedLabels[elementorLabelDynamic], edit: func(node map[string]any) {
			box := node["elements"].([]any)[0].(map[string]any)["elements"].([]any)[0].(map[string]any)
			box["settings"].(map[string]any)["__dynamic__"] = map[string]any{"title_text": "tag"}
		}},
		{name: "number outside its form", ref: "eecf8d1", kind: "pattern", absent: "stats.0.value", edit: func(node map[string]any) {
			counter := node["elements"].([]any)[0].(map[string]any)["elements"].([]any)[0].(map[string]any)
			counter["settings"].(map[string]any)["ending_number"] = "1.5"
		}},
		{name: "formatted faq answer", ref: "43dd8ef", kind: "pattern", absent: "faq.0.answer", edit: func(node map[string]any) {
			accordion := node["elements"].([]any)[0].(map[string]any)
			tab := accordion["settings"].(map[string]any)["tabs"].([]any)[0].(map[string]any)
			tab["tab_content"] = "<p><b>Rich text</b></p>"
		}},
		{name: "empty faq", ref: "43dd8ef", kind: "locked", label: elementorLockedLabels[elementorLabelPatternEdited], edit: func(node map[string]any) {
			node["elements"].([]any)[0].(map[string]any)["settings"].(map[string]any)["tabs"] = []any{}
		}},
		{name: "reserved section anchor", ref: "605cdc2", kind: "group", edit: func(node map[string]any) {
			node["settings"].(map[string]any)["_element_id"] = "content"
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var tree []any
			if err := json.Unmarshal(page.BeforeTree, &tree); err != nil {
				t.Fatal(err)
			}
			node := findWidget(tree, c.ref)
			if node == nil {
				t.Fatal("the golden has no target node")
			}
			c.edit(node)
			decoded, ok := phpDecodeDocument(designedProjectionJSON(t, tree), pageEditTreeMaxDepth, true)
			if !ok {
				t.Fatal("the edited tree does not decode")
			}
			p, err := elementorProject(decoded)
			if err != nil {
				t.Fatal(err)
			}
			at, found := p.refs[c.ref]
			if !found {
				t.Fatal("the target was not projected")
			}
			got := p.nodes[at]
			if got.kind != c.kind || got.label != c.label {
				t.Fatalf("got %s %q, want %s %q", got.kind, got.label, c.kind, c.label)
			}
			for _, field := range got.editable {
				if c.absent != "" && field == c.absent {
					t.Fatalf("offered unreadable path %s", c.absent)
				}
			}
			if c.name == "styling and rewording" && (len(got.text) == 0 || got.text[0].text != "Tom & Jerry's café") {
				t.Fatalf("site text was not decoded: %+v", got.text)
			}
			if got.locked() {
				for _, child := range p.nodes {
					if child.parent == c.ref {
						t.Fatal("a locked pattern's children were walked")
					}
				}
			}
		})
	}
}
