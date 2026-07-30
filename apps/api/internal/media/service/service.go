// Package service is the Media Optimizer orchestration layer (ADR-043). It owns
// the optimize/restore/delete/sync state machine, the CP→agent signed-command
// dispatch, the agent-callback handlers, and SSE/audit emission. It is PURE Go
// (no encoder import); the actual encode runs in the separate media-encoder
// process over the model.EncodeArgs River jobs this service enqueues.
package service

import (
	"context"
	"fmt"
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
	// RatePerSite / RatePerTenant / RateWindow cap optimize REQUESTS per window
	// (one unit per bulk-optimize call, NOT per image — see StartOptimize +
	// ratelimit.go). A backstop against a runaway loop of repeated calls.
	RatePerSite   int
	RatePerTenant int
	RateWindow    time.Duration
}

// Service orchestrates the media domain.
type Service struct {
	repo     Repo
	enqueuer EncodeEnqueuer
	waker    EncoderWaker
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

// SetWaker wires the media-encoder waker (cloud scale-to-zero only). Optional —
// a nil waker leaves HandleEncodeReady's Kick a no-op (self-host always-on
// encoder).
func (s *Service) SetWaker(w EncoderWaker) { s.waker = w }

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
	// Allocate this sync run's generation. Every asset the agent upserts via
	// sync-batch is stamped with it; the sync-finalize callback then sweeps any
	// asset still on an older generation (gone in WP). UnixMicro is monotone
	// per run and never 0, so finalize's "gen==0 → never sweep" guard is safe.
	syncGen := s.clock.Now().UnixMicro()
	job, err := s.repo.InsertJob(ctx, tenantID, repo.InsertJobInput{
		ID:              jobID,
		SiteID:          siteID,
		Kind:            model.JobSync,
		InitiatorUserID: initiator,
		SyncGeneration:  &syncGen,
	})
	if err != nil {
		return SyncResult{}, err
	}
	s.publish(ctx, tenantID, siteID, site.EventMediaSyncStarted, map[string]any{"job_id": jobID})
	s.recordAudit(ctx, tenantID, p, audit.ActionMediaSyncStarted, jobID, map[string]any{"site_id": siteID.String()})

	if s.cmd == nil {
		return SyncResult{}, domain.ServiceUnavailable("media_agent_unwired", "media agent client is not wired")
	}
	ack, err := s.cmd.MediaSync(ctx, siteID, si.URL, agentcmd.MediaSyncRequest{
		JobID:            jobID,
		BatchEndpoint:    s.callbackURL("/agent/v1/media/sync-batch"),
		FinalizeEndpoint: s.callbackURL("/agent/v1/media/sync-finalize"),
	})
	if err != nil {
		s.failJob(ctx, tenantID, siteID, jobID, "sync dispatch failed: "+err.Error())
		return SyncResult{}, domain.Internal("media_sync_dispatch_failed", "failed to dispatch media sync").WithCause(err)
	}
	// The ack's ok field is the agent REFUSING the command on an otherwise fine
	// 200. This work is asynchronous (the agent pushes pages back to
	// BatchEndpoint), so a refused dispatch means no callback will ever arrive
	// and the job would sit "running" forever. Only the transport error was
	// checked before, which left exactly that hole.
	if !ack.OK {
		detail := detailOr(ack.Detail, "agent returned ok=false")
		s.failJob(ctx, tenantID, siteID, jobID, "sync rejected by agent: "+detail)
		return SyncResult{}, domain.Internal("media_sync_rejected", "agent rejected media sync: "+detail)
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
	// OPTIMIZE-ONLY gate: drop non-optimizable-source assets (webp/avif/svg/…) the
	// encoder can't process, so they never become a dangling "queued" job (the
	// .webp stuck-job class). This filter lives HERE, not in the shared
	// resolveAssets, so restore/delete-originals keep every selected asset.
	// (The all_pending path is already MIME-gated in SQL, so this is a no-op there.)
	optimizable := assets[:0]
	for _, a := range assets {
		if media.IsOptimizableMime(a.OriginalMime) {
			optimizable = append(optimizable, a)
		}
	}
	assets = optimizable
	if len(assets) == 0 {
		return BatchResult{}, domain.Validation("no_assets", "no eligible assets to optimize")
	}
	// Rate-limit by REQUEST, not image count: one unit per optimize call. The
	// whole point of the feature is "optimize my entire library in one click",
	// so gating on len(assets) made any library larger than perSite (446 > 200)
	// impossible to optimize — and no fixed image cap could ever scale to a mega
	// library. The fan-out is already bounded downstream (the agent drains
	// uploads in time-budgeted chunks, the encoder runs a fixed MaxWorkers pool,
	// the River queue is durable), so a single large batch is safe. We only need
	// to stop a runaway LOOP of repeated bulk-optimize calls.
	if !s.limiter.allow(tenantID, siteID, 1) {
		return BatchResult{}, domain.RateLimited("media_rate_limited", "too many optimize requests; retry shortly")
	}

	batchID := siteevents.NewULID(s.clock.Now())
	initiator := userPtr(p)
	jobIDs := make([]string, 0, len(assets))
	jobs := make([]agentcmd.MediaJobRef, 0, len(assets))
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
		jobs = append(jobs, agentcmd.MediaJobRef{JobID: jobID, WPAttachmentID: a.WPAttachmentID})
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
	// Dispatch the media_optimize command in the BACKGROUND. The agent
	// synchronously enumerates sizes, presigns, and uploads the source bytes to
	// S3 before it acks (~6s for a large image), and blocking the HTTP handler on
	// that round-trip made POST /media/optimize take 6-7s. Detach so the 202
	// returns immediately; a dispatch failure surfaces asynchronously via failJob
	// → media.job.failed SSE + the job's failed state (which the UI renders).
	dispatchReq := agentcmd.MediaOptimizeRequest{
		Jobs:            jobs,
		TargetFormat:    targetFormat,
		TargetQuality:   targetQuality,
		PresignEndpoint: s.callbackURL("/agent/v1/media/presign"),
		ReadyEndpoint:   s.callbackURL("/agent/v1/media/encode-ready"),
	}
	siteURL := si.URL
	jobIDsCopy := append([]string(nil), jobIDs...)
	go func() {
		// Detach from the request ctx (it cancels the instant the 202 is written)
		// but bound it so a hung agent POST cannot leak a goroutine forever.
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		ack, err := s.cmd.MediaOptimize(bg, siteID, siteURL, dispatchReq)
		switch {
		case err != nil:
			// Genuine dispatch failures (agent unreachable, 4xx/5xx ack, or — with
			// a slow-acking agent — an http.Client.Timeout) still fail the batch's
			// jobs so the UI does not show them stuck "optimizing" forever. This
			// goes through the GUARDED failJob, which re-reads each job and refuses
			// to clobber any that the agent already finalized as succeeded — the
			// success/fail race that previously turned succeeded jobs into failed.
			s.logger.Error("media optimize dispatch failed", "site_id", siteID.String(), "err", err.Error())
			for _, jid := range jobIDsCopy {
				s.failJob(bg, tenantID, siteID, jid, "optimize dispatch failed: "+err.Error())
			}
		case !ack.OK:
			// A 200 whose body says ok=false is the agent refusing the batch. It
			// produces exactly the "stuck optimizing forever" state the transport
			// branch above exists to prevent, because the agent will never call
			// PresignEndpoint or ReadyEndpoint. Fail the jobs through the same
			// guarded path.
			detail := detailOr(ack.Detail, "agent returned ok=false")
			s.logger.Error("media optimize rejected by agent", "site_id", siteID.String(), "detail", detail)
			for _, jid := range jobIDsCopy {
				s.failJob(bg, tenantID, siteID, jid, "optimize rejected by agent: "+detail)
			}
		}
	}()
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
	jobs := make([]agentcmd.MediaJobRef, 0, len(assets))
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
		jobs = append(jobs, agentcmd.MediaJobRef{JobID: jobID, WPAttachmentID: a.WPAttachmentID})
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
	ack, err := s.cmd.MediaRestore(ctx, siteID, si.URL, agentcmd.MediaRestoreRequest{
		Jobs:           jobs,
		StatusEndpoint: s.callbackURL("/agent/v1/media/restore-status"),
	})
	if err != nil {
		for _, jid := range jobIDs {
			s.failJob(ctx, tenantID, siteID, jid, "restore dispatch failed: "+err.Error())
		}
		return BatchResult{}, domain.Internal("media_restore_dispatch_failed", "failed to dispatch media restore").WithCause(err)
	}
	// ok=false on a 200 means the agent refused: no restore ran and no callback to
	// StatusEndpoint will arrive. Reporting the batch as queued here would tell
	// the operator their originals were being restored when nothing was.
	if !ack.OK {
		detail := detailOr(ack.Detail, "agent returned ok=false")
		for _, jid := range jobIDs {
			s.failJob(ctx, tenantID, siteID, jid, "restore rejected by agent: "+detail)
		}
		return BatchResult{}, domain.Internal("media_restore_rejected", "agent rejected media restore: "+detail)
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
	jobs := make([]agentcmd.MediaJobRef, 0, len(assets))
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
		jobs = append(jobs, agentcmd.MediaJobRef{JobID: jobID, WPAttachmentID: a.WPAttachmentID})
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
	ack, err := s.cmd.MediaDeleteOriginals(ctx, siteID, si.URL, agentcmd.MediaDeleteOriginalsRequest{
		Jobs:           jobs,
		StatusEndpoint: s.callbackURL("/agent/v1/media/job-status"),
	})
	if err != nil {
		for _, jid := range jobIDs {
			s.failJob(ctx, tenantID, siteID, jid, "delete-originals dispatch failed: "+err.Error())
		}
		return BatchResult{}, domain.Internal("media_delete_dispatch_failed", "failed to dispatch delete originals").WithCause(err)
	}
	// ok=false on a 200 means the agent refused the (irreversible) delete. The
	// operator already consented and the consent is audited above, so silently
	// reporting the batch as queued would leave the audit trail claiming a
	// destructive action that never happened.
	if !ack.OK {
		detail := detailOr(ack.Detail, "agent returned ok=false")
		for _, jid := range jobIDs {
			s.failJob(ctx, tenantID, siteID, jid, "delete-originals rejected by agent: "+detail)
		}
		return BatchResult{}, domain.Internal("media_delete_rejected", "agent rejected delete originals: "+detail)
	}
	return BatchResult{BatchJobID: batchID, QueuedCount: len(jobIDs)}, nil
}

// CancelResult is returned by Cancel.
type CancelResult struct {
	OK             bool
	CancelledCount int64
}

// DefaultMediaSettings returns the effective settings for a site when no row
// exists yet (auto-optimize off; webp / lossy defaults).
func DefaultMediaSettings(tenantID, siteID uuid.UUID) model.MediaSettings {
	return model.MediaSettings{
		TenantID:            tenantID,
		SiteID:              siteID,
		AutoOptimizeEnabled: false,
		AutoTargetFormat:    media.TargetWebP,
		AutoTargetQuality:   media.QualityLossy,
	}
}

// GetMediaSettings returns the stored media settings or the defaults.
func (s *Service) GetMediaSettings(ctx context.Context, tenantID, siteID uuid.UUID) (model.MediaSettings, error) {
	settings, found, err := s.repo.GetMediaSettings(ctx, tenantID, siteID)
	if err != nil {
		return model.MediaSettings{}, err
	}
	if !found {
		return DefaultMediaSettings(tenantID, siteID), nil
	}
	return settings, nil
}

// SaveMediaSettings validates, upserts, and best-effort pushes the new settings
// to the agent via the sync_media_config command. Returns the stored settings.
// A push failure after a successful upsert is returned as a non-domain error so
// the handler can surface it as a header warning (mirroring SaveConfig in security).
func (s *Service) SaveMediaSettings(ctx context.Context, tenantID, siteID uuid.UUID, in repo.UpsertMediaSettingsInput, p domain.Principal) (model.MediaSettings, error) {
	if !media.ValidTargetFormat(in.AutoTargetFormat) {
		return model.MediaSettings{}, domain.Validation("invalid_target_format", "auto_target_format must be avif, webp, or original")
	}
	if !media.ValidTargetQuality(in.AutoTargetQuality) {
		return model.MediaSettings{}, domain.Validation("invalid_target_quality", "auto_target_quality must be lossy or lossless")
	}
	if in.AutoTargetQuality == "" {
		in.AutoTargetQuality = media.QualityLossy
	}

	saved, err := s.repo.UpsertMediaSettings(ctx, tenantID, siteID, in)
	if err != nil {
		return model.MediaSettings{}, err
	}

	// Best-effort push to the agent (mirrors security.SaveConfig pattern).
	if s.cmd != nil && s.sites != nil {
		si, lookupErr := s.sites.GetMediaSiteInfo(ctx, tenantID, siteID)
		if lookupErr == nil && si.Enrolled {
			req := agentcmd.MediaConfigRequest{
				Enabled:       saved.AutoOptimizeEnabled,
				TargetFormat:  saved.AutoTargetFormat,
				TargetQuality: saved.AutoTargetQuality,
			}
			if _, pushErr := s.cmd.SyncMediaConfig(ctx, siteID, si.URL, req); pushErr != nil {
				s.logger.Warn("media settings stored but sync_media_config push failed",
					"site_id", siteID.String(), "err", pushErr.Error())
				// Return the stored settings + wrapped push error so the handler
				// can surface it as an X-Agent-Push-Warning header (non-fatal).
				return saved, fmt.Errorf("settings stored but agent push failed: %w", pushErr)
			}
		}
		// Site not enrolled or lookup failure — still non-fatal; config is persisted.
	}

	s.recordAudit(ctx, tenantID, p, audit.ActionMediaSettingsUpdated, siteID.String(), map[string]any{
		"site_id":               siteID.String(),
		"auto_optimize_enabled": saved.AutoOptimizeEnabled,
		"auto_target_format":    saved.AutoTargetFormat,
		"auto_target_quality":   saved.AutoTargetQuality,
	})

	return saved, nil
}

// Cancel cancels all non-terminal jobs for a site. It also proactively cancels
// any corresponding River media_encode jobs (m51) so the encoder is never woken
// for discarded work. River cancels are best-effort: a failure to cancel a
// River job is logged and does not fail the operator's cancel request — the
// media_optimization_jobs state is the source of truth, and the encoder worker
// self-heals on a missing row (river.JobCancel on not-found).
func (s *Service) Cancel(ctx context.Context, tenantID, siteID uuid.UUID, p domain.Principal) (CancelResult, error) {
	res, err := s.repo.CancelJobs(ctx, tenantID, siteID)
	if err != nil {
		return CancelResult{}, err
	}
	if res.CancelledCount > 0 {
		s.recordAudit(ctx, tenantID, p, audit.ActionMediaCancelled, siteID.String(), map[string]any{
			"site_id":         siteID.String(),
			"cancelled_count": res.CancelledCount,
		})
	}
	// Proactively cancel any River media_encode jobs whose IDs were stored on
	// the cancelled rows. Best-effort: log failures but never surface them to
	// the operator — the River worker self-heals (discards the job) when it
	// finds the media row already terminal.
	if s.enqueuer != nil {
		for _, rid := range res.EncodeRiverIDs {
			if cerr := s.enqueuer.CancelEncodeJob(ctx, rid); cerr != nil {
				s.logger.Warn("media cancel: could not cancel River encode job (best-effort)",
					"river_job_id", rid,
					"site_id", siteID.String(),
					"err", cerr.Error())
			}
		}
	}
	return CancelResult{OK: true, CancelledCount: res.CancelledCount}, nil
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
		// ListPendingAssetIDs filters to optimizable MIMEs at the SQL level, so a
		// non-optimizable pending asset (webp/avif/gif/svg) never gets a job the
		// agent would skip and leave dangling "queued".
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
		// NO MIME filter here: this helper is SHARED by optimize, restore, and
		// delete-originals. Restore/delete must keep EVERY selected asset
		// regardless of source format — a webp/avif/svg "original" is fully
		// restorable/deletable. The optimizable-MIME gate (which avoids a dangling
		// encode job for a non-encodable source) is applied in StartOptimize ONLY.
		out = append(out, a)
	}
	return out, nil
}

// detailOr returns primary when it is non-empty, else fallback. Used to keep the
// agent's own rejection detail in a dispatch failure message without emitting an
// empty "rejected by agent: " string when the agent sent ok=false and no detail.
func detailOr(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}

// failJob transitions a NON-TERMINAL job to failed + publishes media.job.failed
// (best-effort). It MUST NOT clobber a job the agent already completed: a
// dispatch-timeout-driven failJob can race the agent's own job-status callback,
// and several jobs in a bulk batch may already be succeeded/partially_succeeded
// (or operator-cancelled) by the time the dispatch error surfaces.
//
// The race is closed in TWO layers:
//   - DB layer (authoritative): repo.FinalizeJobAgent's UPDATE carries a
//     `WHERE state NOT IN (terminal states)` guard, so a terminal row's state is
//     NEVER overwritten — on a guard miss it returns the EXISTING row, no error.
//   - SSE layer (this method): we inspect the returned row and only emit
//     media.job.failed when THIS call actually drove the job to failed. If the
//     row was already terminal (succeeded / partially_succeeded / cancelled, or
//     a concurrent writer beat us), we skip the misleading failed event entirely.
func (s *Service) failJob(ctx context.Context, tenantID, siteID uuid.UUID, jobID, reason string) {
	job, err := s.repo.FinalizeJobAgent(ctx, jobID, repo.FinalizeJobInput{
		State:       model.JobFailed,
		ErrorReason: reason,
	})
	// If the guarded UPDATE found the row already terminal it returns that row
	// unchanged (no error). Only publish the failure when WE transitioned it to
	// failed — never when the agent already finalized it (success/fail race).
	if err == nil && job.State != model.JobFailed {
		s.logger.Info("media failJob skipped: job already terminal",
			"job_id", jobID, "state", string(job.State), "reason", reason)
		return
	}
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
