package abilityrequest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/mcp"
)

// GH #824: an answer that settles nothing releases the undo for a retry;
// a definite refusal finishes it.
func TestUndoRetryable(t *testing.T) {
	retry := map[string]error{
		"transport":         errors.New("dial tcp: connection refused"),
		"timeout":           context.DeadlineExceeded,
		"not sent":          fmt.Errorf("wrap: %w", agentcmd.ErrCommandNotSent),
		"target_in_flight":  &agentcmd.AbilityRunRefusal{Code: "target_in_flight"},
		"request_in_flight": &agentcmd.AbilityRunRefusal{Code: "request_in_flight"},
	}
	for name, err := range retry {
		if !undoRetryable(err) {
			t.Errorf("%s: not retryable, so one blip closes undo for good", name)
		}
	}
	final := map[string]error{
		"ok":                     nil,
		"conflict":               &agentcmd.AbilityRunRefusal{Code: "conflict"},
		"created_post_published": &agentcmd.AbilityRunRefusal{Code: "created_post_published"},
		"created_post_touched":   &agentcmd.AbilityRunRefusal{Code: "created_post_touched"},
		"not_revertible":         &agentcmd.AbilityRunRefusal{Code: "not_revertible"},
		"nothing_to_revert":      &agentcmd.AbilityRunRefusal{Code: "nothing_to_revert"},
		"revert_failed":          &agentcmd.AbilityRunRefusal{Code: "revert_failed"},
	}
	for name, err := range final {
		if undoRetryable(err) {
			t.Errorf("%s: retryable, but the site gave a definite answer", name)
		}
	}
}

func tsAt(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// GH #826: which undo a row offers, decided server-side.
func TestUndoKindFor(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	post := int64(42)
	yes, no := true, false
	avail, inProg := "available", "in_progress"
	failed, unknown := OutcomeFailed, OutcomeUnknown

	recovery := sqlc.AssistantAbilityRequest{State: "failed", Outcome: &failed, CreatedPostID: &post, OutcomeAt: tsAt(now.Add(-time.Hour))}
	cases := []struct {
		name string
		row  func(r sqlc.AssistantAbilityRequest) sqlc.AssistantAbilityRequest
		want undoKind
	}{
		{"failed write left a draft", func(r sqlc.AssistantAbilityRequest) sqlc.AssistantAbilityRequest { return r }, undoKindRecovery},
		{"given-up write left a draft", func(r sqlc.AssistantAbilityRequest) sqlc.AssistantAbilityRequest {
			r.State, r.Outcome = "outcome_unknown", &unknown
			return r
		}, undoKindRecovery},
		{"verify mismatch, cleanup failed", func(r sqlc.AssistantAbilityRequest) sqlc.AssistantAbilityRequest {
			r.Trashed = &no
			return r
		}, undoKindRecovery},
		{"cleanup already trashed it", func(r sqlc.AssistantAbilityRequest) sqlc.AssistantAbilityRequest {
			r.Trashed = &yes
			return r
		}, undoKindNone},
		{"still resolving", func(r sqlc.AssistantAbilityRequest) sqlc.AssistantAbilityRequest {
			r.State, r.Outcome = "outcome_unknown", nil
			return r
		}, undoKindNone},
		{"no post recorded", func(r sqlc.AssistantAbilityRequest) sqlc.AssistantAbilityRequest {
			r.CreatedPostID = nil
			return r
		}, undoKindNone},
		{"undo already started", func(r sqlc.AssistantAbilityRequest) sqlc.AssistantAbilityRequest {
			r.UndoState = &inProg
			return r
		}, undoKindNone},
		{"site record retention passed", func(r sqlc.AssistantAbilityRequest) sqlc.AssistantAbilityRequest {
			r.OutcomeAt = tsAt(now.Add(-undoRetention))
			return r
		}, undoKindNone},
		{"not_sent", func(r sqlc.AssistantAbilityRequest) sqlc.AssistantAbilityRequest {
			r.State = "not_sent"
			return r
		}, undoKindNone},
		{"done, available, in window", func(sqlc.AssistantAbilityRequest) sqlc.AssistantAbilityRequest {
			return sqlc.AssistantAbilityRequest{State: "done", UndoState: &avail, UndoAvailableUntil: tsAt(now.Add(time.Hour))}
		}, undoKindDone},
		{"done, window passed", func(sqlc.AssistantAbilityRequest) sqlc.AssistantAbilityRequest {
			return sqlc.AssistantAbilityRequest{State: "done", UndoState: &avail, UndoAvailableUntil: tsAt(now.Add(-time.Second))}
		}, undoKindNone},
		{"done, in progress", func(sqlc.AssistantAbilityRequest) sqlc.AssistantAbilityRequest {
			return sqlc.AssistantAbilityRequest{State: "done", UndoState: &inProg, UndoAvailableUntil: tsAt(now.Add(time.Hour))}
		}, undoKindNone},
	}
	for _, c := range cases {
		row := c.row(recovery)
		if got := undoKindFor(row, "0.61.157", now); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
		if UndoOffered(row, "0.61.157", false, now) != (c.want != undoKindNone) {
			t.Errorf("%s: undo_offered disagrees with the undo the service would run", c.name)
		}
	}
}

// GH #825: the card's final state.
func TestResolveGaveUp(t *testing.T) {
	unknown, failed := OutcomeUnknown, OutcomeFailed
	if !resolveGaveUp(sqlc.AssistantAbilityRequest{State: "outcome_unknown", Outcome: &unknown}) {
		t.Fatal("a given-up row is not reported as given up")
	}
	if resolveGaveUp(sqlc.AssistantAbilityRequest{State: "outcome_unknown"}) {
		t.Fatal("a row still being checked is reported as given up")
	}
	if resolveGaveUp(sqlc.AssistantAbilityRequest{State: "failed", Outcome: &failed}) {
		t.Fatal("a failed row is reported as given up")
	}
}

// GH #824: the stuck-undo reconciler's reading of the ledger.
func TestUndoVerdictFor(t *testing.T) {
	cases := []struct {
		name string
		resp agentcmd.AbilityRunResponse
		err  error
		want undoVerdict
	}{
		{"call failed", agentcmd.AbilityRunResponse{}, errors.New("timeout"), undoVerdictUnknown},
		{"still running", agentcmd.AbilityRunResponse{Found: true, Inflight: true, UndoState: "trashed"}, nil, undoVerdictUnknown},
		{"trashed", agentcmd.AbilityRunResponse{Found: true, UndoState: "trashed"}, nil, undoVerdictReverted},
		{"available", agentcmd.AbilityRunResponse{Found: true, UndoState: "available"}, nil, undoVerdictNotReverted},
		{"none", agentcmd.AbilityRunResponse{Found: true, UndoState: "none"}, nil, undoVerdictNotReverted},
		{"no ledger row", agentcmd.AbilityRunResponse{Found: false}, nil, undoVerdictNotReverted},
	}
	for _, c := range cases {
		if got := undoVerdictFor(mcp.AbilityPageCreate, c.resp, c.err); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// The release runs on a detached, bounded context, as the finish does.
func TestRecordUndoRelease_CancelledRequestStillRecords(t *testing.T) {
	s := &Service{logger: slog.Default()}
	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()
	var finishCtx context.Context
	run := func(ctx context.Context, _ domain.Principal, _ func(*sqlc.Queries, pgx.Tx) error) error {
		finishCtx = ctx
		return ctx.Err()
	}
	if _, _, err := s.recordUndoRelease(reqCtx, run, session(authz.RoleOwner), uuid.New(), uuid.New(), errors.New("transport")); err != nil {
		t.Fatalf("a cancelled request left the undo unreleased: %v", err)
	}
	if dl, ok := finishCtx.Deadline(); !ok || time.Until(dl) <= 0 || time.Until(dl) > undoFinishBudget {
		t.Fatal("the release transaction has no bounded deadline")
	}
}

// zeroRowsDB answers every write with 0 rows affected and every read with an
// empty scan: an undo some other path already settled.
type zeroRowsDB struct{ reads int }

type emptyRow struct{}

func (emptyRow) Scan(...any) error { return nil }

func (zeroRowsDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("UPDATE 0"), nil
}
func (zeroRowsDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected query")
}
func (d *zeroRowsDB) QueryRow(context.Context, string, ...any) pgx.Row { d.reads++; return emptyRow{} }

// A release that finds the undo already settled records no audit event and
// reports the row as it now stands, not a failure. The service has no audit
// recorder, so any attempt to record one errors.
func TestRecordUndoRelease_AlreadySettledAuditsNothing(t *testing.T) {
	s := &Service{logger: slog.Default()}
	db := &zeroRowsDB{}
	run := func(ctx context.Context, _ domain.Principal, fn func(*sqlc.Queries, pgx.Tx) error) error {
		return fn(sqlc.New(db), nil)
	}
	_, released, err := s.recordUndoRelease(context.Background(), run, session(authz.RoleOwner), uuid.New(), uuid.New(), errors.New("transport"))
	if err != nil {
		t.Fatalf("an undo settled elsewhere was treated as an error or audited: %v", err)
	}
	if released {
		t.Fatal("reported released though nothing was released")
	}
	if db.reads != 1 {
		t.Fatalf("row re-read %d times, want 1", db.reads)
	}
}

// The stuck threshold must outlast every budget of the request that started
// the undo, or the reconciler could release an undo whose revert is still on
// its way to the site.
func TestStuckUndoAfterOutlastsTheUndoRequest(t *testing.T) {
	if StuckUndoAfter <= undoSendBudget+undoFinishBudget+tokenSettled {
		t.Fatalf("StuckUndoAfter %v does not outlast the undo request's budgets", StuckUndoAfter)
	}
}
