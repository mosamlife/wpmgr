<?php
/**
 * SpinupWP — purge SpinupWP's NGINX FastCGI page cache.
 *
 * SpinupWP's mu-plugin (\SpinupWp\Cache) ships helper functions:
 * spinupwp_purge_site() (whole site) and spinupwp_purge_url($url) (per page).
 * We prefer the per-URL helper and fall back to the site purge.
 *
 * Detection: the SpinupWP cache class / its purge helpers (the host's own
 * signal). No-op otherwise.
 *
 * @package WPMgr\Agent\Integrations
 */

declare(strict_types=1);

namespace WPMgr\Agent\Integrations;

/**
 * SpinupWP FastCGI cache purger.
 */
final class SpinupWP extends Integration
{
    /** Identifier in the purge report. */
    public const SLUG = 'spinupwp';

    /** How far this host's purge reaches; see Integration. */
    protected const REACH = self::REACH_SHARED;

    /** Evidence for REACH. */
    protected const REACH_NOTE = 'Unverified: that the host\'s full and per-URL purges reach only this site is not yet confirmed.';

    /**
     * @return bool
     */
    protected function detect(): bool
    {
        return class_exists('\SpinupWp\Cache')
            || function_exists('spinupwp_purge_site')
            || function_exists('spinupwp_purge_url');
    }

    /**
     * @return void
     */
    protected function purgeAll(): void
    {
        if (function_exists('spinupwp_purge_site')) {
            \spinupwp_purge_site();
        }
    }

    /**
     * @param list<string> $urls Validated absolute URLs.
     * @return void
     */
    protected function purgeUrls(array $urls): void
    {
        if (function_exists('spinupwp_purge_url')) {
            foreach ($urls as $url) {
                \spinupwp_purge_url($url);
            }
            return;
        }
        $this->purgeAll();
    }
}
