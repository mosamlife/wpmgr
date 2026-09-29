<?php
/**
 * SiteGround — purge SiteGround's dynamic / NGINX cache.
 *
 * The SiteGround Optimizer plugin exposes a `Supercacher` singleton
 * (\SiteGround_Optimizer\Supercacher\Supercacher) with purge_everything() and
 * purge_cache_request($url), plus the legacy `sg_cachepress_purge_everything()`
 * helper. We prefer the singleton's per-URL purge and fall back to a full purge.
 *
 * Detection: the Supercacher class / sg_cachepress helper (SiteGround's own
 * signal). No-op otherwise.
 *
 * @package WPMgr\Agent\Integrations
 */

declare(strict_types=1);

namespace WPMgr\Agent\Integrations;

/**
 * SiteGround SuperCacher purger.
 */
final class SiteGround extends Integration
{
    /** Identifier in the purge report. */
    public const SLUG = 'siteground';

    /** How far this host's purge reaches; see Integration. */
    protected const REACH = self::REACH_SHARED;

    /** Evidence for REACH. */
    protected const REACH_NOTE = 'Unverified: that the host\'s full and per-URL purges reach only this site is not yet confirmed.';

    /** @var class-string */
    private const SUPERCACHER = '\SiteGround_Optimizer\Supercacher\Supercacher';

    /**
     * @return bool
     */
    protected function detect(): bool
    {
        return class_exists(self::SUPERCACHER)
            || function_exists('sg_cachepress_purge_everything');
    }

    /**
     * @return void
     */
    protected function purgeAll(): void
    {
        $cacher = $this->cacher();
        if ($cacher !== null && method_exists($cacher, 'purge_everything')) {
            $cacher->purge_everything();
            return;
        }
        if (function_exists('sg_cachepress_purge_everything')) {
            \sg_cachepress_purge_everything();
        }
    }

    /**
     * @param list<string> $urls Validated absolute URLs.
     * @return void
     */
    protected function purgeUrls(array $urls): void
    {
        $cacher = $this->cacher();
        if ($cacher !== null && method_exists($cacher, 'purge_cache_request')) {
            foreach ($urls as $url) {
                $cacher->purge_cache_request($url);
            }
            return;
        }
        $this->purgeAll();
    }

    /**
     * The Supercacher singleton, if available.
     *
     * @return object|null
     */
    private function cacher(): ?object
    {
        $class = self::SUPERCACHER;
        if (!class_exists($class) || !method_exists($class, 'get_instance')) {
            return null;
        }
        $instance = $class::get_instance();
        return is_object($instance) ? $instance : null;
    }
}
