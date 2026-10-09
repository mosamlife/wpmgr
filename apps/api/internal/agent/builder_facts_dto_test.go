package agent

import (
	"encoding/json"
	"testing"
)

func decodeBuilderFacts(t *testing.T, body string) Metadata {
	t.Helper()
	var dto metadataDTO
	if err := json.Unmarshal([]byte(body), &dto); err != nil {
		t.Fatalf("a metadata body must decode without error, got %v for %s", err, body)
	}
	return dto.toMetadata()
}

func boolStr(p *bool) string {
	if p == nil {
		return "nil"
	}
	if *p {
		return "true"
	}
	return "false"
}

// An agent that predates builder_facts sends no key. The result must be nil,
// which every reader takes as "not reported", never as "off".
func TestBuilderFactsAbsentIsNil(t *testing.T) {
	m := decodeBuilderFacts(t, `{"wp_version":"7.1","plugins":[]}`)
	if m.BuilderFacts != nil {
		t.Fatalf("an absent builder_facts must decode as nil, got %+v", m.BuilderFacts)
	}
}

func TestBuilderFactsFullShapeDecodes(t *testing.T) {
	m := decodeBuilderFacts(t, `{"builder_facts":{"v":1,"theme_template":"bricks","elementor":{"atomic_editor":true}}}`)
	bf := m.BuilderFacts
	if bf == nil {
		t.Fatal("a builder_facts object must decode")
	}
	if bf.SchemaVersion != 1 || bf.ThemeTemplate != "bricks" {
		t.Fatalf("scalars wrong: %+v", bf)
	}
	if bf.Elementor == nil || bf.Elementor.AtomicEditor == nil || !*bf.Elementor.AtomicEditor {
		t.Fatalf("elementor.atomic_editor true lost: %+v", bf.Elementor)
	}
}

// The three answers to "is the Atomic editor on" must stay three: true, false
// and no answer. A value that is not a JSON boolean is no answer; it is never
// coerced to false.
func TestBuilderFactsAtomicEditorIsNeverCoerced(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"true", `true`, "true"},
		{"false", `false`, "false"},
		{"null is no answer", `null`, "nil"},
		{"string true is no answer", `"true"`, "nil"},
		{"string false is no answer", `"false"`, "nil"},
		{"number one is no answer", `1`, "nil"},
		{"number zero is no answer", `0`, "nil"},
		{"object is no answer", `{}`, "nil"},
		{"array is no answer", `[]`, "nil"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := decodeBuilderFacts(t, `{"builder_facts":{"v":1,"elementor":{"atomic_editor":`+c.raw+`}}}`)
			if m.BuilderFacts == nil || m.BuilderFacts.Elementor == nil {
				t.Fatalf("an elementor object must stay present whatever atomic_editor holds: %+v", m.BuilderFacts)
			}
			if got := boolStr(m.BuilderFacts.Elementor.AtomicEditor); got != c.want {
				t.Fatalf("atomic_editor %s decoded as %s, want %s", c.raw, got, c.want)
			}
		})
	}
}

// An absent elementor member means "not loaded". Only a JSON object means
// "loaded"; any other value is treated as absent, never as loaded.
func TestBuilderFactsElementorMemberShapes(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantPresent bool
	}{
		{"absent", ``, false},
		{"object", `,"elementor":{"atomic_editor":false}`, true},
		{"empty object", `,"elementor":{}`, true},
		{"null", `,"elementor":null`, false},
		{"false", `,"elementor":false`, false},
		{"true", `,"elementor":true`, false},
		{"string", `,"elementor":"yes"`, false},
		{"php empty array", `,"elementor":[]`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := decodeBuilderFacts(t, `{"builder_facts":{"v":1`+c.raw+`}}`)
			if m.BuilderFacts == nil {
				t.Fatal("the builder_facts object must survive a bad elementor member")
			}
			if got := m.BuilderFacts.Elementor != nil; got != c.wantPresent {
				t.Fatalf("elementor present = %v, want %v", got, c.wantPresent)
			}
		})
	}
}

// builder_facts that is not an object reads as not reported, and the plugin
// list that rides in the same push still decodes: one bad field never fails
// the whole metadata push.
func TestBuilderFactsNonObjectNeverFailsThePush(t *testing.T) {
	for _, raw := range []string{`"x"`, `7`, `true`, `false`, `[]`, `[1,2]`, `null`} {
		t.Run(raw, func(t *testing.T) {
			m := decodeBuilderFacts(t, `{"wp_version":"7.1","plugins":[{"slug":"a/a.php","version":"1","active":true}],"builder_facts":`+raw+`}`)
			if m.BuilderFacts != nil {
				t.Fatalf("builder_facts %s must read as not reported, got %+v", raw, m.BuilderFacts)
			}
			if len(m.Plugins) != 1 || m.WPVersion != "7.1" {
				t.Fatalf("the rest of the push must survive: %+v", m)
			}
		})
	}
}

// A field of the wrong type is dropped on its own; the others are kept.
func TestBuilderFactsWrongTypedFieldIsDroppedAlone(t *testing.T) {
	m := decodeBuilderFacts(t, `{"builder_facts":{"v":"1","theme_template":5,"elementor":{"atomic_editor":true}}}`)
	bf := m.BuilderFacts
	if bf == nil {
		t.Fatal("the object itself must survive")
	}
	if bf.SchemaVersion != 0 {
		t.Fatalf("v given as a string must be dropped, got %d", bf.SchemaVersion)
	}
	if bf.ThemeTemplate != "" {
		t.Fatalf("theme_template given as a number must be dropped, got %q", bf.ThemeTemplate)
	}
	if bf.Elementor == nil || bf.Elementor.AtomicEditor == nil || !*bf.Elementor.AtomicEditor {
		t.Fatalf("the well-typed elementor member must be kept: %+v", bf.Elementor)
	}
}

// A newer agent's extra keys (the reserved v2 members) are ignored, not
// rejected: an older control plane keeps working.
func TestBuilderFactsUnknownKeysAreIgnored(t *testing.T) {
	m := decodeBuilderFacts(t, `{"builder_facts":{"v":2,"theme_template":"x","bricks":{"abilities_setting":true},
		"elementor":{"atomic_editor":true,"gate":"available","pro_active":true}}}`)
	bf := m.BuilderFacts
	if bf == nil || bf.SchemaVersion != 2 || bf.ThemeTemplate != "x" {
		t.Fatalf("known keys must still decode: %+v", bf)
	}
	if bf.Elementor == nil || bf.Elementor.AtomicEditor == nil || !*bf.Elementor.AtomicEditor {
		t.Fatalf("elementor.atomic_editor lost beside unknown members: %+v", bf.Elementor)
	}
}
