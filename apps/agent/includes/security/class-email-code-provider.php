<?php
/**
 * EmailCodeProvider — one-time code delivered via the per-site SMTP transport.
 *
 * Code generation:
 *   - 8-digit numeric code, generated with random_int().
 *   - Stored as wp_hash(code) + issued_at + expiry in user-meta (array of tokens,
 *     multiple outstanding codes allowed so a stale email does not block).
 *   - 15-minute TTL; expired codes are pruned on read and on success.
 *   - All stored codes are burned on a successful validation (not just the used one)
 *     to prevent re-use of pending codes.
 *
 * @package WPMgr\Agent\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Security;

/**
 * Email one-time-code 2FA provider.
 */
final class EmailCodeProvider implements SiteTwoFactorProvider
{
    /** User-meta key for the pending email code records. */
    public const META_TOKENS = 'wpmgr_2fa_email_token';

    /** Code TTL in seconds. */
    private const TTL = 900; // 15 minutes

    /** Code length (digits). */
    private const LENGTH = 8;

    /** {@inheritDoc} */
    public function key(): string
    {
        return 'email';
    }

    /** {@inheritDoc} */
    public function label(): string
    {
        return __('Email Code', 'wpmgr-agent');
    }

    /** {@inheritDoc} */
    public function isConfiguredFor(\WP_User $user): bool
    {
        // Email provider is always "configured" as long as the user has an email address.
        return isset($user->user_email) && $user->user_email !== '';
    }

    /** {@inheritDoc} — email provider sends the code here. */
    public function preRender(\WP_User $user): void
    {
        $this->sendCode($user);
    }

    /** {@inheritDoc} */
    public function renderForm(\WP_User $user): string
    {
        $field       = esc_attr('wpmgr_email_code');
        $email       = isset($user->user_email) ? esc_html($user->user_email) : '';
        $placeholder = esc_attr__('8-digit code', 'wpmgr-agent');
        return '<p>'
            . esc_html__('A verification code was sent to', 'wpmgr-agent')
            . ' ' . $email . '.</p>'
            . '<p><label for="' . $field . '">'
            . esc_html__('Enter the code:', 'wpmgr-agent')
            . '</label>'
            . '<br><input type="text" id="' . $field . '" name="' . $field . '" '
            . 'autocomplete="one-time-code" inputmode="numeric" pattern="[0-9]{8}" '
            . 'maxlength="8" placeholder="' . $placeholder . '" required autofocus></p>';
    }

    /** {@inheritDoc} */
    public function validate(\WP_User $user, array $input): bool
    {
        $code = isset($input['wpmgr_email_code'])
            ? preg_replace('/\D/', '', $input['wpmgr_email_code'])
            : '';
        if (!is_string($code) || strlen($code) !== self::LENGTH) {
            return false;
        }

        $userId = (int) $user->ID;
        if (!function_exists('get_user_meta') || !function_exists('wp_hash')) {
            return false;
        }

        $tokens = $this->loadTokens($userId);
        $now    = time();
        $valid  = false;
        $hash   = wp_hash($code);

        foreach ($tokens as $token) {
            // Prune expired.
            if (!isset($token['expires']) || $token['expires'] < $now) {
                continue;
            }
            if (isset($token['hash']) && hash_equals($token['hash'], $hash)) {
                $valid = true;
                break;
            }
        }

        if ($valid) {
            // Burn all pending codes on success.
            $this->clearTokens($userId);
        }

        return $valid;
    }

    // -------------------------------------------------------------------------
    // Internal helpers
    // -------------------------------------------------------------------------

    /**
     * Generate and send a new email code for the user.
     *
     * @param \WP_User $user
     * @return bool True if the code was sent successfully.
     */
    public function sendCode(\WP_User $user): bool
    {
        if (!function_exists('wp_mail') || !function_exists('wp_hash')) {
            return false;
        }
        $userId = (int) $user->ID;
        $email  = isset($user->user_email) ? $user->user_email : '';
        if ($email === '') {
            return false;
        }

        $code = str_pad((string) random_int(0, 99999999), self::LENGTH, '0', STR_PAD_LEFT);
        $hash = wp_hash($code);

        // Append to the token list (allow multiple outstanding).
        $tokens   = $this->loadTokens($userId);
        $tokens[] = ['hash' => $hash, 'expires' => time() + self::TTL];
        $this->storeTokens($userId, $tokens);

        $siteName = function_exists('get_bloginfo') ? get_bloginfo('name') : 'WordPress';
        $subject  = sprintf(
            /* translators: %s: the site name. */
            __('[%s] Two-factor verification code', 'wpmgr-agent'),
            $siteName
        );
        $message  = sprintf(
            /* translators: %s: the one-time verification code. */
            __("Your two-factor verification code is: %s\n\nThis code expires in 15 minutes.", 'wpmgr-agent'),
            $code
        );

        return (bool) wp_mail($email, $subject, $message);
    }

    /**
     * Load current token records for a user.
     *
     * @param int $userId
     * @return list<array{hash:string,expires:int}>
     */
    private function loadTokens(int $userId): array
    {
        if (!function_exists('get_user_meta')) {
            return [];
        }
        $raw = get_user_meta($userId, self::META_TOKENS, true);
        if (!is_array($raw)) {
            return [];
        }
        $now  = time();
        $keep = [];
        foreach ($raw as $t) {
            if (is_array($t) && isset($t['hash'], $t['expires']) && $t['expires'] >= $now) {
                $keep[] = ['hash' => (string) $t['hash'], 'expires' => (int) $t['expires']];
            }
        }
        return $keep;
    }

    /**
     * Persist the token list.
     *
     * @param int                                       $userId
     * @param list<array{hash:string,expires:int}>      $tokens
     * @return void
     */
    private function storeTokens(int $userId, array $tokens): void
    {
        if (!function_exists('update_user_meta')) {
            return;
        }
        update_user_meta($userId, self::META_TOKENS, $tokens);
    }

    /**
     * Delete all tokens (burn on success or on password change).
     *
     * @param int $userId
     * @return void
     */
    private function clearTokens(int $userId): void
    {
        if (function_exists('delete_user_meta')) {
            delete_user_meta($userId, self::META_TOKENS);
        }
    }
}
