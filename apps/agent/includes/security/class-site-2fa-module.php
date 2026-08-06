<?php
/**
 * Site2faModule - post-login 2FA interstitial, enrollment flow, and forced-change
 * enforcement for site users.
 *
 * ARCHITECTURE (section 3.2 of the design):
 *
 * 1. Hook wp_login at priority -1000 (very early, after primary auth succeeds).
 * 2. Capture the just-issued auth session token via the auth_cookie filter,
 *    then IMMEDIATELY DESTROY it -- so there is zero window with a half-auth cookie.
 * 3. Create a server-side signed interstitial session in user-meta:
 *       uuid (server-only secret) + user_id + created_at + redirect_to + remember_me
 *    The browser carries: user_id + session_meta_id + HMAC token.
 *    Expiry: 1 hour. The HMAC key is the agent's site secret (SECURE_AUTH_KEY).
 * 4. Render the chosen provider's form, die().
 * 5. On submit: verify the signed session, call provider validate().
 *    On success: wp_set_auth_cookie(), optional remember-device cookie, redirect.
 *    On failure: increment the per-session attempt counter; after 5 failures,
 *    expire the session (ties into the login-protection brute-force events).
 *
 * ENROLLMENT FLOW (SESSION_TYPE_2FA_SETUP):
 * When a required-but-unenrolled user logs in and no non-email method is configured,
 * they are routed to a multi-step setup interstitial instead of the email fallback.
 * During grace: a "set up now" prompt is shown (dismissible while grace remains).
 * After grace: setup is required to proceed.
 *
 * The setup flow is:
 *   Step 1 (intro)    - "Your administrator requires a second login step."
 *   Step 2 (choose)   - Choose method (TOTP / email / backup codes) with status pills.
 *   Step 3 (totp)     - QR code + manual secret (stored as PENDING, never active yet).
 *   Step 4 (confirm)  - 6-digit code verify; on success promotes pending → active.
 *   Step 5 (backup)   - Backup codes displayed once + download link.
 *   Step 6 (done)     - Summary of enabled methods; completes login.
 *
 * PENDING-SECRET INVARIANT:
 * TotpProvider::generateAndStorePending() stores the secret in META_PENDING_SECRET.
 * It is ONLY written to META_SECRET by activatePendingSecret() when the user proves
 * a valid code. An abandoned setup never activates the secret.
 *
 * APPLICATION-PASSWORD BYPASS BLOCK (H1 fix):
 * WordPress Application Passwords authenticate via HTTP Basic or the
 * wp_authenticate_application_password filter WITHOUT firing wp_login,
 * so the interstitial hook above never sees them. For any user who requires
 * 2FA (or who has a non-email method enrolled), application-password auth is
 * rejected outright via the wp_authenticate_application_password filter.
 * The agent's own /wpmgr/v1 REST channel and the autologin path are
 * explicitly exempted -- they carry their own Ed25519 credential and never
 * rely on application passwords.
 *
 * FORCED-CHANGE INTERSTITIAL (H2 fix):
 * When PasswordPolicyModule sets META_FORCE_CHANGE on a user, onWpLogin
 * checks for the flag BEFORE the 2FA check. If set, the user sees a
 * password-change form; META_FORCE_CHANGE is cleared only after a validated
 * new password is submitted (meeting strength + compromised + reuse policy
 * via PasswordPolicyModule::validatePassword). The same WPMGR_DISABLE_SITE_2FA
 * escape hatch disables this enforcement.
 *
 * FORCED-CHANGE ATTEMPT LIMITING (N1 fix):
 * handleVerifySubmit() runs the cross-request cap and per-session MAX_ATTEMPTS
 * guard BEFORE routing to either the 2FA handler or the forced-change handler.
 * Both paths are therefore subject to the same bound. handleForcedChangeSubmit()
 * increments both counters on every validation failure and clears them on success,
 * exactly mirroring the 2FA failure/success paths. A legitimate user who changes
 * their password successfully is NOT counted as a failure.
 *
 * BROWSER-BINDING FIX (I1 fix -- wp.org review, 0.61.9):
 * maybeResumeInterstitial() re-renders a pending session keyed purely on
 * $_POST['wpmgr_2fa_user_id'] plus the existence of a server-side session --
 * with no proof the caller is the browser that started the flow. For a pending
 * SETUP session this let an unauthenticated caller who merely knows or guesses
 * a user_id (e.g. 1) POST it to wp-login.php and have renderSetupScreen()
 * disclose that user's pending TOTP secret/QR, undermining enrollment.
 *
 * Fix: every session minted by createSession() also mints a random "bind"
 * token, stores only its SHA-256 hash in session['bind_hash'], and sets an
 * HttpOnly/SameSite=Lax cookie (COOKIE_BIND) carrying the raw token, scoped to
 * the login path and expiring with the session. maybeResumeInterstitial() now
 * requires hasValidBindCookie() to hash_equals()-match BEFORE rendering ANY
 * pending screen (setup, 2FA verify, or forced-change); a missing or
 * mismatched cookie falls through to the normal login page instead of
 * re-rendering anything. A session minted before this fix (no bind_hash key)
 * is treated as unsafe to resume -- never as implicitly trusted -- the user
 * simply logs in again to mint a bound session. The existing HMAC token on the
 * verify/setup SUBMIT path (computeSessionHmac/verifySessionHmac) is
 * unchanged: this adds the render-time browser binding the resume path was
 * missing, it does not replace the HMAC.
 *
 * LOCKOUT-PROOFING:
 * - If define('WPMGR_DISABLE_SITE_2FA', true) is in wp-config, 2FA and
 *   forced-change enforcement are fully disabled.
 * - The autologin path NEVER fires do_action('wp_login'), so it NEVER
 *   reaches this hook -- bypass by construction (ADR-055 / autologin docblock).
 * - Default-OFF: a fresh or empty policy challenges nobody.
 * - Backup codes always work as recovery, regardless of setup state.
 * - Grace logins: allowed N logins before enrollment is required.
 *
 * @package WPMgr\Agent\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Security;

use WPMgr\Agent\Security\BackupCodesProvider;
use WPMgr\Agent\Security\PasswordPolicyModule;

/**
 * WordPress-hooks enforcer for site-user 2FA.
 */
final class Site2faModule
{
    /** User-meta key for the pending interstitial session. */
    public const META_SESSION = 'wpmgr_2fa_session';

    /** User-meta key for the trusted-device records. */
    public const META_DEVICES = 'wpmgr_2fa_devices';

    /** User-meta key for the grace-login counter. */
    public const META_GRACE_COUNT = 'wpmgr_2fa_grace_count';

    /**
     * User-meta key for the cross-request per-user 2FA attempt counter.
     * Sliding window TTL in seconds equals SESSION_TTL. Used alongside the
     * per-session counter so that session destruction cannot reset the count.
     */
    public const META_ATTEMPT_COUNT = 'wpmgr_2fa_attempt_count';

    /** Cookie name for the trusted-device token. */
    public const COOKIE_DEVICE = 'wpmgr_2fa_device';

    /**
     * Cookie name for the browser-binding token minted alongside every
     * interstitial session. Only the browser holding this HttpOnly cookie may
     * resume a pending session's render on login_init -- see
     * maybeResumeInterstitial() / hasValidBindCookie(). This closes an
     * info-disclosure where an unauthenticated caller who merely knows or
     * guesses a user_id could otherwise re-render a pending SETUP screen
     * (which exposes the pending TOTP secret/QR) without ever having received
     * the original form.
     */
    public const COOKIE_BIND = 'wpmgr_2fa_bind';

    /** Interstitial session TTL in seconds (1 hour). */
    private const SESSION_TTL = 3600;

    /** Maximum failed second-factor attempts per session before expiry. */
    private const MAX_ATTEMPTS = 5;

    /**
     * Maximum cumulative cross-request second-factor attempts per user within
     * the sliding SESSION_TTL window. Prevents session-recreation resets.
     */
    private const MAX_CROSS_REQUEST_ATTEMPTS = 15;

    /** Minimum cookie token length for trusted-device tokens. */
    private const DEVICE_TOKEN_BYTES = 32;

    /** Byte length of the browser-binding token minted for interstitial sessions. */
    private const BIND_TOKEN_BYTES = 32;

    /**
     * Interstitial session type identifier for the standard 2FA challenge.
     * Stored in session['type'] to distinguish from FORCED_CHANGE sessions.
     */
    private const SESSION_TYPE_2FA = '2fa';

    /**
     * Interstitial session type identifier for forced password-change sessions.
     */
    private const SESSION_TYPE_FORCED_CHANGE = 'forced_change';

    /**
     * Interstitial session type identifier for the 2FA enrollment/setup flow.
     * A session of this type walks the user through the multi-step setup screens.
     */
    public const SESSION_TYPE_2FA_SETUP = '2fa_setup';

    /**
     * Session sub-step keys for the setup flow.
     * Stored in session['setup_step'] to track multi-screen state.
     */
    public const SETUP_STEP_INTRO   = 'intro';
    public const SETUP_STEP_CHOOSE  = 'choose';
    public const SETUP_STEP_TOTP    = 'totp';
    public const SETUP_STEP_CONFIRM = 'confirm';
    public const SETUP_STEP_BACKUP  = 'backup';
    public const SETUP_STEP_DONE    = 'done';

    private SecurityPolicy $policy;

    /** @var list<SiteTwoFactorProvider> */
    private array $providers;

    private static bool $verifying = false;

    /**
     * @param SecurityPolicy              $policy    The active site policy.
     * @param list<SiteTwoFactorProvider> $providers All registered providers.
     */
    public function __construct(SecurityPolicy $policy, array $providers)
    {
        $this->policy    = $policy;
        $this->providers = $providers;
    }

    /**
     * Register WordPress hooks. Call once on plugins_loaded.
     * Static guard makes it idempotent.
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

        // Recovery constant: fully disable 2FA enforcement.
        // @see ADR-059 section lockout-proofing invariant 6.
        if (defined('WPMGR_DISABLE_SITE_2FA') && WPMGR_DISABLE_SITE_2FA) {
            return;
        }

        // Forced-change interstitial and 2FA both gate on the same wp_login hook.
        // Register when either feature has active rules.
        $has2fa         = $this->policy->twoFactorEnabled;
        $hasForcedChange = $this->policy->passwordMaxAgeDays > 0
            || $this->policy->forcePasswordChange !== [];

        if ($has2fa || $hasForcedChange) {
            // Hook the post-primary-auth interstitial at very early priority.
            // Fires AFTER WP completes primary password auth; before the browser
            // has a live session. Priority -1000 beats other 2FA plugins' hooks.
            add_action('wp_login', [$this, 'onWpLogin'], -1000, 2);

            // Re-show the interstitial if the user lands on a login page while a
            // pending session exists (prevents navigating around it).
            add_action('login_init', [$this, 'maybeResumeInterstitial']);
        }

        // H1 fix: block application-password auth for 2FA-required users.
        // This filter fires when WP resolves an HTTP-Basic / app-password credential.
        // We gate it on twoFactorEnabled to avoid overhead when 2FA is off.
        if ($has2fa) {
            add_filter('wp_authenticate_application_password', [$this, 'blockAppPasswordFor2faUser'], 10, 5);
        }

        // XML-RPC block for 2FA users.
        if ($has2fa && $this->policy->blockXmlrpcFor2faUsers) {
            add_filter('xmlrpc_login_error', [$this, 'blockXmlrpcFor2faUser'], 10, 2);
            add_filter('authenticate', [$this, 'interceptXmlrpc2fa'], 100, 3);
        }

        // Profile section: proactive enrollment from the WP user profile screen.
        // Hooks fire for the viewing user's own profile and for admin editing others.
        if ($has2fa) {
            add_action('show_user_profile', [$this, 'renderProfileSection']);
            add_action('edit_user_profile', [$this, 'renderProfileSection']);
            add_action('personal_options_update', [$this, 'handleProfileSectionSave']);
            add_action('edit_user_profile_update', [$this, 'handleProfileSectionSave']);
        }
    }

    // -------------------------------------------------------------------------
    // H1 fix: Application Password block
    // -------------------------------------------------------------------------

    /**
     * Reject application-password authentication for users who require 2FA
     * or who have a non-email (real) 2FA method enrolled.
     *
     * EXEMPTED (must not be blocked):
     *  - The agent's own /wpmgr/v1 REST routes (Ed25519-signed; never use app passwords).
     *  - The autologin path (POST /wp-json/wpmgr/v1/autologin; also REST, also Ed25519).
     *  - Any user who does NOT require 2FA and has no non-email method enrolled.
     *
     * Application passwords do NOT satisfy the second factor: they carry only
     * password-equivalent credentials and bypass wp_login entirely, so the 2FA
     * interstitial never fires for them.
     *
     * @param \WP_Error|\WP_User|null $user        Result from earlier authenticate filters.
     * @param \WP_User|false          $inputUser   Resolved user (or false).
     * @param string                  $appPassword The raw application password.
     * @param array<mixed>|null       $item        Application-password DB record.
     * @param \WP_REST_Request|null   $request     The current REST request.
     * @return \WP_Error|\WP_User|null
     */
    public function blockAppPasswordFor2faUser(
        mixed $user,
        mixed $inputUser,
        string $appPassword,
        ?array $item,
        mixed $request
    ): mixed {
        // If a prior filter already produced an error, pass it through.
        if (is_a($user, 'WP_Error')) {
            return $user;
        }

        // Resolve the user we will be acting on.
        $resolvedUser = ($user instanceof \WP_User) ? $user : $inputUser;
        if (!($resolvedUser instanceof \WP_User)) {
            return $user;
        }

        // Exempt the agent's own REST namespace (/wpmgr/v1/*). Those routes
        // are authenticated via Ed25519 signatures at the REST permission callback
        // and never reach application-password resolution.
        if ($this->isAgentRestRequest($request)) {
            return $user;
        }

        // Block if the user requires 2FA.
        if ($this->policy->requires2fa($resolvedUser)) {
            return new \WP_Error(
                'wpmgr_2fa_app_password_blocked',
                esc_html__('Application passwords are disabled for accounts that require two-factor authentication. Use an alternative authentication method.', 'wpmgr-agent')
            );
        }

        // Block if the user has any non-email 2FA method enrolled.
        // Email is intentionally excluded: it is always "configured" for any user
        // with an email address and therefore cannot indicate deliberate 2FA enrollment.
        if ($this->hasNonEmailMethodConfigured($resolvedUser)) {
            return new \WP_Error(
                'wpmgr_2fa_app_password_blocked',
                esc_html__('Application passwords are disabled for accounts with two-factor authentication enrolled. Use an alternative authentication method.', 'wpmgr-agent')
            );
        }

        return $user;
    }

    /**
     * Check whether the current REST request targets the agent's own namespace.
     * The agent's /wpmgr/v1 routes are exempted from app-password blocking.
     *
     * @param mixed $request The WP_REST_Request or null.
     * @return bool
     */
    private function isAgentRestRequest(mixed $request): bool
    {
        // WP_REST_Request carries the route; check it if available.
        if ($request instanceof \WP_REST_Request) {
            $route = '';
            if (method_exists($request, 'get_route')) {
                $route = (string) $request->get_route();
            }
            if (str_contains($route, '/wpmgr/v1/')) {
                return true;
            }
        }

        // Fallback: inspect the request URI directly (for edge cases where
        // WP_REST_Request is not passed through by the caller).
        if (isset($_SERVER['REQUEST_URI']) && is_string($_SERVER['REQUEST_URI'])) { // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- read-only check; sanitized on next line
            $uri = sanitize_text_field(wp_unslash($_SERVER['REQUEST_URI']));
            if (str_contains($uri, '/wpmgr/v1/')) {
                return true;
            }
        }

        return false;
    }

    /**
     * Check whether the user has any non-email 2FA method enrolled.
     * EmailCodeProvider is excluded: it is always "configured" for any user with
     * an email, so including it would make the check meaningless (M1 fix context).
     *
     * @param \WP_User $user
     * @return bool
     */
    private function hasNonEmailMethodConfigured(\WP_User $user): bool
    {
        foreach ($this->providers as $provider) {
            if ($provider->key() === 'email') {
                continue;
            }
            if ($provider->isConfiguredFor($user)) {
                return true;
            }
        }
        return false;
    }

    // -------------------------------------------------------------------------
    // Post-login hook
    // -------------------------------------------------------------------------

    /**
     * Post-primary-auth hook: evaluate forced-change and 2FA requirement.
     * Called by WordPress after wp_authenticate() succeeds.
     *
     * FORCED-CHANGE check runs at priority -2000 (via PasswordPolicyModule) to
     * set the meta flag, then THIS hook at -1000 reads that flag and intercepts
     * BEFORE the normal 2FA interstitial. This means:
     *   forced-change + 2FA-required user: forced-change form fires first, then
     *   on next login 2FA interstitial fires normally.
     *
     * BYPASS PATHS:
     *  - WPMGR_DISABLE_SITE_2FA constant (handled in install()).
     *  - The autologin command NEVER fires do_action('wp_login'), so it NEVER
     *    reaches this hook -- bypass by construction (ADR-055 / autologin docblock).
     *  - $self::$verifying flag prevents re-entry when we re-fire wp_login
     *    after successful 2FA verification.
     *
     * @param string   $userLogin User's login name.
     * @param \WP_User $user      Authenticated user object.
     * @return void
     */
    public function onWpLogin(string $userLogin, \WP_User $user): void
    {
        // Prevent re-entrant triggering when we re-fire wp_login after verify.
        if (self::$verifying) {
            return;
        }

        try {
            // Forced-change check: priority before 2FA.
            if ($this->interceptIfForcedChange($user)) {
                return;
            }

            // 2FA interstitial check.
            if ($this->policy->twoFactorEnabled) {
                $this->interceptIfRequired($user);
            }
        } catch (\Throwable $e) {
            // Never block login due to an engine failure; fall through silently.
        }
    }

    // -------------------------------------------------------------------------
    // H2 fix: Forced-change interstitial
    // -------------------------------------------------------------------------

    /**
     * Check whether the user has a forced-change flag set and, if so, destroy
     * the current session and render the password-change interstitial.
     *
     * Returns true if the flow was intercepted (caller must return immediately).
     * Returns false if the user may continue to the 2FA check or normal login.
     *
     * @param \WP_User $user
     * @return bool True if the forced-change interstitial was triggered.
     */
    private function interceptIfForcedChange(\WP_User $user): bool
    {
        $userId = (int) $user->ID;

        if (!function_exists('get_user_meta')) {
            return false;
        }

        $forceReason = get_user_meta($userId, PasswordPolicyModule::META_FORCE_CHANGE, true);
        if (!is_string($forceReason) || $forceReason === '') {
            return false;
        }

        // Destroy the just-issued auth session before rendering the form.
        $this->destroyCurrentSession($userId);

        $session = $this->createSession($userId, $this->getCurrentRedirectTo(), false, self::SESSION_TYPE_FORCED_CHANGE);
        $this->renderForcedChangeForm($user, $session, $forceReason, '');
        return true;
    }

    /**
     * Explicit wp_kses() tag/attribute allowlist for the 2FA verify/setup/
     * forced-change interstitials and the WP profile-screen section.
     *
     * wp_kses_post() cannot be used for these screens: its default
     * $allowedposttags omits <form>/<input>/<button>, which every one of
     * these forms requires in order to render (and submit) at all. This
     * allowlist is instead scoped to exactly the tags/attributes the module
     * emits -- grep-verified against renderForcedChangeForm(),
     * renderInterstitial(), renderSetupScreen() (and every
     * buildSetup*Html() step it calls), renderProfileSection(), and every
     * SiteTwoFactorProvider::renderForm() implementation (Totp/Email/
     * BackupCodes) -- including the inline TOTP QR <svg>/<rect> emitted by
     * QrEncoder::toSvg(). Each value placed into the markup is still
     * esc_html()/esc_attr()/esc_url()'d at assembly time (belt-and-
     * suspenders); wp_kses() here is the visible escape-at-output-boundary
     * pass the 2026-07 wp.org review asked for.
     *
     * @return array<string,array<string,bool>>
     */
    private static function allowedFormKses(): array
    {
        return [
            'form'   => ['name' => true, 'id' => true, 'action' => true, 'method' => true],
            'p'      => ['class' => true, 'style' => true],
            'label'  => ['for' => true],
            'br'     => [],
            'a'      => ['href' => true, 'class' => true, 'download' => true],
            'h2'     => [],
            'h3'     => [],
            'em'     => [],
            'span'   => ['style' => true],
            'table'  => ['class' => true, 'style' => true],
            'tr'     => ['style' => true],
            'td'     => ['style' => true],
            'th'     => ['scope' => true],
            'strong' => [],
            'small'  => [],
            'code'   => ['id' => true, 'style' => true],
            'ol'     => ['style' => true],
            'ul'     => [],
            'li'     => [],
            'div'    => ['style' => true],
            'button' => ['type' => true, 'name' => true, 'value' => true, 'class' => true],
            'input'  => [
                'type'         => true,
                'name'         => true,
                'id'           => true,
                'value'        => true,
                'class'        => true,
                'autocomplete' => true,
                'inputmode'    => true,
                'pattern'      => true,
                'maxlength'    => true,
                'placeholder'  => true,
                'required'     => true,
                'autofocus'    => true,
            ],
            // The TOTP QR code (QrEncoder::toSvg()) is inline SVG, not user
            // input: only the secret + issuer name feed it, and both go
            // through rawurlencode() before QR encoding.
            'svg'    => [
                'xmlns'      => true,
                'width'      => true,
                'height'     => true,
                'viewbox'    => true,
                'role'       => true,
                'aria-label' => true,
            ],
            'rect'   => [
                'x'      => true,
                'y'      => true,
                'width'  => true,
                'height' => true,
                'fill'   => true,
            ],
        ];
    }

    /**
     * URL protocols allowed inside {@see allowedFormKses()} markup, extending
     * WP's default allowed protocols with `data:`.
     *
     * The backup-codes setup step's client-side "Download backup codes" link
     * (see buildSetupBackupHtml()) is a `data:text/plain,...` URI so the codes
     * never make a server round-trip; `data:` is not in
     * wp_allowed_protocols() by default, so without this addition wp_kses()
     * would strip the scheme from that href and leave a broken link.
     *
     * @return string[]
     */
    private static function allowedFormProtocols(): array
    {
        return array_merge(wp_allowed_protocols(), ['data']);
    }

    /**
     * Render the forced password-change form and die().
     *
     * @param \WP_User            $user
     * @param array<string,mixed> $session
     * @param string              $reason  Reason code from META_FORCE_CHANGE.
     * @param string              $error   Optional validation error message.
     * @return void
     */
    private function renderForcedChangeForm(\WP_User $user, array $session, string $reason, string $error): void
    {
        $userId    = (int) $session['user_id'];
        $sessionId = (string) ($session['id'] ?? '');
        $createdAt = (int) ($session['created_at'] ?? 0);
        $uuid      = (string) ($session['uuid'] ?? '');
        $hmac      = $this->computeSessionHmac($userId, $sessionId, $createdAt, $uuid);

        $loginUrl  = function_exists('wp_login_url') ? wp_login_url() : '/wp-login.php';
        $verifyUrl = add_query_arg(['action' => 'wpmgr_2fa_verify'], $loginUrl);

        $heading = $reason === 'expiry'
            ? esc_html__('Your password has expired. Please choose a new password.', 'wpmgr-agent')
            : esc_html__('You must change your password before continuing.', 'wpmgr-agent');

        if (function_exists('login_header')) {
            login_header(esc_html__('Change Your Password', 'wpmgr-agent'), '', null);
        }

        $formHtml  = '<form name="wpmgr_fc_form" id="loginform" action="' . esc_url($verifyUrl) . '" method="post">';
        $formHtml .= '<p>' . esc_html($heading) . '</p>';
        $formHtml .= '<input type="hidden" name="wpmgr_2fa_user_id" value="' . esc_attr((string) $userId) . '">';
        $formHtml .= '<input type="hidden" name="wpmgr_2fa_session_id" value="' . esc_attr($sessionId) . '">';
        $formHtml .= '<input type="hidden" name="wpmgr_2fa_token" value="' . esc_attr($hmac) . '">';
        $formHtml .= '<input type="hidden" name="wpmgr_2fa_provider" value="forced_change">';

        if ($error !== '') {
            $formHtml .= '<p class="message" style="color:#d63638">' . esc_html($error) . '</p>';
        }

        $formHtml .= '<p><label for="wpmgr_fc_pass1">' . esc_html__('New password:', 'wpmgr-agent') . '</label>';
        $formHtml .= '<br><input type="password" id="wpmgr_fc_pass1" name="wpmgr_fc_pass1" autocomplete="new-password" required></p>';
        $formHtml .= '<p><label for="wpmgr_fc_pass2">' . esc_html__('Confirm new password:', 'wpmgr-agent') . '</label>';
        $formHtml .= '<br><input type="password" id="wpmgr_fc_pass2" name="wpmgr_fc_pass2" autocomplete="new-password" required></p>';
        $formHtml .= '<p class="submit">';
        $formHtml .= '<input type="submit" id="wp-submit" class="button button-primary button-large" value="';
        $formHtml .= esc_attr__('Change Password', 'wpmgr-agent') . '">';
        $formHtml .= '</p></form>';

        // Escape-at-output-boundary: every dynamic value above was already
        // esc_html()/esc_attr()/esc_url()'d at assembly time; wp_kses() here
        // is the visible boundary escape (see allowedFormKses()).
        echo wp_kses($formHtml, self::allowedFormKses(), self::allowedFormProtocols());

        if (function_exists('login_footer')) {
            login_footer();
        }

        exit;
    }

    /**
     * Handle a forced-change form submission.
     * Validates the new password against the policy, clears META_FORCE_CHANGE,
     * and issues the real auth cookie on success.
     *
     * Attempt counting (N1 fix): every validation failure increments both the
     * per-session counter and the cross-request counter, exactly as the 2FA path
     * does. The shared guards in handleVerifySubmit already ran before this is
     * called; here we only need to persist the updated count on failure and clear
     * it on success.
     *
     * @param int                 $userId
     * @param array<string,mixed> $session
     * @param int                 $currentAttempts Current value of session['attempts'] (already read by caller).
     * @return void
     */
    private function handleForcedChangeSubmit(int $userId, array $session, int $currentAttempts): void
    {
        $user = function_exists('get_userdata') ? get_userdata($userId) : false;
        if (!($user instanceof \WP_User)) {
            wp_die(esc_html__('User not found.', 'wpmgr-agent'), '', ['response' => 400]);
        }

        // Retrieve and validate the new password fields.
        // phpcs:ignore WordPress.Security.NonceVerification.Missing -- HMAC session signing; no WP session exists to mint a nonce against (section 3.2)
        $pass1 = isset($_POST['wpmgr_fc_pass1']) && is_string($_POST['wpmgr_fc_pass1']) // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
            // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized,WordPress.Security.NonceVerification.Missing -- plaintext required for strength validation; never stored or echoed
            ? wp_unslash($_POST['wpmgr_fc_pass1'])
            : '';
        // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
        $pass2 = isset($_POST['wpmgr_fc_pass2']) && is_string($_POST['wpmgr_fc_pass2']) // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
            // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized,WordPress.Security.NonceVerification.Missing -- plaintext required for match check; never stored or echoed
            ? wp_unslash($_POST['wpmgr_fc_pass2'])
            : '';

        $forceReason = '';
        if (function_exists('get_user_meta')) {
            $raw = get_user_meta($userId, PasswordPolicyModule::META_FORCE_CHANGE, true);
            $forceReason = is_string($raw) ? $raw : '';
        }

        if (!is_string($pass1) || $pass1 === '') {
            // Failure: increment attempt counters before re-rendering the form (N1 fix).
            $session['attempts'] = $currentAttempts + 1;
            $this->storeSession($userId, $session);
            $this->incrementCrossRequestAttempts($userId);
            $this->renderForcedChangeForm($user, $session, $forceReason, esc_html__('Please enter a new password.', 'wpmgr-agent'));
            return;
        }

        if (!is_string($pass2) || $pass1 !== $pass2) {
            // Failure: increment attempt counters before re-rendering the form (N1 fix).
            $session['attempts'] = $currentAttempts + 1;
            $this->storeSession($userId, $session);
            $this->incrementCrossRequestAttempts($userId);
            $this->renderForcedChangeForm($user, $session, $forceReason, esc_html__('Passwords do not match.', 'wpmgr-agent'));
            return;
        }

        // Validate against the password policy.
        if (class_exists(PasswordPolicyModule::class) && function_exists('class_exists')) {
            $errors = new \WP_Error();
            // We need a PasswordPolicyModule instance to call validatePassword.
            // Build a temporary one with the current policy and stub deps.
            $stubSettings = new class implements CpUrlProvider {
                public function controlPlaneUrl(): string
                {
                    return '';
                }
            };
            $stubSigner = new class implements RequestSigner {
                public function signHeaders(string $method, string $path, string $body): array
                {
                    return [];
                }
            };
            $pwMod = new PasswordPolicyModule($this->policy, $stubSettings, $stubSigner);
            $pwMod->validatePassword($pass1, $user, $errors);

            if (!empty($errors->errors)) {
                // Failure: increment attempt counters before re-rendering the form (N1 fix).
                $session['attempts'] = $currentAttempts + 1;
                $this->storeSession($userId, $session);
                $this->incrementCrossRequestAttempts($userId);
                $messages = implode(' ', array_map(fn ($m) => is_string($m) ? $m : '', array_column(array_values($errors->errors), 0)));
                $this->renderForcedChangeForm($user, $session, $forceReason, $messages);
                return;
            }
        }

        // All checks passed -- update the password.
        if (!function_exists('wp_set_password')) {
            wp_die(esc_html__('Password change is not available.', 'wpmgr-agent'), '', ['response' => 500]);
        }
        wp_set_password($pass1, $userId);

        // Clear the force-change flag and update last-changed timestamp.
        if (function_exists('delete_user_meta')) {
            delete_user_meta($userId, PasswordPolicyModule::META_FORCE_CHANGE);
        }
        if (function_exists('update_user_meta')) {
            update_user_meta($userId, PasswordPolicyModule::META_LAST_CHANGED, time());
        }

        // Clear the interstitial session and the cross-request attempt counter on
        // success (N1 fix: mirrors the 2FA success path at line ~683).
        $this->clearSession($userId);
        $this->clearCrossRequestAttempts($userId);

        $redirectTo = isset($session['redirect_to']) && is_string($session['redirect_to'])
            ? $session['redirect_to']
            : (function_exists('admin_url') ? admin_url() : '/wp-admin/');

        // Issue the real auth cookie.
        wp_set_auth_cookie($userId, false);

        // Re-fire wp_login with the guard so side-effects (activity log, etc.) run.
        self::$verifying = true;
        $freshUser = function_exists('get_userdata') ? get_userdata($userId) : null;
        if ($freshUser instanceof \WP_User) {
            do_action('wp_login', $freshUser->user_login, $freshUser); // phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedHooknameFound -- firing WP core's documented post-login action; not a custom hook
        }
        self::$verifying = false;

        wp_safe_redirect(esc_url_raw($redirectTo));
        exit;
    }

    // -------------------------------------------------------------------------
    // Detect pending interstitial sessions
    // -------------------------------------------------------------------------

    /**
     * Detect pending interstitial sessions on login_init and re-show the form.
     * Prevents the user from navigating away from the interstitial.
     *
     * @return void
     */
    public function maybeResumeInterstitial(): void
    {
        // Only act on the verify-submit action or bare login page load.
        $action = isset($_GET['action']) // phpcs:ignore WordPress.Security.NonceVerification.Recommended -- no state change; read for routing
            ? sanitize_key(wp_unslash($_GET['action'])) // phpcs:ignore WordPress.Security.NonceVerification.Recommended -- same
            : '';

        if ($action === 'wpmgr_2fa_verify') {
            $this->handleVerifySubmit();
            return;
        }

        // Check if there is a pending session for a known user_id in POST/GET.
        // This user_id alone proves nothing (it is attacker-guessable); the
        // hasValidBindCookie() check below is what gates the actual render.
        $userId = isset($_POST['wpmgr_2fa_user_id']) // phpcs:ignore WordPress.Security.NonceVerification.Missing -- interstitial session uses its own HMAC signing, not WP nonces (see section 3.2: machine session, not browser nonce)
            ? absint(wp_unslash($_POST['wpmgr_2fa_user_id'])) // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
            : 0;
        if ($userId <= 0) {
            return;
        }

        $user = function_exists('get_userdata') ? get_userdata($userId) : false;
        if (!$user instanceof \WP_User) {
            return;
        }

        $session = $this->loadSession($userId);
        if ($session === null) {
            return;
        }

        // BROWSER-BINDING FIX (I1): only the browser that originated this
        // session -- and therefore received the wpmgr_2fa_bind cookie when the
        // session was created -- may resume ANY pending screen here. This
        // path is reached with nothing but a guessable user_id and an
        // existing session, so without a bind match we must not render
        // anything: falling through to the normal login page, rather than
        // re-rendering the setup screen (which can expose a pending TOTP
        // secret/QR), the 2FA verify form, or the forced-change form.
        if (!$this->hasValidBindCookie($session)) {
            return;
        }

        $sessionType = (string) ($session['type'] ?? self::SESSION_TYPE_2FA);
        if ($sessionType === self::SESSION_TYPE_FORCED_CHANGE) {
            $forceReason = '';
            if (function_exists('get_user_meta')) {
                $raw = get_user_meta($userId, PasswordPolicyModule::META_FORCE_CHANGE, true);
                $forceReason = is_string($raw) ? $raw : '';
            }
            $this->renderForcedChangeForm($user, $session, $forceReason, '');
            return;
        }

        if ($sessionType === self::SESSION_TYPE_2FA_SETUP) {
            $this->renderSetupScreen($user, $session);
            return;
        }

        $this->renderInterstitial($user, $session);
    }

    /**
     * Handle the 2FA verify form submission (both 2FA codes and forced-change).
     *
     * @return void
     */
    public function handleVerifySubmit(): void
    {
        // phpcs:ignore WordPress.Security.NonceVerification.Missing -- interstitial uses HMAC session signing, not WP nonces (section 3.2 design; the nonce concept does not apply: no WP session exists yet to mint a nonce against)
        $userId    = isset($_POST['wpmgr_2fa_user_id']) ? absint(wp_unslash($_POST['wpmgr_2fa_user_id'])) : 0;
        // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
        $sessionId = isset($_POST['wpmgr_2fa_session_id']) ? sanitize_text_field(wp_unslash($_POST['wpmgr_2fa_session_id'])) : ''; // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
        // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
        $hmac      = isset($_POST['wpmgr_2fa_token']) ? sanitize_text_field(wp_unslash($_POST['wpmgr_2fa_token'])) : ''; // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
        // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
        $providerKey = isset($_POST['wpmgr_2fa_provider']) ? sanitize_key(wp_unslash($_POST['wpmgr_2fa_provider'])) : ''; // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
        // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
        $remember    = isset($_POST['wpmgr_2fa_remember']) && $_POST['wpmgr_2fa_remember'] === '1'; // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same

        if ($userId <= 0 || $sessionId === '' || $hmac === '') {
            wp_die(esc_html__('Invalid 2FA session.', 'wpmgr-agent'), '', ['response' => 400]);
        }

        $user = function_exists('get_userdata') ? get_userdata($userId) : false;
        if (!$user instanceof \WP_User) {
            wp_die(esc_html__('User not found.', 'wpmgr-agent'), '', ['response' => 400]);
        }

        // Verify signed session.
        $session = $this->loadSession($userId);
        if ($session === null
            || !hash_equals($session['id'], $sessionId)
            || !$this->verifySessionHmac($userId, $sessionId, $session['created_at'], $session['uuid'], $hmac)
        ) {
            $this->clearSession($userId);
            wp_die(esc_html__('Session expired or invalid. Please log in again.', 'wpmgr-agent'), '', ['response' => 403]);
        }

        // Check TTL.
        if (time() - $session['created_at'] > self::SESSION_TTL) {
            $this->clearSession($userId);
            wp_die(esc_html__('2FA session expired. Please log in again.', 'wpmgr-agent'), '', ['response' => 403]);
        }

        // --- Shared brute-force guards (apply to BOTH 2FA and forced-change paths) ---

        // Cross-request guard: checked before routing so forced-change submits cannot
        // bypass the per-user cumulative cap by recreating a new session (N1 fix).
        if (!$this->checkCrossRequestAttempts($userId)) {
            $this->clearSession($userId);
            wp_die(esc_html__('Too many failed verification attempts. Please log in again.', 'wpmgr-agent'), '', ['response' => 429]);
        }

        // Per-session brute-force guard: also applies to both paths (N1 fix).
        $attempts = (int) ($session['attempts'] ?? 0);
        if ($attempts >= self::MAX_ATTEMPTS) {
            $this->clearSession($userId);
            wp_die(esc_html__('Too many failed attempts. Please log in again.', 'wpmgr-agent'), '', ['response' => 429]);
        }

        // Route to the forced-change handler when the session type indicates it.
        $sessionType = (string) ($session['type'] ?? self::SESSION_TYPE_2FA);
        if ($sessionType === self::SESSION_TYPE_FORCED_CHANGE) {
            $this->handleForcedChangeSubmit($userId, $session, $attempts);
            return;
        }

        // Route to the setup handler when the session type indicates enrollment.
        if ($sessionType === self::SESSION_TYPE_2FA_SETUP) {
            $this->handleSetupSubmit($userId, $session, $attempts, $user);
            return;
        }

        // --- Standard 2FA code verification path ---

        // Find and validate the chosen provider.
        $provider = $this->findProvider($providerKey);
        if ($provider === null || !$provider->isConfiguredFor($user)) {
            $this->renderInterstitial($user, $session, esc_html__('Invalid provider selected.', 'wpmgr-agent'));
            return;
        }

        // Collect and sanitize all provider input fields.
        $providerInput = $this->collectProviderInput();

        if (!$provider->validate($user, $providerInput)) {
            // Increment both the per-session counter and the cross-request counter.
            $session['attempts'] = $attempts + 1;
            $this->storeSession($userId, $session);
            $this->incrementCrossRequestAttempts($userId);
            $this->renderInterstitial(
                $user,
                $session,
                esc_html__(
                    'Incorrect code. Please try again. If you just used this code, wait for your authenticator to show a new one before trying again.',
                    'wpmgr-agent'
                )
            );
            return;
        }

        // SUCCESS -- clear the interstitial session and issue the real cookie.
        $this->clearSession($userId);
        $this->clearCrossRequestAttempts($userId);
        $redirectTo = isset($session['redirect_to']) && is_string($session['redirect_to'])
            ? $session['redirect_to']
            : admin_url();

        // Handle trusted-device cookie.
        if ($remember && $this->policy->twoFactorRememberDeviceDays > 0) {
            $this->setDeviceCookie($userId);
        }

        // Issue the real WP auth cookie.
        wp_set_auth_cookie($userId, (bool) ($session['remember_me'] ?? false));

        // Re-fire wp_login with the "already verified" guard so normal side-effects
        // (activity log, WooCommerce session, etc.) run correctly -- but our own
        // interstitial does not recurse because $verifying is true.
        self::$verifying = true;
        $user = function_exists('get_userdata') ? get_userdata($userId) : null;
        if ($user instanceof \WP_User) {
            do_action('wp_login', $user->user_login, $user); // phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedHooknameFound -- firing WP core's documented post-login action; not a custom hook
        }
        self::$verifying = false;

        wp_safe_redirect(esc_url_raw($redirectTo));
        exit;
    }

    // -------------------------------------------------------------------------
    // M1 fix: XML-RPC intercept (corrected 2FA-required or non-email enrolled)
    // -------------------------------------------------------------------------

    /**
     * Intercept XML-RPC logins for users with 2FA actually configured.
     *
     * M1 fix: previously this blocked any user whose isConfiguredFor() returned
     * true, which included EmailCodeProvider for every user with an email address,
     * effectively blocking ALL authenticated XML-RPC even when 2FA was not in use.
     *
     * Corrected gate: block only when the user is role-required for 2FA OR has
     * a non-email provider (TOTP, backup codes) enrolled. The email provider is
     * always available as a fallback and does not constitute deliberate enrollment.
     *
     * @param mixed    $user
     * @param string   $username
     * @param string   $password
     * @return mixed
     */
    public function interceptXmlrpc2fa(mixed $user, string $username, string $password): mixed
    {
        if (!is_a($user, 'WP_User')) {
            return $user;
        }
        if (!defined('XMLRPC_REQUEST') || !XMLRPC_REQUEST) {
            return $user;
        }

        // Block if the role requires 2FA for this user.
        if ($this->policy->requires2fa($user)) {
            return new \WP_Error(
                'wpmgr_2fa_xmlrpc_blocked',
                esc_html__('Two-factor authentication is required. XML-RPC password-only access is disabled for this account.', 'wpmgr-agent')
            );
        }

        // Block if the user has a non-email 2FA method enrolled (deliberate enrollment).
        if ($this->hasNonEmailMethodConfigured($user)) {
            return new \WP_Error(
                'wpmgr_2fa_xmlrpc_blocked',
                esc_html__('Two-factor authentication is required. XML-RPC password-only access is disabled for this account.', 'wpmgr-agent')
            );
        }

        return $user;
    }

    /**
     * @param mixed    $error
     * @param \WP_User $user
     * @return mixed
     */
    public function blockXmlrpcFor2faUser(mixed $error, \WP_User $user): mixed
    {
        return $error;
    }

    // -------------------------------------------------------------------------
    // Cross-request attempt limiter (LOW item)
    // -------------------------------------------------------------------------

    /**
     * Check whether the user is within the cross-request attempt limit.
     * Returns true if the attempt should be allowed, false if the limit is exceeded.
     *
     * @param int $userId
     * @return bool
     */
    private function checkCrossRequestAttempts(int $userId): bool
    {
        if (!function_exists('get_user_meta')) {
            return true;
        }
        $record = get_user_meta($userId, self::META_ATTEMPT_COUNT, true);
        if (!is_array($record)) {
            return true;
        }
        $count     = (int) ($record['count'] ?? 0);
        $windowEnd = (int) ($record['window_end'] ?? 0);
        // Reset if the window has expired.
        if (time() > $windowEnd) {
            return true;
        }
        return $count < self::MAX_CROSS_REQUEST_ATTEMPTS;
    }

    /**
     * Increment the cross-request attempt counter for the user.
     * Uses a sliding window equal to SESSION_TTL.
     *
     * @param int $userId
     * @return void
     */
    private function incrementCrossRequestAttempts(int $userId): void
    {
        if (!function_exists('get_user_meta') || !function_exists('update_user_meta')) {
            return;
        }
        $record = get_user_meta($userId, self::META_ATTEMPT_COUNT, true);
        if (!is_array($record) || time() > (int) ($record['window_end'] ?? 0)) {
            $record = ['count' => 1, 'window_end' => time() + self::SESSION_TTL];
        } else {
            $record['count'] = ((int) ($record['count'] ?? 0)) + 1;
        }
        update_user_meta($userId, self::META_ATTEMPT_COUNT, $record);
    }

    /**
     * Clear the cross-request attempt counter on successful verification.
     *
     * @param int $userId
     * @return void
     */
    private function clearCrossRequestAttempts(int $userId): void
    {
        if (function_exists('delete_user_meta')) {
            delete_user_meta($userId, self::META_ATTEMPT_COUNT);
        }
    }

    // -------------------------------------------------------------------------
    // Trusted-device helpers
    // -------------------------------------------------------------------------

    /**
     * Check whether the current request carries a valid, user-bound trusted-device
     * cookie. Mirrors the B1-fix user-binding check from the operator 2FA design.
     *
     * @param int $userId
     * @return bool
     */
    public function hasTrustedDevice(int $userId): bool
    {
        if ($this->policy->twoFactorRememberDeviceDays <= 0) {
            return false;
        }
        if (!isset($_COOKIE[self::COOKIE_DEVICE]) || !is_string($_COOKIE[self::COOKIE_DEVICE])) { // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- sanitized on the next line
            return false;
        }
        $raw = sanitize_text_field(wp_unslash($_COOKIE[self::COOKIE_DEVICE]));
        if ($raw === '' || strlen($raw) < self::DEVICE_TOKEN_BYTES * 2) {
            return false;
        }
        $tokenHash = hash('sha256', $raw);
        return $this->findDevice($userId, $tokenHash) !== null;
    }

    /**
     * Set a trusted-device cookie and record it in user-meta.
     *
     * @param int $userId
     * @return void
     */
    private function setDeviceCookie(int $userId): void
    {
        $token    = bin2hex(random_bytes(self::DEVICE_TOKEN_BYTES));
        $hash     = hash('sha256', $token);
        $expires  = time() + $this->policy->twoFactorRememberDeviceDays * 86400;

        $this->storeDevice($userId, $hash, $expires);

        if (!headers_sent()) {
            setcookie(
                self::COOKIE_DEVICE,
                $token,
                [
                    'expires'  => $expires,
                    'path'     => '/',
                    'httponly' => true,
                    'secure'   => is_ssl(),
                    'samesite' => 'Strict',
                ]
            );
        }
    }

    /**
     * Find a device record by token hash, asserting user-binding (B1 fix).
     *
     * @param int    $userId
     * @param string $tokenHash
     * @return array<string,mixed>|null
     */
    private function findDevice(int $userId, string $tokenHash): ?array
    {
        if (!function_exists('get_user_meta')) {
            return null;
        }
        $devices = get_user_meta($userId, self::META_DEVICES, true);
        if (!is_array($devices)) {
            return null;
        }
        $now = time();
        foreach ($devices as $device) {
            if (!is_array($device)) {
                continue;
            }
            // User-binding check: the device's recorded user_id must match the
            // authenticating user. This is the B1 fix mirror (twofa.go:262-276).
            if (!isset($device['user_id']) || (int) $device['user_id'] !== $userId) {
                continue;
            }
            if (!isset($device['hash']) || !hash_equals($device['hash'], $tokenHash)) {
                continue;
            }
            if (isset($device['expires']) && $device['expires'] < $now) {
                continue;
            }
            return $device;
        }
        return null;
    }

    /**
     * Store a new device record in user-meta.
     *
     * @param int    $userId
     * @param string $tokenHash
     * @param int    $expires
     * @return void
     */
    private function storeDevice(int $userId, string $tokenHash, int $expires): void
    {
        if (!function_exists('get_user_meta') || !function_exists('update_user_meta')) {
            return;
        }
        $devices   = get_user_meta($userId, self::META_DEVICES, true);
        $devices   = is_array($devices) ? $devices : [];
        $now       = time();
        // Prune expired devices.
        $devices   = array_values(array_filter($devices, static function ($d) use ($now): bool {
            return is_array($d) && isset($d['expires']) && $d['expires'] >= $now;
        }));
        $devices[] = ['user_id' => $userId, 'hash' => $tokenHash, 'expires' => $expires];
        update_user_meta($userId, self::META_DEVICES, $devices);
    }

    /**
     * Nuke all trusted devices on password change.
     *
     * @param int $userId
     * @return void
     */
    public function clearDevices(int $userId): void
    {
        if (function_exists('delete_user_meta')) {
            delete_user_meta($userId, self::META_DEVICES);
        }
    }

    // -------------------------------------------------------------------------
    // Core interstitial logic
    // -------------------------------------------------------------------------

    /**
     * Evaluate 2FA requirement for the user and intercept if needed.
     *
     * ENROLLMENT ROUTING:
     * When a required-but-unenrolled user logs in and TOTP is in the allowed methods,
     * they are routed to the setup flow (SESSION_TYPE_2FA_SETUP) instead of the email
     * fallback. During active grace, the setup step is set to SETUP_STEP_INTRO so the
     * user sees the "set up now or skip" prompt; once grace is exhausted, the setup is
     * mandatory (no skip offered). After grace, the email fallback is removed as the
     * default funnel — the user must complete setup to proceed.
     *
     * @param \WP_User $user
     * @return void
     */
    private function interceptIfRequired(\WP_User $user): void
    {
        $userId = (int) $user->ID;

        // Trusted-device check: skip the interstitial.
        if ($this->hasTrustedDevice($userId)) {
            return;
        }

        $isRequired = $this->policy->requires2fa($user);
        $providers  = $this->resolveProvidersFor($user);

        // 2FA is optional and user has nothing enrolled -- no interstitial.
        if (!$isRequired && $providers === []) {
            return;
        }

        // Determine whether the user has any NON-EMAIL method configured (deliberate enrollment).
        // The email provider is always "configured" for any user with an email address and
        // therefore does not count as deliberate enrollment.
        $hasNonEmailEnrolled = $this->hasNonEmailMethodConfigured($user);

        // Check if the required user needs enrollment (no deliberate 2FA method set up yet).
        if ($isRequired && !$hasNonEmailEnrolled) {
            $graceCount = (int) get_user_meta($userId, self::META_GRACE_COUNT, true);
            $graceMax   = $this->policy->twoFactorGraceLogins;

            $hasGrace = $graceMax > 0 && $graceCount < $graceMax;

            // Determine whether TOTP setup is an allowed option for this user.
            $allowedMethods = $this->policy->allowedMethodsFor($user);
            $totpAllowed    = in_array('totp', $allowedMethods, true);

            // Route to setup flow when TOTP is allowed (preferred enrollment path).
            if ($totpAllowed) {
                if ($hasGrace) {
                    update_user_meta($userId, self::META_GRACE_COUNT, $graceCount + 1);
                }
                // Destroy the session before the setup interstitial.
                $this->destroyCurrentSession($userId);
                $redirectTo = $this->getCurrentRedirectTo();
                $rememberMe = isset($_POST['rememberme']); // phpcs:ignore WordPress.Security.NonceVerification.Missing -- wp_login action; WP core has already verified credentials at this point

                $session                    = $this->createSession($userId, $redirectTo, $rememberMe, self::SESSION_TYPE_2FA_SETUP);
                $session['setup_step']      = self::SETUP_STEP_INTRO;
                $session['grace_remaining'] = $hasGrace;
                $this->storeSession($userId, $session);
                $this->renderSetupScreen($user, $session);
                return;
            }

            // TOTP not allowed: fall back to email code during grace, enforce after.
            if ($hasGrace) {
                update_user_meta($userId, self::META_GRACE_COUNT, $graceCount + 1);
                // Allow this login; user will be prompted to set up on a future login.
                return;
            }

            // Grace exhausted and TOTP not available: enforce email fallback.
            foreach ($this->providers as $p) {
                if ($p->key() === 'email') {
                    $providers = [$p];
                    break;
                }
            }
        }

        if ($providers === []) {
            // No suitable provider available -- fail open (never lock out).
            return;
        }

        // Destroy the just-issued auth session before showing the interstitial.
        // This ensures zero half-authenticated window.
        $this->destroyCurrentSession($userId);

        // Pick the first available provider as default.
        $redirectTo = $this->getCurrentRedirectTo();
        $rememberMe = isset($_POST['rememberme']); // phpcs:ignore WordPress.Security.NonceVerification.Missing -- wp_login action; WP core has already verified credentials at this point

        $session = $this->createSession($userId, $redirectTo, $rememberMe, self::SESSION_TYPE_2FA);
        $this->renderInterstitial($user, $session);
    }

    /**
     * Capture and destroy the just-issued WP auth session cookie.
     * Uses wp_destroy_all_sessions + wp_clear_auth_cookie to ensure
     * zero half-authenticated window before the interstitial renders.
     *
     * SECURITY INVARIANT (C1): both calls MUST happen BEFORE the interstitial
     * page renders or exit fires. This is tested in Site2faModuleTest::
     * test_c1_session_destruction_before_interstitial().
     *
     * @param int $userId
     * @return void
     */
    public function destroyCurrentSession(int $userId): void
    {
        // Destroy all existing sessions for this user (simpler and fully correct).
        if (function_exists('wp_destroy_all_sessions')) {
            wp_destroy_all_sessions();
        }
        if (function_exists('wp_clear_auth_cookie')) {
            wp_clear_auth_cookie();
        }
    }

    /**
     * Resolve the set of configured providers for this user, filtered by the
     * policy's allowed_methods for the user's role.
     *
     * @param \WP_User $user
     * @return list<SiteTwoFactorProvider>
     */
    private function resolveProvidersFor(\WP_User $user): array
    {
        $allowed = $this->policy->allowedMethodsFor($user);
        $out     = [];
        foreach ($this->providers as $provider) {
            if (in_array($provider->key(), $allowed, true) && $provider->isConfiguredFor($user)) {
                $out[] = $provider;
            }
        }
        return $out;
    }

    /**
     * Find a provider by key.
     *
     * @param string $key
     * @return SiteTwoFactorProvider|null
     */
    private function findProvider(string $key): ?SiteTwoFactorProvider
    {
        foreach ($this->providers as $p) {
            if ($p->key() === $key) {
                return $p;
            }
        }
        return null;
    }

    /**
     * Create a signed interstitial session in user-meta.
     *
     * @param int    $userId
     * @param string $redirectTo
     * @param bool   $rememberMe
     * @param string $type       SESSION_TYPE_2FA or SESSION_TYPE_FORCED_CHANGE.
     * @return array<string,mixed>
     */
    private function createSession(int $userId, string $redirectTo, bool $rememberMe, string $type = self::SESSION_TYPE_2FA): array
    {
        $uuid    = bin2hex(random_bytes(16));
        $id      = bin2hex(random_bytes(16));
        $created = time();

        // BROWSER-BINDING FIX (I1): mint a random token whose hash alone is
        // stored server-side; the raw token is handed to the browser only via
        // an HttpOnly cookie. Resuming this session's render later requires
        // proving possession of that cookie (see hasValidBindCookie()).
        $bindToken = bin2hex(random_bytes(self::BIND_TOKEN_BYTES));

        $session = [
            'id'          => $id,
            'uuid'        => $uuid,
            'user_id'     => $userId,
            'created_at'  => $created,
            'redirect_to' => $redirectTo,
            'remember_me' => $rememberMe,
            'attempts'    => 0,
            'type'        => $type,
            'bind_hash'   => hash('sha256', $bindToken),
        ];
        $this->storeSession($userId, $session);
        $this->setBindCookie($bindToken);
        return $session;
    }

    /**
     * Store the interstitial session in user-meta.
     *
     * @param int                  $userId
     * @param array<string,mixed>  $session
     * @return void
     */
    private function storeSession(int $userId, array $session): void
    {
        if (function_exists('update_user_meta')) {
            update_user_meta($userId, self::META_SESSION, $session);
        }
    }

    /**
     * Load the stored interstitial session, returning null if absent or expired.
     *
     * @param int $userId
     * @return array<string,mixed>|null
     */
    private function loadSession(int $userId): ?array
    {
        if (!function_exists('get_user_meta')) {
            return null;
        }
        $session = get_user_meta($userId, self::META_SESSION, true);
        if (!is_array($session)) {
            return null;
        }
        // Validate structure.
        if (!isset($session['id'], $session['uuid'], $session['user_id'], $session['created_at'])) {
            return null;
        }
        // TTL check.
        if (time() - (int) $session['created_at'] > self::SESSION_TTL) {
            $this->clearSession($userId);
            return null;
        }
        return $session;
    }

    /**
     * Clear the interstitial session.
     *
     * @param int $userId
     * @return void
     */
    private function clearSession(int $userId): void
    {
        if (function_exists('delete_user_meta')) {
            delete_user_meta($userId, self::META_SESSION);
        }
        $this->clearBindCookie();
    }

    // -------------------------------------------------------------------------
    // Browser-binding cookie (I1 fix)
    // -------------------------------------------------------------------------

    /**
     * Check whether the current request carries a browser-binding cookie that
     * hash-matches the given session's bind_hash.
     *
     * BACKWARD-COMPAT: a session minted before this fix carries no bind_hash
     * key. That is treated as "cannot safely resume a session screen" -- NOT
     * as implicitly trusted -- so this returns false rather than throwing or
     * silently allowing the render. The user simply logs in again, which
     * mints a fresh, bound session.
     *
     * @param array<string,mixed> $session
     * @return bool
     */
    private function hasValidBindCookie(array $session): bool
    {
        if (!isset($session['bind_hash']) || !is_string($session['bind_hash']) || $session['bind_hash'] === '') {
            return false;
        }

        if (!isset($_COOKIE[self::COOKIE_BIND]) || !is_string($_COOKIE[self::COOKIE_BIND])) { // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- sanitized on the next line
            return false;
        }

        $raw = sanitize_text_field(wp_unslash($_COOKIE[self::COOKIE_BIND]));
        if ($raw === '' || strlen($raw) < self::BIND_TOKEN_BYTES * 2) {
            return false;
        }

        return hash_equals($session['bind_hash'], hash('sha256', $raw));
    }

    /**
     * Set the browser-binding cookie for a newly minted interstitial session.
     * HttpOnly + SameSite=Lax + Secure-when-SSL; scoped to the login path and
     * expiring alongside the session (SESSION_TTL).
     *
     * @param string $token Raw bind token (its hash is what session['bind_hash'] stores).
     * @return void
     */
    private function setBindCookie(string $token): void
    {
        if (headers_sent()) {
            return;
        }
        setcookie(
            self::COOKIE_BIND,
            $token,
            [
                'expires'  => time() + self::SESSION_TTL,
                'path'     => $this->loginPath(),
                'httponly' => true,
                'secure'   => is_ssl(),
                'samesite' => 'Lax',
            ]
        );
    }

    /**
     * Clear the browser-binding cookie. Called whenever the interstitial
     * session itself is cleared, so a stale bind token never outlives its
     * session.
     *
     * @return void
     */
    private function clearBindCookie(): void
    {
        if (headers_sent()) {
            return;
        }
        setcookie(
            self::COOKIE_BIND,
            '',
            [
                'expires'  => time() - self::SESSION_TTL,
                'path'     => $this->loginPath(),
                'httponly' => true,
                'secure'   => is_ssl(),
                'samesite' => 'Lax',
            ]
        );
    }

    /**
     * Resolve the path component of the site's login URL, used to scope the
     * browser-binding cookie so it is only ever sent back to wp-login.php.
     *
     * @return string
     */
    private function loginPath(): string
    {
        if (function_exists('wp_login_url') && function_exists('wp_parse_url')) {
            $parsed = wp_parse_url(wp_login_url());
            if (is_array($parsed) && isset($parsed['path']) && is_string($parsed['path']) && $parsed['path'] !== '') {
                return $parsed['path'];
            }
        }
        return '/wp-login.php';
    }

    /**
     * Compute the HMAC token for a session (client-side field).
     * HMAC key: wp_salt('secure_auth') -- site-specific, not the password.
     *
     * @param int    $userId
     * @param string $sessionId
     * @param int    $createdAt
     * @param string $uuid
     * @return string
     */
    private function computeSessionHmac(int $userId, string $sessionId, int $createdAt, string $uuid): string
    {
        $key     = function_exists('wp_salt') ? wp_salt('secure_auth') : 'wpmgr-fallback';
        $message = "{$userId}|{$sessionId}|{$createdAt}|{$uuid}";
        return hash_hmac('sha256', $message, $key);
    }

    /**
     * Verify the client-side HMAC token.
     *
     * @param int    $userId
     * @param string $sessionId
     * @param int    $createdAt
     * @param string $uuid
     * @param string $clientToken
     * @return bool
     */
    private function verifySessionHmac(int $userId, string $sessionId, int $createdAt, string $uuid, string $clientToken): bool
    {
        $expected = $this->computeSessionHmac($userId, $sessionId, $createdAt, $uuid);
        return hash_equals($expected, $clientToken);
    }

    /**
     * Render the 2FA interstitial page and die().
     *
     * @param \WP_User            $user
     * @param array<string,mixed> $session
     * @param string              $error   Optional error message to display.
     * @return void
     */
    private function renderInterstitial(\WP_User $user, array $session, string $error = ''): void
    {
        $providers = $this->resolveProvidersFor($user);

        // Grace fallback: inject email provider for required-but-unenrolled.
        if ($providers === []) {
            foreach ($this->providers as $p) {
                if ($p->key() === 'email') {
                    $providers = [$p];
                    break;
                }
            }
        }

        $activeProvider = $providers[0] ?? null;
        if ($activeProvider === null) {
            // Nothing to show -- fail open.
            return;
        }

        $userId    = (int) $session['user_id'];
        $sessionId = (string) ($session['id'] ?? '');
        $createdAt = (int) ($session['created_at'] ?? 0);
        $uuid      = (string) ($session['uuid'] ?? '');
        $hmac      = $this->computeSessionHmac($userId, $sessionId, $createdAt, $uuid);

        $activeProvider->preRender($user);

        $loginUrl   = function_exists('wp_login_url') ? wp_login_url() : '/wp-login.php';
        $verifyUrl  = add_query_arg(['action' => 'wpmgr_2fa_verify'], $loginUrl);

        if (function_exists('login_header')) {
            login_header(
                esc_html__('Two-Factor Authentication', 'wpmgr-agent'),
                '',
                null
            );
        }

        // Build the form safely without heredocs.
        $formHtml = '<form name="wpmgr_2fa_form" id="loginform" action="' . esc_url($verifyUrl) . '" method="post">';
        $formHtml .= '<input type="hidden" name="wpmgr_2fa_user_id" value="' . esc_attr((string) $userId) . '">';
        $formHtml .= '<input type="hidden" name="wpmgr_2fa_session_id" value="' . esc_attr($sessionId) . '">';
        $formHtml .= '<input type="hidden" name="wpmgr_2fa_token" value="' . esc_attr($hmac) . '">';
        $formHtml .= '<input type="hidden" name="wpmgr_2fa_provider" value="' . esc_attr($activeProvider->key()) . '">';

        if ($error !== '') {
            $formHtml .= '<p class="message" style="color:#d63638">' . esc_html($error) . '</p>';
        }

        $formHtml .= $activeProvider->renderForm($user);

        if ($this->policy->twoFactorRememberDeviceDays > 0) {
            $days      = $this->policy->twoFactorRememberDeviceDays;
            $formHtml .= '<p>';
            $formHtml .= '<input type="checkbox" name="wpmgr_2fa_remember" id="wpmgr_2fa_remember" value="1">';
            $formHtml .= ' <label for="wpmgr_2fa_remember">';
            $formHtml .= esc_html(
                sprintf(
                    // translators: %d is the number of days.
                    __('Remember this device for %d days', 'wpmgr-agent'),
                    $days
                )
            );
            $formHtml .= '</label></p>';
        }

        // Provider switcher tabs (if multiple providers available).
        if (count($providers) > 1) {
            $formHtml .= '<p>' . esc_html__('Or use:', 'wpmgr-agent') . ' ';
            foreach ($providers as $p) {
                if ($p->key() === $activeProvider->key()) {
                    continue;
                }
                $switchUrl = add_query_arg(
                    ['action' => 'wpmgr_2fa_verify', 'wpmgr_2fa_provider' => $p->key()],
                    $loginUrl
                );
                $formHtml .= '<a href="' . esc_url($switchUrl) . '">' . esc_html($p->label()) . '</a> ';
            }
            $formHtml .= '</p>';
        }

        $formHtml .= '<p class="submit">';
        $formHtml .= '<input type="submit" name="wp-submit" id="wp-submit" class="button button-primary button-large" value="';
        $formHtml .= esc_attr__('Verify', 'wpmgr-agent') . '">';
        $formHtml .= '</p>';
        $formHtml .= '</form>';

        // Escape-at-output-boundary: every dynamic value above was already
        // esc_html()/esc_attr()/esc_url()'d at assembly time (including the
        // active provider's own renderForm()); wp_kses() here is the visible
        // boundary escape (see allowedFormKses()).
        echo wp_kses($formHtml, self::allowedFormKses(), self::allowedFormProtocols());

        if (function_exists('login_footer')) {
            login_footer();
        }

        exit;
    }

    // -------------------------------------------------------------------------
    // 2FA enrollment / setup flow
    // -------------------------------------------------------------------------

    /**
     * Render the setup interstitial for the current step.
     *
     * The setup flow shares the same HMAC-signed single-use session framework
     * as the 2FA verify and forced-change paths. Attempt caps apply to the TOTP
     * activation step (step 4) via the shared per-session and cross-request counters.
     *
     * @param \WP_User            $user
     * @param array<string,mixed> $session
     * @param string              $error   Optional error message.
     * @return void
     */
    private function renderSetupScreen(\WP_User $user, array $session, string $error = ''): void
    {
        $step = (string) ($session['setup_step'] ?? self::SETUP_STEP_INTRO);

        $userId    = (int) $session['user_id'];
        $sessionId = (string) ($session['id'] ?? '');
        $createdAt = (int) ($session['created_at'] ?? 0);
        $uuid      = (string) ($session['uuid'] ?? '');
        $hmac      = $this->computeSessionHmac($userId, $sessionId, $createdAt, $uuid);

        $loginUrl  = function_exists('wp_login_url') ? wp_login_url() : '/wp-login.php';
        $verifyUrl = add_query_arg(['action' => 'wpmgr_2fa_verify'], $loginUrl);

        if (function_exists('login_header')) {
            login_header(esc_html__('Two-Factor Setup', 'wpmgr-agent'), '', null);
        }

        $formHtml  = '<form name="wpmgr_setup_form" id="loginform" action="' . esc_url($verifyUrl) . '" method="post">';
        $formHtml .= '<input type="hidden" name="wpmgr_2fa_user_id" value="' . esc_attr((string) $userId) . '">';
        $formHtml .= '<input type="hidden" name="wpmgr_2fa_session_id" value="' . esc_attr($sessionId) . '">';
        $formHtml .= '<input type="hidden" name="wpmgr_2fa_token" value="' . esc_attr($hmac) . '">';
        $formHtml .= '<input type="hidden" name="wpmgr_2fa_provider" value="setup">';
        $formHtml .= '<input type="hidden" name="wpmgr_setup_step" value="' . esc_attr($step) . '">';

        if ($error !== '') {
            $formHtml .= '<p class="message" style="color:#d63638">' . esc_html($error) . '</p>';
        }

        $formHtml .= $this->buildSetupStepHtml($user, $session, $step);

        $formHtml .= '<p class="submit">';
        $formHtml .= '<input type="submit" id="wp-submit" class="button button-primary button-large" value="';

        if ($step === self::SETUP_STEP_INTRO && !empty($session['grace_remaining'])) {
            $formHtml .= esc_attr__('Set Up Now', 'wpmgr-agent') . '">';
            $formHtml .= ' ';
            // Skip button — only available while grace remains.
            $formHtml .= '<input type="submit" name="wpmgr_setup_skip" value="';
            $formHtml .= esc_attr__('Skip for Now', 'wpmgr-agent') . '">';
        } elseif ($step === self::SETUP_STEP_DONE) {
            $formHtml .= esc_attr__('Complete Login', 'wpmgr-agent') . '">';
        } else {
            $formHtml .= esc_attr__('Continue', 'wpmgr-agent') . '">';
        }

        $formHtml .= '</p>';
        $formHtml .= '</form>';

        // Escape-at-output-boundary: every dynamic value above (including
        // every buildSetup*Html() step fragment) was already
        // esc_html()/esc_attr()/esc_url()'d at assembly time; wp_kses() here
        // is the visible boundary escape (see allowedFormKses()).
        echo wp_kses($formHtml, self::allowedFormKses(), self::allowedFormProtocols());

        if (function_exists('login_footer')) {
            login_footer();
        }

        exit;
    }

    /**
     * Build the HTML for a specific setup step.
     *
     * @param \WP_User            $user
     * @param array<string,mixed> $session
     * @param string              $step
     * @return string  Escaped HTML fragment.
     */
    private function buildSetupStepHtml(\WP_User $user, array $session, string $step): string
    {
        switch ($step) {
            case self::SETUP_STEP_INTRO:
                return $this->buildSetupIntroHtml($session);

            case self::SETUP_STEP_CHOOSE:
                return $this->buildSetupChooseHtml($user);

            case self::SETUP_STEP_TOTP:
                return $this->buildSetupTotpHtml($user, $session);

            case self::SETUP_STEP_CONFIRM:
                return $this->buildSetupConfirmHtml();

            case self::SETUP_STEP_BACKUP:
                return $this->buildSetupBackupHtml($session);

            case self::SETUP_STEP_DONE:
                return $this->buildSetupDoneHtml($user);

            default:
                return $this->buildSetupIntroHtml($session);
        }
    }

    /**
     * Step 1 — intro: "Your administrator requires a second login step."
     *
     * @param array<string,mixed> $session
     * @return string
     */
    private function buildSetupIntroHtml(array $session): string
    {
        $html  = '<h3>' . esc_html__('Your administrator requires two-factor authentication.', 'wpmgr-agent') . '</h3>';
        $html .= '<p>' . esc_html__('Two-factor authentication adds an extra layer of security to your account. After logging in with your password, you will need to verify your identity with a second step.', 'wpmgr-agent') . '</p>';

        if (!empty($session['grace_remaining'])) {
            $html .= '<p><em>' . esc_html__('You may skip setup for now, but you will be required to set up two-factor authentication on a future login.', 'wpmgr-agent') . '</em></p>';
        }

        return $html;
    }

    /**
     * Step 2 — choose method: one row per ALLOWED method with status pills.
     *
     * @param \WP_User $user
     * @return string
     */
    private function buildSetupChooseHtml(\WP_User $user): string
    {
        $allowed = $this->policy->allowedMethodsFor($user);

        $labels = [
            'totp'   => esc_html__('Authenticator App', 'wpmgr-agent'),
            'email'  => esc_html__('Email Code', 'wpmgr-agent'),
            'backup' => esc_html__('Backup Codes', 'wpmgr-agent'),
        ];
        $descs = [
            'totp'   => esc_html__('Use an authenticator app (Google Authenticator, Authy, etc.) to generate one-time codes.', 'wpmgr-agent'),
            'email'  => esc_html__('Receive a one-time code by email.', 'wpmgr-agent'),
            'backup' => esc_html__('Generate a set of single-use recovery codes.', 'wpmgr-agent'),
        ];

        $html  = '<h3>' . esc_html__('Choose a two-factor method', 'wpmgr-agent') . '</h3>';
        $html .= '<p>' . esc_html__('Select at least one method to configure. You may configure multiple methods.', 'wpmgr-agent') . '</p>';
        $html .= '<table style="width:100%;border-collapse:collapse">';

        foreach ($allowed as $key) {
            if (!array_key_exists($key, $labels)) {
                continue;
            }

            $configured = false;
            $provider   = $this->findProvider($key);
            if ($provider !== null) {
                $configured = $provider->isConfiguredFor($user);
            }

            $pill = $configured
                ? '<span style="color:#0073aa;font-weight:bold">' . esc_html__('Configured', 'wpmgr-agent') . '</span>'
                : '<span style="color:#666">' . esc_html__('Not configured', 'wpmgr-agent') . '</span>';

            // Only TOTP has a setup screen — email is always available, backup codes are
            // generated in step 5. Show a "Configure" button only for TOTP.
            $html .= '<tr style="border-bottom:1px solid #ddd;padding:8px 0">';
            $html .= '<td style="padding:8px 0"><strong>' . $labels[$key] . '</strong><br>';
            $html .= '<small>' . $descs[$key] . '</small></td>';
            $html .= '<td style="text-align:right;padding:8px">' . $pill;

            if ($key === 'totp') {
                $html .= ' <input type="submit" name="wpmgr_setup_configure_totp" class="button button-secondary button-small" value="';
                $html .= esc_attr__('Configure', 'wpmgr-agent') . '">';
            }

            $html .= '</td></tr>';
        }

        $html .= '</table>';
        $html .= '<p><small>' . esc_html__('Click "Continue" to proceed with the current configuration.', 'wpmgr-agent') . '</small></p>';

        return $html;
    }

    /**
     * Step 3 — TOTP setup: QR code + base32 secret display.
     *
     * Generates and stores a PENDING secret (not yet active). The secret is only
     * promoted to active when the user confirms a valid code in step 4.
     *
     * @param \WP_User            $user
     * @param array<string,mixed> $session
     * @return string
     */
    private function buildSetupTotpHtml(\WP_User $user, array $session): string
    {
        $secret = '';

        // Re-use pending secret if one was already generated in this session.
        if (isset($session['totp_pending_secret']) && is_string($session['totp_pending_secret'])) {
            $secret = $session['totp_pending_secret'];
        }

        if ($secret === '') {
            // Generate a new pending secret for this user.
            $totp = $this->findTotpProvider();
            if ($totp !== null) {
                $secret = $totp->generateAndStorePending($user);
                // Cache the raw secret in the session (session is HMAC-signed).
                // This avoids a decrypt round-trip on page reload without extending trust.
            }
        }

        if ($secret === '') {
            return '<p>' . esc_html__('Unable to generate a secret. Please try again.', 'wpmgr-agent') . '</p>';
        }

        $issuer = '';
        if (function_exists('get_bloginfo')) {
            $issuer = get_bloginfo('name');
        }
        if ($issuer === '') {
            $issuer = 'WordPress';
        }

        $totp = $this->findTotpProvider();
        $otpauthUri = '';
        if ($totp !== null) {
            $otpauthUri = $totp->buildOtpauthUri($user, $secret, $issuer);
        }

        $qrSvg = '';
        if ($otpauthUri !== '' && class_exists(QrEncoder::class)) {
            $qrSvg = QrEncoder::toSvg($otpauthUri, 256);
        }

        $html  = '<h3>' . esc_html__('Set up your authenticator app', 'wpmgr-agent') . '</h3>';
        $html .= '<p>' . esc_html__('Scan the QR code below with your authenticator app (Google Authenticator, Authy, 1Password, etc.), or enter the secret key manually.', 'wpmgr-agent') . '</p>';

        if ($qrSvg !== '') {
            // QrEncoder::toSvg() generates inline SVG with esc_attr()'d
            // attributes; it never contains user-supplied data (only the
            // secret + issuer name go through rawurlencode() before QR
            // encoding). This fragment is returned up to renderSetupScreen(),
            // whose echo runs the whole assembled form through wp_kses()
            // (see allowedFormKses(), which allow-lists svg/rect).
            $html .= '<div style="margin:16px 0;text-align:center">' . $qrSvg . '</div>';
        } else {
            $html .= '<p>' . esc_html__('QR code unavailable. Please use the manual entry key below.', 'wpmgr-agent') . '</p>';
        }

        // Progressive-reveal manual key.
        $html .= '<p>';
        $html .= '<strong>' . esc_html__('Manual entry key:', 'wpmgr-agent') . '</strong><br>';
        $html .= '<code id="wpmgr-totp-secret" style="font-size:14px;letter-spacing:2px">';
        $html .= esc_html($secret);
        $html .= '</code>';
        $html .= '</p>';

        // App hints.
        $html .= '<p><small>';
        $html .= esc_html__('Suggested apps: Google Authenticator, Authy, Microsoft Authenticator, 1Password, Bitwarden.', 'wpmgr-agent');
        $html .= '</small></p>';

        // Re-roll button: generates a fresh secret.
        $html .= '<p>';
        $html .= '<input type="submit" name="wpmgr_setup_reroll_totp" class="button button-secondary button-small" value="';
        $html .= esc_attr__('Generate new secret', 'wpmgr-agent') . '">';
        $html .= '</p>';

        // Hidden field to carry the secret to the confirm step.
        $html .= '<input type="hidden" name="wpmgr_setup_totp_secret" value="' . esc_attr($secret) . '">';

        return $html;
    }

    /**
     * Step 4 — confirm / activate: 6-digit code entry.
     *
     * @return string
     */
    private function buildSetupConfirmHtml(): string
    {
        $field = esc_attr('wpmgr_totp_code');
        $html  = '<h3>' . esc_html__('Verify your authenticator app', 'wpmgr-agent') . '</h3>';
        $html .= '<p>' . esc_html__('Enter the 6-digit code shown in your authenticator app to confirm setup.', 'wpmgr-agent') . '</p>';
        $html .= '<p><label for="' . $field . '">';
        $html .= esc_html__('Verification code:', 'wpmgr-agent');
        $html .= '</label><br>';
        $html .= '<input type="text" id="' . $field . '" name="' . $field . '" ';
        $html .= 'autocomplete="one-time-code" inputmode="numeric" pattern="[0-9]{6}" ';
        $html .= 'maxlength="6" placeholder="' . esc_attr__('6-digit code', 'wpmgr-agent') . '" required autofocus>';
        $html .= '</p>';
        return $html;
    }

    /**
     * Step 5 — backup codes: display once + download link.
     *
     * Codes are read from the session where they were stored after BackupCodesProvider::generateAndStore().
     *
     * @param array<string,mixed> $session
     * @return string
     */
    private function buildSetupBackupHtml(array $session): string
    {
        $codes = isset($session['backup_codes']) && is_array($session['backup_codes'])
            ? $session['backup_codes']
            : [];

        $html  = '<h3>' . esc_html__('Save your backup codes', 'wpmgr-agent') . '</h3>';
        $html .= '<p style="color:#d63638;font-weight:bold">';
        $html .= esc_html__('These codes are shown ONCE. Save them now. You cannot view them again.', 'wpmgr-agent');
        $html .= '</p>';

        if ($codes !== []) {
            $html .= '<ol style="font-family:monospace;font-size:14px">';
            foreach ($codes as $code) {
                if (is_string($code)) {
                    $html .= '<li>' . esc_html($code) . '</li>';
                }
            }
            $html .= '</ol>';

            // Client-side download link (data: URI, no server round-trip).
            $siteHost = function_exists('get_bloginfo') ? get_bloginfo('url') : 'site';
            $siteHost = sanitize_text_field($siteHost);
            $filename = sanitize_file_name($siteHost . '-backup-codes.txt');
            $fileContent = implode("\n", array_filter($codes, 'is_string'));
            $dataUri = 'data:text/plain;charset=utf-8,' . rawurlencode($fileContent);
            $html .= '<p>';
            $html .= '<a href="' . esc_attr($dataUri) . '" download="' . esc_attr($filename) . '" class="button button-secondary">';
            $html .= esc_html__('Download backup codes', 'wpmgr-agent');
            $html .= '</a>';
            $html .= '</p>';
        }

        $html .= '<p>';
        $html .= esc_html__('Each code can be used once. Store them in a safe place (password manager, printed copy, etc.).', 'wpmgr-agent');
        $html .= '</p>';

        return $html;
    }

    /**
     * Step 6 — done: summary of enabled methods.
     *
     * @param \WP_User $user
     * @return string
     */
    private function buildSetupDoneHtml(\WP_User $user): string
    {
        $enabled = [];
        foreach ($this->providers as $p) {
            if ($p->key() !== 'email' && $p->isConfiguredFor($user)) {
                $enabled[] = esc_html($p->label());
            }
        }

        $html = '<h3>' . esc_html__('Two-factor authentication is set up!', 'wpmgr-agent') . '</h3>';

        if ($enabled !== []) {
            $html .= '<p>' . esc_html__('You have the following methods configured:', 'wpmgr-agent') . '</p>';
            $html .= '<ul>';
            foreach ($enabled as $label) {
                // $label is already esc_html()'d above; this fragment is
                // returned up to renderSetupScreen(), whose echo runs the
                // whole assembled form through wp_kses() (see
                // allowedFormKses()).
                $html .= '<li>' . $label . '</li>';
            }
            $html .= '</ul>';
        }

        $html .= '<p>' . esc_html__('You will be asked for a second factor each time you log in.', 'wpmgr-agent') . '</p>';
        return $html;
    }

    /**
     * Handle a setup flow form submission.
     *
     * Routes between steps based on session['setup_step'] (server-authoritative),
     * NEVER on a client-supplied step value. The POST body is used only to signal
     * "advance from the current step"; the actual current step is always read from
     * the server-side session record.
     *
     * SECURITY — two bypasses closed here (see security review findings 1A / 1B):
     *
     * 1A (DONE-jump): previously the step was read from $_POST['wpmgr_setup_step'],
     *    allowing an attacker with a valid HMAC token to POST step=done and receive
     *    an auth cookie without completing enrollment. Fixed: step is now authoritative
     *    from $session['setup_step']; the POST field is ignored for routing.
     *
     * 1B (grace bypass): the skip handler previously called completeLogin() without
     *    re-checking grace_remaining, allowing a grace-exhausted user to POST
     *    wpmgr_setup_skip=1 and bypass mandatory enrollment. Fixed: skip is now
     *    gated on !empty($session['grace_remaining']); otherwise re-renders the
     *    mandatory setup screen.
     *
     * Additionally, completeLogin() is now only reachable from the DONE step if
     * the user is actually enrolled (hasNonEmailMethodConfigured() === true). A
     * grace-skip arriving at the DONE-step code path is impossible because the
     * DONE case asserts enrollment before issuing the cookie.
     *
     * Attempt caps (per-session + cross-request) apply to the activation step (step 4).
     *
     * @param int                 $userId
     * @param array<string,mixed> $session
     * @param int                 $currentAttempts  Per-session attempt count (already read).
     * @param \WP_User            $user
     * @return void
     */
    private function handleSetupSubmit(int $userId, array $session, int $currentAttempts, \WP_User $user): void
    {
        // SECURITY: always read the current step from the server-side session, never
        // from $_POST. The POST field is rendered as a UI hint only; it must not
        // influence the step machine (finding 1A).
        $step = (string) ($session['setup_step'] ?? self::SETUP_STEP_INTRO);

        // Skip button: only allowed when grace_remaining is set in the SERVER session.
        // SECURITY: re-check grace_remaining server-side here (finding 1B) — the Skip
        // button is only rendered while grace remains, but a client could POST
        // wpmgr_setup_skip=1 regardless of UI state. If grace is exhausted, ignore the
        // skip request and re-render the mandatory setup screen.
        // phpcs:ignore WordPress.Security.NonceVerification.Missing -- setup interstitial uses HMAC session signing; no WP session exists to mint a nonce against (section 3.2)
        if (isset($_POST['wpmgr_setup_skip'])) { // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
            if (!empty($session['grace_remaining'])) {
                $this->completeLogin($userId, $session);
                return;
            }
            // Grace exhausted: ignore skip, fall through to re-render the current step.
            $this->renderSetupScreen($user, $session);
            return;
        }

        // Re-roll: user requested a new TOTP secret.
        // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
        if (isset($_POST['wpmgr_setup_reroll_totp'])) { // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
            $totp = $this->findTotpProvider();
            if ($totp !== null) {
                $secret = $totp->generateAndStorePending($user);
                $session['totp_pending_secret'] = $secret;
            }
            $session['setup_step'] = self::SETUP_STEP_TOTP;
            $this->storeSession($userId, $session);
            $this->renderSetupScreen($user, $session);
            return;
        }

        // Configure TOTP button from the choose screen.
        // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
        if (isset($_POST['wpmgr_setup_configure_totp'])) { // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
            $totp = $this->findTotpProvider();
            if ($totp !== null) {
                $secret = $totp->generateAndStorePending($user);
                $session['totp_pending_secret'] = $secret;
            }
            $session['setup_step'] = self::SETUP_STEP_TOTP;
            $this->storeSession($userId, $session);
            $this->renderSetupScreen($user, $session);
            return;
        }

        switch ($step) {
            case self::SETUP_STEP_INTRO:
                // Move to the choose-method step.
                $session['setup_step'] = self::SETUP_STEP_CHOOSE;
                $this->storeSession($userId, $session);
                $this->renderSetupScreen($user, $session);
                return;

            case self::SETUP_STEP_CHOOSE:
                // The user clicked Continue from the choose screen.
                // Determine what to do: if TOTP is not yet configured, require it.
                // If TOTP is configured, move to backup codes. If both done, move to done.
                $totp = $this->findTotpProvider();
                if ($totp !== null && !$totp->isConfiguredFor($user)) {
                    // TOTP not yet set up — go to TOTP step.
                    $secret = $totp->generateAndStorePending($user);
                    $session['totp_pending_secret'] = $secret;
                    $session['setup_step'] = self::SETUP_STEP_TOTP;
                } else {
                    // TOTP is configured (or not allowed). Move to backup codes.
                    $backupProvider = $this->findProvider('backup');
                    if ($backupProvider instanceof BackupCodesProvider) {
                        $codes = $backupProvider->generateAndStore($user);
                        $session['backup_codes'] = $codes;
                        $session['setup_step']   = self::SETUP_STEP_BACKUP;
                    } else {
                        $session['setup_step'] = self::SETUP_STEP_DONE;
                    }
                }
                $this->storeSession($userId, $session);
                $this->renderSetupScreen($user, $session);
                return;

            case self::SETUP_STEP_TOTP:
                // Move to the confirmation/activation step.
                // Carry the pending secret forward in the session.
                // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
                $carriedSecret = isset($_POST['wpmgr_setup_totp_secret']) && is_string($_POST['wpmgr_setup_totp_secret']) // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
                    ? sanitize_text_field(wp_unslash($_POST['wpmgr_setup_totp_secret'])) // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
                    : '';
                if ($carriedSecret !== '') {
                    $session['totp_pending_secret'] = $carriedSecret;
                }
                $session['setup_step'] = self::SETUP_STEP_CONFIRM;
                $this->storeSession($userId, $session);
                $this->renderSetupScreen($user, $session);
                return;

            case self::SETUP_STEP_CONFIRM:
                // Validate the submitted TOTP code against the pending secret.
                // Attempt caps apply here (shared counters).
                // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
                $code = isset($_POST['wpmgr_totp_code']) && is_string($_POST['wpmgr_totp_code']) // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
                    ? sanitize_text_field(wp_unslash($_POST['wpmgr_totp_code'])) // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
                    : '';

                $totp = $this->findTotpProvider();
                if ($totp === null) {
                    $this->renderSetupScreen($user, $session, esc_html__('TOTP provider unavailable. Please contact your administrator.', 'wpmgr-agent'));
                    return;
                }

                if (!$totp->activatePendingSecret($user, $code)) {
                    // Invalid code: increment attempt counters, stay on confirm step.
                    $session['attempts'] = $currentAttempts + 1;
                    $this->storeSession($userId, $session);
                    $this->incrementCrossRequestAttempts($userId);
                    $this->renderSetupScreen($user, $session, esc_html__('Incorrect code. Please check your authenticator app and try again.', 'wpmgr-agent'));
                    return;
                }

                // Activation succeeded: clear attempt counters, clear pending secret.
                unset($session['totp_pending_secret']);
                $session['attempts'] = 0;
                $this->clearCrossRequestAttempts($userId);

                // Move to backup codes step.
                $backupProvider = $this->findProvider('backup');
                if ($backupProvider instanceof BackupCodesProvider) {
                    $codes = $backupProvider->generateAndStore($user);
                    $session['backup_codes'] = $codes;
                    $session['setup_step']   = self::SETUP_STEP_BACKUP;
                } else {
                    $session['setup_step'] = self::SETUP_STEP_DONE;
                }

                $this->storeSession($userId, $session);
                $this->renderSetupScreen($user, $session);
                return;

            case self::SETUP_STEP_BACKUP:
                // User viewed/downloaded codes; clear them from session and move to done.
                unset($session['backup_codes']);
                $session['setup_step'] = self::SETUP_STEP_DONE;
                $this->storeSession($userId, $session);
                $this->renderSetupScreen($user, $session);
                return;

            case self::SETUP_STEP_DONE:
                // SECURITY: assert enrollment before issuing the auth cookie.
                // Even though the step is now server-authoritative (finding 1A fix),
                // this defense-in-depth check ensures completeLogin() is never reached
                // unless a non-email method was actually activated. If somehow the
                // session reached DONE without enrollment, re-render the intro step
                // rather than issuing a cookie.
                if (!$this->hasNonEmailMethodConfigured($user)) {
                    $session['setup_step'] = self::SETUP_STEP_INTRO;
                    $this->storeSession($userId, $session);
                    $this->renderSetupScreen($user, $session);
                    return;
                }
                $this->completeLogin($userId, $session);
                return;

            default:
                $session['setup_step'] = self::SETUP_STEP_INTRO;
                $this->storeSession($userId, $session);
                $this->renderSetupScreen($user, $session);
                return;
        }
    }

    /**
     * Complete the login: issue auth cookie, fire wp_login, redirect.
     * Called from both the setup done step and the grace-skip path.
     *
     * @param int                 $userId
     * @param array<string,mixed> $session
     * @return void
     */
    private function completeLogin(int $userId, array $session): void
    {
        $this->clearSession($userId);
        $redirectTo = isset($session['redirect_to']) && is_string($session['redirect_to'])
            ? $session['redirect_to']
            : (function_exists('admin_url') ? admin_url() : '/wp-admin/');

        wp_set_auth_cookie($userId, (bool) ($session['remember_me'] ?? false));

        self::$verifying = true;
        $user = function_exists('get_userdata') ? get_userdata($userId) : null;
        if ($user instanceof \WP_User) {
            do_action('wp_login', $user->user_login, $user); // phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedHooknameFound -- firing WP core's documented post-login action; not a custom hook
        }
        self::$verifying = false;

        wp_safe_redirect(esc_url_raw($redirectTo));
        exit;
    }

    /**
     * Find the TotpProvider from the registered providers list.
     *
     * @return TotpProvider|null
     */
    private function findTotpProvider(): ?TotpProvider
    {
        foreach ($this->providers as $p) {
            if ($p instanceof TotpProvider) {
                return $p;
            }
        }
        return null;
    }

    // -------------------------------------------------------------------------
    // WP Profile section: proactive enrollment
    // -------------------------------------------------------------------------

    /**
     * Render the WPMgr 2FA section on the WP user profile screen.
     *
     * Shows the current status of each allowed method (configured / not configured),
     * TOTP setup controls (enroll / regenerate), backup-code regeneration, and
     * remaining backup-code count.
     *
     * Hooked to show_user_profile and edit_user_profile.
     *
     * @param \WP_User $profileUser  The user whose profile is being displayed.
     * @return void
     */
    public function renderProfileSection(\WP_User $profileUser): void
    {
        // Only render when 2FA is enabled and the user is in scope.
        if (!$this->policy->twoFactorEnabled) {
            return;
        }

        $allowed = $this->policy->allowedMethodsFor($profileUser);
        if ($allowed === []) {
            return;
        }

        // Nonce for the profile save action.
        $nonce = wp_create_nonce('wpmgr_2fa_profile_' . (int) $profileUser->ID);

        $html  = '<h2>' . esc_html__('Two-Factor Authentication', 'wpmgr-agent') . '</h2>';
        $html .= '<table class="form-table">';

        foreach ($allowed as $key) {
            $provider = $this->findProvider($key);
            if ($provider === null) {
                continue;
            }

            $configured = $provider->isConfiguredFor($profileUser);
            $statusText = $configured
                ? esc_html__('Configured', 'wpmgr-agent')
                : esc_html__('Not configured', 'wpmgr-agent');

            $html .= '<tr>';
            $html .= '<th scope="row">' . esc_html($provider->label()) . '</th>';
            $html .= '<td>';
            $html .= '<p>' . $statusText . '</p>';

            if ($key === 'totp') {
                $html .= '<p>';
                if ($configured) {
                    $html .= '<button type="submit" name="wpmgr_profile_action" value="totp_reset" class="button button-secondary">';
                    $html .= esc_html__('Reset authenticator app', 'wpmgr-agent');
                    $html .= '</button>';
                } else {
                    $html .= '<button type="submit" name="wpmgr_profile_action" value="totp_setup" class="button button-primary">';
                    $html .= esc_html__('Set up authenticator app', 'wpmgr-agent');
                    $html .= '</button>';
                }
                $html .= '</p>';
            }

            if ($key === 'backup') {
                $remaining = 0;
                if ($provider instanceof BackupCodesProvider) {
                    $remaining = $provider->remainingCount($profileUser);
                }
                if ($configured) {
                    $html .= '<p>';
                    $html .= esc_html(
                        sprintf(
                            // translators: %d is the number of remaining codes.
                            __('%d codes remaining.', 'wpmgr-agent'),
                            $remaining
                        )
                    );
                    $html .= '</p>';
                }
                $html .= '<p>';
                $html .= '<button type="submit" name="wpmgr_profile_action" value="backup_regenerate" class="button button-secondary">';
                $html .= esc_html__('Regenerate backup codes', 'wpmgr-agent');
                $html .= '</button>';
                $html .= '</p>';
            }

            $html .= '</td>';
            $html .= '</tr>';
        }

        $html .= '</table>';
        $html .= '<input type="hidden" name="wpmgr_2fa_profile_nonce" value="' . esc_attr($nonce) . '">';
        $html .= '<input type="hidden" name="wpmgr_2fa_profile_user_id" value="' . esc_attr((string) ((int) $profileUser->ID)) . '">';

        // Escape-at-output-boundary: every dynamic value above was already
        // esc_html()/esc_attr()'d at assembly time; wp_kses() here is the
        // visible boundary escape (see allowedFormKses()).
        echo wp_kses($html, self::allowedFormKses(), self::allowedFormProtocols());
    }

    /**
     * Handle WP profile section saves: TOTP enrollment initiation and backup-code regeneration.
     *
     * Hooked to personal_options_update and edit_user_profile_update.
     *
     * @param int $userId  The user ID being saved (WP core passes this).
     * @return void
     */
    public function handleProfileSectionSave(int $userId): void
    {
        // Nonce verification: standard WP profile nonce.
        if (!isset($_POST['wpmgr_2fa_profile_nonce']) || !is_string($_POST['wpmgr_2fa_profile_nonce'])) { // phpcs:ignore WordPress.Security.NonceVerification.Missing -- checked on next line via wp_verify_nonce
            return;
        }
        $nonce = sanitize_text_field(wp_unslash($_POST['wpmgr_2fa_profile_nonce']));
        if (!wp_verify_nonce($nonce, 'wpmgr_2fa_profile_' . $userId)) {
            return;
        }

        // Capability check: the current user must be able to edit this user.
        if (!current_user_can('edit_user', $userId)) {
            return;
        }

        $action = isset($_POST['wpmgr_profile_action']) && is_string($_POST['wpmgr_profile_action'])
            ? sanitize_key(wp_unslash($_POST['wpmgr_profile_action']))
            : '';

        $profileUser = function_exists('get_userdata') ? get_userdata($userId) : false;
        if (!($profileUser instanceof \WP_User)) {
            return;
        }

        if ($action === 'backup_regenerate') {
            $backupProvider = $this->findProvider('backup');
            if ($backupProvider instanceof BackupCodesProvider) {
                $backupProvider->generateAndStore($profileUser);
            }
        }

        // TOTP setup and reset from the profile page: redirect into the interstitial
        // setup flow so the user gets the full QR + confirm experience. We cannot
        // do it inline on the profile page (no QR rendering in the save hook) — instead
        // we set a transient flag that the next profile page load will pick up to
        // display the setup flow inline. For now, we clear the active secret on reset
        // so the user is treated as unenrolled; enrollment then happens at next login.
        if ($action === 'totp_reset') {
            $totpProvider = $this->findTotpProvider();
            if ($totpProvider !== null) {
                $totpProvider->clearSecret($profileUser);
                $totpProvider->clearPendingSecret($profileUser);
            }
        }

        // totp_setup: no action here — the profile page will show the setup button,
        // and enrollment happens via the login interstitial. We do not initiate a QR
        // from the save hook because we have no reliable way to display the QR result
        // back on the profile page within this hook.
    }

    /**
     * Get the current redirect_to target from the request.
     *
     * @return string
     */
    private function getCurrentRedirectTo(): string
    {
        if (isset($_POST['redirect_to']) && is_string($_POST['redirect_to'])) { // phpcs:ignore WordPress.Security.NonceVerification.Missing -- reading redirect target after primary auth; WP core already verified credentials at this point
            $raw = sanitize_text_field(wp_unslash($_POST['redirect_to'])); // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
            if ($raw !== '') {
                return $raw;
            }
        }
        return function_exists('admin_url') ? admin_url() : '/wp-admin/';
    }

    /**
     * Collect and sanitize all known provider input fields from $_POST.
     *
     * @return array<string,string>
     */
    private function collectProviderInput(): array
    {
        $fields = [
            'wpmgr_totp_code',
            'wpmgr_email_code',
            'wpmgr_backup_code',
        ];
        $out = [];
        foreach ($fields as $field) {
            if (isset($_POST[$field]) && is_string($_POST[$field])) { // phpcs:ignore WordPress.Security.NonceVerification.Missing -- 2FA interstitial uses HMAC session token, not WP nonces (no WP session exists yet to mint a nonce against; see section 3.2)
                $out[$field] = sanitize_text_field(wp_unslash($_POST[$field])); // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
            }
        }
        return $out;
    }
}
