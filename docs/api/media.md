# Media API — optimizer dashboard & agent callbacks

Endpoints for the Media Optimizer (M23 / Phase 7). Two surfaces: operator-facing
dashboard routes under `/api/v1/sites/{siteId}/media/...` (session + RBAC) and
agent-callback routes under `/agent/v1/media/...` (Ed25519 signed-request).

Design: [ADR-043](../adr/ADR-043-media-optimizer-architecture.md).
User guide: [features/media-optimizer.md](../features/media-optimizer.md).
Architecture: [architecture/media-optimizer.md](../architecture/media-optimizer.md).

> **Hand-rolled DTOs.** Per ADR-043 §7 these routes hand-roll local DTO structs
> + `c.JSON` (the scan-feature convention), **not** ogen-generated types. They
> are not in `packages/openapi/openapi.yaml` and not exposed by the `@wpmgr/api`
> TS client; the frontend types are hand-written. Source of truth:
> `apps/api/internal/media/handler/handler.go` and `.../agent_handler.go`.

## Auth & RBAC

| Group | Auth | Scope |
|-------|------|-------|
| `GET .../media/assets`, `.../jobs`, `.../jobs/{jobId}` | session / API key | `site:read` + `RequireSiteAccess(:siteId)` |
| `POST .../media/sync`, `/optimize`, `/restore`, `/cancel` | session / API key | `site:write` + `RequireSiteAccess(:siteId)` |
| `POST .../media/delete-originals` | session / API key | **`media:delete_originals`** (admin+) + `RequireSiteAccess(:siteId)` |
| `POST /agent/v1/media/...` | Ed25519 signed-request | site/tenant bound from the verified agent key |

All by-id routes nest under `/sites/{siteId}/media/...` so per-site access is
always enforced (site-scoped collaborators included). Every agent callback
re-asserts the job to the agent's proven tenant+site before any mutation; the
tenant/site come from the verified key, never a client header.

---

## Dashboard endpoints

### GET /api/v1/sites/{siteId}/media/assets — list assets

Query: `limit` (default 50), `cursor`, `status`, `format`, `search`.

**Response** `200 OK`

```json
{
  "items": [
    {
      "id": "1f2e3d4c-…",
      "site_id": "6f1c2b7e-…",
      "wp_attachment_id": 1842,
      "title": "banner",
      "original_url": "https://blog.example.com/wp-content/uploads/2026/05/banner.jpg",
      "original_mime": "image/jpeg",
      "original_size_bytes": 4521983,
      "current_format": "avif",
      "current_size_bytes": 412007,
      "status": "optimized",
      "generation": 2,
      "sizes_optimized": ["full", "thumbnail", "medium", "large"],
      "sizes_unoptimized": { "scaled": "Unsupported source format" },
      "last_optimized_at": "2026-05-31T18:04:11Z"
    }
  ],
  "next_cursor": "…",
  "total_count": 318,
  "summary": { "total": 318, "optimized": 211, "pending": 96, "failed": 11, "bytes_saved": 184392011 }
}
```

`status` ∈ `pending | optimizing | optimized | failed | restoring | restored |
excluded | originals_deleted`. `current_format` ∈ `original | webp | avif`.

### POST /api/v1/sites/{siteId}/media/sync — sync the media library

No body. Tells the agent to scan the WP media library and upsert each attachment
into `site_media_assets`. Nothing on disk changes.

**Response** `202 Accepted`

```json
{ "job_id": "01J9Z…", "started_at": "2026-05-31T18:00:00Z" }
```

### POST /api/v1/sites/{siteId}/media/optimize — start optimization

**Request**

```json
{
  "asset_ids": ["1f2e3d4c-…", "2a3b4c5d-…"],
  "all_pending": false,
  "target_format": "avif",
  "target_quality": "lossy"
}
```

`asset_ids` or `all_pending` selects the assets; `target_format` ∈ `avif | webp
| original`; `target_quality` ∈ `lossy | lossless`. Fans out to one job per
attachment (≤10 variants each).

**Response** `202 Accepted`

```json
{ "batch_job_id": "01J9Z…", "queued_count": 2 }
```

### POST /api/v1/sites/{siteId}/media/restore — restore originals

**Request**

```json
{ "asset_ids": ["1f2e3d4c-…"] }
```

**Response** `202 Accepted` — `{ "batch_job_id": "01J9Z…", "queued_count": 1 }`.
Refused for assets whose originals were deleted.

### POST /api/v1/sites/{siteId}/media/delete-originals — delete originals (irreversible)

Gated on `media:delete_originals` (admin+). Permanently removes archived
originals; restore is no longer possible.

**Request**

```json
{ "asset_ids": ["1f2e3d4c-…"] }
```

**Response** `202 Accepted`

```json
{ "batch_job_id": "01J9Z…", "queued_count": 1, "irreversible": true }
```

### POST /api/v1/sites/{siteId}/media/cancel — cancel running jobs

No body.

**Response** `200 OK` — `{ "ok": true, "cancelled_count": 3 }`.

### GET /api/v1/sites/{siteId}/media/jobs — list jobs

Query: `limit` (default 50), `cursor`, `state`.

**Response** `200 OK`

```json
{
  "items": [
    {
      "id": "01J9Z…",
      "site_id": "6f1c2b7e-…",
      "asset_id": "1f2e3d4c-…",
      "wp_attachment_id": 1842,
      "kind": "optimize",
      "target_format": "avif",
      "target_quality": "lossy",
      "state": "completed",
      "bytes_before": 4521983,
      "bytes_after": 412007,
      "variants_total": 4,
      "variants_succeeded": 4,
      "variants_failed": 0,
      "created_at": "2026-05-31T18:00:02Z",
      "started_at": "2026-05-31T18:00:03Z",
      "completed_at": "2026-05-31T18:04:11Z"
    }
  ],
  "next_cursor": "…"
}
```

`kind` ∈ `sync | optimize | restore | delete_originals`.

### GET /api/v1/sites/{siteId}/media/jobs/{jobId} — job detail (with variants)

**Response** `200 OK` — the job object above, plus a `variants` array:

```json
{
  "id": "01J9Z…",
  "kind": "optimize",
  "state": "completed",
  "variants": [
    {
      "variant_name": "full",
      "source_size_bytes": 4521983,
      "optimized_size_bytes": 412007,
      "source_mime": "image/jpeg",
      "optimized_mime": "image/avif",
      "encode_ms": 1840,
      "state": "succeeded"
    },
    {
      "variant_name": "scaled",
      "source_size_bytes": 0,
      "source_mime": "image/gif",
      "state": "failed",
      "reason": "Unsupported source format"
    }
  ]
}
```

Variant `state` ∈ `succeeded | failed`.

---

## Agent endpoints (Ed25519 signed-request)

Bodies are small JSON, capped well under the agent middleware's 4 MiB buffer.
Image bytes never ride these requests — they move agent ↔ storage over presigned
URLs.

### POST /agent/v1/media/sync-batch — report a page of attachments

Upserts a page (≤200 attachments) of the WP media library into
`site_media_assets`.

**Request**

```json
{
  "attachments": [
    {
      "wp_attachment_id": 1842,
      "title": "banner",
      "original_path": "/var/www/.../uploads/2026/05/banner.jpg",
      "original_url": "https://blog.example.com/.../banner.jpg",
      "original_mime": "image/jpeg",
      "original_width": 4000,
      "original_height": 2667,
      "original_size_bytes": 4521983
    }
  ]
}
```

**Response** `200 OK` — `{ "upserted_count": 1 }`.

### POST /agent/v1/media/presign — request upload URLs for sources

The agent asks for presigned PUT URLs to upload each source variant for a job.

**Request**

```json
{
  "job_id": "01J9Z…",
  "variants": [
    { "name": "full", "source_size": 4521983, "source_mime": "image/jpeg" },
    { "name": "medium", "source_size": 28119, "source_mime": "image/jpeg" }
  ]
}
```

**Response** `200 OK`

```json
{ "uploads": { "full": "https://storage/.../src/full?X-Amz-…", "medium": "https://storage/.../src/medium?X-Amz-…" } }
```

### POST /agent/v1/media/encode-ready — sources uploaded, enqueue encode

Signals that every source variant is in storage; the CP enqueues the
`media_encode` River job.

**Request**

```json
{
  "job_id": "01J9Z…",
  "variants": [ { "name": "full", "source_size": 4521983, "source_mime": "image/jpeg" } ]
}
```

**Response** `200 OK` — `{ "ok": true }`.

### POST /agent/v1/media/job-status — report apply results

Fired after the agent downloads the optimized outputs, applies them on disk,
rewrites the DB, and updates the postmeta blob.

**Request**

```json
{
  "job_id": "01J9Z…",
  "applied_variants": ["full", "thumbnail", "medium", "large"],
  "sizes_unoptimized": { "scaled": "Unsupported source format" },
  "current_format": "avif",
  "current_size_bytes": 412007,
  "bytes_before": 4521983,
  "bytes_after": 412007,
  "compression_level": "lossy",
  "target_format": "avif",
  "rewrite_stats": { "post_content_rows": 3, "postmeta_rows": 12 },
  "error": ""
}
```

**Response** `200 OK` — `{ "ok": true }`. The CP finalizes the job + asset rows
and deletes the job's `src/*` and `out/*` temp objects.

### POST /agent/v1/media/restore-status — report restore results

**Request**

```json
{ "job_id": "01J9Z…", "restored": true, "error": "" }
```

**Response** `200 OK` — `{ "ok": true }`.

---

## SSE events (`media.*`)

Media progress streams over the shared tenant bus
**`GET /api/v1/sites/events`** (see [api/sites.md](./sites.md)), filtered by
`site_id` and registered in `SITE_EVENT_TYPES`. Each frame carries the
standard envelope (`id`, `type`, `tenant_id`, `site_id`, `ts`, `data`).

| `event:` | Emitted when | Notable `data` |
|----------|--------------|----------------|
| `media.sync.started` | A sync job starts. | `job_id` |
| `media.sync.completed` | A sync job finishes. | `job_id` |
| `media.optimize.started` | An optimize batch starts. | `job_id`, `queued_count` |
| `media.optimize.progress` | A variant finishes encoding. | `job_id`, `variant`, `phase` |
| `media.optimize.asset_done` | An attachment's variants are all applied. | `job_id`, `asset_id` |
| `media.optimize.completed` | An optimize batch finishes. | `job_id` |
| `media.restore.started` | A restore batch starts. | `job_id` |
| `media.restore.asset_done` | An attachment is restored. | `job_id`, `asset_id` |
| `media.restore.completed` | A restore batch finishes. | `job_id` |
| `media.delete_originals.completed` | Originals deleted for an asset/batch. | `job_id` |
| `media.job.failed` | A job (or all its variants) failed. | `job_id`, `reason` |

> **Frontend registration.** A `media.*` type not present in `SITE_EVENT_TYPES`
> (`apps/web/src/features/sites/use-site-events.ts`) is silently dropped by the
> Zod `z.enum` validation — every event above is listed there.
