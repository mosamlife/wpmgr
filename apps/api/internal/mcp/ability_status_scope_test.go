package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// statusStoreFake answers as if RLS had NOT narrowed: it returns every row of
// the grant, so the Go-side re-check is what is under test.
type statusStoreFake struct{ rows []AbilityStatusRow }

func (f *statusStoreFake) RunAbilityRequestTx(context.Context, domain.Principal, func(pgx.Tx, abilityRequestQueries) error) error {
	return nil
}

func (f *statusStoreFake) ReadAbilityRequestStatus(_ context.Context, _ domain.Principal, _, id uuid.UUID) (AbilityStatusRow, bool, error) {
	for _, r := range f.rows {
		if r.ID == id {
			return r, true, nil
		}
	}
	return AbilityStatusRow{}, false, nil
}

func (f *statusStoreFake) ListOpenAbilityRequestStatus(context.Context, domain.Principal, uuid.UUID, int32) ([]AbilityStatusRow, error) {
	return f.rows, nil
}

// R5: after a grant's site scope narrows, the dropped site's requests vanish
// from the list and answer absent by id.
func TestAbilityStatus_ReappliesCurrentSiteScope(t *testing.T) {
	kept, dropped := uuid.New(), uuid.New()
	keptReq, droppedReq := uuid.New(), uuid.New()
	store := &statusStoreFake{rows: []AbilityStatusRow{
		{ID: keptReq, SiteID: kept, SiteLabel: "kept", AbilityName: "wpmgr/page-create"},
		{ID: droppedReq, SiteID: dropped, SiteLabel: "dropped", AbilityName: "wpmgr/page-create"},
	}}
	svc := NewService(&fakeStore{})
	eng := &abilityEngine{writes: store}
	auth := AuthorizedRequest{TenantID: uuid.New(), GrantID: uuid.New(), Sites: NewSiteSet([]uuid.UUID{kept})}

	out, err := svc.abilityRequestStatus(context.Background(), eng, auth, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, keptReq.String()) || strings.Contains(out, droppedReq.String()) {
		t.Fatalf("list after narrowing: %s", out)
	}
	if _, err := svc.abilityRequestStatus(context.Background(), eng, auth, droppedReq.String(), true); err == nil {
		t.Fatal("a dropped site's request answered by id")
	}
	if _, err := svc.abilityRequestStatus(context.Background(), eng, auth, keptReq.String(), true); err != nil {
		t.Fatalf("kept request: %v", err)
	}
}
