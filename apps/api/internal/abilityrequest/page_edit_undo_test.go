package abilityrequest

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// BF-E G2c: a page edit's undo and the undo of a page-builder draft WPMgr
// has since edited, decided without a database.

// The page-edit undo's answers map onto the undo results.
func TestUndoResultFor_PageEditAnswers(t *testing.T) {
	refusal := func(code, detail string) error { return &agentcmd.AbilityRunRefusal{Code: code, Detail: detail} }
	for name, c := range map[string]struct {
		resp agentcmd.AbilityRunResponse
		err  error
		want string
	}{
		"reverted":                   {agentcmd.AbilityRunResponse{OK: true, Outcome: "reverted", Mode: "revert"}, nil, UndoDone},
		"already reverted":           {agentcmd.AbilityRunResponse{OK: true, Outcome: "already_reverted", Mode: "revert"}, nil, UndoDone},
		"published now":              {agentcmd.AbilityRunResponse{}, refusal("refused_published", "this page is published now"), UndoRefusedPublished},
		"no longer a draft":          {agentcmd.AbilityRunResponse{}, refusal("target_not_draft", "the page is no longer a draft"), UndoRefusedConflict},
		"a later change rewrote it":  {agentcmd.AbilityRunResponse{}, refusal("conflict", "changed_after_this_change"), UndoRefusedConflict},
		"open in the editor":         {agentcmd.AbilityRunResponse{}, refusal("conflict", "editor_open"), UndoRefusedConflict},
		"autosave pending":           {agentcmd.AbilityRunResponse{}, refusal("conflict", "autosave_pending"), UndoRefusedConflict},
		"copy tampered":              {agentcmd.AbilityRunResponse{}, refusal("snapshot_tampered", ""), UndoFailed},
		"put-back did not read back": {agentcmd.AbilityRunResponse{}, refusal("restore_mismatch", "read_back_differs"), UndoFailed},
		"chain broken":               {agentcmd.AbilityRunResponse{}, refusal("created_post_touched", "chain_broken"), UndoRefusedConflict},
		"builder not compiled":       {agentcmd.AbilityRunResponse{}, refusal("builder_not_available", "not_compiled"), UndoFailed},
		"not revertible":             {agentcmd.AbilityRunResponse{}, refusal("not_revertible", ""), UndoFailed},
	} {
		if got := undoResultFor(c.resp, c.err); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// The site could not read its own copy or the page: nothing was written and
// the undo is released for a retry. A copy that does not match is final.
func TestUndoRetryable_PageEditReadFailures(t *testing.T) {
	for _, code := range []string{"snapshot_unreadable", "data_unreadable"} {
		if !undoRetryable(&agentcmd.AbilityRunRefusal{Code: code}) {
			t.Errorf("%s: not retryable, so a database blip on the site ends the undo for good", code)
		}
	}
	for _, code := range []string{"snapshot_tampered", "restore_mismatch", "refused_published", "target_not_draft", "conflict", "created_post_touched"} {
		if undoRetryable(&agentcmd.AbilityRunRefusal{Code: code}) {
			t.Errorf("%s: retryable, but the site gave a definite answer", code)
		}
	}
}

func appliedEditRow(now time.Time) sqlc.AssistantAbilityRequest {
	avail, applied := "available", OutcomeApplied
	post := int64(42)
	hash := "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"
	return sqlc.AssistantAbilityRequest{
		ID: uuid.New(), AbilityName: mcp.AbilityPageEdit, State: "done", Outcome: &applied,
		TargetPostID: &post, SnapshotSha256: &hash, UndoState: &avail, UndoAvailableUntil: tsAt(now.Add(time.Hour)),
	}
}

// Undo goes newest first: a page edit offers its undo only while it is the
// newest applied edit of its page still in effect. No other ability's undo
// depends on it.
func TestUndoOffered_PageEditOnlyTheNewest(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	edit := appliedEditRow(now)
	if !UndoOffered(edit, "", true, now) {
		t.Fatal("the newest applied edit offers no undo")
	}
	if UndoOffered(edit, "", false, now) {
		t.Fatal("an edit with a later edit still in effect offers its undo")
	}
	undone := edit
	u := UndoDone
	undone.UndoState = &u
	if UndoOffered(undone, "", true, now) {
		t.Fatal("an undone edit offers its undo again")
	}
	late := edit
	late.UndoAvailableUntil = tsAt(now.Add(-time.Minute))
	if UndoOffered(late, "", true, now) {
		t.Fatal("an edit past its window offers its undo")
	}
	create := edit
	create.AbilityName = mcp.AbilityPageCreate
	if !UndoOffered(create, "", false, now) {
		t.Fatal("a page-create undo depends on the page-edit rule")
	}
}

func editAt(created time.Time, state string, outcome, undo *string) sqlc.ListEditRequestsForPostRow {
	return sqlc.ListEditRequestsForPostRow{ID: uuid.New(), State: state, Outcome: outcome, UndoState: undo, CreatedAt: created}
}

func wantDomainCode(t *testing.T, err error, code string) {
	t.Helper()
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}

// A draft's undo names every applied edit of the draft since its creation,
// in the order they were made, undone ones included; edits that changed
// nothing are left out.
func TestRevertChainFor_NamesEveryAppliedEditInOrder(t *testing.T) {
	t0 := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	creation := sqlc.AssistantAbilityRequest{ID: uuid.New(), AbilityName: mcp.AbilityPageCreate, CreatedAt: t0}
	s := func(v string) *string { return &v }
	applied, refused, notSent, gaveUp := s(OutcomeApplied), s(OutcomeRefused), s("not_sent"), s(OutcomeUnknown)

	if r, err := revertChainFor(creation, nil, noPutBack); err != nil || r != nil {
		t.Fatalf("no edits: %+v %v, want nothing sent", r, err)
	}

	edits := []sqlc.ListEditRequestsForPostRow{
		editAt(t0.Add(-time.Hour), "done", applied, s("available")), // an earlier draft's edit of a reused post id
		editAt(t0.Add(1*time.Minute), "done", applied, s("available")),
		editAt(t0.Add(2*time.Minute), "failed", refused, nil),
		editAt(t0.Add(3*time.Minute), "done", applied, s(UndoDone)),
		editAt(t0.Add(4*time.Minute), "declined", nil, nil),
		editAt(t0.Add(5*time.Minute), "done", applied, nil), // applied with no undo of its own
		editAt(t0.Add(6*time.Minute), "not_sent", notSent, nil),
		editAt(t0.Add(7*time.Minute), "outcome_unknown", gaveUp, nil),
		editAt(t0.Add(8*time.Minute), "done", applied, s(UndoRefusedConflict)),
		editAt(t0.Add(9*time.Minute), "pending", nil, nil),
	}
	r, err := revertChainFor(creation, edits, noPutBack)
	if err != nil {
		t.Fatal(err)
	}
	want := []uuid.UUID{edits[1].ID, edits[3].ID, edits[5].ID, edits[8].ID}
	if r == nil || r.SnapshotSHA256 != "" || len(r.Chain) != len(want) {
		t.Fatalf("chain = %+v, want %v", r, want)
	}
	for i := range want {
		if r.Chain[i] != want[i] {
			t.Fatalf("chain[%d] = %s, want %s (order or membership wrong)", i, r.Chain[i], want[i])
		}
	}

	// Edits that are not applied name nothing.
	if r, err := revertChainFor(creation, edits[2:3], noPutBack); err != nil || r != nil {
		t.Fatalf("no applied edit: %+v %v, want nothing sent", r, err)
	}
}

// noPutBack reads every failed edit as one the site did not put back.
func noPutBack(uuid.UUID) (bool, error) { return false, nil }

// A failed edit whose record says the site put the page back left nothing of
// its own on the draft but the revisions its save made. The draft's undo
// names it among the edits, in the order they were made, so the site counts
// those revisions as WPMgr's. A failed edit the site did not put back, or
// that recorded no put-back at all, is never named.
func TestRevertChainFor_NamesFailedEditsThePageWasPutBackFrom(t *testing.T) {
	t0 := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	creation := sqlc.AssistantAbilityRequest{ID: uuid.New(), AbilityName: mcp.AbilityPageCreate, CreatedAt: t0}
	s := func(v string) *string { return &v }
	applied, refused, mismatch := s(OutcomeApplied), s(OutcomeRefused), s(OutcomeVerifyMismatch)

	edits := []sqlc.ListEditRequestsForPostRow{
		editAt(t0.Add(-time.Hour), "failed", refused, nil), // an earlier draft's edit of a reused post id, put back
		editAt(t0.Add(1*time.Minute), "done", applied, s("available")),
		editAt(t0.Add(2*time.Minute), "failed", refused, nil),  // put back
		editAt(t0.Add(3*time.Minute), "failed", refused, nil),  // not put back
		editAt(t0.Add(4*time.Minute), "failed", refused, nil),  // failed before it wrote anything: no report
		editAt(t0.Add(5*time.Minute), "failed", mismatch, nil), // put back
		editAt(t0.Add(6*time.Minute), "done", applied, s("available")),
	}
	putBackIDs := map[uuid.UUID]bool{edits[0].ID: true, edits[2].ID: true, edits[5].ID: true}
	var read []uuid.UUID
	putBack := func(id uuid.UUID) (bool, error) {
		read = append(read, id)
		return putBackIDs[id], nil
	}
	r, err := revertChainFor(creation, edits, putBack)
	if err != nil {
		t.Fatal(err)
	}
	want := []uuid.UUID{edits[1].ID, edits[2].ID, edits[5].ID, edits[6].ID}
	if r == nil || r.SnapshotSHA256 != "" || len(r.Chain) != len(want) {
		t.Fatalf("chain = %+v, want %v", r, want)
	}
	for i := range want {
		if r.Chain[i] != want[i] {
			t.Fatalf("chain[%d] = %s, want %s (order or membership wrong)", i, r.Chain[i], want[i])
		}
	}
	// Only the failed edits since this creation are read.
	wantRead := []uuid.UUID{edits[2].ID, edits[3].ID, edits[4].ID, edits[5].ID}
	if len(read) != len(wantRead) {
		t.Fatalf("read %v, want %v", read, wantRead)
	}
	for i := range wantRead {
		if read[i] != wantRead[i] {
			t.Fatalf("read[%d] = %s, want %s", i, read[i], wantRead[i])
		}
	}

	// A draft whose only edit failed and was put back names that edit.
	if r, err := revertChainFor(creation, edits[2:3], putBack); err != nil || r == nil || len(r.Chain) != 1 || r.Chain[0] != edits[2].ID {
		t.Fatalf("only a put-back failed edit: %+v %v", r, err)
	}
	// One the site did not put back names nothing, as before.
	if r, err := revertChainFor(creation, edits[3:5], putBack); err != nil || r != nil {
		t.Fatalf("failed edits not put back: %+v %v, want nothing sent", r, err)
	}
	// A record that cannot be read refuses the undo before anything is sent.
	boom := errors.New("read failed")
	if _, err := revertChainFor(creation, edits[2:3], func(uuid.UUID) (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the read's error", err)
	}
}

// While an edit of the draft may still change it, the undo waits and sends
// nothing. A write WPMgr gave up resolving does not hold it.
func TestRevertChainFor_WaitsForAnEditInFlight(t *testing.T) {
	t0 := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	creation := sqlc.AssistantAbilityRequest{ID: uuid.New(), AbilityName: mcp.AbilityPageCreate, CreatedAt: t0}
	s := func(v string) *string { return &v }
	done := editAt(t0.Add(time.Minute), "done", s(OutcomeApplied), s("available"))
	for name, e := range map[string]sqlc.ListEditRequestsForPostRow{
		"approved, not yet sent":  editAt(t0.Add(2*time.Minute), "approved", nil, nil),
		"sent, no answer":         editAt(t0.Add(2*time.Minute), "dispatched", nil, nil),
		"answer being resolved":   editAt(t0.Add(2*time.Minute), "outcome_unknown", nil, nil),
		"being undone":            editAt(t0.Add(2*time.Minute), "done", s(OutcomeApplied), s("in_progress")),
		"earlier edit being sent": editAt(t0.Add(30*time.Second), "dispatched", nil, nil),
	} {
		_, err := revertChainFor(creation, []sqlc.ListEditRequestsForPostRow{done, e}, noPutBack)
		if de, ok := domain.AsDomain(err); !ok || de.Code != CodeUndoBusy || de.Message != msgUndoBusy {
			t.Errorf("%s: got %v, want %s", name, err, CodeUndoBusy)
		}
	}
	gaveUp := editAt(t0.Add(2*time.Minute), "outcome_unknown", s(OutcomeUnknown), nil)
	if r, err := revertChainFor(creation, []sqlc.ListEditRequestsForPostRow{done, gaveUp}, noPutBack); err != nil || r == nil || len(r.Chain) != 1 {
		t.Fatalf("a given-up edit held the undo or joined the chain: %+v %v", r, err)
	}
	// An edit request older than this creation is not about this draft.
	old := editAt(t0.Add(-time.Minute), "dispatched", nil, nil)
	if _, err := revertChainFor(creation, []sqlc.ListEditRequestsForPostRow{old, done}, noPutBack); err != nil {
		t.Fatalf("an edit older than the creation held the undo: %v", err)
	}
}

// A draft with more edits than one undo can name, or an edit list that may be
// cut short, is never trashed on a partial chain.
func TestRevertChainFor_RefusesWhatItCannotNameInFull(t *testing.T) {
	t0 := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	creation := sqlc.AssistantAbilityRequest{ID: uuid.New(), AbilityName: mcp.AbilityPageCreate, CreatedAt: t0}
	applied, avail := OutcomeApplied, "available"
	many := func(n int) []sqlc.ListEditRequestsForPostRow {
		out := make([]sqlc.ListEditRequestsForPostRow, n)
		for i := range out {
			out[i] = editAt(t0.Add(time.Duration(i+1)*time.Second), "done", &applied, &avail)
		}
		return out
	}
	r, err := revertChainFor(creation, many(agentcmd.AbilityRunMaxRevertChain), noPutBack)
	if err != nil || r == nil || len(r.Chain) != agentcmd.AbilityRunMaxRevertChain {
		t.Fatalf("a full chain: %v", err)
	}
	_, err = revertChainFor(creation, many(agentcmd.AbilityRunMaxRevertChain+1), noPutBack)
	wantDomainCode(t, err, CodeUndoUnavailable)

	cut := many(editListLimit)
	for i := range cut {
		cut[i].State, cut[i].Outcome, cut[i].UndoState = "declined", nil, nil
	}
	_, err = revertChainFor(creation, cut, noPutBack)
	wantDomainCode(t, err, CodeUndoUnavailable)
}

// The stuck-undo reconciler reads an undone page edit as 'restored' and an
// undone draft as 'trashed', and neither as the other.
func TestUndoVerdictFor_PerAbility(t *testing.T) {
	for _, c := range []struct {
		ability, state string
		want           undoVerdict
	}{
		{mcp.AbilityPageEdit, "restored", undoVerdictReverted},
		{mcp.AbilityPageEdit, "available", undoVerdictNotReverted},
		{mcp.AbilityPageEdit, "trashed", undoVerdictNotReverted},
		{mcp.AbilityPageCreate, "trashed", undoVerdictReverted},
		{mcp.AbilityPageCreate, "restored", undoVerdictNotReverted},
	} {
		if got := undoVerdictFor(c.ability, agentcmd.AbilityRunResponse{OK: true, Found: true, UndoState: c.state}, nil); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.ability, c.state, got, c.want)
		}
	}
	if undoVerdictFor(mcp.AbilityPageEdit, agentcmd.AbilityRunResponse{}, errors.New("timeout")) != undoVerdictUnknown {
		t.Error("a failed ledger call decided a page edit's undo")
	}
}
