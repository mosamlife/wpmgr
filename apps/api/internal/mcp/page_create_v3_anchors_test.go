package mcp

import (
	"strings"
	"testing"
)

// What a designed outline says about the page as a whole: the anchors of its
// sections, the links that point at them, the style of a button, and where
// the hero and the one level 1 heading may stand. The shared case table has
// the honest case of each rule and one refusal; these tests cover the shapes
// around them.

const (
	v3H1   = `{"type":"heading","level":1,"text":"Websites that earn their keep"}`
	v3Hero = `{"type":"pattern","pattern":"hero","variant":"centered","children":[` + v3H1 + `,` + v3Para + `]}`
)

// v3Button is a buttons node holding one button that points at url.
func v3Button(url string) string {
	return `{"type":"buttons","buttons":[{"text":"Go","url":"` + url + `"}]}`
}

// v3Anchors is a page of one section per anchor, each holding a paragraph.
func v3Anchors(anchors ...string) []byte {
	nodes := make([]string, len(anchors))
	for i, a := range anchors {
		nodes[i] = v3Sec(a, "", v3Para)
	}
	return v3Outline(nodes...)
}

func TestOutlineV3Anchors(t *testing.T) {
	t.Run("an anchor is 2 to 40 characters of a-z, 0-9 and -, led by a letter", func(t *testing.T) {
		for _, a := range []string{
			"ab", "a1", "a-", "a-b", "x--y", "a0-9-z", "pricing", "site-footer-2",
			"a" + strings.Repeat("b", 39),
		} {
			v3HeadAccepted(t, v3Anchors(a))
		}
		for _, a := range []string{
			"", "a", "A", "Ab", "aB", "Pricing", "1ab", "-ab", "9", "ab_c", "ab c", "ab.c", "ab#", "#ab", "ab/c",
			`ab\n`, `ab\u0000`, "ünï", "ab\\u00e9",
			"a" + strings.Repeat("b", 40),
			"a" + strings.Repeat("b", 100),
		} {
			v3HeadRefused(t, v3Anchors(a), pageCreateAnchorInvalid)
		}
	})

	t.Run("a reserved id is refused, whatever else the anchor looks like", func(t *testing.T) {
		for _, a := range []string{
			"content", "main", "primary", "page", "masthead", "colophon", "site-header", "site-footer", "wpadminbar",
		} {
			v3HeadRefused(t, v3Anchors(a), pageCreateAnchorReserved)
			v3HeadRefused(t, v3Anchors("intro", a), pageCreateAnchorReserved)
		}
		// Only the ids themselves are reserved.
		for _, a := range []string{"contents", "main-nav", "page-2", "content-area", "site-headers", "primary-menu"} {
			v3HeadAccepted(t, v3Anchors(a))
		}
	})

	t.Run("an anchor stands once on the page", func(t *testing.T) {
		v3HeadAccepted(t, v3Anchors("one", "two", "three"))
		v3HeadRefused(t, v3Anchors("one", "one"), pageCreateAnchorDuplicate)
		v3HeadRefused(t, v3Anchors("one", "two", "one"), pageCreateAnchorDuplicate)
		v3HeadRefused(t, v3Anchors("one", "two", "three", "two"), pageCreateAnchorDuplicate)
		// Two anchors that differ in one character are two anchors.
		v3HeadAccepted(t, v3Anchors("one", "one-"))
		v3HeadAccepted(t, v3Anchors("one", "on"))
	})

	t.Run("a reserved or malformed anchor is told so before a repeat is", func(t *testing.T) {
		v3HeadRefused(t, v3Anchors("content", "content"), pageCreateAnchorReserved)
		v3HeadRefused(t, v3Anchors("Intro", "Intro"), pageCreateAnchorInvalid)
	})

	t.Run("twenty sections hold twenty anchors", func(t *testing.T) {
		anchors := make([]string, 20)
		for i := range anchors {
			anchors[i] = "part-" + string(rune('a'+i))
		}
		v3HeadAccepted(t, v3Anchors(anchors...))
		anchors[19] = anchors[0]
		v3HeadRefused(t, v3Anchors(anchors...), pageCreateAnchorDuplicate)
	})
}

func TestOutlineV3Links(t *testing.T) {
	t.Run("a link to an anchor of the page resolves from anywhere on it", func(t *testing.T) {
		cta := v3BlockPat("cta", "band", v3Button("#top"))
		plan := `{"name":"Starter","price":"19","features":["One site"],"cta":{"text":"Choose","url":"#top"}}`
		columns := `{"type":"columns","columns":[{"children":[` + v3Button("#top") + `]},{"children":[` + v3Para + `]}]}`
		group := `{"type":"group","children":[` + v3Button("#top") + `]}`
		for name, children := range map[string][]string{
			"in a section":   {v3Button("#top")},
			"in a hero":      {v3BlockPat("hero", "centered", v3H1, v3Button("#top"))},
			"in a cta":       {cta},
			"in a pricing":   {v3Pat("pricing", "three-plans", "plans", "["+plan+"]")},
			"in a column":    {columns},
			"in a group":     {group},
			"beside a block": {v3Para, v3Button("#top")},
		} {
			t.Run(name, func(t *testing.T) {
				v3HeadAccepted(t, v3Outline(v3Sec("top", "", children...), v3Sec("end", "", v3Para)))
			})
		}
	})

	t.Run("a link resolves to an anchor that comes later, or earlier", func(t *testing.T) {
		v3HeadAccepted(t, v3Outline(v3Sec("one", "", v3Button("#two")), v3Sec("two", "", v3Para)))
		v3HeadAccepted(t, v3Outline(v3Sec("one", "", v3Para), v3Sec("two", "", v3Button("#one"))))
	})

	t.Run("a link to no anchor of the page is link_anchor_missing", func(t *testing.T) {
		plan := `{"name":"Starter","price":"19","features":["One site"],"cta":{"text":"Choose","url":"#nowhere"}}`
		for name, children := range map[string][]string{
			"a button":       {v3Button("#nowhere")},
			"a hero button":  {v3BlockPat("hero", "centered", v3H1, v3Button("#nowhere"))},
			"a cta button":   {v3BlockPat("cta", "band", v3Button("#nowhere"))},
			"a plan":         {v3Pat("pricing", "three-plans", "plans", "["+plan+"]")},
			"a group button": {`{"type":"group","children":[` + v3Button("#nowhere") + `]}`},
		} {
			t.Run(name, func(t *testing.T) {
				v3HeadRefused(t, v3Outline(v3Sec("top", "", children...)), pageCreateLinkAnchorMissing)
			})
		}
		// One link of several is the one that misses.
		v3HeadRefused(t, v3Outline(v3Sec("top", "", v3Button("#top"), v3Button("#nowhere"))), pageCreateLinkAnchorMissing)
		// Near misses are misses.
		v3HeadRefused(t, v3Outline(v3Sec("top", "", v3Button("#to"))), pageCreateLinkAnchorMissing)
		v3HeadRefused(t, v3Outline(v3Sec("top", "", v3Button("#top-"))), pageCreateLinkAnchorMissing)
		// An id the theme owns is no anchor of the page.
		v3HeadRefused(t, v3Outline(v3Sec("top", "", v3Button("#content"))), pageCreateLinkAnchorMissing)
	})

	t.Run("a link that is not the shape of an anchor is link_invalid", func(t *testing.T) {
		// A section named like the link is no help: the link is malformed.
		long := "#a" + strings.Repeat("b", 40)
		for _, url := range []string{"#", "#a", "#A", "#Top", "#Pricing", "#1a", "#-a", "#a_b", "#a.b", "#a%20b", "#top#top", long} {
			v3HeadRefused(t, v3Outline(v3Sec("top", "", v3Button(url)), v3Sec("pricing", "", v3Para)), pageCreateLinkInvalid)
		}
	})

	t.Run("an address with a fragment, and a path, are not anchors of the page", func(t *testing.T) {
		for _, url := range []string{"/pricing#plans", "https://example.com/page#section", "/#top", "https://example.com/#nowhere"} {
			v3HeadAccepted(t, v3Outline(v3Sec("top", "", v3Button(url))))
		}
		v3HeadRefused(t, v3Outline(v3Sec("top", "", v3Button("javascript:alert(1)"))), pageCreateLinkInvalid)
		v3HeadRefused(t, v3Outline(v3Sec("top", "", v3Button("http://example.com/"))), pageCreateLinkInvalid)
	})

	t.Run("version 2 has no anchors to link to", func(t *testing.T) {
		v2 := v3HeadInput(map[string]string{"outline": "[" + v3Button("#top") + "]"}, "outline_version", "design")
		v3HeadRefused(t, v2, pageCreateLinkInvalid)
	})
}

func TestOutlineV3ButtonStyles(t *testing.T) {
	style := func(s string) []byte {
		return v3Outline(v3Sec("top", "", `{"type":"buttons","buttons":[{"text":"Go","url":"/go","style":`+s+`}]}`))
	}
	for _, s := range []string{"fill", "outline", "text"} {
		v3HeadAccepted(t, style(`"`+s+`"`))
	}
	for _, s := range []string{`"ghost"`, `"Fill"`, `"TEXT"`, `""`, `null`, `1`, `true`, `["text"]`} {
		v3HeadRefused(t, style(s), pageCreateBadInput)
	}
	// A button with no style is accepted, and a hero or a cta button takes the same three.
	v3HeadAccepted(t, v3Outline(v3Sec("top", "", v3Button("/go"))))
	for _, s := range []string{"fill", "outline", "text"} {
		btn := `{"type":"buttons","buttons":[{"text":"Go","url":"/go","style":"` + s + `"}]}`
		v3HeadAccepted(t, v3Outline(v3Sec("top", "", v3BlockPat("hero", "centered", v3H1, btn))))
		v3HeadAccepted(t, v3Outline(v3Sec("top", "", v3BlockPat("cta", "band", btn))))
	}
	// Version 2 keeps its two styles.
	v2 := func(s string) []byte {
		return v3HeadInput(map[string]string{"outline": `[{"type":"buttons","buttons":[{"text":"Go","url":"/go","style":"` + s + `"}]}]`}, "outline_version", "design")
	}
	v3HeadAccepted(t, v2("fill"))
	v3HeadAccepted(t, v2("outline"))
	v3HeadRefused(t, v2("text"), pageCreateBadInput)
}

func TestOutlineV3HeroPlace(t *testing.T) {
	t.Run("the hero is the first child of the first section", func(t *testing.T) {
		for _, variant := range []string{"background", "split", "centered"} {
			hero := v3BlockPat("hero", variant, v3H1, v3Para)
			v3HeadAccepted(t, v3Outline(v3Sec("top", "", hero)))
			v3HeadAccepted(t, v3Outline(v3Sec("top", "", hero, v3Para, v3Heading), v3Sec("more", "", v3Para)))
		}
	})

	t.Run("a hero anywhere else is hero_misplaced", func(t *testing.T) {
		for name, page := range map[string][]byte{
			"the second section": v3Outline(v3Sec("one", "", v3Para), v3Sec("two", "", v3Hero)),
			"the third section":  v3Outline(v3Sec("one", "", v3Para), v3Sec("two", "", v3Para), v3Sec("three", "", v3Hero)),
			"after a paragraph":  v3Outline(v3Sec("one", "", v3Para, v3Hero)),
			"after a heading":    v3Outline(v3Sec("one", "", v3Heading, v3Hero)),
			"after a pattern":    v3Outline(v3Sec("one", "", v3Pat("faq", "accordion", "faq", v3Repeat(`{"question":"How long?","answer":"A week."}`, 1)), v3Hero)),
			"second of a later":  v3Outline(v3Sec("one", "", v3Para), v3Sec("two", "", v3Para, v3Hero)),
		} {
			t.Run(name, func(t *testing.T) { v3HeadRefused(t, page, pageCreateHeroMisplaced) })
		}
	})

	t.Run("a hero that is not a child of a section stays layout_invalid", func(t *testing.T) {
		group := `{"type":"group","children":[` + v3Hero + `]}`
		column := `{"type":"columns","columns":[{"children":[` + v3Hero + `]},{"children":[` + v3Para + `]}]}`
		v3HeadRefused(t, v3Outline(v3Sec("one", "", group)), pageCreateLayoutInvalid)
		v3HeadRefused(t, v3Outline(v3Sec("one", "", column)), pageCreateLayoutInvalid)
	})

	t.Run("only the hero is tied to a place", func(t *testing.T) {
		faq := v3Pat("faq", "accordion", "faq", v3Repeat(`{"question":"How long?","answer":"A week."}`, 1))
		cta := v3BlockPat("cta", "band", v3Button("/go"))
		v3HeadAccepted(t, v3Outline(v3Sec("one", "", faq), v3Sec("two", "", v3Para, cta)))
		v3HeadAccepted(t, v3Outline(v3Sec("one", "", v3Para), v3Sec("two", "", cta, faq)))
	})

	t.Run("a second hero is told it is one too many, not that it is misplaced", func(t *testing.T) {
		v3HeadRefused(t, v3Outline(v3Sec("one", "", v3Hero), v3Sec("two", "", v3Hero)), pageCreateLayoutInvalid)
		v3HeadRefused(t, v3Outline(v3Sec("one", "", v3Para), v3Sec("two", "", v3Hero), v3Sec("three", "", v3Hero)), pageCreateLayoutInvalid)
	})

	t.Run("the place counts sections as they are walked, not as they are named", func(t *testing.T) {
		// Section 1 is the section at the top of the outline whatever its anchor.
		v3HeadAccepted(t, v3Outline(v3Sec("zzz", "", v3Hero), v3Sec("aaa", "", v3Para)))
		v3HeadRefused(t, v3Outline(v3Sec("aaa", "", v3Para), v3Sec("zzz", "", v3Hero)), pageCreateHeroMisplaced)
	})
}

func TestOutlineV3HeadingLevelOne(t *testing.T) {
	t.Run("the hero may hold the one level 1 heading, or none", func(t *testing.T) {
		v3HeadAccepted(t, v3Outline(v3Sec("top", "", v3Hero)))
		v3HeadAccepted(t, v3Outline(v3Sec("top", "", v3BlockPat("hero", "split", `{"type":"heading","level":2,"text":"Lead"}`, v3Para))))
		v3HeadAccepted(t, v3Outline(v3Sec("top", "", v3BlockPat("hero", "split", v3H1, `{"type":"heading","level":2,"text":"Lead"}`, v3Para))))
	})

	t.Run("a level 1 heading outside the hero is h1_outside_hero", func(t *testing.T) {
		for name, page := range map[string][]byte{
			"in a section":       v3Outline(v3Sec("top", "", v3H1)),
			"in a later section": v3Outline(v3Sec("top", "", v3Hero), v3Sec("more", "", v3H1)),
			"beside the hero":    v3Outline(v3Sec("top", "", v3Hero, v3H1)),
			"in a group":         v3Outline(v3Sec("top", "", `{"type":"group","children":[`+v3H1+`]}`)),
			"in a column":        v3Outline(v3Sec("top", "", `{"type":"columns","columns":[{"children":[`+v3H1+`]},{"children":[`+v3Para+`]}]}`)),
			"in a cta":           v3Outline(v3Sec("top", "", v3BlockPat("cta", "band", v3H1, v3Button("/go")))),
			"after a paragraph":  v3Outline(v3Sec("top", "", v3Para, v3H1)),
		} {
			t.Run(name, func(t *testing.T) { v3HeadRefused(t, page, pageCreateH1OutsideHero) })
		}
	})

	t.Run("a second level 1 heading in the hero is layout_invalid", func(t *testing.T) {
		v3HeadRefused(t, v3Outline(v3Sec("top", "", v3BlockPat("hero", "centered", v3H1, v3H1))), pageCreateLayoutInvalid)
		v3HeadRefused(t, v3Outline(v3Sec("top", "", v3BlockPat("hero", "centered", v3H1, v3Para, v3H1))), pageCreateLayoutInvalid)
	})

	t.Run("a level 1 heading is judged as a heading first", func(t *testing.T) {
		// A member the heading does not have, or a text that is not a string,
		// is bad_input wherever the heading stands.
		for _, h := range []string{
			`{"type":"heading","level":1,"text":"Hi","id":"x"}`,
			`{"type":"heading","level":1,"text":5}`,
			`{"type":"heading","level":1}`,
		} {
			v3HeadRefused(t, v3Outline(v3Sec("top", "", h)), pageCreateBadInput)
			v3HeadRefused(t, v3Outline(v3Sec("top", "", v3BlockPat("hero", "centered", h))), pageCreateBadInput)
		}
	})

	t.Run("the other levels are as they were", func(t *testing.T) {
		for _, level := range []string{"2", "3", "4"} {
			h := `{"type":"heading","level":` + level + `,"text":"Hi"}`
			v3HeadAccepted(t, v3Outline(v3Sec("top", "", h)))
			v3HeadAccepted(t, v3Outline(v3Sec("top", "", v3BlockPat("hero", "centered", h))))
		}
		for _, level := range []string{"0", "5", "-1", "1.5", `"1"`, "null"} {
			h := `{"type":"heading","level":` + level + `,"text":"Hi"}`
			v3HeadRefused(t, v3Outline(v3Sec("top", "", h)), pageCreateBadInput)
			v3HeadRefused(t, v3Outline(v3Sec("top", "", v3BlockPat("hero", "centered", h))), pageCreateBadInput)
		}
	})

	t.Run("version 2 has no level 1 heading", func(t *testing.T) {
		v2 := v3HeadInput(map[string]string{"outline": "[" + v3H1 + "]"}, "outline_version", "design")
		v3HeadRefused(t, v2, pageCreateBadInput)
	})
}

func TestOutlineV3InputSize(t *testing.T) {
	// The text of an input is at most 65536 bytes, however many of them the
	// padding makes. The shared table has the two edges for a designed page.
	pad := func(input []byte, to int) []byte {
		return []byte(string(input) + strings.Repeat(" ", to-len(input)))
	}
	designed := v3HeadInput(nil)
	v3HeadAccepted(t, pad(designed, 65536))
	v3HeadRefused(t, pad(designed, 65537), pageCreateBadInput)
	v2 := v3HeadInput(nil, "outline_version", "design")
	v3HeadAccepted(t, pad(v2, 65536))
	v3HeadRefused(t, pad(v2, 65537), pageCreateBadInput)
}
