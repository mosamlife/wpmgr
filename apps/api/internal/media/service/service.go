// Package service is the Media Optimizer orchestration layer (ADR-043). It owns
// the optimize/restore/delete/sync state machine, the CP→agent signed-command
// dispatch, the agent-callback handlers, and SSE/audit emission. It is PURE Go
// (no encoder import); the actual encode runs in the separate media-encoder
// process over the model.EncodeArgs River jobs this service enqueues.
package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/media"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/repo"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	siteevents "github.com/mosamlife/wpmgr/apps/api/internal/site/events"
)

// Config tunes the service.
type Config struct {
	// PresignTTL bounds every minted presigned URL (agent upload / encoder I/O).
	PresignTTL time.Duration
	// CPBaseURL is the public base URL the agent reaches the CP callbacks at.
	CPBaseURL string
	// RatePerSite / RatePerTenant / RateWindow cap encode enqueues (ratelimit.go).
	RatePerSite   int
	RatePerTenant int
	RateWindow    time.Duration
}

// Service orchestrates the media domain.
type Service struct {
	repo     Repo
	enqueuer EncodeEnqueuer
	cmd      AgentMediaClient
	sites    SiteLookup
	store    Presigner
	events   EventPublisher
	audit    *audit.Recorder
	clock    domain.Clock
	cfg      Config
	limiter  *rateLimiter
	logger   *slog.Logger
}

// NewService builds a Service. enqueuer/cmd/sites/store/events may be wired
// after construction (SetEnqueuer / SetAgentClient) — the operator routes that
// need them return a 503 until they are set, mirroring scan.
func NewService(r Repo, store Presigner, events EventPublisher, rec *audit.Recorder, clock domain.Clock, cfg Config, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.PresignTTL <= 0 {
		cfg.PresignTTL = 15 * time.Minute
	}
	if cfg.RateWindow <= 0 {
		cfg.RateWindow = time.Minute
	}
	return &Service{
		repo:    r,
		store:   store,
		events:  events,
		audit:   rec,
		clock:   clock,
		cfg:     cfg,
		limiter: newRateLimiter(cfg.RatePerSite, cfg.RatePerTenant, cfg.RateWindow),
		logger:  logger,
	}
}

// SetEnqueuer wires the River EncodeArgs enqueuer after River starts.
func (s *Service) SetEnqueuer(e EncodeEnqueuer) { s.enqueuer = e }

// SetAgentClient wires the CP→agent commander + site lookup.
func (s *Service) SetAgentClient(cmd AgentMediaClient, sites SiteLookup) {
	s.cmd = cmd
	s.sites = sites
}

// Repo exposes the underlying repo for handlers that need by-id reads with the
// service's tenant gating already applied (mirrors backup.Service.repo usage).
func (s *Service) RepoForReads() Repo { return s.repo }

// ---------------------------------------------------------------------------
// Operator-facing methods
// ---------------------------------------------------------------------------

// ListAssetsResult bundles a page of assets + the summary rollup.
type ListAssetsResult struct {
	Items      []model.Asset
	NextCursor string
	Summary    model.AssetSummary
}

// ListAssets returns a cursor page of assets plus the site summary.
func (s *Service) ListAssets(ctx context.Context, tenantID, siteID uuid.UUID, in repo.ListAssetsInput) (ListAssetsResult, error) {
	in.TenantID = tenantID
	in.SiteID = siteID
	items, next, err := s.repo.ListAssets(ctx, in)
	if err != nil {
		return ListAssetsResult{}, err
	}
	summary, err := s.repo.Summary(ctx, tenantID, siteID)
	if err != nil {
		return ListAssetsResult{}, err
	}
	return ListAssetsResult{Items: items, NextCursor: next, Summary: summary}, nil
}

// ListJobs returns a cursor page of jobs for a site.
func (s *Service) ListJobs(ctx context.Context, tenantID, siteID uuid.UUID, in repo.ListJobsInput) ([]model.Job, string, error) {
	in.TenantID = tenantID
	in.SiteID = siteID
	return s.repo.ListJobs(ctx, in)
}

// JobDetail bundles a job with its variant results.
type JobDetail struct {
	Job      model.Job
	Variants []model.VariantResult
}

// GetJob returns a job + its variants. p gates per-site access for collaborators.
func (s *Service) GetJob(ctx context.Context, tenantID uuid.UUID, jobID string, p domain.Principal) (JobDetail, error) {
	job, err := s.repo.GetJob(ctx, tenantID, jobID)
	if err != nil {
		return JobDetail{}, err
	}
	// The job is resolved by id; gate on site access so a site-scoped
	// collaborator cannot read another site's job by passing its id.
	if !p.CanAccessSite(job.SiteID) {
		return JobDetail{}, domain.Forbidden("forbidden", "you do not have access to this site")
	}
	variants, err := s.repo.ListVariantsForJob(ctx, tenantID, jobID)
	if err != nil {
		return JobDetail{}, err
	}
	return JobDetail{Job: job, Variants: variants}, nil
}

// SyncResult is returned by Sync.
type SyncResult struct {
	JobID     string
	StartedAt time.Time
}

// Sync creates a sync job and dispatches the media_sync command so the agent
// enumerates its library into site_media_assets via the sync-batch callback.
func (s *Service) Sync(ctx context.Context, tenantID, siteID uuid.UUID, p domain.Principal) (SyncResult, error) {
	si, err := s.requireEnrolled(ctx, tenantID, siteID)
	if err != nil {
		return SyncResult{}, err
	}
	jobID := siteevents.NewULID(s.clock.Now())
	initiator := userPtr(p)
	job, err := s.repo.InsertJob(ctx, tenantID, repo.InsertJobInput{
		ID:              jobID,
		SiteID:          siteID,
		Kind:            model.JobSync,
		InitiatorUserID: initiator,
	})
	if err != nil {
		return SyncResult{}, err
	}
	s.publish(ctx, tenantID, siteID, site.EventMediaSyncStarted, map[string]any{"job_id": jobID})
	s.recordAudit(ctx, tenantID, p, audit.ActionMediaSyncStarted, jobID, map[string]any{"site_id": siteID.String()})

	if s.cmd == nil {
		return SyncResult{}, domain.ServiceUnavailable("media_agent_unwired", "media agent client is not wired")
	}
	if _, err := s.cmd.MediaSync(ctx, siteID, si.URL, agentcmd.MediaSyncRequest{
		JobID:         jobID,
		BatchEndpoint: s.callbackURL("/agent/v1/media/sync-batch"),
	}); err != nil {
		s.failJob(ctx, tenantID, siteID, jobID, "sync dispatch failed: "+err.Error())
		return SyncResult{}, domain.Internal("media_sync_dispatch_failed", "failed to dispatch media sync").WithCause(err)
	}
	return SyncResult{JobID: jobID, StartedAt: job.CreatedAt}, nil
}

// BatchResult is returned by the bulk start methods.
type BatchResult struct {
	BatchJobID  string
	QueuedCount int
}

// StartOptimize creates one job per attachment (fan-out — ADR-043 §3) and
// dispatches the media_optimize command so the agent uploads sources. assetIDs
// selects specific assets; allPending fans out over all pending/failed assets.
func (s *Service) StartOptimize(ctx context.Context, tenantID, siteID uuid.UUID, assetIDs []uuid.UUID, allPending bool, targetFormat, targetQuality string, p domain.Principal) (BatchResult, error) {
	if !media.ValidTargetFormat(targetFormat) {
		return BatchResult{}, domain.Validation("invalid_target_format", "target_format must be avif, webp, or original")
	}
	if !media.ValidTargetQuality(targetQuality) {
		return BatchResult{}, domain.Validation("invalid_target_quality", "target_quality must be lossy or lossless")
	}
	if targetQuality == "" {
		targetQuality = media.QualityLossy
	}
	si, err := s.requireEnrolled(ctx, tenantID, siteID)
	if err != nil {
		return BatchResult{}, err
	}

	assets, err := s.resolveAssets(ctx, tenantID, siteID, assetIDs, allPending)
	if err != nil {
		return BatchResult{}, err
	}
	if len(assets) == 0 {
		return BatchResult{}, domain.Validation("no_assets", "no eligible assets to optimize")
	}
	// Rate-limit the encode fan-out so a runaway bulk action can't flood the
	// encoder queue / the agent.
	if !s.limiter.allow(tenantID, siteID, len(assets)) {
		return BatchResult{}, domain.RateLimited("media_rate_limited", "media optimize rate limit exceeded; retry shortly")
	}

	batchID := siteevents.NewULID(s.clock.Now())
	initiator := userPtr(p)
	jobIDs := make([]string, 0, len(assets))
	for _, a := range assets {
		jobID := siteevents.NewULID(s.clock.Now())
		assetID := a.ID
		if _, err := s.repo.InsertJob(ctx, tenantID, repo.InsertJobInput{
			ID:              jobID,
			SiteID:          siteID,
			AssetID:         &assetID,
			WPAttachmentID:  a.WPAttachmentID,
			Kind:            model.JobOptimize,
			TargetFormat:    targetFormat,
			TargetQuality:   targetQuality,
			InitiatorUserID: initiator,
		}); err != nil {
			return BatchResult{}, err
		}
		_ = s.repo.SetAssetStatus(ctx, tenantID, a.ID, model.AssetOptimizing)
		jobIDs = append(jobIDs, jobID)
	}

	s.publish(ctx, tenantID, siteID, site.EventMediaOptimizeStarted, map[string]any{
		"batch_job_id":  batchID,
		"queued_count":  len(jobIDs),
		"target_format": targetFormat,
	})
	s.recordAudit(ctx, tenantID, p, audit.ActionMediaOptimizeStarted, batchID, map[string]any{
		"site_id":       siteID.String(),
		"queued_count":  len(jobIDs),
		"target_format": targetFormat,
	})

	if s.cmd == nil {
		return BatchResult{}, domain.ServiceUnavailable("media_agent_unwired", "media agent client is not wired")
	}
	if _, err := s.cmd.MediaOptimize(ctx, siteID, si.URL, agentcmd.MediaOptimizeRequest{
		JobIDs:          jobIDs,
		TargetFormat:    targetFormat,
		TargetQuality:   targetQuality,
		PresignEndpoint: s.callbackURL("/agent/v1/media/presign"),
		ReadyEndpoint:   s.callbackURL("/agent/v1/media/encode-ready"),
	}); err != nil {
		for _, jid := range jobIDs {
			s.failJob(ctx, tenantID, siteID, jid, "optimize dispatch failed: "+err.Error())
		}
		return BatchResult{}, domain.Internal("media_optimize_dispatch_failed", "failed to dispatch media optimize").WithCause(err)
	}
	return BatchResult{BatchJobID: batchID, QueuedCount: len(jobIDs)}, nil
}

// StartRestore creates one restore job per attachment and dispatches the
// media_restore command.
func (s *Service) StartRestore(ctx context.Context, tenantID, siteID uuid.UUID, assetIDs []uuid.UUID, p domain.Principal) (BatchResult, error) {
	si, err := s.requireEnrolled(ctx, tenantID, siteID)
	if err != nil {
		return BatchResult{}, err
	}
	assets, err := s.resolveAssets(ctx, tenantID, siteID, assetIDs, false)
	if err != nil {
		return BatchResult{}, err
	}
	if len(assets) == 0 {
		return BatchResult{}, domain.Validation("no_assets", "no eligible assets to restore")
	}

	batchID := siteevents.NewULID(s.clock.Now())
	initiator := userPtr(p)
	jobIDs := make([]string, 0, len(assets))
	for _, a := range assets {
		if a.Status == model.AssetOriginalsDeleted {
			return BatchResult{}, domain.Conflict("originals_deleted_cannot_restore",
				"originals were deleted for an attachment in the selection; restore is impossible")
		}
		jobID := siteevents.NewULID(s.clock.Now())
		assetID := a.ID
		if _, err := s.repo.InsertJob(ctx, tenantID, repo.InsertJobInput{
			ID:              jobID,
			SiteID:          siteID,
			AssetID:         &assetID,
			WPAttachmentID:  a.WPAttachmentID,
			Kind:            model.JobRestore,
			InitiatorUserID: initiator,
		}); err != nil {
			return BatchResult{}, err
		}
		_ = s.repo.SetAssetStatus(ctx, tenantID, a.ID, model.AssetRestoring)
		jobIDs = append(jobIDs, jobID)
	}

	s.publish(ctx, tenantID, siteID, site.EventMediaRestoreStarted, map[string]any{
		"batch_job_id": batchID,
		"queued_count": len(jobIDs),
	})
	s.recordAudit(ctx, tenantID, p, audit.ActionMediaRestoreStarted, batchID, map[string]any{
		"site_id":      siteID.String(),
		"queued_count": len(jobIDs),
	})

	if s.cmd == nil {
		return BatchResult{}, domain.ServiceUnavailable("media_agent_unwired", "media agent client is not wired")
	}
	if _, err := s.cmd.MediaRestore(ctx, siteID, si.URL, agentcmd.MediaRestoreRequest{
		JobIDs:         jobIDs,
		StatusEndpoint: s.callbackURL("/agent/v1/media/restore-status"),
	}); err != nil {
		for _, jid := range jobIDs {
			s.failJob(ctx, tenantID, siteID, jid, "restore dispatch failed: "+err.Error())
		}
		return BatchResult{}, domain.Internal("media_restore_dispatch_failed", "failed to dispatch media restore").WithCause(err)
	}
	return BatchResult{BatchJobID: batchID, QueuedCount: len(jobIDs)}, nil
}

// StartDeleteOriginals creates one delete_originals job per attachment and
// dispatches the (IRREVERSIBLE) media_delete_originals command. Gated at the
// handler on PermMediaDeleteOriginals; the destructive consent is audited with
// ActorUser here.
func (s *Service) StartDeleteOriginals(ctx context.Context, tenantID, siteID uuid.UUID, assetIDs []uuid.UUID, p domain.Principal) (BatchResult, error) {
	si, err := s.requireEnrolled(ctx, tenantID, siteID)
	if err != nil {
		return BatchResult{}, err
	}
	assets, err := s.resolveAssets(ctx, tenantID, siteID, assetIDs, false)
	if err != nil {
		return BatchResult{}, err
	}
	if len(assets) == 0 {
		return BatchResult{}, domain.Validation("no_assets", "no eligible assets to delete originals for")
	}

	batchID := siteevents.NewULID(s.clock.Now())
	initiator := userPtr(p)
	jobIDs := make([]string, 0, len(assets))
	for _, a := range assets {
		if a.Status != model.AssetOptimized {
			return BatchResult{}, domain.Conflict("asset_not_optimized",
				"delete-originals requires an optimized attachment")
		}
		jobID := siteevents.NewULID(s.clock.Now())
		assetID := a.ID
		if _, err := s.repo.InsertJob(ctx, tenantID, repo.InsertJobInput{
			ID:              jobID,
			SiteID:          siteID,
			AssetID:         &assetID,
			WPAttachmentID:  a.WPAttachmentID,
			Kind:            model.JobDeleteOriginals,
			InitiatorUserID: initiator,
		}); err != nil {
			return BatchResult{}, err
		}
		jobIDs = append(jobIDs, jobID)
	}

	// Destructive consent: ActorUser so the hash chain attributes it.
	s.recordAudit(ctx, tenantID, p, audit.ActionMediaDeleteOriginalsConfirmed, batchID, map[string]any{
		"site_id":      siteID.String(),
		"queued_count": len(jobIDs),
		"irreversible": true,
	})

	if s.cmd == nil {
		return BatchResult{}, domain.ServiceUnavailable("media_agent_unwired", "media agent client is not wired")
	}
	if _, err := s.cmd.MediaDeleteOriginals(ctx, siteID, si.URL, agentcmd.MediaDeleteOriginalsRequest{
		JobIDs:         jobIDs,
		StatusEndpoint: s.callbackURL("/agent/v1/media/job-status"),
	}); err != nil {
		for _, jid := range jobIDs {
			s.failJob(ctx, tenantID, siteID, jid, "delete-originals dispatch failed: "+err.Error())
		}
		return BatchResult{}, domain.Internal("media_delete_dispatch_failed", "failed to dispatch delete originals").WithCause(err)
	}
	return BatchResult{BatchJobID: batchID, QueuedCount: len(jobIDs)}, nil
}

// CancelResult is returned by Cancel.
type CancelResult struct {
	OK             bool
	CancelledCount int64
}

// Cancel cancels all non-terminal jobs for a site.
func (s *Service) Cancel(ctx context.Context, tenantID, siteID uuid.UUID, p domain.Principal) (CancelResult, error) {
	n, err := s.repo.CancelJobs(ctx, tenantID, siteID)
	if err != nil {
		return CancelResult{}, err
	}
	if n > 0 {
		s.recordAudit(ctx, tenantID, p, audit.ActionMediaCancelled, siteID.String(), map[string]any{
			"site_id":         siteID.String(),
			"cancelled_count": n,
		})
	}
	return CancelResult{OK: true, CancelledCount: n}, nil
}

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

func (s *Service) requireEnrolled(ctx context.Context, tenantID, siteID uuid.UUID) (MediaSiteInfo, error) {
	if s.sites == nil {
		return MediaSiteInfo{}, domain.ServiceUnavailable("media_site_lookup_unwired", "media site lookup is not wired")
	}
	si, err := s.sites.GetMediaSiteInfo(ctx, tenantID, siteID)
	if err != nil {
		return MediaSiteInfo{}, err
	}
	if !si.Enrolled {
		return MediaSiteInfo{}, domain.ServiceUnavailable("site_not_enrolled", "site is not enrolled")
	}
	return si, nil
}

// resolveAssets returns the selected assets (by id) or all pending ones.
func (s *Service) resolveAssets(ctx context.Context, tenantID, siteID uuid.UUID, assetIDs []uuid.UUID, allPending bool) ([]model.Asset, error) {
	if allPending {
		return s.repo.ListPendingAssetIDs(ctx, tenantID, siteID, 500)
	}
	out := make([]model.Asset, 0, len(assetIDs))
	for _, id := range assetIDs {
		a, err := s.repo.GetAsset(ctx, tenantID, id)
		if err != nil {
			return nil, err
		}
		if a.SiteID != siteID {
			return nil, domain.Validation("asset_site_mismatch", "an asset in the selection does not belong to this site")
		}
		out = append(out, a)
	}
	return out, nil
}

// failJob transitions a job to failed + publishes media.job.failed (best-effort).
func (s *Service) failJob(ctx context.Context, tenantID, siteID uuid.UUID, jobID, reason string) {
	_, _ = s.repo.FinalizeJobAgent(ctx, jobID, repo.FinalizeJobInput{
		State:       model.JobFailed,
		ErrorReason: reason,
	})
	s.publish(ctx, tenantID, siteID, site.EventMediaJobFailed, map[string]any{
		"job_id": jobID,
		"reason": reason,
	})
}

func (s *Service) callbackURL(path string) string {
	return s.cfg.CPBaseURL + path
}

func (s *Service) publish(ctx context.Context, tenantID, siteID uuid.UUID, eventType string, data map[string]any) {
	if s.events == nil {
		return
	}
	_ = s.events.Publish(ctx, site.ConnectionEvent{
		Type:     eventType,
		TenantID: tenantID,
		SiteID:   siteID,
		Data:     data,
	})
}

func (s *Service) recordAudit(ctx context.Context, tenantID uuid.UUID, p domain.Principal, action, targetID string, meta map[string]any) {
	if s.audit == nil {
		return
	}
	actType := audit.ActorUser
	if p.Type == domain.PrincipalAPIKey {
		actType = audit.ActorAPIKey
	}
	_, _ = s.audit.Record(ctx, audit.Event{
		TenantID:   tenantID,
		ActorType:  actType,
		ActorID:    p.ActorID(),
		Action:     action,
		TargetType: "media_job",
		TargetID:   targetID,
		Metadata:   meta,
	})
}

func userPtr(p domain.Principal) *uuid.UUID {
	if p.Type == domain.PrincipalUser && p.UserID != uuid.Nil {
		id := p.UserID
		return &id
	}
	return nil
}
