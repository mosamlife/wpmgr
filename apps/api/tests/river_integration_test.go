package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/mosamlife/wpmgr/apps/api/internal/config"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
	"github.com/mosamlife/wpmgr/apps/api/internal/riverutil"
	"github.com/mosamlife/wpmgr/apps/api/internal/screenshot"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// TestHealthCheckerPruneNonces proves the periodic maintenance prune deletes
// agent_nonces older than the signature-skew window while keeping recent ones,
// running under the unprivileged wpmgr_app role on the app pool (same path the
// River health job uses). This is the anti-bloat/DoS fix from the M2 review.
func TestHealthCheckerPruneNonces(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	// Enroll a site so we have a valid site_id for the nonces (FK).
	tenant := seedTenant(t, pool, "nonce-prune")
	repo := site.NewRepo(pool)
	svc := site.NewService(repo, domain.NewValidator(), domain.SystemClock{})
	code, _ := svc.CreatePairingCode(ctx, site.CreatePairingCodeInput{TenantID: tenant})
	_, _, key := genKey(t)
	s, err := svc.Enroll(ctx, site.EnrollRequest{PairingCode: code.Plaintext, SiteURL: "https://nonce.example.com", AgentPublicKey: key})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	// Record two nonces via the normal agent path (app.agent GUC, app role).
	if ok, err := repo.RecordNonce(ctx, s.ID, "old-nonce"); err != nil || !ok {
		t.Fatalf("record old nonce: ok=%v err=%v", ok, err)
	}
	if ok, err := repo.RecordNonce(ctx, s.ID, "recent-nonce"); err != nil || !ok {
		t.Fatalf("record recent nonce: ok=%v err=%v", ok, err)
	}

	// Age the "old" nonce well past the skew window (created_at is server-set on
	// insert, so backdate it out-of-band via the superuser connection).
	admin := connectAdmin(t, pool)
	defer admin.Close()
	if _, err := admin.Exec(ctx, "UPDATE agent_nonces SET created_at = now() - interval '1 hour' WHERE nonce = 'old-nonce'"); err != nil {
		t.Fatalf("age nonce: %v", err)
	}

	// Run the prune through the HealthChecker (5m skew) under the app role.
	checker := site.NewHealthChecker(repo, 10*time.Minute, 5*time.Minute)
	deleted, err := checker.PruneNonces(ctx, time.Now())
	if err != nil {
		t.Fatalf("prune nonces: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 nonce deleted, got %d", deleted)
	}

	// Assert the old row is gone and the recent row is kept.
	var remaining []string
	rows, err := admin.Query(ctx, "SELECT nonce FROM agent_nonces WHERE site_id = $1 ORDER BY nonce", s.ID)
	if err != nil {
		t.Fatalf("query remaining: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		remaining = append(remaining, n)
	}
	if len(remaining) != 1 || remaining[0] != "recent-nonce" {
		t.Fatalf("expected only recent-nonce to remain, got %v", remaining)
	}
}

// TestRiverWiringAndHealthJob proves the production River wiring works against
// the real DB and the NOSUPERUSER app role: River's schema is migrated as the
// owner, the client starts on the app pool, and the periodic health job runs on
// start and marks a stale enrolled site unreachable.
func TestRiverWiringAndHealthJob(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	// Migrate River's own schema as the owner (mirrors main.migrateRiver).
	admin := connectAdmin(t, pool)
	defer admin.Close()
	migrator, err := rivermigrate.New(riverpgxv5.New(admin.Pool), nil)
	if err != nil {
		t.Fatalf("river migrator: %v", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatalf("river migrate: %v", err)
	}
	// River's tables are created by the owner; grant the app role access (in
	// prod the migration owner's ALTER DEFAULT PRIVILEGES covers this — here the
	// container superuser created them, so grant explicitly).
	if _, err := admin.Exec(ctx, "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO wpmgr_app"); err != nil {
		t.Fatalf("grant river tables: %v", err)
	}

	// Seed a stale enrolled site.
	tenant := seedTenant(t, pool, "river-health")
	repo := site.NewRepo(pool)
	svc := site.NewService(repo, domain.NewValidator(), domain.SystemClock{})
	code, _ := svc.CreatePairingCode(ctx, site.CreatePairingCodeInput{TenantID: tenant})
	_, _, key := genKey(t)
	s, err := svc.Enroll(ctx, site.EnrollRequest{PairingCode: code.Plaintext, SiteURL: "https://river.example.com", AgentPublicKey: key})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if _, err := admin.Exec(ctx, "UPDATE sites SET last_seen_at = now() - interval '1 hour' WHERE id = $1", s.ID); err != nil {
		t.Fatalf("age site: %v", err)
	}

	// Build + start the River client on the app pool, exactly as main does.
	checker := site.NewHealthChecker(repo, 10*time.Minute, 5*time.Minute)
	workers := river.NewWorkers()
	river.AddWorker(workers, site.NewHealthCheckWorker(checker))
	client, err := river.NewClient(riverpgxv5.New(pool.Pool), &river.Config{
		Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 2}},
		Workers: workers,
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(
				river.PeriodicInterval(time.Hour),
				func() (river.JobArgs, *river.InsertOpts) { return site.HealthCheckArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true},
			),
		},
	})
	if err != nil {
		t.Fatalf("river client: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("river start: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	}()

	// The RunOnStart periodic job should fire and mark the stale site unreachable.
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, gerr := svc.Get(ctx, tenant, s.ID)
		if gerr != nil {
			t.Fatalf("get site: %v", gerr)
		}
		if got.HealthStatus == "unreachable" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("health job did not mark site unreachable within deadline (status=%s)", got.HealthStatus)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestRiverDualSchemaIsolation verifies job ROUTING when media isolation is
// configured: encoder-owned jobs (media_encode, site_screenshot_capture) are
// inserted into the media schema while API-owned jobs (site_health_check) stay
// in public. The clients here are insert-only and never started, so this asserts
// where jobs land, not River leader election.
func TestRiverDualSchemaIsolation(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	admin := connectAdmin(t, pool)
	defer admin.Close()

	migrator, err := rivermigrate.New(riverpgxv5.New(admin.Pool), nil)
	if err != nil {
		t.Fatalf("river migrator: %v", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatalf("river migrate public: %v", err)
	}
	if _, err := admin.Exec(ctx, "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO wpmgr_app"); err != nil {
		t.Fatalf("grant public river tables: %v", err)
	}
	if _, err := admin.Exec(ctx, "GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO wpmgr_app"); err != nil {
		t.Fatalf("grant public river sequences: %v", err)
	}

	const mediaSchema = "media_encoder"
	if err := riverutil.EnsureSchema(ctx, admin.Pool, mediaSchema, "wpmgr_app"); err != nil {
		t.Fatalf("ensure media schema: %v", err)
	}

	defaultClient, err := river.NewClient(riverpgxv5.New(pool.Pool), &river.Config{
		SkipUnknownJobCheck: true,
	})
	if err != nil {
		t.Fatalf("default river client: %v", err)
	}
	mediaClient, err := river.NewClient(riverpgxv5.New(pool.Pool), &river.Config{
		Schema:              mediaSchema,
		SkipUnknownJobCheck: true,
	})
	if err != nil {
		t.Fatalf("media river client: %v", err)
	}

	if _, err := mediaClient.Insert(ctx, model.EncodeArgs{
		TenantID: uuid.New(),
		SiteID:   uuid.New(),
		JobID:    "job-media-schema-test",
	}, nil); err != nil {
		t.Fatalf("insert media job: %v", err)
	}
	if _, err := mediaClient.Insert(ctx, screenshot.CaptureArgs{
		TenantID: uuid.New(),
		SiteID:   uuid.New(),
		SiteURL:  "https://river-screenshot.example.com",
		Reason:   screenshot.ReasonManual,
	}, nil); err != nil {
		t.Fatalf("insert screenshot job: %v", err)
	}
	if _, err := defaultClient.Insert(ctx, site.HealthCheckArgs{}, nil); err != nil {
		t.Fatalf("insert default job: %v", err)
	}

	if got := countRiverJobs(t, admin, `SELECT count(*) FROM "media_encoder"."river_job" WHERE kind = 'media_encode'`); got != 1 {
		t.Fatalf("media schema media_encode jobs = %d, want 1", got)
	}
	if got := countRiverJobs(t, admin, `SELECT count(*) FROM public.river_job WHERE kind = 'media_encode'`); got != 0 {
		t.Fatalf("public media_encode jobs = %d, want 0", got)
	}
	if got := countRiverJobs(t, admin, `SELECT count(*) FROM "media_encoder"."river_job" WHERE kind = 'site_screenshot_capture'`); got != 1 {
		t.Fatalf("media schema site_screenshot_capture jobs = %d, want 1", got)
	}
	if got := countRiverJobs(t, admin, `SELECT count(*) FROM public.river_job WHERE kind = 'site_screenshot_capture'`); got != 0 {
		t.Fatalf("public site_screenshot_capture jobs = %d, want 0", got)
	}
	if got := countRiverJobs(t, admin, `SELECT count(*) FROM public.river_job WHERE kind = 'site_health_check'`); got != 1 {
		t.Fatalf("public site_health_check jobs = %d, want 1", got)
	}
	if got := countRiverJobs(t, admin, `SELECT count(*) FROM "media_encoder"."river_job" WHERE kind = 'site_health_check'`); got != 0 {
		t.Fatalf("media schema site_health_check jobs = %d, want 0", got)
	}
}

// TestRiverMediaSchemaSelfHealsOnBoot proves GH #205's load-bearing safety
// property for the new binary default: a fresh/upgraded deploy that never
// set WPMGR_RIVER_MEDIA_SCHEMA resolves to config's "media_encoder" default,
// and EnsureSchema — called unconditionally at API boot (cmd/wpmgr/main.go)
// and at media-encoder boot — creates that schema, migrates River's tables
// into it, and grants the app role access, all from a completely fresh
// database where the schema has never existed. Without this, changing the
// binary default would break any deploy that relied on the old empty/public
// default.
func TestRiverMediaSchemaSelfHealsOnBoot(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()
	admin := connectAdmin(t, pool)
	defer admin.Close()

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.River.MediaSchema != "media_encoder" {
		t.Fatalf("config default River.MediaSchema = %q, want media_encoder", cfg.River.MediaSchema)
	}

	// Confirm the schema genuinely does not exist yet on this fresh database.
	if got := countRiverJobs(t, admin,
		"SELECT count(*) FROM information_schema.schemata WHERE schema_name = 'media_encoder'"); got != 0 {
		t.Fatalf("media_encoder schema already exists before EnsureSchema, want 0")
	}

	// Mirror exactly what cmd/wpmgr/main.go's run() and cmd/media-encoder's
	// run() both do unconditionally at boot.
	if err := riverutil.EnsureSchema(ctx, admin.Pool, cfg.River.MediaSchema, "wpmgr_app"); err != nil {
		t.Fatalf("ensure media schema from config default: %v", err)
	}

	if got := countRiverJobs(t, admin,
		"SELECT count(*) FROM information_schema.schemata WHERE schema_name = 'media_encoder'"); got != 1 {
		t.Fatalf("media_encoder schema after EnsureSchema = %d, want 1", got)
	}
	if got := countRiverJobs(t, admin,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'media_encoder' AND table_name = 'river_job'`); got != 1 {
		t.Fatalf("media_encoder.river_job table after EnsureSchema = %d, want 1", got)
	}

	// The unprivileged app role must be able to insert immediately — proving
	// the grants (not just CREATE SCHEMA) are also self-healed.
	client, err := river.NewClient(riverpgxv5.New(pool.Pool), &river.Config{
		Schema: cfg.River.MediaSchema, SkipUnknownJobCheck: true,
	})
	if err != nil {
		t.Fatalf("media client: %v", err)
	}
	if _, err := client.Insert(ctx, model.EncodeArgs{
		TenantID: uuid.New(), SiteID: uuid.New(), JobID: "self-heal-check",
	}, nil); err != nil {
		t.Fatalf("insert into self-healed media schema as app role: %v", err)
	}
}

// countRiverJobs returns the number of River jobs matching a focused
// integration-test query.
func countRiverJobs(t *testing.T, pool *db.Pool, query string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("count river jobs: %v", err)
	}
	return n
}
