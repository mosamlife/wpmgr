package abilityrequest

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// Once Begin has committed in_progress, a client that disconnects must not
// strand the row: the finish transaction runs on a context the request's
// cancellation does not reach, and that context is still bounded.
func TestRecordUndoFinish_CancelledRequestStillRecords(t *testing.T) {
	s := &Service{logger: slog.Default()}
	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()

	var ran bool
	var finishCtx context.Context
	run := func(ctx context.Context, _ domain.Principal, _ func(*sqlc.Queries, pgx.Tx) error) error {
		ran = true
		finishCtx = ctx
		// pgx refuses to begin a transaction on a done context; mirror that.
		return ctx.Err()
	}
	_, err := s.recordUndoFinish(reqCtx, run, session(authz.RoleOwner), uuid.New(), uuid.New(), UndoFailed, errors.New("transport"), nil)
	if err != nil {
		t.Fatalf("a cancelled request left the undo unrecorded: %v", err)
	}
	if !ran {
		t.Fatal("the finish transaction never ran")
	}
	dl, ok := finishCtx.Deadline()
	if !ok {
		t.Fatal("the finish transaction has no deadline")
	}
	if left := time.Until(dl); left <= 0 || left > undoFinishBudget {
		t.Fatalf("finish deadline %v outside (0, %v]", left, undoFinishBudget)
	}
}

// A recording failure is reported, never swallowed.
func TestRecordUndoFinish_RecordFailureIsInternal(t *testing.T) {
	s := &Service{logger: slog.Default()}
	run := func(context.Context, domain.Principal, func(*sqlc.Queries, pgx.Tx) error) error {
		return errors.New("db down")
	}
	_, err := s.recordUndoFinish(context.Background(), run, session(authz.RoleOwner), uuid.New(), uuid.New(), UndoDone, nil, nil)
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "ability_request_undo_unrecorded" {
		t.Fatalf("got %v, want ability_request_undo_unrecorded", err)
	}
}

// finishCapture records the arguments FinishAbilityRequestUndo sends.
type finishCapture struct{ args []any }

func (f *finishCapture) Exec(_ context.Context, _ string, args ...interface{}) (pgconn.CommandTag, error) {
	f.args = args
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (f *finishCapture) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, errors.New("unexpected query")
}
func (f *finishCapture) QueryRow(context.Context, string, ...interface{}) pgx.Row { return nil }

// The finish records the revert report's restored flag on the row, and a
// result with no report sends NULL, which keeps the stored value.
func TestRecordUndoFinish_PassesRestored(t *testing.T) {
	s := &Service{logger: slog.Default()}
	for _, tc := range []struct {
		name   string
		report map[string]any
		want   *bool
	}{
		{"partial", map[string]any{"restored": false, "columns_still_changed": []string{"post_status"}}, boolp(false)},
		{"full", map[string]any{"restored": true}, boolp(true)},
		{"no report", nil, nil},
		{"not a bool", map[string]any{"restored": "false"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &finishCapture{}
			run := func(_ context.Context, _ domain.Principal, fn func(*sqlc.Queries, pgx.Tx) error) error {
				return fn(sqlc.New(db), nil)
			}
			// The audit recorder is not wired, so the finish errors after the
			// update ran; only the update's arguments matter here.
			_, _ = s.recordUndoFinish(context.Background(), run, session(authz.RoleOwner), uuid.New(), uuid.New(), UndoDone, nil, tc.report)
			if len(db.args) < 2 {
				t.Fatalf("FinishAbilityRequestUndo not called: %v", db.args)
			}
			got, _ := db.args[1].(*bool)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("restored arg %v, want %v", db.args[1], tc.want)
			}
		})
	}
}
