package mcp

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The pieces of the features and pricing twin that the golden reaches only in
// part, pinned to what the agent's own classes answer: the row shares of a grid
// and a bento are ElementorPatternCards::rowShares for every count the grammar
// allows; the table of nodes is ElementorPatternCompiler::compile accepting or
// refusing each one, asked of the agent's classes.

// TestDesignedIconsAreTheGrammarsIcons: the map holds the closed list of icons
// and no other, each name mapped to an icon of the solid library.
func TestDesignedIconsAreTheGrammarsIcons(t *testing.T) {
	if len(designedIcons) != len(pageIcons) {
		t.Errorf("the map holds %d icons, the grammar's list %d", len(designedIcons), len(pageIcons))
	}
	for _, name := range pageIcons {
		icon, ok := designedIcon(name)
		if !ok {
			t.Errorf("the icon %q of the grammar's list has no mapping", name)
			continue
		}
		want := phpObject{{"value", designedIcons[name]}, {"library", "fa-solid"}}
		if !reflect.DeepEqual(icon, want) {
			t.Errorf("the icon %q is stored as %v", name, icon)
		}
		if !strings.HasPrefix(designedIcons[name], "fas fa-") {
			t.Errorf("the icon %q is not one of the solid library: %q", name, designedIcons[name])
		}
	}
	for name := range designedIcons {
		if !slices.Contains(pageIcons, name) {
			t.Errorf("the map holds %q, which the grammar's list does not", name)
		}
	}
	for _, name := range []string{"", "Bolt", "bolt ", "sparkles"} {
		if _, ok := designedIcon(name); ok {
			t.Errorf("%q was taken for an icon", name)
		}
	}
}

// TestDesignedRowShares: the share of each card in each row, 0 being a card
// alone in its row, for every count of features the grammar allows.
func TestDesignedRowShares(t *testing.T) {
	cases := []struct {
		variant string
		count   int
		want    [][]int
	}{
		{"bento", 3, [][]int{{60, 40}, {0}}},
		{"bento", 4, [][]int{{60, 40}, {40, 60}}},
		{"bento", 5, [][]int{{60, 40}, {40, 60}, {0}}},
		{"bento", 6, [][]int{{60, 40}, {40, 60}, {60, 40}}},
		{"bento", 7, [][]int{{60, 40}, {40, 60}, {60, 40}, {0}}},
		{"bento", 8, [][]int{{60, 40}, {40, 60}, {60, 40}, {40, 60}}},
		{"grid", 3, [][]int{{33, 33, 33}}},
		{"grid", 4, [][]int{{50, 50}, {50, 50}}},
		{"grid", 5, [][]int{{33, 33, 33}, {50, 50}}},
		{"grid", 6, [][]int{{33, 33, 33}, {33, 33, 33}}},
		{"grid", 7, [][]int{{33, 33, 33}, {50, 50}, {50, 50}}},
		{"grid", 8, [][]int{{33, 33, 33}, {33, 33, 33}, {50, 50}}},
	}
	for _, c := range cases {
		if got := designedRowShares(c.variant, c.count); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s of %d: %v, the agent's %v", c.variant, c.count, got, c.want)
		}
	}
}

// TestDesignedCardsSettingsHelpers: setting a key replaces the value of a key
// the settings have and adds one they have not after the last; a union keeps
// the value of a key the settings have and adds the others after the last. A
// node's own members and the node it was given are left as they were.
func TestDesignedCardsSettingsHelpers(t *testing.T) {
	node := phpObject{{"id", "a"}, {"settings", phpObject{{"x", "1"}, {"y", "2"}}}, {"elements", phpList{}}}
	enc := func(n phpObject) string {
		b, ok := phpEncodeBytes(n)
		if !ok {
			t.Fatal("the node did not encode")
		}
		return string(b)
	}
	before := enc(node)
	cases := []struct{ name, got, want string }{
		{"set a key it has", enc(designedCardsSet(node, "x", "9")), `{"id":"a","settings":{"x":"9","y":"2"},"elements":[]}`},
		{"set a key it has not", enc(designedCardsSet(node, "z", "9")), `{"id":"a","settings":{"x":"1","y":"2","z":"9"},"elements":[]}`},
		{"union with keys it has and has not", enc(designedCardsUnion(node, phpObject{{"y", "8"}, {"z", "9"}, {"w", "7"}})), `{"id":"a","settings":{"x":"1","y":"2","z":"9","w":"7"},"elements":[]}`},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s\n got: %s\nwant: %s", c.name, c.got, c.want)
		}
	}
	if enc(node) != before {
		t.Errorf("the node given was changed: %s", enc(node))
	}
	bare := phpObject{{"id", "a"}}
	if got := enc(designedCardsSet(bare, "x", "1")); got != `{"id":"a"}` {
		t.Errorf("a node with no settings was changed: %s", got)
	}
}

// Nodes of the table below: an icon box and a plan the agent builds, and the
// builders that put a member of them out of true.
const (
	cardsItem = `{"icon":"bolt","title":"Fast","text":"It is quick."}`
	cardsPlan = `{"name":"Plan","price":"10","period":"month","features":["One","Two"],"cta":{"text":"Choose","url":"/buy"},"highlighted":false}`
)

func cardsRepeat(n int, node string) string {
	return strings.TrimSuffix(strings.Repeat(node+",", n), ",")
}

func cardsFeatures(variant, items string) string {
	return `{"type":"pattern","pattern":"features","variant":` + variant + `,"features":` + items + `}`
}

func cardsPricing(variant, plans string) string {
	return `{"type":"pattern","pattern":"pricing","variant":` + variant + `,"plans":` + plans + `}`
}

// cardsFirst is a list of n of the node with its first member changed.
func cardsFirst(n int, node, old, repl string) string {
	return "[" + strings.Replace(node, old, repl, 1) + cardsMore(n-1, node) + "]"
}

func cardsMore(n int, node string) string {
	if n <= 0 {
		return ""
	}
	return "," + cardsRepeat(n, node)
}

// designedCardRows is a features or pricing node, and whether the agent's
// compile builds it.
var designedCardRows = []struct {
	name     string
	node     string
	accepted bool
}{
	{"an honest split of three", cardsFeatures(`"split"`, "["+cardsRepeat(3, cardsItem)+"]"), true},
	{"an honest grid of three", cardsFeatures(`"grid"`, "["+cardsRepeat(3, cardsItem)+"]"), true},
	{"an honest bento of three", cardsFeatures(`"bento"`, "["+cardsRepeat(3, cardsItem)+"]"), true},
	{"eight features, the most", cardsFeatures(`"grid"`, "["+cardsRepeat(8, cardsItem)+"]"), true},
	{"a feature with markup and brackets in its text", cardsFeatures(`"grid"`, cardsFirst(3, cardsItem, `"title":"Fast","text":"It is quick."`, `"title":"A <b> & [c]","text":"x & y <i> [d]"`)), true},
	{"an icon that is not on the list", cardsFeatures(`"grid"`, cardsFirst(3, cardsItem, `"bolt"`, `"sparkles"`)), false},
	{"an icon of another case", cardsFeatures(`"grid"`, cardsFirst(3, cardsItem, `"bolt"`, `"Bolt"`)), false},
	{"an icon that is not text", cardsFeatures(`"grid"`, cardsFirst(3, cardsItem, `"bolt"`, `5`)), false},
	{"a feature with no icon", cardsFeatures(`"grid"`, cardsFirst(3, cardsItem, `"icon":"bolt",`, ``)), false},
	{"a feature with an empty title", cardsFeatures(`"grid"`, cardsFirst(3, cardsItem, `"Fast"`, `""`)), false},
	{"a feature with no title", cardsFeatures(`"grid"`, cardsFirst(3, cardsItem, `"title":"Fast",`, ``)), false},
	{"a feature with an empty text", cardsFeatures(`"grid"`, cardsFirst(3, cardsItem, `"It is quick."`, `""`)), false},
	{"a feature with a title that is not text", cardsFeatures(`"grid"`, cardsFirst(3, cardsItem, `"Fast"`, `7`)), false},
	{"a feature that is not an object", cardsFeatures(`"grid"`, cardsFirst(3, cardsItem, cardsItem, `"bolt"`)), false},
	{"two features, one fewer than the least", cardsFeatures(`"grid"`, "["+cardsRepeat(2, cardsItem)+"]"), false},
	{"nine features, one more than the most", cardsFeatures(`"grid"`, "["+cardsRepeat(9, cardsItem)+"]"), false},
	{"a variant that is not one", cardsFeatures(`"carousel"`, "["+cardsRepeat(3, cardsItem)+"]"), false},
	{"a variant that is not text", cardsFeatures(`5`, "["+cardsRepeat(3, cardsItem)+"]"), false},
	{"no variant", `{"type":"pattern","pattern":"features","features":[` + cardsRepeat(3, cardsItem) + `]}`, false},
	{"features that are an object", cardsFeatures(`"grid"`, `{"a":`+cardsItem+`}`), false},

	{"an honest plan alone", cardsPricing(`"three-plans"`, "["+cardsPlan+"]"), true},
	{"an honest plan, highlighted", cardsPricing(`"three-plans"`, "["+strings.Replace(cardsPlan, `"highlighted":false`, `"highlighted":true`, 1)+"]"), true},
	{"four honest plans, the most", cardsPricing(`"three-plans"`, "["+cardsRepeat(4, cardsPlan)+"]"), true},
	{"a plan with no period", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `"period":"month",`, ``)), true},
	{"a plan with no highlight", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `,"highlighted":false`, ``)), true},
	{"a plan highlighted as null", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `"highlighted":false`, `"highlighted":null`)), true},
	{"eight lines in a plan, the most", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `["One","Two"]`, `["1","2","3","4","5","6","7","8"]`)), true},
	{"a plan with markup in its text", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `"name":"Plan","price":"10","period":"month","features":["One","Two"],"cta":{"text":"Choose"`, `"name":"A <b> & [c]","price":"$9 & up","period":"per <i>mo</i>","features":["a & b","[c]"],"cta":{"text":"Go <now> [x]"`)), true},
	{"a button whose link holds a query without an ampersand", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `"/buy"`, `"/buy?a=1"`)), true},
	{"a plan with no name", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `"name":"Plan",`, ``)), false},
	{"a plan with an empty name", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `"Plan"`, `""`)), false},
	{"a plan with no price", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `"price":"10",`, ``)), false},
	{"a plan with an empty price", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `"price":"10"`, `"price":""`)), false},
	{"a plan whose period is null", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `"month"`, `null`)), false},
	{"a plan whose period is empty", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `"month"`, `""`)), false},
	{"a plan whose period is not text", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `"month"`, `12`)), false},
	{"a plan with no features", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `["One","Two"]`, `[]`)), false},
	{"a plan with nine lines", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `["One","Two"]`, `["1","2","3","4","5","6","7","8","9"]`)), false},
	{"a plan with an empty line", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `["One","Two"]`, `["One",""]`)), false},
	{"a plan with a line that is not text", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `["One","Two"]`, `["One",2]`)), false},
	{"a plan whose features are an object", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `["One","Two"]`, `{"a":"One"}`)), false},
	{"a plan with no button", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `"cta":{"text":"Choose","url":"/buy"},`, ``)), false},
	{"a plan whose button is null", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `{"text":"Choose","url":"/buy"}`, `null`)), false},
	{"a button with an empty link", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `"/buy"`, `""`)), false},
	{"a button with no text", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `"text":"Choose",`, ``)), false},
	{"a button whose link holds an ampersand", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `"/buy"`, `"/buy?a=1&b=2"`)), false},
	{"a plan highlighted as text", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `false`, `"yes"`)), false},
	{"a plan highlighted as a number", cardsPricing(`"three-plans"`, cardsFirst(1, cardsPlan, `false`, `1`)), false},
	{"a plan that is not an object", cardsPricing(`"three-plans"`, `["Solo"]`), false},
	{"no plans", cardsPricing(`"three-plans"`, `[]`), false},
	{"five plans, one more than the most", cardsPricing(`"three-plans"`, "["+cardsRepeat(5, cardsPlan)+"]"), false},
	{"a variant that is not one", cardsPricing(`"monthly"`, "["+cardsPlan+"]"), false},
	{"plans that are an object", cardsPricing(`"three-plans"`, `{"a":`+cardsPlan+`}`), false},
}

// TestDesignedCardPatternsRefuseWhatTheAgentRefuses: the twin builds a page
// holding a features or pricing node exactly when the agent's compile builds
// it, so a node the agent refuses never has a tree to be compared with.
func TestDesignedCardPatternsRefuseWhatTheAgentRefuses(t *testing.T) {
	for _, row := range designedCardRows {
		page := designedPageInput(`{"type":"section","anchor":"cards","children":[` + row.node + `]}`)
		tree, ok := designedTreeWith(page, designedVectorRequestID, []string{"mobile", "tablet"}, nil, designedPatternBuilders)
		if ok != row.accepted {
			t.Errorf("%s: built %v, want %v", row.name, ok, row.accepted)
		}
		if ok && len(tree) != 1 {
			t.Errorf("%s: %d sections built for one", row.name, len(tree))
		}
	}
	accepted, refused := 0, 0
	for _, row := range designedCardRows {
		if row.accepted {
			accepted++
		} else {
			refused++
		}
	}
	if accepted == 0 || refused == 0 {
		t.Errorf("the table holds %d honest nodes and %d refused: it proves nothing without both", accepted, refused)
	}
}
