<?php
/**
 * GridPane — purge GridPane's NGINX (FastCGI / Redis) page cache.
 *
 * GridPane drives cache purges through its `Nginx_Cache_Purger_Admin` helper
 * (register_purge() flushes the server cache) and, on Redis-page-cache stacks,
 * the Nginx Helper-style `rt_nginx_helper_purge_all` action. We call the host's
 * own purger so the flush goes through GridPane's supported mechanism.
 *
 * Detection: the GridPane purger class / its purge action (the host's own
 * signal). No-op otherwise.
 *
 * @package WPMgr\Agent\Integrations
 */

declare(strict_types=1);

namespace WPMgr\Agent\Integrations;

/**
 * GridPane NGINX/Redis cache purger.
 */
final class GridPane extends Integration
{
    /** Identifier in the purge report. */
    public const SLUG = 'gridpane';

    /** How far this host's purge reaches; see Integration. */
    protected const REACH = self::REACH_SHARED;

    /** Evidence for REACH. */
    protected const REACH_NOTE = 'Unverified: that the server\'s page-cache purge reaches only this site is not yet confirmed.';

    /**
     * @return bool
     */
    protected function detect(): bool
    {
        return class_exists('\Nginx_Cache_Purger_Admin');
    }

    /**
     * @return void
     */
    protected function purgeAll(): void
    {
        if (!class_exists('\Nginx_Cache_Purger_Admin')) {
            return;
        }
        $purger = new \Nginx_Cache_Purger_Admin();
        if (method_exists($purger, 'register_purge')) {
            $purger->register_purge();
            return;
        }
        if (method_exists($purger, 'purge_all')) {
            $purger->purge_all();
        }
    }
}
