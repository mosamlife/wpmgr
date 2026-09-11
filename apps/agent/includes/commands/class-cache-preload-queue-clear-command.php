<?php
/**
 * CachePreloadQueueClearCommand — empty the preload queue.
 *
 * Wire contract (CP → agent, SIGNED dispatcher):
 *   POST /wp-json/wpmgr/v1/command/cache_preload_queue_clear
 *   Body: {}
 *
 * Response: { ok: bool, deleted: int }
 *
 * Deletes every row for the preload group+callback. Used by the viewer's
 * "Clear queue" action.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Cache\CacheManager;

/**
 * Deletes all preload-queue rows.
 */
final class CachePreloadQueueClearCommand implements CommandInterface
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
        return 'cache_preload_queue_clear';
    }

    /**
     * Effect: deletes every row from the preload queue. The queue is derived work-tracking state, not site
     * data.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Write;
    }

    /**
     * Repeatability: clearing an already-empty queue is a no-op.
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
     * @param array<string,mixed> $params Request params (unused).
     * @return array{ok:bool,deleted?:int,detail?:string}
     */
    public function execute(array $claims, array $params): array
    {
        try {
            $deleted = $this->cache->preloadQueue()->clearQueue();
            return ['ok' => true, 'deleted' => $deleted];
        } catch (\Throwable $e) {
            return ['ok' => false, 'detail' => 'preload queue clear failed'];
        }
    }
}
