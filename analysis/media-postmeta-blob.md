# Agent postmeta blob — `wpmgr_image_optimization` (Phase 2 spec)

The **single source of truth on the WP site** for an attachment's optimization
state. Stored as one postmeta row per attachment under the key
**`wpmgr_image_optimization`** (`MediaKeystore::KEY`). Shaped after FlyingPress's
`flying_press_image_optimizer_data` (see `reference/flying-press-patterns.md` §1)
but under WPMgr naming. The CP's `site_media_assets` row is a **mirror** of this
blob synced via the agent→CP callbacks; the blob on the site is authoritative for
**restore** (the CP holds no image bytes).

> Difference from FlyingPress: we add an explicit **`wpmgr_job_id`** (CP job
> cross-ref for audit) and **`wpmgr_generation`** int (FlyingPress overloads
> `compression_level` as its regen trigger; we keep an explicit counter, mirrored
> to `site_media_assets.generation`).

## Lifecycle shapes

The blob exists in one of three shapes (FlyingPress pattern §1):

1. **Optimized** — full blob (below). `status = 'optimized'`.
2. **Reduced stub after restore** — if some sizes were *unoptimizable*, restore
   keeps a minimal blob `{ compression_level, sizes_unoptimized }` so we don't
   retry known-bad sizes under the same profile. If nothing was unoptimizable,
   the blob is **deleted entirely** on restore.
3. **Originals deleted** — full blob + `original_deleted = 1`. Restore is then
   impossible (the archived originals are gone); the CP asset goes
   `status = 'originals_deleted'`.

## Full blob shape (`status = 'optimized'`)

```php
[
  'wpmgr_job_id'      => 'job_01J9Z...',     // CP media_optimization_jobs.id (audit cross-ref)
  'wpmgr_generation'  => 2,                   // increments each (re)optimization; mirrors asset.generation
  'status'            => 'optimized',         // 'optimized' | 'excluded' | 'originals_deleted'
  'compression_level' => 'lossy',             // 'lossy' | 'lossless' at time of optimize
  'target_format'     => 'avif',              // 'avif' | 'webp' | 'original'

  // Which registered sizes were optimized, and which weren't (+ why).
  'sizes_optimized'   => ['full', 'thumbnail', 'medium', 'large'],
  'sizes_unoptimized' => [                    // map<size_name, human reason>
    'large' => 'Unsupported source format',
  ],

  // SNAPSHOT of _wp_attachment_metadata BEFORE optimization — the restore bible.
  // Verbatim copy so restore reproduces the pre-optimization tree exactly.
  'original_data' => [
    'file'     => '2026/05/banner.jpg',
    'filesize' => 4521983,
    'width'    => 4000,
    'height'   => 2667,
    'sizes'    => [
      'thumbnail' => ['file' => 'banner-150x150.jpg', 'filesize' => 18445, 'width' => 150, 'height' => 150, 'mime-type' => 'image/jpeg'],
      'medium'    => ['file' => 'banner-300x200.jpg', 'filesize' => 28119, 'width' => 300, 'height' => 200, 'mime-type' => 'image/jpeg'],
      // ... every registered size
    ],
  ],

  // Per-size result snapshot (what the optimized file IS now).
  'optimized_data' => [
    'full' => [
      'size'          => 412007,               // optimized bytes
      'mime_type'     => 'image/avif',
      'url'           => 'https://site/.../banner.avif',
      'path'          => '/var/www/.../banner.avif',
      'relative_path' => '2026/05/banner.avif',
    ],
    // ... one entry per size in sizes_optimized
  ],

  // URL rewrite map applied to post_content + postmeta (used to REVERSE on restore).
  'replacements' => [
    'https://site/.../banner.jpg' => 'https://site/.../banner.avif',
    // ... per size where the extension changed
  ],

  'original_deleted'  => 0,                    // 1 only after "Delete originals"
]
```

## Field reference

| Field | Type | Meaning |
|---|---|---|
| `wpmgr_job_id` | string | CP job id (`media_optimization_jobs.id`, ULID). Audit cross-ref only. |
| `wpmgr_generation` | int | (Re)optimization counter; mirrors `site_media_assets.generation`. |
| `status` | string | `optimized` \| `excluded` \| `originals_deleted`. |
| `compression_level` | string | `lossy` \| `lossless` at last optimize. Re-runs only re-optimize known-bad sizes when this changes (FlyingPress §5). |
| `target_format` | string | `avif` \| `webp` \| `original`. |
| `sizes_optimized` | string[] | Registered size names successfully optimized (incl. `'full'`). |
| `sizes_unoptimized` | map<string,string> | size → human reason (`Unsupported source format`, `Missing file`, `Source too small`, encoder reason). |
| `original_data` | object | Verbatim `_wp_attachment_metadata` snapshot pre-optimize. **Restore bible.** |
| `optimized_data` | map<string,object> | per size: `{size, mime_type, url, path, relative_path}`. |
| `replacements` | map<string,string> | original_url → optimized_url, applied to DB; reversed on restore. |
| `original_deleted` | int(0/1) | 1 after delete-originals → restore impossible. |

## Restore decision logic (drives `MediaRestoreCommand`)

Per FlyingPress §2 (same-ext vs different-ext archive), keyed off `original_data`'s
extension vs each `optimized_data` entry's mime:

- **Different extension** (JPG→AVIF/WebP): the original was **never archived**
  (both files coexist). Restore = delete the optimized file + reverse the URL
  replacement; the `.jpg` is already in place.
- **Same extension** (`target_format = 'original'`, re-compressed): the original
  was **archived** to `…​.wpmgr-original.<ext>`. Restore = delete the optimized
  file at the original path, then `Rename::restore()` the archive back, and reverse
  any URL replacement.
- Always restore `_wp_attachment_metadata` from `original_data`, run the URL
  rewriter in reverse, then **delete or reduce** the blob (lifecycle shape #2).
- If `original_deleted == 1` → restore is refused (CP returns
  `originals_deleted_cannot_restore`).

## CP mirror

The agent reports the blob's salient fields to the CP after each apply/restore;
the CP upserts `site_media_assets` (`current_format`, `current_size_bytes`,
`status`, `generation`, `compression_level`, `target_format`, `sizes_optimized`,
`sizes_unoptimized`, `last_optimized_at`). The blob stays authoritative on-site;
the CP row drives the dashboard. The CP **never** stores image bytes or the
`optimized_data.path`/`url` beyond what's needed to render the table (URLs are the
site's own public URLs).
