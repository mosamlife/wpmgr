<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\ServicePrincipal;
use WPMgr\Agent\Abilities\VersionCompare;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * What the agent knows about Elementor on this site, and whether WPMgr may
 * build an Elementor page here.
 *
 * collect() reads the facts once; the other methods are pure functions of the
 * facts array, read defensively, so a fact that is missing or of the wrong
 * type refuses rather than admits.
 *
 * The tested ranges are agent code. Catalogue data may only narrow them
 * (AdapterStatus::narrowed()). Only the classic format is built; an Atomic
 * request is refused whatever the site runs.
 *
 * Every refusal is builder_not_available with one detail token, never text
 * read from the site.
 */
final class ElementorFacts
{
    /** Lowest Elementor version WPMgr builds classic pages for. */
    public const CLASSIC_MIN = '3.20.0';

    /** Highest Elementor version WPMgr builds classic pages for. */
    public const CLASSIC_MAX = '4.3.99';

    /** Lowest Elementor version for the Atomic format. Declared; admits nothing yet. */
    public const ATOMIC_MIN = '4.3.0';

    /** Highest Elementor version for the Atomic format. Declared; admits nothing yet. */
    public const ATOMIC_MAX = '4.3.99';

    /** Classic widgets in sections and columns, or in containers. */
    public const FORMAT_CLASSIC = 'classic';

    /** Elementor's Atomic elements. */
    public const FORMAT_ATOMIC = 'atomic';

    /** Elementor's experiment for the flexbox container layout. */
    public const EXPERIMENT_CONTAINER = 'container';

    /** Elementor's experiment for Atomic elements. */
    public const EXPERIMENT_ATOMIC = 'e_atomic_elements';

    /** Elementor's list of roles that may not use its editor. */
    public const OPTION_EXCLUDED_ROLES = 'elementor_exclude_user_roles';

    /** Elementor's list of post types it is switched on for. */
    public const OPTION_POST_TYPES = 'elementor_cpt_support';

    /** The post types Elementor is switched on for when the option is absent. */
    public const DEFAULT_POST_TYPES = ['page', 'post'];

    /** The constant Elementor Pro defines its version in. */
    public const PRO_VERSION_CONSTANT = 'ELEMENTOR_PRO_VERSION';

    /** Every refusal code from this class but bad_input. */
    public const CODE = AdapterStatus::CODE;

    /**
     * The keys builderFactsBlock() emits, in order. Each is a key the
     * readiness facts block reserves for Elementor.
     */
    public const BLOCK_KEYS = ['service_role_excluded', 'pro_active'];

    /** A post type key as WordPress registers one. */
    private const RE_POST_TYPE = '/^[a-z0-9_-]{1,20}$/D';

    /** Most post type keys kept from the option. */
    private const MAX_POST_TYPES = 64;

    /**
     * Elementor's breakpoint names, smallest screen first. The facts list
     * only these, in this order.
     */
    public const BREAKPOINTS = ['mobile', 'mobile_extra', 'tablet', 'tablet_extra', 'laptop', 'widescreen'];

    /** The post meta a kit's settings are saved in. */
    public const KIT_SETTINGS_META = '_elementor_page_settings';

    /** The kit setting that lists the active breakpoints. */
    public const KIT_BREAKPOINTS_KEY = 'active_breakpoints';

    /** What the kit's list puts before each breakpoint name. */
    public const KIT_BREAKPOINT_PREFIX = 'viewport_';

    /**
     * Read the facts.
     *
     * role_excluded is true when a role of the service user is in Elementor's
     * excluded list. When the service user cannot be read, the role it is
     * created with stands in: it holds that role alone or it is refused.
     * post_types keeps only post type keys; an option that is not a list
     * switches Elementor on for nothing. active_breakpoints is described at
     * activeBreakpoints(). block_theme is whether the active theme is a
     * block theme (see themeIsBlock()); it decides the page frames a
     * designed page may be built in.
     *
     * @param ElementorApi $api         Elementor.
     * @param int          $principalId The service user's id.
     * @param string       $proConstant Tests only: stands in for Elementor Pro's version constant. Production passes none.
     * @return array{active: bool, version: string|null, classic_in_range: bool, containers: bool|null, atomic: bool|null, role_excluded: bool, post_types: list<string>, active_kit_id: int, pro_active: bool, active_breakpoints: list<string>|null, block_theme: bool}
     */
    public static function collect(ElementorApi $api, int $principalId, string $proConstant = self::PRO_VERSION_CONSTANT): array
    {
        $version = $api->version();
        $kitId   = $api->activeKitId();

        return [
            'active'             => $api->loaded(),
            'version'            => $version,
            'classic_in_range'   => $version !== null && VersionCompare::inRange($version, self::CLASSIC_MIN, self::CLASSIC_MAX),
            'containers'         => $api->experimentActive(self::EXPERIMENT_CONTAINER),
            'atomic'             => $api->experimentActive(self::EXPERIMENT_ATOMIC),
            'role_excluded'      => self::roleExcluded($principalId),
            'post_types'         => self::postTypes(),
            'active_kit_id'      => $kitId,
            'pro_active'         => defined($proConstant),
            'active_breakpoints' => self::activeBreakpoints($api, $kitId),
            'block_theme'        => self::themeIsBlock(),
        ];
    }

    /**
     * Whether $facts say the active theme is a block theme. Only a false
     * block_theme is a classic theme: a fact that is missing or of another
     * type reads as a block theme, which admits only canvas, the frame both
     * kinds of theme show as built.
     *
     * @param array<string, mixed> $facts From collect().
     * @return bool
     */
    public static function blockTheme(array $facts): bool
    {
        return ($facts['block_theme'] ?? null) !== false;
    }

    /**
     * Whether a page of $postType may be created in $format, as a refusal.
     *
     * The detail is the first that applies, in this order: elementor_missing
     * (Elementor is not loaded); atomic_unavailable (an Atomic page was asked
     * for; only classic pages are built); version_unverified (no version, or
     * one outside the classic range); role_excluded (Elementor's role
     * settings exclude the service user); post_type_not_supported (Elementor
     * is not switched on for $postType). A format other than classic or
     * atomic is bad_input.
     *
     * @param array<string, mixed> $facts    From collect().
     * @param string               $postType Post type of the page to create.
     * @param string               $format   classic or atomic.
     * @return array{code: string, detail: string}|null
     */
    public static function createRefusal(array $facts, string $postType, string $format): ?array
    {
        if (($facts['active'] ?? null) !== true) {
            return ['code' => self::CODE, 'detail' => 'elementor_missing'];
        }
        if ($format === self::FORMAT_ATOMIC) {
            return ['code' => self::CODE, 'detail' => 'atomic_unavailable'];
        }
        if ($format !== self::FORMAT_CLASSIC) {
            return ['code' => 'bad_input', 'detail' => 'elementor_format must be classic or atomic'];
        }
        if (($facts['classic_in_range'] ?? null) !== true) {
            return ['code' => self::CODE, 'detail' => 'version_unverified'];
        }
        if (($facts['role_excluded'] ?? null) !== false) {
            return ['code' => self::CODE, 'detail' => 'role_excluded'];
        }
        $postTypes = $facts['post_types'] ?? null;
        if (!is_array($postTypes) || !in_array($postType, $postTypes, true)) {
            return ['code' => self::CODE, 'detail' => 'post_type_not_supported'];
        }

        return null;
    }

    /**
     * Whether Elementor can be used on this site now, for any post type:
     * active, a version inside the classic range, and the service user not
     * excluded by Elementor's role settings.
     *
     * @param array<string, mixed> $facts From collect().
     * @return AdapterStatus
     */
    public static function status(array $facts): AdapterStatus
    {
        $active  = ($facts['active'] ?? null) === true;
        $version = $facts['version'] ?? null;
        $reasons = [];
        if (!$active) {
            $reasons[] = 'elementor_missing';
        }
        if (($facts['role_excluded'] ?? null) !== false) {
            $reasons[] = 'role_excluded';
        }

        return new AdapterStatus($active, is_string($version) ? $version : null, self::CLASSIC_MIN, self::CLASSIC_MAX, $reasons);
    }

    /**
     * The Elementor keys this slice adds to the readiness facts block:
     * exactly BLOCK_KEYS, each a boolean. A role exclusion that is not
     * known to be false is reported as true.
     *
     * @param array<string, mixed> $facts From collect().
     * @return array{service_role_excluded: bool, pro_active: bool}
     */
    public static function builderFactsBlock(array $facts): array
    {
        return [
            'service_role_excluded' => ($facts['role_excluded'] ?? null) !== false,
            'pro_active'            => ($facts['pro_active'] ?? null) === true,
        ];
    }

    /**
     * Whether the active theme is a block theme, as core answers it. A core
     * without the function has no block themes. A read that throws counts
     * as a block theme, so the frames it admits are those either kind of
     * theme shows as built.
     *
     * @return bool
     */
    private static function themeIsBlock(): bool
    {
        if (!function_exists('wp_is_block_theme')) {
            return false;
        }
        try {
            return wp_is_block_theme() === true;
        } catch (\Throwable $e) {
            return true;
        }
    }

    /**
     * Whether a role of the service user is in Elementor's excluded list.
     * A stored value that is not a list excludes no role; a read that throws
     * counts as excluded.
     *
     * @param int $principalId The service user's id.
     * @return bool
     */
    private static function roleExcluded(int $principalId): bool
    {
        try {
            $excluded = get_option(self::OPTION_EXCLUDED_ROLES, []);
            if (!is_array($excluded)) {
                return false;
            }
            $roles = self::principalRoles($principalId);
            foreach ($excluded as $role) {
                if (is_string($role) && in_array($role, $roles, true)) {
                    return true;
                }
            }

            return false;
        } catch (\Throwable $e) {
            return true;
        }
    }

    /**
     * The service user's roles; its pinned role when the user cannot be read.
     *
     * @param int $principalId The service user's id.
     * @return list<string>
     */
    private static function principalRoles(int $principalId): array
    {
        $user = $principalId > 0 ? get_userdata($principalId) : false;
        if (!is_object($user)) {
            return [ServicePrincipal::ROLE];
        }
        $roles = [];
        foreach ((array) $user->roles as $role) {
            if (is_string($role)) {
                $roles[] = $role;
            }
        }

        return $roles;
    }

    /**
     * The breakpoints active on this site, as Elementor's names from
     * BREAKPOINTS in that order: a responsive setting is written for a
     * breakpoint only when its name is listed here.
     *
     * The active kit's saved list is the answer when the kit has saved one.
     * When it has not (no kit, no saved settings, or settings without the
     * key), Elementor's breakpoints manager answers, as Elementor itself
     * then falls back to its defaults. A saved value that is not a list, a
     * kit read that throws, or a manager that cannot answer gives null, and
     * a caller then treats no breakpoint as active.
     *
     * @param ElementorApi $api   Elementor.
     * @param int          $kitId The active kit's post id, or 0.
     * @return list<string>|null
     */
    private static function activeBreakpoints(ElementorApi $api, int $kitId): ?array
    {
        $settings = null;
        if ($kitId > 0) {
            try {
                $settings = get_post_meta($kitId, self::KIT_SETTINGS_META, true);
            } catch (\Throwable $e) {
                return null;
            }
        }
        if (is_array($settings) && isset($settings[self::KIT_BREAKPOINTS_KEY])) {
            $saved = $settings[self::KIT_BREAKPOINTS_KEY];
            if (!is_array($saved)) {
                return null;
            }
            $active = [];
            foreach (self::BREAKPOINTS as $name) {
                if (in_array(self::KIT_BREAKPOINT_PREFIX . $name, $saved, true)) {
                    $active[] = $name;
                }
            }

            return $active;
        }

        $reported = $api->activeBreakpoints();
        if ($reported === null) {
            return null;
        }

        return array_values(array_filter(self::BREAKPOINTS, static fn (string $name): bool => in_array($name, $reported, true)));
    }

    /**
     * The post types Elementor is switched on for: post type keys from the
     * option, in order, without repeats. Empty when the option is not a list
     * or cannot be read.
     *
     * @return list<string>
     */
    private static function postTypes(): array
    {
        try {
            $stored = get_option(self::OPTION_POST_TYPES, self::DEFAULT_POST_TYPES);
        } catch (\Throwable $e) {
            return [];
        }
        if (!is_array($stored)) {
            return [];
        }
        $types = [];
        foreach ($stored as $type) {
            if (!is_string($type) || preg_match(self::RE_POST_TYPE, $type) !== 1 || in_array($type, $types, true)) {
                continue;
            }
            $types[] = $type;
            if (count($types) >= self::MAX_POST_TYPES) {
                break;
            }
        }

        return $types;
    }
}
