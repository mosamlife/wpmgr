package agentcmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/wpversion"
)

func TestMinAgentVersionForBuilderEditPinned(t *testing.T) {
	const firstVersionWithBuilderEdit = "0.61.162"
	if MinAgentVersionForBuilderEdit != firstVersionWithBuilderEdit {
		t.Fatalf("MinAgentVersionForBuilderEdit = %q, want %q; if the floor is re-gated, update this pin and m169's seeds in the same commit",
			MinAgentVersionForBuilderEdit, firstVersionWithBuilderEdit)
	}
	if wpversion.Compare(MinAgentVersionForBuilderEdit, MinAgentVersionForBuilderAdapters) <= 0 {
		t.Fatalf("the builder edit floor %q must be above the builder create floor %q",
			MinAgentVersionForBuilderEdit, MinAgentVersionForBuilderAdapters)
	}
}

func builderEditCall(mode string, ids []int64) AbilityRunCall {
	entry := []byte(`{"name":"wpmgr/page-structure"}`)
	return AbilityRunCall{
		Mode: mode, RequestID: uuid.MustParse("6f1c2a43-3b9e-4d4f-9a57-0c1f7d9a1b2e"),
		Entry: entry, EntrySHA256: SHA256Hex(entry), Input: []byte(`{"post_id":418}`), AllowedDraftIDs: ids,
	}
}

// TestAllowedDraftIDsInP: a nil list sends nothing, an empty list sends [],
// one id sends that id; nothing larger than the input's own post is ever
// sent, so p stays bounded however many drafts WPMgr made on a site.
func TestAllowedDraftIDsInP(t *testing.T) {
	p, _, err := BuildAbilityRunParams(builderEditCall(AbilityRunModeRead, nil))
	if err != nil || bytes.Contains(p, []byte("allowed_draft_ids")) {
		t.Fatalf("nil list: p=%s err=%v, want no allowed_draft_ids", p, err)
	}
	for mode, ids := range map[string][]int64{
		AbilityRunModeRead:     {},
		AbilityRunModePrecheck: {418},
	} {
		p, pd, err := BuildAbilityRunParams(builderEditCall(mode, ids))
		if err != nil {
			t.Fatalf("%s %v: %v", mode, ids, err)
		}
		var got struct {
			AllowedDraftIDs *[]int64 `json:"allowed_draft_ids"`
		}
		if err := json.Unmarshal(p, &got); err != nil || got.AllowedDraftIDs == nil {
			t.Fatalf("%s: p=%s has no allowed_draft_ids list", mode, p)
		}
		if len(*got.AllowedDraftIDs) != len(ids) || (len(ids) == 1 && (*got.AllowedDraftIDs)[0] != ids[0]) {
			t.Fatalf("%s: sent %v, want %v", mode, *got.AllowedDraftIDs, ids)
		}
		if pd != SHA256Hex(p) {
			t.Fatalf("%s: pd is not the digest of p", mode)
		}
	}
	if p, _, _ := BuildAbilityRunParams(builderEditCall(AbilityRunModeRead, []int64{})); !bytes.HasSuffix(p, []byte(`,"allowed_draft_ids":[]}`)) {
		t.Fatalf("an empty list is not sent as []: %s", p)
	}
	// Every draft on a site, unfiltered, is refused before anything is sent.
	many := make([]int64, 0, 5000)
	for i := int64(1); i <= 5000; i++ {
		many = append(many, i)
	}
	if _, _, err := BuildAbilityRunParams(builderEditCall(AbilityRunModePrecheck, many)); err == nil {
		t.Fatal("an unfiltered draft list was sent")
	}
	if _, _, err := BuildAbilityRunParams(builderEditCall(AbilityRunModeRead, []int64{418, 419})); err == nil {
		t.Fatal("two draft ids were sent")
	}
	if _, _, err := BuildAbilityRunParams(builderEditCall(AbilityRunModeRead, []int64{0})); err == nil {
		t.Fatal("post id 0 was sent")
	}
	ledger := builderEditCall(AbilityRunModeLedger, []int64{418})
	ledger.Entry, ledger.EntrySHA256, ledger.Input = nil, "", nil
	if _, _, err := BuildAbilityRunParams(ledger); err == nil {
		t.Fatal("allowed_draft_ids was sent with ledger")
	}
}

func TestBuilderEditRefusalCodesAreKnown(t *testing.T) {
	for _, code := range []string{"ops_invalid", "node_not_found", "node_not_editable", "op_not_supported_by_builder",
		"page_too_large", "target_not_eligible", "post_not_readable", "page_has_admin_only_content", "conflict"} {
		if _, ok := AbilityRunRefusalCodes[code]; !ok {
			t.Errorf("%s is not a known ability_run refusal code; it would arrive as unknown", code)
		}
	}
}
