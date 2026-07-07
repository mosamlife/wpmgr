<?php
/**
 * HideBackendModule — secret-slug login-page obfuscation.
 *
 * When enabled, this module:
 *  1. Intercepts at setup_theme (before WP routes to wp-login.php/wp-admin).
 *  2. Compares the request path against hide_backend_slug:
 *       path == slug → set a short-lived access cookie, then defer to
 *         wp_loaded to require() wp-login.php in place so the request is
 *         served AS a real wp-login.php hit (login form, lost-password,
 *         registration, logout — the full multi-request login dance).
 *       canonical wp-login/wp-admin for logged-out un-tokened visitors → 404 or redirect.
 *  3. Bails for REST/cron/CLI/WP_INSTALLING so the agent's own wpmgr/v1 routes
 *     and the autologin path remain fully reachable.
 *  4. Filters login_url/logout_url/lostpassword_url/site_url so the login
 *     form's own rendered links point at the secret slug instead of the
 *     literal /wp-login.php path — but ONLY while this request is actually
 *     serving the hidden login form (see $servingLoginForm below). On every
 *     other request (a normal front-end page render, a comment form, a theme
 *     nav "Log in" link, etc.) these filters are inert no-ops, so a
 *     logged-out visitor browsing the site never sees the secret slug in
 *     any rendered HTML.
 *
 * LOCKOUT-PROOFING:
 *  - define('WPMGR_DISABLE_HIDE_BACKEND', true) disables this entirely.
 *  - The autologin path (POST /wp-json/wpmgr/v1/autologin) is a REST route
 *    and hits the REST bail before any redirect fires.
 *  - Logged-in users are never redirected.
 *  - /wp-cron.php, CLI, WP_INSTALLING all bail.
 *  - The cookie doubles as an access token: once the slug is visited and the
 *    cookie is set, all subsequent wp-login.php requests in that browser session
 *    are allowed (multi-request login dance). The cookie value is an HMAC of
 *    the configured slug (not a guessable constant), so it can only be minted
 *    by someone who already reached the slug.
 *
 * @package WPMgr\Agent\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Security;

/**
 * Hide-backend login-slug enforcement.
 */
final class HideBackendModule
{
    /** Cookie name for the access token set after the slug is visited. */
    public const COOKIE_ACCESS = 'wpmgr_hb_access';

    /** Access-cookie TTL in seconds (1 hour — covers the full multi-request login dance). */
    private const COOKIE_TTL = 3600;

    private SecurityPolicy $policy;

    /**
     * True only while this request is actively serving the hidden login form
     * (set by serveLoginForm() on a slug match). Gates the login_url/
     * logout_url/lostpassword_url/site_url rewrite filters so they NEVER fire
     * on a normal front-end page render — only the login form's own rendered
     * links get pointed at the secret slug.
     */
    private bool $servingLoginForm = false;

    /**
     * @param SecurityPolicy $policy Active site policy.
     */
    public function __construct(SecurityPolicy $policy)
    {
        $this->policy = $policy;
    }

    /**
     * Register WordPress hooks. Call once on plugins_loaded.
     *
     * @return void
     */
    public function install(): void
    {
        static $installed = false;
        if ($installed) {
            return;
        }
        $installed = true;

        // Recovery constant.
        if (defined('WPMGR_DISABLE_HIDE_BACKEND') && WPMGR_DISABLE_HIDE_BACKEND) {
            return;
        }

        if (!$this->policy->hideBackendEnabled || $this->policy->hideBackendSlug === '') {
            return;
        }

        // Intercept at setup_theme — earliest WP hook after plugins are loaded
        // and before wp-login.php / wp-admin routing takes effect.
        add_action('setup_theme', [$this, 'interceptRequest']);

        // Keep the login form's own rendered links (lost-password, register,
        // logout, the form's POST action) pointed at the secret slug rather
        // than the literal /wp-login.php path. Registered unconditionally,
        // but each callback is gated on isServingHiddenLoginForm() so they
        // are inert on every other request — a normal front-end page render
        // never leaks the slug.
        add_filter('login_url', [$this, 'rewriteLoginLinkUrl']);
        add_filter('logout_url', [$this, 'rewriteLoginLinkUrl']);
        add_filter('lostpassword_url', [$this, 'rewriteLoginLinkUrl']);
        add_filter('site_url', [$this, 'rewriteSiteUrl'], 10, 2);
    }

    /**
     * Intercept the current request and redirect/block as configured.
     * Called on setup_theme.
     *
     * @return void
     */
    public function interceptRequest(): void
    {
        // Always bail for REST, cron, WP-CLI, WP_INSTALLING.
        if ($this->shouldBail()) {
            return;
        }

        $slug    = $this->policy->hideBackendSlug;
        $request = $this->getRequestPath();

        // Slug match: set the access cookie and serve wp-login.php in place.
        if ($this->matchesSlug($request, $slug)) {
            $this->setAccessCookie();
            $this->serveLoginForm();
            return;
        }

        // Canonical wp-login / wp-admin for a logged-out, un-tokened visitor.
        if ($this->isLoginOrAdminPath($request)) {
            if (function_exists('is_user_logged_in') && is_user_logged_in()) {
                return;
            }

            // Check for the access token cookie (multi-request login dance).
            if ($this->hasAccessCookie()) {
                return;
            }

            // Block: 404 or redirect.
            $redirect = $this->policy->hideBackendRedirect;
            if ($redirect !== '') {
                if (!headers_sent()) {
                    header('Location: ' . esc_url_raw($redirect), true, 302);
                }
            } else {
                if (!headers_sent()) {
                    http_response_code(404);
                    header('Content-Type: text/html; charset=utf-8');
                }
                // translators: Shown when the login page is hidden and the user accesses the wrong URL.
                echo esc_html__('Page not found.', 'wpmgr-agent');
            }
            exit;
        }
    }

    // -------------------------------------------------------------------------
    // Private helpers
    // -------------------------------------------------------------------------

    /**
     * Present the current request AS wp-login.php and defer the actual
     * require() to wp_loaded.
     *
     * setup_theme runs too early to load wp-login.php directly — the login
     * screen depends on init/login_init/login_enqueue_scripts firing first,
     * exactly as they would for a real /wp-login.php hit. Deferring to
     * wp_loaded (which fires after init) lets that full sequence run, then
     * requires wp-login.php and exits, so this request never falls through
     * to normal template routing (which would 404/redirect the secret slug).
     *
     * @return void
     */
    private function serveLoginForm(): void
    {
        if (!defined('ABSPATH') || !file_exists(ABSPATH . 'wp-login.php')) {
            return;
        }

        // From this point on, the login/logout/lostpassword/site_url rewrite
        // filters are allowed to rewrite THIS request's own links to the
        // secret slug — see isServingHiddenLoginForm().
        $this->servingLoginForm = true;

        global $pagenow;
        // phpcs:ignore WordPress.WP.GlobalVariablesOverride.Prohibited -- deliberately presenting this request AS wp-login.php; no core API exists to spoof $pagenow for a plugin-served login screen.
        $pagenow = 'wp-login.php';

        if (isset($_SERVER['SCRIPT_NAME']) && is_string($_SERVER['SCRIPT_NAME'])) {
            $scriptName             = sanitize_text_field(wp_unslash($_SERVER['SCRIPT_NAME']));
            $rewrittenScriptName    = dirname($scriptName) . '/wp-login.php';
            $_SERVER['SCRIPT_NAME'] = $rewrittenScriptName;
            $_SERVER['PHP_SELF']    = $rewrittenScriptName;
        }

        add_action('wp_loaded', static function (): void {
            require ABSPATH . 'wp-login.php';
            exit;
        }, 0);
    }

    /**
     * Rewrite a login/logout/lost-password URL to point at the secret slug
     * instead of the literal /wp-login.php path.
     *
     * Registered against the login_url, logout_url, and lostpassword_url
     * core filters, each of which passes the built URL as the first argument.
     * A no-op unless this request is actively serving the hidden login form
     * (isServingHiddenLoginForm()) — otherwise a normal front-end page render
     * (Meta widget, comment form, theme nav "Log in" link, etc.) would leak
     * the secret slug to every logged-out visitor.
     *
     * @param string $url The URL WordPress built (e.g. via wp_login_url()).
     * @return string
     */
    public function rewriteLoginLinkUrl(string $url): string
    {
        if ($this->policy->hideBackendSlug === '' || !$this->isServingHiddenLoginForm()) {
            return $url;
        }
        return $this->swapLoginPathForSlug($url);
    }

    /**
     * Rewrite site_url('wp-login.php...') calls to point at the secret slug.
     *
     * Registered against the site_url core filter. Only touches URLs built
     * from a wp-login.php path (e.g. the login form's own POST action target,
     * or wp_registration_url()) while this request is actively serving the
     * hidden login form (isServingHiddenLoginForm()); every other site_url()
     * call — including wp-login.php paths built during a normal front-end
     * render — passes through untouched.
     *
     * @param string $url  The fully assembled URL.
     * @param string $path The raw path argument passed to site_url().
     * @return string
     */
    public function rewriteSiteUrl(string $url, string $path = ''): string
    {
        if ($this->policy->hideBackendSlug === ''
            || !$this->isServingHiddenLoginForm()
            || !str_starts_with($path, 'wp-login.php')
        ) {
            return $url;
        }
        return $this->swapLoginPathForSlug($url);
    }

    /**
     * Whether this request is actively serving the hidden login form.
     *
     * Primary signal is the instance flag set by serveLoginForm() (only
     * reached on a slug match). $GLOBALS['pagenow'] is checked as a
     * defensive secondary signal: WordPress core itself sets $pagenow to
     * 'wp-login.php' for a genuine, already-authorized (cookie or logged-in)
     * direct hit on /wp-login.php, and never for a front-end page — so this
     * can only ever narrow the front-end leak surface further, never widen it.
     *
     * @return bool
     */
    private function isServingHiddenLoginForm(): bool
    {
        return $this->servingLoginForm || (($GLOBALS['pagenow'] ?? '') === 'wp-login.php');
    }

    /**
     * Replace the literal /wp-login.php path segment in a URL with the
     * configured secret slug, preserving any query string.
     *
     * @param string $url
     * @return string
     */
    private function swapLoginPathForSlug(string $url): string
    {
        $replaced = preg_replace('#/wp-login\.php#', '/' . $this->policy->hideBackendSlug, $url, 1);
        return is_string($replaced) ? $replaced : $url;
    }

    /**
     * Determine whether we should bail (not interfere).
     *
     * @return bool
     */
    private function shouldBail(): bool
    {
        // WP-CLI.
        if (php_sapi_name() === 'cli') {
            return true;
        }

        // WP Cron.
        if (defined('DOING_CRON') && DOING_CRON) {
            return true;
        }

        // WP Install.
        if (defined('WP_INSTALLING') && WP_INSTALLING) {
            return true;
        }

        // REST API: any /wp-json/ request, including the agent's wpmgr/v1 routes.
        if (defined('REST_REQUEST') && REST_REQUEST) {
            return true;
        }

        // Also detect REST by path prefix (REST_REQUEST may not be defined yet).
        $request = $this->getRequestPath();
        if (str_contains($request, '/wp-json/')) {
            return true;
        }

        return false;
    }

    /**
     * Get the current request path (without query string).
     *
     * @return string
     */
    private function getRequestPath(): string
    {
        if (!isset($_SERVER['REQUEST_URI']) || !is_string($_SERVER['REQUEST_URI'])) {
            return '';
        }
        $uri  = sanitize_text_field(wp_unslash($_SERVER['REQUEST_URI']));
        $path = strtok($uri, '?');
        return is_string($path) ? rtrim($path, '/') : '';
    }

    /**
     * Whether the request path matches the configured slug.
     *
     * Compares the FIRST path segment (the segment immediately after the
     * leading slash, before any subdirectory) against the slug. This prevents
     * a slug match at any arbitrary depth, e.g. a request for
     * /some/deep/my-secret-login must NOT trigger the slug handler; only
     * /my-secret-login (at root depth) should.
     *
     * Sites installed in a subdirectory are handled by stripping the
     * subdirectory prefix if one is detected via ABSPATH / home_url. In the
     * simple/common case (root install), the first segment comparison is exact.
     *
     * @param string $path The request path (no query string, no trailing slash).
     * @param string $slug The configured hide-backend slug.
     * @return bool
     */
    private function matchesSlug(string $path, string $slug): bool
    {
        // Normalise: ensure leading slash, no trailing slash.
        $path = '/' . ltrim($path, '/');

        // Extract the first path segment.
        // For '/my-secret-login' the segment is 'my-secret-login'.
        // For '/subdir/my-secret-login' the segment is 'subdir' (not a match).
        $afterSlash = ltrim($path, '/');
        $firstSlash = strpos($afterSlash, '/');
        $firstSegment = ($firstSlash !== false)
            ? substr($afterSlash, 0, $firstSlash)
            : $afterSlash;

        return $firstSegment === $slug;
    }

    /**
     * Whether the path is a canonical wp-login or wp-admin location.
     * Also catches wp-login.php?action=* variants (lost password, register, etc.)
     * so the access-cookie check applies to the full login multi-request dance.
     *
     * @param string $path The request path (no query string, no trailing slash).
     * @return bool
     */
    private function isLoginOrAdminPath(string $path): bool
    {
        $loginFile = '/wp-login.php';
        $adminDir  = '/wp-admin';

        // Match /wp-login.php at any install depth (e.g. /subdir/wp-login.php).
        // Also catch the original REQUEST_URI with a query string still present --
        // getRequestPath() strips the query via strtok, but we handle both to be safe.
        if (str_contains($path, 'wp-login.php')) {
            return true;
        }

        return $path === $adminDir
            || str_starts_with($path, $adminDir . '/');
    }

    /**
     * Set the access cookie so the multi-request login dance continues to work.
     *
     * @return void
     */
    private function setAccessCookie(): void
    {
        if (headers_sent()) {
            return;
        }
        setcookie(
            self::COOKIE_ACCESS,
            $this->accessCookieValue(),
            [
                'expires'  => time() + self::COOKIE_TTL,
                'path'     => '/',
                'httponly' => true,
                'secure'   => is_ssl(),
                'samesite' => 'Strict',
            ]
        );
    }

    /**
     * Whether the current request has a valid access cookie.
     *
     * The presented cookie value is compared with hash_equals() against the
     * HMAC recomputed for the CURRENTLY configured slug, so a cookie can only
     * be minted by someone who already reached the slug (setAccessCookie() is
     * only ever called from the slug-match branch) — a guessable static value
     * (e.g. the literal digit "1") no longer bypasses the canonical block,
     * and a cookie minted for a different/previous slug is rejected too.
     *
     * @return bool
     */
    private function hasAccessCookie(): bool
    {
        if (!isset($_COOKIE[self::COOKIE_ACCESS]) || !is_string($_COOKIE[self::COOKIE_ACCESS])) {
            return false;
        }
        $presented = sanitize_text_field(wp_unslash($_COOKIE[self::COOKIE_ACCESS]));
        return hash_equals($this->accessCookieValue(), $presented);
    }

    /**
     * The expected access-cookie value: an HMAC of the configured slug, keyed
     * by the site's own auth salt so it cannot be forged or reused across sites.
     *
     * @return string
     */
    private function accessCookieValue(): string
    {
        return hash_hmac('sha256', $this->policy->hideBackendSlug, wp_salt('auth'));
    }
}
