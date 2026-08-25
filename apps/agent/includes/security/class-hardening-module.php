<?php
/**
 * HardeningModule — applies the WPMgr security hardening config to WordPress
 * by binding the correct hooks for each enabled toggle.
 *
 * Design principles:
 *   - Default OFF: every hook is only registered when its toggle is on.
 *   - Idempotent: install() is guarded by a static flag; safe to call on every
 *     boot (plugins_loaded).
 *   - Non-breaking: no hook fatals the request; force_ssl and REST restrict
 *     always exempt the agent's own REST routes (/wpmgr/v1/...).
 *   - Server-config writes delegate entirely to ServerConfigWriter; this class
 *     owns only the WP-PHP layer.
 *
 * @package WPMgr\Agent\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Security;

use WPMgr\Agent\Cache\WpConfigEditor;

/**
 * WordPress-hooks enforcer for the hardening config.
 */
final class HardeningModule
{
    /**
     * wp-options key for the stored hardening config (JSON-encoded HardeningConfig::toArray()).
     */
    public const OPTION_CONFIG = 'wpmgr_hardening_config';

    /** REST namespace the agent owns — never restricted. */
    private const AGENT_REST_NAMESPACE = 'wpmgr/v1';

    /** Autologin route path — never restricted. */
    private const AGENT_AUTOLOGIN_PATH = '/autologin';

    /**
     * Informational note from the most recent {@see syncWpConfigFileEdit()}
     * call, captured from {@see WpConfigEditor::lastNotice()}. Null when the
     * wp-config write succeeded/was a plain idempotent re-apply, the toggle
     * was off, or no notice applies. This is NEVER a failure signal — see
     * {@see lastWpConfigNotice()}.
     */
    private ?string $lastWpConfigNotice = null;

    /**
     * Persist a new config and install/refresh the server-config block.
     *
     * Called by SyncSecurityHardeningCommand::execute(). Returns true when
     * persistence succeeded.
     *
     * @param HardeningConfig $config Validated config object.
     * @return bool
     */
    public function applyConfig(HardeningConfig $config): bool
    {
        if (!function_exists('update_option')) {
            return false;
        }

        $encoded = wp_json_encode($config->toArray());
        if ($encoded === false) {
            return false;
        }

        update_option(self::OPTION_CONFIG, $encoded, false);

        // Refresh the server-config block immediately on sync.
        $writer = new ServerConfigWriter();
        if (!$writer->isNginx()) {
            $hasAnyServerRule = $config->forceSsl
                || $config->disableDirectoryBrowsing
                || $config->disablePhpInUploads
                || $config->protectSystemFiles
                || $config->xmlrpcMode === HardeningConfig::XMLRPC_OFF
                || $config->ipRangeBans() !== []
                || $config->userAgentBans() !== [];

            if ($hasAnyServerRule) {
                $writer->install($config);
            } else {
                // All toggles off — remove any prior block cleanly.
                $writer->uninstall();
            }
        }

        return true;
    }

    /**
     * Register WP hooks for every enabled toggle. Call once on plugins_loaded.
     * Safe to call on every boot: a static guard makes it idempotent.
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

        $config = HardeningConfig::load();

        $this->applyDisableFileEditor($config);
        $this->applyXmlrpc($config);
        $this->applyRestRestrict($config);
        $this->applyLoginIdentifier($config);
        $this->applyForceUniqueNickname($config);
        $this->applyAuthorArchiveEnum($config);
        $this->applyForceSsl($config);
        $this->applyBanFilters($config);
    }

    // -------------------------------------------------------------------------
    // Per-toggle appliers (all no-ops when the toggle is off)
    // -------------------------------------------------------------------------

    /**
     * disable_file_editor: write DISALLOW_FILE_EDIT to wp-config. The runtime
     * filter is also bound as a defence-in-depth fallback for sites where the
     * wp-config write fails (e.g. immutable filesystem).
     *
     * @param HardeningConfig $config
     * @return void
     */
    private function applyDisableFileEditor(HardeningConfig $config): void
    {
        if (!$config->disableFileEditor) {
            return;
        }

        // Runtime filter (defence-in-depth / fallback).
        add_filter('user_has_cap', static function (array $caps, array $cap): array {
            if (in_array('edit_themes', $cap, true) || in_array('edit_plugins', $cap, true)) {
                $caps['edit_themes']  = false;
                $caps['edit_plugins'] = false;
            }
            return $caps;
        }, 10, 2);
    }

    /**
     * Enable wp-config write for DISALLOW_FILE_EDIT. Called by the command
     * handler after persistence so the define lands in wp-config immediately.
     * Also removes the define when the toggle is turned off.
     *
     * GH #268: on platforms such as Roots/Bedrock, DISALLOW_FILE_EDIT is
     * already managed elsewhere by the time this runs (see
     * {@see WpConfigEditor::setConstant()}); the write is safely skipped and
     * this still returns true (intent satisfied) rather than fatal. Any
     * informational (non-error) explanation of that skip is available via
     * {@see lastWpConfigNotice()} afterwards.
     *
     * @param HardeningConfig $config
     * @return bool
     */
    public function syncWpConfigFileEdit(HardeningConfig $config): bool
    {
        $this->lastWpConfigNotice = null;

        $editor = new WpConfigEditor();
        if (!$editor->isWritable()) {
            // Non-writable wp-config: runtime filter (registered by install()) is
            // the fallback. Signal partial success to the caller.
            return false;
        }

        if ($config->disableFileEditor) {
            $result = $editor->setConstant('DISALLOW_FILE_EDIT', true);
            $this->lastWpConfigNotice = $editor->lastNotice();
            return $result;
        } else {
            return $editor->removeConstant('DISALLOW_FILE_EDIT');
        }
    }

    /**
     * Informational note from the most recent {@see syncWpConfigFileEdit()}
     * call (e.g. "DISALLOW_FILE_EDIT is already enforced ... left untouched"
     * on a Roots/Bedrock site), or null when there is none. Callers may
     * surface this as an informational detail — it is never an error.
     *
     * @return string|null
     */
    public function lastWpConfigNotice(): ?string
    {
        return $this->lastWpConfigNotice;
    }

    /**
     * xmlrpc_mode: off => add_filter('xmlrpc_enabled','__return_false');
     *              limited => disable only pingback methods;
     *              on => no-op.
     *
     * @param HardeningConfig $config
     * @return void
     */
    private function applyXmlrpc(HardeningConfig $config): void
    {
        if ($config->xmlrpcMode === HardeningConfig::XMLRPC_OFF) {
            add_filter('xmlrpc_enabled', '__return_false');
            return;
        }

        if ($config->xmlrpcMode === HardeningConfig::XMLRPC_LIMITED) {
            // Disable multicall amplification and pingback methods only.
            add_filter(
                'xmlrpc_methods',
                static function (array $methods): array {
                    unset(
                        $methods['system.multicall'],
                        $methods['pingback.ping'],
                        $methods['pingback.extensions.getPingbacks']
                    );
                    return $methods;
                }
            );
        }
    }

    /**
     * restrict_rest_api: restricted => require auth for anon REST requests,
     * excluding the agent's own namespace and a fixed allowlist of safe routes.
     * default => no-op.
     *
     * @param HardeningConfig $config
     * @return void
     */
    private function applyRestRestrict(HardeningConfig $config): void
    {
        if ($config->restrictRestApi !== HardeningConfig::REST_RESTRICTED) {
            return;
        }

        add_filter(
            'rest_authentication_errors',
            static function ($result) {
                // Already handled by another filter or already authenticated.
                if ($result !== null) {
                    return $result;
                }

                // Authenticated users (cookies, application passwords, etc.) pass.
                if (is_user_logged_in()) {
                    return null;
                }

                // Allowlist: oembed (needed for embeds), the agent's own routes.
                // We read the current route from the REST server global.
                $route = '';
                if (isset($GLOBALS['wp']->query_vars['rest_route'])
                    && is_string($GLOBALS['wp']->query_vars['rest_route'])
                ) {
                    $route = $GLOBALS['wp']->query_vars['rest_route'];
                }

                // Agent namespace always passes.
                $agentPrefix = '/' . self::AGENT_REST_NAMESPACE . '/';
                if (str_starts_with($route, $agentPrefix)
                    || $route === '/' . self::AGENT_REST_NAMESPACE
                ) {
                    return null;
                }

                // oembed consumer route.
                if (str_starts_with($route, '/oembed/1.0/')) {
                    return null;
                }

                return new \WP_Error(
                    'rest_not_logged_in',
                    'REST API access requires authentication.',
                    ['status' => 401]
                );
            }
        );
    }

    /**
     * restrict_login_identifier: disable the WordPress core email or username
     * authentication filter so only the allowed identifier type works.
     *
     * WP core registers two filters on 'authenticate':
     *   - wp_authenticate_username_password (priority 20) — username login
     *   - wp_authenticate_email_password    (priority 20) — email login
     *
     * We remove whichever one the operator wants to disable.
     *
     * @param HardeningConfig $config
     * @return void
     */
    private function applyLoginIdentifier(HardeningConfig $config): void
    {
        if ($config->restrictLoginIdentifier === HardeningConfig::LOGIN_BOTH) {
            return;
        }

        // We must hook after plugins_loaded so the default filters exist.
        add_action('init', static function () use ($config): void {
            if ($config->restrictLoginIdentifier === HardeningConfig::LOGIN_USERNAME) {
                // Allow only username login — remove email auth.
                remove_filter('authenticate', 'wp_authenticate_email_password', 20);
            } elseif ($config->restrictLoginIdentifier === HardeningConfig::LOGIN_EMAIL) {
                // Allow only email login — remove username auth.
                remove_filter('authenticate', 'wp_authenticate_username_password', 20);
            }
        });
    }

    /**
     * force_unique_nickname: prevent saving a display name identical to the
     * user's login name (username harvesting via author archives).
     *
     * @param HardeningConfig $config
     * @return void
     */
    private function applyForceUniqueNickname(HardeningConfig $config): void
    {
        if (!$config->forceUniqueNickname) {
            return;
        }

        // $user is typed mixed, not \WP_User: core builds a bare stdClass in
        // edit_user() and passes it by reference, so a \WP_User hint here is a
        // TypeError at argument binding — a fatal on every user-edit screen for
        // any site with this single toggle on, password policy or not.
        // ProfileUpdateUser::resolve() also recovers user_login from the stored
        // user, so an object without it cannot silently switch the check off.
        add_action('user_profile_update_errors', static function (\WP_Error $errors, bool $update, mixed $user): void {
            if (!$update) {
                return;
            }
            $nickname = isset($_POST['nickname']) && is_string($_POST['nickname']) // phpcs:ignore WordPress.Security.NonceVerification.Missing,WordPress.Security.ValidatedSanitizedInput.InputNotSanitized,WordPress.Security.ValidatedSanitizedInput.MissingUnslash -- nonce verified by WP core's profile-update handler before this hook fires; sanitized on the next line
                ? sanitize_text_field(wp_unslash($_POST['nickname'])) // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same as above
                : '';
            $userLogin = ProfileUpdateUser::resolve($user)->user_login;
            if ($nickname !== '' && $userLogin !== '' && $nickname === $userLogin) {
                $errors->add(
                    'wpmgr_nickname_conflict',
                    esc_html__('Your display name must not match your login username.', 'wpmgr-agent')
                );
            }
        }, 10, 3);
    }

    /**
     * disable_author_archive_enum: 404 ?author=N probe redirects, hide user list
     * from anonymous REST requests (/wp/v2/users).
     *
     * @param HardeningConfig $config
     * @return void
     */
    private function applyAuthorArchiveEnum(HardeningConfig $config): void
    {
        if (!$config->disableAuthorArchiveEnum) {
            return;
        }

        // Block ?author=N redirect (redirects to /author/username/, leaking names).
        add_action('template_redirect', static function (): void {
            $authorQuery = isset($_GET['author']) ? sanitize_text_field(wp_unslash($_GET['author'])) : ''; // phpcs:ignore WordPress.Security.NonceVerification.Recommended -- no state change; reading query var for enumeration protection check
            if ($authorQuery !== '' && !is_user_logged_in()) {
                global $wp_query;
                if (isset($wp_query) && $wp_query instanceof \WP_Query && $wp_query->is_author()) {
                    $wp_query->set_404();
                    status_header(404);
                    nocache_headers();
                }
            }
        });

        // Hide user list from anon REST /wp/v2/users.
        add_filter(
            'rest_endpoints',
            static function (array $endpoints): array {
                if (is_user_logged_in()) {
                    return $endpoints;
                }
                $usersRoute = '/wp/v2/users';
                if (isset($endpoints[$usersRoute])) {
                    unset($endpoints[$usersRoute]);
                }
                $meRoute = '/wp/v2/users/me';
                if (isset($endpoints[$meRoute])) {
                    unset($endpoints[$meRoute]);
                }
                return $endpoints;
            }
        );
    }

    /**
     * force_ssl: redirect http -> https and send HSTS at the PHP layer (for
     * non-Apache or when server-config write failed).
     *
     * SAFETY: never redirects requests that arrive on the agent's own REST route
     * on port 443 (already https), and never redirects WP-Cron or CLI.
     *
     * @param HardeningConfig $config
     * @return void
     */
    private function applyForceSsl(HardeningConfig $config): void
    {
        if (!$config->forceSsl) {
            return;
        }

        // Also set FORCE_SSL_ADMIN so wp-admin is covered.
        if (!defined('FORCE_SSL_ADMIN')) {
            define('FORCE_SSL_ADMIN', true);
        }

        add_action('template_redirect', static function (): void {
            if (is_ssl()) {
                return;
            }
            if (defined('DOING_CRON') && DOING_CRON) {
                return;
            }
            if (php_sapi_name() === 'cli') {
                return;
            }
            if (isset($_SERVER['HTTP_HOST'], $_SERVER['REQUEST_URI'])
                && is_string($_SERVER['HTTP_HOST'])
                && is_string($_SERVER['REQUEST_URI'])
            ) {
                $host = sanitize_text_field(wp_unslash($_SERVER['HTTP_HOST'])); // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- sanitized via sanitize_text_field(wp_unslash())
                $uri  = sanitize_text_field(wp_unslash($_SERVER['REQUEST_URI'])); // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- sanitized via sanitize_text_field(wp_unslash())
                wp_safe_redirect('https://' . $host . $uri, 301);
                exit;
            }
        }, 1);

        // HSTS header on every HTTPS response.
        add_action('send_headers', static function (): void {
            if (!is_ssl()) {
                return;
            }
            if (!headers_sent()) {
                header('Strict-Transport-Security: max-age=31536000; includeSubDomains');
            }
        });
    }

    /**
     * Bind a PHP fallback for user-agent bans. The server-config (Apache) is the
     * primary enforcement point; this fires for any request that reaches PHP
     * (e.g. nginx sites or when .htaccess write failed).
     *
     * IP/range bans are fed into the existing WAF mu-plugin's deny_cidrs via the
     * stored wpmgr_security_config option's deny_cidrs key — see syncWafDenyCidrs().
     *
     * @param HardeningConfig $config
     * @return void
     */
    private function applyBanFilters(HardeningConfig $config): void
    {
        $uaBans = $config->userAgentBans();
        if ($uaBans === []) {
            return;
        }

        // PHP-layer UA ban: fires before most output is generated (priority 1 on init).
        add_action('init', static function () use ($uaBans): void {
            if (!isset($_SERVER['HTTP_USER_AGENT']) || !is_string($_SERVER['HTTP_USER_AGENT'])) {
                return;
            }
            $ua = sanitize_text_field(wp_unslash($_SERVER['HTTP_USER_AGENT'])); // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- sanitized via sanitize_text_field(wp_unslash())
            foreach ($uaBans as $pattern) {
                if ($pattern !== '' && stripos($ua, $pattern) !== false) {
                    if (!headers_sent()) {
                        http_response_code(403);
                        header('Content-Type: text/plain; charset=utf-8');
                        header('Cache-Control: no-cache, no-store, must-revalidate');
                    }
                    exit('Access denied.');
                }
            }
        }, 1);
    }

    /**
     * Feed IP/range bans from the hardening config into the WAF mu-plugin's
     * dedicated 'hardening_deny_cidrs' key in wpmgr_security_config.
     *
     * The WAF mu-plugin reads wpmgr_security_config at boot time (before WP loads)
     * and evaluates 'hardening_deny_cidrs' in ALL modes, independent of the
     * login-protection 'mode' field. This is the ITEM 5 fix: explicit operator
     * bans must always block, not only when brute-force protect mode is on.
     *
     * Keeping hardening bans in their own key ('hardening_deny_cidrs') instead of
     * merging into 'deny_cidrs' keeps the two enforcement layers cleanly separated
     * and makes removal on ban-list change trivial (overwrite the key, no diff needed).
     *
     * SAFETY: the WAF mu-plugin's allow_cidrs guard and private/loopback bypass
     * both apply before the hardening_deny_cidrs check. No lock-out is possible
     * for private IPs or allow-listed control-plane egress addresses.
     *
     * This is called by SyncSecurityHardeningCommand after persistence.
     *
     * @param HardeningConfig $config
     * @return void
     */
    public function syncWafDenyCidrs(HardeningConfig $config): void
    {
        if (!function_exists('get_option') || !function_exists('update_option')) {
            return;
        }

        $raw = get_option('wpmgr_security_config', '');
        if (!is_string($raw) || $raw === '') {
            // No WAF config yet — nothing to sync into.
            return;
        }

        $wafConfig = json_decode($raw, true);
        if (!is_array($wafConfig)) {
            return;
        }

        // Write the hardening ban CIDRs into their own dedicated key so the WAF
        // can evaluate them mode-independently (ITEM 5). The 'deny_cidrs' key
        // remains owned solely by the login-protection / brute-force subsystem.
        //
        // Defence-in-depth: filter out broad/private CIDRs before persisting.
        // The runtime WAF already bypasses private IPs, but the agent must not
        // store a dangerous CIDR that it received from the CP (belt-and-braces).
        // WafCidrGuard applies the same rules used by ServerConfigWriter at render
        // time, ensuring a single source of truth for what counts as "unsafe".
        $allowCidrs = isset($wafConfig['allow_cidrs']) && is_array($wafConfig['allow_cidrs'])
            ? array_values(array_filter($wafConfig['allow_cidrs'], 'is_string'))
            : [];
        $rawBans           = $config->ipRangeBans();
        $newHardeningCidrs = [];
        foreach ($rawBans as $cidr) {
            $cidr = trim((string) $cidr);
            if (!WafCidrGuard::isUnsafe($cidr, $allowCidrs)) {
                $newHardeningCidrs[] = $cidr;
            }
        }
        $wafConfig['hardening_deny_cidrs'] = array_values($newHardeningCidrs);

        $encoded = wp_json_encode($wafConfig);
        if ($encoded !== false) {
            update_option('wpmgr_security_config', $encoded, false);
        }
    }
}
