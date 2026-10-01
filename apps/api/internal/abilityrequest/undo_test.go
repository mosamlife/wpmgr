package abilityrequest

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	_, err := s.recordUndoFinish(reqCtx, run, session(authz.RoleOwner), uuid.New(), uuid.New(), UndoFailed, errors.New("transport"))
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
	_, err := s.recordUndoFinish(context.Background(), run, session(authz.RoleOwner), uuid.New(), uuid.New(), UndoDone, nil)
	de, ok := domain.AsDomain(err)
	if !ok || de.Code != "ability_request_undo_unrecorded" {
		t.Fatalf("got %v, want ability_request_undo_unrecorded", err)
	}
}
