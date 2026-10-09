// GUT, the control plane's half of page-create layout requests, end to end
// on the E2 world: site_ability_run over HTTP with a real bearer token, then
// approve and the dispatch worker through internal/abilityrequest, all on the
// wpmgr_app pool with the real audit recorder. The fake agent answers
// precheck with the agent's own fixture answer (apps/agent/tests/fixtures/
// ability-run/page-create-layout.json), re-signed for the entry this database
// stamped. The superuser connection only arranges site state and reads counts.
package tests

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilityrequest"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

const pageLayoutAgentFixture = "../../agent/tests/fixtures/ability-run/page-create-layout.json"

// pageLayoutScenario is one precheck answer the agent fixture records.
type pageLayoutScenario struct {
	Name            string          `json:"name"`
	Input           string          `json:"input"`
	MediaIDs        []int64         `json:"media_ids"`
	BaseFingerprint string          `json:"base_fingerprint"`
	PreviewDigest   string          `json:"preview_digest"`
	Preview         json.RawMessage `json:"preview"`
}

func readPageLayoutScenario(t *testing.T, name string) pageLayoutScenario {
	t.Helper()
	b, err := os.ReadFile(pageLayoutAgentFixture)
	if err != nil {
		t.Fatalf("read %s: %v", pageLayoutAgentFixture, err)
	}
	var doc struct {
		Cases []pageLayoutScenario `json:"cases"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("decode %s: %v", pageLayoutAgentFixture, err)
	}
	for _, c := range doc.Cases {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s has no scenario %q", pageLayoutAgentFixture, name)
	return pageLayoutScenario{}
}

// answerPrecheckWith makes the fake agent answer precheck with sc's preview,
// base fingerprint and preview digest. The precheck digest binds the entry
// hash the control plane sent, so it is recomputed per call; every member of
// its preimage is lowercase hex, which PHP and Go encode alike.
func answerPrecheckWith(w *e2World, sc pageLayoutScenario) {
	w.agent.setOverride(func(call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error, bool) {
		if call.Mode != agentcmd.AbilityRunModePrecheck {
			return agentcmd.AbilityRunResponse{}, nil, false
		}
		pre := e2Hex(e2Arr(call.EntrySHA256, e2Hex(call.Input), sc.BaseFingerprint, sc.PreviewDigest))
		return agentcmd.AbilityRunResponse{
			OK: true, Mode: agentcmd.AbilityRunModePrecheck, Valid: true, BaseFingerprint: sc.BaseFingerprint,
			PreviewDigest: sc.PreviewDigest, PrecheckDigest: pre, Preview: sc.Preview,
		}, nil, true
	})
}

func (w *e2World) askPage(t *testing.T, input string) cpeRPC {
	t.Helper()
	var in map[string]any
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		t.Fatalf("input: %v", err)
	}
	return cpeCall(t, w.eng, w.bearer, mcp.ToolSiteAbilityRun, map[string]any{
		"site_id": w.site.String(), "name": mcp.AbilityPageCreate, "input": in,
	})
}

func (w *e2World) requestCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := w.admin.QueryRow(context.Background(),
		`SELECT count(*) FROM assistant_ability_requests WHERE tenant_id = $1`, w.tenant).Scan(&n); err != nil {
		t.Fatalf("count requests: %v", err)
	}
	return n
}

// TestPageLayoutRequestStoresImageFactsAsAppRole: a layout outline with
// images is stored with its images' facts in card_facts and card copy
// version 2, through the app role and the table's CHECKs; it approves with
// the digest the queue returns; and when the site's plugin goes below the
// layout floor after approval, the worker closes it not_sent/agent_outdated
// and sends nothing.
func TestPageLayoutRequestStoresImageFactsAsAppRole(t *testing.T) {
	w := newE2World(t, true)
	w.setAgentVersion(t, agentcmd.MinAgentVersionForPageLayout)
	sc := readPageLayoutScenario(t, "blocks-every-node")
	if len(sc.MediaIDs) == 0 {
		t.Fatal("the scenario has no images; it cannot prove the image path")
	}
	answerPrecheckWith(w, sc)

	res := w.askPage(t, sc.Input).wantOK(t, "ask for a layout page")
	id, err := uuid.Parse(res["request_id"].(string))
	if err != nil {
		t.Fatalf("request_id: %v", err)
	}
	rows, err := w.svc.List(context.Background(), w.person, &w.site, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.ID != id {
			continue
		}
		found = true
		if r.CardCopyVersion != mcp.AbilityCardCopyVersionLayout {
			t.Fatalf("card_copy_version = %d, want %d", r.CardCopyVersion, mcp.AbilityCardCopyVersionLayout)
		}
		media, ok := mcp.ReadPageCardFacts(r.CardFacts)
		if !ok || len(media) != len(sc.MediaIDs) {
			t.Fatalf("card_facts did not store the images: %s", r.CardFacts)
		}
		for i, m := range media {
			if m.ID != sc.MediaIDs[i] || m.Filename == "" {
				t.Fatalf("card image %d = %+v, want id %d", i, m, sc.MediaIDs[i])
			}
		}
	}
	if !found {
		t.Fatalf("request %s not in the queue", id)
	}

	if _, err := w.svc.Approve(context.Background(), w.person, w.site, id, w.digest(t, id)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	w.setAgentVersion(t, "0.61.158")
	w.dispatch(t, id)
	if n := w.agent.sent(agentcmd.AbilityRunModeWrite); n != 0 {
		t.Fatalf("a layout write was sent to a plugin below the layout floor (%d sends)", n)
	}
	state, _, notSent, _, _ := w.row(t, id)
	if state != "not_sent" || notSent == nil || *notSent != abilityrequest.ReasonAgentOutdated {
		t.Fatalf("row after dispatch: state=%s not_sent=%v, want not_sent/%s", state, notSent, abilityrequest.ReasonAgentOutdated)
	}
}

// TestPageLayoutTextOnlyRequestRunsBelowLayoutFloorAsAppRole: the layout
// floor is per input. A text-only outline on a plugin below it is asked,
// stored without card facts at copy version 1, approved and sent.
func TestPageLayoutTextOnlyRequestRunsBelowLayoutFloorAsAppRole(t *testing.T) {
	w := newE2World(t, true)
	w.setAgentVersion(t, "0.61.158")
	id := w.ask(t)
	rows, err := w.svc.List(context.Background(), w.person, &w.site, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, r := range rows {
		if r.ID == id && (r.CardFacts != nil || r.CardCopyVersion != mcp.AbilityCardCopyVersion) {
			t.Fatalf("text-only row: card_facts=%s copy_version=%d", r.CardFacts, r.CardCopyVersion)
		}
	}
	if _, err := w.svc.Approve(context.Background(), w.person, w.site, id, w.digest(t, id)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	w.dispatch(t, id)
	if n := w.agent.sent(agentcmd.AbilityRunModeWrite); n != 1 {
		t.Fatalf("text-only write sends = %d, want 1", n)
	}
	if state, outcome, _, _, _ := w.row(t, id); state != "done" || outcome == nil || *outcome != "created" {
		t.Fatalf("row after dispatch: state=%s outcome=%v", state, outcome)
	}
}

// TestPageLayoutRequestRefusedBelowLayoutFloorAsAppRole: a layout outline
// on a plugin below the floor is refused before the site is asked, with the
// floor named, and nothing is stored.
func TestPageLayoutRequestRefusedBelowLayoutFloorAsAppRole(t *testing.T) {
	w := newE2World(t, true)
	w.setAgentVersion(t, "0.61.158")
	sc := readPageLayoutScenario(t, "blocks-every-node")
	answerPrecheckWith(w, sc)
	r := w.askPage(t, sc.Input)
	r.wantErr(t, "layout outline below the layout floor", -32011, "")
	if r.data["min_agent_version"] != agentcmd.MinAgentVersionForPageLayout {
		t.Fatalf("the refusal does not name the layout floor: %v", r.data)
	}
	if n := w.agent.sent(agentcmd.AbilityRunModePrecheck); n != 0 {
		t.Fatalf("the site was asked (%d prechecks)", n)
	}
	if n := w.requestCount(t); n != 0 {
		t.Fatalf("%d requests stored", n)
	}
}
