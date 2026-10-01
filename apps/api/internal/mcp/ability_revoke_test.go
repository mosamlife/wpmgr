package mcp

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
)

type fakeAbilityRevoker struct {
	withdrawn, notSent []uuid.UUID
	calls              int
}

func (f *fakeAbilityRevoker) CloseAbilityRequestsForGrantTx(context.Context, pgx.Tx, uuid.UUID, uuid.UUID) ([]uuid.UUID, []uuid.UUID, error) {
	f.calls++
	return f.withdrawn, f.notSent, nil
}

type recordingAudit struct{ events []audit.Event }

func (r *recordingAudit) RecordInTx(_ context.Context, _ pgx.Tx, e audit.Event) (audit.Entry, error) {
	r.events = append(r.events, e)
	return audit.Entry{}, nil
}

func TestRevokeCascade_ClosesAndAuditsAbilityRequests(t *testing.T) {
	w, n := uuid.New(), uuid.New()
	rv := &fakeAbilityRevoker{withdrawn: []uuid.UUID{w}, notSent: []uuid.UUID{n}}
	gotW, gotN, err := closeAbilityRequestsForGrant(context.Background(), rv, nil, uuid.New(), uuid.New())
	if err != nil || rv.calls != 1 || len(gotW) != 1 || len(gotN) != 1 {
		t.Fatalf("cascade did not run: calls=%d err=%v", rv.calls, err)
	}
	rec := &recordingAudit{}
	if err := recordAbilityRevokeCascade(context.Background(), rec, nil, uuid.New(), uuid.New(), "user", "u", gotW, gotN); err != nil {
		t.Fatal(err)
	}
	if len(rec.events) != 2 ||
		rec.events[0].Action != audit.ActionAssistantRequestWithdrawn || rec.events[0].TargetID != w.String() ||
		rec.events[1].Action != audit.ActionAssistantRequestNotSent || rec.events[1].TargetID != n.String() ||
		rec.events[0].TargetType != audit.TargetTypeAssistantAbilityRequest {
		t.Fatalf("audit rows wrong: %+v", rec.events)
	}
}

// The production store must carry the cascade: a Repo that lost the method
// would silently skip it.
func TestRevokeCascade_RepoImplementsIt(t *testing.T) {
	var s any = &Repo{}
	if _, ok := s.(abilityRequestRevoker); !ok {
		t.Fatal("*Repo does not implement the ability revoke cascade")
	}
}
