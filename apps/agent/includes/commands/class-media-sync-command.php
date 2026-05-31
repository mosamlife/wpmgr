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

use WPMgr\Agent\Media\MediaUploader;

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
        if (!function_exists('get_posts')) {
            return ['ok' => false, 'detail' => 'WP not available'];
        }

        $paged    = 1;
        $upserted = 0;

        while ($paged <= self::MAX_PAGES) {
            $ids = $this->attachmentIdsPage($paged);
            if ($ids === []) {
                break;
            }

            $rows = [];
            foreach ($ids as $id) {
                $row = $this->buildRow((int) $id);
                if ($row !== null) {
                    $rows[] = $row;
                }
            }

            if ($rows !== []) {
                $result = $this->uploader->syncBatch($endpoint, $rows);
                if (!$result['ok']) {
                    return ['ok' => false, 'detail' => 'sync-batch rejected on page ' . $paged];
                }
                $upserted += $result['upserted_count'];
            }

            $paged++;
        }

        return ['ok' => true, 'detail' => 'synced ' . $upserted . ' attachments'];
    }

    /**
     * Fetch one page of attachment ids (image mimes only).
     *
     * @param int $paged 1-based page number.
     * @return list<int>
     */
    private function attachmentIdsPage(int $paged): array
    {
        $ids = get_posts([
            'post_type'              => 'attachment',
            'post_status'            => 'inherit',
            'post_mime_type'         => ['image/jpeg', 'image/png', 'image/webp', 'image/avif'],
            'posts_per_page'         => self::PAGE_SIZE,
            'paged'                  => $paged,
            'fields'                 => 'ids',
            'no_found_rows'          => true,
            'update_post_meta_cache' => false,
            'update_post_term_cache' => false,
            'orderby'                => 'ID',
            'order'                  => 'ASC',
        ]);

        if (!is_array($ids)) {
            return [];
        }

        return array_values(array_map('intval', $ids));
    }

    /**
     * Build one syncBatchAttachmentDTO-shaped row.
     *
     * @param int $id Attachment id.
     * @return array<string,mixed>|null Null when the attachment can't be resolved.
     */
    private function buildRow(int $id): ?array
    {
        if (!function_exists('wp_get_attachment_metadata')) {
            return null;
        }
        $meta = wp_get_attachment_metadata($id);
        $meta = is_array($meta) ? $meta : [];

        $path = function_exists('get_attached_file') ? (string) get_attached_file($id) : '';
        $url  = function_exists('wp_get_attachment_url') ? (string) wp_get_attachment_url($id) : '';
        $mime = function_exists('get_post_mime_type') ? (string) get_post_mime_type($id) : '';

        $title = '';
        if (function_exists('get_the_title')) {
            $title = (string) get_the_title($id);
        }

        $width  = isset($meta['width']) ? (int) $meta['width'] : null;
        $height = isset($meta['height']) ? (int) $meta['height'] : null;

        $size = 0;
        if (isset($meta['filesize']) && is_numeric($meta['filesize'])) {
            $size = (int) $meta['filesize'];
        } elseif ($path !== '' && @is_file($path)) {
            $size = (int) @filesize($path);
        }

        return [
            'wp_attachment_id'    => $id,
            'title'               => $title,
            'original_path'       => $path,
            'original_url'        => $url,
            'original_mime'       => $mime,
            'original_width'      => $width,
            'original_height'     => $height,
            'original_size_bytes' => $size,
        ];
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
