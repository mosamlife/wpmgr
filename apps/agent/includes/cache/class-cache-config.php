<?php
/**
 * CacheConfig — the typed page-cache configuration value object.
 *
 * The CP pushes the page-cache settings via the `perf.config.update` command;
 * this object is the in-memory shape the manager, writer, drop-in renderer, and
 * htaccess manager all consume. It is also the exact array that gets var_export-
 * inlined into the serving drop-in (so the drop-in needs zero DB load).
 *
 * @package WPMgr\Agent\Cache
 */

declare(strict_types=1);

namespace WPMgr\Agent\Cache;

/**
 * Immutable page-cache configuration.
 */
final class CacheConfig
{
    /** Master enable flag for page caching. */
    public bool $enabled;

    /** Whether logged-in users get cached (role-segmented) pages. */
    public bool $cacheLoggedIn;

    /** Whether a separate mobile cache bucket is maintained. */
    public bool $cacheMobile;

    /** Auto-purge + preload changed URLs on content updates. */
    public bool $autoPurge;

    /** Scheduled full-cache refresh interval in seconds (0 = disabled). */
    public int $refreshInterval;

    /**
     * P4a: whether the drop-in fires a WP-Cron loopback kick on cache HITs.
     * Default true — keeps WP-Cron alive on page-cached idle sites.
     */
    public bool $cronKickEnabled;

    /**
     * P4a: minimum seconds between loopback cron kicks on cache HITs.
     * Default 60 — one kick per minute maximum; never less than 10.
     */
    public int $cronKickInterval;

    /**
     * Extra cache-varying query params (operator additions to the include list).
     *
     * @var list<string>
     */
    public array $includeQueries;

    /**
     * Cookie names whose values fragment the cache, in order.
     *
     * @var list<string>
     */
    public array $includeCookies;

    /**
     * URL substrings that bypass the cache.
     *
     * @var list<string>
     */
    public array $bypassUrls;

    /**
     * Cookie-name keywords that bypass the cache.
     *
     * @var list<string>
     */
    public array $bypassCookies;

    /**
     * When true, allow anonymous WooCommerce shoppers who hold only a Woo cart or
     * session cookie to receive a shared cached shell. The three Woo cart/session
     * cookie patterns are moved from the hard-bypass set into a non-keying,
     * non-bypassing ignore set so an empty-cart shell is served and the per-user
     * cart widget is repainted client-side by WooCommerce's own cart-fragments.
     *
     * DEFAULT-OFF. When false, behaviour is byte-identical to today.
     */
    public bool $wooCacheableSession;

    /**
     * Operator-configured include-queries, BEFORE i18n/currency presets are folded
     * in. Persisted as-is (so round-trips don't bake auto-detected presets into the
     * stored config) while {@see $includeQueries} carries the effective set.
     *
     * @var list<string>
     */
    public array $operatorIncludeQueries;

    /**
     * Operator-configured include-cookies, BEFORE i18n/currency presets are folded
     * in. See {@see $operatorIncludeQueries}.
     *
     * @var list<string>
     */
    public array $operatorIncludeCookies;

    /**
     * @param array<string,mixed> $data Raw config map (from storage or CP).
     */
    public function __construct(array $data = [])
    {
        $this->enabled           = (bool) ($data['enabled'] ?? false);
        $this->cacheLoggedIn     = (bool) ($data['cache_logged_in'] ?? false);
        $this->cacheMobile       = (bool) ($data['cache_mobile'] ?? false);
        $this->autoPurge         = (bool) ($data['auto_purge'] ?? true);
        $this->refreshInterval   = max(0, (int) ($data['refresh_interval'] ?? 0));
        $this->cronKickEnabled   = isset($data['cron_kick_enabled']) ? (bool) $data['cron_kick_enabled'] : true;
        $this->cronKickInterval  = isset($data['cron_kick_interval']) ? max(10, (int) $data['cron_kick_interval']) : 60;

        $this->operatorIncludeQueries = self::stringList($data['include_queries'] ?? []);
        $this->operatorIncludeCookies = self::stringList($data['include_cookies'] ?? []);

        // Fold in the auto-detected i18n/multi-currency presets so language and
        // currency variants fragment the cache key WITHOUT the operator having to
        // hand-configure each plugin's cookie/param. Operator config still wins
        // (it is merged first); presets are appended as defaults. The unmerged
        // operator lists are preserved above so {@see toArray()} round-trips the
        // operator's intent, not the auto-detected set.
        $this->includeQueries  = EcosystemPresets::effectiveIncludeQueries($this->operatorIncludeQueries);
        $this->includeCookies  = EcosystemPresets::effectiveIncludeCookies($this->operatorIncludeCookies);

        $this->bypassUrls           = self::stringList($data['bypass_urls'] ?? []);
        $this->bypassCookies        = self::stringList($data['bypass_cookies'] ?? []);
        $this->wooCacheableSession  = (bool) ($data['woo_cacheable_session'] ?? false);
    }

    /**
     * The array form inlined into the drop-in (lean: only the keys the serving
     * fast path reads). Marketing-ignore params are added here so the drop-in
     * does not need the MarketingParams class.
     *
     * @return array<string,mixed>
     */
    public function toDropinArray(): array
    {
        // Read the persisted theme-fragments support probe result. Only relevant
        // when the operator flag is on — if off, the Woo cookies stay in bypass
        // regardless and there is no point reading the option.
        // Written by PerfReporter::persistWooSupported() on each reportStats cycle.
        $wooSupported = false;
        if ($this->wooCacheableSession) {
            $wooSupported = (bool) (function_exists('get_option')
                ? get_option(PerfReporter::OPTION_WOO_FRAGMENTS_SUPPORTED, false)
                : false);
        }

        // The Woo cookies are moved to the ignore set only when BOTH the operator
        // flag is on AND the agent's own probe has confirmed fragment support.
        // When either is false the behaviour is byte-identical to flag-off (full
        // bypass on all three Woo cart/session cookie patterns).
        $wooActive = $this->wooCacheableSession && $wooSupported;

        return [
            'cache_logged_in'       => $this->cacheLoggedIn,
            'cache_mobile'          => $this->cacheMobile,
            'include_cookies'       => $this->includeCookies,
            // The baked-in default "always-bypass" cookies (WooCommerce/EDD cart,
            // session, logged-in, password, comment-author) are merged with the
            // operator list via the SAME helper the PHP cacheability path uses, so
            // the pre-WP drop-in and the PHP write layer bypass an identical set.
            // This is what prevents a logged-out cart page from ever being served
            // from (or written to) the shared disk cache.
            // When woo_cacheable_session is ON AND woo_supported is confirmed, the
            // three Woo cart/session patterns are moved to the ignore set so they
            // neither bypass nor key the cache.
            'bypass_cookies'        => Cacheability::effectiveBypassCookies($this->bypassCookies, $wooActive),
            // URL substrings that bypass the cache. Inlined into the drop-in so the
            // pre-WP fast path can reject already-warmed cache files for URLs the
            // operator marked as non-cacheable.
            'bypass_urls'           => $this->bypassUrls,
            'woo_ignore_cookies'    => $wooActive ? Cacheability::WOO_SESSION_COOKIES : [],
            'ignore_queries'        => MarketingParams::ignoreList(),
            'woo_cacheable_session' => $this->wooCacheableSession,
            // Baked probe result: the drop-in can read this for diagnostics; the
            // PHP write path reads it live from the option (same source of truth).
            'woo_supported'         => $wooSupported,
            // P4a: WP-Cron loopback kick on cache HITs — keeps WP-Cron alive on
            // fully page-cached idle sites where no PHP-booting visitor traffic
            // exists. Baked in so the drop-in reads it without any DB/WP calls.
            'cron_kick_enabled'     => $this->cronKickEnabled,
            'cron_kick_interval'    => $this->cronKickInterval,
            // The site's own token: a request carrying it in the preload
            // header is one of the agent's self-fetches, rendered fresh and
            // stored. Kept in its own option, never in toArray(), so a config
            // push from the control plane cannot replace it.
            'preload_token'         => PreloadToken::get(),
        ];
    }

    /**
     * Full serialisable form (storage + CP round-trips).
     *
     * @return array<string,mixed>
     */
    public function toArray(): array
    {
        return [
            'enabled'           => $this->enabled,
            'cache_logged_in'   => $this->cacheLoggedIn,
            'cache_mobile'      => $this->cacheMobile,
            'auto_purge'        => $this->autoPurge,
            'refresh_interval'  => $this->refreshInterval,
            // Persist the OPERATOR-configured lists, not the preset-folded effective
            // ones, so a save→load round-trip never bakes auto-detected i18n/currency
            // presets into stored config (they are re-derived live on each load).
            'include_queries'       => $this->operatorIncludeQueries,
            'include_cookies'       => $this->operatorIncludeCookies,
            'bypass_urls'           => $this->bypassUrls,
            'bypass_cookies'        => $this->bypassCookies,
            'woo_cacheable_session' => $this->wooCacheableSession,
            // P4a: cron kick settings (persisted so the CP can configure them).
            'cron_kick_enabled'     => $this->cronKickEnabled,
            'cron_kick_interval'    => $this->cronKickInterval,
        ];
    }

    /**
     * Coerce a mixed value into a clean list of non-empty strings.
     *
     * @param mixed $value Candidate list.
     * @return list<string>
     */
    private static function stringList($value): array
    {
        if (!is_array($value)) {
            return [];
        }
        $out = [];
        foreach ($value as $item) {
            if (is_scalar($item)) {
                $s = trim((string) $item);
                if ($s !== '') {
                    $out[] = $s;
                }
            }
        }
        return array_values(array_unique($out));
    }
}
