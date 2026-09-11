<?php
/**
 * CachePurgeCommand — purges the page cache, either everything or a single URL.
 *
 * Wire contract (CP → agent):
 *   POST /wp-json/wpmgr/v1/command/cache_purge
 *   Authorization: Bearer <Ed25519 JWT, cmd="cache_purge", aud=<siteId>>
 *   Body: { "scope": "all" }                       // purge everything
 *      or { "scope": "url", "url": "https://…/x" } // purge one URL's variants
 *
 * Response: { "ok": <bool>, "detail": "<text>", "removed": <int>, "stats": {...} }
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Cache\CacheManager;

/**
 * Purges the page cache (all or per-URL).
 */
final class CachePurgeCommand implements CommandInterface
{
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
        return 'cache_purge';
    }

    /**
     * Effect: empties the page cache, all or per-URL. The cache is derived state.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Write;
    }

    /**
     * Repeatability: purging an already-cold cache is a no-op.
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
     * @param array<string,mixed> $params { scope?: "all"|"url", url?: string }.
     * @return array{ok:bool,detail:string,removed?:int,stats?:array<string,mixed>}
     */
    public function execute(array $claims, array $params): array
    {
        $scope = isset($params['scope']) && is_string($params['scope'])
            ? strtolower($params['scope']) : 'all';

        try {
            if ($scope === 'url') {
                if (!isset($params['url']) || !is_string($params['url']) || $params['url'] === '') {
                    return ['ok' => false, 'detail' => 'scope=url requires a url'];
                }
                $result = $this->cache->purge($params['url']);
            } else {
                $result = $this->cache->purge('all');
            }

            $result['stats'] = $this->cache->stats();
            return $result;
        } catch (\Throwable $e) {
            return ['ok' => false, 'detail' => 'cache purge failed'];
        }
    }
}
