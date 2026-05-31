# FlyingPress 5.4.5 Image Optimizer — Pattern Playbook (for WPMgr Media Optimizer)

> **Provenance.** This is a read-only analysis of a *different* project's code (FlyingPress 5.4.5),
> kept at `reference/flying-press/`. We mirror its proven *orchestration patterns* under WPMgr's own
> naming/architecture (cloud-encode + signed-multipart + SSE). We do **not** copy code verbatim.
>
> Primary file: `reference/flying-press/src/ImageOptimizer.php` (cited as `ImageOptimizer.php:N`).
> Supporting: `src/Optimizer/Image.php`, `assets/htaccess.txt`, `src/RestApi.php`, `src/Queue.php`.

---

## Pattern 1 — The postmeta blob (single source of truth for restore)

**Meta key:** `flying_press_image_optimizer_data`, declared `ImageOptimizer.php:8`. Stored per-attachment
via `update_post_meta($image_id, self::META_KEY, [...])`.

The blob is **written in two different shapes** depending on lifecycle stage. This is critical: the keys are
NOT all always present.

### 1a. Full optimized shape — written by `update_image_metadata()` (`ImageOptimizer.php:228-247`)

```php
[
  'status'            => 'optimized',                  // presence gates "already processed" everywhere
  'compression_level' => Config::$config['image_compression_type'], // e.g. 'lossy' | 'lossless' | 'glossy'
  'sizes_unoptimized' => [                             // map<size_name, human-readable reason string>
      'medium' => 'Unsupported image format: image/gif',
      'full'   => 'File does not exist',
      // merged with PREVIOUS run's sizes_unoptimized (line 241-244)
  ],
  'sizes_optimized'   => ['full', 'large', 'medium'],  // flat list = array_keys(optimized_data) (line 245)
  'original_data'     => $metadata,                    // <-- VERBATIM snapshot of _wp_attachment_metadata
                                                       //     BEFORE optimization (line 246). The restore anchor.
]
```

Note: the in-flight working blob (during `optimize_single_image`) *also* carries two transient keys that are
**flattened away** before the final write:
- `optimized_data` (map<size_name, per-size record>) — see 1b below; persisted under this key, see line 234/249.
- `replacements` (map<original_url => optimized_url>) — used to drive the DB rewrite, see line 104, 111.
- `unoptimized` (transient name for the working reason map) — merged into `sizes_unoptimized` at line 241.

Important subtlety: in the working array the code uses `optimization_data['unoptimized']` (key `unoptimized`,
e.g. lines 60, 66, 84) but the *persisted* key is `sizes_unoptimized` (line 241). They are the same concept
under two names across the in-memory vs persisted boundary.

### 1b. Per-size optimized record — each value in `optimized_data[$size]`

Built at `ImageOptimizer.php:93,105` as `array_merge($response, self::get_image_data(...))`. After merge each record holds:

```php
[
  // from get_image_data() (ImageOptimizer.php:862-866):
  'url'           => 'https://site/wp-content/uploads/2024/05/banner.avif',
  'path'          => '/var/www/.../uploads/2024/05/banner.avif',   // absolute fs path
  'relative_path' => '2024/05/banner.avif',                        // _wp_relative_upload_path()
  // from the optimizer API response (fetch_optimized_image, ImageOptimizer.php:157-161):
  'size'          => 48213,                  // optimized byte size (x-optimized-size header)
  'mime_type'     => 'image/avif',           // content-type header
  // ('body' is unset at line 102 before persisting — raw bytes never stored in meta)
]
```

`sizes_optimized` (the flat list at 1a) is literally `array_keys($optimized_data)` (line 245). The full
per-size `optimized_data` map *is* persisted (line 234 reads it back; it is part of the stored array because
`update_post_meta` at 238 stores the whole `$optimization_data` is NOT — re-read: line 238 stores a *curated*
array that does **not** include `optimized_data` or `replacements`). The live per-size records live in the
WP core `_wp_attachment_metadata` (rewritten at lines 254-275), not in this blob. The blob keeps only the
**list** `sizes_optimized` + the **`original_data` snapshot** needed to reverse everything.

### 1c. Reduced shape after restore — written by `restore_single_image()` (`ImageOptimizer.php:213-221`)

- If `sizes_unoptimized` is empty → the meta is **deleted entirely** (`delete_post_meta`, line 215).
- Otherwise only a stub is kept so we don't re-attempt known-bad sizes:
```php
['compression_level' => ..., 'sizes_unoptimized' => [...]]
```

### 1d. The delete-originals flag

`delete_original_image()` (`ImageOptimizer.php:439-448`) sets `$optimizer_data['original_deleted'] = 1` and
re-saves. There is **no separate version/generation integer** in this plugin — `compression_level` doubles as
the "regenerate trigger": if the configured level differs from the stored one, the whole meta is dropped and
*all* sizes re-queued (`get_optimizable_image_sizes`, `ImageOptimizer.php:120-127`).

> **WPMgr adaptation note.** Make the equivalent blob a **typed, versioned JSON document** keyed per attachment,
> stored agent-side in WP postmeta *and* mirrored as the authoritative record in our Postgres `media_variants`
> table (CP is source of truth; agent meta is the recoverable cache). Add the fields FlyingPress lacks:
> an explicit `schema_version` int and an explicit `compression_profile_id` (don't overload it as the
> regen-trigger — keep a separate `regenerate_reason`). Keep `original_metadata` as the verbatim pre-optimize
> `_wp_attachment_metadata` snapshot — this is the **non-negotiable restore anchor**. Per-variant records should
> additionally carry the cloud-encode job id and the multipart upload etag so a restore/audit can prove which
> bytes landed.

---

## Pattern 2 — The `.flying-press-original.*` rename trick (archive / restore)

The whole scheme hinges on one question: **does the optimized output use the SAME extension as the source, or a
DIFFERENT one?** This single branch decides whether the original is archived or left in place.

### 2a. The extension helper — `change_extesion()` (`ImageOptimizer.php:871-884`)

(sic — misspelled "extesion" in the source). Pure string op:
- returns input unchanged if the ext already matches (line 879-881),
- otherwise replaces the trailing `.<ext>` via `preg_replace('/\.[^\.\/]+$/', '.'.$new, ...)` (line 883).

Used to manufacture both real new paths *and* the synthetic `flying-press-original.<ext>` "double extension"
by passing `'flying-press-original.' . $ext` as the new extension (lines 90, 192-197, 777, 838).

### 2b. The rename engine — `rename_files()` (`ImageOptimizer.php:766-781`)

```php
$new_file = $restore
  ? str_replace('.flying-press-original', '', $file)              // restore: strip the marker
  : self::change_extesion($file, 'flying-press-original.' . $ext); // archive: banner.jpg -> banner.flying-press-original.jpg
rename($file, $new_file);
```
Skips missing files; dedups via `array_unique`.

### 2c. WHEN archiving happens — the two scenarios

**Scenario A — SAME extension (re-compress original format, `image_format === 'original'`):**
- Triggered only when `!$already_optimized && 'original' === Config::$config['image_format']` (`ImageOptimizer.php:89`).
- Order of operations, per size, inside `optimize_single_image`:
  1. `rename_files([$image_path])` → archive the original bytes to `banner.flying-press-original.jpg` (line 90).
  2. `file_put_contents($optimized_data['path'], $response['body'])` → write NEW optimized bytes **at the original path** `banner.jpg` (line 98).
  3. Record `replacements[$image_url] = $optimized_data['url']` — but here URL == original URL (same ext), so the DB rewrite is effectively a no-op for the URL; the file at the public URL is now the smaller one.
- Result: public URL is unchanged, original bytes preserved under the double-extension archive name.

**Scenario B — DIFFERENT extension (JPG → AVIF or JPG → WebP, `image_format` ∈ {`avif`,`webp`}):**
- **No archive.** `get_image_data($url, 'optimized')` (line 93) computes a *new* path with the new ext via
  `change_extesion` (`ImageOptimizer.php:852-856`). Optimized bytes written to `banner.avif`; `banner.jpg`
  stays untouched on disk. Both coexist.
- The public HTML/DB now references `banner.avif` (via `replacements`, line 104). Browsers that can't decode
  AVIF/WebP get `banner.jpg` served by the **.htaccess Accept fallback** (Pattern 3) — that is *why* no archive
  is needed: the original IS the fallback.

### 2d. Restore ordering — `restore_single_image()` (`ImageOptimizer.php:164-226`)

Per optimized size (`sizes_optimized` loop, line 176):
- Compute `original_ext` from the snapshot's `original_data['file']` (line 173), and `optimized_ext` from the
  current attachment URL (line 178). `$restoring_original = ($original_ext === $optimized_ext)` (line 187).
- **Always** queue the *optimized* file for deletion: `optimized_files[] = change_extesion($original_path, $optimized_ext)` (line 186).
- If `$restoring_original` (Scenario A): the original is the archived double-extension file → queue
  `banner.flying-press-original.jpg` for **un-rename** (lines 189-198), and set the DB replacement target to the
  un-marked URL.
- If NOT (Scenario B): the JPG was never touched → DB replacement maps the `.avif` URL back to the `.jpg` URL
  with **empty string** for the file part (line 200: `$replacements[$optimized_url] = '';` when restoring original
  is false — actually it sets the original URL; re-read: line 200 assigns `$original_url` when NOT restoring
  original, `''` when restoring original — because in Scenario A the URL is unchanged so no DB rewrite needed).
- Execution order (lines 203-204): **delete optimized files FIRST, then un-rename archives.** Then restore the
  WP metadata from `original_data` (lines 207-211: `update_guid`, `update_attached_file`, `wp_update_attachment_metadata`).

> **WPMgr adaptation note.** Replicate the **same-ext-vs-different-ext decision as an explicit enum on each variant
> record** (`archive_mode: replace_in_place | coexist`). Since our bytes live in cloud storage + are pushed via
> signed multipart, "archive the original" = retain the pre-optimize object under an immutable key (versioned
> bucket / `original/` prefix) rather than an on-disk double extension. The agent still needs the on-disk fallback
> file for the .htaccess path, so when `archive_mode = replace_in_place` the agent writes the new bytes at the
> public path and stashes the original both locally (double-extension) and in our object store for durability.
> Keep FlyingPress's **delete-optimized-before-unrename ordering** to avoid a window where two files claim one URL.

---

## Pattern 3 — Accept-header `.htaccess` fallback (`assets/htaccess.txt:69-106`)

Lives inside `# BEGIN FlyingPress` / `# END FlyingPress` markers (lines 1, 160) — the standard WP managed-block
convention (insert/replace between markers, never touch the rest of the file).

### 3a. The rewrite logic (`htaccess.txt:70-99`)
For each modern format, three rules (one per legacy ext png/jpg/jpeg), all guarded by **two conditions**:
```apache
RewriteCond %{HTTP_ACCEPT} !image/avif [NC]        # client did NOT advertise avif support
RewriteCond %{DOCUMENT_ROOT}/$1.png   -f           # and a .png twin actually exists on disk
RewriteRule ^(.+)\.avif$ $1.png [L]                # serve the .png instead, stop (L)
```
Repeated for `.jpg`, `.jpeg`; then the identical block for `!image/webp`. So the *requested* URL is always the
modern `.avif`/`.webp` (matching what we wrote into the DB), and Apache transparently downgrades only when the
browser can't decode it AND a legacy twin exists.

### 3b. The Vary header (`htaccess.txt:102-106`)
```apache
<FilesMatch "\.(avif|webp)$">
  Header merge Vary Accept
</FilesMatch>
```
`Vary: Accept` so shared caches / CDNs key the cache entry on the Accept header — prevents a no-AVIF client from
being poisoned with an AVIF response cached for an AVIF-capable client (and vice-versa).

> **WPMgr adaptation note.** Two delivery options. (1) If serving from origin Apache, generate the same managed
> block under a `# BEGIN WPMgr-Media` / `# END WPMgr-Media` marker via the agent, and **always** require the
> `-f` existence guard so a missing twin never 404s. (2) Since we control the CDN/edge, prefer doing the
> Accept negotiation at the edge (Cloud CDN / worker): rewrite `.avif`→`.jpg` on `Accept` mismatch and set
> `Vary: Accept`. Either way the DB/HTML must reference the modern URL and the legacy twin must exist — both
> are guaranteed by Pattern 2's "coexist" mode. Don't forget Nginx hosts have no `.htaccess`; the edge path
> covers them.

---

## Pattern 4 — WP Media Library modal injection + edit-screen meta box

Three hooks wired in `init()` (`ImageOptimizer.php:26-28`):
- `wp_prepare_attachment_for_js` → `individual_image_stats` (line 26)
- `add_meta_boxes_attachment` → `individual_image_stats_meta_box` (line 27)
- `admin_footer-upload.php` → `set_stats_in_media_modal` (line 28)

### 4a. Injecting stats into the JS attachment model — `individual_image_stats()` (`ImageOptimizer.php:647-656`)
Gatekeeps on optimizable source mime, then stuffs pre-rendered HTML into the JS-serialized attachment:
```php
$response['flying_press_image_optimizer'] = self::get_image_stats_html($attachment);
```
So every attachment's Backbone model gains a `flying_press_image_optimizer` string attribute.

### 4b. The Backbone monkey-patch — `set_stats_in_media_modal()` (`ImageOptimizer.php:740-757`)
Printed in the upload-screen footer. It wraps `wp.media.view.Attachment.prototype.render`:
```js
const originalRender = attachment.prototype.render;
attachment.prototype.render = function () {
  originalRender.apply(this, arguments);
  const html = this.model.get("flying_press_image_optimizer");
  if (!html) return;
  this.el.querySelector(".settings")?.insertAdjacentHTML(
    "beforebegin",
    `<div class="flying-press-image-stats details">${html}</div>`
  );
};
```
i.e. call core render first, then insert the stats panel **`beforebegin` of `.settings`** (above the standard
attachment detail fields). Null-safe (`?.`) so it no-ops on themes/screens without `.settings`.

### 4c. The edit-screen meta box — `individual_image_stats_meta_box()` (`ImageOptimizer.php:658-674`)
Adds a `side` context meta box titled "FlyingPress optimization" on the `attachment` post type, whose callback
echoes the **same** `get_image_stats_html()` output. One renderer, two surfaces.

### 4d. The stats HTML shape — `get_image_stats_html()` (`ImageOptimizer.php:676-738`)
Conditional, top-to-bottom:
1. **Not optimized / excluded** (no `status`): a single `Status:` line, value is
   `"Excluded from Optimization"` if the filename matches an exclude keyword, else `"Not Optimized yet"` (685-690).
2. **Status line** (696-701): `Status: Optimized` if `sizes_optimized` non-empty, else `Not Optimized`.
3. **Total Size before→after** (704-714): only if something was optimized. Renders
   `size_format(original_total) → size_format(optimized_total)` using `calculate_image_stats()` over the
   `original_data` snapshot vs the live metadata. (Note: it shows before→after; the saved-% is computable but
   this build renders the two absolute sizes with an arrow, not an explicit percent.)
4. **Sizes not optimized** (716-735): a header line then one `<code>{dim}</code>: {reason}` row per entry in
   `sizes_unoptimized`, where dim is `"Full"` for `full` else `"{width}x{height}"` pulled from
   `original_data['sizes'][$size]`.
   (No literal "powered-by" footer string exists in this 5.4.5 source — the "FlyingPress optimization" meta-box
   title is the only branding surface.)

> **WPMgr adaptation note.** Same two-surface strategy, one renderer. Inject a `wpmgr_media_optimizer` attribute
> via `wp_prepare_attachment_for_js`, and wrap `wp.media.view.Attachment.prototype.render` to mount our panel
> `beforebegin` of `.settings` — but render an **empty mount node** and hydrate it from our SSE/status channel
> rather than baking a static HTML string, so the modal reflects live cloud-encode progress (queued → encoding →
> uploaded → live) without a page reload. Keep the meta box for the classic edit screen. Show explicit
> **saved-% and per-size reason chips**, and add the branding footer FlyingPress omits.

---

## Pattern 5 — Per-size optimization status + reasons

The reason map is built **inline as each size is processed** in `optimize_single_image()`:
- `'File does not exist'` — source path missing (`ImageOptimizer.php:60`).
- `'Unsupported image format: <mime>'` — source mime not in `MIME_OPTIMIZABLE` (jpeg/jpg/png) (`:66`).
- `$response['error']` — the optimizer API returned a soft error (HTTP 413/422/415, e.g. too-large /
  unprocessable / unsupported-by-server); recorded verbatim and the size skipped (`:83-86`).
- Hard errors (HTTP ≥500 or any unexpected status) **throw** (`:78-79`) → the whole task fails and Action
  Scheduler can retry; they are NOT recorded as a per-size reason.

"Already optimized this run" dedup: `$optimized[$image_url]` caches a size whose URL was already optimized so
identical-URL sizes reuse the same response instead of re-calling the API (`:70-74, 107`).

Skip-if-known: `get_optimizable_image_sizes()` (`:116-130`) computes
`array_diff(all_sizes, keys(sizes_unoptimized))` — so previously-failed sizes are not retried unless the
compression level changed (which nukes the meta and retries everything, `:120-127`).

The persisted result: `sizes_optimized` (flat list) + `sizes_unoptimized` (map<size,reason>), merged with the
prior run's reasons so history accumulates (`:241-245`). The Media UI (Pattern 4d) renders these directly.

> **WPMgr adaptation note.** Model per-variant status as a small enum (`optimized | skipped_unsupported |
> skipped_too_small | skipped_already | errored_retryable | errored_permanent`) plus a free-text detail, instead
> of a bare reason string — so the UI can render typed chips and River can decide retry vs give-up. Keep
> FlyingPress's "don't retry known-bad sizes unless the profile changed" rule, and keep the **accumulate-across-runs**
> merge so partial re-optimizations don't lose earlier reasons.

---

## Pattern 6 — The DB URL rewriter (the most error-prone surface)

Entry point `replace_images()` (`ImageOptimizer.php:407-411`) calls two siblings; both take a
`map<old_url => new_url>` ($replacements) and both are **LIMITed per pass** (multi-pass implied by the queue
re-running over batches).

### 6a. post_content rewrite — `replace_images_in_post_content()` (`ImageOptimizer.php:291-338`)
- Candidate selection by SQL: `post_content LIKE '%"<url>"%'` per replacement (note the **embedded quotes** in
  the LIKE pattern, line 300-301 — biases toward quoted attribute/JSON occurrences), restricted to public
  post types, `post_status='publish'`, **`ORDER BY post_date DESC LIMIT 100`** (line 313).
- Replacement by regex (lines 319-325):
  ```php
  '/' . preg_quote($url, '/') . '(?=([^0-9A-Za-z]|$))/'
  ```
  The **trailing lookahead** `(?=([^0-9A-Za-z]|$))` is the crux: it asserts the matched URL is followed by a
  non-alphanumeric char or end-of-string, so `banner.jpg` does **not** match inside `banner.jpg.bak` or
  `banner.jpg2` (the `.` / `2` / `b` after would otherwise let a naive replace corrupt unrelated paths).
- Writes back only when content actually changed; returns affected post IDs.

### 6b. postmeta rewrite — `replace_images_in_postmeta()` (`ImageOptimizer.php:340-405`)
- Candidate selection (lines 348-357) uses **two** conditions per replacement: a quoted `LIKE` *and* a
  `REGEXP '<escaped-url>([^0-9A-Za-z]|$)'` — the REGEXP applies the same boundary guard at SQL level so
  serialized/unquoted occurrences are caught too.
- Join to posts; public types; publish; **`ORDER BY pm.meta_id DESC LIMIT 200`** (line 368).
- **SKIPPED meta keys** (line 367):
  `_wp_attached_file`, `_wp_attachment_metadata`, `flying_press_image_optimizer_data`,
  `_wp_attachment_backup_sizes` — these are managed by WP core / by the optimizer itself and must NOT be
  string-rewritten (they're the canonical attachment records and the restore anchor).
- Per-row format handling (lines 378-387):
  - **Serialized PHP** (`is_serialized`): `maybe_unserialize` → `recursive_replace` → `maybe_serialize`.
    Going through (un)serialize is what keeps the PHP serialized **length prefixes** (`s:NN:"..."`) correct
    after the string length changes — a plain `str_replace` on serialized data corrupts it.
  - **JSON** (decodes cleanly to array — Elementor / Beaver Builder store JSON-in-postmeta): `json_decode` →
    `recursive_replace` → `json_encode`.
  - **Plain string** fallback: `str_replace(keys, values, value)` (line 386).
- Batched write-back by `meta_id` (lines 396-402).

### 6c. The recursive walker — `recursive_replace()` (`ImageOptimizer.php:413-437`)
Rebuilds the **same** boundary-guarded regex (lines 416-419), then:
- string → `preg_replace($patterns, $replacements, $data)` (424),
- array → recurse each value (426-429),
- object → recurse each property (430-434).
This is what makes deeply nested page-builder structures (arrays of objects of strings) safe to rewrite.

> **WPMgr adaptation note.** This is the single highest-risk surface to mirror — port it almost behavior-for-behavior
> (NOT line-for-line). Non-negotiables: (1) the **trailing `(?=([^0-9A-Za-z]|$))` boundary** on every URL pattern
> (in SQL REGEXP *and* in the PHP regex) to avoid partial-substring corruption; (2) **(de)serialize round-trips**
> for serialized PHP so length prefixes stay valid; (3) **JSON-aware** decode/encode for Elementor/Beaver/Gutenberg
> block JSON; (4) the **skip-list** of core/optimizer-owned meta keys; (5) **bounded LIMIT per pass + multi-pass**
> so a huge site rewrites incrementally. In WPMgr, the agent runs this locally (it has DB access) and reports
> affected-row counts back to CP over the status/SSE channel so River can show rewrite progress and detect stalls.
> CP must treat the URL-rewrite as a *separate, resumable phase* after bytes are confirmed live.

---

## Pattern 7 — Queue / batching model (Action Scheduler)

### 7a. The Queue wrapper — `src/Queue.php`
- One `Queue` per concern, constructed in `ImageOptimizer::init()` (`ImageOptimizer.php:30-33`):
  `image-optimize`, `image-restore`, `image-delete`, `purge-cache` — each binds a `group_name` + a
  `callback_action` hook.
- **Batch size 100** per runner pass: `add_filter('action_scheduler_queue_runner_batch_size', fn() => 100)`
  (`Queue.php:35`).
- `add_task()` (`Queue.php:38-69`): dedups by querying for an existing pending action with identical args; if
  found just bumps `priority`, else `as_enqueue_async_action(callback, args, group, false, priority)`.
- `start_queue()` (`Queue.php:71-79`): fires a **non-blocking** loopback `admin-ajax.php?action=flying_press_run_queue`
  (`timeout=0.01, blocking=false`) which runs `ActionScheduler::runner()->run($group)` — i.e. it kicks the
  runner immediately rather than waiting for WP-Cron.
- `clear_queue()` = `as_unschedule_all_actions('', [], $group)` (`Queue.php:92-95`).
- Retention zeroed so finished tasks are pruned (`Queue.php:16`).

### 7b. The fan-out — `process_queue()` (`ImageOptimizer.php:493-534`)
- Clears the group, suspends cache addition, then paginates attachments with `WP_Query`
  **`posts_per_page => 2000`, `fields => 'ids'`, `no_found_rows`** (lines 501-520), enqueuing each id as its own
  task (`queue_images`, `:536-546`).
- Priorities: optimize/restore default **15**, delete uses **20** (`:490`) so deletes run after.
- After enqueue, unless it's the delete queue, it also enqueues a single `purge-cache` task (`:528-530`), then
  `start_queue()`.

### 7c. Per-task pacing
Each `optimize_single_image` / `restore_single_image` / `delete_original_image` ends with
`usleep(apply_filters('flying_press_image_optimizer_delay', 0.5) * 1_000_000)` (`:113, 225, 447`) — a
filterable ~0.5s breather to avoid hammering the optimizer API / disk.

### 7d. Status & stop (shape only)
- `get_status()` (`ImageOptimizer.php:555-619`): keyset-paginated SQL over attachments (batch 1000, `ID < last_id`
  descending) summing `original_size` vs `optimized_size`, counting `total_images` / `processed_images`, plus live
  `images_in_queue` / `restore_in_queue` / `delete_in_queue` from `get_pending_count()`.
- `stop_optimization()` (`:621-626`): clears the optimize + purge-cache queues.

### 7e. REST surface — `src/RestApi.php:55-83`
Namespaced `flying-press/image-optimizer` with five POST endpoints, each a thin pass-through to the matching
`ImageOptimizer` static: `/optimize/`, `/restore/`, `/delete/`, `/stop/`, `/status/`. All gated by
`Auth::is_allowed()` once at registration (`RestApi.php:15`), then `permission_callback => '__return_true'`.
Returns plain arrays (`{success: bool}` for actions; the status object for `/status/`).

> **WPMgr adaptation note.** Mirror the **cadence**, not the transport. River replaces Action Scheduler as the
> orchestrator: CP enqueues per-attachment jobs, the agent pulls/receives them, encodes happen in our cloud, and
> bytes flow back via **signed multipart**. Match FlyingPress's chunking: fan out per-attachment, bounded batch
> (their 100/runner-pass ≈ our batch), and the prompt's constraint of **≤10 variants per multipart request**
> maps cleanly onto "one attachment = its full size-set, split into ≤10-variant multipart parts." Keep the
> **separate priority lanes** (optimize=normal, restore=normal, delete=lower) and a per-job pacing knob equivalent
> to the 0.5s `usleep`. Replace the loopback-ajax "kick the runner" hack with River's native dispatch. Surface
> `/status/` as a live SSE stream (we already have the LISTEN/NOTIFY SSE bus) instead of poll-only, but keep a
> poll endpoint for parity. Keep the dedup-by-args behavior so re-enqueues don't double-process an attachment.

---

## Summary of the 7 patterns

1. **Postmeta blob** = the restore bible: `status`, `compression_level`, `sizes_optimized[]`,
   `sizes_unoptimized{size→reason}`, and the verbatim `original_data` snapshot of `_wp_attachment_metadata`;
   shrinks to a stub or vanishes on restore; gains `original_deleted=1` when originals are purged.
2. **`.flying-press-original.*` rename** = archive-vs-coexist driven *entirely* by same-ext vs different-ext.
3. **`.htaccess` Accept fallback** = serve legacy twin when `!image/avif`/`!image/webp` **and** the twin exists,
   with `Vary: Accept`, inside BEGIN/END markers.
4. **Media modal injection** = `wp_prepare_attachment_for_js` attribute + a `render` monkey-patch that mounts a
   stats panel `beforebegin` of `.settings`, plus a twin meta box on the edit screen, one shared HTML renderer.
5. **Per-size status/reasons** = inline reason map (missing file / unsupported mime / API soft-error), retryable
   hard-errors throw, known-bad sizes skipped unless the compression profile changes.
6. **DB URL rewriter** = boundary-guarded regex + (de)serialize round-trips + JSON-aware recursion + core-meta
   skip-list + bounded multi-pass.
7. **Queue/batching** = Action Scheduler, one group per concern, batch 100, per-attachment fan-out (2000/page
   query), priority lanes, ~0.5s pacing, loopback-ajax kick, thin REST pass-throughs.

## The single most subtle thing an implementer MUST get right

**It's a tie between two, and both are about *not corrupting irreversible state*:**

1. **The same-ext-vs-different-ext archive branch (Pattern 2).** Get this wrong and you either (a) overwrite the
   original with optimized bytes while *thinking* you archived it (data loss, no restore), or (b) leave a stale
   AVIF claiming the JPG's URL after a restore. The invariant: **same-ext ⇒ archive original to a
   double-extension name then write in place; different-ext ⇒ never archive, both coexist and the Accept
   fallback covers legacy clients.** And on restore, **delete optimized files before un-renaming archives.**

2. **The serialized/JSON-aware DB rewrite with the trailing boundary lookahead (Pattern 6).** A plain
   `str_replace` over serialized PHP breaks the `s:NN:` length prefixes (silently un-unserializable meta), and a
   regex without `(?=([^0-9A-Za-z]|$))` will rewrite `banner.jpg` inside `banner.jpg.bak`. Either bug corrupts
   page-builder content irreversibly. **Always (de)serialize-round-trip, JSON-decode page-builder meta, recurse
   nested structures, honor the core-meta skip-list, and bound the URL match on a trailing non-alphanumeric.**

If forced to pick one: **Pattern 6's serialized-data rewrite** — Pattern 2's failure is loud and detectable
(missing/wrong file), whereas a botched serialized rewrite fails *silently* and only surfaces when a page builder
later can't parse its own meta.
