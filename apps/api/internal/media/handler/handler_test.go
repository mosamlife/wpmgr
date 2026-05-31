package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/repo"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/service"
)

// minimal repo fake reused for handler wiring (only the methods the exercised
// routes hit are non-trivial).
type stubRepo struct {
	asset model.Asset
}

func (r *stubRepo) UpsertAssetsAgent(context.Context, uuid.UUID, uuid.UUID, []repo.UpsertAssetInput) (int64, error) {
	return 0, nil
}
func (r *stubRepo) ListAssets(context.Context, repo.ListAssetsInput) ([]model.Asset, string, error) {
	return []model.Asset{r.asset}, "", nil
}
func (r *stubRepo) GetAsset(_ context.Context, _, id uuid.UUID) (model.Asset, error) {
	if id == r.asset.ID {
		return r.asset, nil
	}
	return model.Asset{}, domain.NotFound("media_asset_not_found", "not found")
}
func (r *stubRepo) ListPendingAssetIDs(context.Context, uuid.UUID, uuid.UUID, int) ([]model.Asset, error) {
	return []model.Asset{r.asset}, nil
}
func (r *stubRepo) SetAssetStatus(context.Context, uuid.UUID, uuid.UUID, model.AssetStatus) error {
	return nil
}
func (r *stubRepo) ApplyOptimizedAgent(context.Context, uuid.UUID, uuid.UUID, int64, repo.ApplyOptimizedInput) (model.Asset, error) {
	return model.Asset{}, nil
}
func (r *stubRepo) RestoreAssetAgent(context.Context, uuid.UUID, uuid.UUID, int64) (model.Asset, error) {
	return model.Asset{}, nil
}
func (r *stubRepo) Summary(context.Context, uuid.UUID, uuid.UUID) (model.AssetSummary, error) {
	return model.AssetSummary{Total: 1, Optimized: 1, BytesSaved: 42}, nil
}
func (r *stubRepo) InsertJob(_ context.Context, tenantID uuid.UUID, in repo.InsertJobInput) (model.Job, error) {
	return model.Job{ID: in.ID, TenantID: tenantID, SiteID: in.SiteID, Kind: in.Kind, State: model.JobQueued, CreatedAt: time.Now()}, nil
}
func (r *stubRepo) GetJob(context.Context, uuid.UUID, string) (model.Job, error) {
	return model.Job{}, domain.NotFound("media_job_not_found", "not found")
}
func (r *stubRepo) GetJobAgent(context.Context, string) (model.Job, error) {
	return model.Job{}, nil
}
func (r *stubRepo) ListJobs(context.Context, repo.ListJobsInput) ([]model.Job, string, error) {
	return nil, "", nil
}
func (r *stubRepo) MarkJobInProgressAgent(context.Context, string, int) error       { return nil }
func (r *stubRepo) CancelJobs(context.Context, uuid.UUID, uuid.UUID) (int64, error) { return 0, nil }
func (r *stubRepo) FinalizeJobAgent(context.Context, string, repo.FinalizeJobInput) (model.Job, error) {
	return model.Job{}, nil
}
func (r *stubRepo) UpsertVariantAgent(context.Context, uuid.UUID, repo.UpsertVariantInput) error {
	return nil
}
func (r *stubRepo) ListVariantsForJob(context.Context, uuid.UUID, string) ([]model.VariantResult, error) {
	return nil, nil
}
func (r *stubRepo) CountVariantStatesAgent(context.Context, string) (int, int, error) {
	return 0, 0, nil
}

type stubEnq struct{}

func (stubEnq) EnqueueEncode(context.Context, model.EncodeArgs) error { return nil }

type stubAgent struct{}

func (stubAgent) MediaOptimize(context.Context, uuid.UUID, string, agentcmd.MediaOptimizeRequest) (agentcmd.MediaOptimizeResponse, error) {
	return agentcmd.MediaOptimizeResponse{OK: true}, nil
}
func (stubAgent) MediaSync(context.Context, uuid.UUID, string, agentcmd.MediaSyncRequest) (agentcmd.MediaSyncResponse, error) {
	return agentcmd.MediaSyncResponse{OK: true}, nil
}
func (stubAgent) MediaRestore(context.Context, uuid.UUID, string, agentcmd.MediaRestoreRequest) (agentcmd.MediaRestoreResponse, error) {
	return agentcmd.MediaRestoreResponse{OK: true}, nil
}
func (stubAgent) MediaDeleteOriginals(context.Context, uuid.UUID, string, agentcmd.MediaDeleteOriginalsRequest) (agentcmd.MediaDeleteOriginalsResponse, error) {
	return agentcmd.MediaDeleteOriginalsResponse{OK: true}, nil
}

type stubSites struct{}

func (stubSites) GetMediaSiteInfo(context.Context, uuid.UUID, uuid.UUID) (service.MediaSiteInfo, error) {
	return service.MediaSiteInfo{URL: "https://s.example", Enrolled: true}, nil
}

type stubStore struct{}

func (stubStore) PresignPut(_ context.Context, k string, _ time.Duration) (string, error) {
	return "u" + k, nil
}
func (stubStore) PresignGet(_ context.Context, k string, _ time.Duration) (string, error) {
	return "g" + k, nil
}
func (stubStore) Delete(context.Context, string) error           { return nil }
func (stubStore) List(context.Context, string) ([]string, error) { return nil, nil }

func buildEngine(t *testing.T, role string, tenantID, siteID, assetID uuid.UUID) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := &stubRepo{asset: model.Asset{ID: assetID, SiteID: siteID, WPAttachmentID: 1, Status: model.AssetPending}}
	svc := service.NewService(r, stubStore{}, nil, nil, domain.SystemClock{}, service.Config{CPBaseURL: "https://cp"}, nil)
	svc.SetEnqueuer(stubEnq{})
	svc.SetAgentClient(stubAgent{}, stubSites{})
	h := NewHandler(svc)

	eng := gin.New()
	// Inject a principal so authz middleware sees an authenticated, tenant-scoped
	// caller with the given role.
	eng.Use(func(c *gin.Context) {
		p := domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantID, Role: role}
		c.Request = c.Request.WithContext(domain.WithPrincipal(c.Request.Context(), p))
		c.Next()
	})
	grp := eng.Group("/api/v1")
	h.Register(grp)
	return eng
}

func TestOptimizeRoute_OperatorAllowed(t *testing.T) {
	tenantID, siteID, assetID := uuid.New(), uuid.New(), uuid.New()
	eng := buildEngine(t, "operator", tenantID, siteID, assetID)

	body, _ := json.Marshal(optimizeBody{AssetIDs: []string{assetID.String()}, TargetFormat: "avif", TargetQuality: "lossy"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sites/"+siteID.String()+"/media/optimize", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	eng.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("optimize as operator: status %d, want 202 (body=%s)", w.Code, w.Body.String())
	}
	var resp struct {
		BatchJobID  string `json:"batch_job_id"`
		QueuedCount int    `json:"queued_count"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.QueuedCount != 1 {
		t.Errorf("queued_count = %d, want 1", resp.QueuedCount)
	}
}

func TestDeleteOriginalsRoute_OperatorForbidden(t *testing.T) {
	tenantID, siteID, assetID := uuid.New(), uuid.New(), uuid.New()
	eng := buildEngine(t, "operator", tenantID, siteID, assetID)

	body, _ := json.Marshal(assetSelectionBody{AssetIDs: []string{assetID.String()}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sites/"+siteID.String()+"/media/delete-originals", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	eng.ServeHTTP(w, req)

	// Operator lacks PermMediaDeleteOriginals (admin+).
	if w.Code != http.StatusForbidden {
		t.Fatalf("delete-originals as operator: status %d, want 403 (body=%s)", w.Code, w.Body.String())
	}
}

func TestListAssetsRoute_ViewerAllowed(t *testing.T) {
	tenantID, siteID, assetID := uuid.New(), uuid.New(), uuid.New()
	eng := buildEngine(t, "viewer", tenantID, siteID, assetID)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sites/"+siteID.String()+"/media/assets", nil)
	w := httptest.NewRecorder()
	eng.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list assets as viewer: status %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var resp listAssetsDTO
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Summary.BytesSaved != 42 {
		t.Errorf("summary.bytes_saved = %d, want 42", resp.Summary.BytesSaved)
	}
}
