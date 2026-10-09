package abilityrequest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

func undoCodeStr(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}

// TestUndoCodeFor: a failed undo records the site's code when it is one of
// the two closed undo codes, and nothing otherwise; no other result ever
// carries a code.
func TestUndoCodeFor(t *testing.T) {
	refusal := func(code, detail string) error { return &agentcmd.AbilityRunRefusal{Code: code, Detail: detail} }
	for name, c := range map[string]struct {
		result string
		err    error
		want   string
	}{
		"copy tampered":                 {UndoFailed, refusal("snapshot_tampered", ""), "snapshot_tampered"},
		"put-back did not read back":    {UndoFailed, refusal("restore_mismatch", "read_back_differs"), "restore_mismatch"},
		"wrapped refusal":               {UndoFailed, fmt.Errorf("revert: %w", refusal("snapshot_tampered", "")), "snapshot_tampered"},
		"another refusal":               {UndoFailed, refusal("not_revertible", ""), "<null>"},
		"builder not compiled":          {UndoFailed, refusal("builder_not_available", "not_compiled"), "<null>"},
		"code-shaped detail only":       {UndoFailed, refusal("bad_params", "snapshot_tampered"), "<null>"},
		"transport failure":             {UndoFailed, errors.New("transport"), "<null>"},
		"answered, not reverted":        {UndoFailed, nil, "<null>"},
		"undone, with a tampered code":  {UndoDone, refusal("snapshot_tampered", ""), "<null>"},
		"refused, with a mismatch code": {UndoRefusedConflict, refusal("restore_mismatch", ""), "<null>"},
		"published, with a code":        {UndoRefusedPublished, refusal("snapshot_tampered", ""), "<null>"},
	} {
		if got := undoCodeFor(c.result, c.err); undoCodeStr(got) != c.want {
			t.Errorf("%s: undo_code %s, want %s", name, undoCodeStr(got), c.want)
		}
	}
}

// TestUndoCodeForEveryRefusalCode: over every refusal code the control plane
// accepts, the result undoResultFor records carries a code exactly for the
// two undo codes, so the finish never sends a code the table refuses.
func TestUndoCodeForEveryRefusalCode(t *testing.T) {
	coded := 0
	for code := range agentcmd.AbilityRunRefusalCodes {
		err := &agentcmd.AbilityRunRefusal{Code: code}
		result := undoResultFor(agentcmd.AbilityRunResponse{}, err)
		got := undoCodeFor(result, err)
		_, closed := undoCodes[code]
		if closed != (got != nil) || (got != nil && (*got != code || result != UndoFailed)) {
			t.Fatalf("%s: result %s, undo_code %s", code, result, undoCodeStr(got))
		}
		if got != nil {
			coded++
		}
	}
	if coded != len(undoCodes) {
		t.Fatalf("%d refusal codes recorded an undo_code, want each of the %d undo codes once", coded, len(undoCodes))
	}
}

// TestRecordUndoFinish_PassesUndoCode: the finish sends the code with a
// failed undo the site refused with one, and NULL otherwise.
func TestRecordUndoFinish_PassesUndoCode(t *testing.T) {
	s := &Service{logger: slog.Default()}
	for _, tc := range []struct {
		name   string
		result string
		err    error
		want   string
	}{
		{"tampered", UndoFailed, &agentcmd.AbilityRunRefusal{Code: "snapshot_tampered"}, "snapshot_tampered"},
		{"mismatch", UndoFailed, &agentcmd.AbilityRunRefusal{Code: "restore_mismatch", Detail: "read_back_differs"}, "restore_mismatch"},
		{"other failure", UndoFailed, &agentcmd.AbilityRunRefusal{Code: "not_revertible"}, "<null>"},
		{"undone", UndoDone, nil, "<null>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &finishCapture{}
			run := func(_ context.Context, _ domain.Principal, fn func(*sqlc.Queries, pgx.Tx) error) error {
				return fn(sqlc.New(db), nil)
			}
			// The audit recorder is not wired, so the finish errors after the
			// update ran; only the update's arguments matter here.
			_, _ = s.recordUndoFinish(context.Background(), run, session(authz.RoleOwner), uuid.New(), uuid.New(), tc.result, tc.err, nil)
			if len(db.args) < 3 {
				t.Fatalf("FinishAbilityRequestUndo not called: %v", db.args)
			}
			if db.args[0] != tc.result {
				t.Fatalf("undo_result arg %v, want %s", db.args[0], tc.result)
			}
			got, ok := db.args[2].(*string)
			if !ok || undoCodeStr(got) != tc.want {
				t.Fatalf("undo_code arg %#v, want %s", db.args[2], tc.want)
			}
		})
	}
}

// TestUndoCodeOnTheWire: undo_code carries a failed undo's stored code from
// the closed set only, and is null on the wire for every other row.
func TestUndoCodeOnTheWire(t *testing.T) {
	row := func(state, code string) sqlc.AssistantAbilityRequest {
		r := sqlc.AssistantAbilityRequest{State: "done"}
		if state != "" {
			r.UndoState = &state
		}
		if code != "" {
			r.UndoCode = &code
		}
		return r
	}
	for _, c := range []struct {
		r    sqlc.AssistantAbilityRequest
		want string
	}{
		{row(UndoFailed, "snapshot_tampered"), "snapshot_tampered"},
		{row(UndoFailed, "restore_mismatch"), "restore_mismatch"},
		{row(UndoFailed, ""), "<null>"},
		{row(UndoFailed, "bogus"), "<null>"},
		{row(UndoFailed, "Snapshot_tampered"), "<null>"},
		{row(UndoDone, "snapshot_tampered"), "<null>"},
		{row("in_progress", "restore_mismatch"), "<null>"},
		{row("", "snapshot_tampered"), "<null>"},
	} {
		if got := toDTO(c.r, false, "", false).UndoCode; undoCodeStr(got) != c.want {
			t.Fatalf("undo_state %s undo_code %s: on the wire %s, want %s",
				undoCodeStr(c.r.UndoState), undoCodeStr(c.r.UndoCode), undoCodeStr(got), c.want)
		}
	}
	for want, r := range map[string]sqlc.AssistantAbilityRequest{
		"snapshot_tampered": row(UndoFailed, "snapshot_tampered"),
		"<null>":            row(UndoDone, ""),
	} {
		b, err := json.Marshal(toDTO(r, false, "", false))
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err := json.Unmarshal(b, &wire); err != nil {
			t.Fatal(err)
		}
		v, present := wire["undo_code"]
		if !present || (want == "<null>" && v != nil) || (want != "<null>" && v != want) {
			t.Fatalf("undo_code on the wire is %v (present %v), want %s", v, present, want)
		}
	}
}
