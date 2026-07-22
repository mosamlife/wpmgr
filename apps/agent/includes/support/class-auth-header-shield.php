<?php
/**
 * AuthHeaderShield: relocates the agent's signed Authorization bearer token
 * out of $_SERVER before any other plugin's early auth hooks can see it.
 *
 * The control plane sends the agent's Ed25519-signed command token as a plain
 * `Authorization: Bearer <token>` header on the agent's own REST routes
 * (GET /wpmgr/v1/info and POST /wpmgr/v1/command/{command}). Some third-party
 * plugins install a GLOBAL `determine_current_user` / `rest_authentication_errors`
 * filter that unconditionally tries to decode ANY `Authorization: Bearer` value
 * as one of their own JWTs, without checking which REST route the request is
 * for. When handed a token in a format they do not recognize, such a filter
 * can throw an uncaught exception, which fatals the request during WordPress's
 * REST `determine_current_user` phase, BEFORE this plugin's own
 * `permission_callback` (and therefore its own Ed25519 verification) ever runs.
 *
 * This class runs at plugin-include time, which is always BEFORE WordPress's
 * `parse_request` / `rest_api_init` / `determine_current_user` phases (WP loads
 * and includes every active plugin's main file first, then dispatches). Moving
 * the header out of $_SERVER at include time therefore preempts every such
 * filter unconditionally, with no dependency on plugin load order or hook
 * priority: by the time any REST auth hook runs, the header is already gone.
 *
 * The token itself is stashed (not discarded) so this plugin's own Router can
 * still read it — see Router::bearerToken(), which consults this class first
 * and falls back to the live request header for any code path that never went
 * through this shield (e.g. cookie-authenticated REST calls, or a unit test
 * that injects the header directly on a WP_REST_Request).
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/**
 * Pre-dispatch shield that moves this plugin's own Authorization bearer token
 * out of $_SERVER before third-party global auth filters can see it.
 */
final class AuthHeaderShield
{
    /**
     * The bearer token stashed by protect(), or null when nothing was stashed
     * for this request (no matching route, no Authorization header, or a
     * non-Bearer scheme such as Basic auth for Application Passwords).
     *
     * @var string|null
     */
    private static ?string $stashedBearer = null;

    /**
     * Move an Authorization: Bearer header off of $_SERVER when the request
     * targets one of this plugin's own REST routes, stashing the token so
     * Router::bearerToken() can still read it later. A no-op for every other
     * request, at the cost of a couple of cheap strpos() calls.
     *
     * Call this once, at plugin-include time, before anything that could
     * register a `determine_current_user` / `rest_authentication_errors`
     * filter has a chance to run.
     *
     * @return void
     */
    public static function protect(): void
    {
        // Always reflect the CURRENT request, never a token stashed by a
        // prior request in the same persistent process (WP-CLI, the test
        // harness, or any long-running SAPI). Ed25519 verification (jti/exp/
        // cmd) already rejects a stale token on its own, but protect() should
        // never hand out one anyway.
        self::$stashedBearer = null;

        if (!isset($_SERVER['REQUEST_URI'])) {
            return;
        }

        // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized,WordPress.Security.ValidatedSanitizedInput.MissingUnslash -- pre-dispatch route-match probe only, never output or persisted
        $reqUri = (string) $_SERVER['REQUEST_URI'];
        $qpos   = strpos($reqUri, '?');
        $path   = rawurldecode($qpos === false ? $reqUri : substr($reqUri, 0, $qpos));
        $query  = rawurldecode($qpos === false ? '' : substr($reqUri, $qpos + 1));

        // Fast path for the overwhelming majority of requests, which are not
        // for this plugin's REST namespace at all: bail after two strpos()
        // calls with zero class loading beyond this file. The '/wpmgr/v1/'
        // literal MUST equal '/' . Router::NAMESPACE . '/' — see
        // AuthHeaderShieldTest for a regression guard tying the two together.
        //
        // Matched on the PATH for pretty permalinks (.../wp-json/wpmgr/v1/...,
        // including a multisite subdirectory or a custom rest_url_prefix, all
        // of which still carry the namespace in the path) and separately on
        // the QUERY for the plain-permalink `?rest_route=` form, so a
        // completely unrelated route whose URI merely happens to CONTAIN the
        // literal "/wpmgr/v1/" inside some other query parameter's value
        // (e.g. a redirect target) is never matched.
        if (strpos($path, '/wpmgr/v1/') === false
            && strpos($query, 'rest_route=/wpmgr/v1/') === false
        ) {
            return;
        }

        // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized,WordPress.Security.ValidatedSanitizedInput.MissingUnslash -- pre-dispatch auth-header relocation; token stashed verbatim for this plugin's own Ed25519 verify (Connector::verifyCommand), header unset immediately below so third-party JWT middleware cannot fatal on it
        $auth = (string) ($_SERVER['HTTP_AUTHORIZATION'] ?? ($_SERVER['REDIRECT_HTTP_AUTHORIZATION'] ?? ''));

        if (stripos($auth, 'Bearer ') !== 0) {
            // Not our token (missing, or a different scheme such as Basic
            // auth for Application Passwords) — leave $_SERVER untouched.
            return;
        }

        // trim() mirrors Router::bearerToken()'s existing fallback path so
        // both sources of the token are normalized identically.
        self::$stashedBearer = trim(substr($auth, 7));

        unset($_SERVER['HTTP_AUTHORIZATION'], $_SERVER['REDIRECT_HTTP_AUTHORIZATION']);
    }

    /**
     * The bearer token stashed by protect() for this request, if any.
     *
     * @return string|null
     */
    public static function bearer(): ?string
    {
        return self::$stashedBearer;
    }
}
