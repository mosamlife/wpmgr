package mcp

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// designedCPatterns are the patterns this area compiles. A golden case that
// holds one of them is built with the twin's own compiler for it, never with
// the golden's subtree standing in, so a case in the gate cannot pass without
// testing the compile.
var designedCPatterns = []string{"testimonials", "faq"}

// designedCGate are the gate entries of this area: each must be covered by a
// golden case that holds one of the patterns above.
var designedCGate = []string{"testimonials-", "faq-", "escapes-in-every-pattern"}

// TestDesignedTestimonialsAndFAQAreBuiltByTheTwin: for every golden case that
// holds a testimonials or faq pattern, the twin builds those patterns itself
// and the page's tree is the agent's byte for byte. Patterns of other areas
// stand in with the golden's subtree until the twin compiles them.
func TestDesignedTestimonialsAndFAQAreBuiltByTheTwin(t *testing.T) {
	for _, name := range designedCPatterns {
		if designedPatternBuilders[name] == nil {
			t.Fatalf("the %s pattern is not registered: the twin does not compile it", name)
		}
	}
	covered := map[string]bool{}
	for _, c := range readDesignedGolden(t) {
		input := []byte(c.Input)
		kinds := designedPatternKinds(t, input)
		if !slices.ContainsFunc(kinds, func(k string) bool { return slices.Contains(designedCPatterns, k) }) {
			continue
		}
		for _, p := range designedCGate {
			if strings.HasPrefix(c.Name, p) {
				covered[p] = true
			}
		}
		t.Run(c.Name, func(t *testing.T) {
			golden, ok := phpDecodeDocument(c.Tree, elementorTreeMaxDepth, true)
			if !ok {
				t.Fatal("the golden tree does not decode")
			}
			goldenTree, _ := golden.(phpList)
			patterns := map[string]designedPatternFunc{}
			for _, kind := range kinds {
				if build := designedPatternBuilders[kind]; build != nil {
					patterns[kind] = build
				} else {
					patterns[kind] = goldenPatternStandIn(goldenTree)
				}
			}
			tree, ok := designedTreeWith(input, c.Facts.RequestID, c.Facts.ActiveBreakpoints, c.Facts.media(t), patterns)
			if !ok {
				t.Fatal("the control plane could not build the tree")
			}
			got, ok := phpEncodeBytes(tree)
			if !ok {
				t.Fatal("the tree did not encode")
			}
			if want := mustCanonical(t, c.Tree); !bytes.Equal(got, want) {
				t.Fatalf("the control plane's tree differs from the agent's golden\n%s", designedTreeDifference(got, want))
			}
		})
	}
	for _, p := range designedCGate {
		if !covered[p] {
			t.Errorf("no golden case named %q holds a testimonials or faq pattern", p)
		}
	}
}

// TestDesignedPatternLabels: the label names the pattern and the variant, and
// there is none for a pattern or a variant the grammar does not have.
func TestDesignedPatternLabels(t *testing.T) {
	for _, c := range []struct{ pattern, variant, want string }{
		{"testimonials", "featured", "WPMgr · Testimonials featured"},
		{"faq", "accordion", "WPMgr · FAQ accordion"},
		{"features", "grid", "WPMgr · Features grid"},
		{"cta", "band", "WPMgr · CTA band"},
	} {
		if got, ok := designedPatternLabel(c.pattern, c.variant); !ok || got != c.want {
			t.Errorf("%s %s: label %q (%v), want %q", c.pattern, c.variant, got, ok, c.want)
		}
	}
	for _, c := range []struct{ pattern, variant string }{
		{"testimonials", "grid"}, {"faq", ""}, {"carousel", "featured"}, {"", "accordion"},
	} {
		if got, ok := designedPatternLabel(c.pattern, c.variant); ok {
			t.Errorf("%s %s: got the label %q for a pattern the grammar does not have", c.pattern, c.variant, got)
		}
	}
}

// TestDesignedItemPatternsRefuseWhatTheAgentRefuses: the twin builds no tree
// for a testimonials or faq pattern the agent's compile refuses, and builds one
// for the honest pattern next to each refusal.
func TestDesignedItemPatternsRefuseWhatTheAgentRefuses(t *testing.T) {
	picture := func(mime string) []pageMediaFact {
		return []pageMediaFact{{ID: 42, URL: "https://example.com/wp-content/uploads/img-42.jpg", Filename: "img-42.jpg", Mime: mime, Width: 1600, Height: 900}}
	}
	repeat := func(item string, n int) string {
		return strings.TrimSuffix(strings.Repeat(item+",", n), ",")
	}
	quote := `{"quote":"Q","name":"N"}`
	question := `{"question":"Q?","answer":"A."}`
	testimonials := func(items string) string {
		return `{"type":"pattern","pattern":"testimonials","variant":"featured","testimonials":[` + items + `]}`
	}
	faq := func(items string) string {
		return `{"type":"pattern","pattern":"faq","variant":"accordion","faq":[` + items + `]}`
	}
	section := func(patternNode string) string {
		return `{"type":"section","anchor":"top","children":[` + patternNode + `]}`
	}

	cases := []struct {
		name     string
		node     string
		media    []pageMediaFact
		accepted bool
	}{
		{"the honest testimonial", testimonials(quote), nil, true},
		{"a testimonial with a role", testimonials(`{"quote":"Q","name":"N","role":"R"}`), nil, true},
		{"a testimonial with a picture", testimonials(`{"quote":"Q","name":"N","attachment_id":42}`), picture("image/jpeg"), true},
		{"the most testimonials", testimonials(repeat(quote, 6)), nil, true},
		{"the honest question", faq(question), nil, true},
		{"the most questions", faq(repeat(question, 12)), nil, true},

		{"no testimonials", testimonials(``), nil, false},
		{"too many testimonials", testimonials(repeat(quote, 7)), nil, false},
		{"a variant that is not one", `{"type":"pattern","pattern":"testimonials","variant":"grid","testimonials":[` + quote + `]}`, nil, false},
		{"no variant", `{"type":"pattern","pattern":"testimonials","testimonials":[` + quote + `]}`, nil, false},
		{"items that are not a list", `{"type":"pattern","pattern":"testimonials","variant":"featured","testimonials":` + quote + `}`, nil, false},
		{"an item that is not an object", testimonials(`"Q"`), nil, false},
		{"a testimonial with no quote", testimonials(`{"name":"N"}`), nil, false},
		{"a testimonial with an empty quote", testimonials(`{"quote":"","name":"N"}`), nil, false},
		{"a testimonial with a quote that is not text", testimonials(`{"quote":3,"name":"N"}`), nil, false},
		{"a testimonial with no name", testimonials(`{"quote":"Q"}`), nil, false},
		{"a testimonial with an empty name", testimonials(`{"quote":"Q","name":""}`), nil, false},
		{"a testimonial with an empty role", testimonials(`{"quote":"Q","name":"N","role":""}`), nil, false},
		{"a testimonial with a role of null", testimonials(`{"quote":"Q","name":"N","role":null}`), nil, false},
		{"a testimonial with a role that is not text", testimonials(`{"quote":"Q","name":"N","role":7}`), nil, false},
		{"a picture the site did not resolve", testimonials(`{"quote":"Q","name":"N","attachment_id":42}`), nil, false},
		{"a picture that is not an image", testimonials(`{"quote":"Q","name":"N","attachment_id":42}`), picture("application/pdf"), false},
		{"a picture of null", testimonials(`{"quote":"Q","name":"N","attachment_id":null}`), picture("image/jpeg"), false},
		{"a picture named by text", testimonials(`{"quote":"Q","name":"N","attachment_id":"42"}`), picture("image/jpeg"), false},
		{"a picture numbered 0", testimonials(`{"quote":"Q","name":"N","attachment_id":0}`), picture("image/jpeg"), false},
		{"a refused testimonial after honest ones", testimonials(repeat(quote, 3) + `,{"quote":"Q"}`), nil, false},

		{"no questions", faq(``), nil, false},
		{"too many questions", faq(repeat(question, 13)), nil, false},
		{"a faq variant that is not one", `{"type":"pattern","pattern":"faq","variant":"tabs","faq":[` + question + `]}`, nil, false},
		{"a question that is not an object", faq(`"Q?"`), nil, false},
		{"an empty question", faq(`{"question":"","answer":"A."}`), nil, false},
		{"a question with no answer", faq(`{"question":"Q?"}`), nil, false},
		{"an empty answer", faq(`{"question":"Q?","answer":""}`), nil, false},
		{"an answer that is not text", faq(`{"question":"Q?","answer":4}`), nil, false},
		{"a refused question after honest ones", faq(repeat(question, 5) + `,{"answer":"A."}`), nil, false},
	}
	for _, c := range cases {
		tree, ok := designedTreeWith(designedPageInput(section(c.node)), designedVectorRequestID, []string{"mobile", "tablet"}, c.media, designedPatternBuilders)
		if ok != c.accepted {
			t.Errorf("%s: built %v, want %v", c.name, ok, c.accepted)
		}
		if ok && len(tree) != 1 {
			t.Errorf("%s: %d sections built for one", c.name, len(tree))
		}
	}
}

// TestDesignedTestimonialsRowsAreCounted: the featured testimonial stands
// alone, the others in rows of two; a row left with one column is centred, and
// no other is.
func TestDesignedTestimonialsRowsAreCounted(t *testing.T) {
	for _, c := range []struct {
		count   int
		rows    int
		centred []bool
	}{
		{1, 0, nil},
		{2, 1, []bool{true}},
		{3, 1, []bool{false}},
		{4, 2, []bool{false, true}},
		{5, 2, []bool{false, false}},
		{6, 3, []bool{false, false, true}},
	} {
		var items []string
		for i := 1; i <= c.count; i++ {
			items = append(items, `{"quote":"Q`+strconv.Itoa(i)+`","name":"N"}`)
		}
		node := `{"type":"pattern","pattern":"testimonials","variant":"featured","testimonials":[` + strings.Join(items, ",") + `]}`
		tree, ok := designedTreeWith(designedPageInput(`{"type":"section","anchor":"top","children":[`+node+`]}`), designedVectorRequestID, nil, nil, designedPatternBuilders)
		if !ok {
			t.Fatalf("%d testimonials: no tree", c.count)
		}
		section, _ := tree[0].(phpObject)
		elements, _ := phpMember(section, "elements").(phpList)
		group, _ := elements[0].(phpObject)
		parts, _ := phpMember(group, "elements").(phpList)
		if len(parts) != 1+c.rows {
			t.Errorf("%d testimonials: %d parts in the group, want the featured one and %d rows", c.count, len(parts), c.rows)
			continue
		}
		for r, want := range c.centred {
			row, _ := parts[1+r].(phpObject)
			settings, _ := phpMember(row, "settings").(phpObject)
			if got := phpMember(settings, "flex_justify_content") == "center"; got != want {
				t.Errorf("%d testimonials: row %d centred is %v, want %v", c.count, r, got, want)
			}
		}
	}
}
