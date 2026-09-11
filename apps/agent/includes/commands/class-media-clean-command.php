<?php
/**
 * MediaCleanCommand — Unused Media Cleaner (issue #190).
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/media_clean
 *   Authorization: Bearer <Ed25519 JWT, cmd="media_clean", aud=<siteId>>
 *   Body: {
 *     "action":  "scan" | "isolate" | "restore" | "delete" | "list",
 *     "job_id":  "<UUID v4, required for isolate/restore/delete>",
 *
 *     // scan params
 *     "limit":   <int, default 100, max 500>,
 *     "offset":  <int, default 0>,
 *
 *     // isolate params
 *     "attachment_ids": [<int>, ...],   // IDs to quarantine
 *
 *     // restore / delete params
 *     "quarantine_ids": ["<id>", ...],  // manifest entry IDs
 *
 *     // delete params
 *     "confirm": "DELETE"               // must match exactly (hash_equals)
 *   }
 *
 * Responses by action:
 *
 *   scan ->
 *     {
 *       "ok": true,
 *       "total": <int>,                 // unused count (capped at SCAN_MAX)
 *       "candidates": [ {               // unused attachments (paginated by offset/limit)
 *           "id": <int>,
 *           "title": <string>,
 *           "url": <string>,
 *           "thumb": <string|null>,
 *           "file_size": <int>,
 *           "sizes_count": <int>
 *         }, ... ],
 *       "truncated": <bool>,            // true when library has more unused than SCAN_MAX
 *       "total_attachments": <int>,     // all attachment rows the walk visited
 *                                       // (excludes quarantined IDs — they are out-of-scope)
 *       "referenced_count": <int>,      // count classified in-use among those visited
 *       "unused_count": <int>,          // == total (alias kept for backward compat)
 *       "quarantined_count": <int>,     // attachment IDs excluded because they are
 *                                       // already present in a quarantine manifest;
 *                                       // those IDs are not walked, not counted in
 *                                       // total_attachments, and not added to candidates.
 *       "referenced": [                 // in-use attachments among those visited
 *         {
 *           "id": <int>,
 *           "title": <string>,          // post_title, else basename of file
 *           "url": <string>,            // guid
 *           "thumb": <string|null>,     // thumbnail-size URL or null
 *           "usages": [                 // >= 1 entry per reference location
 *             {
 *               "surface": <string>,         // see surface enum below
 *               "source_id": <int|null>,     // post/term/user/menu-item id
 *               "source_label": <string|null>, // post title, option name, etc.
 *               "edit_url": <string|null>,   // wp-admin edit link when applicable
 *               "detail": <string|null>      // meta_key, "wp-image-<id>", path, etc.
 *             }, ...
 *           ]
 *         }, ...
 *       ]
 *     }
 *
 *   Surface enum (canonical, singular, snake_case):
 *     post_content  — ID/URL extracted from a post's post_content field
 *     post_excerpt  — ID/URL extracted from a post's post_excerpt field
 *     revision      — ID/URL in a revision post (source_id = parent post ID)
 *     thumbnail     — _thumbnail_id featured image reference
 *     postmeta      — any other postmeta field (detail = meta_key)
 *     gallery       — [gallery] shortcode or product image gallery
 *     option        — wp_options row (source_label = option_name)
 *     widget        — widget option row (source_label = option key)
 *     menu          — nav_menu_item referencing the attachment directly
 *     term_meta     — wp_termmeta row (source_id = term_id)
 *     user_meta     — wp_usermeta row (source_id = user_id)
 *     direct_id     — matched by numeric attachment ID, source not attributed
 *     path          — matched by file-path fragment, source not attributed
 *
 *   isolate ->
 *     {
 *       "ok": true,
 *       "job_id": "<uuid>",
 *       "moved": <int>,
 *       "manifest_id": "<uuid>",
 *       "entries_recorded": <int>,           // entries written to manifest (including 0-file entries)
 *       "per_attachment": [                  // per-attachment moved file count
 *         { "attachment_id": <int>, "moved": <int> }, ...
 *       ]
 *     }
 *
 *   restore ->
 *     {
 *       "ok": true,                          // true even when incomplete; the counts carry that
 *       "job_id": "<uuid>",
 *       "restored": <int>,                   // files moved back to their original path
 *       "skipped": <int>,                    // files deliberately left in quarantine
 *       "failed": <int>,                     // files whose move was attempted and did not happen
 *       "detail": "<string>"                 // present only when skipped+failed > 0
 *     }
 *
 *   delete ->
 *     {
 *       "ok": true,
 *       "job_id": "<uuid>",
 *       "deleted": <int>,                    // backward-compat alias for posts_deleted
 *       "posts_deleted": <int>,              // attachments whose wp_delete_attachment returned truthy
 *       "posts_failed": <int>,               // attachment_id>0 where wp_delete_attachment returned false/null
 *       "files_deleted": <int>,              // quarantined files unlinked
 *       "entries_processed": <int>,          // total manifest entries seen
 *       "results": [                         // per-attachment outcomes
 *         { "attachment_id": <int>, "post_deleted": <bool>, "files_deleted": <int> }, ...
 *       ]
 *     }
 *
 *   list ->
 *     {
 *       "ok": true,
 *       "manifests": [
 *         {
 *           "manifest_id": "<string>",
 *           "job_id":      "<string>",
 *           "isolated_at": <int unix seconds>,
 *           "total_files": <int>,
 *           "entries": [
 *             { "attachment_id": <int>, "title": "<string>", "file_count": <int> }
 *           ]
 *         }, ...
 *       ]
 *     }
 *     Sorted newest-first by isolated_at. Read-only; no params required.
 *
 *   any error ->
 *     { "ok": false, "detail": "<reason>" }
 *
 * Safety model:
 *   LIST  — read-only; reads manifest files from disk and resolves attachment titles.
 *           No params required. No side effects.
 *   SCAN  — read-only; builds the reference index + queries attachment library.
 *   ISOLATE — moves original + all sub-size files for each selected attachment
 *             into wp-content/wpmgr-quarantine/media/{manifest_id}/.
 *             Writes a JSON manifest (see MediaQuarantine). Does NOT delete the
 *             attachment post (it remains in the media library as a broken link).
 *   RESTORE — moves files back from quarantine to their original paths using
 *             the manifest. Attachment post is already intact.
 *   DELETE  — permanently removes quarantined files + deletes the attachment post
 *             via wp_delete_attachment(). Requires "DELETE" confirm token.
 *
 * Batch / memory guards:
 *   scan:    walks the FULL attachment library (up to SCAN_MAX unused candidates)
 *            to build the unused list, then applies offset/limit to the UNUSED list.
 *            This means offset=0 returns the first N actual unused candidates, not
 *            the unused found among the first N attachments. The reference index
 *            uses $wpdb for all DB reads and avoids loading WP objects into memory.
 *   isolate: max 200 IDs per call.
 *   restore/delete: max 200 quarantine IDs per call.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Media\MediaQuarantine;
use WPMgr\Agent\Media\MediaReferenceIndex;
use WPMgr\Agent\MediaKeystore;
use WPMgr\Agent\Media\AttachmentMeta;

/**
 * Implements the media_clean command: scan, isolate, restore, delete.
 */
final class MediaCleanCommand implements CommandInterface
{
    /** Maximum candidates returned per scan page. */
    private const SCAN_MAX    = 500;
    /** Default scan page size. */
    private const SCAN_DEFAULT = 100;
    /**
     * Maximum referenced entries collected during a scan.
     * Mirrors SCAN_MAX to bound the referenced list the same way the unused
     * list is bounded, preventing a large media library from blowing up the
     * response payload and PHP memory.
     */
    private const REFERENCED_MAX = 500;
    /** Maximum attachment IDs per isolate call. */
    private const ISOLATE_MAX = 200;
    /** Maximum quarantine IDs per restore/delete call. */
    private const OP_MAX      = 200;
    /** Confirm token required for permanent deletion. */
    private const DELETE_CONFIRM = 'DELETE';
    /** Uploads-relative paths named in a restore `detail` before it summarises the rest. */
    private const RESTORE_DETAIL_PATHS = 5;

    /**
     * Optional quarantine instance injected for tests; null = create on demand
     * using the runtime WP_CONTENT_DIR constant.
     */
    private ?MediaQuarantine $quarantine;

    /**
     * @param MediaQuarantine|null $quarantine Inject a pre-configured quarantine
     *   instance for testing. Pass null (default) to let each action construct
     *   its own instance from the runtime WP_CONTENT_DIR constant.
     */
    public function __construct(?MediaQuarantine $quarantine = null)
    {
        $this->quarantine = $quarantine;
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'media_clean';
    }

    /**
     * Effect: worst case of five actions. action=delete permanently removes attachments via
     * wp_delete_attachment and unlinks their quarantined files; scan and list read, isolate and restore
     * move files in and out of a reversible quarantine.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Destructive;
    }

    /**
     * Repeatability: Unsafe, on the restore path -- not on delete, which an earlier pass wrongly blamed.
     * action=isolate and action=delete are both pinned by identifiers already in the request
     * (sanitiseIdList() / sanitiseStringList()); neither re-derives its target at run time, so the
     * earlier docblock's claim that a delete retry "removes whatever now matches" was wrong about the
     * code -- handleDelete() never re-scans.
     *
     * action=restore is likewise pinned by its quarantine_ids and re-derives nothing, and
     * MediaQuarantine::restoreManifest() now guards both ends of every move: a file already
     * occupying the original path is left alone and its quarantined counterpart is kept, so no
     * retry can consume either. It stays classified Unsafe because a retry is still not a no-op --
     * an incomplete restore deliberately keeps its manifest live, and a second call does more work
     * against a tree the first one changed. Unsafe here means "not idempotent", not "destructive".
     *
     * @return CommandRepeatability
     */
    public function repeatability(): CommandRepeatability
    {
        return CommandRepeatability::Unsafe;
    }

    /**
     * {@inheritDoc}
     */
    public function execute(array $claims, array $params): array
    {
        $action = isset($params['action']) && is_string($params['action'])
            ? strtolower(trim($params['action']))
            : '';

        return match ($action) {
            'scan'    => $this->handleScan($params),
            'isolate' => $this->handleIsolate($params),
            'restore' => $this->handleRestore($params),
            'delete'  => $this->handleDelete($params),
            'list'    => $this->handleList(),
            default   => ['ok' => false, 'detail' => 'unknown action; expected scan|isolate|restore|delete|list'],
        };
    }

    // =========================================================================
    // scan
    // =========================================================================

    /**
     * Read-only pass: identify attachment posts that have no reference anywhere
     * in the site's content. Walks the full attachment library (up to SCAN_MAX
     * unused candidates), then applies offset/limit to the UNUSED list so that
     * offset=0&limit=N always returns the first N actual unused candidates.
     *
     * Attachments whose IDs appear in any quarantine manifest on disk are excluded
     * from the walk entirely — they are not counted in total_attachments, not tested
     * for references, and not added to candidates. The count of excluded IDs is
     * returned as quarantined_count.
     *
     * @param array<string,mixed> $params
     * @return array<string,mixed>
     */
    private function handleScan(array $params): array
    {
        $limit  = min(self::SCAN_MAX, max(1, (int)($params['limit'] ?? self::SCAN_DEFAULT)));
        $offset = max(0, (int)($params['offset'] ?? 0));

        global $wpdb;

        // Conservative abort: if the uploads directory is unresolvable we cannot
        // perform URL-based reference detection. Proceeding would silently drop
        // all URL/path-based reference surfaces, causing images referenced only
        // by raw uploads URLs to be falsely flagged as unused (data-loss path).
        $uploadDirCheck  = wp_upload_dir();
        $uploadsUrlCheck = (string)($uploadDirCheck['baseurl'] ?? '');
        $uploadsDirCheck = (string)($uploadDirCheck['basedir'] ?? '');
        if ($uploadsUrlCheck === '' || $uploadsDirCheck === '') {
            return [
                'ok'     => false,
                'detail' => 'uploads directory unresolved; scan aborted to avoid false positives',
            ];
        }

        // Build the exhaustive reference index for THIS request.
        $idx = new MediaReferenceIndex();
        if (!$idx->build()) {
            return [
                'ok'     => false,
                'detail' => 'uploads directory unresolved; scan aborted to avoid false positives',
            ];
        }

        $uploadDir   = wp_upload_dir();
        $uploadsBase = rtrim((string)($uploadDir['basedir'] ?? ''), '/\\');

        // Load the set of attachment IDs that are already in a quarantine manifest.
        // These are excluded from the scan: they are out-of-scope and must not
        // resurface as fresh candidates after a prior isolation run.
        // A missing or unreadable quarantine directory returns an empty set, which
        // preserves the original scan behaviour when no quarantine has been created.
        $quarantine        = $this->quarantine ?? new MediaQuarantine();
        $quarantinedIds    = $quarantine->quarantinedAttachmentIds();
        $quarantinedCount  = count($quarantinedIds);

        // Walk the FULL attachment library in batches, collecting unused candidates
        // until we have SCAN_MAX + 1 (the +1 lets us set the truncated flag).
        // We never apply offset/limit to the attachment walk — only to the UNUSED list.
        // Attachments classified as referenced are collected into $allReferenced[]
        // in parallel; they are "referenced among those visited" (honest when the walk
        // is capped by SCAN_MAX).
        $allUnused           = [];
        $allReferenced       = []; // referenced entries to include in scan result
        $referencedTruncated = false; // true when referenced list hit REFERENCED_MAX
        $totalAttachments    = 0;  // diagnostic: total attachment rows walked (excludes quarantined)
        $scanCap             = self::SCAN_MAX + 1; // collect one extra to detect truncation
        $walkOffset          = 0;
        $walkBatch           = 200; // rows per DB round-trip

        while (count($allUnused) < $scanCap) {
            // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- batched attachment scan on core posts table; caching the full result set would OOM on large media libraries
            $rows = $wpdb->get_results($wpdb->prepare(
                "SELECT ID, post_title, guid
                 FROM {$wpdb->posts}
                 WHERE post_type = 'attachment'
                   AND post_status = 'inherit'
                 ORDER BY ID ASC
                 LIMIT %d OFFSET %d",
                $walkBatch,
                $walkOffset
            ), ARRAY_A);

            if (empty($rows)) {
                break; // end of attachment library
            }
            $walkOffset += $walkBatch;

            foreach ($rows as $row) {
                if (count($allUnused) >= $scanCap) {
                    break;
                }

                $id      = (int)($row['ID'] ?? 0);
                $title   = (string)($row['post_title'] ?? '');
                $guid    = (string)($row['guid'] ?? '');

                // Skip attachments that are already quarantined. They are treated as
                // out-of-scope: not counted in total_attachments, not tested for
                // references, and not added to candidates or the referenced list.
                if (isset($quarantinedIds[$id])) {
                    continue;
                }

                $totalAttachments++; // count every attachment the walk actually visits

                $relPath = (string)get_post_meta($id, '_wp_attached_file', true);
                $metaRaw = get_post_meta($id, '_wp_attachment_metadata', true);
                $meta    = is_array($metaRaw) ? $metaRaw : [];

                // Skip attachments that are referenced somewhere.
                // isReferenced() also populates the per-attachment usage index
                // consumed below by getReferencedUsages().
                if ($idx->isReferenced($id, $relPath, $meta)) {
                    // Collect the referenced entry only up to REFERENCED_MAX to
                    // prevent an unbounded payload on libraries where most media
                    // is in-use. The cap mirrors how SCAN_MAX bounds unused candidates.
                    if (count($allReferenced) < self::REFERENCED_MAX) {
                        // Build the referenced entry (same shape as a candidate, plus usages).
                        $refThumb  = null;
                        $refImgSrc = wp_get_attachment_image_src($id, 'thumbnail');
                        if (is_array($refImgSrc) && isset($refImgSrc[0])) {
                            $refThumb = (string)$refImgSrc[0];
                        }
                        $allReferenced[$id] = [
                            'id'    => $id,
                            'title' => $title !== '' ? $title : basename($relPath),
                            'url'   => $guid,
                            'thumb' => $refThumb,
                            // usages are filled after the walk from getReferencedUsages()
                        ];
                    } else {
                        $referencedTruncated = true;
                    }
                    continue;
                }

                // Compute file size (main file only; sub-sizes are additional).
                $absPath  = $relPath !== '' ? ($uploadsBase . '/' . ltrim($relPath, '/')) : '';
                $fileSize = ($absPath !== '' && file_exists($absPath)) ? (int)filesize($absPath) : 0;

                // Thumbnail URL for the grid.
                $thumb  = null;
                $imgSrc = wp_get_attachment_image_src($id, 'thumbnail');
                if (is_array($imgSrc) && isset($imgSrc[0])) {
                    $thumb = (string)$imgSrc[0];
                }

                // Count generated sizes.
                $sizesCount = !empty($meta['sizes']) && is_array($meta['sizes'])
                    ? count($meta['sizes'])
                    : 0;

                $allUnused[] = [
                    'id'          => $id,
                    'title'       => $title !== '' ? $title : basename($relPath),
                    'url'         => $guid,
                    'thumb'       => $thumb,
                    'file_size'   => $fileSize,
                    'sizes_count' => $sizesCount,
                ];
            }
        }

        // Detect whether the library has more unused than SCAN_MAX.
        $truncated = count($allUnused) > self::SCAN_MAX;
        if ($truncated) {
            array_pop($allUnused); // discard the sentinel entry
        }

        // total = full unused count (capped at SCAN_MAX); this is what the web
        // panel uses to render "N unused attachments found" and drive client pagination.
        $total = count($allUnused);

        // Merge per-attachment usages from the index into the referenced entries.
        // getReferencedUsages() returns only the IDs tested by isReferenced() above.
        $referencedUsagesMap = $idx->getReferencedUsages();
        $referencedEntries   = [];
        foreach ($allReferenced as $refId => $refEntry) {
            $usages = $referencedUsagesMap[$refId] ?? [];
            // Guarantee at least one usage entry so the consumer always sees usages[].
            if (empty($usages)) {
                $usages = [[
                    'surface'      => 'direct_id',
                    'source_id'    => null,
                    'source_label' => null,
                    'edit_url'     => null,
                    'detail'       => null,
                ]];
            }
            $refEntry['usages'] = $usages;
            $referencedEntries[] = $refEntry;
        }

        $referencedCount = count($referencedEntries);
        $unusedCount     = $total;

        // Log diagnostic summary to WP error_log for live investigation.
        // phpcs:disable WordPress.PHP.DevelopmentFunctions.error_log_error_log
        error_log(sprintf(
            '[wpmgr] media-clean scan: total_attachments=%d referenced=%d unused=%d quarantined=%d truncated=%s referenced_truncated=%s',
            $totalAttachments,
            $referencedCount,
            $unusedCount,
            $quarantinedCount,
            $truncated ? 'true' : 'false',
            $referencedTruncated ? 'true' : 'false'
        ));
        // Per-entry referenced logging: only when WP_DEBUG is on, and capped at
        // 20 entries to avoid flooding the log on large media libraries.
        if (!empty($referencedEntries) && defined('WP_DEBUG') && WP_DEBUG) {
            $logLimit = 0;
            foreach ($referencedEntries as $refEntry) {
                if ($logLimit >= 20) {
                    break;
                }
                $surfaces = array_unique(array_column($refEntry['usages'], 'surface'));
                error_log(sprintf(
                    '[wpmgr] media-clean referenced id=%d surfaces=%s',
                    $refEntry['id'],
                    implode(',', $surfaces)
                ));
                $logLimit++;
            }
        }
        // phpcs:enable

        // Apply offset/limit to the UNUSED list (client-side paging surface).
        $candidates = array_slice($allUnused, $offset, $limit);

        return [
            'ok'               => true,
            'total'            => $total,
            'candidates'       => $candidates,
            'truncated'        => $truncated,
            // Diagnostic fields (always present; allows live re-scan diagnosis).
            'total_attachments'      => $totalAttachments,
            'referenced_count'       => $referencedCount,
            'unused_count'           => $unusedCount,
            'quarantined_count'      => $quarantinedCount,
            'referenced'             => $referencedEntries,
            // true when the referenced list was capped at REFERENCED_MAX; the web
            // panel may surface this as "showing first N in-use attachments".
            'referenced_truncated'   => $referencedTruncated,
        ];
    }

    // =========================================================================
    // isolate
    // =========================================================================

    /**
     * Move the selected attachments' files into the quarantine directory and
     * write a reversible manifest. The attachment posts are left intact.
     *
     * @param array<string,mixed> $params
     * @return array<string,mixed>
     */
    private function handleIsolate(array $params): array
    {
        $jobId = $this->requireJobId($params);
        if ($jobId === '') {
            return ['ok' => false, 'detail' => 'missing job_id'];
        }

        if (!isset($params['attachment_ids']) || !is_array($params['attachment_ids'])) {
            return ['ok' => false, 'detail' => 'attachment_ids must be an array'];
        }

        $ids = $this->sanitiseIdList($params['attachment_ids'], self::ISOLATE_MAX);
        if (empty($ids)) {
            return ['ok' => false, 'detail' => 'no valid attachment_ids provided (max ' . self::ISOLATE_MAX . ')'];
        }

        $quarantine  = $this->quarantine ?? new MediaQuarantine();

        try {
            $manifestId = $quarantine->beginManifest($jobId);
        } catch (\Throwable $e) {
            return ['ok' => false, 'detail' => 'media quarantine unavailable: ' . $e->getMessage()];
        }

        $uploadDir   = wp_upload_dir();
        $uploadsBase = rtrim((string)($uploadDir['basedir'] ?? ''), '/\\');
        $moved          = 0;
        $perAttachment  = [];

        foreach ($ids as $attachmentId) {
            $relPath = (string)get_post_meta($attachmentId, '_wp_attached_file', true);
            $metaRaw = get_post_meta($attachmentId, '_wp_attachment_metadata', true);
            $meta    = is_array($metaRaw) ? $metaRaw : [];

            // Collect standard WP files for this attachment.
            $filePaths = $this->allFilePathsForAttachment($uploadsBase, $relPath, $meta);

            // Merge in any extra on-disk files produced by the optimizer. These are
            // path-confined and deduplicated before being passed to quarantine.
            $optimizerPaths = $this->optimizerManagedPaths($attachmentId, $uploadsBase, $relPath, $meta);
            if (!empty($optimizerPaths)) {
                $filePaths = array_values(array_unique(array_merge($filePaths, $optimizerPaths)));
            }

            if (empty($filePaths)) {
                // No on-disk files found; still record the attachment via quarantine
                // so its post can always be deleted later.
                $quarantine->quarantineAttachment($manifestId, $attachmentId, $relPath, []);
                $perAttachment[] = ['attachment_id' => $attachmentId, 'moved' => 0];
                continue;
            }

            $attachmentMoved = $quarantine->quarantineAttachment($manifestId, $attachmentId, $relPath, $filePaths);
            $moved          += $attachmentMoved;
            $perAttachment[] = ['attachment_id' => $attachmentId, 'moved' => $attachmentMoved];
        }

        $quarantine->finaliseManifest($manifestId);

        $entriesRecorded = count($perAttachment);

        // phpcs:disable WordPress.PHP.DevelopmentFunctions.error_log_error_log
        error_log(sprintf(
            '[wpmgr] media-clean isolate: job_id=%s manifest_id=%s ids=%d entries_recorded=%d files_moved=%d',
            $jobId,
            $manifestId,
            count($ids),
            $entriesRecorded,
            $moved
        ));
        // phpcs:enable

        return [
            'ok'               => true,
            'job_id'           => $jobId,
            'moved'            => $moved,
            'manifest_id'      => $manifestId,
            'entries_recorded' => $entriesRecorded,
            'per_attachment'   => $perAttachment,
        ];
    }

    // =========================================================================
    // restore
    // =========================================================================

    /**
     * Move quarantined files back to their original paths.
     *
     * @param array<string,mixed> $params
     * @return array<string,mixed>
     */
    private function handleRestore(array $params): array
    {
        $jobId = $this->requireJobId($params);
        if ($jobId === '') {
            return ['ok' => false, 'detail' => 'missing job_id'];
        }

        $manifestIds = $this->sanitiseStringList($params['quarantine_ids'] ?? [], self::OP_MAX);
        if (empty($manifestIds)) {
            return ['ok' => false, 'detail' => 'no valid quarantine_ids provided'];
        }

        $quarantine = $this->quarantine ?? new MediaQuarantine();
        $restored   = 0;
        $skipped    = 0;
        $failed     = 0;
        $blocked    = [];

        try {
            foreach ($manifestIds as $mId) {
                $receipt   = $quarantine->restoreManifest($mId);
                $restored += (int)($receipt['restored'] ?? 0);
                $skipped  += (int)($receipt['skipped'] ?? 0);
                $failed   += (int)($receipt['failed'] ?? 0);
                foreach ((array)($receipt['blocked'] ?? []) as $frag) {
                    $blocked[] = (string)$frag;
                }
            }
        } catch (\Throwable $e) {
            return ['ok' => false, 'detail' => 'media quarantine unavailable: ' . $e->getMessage()];
        }

        // phpcs:disable WordPress.PHP.DevelopmentFunctions.error_log_error_log
        error_log(sprintf(
            '[wpmgr] media-clean restore: job_id=%s manifests=%d restored=%d skipped=%d failed=%d',
            $jobId,
            count($manifestIds),
            $restored,
            $skipped,
            $failed
        ));
        // phpcs:enable

        $result = [
            // ok stays true for an incomplete restore: files really were moved
            // back, and the control plane treats ok:false as an error and
            // returns before recording the outcome, which would leave a real
            // mutation unaudited. The counts below carry the incompleteness.
            'ok'       => true,
            'job_id'   => $jobId,
            'restored' => $restored,
            'skipped'  => $skipped,
            'failed'   => $failed,
        ];

        if ($skipped > 0 || $failed > 0) {
            $result['detail'] = $this->restoreDetail($skipped, $failed, $blocked);
        }

        return $result;
    }

    /**
     * Human-readable summary of a restore that did not put everything back.
     *
     * Names only the first few uploads-relative paths plus a total, so the
     * operator gets something actionable without the payload growing with the
     * manifest and without absolute server paths appearing on the wire.
     *
     * @param list<string> $blocked Uploads-relative fragments left in quarantine.
     */
    private function restoreDetail(int $skipped, int $failed, array $blocked): string
    {
        $outstanding = $skipped + $failed;
        $detail      = sprintf(
            '%d file(s) were not restored and remain in quarantine (skipped=%d failed=%d)',
            $outstanding,
            $skipped,
            $failed
        );

        $names = array_slice($blocked, 0, self::RESTORE_DETAIL_PATHS);
        if (!empty($names)) {
            $detail .= ': ' . implode(', ', $names);
            $more    = count($blocked) - count($names);
            if ($more > 0) {
                $detail .= sprintf(' (+%d more)', $more);
            }
        }

        return $detail;
    }

    // =========================================================================
    // delete
    // =========================================================================

    /**
     * Permanently delete quarantined files and their attachment posts.
     * Requires explicit "DELETE" confirm token.
     *
     * @param array<string,mixed> $params
     * @return array<string,mixed>
     */
    private function handleDelete(array $params): array
    {
        $jobId = $this->requireJobId($params);
        if ($jobId === '') {
            return ['ok' => false, 'detail' => 'missing job_id'];
        }

        // Require explicit confirm token (hash_equals — constant time).
        $confirm = isset($params['confirm']) && is_string($params['confirm'])
            ? $params['confirm']
            : '';
        if (!hash_equals(self::DELETE_CONFIRM, $confirm)) {
            return ['ok' => false, 'detail' => 'confirm token required for permanent deletion'];
        }

        $manifestIds = $this->sanitiseStringList($params['quarantine_ids'] ?? [], self::OP_MAX);
        if (empty($manifestIds)) {
            return ['ok' => false, 'detail' => 'no valid quarantine_ids provided'];
        }

        $quarantine       = $this->quarantine ?? new MediaQuarantine();
        $postsDeleted     = 0;
        $postsFailed      = 0;
        $filesDeleted     = 0;
        $entriesProcessed = 0;
        $allResults       = [];

        try {
            foreach ($manifestIds as $mId) {
                $r                 = $quarantine->deleteManifest($mId);
                $postsDeleted     += $r['posts_deleted'];
                $postsFailed      += $r['posts_failed'];
                $filesDeleted     += $r['files_deleted'];
                $entriesProcessed += $r['entries_processed'];
                foreach ($r['results'] as $row) {
                    $allResults[] = $row;
                }
            }
        } catch (\Throwable $e) {
            return ['ok' => false, 'detail' => 'media quarantine unavailable: ' . $e->getMessage()];
        }

        // phpcs:disable WordPress.PHP.DevelopmentFunctions.error_log_error_log
        error_log(sprintf(
            '[wpmgr] media-clean delete: manifests=%d entries=%d posts_deleted=%d posts_failed=%d files_deleted=%d',
            count($manifestIds),
            $entriesProcessed,
            $postsDeleted,
            $postsFailed,
            $filesDeleted
        ));
        // phpcs:enable

        return [
            'ok'               => true,
            'job_id'           => $jobId,
            'deleted'          => $postsDeleted, // backward-compat alias
            'posts_deleted'    => $postsDeleted,
            'posts_failed'     => $postsFailed,
            'files_deleted'    => $filesDeleted,
            'entries_processed' => $entriesProcessed,
            'results'          => $allResults,
        ];
    }

    // =========================================================================
    // list
    // =========================================================================

    /**
     * Read-only pass: enumerate all quarantine manifests and return the frozen
     * wire shape. No params required. No side effects.
     *
     * Result shape:
     *   {
     *     "ok": true,
     *     "manifests": [
     *       {
     *         "manifest_id": "<string>",
     *         "job_id":      "<string>",
     *         "isolated_at": <int unix seconds>,
     *         "total_files": <int>,
     *         "entries": [
     *           { "attachment_id": <int>, "title": "<string>", "file_count": <int> }
     *         ]
     *       }, ...
     *     ]
     *   }
     *
     * Manifests are sorted newest-first by isolated_at. When nothing has been
     * quarantined, manifests is an empty array.
     *
     * @return array<string,mixed>
     */
    private function handleList(): array
    {
        $quarantine = $this->quarantine ?? new MediaQuarantine();
        $manifests  = $quarantine->listManifestsDetailed();

        return [
            'ok'        => true,
            'manifests' => $manifests,
        ];
    }

    // =========================================================================
    // Private helpers
    // =========================================================================

    /**
     * Collect ALL filesystem paths for an attachment: main file + all sub-sizes
     * + original_image (WP 5.3+ big-image). Paths are absolute.
     *
     * @param string $uploadsBase Absolute filesystem uploads root (no trailing slash).
     * @param string $relPath     Upload-relative main file path.
     * @param array  $meta        Decoded _wp_attachment_metadata.
     * @return list<string>
     */
    private function allFilePathsForAttachment(
        string $uploadsBase,
        string $relPath,
        array $meta
    ): array {
        $paths = [];

        if ($relPath !== '') {
            $abs = $uploadsBase . '/' . ltrim($relPath, '/');
            if (file_exists($abs)) {
                $paths[] = $abs;
            }
        }

        // Derive sub-dir from the meta file field (which may differ from relPath
        // for very old WP installs that stored just the basename).
        $dir = '';
        if (!empty($meta['file']) && is_string($meta['file'])) {
            $d = dirname($meta['file']);
            if ($d !== '' && $d !== '.') {
                $dir = $uploadsBase . '/' . ltrim($d, '/') . '/';
            } else {
                $dir = $uploadsBase . '/';
            }
        } elseif ($relPath !== '') {
            $d = dirname($relPath);
            if ($d !== '' && $d !== '.') {
                $dir = $uploadsBase . '/' . ltrim($d, '/') . '/';
            } else {
                $dir = $uploadsBase . '/';
            }
        }

        if (!empty($meta['sizes']) && is_array($meta['sizes'])) {
            foreach ($meta['sizes'] as $size) {
                if (!empty($size['file']) && is_string($size['file'])) {
                    $abs = $dir . $size['file'];
                    if (file_exists($abs)) {
                        $paths[] = $abs;
                    }
                }
            }
        }

        if (!empty($meta['original_image']) && is_string($meta['original_image'])) {
            $abs = $dir . $meta['original_image'];
            if (file_exists($abs)) {
                $paths[] = $abs;
            }
        }

        return array_values(array_unique($paths));
    }

    /**
     * Collect extra on-disk files created by the Media Optimizer for this
     * attachment. Returns only paths that (a) exist on disk, (b) are confirmed
     * to be confined within $uploadsBase, and (c) are not already in the
     * standard file list.
     *
     * REPLACE mode (target_format='original', same-ext recompress): the optimizer
     * archived each original file to `<name>.wpmgr-original.<ext>` before
     * overwriting it with optimized bytes. Those archive files must be included so
     * they are not left behind.
     *
     * COEXIST mode (different-ext, e.g. JPG→AVIF/WebP): the optimizer wrote the
     * optimized variant to a new-ext path alongside the original. Those variant
     * files must be included.
     *
     * The authoritative record of what the optimizer produced is the `optimized_data`
     * map in the keystore blob: each entry carries `archive_mode` and `path` (the
     * optimized file's absolute path). For REPLACE mode the archive path is derived
     * from the original path using the same `Rename::archivePathFor()` convention
     * the optimizer used (so we never rely on the blob's `path` alone — the blob
     * records the post-overwrite path, i.e. the original location, not the archive).
     *
     * SECURITY: every candidate path is verified to start with the real, resolved
     * $uploadsBase prefix (including a trailing separator) before being returned.
     * Paths that escape the uploads tree — whether via `..` segments, absolute
     * out-of-tree references, or symlinks — are silently dropped. This prevents a
     * crafted blob from causing files outside uploads to be quarantined or deleted.
     *
     * @param int    $attachmentId WP attachment post ID.
     * @param string $uploadsBase  Absolute filesystem uploads root (no trailing slash).
     * @param string $relPath      Upload-relative main file path.
     * @param array  $meta         Decoded _wp_attachment_metadata.
     * @return list<string>
     */
    private function optimizerManagedPaths(
        int $attachmentId,
        string $uploadsBase,
        string $relPath,
        array $meta
    ): array {
        $blob = (new MediaKeystore())->get($attachmentId);
        if (empty($blob) || !is_array($blob)) {
            return [];
        }

        $optimizedData = $blob['optimized_data'] ?? [];
        if (!is_array($optimizedData) || empty($optimizedData)) {
            return [];
        }

        $rename = new \WPMgr\Agent\Media\Rename();
        $paths  = [];

        foreach ($optimizedData as $sizeName => $rec) {
            if (!is_array($rec)) {
                continue;
            }

            $archiveMode  = isset($rec['archive_mode']) ? (string)$rec['archive_mode'] : '';
            $optimizedPath = isset($rec['path']) ? (string)$rec['path'] : '';

            if ($archiveMode === AttachmentMeta::MODE_REPLACE) {
                // The optimizer archived the original to `<name>.wpmgr-original.<ext>`
                // and then wrote new bytes at the original path (recorded in `path`).
                // The archive path is derived from the original path.
                if ($optimizedPath !== '') {
                    $archivePath = $rename->archivePathFor($optimizedPath);
                    $confined    = $this->confinedPath($archivePath, $uploadsBase);
                    if ($confined !== '' && file_exists($confined)) {
                        $paths[] = $confined;
                    }
                }
            } elseif ($archiveMode === AttachmentMeta::MODE_COEXIST) {
                // The optimizer wrote the variant to a new-ext path (`path`).
                // The original is left in place and is already in the standard list.
                if ($optimizedPath !== '') {
                    $confined = $this->confinedPath($optimizedPath, $uploadsBase);
                    if ($confined !== '' && file_exists($confined)) {
                        $paths[] = $confined;
                    }
                }
            }
        }

        return array_values(array_unique($paths));
    }

    /**
     * Confirm that $candidate is confined within $uploadsBase and return a
     * path string suitable for passing to quarantine (i.e. using the same
     * $uploadsBase prefix the rest of the pipeline uses). Returns '' if the
     * path is outside the uploads tree, contains traversal sequences, or
     * cannot be canonicalized.
     *
     * Canonicalization strategy:
     *   - Resolve `.` and `..` segments manually (no symlink resolution) so that
     *     the returned path uses the same symlink-unresolved prefix as $uploadsBase.
     *     Symlinks are intentionally NOT followed for the confinement check — the
     *     uploads base realpath is used only to catch symlinks that point outside.
     *   - After segment resolution, verify the path starts with $uploadsBase/.
     *   - Additionally verify via realpath (when the file exists) that the resolved
     *     physical location also lies inside the realpath of $uploadsBase, catching
     *     symlinks that point outside uploads.
     *
     * The returned path always uses the original $uploadsBase prefix (not the
     * realpath-resolved form), so it is consistent with how standard file paths
     * are stored throughout the quarantine/manifest pipeline (which stores the
     * unresolved path and uses normalisePath() based on the same unresolved base).
     *
     * This makes it impossible to use a crafted blob entry to quarantine or
     * delete files outside the uploads directory.
     *
     * @param string $candidate   Absolute path candidate derived from blob data.
     * @param string $uploadsBase Absolute uploads root (no trailing slash).
     * @return string The confined path (using $uploadsBase prefix), or '' if rejected.
     */
    private function confinedPath(string $candidate, string $uploadsBase): string
    {
        if ($candidate === '' || $uploadsBase === '') {
            return '';
        }

        // Normalize separators on both sides for a consistent string comparison.
        $normCandidate = str_replace('\\', '/', $candidate);
        $normBase      = rtrim(str_replace('\\', '/', $uploadsBase), '/');

        // Reject any `..` traversal sequences in the candidate before we do
        // segment resolution — belt-and-braces against encoding tricks.
        if (strpos($normCandidate, '/../') !== false || str_ends_with($normCandidate, '/..')) {
            return '';
        }

        // Reject percent-encoded characters (e.g. %2e%2e) and null bytes.
        // These are not valid in real filesystem paths produced by WP/the optimizer,
        // so any occurrence signals a crafted input.
        if (strpos($normCandidate, '%') !== false || strpos($normCandidate, "\0") !== false) {
            return '';
        }

        // Resolve path segments manually without following symlinks.
        // Keep the returned path in the original symlink-unresolved form so it is
        // consistent with $uploadsBase and with how the quarantine pipeline stores paths.
        $parts    = explode('/', ltrim($normCandidate, '/'));
        $resolved = [];
        foreach ($parts as $part) {
            if ($part === '') {
                continue;
            }
            // Reject any all-dots segment (.  ..  ...  ....  etc.).
            // This catches `..` traversal as well as exotic dot-cluster sequences that
            // are not meaningful path components on any supported filesystem.
            if (preg_match('/^\.+$/', $part)) {
                if ($part === '.') {
                    // Current-dir: skip silently.
                    continue;
                }
                // `..` and any longer dot cluster: abort.
                return '';
            }
            $resolved[] = $part;
        }
        $canonical = '/' . implode('/', $resolved);

        // Primary confinement check: the resolved path must begin with the uploads base.
        if (strpos($canonical . '/', $normBase . '/') !== 0) {
            return '';
        }

        // Secondary check via realpath (only when the file exists): confirm the
        // physical inode also lives inside the real uploads base. This catches symlinks
        // that point outside uploads.
        if (file_exists($canonical)) {
            $realFile = realpath($canonical);
            $realBase = realpath($uploadsBase);
            if ($realFile === false) {
                return '';
            }
            if ($realBase !== false) {
                $realBaseSlash = rtrim($realBase, '/') . '/';
                if (strpos($realFile . '/', $realBaseSlash) !== 0) {
                    return ''; // symlink escapes uploads — reject.
                }
            }
        }

        return $canonical;
    }

    /**
     * Extract and bound-check a list of positive integer IDs.
     *
     * @param mixed $raw
     * @param int   $max
     * @return list<int>
     */
    private function sanitiseIdList($raw, int $max): array
    {
        if (!is_array($raw)) {
            return [];
        }
        $ids = [];
        foreach ($raw as $v) {
            $id = (int)$v;
            if ($id > 0) {
                $ids[] = $id;
            }
            if (count($ids) >= $max) {
                break;
            }
        }
        return $ids;
    }

    /**
     * Extract and bound-check a list of non-empty strings (manifest IDs).
     *
     * @param mixed $raw
     * @param int   $max
     * @return list<string>
     */
    private function sanitiseStringList($raw, int $max): array
    {
        if (!is_array($raw)) {
            return [];
        }
        $out = [];
        foreach ($raw as $v) {
            $s = is_string($v) ? trim($v) : '';
            if ($s !== '' && preg_match('/^[a-zA-Z0-9_-]{1,64}$/', $s)) {
                $out[] = $s;
            }
            if (count($out) >= $max) {
                break;
            }
        }
        return $out;
    }

    /**
     * Extract and validate the job_id parameter.
     */
    private function requireJobId(array $params): string
    {
        $id = isset($params['job_id']) && is_string($params['job_id'])
            ? trim($params['job_id'])
            : '';
        // Accept any non-empty alphanumeric/dash string up to 64 chars.
        return (preg_match('/^[a-zA-Z0-9_-]{1,64}$/', $id) === 1) ? $id : '';
    }
}
