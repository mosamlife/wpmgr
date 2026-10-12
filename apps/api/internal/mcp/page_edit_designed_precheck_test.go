package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

type designedEditCase struct {
	Name      string          `json:"name"`
	Page      string          `json:"page"`
	Input     string          `json:"input"`
	RequestID string          `json:"request_id"`
	Answer    json.RawMessage `json:"answer"`
}

func readDesignedEdits(t *testing.T) (string, []designedEditCase) {
	t.Helper()
	raw, err := os.ReadFile(agentAbilityFixtures + "page-edit-designed-previews.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		EntrySHA256 string             `json:"entry_sha256"`
		Cases       []designedEditCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) == 0 || fixture.EntrySHA256 == "" {
		t.Fatal("empty designed edit fixture")
	}
	return fixture.EntrySHA256, fixture.Cases
}

func designedEditResponse(t *testing.T, c designedEditCase) agentcmd.AbilityRunResponse {
	t.Helper()
	var resp agentcmd.AbilityRunResponse
	if err := json.Unmarshal(c.Answer, &resp); err != nil {
		t.Fatal(err)
	}
	resp.Raw = c.Answer
	return resp
}

func TestDesignedPageEditGolden(t *testing.T) {
	entry, cases := readDesignedEdits(t)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			resp := designedEditResponse(t, c)
			input := []byte(c.Input)
			f, code := validatePageEditInput(input)
			if !resp.OK {
				if code != "" && code != resp.Code {
					t.Fatalf("input refusal %s, agent refusal %s", code, resp.Code)
				}
				if _, ok := verifyPageEditPrecheck(resp, entry, input, f); ok {
					t.Fatal("accepted a refusal")
				}
				return
			}
			if code != "" {
				t.Fatalf("golden input refused: %s", code)
			}
			checked, ok := verifyPageEditPrecheck(resp, entry, input, f)
			if !ok {
				t.Fatal("the agent's designed page-edit answer was refused")
			}
			if checked.previewDigest != resp.PreviewDigest || checked.precheckDigest != resp.PrecheckDigest {
				t.Fatal("checked digests differ")
			}
			var pv pageEditPreview
			if err := json.Unmarshal(resp.Preview, &pv); err != nil {
				t.Fatal(err)
			}
			tree, ok := phpDecodeDocument(pv.Tree, pageEditTreeMaxDepth, true)
			if !ok {
				t.Fatal("tree did not decode")
			}
			canon, ok := phpEncodeBytes(tree)
			if !ok || sha256Hex(canon) != pv.TreeSHA256 {
				t.Fatal("tree digest differs")
			}
			// Change one byte of an Elementor setting, keeping the recorded digests.
			changed := bytes.Replace(canon, []byte(`"elType":"container"`), []byte(`"elType":"containeX"`), 1)
			if bytes.Equal(changed, canon) {
				t.Fatal("fixture has no container to mutate")
			}
			pv.Tree = changed
			resp.Preview, _ = json.Marshal(pv)
			if _, ok := verifyPageEditPrecheck(resp, entry, input, f); ok {
				t.Fatal("accepted a tree differing in one byte")
			}
		})
	}
}

// Re-form every digest so these checks exercise the mapping, rather than a
// stale hash. The unchanged control must still bind after re-encoding.
func forgeDesignedEdit(t *testing.T, entry string, c designedEditCase, edit func([]byte) []byte) agentcmd.AbilityRunResponse {
	t.Helper()
	resp := designedEditResponse(t, c)
	var pv pageEditPreview
	if err := json.Unmarshal(resp.Preview, &pv); err != nil {
		t.Fatal(err)
	}
	tree, ok := phpDecodeDocument(pv.Tree, pageEditTreeMaxDepth, true)
	if !ok {
		t.Fatal("tree did not decode")
	}
	canon, _ := phpEncodeBytes(tree)
	pv.Tree = edit(canon)
	tree, ok = phpDecodeDocument(pv.Tree, pageEditTreeMaxDepth, true)
	if !ok {
		t.Fatal("edited tree did not decode")
	}
	canon, _ = phpEncodeBytes(tree)
	pv.Tree = canon
	pv.TreeSHA256 = sha256Hex(canon)
	proj, err := elementorProject(tree)
	if err != nil {
		t.Fatal(err)
	}
	outline, ok := proj.answer(pageStructureMaxNodes, pageStructureMaxBytes)
	if !ok {
		t.Fatal("edited outline did not encode")
	}
	pv.AfterOutline, _ = json.Marshal(outline)
	resp.Preview, _ = json.Marshal(pv)
	resp.PreviewDigest, _ = pageEditPreviewDigest(pv.Builder, pv.BuilderVersion, pv.PostID, resp.BaseFingerprint, canon)
	resp.PrecheckDigest = agentPrecheckDigest(t, entry, []byte(c.Input), resp.BaseFingerprint, resp.PreviewDigest)
	return resp
}

func TestDesignedPageEditRecomputedTampering(t *testing.T) {
	entry, cases := readDesignedEdits(t)
	for _, c := range cases {
		if !designedEditResponse(t, c).OK {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			f, code := validatePageEditInput([]byte(c.Input))
			if code != "" {
				t.Fatal(code)
			}
			check := func(resp agentcmd.AbilityRunResponse) bool {
				_, ok := verifyPageEditPrecheck(resp, entry, []byte(c.Input), f)
				return ok
			}
			if !check(forgeDesignedEdit(t, entry, c, func(b []byte) []byte { return b })) {
				t.Fatal("recomputed unchanged answer refused")
			}
			var pv pageEditPreview
			if err := json.Unmarshal(designedEditResponse(t, c).Preview, &pv); err != nil {
				t.Fatal(err)
			}
			var changes []pageEditChange
			if err := json.Unmarshal(pv.Changes, &changes); err != nil {
				t.Fatal(err)
			}
			for i, op := range f.ops {
				if len(changes[i].NewRefs) > 0 {
					id := changes[i].NewRefs[0]
					resp := forgeDesignedEdit(t, entry, c, func(b []byte) []byte {
						start := bytes.Index(b, []byte(`"id":"`+id+`"`))
						if start < 0 {
							t.Fatal("new node not found")
						}
						at := bytes.Index(b[start:], []byte(`"settings":{`))
						if at < 0 {
							t.Fatal("new settings not found")
						}
						at += start + len(`"settings":{`)
						// Change one ASCII byte in the first setting's value.
						at += bytes.Index(b[at:], []byte(`":"`)) + len(`":"`)
						changed := append([]byte{}, b...)
						changed[at] = 'X'
						if bytes.Equal(changed, b) {
							t.Fatal("one-byte mutation changed nothing")
						}
						return changed
					})
					if check(resp) {
						t.Fatal("accepted a one-byte setting change with recomputed digests")
					}
					resp = forgeDesignedEdit(t, entry, c, func(b []byte) []byte {
						start := bytes.Index(b, []byte(`"id":"`+id+`"`))
						if start < 0 {
							t.Fatal("new node not found")
						}
						at := bytes.Index(b[start:], []byte(`"settings":{`))
						if at < 0 {
							t.Fatal("new settings not found")
						}
						at += start + len(`"settings":{`)
						changed := append(append(append([]byte{}, b[:at]...), []byte(`"unexpected":"x",`)...), b[at:]...)
						return changed
					})
					if check(resp) {
						t.Fatal("accepted an extra setting with recomputed digests")
					}
				}
				if op.Op == pageEditOpSetText && strings.Contains(op.Text, "&") {
					resp := forgeDesignedEdit(t, entry, c, func(b []byte) []byte {
						// The alternate entity projects to the same text.
						fromJSON, _ := phpJSONString(elementorHTMLText.Replace(op.Text))
						from := []byte(fromJSON[1 : len(fromJSON)-1])
						to := bytes.Replace(from, []byte("&amp;"), []byte("&#38;"), 1)
						changed := bytes.Replace(b, from, to, 1)
						if bytes.Equal(changed, b) {
							t.Fatal("pattern text not found")
						}
						return changed
					})
					if check(resp) {
						t.Fatal("accepted different pattern text bytes with the same projection")
					}
				}
			}
			if f.design != nil && f.addsNodes() {
				for _, facts := range []string{`{}`, `{"active_breakpoints":"mobile"}`, `{"active_breakpoints":[1]}`} {
					resp := designedEditResponse(t, c)
					resp.Raw = json.RawMessage(`{"builder_facts":` + facts + `}`)
					if check(resp) {
						t.Fatalf("accepted malformed builder facts: %s", facts)
					}
				}
			}
		})
	}
}

type designedEditAgent struct {
	t     *testing.T
	c     designedEditCase
	calls []agentcmd.AbilityRunCall
}

func (a *designedEditAgent) AbilityRun(_ context.Context, _ uuid.UUID, _ string, call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error) {
	a.calls = append(a.calls, call)
	resp := designedEditResponse(a.t, a.c)
	if !resp.OK {
		if _, known := agentcmd.AbilityRunRefusalCodes[resp.Code]; !known {
			a.t.Fatalf("unknown agent refusal: %s", resp.Code)
		}
		return agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: resp.Code, Detail: resp.Detail, Retryable: resp.Retryable}
	}
	if call.RequestID.String() != a.c.RequestID || string(call.Input) != a.c.Input {
		a.t.Fatal("golden precheck sent with different request or input")
	}
	resp.PrecheckDigest = agentPrecheckDigest(a.t, call.EntrySHA256, call.Input, resp.BaseFingerprint, resp.PreviewDigest)
	return resp, nil
}

func TestDesignedPageEditThroughDecidedWriteRouter(t *testing.T) {
	_, cases := readDesignedEdits(t)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			store := &storingRequestStore{}
			dec := &recordingDecider{store: store}
			agent := &designedEditAgent{t: t, c: c}
			r, site := decidedWriteRouter(t, AbilityPageEdit, agent, store, dec, 41)
			id := uuid.MustParse(c.RequestID)
			uuid.SetRand(requestIDSource{id})
			defer uuid.SetRand(nil)
			body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"` + ToolSiteAbilityRun + `","arguments":{"site_id":"` + site.String() + `","name":"` + AbilityPageEdit + `","input":` + c.Input + `}}}`
			w := post(t, r, body, nil)
			out := w.Body.String()
			resp := designedEditResponse(t, c)
			if !resp.OK {
				var refusal struct {
					Error *struct {
						Data struct {
							Code string `json:"code"`
							Hint string `json:"hint"`
						} `json:"data"`
					} `json:"error"`
				}
				if err := json.Unmarshal([]byte(out), &refusal); err != nil {
					t.Fatal(err)
				}
				if len(store.inserts) != 0 || len(dec.calls) != 0 || refusal.Error == nil {
					t.Fatalf("refusal created a request: %.600s", out)
				}
				if resp.Code != pageCreateBadInput && refusal.Error.Data.Code != resp.Code {
					t.Fatalf("refusal code differs: %.600s", out)
				}
				if resp.Code == pageImageAltFromLibrary || resp.Code == pageBackgroundNotImage {
					if refusal.Error.Data.Code != resp.Code || refusal.Error.Data.Hint != precheckRefusalHint(resp.Code) || len(agent.calls) != 1 {
						t.Fatalf("site's image refusal lost its hint: %s", out)
					}
				}
				return
			}
			if len(store.inserts) != 1 || len(agent.calls) != 1 || len(dec.calls) != 1 {
				t.Fatalf("golden request not decided: %s", out)
			}
			ins := store.inserts[0]
			if ins.InputJson != c.Input || ins.PreviewDigest == nil || *ins.PreviewDigest != resp.PreviewDigest || ins.PrecheckDigest != agentPrecheckDigest(t, ins.EntrySha256, []byte(c.Input), resp.BaseFingerprint, resp.PreviewDigest) {
				t.Fatal("stored input or digests differ")
			}
		})
	}
}

func TestDesignedPageEditPlacement(t *testing.T) {
	entry, cases := readDesignedEdits(t)
	ran := false
	for _, c := range cases {
		if c.Name != "landing-move-a-section" && c.Name != "landing-insert-section-after-pricing" {
			continue
		}
		ran = true
		t.Run(c.Name, func(t *testing.T) {
			f, code := validatePageEditInput([]byte(c.Input))
			if code != "" {
				t.Fatal(code)
			}
			resp := forgeDesignedEdit(t, entry, c, func(raw []byte) []byte {
				tree, ok := phpDecodeDocument(raw, pageEditTreeMaxDepth, true)
				if !ok {
					t.Fatal("tree did not decode")
				}
				var pv pageEditPreview
				if err := json.Unmarshal(designedEditResponse(t, c).Preview, &pv); err != nil {
					t.Fatal(err)
				}
				var changes []pageEditChange
				if err := json.Unmarshal(pv.Changes, &changes); err != nil {
					t.Fatal(err)
				}
				ref := f.ops[0].Ref
				if ref == "" {
					ref = changes[0].NewRefs[0]
				}
				var moved any
				left := phpList{}
				for _, n := range phpValues(tree) {
					id, _ := phpStringAt(n, "id")
					if id == ref {
						moved = n
					} else {
						left = append(left, n)
					}
				}
				if moved == nil {
					t.Fatal("target is not at the top")
				}
				wrong := phpList{}
				for _, n := range left {
					id, _ := phpStringAt(n, "id")
					if id == f.ops[0].AnchorRef {
						wrong = append(wrong, moved)
					}
					wrong = append(wrong, n)
				}
				result, ok := phpEncodeBytes(wrong)
				if !ok {
					t.Fatal("wrong placement did not encode")
				}
				return result
			})
			if _, ok := verifyPageEditPrecheck(resp, entry, []byte(c.Input), f); ok {
				t.Fatal("accepted nodes before the requested after anchor")
			}
		})
	}
	if !ran {
		t.Fatal("no placement fixture ran")
	}
}
