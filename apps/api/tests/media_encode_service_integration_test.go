// media_encode_service_integration_test.go — a CGO-FREE service-level
// integration test for the Media Optimizer orchestration (ADR-043). It exercises
// the real media service + real repos against real Postgres (testcontainers),
// driving the full optimize callback chain:
//
//	StartOptimize  → fan-out one media_optimization_jobs row per asset + dispatch
//	HandlePresign  → mint a presigned PUT for each variant's src object
//	HandleEncodeReady → mark the job in_progress + enqueue ONE EncodeArgs
//	(encode worker stand-in: write a succeeded media_variant_results row + an
//	 out/* object — the FAKE encoder; no lilliput, no CGO)
//	HandleApplyStatus → finalize the asset mirror + the job + sweep temp objects
//
// It uses the REAL repo.Repo (so every RLS/agent-GUC write is genuine) and stubs
// only the outbound seams the service would otherwise touch: the agent command
// client, the site lookup, the encode enqueuer, and the object store (a tracking
// fake that records puts/deletes — no network). This is the companion to the
// CGO encode-path test (media_encode_integration_test.go): together they cover
// the orchestration state machine (here) and the real encode round-trip (there).
//
// This file builds under CGO_ENABLED=0 (no encoder import).
package tests

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/media"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/repo"
	mediaservice "github.com/mosamlife/wpmgr/apps/api/internal/media/service"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// ---------------------------------------------------------------------------
// shared media-test helpers (built in BOTH the CGO and CGO-free sets — this file
// has no build tag, unlike the encoder-using media_encode_integration_test.go)
// ---------------------------------------------------------------------------

// seedEnrolledSite inserts a site row (tenant-scoped, under the tenant GUC) that
// is marked enrolled (enrolled_at set), so any enrollment gate is satisfied.
func seedEnrolledSite(t *testing.T, pool *db.Pool, tenantID uuid.UUID, url string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.InTenantTx(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`INSERT INTO sites (tenant_id, url, name, status, enrolled_at)
			 VALUES ($1, $2, $3, 'connected', now()) RETURNING id`,
			tenantID, url, "media-site").Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed enrolled site: %v", err)
	}
	return id
}

// seedMediaUser inserts a users row directly (users is not RLS-scoped). The
// initiator_user_id on a media job FKs to users, so the operator path needs a
// real user id.
func seedMediaUser(t *testing.T, pool *db.Pool, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		"INSERT INTO users (email) VALUES ($1) RETURNING id", email).Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// eventTypes flattens published events to their type strings for diagnostics.
func eventTypes(evs []site.ConnectionEvent) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

// ---------------------------------------------------------------------------
// stubs (outbound seams only; the repo + Postgres are real)
// ---------------------------------------------------------------------------

// trackingStore is an in-memory object-store fake that records every key put +
// every key deleted. It satisfies the service's Presigner. No network: the
// presigned URLs it returns are never dereferenced in this test (the encode is
// faked), they only need to be non-empty.
type trackingStore struct {
	mu      sync.Mutex
	objects map[string]bool
	deleted []string
}

func newTrackingStore() *trackingStore { return &trackingStore{objects: map[string]bool{}} }

func (s *trackingStore) PresignPut(_ context.Context, key string, _ time.Duration) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Model the agent's PUT landing the object.
	s.objects[key] = true
	return "https://put.example/" + key, nil
}
func (s *trackingStore) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://get.example/" + key, nil
}
func (s *trackingStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	s.deleted = append(s.deleted, key)
	return nil
}
func (s *trackingStore) List(_ context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objects {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, k)
		}
	}
	return out, nil
}
func (s *trackingStore) put(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = true
}
func (s *trackingStore) wasDeleted(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.deleted {
		if d == key {
			return true
		}
	}
	return false
}

// captureEnqueuer records each EncodeArgs the service enqueues (the encode would
// normally run in the media-encoder process; here the test plays its role).
type captureEnqueuer struct{ enqueued []model.EncodeArgs }

func (e *captureEnqueuer) EnqueueEncode(_ context.Context, args model.EncodeArgs) error {
	e.enqueued = append(e.enqueued, args)
	return nil
}

// okAgent answers every media command OK (the dispatch transport is out of scope
// for the orchestration test; the CGO test covers the real apply payload).
type okAgent struct{ optimizeCalls int }

func (a *okAgent) MediaOptimize(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaOptimizeRequest) (agentcmd.MediaOptimizeResponse, error) {
	a.optimizeCalls++
	return agentcmd.MediaOptimizeResponse{OK: true}, nil
}
func (a *okAgent) MediaSync(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaSyncRequest) (agentcmd.MediaSyncResponse, error) {
	return agentcmd.MediaSyncResponse{OK: true}, nil
}
func (a *okAgent) MediaRestore(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaRestoreRequest) (agentcmd.MediaRestoreResponse, error) {
	return agentcmd.MediaRestoreResponse{OK: true}, nil
}
func (a *okAgent) MediaDeleteOriginals(_ context.Context, _ uuid.UUID, _ string, _ agentcmd.MediaDeleteOriginalsRequest) (agentcmd.MediaDeleteOriginalsResponse, error) {
	return agentcmd.MediaDeleteOriginalsResponse{OK: true}, nil
}

type enrolledSites struct{}

func (enrolledSites) GetMediaSiteInfo(_ context.Context, _, _ uuid.UUID) (mediaservice.MediaSiteInfo, error) {
	return mediaservice.MediaSiteInfo{URL: "https://media.example.com", Enrolled: true}, nil
}

// memPublisher collects SSE envelopes.
type memPublisher struct{ events []site.ConnectionEvent }

func (p *memPublisher) Publish(_ context.Context, ev site.ConnectionEvent) error {
	p.events = append(p.events, ev)
	return nil
}

// ---------------------------------------------------------------------------
// the test
// ---------------------------------------------------------------------------

// TestMediaOptimizeService_CallbackChain drives one attachment from StartOptimize
// through the agent callbacks to a terminal succeeded state, asserting against
// REAL rows in Postgres that:
//   - StartOptimize created a queued optimize job + flipped the asset to optimizing
//   - HandlePresign minted a PUT per variant (the src object "lands" in the store)
//   - HandleEncodeReady marked the job in_progress + enqueued one EncodeArgs
//   - HandleApplyStatus finalized the asset (optimized) + the job (succeeded) and
//     swept BOTH the src and out temp objects out of the store
func TestMediaOptimizeService_CallbackChain(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	mediaRepo := repo.NewRepo(pool)
	store := newTrackingStore()
	enq := &captureEnqueuer{}
	agent := &okAgent{}
	pub := &memPublisher{}

	svc := mediaservice.NewService(
		mediaRepo, store, pub, nil /*audit*/, domain.SystemClock{},
		mediaservice.Config{CPBaseURL: "https://cp.example", PresignTTL: 10 * time.Minute},
		nil,
	)
	svc.SetEnqueuer(enq)
	svc.SetAgentClient(agent, enrolledSites{})

	// --- seed: tenant + site + ONE pending asset (via the real agent upsert) -
	tenantID := seedTenant(t, pool, "media-svc-tenant")
	siteID := seedEnrolledSite(t, pool, tenantID, "https://media-svc.example.com")
	const wpAttachmentID = int64(7777)
	const origSize = int64(500_000)
	if _, err := mediaRepo.UpsertAssetsAgent(ctx, tenantID, siteID, []repo.UpsertAssetInput{{
		WPAttachmentID:    wpAttachmentID,
		Title:             "hero.jpg",
		OriginalPath:      "/uploads/hero.jpg",
		OriginalURL:       "https://media-svc.example.com/uploads/hero.jpg",
		OriginalMime:      "image/jpeg",
		OriginalSizeBytes: origSize,
	}}); err != nil {
		t.Fatalf("seed asset: %v", err)
	}

	// Resolve the asset id we just inserted.
	assets, _, err := mediaRepo.ListAssets(ctx, repo.ListAssetsInput{TenantID: tenantID, SiteID: siteID, Limit: 10})
	if err != nil || len(assets) != 1 {
		t.Fatalf("list seeded assets: err=%v n=%d", err, len(assets))
	}
	asset := assets[0]
	if asset.Status != model.AssetPending {
		t.Fatalf("seeded asset status = %q, want pending", asset.Status)
	}

	userID := seedMediaUser(t, pool, "media-operator@example.com")
	p := domain.Principal{Type: domain.PrincipalUser, UserID: userID, TenantID: tenantID, Role: "admin"}

	// === Step 1: StartOptimize ==============================================
	res, err := svc.StartOptimize(ctx, tenantID, siteID, []uuid.UUID{asset.ID}, false, media.TargetAVIF, media.QualityLossy, p)
	if err != nil {
		t.Fatalf("StartOptimize: %v", err)
	}
	if res.QueuedCount != 1 {
		t.Fatalf("queued_count = %d, want 1", res.QueuedCount)
	}
	if agent.optimizeCalls != 1 {
		t.Fatalf("media_optimize dispatched %d times, want 1", agent.optimizeCalls)
	}

	// Real row: exactly one queued optimize job, and the asset flipped to optimizing.
	jobs, _, err := mediaRepo.ListJobs(ctx, repo.ListJobsInput{TenantID: tenantID, SiteID: siteID, Limit: 10})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("list jobs: err=%v n=%d", err, len(jobs))
	}
	job := jobs[0]
	if job.Kind != model.JobOptimize || job.State != model.JobQueued {
		t.Fatalf("job kind/state = %s/%s, want optimize/queued", job.Kind, job.State)
	}
	if got, _ := mediaRepo.GetAsset(ctx, tenantID, asset.ID); got.Status != model.AssetOptimizing {
		t.Fatalf("asset status after StartOptimize = %q, want optimizing", got.Status)
	}

	// === Step 2: HandlePresign (agent asks for upload URLs) =================
	const variantName = "full"
	const srcVariantSize = int64(480_000)
	urls, err := svc.HandlePresign(ctx, tenantID, siteID, job.ID, []mediaservice.PresignVariant{
		{Name: variantName, SourceSize: srcVariantSize, SourceMime: "image/jpeg"},
	})
	if err != nil {
		t.Fatalf("HandlePresign: %v", err)
	}
	if urls[variantName] == "" {
		t.Fatalf("HandlePresign returned no URL for %q", variantName)
	}
	srcKey := media.SrcKey(tenantID, siteID, job.ID, variantName)
	// The PresignPut fake "landed" the src object; confirm it exists.
	if list, _ := store.List(ctx, media.JobPrefix(tenantID, siteID, job.ID)+"/"); len(list) == 0 {
		t.Fatalf("expected src object under job prefix after presign; store empty")
	}

	// === Step 3: HandleEncodeReady (sources uploaded → enqueue encode) ======
	if err := svc.HandleEncodeReady(ctx, tenantID, siteID, job.ID, []mediaservice.EncodeReadyVariant{
		{Name: variantName, SourceSize: srcVariantSize, SourceMime: "image/jpeg"},
	}); err != nil {
		t.Fatalf("HandleEncodeReady: %v", err)
	}
	if len(enq.enqueued) != 1 {
		t.Fatalf("enqueued %d EncodeArgs, want 1", len(enq.enqueued))
	}
	if got := len(enq.enqueued[0].Variants); got != 1 {
		t.Fatalf("EncodeArgs carried %d variants, want 1", got)
	}
	// Job is now in_progress in Postgres.
	if got, _ := mediaRepo.GetJobAgent(ctx, job.ID); got.State != model.JobInProgress {
		t.Fatalf("job state after encode-ready = %q, want in_progress", got.State)
	}

	// === Step 4: FAKE encoder stand-in ======================================
	// Play the role of the media-encoder worker WITHOUT lilliput: write a real
	// succeeded variant row (agent GUC) and "PUT" the out object into the store.
	optSize := int64(120_000)
	encMS := 42
	outKey := media.OutKey(tenantID, siteID, job.ID, variantName)
	store.put(outKey)
	if err := mediaRepo.UpsertVariantAgent(ctx, tenantID, repo.UpsertVariantInput{
		JobID:              job.ID,
		VariantName:        variantName,
		SourceSizeBytes:    srcVariantSize,
		OptimizedSizeBytes: &optSize,
		SourceMime:         "image/jpeg",
		OptimizedMime:      "image/avif",
		EncodeMS:           &encMS,
		State:              model.VariantSucceeded,
	}); err != nil {
		t.Fatalf("write succeeded variant: %v", err)
	}

	// === Step 5: HandleApplyStatus (agent applied on disk → finalize) =======
	bytesBefore := origSize
	bytesAfter := optSize
	if err := svc.HandleApplyStatus(ctx, tenantID, siteID, job.ID, mediaservice.ApplyStatusInput{
		AppliedVariants:  []string{variantName},
		CurrentFormat:    model.FormatAVIF,
		CurrentSizeBytes: optSize,
		BytesBefore:      &bytesBefore,
		BytesAfter:       &bytesAfter,
		CompressionLevel: media.QualityLossy,
		TargetFormat:     media.TargetAVIF,
	}); err != nil {
		t.Fatalf("HandleApplyStatus: %v", err)
	}

	// === ASSERT: asset row finalized to optimized ===========================
	finalAsset, err := mediaRepo.GetAsset(ctx, tenantID, asset.ID)
	if err != nil {
		t.Fatalf("get final asset: %v", err)
	}
	if finalAsset.Status != model.AssetOptimized {
		t.Fatalf("final asset status = %q, want optimized", finalAsset.Status)
	}
	if finalAsset.CurrentFormat != model.FormatAVIF {
		t.Errorf("final current_format = %q, want avif", finalAsset.CurrentFormat)
	}
	if finalAsset.CurrentSizeBytes != optSize {
		t.Errorf("final current_size_bytes = %d, want %d", finalAsset.CurrentSizeBytes, optSize)
	}
	if finalAsset.Generation != asset.Generation+1 {
		t.Errorf("generation = %d, want %d (apply bumps generation)", finalAsset.Generation, asset.Generation+1)
	}
	if len(finalAsset.SizesOptimized) != 1 || finalAsset.SizesOptimized[0] != variantName {
		t.Errorf("sizes_optimized = %v, want [%q]", finalAsset.SizesOptimized, variantName)
	}

	// === ASSERT: job row finalized to succeeded =============================
	finalJob, err := mediaRepo.GetJob(ctx, tenantID, job.ID)
	if err != nil {
		t.Fatalf("get final job: %v", err)
	}
	if finalJob.State != model.JobSucceeded {
		t.Fatalf("final job state = %q, want succeeded", finalJob.State)
	}
	if finalJob.VariantsSucceeded != 1 || finalJob.VariantsFailed != 0 {
		t.Errorf("job counts = %d ok / %d failed, want 1/0", finalJob.VariantsSucceeded, finalJob.VariantsFailed)
	}
	if finalJob.CompletedAt == nil {
		t.Error("final job completed_at is nil")
	}

	// === ASSERT: temp objects swept (src + out gone from the store) =========
	if !store.wasDeleted(srcKey) {
		t.Errorf("src temp object %q was not deleted on apply", srcKey)
	}
	if !store.wasDeleted(outKey) {
		t.Errorf("out temp object %q was not deleted on apply", outKey)
	}
	if remaining, _ := store.List(ctx, media.JobPrefix(tenantID, siteID, job.ID)+"/"); len(remaining) != 0 {
		t.Errorf("temp objects remain after apply: %v", remaining)
	}

	// === ASSERT: completion SSE published ===================================
	var sawCompleted bool
	for _, e := range pub.events {
		if e.Type == site.EventMediaOptimizeCompleted {
			sawCompleted = true
		}
	}
	if !sawCompleted {
		t.Errorf("no %s event published; got %v", site.EventMediaOptimizeCompleted, eventTypes(pub.events))
	}
}
