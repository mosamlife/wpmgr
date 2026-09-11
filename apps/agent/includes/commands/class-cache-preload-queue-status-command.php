<?php
/**
 * CachePreloadQueueStatusCommand — read the preload-queue state for the viewer.
 *
 * Wire contract (CP → agent, SIGNED dispatcher):
 *   POST /wp-json/wpmgr/v1/command/cache_preload_queue_status
 *   Body: { "limit"?: int }   // page size for the row sample (default 50)
 *
 * Response: {
 *   ok: bool, pending: int, processing: int, failed: int,
 *   rows: [ { id, url, device, priority, status, attempts, last_error,
 *             created_at, locked_at, updated_at } ]
 * }
 *
 * Successfully-warmed rows are DELETED (there is no `completed` status), so the
 * viewer presents All/Pending/Processing/Failed and treats "completed" as implied.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Cache\CacheManager;

/**
 * Returns per-status tallies + a bounded page of preload-queue rows.
 */
final class CachePreloadQueueStatusCommand implements CommandInterface
{
    private CacheManager $cache;

    /**
     * @param CacheManager $cache Page-cache orchestrator (builds the queue).
     */
    public function __construct(CacheManager $cache)
    {
        $this->cache = $cache;
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'cache_preload_queue_status';
    }

    /**
     * Effect: returns per-status tallies and a page of queue rows.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Read;
    }

    /**
     * Repeatability: a read.
     *
     * @return CommandRepeatability
     */
    public function repeatability(): CommandRepeatability
    {
        return CommandRepeatability::Idempotent;
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused).
     * @param array<string,mixed> $params { limit?: int }.
     * @return array<string,mixed>
     */
    public function execute(array $claims, array $params): array
    {
        $limit = isset($params['limit']) && is_numeric($params['limit'])
            ? (int) $params['limit']
            : 50;

        try {
            $queue = $this->cache->preloadQueue();
            return [
                'ok'         => true,
                'pending'    => $queue->pendingCount(),
                'processing' => $queue->processingCount(),
                'failed'     => $queue->failedCount(),
                'rows'       => $queue->listRows($limit),
            ];
        } catch (\Throwable $e) {
            return ['ok' => false, 'detail' => 'preload queue status failed'];
        }
    }
}
