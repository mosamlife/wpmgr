// Package db provides the pgx connection pool, an Atlas migration runner, and
// the RLS helper that scopes tenant-isolated work to a single transaction by
// setting the app.tenant_id GUC.
package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool wraps a pgxpool and exposes tenant-scoped transaction helpers.
type Pool struct {
	*pgxpool.Pool
}

// Connect opens a pgx connection pool and verifies connectivity.
func Connect(ctx context.Context, dsn string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	return &Pool{Pool: pool}, nil
}

// Ping verifies the pool can reach the database (used by /readyz).
func (p *Pool) Ping(ctx context.Context) error {
	return p.Pool.Ping(ctx)
}

// EnforceRLSRole hard-fails startup when the application's connecting role is a
// superuser or has BYPASSRLS, because either silently voids Row-Level Security:
// the tenant_id WHERE filter would become the ONLY isolation. The app must
// connect as a dedicated NOSUPERUSER NOBYPASSRLS role.
//
// allowBypass is the explicit escape hatch (WPMGR_ALLOW_RLS_BYPASS_ROLE=true)
// for single-node dev that shares the bootstrap superuser; when set, a bypassing
// role is downgraded from a boot failure to a loud warning. It must never be
// enabled in production.
func (p *Pool) EnforceRLSRole(ctx context.Context, logger *slog.Logger, allowBypass bool) error {
	var super, bypass bool
	err := p.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`,
	).Scan(&super, &bypass)
	if err != nil {
		return fmt.Errorf("verify DB role RLS posture: %w", err)
	}
	if !super && !bypass {
		return nil
	}
	if allowBypass {
		logger.Warn("RLS BYPASS ESCAPE HATCH ENABLED: application DB role bypasses Row-Level Security and tenant isolation is NOT enforced by RLS — this is permitted ONLY because WPMGR_ALLOW_RLS_BYPASS_ROLE=true; never use this in production",
			slog.Bool("rolsuper", super), slog.Bool("rolbypassrls", bypass))
		return nil
	}
	return fmt.Errorf("application DB role %q bypasses Row-Level Security (rolsuper=%t rolbypassrls=%t): connect as a NOSUPERUSER NOBYPASSRLS role (e.g. wpmgr_app), or set WPMGR_ALLOW_RLS_BYPASS_ROLE=true for single-node dev",
		currentUserName(ctx, p), super, bypass)
}

func currentUserName(ctx context.Context, p *Pool) string {
	var name string
	_ = p.QueryRow(ctx, "SELECT current_user").Scan(&name)
	return name
}

// InTenantTx runs fn inside a transaction with app.tenant_id set to the given
// tenant for the lifetime of the transaction (SET LOCAL). This is how RLS is
// enforced: every tenant-scoped query executed via the supplied tx is filtered
// by the sites_tenant_isolation policy against this exact value, so a query
// that forgets its tenant filter still cannot see other tenants' rows.
//
// The transaction commits if fn returns nil, and rolls back otherwise.
func (p *Pool) InTenantTx(ctx context.Context, tenantID uuid.UUID, fn func(tx pgx.Tx) error) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := setTenant(ctx, tx, tenantID); err != nil {
		return err
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// setTenant sets the app.tenant_id GUC for the current transaction. We use
// set_config(..., true) so the setting is scoped to the transaction (LOCAL)
// and parameterized to avoid any SQL injection via the tenant value.
func setTenant(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
		return fmt.Errorf("set app.tenant_id: %w", err)
	}
	return nil
}

// InTenantTxAsUser runs fn inside a transaction with BOTH app.tenant_id and
// app.user_id set. The user GUC enables the memberships_self_read policy in
// addition to the per-tenant isolation policy — used where a tenant-scoped
// operation also needs to read the acting user's own membership rows.
func (p *Pool) InTenantTxAsUser(ctx context.Context, tenantID, userID uuid.UUID, fn func(tx pgx.Tx) error) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := setTenant(ctx, tx, tenantID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.user_id', $1, true)", userID.String()); err != nil {
		return fmt.Errorf("set app.user_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// InUserTx runs fn inside a transaction with app.user_id set (and no tenant
// GUC), enabling the memberships_self_read policy so a principal can enumerate
// its own memberships across every tenant. Used by /auth/me and tenant
// switching, before any active tenant is known.
func (p *Pool) InUserTx(ctx context.Context, userID uuid.UUID, fn func(tx pgx.Tx) error) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.user_id', $1, true)", userID.String()); err != nil {
		return fmt.Errorf("set app.user_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// InAPIKeyLookupTx runs fn inside a transaction with the app.apikey_lookup GUC
// set to 'on', enabling the api_keys_prefix_lookup SELECT-only policy. This is
// the one place a key may be read before its tenant is known; fn must do
// nothing but resolve a key by its (unique) prefix.
func (p *Pool) InAPIKeyLookupTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.apikey_lookup', 'on', true)"); err != nil {
		return fmt.Errorf("set app.apikey_lookup: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}
