<?php
/**
 * CacheDisableCommand — turns page caching off and cleanly reverses every
 * server-side change (.htaccess block, drop-in, WP_CACHE define) and purges the
 * cache.
 *
 * Wire contract (CP → agent):
 *   POST /wp-json/wpmgr/v1/command/cache_disable
 *   Authorization: Bearer <Ed25519 JWT, cmd="cache_disable", aud=<siteId>>
 *   Body: {}
 *
 * Response: { "ok": <bool>, "detail": "<text>", "steps": {...} }
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Cache\CacheManager;

/**
 * Disables the page cache and reverses all artefacts.
 */
final class CacheDisableCommand implements CommandInterface
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
        return 'cache_disable';
    }

    /**
     * Effect: turns page caching off and reverses every artefact -- the WP_CACHE define, the drop-in and the
     * .htaccess block -- then purges, and is a Write: stopping the site from serving cached pages to the next
     * visitor is the whole reason this command exists. Everything it removes is config plus cache content the
     * site rebuilds on its own if caching is re-enabled; that does not decide the label -- purpose does.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Write;
    }

    /**
     * Repeatability: disabling an already-disabled cache converges on the same state.
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
     * @param array<string,mixed> $params Request body (ignored).
     * @return array{ok:bool,detail:string,steps?:array<string,bool>}
     */
    public function execute(array $claims, array $params): array
    {
        try {
            return $this->cache->disable();
        } catch (\Throwable $e) {
            return ['ok' => false, 'detail' => 'cache disable failed'];
        }
    }
}
