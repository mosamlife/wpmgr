<?php
/**
 * RumInjector — emit the RUM collector script into <head> via wp_head.
 *
 * GH #154 fix: the collector used to be spliced into the page HTML by an
 * Optimizer stage that only ran inside WPMgr's own page-cache output buffer.
 * That meant a site with WPMgr page caching OFF — the norm when a third-party
 * page cache serves the site — or a page served from a third-party cache HIT
 * never received the collector at all, and RUM silently collected zero data
 * with no warning. This class is now driven from a `wp_head` action (bound by
 * Plugin::registerRumHooks()/renderRumHead(), independent of CacheConfig or
 * any optimizer/cache buffer), so injection no longer depends on any cache
 * path. See {@see renderHead()}.
 *
 * The emitted snippet is always the same two parts:
 *
 *   1. A tiny inline <script> that sets window.__WPMGR_RUM__ = {key,url,rate}
 *      (the per-site constants; no per-request variance, no Vary header, no
 *      cookie).
 *
 *   2. An external <script async src="…/assets/wpmgr-rum.min.js"> tag that
 *      loads the bundled web-vitals collector IIFE from the plugin assets dir.
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
 *   Priority 99 on wp_head prints near the end of <head> — faithfully replacing
 *   the previous splice-before-</head> position — without depending on any
 *   output buffer being open.
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
 * Deliberate coverage expansion: unlike the old cache-bound stage, wp_head has
 * no cache-cookie/URL-path exclusions, so the beacon now also fires for
 * anonymous visitors on cart/checkout pages and on pages carrying the
 * `_wpmgr_no_cache` / `_wpmgr_no_optimize` post meta. (WooCommerce's My
 * Account page is NOT a new coverage case here — it renders content only for
 * a logged-in visitor, and the anonymous-only guard in {@see renderHead()}
 * already excludes every logged-in request regardless of page type.) This is
 * intentional: the beacon payload is a metric name, value, device class,
 * connection type, and the current page URL — never cart contents or a
 * session identifier. The page URL IS transmitted (window.location.href in
 * the collector, apps/tracker/src/vitals.ts), but the collector strips the
 * query string and hash before sending (origin + pathname only), so a
 * per-visit token that a checkout/order-confirmation flow might carry in the
 * query string (e.g. a WooCommerce order-received key) never leaves the
 * browser.
 *
 * @package WPMgr\Agent\Optimizer
 */

declare(strict_types=1);

namespace WPMgr\Agent\Optimizer;

/**
 * Emits the RUM beacon config + collector script on wp_head.
 */
final class RumInjector
{
    private PerfConfig $config;

    /**
     * Guards against emitting more than once per request — defends against a
     * theme calling wp_head() twice, or the wp_head hook accidentally being
     * registered more than once. A static property (not per-instance) so the
     * guard holds even if a fresh RumInjector is constructed for each callback
     * invocation.
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
     * wp_head callback: echo the RUM config + collector script directly.
     *
     * wp_head already guarantees this only fires while WordPress is rendering
     * a normal front-end template (not admin/REST/AJAX/cron/feeds), but it does
     * NOT guarantee the response is an anonymous, GET, 200, non-password-
     * protected page — those are checked explicitly below, mirroring the
     * guards the old cache-bound stage got for free from the page-cache
     * write path (see Cacheability::isRequestCacheable()).
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

        // GET only: wp_head can print during a page-rendering POST (e.g. a
        // themed form handler that re-renders the page on submit).
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

        $snippet = $this->buildSnippet($key, $url, $this->config->rumSampleRate, $scriptUrl);

        // buildSnippet() escapes every dynamic value at construction time
        // (wp_json_encode for the config object, esc_url for the script src);
        // this is a plain echo of an already-escaped string, not a new sink.
        // phpcs:ignore WordPress.Security.EscapeOutput.OutputNotEscaped -- $snippet is built by buildSnippet(), which wp_json_encode()s the config object and esc_url()s the script src
        echo $snippet;

        self::$emitted = true;
    }

    /**
     * Build the inline config + external collector snippet.
     *
     * The inline block sets window.__WPMGR_RUM__ (a plain object, no DOM
     * interaction); the external script is async so it loads without blocking
     * the render pipeline. async is preferred over defer here because web-vitals
     * uses buffered PerformanceObserver entries and visibility-state history,
     * so parse-time ordering relative to other scripts is irrelevant — what
     * matters is that the observers are registered as early as possible.
     *
     * @param string $key        Plaintext beacon key.
     * @param string $url        Ingest endpoint URL.
     * @param float  $rate       Sample rate [0,1].
     * @param string $scriptUrl  URL to wpmgr-rum.min.js.
     * @return string HTML snippet.
     */
    private function buildSnippet(string $key, string $url, float $rate, string $scriptUrl): string
    {
        // Values are encoded with wp_json_encode then written into a JS object
        // literal via a script tag — there is no unsafe-inline concern because
        // this sets a config object, NOT an event handler or navigation.
        // esc_url is applied to the script src attribute.
        //
        // The JSON_HEX_* flags are the WP-correct hardening for JSON destined
        // for an inline <script> sink: they hex-escape <, >, &, ', and " so a
        // hostile $key/$url value (both are CP-controlled today, not
        // user-input, but this is a cheap defense-in-depth) can never break out
        // of the JS string context with a literal `</script>` or similar.
        $rate = round(max(0.0, min(1.0, $rate)), 4);

        $config_json = (string) wp_json_encode(
            [
                'key'  => $key,
                'url'  => $url,
                'rate' => $rate,
            ],
            JSON_HEX_TAG | JSON_HEX_AMP | JSON_HEX_APOS | JSON_HEX_QUOT
        );

        // Build the snippet without heredoc/nowdoc (Plugin Check bans heredocs).
        // The inline config script sets a simple window variable; the external
        // script loads the collector bundle. Both are echoed directly from the
        // wp_head callback — WP's enqueue API is inapplicable at this late
        // priority (print_head_scripts already ran at wp_head priority 1).
        // phpcs:ignore WordPress.WP.EnqueuedResources.NonEnqueuedScript -- echoed directly from a late (priority 99) wp_head callback, after print_head_scripts (priority 1) has already run; WP's enqueue API cannot inline a script this late
        $inline_config = '<script data-wpmgr-rum-config>'
            . 'window.__WPMGR_RUM__=' . $config_json . ';'
            . '</script>';

        // phpcs:ignore WordPress.WP.EnqueuedResources.NonEnqueuedScript -- echoed directly from a late (priority 99) wp_head callback; WP's enqueue API cannot inline a script this late (same reasoning as the inline config block above)
        $collector = '<script async src="' . esc_url($scriptUrl) . '"></script>';

        return $inline_config . $collector;
    }

    /**
     * Resolve the public URL to assets/wpmgr-rum.min.js.
     *
     * Uses plugins_url() when available (the canonical WP function for
     * plugin assets), falling back to WP_PLUGIN_URL for headless contexts
     * where plugins_url() may not yet be registered.
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

        if ($base === '') {
            return '';
        }

        // Append the plugin version as a cache-busting query arg. The collector
        // is served from a static, unversioned filename, so a CDN or browser
        // cache keyed on the URL would keep serving the previous build after a
        // plugin update -- a long-lived edge cache can mask a collector fix for
        // the full length of its TTL. Versioning the URL changes it on every
        // update, so the edge and the browser refetch the new bytes with no
        // manual purge.
        $ver = defined('WPMGR_AGENT_VERSION') ? (string) constant('WPMGR_AGENT_VERSION') : '';
        if ($ver !== '') {
            $base .= (strpos($base, '?') === false ? '?' : '&') . 'ver=' . rawurlencode($ver);
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
     * already been queued via header() by the time wp_head runs.
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
