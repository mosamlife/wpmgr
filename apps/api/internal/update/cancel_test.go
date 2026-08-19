package update

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// GH #463 — the cancel endpoint's service layer.
//
// The DB-backed proofs (one transaction, tasks 'cancelled' not 'expired', two
// concurrent cancels) live in the integration suite, where the CAS is real.
// These cover the half that turns a repo answer into something a client can
// act on, which is where "too late" either stays distinguishable from a server
// fault or quietly stops being.

func newCancelService(repo *fakeCreateRepo) *Service {
	return NewService(repo, &fakeSiteLookup{}, &countingEnqueuer{}, domain.NewValidator(), domain.SystemClock{})
}

// TestCancelScheduledRunSucceeds is the happy path, and the assertion that
// matters most is the one about the enqueuer: cancelling contacts no site.
func TestCancelScheduledRunSucceeds(t *testing.T) {
	tenant := uuid.New()
	runID := uuid.New()
	repo := &fakeCreateRepo{
		tenantID:    tenant,
		runs:        []Run{{ID: runID, TenantID: tenant, Status: RunScheduled}},
		cancellable: true,
		cancelTasks: 3,
	}
	svc := newCancelService(repo)

	res, err := svc.CancelScheduledRun(context.Background(), tenant, runID)
	if err != nil {
		t.Fatalf("CancelScheduledRun: %v", err)
	}
	if res.Run.Status != RunHalted {
		t.Errorf("run status = %q, want %q", res.Run.Status, RunHalted)
	}
	if res.CancelledTasks != 3 {
		t.Errorf("cancelled tasks = %d, want 3", res.CancelledTasks)
	}
	if repo.cancelCalls != 1 {
		t.Errorf("repo cancel called %d times, want 1", repo.cancelCalls)
	}
}

// TestCancelScheduledRunRefusesARunThatAlreadyFired is the property the whole
// endpoint rests on.
//
// A cancel that "succeeded" on a run whose commands were already out would tell
// an operator their fleet was safe while it was being updated — worse than
// refusing outright, because they would stop watching. The refusal must also be
// a CONFLICT and not an opaque failure, so the UI can render "already started"
// instead of "something went wrong".
func TestCancelScheduledRunRefusesARunThatAlreadyFired(t *testing.T) {
	for _, status := range []string{RunDispatching, RunRunning, RunCompleted, RunHalted, RunExpired} {
		t.Run(status, func(t *testing.T) {
			tenant := uuid.New()
			runID := uuid.New()
			repo := &fakeCreateRepo{
				tenantID: tenant,
				runs:     []Run{{ID: runID, TenantID: tenant, Status: status}},
				// The CAS refuses: the row is not 'scheduled'.
				cancellable: false,
			}
			svc := newCancelService(repo)

			_, err := svc.CancelScheduledRun(context.Background(), tenant, runID)
			if err == nil {
				t.Fatalf("cancel of a %s run succeeded; the operator would be told their fleet was called back while it was updating", status)
			}
			de, ok := domain.AsDomain(err)
			if !ok {
				t.Fatalf("error is not a domain error, so httpx cannot map it: %v", err)
			}
			if de.Kind != domain.KindConflict {
				t.Errorf("error kind = %v, want KindConflict (409); a client cannot tell 'too late' from a server fault otherwise", de.Kind)
			}
			if de.Code != "run_not_cancellable" {
				t.Errorf("error code = %q, want run_not_cancellable", de.Code)
			}
			// The message must name the state the operator is looking at,
			// otherwise the 409 is unactionable.
			if !contains(de.Message, status) {
				t.Errorf("conflict message %q does not name the run's actual status %q", de.Message, status)
			}
		})
	}
}

// TestCancelScheduledRunSecondCancelIsTooLate models two operators pressing the
// button at once. The first wins; the second must get the conflict rather than a
// second success, because two successes would report two cancellations of one
// run and the audit log would say so.
func TestCancelScheduledRunSecondCancelIsTooLate(t *testing.T) {
	tenant := uuid.New()
	runID := uuid.New()
	repo := &fakeCreateRepo{
		tenantID:    tenant,
		runs:        []Run{{ID: runID, TenantID: tenant, Status: RunScheduled}},
		cancellable: true,
		cancelTasks: 2,
	}
	svc := newCancelService(repo)

	if _, err := svc.CancelScheduledRun(context.Background(), tenant, runID); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	_, err := svc.CancelScheduledRun(context.Background(), tenant, runID)
	if err == nil {
		t.Fatal("the second cancel of the same run also succeeded; one run would be reported cancelled twice")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindConflict {
		t.Errorf("second cancel error = %v, want a conflict", err)
	}
	if repo.cancelCalls != 2 {
		t.Errorf("repo cancel called %d times, want 2: the service must let the CAS decide, not short-circuit on its own read", repo.cancelCalls)
	}
}

// TestCancelScheduledRunMissingRunIs404 keeps the two refusals distinct. A run
// that never existed and a run that already fired need different words in front
// of an operator, and folding them together would send someone hunting for a
// deleted run that is actually mid-flight.
func TestCancelScheduledRunMissingRunIs404(t *testing.T) {
	tenant := uuid.New()
	repo := &fakeCreateRepo{tenantID: tenant} // no runs seeded
	svc := newCancelService(repo)

	_, err := svc.CancelScheduledRun(context.Background(), tenant, uuid.New())
	if err == nil {
		t.Fatal("cancel of a nonexistent run succeeded")
	}
	de, ok := domain.AsDomain(err)
	if !ok {
		t.Fatalf("error is not a domain error: %v", err)
	}
	if de.Kind != domain.KindNotFound {
		t.Errorf("error kind = %v, want KindNotFound (404)", de.Kind)
	}
	if repo.cancelCalls != 0 {
		t.Error("the service attempted the CAS on a run it could not read")
	}
}

// TestCancelScheduledRunSurfacesInfrastructureErrors. A database failure must
// NOT be reported as "too late": that would tell the operator the run had
// already fired when in fact nobody knows, and they would stop trying to stop
// it.
func TestCancelScheduledRunSurfacesInfrastructureErrors(t *testing.T) {
	tenant := uuid.New()
	runID := uuid.New()
	boom := errors.New("connection refused")
	repo := &fakeCreateRepo{
		tenantID:  tenant,
		runs:      []Run{{ID: runID, TenantID: tenant, Status: RunScheduled}},
		cancelErr: boom,
	}
	svc := newCancelService(repo)

	_, err := svc.CancelScheduledRun(context.Background(), tenant, runID)
	if err == nil {
		t.Fatal("an infrastructure failure was reported as a successful cancel")
	}
	if de, ok := domain.AsDomain(err); ok && de.Code == "run_not_cancellable" {
		t.Error("a database error was reported as 'too late'; the operator would believe the run had already fired when nobody knows")
	}
	if !errors.Is(err, boom) {
		t.Errorf("the underlying error was swallowed: %v", err)
	}
}

// TestCancelScheduledRunRejectsMissingTenant guards the multi-tenant boundary at
// the service edge, where every other mutation here guards it.
func TestCancelScheduledRunRejectsMissingTenant(t *testing.T) {
	repo := &fakeCreateRepo{}
	svc := newCancelService(repo)

	_, err := svc.CancelScheduledRun(context.Background(), uuid.Nil, uuid.New())
	if err == nil {
		t.Fatal("cancel proceeded with no tenant context")
	}
	if de, ok := domain.AsDomain(err); !ok || de.Kind != domain.KindForbidden {
		t.Errorf("error = %v, want a forbidden domain error", err)
	}
	if repo.cancelCalls != 0 {
		t.Error("the service reached the repo without a tenant")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
