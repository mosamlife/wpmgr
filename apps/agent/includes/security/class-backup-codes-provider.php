<?php
/**
 * BackupCodesProvider — single-use backup/recovery codes for site-user 2FA.
 *
 * Code design:
 *   - 10 codes per set, each 10 digits (random_int), shown once on generation.
 *   - Stored as an array of wp_hash_password() digests in user-meta.
 *   - Each code is deleted on use (delete-on-use, never burn-all).
 *   - "Regenerate codes" replaces the entire set.
 *   - Constant-time comparison via wp_check_password() (argon2id or bcrypt
 *     depending on the WP installation).
 *
 * @package WPMgr\Agent\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Security;

/**
 * Single-use backup-code 2FA provider.
 */
final class BackupCodesProvider implements SiteTwoFactorProvider
{
    /** User-meta key for the stored code hashes. */
    public const META_CODES = 'wpmgr_2fa_backup_codes';

    /** Number of codes in a set. */
    public const CODE_COUNT = 10;

    /** Digits per code. */
    private const CODE_DIGITS = 10;

    /** {@inheritDoc} */
    public function key(): string
    {
        return 'backup';
    }

    /** {@inheritDoc} */
    public function label(): string
    {
        return __('Backup Code', 'wpmgr-agent');
    }

    /** {@inheritDoc} */
    public function isConfiguredFor(\WP_User $user): bool
    {
        if (!function_exists('get_user_meta')) {
            return false;
        }
        $codes = get_user_meta((int) $user->ID, self::META_CODES, true);
        return is_array($codes) && count($codes) > 0;
    }

    /** {@inheritDoc} — no pre-render action for backup codes. */
    public function preRender(\WP_User $user): void
    {
        // No-op.
    }

    /** {@inheritDoc} */
    public function renderForm(\WP_User $user): string
    {
        $field       = esc_attr('wpmgr_backup_code');
        $placeholder = esc_attr__('10-digit backup code', 'wpmgr-agent');
        return '<p><label for="' . $field . '">'
            . esc_html__('Enter a backup code:', 'wpmgr-agent')
            . '</label>'
            . '<br><input type="text" id="' . $field . '" name="' . $field . '" '
            . 'autocomplete="off" inputmode="numeric" pattern="[0-9]{10}" '
            . 'maxlength="10" placeholder="' . $placeholder . '" required autofocus></p>';
    }

    /** {@inheritDoc} */
    public function validate(\WP_User $user, array $input): bool
    {
        $code = isset($input['wpmgr_backup_code'])
            ? preg_replace('/\D/', '', $input['wpmgr_backup_code'])
            : '';
        if (!is_string($code) || strlen($code) !== self::CODE_DIGITS) {
            return false;
        }

        $userId = (int) $user->ID;
        if (!function_exists('get_user_meta') || !function_exists('wp_check_password')) {
            return false;
        }

        $stored = get_user_meta($userId, self::META_CODES, true);
        if (!is_array($stored) || $stored === []) {
            return false;
        }

        $matchIndex = -1;
        foreach ($stored as $idx => $hash) {
            if (is_string($hash) && wp_check_password($code, $hash, $userId)) {
                $matchIndex = (int) $idx;
                break;
            }
        }

        if ($matchIndex < 0) {
            return false;
        }

        // Delete the used code (single-use burn).
        unset($stored[$matchIndex]);
        if (function_exists('update_user_meta')) {
            update_user_meta($userId, self::META_CODES, array_values($stored));
        }

        return true;
    }

    // -------------------------------------------------------------------------
    // Enrollment helpers
    // -------------------------------------------------------------------------

    /**
     * Generate a new set of backup codes, store their hashes, and return the
     * plaintext codes (shown ONCE to the user).
     *
     * @param \WP_User $user
     * @return list<string> Plaintext codes (display once; not stored in plaintext).
     */
    public function generateAndStore(\WP_User $user): array
    {
        if (!function_exists('wp_hash_password') || !function_exists('update_user_meta')) {
            return [];
        }

        $codes  = [];
        $hashes = [];
        for ($i = 0; $i < self::CODE_COUNT; $i++) {
            $code     = str_pad((string) random_int(0, 9999999999), self::CODE_DIGITS, '0', STR_PAD_LEFT);
            $codes[]  = $code;
            $hashes[] = wp_hash_password($code);
        }

        update_user_meta((int) $user->ID, self::META_CODES, $hashes);
        return $codes;
    }

    /**
     * Count remaining codes for a user.
     *
     * @param \WP_User $user
     * @return int
     */
    public function remainingCount(\WP_User $user): int
    {
        if (!function_exists('get_user_meta')) {
            return 0;
        }
        $codes = get_user_meta((int) $user->ID, self::META_CODES, true);
        return is_array($codes) ? count($codes) : 0;
    }

    /**
     * Delete all stored codes (used when operator clears 2FA for a user).
     *
     * @param \WP_User $user
     * @return void
     */
    public function clearCodes(\WP_User $user): void
    {
        if (function_exists('delete_user_meta')) {
            delete_user_meta((int) $user->ID, self::META_CODES);
        }
    }
}
