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

    /**
     * wp-options key holding the agent version whose ServerConfigWriter output
     * is currently on disk. Autoloaded: {@see refreshServerConfigIfStale()}
     * reads it on every boot, so it must not cost a query.
     */
    public const OPTION_SERVER_REV = 'wpmgr_hardening_server_rev';

    /**
     * REST namespace the agent owns — never restricted.
     *
     * Public because ServerConfigWriter renders the same route into the
     * .htaccess recovery exemption. One definition, two enforcement layers.
     */
    public const AGENT_REST_NAMESPACE = 'wpmgr/v1';

    /** Autologin route path — never restricted. */
    public const AGENT_AUTOLOGIN_PATH = '/autologin';

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

        // Refresh the server-config block immediately on sync. The stamp
        // follows the write here too, so a sync that could not reach .htaccess
        // leaves the boot-time repair armed rather than marking it done.
        if ($this->writeServerConfig($config)) {
            $this->stampServerRev();
        }

        return true;
    }

    /**
     * Render the server-config block for $config, or remove it when no toggle
     * needs one. No-op on nginx (no .htaccess auto-write).
     *
     * Extracted from applyConfig() so the upgrade path can re-render an already
     * persisted config without going through a sync — see
     * {@see refreshServerConfigIfStale()}.
     *
     * Returns whether the block on disk is now current. nginx returns true:
     * there is no .htaccess to write, so there is nothing left undone. A false
     * return means the file still holds whatever it held before, which is what
     * the version stamp must not paper over.
     *
     * @param HardeningConfig $config
     * @return bool
     */
    private function writeServerConfig(HardeningConfig $config): bool
    {
        $writer = new ServerConfigWriter();
        if ($writer->isNginx()) {
            return true;
        }

        $hasAnyServerRule = $config->forceSsl
            || $config->disableDirectoryBrowsing
            || $config->disablePhpInUploads
            || $config->protectSystemFiles
            || $config->xmlrpcMode === HardeningConfig::XMLRPC_OFF
            || $config->ipRangeBans() !== []
            || $config->userAgentBans() !== [];

        if ($hasAnyServerRule) {
            return $writer->install($config);
        }

        // All toggles off — remove any prior block cleanly.
        return $writer->uninstall();
    }

    /**
     * Re-render the server-config block when the block on disk was written by a
     * different agent version (GH #529).
     *
     * THE BUG THIS EXISTS FOR. Every path that wrote .htaccess ran from
     * SyncSecurityHardeningCommand, so a site whose stored config already held a
     * generic user-agent ban — `Chrome` — kept 403ing every request, including
     * wp-login.php, after the upgrade that fixed the bug. HardeningConfig::load()
     * now drops that pattern at the config boundary and applyBanFilters() now
     * exempts the recovery surfaces, but neither touches the rule already
     * rendered into the file, and Apache reads the file, not the option. The
     * fix arrived and changed nothing for exactly the sites that needed it.
     *
     * The re-render is what repairs them, and it deliberately does NOT depend on
     * the control plane reaching the site: a locked-out site may have no working
     * inbound request at all, so waiting for the next sync is waiting for a
     * message that a 403 is eating. This runs on plugins_loaded, from the site's
     * own boot, on the first request after the files change.
     *
     * THE STAMP FOLLOWS THE WRITE, NEVER LEADS IT. Stamping unconditionally
     * looks like it avoids an I/O storm on a read-only filesystem, and it does
     * — by permanently abandoning the repair. A .htaccess write fails for
     * transient reasons too (a full disk, a deploy that flips the tree
     * read-only for a minute, a lock held by another process), and one unlucky
     * boot would then strand the site on the stale generic rule until the next
     * version bump or the next sync — the sync being exactly the inbound
     * request the stale 403 is eating. So the stamp is written only when the
     * block on disk is actually current, and an unrepaired site retries on the
     * next boot.
     *
     * COST. One autoloaded option read per request. On a permanently read-only
     * site the retry costs one file read and one writability probe per boot and
     * never reaches a write: ServerConfigWriter::install() compares the rendered
     * block first and returns early when it is already current, then refuses
     * before writing when the path is not writable. That is the correct price
     * for not giving up on a site that could recover.
     *
     * @param HardeningConfig $config The config just loaded by install().
     * @return void
     */
    private function refreshServerConfigIfStale(HardeningConfig $config): void
    {
        if (!function_exists('get_option') || !function_exists('update_option')) {
            return;
        }
        if (!defined('WPMGR_AGENT_VERSION')) {
            return;
        }
        $version = (string) constant('WPMGR_AGENT_VERSION');
        if ($version === '') {
            return;
        }

        $stored = get_option(self::OPTION_SERVER_REV, '');
        if (is_string($stored) && $stored === $version) {
            return;
        }

        if ($this->writeServerConfig($config)) {
            $this->stampServerRev();
        }
    }

    /**
     * Record the agent version that produced the server-config block now on
     * disk. Autoloaded so the boot-time staleness check costs no query.
     *
     * @return void
     */
    private function stampServerRev(): void
    {
        if (!function_exists('update_option') || !defined('WPMGR_AGENT_VERSION')) {
            return;
        }
        update_option(self::OPTION_SERVER_REV, (string) constant('WPMGR_AGENT_VERSION'), true);
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

        // GH #529: repair a server-config block left behind by an older version.
        // Must come after the hook binds, never instead of it — the PHP layer is
        // the fallback when the .htaccess write cannot happen.
        $this->refreshServerConfigIfStale($config);
    }

    /**
     * Whether the operator's recovery constant is set.
     *
     * `define('WPMGR_DISABLE_SITE_2FA', true)` is the documented escape hatch
     * for an admin locked out by this plugin's auth policy. It was honoured by
     * Site2faModule and PasswordPolicyModule but NOT here, so an operator who
     * set it still hit HardeningModule's auth rules — an escape hatch that does
     * not release everything it claims to is worse than none, because the
     * person using it believes they are safe.
     *
     * DELIBERATELY SCOPED TO AUTH POLICY. This gates the two appliers that can
     * keep an administrator out of their own site: applyLoginIdentifier()
     * (which identifier may be used to log in) and applyForceUniqueNickname()
     * (which can refuse a profile save). It does NOT gate the file-editor
     * block, xmlrpc mode, REST restriction, force-SSL, author-archive
     * enumeration or the IP/user-agent bans.
     *
     * That exclusion is NOT because none of those six can lock anyone out —
     * at least one demonstrably could. applyBanFilters() registered on `init`
     * priority 1 with no gate at all; `init` fires on wp-login.php; the match
     * is an unbounded case-insensitive substring
     * (stripos($ua, $pattern) !== false) with, at the time, no length or
     * genericity check; and a match ends in exit('Access denied.'). A ban
     * pattern of "Mozilla", "Chrome", "Safari", "AppleWebKit" or "Gecko" 403'd
     * the administrator's own login page along with everyone else's.
     *
     * GH #529 fixed that lockout WITHOUT widening this constant's reach, which
     * remains the right call: silently dropping a site's file-editor lock,
     * xmlrpc mode, REST restriction, forced SSL, author-archive block or
     * IP/user-agent bans the moment someone sets a recovery constant would turn
     * a recovery step into a worse regression than the lockout it is meant to
     * fix. Instead the ban callback exempts exactly the two surfaces an admin
     * needs to get back in ({@see isRecoverySurface()}), and a pattern generic
     * enough to cause the lockout is now refused at the config boundary by
     * HardeningConfig::userAgentPatternRejection(). The ban itself still bans.
     *
     * The scoping of this constant to just the two auth appliers is therefore
     * correct; only the "cannot lock anyone out" justification for it was.
     *
     * @return bool
     */
    private static function authPolicyDisabled(): bool
    {
        return defined('WPMGR_DISABLE_SITE_2FA') && WPMGR_DISABLE_SITE_2FA;
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
        if (self::authPolicyDisabled()) {
            return;
        }
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
        if (self::authPolicyDisabled()) {
            return;
        }
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
     * GH #529: the recovery surfaces are exempt — see {@see isRecoverySurface()}.
     * A user-agent ban is for traffic; it is not for the door the owner uses to
     * get back in.
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
            // GH #529: never 403 the login screen or the autologin route.
            if (self::isRecoverySurface()) {
                return;
            }
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
     * True when this request is one of the two surfaces an administrator uses
     * to regain access to their own site (GH #529).
     *
     * Scope, deliberately narrow — exactly two surfaces:
     *
     *  1. The login screen. WordPress core sets $pagenow from PHP_SELF in
     *     wp-includes/vars.php, which wp-settings.php requires (line 553 of the
     *     7.0.4 tree) long before it fires `init` (line 771), so the global is
     *     populated and trustworthy by the time the ban callback runs. It is
     *     derived from the resolved script path, NOT from anything the client
     *     sends, so a request cannot claim to be the login page. HideBackendModule
     *     assigns the same value at `setup_theme` (line 697, still before init)
     *     when it serves the secret slug, so a hidden login screen is covered by
     *     the identical check with no second code path.
     *
     *  2. The agent's own autologin route, /wpmgr/v1/autologin. At `init`
     *     priority 1 the REST request has not been routed yet — $wp->query_vars
     *     ['rest_route'] is populated later, during parse_request — so the raw
     *     request path is the only signal available, and it is matched against
     *     the site's OWN canonical autologin path, in full, from ^ to $.
     *
     * WHY A SUFFIX MATCH WAS NOT ENOUGH. str_ends_with($path, '/wp-json/…')
     * anchors the tail and leaves the head unconstrained, so every one of
     * '/anything/wp-json/wpmgr/v1/autologin',
     * '/xmlrpc.php/wp-json/wpmgr/v1/autologin' and
     * '//wp-json/wpmgr/v1/autologin' collected the exemption. The xmlrpc one is
     * the sharp edge: under Apache mod_php and the standard nginx
     * fastcgi_split_path_info, xmlrpc.php executes normally with the trailing
     * segments delivered as PATH_INFO, so the ban was skipped on a live
     * endpoint — system.multicall included. Three checks close it:
     *
     *   a. The request must have been served by the front controller.
     *      SCRIPT_NAME is the resolved script, not client input, exactly like
     *      the PHP_SELF that core derives $pagenow from. basename() !=
     *      'index.php' means some other PHP file is answering, and no other PHP
     *      file is the autologin route.
     *   b. The path must EQUAL the site's own autologin path — home path,
     *      REST prefix and route, no prefix and no suffix. rest_get_url_prefix()
     *      is read rather than assuming 'wp-json', so a site that filtered the
     *      prefix keeps its exemption instead of silently losing it.
     *   c. sanitize_text_field() must not have altered the path. It DELETES
     *      percent-encoded octets rather than decoding them
     *      (wp-includes/formatting.php, _sanitize_text_fields()), which is how
     *      '/wp-admin/options.php%3f/wp-json/wpmgr/v1/autologin' collapsed into
     *      a matching string and how '…/autologin%00' did. Comparing the
     *      sanitized path against the raw one and refusing on any difference
     *      fails closed on the whole class instead of naming its members.
     *      A real autologin URL contains no percent-encoding, so nothing
     *      legitimate is refused.
     *
     * The plain-permalink form gets (a) and (c) too, plus the requirement that
     * the path be the site root — '/wp-admin/?rest_route=/wpmgr/v1/autologin'
     * is not the autologin route and no longer claims to be.
     *
     * NOT a general "logged-in users are exempt" rule, and not a blanket gate on
     * the recovery constant. Both would be wider than the problem: the first
     * would hand any compromised subscriber account a free pass on every page,
     * and the second would drop a site's whole ban list the moment someone
     * recovers a login. This exempts two doors and nothing else.
     *
     * Costs the ban almost nothing. A user-agent is a client-supplied string
     * that any attacker rewrites in one line, so a UA ban was never the control
     * holding wp-login.php: that is the login-protection subsystem's rate limit
     * and its IP deny_cidrs in the WAF mu-plugin, both untouched here and both
     * still fully active on this page.
     *
     * @return bool
     */
    private static function isRecoverySurface(): bool
    {
        // (1) The login screen — core's own resolved-script signal.
        if (isset($GLOBALS['pagenow'])
            && is_string($GLOBALS['pagenow'])
            && $GLOBALS['pagenow'] === 'wp-login.php'
        ) {
            return true;
        }

        // (2) The agent's autologin route.
        if (!isset($_SERVER['REQUEST_URI']) || !is_string($_SERVER['REQUEST_URI'])) {
            return false;
        }
        $rawUri = wp_unslash($_SERVER['REQUEST_URI']); // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- path portion is sanitized below and compared byte-for-byte against the raw form; any difference refuses the exemption
        if (!is_string($rawUri) || $rawUri === '') {
            return false;
        }

        // (a) Only the front controller serves the REST API. Anything else
        //     answering is not this route, whatever the path spells.
        if (!self::isFrontController()) {
            return false;
        }

        $parts       = explode('?', $rawUri, 2);
        $rawPath     = $parts[0];
        $queryString = $parts[1] ?? '';

        // (c) Fail closed when sanitizing rewrites the path: sanitize_text_field()
        //     deletes percent-encoded octets, which turns several non-matching
        //     paths into matching ones.
        $path = sanitize_text_field($rawPath);
        if ($path !== $rawPath) {
            return false;
        }

        $homePath = self::homePath();

        // (b) Pretty permalinks: the site's own canonical autologin path, whole.
        $restPrefix = 'wp-json';
        if (function_exists('rest_get_url_prefix')) {
            $candidate = rest_get_url_prefix();
            if (is_string($candidate) && $candidate !== '') {
                $restPrefix = $candidate;
            }
        }
        $route    = self::AGENT_REST_NAMESPACE . self::AGENT_AUTOLOGIN_PATH;
        $expected = $homePath . '/' . trim($restPrefix, '/') . '/' . $route;
        if (rtrim($path, '/') === $expected) {
            return true;
        }

        // Plain permalinks: /?rest_route=/wpmgr/v1/autologin, addressed to the
        // site root. Exact match on the decoded query argument, never a
        // substring of the raw query string, and never on an arbitrary path.
        if ($queryString === '') {
            return false;
        }
        $trimmedPath = rtrim($path, '/');
        if ($trimmedPath !== $homePath && $trimmedPath !== $homePath . '/index.php') {
            return false;
        }
        $args = [];
        parse_str($queryString, $args);
        return isset($args['rest_route'])
            && is_string($args['rest_route'])
            && rtrim($args['rest_route'], '/') === '/' . $route;
    }

    /**
     * Whether this request was served by WordPress's front controller.
     *
     * SCRIPT_NAME is set by the SAPI from the script the server actually
     * resolved, so it is the same class of signal as the PHP_SELF core derives
     * $pagenow from and cannot be asserted by a client. Every REST request
     * reaches WordPress through index.php under both permalink shapes; a
     * REQUEST_URI that spells the autologin route while xmlrpc.php or
     * options.php is answering is a smuggled path, not the route.
     *
     * Fails closed when SCRIPT_NAME is absent. Every HTTP SAPI sets it, so its
     * absence means CLI or something unrecognised — neither of which is a
     * browser following an autologin link.
     *
     * @return bool
     */
    private static function isFrontController(): bool
    {
        if (!isset($_SERVER['SCRIPT_NAME']) || !is_string($_SERVER['SCRIPT_NAME'])) {
            return false;
        }
        $script = sanitize_text_field(wp_unslash($_SERVER['SCRIPT_NAME'])); // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- sanitized via sanitize_text_field(wp_unslash())
        return $script !== '' && basename($script) === 'index.php';
    }

    /**
     * The site's home URL path with any trailing slash removed — '' for a root
     * install, '/blog' for a subdirectory one.
     *
     * home_url() is the site's own answer to "where do my URLs start", so it is
     * correct on subdirectory and multisite installs where dirname(SCRIPT_NAME)
     * would be wrong. Falls back to the front controller's directory when
     * WordPress is not far enough along to answer, which is the same value on
     * every install where the two can disagree.
     *
     * @return string
     */
    private static function homePath(): string
    {
        if (function_exists('home_url') && function_exists('wp_parse_url')) {
            $path = wp_parse_url(home_url('/'), PHP_URL_PATH);
            if (is_string($path)) {
                return rtrim($path, '/');
            }
        }

        if (isset($_SERVER['SCRIPT_NAME']) && is_string($_SERVER['SCRIPT_NAME'])) {
            $script = sanitize_text_field(wp_unslash($_SERVER['SCRIPT_NAME'])); // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- sanitized via sanitize_text_field(wp_unslash())
            $dir    = rtrim(str_replace('\\', '/', dirname($script)), '/');
            return $dir === '.' ? '' : $dir;
        }

        return '';
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
