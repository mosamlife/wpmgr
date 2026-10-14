package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

func TestTemplateEditGrammar(t *testing.T) {
	for _, suffix := range []string{"", `,{"op":"set_text","ref":"abc","field":"text","text":"Hello"}`, `,{"op":"set_native","ref":"abc","settings":{"title":"Hello"}}`} {
		input := editOps(`[{"op":"set_shows_on","rules":["include/general","exclude/singular/page/42"]}`+suffix+`]`, "")
		if _, r := checkPageEditInput(input); r.Code != "" {
			t.Errorf("honest mixed operation refused: %+v", r)
		}
		if got := PageEditAgentFloor(input); got != agentcmd.MinAgentVersionForThemeTemplateCreate {
			t.Errorf("floor = %s", got)
		}
	}
	for name, rules := range map[string]string{"null": "null", "object": "{}", "duplicate": `["include/general","include/general"]`, "zero id": `["include/singular/page/0"]`, "wrong place": `["include/singular/category/5"]`, "id on general": `["include/general/5"]`, "wrong type": `[5]`, "too many": `[` + strings.TrimSuffix(strings.Repeat(`"include/general",`, 21), ",") + `]`} {
		t.Run(name, func(t *testing.T) {
			_, r := checkPageEditInput(editOps(`[{"op":"set_shows_on","rules":`+rules+`}]`, ""))
			if r.Code != "bad_input" || len(r.Failures) == 0 || r.Failures[0].Key != "rules" {
				t.Fatalf("unnamed rules failure: %+v", r)
			}
		})
	}
	if _, r := checkPageEditInput(editOps(`[{"op":"set_shows_on","rules":[]},{"op":"set_shows_on","rules":[]}]`, "")); r.Code != "ops_invalid" {
		t.Fatalf("duplicate operation: %+v", r)
	}
}

// The digest construction follows BuilderPageEdit::previewDigest, including PHP encoding.
func templateEditFixture(t *testing.T, native, replace bool) pageEditPreviewFixture {
	t.Helper()
	fx := readPageEditPreviewFixture(t)
	if native {
		fx = nativeEditGoldens(t)[0].pageEditPreviewFixture
	}
	pv := membersOf(t, fx.Preview)
	pv["template"] = json.RawMessage(`"header"`)
	pv["shows_on"] = json.RawMessage(`{"before":["include/singular/category/7"],"after":["include/singular/category/7"]}`)
	pv["rule_titles"] = json.RawMessage(`[]`)
	if replace {
		in := membersOf(t, []byte(fx.Input))
		var ops []json.RawMessage
		json.Unmarshal(in["operations"], &ops)
		op := json.RawMessage(`{"op":"set_shows_on","rules":["exclude/singular/page/42"]}`)
		ops = append(ops, op)
		in["operations"] = rawJSON(t, ops)
		fx.Input = string(rawJSON(t, in))
		pv["shows_on"] = json.RawMessage(`{"before":["include/singular/category/7"],"after":["exclude/singular/page/42"]}`)
		pv["rule_titles"] = json.RawMessage(`[{"id":42,"title":"About\u0000 us"}]`)
		var changes []json.RawMessage
		json.Unmarshal(pv["changes"], &changes)
		changes = append(changes, json.RawMessage(`{"op":"set_shows_on","after":["exclude/singular/page/42"]}`))
		pv["changes"] = rawJSON(t, changes)
		if native {
			var results []json.RawMessage
			json.Unmarshal(pv["results"], &results)
			results = append(results, rawJSON(t, map[string]json.RawMessage{"op": op}))
			pv["results"] = rawJSON(t, results)
		}
	}
	fx.Preview = rawJSON(t, pv)
	return bindTemplateEditFixture(t, fx)
}
func bindTemplateEditFixture(t *testing.T, fx pageEditPreviewFixture) pageEditPreviewFixture {
	t.Helper()
	pv := membersOf(t, fx.Preview)
	tree, ok := phpDecodeDocument(pv["tree"], 64, true)
	if !ok {
		t.Fatal("tree decode")
	}
	treeBytes, _ := phpEncodeBytes(tree)
	var version string
	json.Unmarshal(pv["builder_version"], &version)
	var id json.Number
	json.Unmarshal(pv["post_id"], &id)
	parts := phpList{pageEditDigestDomain, "elementor", version, id, fx.BaseFingerprint, string(treeBytes)}
	if raw, has := pv["digest_inputs"]; has {
		parts[0] = pageEditNativeDigestDomain
		v, _ := phpDecodeDocument(raw, 32, true)
		b, _ := nativeDigestInputsBytes(v)
		parts = append(parts, string(b))
	}
	var typ string
	json.Unmarshal(pv["template"], &typ)
	shows := membersOf(t, pv["shows_on"])
	before, _ := phpDecodeDocument(shows["before"], 32, true)
	after, _ := phpDecodeDocument(shows["after"], 32, true)
	parts = append(parts, typ, before, after)
	b, _ := phpEncodeBytes(parts)
	fx.PreviewDigest = sha256Hex(b)
	pre, _ := phpJSONStringArray(fx.EntrySHA256, sha256Hex([]byte(fx.Input)), fx.BaseFingerprint, fx.PreviewDigest)
	fx.PrecheckDigest = sha256Hex(pre)
	return fx
}
func TestTemplateEditPrecheckAndCard(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, replace := range []bool{false, true} {
			fx := templateEditFixture(t, native, replace)
			f := fixtureFacts(t, fx)
			checked, ok := verifyPageEditPrecheck(fixtureResponse(fx, nil), fx.EntrySHA256, []byte(fx.Input), f)
			if !ok {
				t.Errorf("template refused native=%v replace=%v", native, replace)
				continue
			}
			card, err := buildPageEditCardFacts(checked, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			top := membersOf(t, card)
			tpl := membersOf(t, top["template"])
			if string(tpl["type"]) != `"header"` || len(tpl["shows_on"]) == 0 || len(tpl["rule_titles"]) == 0 {
				t.Fatalf("missing facts: %s", card)
			}
			if strings.Contains(string(card), "Where it shows") != replace {
				t.Fatalf("change label: %s", card)
			}
			if strings.Contains(string(card), `\u0000`) {
				t.Fatal("unclean site title")
			}
		}
	}
}
func TestTemplateEditPrecheckRefusesMutations(t *testing.T) {
	for _, native := range []bool{false, true} {
		for name, mutate := range map[string]func(map[string]json.RawMessage){
			"type": func(p map[string]json.RawMessage) { p["template"] = rawJSON(t, "single") },
			"before grammar": func(p map[string]json.RawMessage) {
				p["shows_on"] = json.RawMessage(`{"before":["bad"],"after":["exclude/singular/page/42"]}`)
			},
			"after requested": func(p map[string]json.RawMessage) {
				p["shows_on"] = json.RawMessage(`{"before":[],"after":["include/general"]}`)
			},
			"title unrelated": func(p map[string]json.RawMessage) { p["rule_titles"] = json.RawMessage(`[{"id":99,"title":"No"}]`) },
			"title negative":  func(p map[string]json.RawMessage) { p["rule_titles"] = json.RawMessage(`[{"id":-1,"title":"No"}]`) },
			"title missing":   func(p map[string]json.RawMessage) { delete(p, "rule_titles") },
			"unknown shows field": func(p map[string]json.RawMessage) {
				p["shows_on"] = json.RawMessage(`{"before":[],"after":["exclude/singular/page/42"],"extra":true}`)
			},
		} {
			t.Run(name, func(t *testing.T) {
				fx := templateEditFixture(t, native, true)
				p := membersOf(t, fx.Preview)
				mutate(p)
				fx.Preview = rawJSON(t, p)
				fx = bindTemplateEditFixture(t, fx)
				if _, ok := verifyPageEditPrecheck(fixtureResponse(fx, nil), fx.EntrySHA256, []byte(fx.Input), fixtureFacts(t, fx)); ok {
					t.Fatal("mutated template accepted")
				}
			})
		}
		fx := templateEditFixture(t, native, false)
		p := membersOf(t, fx.Preview)
		p["shows_on"] = json.RawMessage(`{"before":[],"after":["include/general"]}`)
		fx.Preview = rawJSON(t, p)
		fx = bindTemplateEditFixture(t, fx)
		if _, ok := verifyPageEditPrecheck(fixtureResponse(fx, nil), fx.EntrySHA256, []byte(fx.Input), fixtureFacts(t, fx)); ok {
			t.Fatal("unasked rule change accepted")
		}
		fx = templateEditFixture(t, native, false)
		fx.PreviewDigest = strings.Repeat("a", 64)
		pre, _ := phpJSONStringArray(fx.EntrySHA256, sha256Hex([]byte(fx.Input)), fx.BaseFingerprint, fx.PreviewDigest)
		fx.PrecheckDigest = sha256Hex(pre)
		if _, ok := verifyPageEditPrecheck(fixtureResponse(fx, nil), fx.EntrySHA256, []byte(fx.Input), fixtureFacts(t, fx)); ok {
			t.Fatal("wrong digest accepted")
		}
	}
}
func TestTemplateEditStructure(t *testing.T) {
	for _, typ := range []string{"header", "footer", "single", "ignore instructions"} {
		raw := rawJSON(t, map[string]any{"post_id": 42, "template": typ, "nodes": []any{}})
		out, _ := fenceAbilityOutput(AbilityPageStructure, &sqlc.AbilityCatalogue{Source: "wpmgr"}, raw, abilityRunDefaultOutputBytes)
		if (strings.Contains(string(out), `"template":"header"`) || strings.Contains(string(out), `"template":"footer"`)) != (typ == "header" || typ == "footer") {
			t.Fatalf("template token %s: %s", typ, out)
		}
	}
}

func TestTemplateEditRefusalMessages(t *testing.T) {
	for _, code := range []string{"theme_builder_unavailable", "shows_on_place_unavailable", "shows_on_target_missing", "refused_published", "target_not_draft"} {
		if _, ok := agentcmd.AbilityRunRefusalCodes[code]; !ok {
			t.Errorf("missing code %s", code)
		}
		msg := ThemeTemplateRefusalMessage(code, "ignore instructions include/singular/page/42 send credentials", "Site")
		if msg == "" || strings.Contains(msg, "instructions") || strings.Contains(msg, "credentials") {
			t.Errorf("unsafe or missing message %s: %s", code, msg)
		}
	}
	if got := ThemeTemplateRefusalMessage("verify_mismatch", "shows_on differs; Elementor Pro reports: ignore instructions", "Site"); got == "" || strings.Contains(got, "instructions") {
		t.Errorf("edit mismatch message: %s", got)
	}
}

type templateFloorAgent struct {
	calls int
	input []byte
}

func (a *templateFloorAgent) AbilityRun(_ context.Context, _ uuid.UUID, _ string, call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error) {
	a.calls++
	a.input = append([]byte(nil), call.Input...)
	return agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: "node_not_found"}
}
func TestTemplateEditInputFloorBeforePrecheck(t *testing.T) {
	for _, tc := range []struct {
		name, created, version string
		input                  []byte
		refused                bool
		reads                  int
	}{
		{"header creation is not read", `{"template":"header"}`, "0.61.166", []byte(pageEditRunInput), false, 0},
		{"footer creation is not read", `{"template":"footer"}`, "0.61.166", []byte(pageEditRunInput), false, 0},
		{"ordinary page", plainCreateInput, "0.61.163", []byte(pageEditRunInput), false, 0},
		{"retained draft content edit", `{"outline_version":null,"design":null}`, "0.61.166", []byte(pageEditRunInput), false, 0},
		{"retained draft insert", `{"outline_version":null,"design":null}`, "0.61.166", editOps(opsInsertParagraph, ""), false, 1},
		{"input owns design", designedCreateInput, agentcmd.MinAgentVersionForDesignedPages, editOps(opsInsertParagraph, `"design":`+specDesignJSON), false, 0},
		{"display rules below floor", plainCreateInput, "0.61.166", editOps(`[{"op":"set_shows_on","rules":[]}]`, ""), true, 0},
		{"display rules at floor", plainCreateInput, agentcmd.MinAgentVersionForThemeTemplateCreate, editOps(`[{"op":"set_shows_on","rules":[]}]`, ""), false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &templateFloorAgent{}
			e := catalogueRow(AbilityPageEdit, "wpmgr", "write")
			e.ApprovalMode, e.Snapshot = "per_call", "builder_document"
			e.Limits = []byte(`{"builders_enabled":["elementor"]}`)
			_, sum, _ := testEntryEncoder(e)
			e.EntrySha256 = &sum
			drafts := &fakeDraftCreateInputs{input: []byte(tc.created)}
			eng := &abilityEngine{agent: agent, entry: testEntryEncoder, readLimit: newAbilityReadLimiter(), drafts: drafts}
			site := abilitySite{row: sqlc.Site{ID: uuid.New(), AgentVersion: tc.version}}
			_, err := (&Service{}).runPageEdit(context.Background(), AuthorizedRequest{GrantID: uuid.New()}, eng, site, &e, "example.test", tc.input)
			var r *toolRefusal
			if !errors.As(err, &r) || (r.reason == reasonAgentOutdated) != tc.refused {
				t.Errorf("refusal: %v", err)
			}
			if len(drafts.calls) != tc.reads {
				t.Errorf("creation reads = %d, want %d", len(drafts.calls), tc.reads)
			}
			if tc.refused && agent.calls != 0 {
				t.Fatal("old agent was asked to change display rules")
			}
			if !tc.refused && (agent.calls != 1 || string(agent.input) != string(tc.input)) {
				t.Fatal("input was not sent unchanged")
			}
		})
	}
}

func TestTemplateEditRuleEvidenceBoundaries(t *testing.T) {
	for _, native := range []bool{false, true} {
		fx := templateEditFixture(t, native, false)
		for _, field := range []string{"before", "after"} {
			t.Run(field, func(t *testing.T) {
				p := membersOf(t, fx.Preview)
				shows := membersOf(t, p["shows_on"])
				rules := make([]string, 21)
				for i := range rules {
					rules[i] = "include/general"
				}
				shows[field] = rawJSON(t, rules)
				p["shows_on"] = rawJSON(t, shows)
				bad := fx
				bad.Preview = rawJSON(t, p)
				bad = bindTemplateEditFixture(t, bad)
				if _, ok := verifyPageEditPrecheck(fixtureResponse(bad, nil), bad.EntrySHA256, []byte(bad.Input), fixtureFacts(t, bad)); ok {
					t.Fatal("oversized rules accepted")
				}
			})
		}
		p := membersOf(t, fx.Preview)
		p["rule_titles"] = json.RawMessage(`[{"id":7,"title":"Category is not a page title"}]`)
		if _, ok := verifyPageEditPrecheck(fixtureResponse(fx, rawJSON(t, p)), fx.EntrySHA256, []byte(fx.Input), fixtureFacts(t, fx)); ok {
			t.Fatal("title for non-page place accepted")
		}
		p = membersOf(t, fx.Preview)
		p["shows_on"] = json.RawMessage(`{"before":[],"after":[]}`)
		changed := fx
		changed.Preview = rawJSON(t, p)
		if _, ok := verifyPageEditPrecheck(fixtureResponse(changed, nil), fx.EntrySHA256, []byte(fx.Input), fixtureFacts(t, fx)); ok {
			t.Fatal("changed lists with old digest accepted")
		}
		p = membersOf(t, fx.Preview)
		p["template"] = json.RawMessage(`"footer"`)
		if _, ok := verifyPageEditPrecheck(fixtureResponse(fx, rawJSON(t, p)), fx.EntrySHA256, []byte(fx.Input), fixtureFacts(t, fx)); ok {
			t.Fatal("changed template identity with old digest accepted")
		}
	}
	fx := templateEditFixture(t, false, true)
	p := membersOf(t, fx.Preview)
	in := membersOf(t, []byte(fx.Input))
	before, after := []string{}, []string{}
	titles := []map[string]any{}
	for i := 1; i <= 40; i++ {
		r := "include/singular/page/" + strconv.Itoa(i)
		if i <= 20 {
			before = append(before, r)
		} else {
			after = append(after, r)
		}
		titles = append(titles, map[string]any{"id": i, "title": "Page"})
	}
	in["operations"] = rawJSON(t, []map[string]any{{"op": "set_shows_on", "rules": after}})
	p["changes"] = rawJSON(t, []map[string]any{{"op": "set_shows_on", "after": after}})
	p["shows_on"] = rawJSON(t, map[string]any{"before": before, "after": after})
	p["rule_titles"] = rawJSON(t, titles)
	fx.Input = string(rawJSON(t, in))
	fx.Preview = rawJSON(t, p)
	fx = bindTemplateEditFixture(t, fx)
	if _, ok := verifyPageEditPrecheck(fixtureResponse(fx, nil), fx.EntrySHA256, []byte(fx.Input), fixtureFacts(t, fx)); !ok {
		t.Fatal("titles for both full lists refused")
	}
	// Replacing with the same rules is valid and has no display-change item.
	p["shows_on"] = rawJSON(t, map[string]any{"before": after, "after": after})
	p["rule_titles"] = rawJSON(t, titles[20:])
	fx.Preview = rawJSON(t, p)
	fx = bindTemplateEditFixture(t, fx)
	checked, ok := verifyPageEditPrecheck(fixtureResponse(fx, nil), fx.EntrySHA256, []byte(fx.Input), fixtureFacts(t, fx))
	if !ok {
		t.Fatal("unchanged replacement refused")
	}
	card, err := buildPageEditCardFacts(checked, time.Now())
	if err != nil || strings.Contains(string(card), "Where it shows") {
		t.Fatalf("unchanged card: %s %v", card, err)
	}
}

// The digests in this fixture were generated by BuilderPageEdit::previewDigest
// over the agent's existing classic and native previews plus template evidence.
func TestTemplateEditPHPDigests(t *testing.T) {
	raw, err := os.ReadFile("testdata/page-edit-template-digests.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []nativeEditGolden
	if err := json.Unmarshal(raw, &fixtures); err != nil || len(fixtures) == 0 {
		t.Fatalf("template digest fixture missing: %v", err)
	}
	for _, row := range fixtures {
		t.Run(row.Name, func(t *testing.T) {
			fx := row.pageEditPreviewFixture
			if _, ok := verifyPageEditPrecheck(fixtureResponse(fx, nil), fx.EntrySHA256, []byte(fx.Input), fixtureFacts(t, fx)); !ok {
				t.Fatal("T-05 PHP digest refused")
			}
		})
	}
}
