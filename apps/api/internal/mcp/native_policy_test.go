package mcp

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/elementornative"
)

func nativeTestValue(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	if len(raw) == 0 {
		raw = json.RawMessage(`[]`)
	}
	v, ok := phpDecodeDocument(raw, 32, true)
	if !ok {
		t.Fatalf("invalid test JSON: %s", raw)
	}
	return v
}

// Stored rows exercise the agent's stored-shape contract; written/raw rows
// exercise the independent AI grammar. Synthetic controls are never an owned
// allowlist: production narrowing is exercised through CheckNativeSettings.
func TestNativeSharedCases(t *testing.T) {
	raw, err := os.ReadFile("../../../agent/tests/fixtures/ability-run/native-settings-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name        string            `json:"name"`
		Schema      json.RawMessage   `json:"schema"`
		Settings    json.RawMessage   `json:"settings"`
		Breakpoints json.RawMessage   `json:"breakpoints"`
		Expected    json.RawMessage   `json:"expected"`
		Operation   string            `json:"operation"`
		Before      json.RawMessage   `json:"before"`
		Merged      json.RawMessage   `json:"merged"`
		Node        json.RawMessage   `json:"node"`
		RawJSON     string            `json:"raw_json"`
		Media       []NativeMediaFact `json:"library_images"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil || len(cases) == 0 {
		t.Fatalf("missing cases: %v", err)
	}
	executed := 0
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			executed++
			schema := nativeTestValue(t, c.Schema)
			state := nativePolicyState{schema: schema, media: c.Media, active: nativeTestValue(t, c.Breakpoints)}
			settings := nativeTestValue(t, c.Settings)
			switch c.Operation {
			case "written":
				before := nativeTestValue(t, c.Before)
				merged := nativeMerge(before, settings)
				if len(c.Merged) > 0 {
					merged = nativeTestValue(t, c.Merged)
				}
				state.validateWritten(before, settings, merged)
			case "raw":
				written, failure := nativeParseSettings([]byte(c.RawJSON))
				if failure != nil {
					state.failures = append(state.failures, *failure)
				} else {
					state.validateWritten(phpObject{}, written, nativeMerge(phpObject{}, written))
				}
			case "node":
				state.validateNode(nativeTestValue(t, c.Node))
			default:
				state.validateStored(settings)
			}
			if string(c.Expected) == `"ok"` {
				if len(state.failures) != 0 {
					t.Fatalf("wanted ok; failures: %+v", state.failures)
				}
				return
			}
			var expected struct {
				Code    string
				Keys    []string
				Paths   []string
				Allowed []string
			}
			if err := json.Unmarshal(c.Expected, &expected); err != nil {
				t.Fatal(err)
			}
			keys, paths := []string{}, []string{}
			for _, f := range state.failures {
				keys = append(keys, f.Key)
				paths = append(paths, f.Path)
				if f.Reason == "" {
					t.Fatal("empty failure reason")
				}
			}
			if !reflect.DeepEqual(keys, expected.Keys) {
				t.Fatalf("keys = %v; want %v; failures %+v", keys, expected.Keys, state.failures)
			}
			if expected.Paths != nil && !reflect.DeepEqual(paths, expected.Paths) {
				t.Fatalf("paths = %v; want %v", paths, expected.Paths)
			}
			if expected.Allowed != nil && !reflect.DeepEqual(state.failures[0].Allowed, expected.Allowed) {
				t.Fatalf("allowed = %v; want %v", state.failures[0].Allowed, expected.Allowed)
			}
		})
	}
	t.Logf("shared fixture rows executed: %d", executed)
}

func nativeHeadingInput(settings string) NativePolicyInput {
	return NativePolicyInput{Widget: "heading", ElementorVersion: "4.3.4", Schema: json.RawMessage(`{"widget":"heading","atomic":false,"controls":{"title":{"type":"textarea"},"header_size":{"type":"select","options":["h1","h2","h3","div","invented"]},"title_color":{"type":"color"},"align":{"type":"choose","options":["left","center","right"],"responsive":true},"_css_classes":{"type":"text"},"_element_id":{"type":"text"},"_element_cache":{"type":"text"},"invented":{"type":"text"},"link":{"type":"url"}}}`), Before: json.RawMessage(`[]`), Settings: json.RawMessage(settings), Breakpoints: []string{"mobile", "tablet"}}
}

func TestNativeOwnedNarrowing(t *testing.T) {
	for _, tc := range []struct {
		name, patch string
		mutate      func(*NativePolicyInput)
		ok          bool
	}{
		{"owned key", `{"title":"Hello"}`, nil, true},
		{"live extra key", `{"invented":"Hello"}`, nil, false},
		{"type mismatch", `{"title":"Hello"}`, func(i *NativePolicyInput) {
			i.Schema = json.RawMessage(`{"widget":"heading","atomic":false,"controls":{"title":{"type":"text"}}}`)
		}, false},
		{"missing live key", `{"title":"Hello"}`, func(i *NativePolicyInput) {
			i.Schema = json.RawMessage(`{"widget":"heading","atomic":false,"controls":{}}`)
		}, false},
		{"unknown widget", `{}`, func(i *NativePolicyInput) { i.Widget = "unknown" }, false},
		{"below pin", `{"title":"Hello"}`, func(i *NativePolicyInput) { i.ElementorVersion = "3.20.3" }, false},
		{"newer site", `{"title":"Hello"}`, func(i *NativePolicyInput) { i.ElementorVersion = "9.0.0" }, true},
		{"owned option", `{"header_size":"h2"}`, nil, true},
		{"live extra option", `{"header_size":"invented"}`, nil, false},
		{"live missing option", `{"header_size":"h6"}`, nil, false},
		{"responsive active", `{"align_mobile":"center"}`, nil, true},
		{"responsive inactive", `{"align_laptop":"center"}`, nil, false},
		{"responsive cannot widen", `{"title_mobile":"Hello"}`, func(i *NativePolicyInput) {
			i.Schema = json.RawMessage(`{"widget":"heading","atomic":false,"controls":{"title":{"type":"text","responsive":true}}}`)
		}, false},
		{"deny classes", `{"_css_classes":"safe"}`, nil, false},
		{"deny id", `{"_element_id":"safe"}`, nil, false},
		{"deny cache", `{"_element_cache":"safe"}`, nil, false},
		{"plain markup", `{"title":"<b>Hello</b>"}`, nil, false},
		{"numeric scalar", `{"title":12}`, nil, false},
		{"duplicate", `{"title":"Hello","\u0074itle":"Hello"}`, nil, false},
		{"upper colour", `{"title_color":"#12ABEF"}`, nil, true},
		{"lower colour", `{"title_color":"#12abef"}`, nil, false},
		{"link", `{"link":{"url":"/about","is_external":"","nofollow":""}}`, nil, true},
		{"link scheme", `{"link":{"url":"javascript:alert(1)","is_external":"","nofollow":""}}`, nil, false},
		{"carry unknown", `{"title":"Hello"}`, func(i *NativePolicyInput) { i.Before = json.RawMessage(`{"legacy":{"flag":true,"html":"<legacy>"}}`) }, true},
		{"changed carry", `{"title":"Hello"}`, func(i *NativePolicyInput) {
			i.Before = json.RawMessage(`{"legacy":true}`)
			i.Result = json.RawMessage(`{"legacy":"yes","title":"Hello"}`)
		}, false},
		{"merge order", `{"title":"Hello"}`, func(i *NativePolicyInput) {
			i.Before = json.RawMessage(`{"legacy":"Keep"}`)
			i.Result = json.RawMessage(`{"title":"Hello","legacy":"Keep"}`)
		}, false},
		{"delete", `{"title":null}`, func(i *NativePolicyInput) {
			i.Before = json.RawMessage(`{"title":"Old"}`)
			i.Result = json.RawMessage(`[]`)
		}, true},
		{"denied delete", `{"_element_id":null}`, func(i *NativePolicyInput) { i.Before = json.RawMessage(`{"_element_id":"keep"}`) }, false},
		{"binding", `{"title":"Hello"}`, func(i *NativePolicyInput) { i.Before = json.RawMessage(`{"__dynamic__":{"title":"binding"}}`) }, false},
		{"size cap", `{"title":"` + strings.Repeat("a", 65536) + `"}`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := nativeHeadingInput(tc.patch)
			if tc.mutate != nil {
				tc.mutate(&input)
			}
			got := CheckNativeSettings(input)
			if got.OK != tc.ok {
				t.Fatalf("ok = %v; want %v; failures %+v", got.OK, tc.ok, got.Failures)
			}
			if got.OK && len(got.Settings) == 0 {
				t.Fatal("accepted settings need canonical merge bytes")
			}
		})
	}
}

func TestNativeMediaAndRichText(t *testing.T) {
	mediaSchema := json.RawMessage(`{"widget":"image","atomic":false,"controls":{"image":{"type":"media"}}}`)
	for _, tc := range []struct {
		name, patch string
		media       []NativeMediaFact
		ok          bool
	}{
		{"verified", `{"image":{"id":"42","url":"https://example.com/a.jpg"}}`, []NativeMediaFact{{"42", "https://example.com/a.jpg"}}, true},
		{"unverified", `{"image":{"id":"42","url":"https://example.com/a.jpg"}}`, nil, false},
		{"wrong canonical URL", `{"image":{"id":"42","url":"https://example.com/b.jpg"}}`, []NativeMediaFact{{"42", "https://example.com/a.jpg"}}, false},
		{"wrong id", `{"image":{"id":"43","url":"https://example.com/a.jpg"}}`, []NativeMediaFact{{"42", "https://example.com/a.jpg"}}, false},
		{"scalar id", `{"image":{"id":42,"url":"https://example.com/a.jpg"}}`, []NativeMediaFact{{"42", "https://example.com/a.jpg"}}, false},
		{"source extra", `{"image":{"id":"42","url":"https://example.com/a.jpg","source":"library"}}`, []NativeMediaFact{{"42", "https://example.com/a.jpg"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckNativeSettings(NativePolicyInput{Widget: "image", ElementorVersion: "4.3.4", Schema: mediaSchema, Settings: json.RawMessage(tc.patch), Media: tc.media})
			if got.OK != tc.ok {
				t.Fatalf("ok=%v want %v: %+v", got.OK, tc.ok, got.Failures)
			}
		})
	}
	for _, tc := range []struct {
		name, markup string
		ok           bool
	}{
		{"closed tags", `<p>Hello <strong>world</strong></p>`, true},
		{"closed link", `<a href="/about" target="_blank" rel="noopener noreferrer">About</a>`, true},
		{"style", `<p style="color:red">Hello</p>`, false},
		{"encoded tag", `<p>&lt;script&gt;</p>`, false},
		{"balance", `<p><b>Hello</p>`, false},
		{"duplicate attribute", `<a href="/a" href="/b">Hello</a>`, false},
		{"entity alone", `&`, false},
		{"deep tags", strings.Repeat(`<p>`, 9) + "Hello" + strings.Repeat(`</p>`, 9), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patch, _ := json.Marshal(map[string]string{"editor": tc.markup})
			got := CheckNativeSettings(NativePolicyInput{Widget: "text-editor", ElementorVersion: "4.3.4", Schema: json.RawMessage(`{"widget":"text-editor","atomic":false,"controls":{"editor":{"type":"wysiwyg"}}}`), Settings: patch})
			if got.OK != tc.ok {
				t.Fatalf("ok=%v want %v: %+v", got.OK, tc.ok, got.Failures)
			}
		})
	}
}

func TestNativeOwnedRepeater(t *testing.T) {
	schema := json.RawMessage(`{"widget":"icon-list","atomic":false,"controls":{"icon_list":{"type":"repeater","fields":{"text":{"type":"text"},"link":{"type":"url"},"extra":{"type":"text"},"selected_icon":{"type":"icons"}}}}}`)
	for _, tc := range []struct {
		name, patch string
		ok          bool
	}{
		{"owned field", `{"icon_list":[{"_id":"abc1234","text":"Hello"}]}`, true},
		{"unowned field", `{"icon_list":[{"_id":"abc1234","extra":"Hello"}]}`, false},
		{"duplicate field", `{"icon_list":[{"_id":"abc1234","text":"Hello","text":"Hello"}]}`, false},
		{"duplicate id", `{"icon_list":[{"_id":"abc1234","text":"Hello"},{"_id":"abc1234","text":"Hello"}]}`, false},
		{"svg library", `{"icon_list":[{"_id":"abc1234","selected_icon":{"library":"svg","value":"https://example.com/i.svg"}}]}`, false},
		{"font icon", `{"icon_list":[{"_id":"abc1234","selected_icon":{"library":"fa-solid","value":"fas fa-star"}}]}`, true},
		{"nested denied carry replacement", `{"icon_list":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := NativePolicyInput{Widget: "icon-list", ElementorVersion: "4.3.4", Schema: schema, Settings: json.RawMessage(tc.patch)}
			if tc.name == "nested denied carry replacement" {
				input.Before = json.RawMessage(`{"icon_list":[{"_id":"abc1234","custom_css":"legacy"}]}`)
			}
			got := CheckNativeSettings(input)
			if got.OK != tc.ok {
				t.Fatalf("ok=%v want %v: %+v", got.OK, tc.ok, got.Failures)
			}
			if tc.name == "unowned field" && (len(got.Failures) != 1 || got.Failures[0].Path != "settings.icon_list.0" || got.Failures[0].Key != "extra") {
				t.Fatalf("wrong field failure: %+v", got.Failures)
			}
		})
	}
}

func TestNativeRawBoundsAndMerge(t *testing.T) {
	for _, raw := range []string{`{"title":"\ud800"}`, `{"title":"\udc00"}`, `{"title":"\ud800x"}`, `{"title":"Hello"} {}`, `{"title":"Hello"}x`, `["Hello"]`, `null`, `{"title":"` + string([]byte{0xff}) + `"}`, `{"title":` + strings.Repeat(`[`, 17) + `"x"` + strings.Repeat(`]`, 17) + `}`} {
		if got := CheckNativeSettings(nativeHeadingInput(raw)); got.OK {
			t.Fatalf("invalid JSON accepted: %q", raw)
		}
	}
	for _, tc := range []struct{ before, patch, result string }{
		{`{"old":true,"title":"Old","tail":"Keep"}`, `{"title":"New","header_size":"h2"}`, `{"old":true,"title":"New","tail":"Keep","header_size":"h2"}`},
		{`{"title":"Old"}`, `{"title":null}`, `[]`},
		{`[]`, `{}`, `[]`},
		{`{"nested":{"b":"Keep","a":true}}`, `{"title":"New"}`, `{"nested":{"b":"Keep","a":true},"title":"New"}`},
	} {
		i := nativeHeadingInput(tc.patch)
		i.Before = json.RawMessage(tc.before)
		i.Result = json.RawMessage(tc.result)
		got := CheckNativeSettings(i)
		if !got.OK || string(got.Settings) != tc.result {
			t.Fatalf("merge got %s / %+v; want %s", got.Settings, got.Failures, tc.result)
		}
	}
	for _, field := range []string{"Schema", "Before", "Result"} {
		i := nativeHeadingInput(`{"title":"Hello"}`)
		switch field {
		case "Schema":
			i.Schema = json.RawMessage(`{"widget":"heading","atomic":false,"controls":{},"controls":{}}`)
		case "Before":
			i.Before = json.RawMessage(`{"old":"First","old":"Last"}`)
		case "Result":
			i.Result = json.RawMessage(`{"title":"Hello","title":"Hello"}`)
		}
		if CheckNativeSettings(i).OK {
			t.Fatalf("duplicate accepted in %s", field)
		}
	}
}

func TestNativeOwnedGroupCannotWiden(t *testing.T) {
	// The owned dump records this activator as popover_toggle; a site cannot
	// relabel it as select to authorise a group member or a deletion.
	schema := json.RawMessage(`{"widget":"heading","atomic":false,"controls":{"typography_typography":{"type":"select","options":["custom"]},"typography_font_family":{"type":"font","group":"typography","activator":{"key":"typography_typography","values":["custom"]}}}}`)
	for _, patch := range []string{`{"typography_typography":"custom","typography_font_family":"Arial"}`, `{"typography_font_family":"Arial"}`, `{"typography_typography":null}`} {
		got := CheckNativeSettings(NativePolicyInput{Widget: "heading", ElementorVersion: "4.3.4", Schema: schema, Before: json.RawMessage(`{"typography_typography":"custom"}`), Settings: json.RawMessage(patch)})
		if got.OK {
			t.Fatalf("group type widened: %s", patch)
		}
	}
}

func TestNativeWrittenDimensions(t *testing.T) {
	schema := json.RawMessage(`{"widget":"heading","atomic":false,"controls":{"_padding":{"type":"dimensions","units":["px","rem","bad;unit"]},"_margin":{"type":"dimensions","units":["px"]}}}`)
	for _, tc := range []struct {
		name, patch string
		ok          bool
	}{
		{"string flag", `{"_padding":{"unit":"px","top":"1","right":"2","bottom":"3","left":"4","isLinked":"yes"}}`, true},
		{"boolean flag", `{"_padding":{"unit":"px","top":"1","right":"2","bottom":"3","left":"4","isLinked":true}}`, false},
		{"negative", `{"_padding":{"unit":"px","top":"-1","right":"2","bottom":"3","left":"4","isLinked":"yes"}}`, false},
		{"expression", `{"_padding":{"unit":"px","top":"calc(100% - 2px)","right":"2","bottom":"3","left":"4","isLinked":"yes"}}`, false},
		{"unoffered unit", `{"_margin":{"unit":"rem","top":"1","right":"2","bottom":"3","left":"4","isLinked":"yes"}}`, false},
		{"extra key", `{"_padding":{"unit":"px","top":"1","right":"2","bottom":"3","left":"4","isLinked":"yes","evil":"x"}}`, false},
		{"numeric scalar", `{"_padding":{"unit":"px","top":1,"right":"2","bottom":"3","left":"4","isLinked":"yes"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckNativeSettings(NativePolicyInput{Widget: "heading", ElementorVersion: "4.3.4", Schema: schema, Settings: json.RawMessage(tc.patch)})
			if got.OK != tc.ok {
				t.Fatalf("ok=%v want %v: %+v", got.OK, tc.ok, got.Failures)
			}
		})
	}
}

func TestNativeParserDepth(t *testing.T) {
	for _, tc := range []struct {
		depth int
		ok    bool
	}{{15, true}, {16, false}} {
		raw := []byte(`{"title":` + strings.Repeat(`[`, tc.depth) + `"Hello"` + strings.Repeat(`]`, tc.depth) + `}`)
		_, f := nativeParseSettings(raw)
		if (f == nil) != tc.ok {
			t.Fatalf("array nesting %d: failure %+v; want accepted %v", tc.depth, f, tc.ok)
		}
	}
}

func TestNativeReadOnlyAndBounds(t *testing.T) {
	i := nativeHeadingInput(`{"title":"Hello"}`)
	i.Before = json.RawMessage(`{"old":{"b":true,"a":"Keep"}}`)
	before, _ := json.Marshal(i)
	_ = CheckNativeSettings(i)
	after, _ := json.Marshal(i)
	if string(before) != string(after) {
		t.Fatal("validation changed its inputs")
	}
	i.Settings = json.RawMessage(`{"` + strings.Repeat("a", 300) + `":"value"}`)
	r := CheckNativeSettings(i)
	if r.OK || len(r.Failures) == 0 {
		t.Fatal("unknown long key accepted")
	}
	if len([]rune(r.Failures[0].Key)) > 60 {
		t.Fatal("failure key exceeds the agent envelope limit")
	}
	i.Schema = json.RawMessage(`{"widget":"heading","atomic":false,"controls":{"title":{"type":"textarea","label":"` + strings.Repeat("a", nativeSchemaLimit) + `"}}}`)
	if CheckNativeSettings(i).OK {
		t.Fatal("oversized schema accepted")
	}
	for _, field := range []string{"Schema", "Before", "Result"} {
		i := nativeHeadingInput(`{"title":"Hello"}`)
		switch field {
		case "Schema":
			i.Schema = json.RawMessage(`{"widget":"heading","atomic":false,"controls":{"title":{"type":"textarea","label":"\ud800"}}}`)
		case "Before":
			i.Before = json.RawMessage(`{"old":"\ud800"}`)
		case "Result":
			i.Result = json.RawMessage(`{"title":"Hello","old":"\ud800"}`)
		}
		if CheckNativeSettings(i).OK {
			t.Fatalf("invalid unicode accepted in %s", field)
		}
	}
}

func TestNativeOwnedSwitchReturnValues(t *testing.T) {
	for _, typ := range []string{"switcher", "popover_toggle"} {
		for _, tc := range []struct {
			name, liveReturn, value string
			ok                      bool
		}{
			{"agrees", "enabled", "enabled", true}, {"off", "enabled", "", true},
			{"wrong value", "enabled", "yes", false}, {"disagrees", "yes", "yes", false},
			{"disagrees off", "yes", "", false}, {"missing live", "", "enabled", false},
		} {
			t.Run(typ+"/"+tc.name, func(t *testing.T) {
				own := []byte(`{"type":"` + typ + `","return_value":"enabled","options":[],"fields":[]}`)
				var c elementornative.Control
				if err := json.Unmarshal(own, &c); err != nil {
					t.Fatal(err)
				}
				live := map[string]any{"type": typ}
				if tc.liveReturn != "" {
					live["return_value"] = tc.liveReturn
				}
				schema, _ := json.Marshal(map[string]any{"widget": "heading", "atomic": false, "controls": map[string]any{"toggle": live}})
				patch, _ := json.Marshal(map[string]string{"toggle": tc.value})
				state := nativePolicyState{schema: nativeTestValue(t, schema), owned: map[string]elementornative.Control{"toggle": c}, active: phpList{}}
				written := nativeTestValue(t, patch)
				state.validateWritten(phpObject{}, written, written)
				if (len(state.failures) == 0) != tc.ok {
					t.Fatalf("ok=%v want %v: %+v", len(state.failures) == 0, tc.ok, state.failures)
				}
			})
		}
	}
}

func TestNativeOwnedRegisteredDeviceWindow(t *testing.T) {
	desktop := "desktop"
	w := &elementornative.Window{Max: &desktop, Devices: []string{"desktop", "widescreen", "tablet"}}
	if !nativeOwnedResponsive(w, "widescreen") || !nativeOwnedResponsive(w, "tablet") || nativeOwnedResponsive(w, "mobile") {
		t.Fatal("owned responsive window must follow registered devices")
	}
}

func TestNativeRawCSSOwnedCeiling(t *testing.T) {
	for _, liveFlag := range []string{"", `,"raw_css_value":false`, `,"raw_css_value":true`} {
		schema := nativeTestValue(t, json.RawMessage(`{"widget":"heading","atomic":false,"controls":{"token":{"type":"text"`+liveFlag+`}}}`))
		for _, tc := range []struct {
			value string
			ok    bool
		}{{"span 2 / 3.5%", true}, {"span 2; position: fixed", false}} {
			state := nativePolicyState{schema: schema, active: phpList{}, owned: map[string]elementornative.Control{"token": {Type: "text", RawCSSValue: true}}}
			settings := phpObject{{"token", tc.value}}
			state.validateWritten(phpObject{}, settings, nativeMerge(phpObject{}, settings))
			if (len(state.failures) == 0) != tc.ok {
				t.Fatalf("flag=%s value=%s failures=%+v", liveFlag, tc.value, state.failures)
			}
		}
	}
}
