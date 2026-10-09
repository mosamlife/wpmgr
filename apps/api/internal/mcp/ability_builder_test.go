package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

// The agent's golden Elementor classic trees: each case's "input" is an
// outline the agent builds (apps/agent/tests/Builders); never hand-edited.
var elementorGoldenFixtures = []string{
	agentAbilityFixtures + "elementor-classic-containers.json",
	agentAbilityFixtures + "elementor-classic-sections.json",
}

var elementorEnabled = []byte(`{"max_nodes":400,"builders_enabled":["elementor"]}`)

func elementorInput(outline string) string {
	return `{"post_type":"page","editor":"builder:elementor","title":"T","outline":` + outline + `}`
}

// TestValidatePageCreateInput_BuilderEditor: builder:elementor and its
// format pass the grammar; a format with another editor, a value outside the
// published enum and an editor outside the schema's enum do not.
func TestValidatePageCreateInput_BuilderEditor(t *testing.T) {
	layout := `[{"type":"group","children":[{"type":"spacer","size":"small"},{"type":"image","attachment_id":5,"alt":"","caption":"c"}]},` +
		`{"type":"columns","columns":[{"children":[{"type":"paragraph","text":"a"}]},{"children":[{"type":"buttons","buttons":[{"text":"Go","url":"/go"}]}]}]}]`
	accepts := map[string]string{
		elementorInput(`[{"type":"paragraph","text":"x"}]`): pageElementorFormatDefault,
		`{"post_type":"post","editor":"builder:elementor","title":"T","outline":[{"type":"paragraph","text":"x"}],"elementor_format":"site_default"}`: "site_default",
		`{"post_type":"page","editor":"builder:elementor","title":"T","outline":[{"type":"paragraph","text":"x"}],"elementor_format":"classic"}`:      "classic",
		`{"post_type":"page","editor":"builder:elementor","title":"T","outline":[{"type":"paragraph","text":"x"}],"elementor_format":"atomic"}`:       "atomic",
		// The classic editor's layout refusal is not a builder's.
		elementorInput(layout): pageElementorFormatDefault,
	}
	for in, format := range accepts {
		f, code := validatePageCreateInput([]byte(in))
		if code != "" || f.builder != pageBuilderElementor || f.elementorFormat != format || f.editor != pageEditorBuilderElementor {
			t.Errorf("%s: code %q, facts %+v", in, code, f)
		}
	}
	for _, in := range []string{
		`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"paragraph","text":"x"}],"elementor_format":"classic"}`,
		`{"post_type":"page","editor":"wordpress_classic","title":"T","outline":[{"type":"paragraph","text":"x"}],"elementor_format":"site_default"}`,
		`{"post_type":"page","editor":"builder:elementor","title":"T","outline":[{"type":"paragraph","text":"x"}],"elementor_format":"Classic"}`,
		`{"post_type":"page","editor":"builder:elementor","title":"T","outline":[{"type":"paragraph","text":"x"}],"elementor_format":""}`,
		`{"post_type":"page","editor":"builder:elementor","title":"T","outline":[{"type":"paragraph","text":"x"}],"elementor_format":null}`,
		`{"post_type":"page","editor":"builder:elementor","title":"T","outline":[{"type":"paragraph","text":"x"}],"elementor_format":["classic"]}`,
		`{"post_type":"page","editor":"builder:elementor","title":"T","outline":[{"type":"paragraph","text":"x"}],"Elementor_format":"classic"}`,
		`{"post_type":"page","editor":"builder:beaver","title":"T","outline":[{"type":"paragraph","text":"x"}]}`,
		`{"post_type":"page","editor":"builder:Elementor","title":"T","outline":[{"type":"paragraph","text":"x"}]}`,
		`{"post_type":"page","editor":"builder:","title":"T","outline":[{"type":"paragraph","text":"x"}]}`,
		`{"post_type":"page","editor":"elementor","title":"T","outline":[{"type":"paragraph","text":"x"}]}`,
		`{"post_type":"page","editor":"builder:elementor ","title":"T","outline":[{"type":"paragraph","text":"x"}]}`,
		// The grammar is the same for a builder: a structural problem is
		// still the grammar's code.
		elementorInput(`[{"type":"html","text":"x"}]`),
	} {
		if _, code := validatePageCreateInput([]byte(in)); code != pageCreateBadInput {
			t.Errorf("%s: code %q, want bad_input", in, code)
		}
	}
	if _, code := validatePageCreateInput([]byte(elementorInput(`[{"type":"columns","columns":[{"children":[{"type":"separator"}]}]}]`))); code != pageCreateLayoutInvalid {
		t.Errorf("one column under Elementor: code %q, want layout_invalid", code)
	}
	f, _ := validatePageCreateInput([]byte(textOnlyPage))
	if f.builder != "" || f.elementorFormat != "" {
		t.Fatalf("a WordPress editor carries builder facts: %+v", f)
	}
}

func TestPageBuildersEnabled(t *testing.T) {
	well := map[string][]string{
		``:                                   {},
		`null`:                               {},
		`{}`:                                 {},
		`{"max_nodes":400}`:                  {},
		`{"builders_enabled":null}`:          {},
		`{"builders_enabled":[]}`:            {},
		`{"builders_enabled":["elementor"]}`: {"elementor"},
		`{"builders_enabled":["beaver","elementor"]}`: {"beaver", "elementor"},
	}
	for in, want := range well {
		got, ok := pageBuildersEnabled([]byte(in))
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %v %v, want %v", in, got, ok, want)
		}
	}
	many := make([]string, 33)
	for i := range many {
		many[i] = fmt.Sprintf(`"b%d"`, i)
	}
	for _, in := range []string{
		`[]`, `"elementor"`, `7`,
		`{"builders_enabled":"elementor"}`,
		`{"builders_enabled":{"0":"elementor"}}`,
		`{"builders_enabled":["Elementor"]}`,
		`{"builders_enabled":["elementor","elementor"]}`,
		`{"builders_enabled":[null]}`,
		`{"builders_enabled":[1]}`,
		`{"builders_enabled":[""]}`,
		`{"builders_enabled":["builder:elementor"]}`,
		`{"builders_enabled":["` + strings.Repeat("a", 33) + `"]}`,
		`{"builders_enabled":[` + strings.Join(many, ",") + `]}`,
	} {
		if _, ok := pageBuildersEnabled([]byte(in)); ok {
			t.Errorf("%s: read as well formed", in)
		}
	}
	for in, want := range map[string]bool{
		`{"builders_enabled":["elementor"]}`:          true,
		`{"builders_enabled":["beaver","elementor"]}`: true,
		`{}`:                              false,
		`{"builders_enabled":["beaver"]}`: false,
		`{"builders_enabled":["elementor","Elementor"]}`: false,
		`{"builders_enabled":["elementor",7]}`:           false,
	} {
		if got := pageBuilderEnabled([]byte(in), pageBuilderElementor); got != want {
			t.Errorf("%s: enabled %v, want %v", in, got, want)
		}
	}
}

// TestElementorNodeRules: each rule refuses with the agent's code, the node
// in the agent's path syntax, its field and what Elementor builds there; the
// honest neighbours of each rule pass.
func TestElementorNodeRules(t *testing.T) {
	type want struct {
		code, node, field string
		allowed           []string
	}
	none := want{}
	cases := map[string]want{
		`[{"type":"buttons","buttons":[{"text":"A","url":"/a"},{"text":"B","url":"/b","style":"outline"}]}]`:                                                                            {pageNodeNotSupported, "outline[0].buttons[1]", "style", elementorButtonStyles},
		`[{"type":"buttons","buttons":[{"text":"A","url":"/a","style":"fill"}]}]`:                                                                                                       none,
		`[{"type":"image","attachment_id":5,"alt":"","align":"wide"}]`:                                                                                                                  {pageNodeNotSupported, "outline[0]", "align", elementorImageAligns},
		`[{"type":"image","attachment_id":5,"alt":"","align":"full"}]`:                                                                                                                  {pageNodeNotSupported, "outline[0]", "align", elementorImageAligns},
		`[{"type":"image","attachment_id":5,"alt":"","align":"center"}]`:                                                                                                                none,
		`[{"type":"image","attachment_id":5,"alt":"","align":"none"}]`:                                                                                                                  none,
		`[{"type":"paragraph","text":"x"},{"type":"group","children":[{"type":"image","attachment_id":5,"alt":"","align":"full"}]}]`:                                                    {pageNodeNotSupported, "outline[1].children[0]", "align", elementorImageAligns},
		`[{"type":"columns","columns":[{"children":[{"type":"separator"}]},{"children":[{"type":"buttons","buttons":[{"text":"B","url":"/b","style":"outline"}]}]}]}]`:                  {pageNodeNotSupported, "outline[0].columns[1].children[0].buttons[0]", "style", elementorButtonStyles},
		`[{"type":"group","children":[{"type":"columns","columns":[{"children":[{"type":"separator"}]},{"children":[{"type":"paragraph","text":"https://example.com/watch?v=1"}]}]}]}]`: {pageCreateContentInvalid, "outline[0].children[0].columns[1].children[0]", "text", nil},
		`[{"type":"paragraph","text":"https://example.com/watch?v=1"}]`:                                                                                                                 {pageCreateContentInvalid, "outline[0]", "text", nil},
		`[{"type":"paragraph","text":" HTTP://EXAMPLE.COM/x "}]`:                                                                                                                        {pageCreateContentInvalid, "outline[0]", "text", nil},
		`[{"type":"paragraph","text":"Watch https://example.com/watch"}]`:                                                                                                               none,
		`[{"type":"paragraph","text":"https://example.com/watch and more"}]`:                                                                                                            none,
		`[{"type":"paragraph","text":"ftp://example.com/file"}]`:                                                                                                                        none,
		`[{"type":"paragraph","text":"https://"}]`:                                                                                                                                      none,
		`[{"type":"quote","paragraphs":["Said so.","http://example.com"],"citation":"https://example.com"}]`:                                                                            {pageCreateContentInvalid, "outline[0]", "paragraphs[1]", nil},
		`[{"type":"buttons","buttons":[{"text":"A","url":"/a?b=1&c=2"}]}]`:                                                                                                              {pageCreateLinkInvalid, "outline[0].buttons[0]", "url", nil},
		// The first problem in document order wins, and a button's style is
		// read before its link.
		`[{"type":"buttons","buttons":[{"text":"A","url":"/a&b","style":"outline"}]},{"type":"paragraph","text":"https://example.com"}]`: {pageNodeNotSupported, "outline[0].buttons[0]", "style", elementorButtonStyles},
		`[{"type":"paragraph","text":"https://example.com"},{"type":"buttons","buttons":[{"text":"A","url":"/a","style":"outline"}]}]`:   {pageCreateContentInvalid, "outline[0]", "text", nil},
	}
	for outline, w := range cases {
		in := []byte(elementorInput(outline))
		f, code := validatePageCreateInput(in)
		if code != "" {
			t.Fatalf("%s: the grammar refused %q; the case must be valid outline", outline, code)
		}
		p := elementorOutlineProblem(in)
		if w.code == "" {
			if p != nil {
				t.Errorf("%s: refused %+v", outline, *p)
			}
			if r := pageCreateBuilderRefusal(f, elementorEnabled, in); r != nil {
				t.Errorf("%s: the run path refuses it", outline)
			}
			continue
		}
		if p == nil || p.code != w.code || p.node != w.node || p.field != w.field || !reflect.DeepEqual(p.allowed, w.allowed) || p.hint == "" {
			t.Errorf("%s: got %+v, want %+v", outline, p, w)
		}
	}
}

func readElementorGoldenOutlines(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, path := range elementorGoldenFixtures {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		// "input" is the outline itself, a JSON array.
		var doc struct {
			Cases []struct {
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"cases"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if len(doc.Cases) == 0 {
			t.Fatalf("%s has no cases", path)
		}
		for _, c := range doc.Cases {
			out[path+": "+c.Name] = string(c.Input)
		}
	}
	return out
}

// TestElementorGoldenOutlinesAreAccepted: every outline the agent's golden
// Elementor trees are built from passes the control plane's grammar and
// Elementor's node rules, so nothing the agent builds is refused here.
func TestElementorGoldenOutlinesAreAccepted(t *testing.T) {
	outlines := readElementorGoldenOutlines(t)
	kinds := map[string]bool{}
	for name, outline := range outlines {
		in := []byte(elementorInput(outline))
		f, code := validatePageCreateInput(in)
		if code != "" {
			t.Errorf("%s: the grammar refused %q", name, code)
			continue
		}
		if r := pageCreateBuilderRefusal(f, elementorEnabled, in); r != nil {
			t.Errorf("%s: refused by the builder rules: %v", name, r.err)
		}
		for _, k := range regexp.MustCompile(`"type":\s*"([a-z]+)"`).FindAllStringSubmatch(outline, -1) {
			kinds[k[1]] = true
		}
	}
	// A fixture that read as empty, or lost a kind, proves nothing.
	for _, k := range []string{"heading", "paragraph", "list", "quote", "buttons", "image", "spacer", "separator", "group", "columns"} {
		if !kinds[k] {
			t.Errorf("the golden outlines hold no %s node (kinds seen: %v)", k, kinds)
		}
	}
}

// elementorRefusesInLayoutCases are the shared layout cases the block editor
// accepts and Elementor's node rules refuse, with the refusal.
var elementorRefusesInLayoutCases = map[string][3]string{
	"buttons-three-mixed":                 {pageCreateLinkInvalid, "outline[0].buttons[0]", "url"},
	"link-port-query-fragment":            {pageCreateLinkInvalid, "outline[0].buttons[0]", "url"},
	"link-ampersand-not-a-reference":      {pageCreateLinkInvalid, "outline[0].buttons[0]", "url"},
	"link-path-ampersand-not-a-reference": {pageCreateLinkInvalid, "outline[0].buttons[0]", "url"},
}

// TestPageCreateLayoutCasesUnderElementor replays the shared outline case
// table with editor builder:elementor: the grammar answers every structural
// case as for the block editor, and the only accepted cases Elementor's
// rules refuse are the ones listed above.
func TestPageCreateLayoutCasesUnderElementor(t *testing.T) {
	replayed, refused := 0, map[string]bool{}
	for _, c := range readLayoutCases(t) {
		var top map[string]json.RawMessage
		if json.Unmarshal([]byte(c.Input), &top) != nil || string(top["editor"]) != `"wordpress_blocks"` {
			continue
		}
		top["editor"] = json.RawMessage(`"builder:elementor"`)
		in, err := json.Marshal(top)
		if err != nil {
			t.Fatal(err)
		}
		replayed++
		f, code := validatePageCreateInput(in)
		if c.Go == "refuse" {
			want := c.Agent
			if want == "create_content_invalid" {
				want = pageCreateBadInput
			}
			if code != want {
				t.Errorf("%s: code %q under Elementor, want %q", c.Name, code, want)
			}
			continue
		}
		if code != "" {
			t.Errorf("%s: refused %q under Elementor", c.Name, code)
			continue
		}
		p := elementorOutlineProblem(in)
		w, listed := elementorRefusesInLayoutCases[c.Name]
		switch {
		case p == nil && listed:
			t.Errorf("%s: accepted under Elementor, want %v", c.Name, w)
		case p != nil && !listed:
			t.Errorf("%s: refused under Elementor: %+v", c.Name, *p)
		case p != nil && (p.code != w[0] || p.node != w[1] || p.field != w[2]):
			t.Errorf("%s: %+v, want %v", c.Name, *p, w)
		case p != nil:
			refused[c.Name] = true
		}
		if f.builder != pageBuilderElementor {
			t.Errorf("%s: facts %+v", c.Name, f)
		}
	}
	if replayed < 50 {
		t.Fatalf("replayed %d block-editor cases; the table reads as nearly empty", replayed)
	}
	for name := range elementorRefusesInLayoutCases {
		if !refused[name] {
			t.Errorf("%s: listed as refused under Elementor but not seen", name)
		}
	}
}

func TestPageCreateAgentFloor_Builder(t *testing.T) {
	for _, in := range []string{
		elementorInput(`[{"type":"paragraph","text":"x"}]`),
		elementorInput(`[{"type":"columns","columns":[{"children":[{"type":"separator"}]},{"children":[{"type":"separator"}]}]}]`),
		`{"post_type":"page","editor":"builder:beaver","title":"T","outline":[{"type":"paragraph","text":"x"}]}`,
	} {
		if !pageCreateUsesBuilder([]byte(in)) || PageCreateAgentFloor([]byte(in)) != agentcmd.MinAgentVersionForBuilderAdapters {
			t.Errorf("%s: not held to the builder floor", in)
		}
	}
	if PageCreateAgentFloor([]byte(textOnlyPage)) != agentcmd.MinAgentVersionForPageCreate ||
		PageCreateAgentFloor([]byte(layoutPage)) != agentcmd.MinAgentVersionForPageLayout {
		t.Fatal("a WordPress editor's floor moved")
	}
	if wpversion.Compare(agentcmd.MinAgentVersionForBuilderAdapters, agentcmd.MinAgentVersionForPageLayout) <= 0 {
		t.Fatalf("the builder floor %s is not above the layout floor %s", agentcmd.MinAgentVersionForBuilderAdapters, agentcmd.MinAgentVersionForPageLayout)
	}
}

// ---------------------------------------------------------------------------
// The run path, through the real transport
// ---------------------------------------------------------------------------

func TestPageCreateRun_BuilderNeedsTheEntryToEnableIt(t *testing.T) {
	in := elementorInput(`[{"type":"paragraph","text":"x"}]`)
	for _, limits := range []string{`{}`, `{"builders_enabled":["beaver"]}`, `{"builders_enabled":"elementor"}`, `{"builders_enabled":["elementor","elementor"]}`} {
		agent := &refusingPrecheckAgent{code: "create_content_invalid"}
		r, site := pageCreateRunRouterLimits(t, agentcmd.MinAgentVersionForBuilderAdapters, agent, []byte(limits))
		ref := runPageCreate(t, r, site, in)
		if ref.code != codeInvalidToolArguments || ref.data["code"] != pageBuilderNotEnabled || ref.data["hint"] != hintBuilderNotEnabled {
			t.Errorf("limits %s: %s", limits, ref.raw)
		}
		if len(agent.calls) != 0 {
			t.Errorf("limits %s: the site was asked", limits)
		}
	}
	// Enabled, at the floor: the precheck is sent.
	agent := &refusingPrecheckAgent{code: "builder_not_available"}
	r, site := pageCreateRunRouterLimits(t, agentcmd.MinAgentVersionForBuilderAdapters, agent, elementorEnabled)
	ref := runPageCreate(t, r, site, in)
	if len(agent.calls) != 1 || agent.calls[0].Mode != agentcmd.AbilityRunModePrecheck {
		t.Fatalf("enabled builder input: %d precheck calls, want 1 (%s)", len(agent.calls), ref.raw)
	}
	if ref.data["code"] != pageBuilderNotAvailable || ref.data["hint"] != hintBuilderNotAvailable || strings.Contains(ref.raw, plantedInstruction) {
		t.Fatalf("the agent's builder_not_available: %s", ref.raw)
	}
}

func TestPageCreateRun_ElementorNodeRuleRefusedBeforeTheSite(t *testing.T) {
	agent := &refusingPrecheckAgent{code: "create_content_invalid"}
	r, site := pageCreateRunRouterLimits(t, agentcmd.MinAgentVersionForBuilderAdapters, agent, elementorEnabled)
	ref := runPageCreate(t, r, site, elementorInput(`[{"type":"heading","level":2,"text":"H"},{"type":"buttons","buttons":[{"text":"Go","url":"/go","style":"outline"}]}]`))
	allowed, _ := ref.data["allowed"].([]any)
	if ref.code != codeInvalidToolArguments || ref.data["code"] != pageNodeNotSupported || ref.data["node"] != "outline[1].buttons[0]" ||
		ref.data["field"] != "style" || len(allowed) != 1 || allowed[0] != "fill" || ref.data["hint"] != hintNodeNotSupportedByBuilder {
		t.Fatalf("outline button under Elementor: %s", ref.raw)
	}
	if len(agent.calls) != 0 {
		t.Fatal("the site was asked for an outline Elementor cannot build")
	}
}

// A builder input on an agent below the builder floor is refused before the
// site is asked, even where the same outline runs in the block editor.
func TestPageCreateRun_BuilderFloorIsPerInput(t *testing.T) {
	below := agentcmd.MinAgentVersionForPageLayout
	agent := &refusingPrecheckAgent{code: "create_content_invalid"}
	r, site := pageCreateRunRouterLimits(t, below, agent, elementorEnabled)
	ref := runPageCreate(t, r, site, elementorInput(`[{"type":"paragraph","text":"x"}]`))
	if ref.code != codeSiteAgentOutdated || ref.data["min_agent_version"] != agentcmd.MinAgentVersionForBuilderAdapters ||
		!strings.Contains(ref.msg, agentcmd.MinAgentVersionForBuilderAdapters) {
		t.Fatalf("builder input on %s: %s", below, ref.raw)
	}
	if len(agent.calls) != 0 {
		t.Fatalf("the site was asked %d times for a page it cannot build", len(agent.calls))
	}
	_ = runPageCreate(t, r, site, layoutPage)
	if len(agent.calls) != 1 {
		t.Fatalf("a block-editor layout on %s: %d precheck calls, want 1", below, len(agent.calls))
	}
}

// ---------------------------------------------------------------------------
// The floor and the agent tree
// ---------------------------------------------------------------------------

// lastAgentReleaseWithoutBuilderAdapters is the newest released agent that
// does not build pages with a page builder. When another release lands in
// this tree before the one that ships the builder path, raise this to that
// release's number in the same commit that brings it in.
const lastAgentReleaseWithoutBuilderAdapters = "0.61.160"

// builderAdapterMarkers is code the in-tree agent carries when it builds
// pages with a page builder. Each pattern matches code, not a comment.
var builderAdapterMarkers = []struct {
	file    string
	what    string
	pattern *regexp.Regexp
}{
	{
		file:    "includes/abilities/builders/class-builder-registry.php",
		what:    "the registry compiles the Elementor adapter in",
		pattern: regexp.MustCompile(`'elementor'\s*=>\s*ElementorAdapter::class`),
	},
	{
		file:    "includes/commands/class-ability-run-command.php",
		what:    "the command resolves a builder editor through the registry",
		pattern: regexp.MustCompile(`\$resolved\s*=\s*BuilderRegistry::resolve\(\s*\$spec\['editor'\]`),
	},
}

// TestMinAgentVersionForBuilderAdapters_NotAheadOfShippingAgent holds the
// builder floor to the agent this tree ships: once the in-tree agent is newer
// than the last release without the builder path, it is that release or a
// later one, so it must be at or above the floor. An agent that ships the
// builder path below the floor would have every builder input refused as
// agent_outdated.
func TestMinAgentVersionForBuilderAdapters_NotAheadOfShippingAgent(t *testing.T) {
	floor := agentcmd.MinAgentVersionForBuilderAdapters
	if wpversion.Compare(floor, lastAgentReleaseWithoutBuilderAdapters) <= 0 {
		t.Fatalf("MinAgentVersionForBuilderAdapters %q is not above %q, the last release without the builder path",
			floor, lastAgentReleaseWithoutBuilderAdapters)
	}
	for _, m := range builderAdapterMarkers {
		if !m.pattern.Match(agentRepoFile(t, m.file)) {
			t.Errorf("apps/agent/%s: %s is missing (pattern %q); the floor names a builder path this tree does not carry",
				m.file, m.what, m.pattern)
		}
	}
	shipping := shippingAgentVersion(t)
	if wpversion.Compare(shipping, lastAgentReleaseWithoutBuilderAdapters) > 0 && wpversion.Compare(shipping, floor) < 0 {
		t.Errorf("apps/agent ships %[1]q with the builder path, newer than %[2]q, but below MinAgentVersionForBuilderAdapters %[3]q: "+
			"every site on %[1]s would have builder inputs refused as agent_outdated. If %[1]s is the release that ships the "+
			"builder path, set MinAgentVersionForBuilderAdapters and its pin to %[1]s together. "+
			"If %[1]s shipped without it, raise lastAgentReleaseWithoutBuilderAdapters to %[1]s.",
			shipping, lastAgentReleaseWithoutBuilderAdapters, floor)
	}
}
