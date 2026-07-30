package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/media"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/encoder"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/repo"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// EncodeJobRepo is the subset of the media repo the encode worker needs. It runs
// cross-tenant under the agent GUC (the worker resolves the tenant from the job
// args). *repo.Repo satisfies it.
type EncodeJobRepo interface {
	GetJobAgent(ctx context.Context, jobID string) (model.Job, error)
	UpsertVariantAgent(ctx context.Context, tenantID uuid.UUID, in repo.UpsertVariantInput) error
	CountVariantStatesAgent(ctx context.Context, jobID string) (succeeded, failed int, err error)
	FinalizeJobAgent(ctx context.Context, jobID string, in repo.FinalizeJobInput) (model.Job, error)
}

// Presigner mints presigned GET/PUT URLs + deletes temp objects. *blobstore.Store
// satisfies it. The worker NEVER calls a live GetObject (GCS 403s) — presigned
// URLs only; Delete is the live S3 DeleteObject (allowed; only GET/PUT 403 on GCS).
type Presigner interface {
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error)
	Delete(ctx context.Context, key string) error
}

// EventPublisher publishes media.* SSE envelopes.
type EventPublisher interface {
	Publish(ctx context.Context, ev site.ConnectionEvent) error
}

// SiteLookup resolves the agent URL for the media_apply command.
type SiteLookup interface {
	GetMediaSiteURL(ctx context.Context, tenantID, siteID uuid.UUID) (string, bool, error)
}

// AgentApplyClient sends the media_apply command. *agentcmd.Client satisfies it.
type AgentApplyClient interface {
	MediaApply(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.MediaApplyRequest) (agentcmd.MediaApplyResponse, error)
}

// EncodeWorker is the media_encode River worker. It imports the (CGO) encoder,
// so it lives in the encoder-only import set and is registered ONLY in
// cmd/media-encoder. For each attachment job it presigned-GETs every source
// variant, encodes, presigned-PUTs the output, writes a media_variant_results
// row, publishes progress, and — when the attachment's variants are done —
// sends a signed media_apply command so the agent applies on disk.
type EncodeWorker struct {
	river.WorkerDefaults[model.EncodeArgs]
	enc        encoder.Encoder
	repo       EncodeJobRepo
	store      Presigner
	events     EventPublisher
	sites      SiteLookup
	apply      AgentApplyClient
	cpBaseURL  string
	http       *http.Client
	presignTTL time.Duration
	logger     *slog.Logger
}

// NewEncodeWorker builds the worker.
func NewEncodeWorker(
	enc encoder.Encoder,
	r EncodeJobRepo,
	store Presigner,
	events EventPublisher,
	sites SiteLookup,
	apply AgentApplyClient,
	cpBaseURL string,
	presignTTL time.Duration,
	logger *slog.Logger,
) *EncodeWorker {
	if logger == nil {
		logger = slog.Default()
	}
	if presignTTL <= 0 {
		presignTTL = 15 * time.Minute
	}
	return &EncodeWorker{
		enc:        enc,
		repo:       r,
		store:      store,
		events:     events,
		sites:      sites,
		apply:      apply,
		cpBaseURL:  cpBaseURL,
		http:       &http.Client{Timeout: 2 * time.Minute},
		presignTTL: presignTTL,
		logger:     logger,
	}
}

// Timeout is generous (5 min) to cover ≤10 AVIF encodes + S3 round-trips on the
// bounded queue, mirroring the SQL-inspect worker (ADR-043 §5).
func (w *EncodeWorker) Timeout(*river.Job[model.EncodeArgs]) time.Duration {
	return 5 * time.Minute
}

// Work encodes one attachment's variants.
func (w *EncodeWorker) Work(ctx context.Context, job *river.Job[model.EncodeArgs]) error {
	a := job.Args

	// 1. Re-read authoritative job state; return nil early if terminal (dup-safe).
	cur, err := w.repo.GetJobAgent(ctx, a.JobID)
	if err != nil {
		// The media_optimization_jobs row was deleted while the River job was still
		// in the queue (e.g. the user cancelled/deleted the media job from the UI,
		// which does not automatically cancel the corresponding River job). Returning
		// a retryable error here would cause River to retry indefinitely, keeping
		// the queue non-empty (preventing scale-to-zero) and starving live workers.
		// Instead, permanently discard the River job with river.JobCancel so it is
		// removed from the queue on this attempt rather than re-scheduled.
		if de, ok := domain.AsDomain(err); ok && de.Kind == domain.KindNotFound {
			w.logger.WarnContext(ctx, "media encode: job row gone, discarding River job",
				slog.String("job_id", a.JobID))
			return river.JobCancel(err)
		}
		return fmt.Errorf("media encode: get job: %w", err)
	}
	if cur.State.Terminal() {
		return nil
	}

	lossless := a.TargetQuality == model.CompressionLossless
	applyVariants := make([]agentcmd.MediaApplyVariant, 0, len(a.Variants))

	// 2. Encode each variant. A per-variant failure is recorded but does NOT
	//    fail sibling variants (ADR-043 §5).
	for _, v := range a.Variants {
		srcKey := media.SrcKey(a.TenantID, a.SiteID, a.JobID, v.Name)
		outKey := media.OutKey(a.TenantID, a.SiteID, a.JobID, v.Name)

		source, ferr := w.fetchPresigned(ctx, srcKey)
		if ferr != nil {
			w.recordFailure(ctx, a, v, "source fetch failed: "+ferr.Error())
			continue
		}

		start := time.Now()
		res, eerr := w.enc.Encode(ctx, encoder.EncodeRequest{
			Source:       source,
			TargetFormat: a.TargetFormat,
			Lossless:     lossless,
		})
		if eerr != nil {
			w.recordFailure(ctx, a, v, encodeReason(eerr))
			continue
		}
		encodeMS := int(time.Since(start).Milliseconds())

		if perr := w.putPresigned(ctx, outKey, res.Output); perr != nil {
			w.recordFailure(ctx, a, v, "output upload failed: "+perr.Error())
			continue
		}

		optSize := int64(len(res.Output))
		_ = w.repo.UpsertVariantAgent(ctx, a.TenantID, repo.UpsertVariantInput{
			JobID:              a.JobID,
			VariantName:        v.Name,
			SourceSizeBytes:    v.SourceSize,
			OptimizedSizeBytes: &optSize,
			SourceMime:         res.SourceMime,
			OptimizedMime:      res.OutputMime,
			EncodeMS:           &encodeMS,
			State:              model.VariantSucceeded,
		})
		applyVariants = append(applyVariants, agentcmd.MediaApplyVariant{
			Name:          v.Name,
			OptimizedMime: res.OutputMime,
			OptimizedSize: optSize,
		})

		w.publish(ctx, a, site.EventMediaOptimizeProgress, map[string]any{
			"job_id":  a.JobID,
			"variant": v.Name,
			"phase":   "encoded",
		})
	}

	// 3. Finalize: tell the agent to apply the outputs it can download. The CP
	//    holds NO bytes — it mints presigned GET URLs for out/<name>. The agent's
	//    job-status callback finalizes the job + asset rows + deletes the temp
	//    objects (so cleanup is owned by the apply callback, not here).
	if len(applyVariants) == 0 {
		// Every variant failed → no apply phase runs, so the agent's job-status
		// callback (which finalizes the job + sweeps temp objects) never fires.
		// Finalize the job as failed HERE so it doesn't hang in_progress, and
		// best-effort delete the orphaned src/* objects the agent already uploaded.
		if _, ferr := w.repo.FinalizeJobAgent(ctx, a.JobID, repo.FinalizeJobInput{
			State:          model.JobFailed,
			VariantsFailed: len(a.Variants),
			ErrorReason:    "all variants failed to encode",
		}); ferr != nil {
			w.logger.WarnContext(ctx, "media encode: finalize all-failed job", "job_id", a.JobID, "err", ferr.Error())
		}
		for _, v := range a.Variants {
			_ = w.store.Delete(ctx, media.SrcKey(a.TenantID, a.SiteID, a.JobID, v.Name))
		}
		w.publish(ctx, a, site.EventMediaJobFailed, map[string]any{
			"job_id": a.JobID,
			"reason": "all variants failed to encode",
		})
		return nil
	}

	if err := w.dispatchApply(ctx, a, applyVariants); err != nil {
		// Transport error → return for River retry (a fresh JWT is minted on the
		// next attempt; the variant rows + out/* objects are already persisted so
		// the retry is cheap).
		return fmt.Errorf("media encode: dispatch apply: %w", err)
	}
	return nil
}

// dispatchApply mints presigned GET URLs for each output and sends media_apply.
func (w *EncodeWorker) dispatchApply(ctx context.Context, a model.EncodeArgs, variants []agentcmd.MediaApplyVariant) error {
	if w.sites == nil || w.apply == nil {
		return errors.New("media encode: agent apply client not wired")
	}
	siteURL, enrolled, err := w.sites.GetMediaSiteURL(ctx, a.TenantID, a.SiteID)
	if err != nil {
		return err
	}
	if !enrolled {
		return errors.New("media encode: site not enrolled")
	}
	for i := range variants {
		outKey := media.OutKey(a.TenantID, a.SiteID, a.JobID, variants[i].Name)
		url, perr := w.store.PresignGet(ctx, outKey, w.presignTTL)
		if perr != nil {
			return fmt.Errorf("presign out %s: %w", variants[i].Name, perr)
		}
		variants[i].GetURL = url
	}
	ack, err := w.apply.MediaApply(ctx, a.SiteID, siteURL, agentcmd.MediaApplyRequest{
		JobID:          a.JobID,
		WPAttachmentID: a.WPAttachmentID,
		TargetFormat:   a.TargetFormat,
		TargetQuality:  a.TargetQuality,
		// MUST be an ABSOLUTE URL. The agent's MediaUploader posts the status
		// callback via wp_remote_post($status_endpoint), which fails outright on
		// a relative path ("A valid URL was not provided") — silently swallowed
		// by signedPostJson, so the CP never gets job-status and the optimize
		// hangs at "encoded". Mirror the CP service's callbackURL() helper.
		StatusEndpoint: w.cpBaseURL + "/agent/v1/media/job-status",
		Variants:       variants,
	})
	if err != nil {
		return err
	}
	// The agent acks with ok=false when it refuses to apply (on an otherwise
	// clean 200). Returning nil there would complete the River job while the
	// media job hangs at "encoded" forever, since the agent will never post the
	// job-status callback described above. Surface it as an error so the job
	// retries and the reason is recorded.
	if !ack.OK {
		return fmt.Errorf("media_apply rejected by agent: %s", detailOr(ack.Detail, "agent returned ok=false"))
	}
	return nil
}

// detailOr returns primary when it is non-empty, else fallback, so an agent that
// rejects a command without sending a detail still produces a readable error.
func detailOr(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}

// fetchPresigned mints a presigned GET URL for key and fetches the bytes over
// plain HTTP (presigned SigV4 is the only S3 read path that works on GCS).
func (w *EncodeWorker) fetchPresigned(ctx context.Context, key string) ([]byte, error) {
	url, err := w.store.PresignGet(ctx, key, w.presignTTL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch source: status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, encoder.MaxSourceBytes+1))
}

// putPresigned mints a presigned PUT URL for key and uploads the bytes.
func (w *EncodeWorker) putPresigned(ctx context.Context, key string, body []byte) error {
	url, err := w.store.PresignPut(ctx, key, w.presignTTL)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(body))
	resp, err := w.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("upload output: status %d", resp.StatusCode)
	}
	return nil
}

func (w *EncodeWorker) recordFailure(ctx context.Context, a model.EncodeArgs, v model.EncodeVariant, reason string) {
	_ = w.repo.UpsertVariantAgent(ctx, a.TenantID, repo.UpsertVariantInput{
		JobID:           a.JobID,
		VariantName:     v.Name,
		SourceSizeBytes: v.SourceSize,
		SourceMime:      v.SourceMime,
		State:           model.VariantFailed,
		Reason:          reason,
	})
	w.logger.Warn("media variant encode failed",
		slog.String("job_id", a.JobID),
		slog.String("variant", v.Name),
		slog.String("reason", reason))
}

func (w *EncodeWorker) publish(ctx context.Context, a model.EncodeArgs, eventType string, data map[string]any) {
	if w.events == nil {
		return
	}
	_ = w.events.Publish(ctx, site.ConnectionEvent{
		Type:     eventType,
		TenantID: a.TenantID,
		SiteID:   a.SiteID,
		Data:     data,
	})
}

// encodeReason maps an encoder error to a human reason recorded in the variant
// row (and ultimately the asset's sizes_unoptimized map).
func encodeReason(err error) string {
	switch {
	case errors.Is(err, encoder.ErrUnsupportedSource):
		return "Unsupported source format"
	case errors.Is(err, encoder.ErrEncoderTimeout):
		return "Encode timed out"
	case errors.Is(err, encoder.ErrDimensionsTooBig):
		return "Source exceeds size/dimension limits"
	default:
		return "Encode failed: " + err.Error()
	}
}
