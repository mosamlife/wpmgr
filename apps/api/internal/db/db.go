// Package db provides the pgx connection pool, an Atlas migration runner, and
// the RLS helper that scopes tenant-isolated work to a single transaction by
// setting the app.tenant_id GUC.
package db

import (
	"context"
	"fmt"
	"log/slog"
	"net"
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

// SessionCleanupTimeout bounds a session-scoped cleanup statement issued from a
// defer — pg_advisory_unlock above all — on a pinned pooled connection.
//
// Two failure modes, and the value has to sit between them:
//
//  1. Running the cleanup on the caller's own ctx is a leak. When ctx is already
//     cancelled (graceful shutdown mid-pass, a River job timeout, a client
//     disconnect) pgx returns immediately WITHOUT PUTTING THE STATEMENT ON THE
//     WIRE. The connection then goes back to the pool healthy, still holding a
//     SESSION-scoped advisory lock, and every later pass takes a different
//     connection, gets false from pg_try_advisory_lock, concludes a peer is
//     working and skips. Nothing errors; the work simply stops happening until
//     MaxConnLifetime (30 min, below) finally closes the connection.
//     context.WithoutCancel is what stops that.
//
//  2. WithoutCancel alone is unbounded. If the socket is wedged rather than the
//     context cancelled, an uncancellable Exec in a defer blocks shutdown for as
//     long as the kernel takes to give up. That case is also the case where the
//     unlock is pointless: a session that cannot be reached is a session that is
//     about to end, and its locks drop with it. So the bound costs nothing where
//     it fires.
//
// 5 s is the gap. A single unlock round-trip on an ALREADY-ESTABLISHED
// connection is <10 ms against Cloud SQL over the VPC connector (the same
// measurement BeforeAcquire's 500 ms ping budget is sized from), so 5 s is
// ~500× headroom for the healthy path, while staying inside the shutdown grace
// so a cleanup cannot be what makes a drain overrun.
const SessionCleanupTimeout = 5 * time.Second

// CleanupContext returns a context for undoing session-scoped state (an advisory
// unlock, a RESET) before a pinned connection returns to the pool. It detaches
// from ctx's cancellation and deadline while keeping its values, then bounds
// itself by SessionCleanupTimeout. See that constant for why it is both.
//
// The caller must defer the returned cancel, as with context.WithTimeout:
//
//	defer func() {
//		cctx, ccancel := db.CleanupContext(ctx)
//		defer ccancel()
//		_, _ = conn.Exec(cctx, `SELECT pg_advisory_unlock(...)`)
//	}()
func CleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), SessionCleanupTimeout)
}

// Connect opens a pgx connection pool and verifies connectivity. It applies no
// MinConns floor, making it suitable for short-lived pools such as the
// migration runner that are closed immediately after use.
//
// For the long-lived application pool, use ConnectApp instead.
func Connect(ctx context.Context, dsn string) (*Pool, error) {
	return connect(ctx, dsn, false)
}

// ConnectApp opens the long-lived application pool with a warm connection
// floor so that post-idle requests do not pay the full Cloud SQL TCP+TLS+auth
// round-trip cost on every cold start.
//
// Connection budget for Cloud SQL db-g1-small (max_connections ≈ 25,
// Cloud Run maxScale = 4 instances):
//
//	Per-instance cap : MaxConns = 5
//	Per-instance floor: MinConns = 2  (health-checked every 30 s)
//	Worst-case total  : 5 × 4 = 20 connections
//	Idle floor total  : 2 × 4 = 8 connections
//	System headroom   : ~5 remaining for psql/migrations/superuser sessions
//
// River (riverpgxv5.New(pool)) borrows from this same pool — it does NOT open
// its own pgxpool — so River's LISTEN + worker connections count against
// MaxConns per instance, not in addition to it. Periodic jobs and queue
// fetches are brief, so contention is rare; 5 MaxConns per instance
// comfortably handles River + concurrent HTTP handlers.
//
// Dead-connection hardening — why the idle ~8s stall happens and how we stop it:
//
//  1. TCP keepalive (DialFunc): the GCP VPC connector and Cloud SQL silently
//     drop idle TCP connections. Without keepalive, the OS never knows the peer
//     is gone; pgx holds a half-open connection that looks healthy (IsClosed()==
//     false) until the next write blocks until the OS retransmit timeout (~8 s).
//     Setting KeepAlive=30s on the dialer activates OS-level TCP keepalive probes
//     at 30-second intervals so the kernel detects a dead peer in one or two
//     intervals rather than the full retransmit sequence. ConnectTimeout=5s caps
//     the dial for a fresh connection.
//
//  2. BeforeAcquire validation: HealthCheckPeriod runs background pings only for
//     floor connections and only every 30 s. There is a race window: a connection
//     can die between checks and be handed to a request. BeforeAcquire issues a
//     ping with a 500 ms deadline before every Acquire(); if it fails pgx
//     discards the connection and immediately tries the next one (reconnecting if
//     needed). The round-trip on a healthy Cloud SQL connection over the VPC
//     connector is <10 ms, so the 500 ms budget is generous for healthy conns and
//     tight enough to fail a dead one in milliseconds rather than 8 seconds.
//
//  3. MaxConnIdleTime 90 s: the VPC connector's idle timeout is well under a few
//     minutes. Dropping above-floor connections after 90 s of idle prevents them
//     accumulating long enough to go stale in the first place.
//
//  4. MinConns=2 is kept: Cloud SQL VPC reconnect is sub-second, so dialling a
//     fresh connection is fast. The floor still avoids the post-boot dial cost on
//     the very first request, and the BeforeAcquire validation guarantees floor
//     connections are health-checked before every Acquire rather than relying on
//     the 30 s background sweep alone.
func ConnectApp(ctx context.Context, dsn string) (*Pool, error) {
	return connect(ctx, dsn, true)
}

// connect is the shared implementation; warmFloor=true applies the
// ConnectApp tuning.
func connect(ctx context.Context, dsn string, warmFloor bool) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	// TCP keepalive: activates OS-level keep probes so a dead peer (VPC-dropped
	// connection) is detected within one keepalive interval, not the multi-second
	// OS TCP retransmit timeout. Applied on every pool variant so the migration
	// pool also does not hang on a stale connection.
	cfg.ConnConfig.DialFunc = (&net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext

	if warmFloor {
		cfg.MinConns = 2
		cfg.MaxConns = 5
		// 90 s idle eviction: the GCP VPC connector drops idle TCP under a few
		// minutes. Evicting above-floor connections at 90 s prevents stale
		// accumulation without forcing a new dial on every request.
		cfg.MaxConnIdleTime = 90 * time.Second
		cfg.MaxConnLifetime = 30 * time.Minute
		cfg.HealthCheckPeriod = 30 * time.Second

		// BeforeAcquire: ping with a short deadline so a dead connection is
		// discarded before the caller's query runs on it. On a healthy Cloud SQL
		// connection the round-trip is <10 ms; 500 ms is generous for healthy
		// conns and tight enough to fast-fail dead ones. pgx retries Acquire()
		// with a fresh/dialled connection when BeforeAcquire returns false.
		cfg.BeforeAcquire = func(ctx context.Context, conn *pgx.Conn) bool {
			pingCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			defer cancel()
			return conn.Ping(pingCtx) == nil
		}
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

// ProbeTablePrivilege verifies that the connecting role has SELECT access to the
// core application tables by calling has_table_privilege(). This is a functional
// probe — it does NOT compare role names; it checks actual privilege state — so
// it cannot false-positive a correctly-configured instance.
//
// When the application connects as a role that was never granted wpmgr_app
// privileges (e.g. a self-hoster who set WPMGR_DB_USER to a plain login role
// but forgot to GRANT wpmgr_app to it), every table DML silently fails with
// "permission denied", which is extremely hard to diagnose. This probe fires
// exactly in that case and logs a clear, actionable message.
//
// The probe is skipped when the connecting role is a superuser (has_table_privilege
// always returns true for superusers, and EnforceRLSRole already gates that path
// behind AllowRLSBypassRole). The probe is always safe to run: if the role has
// the grant, the query returns true and boot continues; if not, we fail fast
// before serving any traffic.
func (p *Pool) ProbeTablePrivilege(ctx context.Context, logger *slog.Logger) error {
	var ok bool
	err := p.QueryRow(ctx,
		`SELECT has_table_privilege(current_user, 'public.tenants', 'SELECT')`,
	).Scan(&ok)
	if err != nil {
		// has_table_privilege can itself return an error if the table does not
		// exist yet (pre-migration). Treat this as a configuration problem:
		// migrations must run before the app connects with the app role.
		return fmt.Errorf(
			"privilege probe failed — the application DB role (%q) could not query 'public.tenants': %w; "+
				"ensure migrations have run (WPMGR_DB_MIGRATION_DSN) before starting the application",
			currentUserName(ctx, p), err,
		)
	}
	if ok {
		return nil
	}
	return fmt.Errorf(
		"the application database role (%q) lacks SELECT privilege on WPMgr tables; "+
			"the app must connect as the wpmgr_app role created by the migrations, "+
			"or as a role that has been granted wpmgr_app's privileges "+
			"(e.g. GRANT wpmgr_app TO %s). "+
			"Set WPMGR_DB_USER to the correct role. "+
			"See docs/install.md for the two-DSN setup.",
		currentUserName(ctx, p), currentUserName(ctx, p),
	)
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

// InInstallLockTx runs fn inside a single transaction that holds an
// install-wide advisory lock, taken as the transaction's FIRST statement.
//
// It exists for the one decision that is scoped to the whole install rather
// than to a tenant: whether this install has an owner yet. That question is
// read-then-write across three tables, and answering it in one transaction
// under one lock is what makes "exactly one first organisation" true rather
// than merely likely. pg_advisory_xact_lock is released by COMMIT or ROLLBACK,
// so the lock cannot leak however fn ends.
//
// The lock is keyed on (key, key) rather than (key, tenant) — the sibling
// per-tenant locks such as auth.MemberRolesLockKey and org.LifecycleLockKey
// cannot apply, because no tenant exists yet at the moment the decision is
// taken. There is exactly one such lock per install.
//
// fn is handed a scopeTenant callback because the tenant this transaction is
// deciding about does not exist when the transaction opens: fn creates it, then
// calls scopeTenant to bring app.tenant_id into scope for the RLS-protected
// writes that follow, all still inside this one transaction. GUC-setting stays
// in this file; no repo sets app.tenant_id itself.
func (p *Pool) InInstallLockTx(ctx context.Context, key string, fn func(tx pgx.Tx, scopeTenant func(uuid.UUID) error) error) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), hashtext($1))`, key); err != nil {
		return fmt.Errorf("install advisory lock %q: %w", key, err)
	}

	scopeTenant := func(tenantID uuid.UUID) error { return setTenant(ctx, tx, tenantID) }
	if err := fn(tx, scopeTenant); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
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

// InRumIngestLookupTx runs fn inside a transaction with the app.rum_lookup GUC
// set to 'on', enabling the site_perf_config_rum_lookup SELECT-only policy.
// This is the one place a beacon key hash may be resolved before its tenant is
// known: the public ingest endpoint presents only a hash, and this tx resolves
// it to (site_id, tenant_id, rum_enabled, rum_sample_rate). fn must do nothing
// but the beacon-key lookup.
func (p *Pool) InRumIngestLookupTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.rum_lookup', 'on', true)"); err != nil {
		return fmt.Errorf("set app.rum_lookup: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// InRumIngestTx runs fn inside a transaction with three GUCs set:
//   - app.rum_ingest = 'on'    — enables the rum_*_rum_ingest INSERT-only policies.
//   - app.tenant_id = tenantID — pins which tenant row the INSERT belongs to.
//   - app.site_id   = siteID   — pins which site row the INSERT belongs to.
//
// This is the anonymous browser write path: the beacon has already been
// validated (key resolved → tenantID+siteID known, rum_enabled=true, sampling
// applied) by the time this tx is opened. The RLS WITH CHECK clause on each
// rum_ingest policy asserts all three GUCs, so a bug that passes the wrong IDs
// will be caught by the DB rather than silently writing to the wrong tenant/site.
// fn must do nothing but write RUM rows.
func (p *Pool) InRumIngestTx(ctx context.Context, tenantID, siteID uuid.UUID, fn func(tx pgx.Tx) error) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.rum_ingest', 'on', true)"); err != nil {
		return fmt.Errorf("set app.rum_ingest: %w", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
		return fmt.Errorf("set app.tenant_id (rum ingest): %w", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.site_id', $1, true)", siteID.String()); err != nil {
		return fmt.Errorf("set app.site_id (rum ingest): %w", err)
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
