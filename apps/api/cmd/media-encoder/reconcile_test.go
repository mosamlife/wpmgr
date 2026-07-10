package main

// reconcile_test.go — GH #205 fast-follow: proves
// reconcileStrandedPublicJobs re-enqueues encoder-owned rows stranded in the
// public River schema onto a dedicated media schema, and — critically —
// never touches a row belonging to one of the API's own job kinds. Mirrors
// the testcontainers pattern used throughout apps/api/tests (see
// tests/rls_integration_test.go's startPostgres) but lives in-package since
// it exercises this package's unexported reconcile helpers.

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/media/model"
	"github.com/mosamlife/wpmgr/apps/api/internal/riverutil"
	"github.com/mosamlife/wpmgr/apps/api/internal/screenshot"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

// startReconcileTestPostgres spins up an ephemeral Postgres, migrates the
// full WPMgr schema (creates the wpmgr_app role) plus River's own public
// schema, and returns both the unprivileged app pool and the owner pool.
// Trimmed copy of tests/rls_integration_test.go's startPostgres (unexported
// in a different package and so cannot be imported here).
func startReconcileTestPostgres(t *testing.T) (app *db.Pool, admin *db.Pool) {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("wpmgr"),
		tcpostgres.WithUsername("wpmgr"),
		tcpostgres.WithPassword("wpmgr"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Skipf("skipping: cannot start postgres container (docker unavailable?): %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	adminDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	adminPool, err := db.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if err := adminPool.Migrate(ctx); err != nil {
		t.Fatalf("migrate wpmgr schema: %v", err)
	}
	migrator, err := rivermigrate.New(riverpgxv5.New(adminPool.Pool), nil)
	if err != nil {
		t.Fatalf("river migrator: %v", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatalf("migrate river public schema: %v", err)
	}
	for _, stmt := range []string{
		"ALTER ROLE wpmgr_app LOGIN PASSWORD 'app'",
		"GRANT USAGE ON SCHEMA public TO wpmgr_app",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO wpmgr_app",
		"REVOKE UPDATE, DELETE, TRUNCATE ON audit_log FROM wpmgr_app",
	} {
		if _, err := adminPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("provision app role (%q): %v", stmt, err)
		}
	}
	t.Cleanup(adminPool.Close)

	appDSN := strings.Replace(adminDSN, "wpmgr:wpmgr@", "wpmgr_app:app@", 1)
	appPool, err := db.Connect(ctx, appDSN)
	if err != nil {
		t.Fatalf("connect app: %v", err)
	}
	t.Cleanup(appPool.Close)

	return appPool, adminPool
}

// TestReconcileStrandedPublicJobs_MovesOnlyEncoderKinds is the GH #205
// regression lock: encoder-owned jobs (media_encode, site_screenshot_capture)
// left behind in public.river_job are re-enqueued onto the dedicated media
// schema and cancelled in place, while a job of an API-owned kind
// (site_health_check) sitting in the SAME table is left completely
// untouched — proving the reconcile is scoped strictly to encoder kinds and
// can never interfere with the API's own fleet periodics.
func TestReconcileStrandedPublicJobs_MovesOnlyEncoderKinds(t *testing.T) {
	appPool, adminPool := startReconcileTestPostgres(t)
	ctx := context.Background()

	const mediaSchema = "media_encoder"
	if err := riverutil.EnsureSchema(ctx, adminPool.Pool, mediaSchema, "wpmgr_app"); err != nil {
		t.Fatalf("ensure media schema: %v", err)
	}

	// Insert stranded rows into the PUBLIC schema exactly as they would have
	// landed before the fix — one insert-only client per real job type, so
	// every NOT NULL / default column is populated correctly by River itself.
	publicClient, err := river.NewClient(riverpgxv5.New(appPool.Pool), &river.Config{SkipUnknownJobCheck: true})
	if err != nil {
		t.Fatalf("public client: %v", err)
	}
	encodeSiteID, screenshotSiteID := uuid.New(), uuid.New()
	if _, err := publicClient.Insert(ctx, model.EncodeArgs{
		TenantID: uuid.New(), SiteID: encodeSiteID, JobID: "stranded-encode",
	}, nil); err != nil {
		t.Fatalf("insert stranded encode job: %v", err)
	}
	if _, err := publicClient.Insert(ctx, screenshot.CaptureArgs{
		TenantID: uuid.New(), SiteID: screenshotSiteID,
		SiteURL: "https://stranded.example.com", Reason: screenshot.ReasonManual,
	}, nil); err != nil {
		t.Fatalf("insert stranded screenshot job: %v", err)
	}
	// A control row of an API-owned kind — reconcile must never touch this.
	if _, err := publicClient.Insert(ctx, site.HealthCheckArgs{}, nil); err != nil {
		t.Fatalf("insert control health-check job: %v", err)
	}

	mediaClient, err := river.NewClient(riverpgxv5.New(appPool.Pool), &river.Config{
		Schema: mediaSchema, SkipUnknownJobCheck: true,
	})
	if err != nil {
		t.Fatalf("media client: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reconcileStrandedPublicJobs(ctx, appPool, mediaClient, logger)

	// The encoder-owned rows in public must now be cancelled (never
	// re-processed from the schema nothing polls anymore).
	if n := countRows(t, appPool, `SELECT count(*) FROM river_job WHERE kind = 'media_encode' AND state = 'cancelled'`); n != 1 {
		t.Fatalf("public cancelled media_encode rows = %d, want 1", n)
	}
	if n := countRows(t, appPool, `SELECT count(*) FROM river_job WHERE kind = 'site_screenshot_capture' AND state = 'cancelled'`); n != 1 {
		t.Fatalf("public cancelled site_screenshot_capture rows = %d, want 1", n)
	}
	// ...and re-enqueued into the dedicated media schema, available to run.
	if n := countRows(t, appPool, `SELECT count(*) FROM "media_encoder"."river_job" WHERE kind = 'media_encode' AND state = 'available'`); n != 1 {
		t.Fatalf("media schema available media_encode rows = %d, want 1", n)
	}
	if n := countRows(t, appPool, `SELECT count(*) FROM "media_encoder"."river_job" WHERE kind = 'site_screenshot_capture' AND state = 'available'`); n != 1 {
		t.Fatalf("media schema available site_screenshot_capture rows = %d, want 1", n)
	}

	// The API-owned control row must be completely untouched: still
	// available in public, and never copied into the media schema.
	if n := countRows(t, appPool, `SELECT count(*) FROM river_job WHERE kind = 'site_health_check' AND state = 'available'`); n != 1 {
		t.Fatalf("public available site_health_check rows = %d, want 1 (untouched)", n)
	}
	if n := countRows(t, appPool, `SELECT count(*) FROM "media_encoder"."river_job" WHERE kind = 'site_health_check'`); n != 0 {
		t.Fatalf("media schema site_health_check rows = %d, want 0 (never touched)", n)
	}
}

// TestReconcileStrandedPublicJobs_NoStrandedRowsIsNoop verifies the reconcile
// is a safe no-op (no error, no side effects) when public.river_job holds no
// matching rows — the common case on every boot after the one-time
// reconcile has already run.
func TestReconcileStrandedPublicJobs_NoStrandedRowsIsNoop(t *testing.T) {
	appPool, adminPool := startReconcileTestPostgres(t)
	ctx := context.Background()

	const mediaSchema = "media_encoder"
	if err := riverutil.EnsureSchema(ctx, adminPool.Pool, mediaSchema, "wpmgr_app"); err != nil {
		t.Fatalf("ensure media schema: %v", err)
	}
	mediaClient, err := river.NewClient(riverpgxv5.New(appPool.Pool), &river.Config{
		Schema: mediaSchema, SkipUnknownJobCheck: true,
	})
	if err != nil {
		t.Fatalf("media client: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reconcileStrandedPublicJobs(ctx, appPool, mediaClient, logger) // must not panic or hang

	if n := countRows(t, appPool, `SELECT count(*) FROM "media_encoder"."river_job"`); n != 0 {
		t.Fatalf("media schema rows = %d, want 0", n)
	}
}

// TestDecodeEncoderArgs verifies every encoder-owned kind round-trips through
// JSON decode and that an unrecognized (e.g. API-owned) kind is rejected —
// defense in depth behind the SQL-level kind filter.
func TestDecodeEncoderArgs(t *testing.T) {
	if _, err := decodeEncoderArgs("media_encode", []byte(`{"tenant_id":"`+uuid.Nil.String()+`","site_id":"`+uuid.Nil.String()+`","job_id":"j"}`)); err != nil {
		t.Fatalf("decode media_encode: %v", err)
	}
	if _, err := decodeEncoderArgs("font_transcode", []byte(`{}`)); err != nil {
		t.Fatalf("decode font_transcode: %v", err)
	}
	if _, err := decodeEncoderArgs("site_screenshot_capture", []byte(`{}`)); err != nil {
		t.Fatalf("decode site_screenshot_capture: %v", err)
	}
	if _, err := decodeEncoderArgs("screenshot_weekly_fanout", []byte(`{}`)); err != nil {
		t.Fatalf("decode screenshot_weekly_fanout: %v", err)
	}
	if _, err := decodeEncoderArgs("site_health_check", []byte(`{}`)); err == nil {
		t.Fatal("decode site_health_check: want error, got nil (must never accept an API-owned kind)")
	}
}

// countRows runs a focused count query for test assertions.
func countRows(t *testing.T, pool *db.Pool, query string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("count rows (%q): %v", query, err)
	}
	return n
}
