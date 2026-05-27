# API

The WPMgr control-plane API is **contract-first**. The OpenAPI 3.1 spec at
[`packages/openapi/openapi.yaml`](../packages/openapi/openapi.yaml) is the single
source of truth for both the Go server and the TypeScript client.

- **Base URL (dev):** `http://localhost:8080`
- **REST API:** under `/api/v1`
- **Health:** `GET /healthz` (liveness), `GET /readyz` (readiness)
- **Agent protocol:** namespace `wpmgr/v1`, Ed25519-signed — see [agent.md](./agent.md)

## Codegen

```bash
make gen   # regenerates Go server types (ogen, ADR-004) + the TS client
```

- Go server types/router are generated with **ogen** and mounted on a Gin route
  group; Gin stays the outer HTTP layer (ADR-004). See
  [architecture.md](./architecture.md).
- The TypeScript client (`packages/openapi-client`) is generated from the same
  spec. Never hand-edit generated code; change `openapi.yaml` and re-run
  `make gen`.

## Health

```bash
curl -s localhost:8080/healthz
```

```json
{ "status": "ok", "version": "0.0.0" }
```

`/readyz` returns `200` (or `503` when degraded) with per-dependency checks:

```bash
curl -s localhost:8080/readyz
```

```json
{
  "status": "ok",
  "checks": { "postgres": "ok", "redis": "ok", "s3": "ok" }
}
```

## Tenants & sites (V0 CRUD)

> **V0 skeleton.** Tenants and sites CRUD are defined in `openapi.yaml`.
> Authentication (sessions/OIDC) and the live enrollment exchange arrive in
> Phase 5 / M1–M2 — the examples below are unauthenticated as in the current
> skeleton. List responses are paginated via `limit` (default 50, max 200) and
> `offset`.

List sites for the current tenant:

```bash
curl -s localhost:8080/api/v1/sites
```

```json
{
  "items": [
    {
      "id": "9f1c0b6e-2a4d-4d2a-8e2c-1f0a3b5c7d9e",
      "tenant_id": "1b2c3d4e-5f60-4718-9a0b-1c2d3e4f5a6b",
      "url": "https://example.com",
      "name": "example.com",
      "status": "active",
      "wp_version": "6.9.1",
      "php_version": "8.3.0",
      "created_at": "2026-05-27T10:00:00Z",
      "updated_at": "2026-05-27T10:00:00Z"
    }
  ]
}
```

Create a site (`status` defaults to `pending`):

```bash
curl -s -X POST localhost:8080/api/v1/sites \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com","name":"example.com"}'
```

```json
{
  "id": "9f1c0b6e-2a4d-4d2a-8e2c-1f0a3b5c7d9e",
  "tenant_id": "1b2c3d4e-5f60-4718-9a0b-1c2d3e4f5a6b",
  "url": "https://example.com",
  "name": "example.com",
  "status": "pending",
  "wp_version": "",
  "php_version": "",
  "created_at": "2026-05-27T10:00:00Z",
  "updated_at": "2026-05-27T10:00:00Z"
}
```

Other endpoints in the spec: `GET/POST /api/v1/tenants`,
`GET /api/v1/tenants/{tenantId}`, `GET /api/v1/sites/{siteId}`,
`DELETE /api/v1/sites/{siteId}`. Errors use a stable shape:

```json
{ "code": "site_url_exists", "message": "Site URL already exists for this tenant" }
```

A `pending` site is completed by pairing the WordPress agent — see
[agent.md](./agent.md) and the enrollment sequence in
[architecture.md](./architecture.md#agent-enrollment).

> Feature endpoints (backups, updates, monitoring, scans) are Roadmap —
> see [PLAN.md](../PLAN.md).
