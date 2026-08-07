# Install (self-host)

Self-host WPMgr with Docker Compose. The stack runs the control plane (Go),
dashboard (React), and data plane (Postgres, Redis, SeaweedFS, ClickHouse).

> Fastest way in: the one-click installer, no clone needed. See
> [Quickstart: prebuilt images, no clone](#quickstart-prebuilt-images-no-clone-recommended-for-first-time-installs)
> below.

## Prerequisites

- **Docker** 24+ with the Compose plugin (`docker compose`, not `docker-compose`)
- Minimum ~2 GB RAM (lean, no Media Optimizer); ~8 GB recommended for the full
  default stack. See [docs/requirements.md](./requirements.md) for the full
  sizing breakdown by fleet size.

## 1. Configure env

One command copies `.env.example` to `.env` and fills in every boot-critical
secret with a freshly generated, correctly formatted value:

```bash
make quickstart        # or: ./scripts/init-env.sh
```

This is idempotent and safe to re-run: it never overwrites an existing `.env`
(pass `./scripts/init-env.sh --force` to regenerate from `.env.example`, keeping
a `.env.bak`) and only fills secret keys that are still empty or still hold the
committed dev placeholder.

The four secrets it mints — all in the exact formats the control plane validates
at boot, so the app accepts them on the first try — are:

| Var | Format | Why it matters |
|-----|--------|----------------|
| `WPMGR_SESSION_SECRET` | random ≥32-byte string | hard-fails boot if empty/too short |
| `WPMGR_AGENT_SIGNING_PRIVATE_KEY` | base64-std of the **raw** 64-byte Ed25519 key | signs CP→agent commands; rejected in prod if it's the committed dev key |
| `WPMGR_AGENT_SIGNING_PUBLIC_KEY` | base64-std of the **raw** 32-byte Ed25519 key | the public half agents verify with |
| `WPMGR_SITE_DEST_AGE_SECRET` | age X25519 secret (`AGE-SECRET-KEY-1…`) | secrets-at-rest key. **Optional**: if empty, the key is derived from `WPMGR_SESSION_SECRET` and is only as stable as that secret. Pin an explicit value on any platform that regenerates env values per deploy (see "Pin your secrets" below) |

> The values must be base64 of the **raw key bytes**, not of a PEM file — the old
> `base64 < key.pem` recipe produced keys the runtime rejected. The generator
> (`wpmgr-cli gen-secrets`) self-verifies every value by decoding it back through
> the server's own boot parsers before printing it, so a generated line is
> guaranteed to load.

### Pin your secrets

Stored secrets (operator two-factor enrollments, SMTP passwords, backup-destination
and object-cache credentials) are encrypted at rest, keyed by these values, so the
key must be identical across restarts and redeploys. Pin an explicit
`WPMGR_SITE_DEST_AGE_SECRET` (via `wpmgr-cli gen-secrets` or `age-keygen`) and keep
`WPMGR_SESSION_SECRET` fixed; this is mandatory on any platform that regenerates
env values per deploy (many PaaS do). The symptom of an unpinned key: two-factor
works once, then every later login fails, and operators are logged out on each
redeploy. The boot log prints a key fingerprint and a decrypt self-check WARN when
the key has changed. To recover: pin both values, sign in with a recovery code,
then re-enroll two-factor and re-enter the affected secrets.

To print the four secret lines without touching `.env` (e.g. to paste into a
secret manager), run the generator directly — it works with a Go toolchain or,
failing that, through the `api` Docker image:

```bash
./scripts/gen-keys.sh          # or: make gen-secrets
# or, with no Go installed:
docker compose -f infra/docker-compose.yml run --rm --no-deps \
  --entrypoint wpmgr-cli api gen-secrets
```

Then edit `.env` to set the values the generator cannot infer, before exposing
the service:

```bash
WPMGR_ENV=production                              # turns on the prod boot guards
WPMGR_PUBLIC_BASE_URL=https://wpmgr.example.com   # this control plane: the origin browsers and agents use
WPMGR_S3_ENDPOINT=https://s3.example.com          # MUST be reachable by remote agents
WPMGR_DB_PASSWORD=...                             # not the dev default
WPMGR_S3_SECRET_KEY=...                            # not the dev default
```

`WPMGR_PUBLIC_BASE_URL` and `WPMGR_S3_ENDPOINT` must resolve from the WordPress
host where the agent runs — the in-network compose default `http://seaweedfs:8333`
is only reachable inside Docker, so any real (off-host) site needs a publicly
reachable S3 endpoint (e.g. a tunnel/reverse-proxy URL).

`.env.example` groups every variable as **[REQUIRED — ALWAYS]**,
**[REQUIRED — PRODUCTION]**, or **[OPTIONAL]** with the exact format and an
example for each — read it top-to-bottom. Key env vars (all prefixed `WPMGR_`):

| Var | Purpose | Default |
|-----|---------|---------|
| `WPMGR_HTTP_ADDR` | API listen address | `:8080` |
| `WPMGR_DB_HOST` | Postgres host | `localhost` |
| `WPMGR_DB_PORT` | Postgres port | `5432` |
| `WPMGR_DB_NAME` | Postgres database | `wpmgr` |
| `WPMGR_DB_USER` | Postgres user (the `wpmgr_app` role) | `wpmgr_app` |
| `WPMGR_DB_PASSWORD` | Password for `wpmgr_app` | (required) |
| `WPMGR_DB_MIGRATION_DSN` | Full DSN for the migration-owner role (see below) | falls back to app DSN |
| `WPMGR_REDIS_ADDR` | Redis | `localhost:6379` |
| `WPMGR_S3_ENDPOINT` | SeaweedFS S3 gateway | `http://localhost:8333` |
| `WPMGR_S3_FORCE_PATH_STYLE` | required for SeaweedFS | `true` |
| `WPMGR_CLICKHOUSE_ADDR` | ClickHouse | `localhost:9000` |
| `WPMGR_OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP collector | `http://localhost:4318` |
| `WPMGR_SUPERADMIN_EMAILS` | one-shot: grants `is_superadmin` and activates the account at boot. It does NOT mark the address verified: the operator confirms their address like any other user. (Unset after it runs; revoke via `WPMGR_SUPERADMIN_REVOKE_EMAILS`.) | (empty) |
| `WPMGR_WORDFENCE_API_KEY` | vulnerability-feed API key fallback (the key saved in the superadmin UI takes precedence) | (empty) |
| `WPMGR_SCREENSHOT_READY_WAIT` | screenshot capture wait budget in whole seconds (media-encoder; raise on slow hosting) | `8` |
| `WPMGR_HOSTED` | managed-SaaS entitlements switch; hosted only, leave unset on self-host | `false` |
| `VITE_API_BASE_URL` | API base for the SPA | `http://localhost:8080` |

### Postgres: two-DSN model and the `wpmgr_app` role

WPMgr uses two Postgres connection strings:

| Setting | Role | Purpose |
|---------|------|---------|
| `WPMGR_DB_*` | `wpmgr_app` (NOSUPERUSER, NOBYPASSRLS) | Runtime application queries |
| `WPMGR_DB_MIGRATION_DSN` | Database owner or superuser | Migrations (DDL, role creation) |

`WPMGR_DB_MIGRATION_DSN` accepts a full `postgres://` DSN. When unset it falls
back to the app DSN, which works for local dev where a single user can run
migrations. Production deployments should set it to a privileged owner role.

**The application must connect as `wpmgr_app` (or a similarly restricted role).
A superuser or BYPASSRLS role skips Row-Level Security entirely and the server
will refuse to boot.**

Migrations create the `wpmgr_app` role (`NOLOGIN NOSUPERUSER NOBYPASSRLS`,
idempotent) and grant it table privileges. You must enable login + set a password
out of band after the first migration run:

```sql
-- run once after migrations, as the owner role:
ALTER ROLE wpmgr_app LOGIN PASSWORD 'your-password';
```

Then set `WPMGR_DB_USER=wpmgr_app` and `WPMGR_DB_PASSWORD=your-password`.

The `plugin_signatures` corpus table (used by the Database Cleaner) is
**insert-only protected**: `wpmgr_app` has `SELECT` only at runtime. The corpus
seed migration temporarily grants itself `INSERT/UPDATE` (owner bypasses RLS),
populates the table, then REVOKEs write access from `wpmgr_app`. This
GRANT-self/REVOKE pattern means the corpus migration requires the owner role
(`WPMGR_DB_MIGRATION_DSN`); running it as `wpmgr_app` will fail the REVOKE step.

The `WPMGR_ALLOW_RLS_BYPASS_ROLE=true` env var (default `false`) downgrades
the boot-time RLS check to a warning. Intended only for single-node local dev
sharing the bootstrap superuser; never set it in production.

## 2. Bring up the stack

### Quickstart: prebuilt images, no clone (recommended for first-time installs)

The one-liner below fetches every required file, generates all secrets, and
prints the exact `docker compose` command to start WPMgr — no repo clone, no
manual editing:

```bash
curl -fsSL https://raw.githubusercontent.com/mosamlife/wpmgr/main/scripts/quickstart-selfhost.sh | bash
```

Or, if you prefer to inspect the script first:

```bash
curl -fsSL https://raw.githubusercontent.com/mosamlife/wpmgr/main/scripts/quickstart-selfhost.sh -o quickstart-selfhost.sh
bash quickstart-selfhost.sh --hostname=https://wpmgr.example.com
```

Flags:
- `--hostname=URL` — sets `WPMGR_PUBLIC_BASE_URL` non-interactively (recommended for servers).
- `--version=vX.Y.Z` — pins a specific release tag; omit to use `:latest`.
- `--dir=PATH` — writes everything into a custom directory (default: `./wpmgr`).

The script downloads:

| File | Why |
|------|-----|
| `infra/docker-compose.yml` | base stack |
| `infra/docker-compose.prod.yml` | pull-only GHCR overlay |
| `.env.example` + `scripts/init-env.sh` | env bootstrap |
| `infra/seaweedfs/s3.json` | SeaweedFS S3 auth (bind-mounted) |
| `infra/dex/config.yaml` | Dex OIDC config (bind-mounted) |
| `infra/postgres/init/01-app-role.sh` | Postgres role init (bind-mounted) |
| `infra/prometheus/prometheus.yml` + `infra/grafana/…` | observability profile |

> **Port note:** the API listens on `:8080` *inside* the container, but is
> published to the **host** on `:8081` (`WPMGR_API_PORT`). The dashboard nginx
> is on **`:8088`** (`WPMGR_WEB_PORT`). These are the ports you curl and put
> behind a reverse proxy. Neither is `:80` or `:8080` on the host — those are
> deliberately avoided so first boot never needs root or collides with an
> existing web server.

### Or: build from source (clone path)

If you have cloned the repo:

```bash
docker compose -f infra/docker-compose.yml up -d
```

This starts Postgres, Redis, SeaweedFS (S3 gateway on `:8333`), ClickHouse, the
API, the web dashboard (served by nginx), and the media-encoder (screenshots +
Media Optimizer — see [Media encoder](#media-encoder) below for its resource
footprint and how to disable it). To avoid colliding with anything
already bound on the host, the **published** host ports default to non-standard
values — the dashboard on **`:8088`** and the API on **`:8081`** (the
container-side ports are unchanged, so in-network wiring is unaffected). Override
any of them in `.env` with the `WPMGR_*_PORT` vars (`WPMGR_WEB_PORT`,
`WPMGR_API_PORT`, `WPMGR_S3_PORT`, `WPMGR_DEX_PORT`) — e.g. set
`WPMGR_WEB_PORT=80` to serve the dashboard on the standard HTTP port.

### Prebuilt GHCR images (no local build)

Pre-built `linux/amd64` images are published on GitHub Container Registry:
`ghcr.io/mosamlife/wpmgr-api`, `-web`, and `-media-encoder` (each tagged
`:vX.Y.Z` and `:latest`). If you already have the compose files (via the
quickstart or a clone), bring up the stack with the pull-only overlay:

```bash
export WPMGR_VERSION=v0.19.0   # omit to track :latest
docker compose -f infra/docker-compose.yml -f infra/docker-compose.prod.yml up -d
```

The overlay only swaps the three app services to `image:` + `pull_policy:
always`; everything else (Postgres, Redis, SeaweedFS, ClickHouse, env, volumes)
is inherited from the base file, including `media-encoder` — it starts by
default, no profile needed.

> GHCR packages are public. `docker pull` needs no auth. arm64 multi-arch
> images are a near-term follow-up.

## 3. Verify

```bash
# Direct to the API host port (WPMGR_API_PORT, default 8081 — NOT :8080):
curl localhost:8081/healthz   # {"status":"ok"}
curl localhost:8081/readyz    # 200 once DB/Redis/S3 are reachable

# Or via the nginx web container (WPMGR_WEB_PORT, default 8088):
curl localhost:8088/healthz   # proxied by nginx -> api:8080/healthz
```

- `GET /healthz` — liveness (process is up).
- `GET /readyz` — readiness (dependencies reachable).

**Port disambiguation:** `:8080` is the container-internal listen address
(`WPMGR_HTTP_ADDR`). It is **not** published to the host. What you `curl` is
the **host** port — `8081` for the API directly, `8088` for the nginx web
proxy. Both are overridable in `.env` via `WPMGR_API_PORT` and `WPMGR_WEB_PORT`.

Open the dashboard at `http://localhost:8088` (the default `WPMGR_WEB_PORT`; the
`web` service serves the built SPA via nginx and proxies to the API). Set
`WPMGR_WEB_PORT=80` in `.env` if you want it on the standard HTTP port.

## Media encoder

Image encoding (JPEG/PNG to WebP/AVIF, the Media Optimizer tab) and site
screenshot capture (the Sites grid cards) both run on a separate `media-encoder`
service. It is part of the **base** stack and starts automatically with the
plain `docker compose -f infra/docker-compose.yml up -d` shown above — no
Compose profile or extra flag needed. This means both features work out of the
box on a default install.

Resource footprint: the image bundles headless Chromium for screenshot capture,
so it is noticeably heavier than the api/web images (expect on the order of a
few hundred MB of additional RAM at idle, more while a capture or encode job is
running). If you're on a constrained host and don't need screenshots or image
optimization, disable it without editing the compose file:

```bash
# scale it to zero — api/web still start normally
docker compose -f infra/docker-compose.yml up -d --scale media-encoder=0
```

or comment out the `media-encoder:` service block in `infra/docker-compose.yml`
if you never want it built/pulled at all. Either way, screenshot cards fall back
to favicon/monogram permanently and the Media Optimizer tab is unavailable —
everything else in the dashboard is unaffected.

The media-encoder runs its jobs in a dedicated River schema (default
`media_encoder`), separate from the API's own default/public schema. This is
required, not optional: River leader election is per-schema, and if the
media-encoder shared the API's schema it could silently win leadership and
stop **all** of the API's fleet periodic jobs (backups, uptime checks,
sweeps, GC) with no error anywhere (GH #205). The media-encoder binary
refuses to start if it resolves to the API's default/public schema, so this
can't happen by misconfiguration. The compose file sets
`WPMGR_RIVER_MEDIA_SCHEMA=media_encoder` on both the API and media-encoder
services; custom deployments must set the same dedicated value on both
processes so media and screenshot jobs are inserted into the schema the
encoder is polling. When this value names a dedicated schema, the encoder
also needs the migration-owner DSN so it can create and migrate that schema
safely. If you never run the media-encoder (disabled/scaled to zero), this
setting has no effect on the API.

Upgrade note: an existing self-host `.env` from before this variable existed
needs no changes — both the compose default and the binary's built-in default
already resolve to `media_encoder`. If your deployment previously ran the
media-encoder against the API's shared/public schema (before the
`media_encoder` default existed) and had media/screenshot jobs queued at the
time, the encoder reconciles any jobs it still finds stranded in
`public.river_job` automatically the first time it starts on the dedicated
schema, so nothing further is required.

See [features/media-optimizer.md](./features/media-optimizer.md) and
[features/sites.md](./features/sites.md#website-screenshots) for the features
themselves.

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
  your own) in front of the published API port (`WPMGR_API_PORT`, default
  `:8081`) for production.

For local development with hot-reload overrides, use `make dev` (runs
`docker-compose.yml` + `docker-compose.dev.yml`) — see
[contributing.md](./contributing.md).

## Making the S3 endpoint reachable for backups (self-host) {#s3-networking}

The API mints presigned S3 URLs from `WPMGR_S3_ENDPOINT` and hands them to the
agent, which PUTs backup chunks directly to those URLs from the WordPress host.
That means `WPMGR_S3_ENDPOINT` must resolve and accept connections from **two
places at once**: the API container (to build the presigned URL) AND the remote
WordPress server (to actually upload). The compose default
`http://seaweedfs:8333` and `http://localhost:8333` satisfy neither requirement
for a real off-host site. Symptoms when it is wrong: the backup dashboard shows
"stalled, no progress for >2m", or the agent log contains
`EncryptAndUpload: PUT failed for chunk` (the presigned URL is either
unreachable from the WP host or returns 403).

### Recommended: a public HTTPS subdomain via a reverse proxy

Set `WPMGR_S3_ENDPOINT` to a subdomain you control:

```bash
WPMGR_S3_ENDPOINT=https://s3.yourdomain.com
WPMGR_S3_FORCE_PATH_STYLE=true   # required for SeaweedFS/MinIO — keep this true
```

Then proxy that subdomain to the SeaweedFS S3 gateway. The compose stack
publishes the gateway on **host port 8333** (`WPMGR_S3_PORT`).

**If you are using Nginx Proxy Manager (NPM) or a proxy in a separate Docker
network:** NPM runs on the host network (or its own bridge) and cannot resolve
the compose service name `seaweedfs`. Point the proxy at the **Docker bridge
gateway IP and the published host port** instead:

```
https://s3.yourdomain.com  ->  http://172.17.0.1:8333
```

`172.17.0.1` is the default `docker0` bridge gateway; traffic reaches
SeaweedFS through the host without cross-network DNS. If you changed the
published port via `WPMGR_S3_PORT`, substitute that port here.

**NAT hairpinning gotcha:** do not point `WPMGR_S3_ENDPOINT` (or the proxy
upstream) directly at the server's public IP. The API container calling its own
public IP may not loop back on some hosts (common on Hetzner), and can also
produce `SignatureDoesNotMatch` errors. Routing through the bridge IP
(`172.17.0.1`) sidesteps that entirely.

### WAF / Cloudflare in front of the managed WordPress site

This is a separate concern from the S3 endpoint. If the WordPress site WPMgr
manages sits behind Cloudflare or another WAF, managed-challenge rules can
block the control plane's commands to the agent, which also appears as a
stalled backup (no agent response). Allowlist the control-plane server's IP
and the WPMgr agent User-Agent at the WAF so backup and restore traffic is not
challenged.

### Future: separate internal and public endpoints

A dedicated `WPMGR_S3_PUBLIC_ENDPOINT` variable (so the internal API can reach
SeaweedFS over a private path while the agent uses a separate public URL) is
being considered to remove the single-value constraint described above.

## Adding a WordPress site

Once running, install the agent plugin on each managed site and pair it from the
dashboard. See [agent.md](./agent.md).

> **Note:** auto-migrations on startup and the live agent enrollment exchange
> are completed across Phase 4–5; the install steps above are stable.
