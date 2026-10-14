package mcp

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/elementornative"
)

type nativePhase2Fixture struct {
	Widgets []struct {
		Name     string
		Eligible bool
	}
	Devices     []string
	CopiedMedia []struct {
		ID       json.RawMessage
		Accepted bool
	} `json:"copied_media"`
	SectionHint string `json:"section_hint"`
}

func nativePhase2Cases(t *testing.T) nativePhase2Fixture {
	t.Helper()
	var cases nativePhase2Fixture
	b, err := os.ReadFile(agentAbilityFixtures + "native-phase2-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases.Widgets) == 0 || len(cases.Devices) == 0 || len(cases.CopiedMedia) == 0 || cases.SectionHint == "" {
		t.Fatal("phase2 cases missing")
	}
	return cases
}

func TestNativePhase2WidgetEligibility(t *testing.T) {
	for _, c := range nativePhase2Cases(t).Widgets {
		got := nativeWidgetPattern.MatchString(c.Name) && !nativeDeniedWidget(c.Name) && len(elementornative.Lookup(c.Name, "3.35.9")) > 0
		if got != c.Eligible {
			t.Fatalf("%s eligibility=%v, want %v", c.Name, got, c.Eligible)
		}
	}
}

func TestNativePhase2UnderscoreInput(t *testing.T) {
	input := []byte(`{"post_id":41,"base_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","operations":[{"op":"add_native","widget":"google_maps","settings":{},"into":"2222222"}]}`)
	if _, code := validatePageEditInput(input); code != "" {
		t.Fatalf("google_maps input refused: %s", code)
	}
}

func TestNativePhase2Visibility(t *testing.T) {
	for _, device := range nativePhase2Cases(t).Devices {
		t.Run(device, func(t *testing.T) {
			key := "hide_" + device
			settings := nativeTestValue(t, json.RawMessage(`{"`+key+`":"hidden-`+device+`"}`))
			schema, ok := nativeOwnedSchema("button", "3.35.9", settings)
			if !ok {
				t.Fatal("owned visibility schema missing")
			}
			got := CheckNativeSettings(NativePolicyInput{Widget: "button", ElementorVersion: "3.35.9", Schema: schema, Before: json.RawMessage(`[]`), Settings: json.RawMessage(`{"` + key + `":"hidden-` + device + `"}`)})
			if !got.OK {
				t.Fatalf("exact visibility control refused: %+v", got.Failures)
			}
		})
	}
}

func TestNativePhase2CopiedMedia(t *testing.T) {
	for _, c := range nativePhase2Cases(t).CopiedMedia {
		t.Run(string(c.ID), func(t *testing.T) {
			settings := nativeTestValue(t, json.RawMessage(`{"image":{"id":`+string(c.ID)+`,"url":"https://site.example/image.jpg"}}`))
			node := phpObject{{"settings", settings}, {"elements", phpList{}}}
			before, _ := phpEncodeBytes(node)
			used := map[string]bool{}
			got := nativeEditCopiedMedia(node, []pageMediaFact{{ID: 7, URL: "https://site.example/image.jpg"}}, used)
			if got && nativeEditCopiedMedia(node, []pageMediaFact{{ID: 7, URL: "https://site.example/other.jpg"}}, map[string]bool{}) {
				t.Fatal("copied media accepted a mismatched URL")
			}
			if got != c.Accepted {
				t.Fatalf("copied media accepted=%v, want %v", got, c.Accepted)
			}
			after, _ := phpEncodeBytes(node)
			if string(before) != string(after) {
				t.Fatal("copied media bytes changed")
			}
			if got && !used["7"] {
				t.Fatal("copied media fact not used")
			}
			if string(c.ID) == "7" && nativeEditWrittenMedia(settings, []pageMediaFact{{ID: 7, URL: "https://site.example/image.jpg"}}, map[string]bool{}) {
				t.Fatal("AI-written numeric media accepted")
			}
		})
	}
}

func TestNativePhase2ColumnDestinations(t *testing.T) {
	tree := nativeTestValue(t, json.RawMessage(`[{"id":"2222222","elType":"container","settings":{"flex_direction":"row"},"elements":[{"id":"3333333","elType":"container","settings":[],"elements":[]}]}]`))
	index, ok := indexTree(tree)
	if !ok {
		t.Fatal("invalid destination tree")
	}
	node := phpObject{{"id", "aaaaaaa"}, {"elType", "widget"}, {"widgetType", "counter"}, {"settings", phpList{}}, {"elements", phpList{}}}
	for _, name := range []string{"add_native", "duplicate", "move"} {
		for _, anchor := range []string{"into", "before", "after"} {
			t.Run(name+"/"+anchor, func(t *testing.T) {
				ref := "3333333"
				if anchor == "into" {
					ref = "2222222"
				}
				if _, ok := nativePlaceNode(tree, pageEditOp{Op: name, Anchor: anchor, AnchorRef: ref}, node, index, name != "move"); ok {
					t.Fatal("widget placed in columns row")
				}
				if _, ok := nativePlaceNode(tree, pageEditOp{Op: name, Anchor: "into", AnchorRef: "3333333"}, node, index, name != "move"); !ok {
					t.Fatal("widget refused in content column")
				}
			})
		}
	}
}

func TestNativePhase2SectionHint(t *testing.T) {
	hint := nativePhase2Cases(t).SectionHint
	got := sitePrecheckRefusal(AbilityPageEdit, &agentcmd.AbilityRunRefusal{Code: pageEditOpsInvalid, Detail: hint, Retryable: true})
	if got.err.Details["hint"] != hint || got.err.Details["retryable"] != false {
		t.Fatalf("native section hint lost: %+v", got.err.Details)
	}
}
