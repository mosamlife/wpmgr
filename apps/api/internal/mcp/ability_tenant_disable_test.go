package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// switchOffRecorder plays m160's definer: the report switches the entry off
// for the reporting tenant at once.
type switchOffRecorder struct {
	ab    *fakeAbilityStore
	calls int
}

func (r *switchOffRecorder) RecordAbilityReadSideEffect(_ context.Context, _, entryID, _ uuid.UUID) (int32, error) {
	r.calls++
	if r.ab.off == nil {
		r.ab.off = map[uuid.UUID]bool{}
	}
	r.ab.off[entryID] = true
	return 1, nil
}

const vendorReadOK = `{"ok":true,"outcome":"completed","mode":"read","ability":"builder/get-page","entry_sha256":"ENTRYSHA",` +
	`"owner":{"kind":"plugin","dir":"builder","version":"2.4.1"},"abilities_invoked":["builder/get-page"],` +
	`"output":{"title":"fine"}}`

// Owner ruling 2026-10-02: the reporting tenant has the tool switched off for
// itself at once. The next call is refused before the agent is asked, with
// the closed reason and our sentence, and describe says the same.
func TestAbilityRun_VendorReadOffForTenantAfterReport(t *testing.T) {
	f, agent := vendorFixture(t)
	agent.err = &agentcmd.AbilityRunRefusal{Code: "read_side_effect_detected",
		SideEffects: &agentcmd.AbilityRunSideEffects{Options: []string{"x"}, HTTPHosts: []string{}, Blocked: []string{}}}
	rec := &switchOffRecorder{ab: f.ab}
	r := f.routerWith(t, &capturingRecorder{}, func(s *Service) {
		s.abilities.agent = agent
		s.abilities.sideEffects = rec
	})
	call := callBody(ToolSiteAbilityRun, map[string]any{"site_id": f.siteID, "name": "builder/get-page"})
	if resp := decodeRPC(t, post(t, r, call, nil)); resp.Error == nil || rec.calls != 1 || agent.calls != 1 {
		t.Fatalf("first call: err=%v recorded=%d agent=%d", resp.Error, rec.calls, agent.calls)
	}

	// The site would now answer normally; the tenant's switch must win.
	agent.err, agent.raw = nil, vendorReadOK
	w := post(t, r, call, nil)
	body := w.Body.String()
	if agent.calls != 1 {
		t.Fatalf("the agent was asked again after the tenant switch: calls=%d body=%s", agent.calls, body)
	}
	if !strings.Contains(body, notRunnableDisabledForAccount) || !strings.Contains(body, "turned this tool off for your account") {
		t.Fatalf("refusal lacks the reason or our sentence: %s", body)
	}

	w = post(t, r, callBody(ToolSiteAbilityDescribe, map[string]any{"site_id": f.siteID, "name": "builder/get-page"}), nil)
	text := toolText(t, w.Body.String())
	if !strings.Contains(text, `"not_runnable_reason":"`+notRunnableDisabledForAccount+`"`) ||
		!strings.Contains(text, `"runnable_here":false`) || !strings.Contains(text, "turned this tool off for your account") {
		t.Fatalf("describe: %s", text)
	}
}

// A switch on some other entry leaves this one alone.
func TestAbilityRun_VendorReadRunsWhenOtherEntryOff(t *testing.T) {
	f, agent := vendorFixture(t)
	agent.raw = vendorReadOK
	f.ab.off = map[uuid.UUID]bool{uuid.New(): true}
	r := f.routerWith(t, &capturingRecorder{}, func(s *Service) { s.abilities.agent = agent })
	w := post(t, r, callBody(ToolSiteAbilityRun, map[string]any{"site_id": f.siteID, "name": "builder/get-page"}), nil)
	if agent.calls != 1 || strings.Contains(w.Body.String(), notRunnableDisabledForAccount) {
		t.Fatalf("calls=%d body=%s", agent.calls, w.Body.String())
	}
}

func TestOffForTenant(t *testing.T) {
	inv := vendorInventory(uuid.New())
	e := vendorEntry()
	off := map[uuid.UUID]bool{e.EntryID: true}

	c := offForTenant(classify(e.Name, []sqlc.AbilityCatalogue{e}, &inv, vendorAgent, vendorWP), off)
	if c.runnable || c.reason == nil || *c.reason != notRunnableDisabledForAccount {
		t.Fatalf("runnable entry switched off for the tenant: %+v", c)
	}
	// A fleet-wide disable keeps its own, broader reason.
	e.Enabled = false
	c = offForTenant(classify(e.Name, []sqlc.AbilityCatalogue{e}, &inv, vendorAgent, vendorWP), off)
	if c.reason == nil || *c.reason != notRunnableDisabled {
		t.Fatalf("fleet-disabled entry: %+v", c)
	}
	// Not in the set: untouched.
	e.Enabled = true
	c = offForTenant(classify(e.Name, []sqlc.AbilityCatalogue{e}, &inv, vendorAgent, vendorWP), map[uuid.UUID]bool{})
	if !c.runnable {
		t.Fatalf("entry outside the set was switched off: %+v", c)
	}
}
