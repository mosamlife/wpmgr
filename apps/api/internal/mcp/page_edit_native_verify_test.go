package mcp

import (
	"bytes"
	"encoding/json"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type nativeEditGolden struct {
	Name string `json:"name"`
	pageEditPreviewFixture
}

func nativeEditGoldens(t *testing.T) []nativeEditGolden {
	t.Helper()
	b, err := os.ReadFile(agentAbilityFixtures + "native-precheck-answers.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []nativeEditGolden
	if json.Unmarshal(b, &rows) != nil || len(rows) == 0 {
		t.Fatal("native goldens missing or malformed")
	}
	return rows
}

func TestNativeEditGoldens(t *testing.T) {
	for _, row := range nativeEditGoldens(t) {
		t.Run(row.Name, func(t *testing.T) {
			f := fixtureFacts(t, row.pageEditPreviewFixture)
			if _, ok := verifyPageEditPrecheck(fixtureResponse(row.pageEditPreviewFixture, nil), row.EntrySHA256, []byte(row.Input), f); !ok {
				t.Fatal("real PHP precheck refused")
			}
		})
	}
}

func nativeGoldenByName(t *testing.T, name string) pageEditPreviewFixture {
	t.Helper()
	for _, row := range nativeEditGoldens(t) {
		if row.Name == name {
			return row.pageEditPreviewFixture
		}
	}
	t.Fatalf("missing native golden %s", name)
	return pageEditPreviewFixture{}
}

// A mutation is rebound to fresh digests when it changes canonical digest inputs.
// Rebinding tests the semantic checks beyond the outer SHA-256 comparison.
func nativeGoldenResponse(t *testing.T, fx pageEditPreviewFixture, pv pageEditNativePreview, rebind bool) agentcmd.AbilityRunResponse {
	t.Helper()
	resp := fixtureResponse(fx, nil)
	if rebind {
		tree, ok := phpDecodeDocument(pv.Tree, pageEditTreeMaxDepth, true)
		if !ok {
			t.Fatal("mutated tree malformed")
		}
		treeBytes, ok := phpEncodeBytes(tree)
		if !ok {
			t.Fatal("mutated tree unencodable")
		}
		pv.TreeSHA256 = sha256Hex(treeBytes)
		inputs, ok := phpDecodeDocument(pv.DigestInputs, 32, true)
		if !ok {
			t.Fatal("mutated digest inputs malformed")
		}
		inputBytes, ok := nativeDigestInputsBytes(inputs)
		if !ok {
			t.Fatal("mutated digest inputs unencodable")
		}
		digestBytes, ok := phpEncodeBytes(phpList{pageEditNativeDigestDomain, pv.Builder, pv.BuilderVersion, json.Number(strconv.FormatInt(pv.PostID, 10)), fx.BaseFingerprint, string(treeBytes), string(inputBytes)})
		if !ok {
			t.Fatal("mutated digest unencodable")
		}
		resp.PreviewDigest = sha256Hex(digestBytes)
		pre, _ := phpJSONStringArray(fx.EntrySHA256, sha256Hex([]byte(fx.Input)), fx.BaseFingerprint, resp.PreviewDigest)
		resp.PrecheckDigest = sha256Hex(pre)
	}
	b, err := json.Marshal(pv)
	if err != nil {
		t.Fatal(err)
	}
	resp.Preview = b
	return resp
}

func nativeMutateResults(t *testing.T, pv *pageEditNativePreview, change func(*pageEditNativeResult)) {
	t.Helper()
	var results []pageEditNativeResult
	if err := json.Unmarshal(pv.Results, &results); err != nil {
		t.Fatal(err)
	}
	change(&results[0])
	pv.Results = mustNativeJSON(t, results)
}
func mustNativeJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNativeEditGoldenMutationsAreRefused(t *testing.T) {
	cases := []struct {
		name, golden string
		rebind       bool
		mutate       func(*testing.T, *pageEditNativePreview)
	}{
		{"before tree hash", "set_native", false, func(t *testing.T, p *pageEditNativePreview) { p.BeforeTreeSHA256 = strings.Repeat("0", 64) }},
		{"digest before hash", "set_native", true, func(t *testing.T, p *pageEditNativePreview) {
			v, _ := phpDecodeDocument(p.DigestInputs, 32, true)
			v = nativeSet(v.(phpObject), "before_tree_sha256", strings.Repeat("0", 64))
			p.DigestInputs, _ = phpEncodeBytes(v)
		}},
		{"changed tree with valid digest", "set_native", true, func(t *testing.T, p *pageEditNativePreview) {
			v, _ := phpDecodeDocument(p.Tree, pageEditTreeMaxDepth, true)
			index, _ := indexTree(v)
			node := index["ccccccc"]
			settings := nativeSet(nativeGet(node.node, "settings").(phpObject), "title", "Other title")
			replacement := nativeSet(node.node.(phpObject), "settings", settings)
			v, _ = nativeReplaceNode(v, node.path, replacement)
			p.Tree, _ = phpEncodeBytes(v)
		}},
		{"unrelated tree setting with valid digest", "set_native", true, func(t *testing.T, p *pageEditNativePreview) {
			tree, _ := phpDecodeDocument(p.Tree, pageEditTreeMaxDepth, true)
			index, _ := indexTree(tree)
			node := index["52982f9"]
			settings := nativeSet(nativeGet(node.node, "settings").(phpObject), "unused_site_setting", "different")
			replacement := nativeSet(node.node.(phpObject), "settings", settings)
			tree, _ = nativeReplaceNode(tree, node.path, replacement)
			p.Tree, _ = phpEncodeBytes(tree)
		}},
		{"changed ordinary outline text", "set_native", false, func(t *testing.T, p *pageEditNativePreview) {
			var outline map[string]any
			if err := json.Unmarshal(p.AfterOutline, &outline); err != nil {
				t.Fatal(err)
			}
			for _, item := range outline["nodes"].([]any) {
				n := item.(map[string]any)
				if n["ref"] == "52982f9" {
					n["from_the_site"].(map[string]any)["text"] = "Fake site text"
					break
				}
			}
			p.AfterOutline = mustNativeJSON(t, outline)
		}},
		{"changed result node", "set_native", false, func(t *testing.T, p *pageEditNativePreview) {
			nativeMutateResults(t, p, func(r *pageEditNativeResult) {
				r.ResultNode = bytes.Replace(r.ResultNode, []byte("New counter"), []byte("Other title"), 1)
			})
		}},
		{"changed before settings", "set_native", false, func(t *testing.T, p *pageEditNativePreview) {
			nativeMutateResults(t, p, func(r *pageEditNativeResult) {
				r.BeforeSettings = bytes.Replace(r.BeforeSettings, []byte("Counter"), []byte("Other"), 1)
			})
		}},
		{"changed operation echo", "set_native", false, func(t *testing.T, p *pageEditNativePreview) {
			nativeMutateResults(t, p, func(r *pageEditNativeResult) { r.Op = bytes.Replace(r.Op, []byte("ccccccc"), []byte("bbbbbbb"), 1) })
		}},
		{"reused new id", "add_native", false, func(t *testing.T, p *pageEditNativePreview) {
			nativeMutateResults(t, p, func(r *pageEditNativeResult) { r.NewIDs = []string{"ccccccc"} })
		}},
		{"changed builder version", "set_native", true, func(t *testing.T, p *pageEditNativePreview) {
			v, _ := phpDecodeDocument(p.DigestInputs, 32, true)
			v = nativeSet(v.(phpObject), "builder_version", "4.3.4")
			p.DigestInputs, _ = phpEncodeBytes(v)
		}},
		{"missing breakpoints on a settings write", "set_native", true, func(t *testing.T, p *pageEditNativePreview) {
			v, _ := phpDecodeDocument(p.DigestInputs, 32, true)
			v = nativeSet(v.(phpObject), "active_breakpoints", nil)
			p.DigestInputs, _ = phpEncodeBytes(v)
		}},
		{"breakpoints out of order", "set_native", true, func(t *testing.T, p *pageEditNativePreview) {
			v, _ := phpDecodeDocument(p.DigestInputs, 32, true)
			v = nativeSet(v.(phpObject), "active_breakpoints", phpList{"tablet", "mobile"})
			p.DigestInputs, _ = phpEncodeBytes(v)
		}},
		{"unverified media fact", "set_native", true, func(t *testing.T, p *pageEditNativePreview) {
			v, _ := phpDecodeDocument(p.DigestInputs, 32, true)
			v = nativeSet(v.(phpObject), "media", phpList{phpObject{{"id", "7"}, {"url", "https://example.com/picture.png"}, {"filename", "picture.png"}, {"mime", "image/png"}, {"width", json.Number("100")}, {"height", json.Number("100")}, {"modified_gmt", "2026-10-13 00:00:00"}}})
			p.DigestInputs, _ = phpEncodeBytes(v)
		}},
		{"unknown native outline widget", "set_native", false, func(t *testing.T, p *pageEditNativePreview) {
			var outline map[string]any
			if err := json.Unmarshal(p.AfterOutline, &outline); err != nil {
				t.Fatal(err)
			}
			for _, item := range outline["nodes"].([]any) {
				n := item.(map[string]any)
				if n["kind"] == "native" {
					n["widget"] = "unknown-widget"
					break
				}
			}
			p.AfterOutline = mustNativeJSON(t, outline)
		}},
		{"wrong native digest", "set_native", false, func(t *testing.T, p *pageEditNativePreview) {
			v, _ := phpDecodeDocument(p.DigestInputs, 32, true)
			widgets := nativeGet(v, "widgets").(phpObject)
			widgets[0].value = strings.Repeat("0", 64)
			v = nativeSet(v.(phpObject), "widgets", widgets)
			p.DigestInputs, _ = phpEncodeBytes(v)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := nativeGoldenByName(t, c.golden)
			f := fixtureFacts(t, fx)
			var pv pageEditNativePreview
			if err := json.Unmarshal(fx.Preview, &pv); err != nil {
				t.Fatal(err)
			}
			c.mutate(t, &pv)
			resp := nativeGoldenResponse(t, fx, pv, c.rebind)
			if _, ok := verifyPageEditPrecheck(resp, fx.EntrySHA256, []byte(fx.Input), f); ok {
				t.Fatal("mutated real precheck was accepted")
			}
		})
	}
}

func TestNativeEditUnsafeAIValueWithReboundDigestsIsRefused(t *testing.T) {
	fx := nativeGoldenByName(t, "set_native")
	fx.Input = strings.Replace(fx.Input, "New counter", "<script>x</script>", 1)
	f := fixtureFacts(t, fx)
	var pv pageEditNativePreview
	if err := json.Unmarshal(fx.Preview, &pv); err != nil {
		t.Fatal(err)
	}
	tree, _ := phpDecodeDocument(pv.Tree, pageEditTreeMaxDepth, true)
	index, _ := indexTree(tree)
	node := index["ccccccc"]
	changed := nativeSet(node.node.(phpObject), "settings", nativeSet(nativeGet(node.node, "settings").(phpObject), "title", "<script>x</script>"))
	tree, _ = nativeReplaceNode(tree, node.path, changed)
	pv.Tree, _ = phpEncodeBytes(tree)
	nativeMutateResults(t, &pv, func(r *pageEditNativeResult) {
		r.Op = bytes.Replace(r.Op, []byte("New counter"), []byte("<script>x</script>"), 1)
		r.ResultNode = bytes.Replace(r.ResultNode, []byte("New counter"), []byte("<script>x</script>"), 1)
	})
	var outline map[string]any
	if err := json.Unmarshal(pv.AfterOutline, &outline); err != nil {
		t.Fatal(err)
	}
	for _, item := range outline["nodes"].([]any) {
		n := item.(map[string]any)
		if n["kind"] == "native" {
			n["summary"] = "<script>x</script>"
			break
		}
	}
	pv.AfterOutline = mustNativeJSON(t, outline)
	resp := nativeGoldenResponse(t, fx, pv, true)
	if _, ok := verifyPageEditPrecheck(resp, fx.EntrySHA256, []byte(fx.Input), f); ok {
		t.Fatal("unsafe AI-written native text accepted")
	}
}

func TestNativeEditDuplicateProtectsSource(t *testing.T) {
	fx := nativeGoldenByName(t, "duplicate")
	var pv pageEditNativePreview
	if err := json.Unmarshal(fx.Preview, &pv); err != nil {
		t.Fatal(err)
	}
	before, _ := phpDecodeDocument(pv.BeforeTree, pageEditTreeMaxDepth, true)
	index, _ := indexTree(before)
	source := index["ccccccc"]
	for _, c := range []struct {
		name     string
		settings phpObject
	}{
		{"anchor", phpObject{{"title", "Counter"}, {"_element_id", "anchor"}}},
		{"dynamic", phpObject{{"title", "Counter"}, {"__dynamic__", phpObject{{"title", "bound"}}}}},
		{"raw html", phpObject{{"title", "<script>x</script>"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			node := nativeSet(source.node.(phpObject), "settings", c.settings)
			if nativeCopySafe(node, pv.BuilderVersion) {
				t.Fatal("protected source was duplicable")
			}
		})
	}
	t.Run("node metadata", func(t *testing.T) {
		node := append(slices.Clone(source.node.(phpObject)), phpPair{"_element_id", "anchor"})
		if nativeCopySafe(node, pv.BuilderVersion) {
			t.Fatal("protected node metadata was duplicable")
		}
	})
}

func TestNativeEditCardSizeAndStoredFields(t *testing.T) {
	fx := nativeGoldenByName(t, "set_native")
	f := fixtureFacts(t, fx)
	checked, ok := verifyPageEditPrecheck(fixtureResponse(fx, nil), fx.EntrySHA256, []byte(fx.Input), f)
	if !ok {
		t.Fatal("golden refused")
	}
	card, err := buildPageEditCardFacts(checked, time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"before_tree_sha256", "digest_inputs", "result_node", "whole_settings"} {
		if bytes.Contains(card, []byte(secret)) {
			t.Fatalf("card stored %s", secret)
		}
	}
	if !bytes.Contains(card, []byte("New counter")) || !bytes.Contains(card, []byte(siteTextMarker+"Counter")) {
		t.Fatalf("card lost the changed key: %s", card)
	}
	checked.nativeChanges[0].Native.Keys[0].After = strings.Repeat("a", pageEditCardMaxBytes)
	if _, err := buildPageEditCardFacts(checked, time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)); err != errPageEditCardTooLarge {
		t.Fatalf("oversized card: %v", err)
	}
}

func TestNativeCardMeasuresPostgresJSONBText(t *testing.T) {
	for _, c := range []struct{ input, want string }{
		{`{"a":["<tag>",{"b":"é"}]}`, `{"a": ["<tag>", {"b": "é"}]}`},
		{`{"a":1e6}`, `{"a": 1000000}`},
		{`{"a":1.20e1}`, `{"a": 12.0}`},
	} {
		got, err := pageEditJSONBTextSize([]byte(c.input))
		if err != nil || got != len(c.want) {
			t.Fatalf("%s: bytes=%d error=%v; want %d", c.input, got, err, len(c.want))
		}
	}
}

func TestNativeEditRefusalHints(t *testing.T) {
	for _, code := range []string{"native_node_shape", "native_duplicate_refused", "native_preview_too_large", "preview_unreadable"} {
		if nativeRefusalHints[code] == "" {
			t.Errorf("%s has no plain hint", code)
		}
	}
}

func TestNativeEditCopiedDescendantMedia(t *testing.T) {
	node := phpObject{{"settings", phpList{}}, {"elements", phpList{
		phpObject{{"settings", phpObject{{"image", phpObject{{"id", "7"}, {"url", "https://example.com/x.png"}}}}}, {"elements", phpList{}}},
	}}}
	media := []pageMediaFact{{ID: 7, URL: "https://example.com/x.png"}}
	used := map[string]bool{}
	if !nativeEditCopiedMedia(node, media, used) || !used["7"] {
		t.Fatal("copied descendant media was not checked")
	}
	media[0].URL = "https://example.com/other.png"
	if nativeEditCopiedMedia(node, media, map[string]bool{}) {
		t.Fatal("copied descendant accepted a mismatched media fact")
	}
}

func TestNativeEditCardExternalLinks(t *testing.T) {
	links := nativeCardLinks(phpObject{{"link", "https://example.org/path"}, {"media", phpObject{{"url", "https://cdn.example.org/a.png"}}}})
	if len(links) != 2 || links[0].Text != "https://example.org/path" || links[0].ExternalDomain != "example.org" || links[1].ExternalDomain != "cdn.example.org" {
		t.Fatalf("links: %+v", links)
	}
}
