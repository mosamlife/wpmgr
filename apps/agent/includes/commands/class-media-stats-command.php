<?php
/**
 * MediaStatsCommand: returns one attachment's optimization stats blob for the
 * modal / dashboard. This is a LOCAL read command (no CP callback) — the CP (or
 * a local hook) invokes it with `wp_attachment_id` and gets the salient blob
 * fields + the rendered (escaped) stats HTML.
 *
 * There is no dedicated CP endpoint for stats in the Phase-3 contract
 * (agent_handler.go has sync-batch/presign/encode-ready/job-status/restore-status
 * only); the per-attachment stats are mirrored to the CP via the job-status
 * callback after each apply. So this command is the synchronous-read surface the
 * media modal uses (mirrors FlyingPress's get_image_stats_html being the modal
 * data source). It returns the data; it does NOT call back to the CP.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Media\StatsRenderer;
use WPMgr\Agent\MediaKeystore;

/**
 * Returns one attachment's optimization stats (blob fields + rendered HTML).
 */
final class MediaStatsCommand implements CommandInterface
{
    private MediaKeystore $keystore;

    private StatsRenderer $renderer;

    public function __construct(?MediaKeystore $keystore = null, ?StatsRenderer $renderer = null)
    {
        $this->keystore = $keystore ?? new MediaKeystore();
        $this->renderer = $renderer ?? new StatsRenderer();
    }

    public function name(): string
    {
        return 'media_stats';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims
     * @param array<string,mixed> $params { wp_attachment_id }
     * @return array<string,mixed>
     */
    public function execute(array $claims, array $params): array
    {
        $attachmentId = isset($params['wp_attachment_id']) && is_numeric($params['wp_attachment_id'])
            ? (int) $params['wp_attachment_id']
            : 0;
        if ($attachmentId <= 0) {
            return ['ok' => false, 'detail' => 'missing wp_attachment_id'];
        }

        $blob = $this->keystore->get($attachmentId);
        $mime = function_exists('get_post_mime_type') ? (string) get_post_mime_type($attachmentId) : '';

        return [
            'ok'                => true,
            'wp_attachment_id'  => $attachmentId,
            'status'            => isset($blob['status']) ? (string) $blob['status'] : '',
            'target_format'     => isset($blob['target_format']) ? (string) $blob['target_format'] : '',
            'compression_level' => isset($blob['compression_level']) ? (string) $blob['compression_level'] : '',
            'generation'        => (int) ($blob['wpmgr_generation'] ?? 0),
            'sizes_optimized'   => is_array($blob['sizes_optimized'] ?? null) ? array_values($blob['sizes_optimized']) : [],
            'sizes_unoptimized' => is_array($blob['sizes_unoptimized'] ?? null) ? $blob['sizes_unoptimized'] : [],
            'original_deleted'  => (int) ($blob['original_deleted'] ?? 0),
            'optimizable'       => $this->renderer->isOptimizable($attachmentId, $mime),
            'html'              => $this->renderer->renderForAttachment($attachmentId, $mime),
        ];
    }
}
