<?php
/**
 * CachePurgeCommand — purges the page cache, either everything or a single URL.
 *
 * Wire contract (CP → agent):
 *   POST /wp-json/wpmgr/v1/command/cache_purge
 *   Authorization: Bearer <Ed25519 JWT, cmd="cache_purge", aud=<siteId>>
 *   Body: { "scope": "all" }                       // purge everything
 *      or { "scope": "url", "url": "https://…/x" } // purge one URL's variants
 *   Either body may add "origin_only": true, which keeps the clear inside this
 *   site: host and edge integrations whose reach is not confirmed to be this
 *   install do not run.
 *
 * Response: { "ok": <bool>, "detail": "<text>", "removed": <int>, "stats": {...} }
 *   With origin_only, the response adds:
 *     "origin_only_honoured": true,
 *     "integrations": [ { "slug": "<integration>", "action": "<action>" } ]
 *   where action is one of purged_all, purged_urls, purged_urls_exact or
 *   skipped_reach_unconfirmed. Only integrations detected on this site appear.
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
     * Effect: empties the page cache, all or per-URL, and is a Write -- changing what every visitor is served
     * next is the whole reason it exists. The cache is derived state the site rebuilds by itself; that does
     * not decide the label -- purpose does.
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
     * @param array<string,mixed> $params { scope?: "all"|"url", url?: string, origin_only?: bool }.
     * @return array{ok:bool,detail:string,removed?:int,stats?:array<string,mixed>,origin_only_honoured?:bool,integrations?:list<array{slug:string,action:string}>}
     */
    public function execute(array $claims, array $params): array
    {
        $scope = isset($params['scope']) && is_string($params['scope'])
            ? strtolower($params['scope']) : 'all';

        // origin_only asks for a clear that stays inside this site. A value that
        // is present but not a boolean is refused rather than read as false,
        // because reading it as false would widen the clear the caller asked for.
        $originOnly = false;
        if (array_key_exists('origin_only', $params)) {
            if (!is_bool($params['origin_only'])) {
                return ['ok' => false, 'detail' => 'origin_only must be a boolean'];
            }
            $originOnly = $params['origin_only'];
        }
        $options = $originOnly ? ['origin_only' => true] : [];

        try {
            if ($scope === 'url') {
                if (!isset($params['url']) || !is_string($params['url']) || $params['url'] === '') {
                    return ['ok' => false, 'detail' => 'scope=url requires a url'];
                }
                $result = $this->cache->purge($params['url'], $options);
            } else {
                $result = $this->cache->purge('all', $options);
            }

            $result['stats'] = $this->cache->stats();
            return $result;
        } catch (\Throwable $e) {
            return ['ok' => false, 'detail' => 'cache purge failed'];
        }
    }
}
