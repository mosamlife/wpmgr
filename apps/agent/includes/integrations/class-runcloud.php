<?php
/**
 * RunCloud — purge RunCloud Hub's server cache (NGINX FastCGI / Redis).
 *
 * The RunCloud Hub mu-plugin exposes the static `RunCloud_Hub` class with
 * purge_cache_all() (single site) / purge_cache_all_sites() (multisite) and a
 * per-URL purge_cache_object($url). We use its OWN class so the purge runs
 * through RunCloud's supported path.
 *
 * Detection: the `RunCloud_Hub` class (RunCloud's own signal). No-op otherwise.
 *
 * @package WPMgr\Agent\Integrations
 */

declare(strict_types=1);

namespace WPMgr\Agent\Integrations;

/**
 * RunCloud Hub cache purger.
 */
final class RunCloud extends Integration
{
    /** Identifier in the purge report. */
    public const SLUG = 'runcloud';

    /** How far this host's purge reaches; see Integration. */
    protected const REACH = self::REACH_SHARED;

    /** Evidence for REACH. */
    protected const REACH_NOTE = 'Unverified: on multisite the host\'s purge covers every site in the network; single-site reach is not yet confirmed.';

    /**
     * @return bool
     */
    protected function detect(): bool
    {
        return class_exists('\RunCloud_Hub');
    }

    /**
     * @return void
     */
    protected function purgeAll(): void
    {
        $class = '\RunCloud_Hub';
        if (!class_exists($class)) {
            return;
        }
        if (function_exists('is_multisite') && \is_multisite() && method_exists($class, 'purge_cache_all_sites')) {
            $class::purge_cache_all_sites();
            return;
        }
        if (method_exists($class, 'purge_cache_all')) {
            $class::purge_cache_all();
        }
    }

    /**
     * @param list<string> $urls Validated absolute URLs.
     * @return void
     */
    protected function purgeUrls(array $urls): void
    {
        $class = '\RunCloud_Hub';
        if (class_exists($class) && method_exists($class, 'purge_cache_object')) {
            foreach ($urls as $url) {
                $class::purge_cache_object($url);
            }
            return;
        }
        $this->purgeAll();
    }
}
