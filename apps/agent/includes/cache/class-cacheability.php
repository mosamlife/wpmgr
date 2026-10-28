<?php
/**
 * Cacheability — decides whether a request/response may be served from, or
 * written to, the page cache.
 *
 * Two entry points:
 *
 *   isUrlCacheable($url)            — cheap, URL-only. Used by the preloader and
 *                                     auto-purge to decide which URLs to warm.
 *   isRequestCacheable(...)         — full request+response gate. Used at the
 *                                     output-buffer flush before writing a page.
 *
 * The checks follow the standard WordPress disk-cache exclusion set.
 *
 * @package WPMgr\Agent\Cache
 */

declare(strict_types=1);

namespace WPMgr\Agent\Cache;

/**
 * Request/URL cacheability predicates.
 */
final class Cacheability
{
    /**
     * Path fragments that are never cacheable (admin, auth, API, system).
     * Matched case-insensitively against the URL path.
     */
    private const UNCACHEABLE_PATH_PATTERN =
        '#/wp-(admin|login|register|comments-post|cron|json)|/xmlrpc\.php|\.(txt|xml|rss|atom)(/|$)|/sitemap#i';

    /**
     * Cookie name-prefixes that ALWAYS bypass the cache, baked in (NOT operator-
     * configured). These mark a request as personalised — a WooCommerce/EDD cart,
     * a logged-in session, a password-protected page, or a comment author — and
     * serving such a request from (or writing it to) a shared cache leaks one
     * visitor's cart/session to the next. They are merged with the operator
     * `bypass_cookies` everywhere a bypass decision is made (here AND in the
     * inlined drop-in config), so the two paths stay behaviorally identical.
     *
     * Matched as a case-insensitive SUBSTRING of the cookie name, so a prefix
     * such as `wp_woocommerce_session_` covers every per-store-hash variant.
     *
     * @var list<string>
     */
    public const DEFAULT_BYPASS_COOKIES = [
        'woocommerce_items_in_cart',
        'woocommerce_cart_hash',
        'wp_woocommerce_session_',
        'edd_items_in_cart',
        'wp-postpass_',
        'comment_author_',
        'wordpress_logged_in_',
    ];

    /**
     * The three WooCommerce cart/session cookie patterns that are eligible to be
     * moved from the hard-bypass set into a non-keying, non-bypassing ignore set
     * when `woo_cacheable_session` is ON. Moving them allows an anonymous shopper
     * who holds only these cookies to receive the same shared cached shell as a
     * no-cookie anonymous visitor; the per-user cart widget is then repainted by
     * WooCommerce's own cart-fragments mechanism.
     *
     * These patterns are NEVER moved out of bypass unless the flag is explicitly
     * on. When off, they stay in DEFAULT_BYPASS_COOKIES and the behaviour is
     * byte-identical to the pre-feature state.
     *
     * @var list<string>
     */
    public const WOO_SESSION_COOKIES = [
        'wp_woocommerce_session_',
        'woocommerce_cart_hash',
        'woocommerce_items_in_cart',
    ];

    /**
     * Merge the baked-in default bypass cookies with an operator-supplied list,
     * de-duplicated (case-insensitively) and order-stable (defaults first). This
     * is the single source of truth used by BOTH the PHP cacheability path and
     * what {@see CacheConfig::toDropinArray()} inlines into the serving drop-in,
     * guaranteeing the pre-WP fast path and the PHP write layer bypass the exact
     * same cookie set.
     *
     * When $wooSession is TRUE the three WooCommerce cart/session patterns listed
     * in {@see WOO_SESSION_COOKIES} are excluded from the bypass set — they are
     * handled separately as "neither bypass nor key" (ignored) so an anonymous
     * shopper with only those cookies maps to the same shared shell as a no-cookie
     * visitor. When $wooSession is FALSE (the default) the output is byte-identical
     * to the pre-feature behaviour.
     *
     * @param list<string> $operator   Operator-configured bypass cookies.
     * @param bool         $wooSession Whether the WooCommerce cacheable-session flag is ON.
     * @return list<string> Effective bypass-cookie list.
     */
    public static function effectiveBypassCookies(array $operator, bool $wooSession = false): array
    {
        // When the WooCommerce session flag is on, the three Woo cart/session
        // patterns are promoted out of the bypass set. Build a case-insensitive
        // lookup of the patterns to exclude.
        $wooIgnoreLower = [];
        if ($wooSession) {
            foreach (self::WOO_SESSION_COOKIES as $pattern) {
                $wooIgnoreLower[strtolower($pattern)] = true;
            }
        }

        $out  = [];
        $seen = [];
        foreach (array_merge(self::DEFAULT_BYPASS_COOKIES, $operator) as $cookie) {
            $cookie = trim((string) $cookie);
            if ($cookie === '') {
                continue;
            }
            $lower = strtolower($cookie);
            // Skip Woo cart/session patterns when the feature flag is on.
            if ($wooSession && isset($wooIgnoreLower[$lower])) {
                continue;
            }
            if (isset($seen[$lower])) {
                continue;
            }
            $seen[$lower] = true;
            $out[]        = $cookie;
        }
        return $out;
    }

    /**
     * Extra cache-varying query params configured per-site (operator).
     *
     * @var list<string>
     */
    private array $extraIncludeQueries;

    /**
     * Substring keywords that, if present in a URL, bypass the cache.
     *
     * @var list<string>
     */
    private array $bypassUrls;

    /**
     * Keywords matched against cookie NAMES to bypass the cache.
     *
     * @var list<string>
     */
    private array $bypassCookies;

    /**
     * Whether the WooCommerce cacheable-session feature is effectively active.
     * True only when BOTH the operator flag (woo_cacheable_session) AND the
     * persisted probe result (wpmgr_woo_fragments_supported) are true. When
     * either is false the three Woo cart/session cookie patterns remain in the
     * hard-bypass set and behaviour is byte-identical to flag-off. DEFAULT-OFF.
     */
    private bool $wooSession;

    /**
     * @param list<string> $extraIncludeQueries Operator-added cache-varying query params.
     * @param list<string> $bypassUrls          Substrings that disable caching for a URL.
     * @param list<string> $bypassCookies       Cookie-name keywords that disable caching.
     * @param bool         $wooSession          WooCommerce cacheable-session flag (default off).
     * @param bool|null    $wooSupported        Persisted probe result (null = read from option).
     *                                           Explicit false always disables the feature
     *                                           regardless of $wooSession. Injected by tests
     *                                           to avoid a real get_option() call.
     */
    public function __construct(
        array $extraIncludeQueries = [],
        array $bypassUrls = [],
        array $bypassCookies = [],
        bool $wooSession = false,
        ?bool $wooSupported = null
    ) {
        $this->extraIncludeQueries = array_values(array_filter(
            array_map('strval', $extraIncludeQueries),
            static fn (string $s): bool => $s !== ''
        ));
        $this->bypassUrls = array_values(array_filter(
            array_map('strval', $bypassUrls),
            static fn (string $s): bool => $s !== ''
        ));
        // The baked-in default "always-bypass" cookies (cart/session/logged-in/
        // password/comment-author) are ALWAYS merged in, so a logged-out cart
        // request is non-cacheable even when the operator configured nothing.
        // When $wooSession is TRUE AND the probe confirms fragment support, the
        // three Woo cart/session patterns are promoted out of the bypass set.
        // If support is not confirmed, behaviour is byte-identical to flag-off.
        $operatorBypass = array_values(array_filter(
            array_map('strval', $bypassCookies),
            static fn (string $s): bool => $s !== ''
        ));

        // Resolve the support probe result. Only relevant when the operator flag
        // is on — if the flag is off we never promote Woo cookies out of bypass
        // regardless of support state, so there is no point reading the option.
        // This avoids an unnecessary get_option() call (and Brain Monkey stub
        // requirement in tests) on all flag-off sites.
        if ($wooSession) {
            if ($wooSupported === null) {
                $wooSupported = (bool) (function_exists('get_option')
                    ? get_option(PerfReporter::OPTION_WOO_FRAGMENTS_SUPPORTED, false)
                    : false);
            }
        } else {
            $wooSupported = false;
        }

        // The feature is active only when BOTH the operator flag AND the agent's
        // own probe have confirmed support.
        $this->wooSession    = $wooSession && $wooSupported;
        $this->bypassCookies = self::effectiveBypassCookies($operatorBypass, $this->wooSession);
    }

    /**
     * Cheap URL-only cacheability (preload / purge planning).
     *
     * Rejects when: an explicit no-cache flag is present; the path is an
     * admin/auth/API/system path; the URL matches a bypass keyword; or the query
     * contains any param that is neither a known marketing param nor a known
     * cache-varying param (an unknown param ⇒ dynamic, uncacheable).
     *
     * @param string $url Absolute or path-relative URL.
     * @return bool
     */
    public function isUrlCacheable(string $url): bool
    {
        if ($url === '') {
            return false;
        }

        // Explicit per-request opt-out.
        if (stripos($url, 'no_optimize') !== false || stripos($url, 'nocache') !== false) {
            return false;
        }

        $path  = (string) (wp_parse_url($url, PHP_URL_PATH) ?? '');
        if ($path !== '' && preg_match(self::UNCACHEABLE_PATH_PATTERN, $path) === 1) {
            return false;
        }

        if ($this->matchesBypassUrl($url)) {
            return false;
        }

        // Unknown query params make the URL uncacheable.
        $queryStr = (string) (wp_parse_url($url, PHP_URL_QUERY) ?? '');
        if ($queryStr !== '') {
            parse_str($queryStr, $params);
            foreach (array_keys($params) as $key) {
                if (!$this->isKnownQueryParam((string) $key)) {
                    return false;
                }
            }
        }

        return true;
    }

    /**
     * Full request+response cacheability gate (called at output-buffer flush).
     *
     * Recognised $ctx keys (all read defensively with safe defaults): url,
     * method, cookies, is_admin, is_ajax, status, logged_in, cache_logged_in,
     * password_required, body. No superglobal access here.
     *
     * @param array<string,mixed> $ctx Resolved request/response context.
     * @return bool
     */
    public function isRequestCacheable(array $ctx): bool
    {
        $url    = (string) ($ctx['url'] ?? '');
        $method = strtoupper((string) ($ctx['method'] ?? 'GET'));

        if (!$this->isUrlCacheable($url)) {
            return false;
        }
        if ($method !== 'GET') {
            return false;
        }
        if (!empty($ctx['is_ajax'])) {
            return false;
        }
        if (!empty($ctx['is_admin'])) {
            return false;
        }
        if ((int) ($ctx['status'] ?? 200) !== 200) {
            return false;
        }
        // Logged-in requests are only cacheable when explicitly enabled.
        if (!empty($ctx['logged_in']) && empty($ctx['cache_logged_in'])) {
            return false;
        }
        // Password-protected singular content is never cached.
        if (!empty($ctx['password_required'])) {
            return false;
        }
        // Per-page no-cache meta: never store excluded singular pages.
        if (function_exists('is_singular') && is_singular()
            && function_exists('get_queried_object_id') && function_exists('get_post_meta')
        ) {
            if (get_post_meta((int) get_queried_object_id(), '_wpmgr_no_cache', true) === '1') {
                return false;
            }
        }
        // Any bypass cookie disables caching for this request.
        if ($this->matchesBypassCookie((array) ($ctx['cookies'] ?? []))) {
            return false;
        }
        // WooCommerce cart/checkout/account pages must NEVER be cached — they are
        // per-visitor by definition and the cart-hash cookie may not yet be set on
        // the very first add-to-cart. This is the safety net behind the cookie
        // bypass. Resolved by the writer's context probe (see resolveContext) and
        // re-checked here so the gate is unit-testable.
        if (!empty($ctx['wc_excluded']) || $this->isWooCommerceExcludedPage()) {
            return false;
        }

        // Non-empty-cart guard (Phase 2b): when the WooCommerce session flag is ON
        // we must ONLY write the shared shell when the generating request has an
        // EMPTY cart. A non-empty cart must never be baked into the shared shell
        // (that would serve one shopper's cart to the next). The wc_cart_empty
        // context key is resolved by CacheWriter::resolveContext and defaults to
        // true (safe) when absent. When the flag is OFF this guard is vacuously
        // satisfied (wooSession is false so the condition never fires).
        if ($this->wooSession && isset($ctx['wc_cart_empty']) && $ctx['wc_cart_empty'] === false) {
            return false;
        }

        // The response body must be a full HTML document.
        $body = (string) ($ctx['body'] ?? '');
        if (preg_match('/<!DOCTYPE\s*html\b[^>]*>/i', $body) !== 1) {
            return false;
        }

        // Phase 3 nonce guard: when the WooCommerce session flag is ON, inspect
        // the body for state-mutating nonces that cannot be safely refreshed
        // client-side. Add-to-cart nonces are safe (they are refreshed by the
        // cart-fragments response or verified server-side with redirect-on-fail).
        // Any other state-mutating nonce field (checkout, payment, account forms)
        // indicates a page we cannot safely serve as a shared shell — fall back to
        // full bypass. Cart/checkout/account are already excluded above, but
        // plugins may embed actionable nonce fields on otherwise-innocuous pages.
        if ($this->wooSession && $body !== '' && $this->hasNonRefreshableNonce($body)) {
            return false;
        }

        return true;
    }

    /**
     * Detect state-mutating nonce fields in the page body that cannot be safely
     * refreshed client-side from WooCommerce's AJAX surface.
     *
     * Safe nonces (add-to-cart): WooCommerce verifies these server-side and
     * redirects on failure (no destructive consequence of a stale nonce). They
     * are also refreshed in the mini-cart fragment HTML when cart-fragments runs.
     *
     * Unsafe nonces: any nonce field whose name pattern suggests a destructive
     * or irreversible action — checkout submission, payment, account mutation,
     * subscription, etc. If ANY such nonce is present the page is NOT shell-
     * cacheable (safe fallback: full bypass).
     *
     * PRIME DIRECTIVE: when uncertain, return TRUE (force bypass).
     *
     * Hardened checks:
     *   (a) Hidden-input detection tolerates unquoted attribute values (HTML5
     *       allows omitting quotes when the value contains no whitespace or ">").
     *   (b) Name extraction tolerates both quoted and unquoted values.
     *   (c) When the flag is on, a bare `_wpnonce` token or any `*-nonce`/
     *       `*_nonce` token appearing anywhere in the body (outside the allowlist)
     *       also forces bypass — nonces can appear in data-attributes, inline
     *       script variables (e.g. wpApiSettings.nonce), or button values.
     *
     * @param string $body The rendered page HTML.
     * @return bool True when an unsafe nonce was found (page must NOT be cached).
     */
    private function hasNonRefreshableNonce(string $body): bool
    {
        // --- Pass 1: hidden <input> fields (quoted AND unquoted attribute values) ---
        // Matches <input … type="hidden" …> or <input … type=hidden …>.
        if (preg_match_all(
            '/<input\b[^>]+type=(["\']{0,1})hidden\1[^>]*>/i',
            $body,
            $inputs
        )) {
            foreach ($inputs[0] as $input) {
                // Extract the name attribute value, tolerating quoted or unquoted.
                // Pattern: name= followed by an optional quote, then the value,
                // then the matching quote (or end-of-attribute for unquoted).
                if (!preg_match('/\bname=(["\'"]?)([^"\'>\s]+)\1/i', $input, $nameMatch)) {
                    continue;
                }
                $name = strtolower($nameMatch[2]);

                // Skip if this is not nonce-related.
                if (strpos($name, 'nonce') === false && strpos($name, '_wpnonce') === false
                    && strpos($name, 'security') === false
                ) {
                    continue;
                }

                // Safe nonce patterns: add-to-cart actions. These are verified
                // server-side with redirect-on-fail, not destructive, and refreshed
                // by the cart-fragments response.
                if ($this->isSafeNonceName($name)) {
                    continue;
                }

                // Any other nonce field on a page we would cache is a signal that
                // the page has actionable state we cannot safely serve from a
                // shared shell. Safe fallback: force full bypass.
                return true;
            }
        }

        // --- Pass 2: bare nonce tokens anywhere in the body -----------------------
        // Nonces also appear in data-attributes (data-nonce="..."), inline JS
        // variables (wpApiSettings.nonce = "..."), and button values. A bare
        // `_wpnonce` token or any identifier ending in `-nonce` / `_nonce`
        // appearing in the body is treated as a nonce signal — unless it matches
        // the add-to-cart allowlist.
        //
        // Pattern: an identifier that ends with -nonce or _nonce, or is exactly
        // _wpnonce, followed by a non-identifier character (to avoid false-positive
        // on e.g. class="add-noncense"). We also allow a leading word-boundary so
        // we don't match substrings inside longer tokens.
        if (preg_match_all(
            '/\b(_wpnonce|(?:[a-z0-9_-]+-nonce|[a-z0-9_-]+_nonce))\b/i',
            $body,
            $nonces
        )) {
            foreach ($nonces[1] as $token) {
                $lower = strtolower($token);
                if ($this->isSafeNonceName($lower)) {
                    continue;
                }
                return true;
            }
        }

        return false;
    }

    /**
     * Whether a nonce name is on the add-to-cart allowlist (safe to cache).
     * These nonces are verified server-side with redirect-on-fail, are not
     * destructive, and are refreshed by the cart-fragments response.
     *
     * @param string $name Lower-cased nonce name or identifier.
     * @return bool
     */
    private function isSafeNonceName(string $name): bool
    {
        return strpos($name, 'add_to_cart') !== false
            || strpos($name, 'add-to-cart') !== false
            || strpos($name, 'woocommerce-add-to-cart') !== false;
    }

    /**
     * Whether a query param name is recognised (marketing ignore OR cache-varying
     * include OR an operator-configured extra include).
     *
     * @param string $name Query parameter name.
     * @return bool
     */
    public function isKnownQueryParam(string $name): bool
    {
        if (MarketingParams::isIgnored($name) || MarketingParams::isIncluded($name)) {
            return true;
        }
        $lower = strtolower($name);
        foreach ($this->extraIncludeQueries as $extra) {
            if (strtolower($extra) === $lower) {
                return true;
            }
        }
        return false;
    }

    /**
     * Whether the current request resolves to a WooCommerce cart, checkout,
     * "my account", or any WC endpoint (order-received, add-payment-method, etc.)
     * page. Only ever true inside a full WooCommerce request — every predicate is
     * `function_exists`-guarded so this is a cheap `false` on non-WC sites and in
     * unit tests. Returns false unless WooCommerce is actually active.
     *
     * @return bool
     */
    public function isWooCommerceExcludedPage(): bool
    {
        if (!class_exists('WooCommerce')) {
            return false;
        }
        if (function_exists('is_cart') && is_cart()) {
            return true;
        }
        if (function_exists('is_checkout') && is_checkout()) {
            return true;
        }
        if (function_exists('is_account_page') && is_account_page()) {
            return true;
        }
        if (function_exists('is_wc_endpoint_url') && is_wc_endpoint_url()) {
            return true;
        }
        return false;
    }

    /**
     * Whether a URL contains any configured bypass keyword (case-insensitive
     * substring), spelled with or without the trailing slash of its path.
     *
     * The advanced-cache drop-in applies the same rule before WordPress loads
     * and cannot call this class, so the block that builds the subject is
     * duplicated there verbatim; tests/CachePrefixedHomepageTest.php keeps the
     * two copies byte-identical.
     *
     * @param string $url URL to test.
     * @return bool
     */
    public function matchesBypassUrl(string $url): bool
    {
        if ($this->bypassUrls === []) {
            return false;
        }

        // Bridge through the drop-in's variable name so the two copies of the
        // block below are byte-identical once dedented.
        $wpmgr_uri = $url;

        // WPMGR-BYPASS-URL-SHARED BEGIN
        // WordPress answers a page with or without the trailing slash of its
        // path, and a homepage under a path prefix (a language, a subsite) has
        // no redirect between the two spellings: /en and /en/ are one page. A
        // Bypass URL written either way has to cover both, so the subject also
        // carries a slash at the end of its path part, before any query or
        // fragment. An entry is tested against the subject AND against the
        // string as it arrived, so nothing that matched before stops matching.
        $wpmgr_subject = $wpmgr_uri;
        $wpmgr_cut     = strcspn($wpmgr_subject, '?#');
        if ($wpmgr_cut === 0 || substr($wpmgr_subject, $wpmgr_cut - 1, 1) !== '/') {
            $wpmgr_subject = substr($wpmgr_subject, 0, $wpmgr_cut) . '/' . substr($wpmgr_subject, $wpmgr_cut);
        }
        unset($wpmgr_cut);
        // WPMGR-BYPASS-URL-SHARED END

        foreach ($this->bypassUrls as $keyword) {
            if (stripos($wpmgr_uri, $keyword) !== false || stripos($wpmgr_subject, $keyword) !== false) {
                return true;
            }
        }
        return false;
    }

    /**
     * Whether any cookie NAME matches a configured bypass keyword (case-
     * insensitive substring). WooCommerce/EDD cart and session cookies are the
     * usual targets.
     *
     * @param array<string,mixed> $cookies Request cookie map.
     * @return bool
     */
    public function matchesBypassCookie(array $cookies): bool
    {
        foreach (array_keys($cookies) as $name) {
            $name = (string) $name;
            foreach ($this->bypassCookies as $keyword) {
                if (stripos($name, $keyword) !== false) {
                    return true;
                }
            }
        }
        return false;
    }
}
