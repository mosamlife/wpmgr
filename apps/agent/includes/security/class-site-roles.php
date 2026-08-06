<?php
/**
 * SiteRoles: reports the WordPress roles that actually exist on this site.
 *
 * The control plane's password/2FA policy is expressed in ROLE SLUGS, and the
 * agent already enforces it against whatever slugs a user really holds (see
 * SecurityPolicy::effectiveMinZxcvbnScore()). What was missing was DISCOVERY:
 * the dashboard had no way to learn that a WooCommerce site has `shop_manager`
 * or that a membership plugin added `staff`, so an operator could not select
 * those roles and the elevated users in them went ungoverned. This collector
 * closes that gap by reporting the site's real role set as part of the agent's
 * metadata (a read path), slug plus the display name WordPress itself shows.
 *
 * SOURCE OF TRUTH: wp_roles(), NOT get_editable_roles().
 * get_editable_roles() applies the `editable_roles` filter, which membership,
 * multisite and role-editor plugins use to HIDE roles from the user-management
 * screens. A role hidden from the user editor still exists, and its users still
 * log in with a password, so honouring that filter here would recreate exactly
 * the discovery gap this collector exists to fix. wp_roles() is the complete,
 * unfiltered registry of roles that can authenticate, which is the set a
 * SECURITY policy has to be able to name.
 *
 * DISPLAY NAMES: run through translate_user_role(), which is precisely what
 * core's own wp_dropdown_roles() does. An Italian site therefore reports
 * "Amministratore" for `administrator`, matching what the operator sees in
 * wp-admin, while the slug the policy stores stays `administrator`.
 *
 * NO NEW PRIVILEGE: role definitions are read from the roles registry only.
 * No user record, no capability grant, and no user-identifying data is touched.
 *
 * @package WPMgr\Agent\Security
 */

declare(strict_types=1);

namespace WPMgr\Agent\Security;

if (!defined('ABSPATH')) {
    exit;
}

/**
 * Collects the site's WordPress role registry for the control plane.
 */
final class SiteRoles
{
    /**
     * Hard cap on how many roles are reported. Some membership and LMS plugins
     * register dozens of roles; a pathological or corrupted registry could hold
     * far more. The cap bounds the metadata payload so a single site can never
     * blow up the push, and 200 is far above any real-world role count.
     */
    public const MAX_ROLES = 200;

    /**
     * Display names longer than this are truncated. Truncating a NAME is safe:
     * it is presentational only and the slug is what the policy stores.
     */
    public const MAX_NAME_LENGTH = 100;

    /**
     * Slugs longer than this are DROPPED rather than truncated. A truncated
     * slug would not match anything the agent enforces against, so reporting
     * one would be worse than reporting nothing.
     */
    public const MAX_SLUG_LENGTH = 100;

    /**
     * Collect the site's roles as a list of {slug, name} pairs.
     *
     * Never throws and never fatals: an unreadable registry reports an empty
     * list, which the control plane and dashboard read as "this site has not
     * reported its roles".
     *
     * @return list<array{slug:string,name:string}> Ordered as WordPress stores them.
     */
    public static function collect(): array
    {
        $raw = self::rawRoles();
        if ($raw === []) {
            return [];
        }

        $out = [];
        foreach ($raw as $slug => $entry) {
            if (count($out) >= self::MAX_ROLES) {
                break;
            }

            $slug = is_string($slug) ? trim($slug) : '';
            if ($slug === '' || strlen($slug) > self::MAX_SLUG_LENGTH) {
                continue;
            }

            $out[] = [
                'slug' => $slug,
                'name' => self::displayName($slug, $entry),
            ];
        }

        return $out;
    }

    // -------------------------------------------------------------------------
    // Private helpers
    // -------------------------------------------------------------------------

    /**
     * Read the raw slug-keyed role map out of the WordPress roles registry.
     *
     * Prefers WP_Roles::$roles (slug => ['name' => ..., 'capabilities' => ...])
     * and falls back to WP_Roles::$role_names (slug => name) when a replacement
     * roles object only exposes the latter. Duck-typed rather than
     * instanceof-checked so a site running a custom $wp_roles implementation
     * still reports.
     *
     * @return array<string,mixed> Empty when the registry is unavailable.
     */
    private static function rawRoles(): array
    {
        if (!function_exists('wp_roles')) {
            return [];
        }

        try {
            $registry = wp_roles();
        } catch (\Throwable $e) {
            return [];
        }

        if (!is_object($registry)) {
            return [];
        }

        if (isset($registry->roles) && is_array($registry->roles)) {
            return $registry->roles;
        }

        if (isset($registry->role_names) && is_array($registry->role_names)) {
            return $registry->role_names;
        }

        return [];
    }

    /**
     * Resolve one role's operator-facing display name.
     *
     * $entry is the value from the registry map: the full role array from
     * WP_Roles::$roles, or a bare string when it came from $role_names. The
     * slug is the last-resort fallback so a role always has something
     * recognisable to render.
     *
     * @param string $slug  Role slug.
     * @param mixed  $entry Raw registry value for that slug.
     * @return string Localized, length-bounded display name.
     */
    private static function displayName(string $slug, mixed $entry): string
    {
        $name = '';
        if (is_array($entry) && isset($entry['name']) && is_string($entry['name'])) {
            $name = $entry['name'];
        } elseif (is_string($entry)) {
            $name = $entry;
        }

        $name = trim($name);
        if ($name === '') {
            return $slug;
        }

        // Same call core's wp_dropdown_roles() makes, so the reported name is
        // the one the operator already sees on the site's own Users screen.
        if (function_exists('translate_user_role')) {
            $translated = translate_user_role($name);
            if (is_string($translated) && trim($translated) !== '') {
                $name = trim($translated);
            }
        }

        if (function_exists('mb_substr')) {
            return mb_substr($name, 0, self::MAX_NAME_LENGTH);
        }

        return substr($name, 0, self::MAX_NAME_LENGTH);
    }
}
