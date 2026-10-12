package mcp

import (
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The pattern nodes of a designed outline: hero and cta hold blocks, the other
// six hold one array of typed items. The shared case table has the honest case
// of every pattern and the refusal of each cap; these tests cover the shapes
// around them.

// v3Patterns are the item patterns: the array each keeps its items in, its
// variants, its item caps and one valid item.
var v3Patterns = []struct {
	name     string
	variants []string
	array    string
	min, max int
	item     string
}{
	{"features", []string{"split", "bento", "grid"}, "features", 3, 8, `{"icon":"bolt","title":"Fast setup","text":"Live in an afternoon."}`},
	{"testimonials", []string{"featured"}, "testimonials", 1, 6, `{"quote":"It paid for itself.","name":"Dana Whitfield"}`},
	{"pricing", []string{"three-plans"}, "plans", 1, 4, `{"name":"Starter","price":"19","features":["One site"],"cta":{"text":"Choose","url":"/contact"}}`},
	{"faq", []string{"accordion"}, "faq", 1, 12, `{"question":"How long?","answer":"A week."}`},
	{"logos", []string{"row"}, "logos", 3, 10, `{"attachment_id":101,"alt":"Acme logo"}`},
	{"stats", []string{"counters"}, "stats", 2, 4, `{"value":1200,"suffix":"+","label":"sites launched"}`},
}

func v3Repeat(item string, n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = item
	}
	return "[" + strings.Join(items, ",") + "]"
}

// v3Pat is an item pattern node whose array holds items (a JSON array).
func v3Pat(name, variant, array, items string) string {
	return `{"type":"pattern","pattern":"` + name + `","variant":"` + variant + `","` + array + `":` + items + `}`
}

// v3BlockPat is a hero or cta node whose children are these blocks.
func v3BlockPat(name, variant string, children ...string) string {
	return `{"type":"pattern","pattern":"` + name + `","variant":"` + variant + `","children":[` + strings.Join(children, ",") + `]}`
}

// v3Page is a designed page whose one section holds these children.
func v3Page(children ...string) []byte {
	return v3Outline(v3Sec("body", "", children...))
}

func TestOutlineV3ItemPatterns(t *testing.T) {
	for _, p := range v3Patterns {
		t.Run(p.name, func(t *testing.T) {
			ok := func(variant string, n int) []byte {
				return v3Page(v3Pat(p.name, variant, p.array, v3Repeat(p.item, n)))
			}

			t.Run("every variant at both edges of its item count", func(t *testing.T) {
				for _, variant := range p.variants {
					v3HeadAccepted(t, ok(variant, p.min))
					v3HeadAccepted(t, ok(variant, p.max))
				}
			})
			t.Run("outside its item count is pattern_items_out_of_range", func(t *testing.T) {
				v3HeadRefused(t, ok(p.variants[0], p.min-1), pageCreateItemsOutOfRange)
				v3HeadRefused(t, ok(p.variants[0], p.max+1), pageCreateItemsOutOfRange)
				v3HeadRefused(t, ok(p.variants[0], 0), pageCreateItemsOutOfRange)
			})
			t.Run("a variant of the pattern, from its list", func(t *testing.T) {
				for _, variant := range []string{"", "Split", "other", "split ", "three_plans", "background"} {
					if slices.Contains(p.variants, variant) {
						continue
					}
					v3HeadRefused(t, ok(variant, p.min), pageCreateBadInput)
				}
				v3HeadRefused(t, v3Page(strings.Replace(v3Pat(p.name, p.variants[0], p.array, v3Repeat(p.item, p.min)), `"variant":"`+p.variants[0]+`"`, `"variant":5`, 1)), pageCreateBadInput)
				v3HeadRefused(t, v3Page(strings.Replace(v3Pat(p.name, p.variants[0], p.array, v3Repeat(p.item, p.min)), `"variant":"`+p.variants[0]+`",`, ``, 1)), pageCreateBadInput)
			})
			t.Run("the node is closed and its array is an array", func(t *testing.T) {
				good := v3Pat(p.name, p.variants[0], p.array, v3Repeat(p.item, p.min))
				v3HeadRefused(t, v3Page(strings.Replace(good, `{"type":"pattern",`, `{"type":"pattern","extra":1,`, 1)), pageCreateBadInput)
				v3HeadRefused(t, v3Page(strings.Replace(good, `"`+p.array+`":`, `"items":`, 1)), pageCreateBadInput)
				v3HeadRefused(t, v3Page(strings.Replace(good, `"`+p.array+`":`, `"children":`, 1)), pageCreateBadInput)
				for _, not := range []string{`{}`, `null`, `"x"`, `5`} {
					v3HeadRefused(t, v3Page(v3Pat(p.name, p.variants[0], p.array, not)), pageCreateBadInput)
				}
				v3HeadRefused(t, v3Page(`{"type":"pattern","pattern":"`+p.name+`","variant":"`+p.variants[0]+`"}`), pageCreateBadInput)
			})
			t.Run("an item is a closed object", func(t *testing.T) {
				v3HeadRefused(t, v3Page(v3Pat(p.name, p.variants[0], p.array, v3Repeat(`7`, p.min))), pageCreateBadInput)
				v3HeadRefused(t, v3Page(v3Pat(p.name, p.variants[0], p.array, v3Repeat(`null`, p.min))), pageCreateBadInput)
				v3HeadRefused(t, v3Page(v3Pat(p.name, p.variants[0], p.array, v3Repeat(`{}`, p.min))), pageCreateBadInput)
				extra := strings.Replace(p.item, "{", `{"extra":1,`, 1)
				v3HeadRefused(t, v3Page(v3Pat(p.name, p.variants[0], p.array, v3Repeat(extra, p.min))), pageCreateBadInput)
			})
			t.Run("the text of an item is the agent's to judge", func(t *testing.T) {
				marked := strings.NewReplacer(`"Fast setup"`, `"<b>Fast</b>"`, `"Live in an afternoon."`, `"See [shortcode] &amp; more"`,
					`"It paid for itself."`, `"<i>Paid</i>"`, `"Dana Whitfield"`, `"<Dana>"`, `"Starter"`, `"<Starter>"`,
					`"How long?"`, `"<How>"`, `"A week."`, `"<A>"`, `"Acme logo"`, `"<Acme>"`, `"sites launched"`, `"<sites>"`).Replace(p.item)
				v3HeadAccepted(t, v3Page(v3Pat(p.name, p.variants[0], p.array, v3Repeat(marked, p.min))))
			})
		})
	}
}

func TestOutlineV3PatternNames(t *testing.T) {
	items := v3Repeat(v3Patterns[0].item, 3)
	for _, name := range []string{`"Features"`, `"feature"`, `"hero2"`, `""`, `5`, `null`, `["features"]`} {
		node := `{"type":"pattern","pattern":` + name + `,"variant":"split","features":` + items + `}`
		v3HeadRefused(t, v3Page(node), pageCreateBadInput)
	}
	v3HeadRefused(t, v3Page(`{"type":"pattern","variant":"split","features":`+items+`}`), pageCreateBadInput)
	// A pattern is a node of a designed page alone.
	v2 := v3HeadInput(map[string]string{"outline": "[" + v3Pat("features", "split", "features", items) + "]"}, "outline_version", "design")
	v3HeadRefused(t, v2, pageCreateBadInput)
}

func TestOutlineV3PatternPlacement(t *testing.T) {
	faq := v3Pat("faq", "accordion", "faq", v3Repeat(v3Patterns[3].item, 1))
	hero := v3BlockPat("hero", "centered", v3Para)
	for name, node := range map[string]string{
		"in a group":   `{"type":"group","children":[` + faq + `]}`,
		"in a column":  `{"type":"columns","columns":[{"children":[` + faq + `]},{"children":[` + v3Para + `]}]}`,
		"in a hero":    v3BlockPat("hero", "centered", v3Para, faq),
		"in a cta":     v3BlockPat("cta", "band", v3Para, faq),
		"hero in hero": v3BlockPat("hero", "centered", v3Para, hero),
	} {
		t.Run(name, func(t *testing.T) { v3HeadRefused(t, v3Page(node), pageCreateLayoutInvalid) })
	}
	v3HeadAccepted(t, v3Page(v3Para, faq, v3Heading, v3Pat("stats", "counters", "stats", v3Repeat(v3Patterns[5].item, 2))))
}

func TestOutlineV3PatternIcons(t *testing.T) {
	page := func(icon string) []byte {
		item := `{"icon":` + icon + `,"title":"Fast setup","text":"Live in an afternoon."}`
		return v3Page(v3Pat("features", "grid", "features", v3Repeat(item, 3)))
	}
	t.Run("every icon of the closed list", func(t *testing.T) {
		if len(pageIcons) == 0 {
			t.Fatal("the icon list is empty")
		}
		for _, icon := range pageIcons {
			v3HeadAccepted(t, page(`"`+icon+`"`))
		}
	})
	t.Run("a name outside the list is icon_unknown", func(t *testing.T) {
		for _, icon := range []string{"unicorn", "Bolt", "BOLT", "", " bolt", "bolt ", "fa-bolt", "fas fa-bolt", "bolt.svg", "arrow_right"} {
			v3HeadRefused(t, page(`"`+icon+`"`), pageCreateIconUnknown)
		}
	})
	t.Run("a value that is no name is bad_input", func(t *testing.T) {
		for _, icon := range []string{`5`, `null`, `["bolt"]`, `{"value":"bolt"}`, `true`} {
			v3HeadRefused(t, page(icon), pageCreateBadInput)
		}
	})
	t.Run("an item with an unknown icon and a missing title is bad_input", func(t *testing.T) {
		item := `{"icon":"unicorn","text":"Live in an afternoon."}`
		v3HeadRefused(t, v3Page(v3Pat("features", "grid", "features", v3Repeat(item, 3))), pageCreateBadInput)
	})
}

// TestPageIconsAreTheTables: the closed icon list is the shared table's list,
// name for name and in its order.
func TestPageIconsAreTheTables(t *testing.T) {
	b, err := os.ReadFile(pageV3CasesFixture)
	if err != nil {
		t.Fatalf("read %s: %v", pageV3CasesFixture, err)
	}
	var doc struct {
		Icons []string `json:"icons"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("decode %s: %v", pageV3CasesFixture, err)
	}
	if len(doc.Icons) == 0 {
		t.Fatal("the table lists no icons")
	}
	if !slices.Equal(pageIcons, doc.Icons) {
		t.Errorf("pageIcons = %v\n table icons = %v", pageIcons, doc.Icons)
	}
}

func TestOutlineV3ItemFields(t *testing.T) {
	testimonials := func(item string) []byte {
		return v3Page(v3Pat("testimonials", "featured", "testimonials", "["+item+"]"))
	}
	pricing := func(item string) []byte { return v3Page(v3Pat("pricing", "three-plans", "plans", "["+item+"]")) }
	stats := func(item string) []byte { return v3Page(v3Pat("stats", "counters", "stats", v3Repeat(item, 2))) }
	logos := func(ids ...int) string {
		items := make([]string, len(ids))
		for i, id := range ids {
			items[i] = `{"attachment_id":` + strconv.Itoa(id) + `,"alt":"Logo"}`
		}
		return v3Pat("logos", "row", "logos", "["+strings.Join(items, ",")+"]")
	}

	t.Run("testimonials: a portrait is an image of the page", func(t *testing.T) {
		f := v3HeadAccepted(t, testimonials(`{"quote":"Great.","name":"Dana","role":"Owner","attachment_id":201}`))
		if !slices.Equal(f.mediaIDs, []int64{201}) {
			t.Errorf("mediaIDs = %v, want [201]", f.mediaIDs)
		}
		for _, bad := range []string{`"attachment_id":0`, `"attachment_id":-1`, `"attachment_id":"201"`, `"attachment_id":2147483648`, `"attachment_id":null`, `"role":5`, `"role":null`} {
			v3HeadRefused(t, testimonials(`{"quote":"Great.","name":"Dana",`+bad+`}`), pageCreateBadInput)
		}
		v3HeadRefused(t, testimonials(`{"name":"Dana"}`), pageCreateBadInput)
		v3HeadRefused(t, testimonials(`{"quote":"Great."}`), pageCreateBadInput)
	})

	t.Run("pricing: plan features, a call to action and the highlight", func(t *testing.T) {
		plan := func(extra string) string {
			return `{"name":"Team","price":"49","period":"month","features":["Five sites"],"cta":{"text":"Choose","url":"/contact"}` + extra + `}`
		}
		v3HeadAccepted(t, pricing(plan(`,"highlighted":true`)))
		v3HeadAccepted(t, pricing(plan(`,"highlighted":false`)))
		v3HeadRefused(t, pricing(plan(`,"highlighted":"yes"`)), pageCreateBadInput)
		v3HeadRefused(t, pricing(plan(`,"highlighted":null`)), pageCreateBadInput)
		bullets := func(n int) string { return strings.Replace(plan(""), `["Five sites"]`, v3Repeat(`"Bullet"`, n), 1) }
		v3HeadAccepted(t, pricing(bullets(1)))
		v3HeadAccepted(t, pricing(bullets(8)))
		v3HeadRefused(t, pricing(bullets(0)), pageCreateItemsOutOfRange)
		v3HeadRefused(t, pricing(bullets(9)), pageCreateItemsOutOfRange)
		v3HeadRefused(t, pricing(strings.Replace(plan(""), `["Five sites"]`, `[5]`, 1)), pageCreateBadInput)
		v3HeadRefused(t, pricing(strings.Replace(plan(""), `["Five sites"]`, `"Five sites"`, 1)), pageCreateBadInput)
		for _, url := range []string{`"javascript:alert(1)"`, `"http://example.com"`, `"//example.com"`, `"contact"`, `""`, `"/a b"`} {
			v3HeadRefused(t, pricing(strings.Replace(plan(""), `"/contact"`, url, 1)), pageCreateLinkInvalid)
		}
		for _, bad := range []string{`"url":5`, `"url":null`} {
			v3HeadRefused(t, pricing(strings.Replace(plan(""), `"url":"/contact"`, bad, 1)), pageCreateBadInput)
		}
		v3HeadRefused(t, pricing(strings.Replace(plan(""), `"cta":{"text":"Choose","url":"/contact"}`, `"cta":{"text":"Choose"}`, 1)), pageCreateBadInput)
		v3HeadRefused(t, pricing(strings.Replace(plan(""), `"cta":{"text":"Choose","url":"/contact"}`, `"cta":{"text":"Choose","url":"/contact","style":"fill"}`, 1)), pageCreateBadInput)
		v3HeadRefused(t, pricing(strings.Replace(plan(""), `"cta":{"text":"Choose","url":"/contact"}`, `"cta":"/contact"`, 1)), pageCreateBadInput)
		v3HeadRefused(t, pricing(strings.Replace(plan(""), `"period":"month",`, `"period":5,`, 1)), pageCreateBadInput)
	})

	t.Run("stats: a whole number from 0 to 999999999 and an optional suffix", func(t *testing.T) {
		for _, ok := range []string{`0`, `1`, `999999999`} {
			v3HeadAccepted(t, stats(`{"value":`+ok+`,"label":"things"}`))
		}
		for _, bad := range []string{`-1`, `1000000000`, `1.5`, `"7"`, `null`, `true`, `1e3`} {
			v3HeadRefused(t, stats(`{"value":`+bad+`,"label":"things"}`), pageCreateBadInput)
		}
		v3HeadAccepted(t, stats(`{"value":7,"label":"things","suffix":"%"}`))
		v3HeadRefused(t, stats(`{"value":7,"label":"things","suffix":5}`), pageCreateBadInput)
		v3HeadRefused(t, stats(`{"value":7}`), pageCreateBadInput)
	})

	t.Run("logos: every logo is an image of the page, in order", func(t *testing.T) {
		f := v3HeadAccepted(t, v3Page(logos(31, 32, 31)))
		if !slices.Equal(f.mediaIDs, []int64{31, 32}) {
			t.Errorf("mediaIDs = %v, want [31 32]", f.mediaIDs)
		}
		v3HeadRefused(t, v3Page(logos(31, 32, 0)), pageCreateBadInput)
		v3HeadRefused(t, v3Page(strings.Replace(logos(31, 32, 33), `"alt":"Logo"`, `"alt":5`, 1)), pageCreateBadInput)
		v3HeadRefused(t, v3Page(strings.Replace(logos(31, 32, 33), `,"alt":"Logo"`, ``, 1)), pageCreateBadInput)
		// Two rows of ten are the 20 images of a page; a background is the 21st.
		ids := func(from int) []int {
			out := make([]int, 10)
			for i := range out {
				out[i] = from + i
			}
			return out
		}
		two := v3Sec("one", "", logos(ids(1)...)) + "," + v3Sec("two", "", logos(ids(11)...))
		v3HeadAccepted(t, v3Outline(two))
		v3HeadRefused(t, v3Outline(v3Sec("bg", `"background":{"attachment_id":99}`, v3Para), two), pageCreateLayoutInvalid)
	})
}

func TestOutlineV3HeroAndCta(t *testing.T) {
	buttons := `{"type":"buttons","buttons":[{"text":"Start","url":"/start","style":"fill"}]}`
	h1 := `{"type":"heading","level":1,"text":"Websites that earn their keep"}`

	t.Run("a hero holds 1 to 6 headings, paragraphs, buttons and images", func(t *testing.T) {
		for _, variant := range []string{"background", "split", "centered"} {
			f := v3HeadAccepted(t, v3Page(v3BlockPat("hero", variant, h1, v3Para, buttons, v3Image(42), v3Heading, v3Para)))
			if !slices.Equal(f.mediaIDs, []int64{42}) {
				t.Errorf("%s: mediaIDs = %v, want [42]", variant, f.mediaIDs)
			}
		}
		v3HeadAccepted(t, v3Page(v3BlockPat("hero", "split", h1)))
		v3HeadRefused(t, v3Page(v3BlockPat("hero", "split")), pageCreateLayoutInvalid)
		v3HeadRefused(t, v3Page(v3BlockPat("hero", "split", h1, v3Para, v3Para, v3Para, v3Para, v3Para, v3Para)), pageCreateLayoutInvalid)
		for _, bad := range []string{"", "band", "grid", "Split"} {
			v3HeadRefused(t, v3Page(v3BlockPat("hero", bad, h1)), pageCreateBadInput)
		}
	})

	t.Run("a hero heading is a level 1 to 4 heading", func(t *testing.T) {
		for _, level := range []string{"1", "2", "3", "4"} {
			v3HeadAccepted(t, v3Page(v3BlockPat("hero", "split", `{"type":"heading","level":`+level+`,"text":"Hi"}`)))
		}
		for _, level := range []string{"0", "5", `"1"`, "null"} {
			v3HeadRefused(t, v3Page(v3BlockPat("hero", "split", `{"type":"heading","level":`+level+`,"text":"Hi"}`)), pageCreateBadInput)
		}
	})

	t.Run("a hero holds nothing but those four blocks", func(t *testing.T) {
		for _, node := range []string{
			`{"type":"list","ordered":false,"items":["One"]}`,
			`{"type":"quote","paragraphs":["Well said."]}`,
			`{"type":"separator"}`,
			`{"type":"spacer","size":"small"}`,
			`{"type":"table","rows":[["1"]]}`,
			`{"type":"group","children":[` + v3Para + `]}`,
			`{"type":"columns","columns":[{"children":[` + v3Para + `]},{"children":[` + v3Para + `]}]}`,
			v3Sec("inner", "", v3Para),
		} {
			v3HeadRefused(t, v3Page(v3BlockPat("hero", "split", h1, node)), pageCreateLayoutInvalid)
		}
		v3HeadRefused(t, v3Page(v3BlockPat("hero", "split", h1, `{"type":"video"}`)), pageCreateBadInput)
		v3HeadRefused(t, v3Page(v3BlockPat("hero", "split", h1, `7`)), pageCreateBadInput)
	})

	t.Run("a hero is closed and holds children, not items", func(t *testing.T) {
		good := v3BlockPat("hero", "split", h1)
		v3HeadRefused(t, v3Page(strings.Replace(good, `{"type":"pattern",`, `{"type":"pattern","extra":1,`, 1)), pageCreateBadInput)
		v3HeadRefused(t, v3Page(strings.Replace(good, `"children":`, `"features":`, 1)), pageCreateBadInput)
		v3HeadRefused(t, v3Page(`{"type":"pattern","pattern":"hero","variant":"split"}`), pageCreateBadInput)
		for _, not := range []string{`{}`, `null`, `"x"`} {
			v3HeadRefused(t, v3Page(`{"type":"pattern","pattern":"hero","variant":"split","children":`+not+`}`), pageCreateBadInput)
		}
	})

	t.Run("a cta holds 1 to 4 headings, paragraphs and buttons", func(t *testing.T) {
		v3HeadAccepted(t, v3Page(v3BlockPat("cta", "band", v3Heading, v3Para, buttons, v3Para)))
		v3HeadAccepted(t, v3Page(v3BlockPat("cta", "band", buttons)))
		v3HeadRefused(t, v3Page(v3BlockPat("cta", "band")), pageCreateLayoutInvalid)
		v3HeadRefused(t, v3Page(v3BlockPat("cta", "band", v3Para, v3Para, v3Para, v3Para, v3Para)), pageCreateLayoutInvalid)
		v3HeadRefused(t, v3Page(v3BlockPat("cta", "band", v3Heading, v3Image(42))), pageCreateLayoutInvalid)
		for _, bad := range []string{"", "split", "centered", "Band"} {
			v3HeadRefused(t, v3Page(v3BlockPat("cta", bad, v3Heading)), pageCreateBadInput)
		}
	})

	t.Run("a button of a hero is judged as any button is", func(t *testing.T) {
		v3HeadRefused(t, v3Page(v3BlockPat("hero", "split", h1, `{"type":"buttons","buttons":[{"text":"Go","url":"javascript:x"}]}`)), pageCreateLinkInvalid)
		v3HeadRefused(t, v3Page(v3BlockPat("hero", "split", h1, `{"type":"buttons","buttons":[]}`)), pageCreateLayoutInvalid)
	})

	t.Run("one hero in the first section is a page", func(t *testing.T) {
		f := v3HeadAccepted(t, v3Outline(v3Sec("top", "", v3BlockPat("hero", "centered", h1, v3Para)), v3Sec("more", "", v3Para)))
		if f.pageFrame != pageFrameFullWidth {
			t.Errorf("pageFrame = %q, want full_width for a page with a hero", f.pageFrame)
		}
	})
}
