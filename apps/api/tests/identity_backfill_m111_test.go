// identity_backfill_m111_test.go: a backfill only ever runs once, and the
// population it had to cover kept changing after it ran.
//
// Boots a container, applies every embedded migration UP TO (not including)
// m111, seeds the users rows a live install would already have, including the
// one a rollback window leaves behind, then finishes the boot so m111 runs and
// asserts the identities it had to repair now exist. Also proves m111 is
// re-runnable and that it leaves the identity key alone: issuer stays in it.
package tests

import (
	"context"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mosamlife/wpmgr/apps/api/internal/db"
	"github.com/mosamlife/wpmgr/apps/api/migrations"
)

// m111MigrationVersion is the embedded migration that re-runs m110's backfill.
const m111MigrationVersion = "20260813000000_m111_identity_backfill_repair"

// startPostgresBeforeM111 boots a container and applies everything up to but
// not including m111, using the bootstrap superuser connection directly: this
// test is about backfill correctness, and user_identities carries no RLS by
// design (a user spans tenants).
func startPostgresBeforeM111(t *testing.T) *db.Pool {
	t.Helper()
	ctx := context.Background()

	skipIfDockerUnavailable(t, ctx, "postgres")

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
	// container can be non-nil even when err != nil (partial start); register
	// cleanup before the error check so a failure path cannot leak it (see
	// rls_integration_test.go's startPostgres).
	if container != nil {
		t.Cleanup(func() { _ = container.Terminate(ctx) })
	}
	if err != nil {
		setupFatalfOrSkipIfDaemonDied(t, ctx, err, "postgres: container start")
	}

	adminDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	pool, err := db.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(pool.Close)

	applyMigrationsBeforeM111(t, pool, m111MigrationVersion)
	return pool
}

// applyMigrationsBeforeM111 walks the embedded FS in the same lexical order the
// boot-time runner uses and stops at stopAt. Kept file-local, matching the
// convention the other migration tests in this package state explicitly.
func applyMigrationsBeforeM111(t *testing.T, pool *db.Pool, stopAt string) {
	t.Helper()
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text        PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatalf("ensure schema_migrations: %v", err)
	}

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	var versions []string
	files := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		version := strings.TrimSuffix(name, ".sql")
		versions = append(versions, version)
		files[version] = name
	}
	sort.Strings(versions)

	found := false
	for _, version := range versions {
		if version == stopAt {
			found = true
			break
		}
		body, err := fs.ReadFile(migrations.FS, files[version])
		if err != nil {
			t.Fatalf("read migration %s: %v", version, err)
		}
		err = pgx.BeginFunc(ctx, pool.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version)
			return err
		})
		if err != nil {
			t.Fatalf("apply migration %s: %v", version, err)
		}
	}
	if !found {
		t.Fatalf("stopAt version %q not found among embedded migrations (renamed/removed?)", stopAt)
	}
}

func seedRawUser(t *testing.T, pool *db.Pool, email string, issuer *string, subject *string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email, oidc_issuer, oidc_subject, name)
		 VALUES ($1, $2, $3, 'seed') RETURNING id`, email, issuer, subject).Scan(&id)
	if err != nil {
		t.Fatalf("seed user %s: %v", email, err)
	}
	return id
}

func ptr(s string) *string { return &s }

func identitySubjectsFor(t *testing.T, pool *db.Pool, userID uuid.UUID) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT subject FROM user_identities WHERE user_id = $1 ORDER BY subject`, userID)
	if err != nil {
		t.Fatalf("query identities: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan subject: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// TestM111Migration_RepairsTheRollbackWindow is the regression test for the
// backfill gap: m110 could only see the rows that existed when it ran, and
// schema_migrations guarantees it never looks again.
func TestM111Migration_RepairsTheRollbackWindow(t *testing.T) {
	pool := startPostgresBeforeM111(t)
	ctx := context.Background()

	// m110 has run by this point, so anything seeded now is exactly what a
	// rollback to the previous release writes afterwards: legacy columns, no
	// identity row, and no email_verified_at because that release never wrote
	// one.
	rollback := seedRawUser(t, pool, "rollback@acme.com", ptr("https://idp.acme.com"), ptr("rollback-sub"))

	// Two users sharing a subject under different issuers, which the old
	// (oidc_issuer, oidc_subject) unique index permitted. Both are backfilled:
	// (provider, subject, issuer) tells them apart, which is exactly why issuer
	// stays in the key.
	shareA := seedRawUser(t, pool, "share-a@acme.com", ptr("https://idp-a.acme.com"), ptr("shared-sub"))
	shareB := seedRawUser(t, pool, "share-b@acme.com", ptr("https://idp-b.acme.com"), ptr("shared-sub"))

	// oidc_subject set with a NULL oidc_issuer. Deliberately NOT backfilled: the
	// pre-m110 sign-in matched on (oidc_issuer, oidc_subject), so a NULL issuer
	// never matched anything and this is not a sign-in anyone lost. Inventing an
	// issuer-less identity row would create a binding that never existed.
	nullIssuer := seedRawUser(t, pool, "nullissuer@acme.com", nil, ptr("null-issuer-sub"))

	// A password-only user must never acquire an identity row.
	passwordOnly := seedRawUser(t, pool, "pw@acme.com", nil, nil)

	for _, id := range []uuid.UUID{rollback, shareA, shareB, nullIssuer, passwordOnly} {
		if got := identitySubjectsFor(t, pool, id); len(got) != 0 {
			t.Fatalf("precondition: user %s should have no identity row before m111, got %v", id, got)
		}
	}

	// Finish the boot: applies m111.
	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("m111 must apply cleanly: %v", err)
	}

	assert := func(t *testing.T) {
		t.Helper()
		if got := identitySubjectsFor(t, pool, rollback); len(got) != 1 || got[0] != "rollback-sub" {
			t.Fatalf("a rollback-window identity must be backfilled, got %v", got)
		}
		for _, id := range []uuid.UUID{shareA, shareB} {
			if got := identitySubjectsFor(t, pool, id); len(got) != 1 || got[0] != "shared-sub" {
				t.Fatalf("a subject shared across issuers must be backfilled for both users, got %v", got)
			}
		}
		if got := identitySubjectsFor(t, pool, nullIssuer); len(got) != 0 {
			t.Fatalf("an issuer-less legacy row must not be given an identity, got %v", got)
		}
		if got := identitySubjectsFor(t, pool, passwordOnly); len(got) != 0 {
			t.Fatalf("a password-only user must never acquire an identity, got %v", got)
		}
	}
	assert(t)

	// Re-runnable: execute m111's own SQL text again, bypassing
	// schema_migrations, and assert nothing moves. A migration that auto-applies
	// on boot has to survive being re-applied.
	body, err := fs.ReadFile(migrations.FS, m111MigrationVersion+".sql")
	if err != nil {
		t.Fatalf("read m111 body: %v", err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("re-apply m111: %v", err)
	}
	assert(t)

	var total int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_identities`).Scan(&total); err != nil {
		t.Fatalf("total count: %v", err)
	}
	if total != 3 {
		t.Fatalf("total user_identities rows after re-apply = %d, want 3 (idempotent)", total)
	}
}

// The identity key still carries issuer after m111, which is what stops a
// subject minted by two different providers from resolving to one account.
func TestM111Migration_IdentityKeyStillIncludesIssuer(t *testing.T) {
	pool := startPostgresBeforeM111(t)
	ctx := context.Background()

	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	u1 := seedRawUser(t, pool, "one@acme.com", nil, nil)
	u2 := seedRawUser(t, pool, "two@acme.com", nil, nil)

	if _, err := pool.Exec(ctx,
		`INSERT INTO user_identities (user_id, provider, subject, issuer, email)
		 VALUES ($1, 'oidc', 'dup-sub', 'https://idp-a.acme.com', '')`, u1); err != nil {
		t.Fatalf("first identity: %v", err)
	}
	// Same subject, DIFFERENT issuer: two different people, and the key has to
	// be able to represent that.
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_identities (user_id, provider, subject, issuer, email)
		 VALUES ($1, 'oidc', 'dup-sub', 'https://idp-b.acme.com', '')`, u2); err != nil {
		t.Fatalf("a subject collision across issuers must be representable: %v", err)
	}
	// The same triple twice is still one identity.
	u3 := seedRawUser(t, pool, "three@acme.com", nil, nil)
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_identities (user_id, provider, subject, issuer, email)
		 VALUES ($1, 'oidc', 'dup-sub', 'https://idp-a.acme.com', '')`, u3); err == nil {
		t.Fatal("(provider, subject, issuer) must be unique")
	}
}
