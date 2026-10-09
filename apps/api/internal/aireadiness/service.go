package aireadiness

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// MetadataRefresher asks a site's agent to push fresh metadata. The method
// set matches the site package's refresh enqueuer, so the same adapter
// serves both without this package importing a sibling domain.
type MetadataRefresher interface {
	EnqueueRefresh(ctx context.Context, tenantID, siteID uuid.UUID, siteURL, source string) error
}

// InventoryRefresher queues a read of a site's tool list. queued is false when
// an equal job was queued within the last two minutes: the request is then
// already satisfied, not refused.
type InventoryRefresher interface {
	EnqueueInventoryRefresh(ctx context.Context, tenantID, siteID uuid.UUID) (queued bool, err error)
}

// InventoryRefreshFunc adapts a function to InventoryRefresher.
type InventoryRefreshFunc func(ctx context.Context, tenantID, siteID uuid.UUID) (bool, error)

// EnqueueInventoryRefresh implements InventoryRefresher.
func (f InventoryRefreshFunc) EnqueueInventoryRefresh(ctx context.Context, tenantID, siteID uuid.UUID) (bool, error) {
	return f(ctx, tenantID, siteID)
}

// auditRecorder is the part of *audit.Recorder the service uses.
type auditRecorder interface {
	Record(ctx context.Context, e audit.Event) (audit.Entry, error)
}

// refreshSource tags the metadata refresh job for audit and observability.
const refreshSource = "ai_readiness"

// Service computes readiness and handles the refresh request.
type Service struct {
	repo   Repo
	audit  auditRecorder
	logger *slog.Logger
	floors Floors
	now    func() time.Time

	meta       MetadataRefresher
	inventory  InventoryRefresher
	staleAfter time.Duration
}

// NewService builds the service. rec may be nil (no audit).
func NewService(repo Repo, rec *audit.Recorder, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{repo: repo, logger: logger, floors: DefaultFloors(), now: time.Now}
	if rec != nil {
		s.audit = rec
	}
	return s
}

// SetAuditRecorder replaces the audit recorder (tests).
func (s *Service) SetAuditRecorder(r auditRecorder) { s.audit = r }

// SetClock overrides the time source (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// SetRefreshers wires the two refresh enqueuers once River has started. staleAfter
// is how long an agent may be silent before a refresh is refused with
// site_unreachable; zero disables that gate, exactly as for the updates refresh.
func (s *Service) SetRefreshers(meta MetadataRefresher, inventory InventoryRefresher, staleAfter time.Duration) {
	s.meta, s.inventory, s.staleAfter = meta, inventory, staleAfter
}

func errSiteNotFound() error { return domain.NotFound("site_not_found", "site not found") }

// Get evaluates one site. A site that is not the caller's, is archived or has
// never been enrolled is not found.
func (s *Service) Get(ctx context.Context, p domain.Principal, siteID uuid.UUID) (Result, error) {
	// The route gate and the row policies already narrow a site collaborator;
	// this is the third check, on the id the caller named.
	if !p.CanAccessSite(siteID) {
		return Result{}, errSiteNotFound()
	}
	rows, err := s.repo.LoadFacts(ctx, p, &siteID)
	if err != nil {
		return Result{}, domain.Internal("ai_readiness_read_failed", "failed to read AI readiness").WithCause(err)
	}
	for _, f := range rows {
		if f.SiteID == siteID {
			return EvaluateWith(f, s.floors), nil
		}
	}
	return Result{}, errSiteNotFound()
}

// Fleet evaluates every enrolled, non-archived site the caller can see. It
// fans out over many sites, so each one is checked against the caller's site
// allowlist here as well as by the row policies.
func (s *Service) Fleet(ctx context.Context, p domain.Principal) ([]Result, error) {
	rows, err := s.repo.LoadFacts(ctx, p, nil)
	if err != nil {
		return nil, domain.Internal("ai_readiness_read_failed", "failed to read AI readiness").WithCause(err)
	}
	out := make([]Result, 0, len(rows))
	for _, f := range rows {
		if !p.CanAccessSite(f.SiteID) {
			continue
		}
		out = append(out, EvaluateWith(f, s.floors))
	}
	return out, nil
}

// RefreshResult is what a refresh request queued.
type RefreshResult struct {
	// Metadata: a fresh metadata report was requested from the site.
	Metadata bool
	// Abilities: a tool-list read was queued now or already queued within the
	// last two minutes. False when the agent is too old to run one.
	Abilities bool
}

// RequestRefresh asks the site to report again. It is gated exactly like the
// updates refresh: not enrolled, or an agent silent for longer than staleAfter,
// is site_unreachable and nothing is queued.
func (s *Service) RequestRefresh(ctx context.Context, p domain.Principal, siteID uuid.UUID) (RefreshResult, error) {
	if !p.CanAccessSite(siteID) {
		return RefreshResult{}, errSiteNotFound()
	}
	target, err := s.repo.RefreshTarget(ctx, p, siteID)
	if err != nil {
		if _, ok := domain.AsDomain(err); ok {
			return RefreshResult{}, err
		}
		return RefreshResult{}, domain.Internal("ai_readiness_read_failed", "failed to read the site").WithCause(err)
	}
	if target.ConnectionState == "archived" {
		return RefreshResult{}, errSiteNotFound()
	}
	if s.meta == nil {
		return RefreshResult{}, domain.ServiceUnavailable("ai_readiness_refresh_unavailable",
			"Asking the site to report again is not available on this install.")
	}
	if !target.Enrolled {
		return RefreshResult{}, domain.Conflict("site_unreachable", "site is not enrolled with an agent")
	}
	if s.staleAfter > 0 {
		cutoff := s.now().Add(-s.staleAfter)
		if target.LastSeenAt == nil || target.LastSeenAt.Before(cutoff) {
			return RefreshResult{}, domain.Conflict("site_unreachable", "site agent heartbeat is stale; cannot refresh now")
		}
	}

	if err := s.meta.EnqueueRefresh(ctx, p.TenantID, siteID, target.URL, refreshSource); err != nil {
		return RefreshResult{}, domain.Internal("refresh_enqueue_failed", "failed to ask the site to report again").WithCause(err)
	}
	res := RefreshResult{Metadata: true}

	// The tool list is only readable by an agent that ships the ability
	// engine. A queued job is unique per site per two minutes, so pressing the
	// button again does not queue a second read; the answer is still "a read
	// is queued".
	if a, ok := cleanAgentVersion(target.AgentVersion); ok && s.inventory != nil &&
		agentAtLeast(a, s.floors.EngineAgent) {
		if _, err := s.inventory.EnqueueInventoryRefresh(ctx, p.TenantID, siteID); err != nil {
			s.logger.Warn("ai readiness: tool-list read not queued",
				slog.String("site_id", siteID.String()), slog.Any("error", err))
		} else {
			res.Abilities = true
		}
	}

	s.recordRefresh(ctx, p, siteID, res)
	return res, nil
}

// recordRefresh writes the audit row. It is best effort, like the updates
// refresh: a failed audit write does not undo a refresh that was queued.
func (s *Service) recordRefresh(ctx context.Context, p domain.Principal, siteID uuid.UUID, res RefreshResult) {
	if s.audit == nil {
		return
	}
	actorType, actorID := audit.ActorFor(p)
	if _, err := s.audit.Record(ctx, audit.Event{
		TenantID:   p.TenantID,
		ActorType:  actorType,
		ActorID:    actorID,
		Action:     audit.ActionAIReadinessRefreshRequested,
		TargetType: "site",
		TargetID:   siteID.String(),
		Metadata:   map[string]any{"site_id": siteID.String(), "abilities": res.Abilities},
	}); err != nil {
		s.logger.Warn("ai readiness: audit write failed",
			slog.String("site_id", siteID.String()), slog.Any("error", err))
	}
}
