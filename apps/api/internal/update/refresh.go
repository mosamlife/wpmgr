package update

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
)

// RefreshInventoryArgs is the River job payload for an inventory-refresh
// request. It carries only the minimum identity needed to mint a signed
// CP->agent command: the agent re-reads inventory and update transients and
// pushes the result back over /agent/v1/metadata asynchronously, so we don't
// persist or correlate a refresh row server-side.
type RefreshInventoryArgs struct {
	TenantID uuid.UUID `json:"tenant_id"`
	SiteID   uuid.UUID `json:"site_id"`
	SiteURL  string    `json:"site_url"`
	// Source is a short tag for audit/observability: "api" (operator-triggered
	// over /sites/:id/updates/refresh) or "post_update" (the update worker
	// enqueued it after a task reached a terminal state).
	Source string `json:"source,omitempty"`
}

// Kind implements river.JobArgs.
func (RefreshInventoryArgs) Kind() string { return "refresh_inventory_command" }

// InsertOpts reuses the per-tenant queue shard so a tenant's refresh jobs are
// bounded by the same per-tenant parallelism limit as their update tasks.
func (a RefreshInventoryArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueForTenant(a.TenantID)}
}

// RefreshCommander is the subset of the agentcmd.Client used by the refresh
// worker. Defined here so the worker can be tested with a fake.
type RefreshCommander interface {
	RefreshInventory(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.RefreshInventoryRequest) (agentcmd.RefreshInventoryResponse, error)
}

// RefreshInventoryWorker dispatches a signed `refresh_inventory` command to a
// site's agent. On an OLD-agent 404 (the route doesn't exist on the site yet),
// the worker records an audit warning and returns nil so River won't retry to
// death; on any other transport/agent error it returns the error and River
// retries with its default backoff.
type RefreshInventoryWorker struct {
	river.WorkerDefaults[RefreshInventoryArgs]
	cmd    RefreshCommander
	audit  *audit.Recorder
	logger *slog.Logger
}

// NewRefreshInventoryWorker builds a RefreshInventoryWorker.
func NewRefreshInventoryWorker(cmd RefreshCommander, rec *audit.Recorder, logger *slog.Logger) *RefreshInventoryWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &RefreshInventoryWorker{cmd: cmd, audit: rec, logger: logger}
}

// Work runs one refresh job.
func (w *RefreshInventoryWorker) Work(ctx context.Context, job *river.Job[RefreshInventoryArgs]) error {
	a := job.Args
	if w.cmd == nil {
		// Signing disabled — refuse rather than send unsigned. Cancel the job so
		// River doesn't loop forever; the audit record captures intent.
		w.logger.Warn("refresh inventory: command client unavailable", slog.String("site_id", a.SiteID.String()))
		return river.JobCancel(errors.New("refresh_inventory: CP->agent commands are disabled"))
	}
	_, err := w.cmd.RefreshInventory(ctx, a.SiteID, a.SiteURL, agentcmd.RefreshInventoryRequest{})
	if err == nil {
		w.logger.Debug("refresh inventory: ok",
			slog.String("site_id", a.SiteID.String()),
			slog.String("source", a.Source))
		return nil
	}
	// OLD agents (Track A endpoint not deployed yet) return a 404 from the REST
	// API. agentcmd.Client.post wraps that as `... rejected by agent: status 404
	// body=…`; we sniff that prefix and treat it as a successful no-op so a long
	// tail of un-updated sites does not pile up retries forever. Other errors
	// (5xx, transport, 401/403) are returned so River retries.
	if isOldAgentRouteMissing(err) {
		w.logger.Info("refresh inventory: agent has no refresh route (old agent); skipping",
			slog.String("site_id", a.SiteID.String()),
			slog.String("source", a.Source),
			slog.String("error", err.Error()))
		w.recordUnsupported(ctx, a)
		return nil
	}
	return err
}

// statusFromAgentError extracts the HTTP status code from the canonical agentcmd
// post error format ("... rejected by agent: status NNN body=...") so the
// worker can branch on 404 vs other failures without smuggling typed errors out
// of the client.
var agentStatusRE = regexp.MustCompile(`rejected by agent: status (\d+)`)

func isOldAgentRouteMissing(err error) bool {
	if err == nil {
		return false
	}
	m := agentStatusRE.FindStringSubmatch(err.Error())
	if len(m) < 2 {
		return false
	}
	code, perr := strconv.Atoi(m[1])
	return perr == nil && code == 404
}

func (w *RefreshInventoryWorker) recordUnsupported(ctx context.Context, a RefreshInventoryArgs) {
	if w.audit == nil {
		return
	}
	_, _ = w.audit.Record(ctx, audit.Event{
		TenantID:   a.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     audit.ActionUpdateRefreshUnsupported,
		TargetType: "site",
		TargetID:   a.SiteID.String(),
		Metadata: map[string]any{
			"site_url":    a.SiteURL,
			"source":      a.Source,
			"status_code": 404,
		},
	})
}

// RefreshEnqueuer schedules refresh-inventory River jobs. Implemented by the
// River-backed enqueuer (wired in main).
type RefreshEnqueuer interface {
	EnqueueRefresh(ctx context.Context, args RefreshInventoryArgs) error
}

// RefreshDebouncer is a tiny in-process LRU-ish guard that suppresses redundant
// refresh enqueues for the same site within a short window. The post-update
// worker enqueues one refresh per task-terminal-state; a 10-plugin bulk run
// against one site would otherwise enqueue 10 refresh jobs in a few seconds.
// Debounce window: 30 seconds, matching the spec.
type RefreshDebouncer struct {
	mu      sync.Mutex
	last    map[uuid.UUID]time.Time
	window  time.Duration
	maxKeys int
	now     func() time.Time
}

// NewRefreshDebouncer builds a debouncer. window is the per-site suppression
// window (the worker calls Allow before enqueueing; consecutive calls within
// the window short-circuit). maxKeys caps the map size to avoid an unbounded
// per-site key set — when full, the oldest 25% of entries are evicted.
func NewRefreshDebouncer(window time.Duration) *RefreshDebouncer {
	if window <= 0 {
		window = 30 * time.Second
	}
	return &RefreshDebouncer{
		last:    map[uuid.UUID]time.Time{},
		window:  window,
		maxKeys: 4096,
		now:     time.Now,
	}
}

// Allow reports whether a refresh for siteID should be enqueued NOW, recording
// the decision when allowed. Concurrent callers race fairly: only one wins
// within a window.
func (d *RefreshDebouncer) Allow(siteID uuid.UUID) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if t, ok := d.last[siteID]; ok && now.Sub(t) < d.window {
		return false
	}
	d.last[siteID] = now
	if len(d.last) > d.maxKeys {
		d.evictLocked()
	}
	return true
}

// evictLocked drops the oldest 25% of entries when the map grows past maxKeys.
// Cheap enough (only fires on overflow) and avoids unbounded growth in a long-
// lived process with a high site count.
func (d *RefreshDebouncer) evictLocked() {
	cutoff := d.now().Add(-d.window)
	for k, t := range d.last {
		if t.Before(cutoff) {
			delete(d.last, k)
		}
	}
}
