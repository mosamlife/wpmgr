package agentcmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestMinAgentVersionForPageCreate_Pinned pins the literal: the first agent
// release that ships ability_run write/revert and content_editing_enable.
func TestMinAgentVersionForPageCreate_Pinned(t *testing.T) {
	const firstVersionWithPageCreate = "0.61.156"
	if MinAgentVersionForPageCreate != firstVersionWithPageCreate {
		t.Fatalf("MinAgentVersionForPageCreate = %q, want %q; if the floor is re-gated, update this pin in the same commit",
			MinAgentVersionForPageCreate, firstVersionWithPageCreate)
	}
}

func writeEntry() ([]byte, string) {
	e := []byte(`{"name":"wpmgr/page-create","source":"wpmgr","class":"write","status":"admitted","enabled":true,"approval":"per_call","snapshot":"created_post_trash"}`)
	return e, SHA256Hex(e)
}

func TestBuildAbilityRunParams_WriteCarriesExpected(t *testing.T) {
	e, sum := writeEntry()
	pre, prev := strings.Repeat("a", 64), strings.Repeat("b", 64)
	p, pd, err := BuildAbilityRunParams(AbilityRunCall{
		Mode: AbilityRunModeWrite, RequestID: uuid.New(), Entry: e, EntrySHA256: sum,
		Input:    []byte(`{"post_type":"page"}`),
		Expected: &AbilityRunExpected{PrecheckDigest: pre, PreviewDigest: prev},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pd != SHA256Hex(p) {
		t.Fatal("pd is not sha256(p)")
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(p, &got); err != nil {
		t.Fatal(err)
	}
	var exp AbilityRunExpected
	if err := json.Unmarshal(got["expected"], &exp); err != nil || exp.PrecheckDigest != pre || exp.PreviewDigest != prev {
		t.Fatalf("expected member wrong: %s", got["expected"])
	}
	var input string
	_ = json.Unmarshal(got["input"], &input)
	if input != `{"post_type":"page"}` {
		t.Fatalf("input text not carried verbatim: %q", input)
	}
}

func TestBuildAbilityRunParams_WriteRefusedWithoutExpected(t *testing.T) {
	e, sum := writeEntry()
	for _, exp := range []*AbilityRunExpected{nil, {PrecheckDigest: "x", PreviewDigest: strings.Repeat("b", 64)}} {
		if _, _, err := BuildAbilityRunParams(AbilityRunCall{
			Mode: AbilityRunModeWrite, RequestID: uuid.New(), Entry: e, EntrySHA256: sum, Expected: exp,
		}); err == nil {
			t.Fatalf("write with expected %+v was built", exp)
		}
	}
}

func TestBuildAbilityRunParams_ExpectedOnlyOnWrite(t *testing.T) {
	e, sum := writeEntry()
	exp := &AbilityRunExpected{PrecheckDigest: strings.Repeat("a", 64), PreviewDigest: strings.Repeat("b", 64)}
	if _, _, err := BuildAbilityRunParams(AbilityRunCall{
		Mode: AbilityRunModePrecheck, RequestID: uuid.New(), Entry: e, EntrySHA256: sum, Expected: exp,
	}); err == nil {
		t.Fatal("precheck with expected was built")
	}
}

// W3: revert carries no input; the agent reads the object from its ledger.
func TestBuildAbilityRunParams_RevertTakesNoInput(t *testing.T) {
	e, sum := writeEntry()
	if _, _, err := BuildAbilityRunParams(AbilityRunCall{
		Mode: AbilityRunModeRevert, RequestID: uuid.New(), Entry: e, EntrySHA256: sum,
		Input: []byte(`{"post_id":12}`),
	}); err == nil {
		t.Fatal("revert with input was built")
	}
	p, _, err := BuildAbilityRunParams(AbilityRunCall{
		Mode: AbilityRunModeRevert, RequestID: uuid.New(), Entry: e, EntrySHA256: sum,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	_ = json.Unmarshal(p, &got)
	if _, has := got["input"]; has {
		t.Fatalf("revert p carries input: %s", p)
	}
	if _, has := got["expected"]; has {
		t.Fatalf("revert p carries expected: %s", p)
	}
}

func TestAbilityRunRefusalOf_KeepsVerifyMismatchFacts(t *testing.T) {
	r := abilityRunRefusalOf(AbilityRunResponse{Code: "verify_mismatch", PostID: 41, Trashed: true})
	if r.Code != "verify_mismatch" || r.PostID != 41 || !r.Trashed {
		t.Fatalf("got %+v", r)
	}
	if abilityRunRefusalOf(AbilityRunResponse{Code: "made_up"}).Code != "unknown" {
		t.Fatal("an unknown code was passed through")
	}
}
