// BF-E G2c: a page edit's undo and the undo of a page-builder draft WPMgr
// has since edited, through internal/abilityrequest's service on the
// wpmgr_app pool with the real audit recorder (newE2World). Request rows are
// seeded as wpmgr_app through the shipped insert, approve and outcome
// statements (m169's helpers) against the real catalogue entries, so the
// undo encodes and sends the entry as production does. The fake agent stands
// in for the site: it keeps the hash of each edit's copy and the edits its
// ledger records on the draft, and refuses what the agent refuses.
package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilityrequest"
	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// g2cSite is the site's side of an undo.
type g2cSite struct {
	// copies is the hash of the copy each page edit kept, by request id.
	copies map[uuid.UUID]string
	// ledger is each draft's applied page edits as the site recorded them,
	// in the order they were applied, by the request that created the draft.
	ledger map[uuid.UUID][]uuid.UUID
}

// answer is the agent's revert: a page edit's undo runs only when the hash
// sent is its copy's; a draft's undo trashes it only when the chain sent is
// exactly the edits the site recorded.
func (s *g2cSite) answer(call agentcmd.AbilityRunCall) (agentcmd.AbilityRunResponse, error, bool) {
	if call.Mode != agentcmd.AbilityRunModeRevert {
		return agentcmd.AbilityRunResponse{}, nil, false
	}
	if kept, isEdit := s.copies[call.RequestID]; isEdit {
		if call.Revert == nil || call.Revert.Chain != nil {
			return agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: "bad_params"}, true
		}
		if call.Revert.SnapshotSHA256 != kept {
			return agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: "snapshot_tampered"}, true
		}
		return agentcmd.AbilityRunResponse{OK: true, Outcome: "reverted", Mode: "revert", PostID: 42}, nil, true
	}
	var chain []uuid.UUID
	if call.Revert != nil {
		if call.Revert.SnapshotSHA256 != "" {
			return agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: "bad_params"}, true
		}
		chain = call.Revert.Chain
	}
	if fmt.Sprint(chain) != fmt.Sprint(s.ledger[call.RequestID]) {
		return agentcmd.AbilityRunResponse{}, &agentcmd.AbilityRunRefusal{Code: "created_post_touched", Detail: "chain_broken"}, true
	}
	return agentcmd.AbilityRunResponse{OK: true, Outcome: "reverted", Mode: "revert", PostID: 42, Trashed: true}, nil, true
}

// g2cEntryID is the catalogue entry named name, read as wpmgr_app.
func g2cEntryID(t *testing.T, w *e2World, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := w.pool.InTenantTx(context.Background(), w.tenant, func(tx pgx.Tx) error {
		mcpAssertAndReportRole(t, tx, "InTenantTx (g2c catalogue read)")
		id = m157Entry(t, tx, name).EntryID
		return nil
	}); err != nil {
		t.Fatalf("read entry %s: %v", name, err)
	}
	return id
}

// g2cCreated is a done wpmgr/page-create of post on w's site.
func g2cCreated(t *testing.T, w *e2World, post int64, seed string) sqlc.AssistantAbilityRequest {
	t.Helper()
	p := aarParams(w.tenant, w.site, uuid.New(), seed)
	p.EntryID = g2cEntryID(t, w, mcp.AbilityPageCreate)
	row := aarInsert(t, w.pool, acprSitePrincipal(w.tenant, w.site), p)
	m169Dispatch(t, w.pool, row)
	if n, err := m169Outcome(t, w.pool, w.tenant, row.ID, sqlc.RecordAbilityRequestOutcomeParams{
		Outcome: "created", CreatedPostID: &post, UndoAvailableUntil: m169Window(),
	}); err != nil || n != 1 {
		t.Fatalf("record created %s: n=%d err=%v", seed, n, err)
	}
	return row
}

// g2cSent is a wpmgr/page-edit of post on w's site, sent and not answered.
func g2cSent(t *testing.T, w *e2World, post int64, seed string) sqlc.AssistantAbilityRequest {
	t.Helper()
	p := m169EditParams(w.tenant, w.site, uuid.New(), post, seed)
	p.EntryID = g2cEntryID(t, w, mcp.AbilityPageEdit)
	row := aarInsert(t, w.pool, acprSitePrincipal(w.tenant, w.site), p)
	m169Dispatch(t, w.pool, row)
	return row
}

// g2cApplied is an applied wpmgr/page-edit of post whose outcome recorded
// hash, with its undo open.
func g2cApplied(t *testing.T, w *e2World, post int64, seed, hash string) sqlc.AssistantAbilityRequest {
	t.Helper()
	row := g2cSent(t, w, post, seed)
	g2cAnswer(t, w, row, hash)
	return row
}

// g2cAnswer records the applied outcome of a sent edit.
func g2cAnswer(t *testing.T, w *e2World, row sqlc.AssistantAbilityRequest, hash string) {
	t.Helper()
	if n, err := m169Outcome(t, w.pool, w.tenant, row.ID, sqlc.RecordAbilityRequestOutcomeParams{
		Outcome: "applied", SnapshotSha256: &hash, UndoAvailableUntil: m169Window(),
	}); err != nil || n != 1 {
		t.Fatalf("record applied %s: n=%d err=%v", row.ID, n, err)
	}
}

// g2cOffered is undo_offered for each id, as the queue computes it.
func g2cOffered(t *testing.T, w *e2World, ids ...uuid.UUID) map[uuid.UUID]bool {
	t.Helper()
	ctx := context.Background()
	rows, err := w.svc.List(ctx, w.person, &w.site, 100, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	versions := w.svc.AgentVersions(ctx, w.person, rows)
	newest := w.svc.NewestEdits(ctx, w.person, rows)
	out := map[uuid.UUID]bool{}
	for _, r := range rows {
		out[r.ID] = abilityrequest.UndoOffered(r, versions[r.SiteID], newest[r.ID], time.Now())
	}
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			t.Fatalf("request %s not listed", id)
		}
	}
	return out
}

func g2cUndoState(t *testing.T, w *e2World, id uuid.UUID) string {
	t.Helper()
	_, _, _, undo, _ := w.row(t, id)
	if undo == nil {
		return ""
	}
	return *undo
}

// g2cStr is an undo state for a failure message.
func g2cStr(p *string) string {
	if p == nil {
		return "<none>"
	}
	return *p
}

func g2cRefusal(t *testing.T, err error, code, message string) {
	t.Helper()
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != code || de.Message != message {
		t.Fatalf("got %v, want %s %q", err, code, message)
	}
}

// TestPageEditUndoSendsTheRecordedHashAsAppRole: the undo sends the snapshot
// hash the applied outcome recorded and nothing else; the site puts the
// change back. When the site's copy no longer hashes to it, the site answers
// snapshot_tampered and the undo is recorded as failed.
func TestPageEditUndoSendsTheRecordedHashAsAppRole(t *testing.T) {
	ctx := context.Background()
	w := newE2World(t, true)
	g2cCreated(t, w, 42, "g2c-hash-create")
	h1, h2 := acprHex("g2c-copy-1"), acprHex("g2c-copy-2")
	e1 := g2cApplied(t, w, 42, "g2c-hash-1", h1)
	site := &g2cSite{copies: map[uuid.UUID]string{e1.ID: h1}}
	w.agent.setOverride(site.answer)

	got, err := w.svc.Undo(ctx, w.person, w.site, e1.ID)
	if err != nil || got.UndoState == nil || *got.UndoState != abilityrequest.UndoDone {
		t.Fatalf("undo of the edit: %v %v", g2cStr(got.UndoState), err)
	}
	last := w.agent.last
	if last.Mode != agentcmd.AbilityRunModeRevert || last.RequestID != e1.ID || len(last.Input) != 0 ||
		last.Revert == nil || last.Revert.SnapshotSHA256 != h1 || last.Revert.Chain != nil {
		t.Fatalf("revert sent %+v (revert %+v), want only the recorded hash", last, last.Revert)
	}

	e2 := g2cApplied(t, w, 42, "g2c-hash-2", h2)
	site.copies[e2.ID] = acprHex("g2c-copy-rewritten")
	got, err = w.svc.Undo(ctx, w.person, w.site, e2.ID)
	if err != nil || got.UndoState == nil || *got.UndoState != abilityrequest.UndoFailed {
		t.Fatalf("undo against a rewritten copy: %v %v, want failed", g2cStr(got.UndoState), err)
	}
	if w.agent.last.Revert == nil || w.agent.last.Revert.SnapshotSHA256 != h2 {
		t.Fatalf("second undo sent %+v, want the hash its outcome recorded", w.agent.last.Revert)
	}
}

// TestPageEditUndoNewestFirstAsAppRole: only the newest applied edit of a
// page offers and starts its undo; an older one is refused with "Undo the
// later change first." and nothing is sent. Once the later edit is undone,
// the older one is offered and runs.
func TestPageEditUndoNewestFirstAsAppRole(t *testing.T) {
	ctx := context.Background()
	w := newE2World(t, true)
	g2cCreated(t, w, 42, "g2c-order-create")
	h1, h2 := acprHex("g2c-order-1"), acprHex("g2c-order-2")
	e1 := g2cApplied(t, w, 42, "g2c-order-1", h1)
	e2 := g2cApplied(t, w, 42, "g2c-order-2", h2)
	site := &g2cSite{copies: map[uuid.UUID]string{e1.ID: h1, e2.ID: h2}}
	w.agent.setOverride(site.answer)

	offered := g2cOffered(t, w, e1.ID, e2.ID)
	if offered[e1.ID] || !offered[e2.ID] {
		t.Fatalf("undo_offered: older %v newer %v, want only the newer", offered[e1.ID], offered[e2.ID])
	}
	_, err := w.svc.Undo(ctx, w.person, w.site, e1.ID)
	g2cRefusal(t, err, abilityrequest.CodeUndoUnavailable, "Undo the later change first.")
	if n := w.agent.sent(agentcmd.AbilityRunModeRevert); n != 0 {
		t.Fatalf("%d reverts sent for a refused undo", n)
	}
	if s := g2cUndoState(t, w, e1.ID); s != "available" {
		t.Fatalf("older edit's undo_state = %q after the refusal, want available", s)
	}

	if got, err := w.svc.Undo(ctx, w.person, w.site, e2.ID); err != nil || got.UndoState == nil || *got.UndoState != abilityrequest.UndoDone {
		t.Fatalf("undo of the newer edit: %v %v", g2cStr(got.UndoState), err)
	}
	offered = g2cOffered(t, w, e1.ID, e2.ID)
	if !offered[e1.ID] || offered[e2.ID] {
		t.Fatalf("after the newer undo: older %v newer %v, want only the older", offered[e1.ID], offered[e2.ID])
	}
	if got, err := w.svc.Undo(ctx, w.person, w.site, e1.ID); err != nil || got.UndoState == nil || *got.UndoState != abilityrequest.UndoDone {
		t.Fatalf("undo of the older edit: %v %v", g2cStr(got.UndoState), err)
	}
	if w.agent.last.Revert == nil || w.agent.last.Revert.SnapshotSHA256 != h1 {
		t.Fatalf("older undo sent %+v, want its own hash", w.agent.last.Revert)
	}
}

// TestPageEditChainTrashNamesEveryAppliedEditAsAppRole: the undo of a
// builder draft names every applied edit of it in the order applied, an
// undone one included and a failed one left out; the site trashes the draft
// only for that exact chain. A draft with no edit sends no chain.
func TestPageEditChainTrashNamesEveryAppliedEditAsAppRole(t *testing.T) {
	ctx := context.Background()
	w := newE2World(t, true)
	create := g2cCreated(t, w, 42, "g2c-chain-create")
	h1, h2, h4 := acprHex("g2c-chain-1"), acprHex("g2c-chain-2"), acprHex("g2c-chain-4")
	e1 := g2cApplied(t, w, 42, "g2c-chain-1", h1)
	e2 := g2cApplied(t, w, 42, "g2c-chain-2", h2)
	site := &g2cSite{copies: map[uuid.UUID]string{e1.ID: h1, e2.ID: h2}}
	w.agent.setOverride(site.answer)
	if got, err := w.svc.Undo(ctx, w.person, w.site, e2.ID); err != nil || got.UndoState == nil || *got.UndoState != abilityrequest.UndoDone {
		t.Fatalf("undo of e2: %v %v", g2cStr(got.UndoState), err)
	}
	e3 := g2cSent(t, w, 42, "g2c-chain-3-failed")
	if n, err := m169Outcome(t, w.pool, w.tenant, e3.ID, sqlc.RecordAbilityRequestOutcomeParams{Outcome: "refused", OutcomeCode: acprStr("conflict")}); err != nil || n != 1 {
		t.Fatalf("record e3 refused: n=%d err=%v", n, err)
	}
	e4 := g2cApplied(t, w, 42, "g2c-chain-4", h4)
	// Another post's edit is not this draft's.
	g2cApplied(t, w, 43, "g2c-chain-other-post", acprHex("g2c-chain-other"))
	site.ledger = map[uuid.UUID][]uuid.UUID{create.ID: {e1.ID, e2.ID, e4.ID}}

	got, err := w.svc.Undo(ctx, w.person, w.site, create.ID)
	if err != nil || got.UndoState == nil || *got.UndoState != abilityrequest.UndoDone {
		t.Fatalf("chain trash: %v %v (sent %+v)", g2cStr(got.UndoState), err, w.agent.last.Revert)
	}
	if r := w.agent.last.Revert; r == nil || fmt.Sprint(r.Chain) != fmt.Sprint(site.ledger[create.ID]) || r.SnapshotSHA256 != "" {
		t.Fatalf("chain sent %+v, want %v", r, site.ledger[create.ID])
	}

	plain := g2cCreated(t, w, 44, "g2c-chain-no-edits")
	if got, err := w.svc.Undo(ctx, w.person, w.site, plain.ID); err != nil || got.UndoState == nil || *got.UndoState != abilityrequest.UndoDone {
		t.Fatalf("undo of a draft with no edits: %v %v", g2cStr(got.UndoState), err)
	}
	if w.agent.last.RequestID != plain.ID || w.agent.last.Revert != nil {
		t.Fatalf("a draft with no edits sent revert parameters %+v", w.agent.last.Revert)
	}
}

// TestPageEditChainTrashWaitsForAnEditInFlightAsAppRole: while an edit of the
// draft is sent and not answered, its undo is refused before anything is
// sent and stays available; once the edit's outcome is recorded the undo
// names it and runs.
func TestPageEditChainTrashWaitsForAnEditInFlightAsAppRole(t *testing.T) {
	ctx := context.Background()
	w := newE2World(t, true)
	create := g2cCreated(t, w, 42, "g2c-wait-create")
	h1, h2 := acprHex("g2c-wait-1"), acprHex("g2c-wait-2")
	e1 := g2cApplied(t, w, 42, "g2c-wait-1", h1)
	e2 := g2cSent(t, w, 42, "g2c-wait-2")
	site := &g2cSite{copies: map[uuid.UUID]string{e1.ID: h1, e2.ID: h2}, ledger: map[uuid.UUID][]uuid.UUID{create.ID: {e1.ID, e2.ID}}}
	w.agent.setOverride(site.answer)

	_, err := w.svc.Undo(ctx, w.person, w.site, create.ID)
	g2cRefusal(t, err, abilityrequest.CodeUndoBusy, "A change to this draft is still being finished. Try again in a minute.")
	if n := w.agent.sent(agentcmd.AbilityRunModeRevert); n != 0 {
		t.Fatalf("%d reverts sent while an edit was in flight", n)
	}
	if s := g2cUndoState(t, w, create.ID); s != "available" {
		t.Fatalf("draft's undo_state = %q after the wait, want available", s)
	}

	g2cAnswer(t, w, e2, h2)
	got, err := w.svc.Undo(ctx, w.person, w.site, create.ID)
	if err != nil || got.UndoState == nil || *got.UndoState != abilityrequest.UndoDone {
		t.Fatalf("chain trash after the edit settled: %v %v (sent %+v)", g2cStr(got.UndoState), err, w.agent.last.Revert)
	}
}
