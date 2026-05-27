// Package db provides the pgx connection pool, an Atlas migration runner, and
// the RLS helper that scopes tenant-isolated work to a single transaction by
// setting the app.tenant_id GUC.
package db

import (
	"context"
	"fmt"
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
