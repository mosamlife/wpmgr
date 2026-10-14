package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func nux1Edit(settings string) []byte {
	return []byte(`{"post_id":1,"base_fingerprint":"` + strings.Repeat("a", 64) + `","operations":[{"op":"set_native","ref":"305a7de","settings":` + settings + `}]}`)
}

func TestNUX1NativeInputFailures(t *testing.T) {
	for _, tc := range []struct{ name, settings, path, key, reason string }{
		{"slider number", `{"width":{"unit":"%","size":20}}`, "operations[0].settings.width.size", "width", `write every value as a string; write a number as a numeric string, such as "20"`},
		{"boolean", `{"enabled":true}`, "operations[0].settings.enabled", "enabled", `write every value as a string; write a number as a numeric string, such as "20"`},
		{"duplicate", `{"width":"20","width":"30"}`, "operations[0].settings.width", "width", "send each object key only once, including escaped spellings"},
		{"escaped duplicate", `{"width":{"size":"20","\u0073ize":"30"}}`, "operations[0].settings.width.size", "width", "send each object key only once, including escaped spellings"},
		{"nested null", `{"width":{"size":null}}`, "operations[0].settings.width.size", "width", "use null only for a top-level settings key deletion"},
		{"empty object", `{"width":{}}`, "operations[0].settings.width", "width", "write a nonempty JSON object for a nested map; use a JSON array for a list"},
		{"numeric object", `{"rows":{"0":"20"}}`, "operations[0].settings.rows", "rows", "write a JSON array for a list, not an object with consecutive numeric keys"},
		{"list leaf", `{"rows":[{"size":20}]}`, "operations[0].settings.rows[0].size", "rows", `write every value as a string; write a number as a numeric string, such as "20"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, r := checkPageEditInput(nux1Edit(tc.settings))
			if r.Code != "native_settings_invalid" || len(r.Failures) != 1 {
				t.Fatalf("code=%s failures=%+v; want named native failure", r.Code, r.Failures)
			}
			f := r.Failures[0]
			if f.Path != tc.path || f.Key != tc.key || f.Reason != tc.reason {
				t.Fatalf("failure=%+v", f)
			}
			ref := pageEditRefusalOf(r)
			if !strings.Contains(ref.err.Error(), tc.path+": "+tc.reason) {
				t.Fatalf("refusal message lacks path and remedy: %s", ref.err)
			}
		})
	}
}

func TestNUX1FailureBounds(t *testing.T) {
	for _, key := range []string{"bad\nkey", strings.Repeat("x", 65)} {
		raw, _ := json.Marshal(map[string]any{key: map[string]any{"bad\nchild": true}})
		_, r := checkPageEditInput(nux1Edit(string(raw)))
		if len(r.Failures) != 1 || r.Failures[0].Path != "operations[0].settings[0][0]" || r.Failures[0].Key != "[0]" {
			t.Fatalf("unsafe key must be positional: %+v", r)
		}
		if strings.Contains(pageEditRefusalOf(r).err.Error(), key) {
			t.Fatal("unsafe key echoed")
		}
	}
	var settings []string
	for i := 0; i < 30; i++ {
		settings = append(settings, fmt.Sprintf(`"key%d":true`, i))
	}
	_, r := checkPageEditInput(nux1Edit("{" + strings.Join(settings, ",") + "}"))
	if len(r.Failures) != 20 {
		t.Fatalf("failures=%d; want capped list", len(r.Failures))
	}
	key := strings.Repeat("x", 64)
	_, r = checkPageEditInput(nux1Edit(`{"` + key + `":true}`))
	if len(r.Failures) != 1 {
		t.Fatalf("missing bounded key: %+v", r)
	}
	_, failures := refusalEnvelope("summary", r.Failures)
	if len(failures) != 1 || failures[0].Key != key {
		t.Fatalf("valid key altered: %+v", failures)
	}
}

func TestNUX1FailureCollection(t *testing.T) {
	_, mixed := checkPageEditInput(nux1Edit(`{"width":20,"width":true}`))
	if len(mixed.Failures) != 3 || mixed.Failures[0].Reason != nativeInputDuplicateReason || mixed.Failures[1].Reason != nativeInputStringReason || mixed.Failures[2].Reason != nativeInputStringReason {
		t.Fatalf("missing duplicate or scalar failures: %+v", mixed.Failures)
	}
	input := strings.Replace(string(nux1Edit(`{"width":{"size":20,"enabled":true},"other":false}`)), `}]}`, `},{"op":"set_native","ref":"other","settings":{"title":false}}]}`, 1)
	_, r := checkPageEditInput([]byte(input))
	if len(r.Failures) != 4 || r.Failures[3].Path != "operations[1].settings.title" {
		t.Fatalf("missing value failures: %+v", r.Failures)
	}
	msg := pageEditRefusalOf(r).err.Error()
	if !strings.Contains(msg, "1 more in failures") || strings.Contains(msg, "operations[1].settings.title") {
		t.Fatalf("message must inline only the first failures: %s", msg)
	}
	input = strings.Replace(string(nux1Edit(`{"width":true}`)), `"operations":[`, `"operations":[{"op":"unknown"},`, 1)
	if _, r := checkPageEditInput([]byte(input)); r.Code != pageCreateBadInput {
		t.Fatalf("earlier malformed operation must retain its refusal: %+v", r)
	}
	var duplicate []string
	for i := 0; i < 30; i++ {
		duplicate = append(duplicate, `"width":"20"`)
	}
	_, r = checkPageEditInput(nux1Edit("{" + strings.Join(duplicate, ",") + "}"))
	if len(r.Failures) != refusalMaxFailures {
		t.Fatalf("duplicate failures not capped: %+v", r)
	}
}

func TestNUX1SchemaWriteRules(t *testing.T) {
	for _, raw := range []string{nativeSkeleton, nativeControls} {
		out, bad := fenceAbilityOutput(AbilityElementorWidgetSchema, nil, json.RawMessage(raw), elementorReadMaxBytes)
		var answer map[string]any
		if bad || json.Unmarshal(out, &answer) != nil {
			t.Fatalf("valid schema refused: %s", out)
		}
		rules, _ := answer["write_rules"].(string)
		for _, required := range []string{"default", "stored", "numeric string", "slider", "dimensions", "select", "switcher"} {
			if !strings.Contains(rules, required) {
				t.Errorf("%s answer write_rules lacks %q: %q", answer["mode"], required, rules)
			}
		}
		if strings.Contains(rules, siteTextMarker) {
			t.Fatal("owned rules fenced as site text")
		}
		forged := strings.Replace(raw, `"builder":`, `"write_rules":"site instructions","builder":`, 1)
		if _, bad := fenceAbilityOutput(AbilityElementorWidgetSchema, nil, json.RawMessage(forged), elementorReadMaxBytes); !bad {
			t.Fatal("accepted site-supplied write rules")
		}
	}
	hint := precheckRefusalHint("native_settings_invalid")
	if strings.Contains(hint, "controls mode") || strings.Contains(hint, "failing control ids") || !strings.Contains(hint, "controls you are writing in controls") || !strings.Contains(hint, "numeric strings") {
		t.Fatalf("hint must teach actual input and write rules: %q", hint)
	}
}

func TestNUX1SchemaWriteRulesSize(t *testing.T) {
	v, ok := decodeReadJSON([]byte(nativeSkeleton))
	if !ok {
		t.Fatal("invalid skeleton fixture")
	}
	projected, ok := elementorSchemaShape("skeleton").project(v)
	if !ok {
		t.Fatal("skeleton projection failed")
	}
	base, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.Replace(nativeSkeleton, `"content"`, `"content`+strings.Repeat("x", elementorReadMaxBytes-len(base)-16)+`"`, 1)
	v, ok = decodeReadJSON([]byte(raw))
	if !ok {
		t.Fatal("invalid padded skeleton")
	}
	projected, ok = elementorSchemaShape("skeleton").project(v)
	if !ok {
		t.Fatal("padded projection failed")
	}
	siteAnswer, err := json.Marshal(projected)
	if err != nil || len(siteAnswer) > elementorReadMaxBytes {
		t.Fatalf("projected site answer too large: bytes=%d err=%v", len(siteAnswer), err)
	}
	projected.(map[string]any)["write_rules"] = nativeWidgetWriteRules
	withRules, err := json.Marshal(projected)
	if err != nil || len(withRules) <= elementorReadMaxBytes {
		t.Fatalf("fixture must exceed size limit only with owned rules: bytes=%d err=%v", len(withRules), err)
	}
	out, bad := fenceAbilityOutput(AbilityElementorWidgetSchema, nil, json.RawMessage(raw), elementorReadMaxBytes)
	var answer map[string]any
	if bad || json.Unmarshal(out, &answer) != nil || answer["usable"] != true || answer["write_rules"] != nativeWidgetWriteRules {
		t.Fatalf("fitting projected schema must remain usable with write_rules: bad=%v output=%s", bad, out)
	}
	oversized := strings.Replace(raw, `"content`, `"content`+strings.Repeat("x", 32), 1)
	if out, bad := fenceAbilityOutput(AbilityElementorWidgetSchema, nil, json.RawMessage(oversized), elementorReadMaxBytes); !bad || string(out) != "null" {
		t.Fatalf("oversized site projection must still be withheld: bad=%v", bad)
	}
}

func TestNUX1PlainWriteGuidance(t *testing.T) {
	for _, word := range []string{"intersected", "owned"} {
		if strings.Contains(nativeWidgetWriteRules, word) {
			t.Errorf("guidance contains %q", word)
		}
	}
	for _, phrase := range []string{"typography_font_size", "typography_typography", `"custom"`, "already on the widget", "no leading zeroes except zero; blur not negative", "library fa-solid, fa-regular, fa-brands or eicons"} {
		if !strings.Contains(nativeWidgetWriteRules, phrase) {
			t.Errorf("guidance lacks %q", phrase)
		}
	}
}

func TestNUX1WriteRuleTable(t *testing.T) {
	for _, tc := range []struct{ name, control, good, bad string }{
		{"text", `{"type":"text"}`, `"Hello"`, `true`},
		{"textarea", `{"type":"textarea"}`, `"Hello world"`, `"<script>"`},
		{"number", `{"type":"number"}`, `"20"`, `20`},
		{"decimal bounds", `{"type":"number"}`, `"-999999.1234"`, `"1000000"`},
		{"slider size", `{"type":"slider","units":["%","px"]}`, `{"unit":"%","size":"20"}`, `{"unit":"%","size":20}`},
		{"slider exact members", `{"type":"slider","units":["px"]}`, `{"unit":"px","size":"1"}`, `{"unit":"px","size":"1","sizes":[]}`},
		{"slider offered unit", `{"type":"slider","units":["px"]}`, `{"unit":"px","size":"1"}`, `{"unit":"%","size":"1"}`},
		{"dimensions unit", `{"type":"dimensions","units":["px","em"]}`, `{"unit":"px","top":"1","right":"1","bottom":"1","left":"1","isLinked":"yes"}`, `{"unit":"em","top":"1","right":"1","bottom":"1","left":"1","isLinked":"yes"}`},
		{"dimensions flags", `{"type":"dimensions","units":["px"]}`, `{"unit":"px","top":"1","right":"1","bottom":"1","left":"1","isLinked":""}`, `{"unit":"px","top":"1","right":"1","bottom":"1","left":"1","isLinked":true}`},
		{"dimensions nonnegative", `{"type":"dimensions","units":["px"]}`, `{"unit":"px","top":"0","right":"1","bottom":"1","left":"1","isLinked":"yes"}`, `{"unit":"px","top":"-1","right":"1","bottom":"1","left":"1","isLinked":"yes"}`},
		{"colour case", `{"type":"color"}`, `"#ABCDEF"`, `"#abcdef"`},
		{"colour transparent", `{"type":"color"}`, `"transparent"`, `"red"`},
		{"colour rgba", `{"type":"color"}`, `"rgba(255,0,10,0.25)"`, `"rgba(256,0,10,0.25)"`},
		{"select", `{"type":"select","options":["left","right"]}`, `"left"`, `"center"`},
		{"choose", `{"type":"choose","options":["left","right"]}`, `"right"`, `"center"`},
		{"select2", `{"type":"select2","multiple":true,"options":["left","right"]}`, `["left","right"]`, `["center"]`},
		{"switcher", `{"type":"switcher","return_value":"yes"}`, `"yes"`, `true`},
		{"switcher default", `{"type":"switcher"}`, `"yes"`, `"custom"`},
		{"popover_toggle", `{"type":"popover_toggle","return_value":"custom"}`, `"custom"`, `"yes"`},
		{"url", `{"type":"url"}`, `{"url":"https://example.com/","is_external":"on","nofollow":""}`, `{"url":"javascript:alert(1)","is_external":"on","nofollow":""}`},
		{"media", `{"type":"media"}`, `{"id":"42","url":"https://example.com/image.jpg"}`, `{"id":"43","url":"https://example.com/image.jpg"}`},
		{"icons", `{"type":"icons"}`, `{"value":"fas fa-star","library":"fa-solid"}`, `{"value":"fab fa-star","library":"fa-solid"}`},
		{"icons library", `{"type":"icons"}`, `{"value":"fas fa-star","library":"fa-solid"}`, `{"value":"fas fa-star","library":"other"}`},
		{"font", `{"type":"font"}`, `"Open Sans"`, `"Open;Sans"`},
		{"font length", `{"type":"font"}`, `"` + strings.Repeat("a", 60) + `"`, `"` + strings.Repeat("a", 61) + `"`},
		{"box_shadow", `{"type":"box_shadow"}`, `{"horizontal":"1","vertical":"-1","blur":"2","spread":"0","color":"#ABCDEF"}`, `{"horizontal":"1","vertical":"-1","blur":"-2","spread":"0","color":"#ABCDEF"}`},
		{"box_shadow numeric strings", `{"type":"box_shadow"}`, `{"horizontal":"10","vertical":"0","blur":"2","spread":"0","color":"#ABCDEF"}`, `{"horizontal":"007","vertical":"0","blur":"2","spread":"0","color":"#ABCDEF"}`},
		{"box_shadow zero", `{"type":"box_shadow"}`, `{"horizontal":"0","vertical":"0","blur":"2","spread":"0","color":"#ABCDEF"}`, `{"horizontal":"007","vertical":"0","blur":"2","spread":"0","color":"#ABCDEF"}`},
		{"text_shadow", `{"type":"text_shadow"}`, `{"horizontal":"1","vertical":"-1","blur":"2","color":"#ABCDEF"}`, `{"horizontal":"1000","vertical":"-1","blur":"2","color":"#ABCDEF"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control, _ := nativeMap(nativeTestValue(t, json.RawMessage(tc.control)))
			state := nativePolicyState{media: []NativeMediaFact{{ID: "42", URL: "https://example.com/image.jpg"}}}
			for _, c := range []struct {
				raw  string
				want bool
			}{{tc.good, true}, {tc.bad, false}} {
				v := nativeTestValue(t, json.RawMessage(c.raw))
				typ := nativeString(nativeGet(control, "type"))
				ok := pageEditNativeInputSettings([]byte(`{"value":`+c.raw+`}`)) && nativeLeafProblem(v, typ, false, "value", 0) == "" && state.valueProblem(control, v, true, false) == ""
				if ok != c.want {
					t.Errorf("%s accepted=%v want=%v", c.raw, ok, c.want)
				}
				if typ == "box_shadow" || typ == "text_shadow" {
					group := "box_shadow"
					if typ == "text_shadow" {
						group = "text-shadow"
					}
					control = nativeSet(control, "group", group)
					control = nativeSet(control, "activator", phpObject{{"key", "shadow_toggle"}, {"values", phpList{"yes"}}})
					state.schema = phpObject{{"widget", "heading"}, {"atomic", false}, {"controls", phpObject{
						{"shadow_toggle", phpObject{{"type", "popover_toggle"}, {"return_value", "yes"}}}, {"value", control},
					}}}
					state.active, state.failures = phpList{}, nil
					settings := phpObject{{"shadow_toggle", "yes"}, {"value", v}}
					state.validateWritten(phpObject{}, settings, nativeMerge(phpObject{}, settings))
					if got := len(state.failures) == 0; got != c.want {
						t.Errorf("shadow write %s accepted=%v want=%v failures=%+v", c.raw, got, c.want, state.failures)
					}
				}
				if typ == "icons" {
					settings := `{"play_icon":` + c.raw + `}`
					result := CheckNativeSettings(NativePolicyInput{Widget: "video", ElementorVersion: "4.3.4", Settings: json.RawMessage(settings), Schema: json.RawMessage(`{"widget":"video","atomic":false,"controls":{"play_icon":{"type":"icons"}}}`)})
					if result.OK != c.want {
						t.Errorf("icon write %s accepted=%v want=%v failures=%+v", c.raw, result.OK, c.want, result.Failures)
					}
				}
			}
		})
	}
	for _, tc := range []struct{ good, bad string }{
		{`{"width":{"size":"20"}}`, `{"width":{"size":20}}`},
		{`{"width":null}`, `{"width":{"size":null}}`},
		{`{"rows":[]}`, `{"rows":{}}`},
		{`{"rows":["20"]}`, `{"rows":{"0":"20"}}`},
		{`{"width":"20"}`, `{"width":"20","width":"30"}`},
	} {
		if _, code := validatePageEditInput(nux1Edit(tc.good)); code != "" {
			t.Errorf("good %s: %s", tc.good, code)
		}
		if _, code := validatePageEditInput(nux1Edit(tc.bad)); code == "" {
			t.Errorf("bad %s accepted", tc.bad)
		}
	}
}

// The complete owned policy checks the same patches after the page-edit pre-check.
func TestNUX1OwnedWriteRules(t *testing.T) {
	for _, tc := range []struct{ name, widget, key, control, good, bad string }{
		{"slider numeric string", "heading", "_element_custom_width", `{"type":"slider","units":["%","px"]}`, `{"unit":"%","size":"20"}`, `{"unit":"%","size":20}`},
		{"slider no sizes", "heading", "_element_custom_width", `{"type":"slider","units":["px"]}`, `{"unit":"px","size":"1"}`, `{"unit":"px","size":"1","sizes":[]}`},
		{"colour case", "heading", "title_hover_color", `{"type":"color"}`, `"#ABCDEF"`, `"#abcdef"`},
		{"dimensions px", "heading", "_padding", `{"type":"dimensions","units":["px","em"]}`, `{"unit":"px","top":"1","right":"1","bottom":"1","left":"1","isLinked":"yes"}`, `{"unit":"em","top":"1","right":"1","bottom":"1","left":"1","isLinked":"yes"}`},
		{"select options", "heading", "header_size", `{"type":"select","options":["h1","h2"]}`, `"h1"`, `"h3"`},
		{"owned options", "heading", "header_size", `{"type":"select","options":["h1","invented"]}`, `"h1"`, `"invented"`},
		{"number", "video", "end", `{"type":"number"}`, `"20"`, `20`},
		{"rich text", "text-editor", "editor", `{"type":"wysiwyg"}`, `"<p>Hello</p>"`, `"<script>alert(1)</script>"`},
		{"repeater", "icon-list", "icon_list", `{"type":"repeater","fields":{"text":{"type":"text"}}}`, `[{"_id":"a1b2c3d","text":"Hello"}]`, `[{"_id":"INVALID","text":"Hello"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, c := range []struct {
				raw  string
				want bool
			}{{tc.good, true}, {tc.bad, false}} {
				settings := `{"` + tc.key + `":` + c.raw + `}`
				input := NativePolicyInput{Widget: tc.widget, ElementorVersion: "4.3.4", Settings: json.RawMessage(settings), Schema: json.RawMessage(`{"widget":"` + tc.widget + `","atomic":false,"controls":{"` + tc.key + `":` + tc.control + `}}`)}
				result := CheckNativeSettings(input)
				_, code := validatePageEditInput(nux1Edit(settings))
				if got := code == "" && result.OK; got != c.want {
					t.Errorf("%s accepted=%v want=%v precheck=%s policy=%+v", settings, got, c.want, code, result.Failures)
				}
			}
		})
	}
}

func TestNUX1GroupActivation(t *testing.T) {
	schema := nativeTestValue(t, json.RawMessage(`{"widget":"heading","atomic":false,"controls":{"shadow_toggle":{"type":"popover_toggle","return_value":"yes"},"shadow":{"type":"box_shadow","group":"box_shadow","activator":{"key":"shadow_toggle","values":["yes"]}}}}`))
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{"shadow_toggle":"yes","shadow":{"horizontal":"1","vertical":"1","blur":"1","spread":"0","color":"#ABCDEF"}}`, true},
		{`{"shadow_toggle":"","shadow":{"horizontal":"1","vertical":"1","blur":"1","spread":"0","color":"#ABCDEF"}}`, false},
	} {
		state := nativePolicyState{schema: schema, active: phpList{}}
		v := nativeTestValue(t, json.RawMessage(tc.raw))
		state.validateWritten(phpObject{}, v, nativeMerge(phpObject{}, v))
		if got := len(state.failures) == 0; got != tc.want {
			t.Errorf("accepted=%v want=%v failures=%+v", got, tc.want, state.failures)
		}
	}
}

func TestNUX1TypographyGroupActivation(t *testing.T) {
	schema := json.RawMessage(`{"widget":"button","atomic":false,"controls":{"typography_typography":{"type":"popover_toggle","return_value":"custom","group":"typography","activator":{"key":"typography_typography","values":["custom"]}},"typography_font_size":{"type":"slider","units":["px"],"group":"typography","activator":{"key":"typography_typography","values":["custom"]}}}}`)
	for _, tc := range []struct {
		name, before, settings string
		want                   bool
	}{
		{"same edit", `{}`, `{"typography_typography":"custom","typography_font_size":{"unit":"px","size":"20"}}`, true},
		{"already on widget", `{"typography_typography":"custom"}`, `{"typography_font_size":{"unit":"px","size":"20"}}`, true},
		{"group off", `{"typography_typography":""}`, `{"typography_font_size":{"unit":"px","size":"20"}}`, false},
		{"turn group off", `{"typography_typography":"custom"}`, `{"typography_typography":"","typography_font_size":{"unit":"px","size":"20"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := CheckNativeSettings(NativePolicyInput{Widget: "button", ElementorVersion: "4.3.4", Schema: schema, Before: json.RawMessage(tc.before), Settings: json.RawMessage(tc.settings)})
			if r.OK != tc.want {
				t.Fatalf("accepted=%v want=%v failures=%+v", r.OK, tc.want, r.Failures)
			}
		})
	}
}

func TestNUX1ShadowValidationLayers(t *testing.T) {
	control, _ := nativeMap(nativeTestValue(t, json.RawMessage(`{"type":"box_shadow"}`)))
	state := nativePolicyState{}
	for _, tc := range []struct {
		offset string
		leafOK bool
	}{{"1", true}, {"001", false}, {"-01", false}} {
		v := nativeTestValue(t, json.RawMessage(`{"horizontal":"`+tc.offset+`","vertical":"0","blur":"0","spread":"0","color":"#ABCDEF"}`))
		if why := state.valueProblem(control, v, true, false); why != "" {
			t.Fatalf("offset %s: shadow shape refused: %s", tc.offset, why)
		}
		why := nativeLeafProblem(v, "box_shadow", false, "shadow", 0)
		if got := why == ""; got != tc.leafOK {
			t.Fatalf("offset %s: numeric leaf accepted=%v want=%v reason=%s", tc.offset, got, tc.leafOK, why)
		}
		t.Logf("offset %q: shadow shape accepted; numeric leaf accepted=%v", tc.offset, why == "")
	}
}
