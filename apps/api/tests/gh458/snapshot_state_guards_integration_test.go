// Package gh458 proves the backup_snapshots state preconditions added in
// GH #458 through the GENERATED sqlc code, against a real Postgres, connected
// as the non-superuser wpmgr_app role the production control plane runs as.
//
// It lives in its own package (not tests/) on purpose: it must compile and run
// while internal/backup/repo.go is still mid-migration to the new :execrows
// signatures, and package tests imports internal/backup.
package gh458

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
)

const testRecipient = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"

// startPostgres mirrors tests.startPostgres: migrate as the bootstrap
// superuser, then hand the test a pool connected as wpmgr_app (NOSUPERUSER,
// NOBYPASSRLS) so the RLS policies are actually live. The admin pool is
// returned only for fixture seeding.
func startPostgres(t *testing.T) (app *db.Pool, admin *db.Pool) {
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
		t.Fatalf("migrate: %v", err)
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

	// Assert we are NOT a superuser: a superuser bypasses RLS, which would make
	// this proof run on a code path no install ever uses.
	var super bool
	if err := appPool.QueryRow(ctx,
		"SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&super); err != nil {
		t.Fatalf("read current_user rolsuper: %v", err)
	}
	if super {
		t.Fatal("test pool connected as a SUPERUSER; RLS would be bypassed and this proof would be meaningless")
	}
	return appPool, adminPool
}

type fixture struct {
	pool   *db.Pool
	tenant uuid.UUID
	site   uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	appPool, adminPool := startPostgres(t)
	f := &fixture{pool: appPool}

	if err := adminPool.QueryRow(ctx,
		"INSERT INTO tenants (name, slug) VALUES ('gh458', 'gh458') RETURNING id").Scan(&f.tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if err := adminPool.QueryRow(ctx,
		`INSERT INTO sites (tenant_id, url, name) VALUES ($1, $2, 'seed') RETURNING id`,
		f.tenant, "https://gh458-"+uuid.NewString()+".example.com").Scan(&f.site); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	return f
}

// seedSnapshot inserts a snapshot in the given status and returns its id. The
// insert goes through the app pool's tenant transaction, so RLS applies.
func (f *fixture) seedSnapshot(t *testing.T, status, errMsg string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	err := f.pool.InTenantTx(ctx, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO backup_snapshots (tenant_id, site_id, kind, status, age_recipient, error, finished_at)
			 VALUES ($1, $2, 'files', $3, $4, $5,
			         CASE WHEN $3 IN ('completed','failed') THEN now() ELSE NULL END)
			 RETURNING id`,
			f.tenant, f.site, status, testRecipient, errMsg).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed snapshot (status=%s): %v", status, err)
	}
	return id
}

type snapRow struct {
	status     string
	errMsg     string
	totalSize  int64
	chunkCount int64
}

func (f *fixture) read(t *testing.T, id uuid.UUID) snapRow {
	t.Helper()
	ctx := context.Background()
	var r snapRow
	err := f.pool.InTenantTx(ctx, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT status, error, total_size, chunk_count FROM backup_snapshots WHERE id = $1`,
			id).Scan(&r.status, &r.errMsg, &r.totalSize, &r.chunkCount)
	})
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	return r
}

// call runs fn through the GENERATED Queries inside a tenant transaction.
func (f *fixture) call(t *testing.T, fn func(q *sqlc.Queries) (int64, error)) int64 {
	t.Helper()
	var n int64
	err := f.pool.InTenantTx(context.Background(), f.tenant, func(tx pgx.Tx) error {
		var e error
		n, e = fn(sqlc.New(tx))
		return e
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	return n
}

func TestGH458SnapshotStateGuards(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// 1a. FailBackupSnapshot must not overwrite an already-COMPLETED row.
	t.Run("fail_does_not_overwrite_completed", func(t *testing.T) {
		id := f.seedSnapshot(t, "completed", "")
		n := f.call(t, func(q *sqlc.Queries) (int64, error) {
			return q.FailBackupSnapshot(ctx, sqlc.FailBackupSnapshotParams{
				ID: id, TenantID: f.tenant, Error: "late worker error",
			})
		})
		if n != 0 {
			t.Fatalf("FailBackupSnapshot on a completed row: rows=%d, want 0", n)
		}
		after := f.read(t, id)
		if after.status != "completed" {
			t.Fatalf("status = %q, want unchanged \"completed\"", after.status)
		}
		if after.errMsg != "" {
			t.Fatalf("error = %q, want still empty", after.errMsg)
		}
	})

	// 1b. CompleteBackupSnapshot must not resurrect an already-FAILED row.
	t.Run("complete_does_not_resurrect_failed", func(t *testing.T) {
		id := f.seedSnapshot(t, "failed", "cancelled by operator")
		n := f.call(t, func(q *sqlc.Queries) (int64, error) {
			return q.CompleteBackupSnapshot(ctx, sqlc.CompleteBackupSnapshotParams{
				ID: id, TenantID: f.tenant, TotalSize: 999, ChunkCount: 7,
			})
		})
		if n != 0 {
			t.Fatalf("CompleteBackupSnapshot on a failed row: rows=%d, want 0", n)
		}
		after := f.read(t, id)
		if after.status != "failed" {
			t.Fatalf("status = %q, want unchanged \"failed\"", after.status)
		}
		if after.errMsg != "cancelled by operator" {
			t.Fatalf("error was overwritten: %q", after.errMsg)
		}
		if after.totalSize == 999 {
			t.Fatal("total_size was overwritten on a terminal row")
		}
		if after.chunkCount == 7 {
			t.Fatal("chunk_count was overwritten on a terminal row")
		}
	})

	// 2. THE CANCEL CASE. CancelSnapshot fails a still-'pending' snapshot
	// through FailBackupSnapshot. This must still work, or operator cancel of
	// a queued backup is silently dead.
	t.Run("cancel_of_pending_still_works", func(t *testing.T) {
		id := f.seedSnapshot(t, "pending", "")
		n := f.call(t, func(q *sqlc.Queries) (int64, error) {
			return q.FailBackupSnapshot(ctx, sqlc.FailBackupSnapshotParams{
				ID: id, TenantID: f.tenant, Error: "cancelled by operator",
			})
		})
		if n != 1 {
			t.Fatalf("FailBackupSnapshot on a PENDING row: rows=%d, want 1 (CancelSnapshot is broken)", n)
		}
		after := f.read(t, id)
		if after.status != "failed" {
			t.Fatalf("status = %q, want \"failed\"", after.status)
		}
		if after.errMsg != "cancelled by operator" {
			t.Fatalf("error = %q, want the cancel marker", after.errMsg)
		}
	})

	// 2b. A running snapshot completes normally: the guard must not over-fire.
	t.Run("running_completes_normally", func(t *testing.T) {
		id := f.seedSnapshot(t, "running", "")
		n := f.call(t, func(q *sqlc.Queries) (int64, error) {
			return q.CompleteBackupSnapshot(ctx, sqlc.CompleteBackupSnapshotParams{
				ID: id, TenantID: f.tenant, TotalSize: 42, ChunkCount: 3,
			})
		})
		if n != 1 {
			t.Fatalf("CompleteBackupSnapshot on a running row: rows=%d, want 1", n)
		}
		if got := f.read(t, id).status; got != "completed" {
			t.Fatalf("status = %q, want \"completed\"", got)
		}
	})

	// 3. The claim is exactly-once: the second claimant gets 0 rows.
	t.Run("claim_is_exactly_once", func(t *testing.T) {
		id := f.seedSnapshot(t, "pending", "")
		first := f.call(t, func(q *sqlc.Queries) (int64, error) {
			return q.MarkBackupSnapshotRunning(ctx, sqlc.MarkBackupSnapshotRunningParams{
				ID: id, TenantID: f.tenant,
			})
		})
		if first != 1 {
			t.Fatalf("first claim: rows=%d, want 1", first)
		}
		second := f.call(t, func(q *sqlc.Queries) (int64, error) {
			return q.MarkBackupSnapshotRunning(ctx, sqlc.MarkBackupSnapshotRunningParams{
				ID: id, TenantID: f.tenant,
			})
		})
		if second != 0 {
			t.Fatalf("second claim: rows=%d, want 0 (the claim is not exclusive)", second)
		}
		if got := f.read(t, id).status; got != "running" {
			t.Fatalf("status = %q, want \"running\"", got)
		}
	})

	// 3b. A claim must never drag a terminal row back to 'running'.
	t.Run("claim_does_not_revive_terminal", func(t *testing.T) {
		id := f.seedSnapshot(t, "failed", "cancelled by operator")
		n := f.call(t, func(q *sqlc.Queries) (int64, error) {
			return q.MarkBackupSnapshotRunning(ctx, sqlc.MarkBackupSnapshotRunningParams{
				ID: id, TenantID: f.tenant,
			})
		})
		if n != 0 {
			t.Fatalf("claim on a failed row: rows=%d, want 0", n)
		}
		if got := f.read(t, id).status; got != "failed" {
			t.Fatalf("status = %q, want unchanged \"failed\"", got)
		}
	})
}
