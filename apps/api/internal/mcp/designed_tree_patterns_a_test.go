package mcp

import (
	"slices"
	"strings"
	"testing"
)

// designedPatternsAAreas are the golden cases of the hero, cta, stats and logos
// areas, by case-name prefix, and the patterns each area owns.
var designedPatternsAAreas = []struct {
	prefix   string
	patterns []string
}{
	{"hero-", []string{"hero"}},
	{"cta-band", []string{"cta"}},
	{"stats-", []string{"stats"}},
	{"logos-", []string{"logos"}},
}

// TestDesignedPatternsABuiltNotStoodIn: the golden cases of these four areas
// are held to the twin's own tree. A pattern the twin does not register is
// answered, in TestDesignedTreeGolden, by the golden's subtree at its place, so
// the case would pass without the twin compiling anything; here a case of an
// area whose pattern has no compiler fails instead, and so does an area whose
// prefix is not in the gate.
func TestDesignedPatternsABuiltNotStoodIn(t *testing.T) {
	cases := readDesignedGolden(t)
	for _, area := range designedPatternsAAreas {
		if !slices.Contains(designedGoldenGate, area.prefix) {
			t.Errorf("the gate does not enable %q", area.prefix)
		}
		held := 0
		for _, c := range cases {
			if !strings.HasPrefix(c.Name, area.prefix) {
				continue
			}
			held++
			for _, kind := range designedPatternKinds(t, []byte(c.Input)) {
				if _, built := designedPatternBuilders[kind]; !built {
					t.Errorf("%s holds a %s pattern, which the twin does not compile", c.Name, kind)
				}
			}
		}
		if held == 0 {
			t.Errorf("no golden case starts with %q", area.prefix)
		}
		for _, name := range area.patterns {
			if _, built := designedPatternBuilders[name]; !built {
				t.Errorf("the pattern %q has no compiler", name)
			}
		}
	}
}
