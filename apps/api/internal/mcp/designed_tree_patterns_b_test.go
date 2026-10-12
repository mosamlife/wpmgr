package mcp

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// The features and pricing patterns of a designed page, held to the golden
// without the stand-in TestDesignedTreeGolden allows for a pattern the twin
// does not compile: a case named for one of these patterns passes only when
// the twin's own compilers build it.

// designedCardGoldenNames are the golden cases whose names say they are about
// the features or the pricing pattern.
func designedCardGoldenNames(name string) bool {
	return strings.HasPrefix(name, "features-") || strings.HasPrefix(name, "pricing-")
}

// designedIconNamesIn are the icons the features patterns of an input name.
func designedIconNamesIn(t *testing.T, input []byte) []string {
	t.Helper()
	top, ok := jsonObjectOf(input)
	if !ok {
		t.Fatal("the golden input is not an object")
	}
	sections, _ := jsonArrayOf(top["outline"])
	var out []string
	for _, rawSection := range sections {
		section, _ := jsonObjectOf(rawSection)
		children, _ := jsonArrayOf(section["children"])
		for _, rawChild := range children {
			child, _ := jsonObjectOf(rawChild)
			if name, _ := jsonStringOf(child["pattern"]); name != "features" {
				continue
			}
			items, _ := jsonArrayOf(child["features"])
			for _, rawItem := range items {
				item, _ := jsonObjectOf(rawItem)
				if icon, ok := jsonStringOf(item["icon"]); ok {
					out = append(out, icon)
				}
			}
		}
	}
	return out
}

// TestDesignedCardPatternsGolden: the features-* and pricing-* cases are built
// by the twin's registered compilers alone and equal the agent's tree byte for
// byte; together they use every icon of the closed list.
func TestDesignedCardPatternsGolden(t *testing.T) {
	icons := map[string]bool{}
	var ran []string
	for _, c := range readDesignedGolden(t) {
		if !designedCardGoldenNames(c.Name) {
			continue
		}
		ran = append(ran, c.Name)
		t.Run(c.Name, func(t *testing.T) {
			input := []byte(c.Input)
			for _, kind := range designedPatternKinds(t, input) {
				if _, built := designedPatternBuilders[kind]; !built {
					t.Fatalf("the twin has no compiler registered for the %s pattern", kind)
				}
			}
			tree, ok := designedTreeWith(input, c.Facts.RequestID, c.Facts.ActiveBreakpoints, c.Facts.media(t), designedPatternBuilders)
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
			for _, name := range designedIconNamesIn(t, input) {
				icons[name] = true
			}
		})
	}
	if len(ran) == 0 {
		t.Fatal("no features or pricing case in the golden")
	}
	for _, name := range pageIcons {
		if !icons[name] {
			t.Errorf("no golden features case uses the icon %q, so its mapping is not held to the agent's", name)
		}
	}
	t.Logf("held to the golden without a stand-in: %s", strings.Join(ran, ", "))
}

// TestDesignedCardPatternsInEveryCase: every golden case that holds a features
// or a pricing pattern, whatever it is named for, is built with the twin's
// compilers for those two and the golden's own subtree for any other pattern,
// and equals the agent's tree byte for byte: the pages with the breakpoint sets
// and the escapes in the text included.
func TestDesignedCardPatternsInEveryCase(t *testing.T) {
	var ran []string
	for _, c := range readDesignedGolden(t) {
		input := []byte(c.Input)
		kinds := designedPatternKinds(t, input)
		if !slices.Contains(kinds, "features") && !slices.Contains(kinds, "pricing") {
			continue
		}
		ran = append(ran, c.Name)
		t.Run(c.Name, func(t *testing.T) {
			golden, ok := phpDecodeDocument(c.Tree, elementorTreeMaxDepth, true)
			if !ok {
				t.Fatal("the golden tree does not decode")
			}
			goldenTree, _ := golden.(phpList)
			patterns := map[string]designedPatternFunc{}
			for _, kind := range kinds {
				if kind != "features" && kind != "pricing" {
					patterns[kind] = goldenPatternStandIn(goldenTree)
					continue
				}
				build, built := designedPatternBuilders[kind]
				if !built {
					t.Fatalf("the twin has no compiler registered for the %s pattern", kind)
				}
				patterns[kind] = build
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
	// Positive control: the walk found the pages it is meant to hold the twin to.
	for _, prefix := range []string{"features-", "pricing-", "escapes-in-every-pattern", "responsive-probe-", "full-landing-page-"} {
		if !slices.ContainsFunc(ran, func(name string) bool { return strings.HasPrefix(name, prefix) }) {
			t.Errorf("no golden case named %q holds a features or pricing pattern", prefix)
		}
	}
}
