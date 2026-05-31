package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/repo"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// Repo is the persistence surface the service needs. *repo.Repo satisfies it;
// declared as an interface so service-layer unit tests can fake it under
// CGO_ENABLED=0 (no Postgres / no encoder).
type Repo interface {
	// assets
	UpsertAssetsAgent(ctx context.Context, tenantID, siteID uuid.UUID, rows []repo.UpsertAssetInput) (int64, error)
	ListAssets(ctx context.Context, in repo.ListAssetsInput) ([]model.Asset, string, error)
	GetAsset(ctx context.Context, tenantID, assetID uuid.UUID) (model.Asset, error)
	ListPendingAssetIDs(ctx context.Context, tenantID, siteID uuid.UUID, limit int) ([]model.Asset, error)
	SetAssetStatus(ctx context.Context, tenantID, assetID uuid.UUID, status model.AssetStatus) error
	ApplyOptimizedAgent(ctx context.Context, tenantID, siteID uuid.UUID, wpAttachmentID int64, in repo.ApplyOptimizedInput) (model.Asset, error)
	RestoreAssetAgent(ctx context.Context, tenantID, siteID uuid.UUID, wpAttachmentID int64) (model.Asset, error)
	Summary(ctx context.Context, tenantID, siteID uuid.UUID) (model.AssetSummary, error)
	// jobs
	InsertJob(ctx context.Context, tenantID uuid.UUID, in repo.InsertJobInput) (model.Job, error)
	GetJob(ctx context.Context, tenantID uuid.UUID, jobID string) (model.Job, error)
	GetJobAgent(ctx context.Context, jobID string) (model.Job, error)
	ListJobs(ctx context.Context, in repo.ListJobsInput) ([]model.Job, string, error)
	MarkJobInProgressAgent(ctx context.Context, jobID string, variantsTotal int) error
	FinalizeJobAgent(ctx context.Context, jobID string, in repo.FinalizeJobInput) (model.Job, error)
	CancelJobs(ctx context.Context, tenantID, siteID uuid.UUID) (int64, error)
	// variants
	UpsertVariantAgent(ctx context.Context, tenantID uuid.UUID, in repo.UpsertVariantInput) error
	ListVariantsForJob(ctx context.Context, tenantID uuid.UUID, jobID string) ([]model.VariantResult, error)
	CountVariantStatesAgent(ctx context.Context, jobID string) (succeeded, failed int, err error)
}

// EncodeEnqueuer inserts media_encode River jobs. *RiverEnqueuer (worker pkg)
// satisfies it; the main API wires it post-River-start. Insert-only — the API's
// River client registers media_encode with MaxWorkers=0.
type EncodeEnqueuer interface {
	EnqueueEncode(ctx context.Context, args model.EncodeArgs) error
}

// AgentMediaClient is the subset of agentcmd.Client the service dispatches.
// *agentcmd.Client satisfies it; declared as an interface so the service stays
// free of the SSRF transport in tests, and so a nil/disabled commander degrades
// gracefully.
type AgentMediaClient interface {
	MediaOptimize(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.MediaOptimizeRequest) (agentcmd.MediaOptimizeResponse, error)
	MediaSync(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.MediaSyncRequest) (agentcmd.MediaSyncResponse, error)
	MediaRestore(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.MediaRestoreRequest) (agentcmd.MediaRestoreResponse, error)
	MediaDeleteOriginals(ctx context.Context, siteID uuid.UUID, siteURL string, req agentcmd.MediaDeleteOriginalsRequest) (agentcmd.MediaDeleteOriginalsResponse, error)
}

// SiteLookup resolves the slim site projection the media service needs (agent
// URL + enrollment). Wired in main via a narrow adapter to keep this package
// free of the site service import.
type SiteLookup interface {
	GetMediaSiteInfo(ctx context.Context, tenantID, siteID uuid.UUID) (MediaSiteInfo, error)
}

// MediaSiteInfo is the slim site projection for media dispatch.
type MediaSiteInfo struct {
	URL      string
	Enrolled bool
}

// Presigner mints presigned PUT/GET URLs for media temp objects. *blobstore.Store
// satisfies it. The CP NEVER reads image bytes itself — presigned URLs only.
type Presigner interface {
	PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error)
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// EventPublisher publishes media.* SSE envelopes on the shared tenant bus.
// *events.Publisher (which satisfies site.EventPublisher) is injected.
type EventPublisher interface {
	Publish(ctx context.Context, ev site.ConnectionEvent) error
}
