package tests

import (
	"context"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
	"github.com/mosamlife/wpmgr/apps/api/internal/site"
)

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
	checker := site.NewHealthChecker(repo, 10*time.Minute)
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
