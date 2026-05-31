package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/media"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/repo"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// These handlers run behind the agent Ed25519 signed-request middleware. The
// tenant + site come from the verified identity (NEVER a client header); each
// job is re-asserted to belong to that tenant + site before any mutation, so a
// compromised agent cannot manipulate another site's job even within its tenant.

// SyncBatchInput is one page of the agent's media enumeration.
type SyncBatchInput struct {
	Attachments []repo.UpsertAssetInput
}

// HandleSyncBatch upserts a page of attachments under the agent GUC. Returns the
// number of rows upserted.
func (s *Service) HandleSyncBatch(ctx context.Context, tenantID, siteID uuid.UUID, in SyncBatchInput) (int64, error) {
	if len(in.Attachments) > media.MaxSyncBatch {
		return 0, domain.Validation("sync_batch_too_large", "sync batch exceeds the per-page cap")
	}
	return s.repo.UpsertAssetsAgent(ctx, tenantID, siteID, in.Attachments)
}

// PresignVariant is one variant the agent wants to upload a source for.
type PresignVariant struct {
	Name       string
	SourceSize int64
	SourceMime string
}

// HandlePresign mints a presigned PUT URL for each variant's source object
// (media/<tenant>/<site>/<job>/src/<name>). Returns name -> PUT URL. The job is
// re-asserted to the agent's tenant+site first.
func (s *Service) HandlePresign(ctx context.Context, tenantID, siteID uuid.UUID, jobID string, variants []PresignVariant) (map[string]string, error) {
	if s.store == nil {
		return nil, domain.ServiceUnavailable("media_store_unwired", "object storage is not configured")
	}
	if _, err := s.assertJobSite(ctx, tenantID, siteID, jobID); err != nil {
		return nil, err
	}
	if len(variants) == 0 || len(variants) > media.MaxVariantsPerJob {
		return nil, domain.Validation("invalid_variant_count", "variants must be 1..10 per job")
	}
	out := make(map[string]string, len(variants))
	for _, v := range variants {
		// Reject hostile variant names before they reach the object key — a '.'
		// or '/' could escape the job prefix (storage-key path traversal).
		if !media.ValidVariantName(v.Name) {
			return nil, domain.Validation("invalid_variant_name", "variant name must be [A-Za-z0-9_-]{1,64}")
		}
		key := media.SrcKey(tenantID, siteID, jobID, v.Name)
		url, err := s.store.PresignPut(ctx, key, s.cfg.PresignTTL)
		if err != nil {
			return nil, domain.Internal("media_presign_failed", "failed to presign media upload").WithCause(err)
		}
		out[v.Name] = url
	}
	return out, nil
}

// EncodeReadyVariant is one uploaded source the agent reports ready to encode.
type EncodeReadyVariant struct {
	Name       string
	SourceSize int64
	SourceMime string
}

// HandleEncodeReady enqueues ONE EncodeArgs River job carrying the attachment's
// variants (≤10 — ADR-043 §3), marks the job in_progress, and publishes
// media.optimize.progress. The job is re-asserted to the agent's tenant+site.
func (s *Service) HandleEncodeReady(ctx context.Context, tenantID, siteID uuid.UUID, jobID string, variants []EncodeReadyVariant) error {
	job, err := s.assertJobSite(ctx, tenantID, siteID, jobID)
	if err != nil {
		return err
	}
	if job.State.Terminal() {
		return nil // dup callback against a finished/cancelled job
	}
	if len(variants) == 0 || len(variants) > media.MaxVariantsPerJob {
		return domain.Validation("invalid_variant_count", "variants must be 1..10 per job")
	}
	if s.enqueuer == nil {
		return domain.ServiceUnavailable("media_enqueuer_unwired", "media encode enqueuer is not wired")
	}

	encVariants := make([]model.EncodeVariant, 0, len(variants))
	for _, v := range variants {
		if !media.ValidVariantName(v.Name) {
			return domain.Validation("invalid_variant_name", "variant name must be [A-Za-z0-9_-]{1,64}")
		}
		encVariants = append(encVariants, model.EncodeVariant{
			Name:       v.Name,
			SourceSize: v.SourceSize,
			SourceMime: v.SourceMime,
		})
	}
	if err := s.repo.MarkJobInProgressAgent(ctx, jobID, len(encVariants)); err != nil {
		return err
	}
	if err := s.enqueuer.EnqueueEncode(ctx, model.EncodeArgs{
		TenantID:      tenantID,
		SiteID:        siteID,
		JobID:         jobID,
		TargetFormat:  job.TargetFormat,
		TargetQuality: job.TargetQuality,
		Variants:      encVariants,
	}); err != nil {
		s.failJob(ctx, tenantID, siteID, jobID, "encode enqueue failed: "+err.Error())
		return domain.Internal("media_encode_enqueue_failed", "failed to enqueue encode job").WithCause(err)
	}
	s.publish(ctx, tenantID, siteID, site.EventMediaOptimizeProgress, map[string]any{
		"job_id":         jobID,
		"variants_total": len(encVariants),
		"phase":          "encoding",
	})
	return nil
}

// ApplyStatusInput is the agent's post-apply report (job-status callback). It
// finalizes the asset row + the job and emits asset_done/completed.
type ApplyStatusInput struct {
	AppliedVariants  []string
	SizesUnoptimized map[string]string
	CurrentFormat    string
	CurrentSizeBytes int64
	BytesBefore      *int64
	BytesAfter       *int64
	CompressionLevel string
	TargetFormat     string
	OriginalsDeleted bool
	Error            string
}

// HandleApplyStatus finalizes a job after the agent applies (or deletes
// originals). It updates the asset mirror, the job state, deletes the temp S3
// objects, and emits SSE.
func (s *Service) HandleApplyStatus(ctx context.Context, tenantID, siteID uuid.UUID, jobID string, in ApplyStatusInput) error {
	job, err := s.assertJobSite(ctx, tenantID, siteID, jobID)
	if err != nil {
		return err
	}

	// Hard error from the agent → fail the job + asset.
	if in.Error != "" {
		s.failJob(ctx, tenantID, siteID, jobID, in.Error)
		if job.AssetID != nil {
			_ = s.repo.SetAssetStatus(ctx, tenantID, *job.AssetID, model.AssetFailed)
		}
		s.cleanupTempObjects(ctx, tenantID, siteID, jobID)
		return nil
	}

	// Delete-originals path: just flip the asset to originals_deleted + finalize.
	if job.Kind == model.JobDeleteOriginals {
		if job.AssetID != nil {
			_ = s.repo.SetAssetStatus(ctx, tenantID, *job.AssetID, model.AssetOriginalsDeleted)
		}
		_, _ = s.repo.FinalizeJobAgent(ctx, jobID, repo.FinalizeJobInput{State: model.JobSucceeded})
		s.publish(ctx, tenantID, siteID, site.EventMediaDeleteOriginalsCompleted, map[string]any{
			"job_id":           jobID,
			"wp_attachment_id": job.WPAttachmentID,
		})
		return nil
	}

	// Optimize-apply path: mirror the blob into the asset row.
	status := model.AssetOptimized
	if len(in.AppliedVariants) == 0 {
		status = model.AssetFailed
	}
	if _, err := s.repo.ApplyOptimizedAgent(ctx, tenantID, siteID, job.WPAttachmentID, repo.ApplyOptimizedInput{
		CurrentFormat:    orDefault(in.CurrentFormat, model.FormatOriginal),
		CurrentSizeBytes: in.CurrentSizeBytes,
		Status:           status,
		CompressionLevel: in.CompressionLevel,
		TargetFormat:     orDefault(in.TargetFormat, job.TargetFormat),
		SizesOptimized:   in.AppliedVariants,
		SizesUnoptimized: in.SizesUnoptimized,
	}); err != nil {
		return err
	}

	succeeded, failed, _ := s.repo.CountVariantStatesAgent(ctx, jobID)
	jobState := model.JobSucceeded
	switch {
	case succeeded == 0 && failed > 0:
		jobState = model.JobFailed
	case failed > 0:
		jobState = model.JobPartiallySucceeded
	}
	finalJob, _ := s.repo.FinalizeJobAgent(ctx, jobID, repo.FinalizeJobInput{
		State:             jobState,
		BytesBefore:       in.BytesBefore,
		BytesAfter:        in.BytesAfter,
		VariantsSucceeded: succeeded,
		VariantsFailed:    failed,
	})

	// Clean up the per-job temp objects (src/* + out/*).
	s.cleanupTempObjects(ctx, tenantID, siteID, jobID)

	s.publish(ctx, tenantID, siteID, site.EventMediaOptimizeAssetDone, map[string]any{
		"job_id":           jobID,
		"wp_attachment_id": job.WPAttachmentID,
		"applied":          len(in.AppliedVariants),
	})
	s.publish(ctx, tenantID, siteID, site.EventMediaOptimizeCompleted, map[string]any{
		"job_id": jobID,
		"state":  string(finalJob.State),
	})
	return nil
}

// RestoreStatusInput is the agent's restore report.
type RestoreStatusInput struct {
	Restored bool
	Error    string
}

// HandleRestoreStatus finalizes a restore job + the asset row.
func (s *Service) HandleRestoreStatus(ctx context.Context, tenantID, siteID uuid.UUID, jobID string, in RestoreStatusInput) error {
	job, err := s.assertJobSite(ctx, tenantID, siteID, jobID)
	if err != nil {
		return err
	}
	if in.Error != "" || !in.Restored {
		reason := in.Error
		if reason == "" {
			reason = "restore reported not restored"
		}
		s.failJob(ctx, tenantID, siteID, jobID, reason)
		if job.AssetID != nil {
			_ = s.repo.SetAssetStatus(ctx, tenantID, *job.AssetID, model.AssetFailed)
		}
		return nil
	}
	if _, err := s.repo.RestoreAssetAgent(ctx, tenantID, siteID, job.WPAttachmentID); err != nil {
		return err
	}
	_, _ = s.repo.FinalizeJobAgent(ctx, jobID, repo.FinalizeJobInput{State: model.JobSucceeded})
	s.cleanupTempObjects(ctx, tenantID, siteID, jobID)
	s.publish(ctx, tenantID, siteID, site.EventMediaRestoreAssetDone, map[string]any{
		"job_id":           jobID,
		"wp_attachment_id": job.WPAttachmentID,
	})
	s.publish(ctx, tenantID, siteID, site.EventMediaRestoreCompleted, map[string]any{"job_id": jobID})
	return nil
}

// assertJobSite loads a job under the agent GUC and verifies it belongs to the
// agent's verified tenant + site.
func (s *Service) assertJobSite(ctx context.Context, tenantID, siteID uuid.UUID, jobID string) (model.Job, error) {
	job, err := s.repo.GetJobAgent(ctx, jobID)
	if err != nil {
		return model.Job{}, err
	}
	if job.TenantID != tenantID || job.SiteID != siteID {
		return model.Job{}, domain.Forbidden("media_job_site_mismatch", "the job does not belong to this site")
	}
	return job, nil
}

// cleanupTempObjects best-effort deletes every temp object under a job prefix
// (ADR-043 §2 — no media bytes persist on the CP). Failures are logged, not
// fatal (the GC sweep is the backstop).
func (s *Service) cleanupTempObjects(ctx context.Context, tenantID, siteID uuid.UUID, jobID string) {
	if s.store == nil {
		return
	}
	prefix := media.JobPrefix(tenantID, siteID, jobID) + "/"
	keys, err := s.store.List(ctx, prefix)
	if err != nil {
		s.logger.Warn("media temp cleanup list failed", "job_id", jobID, "err", err.Error())
		return
	}
	for _, k := range keys {
		if derr := s.store.Delete(ctx, k); derr != nil {
			s.logger.Warn("media temp cleanup delete failed", "key_prefix", prefix, "err", derr.Error())
		}
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
