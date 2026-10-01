<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The content service principal: one dedicated user with one custom role.
 *
 * The role carries an exact, pinned capability set: it can read and edit
 * posts and pages, and nothing else. It cannot publish, delete, upload, write
 * unfiltered HTML, manage options, edit users, install plugins or edit files.
 * Engine writes run as this user and as nobody else.
 *
 * The user and role are created only by an explicit signed enable, never at
 * enrolment. The user has no usable password: the one set at creation is
 * random, never stored and never returned, and every login, application
 * password and password reset is refused for this user regardless.
 *
 * Any difference between the live capabilities and the pinned set is drift,
 * and drift refuses the write. The agent never repairs drift silently.
 */
final class ServicePrincipal
{
    public const ROLE = 'wpmgr_content_agent';

    public const USER_LOGIN = 'wpmgr-content-agent';

    /** Option holding the service user's id. */
    public const OPTION_USER_ID = 'wpmgr_content_agent_user_id';

    /** User meta marking the account as the agent's own. */
    public const META_MARKER = '_wpmgr_service_principal';

    /** The pinned capability set, exactly. */
    public const CAPS = [
        'read',
        'edit_posts',
        'edit_others_posts',
        'edit_published_posts',
        'edit_pages',
        'edit_others_pages',
        'edit_published_pages',
    ];

    /**
     * Capabilities that must stay false for this user, whatever any filter
     * says. Checked through current_user_can() while switched to the user,
     * so a grant made by a user_has_cap filter is also caught.
     */
    public const FORBIDDEN = [
        'unfiltered_html',
        'manage_options',
        'edit_users',
        'create_users',
        'promote_users',
        'delete_users',
        'install_plugins',
        'activate_plugins',
        'edit_plugins',
        'edit_themes',
        'edit_theme_options',
        'edit_files',
        'upload_files',
        'publish_posts',
        'publish_pages',
        'delete_posts',
        'delete_pages',
        'delete_others_posts',
        'delete_others_pages',
        'delete_published_posts',
        'delete_published_pages',
        'unfiltered_upload',
        'import',
        'export',
    ];

    /**
     * Hooks that keep the account from ever being logged into.
     *
     * @return void
     */
    public static function register(): void
    {
        add_filter('authenticate', [self::class, 'refuseAuthenticate'], PHP_INT_MAX, 3);
        add_filter('wp_authenticate_user', [self::class, 'refuseAuthenticateUser'], PHP_INT_MAX, 2);
        add_filter('wp_is_application_passwords_available_for_user', [self::class, 'refuseAppPasswords'], PHP_INT_MAX, 2);
        add_filter('allow_password_reset', [self::class, 'refusePasswordReset'], PHP_INT_MAX, 2);
        add_filter('determine_current_user', [self::class, 'refuseCurrentUser'], PHP_INT_MAX, 1);
    }

    /**
     * determine_current_user filter: no request ever resolves to the service
     * user, whatever cookie or auth handler named it. The engine switches to
     * it in-process with wp_set_current_user(), which does not run this filter.
     *
     * @param mixed $userId Resolved user id so far.
     * @return mixed
     */
    public static function refuseCurrentUser($userId)
    {
        $id = self::userId();

        return ($id > 0 && is_numeric($userId) && (int) $userId === $id) ? 0 : $userId;
    }

    /**
     * authenticate filter: whatever earlier handlers decided, the service
     * user never authenticates.
     *
     * @param mixed  $user     Result so far.
     * @param mixed  $username Submitted login.
     * @param mixed  $password Submitted password.
     * @return mixed
     */
    public static function refuseAuthenticate($user, $username = '', $password = '')
    {
        unset($password);
        if (self::isPrincipalUser($user)
            || (is_string($username) && $username !== '' && strtolower($username) === self::USER_LOGIN)) {
            return new \WP_Error('wpmgr_service_principal', 'This account cannot sign in.');
        }

        return $user;
    }

    /**
     * wp_authenticate_user filter: the second gate after the password check.
     *
     * @param mixed $user     User or error.
     * @param mixed $password Password.
     * @return mixed
     */
    public static function refuseAuthenticateUser($user, $password = '')
    {
        unset($password);
        if (self::isPrincipalUser($user)) {
            return new \WP_Error('wpmgr_service_principal', 'This account cannot sign in.');
        }

        return $user;
    }

    /**
     * @param mixed $available Available so far.
     * @param mixed $user      User.
     * @return mixed
     */
    public static function refuseAppPasswords($available, $user = null)
    {
        return self::isPrincipalUser($user) ? false : $available;
    }

    /**
     * @param mixed $allow   Allowed so far.
     * @param mixed $userId  User id.
     * @return mixed
     */
    public static function refusePasswordReset($allow, $userId = 0)
    {
        $id = self::userId();

        return ($id > 0 && (int) $userId === $id) ? false : $allow;
    }

    /**
     * The recorded service user id, or 0.
     *
     * @return int
     */
    public static function userId(): int
    {
        $raw = get_option(self::OPTION_USER_ID, 0);

        return is_numeric($raw) ? max(0, (int) $raw) : 0;
    }

    /**
     * Create the role and the user if absent. Idempotent: a second call with
     * an intact principal changes nothing and reports `already_enabled`.
     *
     * @return array{ok:bool,code:string,detail:string,user_id?:int,created?:bool}
     */
    public static function enable(): array
    {
        $created = false;

        $role = get_role(self::ROLE);
        if ($role === null) {
            $caps = [];
            foreach (self::CAPS as $cap) {
                $caps[$cap] = true;
            }
            $role = add_role(self::ROLE, 'WPMgr content agent', $caps);
            if ($role === null) {
                return ['ok' => false, 'code' => 'principal_create_failed', 'detail' => 'the role could not be created'];
            }
            $created = true;
        }

        $id = self::userId();
        if ($id > 0) {
            $user = get_userdata($id);
            if (!is_object($user) || (string) $user->user_login !== self::USER_LOGIN) {
                return ['ok' => false, 'code' => 'principal_missing', 'detail' => 'the recorded service user no longer exists'];
            }
        } else {
            if (get_user_by('login', self::USER_LOGIN) !== false) {
                return ['ok' => false, 'code' => 'principal_login_taken', 'detail' => 'a user with the service login already exists and is not the agent\'s'];
            }
            $id = wp_insert_user([
                'user_login'   => self::USER_LOGIN,
                // Random, never stored, never returned. Login is refused anyway.
                'user_pass'    => bin2hex(random_bytes(32)),
                'display_name' => 'WPMgr content agent',
                'nickname'     => 'WPMgr content agent',
                'role'         => self::ROLE,
            ]);
            if (!is_int($id) || $id < 1) {
                return ['ok' => false, 'code' => 'principal_create_failed', 'detail' => 'the service user could not be created'];
            }
            update_user_meta($id, self::META_MARKER, '1');
            update_option(self::OPTION_USER_ID, $id, false);
            $created = true;
        }

        $drift = self::drift($id);
        if ($drift !== null) {
            return ['ok' => false, 'code' => 'principal_capabilities_drifted', 'detail' => $drift, 'user_id' => $id];
        }

        return [
            'ok'      => true,
            'code'    => $created ? 'enabled' : 'already_enabled',
            'detail'  => '',
            'user_id' => $id,
            'created' => $created,
        ];
    }

    /**
     * Static drift check: the role holds exactly the pinned set, and the user
     * holds exactly that role and no direct capability. Null means intact.
     *
     * @param int $userId Service user id.
     * @return string|null
     */
    public static function drift(int $userId): ?string
    {
        $role = get_role(self::ROLE);
        if (!is_object($role)) {
            return 'the service role is missing';
        }
        $granted = [];
        foreach ((array) $role->capabilities as $cap => $on) {
            if ($on) {
                $granted[] = (string) $cap;
            }
        }
        $pinned = self::CAPS;
        sort($granted);
        sort($pinned);
        if ($granted !== $pinned) {
            return 'the service role capabilities differ from the pinned set';
        }

        $user = get_userdata($userId);
        if (!is_object($user) || (string) $user->user_login !== self::USER_LOGIN) {
            return 'the service user is missing';
        }
        $roles = array_values(array_map('strval', (array) $user->roles));
        if ($roles !== [self::ROLE]) {
            return 'the service user holds a role other than the service role';
        }
        $direct = [];
        foreach ((array) $user->caps as $cap => $on) {
            if ($on) {
                $direct[] = (string) $cap;
            }
        }
        if ($direct !== [self::ROLE]) {
            return 'the service user holds capabilities outside the service role';
        }

        return null;
    }

    /**
     * Live drift check, run while switched to the service user: nothing in
     * FORBIDDEN may be granted, by any filter.
     *
     * @return string|null
     */
    public static function liveDrift(): ?string
    {
        foreach (self::FORBIDDEN as $cap) {
            if (current_user_can($cap)) {
                return 'the service user has a forbidden capability: ' . $cap;
            }
        }
        foreach (self::CAPS as $cap) {
            if (!current_user_can($cap)) {
                return 'the service user lacks a pinned capability: ' . $cap;
            }
        }

        return null;
    }

    /**
     * Resolve the enabled, intact principal. Null id with a code when not.
     *
     * @return array{id:int,code:string,detail:string}
     */
    public static function resolve(): array
    {
        $id = self::userId();
        if ($id < 1) {
            return ['id' => 0, 'code' => 'content_editing_not_enabled', 'detail' => 'content editing has not been enabled on this site'];
        }
        $drift = self::drift($id);
        if ($drift !== null) {
            return ['id' => 0, 'code' => 'principal_capabilities_drifted', 'detail' => $drift];
        }

        return ['id' => $id, 'code' => '', 'detail' => ''];
    }

    /**
     * @param mixed $user Candidate.
     * @return bool
     */
    private static function isPrincipalUser($user): bool
    {
        if (!is_object($user) || !isset($user->ID)) {
            return false;
        }
        $id = self::userId();

        return ($id > 0 && (int) $user->ID === $id)
            || (isset($user->user_login) && (string) $user->user_login === self::USER_LOGIN);
    }
}
