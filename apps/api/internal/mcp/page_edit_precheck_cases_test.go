package mcp

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

// The agent's own LayoutOps answers (elementor-edit-cases.json, written by
// LayoutOpsTest), each given as the precheck answer the agent sends for it:
// the edited tree, its changes and the outline after the edit, every digest
// computed as the agent computes it.

type pageEditCasesFixture struct {
	Cases []pageEditCase `json:"cases"`
}

type pageEditCase struct {
	Name      string          `json:"name"`
	Ops       json.RawMessage `json:"ops"`
	AfterTree json.RawMessage `json:"after_tree"`
	Changes   json.RawMessage `json:"changes"`
}

var (
	editCaseFingerprint = strings.Repeat("ab", 32)
	editCaseEntrySum    = strings.Repeat("cd", 32)
)

const (
	editCasePostID  = 418
	editCaseVersion = "3.35.9"
)

func readPageEditCases(t *testing.T) map[string]pageEditCase {
	t.Helper()
	raw, err := os.ReadFile(agentAbilityFixtures + "elementor-edit-cases.json")
	if err != nil {
		t.Fatalf("read the agent's edit cases: %v", err)
	}
	var fx pageEditCasesFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("decode the agent's edit cases: %v", err)
	}
	out := make(map[string]pageEditCase, len(fx.Cases))
	for _, c := range fx.Cases {
		if c.Name == "" || len(c.Ops) == 0 || len(c.AfterTree) == 0 || len(c.Changes) == 0 {
			t.Fatalf("an edit case without a name, operations, tree or changes: %q", c.Name)
		}
		out[c.Name] = c
	}
	// The batches this file exists for must be there: an empty or renamed
	// fixture fails here, never passes by checking nothing.
	for _, name := range []string{
		"set-text-every-field-containers", "change-then-remove-containers", "change-then-remove-sections",
	} {
		if _, ok := out[name]; !ok {
			t.Fatalf("the agent's edit cases carry no %q", name)
		}
	}
	return out
}

// editCaseCall is the input for ops and the agent's precheck answer for tree
// and changes, with the facts the control plane reads from the input.
func editCaseCall(t *testing.T, ops, tree, changes json.RawMessage) ([]byte, agentcmd.AbilityRunResponse, pageEditFacts) {
	t.Helper()
	input, err := json.Marshal(map[string]any{
		"post_id": editCasePostID, "base_fingerprint": editCaseFingerprint, "operations": ops,
	})
	if err != nil {
		t.Fatal(err)
	}
	f, code := validatePageEditInput(input)
	if code != "" {
		t.Fatalf("the case's input is refused: %s", code)
	}
	decoded, ok := phpDecodeDocument(tree, pageEditTreeMaxDepth, true)
	if !ok {
		t.Fatal("the case's tree does not decode")
	}
	treeJSON, ok := phpEncodeBytes(decoded)
	if !ok {
		t.Fatal("the case's tree does not encode")
	}
	proj, err := elementorProject(decoded)
	if err != nil {
		t.Fatalf("the case's tree does not project: %v", err)
	}
	after, ok := proj.answer(pageStructureMaxNodes, pageStructureMaxBytes)
	if !ok {
		t.Fatal("the case's outline does not fit")
	}
	outline, ok := projectionJSON(after)
	if !ok {
		t.Fatal("the case's outline does not encode")
	}
	preview, err := json.Marshal(map[string]any{
		"post_id": editCasePostID, "builder": pageBuilderElementor, "builder_version": editCaseVersion,
		"format": elementorFormatClassic, "title": "Spring", "changes": changes, "after_outline": outline,
		"tree_sha256": sha256Hex(treeJSON), "tree": tree,
	})
	if err != nil {
		t.Fatal(err)
	}
	digest, ok := pageEditPreviewDigest(pageBuilderElementor, editCaseVersion, editCasePostID, editCaseFingerprint, treeJSON)
	if !ok {
		t.Fatal("no preview digest")
	}
	pre, _ := phpJSONStringArray(editCaseEntrySum, sha256Hex(input), editCaseFingerprint, digest)
	return input, agentcmd.AbilityRunResponse{
		OK: true, Outcome: "prechecked", Mode: "precheck", Ability: AbilityPageEdit,
		RequestID: "22222222-3333-4444-8555-888888888888", Valid: true, BaseFingerprint: editCaseFingerprint,
		PreviewDigest: digest, PrecheckDigest: sha256Hex(pre), Preview: preview,
	}, f
}

// TestPageEditPrecheckBindsTheAgentsEditCases: every answer LayoutOps gives
// for the agent's own batches binds to its input, a batch whose later
// operations take earlier changes off the page among them.
func TestPageEditPrecheckBindsTheAgentsEditCases(t *testing.T) {
	cases := readPageEditCases(t)
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			input, resp, f := editCaseCall(t, c.Ops, c.AfterTree, c.Changes)
			checked, ok := verifyPageEditPrecheck(resp, editCaseEntrySum, input, f)
			if !ok {
				t.Fatal("the agent's own answer for this batch does not bind to its input")
			}
			if len(checked.changes) != len(f.ops) {
				t.Fatalf("checked %d changes for %d operations", len(checked.changes), len(f.ops))
			}
			if _, err := buildPageEditCardFacts(checked, time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)); err != nil {
				t.Fatalf("card: %v", err)
			}
		})
	}
}

// decodeEditCase decodes a case's operations, tree and changes for an edit.
func decodeEditCase(t *testing.T, c pageEditCase) (ops, tree, changes []any) {
	t.Helper()
	for _, part := range []struct {
		raw json.RawMessage
		out *[]any
	}{{c.Ops, &ops}, {c.AfterTree, &tree}, {c.Changes, &changes}} {
		dec := json.NewDecoder(bytes.NewReader(part.raw))
		dec.UseNumber()
		if err := dec.Decode(part.out); err != nil {
			t.Fatal(err)
		}
	}
	return ops, tree, changes
}

func rawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// withoutElement is list with the element id taken out, at any depth.
func withoutElement(list []any, id string) []any {
	out := make([]any, 0, len(list))
	for _, n := range list {
		m, _ := n.(map[string]any)
		if m != nil && m["id"] == id {
			continue
		}
		if m != nil {
			if children, ok := m["elements"].([]any); ok {
				m["elements"] = withoutElement(children, id)
			}
		}
		out = append(out, n)
	}
	return out
}

// widgetNode is a stored widget with id, of an Elementor type WPMgr edits.
func widgetNode(id, widgetType string, settings map[string]any) map[string]any {
	return map[string]any{"id": id, "elType": "widget", "settings": settings, "elements": []any{}, "widgetType": widgetType}
}

// TestPageEditPrecheckRefusesANodeOffThePageWithoutALaterRemoval: a node a
// change names or makes may be off the page only when a later operation of
// the call took off a node that holds others, and then whatever the change
// was placed beside or into went with it. Each answer below is refused, every
// digest recomputed so only that rule can refuse it.
func TestPageEditPrecheckRefusesANodeOffThePageWithoutALaterRemoval(t *testing.T) {
	cases := readPageEditCases(t)
	const (
		heading2   = "605cdc2" // the first heading: the move's anchor
		paragraph1 = "470f2bd" // "One": the second insert's anchor
		lastGroup  = "e359fab" // the group that stays on the page
		row        = "0d3499d" // the row of columns, holding the button
	)
	for name, tc := range map[string]struct {
		base string
		edit func(ops, tree, changes []any) ([]any, []any, []any)
	}{
		"a changed node is off the page and no operation took it off": {
			base: "set-text-every-field-containers",
			edit: func(ops, tree, changes []any) ([]any, []any, []any) {
				return ops, withoutElement(tree, heading2), changes
			},
		},
		"a changed node is off the page and the only later removal holds no nodes": {
			base: "set-text-every-field-containers",
			edit: func(ops, tree, changes []any) ([]any, []any, []any) {
				ops = append(ops, map[string]any{"op": "remove", "ref": paragraph1})
				changes = append(changes, map[string]any{"op": "remove", "ref": paragraph1, "kind": "paragraph", "before": map[string]any{"text": "One"}})
				return ops, withoutElement(withoutElement(tree, paragraph1), heading2), changes
			},
		},
		"a changed node is off the page and only an earlier removal took nodes off": {
			base: "set-text-every-field-containers",
			edit: func(ops, tree, changes []any) ([]any, []any, []any) {
				ops = append([]any{map[string]any{"op": "remove", "ref": row}}, ops...)
				changes = append([]any{map[string]any{"op": "remove", "ref": row, "kind": "columns"}}, changes...)
				// The row held the button whose link the call sets.
				return ops, withoutElement(tree, row), changes
			},
		},
		"a moved node is off the page while its anchor stays": {
			base: "change-then-remove-containers",
			edit: func(ops, tree, changes []any) ([]any, []any, []any) {
				group := findWidget(tree, lastGroup)
				group["elements"] = append([]any{widgetNode(heading2, "heading", map[string]any{"title": "Spring sale", "header_size": "h2"})}, group["elements"].([]any)...)
				return ops, tree, changes
			},
		},
		"an insert's nodes are off the page while its anchor stays": {
			base: "change-then-remove-containers",
			edit: func(ops, tree, changes []any) ([]any, []any, []any) {
				group := findWidget(tree, lastGroup)
				group["elements"] = append([]any{widgetNode(paragraph1, "text-editor", map[string]any{"editor": "<p>One</p>"})}, group["elements"].([]any)...)
				return ops, tree, changes
			},
		},
		"a new node off the page takes a ref the input names": {
			base: "change-then-remove-containers",
			edit: func(ops, tree, changes []any) ([]any, []any, []any) {
				// The group the fifth operation removes.
				changes[3].(map[string]any)["new_refs"] = []any{"93e6ad3"}
				return ops, tree, changes
			},
		},
		"some of an insert's nodes are on the page and some are not": {
			base: "change-then-remove-containers",
			edit: func(ops, tree, changes []any) ([]any, []any, []any) {
				c := changes[2].(map[string]any)
				c["new_refs"] = append(c["new_refs"].([]any), "0a0a0a0")
				group := findWidget(tree, lastGroup)
				group["elements"] = append([]any{widgetNode("0a0a0a0", "text-editor", map[string]any{"editor": "<p>Kept</p>"})}, group["elements"].([]any)...)
				return ops, tree, changes
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, ok := cases[tc.base]
			if !ok {
				t.Fatalf("no case %q", tc.base)
			}
			// The control: the base answer binds.
			input, resp, f := editCaseCall(t, c.Ops, c.AfterTree, c.Changes)
			if _, ok := verifyPageEditPrecheck(resp, editCaseEntrySum, input, f); !ok {
				t.Fatal("the agent's own answer does not bind")
			}
			ops, tree, changes := decodeEditCase(t, c)
			ops, tree, changes = tc.edit(ops, tree, changes)
			input, resp, f = editCaseCall(t, rawJSON(t, ops), rawJSON(t, tree), rawJSON(t, changes))
			if _, ok := verifyPageEditPrecheck(resp, editCaseEntrySum, input, f); ok {
				t.Fatal("an answer with a node off the page that no later operation took off was accepted")
			}
		})
	}
}
