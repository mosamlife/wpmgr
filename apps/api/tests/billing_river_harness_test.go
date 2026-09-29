package tests

// billing_river_harness_test.go — runs the billing River workers against the
// test database as wpmgr_app, the same way cmd/wpmgr does, so webhook intake
// (which only records and enqueues) can be followed by the apply, refresh and
// audit jobs it queued.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/mosamlife/wpmgr/apps/api/internal/billing"
	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// alertRecorder is a slog.Handler that keeps every record carrying an
// "alert" attribute, and forwards nothing else.
type alertRecorder struct {
	mu     sync.Mutex
	alerts []string
}

func (a *alertRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (a *alertRecorder) WithAttrs([]slog.Attr) slog.Handler       { return a }
func (a *alertRecorder) WithGroup(string) slog.Handler            { return a }
func (a *alertRecorder) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(at slog.Attr) bool {
		if at.Key == "alert" {
			a.mu.Lock()
			a.alerts = append(a.alerts, at.Value.String())
			a.mu.Unlock()
			return false
		}
		return true
	})
	return nil
}

func (a *alertRecorder) names() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.alerts...)
}

func (a *alertRecorder) has(name string) bool {
	for _, n := range a.names() {
		if n == name {
			return true
		}
	}
	return false
}

// migrateRiver applies River's own schema as the migration owner, as main
// does.
func migrateRiver(t *testing.T, pool *db.Pool) {
	t.Helper()
	owner := connectOwner(t, pool)
	defer owner.Close()
	migrator, err := rivermigrate.New(riverpgxv5.New(owner.Pool), nil)
	if err != nil {
		t.Fatalf("river migrator: %v", err)
	}
	if _, err := migrator.Migrate(context.Background(), rivermigrate.DirectionUp, nil); err != nil {
		t.Fatalf("river migrate: %v", err)
	}
}

// billingHarness is a billing Service wired to a running River client with
// the billing workers, plus the alerts its logger recorded.
type billingHarness struct {
	svc    *billing.Service
	client *river.Client[pgx.Tx]
	alerts *alertRecorder
	pool   *db.Pool
}

// newBillingHarness builds the Service over providers, migrates River, and
// starts a client with the billing apply, refresh and audit workers on the
// billing queue. The client stops at test cleanup.
func newBillingHarness(t *testing.T, pool *db.Pool, providers ...billing.Provider) *billingHarness {
	t.Helper()
	migrateRiver(t, pool)
	alerts := &alertRecorder{}
	logger := slog.New(alerts)
	svc := billing.New(pool, nil, true, domain.SystemClock{}, logger)
	defaultProvider := ""
	if len(providers) > 0 {
		defaultProvider = providers[0].Name()
	}
	svc.SetProviders(billing.NewRegistry(providers...), defaultProvider)

	workers := river.NewWorkers()
	river.AddWorker(workers, billing.NewBillingApplyWorker(svc))
	river.AddWorker(workers, billing.NewBillingRefreshWorker(svc))
	river.AddWorker(workers, billing.NewBillingAuditWorker(svc))
	client, err := river.NewClient(riverpgxv5.New(pool.Pool), &river.Config{
		Queues:            map[string]river.QueueConfig{billing.BillingQueue: {MaxWorkers: 1}},
		Workers:           workers,
		FetchCooldown:     20 * time.Millisecond,
		FetchPollInterval: 50 * time.Millisecond,
		Logger:            slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	if err != nil {
		t.Fatalf("river client: %v", err)
	}
	if err := client.Start(context.Background()); err != nil {
		t.Fatalf("river start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	})
	svc.SetRiver(client)
	return &billingHarness{svc: svc, client: client, alerts: alerts, pool: pool}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// countBillingJobs counts River jobs of kind in any of states (all states
// when none are given).
func countBillingJobs(t *testing.T, pool *db.Pool, kind string, states ...string) int {
	t.Helper()
	q := `SELECT count(*) FROM river_job WHERE kind = $1`
	args := []any{kind}
	if len(states) > 0 {
		q += ` AND state::text = ANY($2)`
		args = append(args, states)
	}
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("count river jobs: %v", err)
	}
	return n
}

// drain waits, with a bounded number of polls, until no billing-queue job is
// waiting or running, then fails the test if any job was discarded. It fails
// when the bound runs out.
func (h *billingHarness) drain(t *testing.T) {
	t.Helper()
	const polls = 200
	for i := 0; i < polls; i++ {
		var pending int
		if err := h.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM river_job WHERE queue = $1
			   AND state::text IN ('available','pending','running','scheduled','retryable')`,
			billing.BillingQueue).Scan(&pending); err != nil {
			t.Fatalf("drain: count pending jobs: %v", err)
		}
		if pending == 0 {
			var discarded int
			var errs []string
			rows, err := h.pool.Query(context.Background(),
				`SELECT kind, errors::text FROM river_job WHERE queue = $1 AND state::text = 'discarded'`, billing.BillingQueue)
			if err != nil {
				t.Fatalf("drain: read discarded jobs: %v", err)
			}
			for rows.Next() {
				var kind, e string
				if err := rows.Scan(&kind, &e); err != nil {
					t.Fatalf("drain: scan: %v", err)
				}
				discarded++
				errs = append(errs, kind+": "+e)
			}
			rows.Close()
			if discarded > 0 {
				t.Fatalf("drain: %d billing job(s) discarded:\n%s", discarded, strings.Join(errs, "\n"))
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("drain: billing jobs still pending after %d polls", polls)
}
