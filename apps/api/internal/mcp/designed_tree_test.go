package mcp

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strconv"
	"testing"
)

type designedGoldenMedia struct {
	ID          int64  `json:"id"`
	URL         string `json:"url"`
	Filename    string `json:"filename"`
	Mime        string `json:"mime"`
	Width       int64  `json:"width"`
	Height      int64  `json:"height"`
	ModifiedGMT string `json:"modified_gmt"`
}

// designedGoldenFacts is what the site and the request gave the compile.
type designedGoldenFacts struct {
	ElementorVersion  string                `json:"elementor_version"`
	Layout            string                `json:"layout"`
	ActiveBreakpoints []string              `json:"active_breakpoints"`
	Media             []designedGoldenMedia `json:"media"`
	RequestID         string                `json:"request_id"`
}

// designedGoldenCase is one golden page. BuilderFacts is the JSON text of the
// block the precheck answers beside its preview: the theme fact, the page
// frame and the breakpoints the tree was compiled for.
type designedGoldenCase struct {
	Name          string              `json:"name"`
	Input         string              `json:"input"`
	Facts         designedGoldenFacts `json:"facts"`
	BuilderFacts  json.RawMessage     `json:"builder_facts"`
	PreviewDigest string              `json:"preview_digest"`
	Tree          json.RawMessage     `json:"tree"`
}

func readDesignedGolden(t *testing.T) []designedGoldenCase {
	t.Helper()
	path := agentAbilityFixtures + "elementor-designed-containers.json"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var g struct {
		Cases []designedGoldenCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if len(g.Cases) == 0 {
		t.Fatalf("%s holds no cases", path)
	}
	return g.Cases
}

func (f designedGoldenFacts) media(t *testing.T) []pageMediaFact {
	t.Helper()
	out := make([]pageMediaFact, 0, len(f.Media))
	for _, m := range f.Media {
		fact := pageMediaFact{ID: m.ID, URL: m.URL, Filename: m.Filename, Mime: m.Mime, Width: m.Width, Height: m.Height, ModifiedGMT: m.ModifiedGMT}
		if !fact.usable() {
			t.Fatalf("the golden's image fact %d is not usable", m.ID)
		}
		out = append(out, fact)
	}
	return out
}

// designedPatternKinds are the pattern names an input's sections hold, in
// order of first appearance.
func designedPatternKinds(t *testing.T, input []byte) []string {
	t.Helper()
	top, ok := jsonObjectOf(input)
	if !ok {
		t.Fatal("the golden input is not an object")
	}
	sections, _ := jsonArrayOf(top["outline"])
	var kinds []string
	for _, rawSection := range sections {
		section, _ := jsonObjectOf(rawSection)
		children, _ := jsonArrayOf(section["children"])
		for _, rawChild := range children {
			child, _ := jsonObjectOf(rawChild)
			if kind, _ := jsonStringOf(child["type"]); kind != "pattern" {
				continue
			}
			name, _ := jsonStringOf(child["pattern"])
			if !slices.Contains(kinds, name) {
				kinds = append(kinds, name)
			}
		}
	}
	return kinds
}

var designedPatternPath = regexp.MustCompile(`^outline\[([0-9]+)\]\.children\[([0-9]+)\]$`)

// phpMember is the value of a key of a decoded PHP object.
func phpMember(o phpObject, key string) any {
	for _, p := range o {
		if p.key == key {
			return p.value
		}
	}
	return nil
}

// goldenPatternStandIn answers a pattern node with the golden's own subtree at
// the node's place, for a pattern the twin does not compile yet. Every outline
// child of a section is exactly one element of the section's container, so the
// place is the section's index and the child's.
func goldenPatternStandIn(golden phpList) designedPatternFunc {
	return func(_ *designedBuild, _ map[string]json.RawMessage, path, _ string) (phpObject, bool) {
		at := designedPatternPath.FindStringSubmatch(path)
		if at == nil {
			return nil, false
		}
		i, _ := strconv.Atoi(at[1])
		j, _ := strconv.Atoi(at[2])
		if i >= len(golden) {
			return nil, false
		}
		section, _ := golden[i].(phpObject)
		elements, _ := phpMember(section, "elements").(phpList)
		if j >= len(elements) {
			return nil, false
		}
		node, ok := elements[j].(phpObject)
		return node, ok
	}
}

// designedTreeDifference describes where two encodings part.
func designedTreeDifference(got, want []byte) string {
	n := min(len(got), len(want))
	i := 0
	for i < n && got[i] == want[i] {
		i++
	}
	from := max(i-100, 0)
	return "first difference at byte " + strconv.Itoa(i) + " (lengths " + strconv.Itoa(len(got)) + " and " + strconv.Itoa(len(want)) + ")\n got: ..." +
		string(got[from:min(i+120, len(got))]) + "\nwant: ..." + string(want[from:min(i+120, len(want))])
}

// TestDesignedTreeGolden: for every golden case the tree the control plane
// builds, with its own compiler for every pattern the case holds, is the
// agent's tree byte for byte as PHP encodes it, and the preview digest over
// those bytes is the one the agent answered. No case is left out and no
// pattern is answered by the golden's own subtree.
func TestDesignedTreeGolden(t *testing.T) {
	cases := readDesignedGolden(t)
	ran := 0
	for _, c := range cases {
		ran++
		t.Run(c.Name, func(t *testing.T) {
			input := []byte(c.Input)
			for _, kind := range designedPatternKinds(t, input) {
				if _, built := designedPatternBuilders[kind]; !built {
					t.Fatalf("the twin has no compiler for the %s pattern", kind)
				}
			}
			tree, ok := designedClassicTree(input, c.Facts.RequestID, c.Facts.ActiveBreakpoints, c.Facts.media(t))
			if !ok {
				t.Fatal("the control plane could not build the tree")
			}
			got, ok := phpEncodeBytes(tree)
			if !ok {
				t.Fatal("the tree did not encode")
			}
			want := mustCanonical(t, c.Tree)
			if !bytes.Equal(got, want) {
				t.Fatalf("the control plane's tree differs from the agent's golden\n%s", designedTreeDifference(got, want))
			}

			var page struct {
				Editor   string `json:"editor"`
				PostType string `json:"post_type"`
				Title    string `json:"title"`
			}
			if err := json.Unmarshal(input, &page); err != nil {
				t.Fatal(err)
			}
			prev, ok := phpJSONStringArray(page.Editor, elementorFormatClassic, c.Facts.ElementorVersion, page.PostType, "draft", page.Title, string(got))
			if !ok {
				t.Fatal("the preview did not encode")
			}
			if digest := sha256Hex(prev); digest != c.PreviewDigest {
				t.Errorf("preview digest %s, the agent answered %s", digest, c.PreviewDigest)
			}
		})
	}
	if ran == 0 || ran != len(cases) {
		t.Fatalf("ran %d of %d golden cases", ran, len(cases))
	}
	t.Logf("ran %d of %d golden cases", ran, len(cases))
}
