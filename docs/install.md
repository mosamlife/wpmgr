# Install (self-host)

Self-host WPMgr with Docker Compose. The stack runs the control plane (Go),
dashboard (React), and data plane (Postgres, Redis, SeaweedFS, ClickHouse).

> **V0 skeleton.** This brings up the control plane with health endpoints and
> the tenants + sites CRUD API. Feature milestones (backups, updates, scans) are
> Roadmap — see [PLAN.md](../PLAN.md).

## Prerequisites

- **Docker** 24+ with the Compose plugin (`docker compose`, not `docker-compose`)
- ~2 GB free RAM for the full stack

## 1. Configure env

```bash
cp .env.example .env
```

Edit `.env`. At minimum change these before exposing the service:

```bash
WPMGR_SESSION_SECRET=...      # 32-byte base64; openssl rand -base64 32
WPMGR_DB_PASSWORD=...         # not the default
WPMGR_S3_SECRET_KEY=...       # not the default
```

Generate the Ed25519 control-plane signing keypair used for the agent protocol:

```bash
./scripts/gen-keys.sh
# writes WPMGR_AGENT_SIGNING_PRIVATE_KEY / _PUBLIC_KEY into .env
```

Key env vars (all prefixed `WPMGR_`):

| Var | Purpose | Default |
|-----|---------|---------|
| `WPMGR_HTTP_ADDR` | API listen address | `:8080` |
| `WPMGR_DB_*` | Postgres connection | `localhost:5432`, db/user `wpmgr` |
| `WPMGR_REDIS_ADDR` | Redis | `localhost:6379` |
| `WPMGR_S3_ENDPOINT` | SeaweedFS S3 gateway | `http://localhost:8333` |
| `WPMGR_S3_FORCE_PATH_STYLE` | required for SeaweedFS | `true` |
| `WPMGR_CLICKHOUSE_ADDR` | ClickHouse | `localhost:9000` |
| `WPMGR_OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP collector | `http://localhost:4318` |
| `VITE_API_BASE_URL` | API base for the SPA | `http://localhost:8080` |

## 2. Bring up the stack

```bash
docker compose -f infra/docker-compose.yml up -d
```

This starts Postgres, Redis, SeaweedFS (S3 gateway on `:8333`), ClickHouse, the
API (`:8080`), and the web dashboard (served by nginx on `:80`).

## 3. Verify

```bash
curl localhost:8080/healthz   # {"status":"ok"}
curl localhost:8080/readyz    # 200 once DB/Redis/S3 are reachable
```

- `GET /healthz` — liveness (process is up).
- `GET /readyz` — readiness (dependencies reachable).

Open the dashboard at `http://localhost` (the `web` service serves the built
SPA via nginx and proxies to the API).

## Observability profile

Traces (Tempo) + metrics (Prometheus) + Grafana are opt-in via a Compose
profile (ADR-011):

```bash
docker compose -f infra/docker-compose.yml --profile observability up -d
```

Grafana then ships with the WPMgr dashboards pre-provisioned. See
[architecture.md](./architecture.md#observability).

## First-run notes

- **Migrations** run automatically on API startup (Atlas, ADR-002).
- **Default credentials in `.env.example` are for local dev only** — rotate the
  session secret, DB password, and S3 keys before any network-exposed deploy.
- Put a TLS-terminating reverse proxy (the bundled `infra/nginx/` config, or
  your own) in front of `:8080` for production.

For local development with hot-reload overrides, use `make dev` (runs
`docker-compose.yml` + `docker-compose.dev.yml`) — see
[contributing.md](./contributing.md).

## Adding a WordPress site

Once running, install the agent plugin on each managed site and pair it from the
dashboard. See [agent.md](./agent.md).

> **Note:** auto-migrations on startup and the live agent enrollment exchange
> are completed across Phase 4–5; the install steps above are stable.
