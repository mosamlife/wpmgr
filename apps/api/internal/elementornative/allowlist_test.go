package elementornative

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNativeEmbeddedAllowlist(t *testing.T) {
	controls := Lookup("heading", "3.20.4")
	if len(controls) == 0 || controls["title"].Type != "textarea" || controls["header_size"].Type != "select" {
		t.Fatal("embedded allowlist must load real heading controls")
	}
	if controls["_margin"].Type != "dimensions" || controls["title_color"].Type != "color" {
		t.Fatal("editor style controls must be present")
	}
	for _, version := range []string{"", "broken", "3.20.3", "3.20.4-beta", "-1.0.0"} {
		if got := Lookup("heading", version); len(got) != 0 {
			t.Errorf("version %q widened: %v", version, got)
		}
	}
	if len(Lookup("unknown", "4.3.4")) != 0 {
		t.Fatal("unknown widget accepted")
	}
	if !reflect.DeepEqual(Lookup("heading", "9.0.0"), Lookup("heading", "4.3.4")) {
		t.Fatal("newer site must use newest pin")
	}
	if !reflect.DeepEqual(Lookup("heading", "3.34.0"), controls) {
		t.Fatal("site between pins must use lower pin")
	}
	if _, ok := Lookup("text-editor", "3.20.4")["link_color"]; ok {
		t.Fatal("new control leaked into old pin")
	}
	if Lookup("text-editor", "3.35.9")["link_color"].Type != "color" {
		t.Fatal("new control absent at its pin")
	}
	controls["title"] = Control{Type: "code"}
	if Lookup("heading", "3.20.4")["title"].Type != "textarea" {
		t.Fatal("lookup exposes mutable policy")
	}
}

func TestNativeMalformedAllowlist(t *testing.T) {
	for _, raw := range []string{"", "{}", `{"schema_version":2,"elementor_versions":[],"widgets":[]}`, `{"schema_version":99}`, `{"schema_version":2,"unexpected":true}`} {
		if _, err := loadAllowlist([]byte(raw)); err == nil {
			t.Errorf("malformed allowlist accepted: %s", raw)
		}
	}
	for _, mutate := range []func(map[string]any){
		func(a map[string]any) { a["elementor_versions"] = []any{"3.35.9", "3.20.4"} },
		func(a map[string]any) {
			w := a["widgets"].([]any)[0].(map[string]any)
			b := w["shared_controls"].([]any)[0].(map[string]any)
			b["controls"].(map[string]any)["_margin"] = 99999
		},
		func(a map[string]any) {
			w := a["widgets"].([]any)[0].(map[string]any)
			w["controls_skipped"].(map[string]any)["3.20.4"] = []any{map[string]any{"id": "_margin", "reason": "skip"}}
		},
		func(a map[string]any) {
			c := a["shared_controls"].(map[string]any)["_margin"].(map[string]any)["definitions"].([]any)[0].(map[string]any)
			c["responsive"].(map[string]any)["devices"] = []any{"watch"}
		},
	} {
		var doc map[string]any
		if err := json.Unmarshal(embeddedAllowlist, &doc); err != nil {
			t.Fatal(err)
		}
		mutate(doc)
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := loadAllowlist(raw); err == nil {
			t.Fatal("malformed embedded relationships accepted")
		}
	}
}

func TestNativeReturnValueDefinitions(t *testing.T) {
	for _, typ := range []string{"switcher", "popover_toggle"} {
		raw := []byte(`{"schema_version":2,"elementor_versions":["3.35.9"],"shared_controls":{},"widgets":[{"name":"heading","versions":["3.35.9"],"controls":[{"id":"toggle","versions":["3.35.9"],"definitions":[{"type":"` + typ + `","return_value":"enabled","options":[],"responsive":null,"group":null,"fields":[],"versions":["3.35.9"]}]}]}]}`)
		a, err := loadAllowlist(raw)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(cloneControl(a.byVersion["3.35.9"]["heading"]["toggle"]))
		if err != nil || !strings.Contains(string(encoded), `"return_value":"enabled"`) {
			t.Fatalf("return value lost: %s %v", encoded, err)
		}
		for _, invalid := range []string{`""`, `true`, `null`} {
			if _, err := loadAllowlist(bytes.ReplaceAll(raw, []byte(`"enabled"`), []byte(invalid))); err == nil {
				t.Fatalf("invalid return value accepted: %s", invalid)
			}
		}
	}
}
