package mcp

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

const themeTemplatesRead = AbilityElementorThemeTemplates

func themeTemplatesFixture(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../../agent/tests/fixtures/ability-run/elementor-theme-templates-answer.json")
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func themeTemplate(v map[string]any) map[string]any {
	return v["templates"].([]any)[0].(map[string]any)
}
func themeMenu(v map[string]any) map[string]any  { return v["menus"].([]any)[0].(map[string]any) }
func themePlace(v map[string]any) map[string]any { return v["places"].([]any)[0].(map[string]any) }

func assertThemeGolden(t *testing.T, out json.RawMessage) {
	t.Helper()
	raw, err := os.ReadFile("testdata/elementor-theme-templates-fenced.json")
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("projection differs from fenced golden: %s", out)
	}
}

func TestThemeTemplatesReadGolden(t *testing.T) {
	out, bad := fenceAbilityOutput(themeTemplatesRead, nil, mustElementorJSON(t, themeTemplatesFixture(t)), elementorReadMaxBytes)
	if bad {
		t.Fatalf("agent fixture refused: %s", out)
	}
	assertThemeGolden(t, out)
}

func TestThemeTemplatesReadMalformed(t *testing.T) {
	objects := map[string]func(map[string]any) map[string]any{
		"top":      func(v map[string]any) map[string]any { return v },
		"pro":      func(v map[string]any) map[string]any { return v["pro"].(map[string]any) },
		"template": themeTemplate, "menu": themeMenu, "place": themePlace,
	}
	for label, object := range objects {
		t.Run(label, func(t *testing.T) {
			for key, value := range object(themeTemplatesFixture(t)) {
				if value != nil && !(label == "pro" && key == "version") && !(label == "menu" && key == "slug") && !(label == "template" && key == "shows_on") {
					t.Run("null/"+key, func(t *testing.T) {
						v := themeTemplatesFixture(t)
						object(v)[key] = nil
						assertThemeMalformed(t, mustElementorJSON(t, v))
					})
				}
				t.Run("missing/"+key, func(t *testing.T) {
					v := themeTemplatesFixture(t)
					delete(object(v), key)
					assertThemeMalformed(t, mustElementorJSON(t, v))
				})
				t.Run("wrong type/"+key, func(t *testing.T) {
					v := themeTemplatesFixture(t)
					replacement := any("wrong type")
					if _, ok := value.(string); ok {
						replacement = false
					}
					object(v)[key] = replacement
					assertThemeMalformed(t, mustElementorJSON(t, v))
				})
			}
			v := themeTemplatesFixture(t)
			object(v)["unknown"] = true
			assertThemeMalformed(t, mustElementorJSON(t, v))
		})
	}
	variants := map[string]func(map[string]any){
		"pro problem":    func(v map[string]any) { v["pro"].(map[string]any)["problem"] = "unknown" },
		"type":           func(v map[string]any) { themeTemplate(v)["type"] = "page" },
		"status":         func(v map[string]any) { themeTemplate(v)["status"] = "trash" },
		"shows problem":  func(v map[string]any) { v["templates"].([]any)[1].(map[string]any)["shows_on_problem"] = "unknown" },
		"both present":   func(v map[string]any) { themeTemplate(v)["shows_on_problem"] = "unreadable" },
		"both null":      func(v map[string]any) { themeTemplate(v)["shows_on"] = nil },
		"too many rules": func(v map[string]any) { themeTemplate(v)["shows_on"] = stringsToAny(21, "include/general") },
		"too many templates": func(v map[string]any) {
			rows := make([]any, 26)
			for i := range rows {
				rows[i] = themeTemplate(v)
			}
			v["templates"] = rows
			v["templates_total"] = 26
		},
		"too many menus": func(v map[string]any) {
			rows := make([]any, 26)
			for i := range rows {
				rows[i] = themeMenu(v)
			}
			v["menus"] = rows
			v["menus_total"] = 26
		},
		"templates exceed total": func(v map[string]any) { v["templates_total"] = 1 },
		"menus exceed total":     func(v map[string]any) { v["menus_total"] = 0 },
		"inactive builder":       func(v map[string]any) { v["pro"].(map[string]any)["active"] = false },
		"unavailable places":     func(v map[string]any) { v["pro"].(map[string]any)["theme_builder"] = false },
		"place rule":             func(v map[string]any) { themePlace(v)["rule"] = "singular/attachment" },
		"place takes id":         func(v map[string]any) { themePlace(v)["takes_id"] = true },
		"too many places":        func(v map[string]any) { v["places"] = append(v["places"].([]any), themePlace(v)) },
		"duplicate place":        func(v map[string]any) { v["places"] = []any{themePlace(v), themePlace(v)} },
		"missing takes id":       func(v map[string]any) { v["places"].([]any)[2].(map[string]any)["takes_id"] = false },
	}
	for label, mutate := range variants {
		t.Run(label, func(t *testing.T) {
			v := themeTemplatesFixture(t)
			mutate(v)
			assertThemeMalformed(t, mustElementorJSON(t, v))
		})
	}
	for _, c := range []struct {
		label, key string
		object     func(map[string]any) map[string]any
		positive   bool
	}{
		{"template id", "id", themeTemplate, true}, {"menu id", "id", themeMenu, true}, {"items", "items", themeMenu, false},
		{"templates total", "templates_total", objects["top"], false}, {"menus total", "menus_total", objects["top"], false},
	} {
		values := []any{-1, 1.5, nil, true, "1", json.Number("1e0"), json.Number("999999999999999999999999999")}
		if c.positive {
			values = append(values, 0)
		}
		for _, value := range values {
			t.Run(c.label+"/"+string(mustElementorJSON(t, value)), func(t *testing.T) {
				v := themeTemplatesFixture(t)
				c.object(v)[c.key] = value
				assertThemeMalformed(t, mustElementorJSON(t, v))
			})
		}
	}
	for _, rule := range []any{nil, false, "", "include", "include/", "include/General", "include/a b", "allow/general", "include/a/b/c/d", "include/singular/page/0", "include/singular/page/01", "include/singular/page/-1", "include/singular/page/123456789012345678901", "include/" + strings.Repeat("x", 65), "include/general\n"} {
		t.Run("rule/"+string(mustElementorJSON(t, rule)), func(t *testing.T) {
			v := themeTemplatesFixture(t)
			themeTemplate(v)["shows_on"] = []any{rule}
			assertThemeMalformed(t, mustElementorJSON(t, v))
		})
	}
	for _, version := range []string{"", "v3.2", "3 2", "3<script>", strings.Repeat("3", 33), "3.2\n"} {
		t.Run("version/"+version, func(t *testing.T) {
			v := themeTemplatesFixture(t)
			v["pro"].(map[string]any)["version"] = version
			assertThemeMalformed(t, mustElementorJSON(t, v))
		})
	}
	for _, slug := range []string{"", "Menu", "menu slug", "menu/slug", strings.Repeat("x", 201), "menu\n"} {
		t.Run("slug/"+slug, func(t *testing.T) {
			v := themeTemplatesFixture(t)
			themeMenu(v)["slug"] = slug
			assertThemeMalformed(t, mustElementorJSON(t, v))
		})
	}
	raw := mustElementorJSON(t, themeTemplatesFixture(t))
	for label, raw := range map[string][]byte{"null": []byte(`null`), "array": []byte(`[]`), "trailing": append(append([]byte{}, raw...), []byte(` {}`)...), "duplicate": []byte(strings.Replace(string(raw), `"pro":`, `"pro":null,"pro":`, 1)), "oversize": append(append([]byte{}, raw...), []byte(strings.Repeat(" ", elementorReadMaxBytes))...)} {
		t.Run(label, func(t *testing.T) { assertThemeMalformed(t, raw) })
	}
	for _, field := range []string{"templates", "menus", "places"} {
		v := themeTemplatesFixture(t)
		v[field] = []any{nil}
		assertThemeMalformed(t, mustElementorJSON(t, v))
	}
}
func stringsToAny(count int, s string) []any {
	out := make([]any, count)
	for i := range out {
		out[i] = s
	}
	return out
}
func assertThemeMalformed(t *testing.T, raw []byte) {
	t.Helper()
	out, bad := fenceAbilityOutput(themeTemplatesRead, nil, raw, elementorReadMaxBytes)
	if !bad || string(out) != "null" {
		t.Fatalf("malformed read accepted: bad=%v output=%s", bad, out)
	}
}

func TestThemeTemplatesReadHonestBoundaries(t *testing.T) {
	for _, status := range []string{"publish", "draft", "pending", "private", "future", "other"} {
		v := themeTemplatesFixture(t)
		themeTemplate(v)["status"] = status
		themeTemplate(v)["shows_on"] = stringsToAny(20, "include/singular/page/12345678901234567890")
		v["pro"].(map[string]any)["version"] = "3.35.1-beta+build_1"
		themeMenu(v)["slug"] = strings.Repeat("a", 200)
		themeMenu(v)["items"] = 0
		v["templates_total"] = 100
		v["menus_total"] = 100
		out, bad := fenceAbilityOutput(themeTemplatesRead, nil, mustElementorJSON(t, v), elementorReadMaxBytes)
		if bad || string(out) == "null" {
			t.Fatalf("honest status %s refused: %s", status, out)
		}
	}
	for _, problem := range []string{"pro_inactive", "theme_builder_unavailable"} {
		v := themeTemplatesFixture(t)
		pro := v["pro"].(map[string]any)
		pro["active"] = problem != "pro_inactive"
		pro["theme_builder"] = false
		pro["problem"] = problem
		pro["version"] = nil
		v["places"] = []any{}
		v["templates"] = []any{}
		v["templates_total"] = 0
		themeMenu(v)["slug"] = nil
		out, bad := fenceAbilityOutput(themeTemplatesRead, nil, mustElementorJSON(t, v), elementorReadMaxBytes)
		if bad || string(out) == "null" {
			t.Fatalf("honest unavailable state refused: %s", out)
		}
	}
	for _, problem := range []string{"pro_unavailable", "unreadable", "too_many"} {
		v := themeTemplatesFixture(t)
		themeTemplate(v)["shows_on"] = nil
		themeTemplate(v)["shows_on_problem"] = problem
		if out, bad := fenceAbilityOutput(themeTemplatesRead, nil, mustElementorJSON(t, v), elementorReadMaxBytes); bad || string(out) == "null" {
			t.Fatalf("honest shows_on problem refused: %s", out)
		}
	}
	v := themeTemplatesFixture(t)
	themeTemplate(v)["shows_on"] = []any{}
	if out, bad := fenceAbilityOutput(themeTemplatesRead, nil, mustElementorJSON(t, v), elementorReadMaxBytes); bad || string(out) == "null" {
		t.Fatalf("empty rules refused: %s", out)
	}
	for _, field := range []string{"templates", "menus"} {
		v := themeTemplatesFixture(t)
		row := v[field].([]any)[0]
		rows := make([]any, 25)
		for i := range rows {
			rows[i] = row
		}
		v[field], v[field+"_total"] = rows, 25
		if out, bad := fenceAbilityOutput(themeTemplatesRead, nil, mustElementorJSON(t, v), elementorReadMaxBytes); bad || string(out) == "null" {
			t.Fatalf("row boundary refused for %s: %s", field, out)
		}
	}
}

func TestThemeTemplatesReadByteLimits(t *testing.T) {
	v := themeTemplatesFixture(t)
	raw := mustElementorJSON(t, v)
	out, bad := fenceAbilityOutput(themeTemplatesRead, nil, raw, elementorReadMaxBytes)
	if bad {
		t.Fatal("valid fixture refused")
	}
	if _, bad := fenceAbilityOutput(themeTemplatesRead, nil, raw, len(out)); bad {
		t.Fatal("exact projected byte limit refused")
	}
	if out, bad := fenceAbilityOutput(themeTemplatesRead, nil, raw, len(out)-1); !bad || string(out) != "null" {
		t.Fatal("projection above byte limit accepted")
	}
	// Whitespace counts towards the wire limit even when projection is smaller.
	padded := append(append([]byte{}, raw...), []byte(strings.Repeat(" ", elementorReadMaxBytes-len(raw)))...)
	if _, bad := fenceAbilityOutput(themeTemplatesRead, nil, padded, elementorReadMaxBytes); bad {
		t.Fatal("exact wire byte limit refused")
	}
	assertThemeMalformed(t, append(padded, ' '))
}

func TestThemeTemplatesReadInputs(t *testing.T) {
	for _, input := range []string{`{}`, ` { } `, `null`, `[]`, `true`, `0`, `""`, `{"extra":true}`, `{"controls":[]}`, `{} {}`} {
		want := input != `{}` && input != ` { } `
		if bad := validateOwnInput(themeTemplatesRead, []byte(input)); bad != want {
			t.Errorf("input %s: bad=%v want %v", input, bad, want)
		}
	}
	if len(ownAbilityInputSchemas[themeTemplatesRead]) == 0 {
		t.Fatal("missing empty-object input schema")
	}
}

func TestThemeTemplatesReadRun(t *testing.T) {
	for _, scenario := range []string{"valid", "malformed", "oversized", "old agent", "bad input", "smaller entry limit"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAbilityFixture(t)
			f.store.sites[0].AgentVersion = "0.61.167"
			row := catalogueRow(themeTemplatesRead, "wpmgr", "read")
			row.Limits = []byte(`{"max_output_bytes":32768}`)
			if scenario == "smaller entry limit" {
				row.Limits = []byte(`{"max_output_bytes":64}`)
			}
			f.ab.cat = append(f.ab.cat, row)
			f.ab.rows = append(f.ab.rows, sqlc.SiteAbilityInventory{SiteID: f.siteID, Name: themeTemplatesRead, OwnerKind: "plugin"})
			v := themeTemplatesFixture(t)
			if scenario == "malformed" {
				v["unknown"] = true
			}
			if scenario == "oversized" {
				themeTemplate(v)["title"] = strings.Repeat("x", elementorReadMaxBytes)
			}
			f.agent.out = string(mustElementorJSON(t, v))
			if scenario == "old agent" {
				f.store.sites[0].AgentVersion = "0.61.166"
			}
			input := map[string]any{}
			if scenario == "bad input" {
				input["unknown"] = true
			}
			w := post(t, f.router(t, &capturingRecorder{}), callBody(ToolSiteAbilityRun, map[string]any{"site_id": f.siteID, "name": themeTemplatesRead, "input": input}), nil)
			rpc := decodeRPC(t, w)
			if scenario == "valid" {
				if rpc.Error != nil {
					t.Fatalf("valid read refused: %s", w.Body.String())
				}
				var res runResult
				if err := json.Unmarshal([]byte(toolText(t, w.Body.String())), &res); err != nil {
					t.Fatal(err)
				}
				assertThemeGolden(t, res.Output)
				if f.agent.calls != 1 || f.agent.last.Mode != agentcmd.AbilityRunModeRead || string(f.agent.last.Input) != "{}" {
					t.Fatalf("wrong dispatch: %+v", f.agent)
				}
			} else {
				if rpc.Error == nil {
					t.Fatalf("%s accepted: %s", scenario, w.Body.String())
				}
				if scenario == "old agent" || scenario == "bad input" {
					if f.agent.calls != 0 {
						t.Fatal("refused input dispatched")
					}
				}
				if scenario == "old agent" && (!strings.Contains(w.Body.String(), `"min_agent_version":"0.61.167"`) || !strings.Contains(w.Body.String(), "0.61.167 or later")) {
					t.Fatalf("wrong floor refusal: %s", w.Body.String())
				}
				if scenario == "malformed" || scenario == "oversized" || scenario == "smaller entry limit" {
					if !strings.Contains(w.Body.String(), "malformed_output") {
						t.Fatalf("wrong malformed refusal: %s", w.Body.String())
					}
				}
			}
		})
	}
}

func TestThemeTemplatesReadDiscoveryDescribe(t *testing.T) {
	for _, version := range []string{"0.61.166", "0.61.167"} {
		f := newAbilityFixture(t)
		f.store.sites[0].AgentVersion = version
		row := catalogueRow(themeTemplatesRead, "wpmgr", "read")
		usage := "Read templates and menus before drafting."
		row.Usage = &usage
		f.ab.cat = append(f.ab.cat, row)
		f.ab.rows = append(f.ab.rows, sqlc.SiteAbilityInventory{SiteID: f.siteID, Name: themeTemplatesRead, OwnerKind: "plugin"})
		r := f.router(t, &capturingRecorder{})
		w := post(t, r, callBody(ToolSiteAbilitiesDiscover, map[string]any{"site_id": f.siteID}), nil)
		if decodeRPC(t, w).Error != nil {
			t.Fatal(w.Body.String())
		}
		var discover discoverResult
		if err := json.Unmarshal([]byte(toolText(t, w.Body.String())), &discover); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, ability := range discover.Abilities {
			if ability.Name == themeTemplatesRead {
				found = true
				if ability.Title == nil || *ability.Title != row.Title || ability.RunnableHere != (version == "0.61.167") {
					t.Fatalf("wrong discovery: %+v", ability)
				}
			}
		}
		if !found {
			t.Fatal("read missing from discovery")
		}
		w = post(t, r, callBody(ToolSiteAbilityDescribe, map[string]any{"site_id": f.siteID, "name": themeTemplatesRead}), nil)
		if decodeRPC(t, w).Error != nil {
			t.Fatal(w.Body.String())
		}
		var describe describeResult
		if err := json.Unmarshal([]byte(toolText(t, w.Body.String())), &describe); err != nil {
			t.Fatal(err)
		}
		if describe.WPMgr == nil || describe.WPMgr.Description != row.Description || describe.WPMgr.Usage == nil || *describe.WPMgr.Usage != usage || describe.RunnableHere != (version == "0.61.167") {
			t.Fatalf("wrong description: %+v", describe)
		}
		var schema map[string]any
		if err := json.Unmarshal(describe.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		properties, ok := schema["properties"].(map[string]any)
		if schema["type"] != "object" || schema["additionalProperties"] != false || !ok || len(properties) != 0 {
			t.Fatalf("wrong input schema: %s", describe.InputSchema)
		}
	}
}
