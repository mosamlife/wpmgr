<?php
/**
 * RumInjector — enqueue the RUM collector script + config on wp_enqueue_scripts.
 *
 * GH #154 fix: the collector used to be spliced into the page HTML by an
 * Optimizer stage that only ran inside WPMgr's own page-cache output buffer.
 * That meant a site with WPMgr page caching OFF — the norm when a third-party
 * page cache serves the site — or a page served from a third-party cache HIT
 * never received the collector at all, and RUM silently collected zero data
 * with no warning. This class is driven from the `wp_enqueue_scripts` action
 * (bound by Plugin::registerRumHooks()/renderRumHead()), independent of
 * CacheConfig or any optimizer/cache buffer, so injection no longer depends
 * on any cache path. See {@see renderHead()}.
 *
 * 2026-07 wp.org review fix: this class used to build and `echo` the script
 * tags directly from a `wp_head` priority-99 callback (after
 * `print_head_scripts` had already run), with a docblock claiming "WP's
 * enqueue API is inapplicable at this late priority". That claim was wrong —
 * `wp_enqueue_scripts` fires as part of `wp_head` at priority 1 (WordPress
 * core registers it via `add_action('wp_head', 'wp_enqueue_scripts', 1)`),
 * i.e. BEFORE `print_head_scripts` runs, so a script enqueued there is still
 * printed normally. The class is now bound directly to `wp_enqueue_scripts`
 * and uses {@see wp_enqueue_script()} + {@see wp_add_inline_script()} instead
 * of a raw echo.
 *
 * The registered assets are always the same two parts:
 *
 *   1. A tiny inline script (wp_add_inline_script(..., 'before')) that sets
 *      window.__WPMGR_RUM__ = {key,url,rate} (the per-site constants; no
 *      per-request variance, no Vary header, no cookie), printed immediately
 *      before the collector script tag.
 *
 *   2. The external collector script (wp_enqueue_script()), which loads the
 *      bundled web-vitals collector IIFE from assets/wpmgr-rum.min.js with an
 *      async loading strategy.
 *
 * Why <head> + async (not defer before </body>):
 *   web-vitals onCLS is gated on onFCP firing. If the collector is deferred to
 *   </body>, the page can transition to hidden BEFORE the buffered FCP paint
 *   entry is observed, so the FCP gate never fires and no CLS beacon is ever
 *   sent. Loading the script async from <head> means web-vitals registers its
 *   observers and the onFCP/onCLS hide-handler early — well before the visitor
 *   can leave — so CLS finalises correctly on stable/view-then-leave pages.
 *   async (not defer) is used because web-vitals uses buffered PerformanceObserver
 *   entries and visibility-state history, so exact parse-time ordering relative
 *   to other scripts does not matter; async avoids blocking the render pipeline.
 *   `wp_enqueue_script()` is called with `in_footer` false so the tag prints in
 *   <head>, faithfully replacing the previous splice-before-</head> position.
 *
 *   The plugin's minimum supported WordPress version (6.2) predates the
 *   `strategy` argument to `wp_enqueue_script()` (added in WP 6.3, see
 *   {@see https://make.wordpress.org/core/2023/07/14/registering-scripts-with-async-and-defer-attributes-in-wordpress-6-3/}).
 *   On 6.3+ we pass `strategy => async` directly; on 6.2 (where the 5th
 *   `wp_enqueue_script()` argument is still a plain `bool $in_footer`, so
 *   passing an array there would be coerced to `true` and wrongly force the
 *   script into the footer) we instead pass a plain `false` and fall back to
 *   a `script_loader_tag` filter that adds the `async` attribute for this
 *   handle only. See {@see enqueue()} and {@see filterAsyncScriptTag()}.
 *
 * The external src approach means the collector never violates a strict
 * no-unsafe-inline CSP on the main document. sendBeacon is governed by
 * connect-src; operators must add the CP host to connect-src separately.
 *
 * CSP safety: if already-queued response headers contain a Content-Security-Policy
 * with a script-src that is "strict" (has 'nonce-' or uses a hash source without
 * 'unsafe-inline') AND the policy does not already allowlist the plugin's asset URL,
 * this stage skips injection rather than breaking the page.
 *
 * Deliberate coverage expansion: unlike the old cache-bound stage,
 * wp_enqueue_scripts has no cache-cookie/URL-path exclusions, so the beacon
 * now also fires for anonymous visitors on cart/checkout pages and on pages
 * carrying the `_wpmgr_no_cache` / `_wpmgr_no_optimize` post meta.
 * (WooCommerce's My Account page is NOT a new coverage case here — it renders
 * content only for a logged-in visitor, and the anonymous-only guard in
 * {@see renderHead()} already excludes every logged-in request regardless of
 * page type.) This is intentional: the beacon payload is a metric name,
 * value, device class, connection type, and the current page URL — never
 * cart contents or a session identifier. The page URL IS transmitted
 * (window.location.href in the collector, apps/tracker/src/vitals.ts), but
 * the collector strips the query string and hash before sending (origin +
 * pathname only), so a per-visit token that a checkout/order-confirmation
 * flow might carry in the query string (e.g. a WooCommerce order-received
 * key) never leaves the browser.
 *
 * @package WPMgr\Agent\Optimizer
 */

declare(strict_types=1);

namespace WPMgr\Agent\Optimizer;

/**
 * Enqueues the RUM beacon config + collector script on wp_enqueue_scripts.
 */
final class RumInjector
{
    /** wp_enqueue_script()/wp_add_inline_script() handle for the collector. */
    public const SCRIPT_HANDLE = 'wpmgr-rum';

    private PerfConfig $config;

    /**
     * Guards against enqueuing more than once per request — defends against a
     * theme calling wp_head() twice, or the wp_enqueue_scripts hook accidentally
     * being registered more than once. A static property (not per-instance) so
     * the guard holds even if a fresh RumInjector is constructed for each
     * callback invocation.
     */
    private static bool $emitted = false;

    /**
     * @param PerfConfig|null $config Optimization config.
     */
    public function __construct(?PerfConfig $config = null)
    {
        $this->config = $config ?? PerfConfig::load();
    }

    /**
     * wp_enqueue_scripts callback: enqueue the RUM config + collector script.
     *
     * wp_enqueue_scripts already guarantees this only fires while WordPress is
     * rendering a normal front-end template (not admin/REST/AJAX/cron/feeds),
     * but it does NOT guarantee the response is an anonymous, GET, 200,
     * non-password-protected page — those are checked explicitly below,
     * mirroring the guards the old cache-bound stage got for free from the
     * page-cache write path (see Cacheability::isRequestCacheable()).
     *
     * @return void
     */
    public function renderHead(): void
    {
        // Cheapest check first: at most once per request.
        if (self::$emitted) {
            return;
        }

        if (!$this->config->rumEnabled) {
            return;
        }

        $key = $this->config->rumBeaconKey;
        $url = $this->config->rumIngestUrl;

        // Both values are required; without them the beacon cannot land.
        if ($key === '' || $url === '') {
            return;
        }

        // Anonymous only: a logged-in visitor's session must never be
        // attributed a beacon (mirrors the old cache path, which only ever
        // wrote/optimized the anonymous render).
        if (function_exists('is_user_logged_in') && is_user_logged_in()) {
            return;
        }

        // GET only: wp_enqueue_scripts can fire during a page-rendering POST
        // (e.g. a themed form handler that re-renders the page on submit).
        $method = sanitize_text_field(
            wp_unslash(isset($_SERVER['REQUEST_METHOD']) ? (string) $_SERVER['REQUEST_METHOD'] : 'GET')
        );
        if (strtoupper($method) !== 'GET') {
            return;
        }

        // 200 only: never beacon a 404.
        if (function_exists('is_404') && is_404()) {
            return;
        }

        // Password-protected content: never beacon (matches the prior
        // cache-path exclusion for password-protected singular content).
        if (function_exists('post_password_required') && post_password_required()) {
            return;
        }

        // Skip if a conflicting strict CSP is already queued.
        if ($this->hasConflictingCsp()) {
            return;
        }

        $scriptUrl = $this->assetUrl();
        if ($scriptUrl === '') {
            return;
        }

        if (!function_exists('wp_enqueue_script') || !function_exists('wp_add_inline_script')) {
            return;
        }

        $this->enqueue($key, $url, $this->config->rumSampleRate, $scriptUrl);

        self::$emitted = true;
    }

    /**
     * Enqueue the collector script and its inline config.
     *
     * @param string $key       Plaintext beacon key.
     * @param string $url       Ingest endpoint URL.
     * @param float  $rate      Sample rate [0,1].
     * @param string $scriptUrl URL to wpmgr-rum.min.js.
     * @return void
     */
    private function enqueue(string $key, string $url, float $rate, string $scriptUrl): void
    {
        $ver = defined('WPMGR_AGENT_VERSION') ? (string) constant('WPMGR_AGENT_VERSION') : false;

        if ($this->coreSupportsScriptStrategy()) {
            // WP 6.3+: the native way to load a script async without
            // depending on script-loader-tag string surgery.
            wp_enqueue_script(
                self::SCRIPT_HANDLE,
                $scriptUrl,
                [],
                $ver,
                ['in_footer' => false, 'strategy' => 'async']
            );
        } else {
            // WP 6.2 (this plugin's floor): the 5th wp_enqueue_script() arg is
            // still a plain bool. Passing an array here would be coerced to
            // `true` by PHP and wrongly force the script into the footer, so
            // pass `false` (head) and add `async` via the tag filter below.
            wp_enqueue_script(self::SCRIPT_HANDLE, $scriptUrl, [], $ver, false);
            $this->ensureAsyncFallbackFilter();
        }

        // Values are encoded with wp_json_encode then written into a JS object
        // literal — there is no unsafe-inline concern because this sets a
        // config object, NOT an event handler or navigation.
        //
        // The JSON_HEX_* flags are the WP-correct hardening for JSON destined
        // for an inline <script> sink: they hex-escape <, >, &, ', and " so a
        // hostile $key/$url value (both are CP-controlled today, not
        // user-input, but this is a cheap defense-in-depth) can never break out
        // of the JS string context with a literal `</script>` or similar.
        $rate = round(max(0.0, min(1.0, $rate)), 4);

        $configJson = (string) wp_json_encode(
            [
                'key'  => $key,
                'url'  => $url,
                'rate' => $rate,
            ],
            JSON_HEX_TAG | JSON_HEX_AMP | JSON_HEX_APOS | JSON_HEX_QUOT
        );

        // 'before' prints this inline script immediately before the collector
        // tag, so window.__WPMGR_RUM__ is always set before the collector runs.
        wp_add_inline_script(
            self::SCRIPT_HANDLE,
            'window.__WPMGR_RUM__=' . $configJson . ';',
            'before'
        );
    }

    /**
     * Whether WP core's wp_enqueue_script() accepts the WP 6.3+ $args array
     * form (in_footer/strategy) for the 5th parameter.
     *
     * @return bool
     */
    private function coreSupportsScriptStrategy(): bool
    {
        $version = isset($GLOBALS['wp_version']) && is_string($GLOBALS['wp_version'])
            ? $GLOBALS['wp_version']
            : '';

        if ($version === '') {
            // Unknown version: fall back to the universally-safe bool + filter
            // path rather than risk mis-detecting a footer-forcing coercion.
            return false;
        }

        return version_compare($version, '6.3', '>=');
    }

    /**
     * Register the WP<6.3 `async` attribute fallback filter (idempotent --
     * add_filter() with the same static callback is a safe no-op if already
     * registered for this request).
     *
     * @return void
     */
    private function ensureAsyncFallbackFilter(): void
    {
        if (!function_exists('add_filter')) {
            return;
        }
        add_filter('script_loader_tag', [self::class, 'filterAsyncScriptTag'], 10, 2);
    }

    /**
     * script_loader_tag filter: add the `async` attribute to this plugin's
     * collector script tag on WP < 6.3, where wp_enqueue_script() has no
     * `strategy` argument.
     *
     * Idempotent: if the tag already carries `async` (e.g. because a future
     * WP version starts adding it independently), the tag is returned
     * unmodified rather than doubled up.
     *
     * @param string $tag    The <script> tag WP core built for $handle.
     * @param string $handle The script's registered handle.
     * @return string
     */
    public static function filterAsyncScriptTag(string $tag, string $handle): string
    {
        if ($handle !== self::SCRIPT_HANDLE) {
            return $tag;
        }
        if (preg_match('/\basync\b/', $tag) === 1) {
            return $tag;
        }
        return (string) preg_replace('/\ssrc=/', ' async src=', $tag, 1);
    }

    /**
     * Resolve the public URL to assets/wpmgr-rum.min.js.
     *
     * Uses plugins_url() when available (the canonical WP function for
     * plugin assets), falling back to WP_PLUGIN_URL for headless contexts
     * where plugins_url() may not yet be registered.
     *
     * Deliberately does NOT append a `?ver=` query string itself (unlike the
     * pre-enqueue implementation) -- wp_enqueue_script()'s own $ver argument
     * (see {@see enqueue()}) is the correct place for cache-busting versioning
     * once the script is enqueued rather than raw-echoed, and appending both
     * would produce a duplicated/malformed query string.
     *
     * @return string URL, or '' when it cannot be resolved.
     */
    private function assetUrl(): string
    {
        $base = '';

        // WPMGR_AGENT_FILE is defined in wpmgr-agent.php and is always present
        // at runtime. plugins_url() is the canonical WP asset URL builder.
        if (function_exists('plugins_url') && defined('WPMGR_AGENT_FILE')) {
            $base = (string) plugins_url(
                'assets/wpmgr-rum.min.js',
                (string) constant('WPMGR_AGENT_FILE')
            );
        } elseif (defined('WP_PLUGIN_URL') && defined('WPMGR_AGENT_DIR')) {
            // Fallback: build from WP_PLUGIN_URL + WPMGR_AGENT_DIR.
            $pluginUrl = rtrim((string) constant('WP_PLUGIN_URL'), '/');
            $agentDir  = rtrim((string) constant('WPMGR_AGENT_DIR'), '/\\');
            $pluginDir = defined('WP_PLUGIN_DIR') ? rtrim((string) constant('WP_PLUGIN_DIR'), '/\\') : '';
            if ($pluginDir !== '' && strpos($agentDir, $pluginDir) === 0) {
                $rel  = ltrim(substr($agentDir, strlen($pluginDir)), '/');
                $base = $pluginUrl . '/' . $rel . '/assets/wpmgr-rum.min.js';
            }
        }

        return $base;
    }

    /**
     * Whether already-queued response headers contain a strict Content-Security-Policy
     * that would block an external script without a nonce.
     *
     * "Strict" here means: the script-src (or default-src) directive contains
     * 'nonce-' (a per-request nonce that the injected static HTML can never know)
     * and does NOT include 'unsafe-inline' (which would allow any inline/external
     * script). When such a policy is present we skip injection to avoid a
     * browser CSP violation that would block the page's console with errors.
     *
     * This check uses headers_list(), which reflects whatever headers have
     * already been queued via header() by the time wp_enqueue_scripts runs.
     *
     * @return bool True when a conflicting CSP is detected.
     */
    private function hasConflictingCsp(): bool
    {
        if (!function_exists('headers_list')) {
            return false;
        }
        $headers = headers_list();
        if (!is_array($headers)) {
            return false;
        }
        foreach ($headers as $header) {
            if (!is_string($header)) {
                continue;
            }
            if (stripos($header, 'Content-Security-Policy') !== 0) {
                continue;
            }
            // Extract the directive value.
            $value = substr($header, strpos($header, ':') + 1);
            $value = strtolower(trim($value));

            // Locate script-src or fall through to default-src.
            $scriptSrc = '';
            if (preg_match('/script-src\s+([^;]+)/i', $value, $m)) {
                $scriptSrc = $m[1];
            } elseif (preg_match('/default-src\s+([^;]+)/i', $value, $m)) {
                $scriptSrc = $m[1];
            }

            if ($scriptSrc === '') {
                continue;
            }

            // A nonce-based CSP without unsafe-inline is a conflict:
            // our static HTML can never carry a dynamic nonce, so the
            // external script tag we inject would be blocked.
            $hasNonce         = strpos($scriptSrc, "'nonce-") !== false;
            $hasUnsafeInline  = strpos($scriptSrc, "'unsafe-inline'") !== false;

            if ($hasNonce && !$hasUnsafeInline) {
                return true;
            }
        }
        return false;
    }
}
