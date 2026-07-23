# Requirements (self-host)

These are the system requirements for running the WPMgr control plane
(control plane, dashboard, and data plane) on your own infrastructure. They
are architecture-derived estimates, not load-tested benchmarks; see
[Caveats](#caveats) before you provision hardware. They do not cover the
managed WordPress hosts themselves: the agent plugin only needs PHP 8.1+ and
WordPress 6.0+, see [agent.md](./agent.md).

## Host prerequisites

- 64-bit Linux host, x86-64 or ARM64
- Docker Engine 24+ with the Compose v2 plugin (`docker compose`, not `docker-compose`)
- Outbound network access (image pulls, WordPress.org checksum lookups, TLS certificate checks)
- A `WPMGR_S3_ENDPOINT` reachable by both the control plane and the managed
  WordPress hosts, see [install.md#s3-networking](./install.md#s3-networking)

## Minimum vs recommended

### Minimum: lean control plane, no Media Optimizer, fleet under 25 sites

| Resource | Spec |
|---|---|
| CPU | 2 vCPU |
| RAM | 2 GB |
| Disk | 20 GB SSD to start, growing with backups |

What's running: postgres, redis, api, web, plus an object-storage target
(external S3/R2, or the bundled SeaweedFS container). What's off:
media-encoder scaled to 0, ClickHouse off (`WPMGR_CLICKHOUSE_ADDR` unset falls
back to the Postgres metrics store), Dex off (email and password auth).

RAM breakdown: core services idle at roughly 250-500 MB combined; SeaweedFS
adds another 40-100 MB idle. Disk breakdown: OS plus roughly 2-3 GB of images,
Postgres at 1-2 GB, and a few small backups to start.

This boots the full control plane: enrollment, uptime monitoring, updates,
backups and restore, alerts, and the dashboard. You only give up site
screenshots, the Media Optimizer, WOFF2 font tooling, and large-fleet
time-series performance.

### Recommended: full default stack, small-to-mid fleet

| Resource | Spec |
|---|---|
| CPU | 4 vCPU |
| RAM | 8 GB |
| Disk | 100 GB+ SSD, dominated by backup chunk storage |

What's running: postgres, redis, api, web, object storage, and media-encoder.
Set `WPMGR_MEDIA_ENCODE_WORKERS=2` on self-host (the image default is 3).

RAM jumps from 2 GB to 8 GB because of media-encoder: each in-flight headless
Chromium capture is roughly 150-300 MiB at the default capture concurrency of
2, plus encode workers on top, needing about 1-2 GB of live headroom over the
core services.

A plain `docker compose up` also starts ClickHouse
(`WPMGR_CLICKHOUSE_ADDR` defaults to `clickhouse:9000`) and Dex. ClickHouse is
the heaviest idle datastore (roughly 250-500 MB, and it wants several GB at
scale) and buys nothing under about 100 sites, so below that size leave it off
and 4-6 GB RAM is comfortable.

## By fleet size

| Fleet size | CPU | RAM | Disk | Notes |
|---|---|---|---|---|
| Under 25 sites | 2 vCPU | 2 GB without Media Optimizer, 4 GB with it | 20-50 GB SSD (Postgres 1-2 GB; backups dominate) | Defaults are fine: ClickHouse off, probe concurrency 10, probe interval 60s. Set `WPMGR_MEDIA_ENCODE_WORKERS=2` if running the encoder. |
| 25 to 100 sites | 2-4 vCPU | 4 GB without Media Optimizer, 8 GB with it | 100-300 GB SSD (backup chunks dominant; Postgres roughly 5-10 GB) | The Postgres metrics store is still fine to about 100 sites. Raise `WPMGR_UPTIME_PROBE_CONCURRENCY` toward 25-50 as you approach 100 so each 60s sweep does not overlap. |
| 100 to 500+ sites | 4-8 vCPU | 8-16 GB (add 2-4 GB if enabling ClickHouse; add 2-4 GB for the Media Optimizer) | Hundreds of GB to multiple TB | Strongly prefer external S3/R2 over a local SeaweedFS volume at this tier. Turn on ClickHouse (`WPMGR_CLICKHOUSE_ADDR`) to offload the uptime time series and give it its own 2-4 GB or more. Raise probe concurrency to 50-100, and/or lengthen the probe interval, and/or trim retention. Give Postgres 4-8 GB and tune `shared_buffers`; only raise `max_connections` if you also scale the api service horizontally (the pgx pool is a hardcoded 5 connections per api instance). |

## Required vs optional services

The base compose stack has 9 services. Here is what each one does, whether
you can drop it, and what dropping it costs.

| Service | Required? | What it does | Dropping it |
|---|---|---|---|
| `postgres` | Required | System of record, plus the embedded River job queue, plus the Postgres metrics fallback. `postgres:16.4-alpine`. | Not possible; the control plane cannot boot without it. |
| `redis` | Required | Operator dashboard session store. `redis:7.4-alpine`. | Not possible in production: if Redis is unreachable, every authenticated request returns 500 (there is no cookie or in-memory fallback in the production boot path). |
| `api` | Required | The Go control plane. Runs all fleet cron: uptime probe, backup scheduler, garbage collection, sweeps. Pure-Go static binary, light at idle on a single instance (roughly 30-80 MB). | Not possible; this is the control plane itself. |
| `web` | Required | nginx serving the React dashboard bundle and proxying `/api` and `/auth`. Trivial footprint (roughly 10-30 MB). | Not possible without replacing it with your own reverse proxy in front of the api. |
| object storage (`seaweedfs` or external S3) | Conditional | SeaweedFS (bundled, Apache-2.0 S3 gateway) or any external S3-compatible endpoint (AWS S3, Cloudflare R2, external MinIO) via `WPMGR_S3_ENDPOINT`. | The api boots without it, but any storage-backed feature needs it: backups and restore, the Media Optimizer, screenshots, WOFF2 fonts, client report PDFs. Presigned URLs must be reachable by the managed WordPress hosts, so an in-network-only SeaweedFS default (`http://seaweedfs:8333`) is not usable for real remote sites; give it a routable address or use external S3. |
| `media-encoder` | Optional | The single heaviest RAM component. A pull worker for screenshots (headless Chromium), image optimize to AVIF/WebP, and WOFF2 font transcode. | Disable without editing files: `docker compose -f infra/docker-compose.yml up -d --scale media-encoder=0`. Screenshots stop (cards fall back to favicon/monogram), the Media Optimizer's optimize step never completes, WOFF2 transcode is dead; everything else keeps working. When it does run, it must use a dedicated River schema (`WPMGR_RIVER_MEDIA_SCHEMA=media_encoder`, set identically on `api` and `media-encoder`) or it wins River leader election and silently stops all fleet cron. |
| `clickhouse` | Optional | Metrics time-series store. The base compose wires it on by default (`WPMGR_CLICKHOUSE_ADDR=clickhouse:9000`). | To drop it, unset that env and do not start the container; an empty address falls back cleanly to the Postgres metrics store. It is the heaviest idle datastore (roughly 250-500 MB) and only pays off at large fleet size or high probe volume. |
| `dex` | Optional | Self-host OIDC identity provider for SSO. | The api falls back to email and password auth when `WPMGR_OIDC_ISSUER` is unset. A plain `compose up` waits on Dex via `depends_on`, so fully dropping it means removing that dependency and leaving the issuer unset. |
| `otel-lgtm` (observability bundle) | Optional | Traces, metrics, and Grafana. | Behind the `observability` compose profile; never starts on a default `up`. |

## Storage guidance

Two stores dominate. Disk is driven far more by backups than by site count.

**Postgres.** The data dir is dominated by the `site_uptime_probes` table:
exactly one row per site per probe interval (default 60s, 1,440 rows per site
per day), retained 90 days and rolled up daily. Budget roughly 50-100 MB
Postgres per site: about 1-2 GB at 25 sites, about 5-10 GB at 100 sites, about
15-30 GB at 500 sites. Shrink it by lengthening
`WPMGR_UPTIME_PROBE_INTERVAL`, lowering retention, or offloading to
ClickHouse.

**Backup chunk storage (S3 or SeaweedFS).** This is the dominant consumer and
dwarfs everything else. Chunks are content-addressed (blake3), age-encrypted
on the agent, and deduplicated within a tenant, with incremental
archive-delta backups: each increment packs only changed or new files.
Budget roughly 1.5 to 3 times one full backup per site (a typical WordPress
full backup is 0.5-5 GB), sized by sites times site size times
`WPMGR_BACKUP_RETENTION_DAYS` (default 30), plus 12 monthly archives. Worked
example: 100 sites at roughly 2 GB effective works out to about 200-300 GB.
Plan disk around this line item, and at scale prefer external S3/R2.
Screenshots, Media Optimizer output, and WOFF2 fonts share the same bucket
but are a rounding error next to backups.

## Tuning knobs

| Variable | Purpose | Default |
|---|---|---|
| `WPMGR_UPTIME_PROBE_INTERVAL` | How often each site is probed | `60s` |
| `WPMGR_UPTIME_PROBE_CONCURRENCY` | Probes run concurrently per sweep | `10` |
| `WPMGR_CLICKHOUSE_ADDR` | ClickHouse address; empty falls back to the Postgres metrics store | `clickhouse:9000` in the base compose |
| `WPMGR_S3_ENDPOINT` / `_BUCKET` / `_ACCESS_KEY` / `_SECRET_KEY` | Object storage target: bundled SeaweedFS or external S3 | see `.env.example` |
| `WPMGR_S3_FORCE_PATH_STYLE` | Required `true` for SeaweedFS/MinIO-style endpoints; can be `false` for AWS S3 | `true` |
| `WPMGR_MEDIA_ENCODE_WORKERS` | media-encoder concurrency | `3` |
| `WPMGR_RIVER_MEDIA_SCHEMA` | Dedicated River schema for media-encoder jobs (must match on `api` and `media-encoder`) | `media_encoder` |
| `WPMGR_BACKUP_RETENTION_DAYS` | How long backup snapshots are retained before garbage collection | `30` |

## Caveats

- These are architecture-derived estimates, not load-tested benchmarks. Real
  usage varies with site size, backup frequency, probe interval, and which
  features are enabled.
- media-encoder is the RAM swing factor: it scales with concurrent screenshot
  captures, not fleet size.
- Backup chunk storage is the dominant, least predictable disk cost. It
  tracks total managed site size times retention, not site count.
- Postgres connections are not a bottleneck on a single-node self-host
  (roughly 13 live connections against the image default `max_connections=100`).
- A plain `docker compose up` starts the heavy default: ClickHouse,
  media-encoder, Dex, and SeaweedFS all together. Reaching the lean minimum
  requires actively trimming (scale media-encoder to 0, unset
  `WPMGR_CLICKHOUSE_ADDR` and skip the container, remove the Dex dependency).
- Hosted-production Cloud Run caps (api 1 vCPU/1 GiB, web 1 vCPU/512 MiB,
  media-encoder 4 vCPU/16 GiB) are a fleet-wide upper bound serving every
  tenant at once, not a self-host floor.

## See also

- [install.md](./install.md) for the step-by-step setup, including the
  one-click installer and the S3-networking gotcha for remote WordPress
  hosts.
