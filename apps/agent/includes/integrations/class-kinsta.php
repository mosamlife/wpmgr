<?php
/**
 * Kinsta — purge Kinsta's edge/server cache.
 *
 * Kinsta ships its "Kinsta MU" plugin which exposes a `$kinsta_cache` global
 * whose `->kinsta_cache_purge` object purges by URL or wholesale, and registers
 * an internal `/kinsta-clear-cache-all` REST route. We drive its OWN object so
 * the purge goes through Kinsta's supported path.
 *
 * Detection: the `$kinsta_cache` global (Kinsta's own signal). No-op otherwise.
 *
 * @package WPMgr\Agent\Integrations
 */

declare(strict_types=1);

namespace WPMgr\Agent\Integrations;

/**
 * Kinsta cache purger.
 */
final class Kinsta extends Integration
{
    /** Identifier in the purge report. */
    public const SLUG = 'kinsta';

    /** How far this host's purge reaches; see Integration. */
    protected const REACH = self::REACH_SHARED;

    /** Evidence for REACH. */
    protected const REACH_NOTE = 'Unverified: that the host\'s full and per-URL purges reach only this site is not yet confirmed.';

    /**
     * @return bool
     */
    protected function detect(): bool
    {
        return $this->purger() !== null;
    }

    /**
     * @return void
     */
    protected function purgeAll(): void
    {
        $purger = $this->purger();
        if ($purger !== null && method_exists($purger, 'purge_complete_caches')) {
            $purger->purge_complete_caches();
        }
    }

    /**
     * Purge by URL where Kinsta exposes it; else fall back to a full purge.
     *
     * @param list<string> $urls Validated absolute URLs.
     * @return void
     */
    protected function purgeUrls(array $urls): void
    {
        $purger = $this->purger();
        if ($purger !== null && method_exists($purger, 'purge_caches_urls')) {
            $purger->purge_caches_urls($urls);
            return;
        }
        $this->purgeAll();
    }

    /**
     * Resolve Kinsta's purge object from its global.
     *
     * @return object|null
     */
    private function purger(): ?object
    {
        if (!isset($GLOBALS['kinsta_cache']) || !is_object($GLOBALS['kinsta_cache'])) {
            return null;
        }
        $cache = $GLOBALS['kinsta_cache'];
        return isset($cache->kinsta_cache_purge) && is_object($cache->kinsta_cache_purge)
            ? $cache->kinsta_cache_purge
            : null;
    }
}
