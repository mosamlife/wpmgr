# apps/api — WPMgr control plane (Go + Gin)

Modular monolith. Domains live in `internal/<domain>/` (model, repo, service,
handler in one package). Binary entrypoint: `cmd/wpmgr/main.go`. Admin tooling
(migrations): `cmd/wpmgr-cli/main.go`.

```bash
go build ./...
go vet ./...
go test ./...   # includes a testcontainers Postgres RLS integration test (needs Docker)
```

## Layout

| Path | Purpose |
|------|---------|
| `internal/config` | koanf loader → typed `Config` (defaults < file < `WPMGR_*` env). |
| `internal/db` | pgxpool connection, embedded-migration runner, `InTenantTx` RLS helper. |
| `internal/db/sqlc` | sqlc-generated, type-safe query layer (pgx/v5). |
| `internal/telemetry` | OTel tracer provider + graceful shutdown (no-op unless OTLP endpoint set). |
| `internal/middleware` | request-id, slog logging, panic recovery, tenant extraction. |
| `internal/domain` | shared typed errors + HTTP mapping, `Clock`, validator wrapper, tenant context. |
| `internal/server` | Gin engine, route groups, `/healthz` + `/readyz`, graceful shutdown. |
| `internal/server/httpx` | central error → HTTP `Error` response mapping. |
| `internal/tenant` | tenant domain (CRUD; not RLS-scoped — it's the scoping key). |
| `internal/site` | site domain (CRUD under `/api/v1/sites`, tenant-scoped + RLS). |
| `internal/api/gen` | ogen-generated OpenAPI types/validation/client (contract types). |
| `db/schema.sql` | single source of truth consumed by BOTH sqlc and Atlas. |
| `db/query/*.sql` | sqlc query definitions. |
| `migrations/` | Atlas versioned migrations (also embedded for startup apply). |

## Multi-tenancy & Row-Level Security

The `sites` table has Postgres RLS `ENABLE`d and `FORCE`d with a policy keyed on
the `app.tenant_id` runtime setting. `db.Pool.InTenantTx` opens a transaction,
runs `set_config('app.tenant_id', <tenant>, true)` (transaction-local), and runs
all tenant-scoped queries within it. Even a query that forgets its `tenant_id`
filter cannot read or write another tenant's rows.

**The application must connect as a non-superuser, non-`BYPASSRLS` role.**
Postgres superusers (and `BYPASSRLS` roles) ignore RLS entirely.

### Two-DSN model (M1)

There are two connection strings:

| Setting | Role | Used for |
|---------|------|----------|
| `WPMGR_DB_*` (→ `DBConfig.DSN()`) | `wpmgr_app` (NOSUPERUSER NOBYPASSRLS) | the running application |
| `WPMGR_DB_MIGRATION_DSN` (→ `MigrateDSN()`) | owner/superuser | migrations only |

`MigrateDSN()` falls back to the app DSN when `WPMGR_DB_MIGRATION_DSN` is unset
(single-DSN dev). Migrations run privileged DDL and **create the `wpmgr_app`
role** (`NOLOGIN NOSUPERUSER NOBYPASSRLS`, idempotent) and grant it the table
privileges + `ALTER DEFAULT PRIVILEGES`. Deployments/infra provision the role's
**LOGIN + password** out of band (the migration deliberately does not hard-code a
password):

```sql
-- infra step, once, after migrations have created the NOLOGIN role:
ALTER ROLE wpmgr_app LOGIN PASSWORD '<from-secrets-manager>';
```

### Hard-fail on RLS-bypassing app role

At startup the app calls `Pool.EnforceRLSRole`: if the connecting role is a
superuser or has `BYPASSRLS`, the server **refuses to boot**. The single escape
hatch is `WPMGR_ALLOW_RLS_BYPASS_ROLE=true` (default `false`), intended only for
single-node dev sharing the bootstrap superuser; when set, the bypass is
downgraded to a *loud* warning. Never enable it in production.

The RLS integration tests (`tests/*_integration_test.go`) connect as the
`wpmgr_app` role and prove cross-tenant SELECT on `sites`, `memberships`,
`api_keys`, and `audit_log` returns zero rows.

### Auth, sessions, RBAC, API keys, audit (M1)

- **Auth**: argon2id password hashing (OWASP 19 MiB/t=2/p=1). `POST /auth/login`,
  `POST /auth/logout`, `GET /auth/me`, `POST /auth/register` (first-run bootstrap
  creates the first user + tenant + owner membership; afterwards registration is
  closed and members are added via `POST /api/v1/members`). OIDC relying party
  (coreos/go-oidc v3 + x/oauth2, PKCE + state + nonce) at `GET /auth/oidc/login`
  and `GET /auth/oidc/callback`; disabled cleanly (501) when `WPMGR_OIDC_ISSUER`
  is unset.
- **Sessions**: SCS v2 with a Redis store (redigo pool, `WPMGR_REDIS_ADDR`),
  opaque `wpmgr_session` cookie (HttpOnly, SameSite=Lax, Secure in prod), idle +
  absolute lifetimes. The session holds `user_id` + `active_tenant_id`. The
  server refuses to boot if `WPMGR_SESSION_SECRET` is empty, a `change-me*`
  placeholder, or shorter than 32 bytes.
- **Tenant context**: the old `X-Tenant-ID` *trust* is gone. The active tenant is
  derived from the authenticated principal (session's `active_tenant_id`, or the
  tenant an API key is bound to) and the user's membership is re-verified.
  A session caller may still pass `X-Tenant-ID` to *select* one of their own
  tenants, but membership is always checked — the header alone grants nothing.
- **RBAC**: roles `owner > admin > operator > viewer`. Middleware
  `authz.RequireRole(min)` / `authz.RequirePermission(perm)`. Matrix:

  | Permission | Min role |
  |------------|----------|
  | site:read, member:read, tenant/site reads | viewer |
  | site:write (create/delete site) | operator |
  | member:manage, apikey:read, apikey:manage, audit:read | admin |
  | tenant:manage (create tenant) | owner |

- **API keys**: tenant-scoped. Token format `wpmgr_<prefix>_<secret>`; only a
  sha256 hash + the prefix are stored, and the full token is returned once at
  creation. Auth middleware accepts either a session cookie OR
  `Authorization: Bearer <key>`. `POST/GET/DELETE /api/v1/api-keys` (admin+).
- **Audit log**: append-only, per-tenant hash-chained (`hash = sha256(prev_hash,
  tenant, actor, action, target, metadata, created_at)`). `UPDATE`/`DELETE` are
  revoked from `wpmgr_app`, so the table is insert-only at the privilege level.
  `GET /api/v1/audit` (paginated) and `GET /api/v1/audit/verify` (admin+).

## Configuration

Config is loaded with koanf: built-in defaults < optional YAML file
(`WPMGR_CONFIG_FILE`) < `WPMGR_*` environment variables. See repo
`.env.example` for variable names. Key vars: `WPMGR_HTTP_ADDR`,
`WPMGR_LOG_LEVEL`, `WPMGR_ENV`, `WPMGR_DB_*`, `WPMGR_OTEL_EXPORTER_OTLP_ENDPOINT`,
`WPMGR_OTEL_SERVICE_NAME`.

## Code generation (`make gen` targets)

Tools are pinned in the `go.mod` `tool` block, so `go tool <name>` works without
a separate install. The same commands are also exposed via `//go:generate`.

### sqlc — query layer

```bash
go tool sqlc -f sqlc.yaml generate
# or: go generate ./internal/db/sqlc
```

Reads `db/schema.sql` + `db/query/*.sql` → `internal/db/sqlc/`.

### ogen — OpenAPI types (Go)

```bash
go tool ogen --target internal/api/gen --package gen --clean ../../packages/openapi/openapi.yaml
# or: go generate ./internal/api/gen
```

Reads `packages/openapi/openapi.yaml` → `internal/api/gen/`. ogen generates a
full typed server/client; we consume the models + validation and wire handlers
under Gin route groups (ogen's own router is not mounted — see ADR-004).

### Atlas — versioned migrations

The schema source of truth is `db/schema.sql`. Atlas Community Edition cannot
*diff* RLS policies without login (open-core limitation, ADR-002 risk #2), so
the table/index diff is generated by Atlas and the RLS statements are appended
to the generated migration by hand, then the dir is re-hashed.

```bash
# A throwaway dev Postgres is required for diffing:
export ATLAS_DEV_URL="postgres://USER:PASS@localhost:5432/dev?sslmode=disable"

# 1. Diff schema (tables/indexes) into a new versioned migration:
atlas migrate diff <name> --env local           # uses atlas.hcl

# 2. Append the RLS ALTER/POLICY statements to the new migration file,
#    then re-hash the directory:
atlas migrate hash --dir "file://migrations"

# Apply migrations (CLI path; the server also applies them on startup):
atlas migrate apply --dir "file://migrations" --url "$DATABASE_URL"
```

The initial migration (`migrations/20260527115454_initial.sql`) was generated
this way: tables/indexes via `atlas migrate diff initial`, RLS appended, then
`atlas migrate hash`.

Migrations are embedded (`migrations/migrations.go`) and applied automatically
on server startup (`db.Pool.Migrate`); `wpmgr-cli migrate` applies them too.

## Endpoints

- `GET /healthz` — liveness.
- `GET /readyz` — readiness (pings the DB; 503 if unreachable).
- `POST /auth/register`, `POST /auth/login`, `POST /auth/logout`, `GET /auth/me`,
  `GET /auth/oidc/login`, `GET /auth/oidc/callback`.
- `POST /api/v1/tenants`, `GET /api/v1/tenants`, `GET /api/v1/tenants/{tenantId}`.
- `POST /api/v1/sites`, `GET /api/v1/sites`, `GET /api/v1/sites/{siteId}`,
  `DELETE /api/v1/sites/{siteId}` — active tenant + role come from the
  authenticated principal.
- `GET /api/v1/members`, `POST /api/v1/members` (admin+).
- `GET /api/v1/api-keys`, `POST /api/v1/api-keys`, `DELETE /api/v1/api-keys/{apiKeyId}` (admin+).
- `GET /api/v1/audit`, `GET /api/v1/audit/verify` (admin+).

### New M1 env vars

`WPMGR_DB_MIGRATION_DSN` (optional owner DSN for migrations; falls back to the
app DSN), `WPMGR_ALLOW_RLS_BYPASS_ROLE` (dev escape hatch, default false),
`WPMGR_SESSION_SECRET` (≥32 bytes, no `change-me*`), `WPMGR_REDIS_ADDR`,
`WPMGR_REDIS_PASSWORD`, `WPMGR_OIDC_ISSUER`/`_CLIENT_ID`/`_CLIENT_SECRET`/`_REDIRECT_URL`,
and optional `WPMGR_AUTH_IDLE_TIMEOUT` / `WPMGR_AUTH_ABSOLUTE_EXPIRY`.
