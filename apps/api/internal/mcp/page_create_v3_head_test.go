package mcp

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

// The top-level members that make a page-create input a designed page
// (outline_version 3, design, page_frame): what they need around them and
// what they default to. The outline grammar of version 3 is under test in
// page_create_v3_outline_test.go; every input below holds one section of
// version 2 blocks (a version 2 input, one outline of those blocks), so a
// refusal can only come from the members this file is about.

// v3HeadDesign is a design with every field explicit.
const v3HeadDesign = `{"colors":{"ground":"#F6F5F2","surface":"#FFFFFF","ink":"#15171C","muted":"#5A606C","accent":"#0D7A63","on_accent":"#FFFFFF"},` +
	`"fonts":{"heading":"Manrope","body":"Source Sans 3"},"scale":{"base_px":17,"ratio":"major_third"},"radius":"md","density":"normal"}`

// v3HeadOutlineV2 is an outline of version 2 the grammar accepts, and
// v3HeadOutline the outline of a designed page that holds the same blocks in a
// section. Neither holds a hero.
const (
	v3HeadOutlineV2 = `[{"type":"heading","level":2,"text":"Welcome"},{"type":"paragraph","text":"A plain outline."}]`
	v3HeadOutline   = `[{"type":"section","anchor":"intro","children":[{"type":"heading","level":2,"text":"Welcome"},{"type":"paragraph","text":"A plain outline."}]}]`
)

// v3HeadMembers are the members of a valid designed-page input, in the order
// the shared case table writes them.
var v3HeadMembers = []struct{ key, raw string }{
	{"post_type", `"page"`},
	{"editor", `"builder:elementor"`},
	{"elementor_format", `"classic"`},
	{"title", `"Launch page"`},
	{"outline_version", `3`},
	{"design", v3HeadDesign},
	{"outline", v3HeadOutline},
}

// v3HeadInput builds an input from v3HeadMembers: set replaces a member's JSON
// (a key that is not a member is added at the end, sorted), drop removes
// members. An input without outline_version holds the outline of version 2,
// unless set names an outline.
func v3HeadInput(set map[string]string, drop ...string) []byte {
	dropped := map[string]bool{}
	for _, d := range drop {
		dropped[d] = true
	}
	var parts []string
	known := map[string]bool{}
	for _, m := range v3HeadMembers {
		known[m.key] = true
		if dropped[m.key] {
			continue
		}
		raw := m.raw
		if m.key == "outline" && dropped["outline_version"] {
			raw = v3HeadOutlineV2
		}
		if o, ok := set[m.key]; ok {
			raw = o
		}
		parts = append(parts, `"`+m.key+`":`+raw)
	}
	var extra []string
	for k := range set {
		if !known[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		parts = append(parts, `"`+k+`":`+set[k])
	}
	return []byte("{" + strings.Join(parts, ",") + "}")
}

func v3HeadAccepted(t *testing.T, input []byte) pageCreateFacts {
	t.Helper()
	f, code := validatePageCreateInput(input)
	if code != "" {
		t.Fatalf("refused with %s, want accepted: %s", code, input)
	}
	return f
}

func v3HeadRefused(t *testing.T, input []byte, want string) {
	t.Helper()
	if _, code := validatePageCreateInput(input); code != want {
		t.Errorf("code %q, want %q: %s", code, want, input)
	}
}

func TestOutlineV3OnlyElementorClassic(t *testing.T) {
	t.Run("a designed page is Elementor, classic stated, with a design", func(t *testing.T) {
		f := v3HeadAccepted(t, v3HeadInput(nil))
		if f.outlineVersion != 3 || f.builder != "elementor" || f.elementorFormat != "classic" {
			t.Errorf("facts = %+v", f)
		}
		if f.design == nil || f.design.Fonts.Heading != "Manrope" || f.design.Colors.Accent != "#0D7A63" {
			t.Errorf("design = %+v", f.design)
		}
	})

	t.Run("the colours of the design come back upper case", func(t *testing.T) {
		low := strings.Replace(v3HeadDesign, "#0D7A63", "#0d7a63", 1)
		f := v3HeadAccepted(t, v3HeadInput(map[string]string{"design": low}))
		if f.design == nil || f.design.Colors.Accent != "#0D7A63" {
			t.Errorf("design = %+v", f.design)
		}
	})

	t.Run("a version 2 input is unchanged and carries nothing of a designed page", func(t *testing.T) {
		for name, in := range map[string][]byte{
			"elementor": v3HeadInput(nil, "outline_version", "design"),
			"blocks":    v3HeadInput(map[string]string{"editor": `"wordpress_blocks"`}, "elementor_format", "outline_version", "design"),
			"classic":   v3HeadInput(map[string]string{"editor": `"wordpress_classic"`}, "elementor_format", "outline_version", "design"),
		} {
			f := v3HeadAccepted(t, in)
			if f.outlineVersion != 0 || f.design != nil || f.pageFrame != "" {
				t.Errorf("%s: facts = %+v", name, f)
			}
		}
	})

	t.Run("only the whole number 3 is a version", func(t *testing.T) {
		for _, v := range []string{`4`, `2`, `1`, `0`, `-3`, `30`, `3.0`, `3.5`, `3e0`, `"3"`, `true`, `null`, `[3]`, `{"v":3}`} {
			v3HeadRefused(t, v3HeadInput(map[string]string{"outline_version": v}), pageCreateBadInput)
		}
	})

	t.Run("version 3 needs the Elementor editor", func(t *testing.T) {
		// elementor_format belongs to builder:elementor alone, so the other
		// editors drop it: the editor is the one defect.
		for _, editor := range []string{`"wordpress_blocks"`, `"wordpress_classic"`} {
			v3HeadRefused(t, v3HeadInput(map[string]string{"editor": editor}, "elementor_format"), pageCreateBadInput)
		}
	})

	t.Run("version 3 needs the classic format, stated", func(t *testing.T) {
		for name, in := range map[string][]byte{
			"atomic":       v3HeadInput(map[string]string{"elementor_format": `"atomic"`}),
			"site default": v3HeadInput(map[string]string{"elementor_format": `"site_default"`}),
			"not stated":   v3HeadInput(nil, "elementor_format"),
		} {
			t.Run(name, func(t *testing.T) { v3HeadRefused(t, in, pageCreateBadInput) })
		}
	})

	t.Run("design and page_frame are unknown members without version 3", func(t *testing.T) {
		v3HeadRefused(t, v3HeadInput(nil, "outline_version"), pageCreateBadInput)
		v3HeadRefused(t, v3HeadInput(map[string]string{"page_frame": `"theme"`}, "outline_version", "design"), pageCreateBadInput)
		// The version 2 editors refuse them as well.
		v3HeadRefused(t, v3HeadInput(map[string]string{"editor": `"wordpress_blocks"`}, "elementor_format", "outline_version"), pageCreateBadInput)
	})

	t.Run("a designed page needs its design", func(t *testing.T) {
		v3HeadRefused(t, v3HeadInput(nil, "design"), pageDesignNotReady)
		v3HeadRefused(t, v3HeadInput(map[string]string{"design": `null`}), pageCreateBadInput)
		v3HeadRefused(t, v3HeadInput(map[string]string{"design": `[]`}), pageCreateBadInput)
		v3HeadRefused(t, v3HeadInput(map[string]string{"design": strings.Replace(v3HeadDesign, `,"density":"normal"`, "", 1)}), pageDesignNotReady)
	})

	t.Run("a refused design answers with the design's own code", func(t *testing.T) {
		pureBlack := strings.Replace(v3HeadDesign, "#15171C", "#000000", 1)
		v3HeadRefused(t, v3HeadInput(map[string]string{"design": pureBlack}), pageCreateBadInput)
		lowContrast := strings.Replace(v3HeadDesign, "#15171C", "#F6F5F2", 1)
		v3HeadRefused(t, v3HeadInput(map[string]string{"design": lowContrast}), pageDesignContrastLow)
	})

	t.Run("the design is judged before the outline", func(t *testing.T) {
		// The design comes first: an outline that is no designed page's still
		// gets the design's own answer.
		sections := `[{"type":"section","anchor":"intro","children":[{"type":"paragraph","text":"Hi."}]}]`
		v3HeadRefused(t, v3HeadInput(map[string]string{"outline": sections}, "design"), pageDesignNotReady)
	})
}

// pageFrameOutline parses an outline for the page frame tests.
func pageFrameOutline(t *testing.T, outline string) []json.RawMessage {
	t.Helper()
	list, ok := jsonArrayOf([]byte(outline))
	if !ok {
		t.Fatalf("not a JSON array: %s", outline)
	}
	return list
}

// frameHero is a section that opens with a hero, frameNoHero one with a
// paragraph and a features pattern.
const (
	frameHero = `{"type":"section","anchor":"top","children":[{"type":"pattern","pattern":"hero","variant":"centered","children":[` +
		`{"type":"heading","level":1,"text":"Websites that earn their keep"},{"type":"paragraph","text":"We look after small business sites."}]}]}`
	frameNoHero = `{"type":"section","anchor":"more","children":[{"type":"paragraph","text":"More."},` +
		`{"type":"pattern","pattern":"features","variant":"grid","items":[]}]}`
)

func TestPageFrameDefault(t *testing.T) {
	member := func(raw string) map[string]json.RawMessage {
		m := map[string]json.RawMessage{}
		if raw != "" {
			m["page_frame"] = json.RawMessage(raw)
		}
		return m
	}

	t.Run("with no page_frame, a hero makes it full_width and no hero makes it theme", func(t *testing.T) {
		for name, tc := range map[string]struct{ outline, want string }{
			"hero first":      {"[" + frameHero + "]", pageFrameFullWidth},
			"hero and more":   {"[" + frameHero + "," + frameNoHero + "]", pageFrameFullWidth},
			"no hero":         {"[" + frameNoHero + "]", pageFrameTheme},
			"empty sections":  {`[{"type":"section","anchor":"x","children":[]}]`, pageFrameTheme},
			"version 2 nodes": {v3HeadOutlineV2, pageFrameTheme},
		} {
			got, ok := pageFrameOf(member(""), pageFrameOutline(t, tc.outline))
			if !ok || got != tc.want {
				t.Errorf("%s: (%q, %v), want %q", name, got, ok, tc.want)
			}
		}
	})

	t.Run("a page_frame that is sent wins over the default", func(t *testing.T) {
		for _, tc := range []struct{ outline, frame, want string }{
			{"[" + frameHero + "]", `"theme"`, pageFrameTheme},
			{"[" + frameHero + "]", `"canvas"`, pageFrameCanvas},
			{"[" + frameNoHero + "]", `"full_width"`, pageFrameFullWidth},
			{"[" + frameNoHero + "]", `"canvas"`, pageFrameCanvas},
		} {
			got, ok := pageFrameOf(member(tc.frame), pageFrameOutline(t, tc.outline))
			if !ok || got != tc.want {
				t.Errorf("%s with %s: (%q, %v), want %q", tc.outline[:30], tc.frame, got, ok, tc.want)
			}
		}
	})

	t.Run("canvas is never a default", func(t *testing.T) {
		for _, outline := range []string{"[" + frameHero + "]", "[" + frameNoHero + "]", "[]"} {
			if got, _ := pageFrameOf(member(""), pageFrameOutline(t, outline)); got == pageFrameCanvas {
				t.Errorf("%s defaulted to canvas", outline)
			}
		}
	})

	t.Run("a value outside the three frames is refused", func(t *testing.T) {
		for _, v := range []string{`"wide"`, `""`, `"Theme"`, `" theme"`, `"full-width"`, `"fullwidth"`, `3`, `true`, `null`, `["theme"]`, `{}`} {
			if got, ok := pageFrameOf(member(v), pageFrameOutline(t, "["+frameHero+"]")); ok {
				t.Errorf("page_frame %s was accepted as %q", v, got)
			}
		}
	})

	t.Run("a node that is not a hero is not one", func(t *testing.T) {
		for name, outline := range map[string]string{
			"another pattern":  `[{"type":"section","anchor":"a","children":[{"type":"pattern","pattern":"cta","variant":"band","children":[]}]}]`,
			"hero as a type":   `[{"type":"section","anchor":"a","children":[{"type":"hero"}]}]`,
			"hero as a word":   `[{"type":"section","anchor":"a","children":[{"type":"heading","level":2,"text":"pattern hero"}]}]`,
			"not a string":     `[{"type":"section","anchor":"a","children":[{"type":"pattern","pattern":["hero"]}]}]`,
			"a wrong case":     `[{"type":"section","anchor":"a","children":[{"type":"pattern","pattern":"Hero"}]}]`,
			"not sections":     `[{"type":"pattern","pattern":"hero"}]`,
			"children missing": `[{"type":"section","anchor":"a"}]`,
			"children garbage": `[{"type":"section","anchor":"a","children":"hero"}]`,
			"nodes garbage":    `[7,"hero",null,{"children":[1,null]}]`,
		} {
			if pageOutlineHasHero(pageFrameOutline(t, outline)) {
				t.Errorf("%s: counted as a hero", name)
			}
		}
		if !pageOutlineHasHero(pageFrameOutline(t, "["+frameNoHero+","+frameHero+"]")) {
			t.Error("a hero in a later section was not found")
		}
	})

	t.Run("the default and the choice reach the facts of an input", func(t *testing.T) {
		if f := v3HeadAccepted(t, v3HeadInput(nil)); f.pageFrame != pageFrameTheme {
			t.Errorf("no hero, no page_frame: %q", f.pageFrame)
		}
		for _, frame := range []string{pageFrameTheme, pageFrameFullWidth, pageFrameCanvas} {
			f := v3HeadAccepted(t, v3HeadInput(map[string]string{"page_frame": `"` + frame + `"`}))
			if f.pageFrame != frame {
				t.Errorf("page_frame %s came back as %q", frame, f.pageFrame)
			}
		}
		v3HeadRefused(t, v3HeadInput(map[string]string{"page_frame": `"wide"`}), pageCreateBadInput)
		v3HeadRefused(t, v3HeadInput(map[string]string{"page_frame": `null`}), pageCreateBadInput)
	})

	t.Run("the shared case table gets the frame its outline calls for", func(t *testing.T) {
		b, err := os.ReadFile(pageV3CasesFixture)
		if err != nil {
			t.Fatalf("read %s: %v", pageV3CasesFixture, err)
		}
		var doc struct {
			Cases []struct {
				Name  string `json:"name"`
				Input string `json:"input"`
			} `json:"cases"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("decode %s: %v", pageV3CasesFixture, err)
		}
		// The oracle is the case itself: a page_frame it sends is the answer,
		// and one it leaves out is full_width exactly for the cases that open
		// with a hero pattern.
		want := map[string]string{
			"v3-minimal-page":                   pageFrameTheme,
			"v3-full-landing-page":              pageFrameFullWidth,
			"hero-first-with-the-h1":            pageFrameFullWidth,
			"patterns-at-their-smallest-counts": pageFrameFullWidth,
			"patterns-at-their-largest-counts":  pageFrameTheme,
			"section-with-every-option":         pageFrameTheme,
			"sections-20":                       pageFrameTheme,
		}
		seen := 0
		for _, c := range doc.Cases {
			w, ok := want[c.Name]
			if !ok {
				continue
			}
			seen++
			top, ok := jsonObjectOf([]byte(c.Input))
			if !ok {
				t.Errorf("%s: the input is not an object", c.Name)
				continue
			}
			outline, ok := jsonArrayOf(top["outline"])
			if !ok {
				t.Errorf("%s: no outline", c.Name)
				continue
			}
			if got, ok := pageFrameOf(top, outline); !ok || got != w {
				t.Errorf("%s: (%q, %v), want %q", c.Name, got, ok, w)
			}
		}
		if seen != len(want) {
			t.Errorf("found %d of the %d named cases in the table", seen, len(want))
		}
	})
}
