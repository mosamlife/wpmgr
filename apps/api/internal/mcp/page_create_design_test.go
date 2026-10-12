package mcp

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// The design object of outline v3 (elementor.md 2.3): the control plane's
// checks run before any agent call. The tests reach the validators with the
// raw JSON an input carries, so a wrong JSON type is as real as a wrong value.

// designColourRolesWant is the spec's list of colour roles, written out here
// so a change to the validator's list is a visible diff in this file.
var designColourRolesWant = []string{"ground", "surface", "ink", "muted", "accent", "on_accent"}

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
