package mcp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPageInputFieldsRefusalFallback(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		required     []string
	}{
		{"required fields", "send only op and ref; op and ref are required", []string{"op", "ref"}},
		{"no required fields", "send only op and ref", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := pageInputFieldsRefusal([]byte(`{"op":"remove","ref":"a"}`), "operations[2]", []string{"op", "ref"}, tc.required...)
			if failure.Key == "" || failure.Reason == "" {
				t.Fatal("describer returned an empty failure for an otherwise valid object")
			}
			if failure.Path != "operations[2]" || failure.Key != "input" || failure.Reason != tc.reason {
				t.Errorf("fallback %+v; want operations[2].input: %s", failure, tc.reason)
			}
		})
	}
}

func TestPageEditOperationMemberRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, raw, missing string
		allowed            []string
	}{
		{"set_text", `{"op":"set_text","ref":"a","field":"text","text":"Hello"}`, "ref", []string{"op", "ref", "field", "text"}},
		{"insert after", `{"op":"insert","after":"a","outline":[{"type":"paragraph","text":"Hello"}]}`, "outline", []string{"op", "after", "outline"}},
		{"insert into", `{"op":"insert","into":"a","position":"first","outline":[{"type":"paragraph","text":"Hello"}]}`, "outline", []string{"op", "into", "position", "outline"}},
		{"replace", `{"op":"replace","ref":"a","outline":[{"type":"paragraph","text":"Hello"}]}`, "outline", []string{"op", "ref", "outline"}},
		{"remove", `{"op":"remove","ref":"a"}`, "ref", []string{"op", "ref"}},
		{"set_native", `{"op":"set_native","ref":"a","settings":{}}`, "settings", []string{"op", "ref", "settings"}},
		{"move after", `{"op":"move","ref":"a","after":"b"}`, "ref", []string{"op", "after", "ref"}},
		{"move into", `{"op":"move","ref":"a","into":"b","position":"last"}`, "ref", []string{"op", "into", "position", "ref"}},
		{"duplicate after", `{"op":"duplicate","ref":"a","after":"b"}`, "ref", []string{"op", "after", "ref"}},
		{"duplicate into", `{"op":"duplicate","ref":"a","into":"b","position":"first"}`, "ref", []string{"op", "into", "position", "ref"}},
		{"add_native after", `{"op":"add_native","after":"a","widget":"heading","settings":{}}`, "settings", []string{"op", "after", "widget", "settings"}},
		{"add_native into", `{"op":"add_native","into":"a","position":"last","widget":"heading","settings":{}}`, "settings", []string{"op", "into", "position", "widget", "settings"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("accepted", func(t *testing.T) {
				op, failure := checkPageEditOperation([]byte(tc.raw), "operations[0]")
				if failure != nil || op.Op == "" {
					t.Fatalf("honest operation refused: op=%+v failure=%+v", op, failure)
				}
				if _, refusal := checkPageEditInput(editOps("["+tc.raw+"]", "")); refusal.Code != "" {
					t.Fatalf("honest input refused: %+v", refusal)
				}
			})
			for _, mutation := range []string{"unknown", "missing"} {
				t.Run(mutation, func(t *testing.T) {
					var members map[string]json.RawMessage
					if err := json.Unmarshal([]byte(tc.raw), &members); err != nil {
						t.Fatal(err)
					}
					key, reason := "extra", "unknown field: send only "+pageInputFieldList(tc.allowed)
					if mutation == "unknown" {
						members[key] = json.RawMessage(`true`)
					} else {
						key, reason = tc.missing, "is required"
						delete(members, key)
					}
					raw, err := json.Marshal(members)
					if err != nil {
						t.Fatal(err)
					}
					op, failure := checkPageEditOperation(raw, "operations[0]")
					if failure == nil {
						t.Fatalf("refused input accepted: op=%+v zero=%v", op, reflect.DeepEqual(op, pageEditOp{}))
					}
					if failure.Key != key || failure.Reason != reason || failure.Path != "operations[0]" {
						t.Errorf("operation failure %+v; want %s: %s", failure, key, reason)
					}
					_, refusal := checkPageEditInput(editOps("["+string(raw)+"]", ""))
					assertPageInputFailure(t, refusal, pageEditRefusalOf(refusal), "operations[0]", key, reason)
				})
			}
		})
	}
}

func assertPageInputFailure(t *testing.T, refusal pageCreateRefusal, ref *toolRefusal, path, key, reason string) {
	t.Helper()
	if refusal.Code != pageCreateBadInput || len(refusal.Failures) != 1 {
		t.Fatalf("got %+v; want bad_input with one failure at %s.%s", refusal, path, key)
	}
	f := refusal.Failures[0]
	if f.Path != path || f.Key != key || f.Reason != reason {
		t.Errorf("failure %+v; want path %q key %q reason %q", f, path, key, reason)
	}
	where := key
	if path != "" {
		where = path + "." + key
	}
	if !strings.Contains(ref.err.Message, where+": "+reason) || strings.Contains(ref.err.Message, "page design has a value") {
		t.Errorf("message %q; want field and repair without a design summary", ref.err.Message)
	}
	d := refusalDetails(t, ref)
	failures, _ := d["failures"].([]refusalFailure)
	if len(failures) != 1 || failures[0].Path != path || failures[0].Key != key || failures[0].Reason != reason {
		t.Errorf("details failures = %+v", d["failures"])
	}
	if schema, _ := d["schema"].(json.RawMessage); len(schema) == 0 || d["retryable"] != false {
		t.Errorf("bad_input lost its schema or retryable false: %+v", d)
	}
}

func TestPageCreateNamedInputFailures(t *testing.T) {
	const formatReason = "a designed page (outline_version 3) is built as classic widgets: send elementor_format classic"
	cases := []struct {
		name, key, reason string
		input             []byte
	}{
		{"unknown field", "extra", "unknown field: send only post_type, editor, title, outline, elementor_format, outline_version, design and page_frame", v3HeadInput(map[string]string{"extra": `true`})},
		{"post type", "post_type", "must be page or post", v3HeadInput(map[string]string{"post_type": `"other"`})},
		{"editor type", "editor", "must be wordpress_blocks, wordpress_classic or builder:elementor", v3HeadInput(map[string]string{"editor": `null`})},
		{"editor value", "editor", "must be wordpress_blocks, wordpress_classic or builder:elementor", v3HeadInput(map[string]string{"editor": `"other"`})},
		{"format value", "elementor_format", "must be site_default, classic or atomic, only with editor builder:elementor", v3HeadInput(map[string]string{"elementor_format": `"other"`})},
		{"format editor", "elementor_format", "must be site_default, classic or atomic, only with editor builder:elementor", v3HeadInput(map[string]string{"editor": `"wordpress_blocks"`})},
		{"version", "outline_version", "must be the integer 3 or left out", v3HeadInput(map[string]string{"outline_version": `3.0`})},
		{"designed format omitted", "elementor_format", formatReason, v3HeadInput(nil, "elementor_format")},
		{"designed format atomic", "elementor_format", formatReason, v3HeadInput(map[string]string{"elementor_format": `"atomic"`})},
		{"designed format site default", "elementor_format", formatReason, v3HeadInput(map[string]string{"elementor_format": `"site_default"`})},
		{"designed editor", "elementor_format", formatReason, v3HeadInput(map[string]string{"editor": `"wordpress_blocks"`}, "elementor_format")},
		{"design without version", "design", "is accepted only with outline_version 3", v3HeadInput(nil, "outline_version")},
		{"frame without version", "page_frame", "is accepted only with outline_version 3", v3HeadInput(map[string]string{"page_frame": `"theme"`}, "outline_version", "design")},
		{"title", "title", "must be a nonempty string containing more than whitespace", v3HeadInput(map[string]string{"title": `" "`})},
		{"frame", "page_frame", "must be theme, full_width or canvas", v3HeadInput(map[string]string{"page_frame": `"other"`})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, refusal := checkPageCreateInput(tc.input)
			assertPageInputFailure(t, refusal, pageCreateInputRefusal(refusal), "", tc.key, tc.reason)
		})
	}
}

func TestPageEditNamedInputFailures(t *testing.T) {
	cases := []struct {
		name, key, reason string
		input             []byte
	}{
		{"unknown field", "extra", "unknown field: send only post_id, base_fingerprint, operations and design", editOps(opsSetText, `"extra":true`)},
		{"missing field", "post_id", "is required", []byte(`{"base_fingerprint":"` + targetTestFingerprint + `","operations":` + opsSetText + `}`)},
		{"not object", "input", "must be a JSON object", []byte(`[]`)},
		{"post id", "post_id", "must be a positive integer", []byte(strings.Replace(string(editOps(opsSetText, "")), `"post_id":418`, `"post_id":0`, 1))},
		{"fingerprint", "base_fingerprint", "must be a string of 64 lowercase hexadecimal characters from wpmgr/page-structure", []byte(strings.Replace(string(editOps(opsSetText, "")), targetTestFingerprint, "wrong", 1))},
		{"operations type", "operations", "must be a list of 1 to 25 operations", editOps(`{}`, "")},
		{"operations empty", "operations", "must be a list of 1 to 25 operations", editOps(`[]`, "")},
		{"operations limit", "operations", "must be a list of 1 to 25 operations", editOps(`[`+strings.TrimSuffix(strings.Repeat(`{"op":"remove","ref":"a"},`, pageEditMaxOps+1), ",")+`]`, "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, refusal := checkPageEditInput(tc.input)
			assertPageInputFailure(t, refusal, pageEditRefusalOf(refusal), "", tc.key, tc.reason)
		})
	}

	for _, tc := range []struct{ name, op, key, reason string }{
		{"not object", `null`, "input", "must be a JSON object"},
		{"op type", `{"op":null}`, "op", "must be set_text, insert, replace, remove, set_native, move, duplicate or add_native"},
		{"op value", `{"op":"other"}`, "op", "must be set_text, insert, replace, remove, set_native, move, duplicate or add_native"},
		{"unknown field", `{"op":"remove","ref":"a","extra":true}`, "extra", "unknown field: send only op and ref"},
		{"missing field", `{"op":"remove"}`, "ref", "is required"},
		{"ref", `{"op":"remove","ref":"bad ref"}`, "ref", "must be a node ref of 1 to 32 letters, digits, underscores or hyphens from wpmgr/page-structure"},
		{"field type", `{"op":"set_text","ref":"a","field":null,"text":"x"}`, "field", "must be text, alt, caption, url or a pattern field path"},
		{"field value", `{"op":"set_text","ref":"a","field":"other","text":"x"}`, "field", "must be text, alt, caption, url or a pattern field path"},
		{"text type", `{"op":"set_text","ref":"a","field":"text","text":null}`, "text", "must be a string of at most 5000 characters"},
		{"text content", `{"op":"set_text","ref":"a","field":"text","text":"<b>x</b>"}`, "text", "must follow the plain text, length and link rules for the selected field"},
		{"anchor absent", `{"op":"insert","outline":[]}`, "after", "send exactly one of after, before or into"},
		{"anchors conflict", `{"op":"move","ref":"a","after":"b","into":"c"}`, "after", "send exactly one of after, before or into"},
		{"anchor ref", `{"op":"insert","after":"bad ref","outline":[]}`, "after", "must be a node ref of 1 to 32 letters, digits, underscores or hyphens from wpmgr/page-structure"},
		{"position placement", `{"op":"insert","after":"a","position":"first","outline":[]}`, "position", "is accepted only with into"},
		{"position value", `{"op":"insert","into":"a","position":"other","outline":[]}`, "position", "must be first or last"},
		{"outline", `{"op":"replace","ref":"a","outline":[]}`, "outline", "must be a list of 1 to 50 nodes following the page outline grammar"},
		{"widget", `{"op":"add_native","after":"a","widget":"bad widget","settings":{}}`, "widget", "must be a widget name starting with a lowercase letter, followed by up to 63 lowercase letters, digits, underscores or hyphens"},
		{"settings", `{"op":"set_native","ref":"a","settings":"other"}`, "settings", "must be an object of native settings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Earlier honest operations make the failing operation's index observable.
			in := editOps(`[{"op":"remove","ref":"prior1"},{"op":"remove","ref":"prior2"},`+tc.op+`]`, "")
			_, refusal := checkPageEditInput(in)
			assertPageInputFailure(t, refusal, pageEditRefusalOf(refusal), "operations[2]", tc.key, tc.reason)
		})
	}
}

func TestPageInputUnknownFieldNames(t *testing.T) {
	for _, key := range []string{"safe_Key-1", "bad\nkey", "bad.key", "<script>", "", strings.Repeat("x", 65)} {
		encoded, _ := json.Marshal(key)
		for _, edit := range []bool{false, true} {
			name := "create/" + key
			in := []byte(`{` + string(encoded) + `:true,"post_type":"page"}`)
			build := pageCreateInputRefusal
			var refusal pageCreateRefusal
			if edit {
				name = "edit/" + key
				in = []byte(`{` + string(encoded) + `:true,"post_id":1,"base_fingerprint":"` + targetTestFingerprint + `","operations":` + opsSetText + `}`)
				_, refusal = checkPageEditInput(in)
				build = pageEditRefusalOf
			} else {
				_, refusal = checkPageCreateInput(in)
			}
			t.Run(name, func(t *testing.T) {
				if len(refusal.Failures) != 1 {
					t.Fatalf("want one failure, got %+v", refusal)
				}
				want := "fields[0]"
				if key == "safe_Key-1" {
					want = key
				}
				if refusal.Failures[0].Key != want {
					t.Errorf("key %q, want %q", refusal.Failures[0].Key, want)
				}
				ref := build(refusal)
				if key != "" && want != key && strings.Contains(ref.err.Message, key) {
					t.Errorf("unsafe key echoed in message %q", ref.err.Message)
				}
				if !strings.Contains(ref.err.Message, want+": unknown field:") {
					t.Errorf("message does not name unknown field: %q", ref.err.Message)
				}
			})
		}
	}

	t.Run("operation field position and wire", func(t *testing.T) {
		input := editOps(`[{"op":"remove","ref":"a"},{"op":"remove","ref":"b","bad.key":"private value"}]`, "")
		_, refusal := checkPageEditInput(input)
		ref := pageEditRefusalOf(refusal)
		assertPageInputFailure(t, refusal, ref, "operations[1]", "fields[2]", "unknown field: send only op and ref")
		wire, err := json.Marshal(toolErrorWire(t, ref))
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"bad.key", "private value"} {
			if carriesText(wire, forbidden) {
				t.Errorf("caller text %q echoed in refusal", forbidden)
			}
		}
	})
}
