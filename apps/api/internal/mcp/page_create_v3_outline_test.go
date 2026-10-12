package mcp

import (
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The outline grammar of a designed page (outline_version 3): sections at the
// top of the outline, and what they hold. The shared case table is the oracle
// the agent's grammar answers as well; the tests after it cover what the table
// does not say.

// v3Case is one row of the shared table. Input is the exact JSON text; a row
// with Generate pads it with Generate.Append up to Generate.ToBytes bytes.
type v3Case struct {
	Name     string `json:"name"`
	Rule     string `json:"rule"`
	Input    string `json:"input"`
	Agent    string `json:"agent"`
	Go       string `json:"go"`
	Generate *struct {
		Append  string `json:"append"`
		ToBytes int    `json:"to_bytes"`
	} `json:"generate"`
}

func (c v3Case) body() []byte {
	in := c.Input
	if c.Generate != nil && len(in) < c.Generate.ToBytes {
		in += strings.Repeat(c.Generate.Append, c.Generate.ToBytes-len(in))
	}
	return []byte(in)
}

// want is the answer of the control plane's validation: "" for accept, else
// the code. A refusing control plane answers the agent's code, except that an
// unknown node is the agent's create_content_invalid and bad_input here.
func (c v3Case) want() string {
	if c.Go != "refuse" {
		return ""
	}
	if c.Agent == "create_content_invalid" {
		return pageCreateBadInput
	}
	return c.Agent
}

func readV3Cases(t *testing.T) []v3Case {
	t.Helper()
	b, err := os.ReadFile(pageV3CasesFixture)
	if err != nil {
		t.Fatalf("read %s: %v", pageV3CasesFixture, err)
	}
	var doc struct {
		Cases []v3Case `json:"cases"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("decode %s: %v", pageV3CasesFixture, err)
	}
	return doc.Cases
}

// The groups of rules in the table. A group in v3GroupsRun is judged by
// TestOutlineV3SharedCases; every rule of the table belongs to one group.
const (
	v3GroupHead     = "head"     // outline_version, design, page_frame
	v3GroupSections = "sections" // section nodes, their fields and caps
	v3GroupPatterns = "patterns" // pattern variants, item caps, icons
	v3GroupLater    = "later"    // anchors, links, button style, hero and h1 placement
	v3GroupInput    = "input"    // the size of the input text
)

var v3GroupsRun = []string{v3GroupHead, v3GroupSections, v3GroupPatterns, v3GroupLater, v3GroupInput}

var v3RuleGroups = map[string]string{
	"outline_version_3":          v3GroupHead,
	"v3_needs_elementor_classic": v3GroupHead,
	"v3_fields_need_version":     v3GroupHead,
	"page_frame_enum":            v3GroupHead,
	"page_frame_fits_theme":      v3GroupHead,
	"design_complete":            v3GroupHead,
	"design_contrast":            v3GroupHead,
	"design_values_closed":       v3GroupHead,
	"font_in_registry":           v3GroupHead,

	"minimal_v3_page":          v3GroupSections,
	"section_needs_version":    v3GroupSections,
	"sections_at_most_20":      v3GroupSections,
	"section_only_at_top":      v3GroupSections,
	"outline_all_sections":     v3GroupSections,
	"section_children_1_to_50": v3GroupSections,
	"section_fields_closed":    v3GroupSections,
	"section_options":          v3GroupSections,
	"background_is_image":      v3GroupSections,
	"containers_required":      v3GroupSections,
	"images_at_most_20":        v3GroupSections,
	"anchor_required":          v3GroupSections,

	"pattern_item_caps":  v3GroupPatterns,
	"icon_in_list":       v3GroupPatterns,
	"pattern_text_rules": v3GroupPatterns,

	"full_landing_page":    v3GroupLater,
	"anchor_regex":         v3GroupLater,
	"anchor_unique":        v3GroupLater,
	"anchor_not_reserved":  v3GroupLater,
	"link_anchor_resolves": v3GroupLater,
	"link_anchor_syntax":   v3GroupLater,
	"link_no_ampersand":    v3GroupLater,
	"button_style_enum":    v3GroupLater,
	"hero_first":           v3GroupLater,
	"h1_inside_hero_only":  v3GroupLater,
	"h1_at_most_once":      v3GroupLater,
	"hero_at_most_once":    v3GroupLater,
	"input_at_most_64_kib": v3GroupInput,
}

// v3GroupMustHold names rows a group has to find in the table, so a group that
// lost its rows (a rename, a re-cut table) fails instead of passing empty.
var v3GroupMustHold = map[string][]string{
	v3GroupHead: {"outline-version-4", "design-missing", "design-ink-on-ground-contrast-low", "page-frame-unknown", "design-ratio-as-a-number", "design-ratio-unknown-name",
		"block-theme-without-a-frame", "block-theme-theme-frame"},
	v3GroupSections: {"v3-minimal-page", "sections-20", "sections-21", "section-inside-a-section", "paragraph-at-the-top-level", "section-without-children", "section-raw-padding", "section-with-every-option", "images-21-counting-backgrounds", "anchor-missing"},
	v3GroupPatterns: {"patterns-at-their-smallest-counts", "patterns-at-their-largest-counts", "features-2-items", "testimonials-7-items", "pricing-5-plans", "faq-0-items", "logos-11-items", "stats-5-items", "icon-not-in-the-list", "pattern-text-with-markup"},
	v3GroupLater: {"v3-full-landing-page", "anchor-uppercase", "anchor-41-characters", "anchors-at-2-and-40-characters", "anchor-duplicate", "anchor-reserved",
		"link-to-a-later-anchor", "link-anchor-missing", "link-anchor-malformed", "button-url-with-an-ampersand", "hero-button-url-with-an-ampersand", "plan-cta-url-with-an-ampersand", "links-with-a-query-and-no-ampersand", "buttons-in-three-styles",
		"hero-first-with-the-h1", "hero-in-the-second-section", "hero-not-the-first-child", "h1-without-a-hero", "h1-in-a-later-section", "h1-twice-in-the-hero",
		"two-heroes", "two-heroes-the-first-out-of-place"},
	v3GroupInput: {"landing-page-at-the-input-limit", "landing-page-over-the-input-limit"},
}

func TestOutlineV3SharedCases(t *testing.T) {
	cases := readV3Cases(t)
	for _, group := range v3GroupsRun {
		t.Run(group, func(t *testing.T) {
			held := map[string]bool{}
			for _, c := range cases {
				if v3RuleGroups[c.Rule] != group {
					continue
				}
				held[c.Name] = true
				t.Run(c.Name, func(t *testing.T) {
					if _, code := validatePageCreateInput(c.body()); code != c.want() {
						t.Errorf("rule %s: code %q, want %q (agent %q, go %q)", c.Rule, code, c.want(), c.Agent, c.Go)
					}
				})
			}
			if len(held) == 0 {
				t.Fatalf("no row of the table is in the %s group", group)
			}
			for _, name := range v3GroupMustHold[group] {
				if !held[name] {
					t.Errorf("the %s group does not hold the row %q", group, name)
				}
			}
		})
	}
}

// TestOutlineV3RulesAreGrouped: every rule of the table belongs to a group, so
// a rule added to the table cannot go unjudged here, and no group names a rule
// the table dropped.
func TestOutlineV3RulesAreGrouped(t *testing.T) {
	inTable := map[string]bool{}
	for _, c := range readV3Cases(t) {
		inTable[c.Rule] = true
		if v3RuleGroups[c.Rule] == "" {
			t.Errorf("row %q: rule %q is in no group", c.Name, c.Rule)
		}
	}
	for rule := range v3RuleGroups {
		if !inTable[rule] {
			t.Errorf("group entry %q is no rule of the table", rule)
		}
	}
}

// TestOutlineV3AcceptedCasesPassTheRunEntry: a page the table accepts is one
// the run entry lets through. Its integer check refuses a number with a
// fraction, so a designed page holds none, the design's ratio being a name.
func TestOutlineV3AcceptedCasesPassTheRunEntry(t *testing.T) {
	var accepted int
	for _, c := range readV3Cases(t) {
		if c.Go != "accept" {
			continue
		}
		accepted++
		if !checkIntegersOnly(c.body()) {
			t.Errorf("%s: the run entry's integer check refuses a case the table accepts", c.Name)
		}
	}
	if accepted == 0 {
		t.Fatal("the table has no accepted row, so nothing was judged")
	}
	// The check is not blind: the number a ratio name stands for is refused.
	if checkIntegersOnly([]byte(`{"design":{"scale":{"base_px":17,"ratio":1.25}}}`)) {
		t.Error("the run entry's integer check lets a fraction through")
	}
}

// ---------------------------------------------------------------------------
// Sections
// ---------------------------------------------------------------------------

const (
	v3Para    = `{"type":"paragraph","text":"Hello."}`
	v3Heading = `{"type":"heading","level":2,"text":"Our story"}`
)

// v3Sec is a section: its anchor, any further members ("" for none) and its
// children.
func v3Sec(anchor, extra string, children ...string) string {
	if extra != "" {
		extra += ","
	}
	return `{"type":"section","anchor":"` + anchor + `",` + extra + `"children":[` + strings.Join(children, ",") + `]}`
}

// v3Outline is a designed-page input holding these top-level nodes.
func v3Outline(nodes ...string) []byte {
	return v3HeadInput(map[string]string{"outline": "[" + strings.Join(nodes, ",") + "]"})
}

func v3Image(id int) string {
	return `{"type":"image","attachment_id":` + strconv.Itoa(id) + `,"alt":"A picture"}`
}

func TestOutlineV3Sections(t *testing.T) {
	t.Run("a section holds every block of the version 2 grammar", func(t *testing.T) {
		blocks := []string{
			v3Heading, v3Para,
			`{"type":"list","ordered":false,"items":["One","Two"]}`,
			v3Image(7),
			`{"type":"buttons","buttons":[{"text":"Start","url":"/start","style":"fill"}]}`,
			`{"type":"quote","paragraphs":["Well said."],"citation":"A client"}`,
			`{"type":"separator"}`,
			`{"type":"spacer","size":"small"}`,
			`{"type":"table","header":["A","B"],"rows":[["1","2"]]}`,
			`{"type":"group","children":[` + v3Para + `]}`,
			`{"type":"columns","columns":[{"children":[` + v3Para + `]},{"children":[` + v3Para + `]}]}`,
		}
		f := v3HeadAccepted(t, v3Outline(v3Sec("intro", "", blocks...)))
		if !f.usesLayout {
			t.Error("a section is layout, but usesLayout is false")
		}
		if len(f.mediaIDs) != 1 || f.mediaIDs[0] != 7 {
			t.Errorf("mediaIDs = %v, want [7]", f.mediaIDs)
		}
	})

	t.Run("every choice of every option is accepted", func(t *testing.T) {
		for key, values := range map[string][]string{
			"width":      {"boxed", "full"},
			"tone":       {"ground", "surface", "inverse", "accent"},
			"spacing":    {"md", "lg", "xl"},
			"align":      {"start", "center"},
			"min_height": {"auto", "screen"},
		} {
			for _, v := range values {
				v3HeadAccepted(t, v3Outline(v3Sec("intro", `"`+key+`":"`+v+`"`, v3Para)))
			}
		}
		for _, overlay := range []string{"none", "soft", "strong"} {
			v3HeadAccepted(t, v3Outline(v3Sec("intro", `"background":{"attachment_id":42,"overlay":"`+overlay+`"}`, v3Para)))
		}
		for _, focal := range []string{"center", "top", "bottom"} {
			v3HeadAccepted(t, v3Outline(v3Sec("intro", `"background":{"attachment_id":42,"focal":"`+focal+`"}`, v3Para)))
		}
		v3HeadAccepted(t, v3Outline(v3Sec("intro", `"background":{"attachment_id":2147483647}`, v3Para)))
	})

	t.Run("a member outside the options, or a value outside its choices, is bad_input", func(t *testing.T) {
		for _, extra := range []string{
			`"padding":48`, `"id":"x"`, `"class":"hero"`, `"style":"dark"`, `"columns":2`,
			`"width":"wide"`, `"width":1`, `"width":null`,
			`"tone":"dark"`, `"tone":"Ground"`,
			`"spacing":"sm"`, `"spacing":"2xl"`, `"spacing":16`,
			`"align":"left"`, `"align":"end"`,
			`"min_height":"tall"`, `"min_height":400`,
			`"background":42`, `"background":null`, `"background":[]`, `"background":{}`,
			`"background":{"overlay":"soft"}`,
			`"background":{"attachment_id":0}`, `"background":{"attachment_id":-1}`,
			`"background":{"attachment_id":"42"}`, `"background":{"attachment_id":4.5}`,
			`"background":{"attachment_id":2147483648}`,
			`"background":{"attachment_id":42,"overlay":"medium"}`,
			`"background":{"attachment_id":42,"overlay":null}`,
			`"background":{"attachment_id":42,"focal":"left"}`,
			`"background":{"attachment_id":42,"url":"https://example.com/a.jpg"}`,
		} {
			v3HeadRefused(t, v3Outline(v3Sec("intro", extra, v3Para)), pageCreateBadInput)
		}
	})

	t.Run("the anchor is required and a string", func(t *testing.T) {
		for _, node := range []string{
			`{"type":"section","children":[` + v3Para + `]}`,
			`{"type":"section","anchor":5,"children":[` + v3Para + `]}`,
			`{"type":"section","anchor":null,"children":[` + v3Para + `]}`,
			`{"type":"section","anchor":["intro"],"children":[` + v3Para + `]}`,
		} {
			v3HeadRefused(t, v3Outline(node), pageCreateBadInput)
		}
	})

	t.Run("a section holds 1 to 50 children", func(t *testing.T) {
		many := func(n int) []string {
			out := make([]string, n)
			for i := range out {
				out[i] = v3Para
			}
			return out
		}
		v3HeadAccepted(t, v3Outline(v3Sec("intro", "", many(1)...)))
		v3HeadAccepted(t, v3Outline(v3Sec("intro", "", many(50)...)))
		v3HeadRefused(t, v3Outline(v3Sec("intro", "", many(0)...)), pageCreateLayoutInvalid)
		v3HeadRefused(t, v3Outline(v3Sec("intro", "", many(51)...)), pageCreateLayoutInvalid)
		for _, children := range []string{`"x"`, `null`, `{}`, `5`, `[5]`, `[null]`, `["x"]`} {
			v3HeadRefused(t, v3Outline(`{"type":"section","anchor":"intro","children":`+children+`}`), pageCreateBadInput)
		}
		v3HeadRefused(t, v3Outline(`{"type":"section","anchor":"intro"}`), pageCreateBadInput)
	})

	t.Run("every top-level node is a section", func(t *testing.T) {
		// A block of the grammar in the wrong place is layout_invalid, whatever
		// else is wrong with it; a node of no known type is bad_input.
		misplaced := []string{
			v3Para, v3Heading,
			`{"type":"list","ordered":false,"items":["One"]}`,
			v3Image(7),
			`{"type":"buttons","buttons":[{"text":"Start","url":"/start"}]}`,
			`{"type":"quote","paragraphs":["Well said."]}`,
			`{"type":"separator"}`,
			`{"type":"spacer","size":"small"}`,
			`{"type":"table","rows":[["1"]]}`,
			`{"type":"group","children":[` + v3Para + `]}`,
			`{"type":"columns","columns":[{"children":[` + v3Para + `]},{"children":[` + v3Para + `]}]}`,
			`{"type":"pattern","pattern":"faq","variant":"accordion","faq":[]}`,
			`{"type":"paragraph"}`,
		}
		for _, node := range misplaced {
			v3HeadRefused(t, v3Outline(v3Sec("intro", "", v3Para), node), pageCreateLayoutInvalid)
			v3HeadRefused(t, v3Outline(node), pageCreateLayoutInvalid)
		}
		for _, node := range []string{`{"type":"hero"}`, `{"type":"Section","anchor":"a","children":[` + v3Para + `]}`, `{}`, `7`, `"section"`, `null`, `[]`} {
			v3HeadRefused(t, v3Outline(v3Sec("intro", "", v3Para), node), pageCreateBadInput)
		}
	})

	t.Run("a section is never inside another node", func(t *testing.T) {
		inner := v3Sec("inner", "", v3Para)
		for name, node := range map[string]string{
			"in a section": v3Sec("outer", "", v3Para, inner),
			"in a group":   v3Sec("outer", "", `{"type":"group","children":[`+inner+`]}`),
			"in a column":  v3Sec("outer", "", `{"type":"columns","columns":[{"children":[`+inner+`]},{"children":[`+v3Para+`]}]}`),
		} {
			t.Run(name, func(t *testing.T) { v3HeadRefused(t, v3Outline(node), pageCreateLayoutInvalid) })
		}
	})

	t.Run("a page holds at most 20 sections, however many are sent", func(t *testing.T) {
		page := func(n int) []byte {
			nodes := make([]string, n)
			for i := range nodes {
				nodes[i] = v3Sec("s"+strconv.Itoa(100+i), "", v3Para)
			}
			return v3Outline(nodes...)
		}
		v3HeadAccepted(t, page(20))
		v3HeadRefused(t, page(21), pageCreateLayoutInvalid)
		// Past the version 2 limit on top-level nodes it is the same answer.
		v3HeadRefused(t, page(pageCreateMaxTopLevelNodes+1), pageCreateLayoutInvalid)
		v3HeadRefused(t, page(pageCreateMaxTopLevelNodes*2), pageCreateLayoutInvalid)
	})

	t.Run("a background counts as an image and its id comes first", func(t *testing.T) {
		f := v3HeadAccepted(t, v3Outline(
			v3Sec("one", `"background":{"attachment_id":9}`, v3Image(7), v3Image(9)),
			v3Sec("two", "", v3Image(9)),
		))
		if !slices.Equal(f.mediaIDs, []int64{9, 7}) {
			t.Errorf("mediaIDs = %v, want 9 then 7, each once", f.mediaIDs)
		}
		// Three uses of one attachment are three images toward the cap of 20.
		var nodes []string
		for i := 0; i < 7; i++ {
			nodes = append(nodes, v3Sec("pic"+strconv.Itoa(100+i), `"background":{"attachment_id":5}`, v3Image(5), v3Image(5)))
		}
		v3HeadRefused(t, v3Outline(nodes...), pageCreateLayoutInvalid)
	})

	t.Run("a page holds one hero at most, counted before anything else is read", func(t *testing.T) {
		hero := `{"type":"pattern","pattern":"hero","variant":"centered","children":[{"type":"heading","level":1,"text":"Hi"}]}`
		v3HeadRefused(t, v3Outline(v3Sec("one", "", hero), v3Sec("two", "", hero)), pageCreateLayoutInvalid)
		v3HeadRefused(t, v3Outline(v3Sec("one", "", hero, v3Para, hero)), pageCreateLayoutInvalid)
	})

	t.Run("version 2 does not read a section, and page-edit does", func(t *testing.T) {
		// A version 2 input has no section. The outline a page-edit adds is
		// read for what each node is, so it takes a section (the shared edit
		// table's version 3 rows pin it) and still refuses one inside another.
		v2 := v3HeadInput(map[string]string{"outline": "[" + v3Sec("intro", "", v3Para) + "]"}, "outline_version", "design")
		v3HeadRefused(t, v2, pageCreateBadInput)
		if _, ok := pageEditOutline(json.RawMessage("[" + v3Sec("intro", "", v3Para) + "]")); !ok {
			t.Error("pageEditOutline refused a section")
		}
		nested := v3Sec("outer", "", v3Sec("inner", "", v3Para))
		if _, ok := pageEditOutline(json.RawMessage("[" + nested + "]")); ok {
			t.Error("pageEditOutline accepted a section inside a section")
		}
	})
}
