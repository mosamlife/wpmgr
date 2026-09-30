<?php
/**
 * WPEngine — purge WP Engine's server-side caches.
 *
 * WP Engine's mu-plugin exposes the `WpeCommon` class whose static methods clear
 * each layer it runs: page cache (Varnish), object cache (memcached) and its CDN.
 * It does not offer a documented per-URL purge, so any WPMgr purge maps to a full
 * server-cache flush — correct, just coarser.
 *
 * Detection: the `WpeCommon` class (WP Engine's own signal). No-op otherwise.
 *
 * @package WPMgr\Agent\Integrations
 */

declare(strict_types=1);

namespace WPMgr\Agent\Integrations;

/**
 * WP Engine cache purger.
 */
final class WPEngine extends Integration
{
    /** Identifier in the purge report. */
    public const SLUG = 'wpengine';

    /** How far this host's purge reaches; see Integration. */
    protected const REACH = self::REACH_SHARED;

    /** Evidence for REACH. */
    protected const REACH_NOTE = 'Unverified: the host purge also clears its CDN and object cache; that each reaches only this site is not yet confirmed.';

    /**
     * @return bool
     */
    protected function detect(): bool
    {
        return class_exists('\WpeCommon');
    }

    /**
     * Flush every WP Engine cache layer that exposes a purge method.
     *
     * @return void
     */
    protected function purgeAll(): void
    {
        $class = '\WpeCommon';
        if (!class_exists($class)) {
            return;
        }
        foreach (['purge_varnish_cache', 'purge_memcached', 'clear_maxcdn_cache'] as $method) {
            if (method_exists($class, $method)) {
                $class::$method();
            }
        }
    }
}
