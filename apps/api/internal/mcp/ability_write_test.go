package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// The fixture is PHP's own output for this array, produced with
//
//	php -r 'echo json_encode(["wordpress_blocks","page","draft","Café / “Sale” 😀","<!-- wp:paragraph -->\n<p>a\tb</p>\u{1}"]);'
//
// so the control plane recomputes the agent's preview_digest byte for byte.
func TestPHPJSONStringArray_MatchesPHPJSONEncode(t *testing.T) {
	got, ok := phpJSONStringArray("wordpress_blocks", "page", "draft", "Café / “Sale” 😀",
		"<!-- wp:paragraph -->\n<p>a\tb</p>\x01")
	if !ok {
		t.Fatal("encoding refused valid UTF-8")
	}
	for _, frag := range []string{"Caf\\u00e9", "\\/ \\u201cSale\\u201d", "\\ud83d\\ude00", "<\\/p>\\u0001"} {
		if !strings.Contains(string(got), frag) {
			t.Fatalf("missing PHP escape %q in %s", frag, got)
		}
	}
	if sha256Hex(got) != "f90800c2888313985cfcd5ef1e4370c0fc8315b575043782df14de9b8714729c" {
		t.Fatal("digest differs from PHP's")
	}
	if _, ok := phpJSONString("bad \xff"); ok {
		t.Fatal("invalid UTF-8 was encoded; PHP's json_encode fails there")
	}
}

const testPageInput = `{"post_type":"page","editor":"wordpress_blocks","title":"Spring sale","outline":[{"type":"heading","level":2,"text":"Hi"},{"type":"paragraph","text":"Body"},{"type":"list","ordered":false,"items":["a","b"]}]}`

func TestValidatePageCreateInput(t *testing.T) {
	f, code := validatePageCreateInput([]byte(testPageInput))
	if code != "" || f.postType != "page" || f.editor != "wordpress_blocks" || f.title != "Spring sale" {
		t.Fatalf("valid input refused: %+v %q", f, code)
	}
	bad := []string{
		`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"paragraph","text":"x"}],"status":"publish"}`,
		`{"post_type":"product","editor":"wordpress_blocks","title":"T","outline":[{"type":"paragraph","text":"x"}]}`,
		`{"post_type":"page","editor":"builder:elementor","title":"T","outline":[{"type":"paragraph","text":"x"}]}`,
		`{"post_type":"page","editor":"wordpress_blocks","title":" ","outline":[{"type":"paragraph","text":"x"}]}`,
		`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[]}`,
		`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"heading","level":1,"text":"x"}]}`,
		`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"paragraph","text":"x","html":"<b>"}]}`,
		`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"list","ordered":false,"items":[]}]}`,
		`{"post_type":"page","editor":"wordpress_blocks","title":"T","outline":[{"type":"image","src":"x"}]}`,
	}
	for _, b := range bad {
		if _, code := validatePageCreateInput([]byte(b)); code == "" {
			t.Errorf("accepted %s", b)
		}
	}
}

// precheckFor builds the agent's honest precheck answer for input.
func precheckFor(t *testing.T, entrySum string, input []byte, content string) agentcmd.AbilityRunResponse {
	t.Helper()
	f, _ := validatePageCreateInput(input)
	prev, _ := phpJSONStringArray(f.editor, f.postType, "draft", f.title, content)
	base, _ := phpJSONStringArray("new_post", f.postType)
	baseFP := sha256Hex(base)
	pre, _ := phpJSONStringArray(entrySum, sha256Hex(input), baseFP, sha256Hex(prev))
	pv, _ := json.Marshal(pagePreview{PostType: f.postType, Editor: f.editor, Status: "draft", Title: f.title, Content: content})
	return agentcmd.AbilityRunResponse{
		OK: true, Mode: "precheck", Valid: true, BaseFingerprint: baseFP,
		PreviewDigest: sha256Hex(prev), PrecheckDigest: sha256Hex(pre), Preview: pv,
	}
}

func TestVerifyPageCreatePrecheck(t *testing.T) {
	entrySum := strings.Repeat("e", 64)
	input := []byte(testPageInput)
	f, _ := validatePageCreateInput(input)
	good := precheckFor(t, entrySum, input, "<!-- wp:heading -->\n<h2>Hi</h2>")
	if _, ok := verifyPageCreatePrecheck(good, entrySum, input, f); !ok {
		t.Fatal("an honest precheck was refused")
	}
	// A precheck for other input, another entry, or a preview the digest
	// does not cover is refused.
	if _, ok := verifyPageCreatePrecheck(good, strings.Repeat("f", 64), input, f); ok {
		t.Fatal("precheck for another entry accepted")
	}
	other := []byte(strings.Replace(testPageInput, "Body", "Other", 1))
	if _, ok := verifyPageCreatePrecheck(good, entrySum, other, f); ok {
		t.Fatal("precheck for other input accepted")
	}
	swapped := good
	pv, _ := json.Marshal(pagePreview{PostType: "page", Editor: "wordpress_blocks", Status: "draft", Title: "Spring sale", Content: "<p>not what was hashed</p>"})
	swapped.Preview = pv
	if _, ok := verifyPageCreatePrecheck(swapped, entrySum, input, f); ok {
		t.Fatal("a preview the digest does not cover was accepted")
	}
	published := good
	pv, _ = json.Marshal(pagePreview{PostType: "page", Editor: "wordpress_blocks", Status: "publish", Title: "Spring sale", Content: "<!-- wp:heading -->\n<h2>Hi</h2>"})
	published.Preview = pv
	if _, ok := verifyPageCreatePrecheck(published, entrySum, input, f); ok {
		t.Fatal("a non-draft preview was accepted")
	}
}

func writeEntryRow() *sqlc.AbilityCatalogue {
	perm, sum := "site.content.edit", strings.Repeat("e", 64)
	return &sqlc.AbilityCatalogue{
		Name: AbilityPageCreate, Source: "wpmgr", Class: "write", Status: "admitted", Enabled: true,
		ApprovalMode: "per_call", Snapshot: "created_post_trash", EffectCopy: "draft",
		OperatorPermission: &perm, EntrySha256: &sum,
	}
}

func TestWriteEntryRunnable(t *testing.T) {
	inv := &sqlc.SiteAbilityInventory{Name: AbilityPageCreate}
	if r := writeEntryRunnable(writeEntryRow(), inv, agentcmd.MinAgentVersionForPageCreate); r != "" {
		t.Fatalf("runnable entry refused: %s", r)
	}
	if r := writeEntryRunnable(writeEntryRow(), inv, agentcmd.MinAgentVersionForAbilityEngine); r != notRunnableAgentOutdated {
		t.Fatalf("agent below the page-create floor: got %q", r)
	}
	unstamped := writeEntryRow()
	unstamped.EntrySha256 = nil
	if r := writeEntryRunnable(unstamped, inv, agentcmd.MinAgentVersionForPageCreate); r != notRunnableNotAdmitted {
		t.Fatalf("unstamped entry: got %q", r)
	}
	noPerm := writeEntryRow()
	noPerm.OperatorPermission = nil
	if r := writeEntryRunnable(noPerm, inv, agentcmd.MinAgentVersionForPageCreate); r != notRunnableNotAdmitted {
		t.Fatalf("entry without operator_permission: got %q", r)
	}
	other := writeEntryRow()
	other.Name = "wpmgr/menu-item-add"
	if r := writeEntryRunnable(other, inv, agentcmd.MinAgentVersionForPageCreate); r != notRunnableWritesOff {
		t.Fatalf("unimplemented write: got %q", r)
	}
	if r := writeEntryRunnable(writeEntryRow(), nil, agentcmd.MinAgentVersionForPageCreate); r != notRunnableNotOnSite {
		t.Fatalf("not inventoried: got %q", r)
	}
}

func TestAbilityStateFor(t *testing.T) {
	now := time.Now()
	out := "created"
	cases := []struct {
		row  AbilityStatusRow
		want string
	}{
		{AbilityStatusRow{State: "pending", ExpiresAt: now.Add(time.Hour)}, stateWaitingForApproval},
		{AbilityStatusRow{State: "pending", ExpiresAt: now.Add(-time.Second)}, "expired"},
		{AbilityStatusRow{State: "approved"}, "approved_not_started"},
		{AbilityStatusRow{State: "dispatched"}, "running"},
		{AbilityStatusRow{State: "outcome_unknown"}, "running"},
		{AbilityStatusRow{State: "outcome_unknown", Outcome: &out}, "done"},
		{AbilityStatusRow{State: "done", Outcome: &out}, "done"},
		{AbilityStatusRow{State: "declined"}, "declined"},
	}
	for _, c := range cases {
		if got := abilityStateFor(c.row, now); got != c.want {
			t.Errorf("%s: got %s want %s", c.row.State, got, c.want)
		}
	}
}
