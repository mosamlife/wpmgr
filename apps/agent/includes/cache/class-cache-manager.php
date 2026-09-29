<?php
/**
 * CacheManager — the page-cache orchestrator.
 *
 * Owns the page-cache config (one wp-option), wires the request-path hooks
 * (output-buffer writer, auto-purge, role cookie, refresh cron), and exposes the
 * high-level operations the CP command handlers call:
 *
 *   enable()        — write WP_CACHE, install the drop-in, add the .htaccess block.
 *   disable()       — reverse all three cleanly.
 *   purge(all|url)  — delegate to Purge.
 *   preload(urls)   — delegate to Preload.
 *   applyConfig()   — persist a new config, re-render the drop-in + re-eval the
 *                     .htaccess mobile flag, re-arm the refresh cron.
 *   stats()         — page count + bytes for the heartbeat.
 *
 * The config is stored as a single wp-option (autoload off). On the request path
 * the manager reads it cheaply once and, if caching is enabled, opens the output
 * buffer as early as template_redirect.
 *
 * @package WPMgr\Agent\Cache
 */

declare(strict_types=1);

namespace WPMgr\Agent\Cache;

use WPMgr\Agent\Integrations\Integration;

/**
 * Orchestrates the page-cache lifecycle and request hooks.
 */
final class CacheManager
{
    /** Single wp-option storing the serialised CacheConfig. */
    public const OPTION_CONFIG = 'wpmgr_cache_config';

    /** WP option flagging the last enable/disable error for the admin/CP. */
    public const OPTION_LAST_ERROR = 'wpmgr_cache_last_error';

    /**
     * Cheap stats snapshot for the heartbeat. Refreshed on every state change
     * (enable/disable/purge/applyConfig) so the 60s heartbeat reads this option
     * directly instead of walking the cache dir on every tick.
     */
    public const OPTION_STATS = 'wpmgr_cache_stats';

    private WpCacheConstant $wpCache;

    private DropinInstaller $dropin;

    private HtaccessManager $htaccess;

    private NginxHelper $nginx;

    /** Cached config for this request. */
    private ?CacheConfig $config = null;

    /**
     * @param WpCacheConstant|null $wpCache  WP_CACHE editor.
     * @param DropinInstaller|null $dropin   Drop-in installer.
     * @param HtaccessManager|null $htaccess .htaccess manager.
     * @param NginxHelper|null     $nginx    Nginx snippet helper.
     */
    public function __construct(
        ?WpCacheConstant $wpCache = null,
        ?DropinInstaller $dropin = null,
        ?HtaccessManager $htaccess = null,
        ?NginxHelper $nginx = null
    ) {
        $this->wpCache  = $wpCache ?? new WpCacheConstant();
        $this->dropin   = $dropin ?? new DropinInstaller();
        $this->htaccess = $htaccess ?? new HtaccessManager();
        $this->nginx    = $nginx ?? new NginxHelper();
    }

    /**
     * Bind request-path hooks. Only opens the output buffer + auto-purge when
     * caching is actually enabled, so an inert site has zero request overhead
     * beyond a single option read.
     *
     * @return void
     */
    public function registerHooks(): void
    {
        $config = $this->config();

        // The role cookie + refresh cron schedule are armed regardless so a
        // later enable already has the role data and the cron is consistent.
        (new LoggedInRoles())->registerHooks();

        $refresh = new CacheRefreshCron(
            $this->purgeEngine(),
            $this->preloadEngine(),
            $config->refreshInterval
        );
        $refresh->registerHooks();

        // The preload cron handler is always bound (it self-no-ops when empty).
        if (function_exists('add_action')) {
            add_action(Preload::HOOK, [$this, 'runPreload']);
        }

        if (!$config->enabled) {
            return;
        }

        // Auto-purge on content change.
        if ($config->autoPurge) {
            (new AutoPurge($this->purgeEngine(), $this->preloadEngine()))->registerHooks();
        }

        // Output-buffer writer: open as early as template_redirect for front-end
        // GET requests — INCLUDING preload / RUCSS-compute self-fetches (which
        // carry the x-wpmgr-preload header). The drop-in already serves those
        // requests fresh (no HIT), and this buffer is exactly what (a) writes the
        // freshly rendered page to the disk cache — so preload actually WARMS the
        // cache — and (b) runs the optimization + RUCSS pipeline. Skipping it on
        // preload made preload a no-op for warming AND made "Compute Used-CSS now"
        // never trigger the optimizer (the buffer-bound optimizer never ran). The
        // warmer still receives the rendered HTML — the buffer returns it verbatim.
        if (function_exists('add_action')) {
            add_action('template_redirect', [$this, 'startBuffer'], 0);
        }
    }

    /**
     * Open the page-cache output buffer (bound to template_redirect).
     *
     * @return void
     */
    public function startBuffer(): void
    {
        $writer = new CacheWriter($this->config(), $this->cacheRoot());
        // Full-page output buffer with a callback handler: this is the standard
        // page-cache capture pattern. PHP finalizes the buffer at request end,
        // invoking CacheWriter::handle() with the complete response. The buffer
        // is intentionally held open for the entire request lifecycle so the
        // handler receives the full page output — an in-scope ob_get_clean() is
        // not applicable here because the response is not yet complete.
        ob_start([$writer, 'handle']);
    }

    /**
     * Preload cron callback. Passes a PerfReporter so progress/completion are
     * reported back to the CP after each batch (fire-and-forget).
     *
     * @return void
     */
    public function runPreload(): void
    {
        $reporter = $this->makePerfReporter();
        $this->preloadEngine()->run(50, $reporter);
    }

    // -------------------------------------------------------------------------
    // High-level operations (called by command handlers)
    // -------------------------------------------------------------------------

    /**
     * Enable page caching: WP_CACHE define → drop-in → .htaccess block. Persists
     * `enabled=true` in the config so the request path activates on next load.
     *
     * @return array{ok:bool,detail:string,steps:array<string,bool>}
     */
    public function enable(): array
    {
        $config = $this->config();
        $config->enabled = true;
        $this->saveConfig($config);

        $steps = [
            'wp_cache' => $this->wpCache->enable(),
            'dropin'   => $this->dropin->install($config->toDropinArray()),
            'htaccess' => $this->htaccessApply($config),
        ];

        // Nginx has no .htaccess fast-path; surface the manual snippet instead of
        // treating the missing block as a failure.
        $nginxNotice = false;
        if ($this->nginx->isNginx()) {
            $steps['htaccess'] = true;
            $nginxNotice = true;
            if (function_exists('update_option')) {
                update_option(NginxHelper::OPTION_NGINX_NOTICE, 1, false);
            }
        }

        $ok = $steps['wp_cache'] && $steps['dropin'] && $steps['htaccess'];
        $detail = $ok ? 'cache enabled' : 'cache enable incomplete';
        if ($nginxNotice) {
            $detail .= ' (nginx: add the manual location snippet)';
        }
        $this->recordError($ok ? '' : $detail);

        return ['ok' => $ok, 'detail' => $detail, 'steps' => $steps];
    }

    /**
     * Disable page caching: remove the .htaccess block, the drop-in, and the
     * WP_CACHE define, then purge everything and flip `enabled=false`.
     *
     * @return array{ok:bool,detail:string,steps:array<string,bool>}
     */
    public function disable(): array
    {
        $config = $this->config();
        $config->enabled = false;
        $this->saveConfig($config);

        $steps = [
            'htaccess' => $this->htaccess->uninstall(),
            'dropin'   => $this->dropin->uninstall(),
            'wp_cache' => $this->wpCache->disable(),
            'purge'    => $this->purgeEngine()->purgeEverything(),
        ];

        if (function_exists('delete_option')) {
            delete_option(NginxHelper::OPTION_NGINX_NOTICE);
        }
        $refresh = new CacheRefreshCron($this->purgeEngine(), $this->preloadEngine(), 0);
        $refresh->clear();

        $ok = $steps['htaccess'] && $steps['dropin'] && $steps['wp_cache'];
        $detail = $ok ? 'cache disabled' : 'cache disable incomplete';
        $this->recordError($ok ? '' : $detail);

        return ['ok' => $ok, 'detail' => $detail, 'steps' => $steps];
    }

    /**
     * Purge the cache. $url empty / "all" purges everything; otherwise purge that
     * single URL's variants.
     *
     * With ['origin_only' => true] the purge stays inside this site: a host or
     * edge integration whose reach is not confirmed to be this install does not
     * run, and the result reports what each detected integration did.
     *
     * @param string              $url     Target URL, or '' / 'all' for everything.
     * @param array<string,mixed> $options { origin_only?: bool }.
     * @return array{ok:bool,detail:string,removed:int,origin_only_honoured?:bool,integrations?:list<array{slug:string,action:string}>}
     */
    public function purge(string $url = '', array $options = []): array
    {
        $originOnly   = ($options['origin_only'] ?? false) === true;
        $purgeOptions = $originOnly ? ['origin_only' => true] : [];
        $purge        = $this->purgeEngine();

        if ($originOnly) {
            Integration::beginReport();
        }
        try {
            if ($url === '' || strtolower($url) === 'all') {
                $ok     = $purge->purgeEverything($purgeOptions);
                $result = ['ok' => $ok, 'detail' => $ok ? 'purged all' : 'purge failed', 'removed' => -1];
            } else {
                $removed = $purge->purgeUrl($url, $purgeOptions);
                $result  = ['ok' => true, 'detail' => 'purged url', 'removed' => $removed];
            }
        } finally {
            $report = $originOnly ? Integration::endReport() : [];
        }

        if ($originOnly) {
            $result['origin_only_honoured'] = true;
            $result['integrations']         = $report;
        }
        return $result;
    }

    /**
     * Queue URLs for preload warming.
     *
     * @param list<string> $urls URLs to warm (empty ⇒ warm the home page).
     * @return array{ok:bool,detail:string,queued:int}
     */
    public function preload(array $urls = []): array
    {
        // Empty set ⇒ full-site warm: enumerate EVERY cacheable front-end URL
        // (home, all public post types incl. WooCommerce products, all taxonomy
        // term archives, author archives) rather than warming only the home page.
        if ($urls === []) {
            $urls = $this->enumerateSiteUrls();
        }
        $queued = $this->preloadEngine()->queue($urls);
        // Return BOTH `total` (the CP contract reads this as the live-progress
        // denominator) and `queued` (back-compat).
        return [
            'ok'     => true,
            'detail' => 'preload queued ' . $queued . ' url(s)',
            'total'  => $queued,
            'queued' => $queued,
        ];
    }

    /** Hard ceiling on the full-site URL set so the persisted queue stays bounded. */
    private const PRELOAD_MAX_URLS = 10000;

    /**
     * Enumerate the full set of cacheable front-end URLs to warm. Mirrors a
     * standard full-site warm: (1) home; (2) every PUBLISHED entry of every PUBLIC
     * post type — one get_post_types(public=true) query auto-covers pages, posts,
     * WooCommerce products, and any CPT (no special-casing); (3) every non-empty
     * term archive of every public+rewritable taxonomy (category, tag, product_cat,
     * product_tag, attributes); (4) author archives for users with published posts.
     * Wrapped in wp_suspend_cache_addition so the crawl does not balloon the object
     * cache; queries skip meta/term cache + found-rows for speed. Best-effort:
     * whatever is collected is returned even if a sub-step fails. The preload
     * engine SSRF-filters each URL to same-host before warming.
     *
     * @return list<string>
     */
    public function enumerateSiteUrls(): array
    {
        $urls = [];
        if (function_exists('home_url')) {
            $urls[] = (string) home_url('/');
        }
        if (!function_exists('get_post_types') || !function_exists('get_permalink')) {
            return array_values(array_unique($urls));
        }

        $suspended = false;
        if (function_exists('wp_suspend_cache_addition')) {
            wp_suspend_cache_addition(true);
            $suspended = true;
        }
        try {
            // 1. Every published entry of every public post type (excl. attachments).
            $postTypes = get_post_types(['public' => true, 'exclude_from_search' => false]);
            if (is_array($postTypes)) {
                unset($postTypes['attachment']);
            }
            if (is_array($postTypes) && $postTypes !== [] && class_exists('WP_Query')) {
                $paged = 1;
                do {
                    $q = new \WP_Query([
                        'post_type'              => array_values($postTypes),
                        'post_status'            => 'publish',
                        'has_password'           => false,
                        'fields'                 => 'ids',
                        'posts_per_page'         => 2000,
                        'paged'                  => $paged,
                        'no_found_rows'          => true,
                        'update_post_meta_cache' => false,
                        'update_post_term_cache' => false,
                        'ignore_sticky_posts'    => true,
                    ]);
                    $ids = is_array($q->posts) ? $q->posts : [];
                    foreach ($ids as $id) {
                        $link = get_permalink((int) $id);
                        if (is_string($link) && $link !== '') {
                            $urls[] = $link;
                        }
                    }
                    $paged++;
                } while (count($ids) === 2000 && $paged <= 50 && count($urls) < self::PRELOAD_MAX_URLS);
            }

            // 2. Every non-empty term archive of every public, rewritable taxonomy.
            if (count($urls) < self::PRELOAD_MAX_URLS
                && function_exists('get_taxonomies') && function_exists('get_terms') && function_exists('get_term_link')
            ) {
                $taxes = get_taxonomies(['public' => true, 'rewrite' => true]);
                if (is_array($taxes) && $taxes !== []) {
                    $terms = get_terms([
                        'taxonomy'   => array_values($taxes),
                        'hide_empty' => true,
                        'number'     => 5000,
                    ]);
                    if (is_array($terms)) {
                        foreach ($terms as $term) {
                            if (!is_object($term)) {
                                continue;
                            }
                            $link = get_term_link($term);
                            if (is_string($link) && $link !== '') {
                                $urls[] = $link;
                            }
                        }
                    }
                }
            }

            // 3. Author archives (only users with published posts → no 404s).
            if (count($urls) < self::PRELOAD_MAX_URLS
                && function_exists('get_users') && function_exists('get_author_posts_url')
            ) {
                $authorIds = get_users([
                    'has_published_posts' => true,
                    'fields'              => 'ID',
                    'number'              => 500,
                ]);
                if (is_array($authorIds)) {
                    foreach ($authorIds as $aid) {
                        $link = get_author_posts_url((int) $aid);
                        if (is_string($link) && $link !== '') {
                            $urls[] = $link;
                        }
                    }
                }
            }
        } catch (\Throwable $e) {
            // Best-effort: whatever we collected is still warmed.
            \WPMgr\Agent\Support\DebugLog::write('wpmgr-agent: preload enumeration degraded (' . $e->getMessage() . ')');
        } finally {
            if ($suspended && function_exists('wp_suspend_cache_addition')) {
                wp_suspend_cache_addition(false);
            }
        }

        $urls = array_values(array_unique($urls));
        if (count($urls) > self::PRELOAD_MAX_URLS) {
            $urls = array_slice($urls, 0, self::PRELOAD_MAX_URLS);
        }
        return $urls;
    }

    /**
     * Silently reinstall the advanced-cache.php drop-in when its on-disk version
     * is older than the template version. Called on every boot (plugins_loaded)
     * so agent updates propagate to the drop-in on idle sites that never trigger
     * a manual enable/disable or applyConfig cycle.
     *
     * Idempotent: a no-op when the installed version already matches the template,
     * when the drop-in is absent, or when the drop-in belongs to another plugin.
     *
     * @return void
     */
    public function maybeRefreshDropin(): void
    {
        if (!$this->config()->enabled) {
            return; // page caching is off — nothing to refresh
        }
        if (!$this->dropin->needsRefresh()) {
            return;
        }
        $this->dropin->install($this->config()->toDropinArray());
    }

    /**
     * Apply a new config: persist it, re-render the drop-in, re-evaluate the
     * .htaccess mobile flag, and re-arm the refresh cron. Used by perf.config.update.
     *
     * @param array<string,mixed> $data Raw config map from the CP.
     * @return array{ok:bool,detail:string}
     */
    public function applyConfig(array $data): array
    {
        // Preserve the current enabled state unless the payload explicitly sets it.
        $current = $this->config();
        if (!array_key_exists('enabled', $data)) {
            $data['enabled'] = $current->enabled;
        }

        $config = new CacheConfig($data);
        $this->saveConfig($config);

        // Only touch server artefacts when caching is enabled.
        if ($config->enabled) {
            $this->dropin->install($config->toDropinArray());
            $this->htaccessApply($config);
        }

        // Re-arm the refresh schedule to the (possibly new) interval.
        $refresh = new CacheRefreshCron($this->purgeEngine(), $this->preloadEngine(), $config->refreshInterval);
        $refresh->schedule();

        return ['ok' => true, 'detail' => 'config applied'];
    }

    /**
     * Page count + total bytes for the heartbeat gauge. Walks the cache dir and
     * persists the result as a cheap snapshot (see {@see snapshot()}).
     *
     * @return array{enabled:bool,pages:int,bytes:int}
     */
    public function stats(): array
    {
        $s = (new CacheStats($this->cacheRoot()))->collect();
        $stats = [
            'enabled' => $this->config()->enabled,
            'pages'   => $s['pages'],
            'bytes'   => $s['bytes'],
        ];

        if (function_exists('update_option')) {
            update_option(self::OPTION_STATS, $stats, false);
        }

        return $stats;
    }

    /**
     * The cheap stats snapshot for the heartbeat. Reads the persisted option
     * (refreshed on every cache state change) WITHOUT walking the cache dir, so
     * it is safe to call every 60s.
     *
     * @return array{enabled:bool,pages:int,bytes:int}
     */
    public static function snapshot(): array
    {
        $default = ['enabled' => false, 'pages' => 0, 'bytes' => 0];
        if (!function_exists('get_option')) {
            return $default;
        }
        $stored = get_option(self::OPTION_STATS, $default);
        if (!is_array($stored)) {
            return $default;
        }
        return [
            'enabled' => (bool) ($stored['enabled'] ?? false),
            'pages'   => (int) ($stored['pages'] ?? 0),
            'bytes'   => (int) ($stored['bytes'] ?? 0),
        ];
    }

    /**
     * The current (request-cached) config.
     *
     * @return CacheConfig
     */
    public function config(): CacheConfig
    {
        if ($this->config === null) {
            $stored = function_exists('get_option') ? get_option(self::OPTION_CONFIG, []) : [];
            $this->config = new CacheConfig(is_array($stored) ? $stored : []);
        }
        return $this->config;
    }

    // -------------------------------------------------------------------------
    // Private helpers
    // -------------------------------------------------------------------------

    /**
     * Persist a config and refresh the request-cached copy.
     *
     * @param CacheConfig $config Config to store.
     * @return void
     */
    private function saveConfig(CacheConfig $config): void
    {
        if (function_exists('update_option')) {
            update_option(self::OPTION_CONFIG, $config->toArray(), false);
        }
        $this->config = $config;
    }

    /**
     * Install/refresh the .htaccess block for the current host + mobile flag.
     *
     * @param CacheConfig $config Active config.
     * @return bool
     */
    private function htaccessApply(CacheConfig $config): bool
    {
        if ($this->htaccess->isNginx()) {
            return false;
        }
        return $this->htaccess->install($this->hostname(), $config->cacheMobile);
    }

    /**
     * The disk cache root (…/wp-content/cache/wpmgr).
     *
     * @return string
     */
    public function cacheRoot(): string
    {
        $content = defined('WP_CONTENT_DIR') ? rtrim((string) constant('WP_CONTENT_DIR'), '/\\') : '';
        if ($content === '') {
            return '';
        }
        return $content . '/cache/wpmgr';
    }

    /**
     * Build a Purge engine for the current cache root.
     *
     * @return Purge
     */
    private function purgeEngine(): Purge
    {
        return new Purge($this->cacheRoot());
    }

    /**
     * Build a Preload engine honouring the mobile-warm config. The warmer builds
     * its own PreloadQueue from the persisted PerfConfig throttle values
     * (preload_concurrency / preload_delay_ms / preload_batch_size /
     * preload_max_load); the dashboard is the primary surface and the legacy
     * wpmgr_preload_* filters remain the final escape-hatch override.
     *
     * @return Preload
     */
    private function preloadEngine(): Preload
    {
        return new Preload($this->config()->cacheMobile);
    }

    /**
     * Build a PreloadQueue from the persisted PerfConfig throttle values. Used by
     * the signed preload-queue status/maintenance commands (Track A item 9) and
     * the watchdog binding in Plugin.
     *
     * @return PreloadQueue
     */
    public function preloadQueue(): PreloadQueue
    {
        return PreloadQueue::fromConfig();
    }

    /**
     * The current request host (or the configured site host as a fallback).
     *
     * @return string
     */
    private function hostname(): string
    {
        if (isset($_SERVER['HTTP_HOST']) && is_string($_SERVER['HTTP_HOST']) && $_SERVER['HTTP_HOST'] !== '') {
            return sanitize_text_field(wp_unslash($_SERVER['HTTP_HOST'])); // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized,WordPress.Security.ValidatedSanitizedInput.MissingUnslash -- sanitize_text_field+wp_unslash applied; cache-key path uses sanitize_text_field (not esc_url_raw) to preserve drop-in key parity
        }
        if (function_exists('home_url')) {
            $host = (string) (wp_parse_url((string) home_url('/'), PHP_URL_HOST) ?? '');
            if ($host !== '') {
                return $host;
            }
        }
        return '';
    }

    /**
     * Build a PerfReporter for the current site. Returns null when the reporter
     * cannot be constructed (e.g. before enrollment). The reporter is cheap to
     * build (no DB reads); actual I/O only happens when report*() is called.
     *
     * @return PerfReporter|null
     */
    public function makePerfReporter(): ?PerfReporter
    {
        try {
            // PerfReporter needs Settings + Signer. These mirror what Plugin
            // holds, but CacheManager is constructed without them. We build
            // lightweight instances here; they share the same wp-options storage.
            $settings = new \WPMgr\Agent\Settings();
            if (!$settings->isEnrolled()) {
                return null;
            }
            $keystore = new \WPMgr\Agent\Keystore();
            $signer   = new \WPMgr\Agent\Signer($keystore);
            return new PerfReporter($settings, $signer, $this);
        } catch (\Throwable $e) {
            return null;
        }
    }

    /**
     * Record (or clear) the last error for surfacing to the admin/CP. Never logs
     * secrets — only short operational strings.
     *
     * @param string $error Error text or '' to clear.
     * @return void
     */
    private function recordError(string $error): void
    {
        if (!function_exists('update_option') || !function_exists('delete_option')) {
            return;
        }
        if ($error === '') {
            delete_option(self::OPTION_LAST_ERROR);
        } else {
            update_option(self::OPTION_LAST_ERROR, $error, false);
        }
    }
}
