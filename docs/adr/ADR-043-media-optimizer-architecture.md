# ADR-043 — Media Optimizer: architecture, encode topology & transport

**Status:** Accepted · **Date:** 2026-05-31
**Relates:** ADR-033 (backup transport — presigned S3), ADR-010 (object storage), ADR-031 (CP→agent signed commands).
**Recon:** `analysis/media-optimizer-recon.md`, `reference/flying-press-patterns.md`.

## Context

WPMgr is adding a Media Optimizer (JPEG/PNG → WebP/AVIF) with the same product
surface as FlyingPress's image optimization, but the encode runs on **our own
software** (Discord's MIT-licensed `lilliput`), not a third-party SaaS. WPMgr is
**open-source and self-hostable**, so every choice below must work for a
self-hoster running plain `docker compose`, not only our hosted (Cloud Run)
deployment.

Recon surfaced hard constraints that reshape the original feature spec:

- The CP API ships **`CGO_ENABLED=0` on `distroless/static`** (a tiny static
  binary). lilliput is **CGO + native codec libs** → it cannot live in that image.
- The agent→CP signed-request path is **JSON-only, 4 MiB body cap** — unsuitable
  for streaming image multipart through the CP.
- The CP **never reads object bytes via the SDK `GetObject`** (it 403s on GCS
  S3-compat — ADR-042 fix) and holds no site decryption keys. Bytes move
  agent↔storage via **presigned URLs** (the backup model).

## Decisions

### 1. Image library + encode topology — lilliput in a **separate, optional `media-encoder` service**

- **Library:** `github.com/discord/lilliput` v1.5.0 (**MIT**). Verified: real AVIF
  *encode* via **libaom** (decode via dav1d), exposing `AvifQuality` (0–100),
  `AvifSpeed` (0–10), `WebpQuality`, `JpegQuality`, `JpegProgressive`,
  `PngCompression`. Prebuilt static codec libs ship for **linux/amd64** (prod)
  and **darwin/arm64** (dev Mac). lilliput needs **CGO=1 + a glibc base**
  (`distroless/base-nonroot`), not `distroless/static`.
- **Topology:** a NEW **`media-encoder`** service (own image `cmd/media-encoder`
  + `infra/Dockerfile.media-encoder`, CGO + lilliput + glibc) that runs a **River
  worker** on a dedicated bounded `media_encode` queue. The **main API stays
  exactly as-is** (`CGO_ENABLED=0`, `distroless/static`). Rejected: CGO-enabling
  the main API (poisons the lean control plane with native deps + glibc CVE
  surface to serve one background feature, and couples encode CPU bursts to the
  API instance).
- **Open-source / self-host posture (load-bearing):** the encoder is an
  **optional, pluggable component**. It is a service in `infra/docker-compose.yml`
  behind an opt-in profile (`--profile media`); a self-hoster who wants image
  optimization runs `docker compose --profile media up`, one who doesn't simply
  doesn't, and their core API stays a minimal static binary. On our hosted
  deployment the same container is a second Cloud Run service — but nothing here
  is GCP-specific; it is just another container connecting to the same Postgres +
  object storage. No proprietary or managed encode API is used anywhere.
- **Fallback (documented escape hatch):** `govips`/libvips (LGPL/MIT) drops into
  the **same** `media-encoder` image (Debian runtime + `libvips`, not Alpine) with
  no change to the main API, if a lilliput AVIF knob is ever missing.

### 2. Transport — presigned object storage, never multipart-through-CP

Bytes move exactly like backups (ADR-033), never through the API/encoder request
body:

1. Agent reads each image variant from disk and **presigned-PUT**s it to a
   per-job temp object in storage (`media/<tenant>/<site>/<job>/src/<variant>`).
2. The `media-encoder` River worker **presigned-GET**s the source, encodes with
   lilliput, and **presigned-PUT**s the optimized bytes
   (`media/.../out/<variant>`).
3. The agent **presigned-GET**s each optimized output and applies it on disk.
4. All temp objects are **deleted** when the job ends (success, failure, or
   cancel). **No optimized or source image bytes are persisted on the control
   plane.** The CP persists only metadata rows (`site_media_assets`,
   `media_optimization_jobs`, `media_variant_results`) + audit entries.
5. Dashboard thumbnails load **directly from the site's own public media URLs**
   (`original_url`) — the CP stores no image bytes, including thumbnails.

The CP↔agent choreography (who tells the agent to upload / apply, and how it
learns outputs are ready) reuses the existing CP→agent **signed command** channel
(ADR-031) + the agent→CP **signed JSON** callbacks; the exact handshake is a
Phase-3 design detail.

### 3. Batching — ≤ 10 variants per attachment per job

One optimize job covers one WP attachment and all its registered sizes
(full + thumbnail + medium + large + …), capped at **10 variants** per job.
Variants beyond one attachment are NOT batched together; a bulk "optimize 50
assets" fans out to 50 jobs. Per-variant failures do not block sibling variants.

### 4. Encoder defaults

| Source → target | Lossy (default) | Lossless mode (per-request) |
|---|---|---|
| → **AVIF** | `AvifQuality=50`, `AvifSpeed=6` | `AvifQuality=100`, `AvifSpeed=6` |
| → **WebP** | `WebpQuality=80` | `WebpQuality=100` |
| → **original (JPEG)** | `JpegQuality=82`, `JpegProgressive=1` (mozjpeg-class) | `JpegQuality=95`, progressive |
| → **original (PNG)** | lossless, `PngCompression=9` | lossless, `PngCompression=9` |

Source format is detected from **magic bytes by lilliput**, never trusted from the
agent's MIME claim. Output MIME derives from the target format. Only `image/jpeg`
and `image/png` sources are optimizable; others are recorded `excluded`.

### 5. Resource limits + per-variant retry

- Encoder refuses sources > **50 MB** or > **100 MP** (`ErrDimensionsTooBig`);
  per-encode timeout 60 s (`ErrEncoderTimeout`). The `media_encode` queue uses a
  small `MaxWorkers` so a burst of large images cannot OOM the encoder instance;
  the River job `Timeout()` is generous (≈5 min), mirroring the SQL-inspect worker.
- **Per-variant retry up to 2× with exponential backoff.** A failed variant
  surfaces in the asset's `sizes_unoptimized` map with a human reason; it does
  NOT fail the whole attachment.

### 6. AuthZ (adapted to the real role-rank model — no named permissions exist)

- `sync` / `optimize` / `restore` gate on **`PermSiteWrite`** (operator+), via the
  existing `RequireSiteAccess(":siteId")` per-site middleware.
- **`delete-originals`** (irreversible) gates on a new **`PermMediaDeleteOriginals`
  → minimum role `admin`**, added to `authz/role.go`, plus a type-the-hostname UI
  confirmation. By-id job routes nest under `/sites/:siteId/media/...` so per-site
  access is always enforced (site-scoped collaborators included).

### 7. Conventions adopted from recon

- **Migration:** single file `20260531110000_m23_media_optimizer.sql`, all
  `CREATE … IF NOT EXISTS`, **no `.up`/`.down`, no `set_updated_at` trigger**
  (`updated_at` set by repo code), `ENABLE` + `FORCE ROW LEVEL SECURITY` with a
  tenant-isolation policy (`current_setting('app.tenant_id', true)`) **and** an
  `app.agent` worker policy; grants inherited via default privileges.
- **SSE:** new `media.*` event types added to the shared tenant bus
  (`SITE_EVENT_TYPES` in `use-site-events.ts`), filtered by `site_id`.
- **API DTOs:** hand-rolled local DTO structs + `c.JSON` (like the `scan`
  feature), not OpenAPI/ogen regen.
- **Tests:** agent PHPUnit (Brain Monkey) + CP Go testcontainers (Postgres +
  MinIO + River). A live-WordPress E2E container does **not** exist in the repo
  and is **deferred** (net-new infra) rather than built this milestone.

## Consequences

- ✅ Main API image + CVE surface untouched; encoder scales/fails independently;
  media optimization is opt-in for self-hosters (modular, no core bloat).
- ✅ No media bytes ever persist on the CP; transport reuses the proven
  presigned-S3 backup path; MIT/LGPL licensing keeps the whole stack open-source.
- ⚠️ One extra deployable container (a compose service / a 2nd Cloud Run service)
  that needs Postgres + object-storage access; larger encoder image (bundled
  codec libs + glibc).
- ⚠️ AVIF/libaom encode is CPU-heavy — bounded queue + generous timeout + the
  50 MB / 100 MP guards contain it.
- Fallback to govips/libvips is isolated to the `media-encoder` image.

## Phased plan

See `PLAN.md` → **Phase 5.8 — Media Optimizer**. Mirrors FlyingPress's
orchestration *patterns* (postmeta blob, `.wpmgr-original.*` rename, Accept-header
`.htaccess` fallback, serialized-safe URL rewriter, media-modal injection) under
WPMgr naming — **no FlyingPress code is copied** (credit: `apps/agent/NOTICE.md`).
