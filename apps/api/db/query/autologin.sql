-- ============================================================================
-- autologin (Phase 5.5 — One-Click Login) sqlc queries.
--
-- The MINT path runs tenant-scoped (app.tenant_id) so RLS isolates the
-- INSERT. The CONSUME path runs cross-tenant under app.agent (the agent
-- presents the verified site_id + nonce — no tenant context yet); the
-- autologin_tokens_agent / autologin_tokens_agent_consume policies cover it.
-- The same policy split applies to autologin_policies (tenant-scoped read/
-- upsert + cross-tenant SELECT under app.agent for the agent's roles lookup).
-- ============================================================================

-- name: InsertAutologinToken :one
-- Mint path (app.tenant_id). The id is the JWT jti (base64url 32B random).
INSERT INTO autologin_tokens (
    id, tenant_id, site_id, initiator_user_id, target_wp_user_login,
    initiator_ip, initiator_user_agent, expires_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ConsumeAutologinToken :one
-- Consume path (app.agent). Atomic single-shot UPDATE that wins exactly once
-- across concurrent agent callbacks: the (consumed_at IS NULL) predicate makes
-- the second consume a 0-row update; RETURNING tells the caller it lost. The
-- site_id check binds the consume to the agent's verified identity (anti
-- cross-tenant replay).
UPDATE autologin_tokens
SET consumed_at = now(),
    consumed_from_ip = $2
WHERE id = $1
  AND site_id = $3
  AND consumed_at IS NULL
  AND expires_at > now()
RETURNING id, tenant_id, site_id, initiator_user_id, target_wp_user_login;

-- name: MarkAutologinTokenConsumed :execrows
-- Used on the Redis-hot-path success: Redis already produced the payload, but
-- we still UPDATE the PG row so the audit/observability story is complete.
-- Idempotent: returns 0 if another path already marked it (safe).
UPDATE autologin_tokens
SET consumed_at = COALESCE(consumed_at, now()),
    consumed_from_ip = COALESCE(consumed_from_ip, $2)
WHERE id = $1
  AND site_id = $3;

-- name: GetAutologinPolicy :one
-- Mint path (app.tenant_id). When no row exists the service auto-creates the
-- default policy on first read (see UpsertAutologinPolicyDefault).
SELECT * FROM autologin_policies
WHERE site_id = $1 AND tenant_id = $2;

-- name: UpsertAutologinPolicyDefault :one
-- Mint path (app.tenant_id). Idempotent insert-or-return: the first call seeds
-- the default policy (enabled, allowed=administrator, 2FA off, max age 30m);
-- subsequent calls return the existing row unchanged.
INSERT INTO autologin_policies (site_id, tenant_id)
VALUES ($1, $2)
ON CONFLICT (site_id) DO UPDATE SET updated_at = autologin_policies.updated_at
RETURNING *;

-- name: GetAutologinPolicyForAgent :one
-- Consume path (app.agent). Cross-tenant SELECT-only read of the policy so the
-- agent learns which WP roles it may log in as. NULL row -> defaults applied.
SELECT * FROM autologin_policies
WHERE site_id = $1;
