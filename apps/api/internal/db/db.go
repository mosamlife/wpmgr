// Package db provides the pgx connection pool, an Atlas migration runner, and
// the RLS helper that scopes tenant-isolated work to a single transaction by
// setting the app.tenant_id GUC.
package db

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
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

// InEnrollTx runs fn inside a transaction with the app.enroll GUC set to 'on',
// enabling the sites_enroll and pairing_codes_enroll policies. This is the one
// place pairing codes and sites are resolved/created/attached BEFORE a tenant
// scope exists: the /enroll endpoint is public (the agent has only a code), so
// the code's hash is the bootstrap. fn must do nothing but enrollment work.
func (p *Pool) InEnrollTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.enroll', 'on', true)"); err != nil {
		return fmt.Errorf("set app.enroll: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// InAgentTx runs fn inside a transaction with the app.agent GUC set to 'on',
// enabling the sites_agent and agent_nonces_agent policies. An authenticated
// agent->CP request resolves its identity (the site) by the stored agent public
// key, which precedes any tenant scope; the cross-tenant health job also uses
// this scope. fn must confine itself to agent-path work.
func (p *Pool) InAgentTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.agent', 'on', true)"); err != nil {
		return fmt.Errorf("set app.agent: %w", err)
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

// InScopedTenantTx runs fn inside a transaction with four GUCs set:
//   - app.tenant_id  — the active org (same as InTenantTx)
//   - app.user_id    — the acting user (same as InTenantTxAsUser)
//   - app.allowed_site_ids — comma-joined UUIDs from the caller's site_shares
//   - app.site_scope = 'on' — activates the RESTRICTIVE <t>_site_scope policies
//
// This is the correct tx wrapper for site-scoped principals (Scope == "site").
// The restrictive RLS policies on all 21 direct tables (and the 2 indirect
// children) evaluate:
//
//	coalesce(current_setting('app.site_scope',true),'') <> 'on'
//	OR site_id = ANY(string_to_array(current_setting('app.allowed_site_ids',true),',')::uuid[])
//
// When site_scope is 'on' that clause becomes a real filter; when it is ” or
// unset (normal member paths) it is a tautology. ALL GUCs are set with
// set_config(name, val, true) — the third arg is the "is_local" flag, which
// restricts the setting to the current transaction (equivalent to SET LOCAL)
// and is safe with pgBouncer transaction-mode pooling.
func (p *Pool) InScopedTenantTx(ctx context.Context, tenantID, userID uuid.UUID, allowedSiteIDs []uuid.UUID, fn func(tx pgx.Tx) error) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// app.tenant_id
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
		return fmt.Errorf("set app.tenant_id: %w", err)
	}
	// app.user_id
	if _, err := tx.Exec(ctx, "SELECT set_config('app.user_id', $1, true)", userID.String()); err != nil {
		return fmt.Errorf("set app.user_id: %w", err)
	}
	// app.allowed_site_ids — comma-joined UUID strings; empty string is safe
	// (string_to_array('', ',') returns {''} which matches nothing)
	siteIDStrs := make([]string, len(allowedSiteIDs))
	for i, id := range allowedSiteIDs {
		siteIDStrs[i] = id.String()
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.allowed_site_ids', $1, true)", strings.Join(siteIDStrs, ",")); err != nil {
		return fmt.Errorf("set app.allowed_site_ids: %w", err)
	}
	// app.site_scope — activates the RESTRICTIVE policies
	if _, err := tx.Exec(ctx, "SELECT set_config('app.site_scope', 'on', true)"); err != nil {
		return fmt.Errorf("set app.site_scope: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// InInviteLookupTx runs fn inside a transaction with app.invite_lookup set to
// 'on', enabling the invitations_token_lookup SELECT-only policy. This mirrors
// InAPIKeyLookupTx: it is the one place an invitation may be read before any
// authenticated session or tenant scope is established (the public accept
// endpoint). fn must do nothing but resolve an invitation by its token hash.
func (p *Pool) InInviteLookupTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.invite_lookup', 'on', true)"); err != nil {
		return fmt.Errorf("set app.invite_lookup: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// ScopedPrincipal is the interface RunTenantTx requires from a principal. It
// is implemented by domain.Principal and any test double that carries the four
// scope fields. Using an interface here avoids a circular import between db and
// domain (domain cannot import db; db cannot import domain).
type ScopedPrincipal interface {
	GetScope() string
	GetUserID() uuid.UUID
	GetTenantID() uuid.UUID
	GetAllowedSiteIDs() []uuid.UUID
}

// RunTenantTx is the central dispatch helper that chooses the correct
// transaction wrapper based on the principal's Scope. Repos and services
// MUST use this instead of calling InTenantTx / InTenantTxAsUser /
// InScopedTenantTx directly, so that a forgotten call-site cannot silently
// bypass the site-scope RLS.
//
// Dispatch rules:
//   - Scope == "site": InScopedTenantTx with p.AllowedSiteIDs
//   - Scope == "org" (or empty, for backward compat): InTenantTxAsUser when
//     UserID is non-nil, InTenantTx otherwise (API-key principals have no
//     UserID; they don't need the memberships_self_read policy)
//
// The fn receives the raw pgx.Tx. Callers wrap it with sqlc.New(tx) as usual.
func (p *Pool) RunTenantTx(ctx context.Context, principal ScopedPrincipal, fn func(tx pgx.Tx) error) error {
	scope := principal.GetScope()
	tenantID := principal.GetTenantID()
	userID := principal.GetUserID()

	if scope == "site" {
		return p.InScopedTenantTx(ctx, tenantID, userID, principal.GetAllowedSiteIDs(), fn)
	}
	// "org" or "" (backward compat for existing flows that never set Scope)
	if userID != uuid.Nil {
		return p.InTenantTxAsUser(ctx, tenantID, userID, fn)
	}
	return p.InTenantTx(ctx, tenantID, fn)
}
