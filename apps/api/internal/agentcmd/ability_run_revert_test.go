package agentcmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// BF-E G2c: p.revert is the undo's signed parameters. The agent accepts
// exactly {"snapshot_sha256": <lowercase hex64>} for a page-edit undo and
// exactly {"chain": [<at most 100 distinct lowercase request ids>]} (or
// nothing) for a page-create undo; the bytes built here are those shapes and
// nothing else.

func revertCall(r *AbilityRunRevert) AbilityRunCall {
	e, sum := writeEntry()
	return AbilityRunCall{
		Mode: AbilityRunModeRevert, RequestID: uuid.MustParse("6f1c2b8e-4a3d-4c5e-9f7a-0b1c2d3e4f50"),
		Entry: e, EntrySHA256: sum, Revert: r,
	}
}

// revertP is the p text BuildAbilityRunParams must produce for call with the
// given p.revert member text ("" for none).
func revertP(t *testing.T, call AbilityRunCall, revert string) string {
	t.Helper()
	entry, err := json.Marshal(string(call.Entry))
	if err != nil {
		t.Fatal(err)
	}
	p := fmt.Sprintf(`{"mode":"revert","request_id":"%s","entry":%s,"entry_sha256":"%s"`, call.RequestID, entry, call.EntrySHA256)
	if revert != "" {
		p += `,"revert":` + revert
	}
	return p + "}"
}

func TestBuildAbilityRunParams_RevertCarriesTheSignedParameters(t *testing.T) {
	hash := strings.Repeat("0f", 32)
	a := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	b := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	for _, c := range []struct {
		name   string
		revert *AbilityRunRevert
		want   string
	}{
		{"no parameters", nil, ""},
		{"page edit", &AbilityRunRevert{SnapshotSHA256: hash}, `{"snapshot_sha256":"` + hash + `"}`},
		{"chain, in the order given", &AbilityRunRevert{Chain: []uuid.UUID{b, a}}, `{"chain":["` + b.String() + `","` + a.String() + `"]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			call := revertCall(c.revert)
			p, pd, err := BuildAbilityRunParams(call)
			if err != nil {
				t.Fatal(err)
			}
			if want := revertP(t, call, c.want); string(p) != want {
				t.Fatalf("p =\n%s\nwant\n%s", p, want)
			}
			if pd != SHA256Hex(p) {
				t.Fatal("pd is not the hash of the p bytes")
			}
		})
	}

	// A full chain is sent; the ids are the lowercase form the agent reads.
	full := make([]uuid.UUID, AbilityRunMaxRevertChain)
	for i := range full {
		full[i] = uuid.New()
	}
	p, _, err := BuildAbilityRunParams(revertCall(&AbilityRunRevert{Chain: full}))
	if err != nil {
		t.Fatalf("a chain of %d: %v", AbilityRunMaxRevertChain, err)
	}
	var got struct {
		Revert struct {
			Chain []string `json:"chain"`
		} `json:"revert"`
	}
	if err := json.Unmarshal(p, &got); err != nil || len(got.Revert.Chain) != len(full) {
		t.Fatalf("chain not sent in full: %v %d", err, len(got.Revert.Chain))
	}
	for i, id := range got.Revert.Chain {
		if id != full[i].String() || id != strings.ToLower(id) {
			t.Fatalf("chain[%d] = %q, want %q", i, id, full[i])
		}
	}
}

func TestBuildAbilityRunParams_RevertParametersRefused(t *testing.T) {
	hash := strings.Repeat("0f", 32)
	a := uuid.New()
	long := make([]uuid.UUID, AbilityRunMaxRevertChain+1)
	for i := range long {
		long[i] = uuid.New()
	}
	for name, r := range map[string]*AbilityRunRevert{
		"empty":            {},
		"empty chain":      {Chain: []uuid.UUID{}},
		"both":             {SnapshotSHA256: hash, Chain: []uuid.UUID{a}},
		"uppercase hash":   {SnapshotSHA256: strings.ToUpper(hash)},
		"short hash":       {SnapshotSHA256: hash[:63]},
		"not hex":          {SnapshotSHA256: strings.Repeat("zz", 32)},
		"chain too long":   {Chain: long},
		"chain duplicate":  {Chain: []uuid.UUID{a, uuid.New(), a}},
		"chain nil member": {Chain: []uuid.UUID{a, uuid.Nil}},
	} {
		if p, _, err := BuildAbilityRunParams(revertCall(r)); err == nil {
			t.Errorf("%s: built %s", name, p)
		}
	}

	// Only a revert carries them.
	for _, mode := range []string{AbilityRunModeRead, AbilityRunModePrecheck, AbilityRunModeLedger, AbilityRunModeWrite} {
		call := revertCall(&AbilityRunRevert{SnapshotSHA256: hash})
		call.Mode = mode
		if mode == AbilityRunModeWrite {
			call.Expected = &AbilityRunExpected{PrecheckDigest: hash, PreviewDigest: hash}
		}
		if p, _, err := BuildAbilityRunParams(call); err == nil {
			t.Errorf("%s with revert parameters: built %s", mode, p)
		}
	}
}

// The page-edit undo's refusals keep their codes; an unknown code would
// read as "unknown" and lose what the site said.
func TestAbilityRunRefusal_PageEditUndoCodesKnown(t *testing.T) {
	for _, code := range []string{"refused_published", "target_not_draft", "snapshot_unreadable", "snapshot_tampered", "restore_mismatch", "created_post_touched", "conflict"} {
		if got := abilityRunRefusalOf(AbilityRunResponse{Code: code}); got.Code != code {
			t.Errorf("%s decoded as %q", code, got.Code)
		}
	}
}
