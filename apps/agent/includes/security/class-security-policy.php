<?php
/**
 * SecurityPolicy — validated value object for the site-user authentication
 * policy pushed by the control plane via sync_security_policy.
 *
 * Every field defaults to the OFF/safe value so a fresh push with missing
 * fields never activates any enforcement. Mirrors the discipline established
 * by HardeningConfig for the hardening command.
 *
 * Wire contract:
 *   POST /wp-json/wpmgr/v1/command/sync_security_policy
 *   Body: {
 *     "policy": { <site-level knobs from §1.1> },
 *     "groups": [ { "role": "administrator", "require_2fa": true, ... } ],
 *     "force_password_change": [ { "user_login": "jane", "reason": "admin_reset" } ]
 *   }
 *
 * @package WPMgr\Agent\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Security;

/**
 * Immutable, validated value object for the site authentication policy.
 */
final class SecurityPolicy
{
    /** wp-options key where the policy JSON is stored. */
    public const OPTION_KEY = 'wpmgr_security_policy';

    /** Allowed 2FA provider keys. */
    public const ALLOWED_METHODS = ['totp', 'email', 'backup'];

    // -------------------------------------------------------------------------
    // Site-level policy properties
    // -------------------------------------------------------------------------

    /** Master switch: site-user 2FA. Default: off. */
    public readonly bool $twoFactorEnabled;

    /**
     * Allowed 2FA providers (subset of ALLOWED_METHODS).
     *
     * @var list<string>
     */
    public readonly array $twoFactorMethods;

    /**
     * WP roles that must use 2FA.
     *
     * @var list<string>
     */
    public readonly array $twoFactorRequiredRoles;

    /** Allowed logins before required-but-unenrolled user is forced to enroll. */
    public readonly int $twoFactorGraceLogins;

    /** Trusted-device TTL in days; 0 = disabled. */
    public readonly int $twoFactorRememberDeviceDays;

    /** Block XML-RPC for users with 2FA configured. */
    public readonly bool $blockXmlrpcFor2faUsers;

    /** Minimum zxcvbn score on password set/change; 0 = disabled. */
    public readonly int $passwordMinZxcvbnScore;

    /**
     * Roles the password-strength rule applies to; empty = all.
     *
     * @var list<string>
     */
    public readonly array $passwordMinZxcvbnRoles;

    /** Reject HIBP-breached passwords on set/change. */
    public readonly bool $passwordBlockCompromised;

    /** Block reusing the last N passwords; 0 = off. */
    public readonly int $passwordReuseBlockCount;

    /** Force change after N days since last change; 0 = off. */
    public readonly int $passwordMaxAgeDays;

    /**
     * Roles the password-expiry rule applies to; empty = all.
     *
     * @var list<string>
     */
    public readonly array $passwordExpiryRoles;

    /** Master switch: secret login slug. */
    public readonly bool $hideBackendEnabled;

    /** Secret login slug (validated ^[a-z0-9-]{4,64}$). */
    public readonly string $hideBackendSlug;

    /** Where to send logged-out hits on wp-login/wp-admin; empty = 404. */
    public readonly string $hideBackendRedirect;

    // -------------------------------------------------------------------------
    // Per-group overrides
    // -------------------------------------------------------------------------

    /**
     * Per-role group overrides. Each entry:
     *   role: string, require_2fa: bool|null, allowed_methods: string[]|null,
     *   min_zxcvbn_score: int|null, block_compromised: bool|null, max_age_days: int|null
     *
     * @var list<array{role:string,require_2fa:bool|null,allowed_methods:list<string>|null,min_zxcvbn_score:int|null,block_compromised:bool|null,max_age_days:int|null}>
     */
    public readonly array $groups;

    // -------------------------------------------------------------------------
    // Force-password-change list
    // -------------------------------------------------------------------------

    /**
     * Users to force into a password-change on next login.
     * Each entry: { user_login: string, reason: string }
     *
     * @var list<array{user_login:string,reason:string}>
     */
    public readonly array $forcePasswordChange;

    /**
     * @param bool           $twoFactorEnabled
     * @param list<string>   $twoFactorMethods
     * @param list<string>   $twoFactorRequiredRoles
     * @param int            $twoFactorGraceLogins
     * @param int            $twoFactorRememberDeviceDays
     * @param bool           $blockXmlrpcFor2faUsers
     * @param int            $passwordMinZxcvbnScore
     * @param list<string>   $passwordMinZxcvbnRoles
     * @param bool           $passwordBlockCompromised
     * @param int            $passwordReuseBlockCount
     * @param int            $passwordMaxAgeDays
     * @param list<string>   $passwordExpiryRoles
     * @param bool           $hideBackendEnabled
     * @param string         $hideBackendSlug
     * @param string         $hideBackendRedirect
     * @param list<array{role:string,require_2fa:bool|null,allowed_methods:list<string>|null,min_zxcvbn_score:int|null,block_compromised:bool|null,max_age_days:int|null}> $groups
     * @param list<array{user_login:string,reason:string}> $forcePasswordChange
     */
    public function __construct(
        bool   $twoFactorEnabled,
        array  $twoFactorMethods,
        array  $twoFactorRequiredRoles,
        int    $twoFactorGraceLogins,
        int    $twoFactorRememberDeviceDays,
        bool   $blockXmlrpcFor2faUsers,
        int    $passwordMinZxcvbnScore,
        array  $passwordMinZxcvbnRoles,
        bool   $passwordBlockCompromised,
        int    $passwordReuseBlockCount,
        int    $passwordMaxAgeDays,
        array  $passwordExpiryRoles,
        bool   $hideBackendEnabled,
        string $hideBackendSlug,
        string $hideBackendRedirect,
        array  $groups,
        array  $forcePasswordChange
    ) {
        $this->twoFactorEnabled            = $twoFactorEnabled;
        $this->twoFactorMethods            = $twoFactorMethods;
        $this->twoFactorRequiredRoles      = $twoFactorRequiredRoles;
        $this->twoFactorGraceLogins        = $twoFactorGraceLogins;
        $this->twoFactorRememberDeviceDays = $twoFactorRememberDeviceDays;
        $this->blockXmlrpcFor2faUsers      = $blockXmlrpcFor2faUsers;
        $this->passwordMinZxcvbnScore      = $passwordMinZxcvbnScore;
        $this->passwordMinZxcvbnRoles      = $passwordMinZxcvbnRoles;
        $this->passwordBlockCompromised    = $passwordBlockCompromised;
        $this->passwordReuseBlockCount     = $passwordReuseBlockCount;
        $this->passwordMaxAgeDays          = $passwordMaxAgeDays;
        $this->passwordExpiryRoles         = $passwordExpiryRoles;
        $this->hideBackendEnabled          = $hideBackendEnabled;
        $this->hideBackendSlug             = $hideBackendSlug;
        $this->hideBackendRedirect         = $hideBackendRedirect;
        $this->groups                      = $groups;
        $this->forcePasswordChange         = $forcePasswordChange;
    }

    /**
     * Build a SecurityPolicy from the raw decoded JSON body. Every missing or
     * invalid field is coerced to its safe off-default. Never throws.
     *
     * @param array<string,mixed> $raw Top-level decoded body (policy/groups/force_password_change).
     * @return self
     */
    public static function fromArray(array $raw): self
    {
        $p = isset($raw['policy']) && is_array($raw['policy']) ? $raw['policy'] : [];

        // 2FA knobs
        $twoFactorEnabled  = (bool) ($p['two_factor_enabled'] ?? false);
        $twoFactorMethods  = self::coerceStringList(
            $p['two_factor_methods'] ?? self::ALLOWED_METHODS,
            self::ALLOWED_METHODS
        );
        $twoFactorRoles    = self::coerceRoleList($p['two_factor_required_roles'] ?? []);
        $graceLogins       = max(0, (int) ($p['two_factor_grace_logins'] ?? 3));
        $rememberDays      = max(0, (int) ($p['two_factor_remember_device_days'] ?? 30));
        $blockXmlrpc       = (bool) ($p['block_xmlrpc_for_2fa_users'] ?? true);

        // Password knobs
        $minScore          = min(4, max(0, (int) ($p['password_min_zxcvbn_score'] ?? 0)));
        $minScoreRoles     = self::coerceRoleList($p['password_min_zxcvbn_roles'] ?? []);
        $blockCompromised  = (bool) ($p['password_block_compromised'] ?? false);
        $reuseCount        = max(0, (int) ($p['password_reuse_block_count'] ?? 0));
        $maxAgeDays        = max(0, (int) ($p['password_max_age_days'] ?? 0));
        $expiryRoles       = self::coerceRoleList($p['password_expiry_roles'] ?? []);

        // Hide-backend knobs
        $hideEnabled       = (bool) ($p['hide_backend_enabled'] ?? false);
        $hideSlug          = self::coerceSlug($p['hide_backend_slug'] ?? '');
        $hideRedirect      = self::coerceUrl($p['hide_backend_redirect'] ?? '');

        // Disable hide-backend if the slug is invalid when it's supposed to be enabled.
        if ($hideEnabled && $hideSlug === '') {
            $hideEnabled = false;
        }

        // Groups
        $groupsRaw = isset($raw['groups']) && is_array($raw['groups']) ? $raw['groups'] : [];
        $groups    = self::coerceGroups($groupsRaw);

        // Force-password-change list
        $forceRaw = isset($raw['force_password_change']) && is_array($raw['force_password_change'])
            ? $raw['force_password_change']
            : [];
        $forceList = self::coerceForceList($forceRaw);

        return new self(
            $twoFactorEnabled,
            $twoFactorMethods,
            $twoFactorRoles,
            $graceLogins,
            $rememberDays,
            $blockXmlrpc,
            $minScore,
            $minScoreRoles,
            $blockCompromised,
            $reuseCount,
            $maxAgeDays,
            $expiryRoles,
            $hideEnabled,
            $hideSlug,
            $hideRedirect,
            $groups,
            $forceList
        );
    }

    /**
     * Return an all-off policy (default-OFF invariant).
     *
     * @return self
     */
    public static function defaults(): self
    {
        return new self(
            false,
            self::ALLOWED_METHODS,
            [],
            3,
            30,
            true,
            0,
            [],
            false,
            0,
            0,
            [],
            false,
            '',
            '',
            [],
            []
        );
    }

    /**
     * Load the stored policy from wp-options, returning defaults on any error.
     *
     * @return self
     */
    public static function load(): self
    {
        if (!function_exists('get_option')) {
            return self::defaults();
        }
        $raw = get_option(self::OPTION_KEY, '');
        if (!is_string($raw) || $raw === '') {
            return self::defaults();
        }
        $decoded = json_decode($raw, true);
        if (!is_array($decoded)) {
            return self::defaults();
        }
        return self::fromArray($decoded);
    }

    /**
     * Serialize to an array for wp-options storage.
     *
     * @return array<string,mixed>
     */
    public function toArray(): array
    {
        return [
            'policy' => [
                'two_factor_enabled'              => $this->twoFactorEnabled,
                'two_factor_methods'              => $this->twoFactorMethods,
                'two_factor_required_roles'       => $this->twoFactorRequiredRoles,
                'two_factor_grace_logins'         => $this->twoFactorGraceLogins,
                'two_factor_remember_device_days' => $this->twoFactorRememberDeviceDays,
                'block_xmlrpc_for_2fa_users'      => $this->blockXmlrpcFor2faUsers,
                'password_min_zxcvbn_score'       => $this->passwordMinZxcvbnScore,
                'password_min_zxcvbn_roles'       => $this->passwordMinZxcvbnRoles,
                'password_block_compromised'      => $this->passwordBlockCompromised,
                'password_reuse_block_count'      => $this->passwordReuseBlockCount,
                'password_max_age_days'           => $this->passwordMaxAgeDays,
                'password_expiry_roles'           => $this->passwordExpiryRoles,
                'hide_backend_enabled'            => $this->hideBackendEnabled,
                'hide_backend_slug'               => $this->hideBackendSlug,
                'hide_backend_redirect'           => $this->hideBackendRedirect,
            ],
            'groups'               => $this->groups,
            'force_password_change' => $this->forcePasswordChange,
        ];
    }

    /**
     * Resolve the effective 2FA required-ness for a WP_User.
     * Returns true when any matching site-level role or group requires 2FA.
     *
     * @param \WP_User $user WordPress user object.
     * @return bool
     */
    public function requires2fa(\WP_User $user): bool
    {
        if (!$this->twoFactorEnabled) {
            return false;
        }
        $roles = $this->userRoles($user);

        // Site-level required roles
        foreach ($roles as $role) {
            if (in_array($role, $this->twoFactorRequiredRoles, true)) {
                return true;
            }
        }

        // Per-group overrides — any matching group with require_2fa = true wins
        foreach ($this->groups as $group) {
            if (in_array($group['role'], $roles, true) && $group['require_2fa'] === true) {
                return true;
            }
        }

        return false;
    }

    /**
     * Get the effective allowed 2FA methods for a WP_User.
     * The intersection of site methods and any matching group restriction.
     *
     * @param \WP_User $user
     * @return list<string>
     */
    public function allowedMethodsFor(\WP_User $user): array
    {
        $methods = $this->twoFactorMethods;
        $roles   = $this->userRoles($user);

        foreach ($this->groups as $group) {
            if (in_array($group['role'], $roles, true) && is_array($group['allowed_methods'])) {
                // Intersection: only methods allowed at both levels.
                $methods = array_values(array_intersect($methods, $group['allowed_methods']));
            }
        }

        return $methods === [] ? ['email'] : $methods;
    }

    /**
     * Get the effective minimum zxcvbn score for a user's role.
     *
     * Accepts an object rather than a \WP_User because the
     * `user_profile_update_errors` chain reaches here carrying whatever core
     * passed, and core passes a bare stdClass. Callers on that path should
     * resolve through ProfileUpdateUser::resolve() first; userRoles() below is
     * the backstop for any that do not.
     *
     * @param object $user \WP_User, or the \stdClass core builds in edit_user().
     * @return int
     */
    public function effectiveMinZxcvbnScore(object $user): int
    {
        $roles = $this->userRoles($user);

        // Check if the user's role is in passwordMinZxcvbnRoles (empty = all).
        $appliesAtSiteLevel = $this->passwordMinZxcvbnRoles === []
            || array_intersect($roles, $this->passwordMinZxcvbnRoles) !== [];

        $score = $appliesAtSiteLevel ? $this->passwordMinZxcvbnScore : 0;

        // Per-group override (take strictest).
        foreach ($this->groups as $group) {
            if (in_array($group['role'], $roles, true) && $group['min_zxcvbn_score'] !== null) {
                $score = max($score, $group['min_zxcvbn_score']);
            }
        }

        return $score;
    }

    /**
     * Get the effective password max-age in days for a WP_User (0 = disabled).
     *
     * @param \WP_User $user
     * @return int
     */
    public function effectiveMaxAgeDays(\WP_User $user): int
    {
        $roles = $this->userRoles($user);

        $appliesAtSiteLevel = $this->passwordExpiryRoles === []
            || array_intersect($roles, $this->passwordExpiryRoles) !== [];

        $days = $appliesAtSiteLevel ? $this->passwordMaxAgeDays : 0;

        // Per-group override (take strictest non-zero, i.e. shortest rotation).
        foreach ($this->groups as $group) {
            if (in_array($group['role'], $roles, true) && $group['max_age_days'] !== null && $group['max_age_days'] > 0) {
                if ($days === 0) {
                    $days = $group['max_age_days'];
                } else {
                    $days = min($days, $group['max_age_days']);
                }
            }
        }

        return $days;
    }

    /**
     * Get whether compromised-password blocking applies to a user.
     *
     * Object-typed for the same reason as effectiveMinZxcvbnScore() above.
     *
     * @param object $user \WP_User, or the \stdClass core builds in edit_user().
     * @return bool
     */
    public function blockCompromisedFor(object $user): bool
    {
        if ($this->passwordBlockCompromised) {
            return true;
        }
        $roles = $this->userRoles($user);
        foreach ($this->groups as $group) {
            if (in_array($group['role'], $roles, true) && $group['block_compromised'] === true) {
                return true;
            }
        }
        return false;
    }

    // -------------------------------------------------------------------------
    // Private helpers
    // -------------------------------------------------------------------------

    /**
     * Extract the roles array from a user object safely.
     *
     * The `role` fallback is the backstop against silent under-enforcement: the
     * stdClass core passes to `user_profile_update_errors` has no `roles`
     * property, only the single submitted `role` string. Without this branch a
     * caller that skipped ProfileUpdateUser::resolve() would read an empty role
     * list, conclude no role-scoped rule applies, and enforce nothing — with no
     * error and no log. Reading the submitted role keeps scoping alive there.
     *
     * This fallback is a REPLACEMENT, not a union, and is strictly weaker than
     * ProfileUpdateUser::resolve(): on the raw stdClass it can only ever see
     * the single submitted `role`, never the account's stored role. Any future
     * caller that reaches effectiveMinZxcvbnScore() or blockCompromisedFor()
     * with that unresolved object on a demotion gets `['subscriber']` and
     * silently skips the stored administrator rule — the exact defect class
     * this fallback exists to catch, one level down. Do not rely on it for a
     * role-scoped decision; resolve the user first.
     *
     * @param object $user \WP_User, or the \stdClass core builds in edit_user().
     * @return list<string>
     */
    private function userRoles(object $user): array
    {
        if (isset($user->roles) && is_array($user->roles)) {
            return array_values(array_filter(array_map('strval', $user->roles)));
        }
        if (isset($user->role) && is_string($user->role) && $user->role !== '') {
            return [$user->role];
        }
        return [];
    }

    /**
     * Coerce a raw value to a filtered list of strings from an allowlist.
     *
     * @param mixed         $raw
     * @param list<string>  $allowed
     * @return list<string>
     */
    private static function coerceStringList(mixed $raw, array $allowed): array
    {
        if (!is_array($raw)) {
            return $allowed;
        }
        $out = [];
        foreach ($raw as $v) {
            if (is_string($v) && in_array($v, $allowed, true)) {
                $out[] = $v;
            }
        }
        return $out === [] ? $allowed : array_values($out);
    }

    /**
     * Coerce a raw value to a list of non-empty, control-char-free strings
     * (suitable for role names). Drops any entry containing control chars or
     * exceeding 64 chars.
     *
     * @param mixed $raw
     * @return list<string>
     */
    private static function coerceRoleList(mixed $raw): array
    {
        if (!is_array($raw)) {
            return [];
        }
        $out = [];
        foreach ($raw as $v) {
            if (!is_string($v)) {
                continue;
            }
            $v = trim($v);
            if ($v === '' || strlen($v) > 64) {
                continue;
            }
            if (preg_match('/[\x00-\x1F\x7F]/', $v) === 1) {
                continue;
            }
            $out[] = $v;
        }
        return array_values(array_unique($out));
    }

    /**
     * Validate a hide-backend slug: ^[a-z0-9-]{4,64}$.
     * Returns '' when the value is invalid.
     *
     * @param mixed $raw
     * @return string
     */
    private static function coerceSlug(mixed $raw): string
    {
        if (!is_string($raw)) {
            return '';
        }
        $slug = strtolower(trim($raw));
        if (preg_match('/^[a-z0-9\-]{4,64}$/', $slug) !== 1) {
            return '';
        }
        // Reject known reserved paths.
        $reserved = ['wp-login', 'wp-admin', 'wp-json', 'wp-content', 'wp-includes', 'feed', 'sitemap'];
        if (in_array($slug, $reserved, true)) {
            return '';
        }
        return $slug;
    }

    /**
     * Coerce a hide-backend redirect URL. Must be http(s) or empty.
     *
     * @param mixed $raw
     * @return string
     */
    private static function coerceUrl(mixed $raw): string
    {
        if (!is_string($raw) || $raw === '') {
            return '';
        }
        $url = trim($raw);
        if (preg_match('/^https?:\/\//i', $url) !== 1) {
            return '';
        }
        return esc_url_raw($url);
    }

    /**
     * Validate and coerce per-group rows.
     *
     * @param array<mixed> $raw
     * @return list<array{role:string,require_2fa:bool|null,allowed_methods:list<string>|null,min_zxcvbn_score:int|null,block_compromised:bool|null,max_age_days:int|null}>
     */
    private static function coerceGroups(array $raw): array
    {
        $out = [];
        foreach ($raw as $entry) {
            if (!is_array($entry)) {
                continue;
            }
            $role = is_string($entry['role'] ?? null) ? trim((string) $entry['role']) : '';
            if ($role === '' || strlen($role) > 64) {
                continue;
            }
            if (preg_match('/[\x00-\x1F\x7F]/', $role) === 1) {
                continue;
            }

            $require2fa    = isset($entry['require_2fa'])     ? (bool) $entry['require_2fa']     : null;
            $methods       = isset($entry['allowed_methods']) && is_array($entry['allowed_methods'])
                ? self::coerceStringList($entry['allowed_methods'], self::ALLOWED_METHODS)
                : null;
            $minScore      = isset($entry['min_zxcvbn_score'])
                ? min(4, max(0, (int) $entry['min_zxcvbn_score']))
                : null;
            $blockComp     = isset($entry['block_compromised']) ? (bool) $entry['block_compromised'] : null;
            $maxAge        = isset($entry['max_age_days'])      ? max(0, (int) $entry['max_age_days']) : null;

            $out[] = [
                'role'              => $role,
                'require_2fa'       => $require2fa,
                'allowed_methods'   => $methods,
                'min_zxcvbn_score'  => $minScore,
                'block_compromised' => $blockComp,
                'max_age_days'      => $maxAge,
            ];
        }
        return $out;
    }

    /**
     * Validate and coerce the force-password-change list.
     *
     * @param array<mixed> $raw
     * @return list<array{user_login:string,reason:string}>
     */
    private static function coerceForceList(array $raw): array
    {
        $out = [];
        foreach ($raw as $entry) {
            if (!is_array($entry)) {
                continue;
            }
            $login = is_string($entry['user_login'] ?? null) ? trim((string) $entry['user_login']) : '';
            if ($login === '') {
                continue;
            }
            if (preg_match('/[\x00-\x1F\x7F]/', $login) === 1) {
                continue;
            }
            $reason = is_string($entry['reason'] ?? null) ? trim((string) $entry['reason']) : 'operator_request';
            $out[]  = ['user_login' => $login, 'reason' => $reason];
        }
        return $out;
    }
}
