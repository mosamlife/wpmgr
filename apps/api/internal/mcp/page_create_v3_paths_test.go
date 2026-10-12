package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// A refusal of the outline says where: failures[] holds the address of the
// node and the field, the way the agent's refusals give them. The address
// reads outline[i], then .children[j] for a block of a section, .children[k]
// for a block of a hero, a call to action, a group or a column, and
// .features[k] (or the pattern's own array) for an item.

// The outline rules of the shared table: a refused row of one of these groups
// is the outline's to explain. The rule that a section needs outline_version 3
// is about the head of the input, not the outline.
func v3OutlineRule(c v3Case) bool {
	if c.Rule == "section_needs_version" {
		return false
	}
	switch v3RuleGroups[c.Rule] {
	case v3GroupSections, v3GroupPatterns, v3GroupLater:
		return true
	}
	return false
}

// outlineOf is the input of a row of the shared table with its head (version,
// design, page frame) replaced by a valid one, so that the outline is judged
// on its own whatever the design of the row holds.
func outlineOf(t *testing.T, c v3Case) []byte {
	t.Helper()
	var in struct {
		Outline json.RawMessage `json:"outline"`
	}
	if err := json.Unmarshal(c.body(), &in); err != nil || len(in.Outline) == 0 {
		t.Fatalf("%s: the row has no outline: %v", c.Name, err)
	}
	return v3HeadInput(map[string]string{"outline": string(in.Outline)})
}

// failureAddress is where a failure is, the way the message prints it.
func failureAddress(f refusalFailure) string { return f.Path + "." + f.Key }

func TestOutlineRefusalCarriesPath(t *testing.T) {
	rows := map[string]v3Case{}
	for _, c := range readV3Cases(t) {
		rows[c.Name] = c
	}

	// Every refused row of the outline rules names where, whatever its code.
	t.Run("a refused row of the outline rules names where", func(t *testing.T) {
		judged := 0
		for _, c := range readV3Cases(t) {
			if c.want() == "" || !v3OutlineRule(c) {
				continue
			}
			judged++
			_, refusal := checkPageCreateInput(outlineOf(t, c))
			if refusal.Code != c.want() {
				t.Errorf("%s: code %q, want %q", c.Name, refusal.Code, c.want())
				continue
			}
			if len(refusal.Failures) == 0 {
				t.Errorf("%s: %s carries no failures", c.Name, refusal.Code)
				continue
			}
			f := refusal.Failures[0]
			if !(strings.HasPrefix(f.Path, "outline[") || f.Path == "input") || f.Key == "" || f.Reason == "" || strings.ContainsAny(f.Reason, "\n\t") {
				t.Errorf("%s: first failure %+v", c.Name, f)
			}
		}
		if judged < 20 {
			t.Fatalf("only %d refused rows are about the outline", judged)
		}
	})

	// The rows of the shared table whose address is known, as the agent's
	// grammar gives it (path and key joined by a dot).
	t.Run("the address of a refused row is the agent's", func(t *testing.T) {
		for _, e := range []struct{ row, code, path, key string }{
			{"sections-21", pageCreateLayoutInvalid, "input", "outline"},
			{"section-inside-a-section", pageCreateLayoutInvalid, "outline[0].children[1]", "type"},
			{"paragraph-at-the-top-level", pageCreateLayoutInvalid, "outline[1]", "type"},
			{"section-without-children", pageCreateLayoutInvalid, "outline[0]", "children"},
			{"section-raw-padding", pageCreateBadInput, "outline[0]", "fields"},
			{"anchor-uppercase", pageCreateAnchorInvalid, "outline[0]", "anchor"},
			{"anchor-41-characters", pageCreateAnchorInvalid, "outline[0]", "anchor"},
			{"anchor-missing", pageCreateBadInput, "outline[0]", "anchor"},
			{"anchor-duplicate", pageCreateAnchorDuplicate, "outline[1]", "anchor"},
			{"anchor-reserved", pageCreateAnchorReserved, "outline[0]", "anchor"},
			{"link-anchor-missing", pageCreateLinkAnchorMissing, "outline[0].children[0].buttons[0]", "url"},
			{"link-anchor-malformed", pageCreateLinkInvalid, "outline[0].children[0].buttons[0]", "url"},
			{"hero-in-the-second-section", pageCreateHeroMisplaced, "outline[1].children[0]", "pattern"},
			{"hero-not-the-first-child", pageCreateHeroMisplaced, "outline[0].children[1]", "pattern"},
			{"h1-without-a-hero", pageCreateH1OutsideHero, "outline[0].children[0]", "level"},
			{"h1-in-a-later-section", pageCreateH1OutsideHero, "outline[1].children[0]", "level"},
			{"h1-twice-in-the-hero", pageCreateLayoutInvalid, "outline[0].children[0].children[1]", "level"},
			{"features-2-items", pageCreateItemsOutOfRange, "outline[0].children[0]", "features"},
			{"testimonials-7-items", pageCreateItemsOutOfRange, "outline[0].children[0]", "testimonials"},
			{"pricing-5-plans", pageCreateItemsOutOfRange, "outline[0].children[0]", "plans"},
			{"faq-0-items", pageCreateItemsOutOfRange, "outline[0].children[0]", "faq"},
			{"logos-11-items", pageCreateItemsOutOfRange, "outline[0].children[0]", "logos"},
			{"stats-5-items", pageCreateItemsOutOfRange, "outline[0].children[0]", "stats"},
			{"icon-not-in-the-list", pageCreateIconUnknown, "outline[0].children[0].features[1]", "icon"},
		} {
			c, ok := rows[e.row]
			if !ok {
				t.Errorf("the shared table has no row %q", e.row)
				continue
			}
			_, refusal := checkPageCreateInput(outlineOf(t, c))
			if refusal.Code != e.code || len(refusal.Failures) == 0 {
				t.Errorf("%s: %+v, want %s", e.row, refusal, e.code)
				continue
			}
			if f := refusal.Failures[0]; f.Path != e.path || f.Key != e.key {
				t.Errorf("%s: at %s, want %s", e.row, failureAddress(f), e.path+"."+e.key)
			}
		}
	})

	plan := `{"name":"Starter","price":"19","features":["One site"],"cta":{"text":"Choose","url":"/contact"}}`
	lostPlan := `{"name":"Pro","price":"49","features":["Ten sites"],"cta":{"text":"Choose","url":"#nowhere"}}`
	lostButton := v3Button("#nowhere")

	// A refusal inside a pattern, a group or a column continues the address
	// of the block that holds it.
	t.Run("the address goes down through patterns, groups and columns", func(t *testing.T) {
		for _, e := range []struct {
			name  string
			input []byte
			code  string
			path  string
			key   string
		}{
			{"a plan's call to action", v3Page(v3Pat("pricing", "three-plans", "plans", "["+plan+","+plan+","+lostPlan+"]")),
				pageCreateLinkAnchorMissing, "outline[0].children[0].plans[2].cta", "url"},
			{"a button of the hero", v3Outline(v3Sec("intro", "", v3BlockPat("hero", "centered", v3H1, lostButton))),
				pageCreateLinkAnchorMissing, "outline[0].children[0].children[1].buttons[0]", "url"},
			{"a button in a group", v3Page(`{"type":"group","children":[` + lostButton + `]}`),
				pageCreateLinkAnchorMissing, "outline[0].children[0].children[0].buttons[0]", "url"},
			{"a button in a column", v3Page(`{"type":"columns","columns":[{"children":[` + v3Para + `]},{"children":[` + lostButton + `]}]}`),
				pageCreateLinkAnchorMissing, "outline[0].children[0].columns[1].children[0].buttons[0]", "url"},
			{"a heading in a call to action", v3Page(v3BlockPat("cta", "band", `{"type":"heading","level":9,"text":"x"}`)),
				pageCreateBadInput, "outline[0].children[0].children[0]", "level"},
			{"a member an item does not take", v3Page(v3Pat("faq", "accordion", "faq", `[{"question":"Q","answer":"A","extra":1}]`)),
				pageCreateBadInput, "outline[0].children[0].faq[0]", "fields"},
			{"a missing item member", v3Page(v3Pat("faq", "accordion", "faq", `[{"question":"Q"}]`)),
				pageCreateBadInput, "outline[0].children[0].faq[0]", "answer"},
			{"too many lines in a plan", v3Page(v3Pat("pricing", "three-plans", "plans",
				`[{"name":"Pro","price":"49","features":[`+strings.Repeat(`"x",`, 8)+`"x"],"cta":{"text":"Go","url":"/contact"}}]`)),
				pageCreateItemsOutOfRange, "outline[0].children[0].plans[0]", "features"},
			{"a link in a plan that cannot be carried", v3Page(v3Pat("pricing", "three-plans", "plans",
				`[{"name":"Pro","price":"49","features":["x"],"cta":{"text":"Go","url":"javascript:alert(1)"}}]`)),
				pageCreateLinkInvalid, "outline[0].children[0].plans[0].cta", "url"},
			{"a second hero, counted before the walk", v3Outline(v3Sec("alpha", "", v3Hero), v3Sec("beta", "", v3Hero)),
				pageCreateLayoutInvalid, "outline[1].children[0]", "pattern"},
			{"a background that is not an object", v3Outline(v3Sec("alpha", `"background":7`, v3Para)),
				pageCreateBadInput, "outline[0]", "background"},
			{"a background overlay outside its set", v3Outline(v3Sec("alpha", `"background":{"attachment_id":5,"overlay":"loud"}`, v3Para)),
				pageCreateBadInput, "outline[0].background", "overlay"},
			{"a section option outside its set", v3Outline(v3Sec("alpha", `"tone":"neon"`, v3Para)),
				pageCreateBadInput, "outline[0]", "tone"},
		} {
			t.Run(e.name, func(t *testing.T) {
				_, refusal := checkPageCreateInput(e.input)
				if refusal.Code != e.code || len(refusal.Failures) == 0 {
					t.Fatalf("%+v, want %s", refusal, e.code)
				}
				if f := refusal.Failures[0]; f.Path != e.path || f.Key != e.key || f.Reason == "" {
					t.Errorf("at %s (%q), want %s", failureAddress(f), f.Reason, e.path+"."+e.key)
				}
			})
		}
	})

	t.Run("a version 2 outline names where as well", func(t *testing.T) {
		in := []byte(`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"paragraph","text":"x"},{"type":"heading","level":9,"text":"y"}]}`)
		_, refusal := checkPageCreateInput(in)
		if refusal.Code != pageCreateBadInput || len(refusal.Failures) != 1 || failureAddress(refusal.Failures[0]) != "outline[1].level" {
			t.Errorf("%+v", refusal)
		}
		in = []byte(`{"post_type":"page","editor":"wordpress_classic","title":"T","outline":[{"type":"paragraph","text":"x"},{"type":"spacer","size":"small"}]}`)
		_, refusal = checkPageCreateInput(in)
		if refusal.Code != pageCreateNeedsBlockEditor || len(refusal.Failures) != 1 || failureAddress(refusal.Failures[0]) != "outline[1].type" {
			t.Errorf("%+v", refusal)
		}
	})

	// Every link that leads nowhere is listed, in the order of the page, and
	// each failure names the anchors the page has.
	t.Run("every link to a missing anchor is listed with the anchors the page has", func(t *testing.T) {
		in := v3Outline(
			v3Sec("one", "", v3Button("#two"), v3Button("#nowhere")),
			v3Sec("two", "", v3Button("#gone"), v3Button("#one")),
		)
		_, refusal := checkPageCreateInput(in)
		if refusal.Code != pageCreateLinkAnchorMissing || len(refusal.Failures) != 2 {
			t.Fatalf("%+v", refusal)
		}
		for i, want := range []string{"outline[0].children[1].buttons[0].url", "outline[1].children[0].buttons[0].url"} {
			f := refusal.Failures[i]
			if failureAddress(f) != want || strings.Join(f.Allowed, ",") != "one,two" {
				t.Errorf("failure %d: %s allowing %v, want %s allowing one,two", i, failureAddress(f), f.Allowed, want)
			}
		}
	})

	// The icons of a pattern are listed together, as a refusal that names
	// one at a time sends the AI round the loop once for each.
	t.Run("every unknown icon of a pattern is listed", func(t *testing.T) {
		items := `[{"icon":"bolt","title":"A","text":"a"},{"icon":"nope","title":"B","text":"b"},{"icon":"also-nope","title":"C","text":"c"}]`
		_, refusal := checkPageCreateInput(v3Page(v3Pat("features", "grid", "features", items)))
		if refusal.Code != pageCreateIconUnknown || len(refusal.Failures) != 2 {
			t.Fatalf("%+v", refusal)
		}
		if failureAddress(refusal.Failures[0]) != "outline[0].children[0].features[1].icon" ||
			failureAddress(refusal.Failures[1]) != "outline[0].children[0].features[2].icon" {
			t.Errorf("%+v", refusal.Failures)
		}
		if len(refusal.Failures[0].Allowed) == 0 {
			t.Error("the failure does not offer the icons")
		}
		// A problem in a later item is the one told: the icons wait.
		bad := `[{"icon":"nope","title":"A","text":"a"},{"icon":"bolt","title":"B"},{"icon":"bolt","title":"C","text":"c"}]`
		_, refusal = checkPageCreateInput(v3Page(v3Pat("features", "grid", "features", bad)))
		if refusal.Code != pageCreateBadInput || len(refusal.Failures) != 1 || failureAddress(refusal.Failures[0]) != "outline[0].children[0].features[1].text" {
			t.Errorf("%+v", refusal)
		}
	})

	// The refusal that goes to the AI: the message spells out the first
	// failure and failures[] holds it; a bad_input refusal also keeps the
	// schema and speaks of the outline, not of the design.
	t.Run("the refusal that reaches the AI", func(t *testing.T) {
		_, refusal := checkPageCreateInput(outlineOf(t, rows["link-anchor-missing"]))
		ref := pageCreateInputRefusal(refusal)
		de, ok := domain.AsDomain(ref.err)
		if !ok {
			t.Fatalf("not a domain error: %v", ref.err)
		}
		if !strings.Contains(de.Message, "1. outline[0].children[0].buttons[0].url: ") {
			t.Errorf("message %q", de.Message)
		}
		if got, _ := de.Details["failures"].([]refusalFailure); len(got) != 1 || got[0].Path != "outline[0].children[0].buttons[0]" {
			t.Errorf("failures %v", de.Details["failures"])
		}
		if de.Details["code"] != pageCreateLinkAnchorMissing || de.Details["hint"] != precheckRefusalHints[pageCreateLinkAnchorMissing] {
			t.Errorf("details %v", de.Details)
		}

		_, refusal = checkPageCreateInput(outlineOf(t, rows["section-raw-padding"]))
		ref = pageCreateInputRefusal(refusal)
		de, ok = domain.AsDomain(ref.err)
		if !ok {
			t.Fatalf("not a domain error: %v", ref.err)
		}
		if strings.Contains(de.Message, "design") || !strings.Contains(de.Message, "1. outline[0].fields: ") {
			t.Errorf("message %q", de.Message)
		}
		if schema, _ := de.Details["schema"].(json.RawMessage); len(schema) == 0 {
			t.Error("a bad_input refusal lost the schema")
		}
		if got, _ := de.Details["failures"].([]refusalFailure); len(got) != 1 {
			t.Errorf("failures %v", de.Details["failures"])
		}
	})

	// An input the outline accepts carries nothing.
	t.Run("an accepted outline carries no failure", func(t *testing.T) {
		accepted := 0
		for _, c := range readV3Cases(t) {
			if !v3OutlineRule(c) || c.want() != "" {
				continue
			}
			accepted++
			if _, refusal := checkPageCreateInput(outlineOf(t, c)); refusal.Code != "" || len(refusal.Failures) != 0 {
				t.Errorf("%s: %+v", c.Name, refusal)
			}
		}
		if accepted < 5 {
			t.Fatalf("only %d accepted rows of the outline rules", accepted)
		}
	})
}
