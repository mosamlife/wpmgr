package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const nativeCatalogue = `{"builder":"elementor","builder_version":"3.35.9","truncated":false,"widgets":[{"name":"heading","title":"site title","categories":["site category"],"pro":false,"atomic":false,"usable":true}]}`
const nativeSkeleton = `{"builder":"elementor","builder_version":"3.35.9","truncated":false,"widget":"heading","atomic":false,"usable":true,"mode":"skeleton","tabs":[{"id":"content","sections":[{"id":"section_title","controls":[{"id":"title","type":"text"}]}]}]}`
const nativeControls = `{"builder":"elementor","builder_version":"3.35.9","truncated":false,"widget":"heading","atomic":false,"usable":true,"mode":"controls","controls":{"title":{"type":"text","options":["left"],"default":"site default","responsive":null,"group":null}}}`

func TestNativeReadShapes(t *testing.T) {
	for _, c := range []struct {
		label, name, raw string
		text             []string
	}{
		{"catalogue", "wpmgr/elementor-widgets", nativeCatalogue, []string{"site title", "site category", "3.35.9"}},
		{"skeleton", "wpmgr/elementor-widget-schema", nativeSkeleton, []string{"3.35.9"}},
		{"controls", "wpmgr/elementor-widget-schema", nativeControls, []string{"site default", "3.35.9"}},
	} {
		t.Run(c.label, func(t *testing.T) {
			out, bad := fenceAbilityOutput(c.name, nil, json.RawMessage(c.raw), 1<<18)
			if bad || string(out) == "null" {
				t.Errorf("valid read withheld: %s", out)
			}
			for _, s := range c.text {
				if !strings.Contains(string(out), siteTextMarker+s) {
					t.Errorf("unfenced text %q: %s", s, out)
				}
			}
			for label, raw := range map[string]string{
				"extra":         strings.Replace(c.raw, `"builder":`, `"extra":true,"builder":`, 1),
				"missing":       strings.Replace(c.raw, `"truncated":false,`, "", 1),
				"bad name":      strings.Replace(c.raw, `"heading"`, `"bad name"`, 1),
				"oversized":     strings.Replace(c.raw, `"3.35.9"`, `"`+strings.Repeat("a", 32769)+`"`, 1),
				"trailing JSON": c.raw + ` {}`,
				"null boolean":  strings.Replace(c.raw, `"truncated":false`, `"truncated":null`, 1),
			} {
				t.Run(label, func(t *testing.T) {
					out, bad := fenceAbilityOutput(c.name, nil, json.RawMessage(raw), 1<<18)
					if !bad || string(out) != "null" {
						t.Errorf("malformed read accepted: %s", out)
					}
				})
			}
		})
	}
	out, bad := fenceAbilityOutput("wpmgr/elementor-widgets", nil, json.RawMessage(strings.Replace(nativeCatalogue, `"heading"`, `"Odd_Widget"`, 1)), 1<<18)
	if bad || !strings.Contains(string(out), `"usable":false`) {
		t.Errorf("nonclassic name not unusable: %s", out)
	}
}
func TestNativeReadInputs(t *testing.T) {
	for _, c := range []struct {
		name, in string
		bad      bool
	}{
		{"wpmgr/elementor-widgets", `{}`, false}, {"wpmgr/elementor-widgets", `{"category":"basic","search":"title"}`, false},
		{"wpmgr/elementor-widgets", `{"category":"bad name"}`, true}, {"wpmgr/elementor-widgets", `{"search":"` + strings.Repeat("x", 129) + `"}`, true},
		{"wpmgr/elementor-widget-schema", `{"widget":"heading"}`, false}, {"wpmgr/elementor-widget-schema", `{"widget":"heading","controls":["title"]}`, false},
		{"wpmgr/elementor-widget-schema", `{"widget":"heading","controls":["title","title"]}`, true}, {"wpmgr/elementor-widget-schema", `{}`, true},
		{"wpmgr/elementor-widget-schema", `{"widget":"bad name"}`, true}, {"wpmgr/elementor-widget-schema", `{"widget":"heading","mode":"controls"}`, true},
	} {
		if got := validateOwnInput(c.name, []byte(c.in)); got != c.bad {
			t.Errorf("%s %s: bad=%v want %v", c.name, c.in, got, c.bad)
		}
	}
	for _, name := range []string{"wpmgr/elementor-widgets", "wpmgr/elementor-widget-schema"} {
		if len(ownAbilityInputSchemas[name]) == 0 {
			t.Errorf("missing input schema: %s", name)
		}
	}
}
func TestNativeProjection(t *testing.T) {
	good := `{"nodes":[{"ref":"a1b2c3d","parent":"root","kind":"native","widget":"heading","summary":"site summary"}]}`
	out, bad := fenceAbilityOutput(AbilityPageStructure, nil, json.RawMessage(good), 1<<18)
	if bad || !strings.Contains(string(out), `"widget":"heading"`) || !strings.Contains(string(out), siteTextMarker+"site summary") {
		t.Errorf("native node missing or unfenced: %s", out)
	}
	for label, raw := range map[string]string{"bad widget": strings.Replace(good, `"heading"`, `"Odd_Widget"`, 1), "extra": strings.Replace(good, `"summary":`, `"settings":{},"summary":`, 1), "missing": strings.Replace(good, `,"summary":"site summary"`, "", 1), "long summary": strings.Replace(good, "site summary", strings.Repeat("界", 121), 1)} {
		t.Run(label, func(t *testing.T) {
			out, bad := fenceAbilityOutput(AbilityPageStructure, nil, json.RawMessage(raw), 1<<18)
			if !bad || string(out) != "null" {
				t.Errorf("malformed native node accepted: %s", out)
			}
		})
	}
	legacy := `{"nodes":[{"ref":"a1b2c3d","parent":"root","kind":"heading","from_the_site":{"text":"old"}}]}`
	out, bad = fenceAbilityOutput(AbilityPageStructure, nil, json.RawMessage(legacy), 1<<18)
	want := `{"nodes":[{"from_the_site":{"text":"` + siteTextMarker + `old"},"kind":"heading","parent":"root","ref":"a1b2c3d"}]}`
	if bad || string(out) != want {
		t.Errorf("legacy projection changed: %s != %s", out, want)
	}
}
func TestNativeFloors(t *testing.T) {
	for _, op := range []string{"add_native", "set_native", "duplicate"} {
		in := []byte(`{"design":{},"operations":[{"op":"` + op + `"}]}`)
		if got := PageEditAgentFloor(in); got != "0.61.166" {
			t.Errorf("%s floor=%s", op, got)
		}
		if pageEditFloorRefusal("0.61.165", in) == nil {
			t.Errorf("%s ran below floor", op)
		}
		if pageEditFloorRefusal("0.61.166", in) != nil {
			t.Errorf("%s refused at floor", op)
		}
	}
	if got := PageEditAgentFloor([]byte(`{"operations":[{"op":"set_text"}]}`)); got != agentcmd.MinAgentVersionForBuilderEdit {
		t.Errorf("legacy floor=%s", got)
	}
}
func TestNativeRefusalHints(t *testing.T) {
	for _, code := range []string{"widget_registry_unreadable", "widget_not_found", "atomic_not_supported", "widget_not_usable", "widget_schema_unreadable", "control_not_found", "native_settings_invalid", "native_designed_node"} {
		if _, ok := agentcmd.AbilityRunRefusalCodes[code]; !ok {
			t.Errorf("code not admitted: %s", code)
		}
		if precheckRefusalHint(code) == "" {
			t.Errorf("code has no hint: %s", code)
		}
	}
	for _, word := range []string{"wpmgr/elementor-widget-schema", "controls", "stored"} {
		if !strings.Contains(precheckRefusalHint("native_settings_invalid"), word) {
			t.Errorf("native hint lacks %s", word)
		}
	}
}
func TestNativeReadsRun(t *testing.T) {
	for _, c := range []struct{ name, raw string }{{"wpmgr/elementor-widgets", nativeCatalogue}, {"wpmgr/elementor-widget-schema", nativeControls}} {
		for _, scenario := range []string{"valid", "malformed", "oversized", "old agent", "smaller entry limit"} {
			t.Run(c.name+"/"+scenario, func(t *testing.T) {
				f := newAbilityFixture(t)
				f.store.sites[0].AgentVersion = "0.61.166"
				row := catalogueRow(c.name, "wpmgr", "read")
				row.Limits = []byte(`{"max_output_bytes":32768}`)
				if scenario == "smaller entry limit" {
					row.Limits = []byte(`{"max_output_bytes":64}`)
				}
				f.ab.cat = append(f.ab.cat, row)
				f.ab.rows = append(f.ab.rows, sqlc.SiteAbilityInventory{SiteID: f.siteID, Name: c.name, OwnerKind: "plugin"})
				f.agent.out = c.raw
				if scenario == "old agent" {
					f.store.sites[0].AgentVersion = "0.61.165"
				}
				if scenario == "malformed" {
					f.agent.out = strings.Replace(c.raw, `"builder":`, `"extra":true,"builder":`, 1)
				}
				if scenario == "oversized" {
					f.agent.out = strings.Replace(c.raw, `"3.35.9"`, `"`+strings.Repeat("a", 32769)+`"`, 1)
				}
				input := map[string]any{}
				if c.name == "wpmgr/elementor-widget-schema" {
					input = map[string]any{"widget": "heading", "controls": []string{"title"}}
				}
				w := post(t, f.router(t, &capturingRecorder{}), callBody(ToolSiteAbilityRun, map[string]any{"site_id": f.siteID, "name": c.name, "input": input}), nil)
				rpc := decodeRPC(t, w)
				if scenario == "valid" {
					if rpc.Error != nil {
						t.Fatalf("valid read refused: %s", w.Body.String())
					}
					if !strings.Contains(toolText(t, w.Body.String()), siteTextMarker) {
						t.Error("unfenced site text")
					}
				} else if rpc.Error == nil {
					t.Errorf("%s read accepted: %s", scenario, w.Body.String())
				}
				if scenario == "old agent" && f.agent.calls != 0 {
					t.Error("old agent called")
				}
			})
		}
	}
}

func TestNativeControlDetails(t *testing.T) {
	var v map[string]any
	if err := json.Unmarshal([]byte(nativeControls), &v); err != nil {
		t.Fatal(err)
	}
	controls := v["controls"].(map[string]any)
	control := controls["title"].(map[string]any)
	control["responsive"] = map[string]any{"devices": []any{"desktop", "mobile"}, "min": nil, "max": "desktop"}
	control["group"] = map[string]any{"family": "typography", "prefix": "typography_", "activator": "typography_typography"}
	control["condition"] = map[string]any{"align!": "site condition"}
	control["conditions"] = map[string]any{"terms": []any{map[string]any{"name": "title", "value": "site term"}}}
	control["return_value"] = "site return"
	control["size_units"] = []any{"px"}
	control["range"] = map[string]any{"px": map[string]any{"min": 0, "max": 100}}
	control["multiple"] = true
	control["default"] = map[string]any{"url": "site url", "rows": []any{map[string]any{"caption": "site caption"}}}
	control["fields"] = map[string]any{"caption": map[string]any{"type": "text", "options": []any{}, "default": "site field", "responsive": nil, "group": nil}}
	encode := func() json.RawMessage {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	out, bad := fenceAbilityOutput("wpmgr/elementor-widget-schema", nil, encode(), 1<<18)
	if bad {
		t.Fatalf("valid detailed controls refused: %s", out)
	}
	for _, s := range []string{"site condition", "site term", "site return", "site url", "site caption", "site field", "px"} {
		if !strings.Contains(string(out), siteTextMarker+s) {
			t.Errorf("unfenced nested text %s: %s", s, out)
		}
	}
	for _, c := range []struct {
		label   string
		plant   func()
		restore func()
	}{
		{"extra control member", func() { control["unknown"] = true }, func() { delete(control, "unknown") }},
		{"missing control member", func() { delete(control, "type") }, func() { control["type"] = "text" }},
		{"bad control id", func() { controls["bad id"] = control }, func() { delete(controls, "bad id") }},
		{"bad option key", func() { control["options"] = []any{"bad<key"} }, func() { control["options"] = []any{"left"} }},
		{"extra responsive member", func() { control["responsive"].(map[string]any)["extra"] = true }, func() { delete(control["responsive"].(map[string]any), "extra") }},
		{"missing group member", func() { delete(control["group"].(map[string]any), "activator") }, func() { control["group"].(map[string]any)["activator"] = "typography_typography" }},
		{"bad repeater id", func() { control["fields"].(map[string]any)["bad id"] = control["fields"].(map[string]any)["caption"] }, func() { delete(control["fields"].(map[string]any), "bad id") }},
		{"both modes", func() { v["tabs"] = []any{} }, func() { delete(v, "tabs") }},
	} {
		t.Run(c.label, func(t *testing.T) {
			c.plant()
			defer c.restore()
			out, bad := fenceAbilityOutput("wpmgr/elementor-widget-schema", nil, encode(), 1<<18)
			if !bad || string(out) != "null" {
				t.Fatalf("malformed controls accepted: %s", out)
			}
		})
	}
	for _, raw := range []string{strings.Replace(nativeControls, `"truncated":false`, `"truncated":true,"truncated":false`, 1), strings.Replace(nativeControls, `"title":{"type"`, `"title":{"type":"text","type"`, 1)} {
		out, bad := fenceAbilityOutput("wpmgr/elementor-widget-schema", nil, json.RawMessage(raw), 1<<18)
		if !bad || string(out) != "null" {
			t.Fatalf("duplicate members accepted: %s", out)
		}
	}
}

func TestNativeRunFloorBeforeInputCheck(t *testing.T) {
	for _, op := range []string{"add_native", "set_native", "duplicate"} {
		site := abilitySite{row: sqlc.Site{AgentVersion: "0.61.165"}}
		_, err := (&Service{}).runPageEditChecked(context.Background(), AuthorizedRequest{}, nil, site, nil, "", []byte(`{"operations":[{"op":"`+op+`"}]}`))
		var ref *toolRefusal
		if !errors.As(err, &ref) || ref.reason != reasonAgentOutdated {
			t.Errorf("%s did not enforce floor at run: %v", op, err)
		}
	}
}

func TestNativeAgentCodesHaveHints(t *testing.T) {
	patterns := regexp.MustCompile(`(?:refuse\(|CODE\s*=\s*)'([a-z_]+)'`)
	for _, file := range []string{"class-elementor-widget-discovery.php", "class-native-settings-validator.php", "class-native-settings-json.php"} {
		raw, err := os.ReadFile("../../../agent/includes/abilities/builders/" + file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range patterns.FindAllStringSubmatch(string(raw), -1) {
			code := m[1]
			if _, ok := agentcmd.AbilityRunRefusalCodes[code]; !ok {
				t.Errorf("agent code not admitted: %s", code)
			}
			if precheckRefusalHint(code) == "" {
				t.Errorf("agent code has no hint: %s", code)
			}
		}
	}
}

type nativeReadRefusingAgent struct{ code string }

func (a nativeReadRefusingAgent) AbilityRun(context.Context, uuid.UUID, string, agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error) {
	return agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: a.code, Detail: "site secret", Retryable: false}
}
func TestNativeReadRefusalsUseHints(t *testing.T) {
	for _, code := range []string{"widget_not_found", "atomic_not_supported", "native_settings_invalid"} {
		f := newAbilityFixture(t)
		f.store.sites[0].AgentVersion = "0.61.166"
		f.ab.cat = append(f.ab.cat, catalogueRow("wpmgr/elementor-widgets", "wpmgr", "read"))
		f.ab.rows = append(f.ab.rows, sqlc.SiteAbilityInventory{SiteID: f.siteID, Name: "wpmgr/elementor-widgets", OwnerKind: "plugin"})
		r := f.routerWith(t, &capturingRecorder{}, func(s *Service) { s.abilities.agent = nativeReadRefusingAgent{code} })
		w := post(t, r, callBody(ToolSiteAbilityRun, map[string]any{"site_id": f.siteID, "name": "wpmgr/elementor-widgets", "input": map[string]any{}}), nil)
		rpc := decodeRPC(t, w)
		if rpc.Error == nil {
			t.Fatalf("refusal answered success: %s", w.Body.String())
		}
		if !strings.Contains(w.Body.String(), precheckRefusalHint(code)) || strings.Contains(w.Body.String(), "site secret") {
			t.Errorf("hint absent or site detail exposed: %s", w.Body.String())
		}
	}
}
func TestNativeStructureRunRefusesMalformed(t *testing.T) {
	for _, native := range []bool{true, false} {
		f := newAbilityFixture(t)
		f.store.sites[0].AgentVersion = "0.61.166"
		f.ab.cat = append(f.ab.cat, catalogueRow(AbilityPageStructure, "wpmgr", "read"))
		f.ab.rows = append(f.ab.rows, sqlc.SiteAbilityInventory{SiteID: f.siteID, Name: AbilityPageStructure, OwnerKind: "plugin"})
		f.agent.out = `{"nodes":[{"ref":"a1b2c3d","parent":"root","kind":"native","widget":"bad name","summary":"site summary"}]}`
		if !native {
			f.agent.out = `{"nodes":[],"status":"` + strings.Repeat("x", abilityRunDefaultOutputBytes) + `"}`
		}
		r := f.routerWith(t, &capturingRecorder{}, func(s *Service) { s.abilities.drafts = inputDraftStore{} })
		w := post(t, r, callBody(ToolSiteAbilityRun, map[string]any{"site_id": f.siteID, "name": AbilityPageStructure, "input": map[string]any{"post_id": 418}}), nil)
		rpc := decodeRPC(t, w)
		if native && rpc.Error == nil {
			t.Errorf("malformed native answer accepted: %s", w.Body.String())
		}
		if !native {
			if rpc.Error != nil {
				t.Fatalf("legacy oversized projection changed: %s", w.Body.String())
			}
			var res runResult
			if err := json.Unmarshal([]byte(toolText(t, w.Body.String())), &res); err != nil {
				t.Fatal(err)
			}
			if !res.Truncated || string(res.Output) != "null" {
				t.Errorf("legacy oversized result changed: %+v", res)
			}
		}
	}
}

func TestNativeReadGuidance(t *testing.T) {
	for _, policy := range abilityToolPolicies() {
		if policy.Name != ToolSiteAbilityRun {
			continue
		}
		text := policy.Description
		catalogue := strings.Index(text, "wpmgr/elementor-widgets")
		schema := strings.Index(text, "wpmgr/elementor-widget-schema")
		if catalogue < 0 || schema <= catalogue {
			t.Fatalf("tool guidance must name the catalogue before the schema: %s", text)
		}
		for _, word := range []string{"catalogue", "skeleton", "named controls", "stored shapes"} {
			if !strings.Contains(text, word) {
				t.Errorf("tool guidance lacks %s", word)
			}
		}
		return
	}
	t.Fatal("ability run tool is missing")
}

func TestElementorWidgetNamesAllowUnderscores(t *testing.T) {
	for _, c := range []struct{ name, raw string }{
		{AbilityElementorWidgets, strings.Replace(nativeCatalogue, `"heading"`, `"google_maps"`, 1)},
		{AbilityElementorWidgetSchema, strings.Replace(nativeControls, `"heading"`, `"google_maps"`, 1)},
		{AbilityPageStructure, `{"nodes":[{"ref":"a1b2c3d","parent":"root","kind":"native","widget":"google_maps","summary":"Map"}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, bad := fenceAbilityOutput(c.name, nil, json.RawMessage(c.raw), elementorReadMaxBytes)
			if bad || string(out) == "null" {
				t.Fatalf("google_maps refused: %s", out)
			}
			var top map[string]any
			if err := json.Unmarshal(out, &top); err != nil {
				t.Fatal(err)
			}
			if c.name == AbilityElementorWidgets {
				if top["widgets"].([]any)[0].(map[string]any)["usable"] != true {
					t.Errorf("google_maps catalogue entry must be usable: %s", out)
				}
			}
			if c.name == AbilityElementorWidgetSchema && top["usable"] != true {
				t.Errorf("google_maps schema must be usable: %s", out)
			}
		})
	}
}

func TestElementorOptionKeys(t *testing.T) {
	for _, c := range []struct {
		key, want string
		bad       bool
	}{
		{"", "", false}, {"400", "400", false}, {"center center", siteTextMarker + "center center", false}, {"h1", "h1", false},
		{"weight_1.min:50%/+extra-1", "weight_1.min:50%/+extra-1", false},
		{strings.Repeat("x", 64), strings.Repeat("x", 64), false},
		{"bad<key", "", true}, {" space", "", true}, {"center\ncenter", "", true}, {strings.Repeat("x", 65), "", true},
	} {
		t.Run(c.key, func(t *testing.T) {
			raw := strings.Replace(nativeControls, `["left"]`, string(mustElementorJSON(t, []string{c.key})), 1)
			out, bad := fenceElementorRead(AbilityElementorWidgetSchema, []byte(raw), elementorReadMaxBytes)
			if c.bad {
				if !bad || string(out) != "null" {
					t.Errorf("malformed option accepted: %s", out)
				}
				return
			}
			if bad || string(out) == "null" {
				t.Fatalf("honest option key refused: %q", c.key)
			}
			var top map[string]any
			if err := json.Unmarshal(out, &top); err != nil {
				t.Fatal(err)
			}
			got := top["controls"].(map[string]any)["title"].(map[string]any)["options"].([]any)[0]
			if got != c.want {
				t.Errorf("option projected as %q, want %q", got, c.want)
			}
		})
	}
}
func mustElementorJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestElementorWidgetSchemaAgentGolden(t *testing.T) {
	raw, err := os.ReadFile("../../../agent/tests/fixtures/ability-run/elementor-widget-schema-answer.json")
	if err != nil {
		t.Fatal(err)
	}
	out, bad := fenceElementorRead(AbilityElementorWidgetSchema, raw, elementorReadMaxBytes)
	if bad || string(out) == "null" {
		t.Fatalf("agent's fake heading schema refused: %s", out)
	}
	var top map[string]any
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatal(err)
	}
	if top["widget"] != "heading" || top["mode"] != "controls" {
		t.Fatalf("golden must be the heading controls answer: %s", out)
	}
	controls := top["controls"].(map[string]any)
	opts := controls["size"].(map[string]any)["options"].([]any)
	if len(opts) != 3 || opts[0] != "h1" || opts[1] != "h2" || opts[2] != "0" {
		t.Errorf("agent's size option keys changed: %v", opts)
	}
	opts = controls["typography_typography"].(map[string]any)["options"].([]any)
	if len(opts) != 2 || opts[0] != "" || opts[1] != "custom" {
		t.Errorf("agent's group activator keys changed: %v", opts)
	}
	if _, ok := controls["items"].(map[string]any)["fields"].(map[string]any)["link"]; !ok {
		t.Fatal("agent's repeater link field missing")
	}
}

func TestElementorOptionGuidance(t *testing.T) {
	const sentence = "Send an option shown as site-supplied text back without its marker."
	if !strings.Contains(abilityGuidanceNativeWidgets, sentence) {
		t.Errorf("option guidance is missing: %s", abilityGuidanceNativeWidgets)
	}
	_, answer := discoverGuidanceOf(t)
	answer.Abilities = []discoverAbility{}
	raw := mustElementorJSON(t, answer)
	if len(raw) > abilityDiscoverReservedBytes {
		t.Errorf("guidance exceeds discover's reserved bytes: %d > %d", len(raw), abilityDiscoverReservedBytes)
	}
}
