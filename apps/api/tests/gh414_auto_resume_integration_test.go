// gh414_auto_resume_integration_test.go — GH #414 phase 5, the auto-resume
// sweep, proved against a real database.
//
// The sweep's two load-bearing properties — resumed EXACTLY ONCE, and safe
// against an operator resuming by hand at the same moment — are properties of
// claimDueAutoResumesSQL, not of any Go code. A fake repo asserting them would
// be asserting its own behaviour. So every assertion below reaches the database
// through site.Repo on the pool startPostgres hands back, which connects as the
// NON-superuser, non-BYPASSRLS wpmgr_app role, and the sweep's claim runs under
// the same InAgentTx wrapper production uses — so the sites_agent policy is live
// rather than inert.
package tests

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// pauseSiteWithResumeAt pauses a site through the REAL repo write and then sets
// monitoring_resume_at directly, because the service refuses a resume_at in the
// past (correctly — it is a typo, not an intent) and "already due" is exactly
// the state the sweep exists to handle. The direct UPDATE still goes through
// InTenantTx, so the CHECK constraint and every sites policy apply.
func pauseSiteWithResumeAt(t *testing.T, pool *db.Pool, repo site.Repo, tenantID, siteID uuid.UUID, reason string, resumeAt time.Time) {
	t.Helper()
	ctx := context.Background()
	states, err := repo.PauseMonitoring(ctx, site.PauseMonitoringInput{
		TenantID:  tenantID,
		Principal: autoResumePrincipal{tenantID: tenantID},
		SiteIDs:   []uuid.UUID{siteID},
		Reason:    reason,
	})
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if len(states) != 1 || !states[0].Paused() {
		t.Fatalf("pause did not take: %+v", states)
	}
	if err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE sites SET monitoring_resume_at = $2 WHERE id = $1`, siteID, resumeAt)
		return e
	}); err != nil {
		t.Fatalf("set resume_at: %v", err)
	}
}

// autoResumePrincipal is the minimum principal PauseMonitoring needs: an
// org-scoped operator, so RunTenantTx lands on InTenantTxAsUser exactly as a
// real request would.
type autoResumePrincipal struct{ tenantID uuid.UUID }

func (p autoResumePrincipal) GetTenantID() uuid.UUID         { return p.tenantID }
func (p autoResumePrincipal) GetUserID() uuid.UUID           { return uuid.Nil }
func (p autoResumePrincipal) GetScope() string               { return "" }
func (p autoResumePrincipal) GetAllowedSiteIDs() []uuid.UUID { return nil }

func readMonitoringState(t *testing.T, pool *db.Pool, tenantID, siteID uuid.UUID) (pausedAt, resumeAt *string, reason string) {
	t.Helper()
	ctx := context.Background()
	if err := pool.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT monitoring_paused_at::text, monitoring_resume_at::text, monitoring_paused_reason
			  FROM sites WHERE id = $1`, siteID).Scan(&pausedAt, &resumeAt, &reason)
	}); err != nil {
		t.Fatalf("read monitoring state: %v", err)
	}
	return pausedAt, resumeAt, reason
}

func countResumeAuditEntries(t *testing.T, rec *audit.Recorder, tenantID, siteID uuid.UUID) (int, []map[string]any) {
	t.Helper()
	entries, err := rec.List(context.Background(), tenantID, 200, 0)
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	n := 0
	var metas []map[string]any
	for _, e := range entries {
		if e.Action == audit.ActionSiteMonitoringResumed && e.TargetID == siteID.String() {
			n++
			metas = append(metas, e.Metadata)
		}
	}
	return n, metas
}

// TestGH414_AutoResume_DueSiteResumedExactlyOnceAndAudited is the core proof.
func TestGH414_AutoResume_DueSiteResumedExactlyOnceAndAudited(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenantID := seedTenant(t, pool, "gh414-auto-resume")
	repo := site.NewRepo(pool)
	rec := audit.NewRecorder(pool, domain.SystemClock{})

	created, err := repo.Create(ctx, site.CreateInput{
		TenantID: tenantID, URL: "https://auto-resume.example.com", Name: "auto resume",
	})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}
	past := time.Now().Add(-2 * time.Minute)
	pauseSiteWithResumeAt(t, pool, repo, tenantID, created.ID, "migrating to new host", past)

	resumer := site.NewAutoResumer(repo, rec, nil)

	n, err := resumer.Sweep(ctx, time.Now())
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("first sweep resumed %d, want 1", n)
	}

	pausedAt, resumeAt, reason := readMonitoringState(t, pool, tenantID, created.ID)
	t.Logf("after sweep: paused_at=%v resume_at=%v reason=%q", pausedAt, resumeAt, reason)
	if pausedAt != nil {
		t.Errorf("monitoring_paused_at = %v, want NULL", *pausedAt)
	}
	// BOTH columns must move in one UPDATE or
	// sites_monitoring_resume_requires_pause_check raises 23514. A non-NULL
	// resume_at here would also mean the row still matches the claim predicate
	// and would be swept forever.
	if resumeAt != nil {
		t.Errorf("monitoring_resume_at = %v, want NULL — the pause and the resume instant must clear together", *resumeAt)
	}
	if reason != "" {
		t.Errorf("monitoring_paused_reason = %q, want cleared", reason)
	}

	count, metas := countResumeAuditEntries(t, rec, tenantID, created.ID)
	if count != 1 {
		t.Fatalf("audit entries for the resume = %d, want exactly 1", count)
	}
	if metas[0]["auto"] != true {
		t.Errorf("audit metadata does not mark the resume automatic: %v", metas[0])
	}
	if metas[0]["reason"] != "migrating to new host" {
		t.Errorf("audit metadata lost the pause reason: %v", metas[0])
	}

	// SECOND SWEEP: the row no longer matches its own predicate, so nothing is
	// resumed and nothing is audited a second time.
	n2, err := resumer.Sweep(ctx, time.Now())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second sweep resumed %d, want 0 — the sweep is not idempotent", n2)
	}
	count2, _ := countResumeAuditEntries(t, rec, tenantID, created.ID)
	if count2 != 1 {
		t.Errorf("audit entries after the second sweep = %d, want still 1", count2)
	}
}

// TestGH414_AutoResume_NotYetDueIsUntouched is the over-fire check: a pause
// scheduled for the future must survive every tick until its instant arrives.
func TestGH414_AutoResume_NotYetDueIsUntouched(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenantID := seedTenant(t, pool, "gh414-not-due")
	repo := site.NewRepo(pool)

	created, err := repo.Create(ctx, site.CreateInput{
		TenantID: tenantID, URL: "https://not-due.example.com", Name: "not due",
	})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}
	future := time.Now().Add(24 * time.Hour)
	pauseSiteWithResumeAt(t, pool, repo, tenantID, created.ID, "planned maintenance", future)

	n, err := site.NewAutoResumer(repo, nil, nil).Sweep(ctx, time.Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("sweep resumed %d sites, want 0 — a future resume instant is not due", n)
	}
	pausedAt, resumeAt, _ := readMonitoringState(t, pool, tenantID, created.ID)
	if pausedAt == nil {
		t.Error("a not-yet-due pause was cleared")
	}
	if resumeAt == nil {
		t.Error("a not-yet-due resume instant was cleared")
	}

	// A pause with NO resume_at at all must likewise never be swept: "paused
	// until someone resumes it" is the default and the common case.
	forever, err := repo.Create(ctx, site.CreateInput{
		TenantID: tenantID, URL: "https://forever.example.com", Name: "forever",
	})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}
	if _, err := repo.PauseMonitoring(ctx, site.PauseMonitoringInput{
		TenantID: tenantID, Principal: autoResumePrincipal{tenantID: tenantID},
		SiteIDs: []uuid.UUID{forever.ID}, Reason: "indefinite",
	}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// Sweep a YEAR into the future. The count is deliberately NOT asserted here:
	// the +24h site above is genuinely due at that instant and resuming it is
	// correct, and an assertion of "0 resumed" would redden correct work — which
	// is exactly what it did on the first run of this file. The claim under test
	// is about ONE row, so the assertion is about that one row.
	if _, err := site.NewAutoResumer(repo, nil, nil).Sweep(ctx, time.Now().Add(365*24*time.Hour)); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	foreverPausedAt, foreverResumeAt, _ := readMonitoringState(t, pool, tenantID, forever.ID)
	if foreverPausedAt == nil {
		t.Error("an indefinitely-paused site was auto-resumed; a NULL monitoring_resume_at means 'until someone resumes it', never 'eventually'")
	}
	if foreverResumeAt != nil {
		t.Errorf("the sweep invented a resume instant: %v", *foreverResumeAt)
	}
}

// TestGH414_AutoResume_HandResumeBeforeDueIsNotDoubleResumed — the operator got
// there first. The sweep must find nothing, and the audit log must hold exactly
// one resume event (the operator's), never two.
func TestGH414_AutoResume_HandResumeBeforeDueIsNotDoubleResumed(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenantID := seedTenant(t, pool, "gh414-hand-resume")
	repo := site.NewRepo(pool)
	rec := audit.NewRecorder(pool, domain.SystemClock{})

	created, err := repo.Create(ctx, site.CreateInput{
		TenantID: tenantID, URL: "https://hand-resume.example.com", Name: "hand resume",
	})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}
	// Due in the past, so the sweep WOULD have taken it.
	pauseSiteWithResumeAt(t, pool, repo, tenantID, created.ID, "short pause", time.Now().Add(-time.Minute))

	// The operator resumes by hand first, through the real route's repo write.
	states, err := repo.ResumeMonitoring(ctx, site.ResumeMonitoringInput{
		TenantID: tenantID, Principal: autoResumePrincipal{tenantID: tenantID},
		SiteIDs: []uuid.UUID{created.ID},
	})
	if err != nil {
		t.Fatalf("hand resume: %v", err)
	}
	if len(states) != 1 || !states[0].Changed {
		t.Fatalf("hand resume did not change the row: %+v", states)
	}

	n, err := site.NewAutoResumer(repo, rec, nil).Sweep(ctx, time.Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("sweep resumed %d already-resumed sites, want 0 — that is a phantom transition", n)
	}
	count, _ := countResumeAuditEntries(t, rec, tenantID, created.ID)
	if count != 0 {
		t.Errorf("the sweep wrote %d audit entries for a resume it did not perform", count)
	}
}

// TestGH414_AutoResume_ConcurrentSweepsResumeEachSiteOnce runs several sweeps at
// once against one pool of due sites. Every site must be resumed by exactly one
// sweep: the totals across all goroutines must sum to the number of due sites,
// never more.
func TestGH414_AutoResume_ConcurrentSweepsResumeEachSiteOnce(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	tenantID := seedTenant(t, pool, "gh414-concurrent-resume")
	repo := site.NewRepo(pool)
	rec := audit.NewRecorder(pool, domain.SystemClock{})

	const siteCount = 6
	ids := make([]uuid.UUID, 0, siteCount)
	past := time.Now().Add(-time.Minute)
	for i := 0; i < siteCount; i++ {
		s, err := repo.Create(ctx, site.CreateInput{
			TenantID: tenantID,
			URL:      "https://concurrent-" + uuid.NewString() + ".example.com",
			Name:     "concurrent",
		})
		if err != nil {
			t.Fatalf("create site: %v", err)
		}
		pauseSiteWithResumeAt(t, pool, repo, tenantID, s.ID, "batch", past)
		ids = append(ids, s.ID)
	}

	const sweeps = 4
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		total int
		errs  []error
	)
	start := make(chan struct{})
	for i := 0; i < sweeps; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			n, err := site.NewAutoResumer(repo, rec, nil).Sweep(ctx, time.Now())
			mu.Lock()
			defer mu.Unlock()
			total += n
			if err != nil {
				errs = append(errs, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("concurrent sweep failed: %v", err)
	}
	if total != siteCount {
		t.Errorf("concurrent sweeps resumed %d in total, want exactly %d — a site was resumed twice or dropped", total, siteCount)
	}
	for _, id := range ids {
		pausedAt, resumeAt, _ := readMonitoringState(t, pool, tenantID, id)
		if pausedAt != nil || resumeAt != nil {
			t.Errorf("site %s left paused_at=%v resume_at=%v after concurrent sweeps", id, pausedAt, resumeAt)
		}
		count, _ := countResumeAuditEntries(t, rec, tenantID, id)
		if count != 1 {
			t.Errorf("site %s has %d resume audit entries, want exactly 1", id, count)
		}
	}
}
