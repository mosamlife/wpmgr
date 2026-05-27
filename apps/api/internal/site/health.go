package site

import (
	"context"
	"time"

	"github.com/riverqueue/river"
)

// HealthChecker performs the periodic connection-health sweep. M2 health is
// derived purely from agent-heartbeat freshness: a site whose last_seen_at is
// older than staleAfter (≈2 missed heartbeat intervals) is marked unreachable.
// Active external probing of the site is deferred to M5 (uptime monitoring);
// this is documented in the schema and README.
type HealthChecker struct {
	repo          Repo
	staleAfter    time.Duration
	signatureSkew time.Duration
}

// NewHealthChecker builds a HealthChecker. staleAfter is the freshness
// threshold past which a site is considered unreachable. signatureSkew is the
// anti-replay window; the periodic maintenance prune deletes agent_nonces older
// than now-signatureSkew, which can never replay, bounding table growth.
func NewHealthChecker(repo Repo, staleAfter, signatureSkew time.Duration) *HealthChecker {
	return &HealthChecker{repo: repo, staleAfter: staleAfter, signatureSkew: signatureSkew}
}

// Sweep examines every enrolled site and marks unreachable any whose last
// heartbeat is older than the stale threshold relative to now. It returns the
// number of sites transitioned to unreachable. Pure logic over the repo, so it
// is unit/integration-testable directly without River.
func (h *HealthChecker) Sweep(ctx context.Context, now time.Time) (int, error) {
	sites, err := h.repo.ListEnrolled(ctx)
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-h.staleAfter)
	marked := 0
	for _, s := range sites {
		if s.HealthStatus == "unreachable" {
			continue
		}
		// A site with no heartbeat yet but enrolled is considered stale once the
		// threshold elapses; LastSeenAt is set at enroll time, so it is non-nil in
		// practice, but treat nil as stale defensively.
		if s.LastSeenAt != nil && s.LastSeenAt.After(cutoff) {
			continue
		}
		changed, err := h.repo.MarkUnreachable(ctx, s.ID)
		if err != nil {
			return marked, err
		}
		if changed {
			marked++
		}
	}
	return marked, nil
}

// PruneNonces deletes anti-replay nonces older than the signature-skew window
// relative to now (cross-tenant maintenance, like Sweep). A nonce older than
// now-skew can never be presented within a still-valid signed request, so it is
// safe to delete; this bounds unbounded growth of agent_nonces (a compromised
// agent could otherwise accelerate it). It returns the number of rows deleted.
// A non-positive skew disables pruning (returns 0) to avoid deleting live
// nonces. Pure logic over the repo, so it is testable directly without River.
func (h *HealthChecker) PruneNonces(ctx context.Context, now time.Time) (int64, error) {
	if h.signatureSkew <= 0 {
		return 0, nil
	}
	cutoff := now.Add(-h.signatureSkew)
	return h.repo.PruneNonces(ctx, cutoff)
}

// HealthCheckArgs is the River job payload for the periodic health sweep. It
// carries no per-run data; the schedule is configured on the periodic job.
type HealthCheckArgs struct{}

// Kind implements river.JobArgs.
func (HealthCheckArgs) Kind() string { return "site_health_check" }

// HealthCheckWorker runs the health sweep as a River job.
type HealthCheckWorker struct {
	river.WorkerDefaults[HealthCheckArgs]
	checker *HealthChecker
}

// NewHealthCheckWorker builds the River worker around a HealthChecker.
func NewHealthCheckWorker(checker *HealthChecker) *HealthCheckWorker {
	return &HealthCheckWorker{checker: checker}
}

// Work runs one health sweep, then prunes expired anti-replay nonces. The prune
// is a best-effort maintenance step: its failure must not mask a sweep failure,
// but a prune-only failure is still surfaced so the job retries.
func (w *HealthCheckWorker) Work(ctx context.Context, _ *river.Job[HealthCheckArgs]) error {
	now := time.Now()
	if _, err := w.checker.Sweep(ctx, now); err != nil {
		return err
	}
	_, err := w.checker.PruneNonces(ctx, now)
	return err
}
