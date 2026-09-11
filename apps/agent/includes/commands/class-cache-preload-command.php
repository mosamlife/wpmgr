<?php
/**
 * CachePreloadCommand — queues URLs for cache warming.
 *
 * Wire contract (CP → agent):
 *   POST /wp-json/wpmgr/v1/command/cache_preload
 *   Authorization: Bearer <Ed25519 JWT, cmd="cache_preload", aud=<siteId>>
 *   Body: { "urls": ["https://…/a", "https://…/b"] }  // empty ⇒ warm the home page
 *
 * Response: { "ok": <bool>, "detail": "<text>", "queued": <int> }
 *
 * The warming itself runs in the background (wp-cron, desktop + mobile UAs,
 * 0.5s spacing); this command only enqueues + schedules.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Cache\CacheManager;

/**
 * Enqueues URLs for background preload.
 */
final class CachePreloadCommand implements CommandInterface
{
    /** Hard cap on URLs accepted in one push (defence against an oversized body). */
    private const MAX_URLS = 1000;

    private CacheManager $cache;

    /**
     * @param CacheManager $cache Page-cache orchestrator.
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
        return 'cache_preload';
    }

    /**
     * Effect: queues background warm jobs for the supplied URLs. Only the preload queue and the page cache
     * change, both derived.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Write;
    }

    /**
     * Repeatability: a second call re-queues the same URLs: real work is redone, nothing is lost.
     *
     * @return CommandRepeatability
     */
    public function repeatability(): CommandRepeatability
    {
        return CommandRepeatability::Repeatable;
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused).
     * @param array<string,mixed> $params { urls?: string[] }.
     * @return array{ok:bool,detail:string,queued?:int}
     */
    public function execute(array $claims, array $params): array
    {
        $urls = [];
        if (isset($params['urls'])) {
            if (!is_array($params['urls'])) {
                return ['ok' => false, 'detail' => 'urls must be an array'];
            }
            foreach ($params['urls'] as $url) {
                if (is_string($url) && $url !== '') {
                    $urls[] = $url;
                }
                if (count($urls) >= self::MAX_URLS) {
                    break;
                }
            }
        }

        try {
            return $this->cache->preload($urls);
        } catch (\Throwable $e) {
            return ['ok' => false, 'detail' => 'cache preload failed'];
        }
    }
}
