<?php
/**
 * Cloudways — purge Cloudways' Varnish (and its optional Cloudflare) cache.
 *
 * Cloudways fronts PHP with Varnish and ships the Breeze plugin to control it.
 * Breeze exposes `Breeze_PurgeVarnish` (purge_cache() / purge_url($url)) and,
 * when the customer enables Cloudflare on the platform,
 * `Breeze_CloudFlare_Helper` (reset_all_cache() / purge_cloudflare_cache_urls()).
 * We drive Breeze's OWN classes so we use the platform's supported purge path.
 *
 * Detection: the Breeze classes (Cloudways' own signal). No-op otherwise.
 *
 * @package WPMgr\Agent\Integrations
 */

declare(strict_types=1);

namespace WPMgr\Agent\Integrations;

/**
 * Cloudways (Breeze/Varnish) cache purger.
 */
final class Cloudways extends Integration
{
    /** Identifier in the purge report. */
    public const SLUG = 'cloudways';

    /** How far this host's purge reaches; see Integration. */
    protected const REACH = self::REACH_SHARED;

    /** Evidence for REACH. */
    protected const REACH_NOTE = 'Shared: the host\'s purge clears a whole zone.';

    /**
     * @return bool
     */
    protected function detect(): bool
    {
        return class_exists('\Breeze_PurgeVarnish') || class_exists('\Breeze_CloudFlare_Helper');
    }

    /**
     * @return void
     */
    protected function purgeAll(): void
    {
        if (class_exists('\Breeze_PurgeVarnish')) {
            $varnish = new \Breeze_PurgeVarnish();
            if (method_exists($varnish, 'purge_cache')) {
                $varnish->purge_cache();
            }
        }
        $this->purgeCloudflareAll();
    }

    /**
     * @param list<string> $urls Validated absolute URLs.
     * @return void
     */
    protected function purgeUrls(array $urls): void
    {
        if (class_exists('\Breeze_PurgeVarnish')) {
            $varnish = new \Breeze_PurgeVarnish();
            if (method_exists($varnish, 'purge_url')) {
                foreach ($urls as $url) {
                    $varnish->purge_url($url);
                }
            }
        }

        if ($this->cloudflareEnabled() && method_exists('\Breeze_CloudFlare_Helper', 'purge_cloudflare_cache_urls')) {
            \Breeze_CloudFlare_Helper::purge_cloudflare_cache_urls($urls);
        } else {
            $this->purgeCloudflareAll();
        }
    }

    /**
     * Reset the Cloudways-managed Cloudflare zone if the customer enabled it.
     *
     * @return void
     */
    private function purgeCloudflareAll(): void
    {
        if ($this->cloudflareEnabled() && method_exists('\Breeze_CloudFlare_Helper', 'reset_all_cache')) {
            \Breeze_CloudFlare_Helper::reset_all_cache();
        }
    }

    /**
     * @return bool
     */
    private function cloudflareEnabled(): bool
    {
        return class_exists('\Breeze_CloudFlare_Helper')
            && method_exists('\Breeze_CloudFlare_Helper', 'is_cloudflare_enabled')
            && (bool) \Breeze_CloudFlare_Helper::is_cloudflare_enabled();
    }
}
