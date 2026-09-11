<?php
/**
 * CachePreloadQueueRetryFailedCommand — revive permanently-failed rows.
 *
 * Wire contract (CP → agent, SIGNED dispatcher):
 *   POST /wp-json/wpmgr/v1/command/cache_preload_queue_retry_failed
 *   Body: {}
 *
 * Response: { ok: bool, revived: int }
 *
 * Flips every `failed` row back to `pending` (clearing locked_at so it is
 * immediately due) and re-kicks the loopback drain.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Cache\CacheManager;

/**
 * Revives failed preload-queue rows to pending and re-dispatches runners.
 */
final class CachePreloadQueueRetryFailedCommand implements CommandInterface
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
        return 'cache_preload_queue_retry_failed';
    }

    /**
     * Effect: flips failed queue rows back to pending and dispatches runners.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Write;
    }

    /**
     * Repeatability: a second call finds no failed rows left to revive.
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
     * @return array{ok:bool,revived?:int,detail?:string}
     */
    public function execute(array $claims, array $params): array
    {
        try {
            $queue   = $this->cache->preloadQueue();
            $revived = $queue->retryFailed();
            if ($revived > 0) {
                $queue->dispatchRunners();
            }
            return ['ok' => true, 'revived' => $revived];
        } catch (\Throwable $e) {
            return ['ok' => false, 'detail' => 'preload queue retry failed'];
        }
    }
}
