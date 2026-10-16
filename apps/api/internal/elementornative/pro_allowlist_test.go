package elementornative

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func proFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/pro_theme_widgets.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestLookupProThemeEmptyAdmitsNothing(t *testing.T) {
	for _, name := range []string{"theme-site-logo", "theme-site-title", "nav-menu", "search-form", "heading", "unknown", ""} {
		for _, v := range []string{"", "broken", "3.35.9", "99.0.0"} {
			if got := LookupProTheme(name, v, v); got != nil {
				t.Errorf("empty policy admits %q at %q: %v", name, v, got)
			}
		}
	}
}

func TestLoadProThemeAllowlistRejects(t *testing.T) {
	mutations := map[string]func(map[string]any){
		"unknown widget": func(a map[string]any) { a["widgets"].([]any)[0].(map[string]any)["name"] = "heading" },
		"duplicate pair": func(a map[string]any) { a["pairs"] = []string{"3.35.9+3.35.1", "3.35.9+3.35.1"} },
		"bad pair":       func(a map[string]any) { a["pairs"] = []string{"3.35.9+3.35"} },
		"undeclared widget pair": func(a map[string]any) {
			a["widgets"].([]any)[0].(map[string]any)["versions"] = []string{"3.35.9+9.0.0"}
		},
		"dangling shared reference": func(a map[string]any) {
			a["widgets"].([]any)[0].(map[string]any)["shared_controls"].([]any)[0].(map[string]any)["controls"] = map[string]int{"absent": 0}
		},
		"schema version": func(a map[string]any) { a["schema_version"] = 2 },
		"unknown field":  func(a map[string]any) { a["unexpected"] = true },
		"null pairs":     func(a map[string]any) { a["pairs"] = nil },
		"null widgets":   func(a map[string]any) { a["widgets"] = nil },
		"null shared":    func(a map[string]any) { a["shared_controls"] = nil },
		"undeclared definition pair": func(a map[string]any) {
			a["shared_controls"].(map[string]any)["sample_text"].(map[string]any)["definitions"].([]any)[0].(map[string]any)["versions"] = []string{"3.35.9+9.0.0"}
		},
		"duplicate widget": func(a map[string]any) { a["widgets"] = append(a["widgets"].([]any), a["widgets"].([]any)[0]) },
		"shared index": func(a map[string]any) {
			a["widgets"].([]any)[0].(map[string]any)["shared_controls"].([]any)[0].(map[string]any)["controls"] = map[string]int{"sample_text": -1}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(proFixture(t), &doc); err != nil {
				t.Fatal(err)
			}
			mutate(doc)
			raw, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loadProThemeAllowlist(raw); err == nil {
				t.Fatal("malformed policy accepted")
			}
		})
	}
	for _, raw := range []string{"", "{}", string(proFixture(t)) + " {}"} {
		if _, err := loadProThemeAllowlist([]byte(raw)); err == nil {
			t.Errorf("malformed JSON accepted: %q", raw)
		}
	}
}

func TestLookupProThemePairs(t *testing.T) {
	a, err := loadProThemeAllowlist(proFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	old := ownedProThemeAllowlist
	ownedProThemeAllowlist = a
	t.Cleanup(func() { ownedProThemeAllowlist = old })
	for _, tc := range []struct{ name, elementor, pro, choice string }{
		{"theme-site-title", "3.35.9", "3.35.1", "first"},
		{"theme-site-title", "9.0.0", "9.0.0", "first"},
		{"theme-site-title", "3.36.0", "3.34.0", "second"},
		{"theme-site-title", "3.35.8", "3.35.1", ""},
		{"theme-site-title", "3.35.9", "3.35.0", ""},
		{"theme-site-title", "3.36.0", "3.33.9", ""},
		{"heading", "9.0.0", "9.0.0", ""},
		{"nav-menu", "9.0.0", "9.0.0", ""},
		{"theme-site-title", "3.35.9-beta", "3.35.1", ""},
		{"theme-site-title", "3.35.9", "broken", ""},
		{"theme-site-title", "", "3.35.1", ""},
	} {
		t.Run(tc.name+"/"+tc.elementor+"/"+tc.pro, func(t *testing.T) {
			got := LookupProTheme(tc.name, tc.elementor, tc.pro)
			if tc.choice == "" {
				if got != nil {
					t.Fatal("unavailable policy admitted controls")
				}
			} else if got["sample_text"].Type != "text" || !reflect.DeepEqual(got["sample_choice"].Options, []string{tc.choice}) {
				t.Fatalf("wrong pair controls: %v", got)
			}
		})
	}
	first := LookupProTheme("theme-site-title", "9.0.0", "9.0.0")
	before := LookupProTheme("theme-site-title", "9.0.0", "9.0.0")
	c := first["sample_choice"]
	c.Options[0] = "mutated"
	c.Versions[0] = "mutated"
	delete(first, "sample_text")
	if !reflect.DeepEqual(LookupProTheme("theme-site-title", "9.0.0", "9.0.0"), before) {
		t.Fatal("lookup exposes mutable policy")
	}
}

func TestLookupProThemeTieBreakAndMissingControls(t *testing.T) {
	raw := bytes.ReplaceAll(proFixture(t), []byte("3.36.0+3.34.0"), []byte("3.36.0+3.35.1"))
	a, err := loadProThemeAllowlist(raw)
	if err != nil {
		t.Fatal(err)
	}
	old := ownedProThemeAllowlist
	ownedProThemeAllowlist = a
	t.Cleanup(func() { ownedProThemeAllowlist = old })
	// Pair selection is independent of declaration order.
	a.Versions[0], a.Versions[1] = a.Versions[1], a.Versions[0]
	got := LookupProTheme("theme-site-title", "9.0.0", "9.0.0")
	if !reflect.DeepEqual(got["sample_choice"].Options, []string{"second"}) {
		t.Fatalf("equal Pro pins must prefer higher Elementor: %v", got)
	}
	delete(a.byVersion["3.36.0+3.35.1"], "theme-site-title")
	if LookupProTheme("theme-site-title", "9.0.0", "9.0.0") != nil {
		t.Fatal("missing widget at selected pair must not fall back")
	}
	a.byVersion["3.36.0+3.35.1"]["theme-site-title"] = map[string]Control{}
	if LookupProTheme("theme-site-title", "9.0.0", "9.0.0") != nil {
		t.Fatal("empty control map must admit nothing")
	}
}

func TestProThemeWidgetNamesAreDetached(t *testing.T) {
	want := []string{"theme-site-logo", "theme-site-title", "nav-menu", "search-form"}
	names := ProThemeWidgets()
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("unexpected theme widgets: %v", names)
	}
	names[0] = "unknown"
	if !reflect.DeepEqual(ProThemeWidgets(), want) {
		t.Fatal("widget names expose mutable policy")
	}
}

func TestLoadProThemeControlRelationships(t *testing.T) {
	for name, mutate := range map[string]func(*proThemeAllowlist){
		"undeclared binding pair": func(a *proThemeAllowlist) { a.Widgets[0].Shared[0].Versions = []string{"9.0.0+9.0.0"} },
		"undeclared control pair": func(a *proThemeAllowlist) { a.Widgets[0].Controls[0].Versions = []string{"9.0.0+9.0.0"} },
		"undeclared own definition pair": func(a *proThemeAllowlist) {
			a.Widgets[0].Controls[0].Definitions[0].Versions = []string{"9.0.0+9.0.0"}
		},
		"unavailable widget pair": func(a *proThemeAllowlist) { a.Widgets[0].Versions = a.Versions[:1] },
		"duplicate control": func(a *proThemeAllowlist) {
			a.Widgets[0].Controls = append(a.Widgets[0].Controls, a.Widgets[0].Controls[0])
		},
		"missing definition": func(a *proThemeAllowlist) {
			a.Widgets[0].Controls[0].Definitions = a.Widgets[0].Controls[0].Definitions[:1]
		},
		"duplicate versions":            func(a *proThemeAllowlist) { a.Widgets[0].Versions = []string{a.Versions[0], a.Versions[0]} },
		"duplicate binding":             func(a *proThemeAllowlist) { a.Widgets[0].Shared = append(a.Widgets[0].Shared, a.Widgets[0].Shared[0]) },
		"unavailable shared definition": func(a *proThemeAllowlist) { a.Shared["sample_text"].Definitions[0].Versions = a.Versions[:1] },
		"invalid option list":           func(a *proThemeAllowlist) { a.Widgets[0].Controls[0].Definitions[0].Options = nil },
		"invalid shared control":        func(a *proThemeAllowlist) { a.Shared["sample_text"].Definitions[0].Type = "" },
		"unavailable skipped pair":      func(a *proThemeAllowlist) { a.Widgets[0].Skipped["9.0.0+9.0.0"] = []skippedControl{} },
		"skipped allowed control": func(a *proThemeAllowlist) {
			a.Widgets[0].Skipped[a.Versions[0]] = []skippedControl{{ID: "sample_text", Reason: "skip"}}
		},
		"undeclared nested pair": func(a *proThemeAllowlist) {
			c := &a.Widgets[0].Controls[0].Definitions[0]
			c.Type = "repeater"
			c.Fields = []Control{{ID: "sample_field", Type: "text", Options: []string{}, Fields: []Control{}, Versions: []string{"9.0.0+9.0.0"}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var a proThemeAllowlist
			if err := json.Unmarshal(proFixture(t), &a); err != nil {
				t.Fatal(err)
			}
			mutate(&a)
			raw, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loadProThemeAllowlist(raw); err == nil {
				t.Fatal("malformed control relationships accepted")
			}
		})
	}
}
