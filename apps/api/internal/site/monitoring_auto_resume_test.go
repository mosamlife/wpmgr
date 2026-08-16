package site

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// GH #414 phase 5 — the auto-resume sweep, at the unit level.
//
// The properties that matter are exactly-once and safe-under-concurrency, and
// both of those live in claimDueAutoResumesSQL rather than in Go — so these
// tests own what Go owns (the sweep drives the claim once, audits what came
// back, and reports the count) and the SQL's own properties are proved against
// a real database in tests/gh414_auto_resume_integration_test.go. Splitting it
// that way is deliberate: a fake repo that "resumes exactly once" because the
// fake says so would prove nothing about the statement that actually ships.

// fakeAutoResumeRepo is a claim that hands back a fixed batch the first time and
// nothing afterwards, which is the shape the real statement has: the UPDATE
// clears monitoring_resume_at, so the row stops matching its own predicate.
type fakeAutoResumeRepo struct {
	mu       sync.Mutex
	batches  [][]AutoResumed
	calls    int
	lastNow  time.Time
	lastLim  int
	claimErr error
}

func (f *fakeAutoResumeRepo) ClaimDueAutoResumes(_ context.Context, now time.Time, limit int) ([]AutoResumed, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastNow = now
	f.lastLim = limit
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	if len(f.batches) == 0 {
		return nil, nil
	}
	out := f.batches[0]
	f.batches = f.batches[1:]
	return out, nil
}

func TestAutoResumerSweepsWhatTheClaimReturns(t *testing.T) {
	due := []AutoResumed{
		{SiteID: uuid.New(), TenantID: uuid.New(), PausedReason: "migration"},
		{SiteID: uuid.New(), TenantID: uuid.New()},
	}
	repo := &fakeAutoResumeRepo{batches: [][]AutoResumed{due}}
	// nil recorder: this test is about the sweep's arithmetic, and the audit
	// path has its own proof in the integration suite where a real chain exists.
	a := NewAutoResumer(repo, nil, nil)

	now := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	n, err := a.Sweep(context.Background(), now)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 2 {
		t.Errorf("Sweep resumed %d, want 2", n)
	}
	if repo.calls != 1 {
		t.Errorf("claim called %d times in one sweep, want 1", repo.calls)
	}
	if !repo.lastNow.Equal(now) {
		t.Errorf("sweep instant not passed through: got %v want %v", repo.lastNow, now)
	}
	if repo.lastLim != autoResumeBatchSize {
		t.Errorf("batch cap = %d, want %d — an uncapped claim is an unbounded write", repo.lastLim, autoResumeBatchSize)
	}

	// A SECOND sweep finds nothing: the real statement clears
	// monitoring_resume_at, so the row no longer matches its own predicate. This
	// is the Go-side half of "resumed exactly once".
	n2, err := a.Sweep(context.Background(), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second sweep resumed %d, want 0", n2)
	}
}

// TestAutoResumerSurfacesClaimFailures — a sweep that cannot read the database
// must return the error so River retries the job, never report 0 resumed and
// exit green. This project's signature defect is announcing success over its
// own errors.
func TestAutoResumerSurfacesClaimFailures(t *testing.T) {
	boom := errors.New("connection refused")
	repo := &fakeAutoResumeRepo{claimErr: boom}
	a := NewAutoResumer(repo, nil, nil)
	n, err := a.Sweep(context.Background(), time.Now())
	if !errors.Is(err, boom) {
		t.Errorf("Sweep swallowed the claim error: got %v", err)
	}
	if n != 0 {
		t.Errorf("Sweep reported %d resumed on a failed claim", n)
	}
}

func TestAutoResumerBatchSizeIsOverridableAndNeverZero(t *testing.T) {
	repo := &fakeAutoResumeRepo{}
	a := NewAutoResumer(repo, nil, nil)
	a.SetBatchSize(7)
	if _, err := a.Sweep(context.Background(), time.Now()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if repo.lastLim != 7 {
		t.Errorf("SetBatchSize(7) not honoured: limit = %d", repo.lastLim)
	}
	a.SetBatchSize(0)
	if _, err := a.Sweep(context.Background(), time.Now()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if repo.lastLim != 7 {
		t.Errorf("SetBatchSize(0) clobbered the cap to %d; zero must be ignored", repo.lastLim)
	}
}

// TestAutoResumeClaimSQLShape guards the three clauses the exactly-once and
// concurrency properties actually rest on. It is a text assertion on purpose:
// each of these is one word that a future edit can drop without breaking any
// other test, and each drop is a real defect.
//
//   - FOR UPDATE SKIP LOCKED — two replicas ticking together must take disjoint
//     rows rather than one blocking on the other.
//   - the re-checked monitoring_paused_at predicate on the OUTER update — the
//     inner select's snapshot can be stale by the time the lock is granted, and
//     without this an operator's concurrent manual resume gets double-resumed
//     and double-audited.
//   - monitoring_resume_at = NULL in the SET list — clearing the pause while
//     leaving the resume instant behind violates
//     sites_monitoring_resume_requires_pause_check (23514) and would abort the
//     whole sweep; it is also what makes a second tick a no-op.
func TestAutoResumeClaimSQLShape(t *testing.T) {
	for _, clause := range []string{
		"FOR UPDATE SKIP LOCKED",
		"monitoring_resume_at     = NULL",
		"monitoring_paused_at     = NULL",
	} {
		if !strings.Contains(claimDueAutoResumesSQL, clause) {
			t.Errorf("claimDueAutoResumesSQL lost %q", clause)
		}
	}
	// The outer UPDATE must re-check both predicates against the locked row.
	outer := claimDueAutoResumesSQL[strings.Index(claimDueAutoResumesSQL, "UPDATE sites s"):]
	if !strings.Contains(outer, "s.monitoring_paused_at IS NOT NULL") {
		t.Error("the outer UPDATE does not re-check s.monitoring_paused_at; a hand resume racing the sweep would be double-applied")
	}
	if !strings.Contains(outer, "s.monitoring_resume_at <= $1") {
		t.Error("the outer UPDATE does not re-check the due instant against the locked row")
	}
	// RETURNING must read the PRIOR values out of the due CTE, not the freshly
	// nulled columns, or every audit entry describes a pause with no start and
	// no reason.
	ret := claimDueAutoResumesSQL[strings.Index(claimDueAutoResumesSQL, "RETURNING"):]
	for _, col := range []string{"due.monitoring_paused_at", "due.monitoring_paused_reason", "due.monitoring_resume_at"} {
		if !strings.Contains(ret, col) {
			t.Errorf("RETURNING does not carry %s; the audit entry would describe nothing", col)
		}
	}
}

// TestAutoResumeArgsKindIsStable — the River job kind is a persisted string.
// Renaming it strands every queued row under the old name.
func TestAutoResumeArgsKindIsStable(t *testing.T) {
	if got := (AutoResumeArgs{}).Kind(); got != "site_monitoring_auto_resume" {
		t.Errorf("AutoResumeArgs.Kind() = %q", got)
	}
}
