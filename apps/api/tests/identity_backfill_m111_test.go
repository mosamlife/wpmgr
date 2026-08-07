// identity_backfill_m111_test.go: m110's backfill was not exhaustive, and a
// backfill only ever runs once.
//
// Boots a container, applies every embedded migration UP TO (not including)
// m111, seeds the users rows a live install would already have (including the
// two groups m110 skipped and the row a rollback window leaves behind), then
// finishes the boot so m111 runs, and asserts every identity that can be
// resolved unambiguously now has a user_identities row. Also proves m111 is
// re-runnable and that it re-keys the unique index off issuer.
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

// m111MigrationVersion is the embedded migration that drops issuer from the
// identity key and re-runs m110's backfill.
const m111MigrationVersion = "20260813000000_m111_identity_key_drops_issuer"

// startPostgresBeforeM111 boots a container and applies everything up to but
// not including m111, using the bootstrap superuser connection directly: this
// test is about backfill correctness, and user_identities carries no RLS by
// design (a user spans tenants).
func startPostgresBeforeM111(t *testing.T) *db.Pool {
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

// TestM111Migration_ReBackfillsWhatM110Skipped is the regression test for the
// backfill gap. Each seeded user is one of the shapes m110 left without an
// identity row.
func TestM111Migration_ReBackfillsWhatM110Skipped(t *testing.T) {
	pool := startPostgresBeforeM111(t)
	ctx := context.Background()

	// m110 has run by this point, so anything seeded now is exactly what a
	// rollback to the previous release writes afterwards: legacy columns, no
	// identity row, and no email_verified_at because that release never wrote
	// one.
	rollback := seedRawUser(t, pool, "rollback@acme.com", ptr("https://idp.acme.com"), ptr("rollback-sub"))

	// The other group m110 skipped outright: oidc_subject set, oidc_issuer NULL.
	// m110's SELECT already COALESCEd the NULL, so the WHERE clause requiring it
	// non-null did nothing except drop these rows on the floor.
	nullIssuer := seedRawUser(t, pool, "nullissuer@acme.com", nil, ptr("null-issuer-sub"))

	// Two users sharing a subject under different issuers, which the old
	// (oidc_issuer, oidc_subject) unique index permitted. Neither may be
	// backfilled: once issuer leaves the key the subject identifies neither of
	// them, and picking one hands somebody the other's account.
	ambigA := seedRawUser(t, pool, "ambig-a@acme.com", ptr("https://idp-a.acme.com"), ptr("shared-sub"))
	ambigB := seedRawUser(t, pool, "ambig-b@acme.com", ptr("https://idp-b.acme.com"), ptr("shared-sub"))

	// A password-only user must never acquire an identity row.
	passwordOnly := seedRawUser(t, pool, "pw@acme.com", nil, nil)

	for _, id := range []uuid.UUID{rollback, nullIssuer, ambigA, ambigB, passwordOnly} {
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
		if got := identitySubjectsFor(t, pool, nullIssuer); len(got) != 1 || got[0] != "null-issuer-sub" {
			t.Fatalf("a NULL-issuer identity must be backfilled, got %v", got)
		}
		if got := identitySubjectsFor(t, pool, ambigA); len(got) != 0 {
			t.Fatalf("an ambiguous subject must NOT be bound to either user, got %v for A", got)
		}
		if got := identitySubjectsFor(t, pool, ambigB); len(got) != 0 {
			t.Fatalf("an ambiguous subject must NOT be bound to either user, got %v for B", got)
		}
		if got := identitySubjectsFor(t, pool, passwordOnly); len(got) != 0 {
			t.Fatalf("a password-only user must never acquire an identity, got %v", got)
		}

		// The NULL issuer is recorded as '' rather than propagating NULL into a
		// NOT NULL column.
		var issuer string
		if err := pool.QueryRow(ctx,
			`SELECT issuer FROM user_identities WHERE user_id = $1`, nullIssuer).Scan(&issuer); err != nil {
			t.Fatalf("read backfilled issuer: %v", err)
		}
		if issuer != "" {
			t.Fatalf("backfilled issuer = %q, want '' for a NULL oidc_issuer", issuer)
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
	if total != 2 {
		t.Fatalf("total user_identities rows after re-apply = %d, want 2 (idempotent)", total)
	}
}

// TestM111Migration_IdentityKeyNoLongerIncludesIssuer proves the re-key landed:
// the same (provider, subject) under a second issuer is now rejected by the
// unique index, which is what stops an issuer edit from stranding a row.
func TestM111Migration_IdentityKeyNoLongerIncludesIssuer(t *testing.T) {
	pool := startPostgresBeforeM111(t)
	ctx := context.Background()

	// Before m111 the key still carries issuer, so two rows are accepted. This
	// is the state m111 has to be able to clean up.
	u1 := seedRawUser(t, pool, "one@acme.com", nil, nil)
	u2 := seedRawUser(t, pool, "two@acme.com", nil, nil)
	for _, x := range []struct {
		user   uuid.UUID
		issuer string
	}{{u1, "https://idp.acme.com"}, {u2, "https://login.acme.com"}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO user_identities (user_id, provider, subject, issuer, email, last_login_at)
			 VALUES ($1, 'oidc', 'dup-sub', $2, '', now())`, x.user, x.issuer); err != nil {
			t.Fatalf("precondition insert: %v", err)
		}
	}

	// m111 must dedupe rather than fail. A CREATE UNIQUE INDEX that errors
	// inside the boot-time migration transaction is a crash loop.
	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("m111 must apply even with a pre-existing duplicate subject: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM user_identities WHERE subject = 'dup-sub'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected the duplicate to be resolved to 1 row, got %d", n)
	}

	// The old index is gone and the new one holds.
	var oldIdx int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'user_identities_provider_subject_key'`).Scan(&oldIdx); err != nil {
		t.Fatalf("index lookup: %v", err)
	}
	if oldIdx != 0 {
		t.Fatal("the issuer-bearing unique index must be dropped")
	}

	survivor := u1
	if err := pool.QueryRow(ctx,
		`SELECT user_id FROM user_identities WHERE subject = 'dup-sub'`).Scan(&survivor); err != nil {
		t.Fatalf("read survivor: %v", err)
	}
	other := u1
	if survivor == u1 {
		other = u2
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_identities (user_id, provider, subject, issuer, email)
		 VALUES ($1, 'oidc', 'dup-sub', 'https://third.acme.com', '')`, other); err != nil {
		return // rejected, which is the pass
	}
	t.Fatal("(provider, subject) must be unique regardless of issuer after m111")
}
