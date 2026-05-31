package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/repo"
)

// ---------------------------------------------------------------------------
// fakes (pure Go — no Postgres, no encoder; this whole test file builds under
// CGO_ENABLED=0)
// ---------------------------------------------------------------------------

type fakeRepo struct {
	assets   map[uuid.UUID]model.Asset
	jobs     map[string]model.Job
	variants map[string][]model.VariantResult
	pending  []model.Asset

	insertedJobs   []repo.InsertJobInput
	enqueuedStatus map[string]model.JobState
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		assets:         map[uuid.UUID]model.Asset{},
		jobs:           map[string]model.Job{},
		variants:       map[string][]model.VariantResult{},
		enqueuedStatus: map[string]model.JobState{},
	}
}

func (r *fakeRepo) UpsertAssetsAgent(_ context.Context, _, _ uuid.UUID, rows []repo.UpsertAssetInput) (int64, error) {
	return int64(len(rows)), nil
}
func (r *fakeRepo) ListAssets(_ context.Context, _ repo.ListAssetsInput) ([]model.Asset, string, error) {
	out := make([]model.Asset, 0, len(r.assets))
	for _, a := range r.assets {
		out = append(out, a)
	}
	return out, "", nil
}
func (r *fakeRepo) GetAsset(_ context.Context, _, assetID uuid.UUID) (model.Asset, error) {
	a, ok := r.assets[assetID]
	if !ok {
		return model.Asset{}, domain.NotFound("media_asset_not_found", "not found")
	}
	return a, nil
}
func (r *fakeRepo) ListPendingAssetIDs(_ context.Context, _, _ uuid.UUID, _ int) ([]model.Asset, error) {
	return r.pending, nil
}
func (r *fakeRepo) SetAssetStatus(_ context.Context, _, assetID uuid.UUID, status model.AssetStatus) error {
	a := r.assets[assetID]
	a.Status = status
	r.assets[assetID] = a
	return nil
}
func (r *fakeRepo) ApplyOptimizedAgent(_ context.Context, _, _ uuid.UUID, wpID int64, in repo.ApplyOptimizedInput) (model.Asset, error) {
	for id, a := range r.assets {
		if a.WPAttachmentID == wpID {
			a.Status = in.Status
			a.CurrentFormat = in.CurrentFormat
			a.CurrentSizeBytes = in.CurrentSizeBytes
			a.Generation++
			r.assets[id] = a
			return a, nil
		}
	}
	return model.Asset{}, domain.NotFound("media_asset_not_found", "not found")
}
func (r *fakeRepo) RestoreAssetAgent(_ context.Context, _, _ uuid.UUID, wpID int64) (model.Asset, error) {
	for id, a := range r.assets {
		if a.WPAttachmentID == wpID {
			a.Status = model.AssetRestored
			r.assets[id] = a
			return a, nil
		}
	}
	return model.Asset{}, domain.NotFound("media_asset_not_found", "not found")
}
func (r *fakeRepo) Summary(_ context.Context, _, _ uuid.UUID) (model.AssetSummary, error) {
	return model.AssetSummary{Total: int64(len(r.assets))}, nil
}
func (r *fakeRepo) InsertJob(_ context.Context, tenantID uuid.UUID, in repo.InsertJobInput) (model.Job, error) {
	r.insertedJobs = append(r.insertedJobs, in)
	j := model.Job{
		ID: in.ID, TenantID: tenantID, SiteID: in.SiteID, AssetID: in.AssetID,
		WPAttachmentID: in.WPAttachmentID, Kind: in.Kind, TargetFormat: in.TargetFormat,
		TargetQuality: in.TargetQuality, State: model.JobQueued, CreatedAt: time.Now(),
	}
	r.jobs[in.ID] = j
	return j, nil
}
func (r *fakeRepo) GetJob(_ context.Context, _ uuid.UUID, jobID string) (model.Job, error) {
	j, ok := r.jobs[jobID]
	if !ok {
		return model.Job{}, domain.NotFound("media_job_not_found", "not found")
	}
	return j, nil
}
func (r *fakeRepo) GetJobAgent(_ context.Context, jobID string) (model.Job, error) {
	j, ok := r.jobs[jobID]
	if !ok {
		return model.Job{}, domain.NotFound("media_job_not_found", "not found")
	}
	return j, nil
}
func (r *fakeRepo) ListJobs(_ context.Context, _ repo.ListJobsInput) ([]model.Job, string, error) {
	out := make([]model.Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, j)
	}
	return out, "", nil
}
func (r *fakeRepo) MarkJobInProgressAgent(_ context.Context, jobID string, total int) error {
	j := r.jobs[jobID]
	j.State = model.JobInProgress
	j.VariantsTotal = total
	r.jobs[jobID] = j
	return nil
}
func (r *fakeRepo) FinalizeJobAgent(_ context.Context, jobID string, in repo.FinalizeJobInput) (model.Job, error) {
	j := r.jobs[jobID]
	if !j.State.Terminal() {
		j.State = in.State
		j.VariantsSucceeded = in.VariantsSucceeded
		j.VariantsFailed = in.VariantsFailed
		r.jobs[jobID] = j
	}
	r.enqueuedStatus[jobID] = j.State
	return j, nil
}
func (r *fakeRepo) CancelJobs(_ context.Context, _, _ uuid.UUID) (int64, error) {
	var n int64
	for id, j := range r.jobs {
		if j.State == model.JobQueued || j.State == model.JobInProgress {
			j.State = model.JobCancelled
			r.jobs[id] = j
			n++
		}
	}
	return n, nil
}
func (r *fakeRepo) UpsertVariantAgent(_ context.Context, _ uuid.UUID, in repo.UpsertVariantInput) error {
	r.variants[in.JobID] = append(r.variants[in.JobID], model.VariantResult{
		JobID: in.JobID, VariantName: in.VariantName, State: in.State,
	})
	return nil
}
func (r *fakeRepo) ListVariantsForJob(_ context.Context, _ uuid.UUID, jobID string) ([]model.VariantResult, error) {
	return r.variants[jobID], nil
}
func (r *fakeRepo) CountVariantStatesAgent(_ context.Context, jobID string) (int, int, error) {
	var s, f int
	for _, v := range r.variants[jobID] {
		switch v.State {
		case model.VariantSucceeded:
			s++
		case model.VariantFailed:
			f++
		}
	}
	return s, f, nil
}

type fakeEnqueuer struct{ enqueued []model.EncodeArgs }

func (e *fakeEnqueuer) EnqueueEncode(_ context.Context, args model.EncodeArgs) error {
	e.enqueued = append(e.enqueued, args)
	return nil
}

type fakeAgent struct {
	optimizeCalls int
	restoreCalls  int
	deleteCalls   int
	syncCalls     int
}

func (a *fakeAgent) MediaOptimize(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaOptimizeRequest) (agentcmd.MediaOptimizeResponse, error) {
	a.optimizeCalls++
	return agentcmd.MediaOptimizeResponse{OK: true}, nil
}
func (a *fakeAgent) MediaSync(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaSyncRequest) (agentcmd.MediaSyncResponse, error) {
	a.syncCalls++
	return agentcmd.MediaSyncResponse{OK: true}, nil
}
func (a *fakeAgent) MediaRestore(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaRestoreRequest) (agentcmd.MediaRestoreResponse, error) {
	a.restoreCalls++
	return agentcmd.MediaRestoreResponse{OK: true}, nil
}
func (a *fakeAgent) MediaDeleteOriginals(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaDeleteOriginalsRequest) (agentcmd.MediaDeleteOriginalsResponse, error) {
	a.deleteCalls++
	return agentcmd.MediaDeleteOriginalsResponse{OK: true}, nil
}

type fakeSites struct{ enrolled bool }

func (s fakeSites) GetMediaSiteInfo(_ context.Context, _, _ uuid.UUID) (MediaSiteInfo, error) {
	return MediaSiteInfo{URL: "https://site.example", Enrolled: s.enrolled}, nil
}

type fakeStore struct{ deleted, listed int }

func (f *fakeStore) PresignPut(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://put/" + key, nil
}
func (f *fakeStore) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://get/" + key, nil
}
func (f *fakeStore) Delete(_ context.Context, _ string) error { f.deleted++; return nil }
func (f *fakeStore) List(_ context.Context, _ string) ([]string, error) {
	f.listed++
	return []string{"media/k1", "media/k2"}, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newTestService(r Repo, store Presigner, enq EncodeEnqueuer, cmd AgentMediaClient, sites SiteLookup) *Service {
	s := NewService(r, store, nil, nil, domain.SystemClock{}, Config{CPBaseURL: "https://cp.example"}, nil)
	s.SetEnqueuer(enq)
	s.SetAgentClient(cmd, sites)
	return s
}

func userPrincipal(tenantID uuid.UUID) domain.Principal {
	return domain.Principal{Type: domain.PrincipalUser, UserID: uuid.New(), TenantID: tenantID, Role: "admin"}
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestStartOptimize_FansOutOneJobPerAsset(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	r := newFakeRepo()
	a1 := model.Asset{ID: uuid.New(), SiteID: siteID, WPAttachmentID: 1, Status: model.AssetPending}
	a2 := model.Asset{ID: uuid.New(), SiteID: siteID, WPAttachmentID: 2, Status: model.AssetPending}
	r.assets[a1.ID] = a1
	r.assets[a2.ID] = a2

	agent := &fakeAgent{}
	svc := newTestService(r, &fakeStore{}, &fakeEnqueuer{}, agent, fakeSites{enrolled: true})

	res, err := svc.StartOptimize(context.Background(), tenantID, siteID, []uuid.UUID{a1.ID, a2.ID}, false, "avif", "lossy", userPrincipal(tenantID))
	if err != nil {
		t.Fatalf("StartOptimize: %v", err)
	}
	if res.QueuedCount != 2 {
		t.Errorf("queued_count = %d, want 2", res.QueuedCount)
	}
	if len(r.insertedJobs) != 2 {
		t.Errorf("inserted %d jobs, want 2 (one per asset — ADR-043 §3 fan-out)", len(r.insertedJobs))
	}
	if agent.optimizeCalls != 1 {
		t.Errorf("agent optimize dispatched %d times, want 1", agent.optimizeCalls)
	}
	// Assets flipped to optimizing.
	if r.assets[a1.ID].Status != model.AssetOptimizing {
		t.Errorf("asset 1 status = %q, want optimizing", r.assets[a1.ID].Status)
	}
}

func TestStartOptimize_RejectsBadFormat(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	svc := newTestService(newFakeRepo(), &fakeStore{}, &fakeEnqueuer{}, &fakeAgent{}, fakeSites{enrolled: true})
	_, err := svc.StartOptimize(context.Background(), tenantID, siteID, nil, true, "gif", "", userPrincipal(tenantID))
	if err == nil {
		t.Fatal("expected validation error for target_format=gif")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindValidation {
		t.Errorf("err = %v, want validation", err)
	}
}

func TestHandleEncodeReady_EnqueuesAndMarksInProgress(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	r := newFakeRepo()
	jobID := "JOB1"
	r.jobs[jobID] = model.Job{ID: jobID, TenantID: tenantID, SiteID: siteID, Kind: model.JobOptimize, TargetFormat: "webp", TargetQuality: "lossy", State: model.JobQueued}
	enq := &fakeEnqueuer{}
	svc := newTestService(r, &fakeStore{}, enq, &fakeAgent{}, fakeSites{enrolled: true})

	err := svc.HandleEncodeReady(context.Background(), tenantID, siteID, jobID, []EncodeReadyVariant{
		{Name: "full", SourceSize: 1000, SourceMime: "image/jpeg"},
		{Name: "thumbnail", SourceSize: 200, SourceMime: "image/jpeg"},
	})
	if err != nil {
		t.Fatalf("HandleEncodeReady: %v", err)
	}
	if len(enq.enqueued) != 1 {
		t.Fatalf("enqueued %d encode jobs, want 1 (one EncodeArgs carrying both variants)", len(enq.enqueued))
	}
	if got := len(enq.enqueued[0].Variants); got != 2 {
		t.Errorf("EncodeArgs carried %d variants, want 2", got)
	}
	if r.jobs[jobID].State != model.JobInProgress {
		t.Errorf("job state = %q, want in_progress", r.jobs[jobID].State)
	}
}

func TestHandleEncodeReady_SiteMismatchRejected(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	otherSite := uuid.New()
	r := newFakeRepo()
	r.jobs["JOB1"] = model.Job{ID: "JOB1", TenantID: tenantID, SiteID: otherSite, State: model.JobQueued}
	svc := newTestService(r, &fakeStore{}, &fakeEnqueuer{}, &fakeAgent{}, fakeSites{enrolled: true})

	err := svc.HandleEncodeReady(context.Background(), tenantID, siteID, "JOB1", []EncodeReadyVariant{{Name: "full"}})
	if err == nil {
		t.Fatal("expected forbidden for a job belonging to another site")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindForbidden {
		t.Errorf("err = %v, want forbidden (job/site mismatch)", err)
	}
}

func TestHandleApplyStatus_FinalizesAndCleansUp(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	r := newFakeRepo()
	asset := model.Asset{ID: uuid.New(), SiteID: siteID, WPAttachmentID: 7, Status: model.AssetOptimizing}
	r.assets[asset.ID] = asset
	aid := asset.ID
	jobID := "JOB7"
	r.jobs[jobID] = model.Job{ID: jobID, TenantID: tenantID, SiteID: siteID, AssetID: &aid, WPAttachmentID: 7, Kind: model.JobOptimize, State: model.JobInProgress, TargetFormat: "avif"}
	r.variants[jobID] = []model.VariantResult{{JobID: jobID, State: model.VariantSucceeded}}
	store := &fakeStore{}
	svc := newTestService(r, store, &fakeEnqueuer{}, &fakeAgent{}, fakeSites{enrolled: true})

	err := svc.HandleApplyStatus(context.Background(), tenantID, siteID, jobID, ApplyStatusInput{
		AppliedVariants:  []string{"full"},
		CurrentFormat:    "avif",
		CurrentSizeBytes: 500,
	})
	if err != nil {
		t.Fatalf("HandleApplyStatus: %v", err)
	}
	if r.jobs[jobID].State != model.JobSucceeded {
		t.Errorf("job state = %q, want succeeded", r.jobs[jobID].State)
	}
	if r.assets[asset.ID].Status != model.AssetOptimized {
		t.Errorf("asset status = %q, want optimized", r.assets[asset.ID].Status)
	}
	if store.deleted == 0 {
		t.Error("expected temp objects to be deleted after apply (ADR-043 §2)")
	}
}

func TestHandleApplyStatus_DeleteOriginals(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	r := newFakeRepo()
	asset := model.Asset{ID: uuid.New(), SiteID: siteID, WPAttachmentID: 9, Status: model.AssetOptimized}
	r.assets[asset.ID] = asset
	aid := asset.ID
	jobID := "JOB9"
	r.jobs[jobID] = model.Job{ID: jobID, TenantID: tenantID, SiteID: siteID, AssetID: &aid, WPAttachmentID: 9, Kind: model.JobDeleteOriginals, State: model.JobInProgress}
	svc := newTestService(r, &fakeStore{}, &fakeEnqueuer{}, &fakeAgent{}, fakeSites{enrolled: true})

	if err := svc.HandleApplyStatus(context.Background(), tenantID, siteID, jobID, ApplyStatusInput{OriginalsDeleted: true}); err != nil {
		t.Fatalf("HandleApplyStatus(delete): %v", err)
	}
	if r.assets[asset.ID].Status != model.AssetOriginalsDeleted {
		t.Errorf("asset status = %q, want originals_deleted", r.assets[asset.ID].Status)
	}
	if r.jobs[jobID].State != model.JobSucceeded {
		t.Errorf("job state = %q, want succeeded", r.jobs[jobID].State)
	}
}

func TestStartDeleteOriginals_RequiresOptimized(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	r := newFakeRepo()
	a := model.Asset{ID: uuid.New(), SiteID: siteID, WPAttachmentID: 3, Status: model.AssetPending}
	r.assets[a.ID] = a
	svc := newTestService(r, &fakeStore{}, &fakeEnqueuer{}, &fakeAgent{}, fakeSites{enrolled: true})

	_, err := svc.StartDeleteOriginals(context.Background(), tenantID, siteID, []uuid.UUID{a.ID}, userPrincipal(tenantID))
	if err == nil {
		t.Fatal("expected conflict: delete-originals requires an optimized asset")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindConflict {
		t.Errorf("err = %v, want conflict", err)
	}
}

func TestStartRestore_RefusesOriginalsDeleted(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	r := newFakeRepo()
	a := model.Asset{ID: uuid.New(), SiteID: siteID, WPAttachmentID: 4, Status: model.AssetOriginalsDeleted}
	r.assets[a.ID] = a
	svc := newTestService(r, &fakeStore{}, &fakeEnqueuer{}, &fakeAgent{}, fakeSites{enrolled: true})

	_, err := svc.StartRestore(context.Background(), tenantID, siteID, []uuid.UUID{a.ID}, userPrincipal(tenantID))
	if err == nil {
		t.Fatal("expected conflict: cannot restore when originals are deleted")
	}
}

func TestCancel_CancelsNonTerminalJobs(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	r := newFakeRepo()
	r.jobs["A"] = model.Job{ID: "A", SiteID: siteID, State: model.JobQueued}
	r.jobs["B"] = model.Job{ID: "B", SiteID: siteID, State: model.JobInProgress}
	r.jobs["C"] = model.Job{ID: "C", SiteID: siteID, State: model.JobSucceeded}
	svc := newTestService(r, &fakeStore{}, &fakeEnqueuer{}, &fakeAgent{}, fakeSites{enrolled: true})

	res, err := svc.Cancel(context.Background(), tenantID, siteID, userPrincipal(tenantID))
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if res.CancelledCount != 2 {
		t.Errorf("cancelled %d, want 2 (queued + in_progress; succeeded untouched)", res.CancelledCount)
	}
}

func TestRateLimit_BlocksRunawayOptimize(t *testing.T) {
	tenantID, siteID := uuid.New(), uuid.New()
	r := newFakeRepo()
	pending := make([]model.Asset, 0, 10)
	for i := 0; i < 10; i++ {
		a := model.Asset{ID: uuid.New(), SiteID: siteID, WPAttachmentID: int64(i + 1), Status: model.AssetPending}
		r.assets[a.ID] = a
		pending = append(pending, a)
	}
	r.pending = pending

	s := NewService(r, &fakeStore{}, nil, nil, domain.SystemClock{}, Config{
		CPBaseURL: "https://cp.example", RatePerSite: 5, RatePerTenant: 5, RateWindow: time.Minute,
	}, nil)
	s.SetEnqueuer(&fakeEnqueuer{})
	s.SetAgentClient(&fakeAgent{}, fakeSites{enrolled: true})

	_, err := s.StartOptimize(context.Background(), tenantID, siteID, nil, true, "avif", "lossy", userPrincipal(tenantID))
	if err == nil {
		t.Fatal("expected rate-limit error for 10 assets against a per-site cap of 5")
	}
	de, ok := domain.AsDomain(err)
	if !ok || de.Kind != domain.KindRateLimited {
		t.Errorf("err = %v, want rate-limited", err)
	}
}
