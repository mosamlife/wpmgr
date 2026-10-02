package mcp

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
)

type sideEffectRecorderFake struct {
	calls                   int
	tenant, entry, siteSeen uuid.UUID
	n                       int32
	err                     error
}

func (f *sideEffectRecorderFake) RecordAbilityReadSideEffect(_ context.Context, tenantID, entryID, siteID uuid.UUID) (int32, error) {
	f.calls++
	f.tenant, f.entry, f.siteSeen = tenantID, entryID, siteID
	return f.n, f.err
}

// Ruling 4: a side-effecting read counts its site against the entry (m160).
// A refusal from the definer is best effort: the call stays refused and the
// audit row is still written.
func TestAbilityRun_ReadSideEffectCountsTheSite(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  *sideEffectRecorderFake
	}{
		{"counted", &sideEffectRecorderFake{n: 3}},
		{"not a vendor read", &sideEffectRecorderFake{err: &pgconn.PgError{Code: "42501"}}},
		{"no entry", &sideEffectRecorderFake{err: &pgconn.PgError{Code: "P0002"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, agent := vendorFixture(t)
			agent.err = &agentcmd.AbilityRunRefusal{Code: "read_side_effect_detected",
				SideEffects: &agentcmd.AbilityRunSideEffects{Options: []string{"x"}, HTTPHosts: []string{}, Blocked: []string{}}}
			rec := &capturingRecorder{}
			r := f.routerWith(t, rec, func(s *Service) {
				s.abilities.agent = agent
				s.abilities.sideEffects = tc.rec
			})
			w := post(t, r, callBody(ToolSiteAbilityRun, map[string]any{"site_id": f.siteID, "name": "builder/get-page"}), nil)
			if resp := decodeRPC(t, w); resp.Error == nil {
				t.Fatalf("returned a result: %s", w.Body.String())
			}
			entryID := f.ab.cat[len(f.ab.cat)-1].EntryID
			if tc.rec.calls != 1 || tc.rec.siteSeen != f.siteID || tc.rec.entry != entryID || tc.rec.tenant == uuid.Nil {
				t.Fatalf("recorder: %+v", tc.rec)
			}
			rec.only(t, ActionAbilityReadSideEffect)
		})
	}
}
