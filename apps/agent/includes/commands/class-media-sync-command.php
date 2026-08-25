<?php
/**
 * MediaSyncCommand: handles the CP's `media_sync` command — enumerate the WP
 * media library and push paged batches (<=200) to the CP's sync-batch endpoint.
 *
 * CP contract (apps/api/internal/agentcmd/media_contract.go MediaSyncRequest):
 *   POST /wp-json/wpmgr/v1/command/media_sync
 *   body: { "job_id", "batch_endpoint" }
 *   resp: MediaSyncResponse { "ok", "detail" }
 *
 * Each batch row matches syncBatchAttachmentDTO
 * (apps/api/internal/media/handler/agent_handler.go:48-57):
 *   { wp_attachment_id, title, original_path, original_url, original_mime,
 *     original_width, original_height, original_size_bytes }
 *
 * NOTE / contract deviation: the prompt's flow text lists a richer per-row
 * shape (a nested `sizes:{name:{file,filesize,width,height}}` map). The AUTHORITATIVE
 * CP DTO (syncBatchAttachmentDTO) carries ONLY the full-size fields above — there
 * is no `sizes` field on the CP side — so we send the CP's exact shape and do NOT
 * invent a `sizes` field (per the prompt: "If a needed field isn't in the CP
 * contract, note it — don't invent silently"). Registered-size data still rides
 * the optimize flow via the presign `variants` list.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Media\MediaAttachmentRow;
use WPMgr\Agent\Media\MediaUploader;
use WPMgr\Agent\Support\LongRunningJob;

/**
 * Enumerates attachments and pushes them to the CP in <=200-row pages.
 */
final class MediaSyncCommand implements CommandInterface
{
    /** Page size cap — mirrors media.MaxSyncBatch (domain.go:49). */
    private const PAGE_SIZE = 200;

    /** Safety cap on total pages per command invocation. */
    private const MAX_PAGES = 1000;

    private MediaUploader $uploader;

    public function __construct(MediaUploader $uploader)
    {
        $this->uploader = $uploader;
    }

    public function name(): string
    {
        return 'media_sync';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims
     * @param array<string,mixed> $params MediaSyncRequest fields.
     * @return array{ok:bool,detail:string}
     */
    public function execute(array $claims, array $params): array
    {
        $endpoint = $this->str($params, 'batch_endpoint');
        if ($endpoint === '') {
            return ['ok' => false, 'detail' => 'missing batch_endpoint'];
        }
        global $wpdb;
        if (!isset($wpdb) || !is_object($wpdb)) {
            return ['ok' => false, 'detail' => 'WP not available'];
        }
        /** @var \wpdb $wpdb */

        // A mega library can enumerate thousands of rows across many pages; this
        // runs in the command's REST request, so lift PHP's per-request caps
        // (the dispatch is bounded by the CP's 120s media commander) and survive
        // a dropped loopback connection. Mirrors the backup/diagnostics paths.
        if (function_exists('set_time_limit')) {
            @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running backup/restore loop must not hit max_execution_time; @-guarded
        }
        if (function_exists('ignore_user_abort')) {
            @ignore_user_abort(true);
        }

        // The sync job_id rides EVERY sync-batch page (so the CP can scope the
        // enumeration) AND the post-enumeration sync-finalize (so it can
        // reconcile offline deletions against exactly this run's pages).
        $jobId = $this->str($params, 'job_id');

        $afterId   = 0;     // keyset cursor: exclusive lower-bound attachment id
        $pages     = 0;
        $totalSeen = 0;
        $upserted  = 0;

        while ($pages < self::MAX_PAGES) {
            $ids = $this->attachmentIdsPage($afterId);
            if ($ids === []) {
                break;
            }
            $totalSeen += count($ids);

            $rows = [];
            foreach ($ids as $id) {
                $row = $this->buildRow((int) $id);
                if ($row !== null) {
                    $rows[] = $row;
                }
            }

            if ($rows !== []) {
                $result = $this->uploader->syncBatch($endpoint, $rows, $jobId);
                if (!$result['ok']) {
                    // PARTIAL/ERRORED run: DO NOT finalize. Finalizing here would
                    // let a half-enumerated (or transiently-empty) WP wipe the
                    // CP's asset list — the critical blast-radius guard.
                    return ['ok' => false, 'detail' => 'sync-batch rejected after ' . $upserted . ' upserted'];
                }
                $upserted += $result['upserted_count'];
            }

            // Advance the keyset cursor past the highest id in this page. Rows are
            // ordered ID ASC, so the last element is the max — the next page starts
            // strictly after it. Keyset (not OFFSET) stays O(1) on huge libraries.
            $afterId = (int) $ids[count($ids) - 1];
            $pages++;
        }

        if (defined('WP_DEBUG') && WP_DEBUG) {
            \WPMgr\Agent\Support\DebugLog::write(sprintf('WPMgr media_sync: enumerated %d image attachments across %d page(s), upserted %d', $totalSeen, $pages, $upserted));
        }

        // CLEAN full enumeration: tell the CP to reconcile offline deletions. The
        // CP supplies finalize_endpoint in MediaSyncRequest (media_contract.go);
        // we fall back to deriving it from the batch endpoint (swap the trailing
        // /sync-batch for /sync-finalize) only if the field is absent. The
        // uploader signs over its path exactly like the batch pages. Best-effort:
        // a failed finalize never fails the sync. Only reached on a CLEAN run —
        // an errored page returned above WITHOUT finalizing (blast-radius guard).
        if ($jobId !== '') {
            $finalize = $this->str($params, 'finalize_endpoint');
            if ($finalize === '') {
                $finalize = $this->finalizeEndpoint($endpoint);
            }
            $this->uploader->syncFinalize($finalize, $jobId);
        }

        return ['ok' => true, 'detail' => 'synced ' . $upserted . ' attachments'];
    }

    /**
     * Derive the sync-finalize endpoint URL from the CP-supplied batch endpoint
     * by swapping the trailing `/sync-batch` path segment for `/sync-finalize`.
     * Fallback only: the CP normally supplies finalize_endpoint directly. The CP
     * mints both from the same base (media/service.go callbackURL), so this is a
     * pure string transform with no extra plumbing.
     *
     * @param string $batchEndpoint Absolute batch endpoint URL.
     * @return string Absolute sync-finalize endpoint URL ('' when underivable).
     */
    private function finalizeEndpoint(string $batchEndpoint): string
    {
        if (substr($batchEndpoint, -strlen('/sync-batch')) !== '/sync-batch') {
            return '';
        }

        return substr($batchEndpoint, 0, -strlen('/sync-batch')) . '/sync-finalize';
    }

    /**
     * Fetch one keyset page of IMAGE attachment ids with ID > $afterId.
     *
     * Deliberately a DIRECT $wpdb query, NOT get_posts()/WP_Query. The previous
     * get_posts() enumeration silently truncated real libraries: it filtered
     * post_status='inherit' (so imported/migrated images carrying 'publish' or
     * 'private' status were dropped) and its `paged` pagination is exposed to any
     * site `pre_get_posts` hook. The result was a 500-image library syncing only
     * ~118 rows. This raw query is immune to query-var filtering and covers:
     *   - EVERY image MIME  (post_mime_type LIKE 'image/%') — jpeg/png/webp/avif
     *     PLUS gif/svg/bmp/tiff, so the synced set mirrors the WP Media Library's
     *     image list exactly (optimizability is decided downstream per-format);
     *   - EVERY non-junk status (anything but trash/auto-draft).
     * Keyset (ID > cursor) rather than OFFSET keeps each page O(1) on huge
     * libraries. The LIMIT guarantees every page is <= MaxSyncBatch (no 422).
     *
     * @param int $afterId Exclusive lower-bound attachment id (0 for the first page).
     * @return list<int> Up to PAGE_SIZE ids, ascending.
     */
    private function attachmentIdsPage(int $afterId): array
    {
        global $wpdb;
        if (!isset($wpdb) || !is_object($wpdb)) {
            return [];
        }
        /** @var \wpdb $wpdb */

        // $wpdb->posts is a trusted core table name; 'image/%' passes through
        // prepare() as a literal value whose % stays a SQL LIKE wildcard.
        // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- already prepared on the preceding line; table name is a trusted core $wpdb property
        $sql = $wpdb->prepare(
            "SELECT ID FROM {$wpdb->posts}
             WHERE post_type = 'attachment'
               AND ID > %d
               AND post_mime_type LIKE %s
               AND post_status NOT IN ('trash', 'auto-draft')
             ORDER BY ID ASC
             LIMIT %d",
            $afterId,
            'image/%',
            self::PAGE_SIZE
        );

        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching,WordPress.DB.PreparedSQL.NotPrepared -- direct query on core table; identifier validated against information_schema / prefix+constant; values bound via placeholders; get_posts() silently truncates real libraries via post_status filtering and pre_get_posts hooks
        $ids = $wpdb->get_col($sql);

        return is_array($ids) ? array_values(array_map('intval', $ids)) : [];
    }

    /**
     * Build one syncBatchAttachmentDTO-shaped row.
     *
     * Delegates to MediaAttachmentRow::build — the single source of truth for
     * the row shape shared with AutoOptimizeUpload::drain().
     *
     * @param int $id Attachment id.
     * @return array<string,mixed>|null Null when the attachment can't be resolved.
     */
    private function buildRow(int $id): ?array
    {
        return MediaAttachmentRow::build($id);
    }

    /**
     * @param array<string,mixed> $params
     * @param string              $key
     * @return string
     */
    private function str(array $params, string $key): string
    {
        return isset($params[$key]) && is_string($params[$key]) ? $params[$key] : '';
    }
}
