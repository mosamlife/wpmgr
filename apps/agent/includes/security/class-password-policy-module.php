<?php
/**
 * PasswordPolicyModule — enforces password requirements on login, profile update,
 * and password reset for site users.
 *
 * Requirements enforced (all gated by policy and role):
 *   - Minimum zxcvbn strength score (vendored bjeavons/zxcvbn-php).
 *   - HIBP compromised-password check (via CP proxy; fail-open).
 *   - Password reuse block (last N hashes in user-meta).
 *   - Password expiry (force-change interstitial on login after N days).
 *   - CP-pushed force-password-change list.
 *
 * Recovery constant: define('WPMGR_DISABLE_SITE_2FA', true) disables all
 * enforcement (same constant as the 2FA interstitial, per §6 of ADR-059).
 *
 * @package WPMgr\Agent\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Security;

use ZxcvbnPhp\Zxcvbn;

/**
 * WordPress-hooks enforcer for the password policy.
 */
final class PasswordPolicyModule
{
    /** User-meta key: rolling array of last-N password hashes. */
    public const META_HISTORY = 'wpmgr_pw_history';

    /** User-meta key: GMT timestamp of last password change. */
    public const META_LAST_CHANGED = 'wpmgr_pw_last_changed';

    /** User-meta key: reason code for force-change ('expiry', 'admin_reset', etc.). */
    public const META_FORCE_CHANGE = 'wpmgr_pw_change_required';

    /** Transient prefix for caching HIBP results per prefix+user. */
    public const HIBP_TRANSIENT_PREFIX = 'wpmgr_hibp_';

    private SecurityPolicy $policy;

    private CpUrlProvider $settings;

    private RequestSigner $signer;

    /**
     * @param SecurityPolicy $policy   Active site policy.
     * @param CpUrlProvider  $settings For the CP URL (HIBP proxy endpoint).
     * @param RequestSigner  $signer   For signing agent->CP requests.
     */
    public function __construct(SecurityPolicy $policy, CpUrlProvider $settings, RequestSigner $signer)
    {
        $this->policy   = $policy;
        $this->settings = $settings;
        $this->signer   = $signer;
    }

    /**
     * Register WordPress hooks. Call once on plugins_loaded.
     * No hooks are registered when enforcement is fully off.
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

        // Recovery constant: disables ALL auth policy enforcement.
        if (defined('WPMGR_DISABLE_SITE_2FA') && WPMGR_DISABLE_SITE_2FA) {
            return;
        }

        $hasAnyRule = $this->policy->passwordMinZxcvbnScore > 0
            || $this->policy->passwordBlockCompromised
            || $this->policy->passwordReuseBlockCount > 0
            || $this->policy->passwordMaxAgeDays > 0
            || $this->policy->forcePasswordChange !== [];

        if (!$hasAnyRule) {
            return;
        }

        // Gate password set/change/reset.
        // Priority 0 so we run before the profile form re-renders error messages.
        add_action('user_profile_update_errors', [$this, 'validateOnProfileUpdate'], 0, 3);
        add_filter('validate_password_reset', [$this, 'validateOnPasswordReset'], 0, 2);
        add_filter('registration_errors', [$this, 'validateOnRegistration'], 0, 3);

        // Track password changes: update last-changed timestamp + reuse history.
        add_action('profile_update', [$this, 'onProfileUpdate'], 10, 2);
        add_action('password_reset', [$this, 'onPasswordReset'], 10, 2);
        add_action('user_register', [$this, 'onUserRegister'], 10, 1);

        // Expiry + force-change: check at wp_login (priority -2000 = before 2FA at -1000).
        if ($this->policy->passwordMaxAgeDays > 0 || $this->policy->forcePasswordChange !== []) {
            add_action('wp_login', [$this, 'checkExpiryOnLogin'], -2000, 2);
        }
    }

    // -------------------------------------------------------------------------
    // Validation hooks
    // -------------------------------------------------------------------------

    /**
     * Validate password strength on profile update.
     *
     * WordPress core does NOT pass a WP_User here. edit_user() builds a bare
     * stdClass and passes it by reference (wp-admin/includes/user.php), so a
     * \WP_User hint on this method throws a TypeError at argument binding —
     * before any guard in the body can run, and outside validatePassword()'s
     * catch(\Throwable). That is a fatal on every wp-admin route that edits a
     * user, an administrator's own profile.php included.
     *
     * The object is deliberately NOT trusted for role-scoped decisions: it has
     * no `roles` and no `caps`, so reading roles off it would silently
     * under-enforce every role-scoped rule. ProfileUpdateUser::resolve()
     * produces a real user to evaluate against instead.
     *
     * @param \WP_Error $errors Error object to append to.
     * @param bool      $update Whether this is an update (vs. create).
     * @param mixed     $user   User being updated. Core passes \stdClass; typed
     *                          mixed because this is a hook boundary.
     * @return void
     */
    public function validateOnProfileUpdate(\WP_Error $errors, bool $update, mixed $user): void
    {
        // phpcs:ignore WordPress.Security.NonceVerification.Missing -- nonce verified by WP core's profile-update handler before this hook fires
        $password = isset($_POST['pass1']) && is_string($_POST['pass1']) // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
            ? wp_unslash($_POST['pass1']) // phpcs:ignore WordPress.Security.NonceVerification.Missing,WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- plaintext required for zxcvbn + HIBP SHA-1; sanitize_text_field alters special chars, defeating both; never stored or echoed
            : '';
        if ($password === '') {
            return;
        }
        // Resolved after the early return: no user load on the far more common
        // profile save that does not touch the password.
        $this->validatePassword($password, ProfileUpdateUser::resolve($user), $errors);
    }

    /**
     * Validate password strength on password reset.
     *
     * Core documents this argument as `WP_User|WP_Error` — "WP_User object if
     * the login and reset key match, WP_Error object otherwise" (wp-login.php).
     * Core's own resetpass branch bails before firing the action when the key
     * did not match, but the published contract is the contract, and any other
     * caller firing `validate_password_reset` may hand us the WP_Error. A
     * \WP_User hint would make that a fatal on the reset form.
     *
     * A WP_Error means there is no user whose password is being set — core will
     * not call reset_password() either — so there is nothing to validate.
     *
     * @param \WP_Error $errors
     * @param mixed     $user   \WP_User on success, \WP_Error otherwise.
     * @return \WP_Error
     */
    public function validateOnPasswordReset(\WP_Error $errors, mixed $user): \WP_Error
    {
        if (!$user instanceof \WP_User) {
            return $errors;
        }
        // phpcs:ignore WordPress.Security.NonceVerification.Missing -- WP core verifies the reset key before this filter fires; no additional nonce is needed
        $password = isset($_POST['pass1']) && is_string($_POST['pass1']) // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
            ? wp_unslash($_POST['pass1']) // phpcs:ignore WordPress.Security.NonceVerification.Missing,WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- plaintext required for zxcvbn + HIBP SHA-1; sanitize_text_field alters special chars; never stored or echoed
            : '';
        if ($password !== '') {
            $this->validatePassword($password, $user, $errors);
        }
        return $errors;
    }

    /**
     * Validate password strength on registration.
     *
     * @param \WP_Error $errors
     * @param string    $sanitizedUserLogin
     * @param string    $userEmail
     * @return \WP_Error
     */
    public function validateOnRegistration(\WP_Error $errors, string $sanitizedUserLogin, string $userEmail): \WP_Error
    {
        // phpcs:ignore WordPress.Security.NonceVerification.Missing -- WP core handles nonce for registration forms
        $password = isset($_POST['user_pass']) && is_string($_POST['user_pass']) // phpcs:ignore WordPress.Security.NonceVerification.Missing -- same
            ? wp_unslash($_POST['user_pass']) // phpcs:ignore WordPress.Security.NonceVerification.Missing,WordPress.Security.ValidatedSanitizedInput.InputNotSanitized -- plaintext required for zxcvbn + HIBP SHA-1; sanitize_text_field alters special chars; never stored or echoed
            : '';
        if ($password !== '') {
            // Build a stub user for role-based checks (role not known at registration).
            $userStub             = new \WP_User();
            $userStub->user_login = $sanitizedUserLogin;
            $userStub->user_email = $userEmail;
            $this->validatePassword($password, $userStub, $errors);
        }
        return $errors;
    }

    // -------------------------------------------------------------------------
    // Change-tracking hooks
    // -------------------------------------------------------------------------

    /**
     * After a profile update, if the password changed: update last-changed
     * timestamp, push old hash onto reuse history, clear force-change flag.
     *
     * @param int                 $userId
     * @param \WP_User            $oldUser
     * @return void
     */
    public function onProfileUpdate(int $userId, \WP_User $oldUser): void
    {
        $user = function_exists('get_userdata') ? get_userdata($userId) : false;
        if (!$user instanceof \WP_User) {
            return;
        }
        // WP core does not expose the plaintext on profile_update; we detect
        // a change by comparing the stored hash with the old user's hash.
        if (!function_exists('get_user_meta')) {
            return;
        }
        $newHash = $user->user_pass;
        $oldHash = $oldUser->user_pass;
        if ($newHash === $oldHash || $newHash === '') {
            return;
        }
        $this->recordPasswordChange($userId, $oldHash);
    }

    /**
     * After a password reset, record the change.
     *
     * @param \WP_User $user
     * @param string   $newPassword Plaintext (available only on this hook).
     * @return void
     */
    public function onPasswordReset(\WP_User $user, string $newPassword): void
    {
        $userId = (int) $user->ID;
        // Use the hash of the new password (WP has already stored it).
        $refreshed = function_exists('get_userdata') ? get_userdata($userId) : false;
        $newHash   = ($refreshed instanceof \WP_User) ? $refreshed->user_pass : '';
        if ($newHash !== '') {
            $this->recordPasswordChange($userId, $newHash);
        }
    }

    /**
     * After user registration, record the initial last-changed timestamp.
     *
     * @param int $userId
     * @return void
     */
    public function onUserRegister(int $userId): void
    {
        if (function_exists('update_user_meta')) {
            update_user_meta($userId, self::META_LAST_CHANGED, time());
        }
    }

    /**
     * Check password expiry + force-change on login (priority -2000).
     * If a force-change is needed, set the user-meta flag; the 2FA interstitial
     * or the next page load's admin_init will intercept.
     *
     * @param string   $userLogin
     * @param \WP_User $user
     * @return void
     */
    public function checkExpiryOnLogin(string $userLogin, \WP_User $user): void
    {
        $userId = (int) $user->ID;

        // Check CP-pushed force list.
        foreach ($this->policy->forcePasswordChange as $entry) {
            if (hash_equals($entry['user_login'], $userLogin)) {
                if (function_exists('update_user_meta')) {
                    update_user_meta($userId, self::META_FORCE_CHANGE, $entry['reason']);
                }
                break;
            }
        }

        // Check expiry. The force-list reason (set above) takes precedence:
        // only set 'expiry' if no flag is already recorded for this user.
        $maxAge = $this->policy->effectiveMaxAgeDays($user);
        if ($maxAge > 0) {
            $lastChanged = (int) get_user_meta($userId, self::META_LAST_CHANGED, true);
            if ($lastChanged === 0) {
                // Never recorded — set now and let a grace period expire naturally.
                if (function_exists('update_user_meta')) {
                    update_user_meta($userId, self::META_LAST_CHANGED, time());
                }
            } elseif (time() - $lastChanged > $maxAge * 86400) {
                // Only set 'expiry' if the force-list did not already set a more
                // specific reason above. This preserves operator intent.
                $existingFlag = function_exists('get_user_meta')
                    ? get_user_meta($userId, self::META_FORCE_CHANGE, true)
                    : '';
                if (!is_string($existingFlag) || $existingFlag === '') {
                    if (function_exists('update_user_meta')) {
                        update_user_meta($userId, self::META_FORCE_CHANGE, 'expiry');
                    }
                }
            }
        }
    }

    // -------------------------------------------------------------------------
    // Core validation logic
    // -------------------------------------------------------------------------

    /**
     * Run all enabled password requirements against the plaintext password.
     * Appends error codes to $errors; does not throw.
     *
     * @param string    $password Plaintext password.
     * @param \WP_User  $user     User context for role checks.
     * @param \WP_Error $errors   Error accumulator.
     * @return void
     */
    public function validatePassword(string $password, \WP_User $user, \WP_Error $errors): void
    {
        try {
            $this->checkStrength($password, $user, $errors);
            $this->checkCompromised($password, $user, $errors);
            $this->checkReuse($password, $user, $errors);
        } catch (\Throwable $e) {
            // Validation engine failure is non-fatal; fail-open to avoid lockout.
            // Log the error when WP_DEBUG is on so operators can diagnose failures.
            if (defined('WP_DEBUG') && WP_DEBUG) {
                // phpcs:ignore WordPress.PHP.DevelopmentFunctions.error_log_error_log -- WP_DEBUG-gated diagnostic log; never in production
                error_log('wpmgr-agent: PasswordPolicyModule::validatePassword caught exception: ' . $e->getMessage());
            }
        }
    }

    // -------------------------------------------------------------------------
    // Private requirement checks
    // -------------------------------------------------------------------------

    /**
     * Check minimum zxcvbn score.
     *
     * @param string    $password
     * @param \WP_User  $user
     * @param \WP_Error $errors
     * @return void
     */
    private function checkStrength(string $password, \WP_User $user, \WP_Error $errors): void
    {
        $minScore = $this->policy->effectiveMinZxcvbnScore($user);
        if ($minScore <= 0) {
            return;
        }

        $userInputs = $this->buildUserInputs($user);

        $zxcvbn = new Zxcvbn();
        $result  = $zxcvbn->passwordStrength($password, $userInputs);
        $score   = (int) ($result['score'] ?? 0);

        if ($score < $minScore) {
            $errors->add(
                'wpmgr_password_strength',
                esc_html__(
                    'The chosen password is too weak. Please use a stronger password with a mix of letters, numbers, and symbols.',
                    'wpmgr-agent'
                )
            );
        }
    }

    /**
     * Check whether the password appears in a breach corpus via the CP HIBP proxy.
     * Fail-open: any proxy error or empty response is treated as not-breached.
     *
     * @param string    $password
     * @param \WP_User  $user
     * @param \WP_Error $errors
     * @return void
     */
    private function checkCompromised(string $password, \WP_User $user, \WP_Error $errors): void
    {
        if (!$this->policy->blockCompromisedFor($user)) {
            return;
        }

        // k-anonymity: send only the 5-char SHA-1 prefix.
        $sha1   = strtoupper(sha1($password));
        $prefix = substr($sha1, 0, 5);
        $suffix = substr($sha1, 5);

        $body = $this->queryHibpProxy($prefix);
        if ($body === '') {
            // Fail-open: empty body means proxy returned nothing or errored.
            return;
        }

        // Match the 35-char suffix locally; never send the full hash.
        foreach (explode("\n", $body) as $line) {
            $line = trim($line);
            if ($line === '') {
                continue;
            }
            $parts = explode(':', $line, 2);
            if (count($parts) !== 2) {
                continue;
            }
            $lineSuffix = strtoupper(trim($parts[0]));
            $count      = (int) $parts[1];
            if ($count > 0 && hash_equals($suffix, $lineSuffix)) {
                $errors->add(
                    'wpmgr_password_compromised',
                    esc_html__(
                        'This password has appeared in a data breach and cannot be used. Please choose a different password.',
                        'wpmgr-agent'
                    )
                );
                return;
            }
        }
    }

    /**
     * Check password-reuse block against the stored history.
     *
     * @param string    $password
     * @param \WP_User  $user
     * @param \WP_Error $errors
     * @return void
     */
    private function checkReuse(string $password, \WP_User $user, \WP_Error $errors): void
    {
        $blockCount = $this->policy->passwordReuseBlockCount;
        if ($blockCount <= 0) {
            return;
        }

        $userId  = (int) $user->ID;
        $history = function_exists('get_user_meta')
            ? get_user_meta($userId, self::META_HISTORY, true)
            : [];
        if (!is_array($history) || $history === []) {
            return;
        }

        $recent = array_slice(array_reverse($history), 0, $blockCount);
        foreach ($recent as $hash) {
            if (!is_string($hash)) {
                continue;
            }
            if (function_exists('wp_check_password') && wp_check_password($password, $hash, $userId)) {
                $errors->add(
                    'wpmgr_password_reuse',
                    esc_html(
                        sprintf(
                            // translators: %d is the number of previous passwords that cannot be reused.
                            __('You cannot reuse any of your last %d passwords. Please choose a different password.', 'wpmgr-agent'),
                            $blockCount
                        )
                    )
                );
                return;
            }
        }
    }

    /**
     * Query the CP HIBP range proxy. Returns the raw SUFFIX:COUNT body.
     * Returns '' on any error (fail-open contract).
     *
     * @param string $prefix 5-char uppercase hex SHA-1 prefix.
     * @return string
     */
    private function queryHibpProxy(string $prefix): string
    {
        $cpUrl = $this->settings->controlPlaneUrl();
        if ($cpUrl === '') {
            return '';
        }

        $path = '/api/v1/security/hibp/range/' . rawurlencode($prefix);
        $url  = rtrim($cpUrl, '/') . $path;

        try {
            $headers  = $this->signer->signHeaders('GET', $path, '');
            $response = wp_remote_get(
                $url,
                [
                    'headers'   => $headers,
                    'timeout'   => 5,
                    'sslverify' => true,
                ]
            );

            if (is_wp_error($response)) {
                return '';
            }

            $code = (int) wp_remote_retrieve_response_code($response);
            if ($code !== 200) {
                return '';
            }

            $body = wp_remote_retrieve_body($response);
            return is_string($body) ? $body : '';
        } catch (\Throwable $e) {
            return '';
        }
    }

    /**
     * Build the user-inputs array for zxcvbn to penalise obvious guesses.
     *
     * @param \WP_User $user
     * @return list<string>
     */
    private function buildUserInputs(\WP_User $user): array
    {
        $inputs = [];
        if (isset($user->user_login) && $user->user_login !== '') {
            $inputs[] = $user->user_login;
        }
        if (isset($user->user_email) && $user->user_email !== '') {
            $inputs[] = $user->user_email;
            // Email local part.
            $atPos = strpos($user->user_email, '@');
            if ($atPos !== false) {
                $inputs[] = substr($user->user_email, 0, $atPos);
            }
        }
        if (isset($user->display_name) && $user->display_name !== '') {
            $inputs[] = $user->display_name;
        }
        if (function_exists('get_bloginfo')) {
            $siteName = get_bloginfo('name');
            if ($siteName !== '') {
                $inputs[] = $siteName;
            }
        }
        return array_values(array_unique($inputs));
    }

    /**
     * Record a password change: update last-changed timestamp, push old hash
     * to reuse history, clear the force-change flag.
     *
     * @param int    $userId
     * @param string $oldHash Previous password hash to add to history.
     * @return void
     */
    private function recordPasswordChange(int $userId, string $oldHash): void
    {
        if (!function_exists('update_user_meta') || !function_exists('get_user_meta')) {
            return;
        }

        // Update last-changed.
        update_user_meta($userId, self::META_LAST_CHANGED, time());

        // Clear force-change flag.
        delete_user_meta($userId, self::META_FORCE_CHANGE);

        // Push old hash to history (keep last N).
        $maxN   = max(1, $this->policy->passwordReuseBlockCount);
        $history = get_user_meta($userId, self::META_HISTORY, true);
        if (!is_array($history)) {
            $history = [];
        }
        $history[] = $oldHash;
        // Keep only the last maxN entries.
        if (count($history) > $maxN) {
            $history = array_slice($history, -$maxN);
        }
        update_user_meta($userId, self::META_HISTORY, $history);
    }
}
