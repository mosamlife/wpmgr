package site

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// fakeBillingGate implements site.BillingGate for wiring-only assertions. It
// is never invoked against a real database in these tests.
type fakeBillingGate struct{ calls int }

func (f *fakeBillingGate) CheckSiteCreate(_ context.Context, _ pgx.Tx, _ uuid.UUID) error {
	f.calls++
	return nil
}

// TestServiceSetBillingGate_WiresServiceOwnRepo guards the exact wiring bug
// class fixed for the screenshot enricher in v0.49.1 (see
// screenshot_enricher_wiring_test.go): the billing gate must land on the
// Service's OWN repo instance — the one Create()/Enroll() actually use — not
// on some other repo instance constructed elsewhere in cmd/wpmgr.
func TestServiceSetBillingGate_WiresServiceOwnRepo(t *testing.T) {
	svc := NewService(NewRepo(nil), domain.NewValidator(), domain.SystemClock{})

	gate := &fakeBillingGate{}
	svc.SetBillingGate(gate)

	r, ok := svc.repo.(*pgRepo)
	if !ok {
		t.Fatalf("service repo is not *pgRepo: %T", svc.repo)
	}
	if r.billing == nil {
		t.Fatal("SetBillingGate did not wire the gate onto the service's own repo")
	}
	if r.billing != BillingGate(gate) {
		t.Fatal("wired gate is not the one provided to SetBillingGate")
	}
}

// TestFreeSetBillingGate_NoopOnNonPgRepo proves the free SetBillingGate
// function is a safe no-op when the Repo is not a *pgRepo (e.g. a test
// fakeRepo), mirroring SetScreenshotEnricher's contract exactly.
func TestFreeSetBillingGate_NoopOnNonPgRepo(t *testing.T) {
	// Must not panic.
	SetBillingGate(&fakeRepo{}, &fakeBillingGate{})
}

// TestRestore_SetsCheckSiteQuota proves connService.Restore asks the repo to
// re-check the site cap (TransitionInput.CheckSiteQuota=true) on the
// archived->disconnected un-archive transition — the one transition that
// grows the active (non-archived) site count without a fresh INSERT.
func TestRestore_SetsCheckSiteQuota(t *testing.T) {
	repo := &capturingTransitionRepo{}
	// audit recorder is nil: connService.recordAudit no-ops on a nil *audit.
	// Recorder, so Restore's post-commit audit write is safely skipped here.
	conn := NewConnectionService(repo, domain.NewValidator(), nil, nil, domain.SystemClock{}, nil)

	tenantID, siteID := uuid.New(), uuid.New()
	if _, err := conn.Restore(context.Background(), ActorSiteInput{TenantID: tenantID, SiteID: siteID}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if !repo.lastInput.CheckSiteQuota {
		t.Fatal("Restore did not set TransitionInput.CheckSiteQuota — the un-archive site-cap re-check would be skipped")
	}
	if repo.lastInput.RequireFrom != StateArchived {
		t.Fatalf("Restore RequireFrom = %q, want %q", repo.lastInput.RequireFrom, StateArchived)
	}
}

// capturingTransitionRepo is a minimal fakeRepo specialisation that records
// the TransitionInput it was called with, so tests can assert on fields the
// shared stateRepo fake (connection_service_test.go) does not expose.
type capturingTransitionRepo struct {
	fakeRepo
	lastInput TransitionInput
}

func (r *capturingTransitionRepo) Transition(_ context.Context, in TransitionInput) (TransitionResult, error) {
	r.lastInput = in
	return TransitionResult{
		Site: Site{ID: in.SiteID, TenantID: in.TenantID, ConnectionState: in.To},
		From: StateArchived,
	}, nil
}
