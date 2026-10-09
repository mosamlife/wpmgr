<?php
/**
 * BuilderFacts: read-only facts about the page builders installed on this site,
 * reported as the optional `builder_facts` block of the metadata push.
 *
 * WHAT IT IS FOR. The control plane shows each site an "AI readiness" checklist.
 * Most of what that checklist needs is already reported elsewhere on the push
 * (the plugin and theme inventory, the WordPress and agent versions). This block
 * carries the few facts that no existing field answers:
 *
 *   - `theme_template`: the directory name of the PARENT theme. The inventory's
 *     `active` flag is true only for the stylesheet (child) theme, so a site
 *     running a child theme of a page-builder theme looks like "installed, not
 *     active" without it.
 *   - `elementor.atomic_editor`: whether Elementor's Atomic editor is active,
 *     answered by Elementor's own experiments manager. The raw option cannot
 *     answer it, because an experiment's default state is resolved by the
 *     manager and not stored.
 *
 * WIRE SHAPE (schema version 1).
 *
 *   {
 *     "v": 1,
 *     "theme_template": "<directory name>",        // omitted when unreadable
 *     "elementor": { "atomic_editor": true|false|null }   // omitted when Elementor is not loaded
 *   }
 *
 * RULES THE COLLECTOR HOLDS.
 *   - Read-only. It writes no option, transient, file or schedule.
 *   - It never throws. Each fact is read on its own and a failure degrades that
 *     one fact; the rest of the metadata push is unaffected.
 *   - Unknown is never reported as a fact. A value that cannot be read is
 *     `null` (when its parent key is present) or its key is omitted. Absence of
 *     a builder is the absence of its key, never `false`.
 *   - Bounded and free of site text. The only string is a directory name that
 *     matches a strict pattern and is at most MAX_THEME_TEMPLATE_LENGTH bytes;
 *     everything else is a boolean or null.
 *   - Advisory. The control plane reads the block to render a checklist; no
 *     command, approval or dispatch decision depends on it.
 *
 * RESERVED KEYS. Later schema versions add the keys named in RESERVED_KEYS.
 * They are not collected at schema version 1.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

if (!defined('ABSPATH')) {
    exit;
}

/**
 * Collects the `builder_facts` block.
 */
final class BuilderFacts
{
    /**
     * Wire schema version of the block. Raise it, together with the key
     * whitelist test and the control plane's decoder, in the same change that
     * starts emitting any key in RESERVED_KEYS.
     */
    public const SCHEMA_VERSION = 1;

    /**
     * Longest parent-theme directory name that is reported. A longer or
     * otherwise non-conforming name is omitted rather than truncated: a
     * truncated name would name a directory that does not exist.
     */
    public const MAX_THEME_TEMPLATE_LENGTH = 100;

    /**
     * Keys reserved for a later schema version, by parent key. They are NOT
     * emitted at SCHEMA_VERSION 1, and no collector may use one of these names
     * for any other meaning.
     *
     *   - elementor.gate: one of `available`, `unavailable`, `disabled_on_site`.
     *   - elementor.service_role_excluded: bool.
     *   - elementor.pro_active: bool.
     *   - bricks.abilities_setting: true, false or null.
     *
     * @var array<string,list<string>>
     */
    public const RESERVED_KEYS = [
        'elementor' => ['gate', 'service_role_excluded', 'pro_active'],
        'bricks'    => ['abilities_setting'],
    ];

    /**
     * Fully-qualified class name of Elementor's main plugin class, without a
     * leading backslash. Its presence in memory is how "Elementor is loaded on
     * this request" is decided.
     */
    private const ELEMENTOR_PLUGIN_CLASS = 'Elementor\\Plugin';

    /**
     * Name of Elementor's Atomic editor experiment.
     */
    private const ELEMENTOR_ATOMIC_EXPERIMENT = 'e_atomic_elements';

    /**
     * Collect the block.
     *
     * Always returns at least `{"v": 1}`. Never throws.
     *
     * @param string $elementorPluginClass Class that stands for Elementor's main
     *   plugin class. Production callers pass nothing; the parameter exists so a
     *   test can point the collector at a stand-in without defining a class in
     *   Elementor's namespace for the rest of the process.
     * @return array{v:int,theme_template?:string,elementor?:array{atomic_editor:?bool}}
     */
    public static function collect(string $elementorPluginClass = self::ELEMENTOR_PLUGIN_CLASS): array
    {
        $facts = ['v' => self::SCHEMA_VERSION];

        $template = self::themeTemplate();
        if ($template !== null) {
            $facts['theme_template'] = $template;
        }

        // Only a builder that is loaded gets a key. A site without Elementor
        // sends no `elementor` key at all, which the control plane reads as
        // "not installed", never as "the Atomic editor is off".
        if (class_exists($elementorPluginClass, false)) {
            $facts['elementor'] = ['atomic_editor' => self::atomicEditor($elementorPluginClass)];
        }

        return $facts;
    }

    /**
     * Directory name of the active theme's PARENT (the theme itself when it is
     * not a child theme), or null when it cannot be read or does not match the
     * accepted pattern.
     *
     * @return string|null
     */
    private static function themeTemplate(): ?string
    {
        if (!function_exists('get_template')) {
            return null;
        }

        try {
            $template = get_template();
        } catch (\Throwable $e) {
            return null;
        }

        if (!is_string($template)) {
            return null;
        }

        // \A ... \z, not ^ ... $: `$` also matches before a trailing newline.
        $pattern = '/\A[A-Za-z0-9._-]{1,' . self::MAX_THEME_TEMPLATE_LENGTH . '}\z/';

        return preg_match($pattern, $template) === 1 ? $template : null;
    }

    /**
     * Whether Elementor's Atomic editor is active, asked of Elementor's own
     * experiments manager. That call answers from the manager's in-memory
     * registry and writes nothing.
     *
     * Returns null for anything other than a definite boolean answer: the
     * instance or the manager is not there yet, the method is missing, the
     * call throws, or it returns a non-boolean.
     *
     * @param class-string $pluginClass Loaded Elementor main plugin class.
     * @return bool|null
     */
    private static function atomicEditor(string $pluginClass): ?bool
    {
        try {
            $instance = $pluginClass::$instance ?? null;
            if (!is_object($instance)) {
                return null;
            }

            $manager = $instance->experiments ?? null;
            if (!is_object($manager) || !is_callable([$manager, 'is_feature_active'])) {
                return null;
            }

            $active = $manager->is_feature_active(self::ELEMENTOR_ATOMIC_EXPERIMENT);

            return is_bool($active) ? $active : null;
        } catch (\Throwable $e) {
            return null;
        }
    }
}
