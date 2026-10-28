<?php
/**
 * TextDomain: loads this plugin's own translation catalogues from its
 * languages/ folder.
 *
 * Every user-visible string in the agent uses the `wpmgr-agent` text domain.
 * Without a load, a catalogue dropped into languages/ (wpmgr-agent-de_DE.mo and
 * so on) is never read and every string stays English.
 *
 * The load happens on `init`, the hook WordPress documents for it. A string
 * that is translated before `init` runs (during plugin include or on an earlier
 * hook) is therefore returned untranslated.
 *
 * The wordpress.org build does not load anything here. WordPress downloads and
 * loads language packs for a directory plugin's slug on its own, and Plugin
 * Check discourages the explicit call for directory plugins. That build defines
 * WPMGR_WPORG_BUILD (see the agent-zip-wporg Makefile target), which turns this
 * class into a no-op: no hook is bound and load_plugin_textdomain() is never
 * reached.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/**
 * Binds and performs the plugin's text-domain load.
 */
final class TextDomain
{
    /**
     * Bind the loader to `init`. Does nothing in the wp.org build.
     *
     * @return void
     */
    public static function register(): void
    {
        if (self::isWporgBuild()) {
            return;
        }

        add_action('init', [self::class, 'load']);
    }

    /**
     * Load the `wpmgr-agent` catalogue from the plugin's languages/ folder.
     *
     * Returns false, without calling WordPress, in the wp.org build and when
     * the main-file constant is not defined (there is no folder to derive
     * without it, and guessing one would point the load at the wrong place).
     *
     * @return bool Whether a catalogue was loaded.
     */
    public static function load(): bool
    {
        if (self::isWporgBuild() || !defined('WPMGR_AGENT_FILE')) {
            return false;
        }

        // phpcs:ignore PluginCheck.CodeAnalysis.DiscouragedFunctions.load_plugin_textdomainFound -- self-hosted and managed builds only; the wp.org build defines WPMGR_WPORG_BUILD and returns above, because wordpress.org serves its language packs itself.
        return load_plugin_textdomain('wpmgr-agent', false, self::languagesPath((string) constant('WPMGR_AGENT_FILE')));
    }

    /**
     * The languages/ folder, as a path relative to the plugins directory, for
     * the plugin whose main file is $mainFile. Derived the way WordPress
     * derives the plugin basename, so it follows the folder the plugin is
     * actually installed in.
     *
     * @param string $mainFile Absolute path of the plugin's main file.
     * @return string e.g. "wpmgr-agent/languages".
     */
    public static function languagesPath(string $mainFile): string
    {
        return dirname(plugin_basename($mainFile)) . '/languages';
    }

    /**
     * Whether this is the wordpress.org distribution build.
     *
     * @return bool
     */
    private static function isWporgBuild(): bool
    {
        return defined('WPMGR_WPORG_BUILD') && (bool) constant('WPMGR_WPORG_BUILD');
    }
}
