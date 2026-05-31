//go:build cgo

// media_encode_integration_test.go — the highest-value automated proof of the
// Media Optimizer encode path (ADR-043 Phase 7) that we can run WITHOUT a live
// WordPress (the live-WP E2E container is deferred per ADR-043 §7).
//
// It exercises the REAL pieces end-to-end on the CP side:
//   - real Postgres (testcontainers, ALL migrations, non-superuser wpmgr_app role)
//   - real MinIO (testcontainers) behind the real blobstore.Store
//   - the REAL lilliput CGO encoder (encoder.LilliputEncoder)
//   - the REAL media repos (repo.Repo) for the agent-GUC writes
//   - the REAL worker.EncodeWorker.Work entrypoint
//
// Only the OUTBOUND collaborators that would otherwise touch the network are
// stubbed: the SSE EventPublisher (collected), the SiteLookup (returns a URL +
// enrolled), and the AgentApplyClient (captures the MediaApplyRequest the agent
// would receive). The source bytes really transit object storage via presigned
// PUT/GET, the encoder really produces an AVIF, and the output really lands in
// MinIO.
//
// This file lives behind //go:build cgo because it imports the lilliput encoder
// (CGO + native codec libs). The main API (cmd/wpmgr) is CGO_ENABLED=0 and never
// imports the encoder; this test does not change that — it only adds a test file
// to the already-CGO test set.
//
// It calls EncodeWorker.Work directly with a hand-built *river.Job rather than
// standing up a River client: Work is the unit of work and takes only the args,
// so a direct call is the tightest possible exercise of the encode path and
// needs no River DB wiring. (The River insert/dispatch seam is covered by the
// pure-Go service test in media_encode_service_integration_test.go.)
package tests

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/mosamlife/wpmgr/apps/api/internal/agentcmd"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/media"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/encoder"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/repo"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/worker"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
	siteevents "github.com/mosamlife/wpmgr/apps/api/internal/site/events"
)

// ---------------------------------------------------------------------------
// stubs for the worker's outbound (network-touching) collaborators
// ---------------------------------------------------------------------------

// collectingPublisher records every published SSE envelope so the test can
// assert a media.optimize.progress event was emitted for the encoded variant.
type collectingPublisher struct {
	events []site.ConnectionEvent
}

func (p *collectingPublisher) Publish(_ context.Context, ev site.ConnectionEvent) error {
	p.events = append(p.events, ev)
	return nil
}

// mediaEncoderSiteLookup returns a fixed URL + enrolled, standing in for the DB
// site lookup the media-encoder would otherwise run. (Named to avoid colliding
// with the backup test's stubSiteLookup in this package.)
type mediaEncoderSiteLookup struct {
	url      string
	enrolled bool
}

func (l mediaEncoderSiteLookup) GetMediaSiteURL(_ context.Context, _, _ uuid.UUID) (string, bool, error) {
	return l.url, l.enrolled, nil
}

// captureApplyClient records the MediaApplyRequest the worker dispatches so the
// test can verify the agent would receive each variant's presigned GET URL.
type captureApplyClient struct {
	called  int
	siteID  uuid.UUID
	siteURL string
	req     agentcmd.MediaApplyRequest
}

func (c *captureApplyClient) MediaApply(_ context.Context, siteID uuid.UUID, siteURL string, req agentcmd.MediaApplyRequest) (agentcmd.MediaApplyResponse, error) {
	c.called++
	c.siteID = siteID
	c.siteURL = siteURL
	c.req = req
	return agentcmd.MediaApplyResponse{OK: true}, nil
}

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

// makeFixtureJPEG renders a small gradient JPEG (mirrors the encoder package's
// golden fixture) so the test needs no checked-in binary. 256x256 is large
// enough that the AVIF re-encode produces a non-trivial container.
func makeFixtureJPEG(t *testing.T) []byte {
	t.Helper()
	const w, h = 256, 256
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: uint8((x + y) / 2), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatalf("encode fixture jpeg: %v", err)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// the test
// ---------------------------------------------------------------------------

// TestMediaEncodeWorker_EndToEnd proves the full CP-side encode round-trip:
// a real source JPEG put to media/<t>/<s>/<job>/src/<name> in MinIO is
// presigned-GET'd, encoded to AVIF by lilliput, presigned-PUT to out/<name>,
// recorded as a succeeded media_variant_results row, announced over SSE, and
// dispatched to the agent as a media_apply command carrying the variant's
// presigned GET URL.
func TestMediaEncodeWorker_EndToEnd(t *testing.T) {
	// startPostgres / startBlobstore both skip (not fail) when Docker is absent.
	pool := startPostgres(t)
	store := startBlobstore(t)
	ctx := context.Background()

	// Real encoder (CGO). Concurrency 1 is plenty for one variant.
	enc := encoder.NewLilliputEncoder(1)
	defer enc.Close()

	mediaRepo := repo.NewRepo(pool)

	// --- seed: tenant + site + an in-progress optimize job ------------------
	tenantID := seedTenant(t, pool, "media-encode-tenant")
	siteID := seedEnrolledSite(t, pool, tenantID, "https://media.example.com")
	jobID := siteevents.NewULID(time.Now())
	const wpAttachmentID = int64(4242)
	seedMediaJob(t, pool, tenantID, siteID, jobID, wpAttachmentID)

	// --- upload a REAL source JPEG to the exact src/<name> key the worker reads.
	src := makeFixtureJPEG(t)
	const variantName = "full"
	srcKey := media.SrcKey(tenantID, siteID, jobID, variantName)
	if err := store.Put(ctx, srcKey, bytes.NewReader(src), int64(len(src))); err != nil {
		t.Fatalf("seed source object: %v", err)
	}

	// --- wire the worker with real encoder/repo/store + stubbed outbound deps.
	pub := &collectingPublisher{}
	sites := mediaEncoderSiteLookup{url: "https://media.example.com", enrolled: true}
	apply := &captureApplyClient{}
	w := worker.NewEncodeWorker(enc, mediaRepo, store, pub, sites, apply, 10*time.Minute, nil)

	// --- run the unit of work directly (no River client needed).
	job := &river.Job[model.EncodeArgs]{Args: model.EncodeArgs{
		TenantID:      tenantID,
		SiteID:        siteID,
		JobID:         jobID,
		TargetFormat:  media.TargetAVIF,
		TargetQuality: media.QualityLossy,
		Variants: []model.EncodeVariant{
			{Name: variantName, SourceSize: int64(len(src)), SourceMime: "image/jpeg"},
		},
	}}
	if err := w.Work(ctx, job); err != nil {
		t.Fatalf("EncodeWorker.Work: %v", err)
	}

	// === ASSERT 1: media_variant_results row succeeded with optimized_size>0 =
	variants, err := mediaRepo.ListVariantsForJob(ctx, tenantID, jobID)
	if err != nil {
		t.Fatalf("list variants: %v", err)
	}
	if len(variants) != 1 {
		t.Fatalf("got %d variant rows, want 1", len(variants))
	}
	vr := variants[0]
	if vr.State != model.VariantSucceeded {
		t.Fatalf("variant state = %q, want succeeded", vr.State)
	}
	if vr.OptimizedSizeBytes == nil || *vr.OptimizedSizeBytes <= 0 {
		t.Fatalf("optimized_size_bytes = %v, want > 0", vr.OptimizedSizeBytes)
	}
	if vr.OptimizedMime != "image/avif" {
		t.Errorf("optimized_mime = %q, want image/avif", vr.OptimizedMime)
	}
	if vr.SourceMime != "image/jpeg" {
		t.Errorf("source_mime = %q, want image/jpeg (magic-byte detection)", vr.SourceMime)
	}

	// === ASSERT 2: out/<name> exists in MinIO and is a VALID AVIF container ==
	outKey := media.OutKey(tenantID, siteID, jobID, variantName)
	exists, size, err := store.Head(ctx, outKey)
	if err != nil {
		t.Fatalf("head out object: %v", err)
	}
	if !exists || size <= 0 {
		t.Fatalf("out object exists=%v size=%d, want exists & size>0", exists, size)
	}
	rc, err := store.Get(ctx, outKey)
	if err != nil {
		t.Fatalf("get out object: %v", err)
	}
	outBytes, _ := io.ReadAll(rc)
	_ = rc.Close()
	if int64(len(outBytes)) != size {
		t.Fatalf("out body len %d != head size %d", len(outBytes), size)
	}
	if !isAVIF(outBytes) {
		t.Fatalf("out object is not a valid AVIF container: % x", head(outBytes, 16))
	}
	// Recorded size must match the bytes actually stored.
	if *vr.OptimizedSizeBytes != int64(len(outBytes)) {
		t.Errorf("recorded optimized_size %d != stored out size %d", *vr.OptimizedSizeBytes, len(outBytes))
	}

	// === ASSERT 3: a media.optimize.progress SSE event was published ========
	var progress *site.ConnectionEvent
	for i := range pub.events {
		if pub.events[i].Type == site.EventMediaOptimizeProgress {
			progress = &pub.events[i]
			break
		}
	}
	if progress == nil {
		t.Fatalf("no %s event published; got %v", site.EventMediaOptimizeProgress, eventTypes(pub.events))
	}
	if progress.TenantID != tenantID || progress.SiteID != siteID {
		t.Errorf("progress event scope = (%s,%s), want (%s,%s)", progress.TenantID, progress.SiteID, tenantID, siteID)
	}
	if progress.Data["variant"] != variantName {
		t.Errorf("progress event variant = %v, want %q", progress.Data["variant"], variantName)
	}

	// === ASSERT 4: agent received a MediaApplyRequest with the presigned URL =
	if apply.called != 1 {
		t.Fatalf("MediaApply dispatched %d times, want 1", apply.called)
	}
	if apply.siteID != siteID {
		t.Errorf("apply siteID = %s, want %s", apply.siteID, siteID)
	}
	if apply.req.JobID != jobID {
		t.Errorf("apply job_id = %q, want %q", apply.req.JobID, jobID)
	}
	if len(apply.req.Variants) != 1 {
		t.Fatalf("apply carried %d variants, want 1", len(apply.req.Variants))
	}
	av := apply.req.Variants[0]
	if av.Name != variantName {
		t.Errorf("apply variant name = %q, want %q", av.Name, variantName)
	}
	if av.GetURL == "" {
		t.Fatal("apply variant get_url is empty; agent could not download the output")
	}
	if av.OptimizedSize != *vr.OptimizedSizeBytes {
		t.Errorf("apply variant optimized_size %d != recorded %d", av.OptimizedSize, *vr.OptimizedSizeBytes)
	}
	// The presigned GET url in the apply request must actually fetch the AVIF we
	// stored — proving it is a live, correctly-signed handle the agent can use.
	resp, err := http.Get(av.GetURL)
	if err != nil {
		t.Fatalf("fetch apply get_url: %v", err)
	}
	fetched, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apply get_url status = %d", resp.StatusCode)
	}
	if !bytes.Equal(fetched, outBytes) {
		t.Fatalf("apply get_url returned %d bytes, want the stored AVIF (%d bytes)", len(fetched), len(outBytes))
	}
}

// ---------------------------------------------------------------------------
// seed + assertion helpers (CGO-path only; shared helpers seedEnrolledSite +
// eventTypes live in media_encode_service_integration_test.go so both the CGO
// and the CGO-free build sets can use them)
// ---------------------------------------------------------------------------

// seedMediaJob inserts an in_progress optimize job under the agent GUC (matching
// the encode-ready callback's state). The worker re-reads this row and proceeds
// only because it is non-terminal.
func seedMediaJob(t *testing.T, pool *db.Pool, tenantID, siteID uuid.UUID, jobID string, wpAttachmentID int64) {
	t.Helper()
	err := pool.InAgentTx(context.Background(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`INSERT INTO media_optimization_jobs
				(id, tenant_id, site_id, wp_attachment_id, kind, target_format,
				 target_quality, state, variants_total, started_at, created_at)
			 VALUES ($1, $2, $3, $4, 'optimize', 'avif', 'lossy', 'in_progress', 1, now(), now())`,
			jobID, tenantID, siteID, wpAttachmentID)
		return err
	})
	if err != nil {
		t.Fatalf("seed media job: %v", err)
	}
}

// isAVIF reports whether b is an AVIF container: an ISO-BMFF 'ftyp' box at
// offset 4 whose major/compatible brand is 'avif' (or 'avis'). This is the same
// magic-byte family the encoder golden test asserts on.
func isAVIF(b []byte) bool {
	if len(b) < 12 {
		return false
	}
	if !bytes.Equal(b[4:8], []byte("ftyp")) {
		return false
	}
	brand := b[8:12]
	return bytes.Equal(brand, []byte("avif")) || bytes.Equal(brand, []byte("avis"))
}

func head(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}
