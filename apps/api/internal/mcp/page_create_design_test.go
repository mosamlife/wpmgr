package mcp

import (
	"encoding/json"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The design object of outline v3 (elementor.md 2.3): the control plane's
// checks run before any agent call. The tests reach the validators with the
// raw JSON an input carries, so a wrong JSON type is as real as a wrong value.

// designColourRolesWant is the spec's list of colour roles, written out here
// so a change to the validator's list is a visible diff in this file.
var designColourRolesWant = []string{"ground", "surface", "ink", "muted", "accent", "on_accent"}

// designRatiosWant is the spec's list of type-scale ratio names, smallest
// first, written out here for the same reason. The input holds a name, never
// the number it stands for.
var designRatiosWant = []string{"major_second", "minor_third", "major_third", "perfect_fourth"}

// ratioEnumsIn collects the enum of every member named ratio that has one,
// at any depth of a decoded JSON schema.
func ratioEnumsIn(v any, found *[][]string) {
	switch x := v.(type) {
	case map[string]any:
		for key, child := range x {
			if member, ok := child.(map[string]any); ok && key == "ratio" {
				if enum, ok := member["enum"].([]any); ok {
					var names []string
					for _, e := range enum {
						s, _ := e.(string)
						names = append(names, s)
					}
					*found = append(*found, names)
				}
			}
			ratioEnumsIn(child, found)
		}
	case []any:
		for _, child := range x {
			ratioEnumsIn(child, found)
		}
	}
}

// TestDesignRatioNamesAreTheSchemasEnum: the names the validator takes are
// the names both input schemas, which describe serves, give for the ratio. A
// schema that lists another set would send the AI to a value the control
// plane refuses, or the reverse.
func TestDesignRatioNamesAreTheSchemasEnum(t *testing.T) {
	want := strings.Join(designRatiosWant, ",")
	if got := strings.Join(designRatios, ","); got != want {
		t.Errorf("the validator takes %s, the spec lists %s", got, want)
	}
	for name, schema := range map[string][]byte{"page-create": pageCreateInputSchema, "page-edit": pageEditInputSchema} {
		var doc any
		if err := json.Unmarshal(schema, &doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var found [][]string
		ratioEnumsIn(doc, &found)
		if len(found) == 0 {
			t.Errorf("%s: the schema gives no enum for a ratio, so this test compared nothing", name)
		}
		for _, enum := range found {
			if got := strings.Join(enum, ","); got != want {
				t.Errorf("%s: the schema lists the ratios %s, want %s", name, got, want)
			}
		}
	}
}

// specDesignColours are the colours of the design example in the spec.
var specDesignColours = map[string]string{
	"ground": `"#F6F5F2"`, "surface": `"#FFFFFF"`, "ink": `"#15171C"`,
	"muted": `"#5A606C"`, "accent": `"#0D7A63"`, "on_accent": `"#FFFFFF"`,
}

// coloursJSON builds a colours object: the spec example, with set replacing
// or adding raw JSON values (a key outside the six roles is appended in
// sorted order) and drop removing roles.
func coloursJSON(set map[string]string, drop ...string) json.RawMessage {
	dropped := map[string]bool{}
	for _, d := range drop {
		dropped[d] = true
	}
	var parts []string
	for _, role := range designColourRolesWant {
		if dropped[role] {
			continue
		}
		v := specDesignColours[role]
		if o, ok := set[role]; ok {
			v = o
		}
		parts = append(parts, `"`+role+`":`+v)
	}
	var extra []string
	for k := range set {
		if _, known := specDesignColours[k]; !known {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		parts = append(parts, `"`+k+`":`+set[k])
	}
	return json.RawMessage("{" + strings.Join(parts, ",") + "}")
}

func TestDesignColoursRefused(t *testing.T) {
	// A refused colour is one failure at design.colors.<role>, and the
	// refusal code is the grammar's bad_input; an absent one is not ready.
	cases := []struct {
		name string
		set  map[string]string
		key  string
	}{
		{"no hash sign", map[string]string{"ground": `"F6F5F2"`}, "ground"},
		{"three digit shorthand", map[string]string{"surface": `"#FFF"`}, "surface"},
		{"eight digits with alpha", map[string]string{"ink": `"#15171CFF"`}, "ink"},
		{"five digits", map[string]string{"muted": `"#5A606"`}, "muted"},
		{"digit outside hex", map[string]string{"accent": `"#0G7A63"`}, "accent"},
		{"trailing line feed", map[string]string{"on_accent": `"#FFFFFF\n"`}, "on_accent"},
		{"leading space", map[string]string{"ground": `" #F6F5F2"`}, "ground"},
		{"colour name", map[string]string{"ink": `"red"`}, "ink"},
		{"rgb function", map[string]string{"accent": `"rgb(13,122,99)"`}, "accent"},
		{"empty string", map[string]string{"muted": `""`}, "muted"},
		{"full width digits", map[string]string{"surface": `"#ＦＦＦＦＦＦ"`}, "surface"},
		{"number", map[string]string{"ground": `16119282`}, "ground"},
		{"null", map[string]string{"ink": `null`}, "ink"},
		{"boolean", map[string]string{"accent": `true`}, "accent"},
		{"array", map[string]string{"muted": `["#5A606C"]`}, "muted"},
		{"object", map[string]string{"surface": `{"hex":"#FFFFFF"}`}, "surface"},
		{"pure black as ink", map[string]string{"ink": `"#000000"`}, "ink"},
		{"pure black as muted", map[string]string{"muted": `"#000000"`}, "muted"},
		{"pure black as accent", map[string]string{"accent": `"#000000"`}, "accent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, fails := parseDesignColours(coloursJSON(c.set))
			if len(fails) != 1 {
				t.Fatalf("got %d failures %+v, want exactly one", len(fails), fails)
			}
			f := fails[0]
			if f.Path != "design.colors" || f.Key != c.key || f.code != pageCreateBadInput || f.Reason == "" {
				t.Errorf("failure %+v, want path design.colors, key %s, code %s and a reason", f, c.key, pageCreateBadInput)
			}
		})
	}

	t.Run("pure black says how to fix it", func(t *testing.T) {
		_, fails := parseDesignColours(coloursJSON(map[string]string{"ink": `"#000000"`}))
		if len(fails) != 1 || !strings.Contains(fails[0].Reason, "black") || !strings.Contains(fails[0].Reason, "#15171C") {
			t.Errorf("black is refused without saying so: %+v", fails)
		}
	})

	t.Run("a role left out is not ready", func(t *testing.T) {
		for _, role := range designColourRolesWant {
			_, fails := parseDesignColours(coloursJSON(nil, role))
			if len(fails) != 1 || fails[0].Key != role || fails[0].Path != "design.colors" || fails[0].code != pageDesignNotReady {
				t.Errorf("without %s: %+v, want one design_not_ready failure at design.colors.%s", role, fails, role)
			}
		}
	})

	t.Run("a key that is not a role is refused with the roles", func(t *testing.T) {
		_, fails := parseDesignColours(coloursJSON(map[string]string{"background": `"#FFFFFF"`}))
		if len(fails) != 1 || fails[0].Key != "background" || fails[0].code != pageCreateBadInput ||
			strings.Join(fails[0].Allowed, ",") != strings.Join(designColourRolesWant, ",") {
			t.Errorf("unknown role: %+v, want one bad_input failure at background allowing %v", fails, designColourRolesWant)
		}
	})

	t.Run("a role spelt in capitals is unknown and the role is missing", func(t *testing.T) {
		in := coloursJSON(map[string]string{"Ground": `"#F6F5F2"`}, "ground")
		_, fails := parseDesignColours(in)
		if len(fails) != 2 || fails[0].Key != "ground" || fails[0].code != pageDesignNotReady ||
			fails[1].Key != "Ground" || fails[1].code != pageCreateBadInput {
			t.Errorf("got %+v, want ground not ready, then Ground unknown", fails)
		}
	})

	t.Run("unknown keys are reported in sorted order, three at most, cleaned", func(t *testing.T) {
		in := coloursJSON(map[string]string{
			"zz": `"#FFFFFF"`, "aa": `"#FFFFFF"`, "mm": `"#FFFFFF"`, "bb": `"#FFFFFF"`, "evil\\nkey": `"#FFFFFF"`,
		})
		_, fails := parseDesignColours(in)
		var keys []string
		for _, f := range fails {
			keys = append(keys, f.Key)
		}
		if strings.Join(keys, "|") != "aa|bb|evil key" {
			t.Errorf("unknown keys reported as %q, want aa|bb|evil key", keys)
		}
	})

	t.Run("every failure is reported in one pass, roles in order", func(t *testing.T) {
		in := coloursJSON(map[string]string{"muted": `"#000000"`, "ground": `"nope"`}, "accent")
		_, fails := parseDesignColours(in)
		var keys []string
		for _, f := range fails {
			keys = append(keys, f.Key)
		}
		if strings.Join(keys, ",") != "ground,muted,accent" {
			t.Errorf("failures at %q, want ground,muted,accent", keys)
		}
	})

	t.Run("colours that are not an object", func(t *testing.T) {
		for _, raw := range []string{`"#FFFFFF"`, `[]`, `null`, `7`, `true`} {
			_, fails := parseDesignColours(json.RawMessage(raw))
			if len(fails) != 1 || fails[0].Path != "design" || fails[0].Key != "colors" || fails[0].code != pageCreateBadInput ||
				strings.Join(fails[0].Allowed, ",") != strings.Join(designColourRolesWant, ",") {
				t.Errorf("colours %s: %+v, want one bad_input failure at design.colors naming the roles", raw, fails)
			}
		}
	})

	t.Run("colours that are absent are not ready", func(t *testing.T) {
		_, fails := parseDesignColours(nil)
		if len(fails) != 1 || fails[0].Path != "design" || fails[0].Key != "colors" || fails[0].code != pageDesignNotReady {
			t.Errorf("absent colours: %+v, want one design_not_ready failure at design.colors", fails)
		}
	})

	// The positive controls: what must not be refused.
	t.Run("the spec example is accepted unchanged", func(t *testing.T) {
		got, fails := parseDesignColours(coloursJSON(nil))
		if len(fails) != 0 {
			t.Fatalf("the spec example is refused: %+v", fails)
		}
		want := designColours{Ground: "#F6F5F2", Surface: "#FFFFFF", Ink: "#15171C", Muted: "#5A606C", Accent: "#0D7A63", OnAccent: "#FFFFFF"}
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("lower and mixed case are uppercased", func(t *testing.T) {
		in := coloursJSON(map[string]string{
			"ground": `"#f6f5f2"`, "surface": `"#ffffff"`, "ink": `"#15171c"`,
			"muted": `"#5a606C"`, "accent": `"#0d7A63"`, "on_accent": `"#FfFfFf"`,
		})
		got, fails := parseDesignColours(in)
		if len(fails) != 0 {
			t.Fatalf("lower case is refused: %+v", fails)
		}
		want := designColours{Ground: "#F6F5F2", Surface: "#FFFFFF", Ink: "#15171C", Muted: "#5A606C", Accent: "#0D7A63", OnAccent: "#FFFFFF"}
		if got != want {
			t.Errorf("got %+v, want the upper-case %+v", got, want)
		}
	})

	t.Run("a near black and pure white are not black", func(t *testing.T) {
		for _, v := range []string{`"#000001"`, `"#010101"`, `"#0A0A0A"`, `"#FFFFFF"`} {
			if _, fails := parseDesignColours(coloursJSON(map[string]string{"muted": v})); len(fails) != 0 {
				t.Errorf("muted %s is refused: %+v", v, fails)
			}
		}
	})

	t.Run("the colours marshal as the spec writes them", func(t *testing.T) {
		got, _ := parseDesignColours(coloursJSON(nil))
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"ground":"#F6F5F2","surface":"#FFFFFF","ink":"#15171C","muted":"#5A606C","accent":"#0D7A63","on_accent":"#FFFFFF"}`
		if string(b) != want {
			t.Errorf("marshalled %s, want %s", b, want)
		}
	})
}

// hexOf turns role -> "#RRGGBB" into the raw JSON overrides coloursJSON takes.
func hexOf(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = `"` + v + `"`
	}
	return out
}

func TestDesignContrastLowNamesPair(t *testing.T) {
	t.Run("ratios follow the WCAG formula", func(t *testing.T) {
		for _, c := range []struct {
			a, b string
			want float64
		}{
			{"#000000", "#FFFFFF", 21},
			{"#FFFFFF", "#000000", 21},
			{"#FFFFFF", "#FFFFFF", 1},
			{"#767676", "#FFFFFF", 4.54},
			{"#777777", "#FFFFFF", 4.48},
			{"#15171C", "#F6F5F2", 16.45},
			{"#FFFFFF", "#0D7A63", 5.27},
		} {
			if got := designContrast(c.a, c.b); got < c.want-0.01 || got > c.want+0.01 {
				t.Errorf("contrast of %s and %s is %.4f, want %.2f", c.a, c.b, got, c.want)
			}
		}
	})

	pairNames := []string{"ink on ground", "ink on surface", "on_accent on accent"}
	for _, c := range []struct {
		name string
		set  map[string]string
		fail []int // indexes into pairNames, in report order
	}{
		{"ink on ground alone", map[string]string{"ground": "#6E6E6E"}, []int{0}},
		{"ink on surface alone", map[string]string{"surface": "#4A4A4A"}, []int{1}},
		{"on_accent on accent alone", map[string]string{"accent": "#F0F0F0"}, []int{2}},
		{"ink on both grounds", map[string]string{"ink": "#CCCCCC", "ground": "#FFFFFF", "surface": "#F6F5F2"}, []int{0, 1}},
		{"all three pairs", map[string]string{"ink": "#CCCCCC", "ground": "#FFFFFF", "surface": "#F6F5F2", "accent": "#FFFFFF"}, []int{0, 1, 2}},
		{"a colour on itself", map[string]string{"ink": "#F6F5F2"}, []int{0, 1}},
		{"just under the minimum", map[string]string{"ink": "#777777", "ground": "#FFFFFF"}, []int{0, 1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, fails := parseDesignColours(coloursJSON(hexOf(c.set)))
			if len(fails) != len(c.fail) {
				t.Fatalf("%d failures %+v, want %d (%v)", len(fails), fails, len(c.fail), c.fail)
			}
			for i, idx := range c.fail {
				f := fails[i]
				if f.code != pageDesignContrastLow || f.Path != "design.colors" {
					t.Errorf("failure %d: %+v, want code %s at design.colors", i, f, pageDesignContrastLow)
				}
				for j, name := range pairNames {
					if has := strings.Contains(f.Reason, name); has != (j == idx) {
						t.Errorf("failure %d reason %q: names %q = %v, want %v", i, f.Reason, name, has, j == idx)
					}
				}
				if !strings.Contains(f.Reason, "4.5:1") {
					t.Errorf("failure %d reason %q does not give the minimum", i, f.Reason)
				}
				if want := strings.Fields(pairNames[idx])[0]; f.Key != want {
					t.Errorf("failure %d key %q, want %q", i, f.Key, want)
				}
			}
			// The colours that were read stay readable, so a caller can show
			// what was sent.
			if got.Ink == "" || got.Ground == "" {
				t.Errorf("a contrast failure empties the colours: %+v", got)
			}
		})
	}

	t.Run("the reason gives the measured ratio, cut not rounded", func(t *testing.T) {
		_, fails := parseDesignColours(coloursJSON(hexOf(map[string]string{"ink": "#777777", "ground": "#FFFFFF"})))
		if len(fails) != 2 || !strings.Contains(fails[0].Reason, "4.47:1") || !strings.Contains(fails[1].Reason, "4.47:1") {
			t.Errorf("got %+v, want both ink pairs at the ratio 4.478 written 4.47:1", fails)
		}
	})

	t.Run("at and above the minimum is accepted", func(t *testing.T) {
		in := coloursJSON(hexOf(map[string]string{"ink": "#767676", "ground": "#FFFFFF", "surface": "#FFFFFF"}))
		if _, fails := parseDesignColours(in); len(fails) != 0 {
			t.Errorf("4.54:1 is refused: %+v", fails)
		}
		if _, fails := parseDesignColours(coloursJSON(nil)); len(fails) != 0 {
			t.Errorf("the spec example is refused: %+v", fails)
		}
	})

	t.Run("a pair with a colour that failed is not judged twice", func(t *testing.T) {
		_, fails := parseDesignColours(coloursJSON(map[string]string{"ink": `"nope"`}))
		if len(fails) != 1 || fails[0].code != pageCreateBadInput || fails[0].Key != "ink" {
			t.Errorf("got %+v, want only the bad ink", fails)
		}
		_, fails = parseDesignColours(coloursJSON(nil, "accent"))
		if len(fails) != 1 || fails[0].code != pageDesignNotReady {
			t.Errorf("got %+v, want only accent not ready", fails)
		}
		if fails := designContrastFailures(designColours{}); len(fails) != 0 {
			t.Errorf("empty colours are judged: %+v", fails)
		}
	})
}

// specDesignJSON is the design example of the spec (elementor.md 2.3).
const specDesignJSON = `{"colors": {"ground": "#F6F5F2", "surface": "#FFFFFF", "ink": "#15171C", "muted": "#5A606C", "accent": "#0D7A63", "on_accent": "#FFFFFF"},
 "fonts": {"heading": "Manrope", "body": "Source Sans 3"}, "scale": {"base_px": 17, "ratio": "major_third"},
 "radius": "md", "density": "normal"}`

// specDesign is what the validator makes of it.
var specDesign = pageDesign{
	Colors:  designColours{Ground: "#F6F5F2", Surface: "#FFFFFF", Ink: "#15171C", Muted: "#5A606C", Accent: "#0D7A63", OnAccent: "#FFFFFF"},
	Fonts:   designFonts{Heading: "Manrope", Body: "Source Sans 3"},
	Scale:   designScale{BasePx: 17, Ratio: "major_third"},
	Radius:  "md",
	Density: "normal",
}

// designMember is the name of a top-level member of the design object, read
// from the list the validator walks (designTopKeys). A test that names a
// member through it fails here, with the list in view, when the validator no
// longer has that member.
func designMember(t *testing.T, name string) string {
	t.Helper()
	i := slices.Index(designTopKeys, name)
	if i < 0 {
		t.Fatalf("the design object has no member %q; the validator lists %v", name, designTopKeys)
	}
	return designTopKeys[i]
}

// designChange replaces one field of the spec design with raw JSON, or
// removes it when raw is empty. path is "design" for a field of the design
// itself and "design.<group>" for a field of a group; a key with no group
// path names a whole group ("design", "fonts").
type designChange struct{ path, key, raw string }

func designEdit(t *testing.T, changes ...designChange) json.RawMessage {
	t.Helper()
	var top map[string]any
	if err := json.Unmarshal([]byte(specDesignJSON), &top); err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		target := top
		if group := strings.TrimPrefix(strings.TrimPrefix(c.path, "design"), "."); group != "" {
			target = top[group].(map[string]any)
		}
		if c.raw == "" {
			delete(target, c.key)
		} else {
			target[c.key] = json.RawMessage(c.raw)
		}
	}
	b, err := json.Marshal(top)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// designLeaves are the twelve fields a design must hold, as path and key.
var designLeaves = []struct{ path, key string }{
	{"design.colors", "ground"}, {"design.colors", "surface"}, {"design.colors", "ink"},
	{"design.colors", "muted"}, {"design.colors", "accent"}, {"design.colors", "on_accent"},
	{"design.fonts", "heading"}, {"design.fonts", "body"},
	{"design.scale", "base_px"}, {"design.scale", "ratio"},
	{"design", "radius"}, {"design", "density"},
}

func TestDesignNotReady(t *testing.T) {
	t.Run("the spec example is ready, and marshals as the spec writes it", func(t *testing.T) {
		got, problem := validatePageDesign(json.RawMessage(specDesignJSON))
		if problem != nil {
			t.Fatalf("the spec example is refused: %+v", problem)
		}
		if got != specDesign {
			t.Errorf("got %+v, want %+v", got, specDesign)
		}
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"colors":{"ground":"#F6F5F2","surface":"#FFFFFF","ink":"#15171C","muted":"#5A606C","accent":"#0D7A63","on_accent":"#FFFFFF"},` +
			`"fonts":{"heading":"Manrope","body":"Source Sans 3"},"scale":{"base_px":17,"ratio":"major_third"},"radius":"md","density":"normal"}`
		if string(b) != want {
			t.Errorf("marshalled %s, want %s", b, want)
		}
	})

	t.Run("every field left out makes the design not ready", func(t *testing.T) {
		for _, l := range designLeaves {
			got, problem := validatePageDesign(designEdit(t, designChange{path: l.path, key: l.key}))
			if problem == nil || problem.Code != pageDesignNotReady || len(problem.Failures) != 1 {
				t.Errorf("without %s.%s: %+v, want one design_not_ready failure", l.path, l.key, problem)
				continue
			}
			if f := problem.Failures[0]; f.Path != l.path || f.Key != l.key || f.code != pageDesignNotReady || f.Reason == "" {
				t.Errorf("without %s.%s: failure %+v", l.path, l.key, f)
			}
			if got != (pageDesign{}) {
				t.Errorf("without %s.%s: a refused design returned %+v", l.path, l.key, got)
			}
		}
	})

	t.Run("a group left out is one failure", func(t *testing.T) {
		for _, group := range []string{"colors", "fonts", "scale"} {
			_, problem := validatePageDesign(designEdit(t, designChange{path: "design", key: group}))
			if problem == nil || problem.Code != pageDesignNotReady || len(problem.Failures) != 1 ||
				problem.Failures[0].Path != "design" || problem.Failures[0].Key != group || len(problem.Failures[0].Allowed) == 0 {
				t.Errorf("without %s: %+v, want one design_not_ready failure naming what the group holds", group, problem)
			}
		}
	})

	t.Run("an empty design lists every part, in the spec's order", func(t *testing.T) {
		_, problem := validatePageDesign(json.RawMessage(`{}`))
		if problem == nil || problem.Code != pageDesignNotReady {
			t.Fatalf("got %+v", problem)
		}
		var keys []string
		for _, f := range problem.Failures {
			if f.Path != "design" || f.code != pageDesignNotReady {
				t.Errorf("failure %+v", f)
			}
			keys = append(keys, f.Key)
		}
		if strings.Join(keys, ",") != "colors,fonts,scale,radius,density" {
			t.Errorf("failures at %q, want colors,fonts,scale,radius,density", keys)
		}
	})

	t.Run("no design at all is not ready", func(t *testing.T) {
		for _, raw := range []json.RawMessage{nil, {}} {
			_, problem := validatePageDesign(raw)
			if problem == nil || problem.Code != pageDesignNotReady || len(problem.Failures) != 1 ||
				problem.Failures[0].Path != "" || problem.Failures[0].Key != "design" {
				t.Errorf("absent design %q: %+v, want one design_not_ready failure at design", raw, problem)
			}
		}
	})

	t.Run("a design that is not an object is bad input, not missing", func(t *testing.T) {
		for _, raw := range []string{`"md"`, `[]`, `null`, `7`, `true`} {
			_, problem := validatePageDesign(json.RawMessage(raw))
			if problem == nil || problem.Code != pageCreateBadInput || len(problem.Failures) != 1 ||
				problem.Failures[0].Key != "design" || len(problem.Failures[0].Allowed) != 5 {
				t.Errorf("design %s: %+v, want one bad_input failure at design", raw, problem)
			}
		}
	})

	t.Run("a field that is there but null is wrong, not missing", func(t *testing.T) {
		for _, l := range designLeaves {
			_, problem := validatePageDesign(designEdit(t, designChange{path: l.path, key: l.key, raw: "null"}))
			if problem == nil || problem.Code != pageCreateBadInput || len(problem.Failures) != 1 ||
				problem.Failures[0].Path != l.path || problem.Failures[0].Key != l.key {
				t.Errorf("%s.%s null: %+v, want one bad_input failure", l.path, l.key, problem)
			}
		}
		for _, group := range []string{"colors", "fonts", "scale"} {
			_, problem := validatePageDesign(designEdit(t, designChange{path: "design", key: group, raw: "null"}))
			if problem == nil || problem.Code != pageCreateBadInput || len(problem.Failures) != 1 || problem.Failures[0].Key != group {
				t.Errorf("%s null: %+v, want one bad_input failure", group, problem)
			}
		}
	})

	t.Run("the code is the first failure's, in the spec's order", func(t *testing.T) {
		for _, c := range []struct {
			name    string
			changes []designChange
			code    string
			keys    string
		}{
			{"radius wrong, density left out", []designChange{{"design", "radius", `"round"`}, {"design", "density", ""}},
				pageCreateBadInput, "radius,density"},
			{"fonts.body left out, density wrong", []designChange{{"design.fonts", "body", ""}, {"design", "density", `"dense"`}},
				pageDesignNotReady, "body,density"},
			{"low contrast, radius left out", []designChange{{"design.colors", "ground", `"#6E6E6E"`}, {"design", "radius", ""}},
				pageDesignContrastLow, "ink,radius"},
			{"low contrast, ratio wrong", []designChange{{"design.colors", "surface", `"#4A4A4A"`}, {"design.scale", "ratio", `"x"`}},
				pageDesignContrastLow, "ink,ratio"},
			{"colour wrong, contrast would also fail", []designChange{{"design.colors", "ink", `"#000000"`}, {"design.colors", "ground", `"#111111"`}},
				pageCreateBadInput, "ink"},
		} {
			_, problem := validatePageDesign(designEdit(t, c.changes...))
			if problem == nil {
				t.Errorf("%s: accepted", c.name)
				continue
			}
			var keys []string
			for _, f := range problem.Failures {
				keys = append(keys, f.Key)
			}
			if problem.Code != c.code || strings.Join(keys, ",") != c.keys {
				t.Errorf("%s: code %s on %q, want %s on %q", c.name, problem.Code, keys, c.code, c.keys)
			}
		}
	})
}

func TestDesignFontsScaleRadiusDensity(t *testing.T) {
	refused := func(t *testing.T, path, key, raw string) {
		t.Helper()
		got, problem := validatePageDesign(designEdit(t, designChange{path: path, key: key, raw: raw}))
		if problem == nil || problem.Code != pageCreateBadInput || len(problem.Failures) != 1 {
			t.Errorf("%s.%s = %s: %+v, want one bad_input failure", path, key, raw, problem)
			return
		}
		if f := problem.Failures[0]; f.Path != path || f.Key != key || f.Reason == "" {
			t.Errorf("%s.%s = %s: failure %+v", path, key, raw, f)
		}
		if got != (pageDesign{}) {
			t.Errorf("%s.%s = %s: a refused design returned %+v", path, key, raw, got)
		}
	}
	accepted := func(t *testing.T, path, key, raw string) pageDesign {
		t.Helper()
		got, problem := validatePageDesign(designEdit(t, designChange{path: path, key: key, raw: raw}))
		if problem != nil {
			t.Errorf("%s.%s = %s is refused: %+v", path, key, raw, problem)
		}
		return got
	}

	t.Run("a font family is 1 to 60 letters, digits and spaces, or system", func(t *testing.T) {
		for _, key := range []string{"heading", "body"} {
			for _, name := range []string{"Manrope", "Source Sans 3", "system", "A", "Noto Sans 400", strings.Repeat("a", 60)} {
				got := accepted(t, "design.fonts", key, `"`+name+`"`)
				if (key == "heading" && got.Fonts.Heading != name) || (key == "body" && got.Fonts.Body != name) {
					t.Errorf("%s %q came back as %+v", key, name, got.Fonts)
				}
			}
			for _, raw := range []string{
				`""`, `"` + strings.Repeat("a", 61) + `"`, `"Manrope;"`, `"Manrope\n"`, `"Manrope\t"`, `"Man\"rope"`, `"a/b"`,
				`"Source Sans 3 (var)"`, `"Ünicode"`, `"<b>"`, `"font-family"`, `"Playfair_Display"`, `"Mañana"`, `"Manrope\u0000"`,
				`"body { color: red }"`, `7`, `null`, `true`, `[]`, `{}`,
			} {
				refused(t, "design.fonts", key, raw)
			}
		}
	})

	t.Run("a font key that is not heading or body is refused with both", func(t *testing.T) {
		_, problem := validatePageDesign(designEdit(t, designChange{path: "design.fonts", key: "mono", raw: `"Fira Code"`}))
		if problem == nil || len(problem.Failures) != 1 || problem.Failures[0].Key != "mono" ||
			strings.Join(problem.Failures[0].Allowed, ",") != "heading,body" {
			t.Errorf("got %+v", problem)
		}
	})

	t.Run("base_px is a whole number from 15 to 20", func(t *testing.T) {
		for _, n := range []string{"15", "16", "17", "18", "19", "20"} {
			want, _ := strconv.ParseInt(n, 10, 64)
			if got := accepted(t, "design.scale", "base_px", n); got.Scale.BasePx != want {
				t.Errorf("base_px %s came back as %+v", n, got.Scale)
			}
		}
		for _, raw := range []string{"14", "21", "0", "-17", "17.5", "17.0", "1.7e1", `"17"`, "null", "true", "[17]", "99999999999999999999"} {
			refused(t, "design.scale", "base_px", raw)
		}
	})

	t.Run("ratio is major_second, minor_third, major_third or perfect_fourth", func(t *testing.T) {
		for _, name := range designRatiosWant {
			if got := accepted(t, "design.scale", "ratio", `"`+name+`"`); got.Scale.Ratio != name {
				t.Errorf("ratio %s came back as %q", name, got.Scale.Ratio)
			}
		}
		for _, raw := range []string{
			// A number is refused, one a name stands for as much as any other.
			"1.125", "1.2", "1.25", "1.333", "1", "0", "-1.25", "2", "1e0", "1.25e0", "1e999",
			// A name written another way, or outside the set.
			`"Major_Third"`, `"MAJOR_THIRD"`, `"major third"`, `"major-third"`, `"majorthird"`, `"major_third "`, `" major_third"`,
			`"major_third\n"`, `"major_third\u0000"`, `"major_thir"`, `""`,
			`"minor_second"`, `"major_second;"`, `"golden"`, `"perfect_fifth"`, `"augmented_fourth"`, `"x"`,
			// A number written as text, and every other JSON type.
			`"1.25"`, `"1"`, "null", "true", "false", "[]", `["major_third"]`, "{}", `{"name":"major_third"}`,
		} {
			refused(t, "design.scale", "ratio", raw)
		}
	})

	t.Run("a ratio left out or refused lists the four names, smallest first", func(t *testing.T) {
		want := strings.Join(designRatiosWant, ",")
		for name, change := range map[string]designChange{
			"left out":   {path: "design.scale", key: "ratio"},
			"a number":   {path: "design.scale", key: "ratio", raw: "1.25"},
			"not a name": {path: "design.scale", key: "ratio", raw: `"golden"`},
		} {
			_, problem := validatePageDesign(designEdit(t, change))
			if problem == nil || len(problem.Failures) != 1 || strings.Join(problem.Failures[0].Allowed, ",") != want {
				t.Errorf("ratio %s: %+v, want one failure that lists %s", name, problem, want)
			}
		}
	})

	t.Run("a scale key that is not base_px or ratio is refused with both", func(t *testing.T) {
		_, problem := validatePageDesign(designEdit(t, designChange{path: "design.scale", key: "steps", raw: "5"}))
		if problem == nil || len(problem.Failures) != 1 || problem.Failures[0].Key != "steps" ||
			strings.Join(problem.Failures[0].Allowed, ",") != "base_px,ratio" {
			t.Errorf("got %+v", problem)
		}
	})

	t.Run("radius is none, sm, md or lg", func(t *testing.T) {
		for _, v := range []string{"none", "sm", "md", "lg"} {
			if got := accepted(t, "design", "radius", `"`+v+`"`); got.Radius != v {
				t.Errorf("radius %s came back as %q", v, got.Radius)
			}
		}
		for _, raw := range []string{`"xl"`, `""`, `"MD"`, `"md "`, `"medium"`, "1", "null", `["md"]`} {
			refused(t, "design", "radius", raw)
		}
		_, problem := validatePageDesign(designEdit(t, designChange{path: "design", key: "radius", raw: `"xl"`}))
		if problem == nil || strings.Join(problem.Failures[0].Allowed, ",") != "none,sm,md,lg" {
			t.Errorf("a refused radius does not list the values: %+v", problem)
		}
	})

	t.Run("density is compact, normal or airy", func(t *testing.T) {
		for _, v := range []string{"compact", "normal", "airy"} {
			if got := accepted(t, "design", "density", `"`+v+`"`); got.Density != v {
				t.Errorf("density %s came back as %q", v, got.Density)
			}
		}
		for _, raw := range []string{`"dense"`, `"Normal"`, `""`, `"airy "`, "0", "null", `["airy"]`} {
			refused(t, "design", "density", raw)
		}
		_, problem := validatePageDesign(designEdit(t, designChange{path: "design", key: "density", raw: `"dense"`}))
		if problem == nil || strings.Join(problem.Failures[0].Allowed, ",") != "compact,normal,airy" {
			t.Errorf("a refused density does not list the values: %+v", problem)
		}
	})

	t.Run("a key the design does not have is refused with the five it has", func(t *testing.T) {
		_, problem := validatePageDesign(designEdit(t, designChange{path: "design", key: "shadows", raw: `{}`}))
		if problem == nil || problem.Code != pageCreateBadInput || len(problem.Failures) != 1 ||
			problem.Failures[0].Path != "design" || problem.Failures[0].Key != "shadows" ||
			strings.Join(problem.Failures[0].Allowed, ",") != "colors,fonts,scale,radius,density" {
			t.Errorf("got %+v", problem)
		}
	})

	t.Run("colours are checked on the way through, and come back upper case", func(t *testing.T) {
		got := accepted(t, "design.colors", "accent", `"#0d7a63"`)
		if got.Colors.Accent != "#0D7A63" {
			t.Errorf("accent came back as %q", got.Colors.Accent)
		}
		refused(t, "design.colors", "ink", `"#000000"`)
	})
}

// pageV3CasesFixture is the shared accept/refuse table of outline v3. The
// agent's tests own it; it is never hand-edited from here.
const pageV3CasesFixture = agentAbilityFixtures + "page-create-v3-cases.json"

// TestDesignSharedCases: the design validator answers the shared v3 table.
// A case whose rule is about the design is refused with the agent's own
// code, and a v3 case the control plane accepts as a whole holds a design
// that passes. (The table's other refusals break one other rule each, so the
// design is not what they test.)
func TestDesignSharedCases(t *testing.T) {
	b, err := os.ReadFile(pageV3CasesFixture)
	if err != nil {
		t.Fatalf("read %s: %v", pageV3CasesFixture, err)
	}
	var doc struct {
		Cases []struct {
			Name  string `json:"name"`
			Rule  string `json:"rule"`
			Input string `json:"input"`
			Agent string `json:"agent"`
			Go    string `json:"go"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("decode %s: %v", pageV3CasesFixture, err)
	}
	refusedWith := map[string]int{}
	accepted := 0
	for _, c := range doc.Cases {
		top, ok := jsonObjectOf([]byte(c.Input))
		if !ok {
			t.Errorf("%s: the input is not a JSON object", c.Name)
			continue
		}
		raw, hasDesign := top["design"]
		designRule := strings.HasPrefix(c.Rule, "design")
		if !designRule && (c.Go != "accept" || !hasDesign) {
			continue
		}
		_, problem := validatePageDesign(raw)
		if designRule && c.Go == "refuse" {
			if problem == nil || problem.Code != c.Agent {
				t.Errorf("%s: %+v, want a %s problem", c.Name, problem, c.Agent)
			}
			refusedWith[c.Agent]++
			continue
		}
		if problem != nil {
			t.Errorf("%s: the control plane accepts the case, but its design is refused: %+v", c.Name, problem)
		}
		accepted++
	}
	// A table that read as empty, or lost a class of case, proves nothing.
	for _, code := range []string{pageDesignNotReady, pageDesignContrastLow, pageCreateBadInput} {
		if refusedWith[code] == 0 {
			t.Errorf("the table has no design case refused with %s: %v", code, refusedWith)
		}
	}
	if accepted < 3 {
		t.Errorf("the table has %d accepted designs, want at least 3", accepted)
	}
}
