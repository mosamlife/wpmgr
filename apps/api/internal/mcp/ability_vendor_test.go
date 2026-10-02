package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

// ---------------------------------------------------------------------------
// E3-G1: vendor and core reads, control-plane side.
// ---------------------------------------------------------------------------

func sp(s string) *string { return &s }
func bp(b bool) *bool     { return &b }

const vendorSchemaHex = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func vendorEntry() sqlc.AbilityCatalogue {
	e := catalogueRow("builder/get-page", "vendor", "read")
	e.OwnerDir = sp("builder")
	e.VersionMin = sp("2.0")
	e.VersionMaxTested = sp("2.4.1")
	e.SchemaStructSha256 = sp(vendorSchemaHex)
	e.OutputFields = []byte(`{"fields":{"title":"string","count":"int","items":{"items":{"fields":{"id":"int","label":"string"}}}}}`)
	return e
}

func vendorInventory(siteID uuid.UUID) sqlc.SiteAbilityInventory {
	return sqlc.SiteAbilityInventory{
		SiteID: siteID, Name: "builder/get-page", OwnerKind: "plugin", OwnerDir: sp("builder"),
		OwnerOk: bp(true), OwnerVersion: sp("2.4.1"), SchemaStructSha256: sp("sha256:" + vendorSchemaHex),
	}
}

const (
	vendorAgent = agentcmd.MinAgentVersionForVendorReads
	vendorWP    = "7.1"
)

func TestPickCatalogueEntry_VendorVersionRange(t *testing.T) {
	inv := vendorInventory(uuid.New())
	a, b := vendorEntry(), vendorEntry()
	a.VersionMin, a.VersionMaxTested = sp("1.0"), sp("1.9")
	entries := []sqlc.AbilityCatalogue{a, b}
	got, miss := pickCatalogueEntry(entries, &inv)
	if miss || got == nil || *got.VersionMin != "2.0" {
		t.Fatalf("picked %+v miss=%v, want the 2.0..2.4.1 entry", got, miss)
	}
	// Inclusive upper bound: 2.4.1 runs, 2.4.2 does not.
	inv.OwnerVersion = sp("2.4.2")
	if _, miss := pickCatalogueEntry(entries, &inv); !miss {
		t.Fatal("2.4.2 is outside every range but was picked")
	}
	c := classify("builder/get-page", entries, &inv, vendorAgent, vendorWP)
	if c.runnable || c.reason == nil || *c.reason != notRunnableVersionUnverified {
		t.Fatalf("out-of-range site: runnable=%v reason=%v", c.runnable, c.reason)
	}
}

func TestClassify_VendorReadChecks(t *testing.T) {
	siteID := uuid.New()
	cases := []struct {
		name      string
		mutE      func(*sqlc.AbilityCatalogue)
		mutI      func(*sqlc.SiteAbilityInventory)
		agent, wp string
		want      string
	}{
		{name: "runs", want: ""},
		{name: "write", mutE: func(e *sqlc.AbilityCatalogue) { e.Class = "write" }, want: notRunnableNotYet},
		{name: "asserted", mutE: func(e *sqlc.AbilityCatalogue) { e.PermissionMode = "asserted" }, want: notRunnablePermissionMode},
		{name: "no shape", mutE: func(e *sqlc.AbilityCatalogue) { e.OutputFields = nil }, want: notRunnableNotAdmitted},
		{name: "bad shape", mutE: func(e *sqlc.AbilityCatalogue) { e.OutputFields = []byte(`{"object":{}}`) }, want: notRunnableNotAdmitted},
		{name: "owner not ok", mutI: func(i *sqlc.SiteAbilityInventory) { i.OwnerOk = bp(false) }, want: notRunnableOwnerMismatch},
		{name: "owner unknown", mutI: func(i *sqlc.SiteAbilityInventory) { i.OwnerOk = nil }, want: notRunnableOwnerMismatch},
		{name: "other dir", mutI: func(i *sqlc.SiteAbilityInventory) { i.OwnerDir = sp("builder-evil") }, want: notRunnableOwnerMismatch},
		{name: "theme kind", mutI: func(i *sqlc.SiteAbilityInventory) { i.OwnerKind = "theme" }, want: notRunnableOwnerMismatch},
		{name: "schema", mutI: func(i *sqlc.SiteAbilityInventory) { i.SchemaStructSha256 = sp("sha256:" + strings.Repeat("b", 64)) }, want: notRunnableSchemaChanged},
		{name: "wp 7.0", wp: "7.0.2", want: notRunnableWPTooOld},
		{name: "wp blank", wp: "", want: notRunnableWPTooOld},
		{name: "agent 0.61.157", agent: "0.61.157", want: notRunnableAgentOutdated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, inv := vendorEntry(), vendorInventory(siteID)
			if tc.mutE != nil {
				tc.mutE(&e)
			}
			if tc.mutI != nil {
				tc.mutI(&inv)
			}
			agent, wp := vendorAgent, vendorWP
			if tc.agent != "" {
				agent = tc.agent
			}
			if tc.name == "wp 7.0" || tc.name == "wp blank" {
				wp = tc.wp
			}
			c := classify(e.Name, []sqlc.AbilityCatalogue{e}, &inv, agent, wp)
			got := ""
			if c.reason != nil {
				got = *c.reason
			}
			if got != tc.want || c.runnable != (tc.want == "") {
				t.Fatalf("reason=%q runnable=%v, want %q", got, c.runnable, tc.want)
			}
		})
	}
}

// PR1: only listed keys survive, every string leaf is fenced, and a value of
// the wrong kind is dropped.
func TestFenceAbilityOutput_VendorPinnedShape(t *testing.T) {
	e := vendorEntry()
	raw := `{"title":"` + plantedInstruction + `","count":3,"admin_email":"owner@example.test",` +
		`"items":[{"id":1,"label":"` + plantedInstruction + `","secret":"x"},{"id":"2","label":7}]}`
	out, truncated := fenceAbilityOutput(e.Name, &e, json.RawMessage(raw), 1<<16)
	if truncated {
		t.Fatal("truncated")
	}
	s := string(out)
	if strings.Contains(s, "admin_email") || strings.Contains(s, "owner@example.test") || strings.Contains(s, "secret") {
		t.Fatalf("an unlisted key reached the answer: %s", s)
	}
	if strings.Count(s, siteTextMarker+plantedInstruction) != 2 || strings.Count(s, plantedInstruction) != 2 {
		t.Fatalf("string leaves not fenced: %s", s)
	}
	var v struct {
		Count int
		Items []map[string]any
	}
	_ = json.Unmarshal(out, &v)
	if v.Count != 3 || v.Items[1]["id"] != nil || v.Items[1]["label"] != nil {
		t.Fatalf("typed leaves not enforced: %s", s)
	}

	// An empty shape keeps nothing.
	e.OutputFields = []byte(`{"fields":{}}`)
	out, _ = fenceAbilityOutput(e.Name, &e, json.RawMessage(`{"admin_email":"owner@example.test"}`), 1<<16)
	if string(out) != `{}` {
		t.Fatalf("empty shape: %s", out)
	}
	// A vendor entry with no valid shape returns nothing at all.
	e.OutputFields = nil
	if out, _ := fenceAbilityOutput(e.Name, &e, json.RawMessage(`{"a":"b"}`), 1<<16); string(out) != "null" {
		t.Fatalf("no shape: %s", out)
	}
}

type vendorAgentFake struct {
	calls int
	raw   string
	err   error
}

func (f *vendorAgentFake) AbilityRun(_ context.Context, _ uuid.UUID, _ string, call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error) {
	f.calls++
	if f.err != nil {
		return agentcmd.AbilityRunResponse{}, f.err
	}
	raw := strings.ReplaceAll(f.raw, "ENTRYSHA", call.EntrySHA256)
	var resp agentcmd.AbilityRunResponse
	_ = json.Unmarshal([]byte(raw), &resp)
	resp.Raw = json.RawMessage(raw)
	return resp, nil
}

func vendorFixture(t *testing.T) (abilityFixture, *vendorAgentFake) {
	f := newAbilityFixture(t)
	f.store.sites[0].AgentVersion = vendorAgent
	f.store.sites[0].WpVersion = vendorWP
	f.ab.rows = append(f.ab.rows, vendorInventory(f.siteID))
	f.ab.cat = append(f.ab.cat, vendorEntry())
	return f, &vendorAgentFake{}
}

func TestAbilityRun_VendorReadRunsAndFencesOutput(t *testing.T) {
	f, agent := vendorFixture(t)
	agent.raw = `{"ok":true,"outcome":"completed","mode":"read","ability":"builder/get-page","entry_sha256":"ENTRYSHA",` +
		`"owner":{"kind":"plugin","dir":"builder","version":"2.4.1"},"abilities_invoked":["builder/get-page"],` +
		`"output":{"title":"` + plantedInstruction + `","admin_email":"owner@example.test"}}`
	r := f.routerWith(t, &capturingRecorder{}, func(s *Service) { s.abilities.agent = agent })
	w := post(t, r, callBody(ToolSiteAbilityRun, map[string]any{"site_id": f.siteID, "name": "builder/get-page"}), nil)
	text := toolText(t, w.Body.String())
	if agent.calls != 1 || !strings.Contains(text, siteTextMarker+plantedInstruction) || strings.Contains(text, "admin_email") {
		t.Fatalf("calls=%d text=%s", agent.calls, text)
	}
	if !strings.Contains(text, `"version":"`+siteTextMarker+`2.4.1"`) {
		t.Fatalf("owner version not fenced: %s", text)
	}
}

func TestAbilityRun_VendorReplyOffContractIsWithheld(t *testing.T) {
	f, agent := vendorFixture(t)
	agent.raw = `{"ok":true,"outcome":"completed","mode":"read","ability":"builder/get-page","entry_sha256":"ENTRYSHA",` +
		`"owner":{"kind":"plugin","dir":"builder","version":"2.4.1"},"abilities_invoked":[],` +
		`"output":{"title":"` + abilityCanary + `"},"note":"` + plantedInstruction + `"}`
	r := f.routerWith(t, &capturingRecorder{}, func(s *Service) { s.abilities.agent = agent })
	w := post(t, r, callBody(ToolSiteAbilityRun, map[string]any{"site_id": f.siteID, "name": "builder/get-page"}), nil)
	if strings.Contains(w.Body.String(), abilityCanary) || strings.Contains(w.Body.String(), plantedInstruction) {
		t.Fatalf("an off-contract reply reached the model: %s", w.Body.String())
	}
}

// Every site-text field of a refusal is fenced and only inside
// from_the_site; our sentence carries none of it.
func TestVendorRefusal_SiteTextFenced(t *testing.T) {
	r := &agentcmd.AbilityRunRefusal{
		Code: "read_side_effect_detected", Violations: []string{"short_circuit"},
		ErrorCode: plantedInstruction,
		SideEffects: &agentcmd.AbilityRunSideEffects{
			Options: []string{plantedInstruction}, HTTPHosts: []string{plantedInstruction},
			Posts: 1, Blocked: []string{"user_capabilities_meta"},
		},
	}
	ref := vendorRefusal(r)
	if strings.Contains(ref.err.Message, plantedInstruction) {
		t.Fatalf("site text in our sentence: %s", ref.err.Message)
	}
	b, _ := json.Marshal(ref.err.Details)
	s := string(b)
	if strings.Count(s, plantedInstruction) != 3 || strings.Count(s, siteTextMarker+plantedInstruction) != 3 {
		t.Fatalf("details: %s", s)
	}
	fs, _ := json.Marshal(ref.err.Details["from_the_site"])
	if strings.Count(string(fs), plantedInstruction) != 3 {
		t.Fatalf("site text outside from_the_site: %s", s)
	}
	for code := range agentcmd.AbilityRunRefusalCodes {
		_ = code
	}
	for code := range agentRefusalCopy {
		if _, ok := agentcmd.AbilityRunRefusalCodes[code]; !ok {
			t.Fatalf("copy for a code outside the closed set: %s", code)
		}
	}
}

func TestAbilityRun_ReadSideEffectIsAudited(t *testing.T) {
	f, agent := vendorFixture(t)
	agent.err = &agentcmd.AbilityRunRefusal{
		Code: "read_side_effect_detected",
		SideEffects: &agentcmd.AbilityRunSideEffects{
			Options: []string{"evil_option"}, HTTPHosts: []string{}, Blocked: []string{},
		},
	}
	rec := &capturingRecorder{}
	r := f.routerWith(t, rec, func(s *Service) { s.abilities.agent = agent })
	w := post(t, r, callBody(ToolSiteAbilityRun, map[string]any{"site_id": f.siteID, "name": "builder/get-page"}), nil)
	if resp := decodeRPC(t, w); resp.Error == nil {
		t.Fatalf("a side-effecting read returned a result: %s", w.Body.String())
	}
	e := rec.only(t, ActionAbilityReadSideEffect)
	if e.TargetID == "" || !strings.Contains(mustJSON(e.Metadata), siteTextMarker+"evil_option") {
		t.Fatalf("audit row: %+v", e)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// R5: a cursor sealed for site A is refused for site B in the same tenant
// and grant.
func TestDiscoverCursor_BoundToSite(t *testing.T) {
	eng := &abilityEngine{cursorKey: []byte("k")}
	grant, a, b, snap := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	c := eng.sealCursor(grant, a, discoverFilters{}, snap, 5)
	if _, _, ok := eng.openCursor(grant, a, discoverFilters{}, c); !ok {
		t.Fatal("own cursor refused")
	}
	if _, _, ok := eng.openCursor(grant, b, discoverFilters{}, c); ok {
		t.Fatal("site A's cursor opened for site B")
	}
}

func TestEnableAbilityTools_RefusesWithoutCursorKey(t *testing.T) {
	f := newAbilityFixture(t)
	svc := NewService(f.store).withAuditRecorder(&capturingRecorder{})
	if err := svc.EnableAbilityTools(f.ab, f.agent, testEntryEncoder, " "); err == nil {
		t.Fatal("enabled with no cursor key")
	}
}
