package mcp

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

type nativeEditPolicyCases struct {
	Kinds   map[string]string `json:"kinds"`
	HTMLIDs []struct {
		Key    string `json:"key"`
		Denied bool   `json:"denied"`
	} `json:"html_ids"`
	Batches []struct {
		Name       string          `json:"name"`
		Operations json.RawMessage `json:"operations"`
		ParseCode  string          `json:"parse_code"`
		PageCode   string          `json:"page_code"`
	} `json:"batches"`
	Destinations []struct {
		Label  string `json:"label"`
		Denied bool   `json:"denied"`
	} `json:"destinations"`
}

func loadNativeEditPolicyCases(t *testing.T) nativeEditPolicyCases {
	t.Helper()
	raw, err := os.ReadFile(agentAbilityFixtures + "native-edit-policy-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases nativeEditPolicyCases
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases.HTMLIDs) == 0 || len(cases.Batches) == 0 || len(cases.Destinations) == 0 {
		t.Fatal("shared native edit policy cases missing")
	}
	return cases
}

func TestNativeEditSharedHTMLIDs(t *testing.T) {
	for _, c := range loadNativeEditPolicyCases(t).HTMLIDs {
		t.Run(c.Key, func(t *testing.T) {
			settings := phpObject{{c.Key, "Value"}}
			node := phpObject{{"id", "aaaaaaa"}, {"elType", "widget"}, {"widgetType", "button"}, {"settings", settings}, {"elements", phpList{}}}
			if safe := nativeCopySafe(node, "3.35.9"); safe == c.Denied {
				t.Errorf("duplicate safe=%v, denied=%v", safe, c.Denied)
			}
			for _, before := range []json.RawMessage{json.RawMessage(`[]`), json.RawMessage(`{"text":"Original"}`)} {
				written, _ := phpEncodeBytes(settings)
				schema, _ := phpEncodeBytes(phpObject{{"widget", "button"}, {"atomic", false}, {"controls", phpObject{{c.Key, phpObject{{"type", "text"}}}}}})
				got := CheckNativeSettings(NativePolicyInput{Widget: "button", ElementorVersion: "3.35.9", Schema: schema, Before: before, Settings: written})
				if got.OK == c.Denied {
					t.Errorf("write ok=%v, denied=%v: %+v", got.OK, c.Denied, got.Failures)
				}
			}
			if c.Denied {
				before, _ := phpEncodeBytes(phpObject{{"text", "Original"}, {c.Key, "existing"}})
				schema := json.RawMessage(`{"widget":"button","atomic":false,"controls":{"text":{"type":"text"}}}`)
				got := CheckNativeSettings(NativePolicyInput{Widget: "button", ElementorVersion: "3.35.9", Schema: schema, Before: before, Settings: json.RawMessage(`{"text":"Updated"}`)})
				if !got.OK || nativeGet(nativeTestValue(t, got.Settings), c.Key) != "existing" {
					t.Fatalf("carried HTML id changed or refused: %+v", got)
				}
			}
		})
	}
}

func TestNativeEditSharedMixedBatches(t *testing.T) {
	cases := loadNativeEditPolicyCases(t)
	tree := nativeTestValue(t, json.RawMessage(`[{"id":"1111111","elType":"container","settings":[],"elements":[{"id":"aaaaaaa","elType":"widget","widgetType":"counter","settings":{"title":"Counter"},"elements":[]},{"id":"bbbbbbb","elType":"widget","widgetType":"heading","settings":{"title":"Heading"},"elements":[]}]},{"id":"2222222","elType":"container","settings":[],"elements":[]}]`))
	index, ok := indexTree(tree)
	if !ok {
		t.Fatal("invalid shared batch tree")
	}
	kinds, ok := nativeEditOperationKinds(tree, index, "3.35.9")
	if !ok || !reflect.DeepEqual(kinds, cases.Kinds) {
		t.Fatalf("source kinds=%v, want %v", kinds, cases.Kinds)
	}
	for _, c := range cases.Batches {
		t.Run(c.Name, func(t *testing.T) {
			input := []byte(`{"post_id":41,"base_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","operations":` + string(c.Operations) + `}`)
			facts, code := validatePageEditInput(input)
			if code != c.ParseCode {
				t.Fatalf("code=%q, want %q", code, c.ParseCode)
			}
			if code == "" && pageEditMixedOperations(facts.ops, kinds) != (c.PageCode == pageEditOpsInvalid) {
				t.Fatal("page-dependent batch classification differs")
			}
			if code == pageEditOpsInvalid {
				_, refusal := pageEditPreInputRefusal(input)
				if refusal == nil || refusal.err.Details["hint"] != pageEditMixedHint {
					t.Fatalf("missing mixed batch hint: %+v", refusal)
				}
			}
		})
	}
}

func TestNativeEditSharedDesignedDestinations(t *testing.T) {
	for _, c := range loadNativeEditPolicyCases(t).Destinations {
		for _, descendant := range []bool{false, true} {
			for _, name := range []string{"add_native", "duplicate", "move"} {
				for _, anchor := range []string{"into", "before", "after"} {
					t.Run(c.Label+"/"+name+"/"+anchor+map[bool]string{true: "/descendant", false: "/direct"}[descendant], func(t *testing.T) {
						leaf := phpObject{{"id", "ccccccc"}, {"elType", "widget"}, {"widgetType", "heading"}, {"settings", phpObject{{"title", "Destination"}}}, {"elements", phpList{}}}
						child := phpObject{{"id", "3333333"}, {"elType", "container"}, {"settings", phpList{}}, {"elements", phpList{leaf}}}
						host := phpObject{{"id", "2222222"}, {"elType", "container"}, {"settings", phpObject{{"_title", c.Label}}}, {"elements", phpList{child}}}
						tree := phpList{host}
						index, ok := indexTree(tree)
						if !ok {
							t.Fatal("invalid test tree")
						}
						into, beside := "2222222", "3333333"
						if descendant {
							into, beside = "3333333", "ccccccc"
						}
						ref := beside
						if anchor == "into" {
							ref = into
						}
						node := phpObject{{"id", "aaaaaaa"}, {"elType", "widget"}, {"widgetType", "counter"}, {"settings", phpObject{{"title", "Counter"}}}, {"elements", phpList{}}}
						_, ok = nativePlaceNode(tree, pageEditOp{Op: name, Anchor: anchor, AnchorRef: ref}, node, index, name != "move")
						if ok == c.Denied {
							t.Fatalf("placement ok=%v, denied=%v", ok, c.Denied)
						}
					})
				}
			}
		}
	}
}

func nativePolicyTestOutline(t *testing.T, tree any, version string) json.RawMessage {
	t.Helper()
	proj, err := elementorProject(tree)
	if err != nil {
		t.Fatal(err)
	}
	answer, ok := proj.answer(pageStructureMaxNodes, pageStructureMaxBytes)
	if !ok {
		t.Fatal("outline failed")
	}
	decoded, ok := projectionJSON(answer)
	if !ok {
		t.Fatal("outline decode failed")
	}
	outline := decoded.(map[string]any)
	index, _ := indexTree(tree)
	for i, n := range proj.nodes {
		if n.locked() && n.label != elementorLabelDynamic {
			e := index[n.ref]
			if nativeCopySafe(e.node, version) {
				outline["nodes"].([]any)[i] = map[string]any{"ref": n.ref, "parent": n.parent, "kind": "native", "widget": nativeGet(e.node, "widgetType"), "summary": "Counter"}
			}
		}
	}
	return mustNativeJSON(t, outline)
}

func TestNativeEditDesignedDestinationReboundAnswers(t *testing.T) {
	for _, name := range []string{"add_native", "duplicate", "move"} {
		for _, descendant := range []bool{false, true} {
			for _, c := range loadNativeEditPolicyCases(t).Destinations {
				t.Run(name+"/"+c.Label+map[bool]string{true: "/descendant", false: "/direct"}[descendant], func(t *testing.T) {
					fx := nativeGoldenByName(t, name)
					var pv pageEditNativePreview
					if err := json.Unmarshal(fx.Preview, &pv); err != nil {
						t.Fatal(err)
					}
					before, _ := phpDecodeDocument(pv.BeforeTree, pageEditTreeMaxDepth, true)
					// A separate destination keeps the source outside the destination's ancestry.
					destination := phpObject{{"id", "ddddddd"}, {"elType", "container"}, {"settings", phpObject{{"_title", c.Label}}}, {"elements", phpList{}}}
					hostID := "ddddddd"
					if descendant {
						destination = nativeSet(destination, "elements", phpList{phpObject{{"id", "eeeeeee"}, {"elType", "container"}, {"settings", phpList{}}, {"elements", phpList{}}}})
						hostID = "eeeeeee"
					}
					before = append(before.(phpList), destination)
					facts := fixtureFacts(t, fx)
					op := facts.ops[0]
					op.Anchor, op.AnchorRef, op.Position = "into", hostID, ""
					op.Parsed = nil
					operation := nativeParsedOperation(op)
					input := phpObject{{"post_id", json.Number("41")}, {"base_fingerprint", fx.BaseFingerprint}, {"operations", phpList{operation}}}
					raw, _ := phpEncodeBytes(input)
					fx.Input = string(raw)
					facts = fixtureFacts(t, fx)
					var results []pageEditNativeResult
					if err := json.Unmarshal(pv.Results, &results); err != nil {
						t.Fatal(err)
					}
					results[0].Op, _ = phpEncodeBytes(operation)
					index, _ := indexTree(before)
					var node any
					edited := before
					switch name {
					case "add_native":
						node, _ = phpDecodeDocument(results[0].ResultNode, 32, true)
					case "duplicate":
						i := 0
						node = nativeCopyIDs(index[op.Ref].node, results[0].NewIDs, &i)
					case "move":
						source := index[op.Ref]
						node = source.node
						var ok bool
						edited, ok = nativeSplice(edited, source.path[:len(source.path)-1], source.path[len(source.path)-1], 1, nil)
						if !ok {
							t.Fatal("remove failed")
						}
					}
					index, _ = indexTree(edited)
					var ok bool
					edited, ok = nativeSplice(edited, index[hostID].path, 0, 0, phpList{node})
					if !ok {
						t.Fatal("insert failed")
					}
					pv.BeforeTree, _ = phpEncodeBytes(before)
					pv.BeforeTreeSHA256 = sha256Hex(pv.BeforeTree)
					pv.Tree, _ = phpEncodeBytes(edited)
					pv.Results = mustNativeJSON(t, results)
					inputs, _ := phpDecodeDocument(pv.DigestInputs, 32, true)
					inputs = nativeSet(inputs.(phpObject), "before_tree_sha256", pv.BeforeTreeSHA256)
					pv.DigestInputs, _ = nativeDigestInputsBytes(inputs)
					pv.AfterOutline = nativePolicyTestOutline(t, edited, pv.BuilderVersion)
					resp := nativeGoldenResponse(t, fx, pv, true)
					_, ok = verifyPageEditPrecheck(resp, fx.EntrySHA256, []byte(fx.Input), facts)
					if ok == c.Denied {
						t.Fatalf("consistent answer accepted=%v, denied=%v", ok, c.Denied)
					}
				})
			}
		}
	}
}

func TestNativeEditMixedRemoveMoveReboundAnswer(t *testing.T) {
	fx := nativeGoldenByName(t, "move")
	var pv pageEditNativePreview
	if err := json.Unmarshal(fx.Preview, &pv); err != nil {
		t.Fatal(err)
	}
	facts := fixtureFacts(t, fx)
	remove := pageEditOp{Op: "remove", Ref: "52982f9"}
	op := nativeParsedOperation(facts.ops[0])
	input := phpObject{{"post_id", json.Number("41")}, {"base_fingerprint", fx.BaseFingerprint}, {"operations", phpList{nativeParsedOperation(remove), op}}}
	raw, _ := phpEncodeBytes(input)
	fx.Input = string(raw)
	facts = fixtureFacts(t, fx)
	var results []pageEditNativeResult
	if err := json.Unmarshal(pv.Results, &results); err != nil {
		t.Fatal(err)
	}
	removeRaw, _ := phpEncodeBytes(nativeParsedOperation(remove))
	results = append([]pageEditNativeResult{{Op: removeRaw}}, results...)
	pv.Results = mustNativeJSON(t, results)
	tree, _ := phpDecodeDocument(pv.Tree, pageEditTreeMaxDepth, true)
	index, _ := indexTree(tree)
	source := index[remove.Ref]
	tree, ok := nativeSplice(tree, source.path[:len(source.path)-1], source.path[len(source.path)-1], 1, nil)
	if !ok {
		t.Fatal("remove failed")
	}
	pv.Tree, _ = phpEncodeBytes(tree)
	pv.AfterOutline = nativePolicyTestOutline(t, tree, pv.BuilderVersion)
	resp := nativeGoldenResponse(t, fx, pv, true)
	if _, ok := verifyPageEditPrecheck(resp, fx.EntrySHA256, []byte(fx.Input), facts); ok {
		t.Fatal("mixed classified remove and native move accepted")
	}
}

func TestNativeEditHTMLIDReboundAnswer(t *testing.T) {
	fx := nativeGoldenByName(t, "add_native")
	var pv pageEditNativePreview
	if err := json.Unmarshal(fx.Preview, &pv); err != nil {
		t.Fatal(err)
	}
	facts := fixtureFacts(t, fx)
	op := facts.ops[0]
	written, _ := phpDecodeDocument(op.Settings, 32, true)
	written = nativeSet(written.(phpObject), "button_css_id", "anchor")
	op.Settings, _ = phpEncodeBytes(written)
	op.Parsed = nil
	operation := nativeParsedOperation(op)
	input := phpObject{{"post_id", json.Number("41")}, {"base_fingerprint", fx.BaseFingerprint}, {"operations", phpList{operation}}}
	raw, _ := phpEncodeBytes(input)
	fx.Input = string(raw)
	facts = fixtureFacts(t, fx)
	var results []pageEditNativeResult
	if err := json.Unmarshal(pv.Results, &results); err != nil {
		t.Fatal(err)
	}
	node, _ := phpDecodeDocument(results[0].ResultNode, 32, true)
	node = nativeSet(node.(phpObject), "settings", written)
	results[0].Op, _ = phpEncodeBytes(operation)
	results[0].ResultNode, _ = phpEncodeBytes(node)
	pv.Results = mustNativeJSON(t, results)
	tree, _ := phpDecodeDocument(pv.Tree, pageEditTreeMaxDepth, true)
	index, _ := indexTree(tree)
	tree, ok := nativeReplaceNode(tree, index[results[0].NewIDs[0]].path, node)
	if !ok {
		t.Fatal("replace failed")
	}
	pv.Tree, _ = phpEncodeBytes(tree)
	pv.AfterOutline = nativePolicyTestOutline(t, tree, pv.BuilderVersion)
	resp := nativeGoldenResponse(t, fx, pv, true)
	if _, ok := verifyPageEditPrecheck(resp, fx.EntrySHA256, []byte(fx.Input), facts); ok {
		t.Fatal("AI HTML id accepted")
	}
}

func TestNativeEditMixedBatchSiteHint(t *testing.T) {
	refusal := sitePrecheckRefusal(AbilityPageEdit, &agentcmd.AbilityRunRefusal{Code: pageEditOpsInvalid, Detail: pageEditMixedHint, Retryable: true})
	if refusal.err.Details["hint"] != pageEditMixedHint || refusal.err.Details["retryable"] != false {
		t.Fatalf("mixed batch hint: %+v", refusal.err.Details)
	}
	ordinary := sitePrecheckRefusal(AbilityPageEdit, &agentcmd.AbilityRunRefusal{Code: pageEditOpsInvalid, Detail: "untrusted text"})
	if ordinary.err.Details["hint"] == pageEditMixedHint {
		t.Fatal("untrusted detail selected the mixed batch hint")
	}
}
