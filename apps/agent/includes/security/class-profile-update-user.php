<?php
/**
 * ProfileUpdateUser — resolves the object WordPress core hands to the
 * `user_profile_update_errors` action into a real WP_User that carries the
 * roles our role-scoped policy decisions depend on.
 *
 * WHY THIS EXISTS
 * ---------------
 * Core does NOT pass a WP_User to `user_profile_update_errors`. It builds a
 * bare stdClass in edit_user() and passes it by reference:
 *
 *     wp-admin/includes/user.php
 *       $user = new stdClass();          // plain stdClass, never a WP_User
 *       if ( $user_id ) { $user->ID = $user_id; ... }   // ID ONLY on update
 *       ...
 *       $user->role = $new_role;         // only when the actor can promote_users
 *       ...
 *       do_action_ref_array( 'user_profile_update_errors', array( &$errors, $update, &$user ) );
 *
 * Two consequences drive everything below:
 *
 *   1. A callback that type-hints \WP_User throws a TypeError at argument
 *      binding — before any guard inside the method body can run. That is a
 *      hard fatal on every wp-admin route that edits a user, including an
 *      administrator opening their own profile.php.
 *
 *   2. Merely widening the hint is NOT a fix. The stdClass has no `roles`
 *      property and no `caps`, so every role-scoped rule
 *      (SecurityPolicy::effectiveMinZxcvbnScore(), ::blockCompromisedFor())
 *      would silently see an empty role list and fall back to the site
 *      default. That converts a loud crash into silent under-enforcement of a
 *      security policy, which is strictly worse. So we resolve a REAL user
 *      instead of trusting the object the hook handed us.
 *
 * @package WPMgr\Agent\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Security;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Resolver for the `user_profile_update_errors` user argument.
 */
final class ProfileUpdateUser
{
    /**
     * Resolve whatever the hook passed into a WP_User suitable for
     * role-scoped policy evaluation.
     *
     * Total function: it never throws, for any input. It is called from a hook
     * callback that sits OUTSIDE PasswordPolicyModule::validatePassword()'s
     * catch(\Throwable) block, so a throw here would be a fatal — exactly the
     * defect being fixed.
     *
     * Role resolution, stated explicitly because the wrong answer is silent:
     *
     *   UPDATE (ID present) — load the stored user with get_userdata() and take
     *   its real roles. When the same submit also changes the role
     *   ($user->role, set by core only when the actor can promote_users), take
     *   the UNION of stored and submitted roles: the password must satisfy the
     *   policy for every role the account holds or is about to hold. The union
     *   is never weaker than either role alone, which is the safe direction for
     *   a promotion (subscriber -> administrator must meet administrator rules).
     *
     *   CREATE (no ID) — there is no stored user to load. Core puts the
     *   submitted role on the object; when it is absent (the actor could not
     *   promote_users) wp_insert_user() will apply the site's `default_role`
     *   option, so that is what we evaluate against. Either way we evaluate the
     *   role the new account will ACTUALLY receive, not an empty list.
     *
     * The returned object is always a fresh stub, never the instance
     * get_userdata() returned. get_userdata() hands back the request-cached
     * WP_User, and writing a unioned role list onto it would corrupt that cache
     * for the rest of the request.
     *
     * @param mixed $raw The third `user_profile_update_errors` argument. Typed
     *                   mixed rather than object because this is a hook
     *                   boundary: any plugin may fire the action with anything.
     * @return \WP_User User to evaluate role-scoped rules against.
     */
    public static function resolve(mixed $raw): \WP_User
    {
        // A caller that already handed us a real user is trusted as-is; it
        // carries its own roles and caps, and rebuilding it would only lose
        // information.
        if ($raw instanceof \WP_User) {
            return $raw;
        }

        $id           = self::intProp($raw, 'ID');
        $submittedRole = self::stringProp($raw, 'role');

        $roles    = [];
        $existing = null;

        if ($id > 0 && function_exists('get_userdata')) {
            $loaded = get_userdata($id);
            if ($loaded instanceof \WP_User) {
                $existing = $loaded;
                // No isset() here: WP_User::$roles is populated by the
                // constructor on every real instance (core's WP_User and this
                // suite's stub both declare it with a `[]` default), so once
                // $loaded is confirmed instanceof \WP_User the property is
                // always set. is_array() stays as the actual runtime guard —
                // a plugin filtering user data could still hand back something
                // that is not an array despite the declared type.
                if (is_array($loaded->roles)) {
                    $roles = array_values(array_filter(array_map('strval', $loaded->roles)));
                }
            }
        }

        if ($submittedRole !== '' && !in_array($submittedRole, $roles, true)) {
            // Union, not replacement — see the docblock.
            $roles[] = $submittedRole;
        }

        if ($roles === [] && $id === 0) {
            // Create path with no submitted role: wp_insert_user() will apply
            // the site default, so that is the role this password belongs to.
            $default = function_exists('get_option') ? get_option('default_role') : '';
            if (is_string($default) && $default !== '') {
                $roles[] = $default;
            }
        }

        $user             = new \WP_User();
        $user->ID         = $id;
        $user->roles      = $roles;
        $user->user_login = self::firstNonEmpty(
            self::stringProp($raw, 'user_login'),
            $existing !== null ? self::stringProp($existing, 'user_login') : ''
        );
        $user->user_email = self::firstNonEmpty(
            self::stringProp($raw, 'user_email'),
            $existing !== null ? self::stringProp($existing, 'user_email') : ''
        );
        $user->display_name = self::firstNonEmpty(
            self::stringProp($raw, 'display_name'),
            $existing !== null ? self::stringProp($existing, 'display_name') : ''
        );

        return $user;
    }

    /**
     * Read an int property from an arbitrary object without throwing.
     *
     * @param mixed  $obj
     * @param string $prop
     * @return int Zero when absent or not coercible.
     */
    private static function intProp(mixed $obj, string $prop): int
    {
        if (!is_object($obj) || !isset($obj->$prop)) {
            return 0;
        }
        $value = $obj->$prop;
        return is_int($value) || (is_string($value) && ctype_digit($value)) ? (int) $value : 0;
    }

    /**
     * Read a string property from an arbitrary object without throwing.
     *
     * @param mixed  $obj
     * @param string $prop
     * @return string Empty string when absent or not a string.
     */
    private static function stringProp(mixed $obj, string $prop): string
    {
        if (!is_object($obj) || !isset($obj->$prop)) {
            return '';
        }
        $value = $obj->$prop;
        return is_string($value) ? $value : '';
    }

    /**
     * First non-empty of two strings.
     *
     * @param string $a
     * @param string $b
     * @return string
     */
    private static function firstNonEmpty(string $a, string $b): string
    {
        return $a !== '' ? $a : $b;
    }
}
