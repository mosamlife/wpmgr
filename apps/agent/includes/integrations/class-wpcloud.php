<?php
/**
 * WPCloud — purge the WP Cloud / Atomic edge cache.
 *
 * WP Cloud (the WordPress.com Atomic platform) runs an edge cache cleared via
 * the `Edge_Cache_Atomic` class (purge_domain()); the platform is identified by
 * the `Atomic_Persistent_Data` class. There is no per-URL edge purge, so a WPMgr
 * purge maps to a full domain purge.
 *
 * Detection: the Atomic platform + edge-cache classes (the host's own signal).
 * No-op otherwise.
 *
 * @package WPMgr\Agent\Integrations
 */

declare(strict_types=1);

namespace WPMgr\Agent\Integrations;

/**
 * WP Cloud / Atomic edge cache purger.
 */
final class WPCloud extends Integration
{
    /** Identifier in the purge report. */
    public const SLUG = 'wpcloud';

    /** How far this host's purge reaches; see Integration. */
    protected const REACH = self::REACH_SHARED;

    /** Evidence for REACH. */
    protected const REACH_NOTE = 'Unverified: the edge purge names this site\'s domain; that it reaches only that domain is not yet confirmed.';

    /**
     * @return bool
     */
    protected function detect(): bool
    {
        return class_exists('\Atomic_Persistent_Data') && class_exists('\Edge_Cache_Atomic');
    }

    /**
     * Purge the whole domain from the Atomic edge cache.
     *
     * @return void
     */
    protected function purgeAll(): void
    {
        if (!class_exists('\Edge_Cache_Atomic')) {
            return;
        }
        $edge = new \Edge_Cache_Atomic();
        if (!method_exists($edge, 'purge_domain') || !method_exists($edge, 'get_domain_name')) {
            return;
        }
        $domain = (string) $edge->get_domain_name();
        if ($domain === '') {
            return;
        }
        $edge->purge_domain($domain, [
            'wp_actions' => 'purge_cache',
            'wp_action'  => 'edge_cache_purge',
        ]);
    }
}
