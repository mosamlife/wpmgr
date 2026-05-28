<?php
/**
 * Autologin command: browser-initiated one-click login to wp-admin.
 *
 * Flow (high-level):
 *   1. The browser hits GET {site}/wp-json/wpmgr/v1/autologin?token=<JWT>&redirect_to=<path>.
 *   2. Connector::verifyCommand validates the JWT (Ed25519 sig, exp <= 60s,
 *      anti-replay jti, aud == own site_id, cmd == "autologin").
 *   3. We consult our DEDICATED autologin replay cache (separate from the
 *      Connector's short-window jti table) to guarantee single use across the
 *      full autologin lifetime.
 *   4. We call back to the control plane (signed agent-auth) at
 *      POST /agent/v1/autologin/consume with {nonce, site_id, consumed_from_ip}.
 *      The CP body is AUTHORITATIVE for both the target wp user login and the
 *      list of allowed wp roles — defense-in-depth on top of the JWT.
 *   5. We resolve the local WP user (by login, or first administrator if the
 *      CP told us to "pick").
 *   6. We intersect the user's roles with the CP-supplied allow-list.
 *   7. We MARK replay BEFORE issuing the cookie so a parallel presentation of
 *      the same token cannot race two cookies through.
 *   8. We clear any existing auth, set the WP cookie, fire `wp_login`.
 *   9. We sanitize redirect_to (same-origin, strict charset) and wp_safe_redirect.
 *
 * Notes:
 *   - This route is NOT command-dispatched. It is registered DIRECTLY as a GET
 *     route with `permission_callback => '__return_true'` because the JWT IS the
 *     authorization. AutologinCommand implements CommandInterface only for
 *     registry consistency; its execute() is never invoked by the Router.
 *   - We don't second-guess is_ssl(): operators behind a TLS-terminating proxy
 *     or Cloudflare must configure WordPress (HTTPS env var or a must-use
 *     plugin that sets $_SERVER['HTTPS']) so that wp_set_auth_cookie() picks
 *     the right cookie scheme. See:
 *       https://developer.wordpress.org/reference/functions/is_ssl/
 *   - Hardened security plugins (Wordfence "Block REST API", iThemes Security
 *     "Disable XML-RPC/REST" presets) may strip auth cookies set from a REST
 *     handler. If autologin appears to "succeed" but the next page redirects
 *     to wp-login.php, allow-list /wp-json/wpmgr/v1/autologin in those plugins.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Connector;
use WPMgr\Agent\Enrollment;
use WPMgr\Agent\ReplayCache;
use WPMgr\Agent\Schema;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;

/**
 * Verifies a one-click-login JWT, single-uses it, and issues a WP auth cookie.
 */
final class AutologinCommand implements CommandInterface
{
    /** CP-side path for the single-use consume callback. */
    public const PATH_CONSUME = '/agent/v1/autologin/consume';

    /** Outbound HTTP timeout for the consume callback, in seconds. */
    private const CONSUME_TIMEOUT = 10;

    /** Replay TTL for an autologin jti, in seconds. 24h matches the CP exp ceiling. */
    private const REPLAY_TTL = 86400;

    private Connector $connector;

    private ReplayCache $replay;

    private Signer $signer;

    private Settings $settings;

    /**
     * @param Connector   $connector Ed25519 JWT verifier (reused as-is).
     * @param ReplayCache $replay    DB-backed single-use cache.
     * @param Signer      $signer    Outbound request signer (agent -> CP).
     * @param Settings    $settings  Provides CP URL + own site_id.
     */
    public function __construct(Connector $connector, ReplayCache $replay, Signer $signer, Settings $settings)
    {
        $this->connector = $connector;
        $this->replay    = $replay;
        $this->signer    = $signer;
        $this->settings  = $settings;
    }

    /**
     * {@inheritDoc} Identifier only; this command is NOT routed via /command/.
     */
    public function name(): string
    {
        return 'autologin';
    }

    /**
     * {@inheritDoc} The autologin flow is NEVER reached through the dispatch
     * Router (it's a browser GET, not an agent POST). Implemented to satisfy
     * the registry contract; calling it is a programming error.
     *
     * @param array<string,mixed> $claims Unused.
     * @param array<string,mixed> $params Unused.
     * @return array<string,mixed>
     */
    public function execute(array $claims, array $params): array
    {
        return ['ok' => false, 'error' => 'not_dispatchable'];
    }

    /**
     * REST callback for GET /wpmgr/v1/autologin.
     *
     * @param \WP_REST_Request<array<string,mixed>> $request Incoming request.
     * @return \WP_REST_Response|\WP_Error Either a 302 redirect (success) or
     *                                     a structured error.
     */
    public function handle(\WP_REST_Request $request)
    {
        $token = $request->get_param('token');
        $token = is_string($token) ? $token : '';
        if ($token === '') {
            return $this->fail('invalid_signature', 401, '');
        }

        // ---- Step 1: verify Ed25519 signature + bind to site/cmd. ----
        try {
            $claims = $this->connector->verifyCommand($token, 'autologin');
        } catch (\Throwable $e) {
            // Generic code; never leak the verifier's specific failure reason.
            return $this->fail('invalid_signature', 401, '');
        }

        // ---- Step 2: extract jti (Connector already enforced its presence). ----
        $jti = isset($claims['jti']) && is_string($claims['jti']) ? $claims['jti'] : '';
        if ($jti === '') {
            return $this->fail('invalid_signature', 401, '');
        }

        // ---- Step 3: local single-use shield. ----
        if ($this->replay->seen($jti)) {
            return $this->fail('replay_detected', 410, $jti);
        }

        // ---- Step 4: control-plane consume callback (authoritative target). ----
        $consume = $this->consume($jti, $request);
        if (!$consume['ok']) {
            // CP rejected the consume (already used / expired / not found / down).
            return $this->fail('consume_rejected', 410, $jti);
        }

        $targetLogin  = $consume['target_wp_user_login'];
        $allowedRoles = $consume['allowed_wp_roles'];

        // ---- Step 5: resolve a local WP user. ----
        $user = $this->resolveUser($targetLogin);
        if ($user === null) {
            return $this->fail('wp_user_not_found', 404, $jti);
        }

        // ---- Step 6: role allow-list (CP-supplied policy is authoritative). ----
        if (!$this->rolesAllowed($user, $allowedRoles)) {
            return $this->fail('role_not_allowed', 403, $jti);
        }

        // ---- Step 7: mark replay BEFORE cookie issue. ----
        if (!$this->replay->mark($jti, self::REPLAY_TTL)) {
            // Defensive self-heal: the M5.5 autologin replay table is created
            // only by the schema migration runner, and an install where that
            // table is missing (e.g. same-version re-upload that bypassed
            // register_activation_hook AND a stale db-version option) will
            // fail every mark() until the operator intervenes. Run the
            // migration once and retry the insert exactly once before giving
            // up — this turns a permanent 500 into a transient one and
            // unblocks the user on the very next click.
            Schema::ensureCurrent(true);
            // PHPStan infers mark() is always false here from the outer guard,
            // but mark() is impure (consults $wpdb/the live table) and the
            // intervening Schema::ensureCurrent(true) may have CREATEd the
            // missing table, flipping the second call's outcome.
            // @phpstan-ignore-next-line booleanNot.alwaysTrue
            if (!$this->replay->mark($jti, self::REPLAY_TTL)) {
                // If we still can't durably mark this jti, do NOT issue a
                // cookie: a race-window with a parallel presentation would
                // defeat single-use.
                return $this->fail('replay_mark_failed', 500, $jti);
            }
        }

        // ---- Step 8: issue the WP auth cookie. ----
        $this->issueAuthCookie($user);

        // ---- Step 10 (success branch): integration hook BEFORE the redirect. ----
        if (function_exists('do_action')) {
            do_action('wpmgr_autologin_success', (int) $user->ID, $jti);
        }

        // ---- Step 9: sanitize redirect_to and wp_safe_redirect. ----
        $rawRedirect = $request->get_param('redirect_to');
        $rawRedirect = is_string($rawRedirect) ? $rawRedirect : '';
        $target      = $this->sanitizeRedirect($rawRedirect);

        if (function_exists('wp_safe_redirect')) {
            wp_safe_redirect($target, 302);
        }

        // In a real WP runtime wp_safe_redirect calls exit; in tests it does
        // not, so we return a 302 response so callers can assert the target.
        return new \WP_REST_Response(null, 302, ['Location' => $target]);
    }

    /**
     * Build the signed POST to the CP consume endpoint and interpret the body.
     *
     * @param string                                 $jti     Token identifier.
     * @param \WP_REST_Request<array<string,mixed>>  $request Incoming request.
     * @return array{ok:bool,target_wp_user_login:string,allowed_wp_roles:array<int,string>,audit_id:string}
     */
    private function consume(string $jti, \WP_REST_Request $request): array
    {
        $siteId = $this->settings->siteId();
        $base   = $this->settings->controlPlaneUrl();
        if ($siteId === '' || $base === '') {
            return $this->consumeFailure();
        }

        $body = wp_json_encode([
            'nonce'            => $jti,
            'site_id'          => $siteId,
            'consumed_from_ip' => $this->clientIp($request),
        ]);
        if (!is_string($body)) {
            return $this->consumeFailure();
        }

        try {
            $authHeaders = $this->signer->signHeaders('POST', self::PATH_CONSUME, $body);
        } catch (\Throwable $e) {
            return $this->consumeFailure();
        }

        $headers = array_merge(
            ['Content-Type' => 'application/json', 'Accept' => 'application/json'],
            $authHeaders
        );

        $response = wp_remote_post(
            $base . self::PATH_CONSUME,
            [
                'timeout' => self::CONSUME_TIMEOUT,
                'headers' => $headers,
                'body'    => $body,
            ]
        );

        if (function_exists('is_wp_error') && is_wp_error($response)) {
            return $this->consumeFailure();
        }

        $status = (int) wp_remote_retrieve_response_code($response);
        if ($status !== 200) {
            // 404/410/5xx: do NOT leak the CP body to the caller.
            return $this->consumeFailure();
        }

        $raw    = (string) wp_remote_retrieve_body($response);
        $parsed = json_decode($raw, true);
        if (!is_array($parsed) || !isset($parsed['ok']) || $parsed['ok'] !== true) {
            return $this->consumeFailure();
        }

        $target = isset($parsed['target_wp_user_login']) && is_string($parsed['target_wp_user_login'])
            ? $parsed['target_wp_user_login']
            : '';
        $roles  = isset($parsed['allowed_wp_roles']) && is_array($parsed['allowed_wp_roles'])
            ? array_values(array_filter($parsed['allowed_wp_roles'], 'is_string'))
            : [];
        $audit  = isset($parsed['audit_id']) && is_string($parsed['audit_id'])
            ? $parsed['audit_id']
            : '';

        return [
            'ok'                   => true,
            'target_wp_user_login' => $target,
            'allowed_wp_roles'     => $roles,
            'audit_id'             => $audit,
        ];
    }

    /**
     * Uniform "consume failed" tuple.
     *
     * @return array{ok:bool,target_wp_user_login:string,allowed_wp_roles:array<int,string>,audit_id:string}
     */
    private function consumeFailure(): array
    {
        return [
            'ok'                   => false,
            'target_wp_user_login' => '',
            'allowed_wp_roles'     => [],
            'audit_id'             => '',
        ];
    }

    /**
     * Resolve the local WP user the cookie should be minted for. If the CP told
     * us "pick", we fall back to the lowest-ID administrator.
     *
     * @param string $targetLogin CP-supplied login (empty => agent picks).
     * @return \WP_User|null
     */
    private function resolveUser(string $targetLogin): ?\WP_User
    {
        if ($targetLogin !== '' && function_exists('get_user_by')) {
            $user = get_user_by('login', $targetLogin);
            if ($user instanceof \WP_User) {
                return $user;
            }
            return null;
        }

        if (!function_exists('get_users')) {
            return null;
        }

        $admins = get_users([
            'role'    => 'administrator',
            'number'  => 1,
            'orderby' => 'ID',
            'order'   => 'ASC',
        ]);
        if (!is_array($admins) || $admins === []) {
            return null;
        }

        $first = $admins[0];
        if (!($first instanceof \WP_User)) {
            return null;
        }

        return $first;
    }

    /**
     * Whether the user holds at least one role in the CP-supplied allow-list.
     *
     * @param \WP_User              $user     Resolved user.
     * @param array<int,string>     $allowed  CP-supplied role names.
     * @return bool
     */
    private function rolesAllowed(\WP_User $user, array $allowed): bool
    {
        if ($allowed === []) {
            return false;
        }

        $roles = property_exists($user, 'roles') && is_array($user->roles) ? $user->roles : [];
        /** @var array<int|string,mixed> $roles */
        $userRoles = array_values(array_filter($roles, 'is_string'));
        if ($userRoles === []) {
            return false;
        }

        return array_intersect($userRoles, $allowed) !== [];
    }

    /**
     * Clear any existing auth, set the new auth cookie, and fire wp_login.
     *
     * @param \WP_User $user Resolved user.
     * @return void
     */
    private function issueAuthCookie(\WP_User $user): void
    {
        $userId    = (int) $user->ID;
        $userLogin = is_string($user->user_login) ? $user->user_login : '';

        if (function_exists('wp_clear_auth_cookie')) {
            wp_clear_auth_cookie();
        }
        if (function_exists('wp_set_current_user')) {
            wp_set_current_user($userId, $userLogin);
        }
        if (function_exists('wp_set_auth_cookie')) {
            // Session cookie (remember=false). Secure flag follows is_ssl().
            wp_set_auth_cookie($userId, false, function_exists('is_ssl') ? is_ssl() : false);
        }
        if (function_exists('do_action')) {
            do_action('wp_login', $userLogin, $user);
        }
    }

    /**
     * Sanitize a redirect_to value. Returns either a same-origin path
     * (beginning with `/`) made up only of safe characters, or admin_url() if
     * the input is empty/invalid.
     *
     * The accepted shape is a deliberately small subset:
     *   ^/[A-Za-z0-9._~/-]*(\?[A-Za-z0-9._~/\-&=%]*)?(#[A-Za-z0-9._~/\-]*)?$
     *
     * This excludes:
     *   - protocol-relative URLs (//evil.com)
     *   - absolute URLs (https://...)
     *   - backslash variants (\\evil.com, /\evil.com)
     *   - control chars + whitespace + newlines (CR/LF)
     *   - javascript:/data: schemes
     *
     * We pair the regex with wp_safe_redirect() (which validates host against
     * the allowed-redirect-hosts list) for defense-in-depth.
     *
     * @param string $raw User-supplied redirect_to.
     * @return string A safe target URL (absolute) to pass to wp_safe_redirect.
     */
    private function sanitizeRedirect(string $raw): string
    {
        $fallback = function_exists('admin_url') ? (string) admin_url() : '/wp-admin/';

        if ($raw === '') {
            return $fallback;
        }

        // Reject anything carrying control or whitespace characters (CR/LF
        // injection guard) BEFORE pattern matching.
        if (preg_match('/[\x00-\x1F\x7F\s]/', $raw) === 1) {
            return $fallback;
        }

        // Strict same-origin path shape. Must begin with a single `/` and the
        // next char (if any) must NOT be `/` or `\` (protocol-relative guard).
        if (!preg_match('#^/(?![/\\\\])[A-Za-z0-9._~/\-]*(\?[A-Za-z0-9._~/\-&=%]*)?(\#[A-Za-z0-9._~/\-]*)?$#', $raw)) {
            return $fallback;
        }

        // Compose against home_url() so wp_safe_redirect doesn't view us as
        // off-host; if home_url isn't available, return the path as-is and let
        // wp_safe_redirect's allow-list be the final arbiter.
        if (function_exists('home_url')) {
            return (string) home_url($raw);
        }

        return $raw;
    }

    /**
     * Best-effort client IP. Used only as a CP-side audit hint; the CP is the
     * final authority on whether the consume is allowed.
     *
     * @param \WP_REST_Request<array<string,mixed>> $request Incoming request.
     * @return string
     */
    private function clientIp(\WP_REST_Request $request): string
    {
        // Prefer REMOTE_ADDR from the underlying SAPI; never trust X-Forwarded-For
        // unless an operator has configured a trusted-proxy chain (out of scope).
        $remote = isset($_SERVER['REMOTE_ADDR']) && is_string($_SERVER['REMOTE_ADDR'])
            ? $_SERVER['REMOTE_ADDR']
            : '';

        // Validate as an IP; reject anything else (including spoofed garbage).
        if ($remote !== '' && filter_var($remote, FILTER_VALIDATE_IP) !== false) {
            return $remote;
        }

        return '';
    }

    /**
     * Build a uniform failure response and fire the failure hook.
     *
     * @param string $code   Short, generic machine code.
     * @param int    $status HTTP status.
     * @param string $jti    Token identifier (empty before extraction).
     * @return \WP_Error
     */
    private function fail(string $code, int $status, string $jti): \WP_Error
    {
        if (function_exists('do_action')) {
            do_action('wpmgr_autologin_failure', $code, $jti);
        }

        return new \WP_Error('wpmgr_' . $code, $code, ['status' => $status]);
    }
}
