package mcp

import (
	"strconv"
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
// are held to the twin's own tree: a case of an area whose pattern has no
// compiler fails, and so does an area that no golden case starts with.
func TestDesignedPatternsABuiltNotStoodIn(t *testing.T) {
	cases := readDesignedGolden(t)
	for _, area := range designedPatternsAAreas {
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

// designedPatternsAMedia is the images the refusal table resolves, as the
// site's precheck would: a picture per id, of the mime given.
func designedPatternsAMedia(mime string, ids ...int64) []pageMediaFact {
	out := make([]pageMediaFact, 0, len(ids))
	for _, id := range ids {
		n := strconv.FormatInt(id, 10)
		out = append(out, pageMediaFact{ID: id, URL: "https://example.com/wp-content/uploads/img-" + n + ".jpg", Filename: "img-" + n + ".jpg", Mime: mime, Width: 1600, Height: 900})
	}
	return out
}

// TestDesignedPatternsARefuseWhatTheAgentRefuses: the twin builds no tree for a
// hero, call to action, stats or logos pattern the agent's compile refuses, and
// builds one for the honest pattern next to each refusal, so a refusal is the
// pattern's own and not the harness's.
func TestDesignedPatternsARefuseWhatTheAgentRefuses(t *testing.T) {
	section := func(pattern string) string {
		return `{"type":"section","anchor":"top","children":[` + pattern + `]}`
	}
	blocks := `[{"type":"heading","level":1,"text":"Hi"},{"type":"paragraph","text":"Lead"},{"type":"buttons","buttons":[{"text":"Go","url":"/go"}]}]`
	withPicture := `[{"type":"heading","level":1,"text":"Hi"},{"type":"image","attachment_id":42,"alt":"x"},{"type":"paragraph","text":"Lead"}]`
	hero := func(variant, children string) string {
		return `{"type":"pattern","pattern":"hero","variant":"` + variant + `","children":` + children + `}`
	}
	cta := func(variant, children string) string {
		return `{"type":"pattern","pattern":"cta","variant":"` + variant + `","children":` + children + `}`
	}
	stats := func(items ...string) string {
		return `{"type":"pattern","pattern":"stats","variant":"counters","stats":[` + strings.Join(items, ",") + `]}`
	}
	logos := func(items ...string) string {
		return `{"type":"pattern","pattern":"logos","variant":"row","logos":[` + strings.Join(items, ",") + `]}`
	}
	counter := `{"value":5,"label":"years"}`
	logo := func(id string) string { return `{"attachment_id":` + id + `,"alt":"A"}` }
	repeat := func(item string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = item
		}
		return out
	}
	three := designedPatternsAMedia("image/jpeg", 41, 42, 43)
	pictures := designedPatternsAMedia("image/jpeg", 42)

	cases := []struct {
		name     string
		section  string
		media    []pageMediaFact
		accepted bool
	}{
		{"the honest centred hero", section(hero("centered", blocks)), nil, true},
		{"the honest background hero", section(hero("background", blocks)), nil, true},
		{"the honest split hero with a picture", section(hero("split", withPicture)), pictures, true},
		{"the honest call to action", section(cta("band", blocks)), nil, true},
		{"the honest two counters", section(stats(counter, counter)), nil, true},
		{"the honest four counters", section(stats(repeat(counter, 4)...)), nil, true},
		{"a counter with a suffix of null", section(stats(`{"value":5,"suffix":null,"label":"years"}`, counter)), nil, true},
		{"the honest three logos", section(logos(logo("41"), logo("42"), logo("43"))), three, true},
		{"the honest ten logos", section(logos(repeat(logo("41"), 10)...)), three, true},

		{"a hero of the call to action's variant", section(hero("band", blocks)), nil, false},
		{"a call to action of the hero's variant", section(cta("centered", blocks)), nil, false},
		{"a hero with no variant", section(`{"type":"pattern","pattern":"hero","children":` + blocks + `}`), nil, false},
		{"a hero with a variant of null", section(`{"type":"pattern","pattern":"hero","variant":null,"children":` + blocks + `}`), nil, false},
		{"a hero with no blocks", section(hero("centered", `[]`)), nil, false},
		{"a hero whose blocks are not a list", section(hero("centered", `{}`)), nil, false},
		{"a hero holding a group", section(hero("centered", `[{"type":"group","children":[{"type":"paragraph","text":"x"}]}]`)), nil, false},
		{"a hero picture the site did not resolve", section(hero("split", withPicture)), nil, false},
		{"a call to action with no blocks", section(cta("band", `[]`)), nil, false},
		{"a call to action holding columns", section(cta("band", `[{"type":"columns","columns":[{"children":[{"type":"paragraph","text":"a"}]},{"children":[{"type":"paragraph","text":"b"}]}]}]`)), nil, false},

		{"one counter", section(stats(counter)), nil, false},
		{"five counters", section(stats(repeat(counter, 5)...)), nil, false},
		{"counters under another name", section(`{"type":"pattern","pattern":"stats","variant":"counters","logos":[` + counter + `,` + counter + `]}`), nil, false},
		{"counters of another pattern's variant", section(`{"type":"pattern","pattern":"stats","variant":"row","stats":[` + counter + `,` + counter + `]}`), nil, false},
		{"a negative counter", section(stats(`{"value":-1,"label":"x"}`, counter)), nil, false},
		{"a counter above the largest", section(stats(`{"value":1000000000,"label":"x"}`, counter)), nil, false},
		{"a counter that is text", section(stats(`{"value":"5","label":"x"}`, counter)), nil, false},
		{"a counter with a fraction", section(stats(`{"value":1.5,"label":"x"}`, counter)), nil, false},
		{"a counter with no label", section(stats(`{"value":5}`, counter)), nil, false},
		{"a counter with an empty label", section(stats(`{"value":5,"label":""}`, counter)), nil, false},
		{"a counter with an empty suffix", section(stats(`{"value":5,"suffix":"","label":"x"}`, counter)), nil, false},
		{"a counter with a suffix that is not text", section(stats(`{"value":5,"suffix":5,"label":"x"}`, counter)), nil, false},
		{"a counter that is not an object", section(stats(`5`, counter)), nil, false},

		{"two logos", section(logos(logo("41"), logo("42"))), three, false},
		{"eleven logos", section(logos(repeat(logo("41"), 11)...)), three, false},
		{"a logo the site did not resolve", section(logos(logo("41"), logo("42"), logo("44"))), three, false},
		{"a logo that is not an image", section(logos(logo("41"), logo("42"), logo("43"))), designedPatternsAMedia("application/pdf", 41, 42, 43), false},
		{"a logo with attachment 0", section(logos(logo("41"), logo("42"), logo("0"))), three, false},
		{"a logo with a negative attachment", section(logos(logo("41"), logo("42"), logo("-3"))), three, false},
		{"a logo named by text", section(logos(logo("41"), logo("42"), logo(`"43"`))), three, false},
		{"a logo with no attachment", section(logos(logo("41"), logo("42"), `{"alt":"A"}`)), three, false},
	}
	for _, c := range cases {
		tree, ok := designedTreeWith(designedPageInput(c.section), designedVectorRequestID, []string{"mobile", "tablet"}, c.media, designedPatternBuilders)
		if ok != c.accepted {
			t.Errorf("%s: built %v, want %v", c.name, ok, c.accepted)
		}
		if ok && len(tree) != 1 {
			t.Errorf("%s: %d sections built for one", c.name, len(tree))
		}
	}
}

// TestDesignedPatternsASettingAndCentring: a container's setting is written as
// PHP assigns an array key, and a row of buttons is centred only when it holds
// nothing but buttons and no justification of its own.
func TestDesignedPatternsASettingAndCentring(t *testing.T) {
	p := designedPatternsA{}
	widget := func(kind string) phpObject {
		return phpObject{{"id", kind}, {"elType", "widget"}, {"settings", phpObject{{"x", "1"}}}, {"elements", phpList{}}, {"widgetType", kind}}
	}
	container := func(settings phpObject, children ...any) phpObject {
		return phpObject{{"id", "row"}, {"elType", "container"}, {"settings", settings}, {"elements", phpList(children)}, {"isInner", true}}
	}
	encode := func(v any) string {
		out, ok := phpEncodeBytes(v)
		if !ok {
			t.Fatal("did not encode")
		}
		return string(out)
	}
	rowSettings := func(extra ...phpPair) phpObject {
		return append(phpObject{{"flex_direction", "row"}}, extra...)
	}

	cases := []struct {
		name string
		in   phpObject
		want bool
	}{
		{"a row of buttons", container(rowSettings(), widget("button"), widget("button")), true},
		{"a row with a justification of its own", container(rowSettings(phpPair{"flex_justify_content", "flex-end"}), widget("button")), false},
		{"a row holding a heading", container(rowSettings(), widget("button"), widget("heading")), false},
		{"a row holding a container", container(rowSettings(), widget("button"), container(rowSettings(), widget("button"))), false},
		{"a row with no children", container(rowSettings()), false},
		{"a widget", widget("button"), false},
	}
	for _, c := range cases {
		before := encode(c.in)
		got := encode(p.centred(phpList{c.in})[0])
		if c.want {
			wantJSON := strings.Replace(before, `"flex_direction":"row"}`, `"flex_direction":"row","flex_justify_content":"center"}`, 1)
			if got != wantJSON || got == before {
				t.Errorf("%s\n got: %s\nwant: %s", c.name, got, wantJSON)
			}
		} else if got != before {
			t.Errorf("%s was centred\n got: %s\nwas: %s", c.name, got, before)
		}
		if encode(c.in) != before {
			t.Errorf("%s: centring changed the container it was given", c.name)
		}
	}

	// A label is written after the last key, and over an earlier one in its place.
	fresh := container(phpObject{{"a", "1"}, {"b", "2"}})
	if got, want := encode(p.labelled(fresh, "L")), `{"id":"row","elType":"container","settings":{"a":"1","b":"2","_title":"L"},"elements":[],"isInner":true}`; got != want {
		t.Errorf("a label after the last key\n got: %s\nwant: %s", got, want)
	}
	again := container(phpObject{{"_title", "old"}, {"b", "2"}})
	if got, want := encode(p.labelled(again, "L")), `{"id":"row","elType":"container","settings":{"_title":"L","b":"2"},"elements":[],"isInner":true}`; got != want {
		t.Errorf("a label over an earlier one\n got: %s\nwant: %s", got, want)
	}
	if encode(again) != `{"id":"row","elType":"container","settings":{"_title":"old","b":"2"},"elements":[],"isInner":true}` {
		t.Error("labelling changed the container it was given")
	}
}
