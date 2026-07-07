<?php
/**
 * SyncSecurityPolicyCommand — receives the site-user authentication policy
 * from the control plane and atomically applies it on the WordPress site.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/sync_security_policy
 *   Authorization: Bearer <Ed25519 JWT with cmd="sync_security_policy", aud=<siteId>>
 *   Content-Type: application/json
 *   Body: {
 *     "policy": {
 *       "two_factor_enabled":              bool,
 *       "two_factor_methods":              string[],
 *       "two_factor_required_roles":       string[],
 *       "two_factor_grace_logins":         int,
 *       "two_factor_remember_device_days": int,
 *       "block_xmlrpc_for_2fa_users":      bool,
 *       "password_min_zxcvbn_score":       int (0-4),
 *       "password_min_zxcvbn_roles":       string[],
 *       "password_block_compromised":      bool,
 *       "password_reuse_block_count":      int,
 *       "password_max_age_days":           int,
 *       "password_expiry_roles":           string[],
 *       "hide_backend_enabled":            bool,
 *       "hide_backend_slug":               string,
 *       "hide_backend_redirect":           string
 *     },
 *     "groups": [ { "role": str, "require_2fa"?: bool, "allowed_methods"?: str[],
 *                   "min_zxcvbn_score"?: int, "block_compromised"?: bool, "max_age_days"?: int } ],
 *     "force_password_change": [ { "user_login": str, "reason": str } ]
 *   }
 *
 * Response (200 OK, wrapped by Router):
 *   {
 *     "ok": true,
 *     "detail": "applied",
 *     "enrollment_summary": {
 *       "per_role": {
 *         "administrator": { "enrolled": 2, "required": 2, "total": 3 }
 *       }
 *     }
 *   }
 *   per_role serializes as {} (a JSON object, never a bare []) when there is
 *   nothing to report — e.g. 2FA is off — so the CP can always unmarshal it
 *   into map[string]RoleEnrollment.
 *
 * Auth: Router's permission_callback enforces Ed25519 + anti-replay JWT
 * before execute() is called (Connector::verifyCommand).
 *
 * Full-snapshot replace on every push (no diffing) — mirrors
 * SyncSecurityHardeningCommand / HardeningConfig discipline.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Security\BackupCodesProvider;
use WPMgr\Agent\Security\EmailCodeProvider;
use WPMgr\Agent\Security\SecurityPolicy;
use WPMgr\Agent\Security\TotpProvider;

/**
 * Persists and applies the CP-pushed site authentication policy.
 */
final class SyncSecurityPolicyCommand implements CommandInterface
{
    /** {@inheritDoc} */
    public function name(): string
    {
        return 'sync_security_policy';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused; Router enforced aud+cmd).
     * @param array<string,mixed> $params Decoded JSON body.
     * @return array{ok:bool,detail:string,enrollment_summary?:array<string,mixed>}
     */
    public function execute(array $claims, array $params): array
    {
        // Top-level structure validation: policy must be an object if present.
        if (array_key_exists('policy', $params) && !is_array($params['policy'])) {
            return ['ok' => false, 'detail' => 'policy must be an object'];
        }

        // groups must be an array if present.
        if (array_key_exists('groups', $params) && !is_array($params['groups'])) {
            return ['ok' => false, 'detail' => 'groups must be an array'];
        }

        // force_password_change must be an array if present.
        if (array_key_exists('force_password_change', $params) && !is_array($params['force_password_change'])) {
            return ['ok' => false, 'detail' => 'force_password_change must be an array'];
        }

        try {
            // Build and validate the full policy; safe defaults for any missing/
            // invalid field (never throws, always returns a valid object).
            $policy = SecurityPolicy::fromArray($params);

            // Persist atomically.
            if (!function_exists('update_option') || !function_exists('wp_json_encode')) {
                return ['ok' => false, 'detail' => 'WordPress functions not available'];
            }

            $encoded = wp_json_encode($policy->toArray());
            if ($encoded === false) {
                return ['ok' => false, 'detail' => 'failed to encode policy'];
            }

            update_option(SecurityPolicy::OPTION_KEY, $encoded, false);

            // Apply force-password-change flags to the named users.
            $this->applyForcePasswordChange($policy);

            // Build the enrollment summary.
            $enrollmentSummary = $this->buildEnrollmentSummary($policy);
        } catch (\Throwable $e) {
            // Never let the policy sync fatal the request.
            return ['ok' => false, 'detail' => 'failed to apply security policy'];
        }

        return [
            'ok'                 => true,
            'detail'             => 'applied',
            'enrollment_summary' => $enrollmentSummary,
        ];
    }

    // -------------------------------------------------------------------------
    // Private helpers
    // -------------------------------------------------------------------------

    /**
     * Mark the force-password-change users in their user-meta so they are
     * intercepted on next login.
     *
     * @param SecurityPolicy $policy
     * @return void
     */
    private function applyForcePasswordChange(SecurityPolicy $policy): void
    {
        if ($policy->forcePasswordChange === [] || !function_exists('get_user_by')) {
            return;
        }

        foreach ($policy->forcePasswordChange as $entry) {
            $login = $entry['user_login'];
            $reason = $entry['reason'];

            $user = get_user_by('login', $login);
            if (!$user instanceof \WP_User) {
                continue;
            }

            if (function_exists('update_user_meta')) {
                update_user_meta(
                    (int) $user->ID,
                    \WPMgr\Agent\Security\PasswordPolicyModule::META_FORCE_CHANGE,
                    sanitize_key($reason)
                );
            }
        }
    }

    /**
     * Build per-role enrollment summary for the response.
     * Counts enrolled vs required vs total for each required role.
     *
     * This function iterates WP users — it is bounded by get_users()
     * which WordPress limits to a reasonable page size. The result is
     * aggregate counts only; no usernames or secrets leave the site.
     *
     * @param SecurityPolicy $policy
     * @return array<string,mixed>
     */
    private function buildEnrollmentSummary(SecurityPolicy $policy): array
    {
        $perRole = [];

        if ($policy->twoFactorEnabled && function_exists('get_users')) {
            // Collect all roles that appear in the required list or in groups with require_2fa=true.
            $requiredRoles = $policy->twoFactorRequiredRoles;
            foreach ($policy->groups as $group) {
                if ($group['require_2fa'] === true && !in_array($group['role'], $requiredRoles, true)) {
                    $requiredRoles[] = $group['role'];
                }
            }

            foreach ($requiredRoles as $role) {
                // Fetch users with this role (bounded by WordPress default limit).
                $users = get_users([
                    'role'   => $role,
                    'fields' => ['ID'],
                    'number' => 500, // cap for performance
                ]);

                if (!is_array($users)) {
                    continue;
                }

                $total    = count($users);
                $enrolled = 0;

                foreach ($users as $userRow) {
                    $uid = is_object($userRow) && isset($userRow->ID)
                        ? (int) $userRow->ID
                        : (int) $userRow;

                    if ($this->isEnrolled($uid)) {
                        $enrolled++;
                    }
                }

                $perRole[$role] = [
                    'enrolled' => $enrolled,
                    'required' => $total,
                    'total'    => $total,
                ];
            }
        }

        // Cast to object: an empty PHP array json_encodes as `[]`, which the CP
        // cannot unmarshal into its Go map[string]RoleEnrollment field. Casting
        // makes the empty case serialize as `{}` while the populated case is
        // unaffected (the inner per-role arrays are always non-empty assoc
        // arrays and already encode as JSON objects).
        return ['per_role' => (object) $perRole];
    }

    /**
     * Check whether a user has at least one 2FA method configured.
     *
     * @param int $userId
     * @return bool
     */
    private function isEnrolled(int $userId): bool
    {
        if (!function_exists('get_user_meta')) {
            return false;
        }

        // TOTP secret set.
        $totpSecret = get_user_meta($userId, TotpProvider::META_SECRET, true);
        if (is_string($totpSecret) && $totpSecret !== '') {
            return true;
        }

        // Backup codes set.
        $backupCodes = get_user_meta($userId, BackupCodesProvider::META_CODES, true);
        if (is_array($backupCodes) && $backupCodes !== []) {
            return true;
        }

        // Email provider is always "enrolled" for users with an email address,
        // but we don't count it here — the dashboard surface wants users who have
        // actively configured TOTP or backup codes.

        return false;
    }
}
