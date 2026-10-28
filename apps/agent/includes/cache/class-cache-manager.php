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
 * the manager reads it cheaply once and, if caching is enabled, binds startBuffer()
 * to init at the lowest priority, so the capture buffer opens ahead of every
 * other plugin's init callback and is the outer buffer: it receives the page as
 * the visitor does, after any buffer a plugin opens later has rewritten it. The
 * writer it hands the buffer to stores a page only if it was rendered through
 * template_redirect, and stores it under the key the serving drop-in handed over
 * for the request, or under the key the drop-in's rule gives when the drop-in did
 * not run (WP_CACHE off, another plugin's advanced-cache.php, an alternate drop-in
 * file name).
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

    /**
     * Records which key scheme the stored pages were last cleared for (see
     * {@see KEY_SCHEME}). Autoloaded, so the check costs nothing once it holds
     * the current scheme.
     */
    public const OPTION_PURGED_FOR_KEYS = 'wpmgr_cache_purged_for_keys';

    /**
     * The key scheme stored pages are written under. Since scheme 2 each page
     * is stored under the key the drop-in serves it from. The first boot on a
     * new scheme removes every stored page, once, so only pages written under
     * the current scheme are served. Raise it whenever a change means pages
     * already on disk may not be under the key they would be stored under now.
     */
    public const KEY_SCHEME = '2';

    private WpCacheConstant $wpCache;

    private DropinInstaller $dropin;

    private HtaccessManager $htaccess;

    private NginxHelper $nginx;

    /** Cached config for this request. */
    private ?CacheConfig $config = null;

    /** Whether the capture buffer has been opened for this request. */
    private bool $bufferOpen = false;

    /**
     * The request URI as it stood when the hooks were registered: at plugin
     * load, ahead of every plugins_loaded callback and ahead of the slashes
     * WordPress adds to the superglobals. Null until then. It decides only
     * whether a key is worked out for a request the drop-in handed none over
     * (see {@see storeKey()}).
     */
    private ?string $uriAtLoad = null;

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
        // The request path as it arrived, read before anything else here and
        // before another plugin's plugins_loaded callback can change it. The
        // first read stands.
        // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.MissingUnslash,WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- read before WordPress adds slashes to the superglobals (it does so after plugins_loaded), so there are none to remove; only compared with the unslashed value when the buffer opens, never output, stored or put in a query
        $this->uriAtLoad ??= isset( $_SERVER['REQUEST_URI'] ) ? (string) $_SERVER['REQUEST_URI'] : '/';

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

        // Output-buffer writer: open it before any other plugin's init callback,
        // for front-end GET requests — INCLUDING preload / RUCSS-compute
        // self-fetches (which carry the x-wpmgr-preload header). The drop-in
        // already serves those requests fresh (no HIT), and this buffer is
        // exactly what (a) writes the freshly rendered page to the disk cache —
        // so preload actually WARMS the cache — and (b) runs the optimization +
        // RUCSS pipeline. Skipping it on preload made preload a no-op for
        // warming AND made "Compute Used-CSS now" never trigger the optimizer
        // (the buffer-bound optimizer never ran). The warmer still receives the
        // rendered HTML — the buffer returns it verbatim.
        //
        // Why before init, and not at template_redirect: output buffers unwind
        // innermost first, so a buffer opened EARLIER wraps one opened later
        // and receives that one's finished output. A plugin that rewrites the
        // page in its own buffer (a translator, a minifier) opens it at init or
        // later; a capture buffer opened after it would store the page as it
        // was BEFORE the rewrite, and every cache hit would then serve that
        // unrewritten page, for instance the default language on a translated
        // URL. Opening at the lowest init priority puts the capture on the
        // outside of every buffer a plugin opens from init onward.
        if (function_exists('add_action')) {
            add_action('init', [$this, 'startBuffer'], PHP_INT_MIN);
        }
    }

    /**
     * Open the page-cache output buffer (bound to init at the lowest priority).
     *
     * The writer is handed the key the page is stored under, fixed now, before
     * any other plugin's init callback runs (see {@see storeKey()}): the key the
     * drop-in chose, or the key its rule gives when the drop-in did not run, or
     * none, in which case nothing is stored (the rule declines the request, or
     * its path changed after this plugin loaded). The request URI captured here
     * only feeds the URL rules (Bypass URLs and the never-cached paths).
     *
     * @return void
     */
    public function startBuffer(): void
    {
        if ($this->bufferOpen || !$this->isBufferCandidate()) {
            return;
        }
        $this->bufferOpen = true;

        $requestUri = sanitize_text_field( wp_unslash( isset( $_SERVER['REQUEST_URI'] ) ? (string) $_SERVER['REQUEST_URI'] : '/' ) );
        $writer     = new CacheWriter($this->config(), $this->cacheRoot(), $this->storeKey(), null, null, $requestUri);
        // Full-page output buffer with a callback handler: this is the standard
        // page-cache capture pattern. PHP finalizes the buffer at request end,
        // invoking CacheWriter::handle() with the complete response. The buffer
        // is intentionally held open for the entire request lifecycle so the
        // handler receives the full page output — an in-scope ob_get_clean() is
        // not applicable here because the response is not yet complete. The
        // chunk size bounds what is held: a response that reaches it (a
        // download, a very large page) is passed through as it is produced and
        // is not stored, and output is never held back across chunks.
        ob_start([$writer, 'handle'], CacheWriter::CHUNK_BYTES);
    }

    /**
     * The key this request's page is stored under, relative to the cache root.
     *
     * A key the drop-in handed over always wins, even one the writer will
     * refuse: it is the file the drop-in serves the request from. When it handed
     * none over, the drop-in did not run on this request (WP_CACHE is off,
     * another plugin owns advanced-cache.php, ours sits under another file
     * name), or it declined the request. The key is then the one the drop-in's
     * rule gives for the request as it stands now (see CacheKey::forRequest()),
     * which is the file the web-server rules look a stored page up by; the rule
     * declines the same requests the drop-in does, and then nothing is stored.
     * Nothing is stored either when the request path is no longer the one this
     * plugin loaded with: the drop-in read the path as it arrived, ahead of
     * every plugin, so a key worked out from a changed path is not the file the
     * request is served from.
     *
     * @return string|null
     */
    private function storeKey(): ?string
    {
        if (defined(CacheWriter::KEY_CONSTANT)) {
            $handed = constant(CacheWriter::KEY_CONSTANT);
            return is_string($handed) ? $handed : '';
        }
        $arrived = self::arrivedRequest();
        if ($this->uriAtLoad !== null && $this->uriAtLoad !== $arrived['uri']) {
            return null;
        }
        return CacheKey::forRequest($arrived, $this->config());
    }

    /**
     * The request as the drop-in read it, for working out the key it would have
     * handed over. WordPress adds slashes to the superglobals once the plugins
     * have loaded and the drop-in runs ahead of all of that, so the slashes come
     * off again and nothing else is done to a value: the key is a function of
     * exactly what the drop-in read. Nothing here is output, stored or put in a
     * query; the key function validates every part of it.
     *
     * @return array{uri:string,host:string,user_agent:string,cookies:array<mixed>,query:array<mixed>}
     */
    private static function arrivedRequest(): array
    {
        // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- the unsanitised value is the key input; CacheKey::forRequest() validates it whole and uses only its normalised form
        $uri = wp_unslash( isset( $_SERVER['REQUEST_URI'] ) ? (string) $_SERVER['REQUEST_URI'] : '/' );
        // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- validated whole by CacheKey::normalizeHost() against an anchored allow-list before use
        $host = wp_unslash( isset( $_SERVER['HTTP_HOST'] ) ? (string) $_SERVER['HTTP_HOST'] : '' );
        // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- only matched against the mobile pattern after control characters are stripped and the length is capped; never stored or output
        $agent = wp_unslash( isset( $_SERVER['HTTP_USER_AGENT'] ) ? (string) $_SERVER['HTTP_USER_AGENT'] : '' );
        $cookies = wp_unslash( $_COOKIE ); // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized,WordPress.Security.NonceVerification.Recommended -- cache-key input only; each value is slugified to [a-z0-9-_] by CacheKey::sanitizeSegment, as in the drop-in; no output, no SQL, no state change
        $query   = wp_unslash( $_GET ); // phpcs:ignore WordPress.Security.NonceVerification.Recommended -- cache-key input only; key names and values are hashed for routing, as in the drop-in; no state change, no form processing

        return [
            'uri'        => is_string($uri) ? $uri : '/',
            'host'       => is_string($host) ? $host : '',
            'user_agent' => is_string($agent) ? $agent : '',
            'cookies'    => is_array($cookies) ? $cookies : [],
            'query'      => is_array($query) ? $query : [],
        ];
    }

    /**
     * Whether this request could ever have a page stored from it. The buffer
     * now opens at init rather than at template_redirect, so it must not be
     * opened for requests that are never stored (they would only have their
     * streaming output held back): anything but GET, the admin area and
     * admin-ajax, cron, WP-CLI, XML-RPC, the REST API and the login screen, and
     * every script other than the front controller. Everything refused here is
     * also refused by the writer at flush time, so no storable page is lost to
     * this gate.
     *
     * @return bool
     */
    private function isBufferCandidate(): bool
    {
        if (defined('WP_CLI') && WP_CLI) {
            return false;
        }
        // Only the front controller renders pages that are stored. The other
        // scripts that load WordPress (the multisite signup and activation
        // pages, the editor and feed helpers) never reached template_redirect,
        // where this buffer used to open, so they were never stored: opening it
        // earlier must not start storing them. An unset value (an unusual
        // bootstrap) leaves the decision to the checks below.
        $pagenow = isset($GLOBALS['pagenow']) && is_string($GLOBALS['pagenow']) ? $GLOBALS['pagenow'] : '';
        if ($pagenow !== '' && $pagenow !== 'index.php') {
            return false;
        }
        if (defined('XMLRPC_REQUEST') && XMLRPC_REQUEST) {
            return false;
        }
        $method = sanitize_text_field( wp_unslash( isset( $_SERVER['REQUEST_METHOD'] ) ? (string) $_SERVER['REQUEST_METHOD'] : 'GET' ) );
        if (strtoupper($method) !== 'GET') {
            return false;
        }
        if (function_exists('is_admin') && is_admin()) {
            return false;
        }
        if (function_exists('wp_doing_ajax') && wp_doing_ajax()) {
            return false;
        }
        if (function_exists('wp_doing_cron') && wp_doing_cron()) {
            return false;
        }
        $uri = sanitize_text_field( wp_unslash( isset( $_SERVER['REQUEST_URI'] ) ? (string) $_SERVER['REQUEST_URI'] : '/' ) );
        return preg_match('#/wp-json(?:/|$|\?)|[?&]rest_route=|/wp-login\.php#i', $uri) !== 1;
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
     * Then, once per key scheme, it removes the pages stored before it (see
     * {@see KEY_SCHEME}).
     *
     * @return void
     */
    public function maybeRefreshDropin(): void
    {
        $config = $this->config();
        if ($config->enabled && $this->dropin->needsRefresh()) {
            $this->dropin->install($config->toDropinArray());
        }
        $this->maybePurgeForKeyScheme($config);
    }

    /**
     * Remove every stored page once, the first time this key scheme boots, so
     * only pages stored under the current scheme are served.
     * Runs where something could serve a stored page: the page cache is on, or
     * our drop-in is still installed (a pushed setting that switches the cache
     * off leaves it there). Elsewhere there is nothing to clear, and a purge
     * would still clear host and edge caches through the integrations, so the
     * scheme is only recorded.
     *
     * The run is claimed before anything is purged ({@see claimKeySchemePurge()}),
     * and only the request that claimed it purges: requests that find the scheme
     * not recorded at the same moment purge once between them, and a record that
     * cannot be written leaves nobody to purge, instead of every request doing so.
     *
     * @param CacheConfig $config Current config.
     * @return void
     */
    private function maybePurgeForKeyScheme(CacheConfig $config): void
    {
        if (!function_exists('get_option')) {
            return;
        }
        if ((string) get_option(self::OPTION_PURGED_FOR_KEYS, '') === self::KEY_SCHEME) {
            return;
        }
        if (!self::claimKeySchemePurge()) {
            return;
        }
        if ($config->enabled || $this->dropin->isInstalled()) {
            $this->purgeEngine()->purgeEverything();
        }
    }

    /**
     * Record the current key scheme as handled, and report whether this call is
     * the one that recorded it. Exactly one of any number of concurrent calls
     * is: the row is created with an INSERT IGNORE on the unique option name,
     * or, when an older scheme is recorded, replaced by an UPDATE conditional on
     * the value read from the table, so a second statement finds nothing left
     * to change. False when another call recorded it first, and when the table
     * cannot be written.
     *
     * @return bool True only when this call recorded the scheme.
     */
    private static function claimKeySchemePurge(): bool
    {
        global $wpdb;
        if (!isset($wpdb)) {
            return false;
        }
        $name = self::OPTION_PURGED_FOR_KEYS;

        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- the claim must be one INSERT IGNORE on the unique option name, so that exactly one caller creates the row; the options cache is dropped below
        $rows = $wpdb->query($wpdb->prepare("INSERT IGNORE INTO {$wpdb->options} (option_name, option_value, autoload) VALUES (%s, %s, 'yes')", $name, self::KEY_SCHEME));
        if ($rows !== 1) {
            // A row exists. Read it as stored, past the options cache, and
            // replace it only from the value read: of the calls that read the
            // same older scheme, one UPDATE changes the row and the rest find
            // it changed.
            // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- the value as stored, never a cached or filtered option value
            $recorded = $wpdb->get_var($wpdb->prepare("SELECT option_value FROM {$wpdb->options} WHERE option_name = %s", $name));
            if (!is_string($recorded) || $recorded === self::KEY_SCHEME) {
                return false;
            }
            // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- the replacement must be one UPDATE conditional on the value read, so only one caller changes the row; update_option() is not conditional
            $rows = $wpdb->query($wpdb->prepare("UPDATE {$wpdb->options} SET option_value = %s WHERE option_name = %s AND option_value = %s", self::KEY_SCHEME, $name, $recorded));
            if ($rows !== 1) {
                return false;
            }
        }

        // The row was written past the options cache: drop the entries that
        // would still answer with the old record, so later reads find the row.
        if (function_exists('wp_cache_delete')) {
            foreach ([$name, 'alloptions', 'notoptions'] as $key) {
                wp_cache_delete($key, 'options');
            }
        }

        return true;
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

            // A Bypass URL added now covers pages that may already be cached. The
            // list is read by PHP only, and a web server serving a cached file
            // straight from disk (the Apache rewrite block, the nginx snippet)
            // never reads it, so the file would keep being served. Remove what
            // is cached; the writer now refuses to store the covered pages. A
            // push that adds no entry (the same list again, a removal, any other
            // setting) leaves the cache alone.
            if (self::addedEntries($current->bypassUrls, $config->bypassUrls) !== []) {
                $this->purgeEngine()->purgeEverything();
            }
        } elseif ($current->enabled) {
            // A push that switches the cache off leaves the drop-in and the
            // server rules in place, and both would keep serving what is
            // stored. Clear it; the writer stores nothing while the cache is
            // off.
            $this->purgeEngine()->purgeEverything();
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
     * Entries present in the new list and absent from the old one, compared
     * case-insensitively (Bypass URLs match case-insensitively).
     *
     * @param list<string> $before Previous list.
     * @param list<string> $after  New list.
     * @return list<string> Entries that were added.
     */
    private static function addedEntries(array $before, array $after): array
    {
        $known = [];
        foreach ($before as $entry) {
            $known[strtolower($entry)] = true;
        }
        $added = [];
        foreach ($after as $entry) {
            if (!isset($known[strtolower($entry)])) {
                $added[] = $entry;
            }
        }
        return $added;
    }

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
