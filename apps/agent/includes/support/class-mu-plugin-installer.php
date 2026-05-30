<?php
/**
 * MuPluginInstaller (ADR-037 Sprint 2): copy the error-trap loader into the
 * site's mu-plugins/ directory.
 *
 * Why this exists:
 *   `set_error_handler` registered from a normal plugin only fires AFTER the
 *   plugin's autoloader runs. Errors that occur DURING the bootstrap of OTHER
 *   plugins (e.g. a fatal in a third-party plugin's main file) are missed.
 *
 *   A mu-plugin loads at the very top of WordPress's plugin-loading pass, so a
 *   handler registered there traps fatals throughout the rest of bootstrap.
 *   This installer copies our trap loader into mu-plugins/ on activation; the
 *   loader queues errors into a global the main plugin drains once it boots.
 *
 * Install path:
 *   - Source: WPMGR_AGENT_DIR/mu-plugin-loader/a-wpmgr-error-trap.php
 *   - Dest:   WPMU_PLUGIN_DIR/a-wpmgr-error-trap.php
 *
 * Filename starts with `a-` so directory-alphabetical sort places it FIRST
 * among installed mu-plugins. WordPress loads mu-plugins in `glob()` order
 * (which is alphabetical on all sane filesystems).
 *
 * Failure modes:
 *   - mu-plugins/ not writable → silent fail (the operator gets a fix-it notice
 *     surfaced via the diagnostics endpoint, not a fatal)
 *   - Directory does not exist → create with mkdir(); on failure, silent fail
 *   - Same file already present (re-activation, content unchanged) → no-op
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * Idempotent installer for the agent's error-trap mu-plugin loader.
 */
final class MuPluginInstaller
{
    /** Destination basename within wp-content/mu-plugins/ for the error-trap. */
    public const DEST_BASENAME = 'a-wpmgr-error-trap.php';

    /** Source basename within the plugin's `mu-plugin-loader/` directory (error-trap). */
    public const SOURCE_BASENAME = 'a-wpmgr-error-trap.php';

    /** Destination basename within wp-content/mu-plugins/ for the WAF IP gate. */
    public const WAF_DEST_BASENAME = 'a-wpmgr-waf.php';

    /** Source basename within the plugin's `mu-plugin-loader/` directory (WAF gate). */
    public const WAF_SOURCE_BASENAME = 'a-wpmgr-waf.php';

    /** wp-options flag set when error-trap install has been verified at least once. */
    public const OPTION_INSTALLED = 'wpmgr_agent_mu_installed_at';

    /** wp-options flag set when WAF install has been verified at least once. */
    public const OPTION_WAF_INSTALLED = 'wpmgr_agent_mu_waf_installed_at';

    private string $pluginDir;

    /**
     * @param string $pluginDir The plugin's root directory (WPMGR_AGENT_DIR).
     */
    public function __construct(string $pluginDir)
    {
        $this->pluginDir = rtrim($pluginDir, '/\\');
    }

    /**
     * Ensure the mu-plugin file is present and up-to-date. Returns true when
     * the mu-plugin is in place; false when install was attempted but failed.
     *
     * Idempotent: same source content + same destination content = no-op.
     *
     * @return bool
     */
    public function install(): bool
    {
        $source = $this->pluginDir . '/mu-plugin-loader/' . self::SOURCE_BASENAME;
        if (!file_exists($source) || !is_readable($source)) {
            return false;
        }

        $muDir = defined('WPMU_PLUGIN_DIR') ? (string) constant('WPMU_PLUGIN_DIR') : (defined('WP_CONTENT_DIR') ? constant('WP_CONTENT_DIR') . '/mu-plugins' : '');
        if ($muDir === '') {
            return false;
        }

        if (!is_dir($muDir)) {
            // Best-effort create; on a host where the WP user cannot write
            // wp-content/, this returns false and we surface that via
            // diagnostics rather than fatal.
            if (!@mkdir($muDir, 0755, true) && !is_dir($muDir)) {
                return false;
            }
        }

        $dest = rtrim($muDir, '/\\') . '/' . self::DEST_BASENAME;

        // Content-fingerprint short-circuit: if the destination matches the
        // source byte-for-byte, do nothing. This is the common path (every
        // activation hit after the first).
        if (file_exists($dest) && @sha1_file($dest) === @sha1_file($source)) {
            $this->markInstalled();
            return true;
        }

        $bytes = @file_get_contents($source);
        if ($bytes === false) {
            return false;
        }
        $written = @file_put_contents($dest, $bytes);
        if ($written === false) {
            return false;
        }
        $this->markInstalled();
        return true;
    }

    /**
     * Ensure the WAF mu-plugin file is present and up-to-date. Mirrors install()
     * but operates on the WAF source/destination pair.
     *
     * The WAF gate (a-wpmgr-waf.php) reads wpmgr_security_config directly via
     * $wpdb and fires BEFORE WordPress boots, so updating it to match the
     * source on every plugins_loaded call guarantees the gate binary stays
     * current even after a plugin update that rewrote the source file.
     *
     * Idempotent: same source content + same destination content = no-op.
     *
     * @return bool True when the WAF mu-plugin is in place; false when install
     *              was attempted but failed (e.g. mu-plugins/ not writable).
     */
    public function installWaf(): bool
    {
        $source = $this->pluginDir . '/mu-plugin-loader/' . self::WAF_SOURCE_BASENAME;
        if (!file_exists($source) || !is_readable($source)) {
            return false;
        }

        $muDir = defined('WPMU_PLUGIN_DIR') ? (string) constant('WPMU_PLUGIN_DIR') : (defined('WP_CONTENT_DIR') ? constant('WP_CONTENT_DIR') . '/mu-plugins' : '');
        if ($muDir === '') {
            return false;
        }

        if (!is_dir($muDir)) {
            if (!@mkdir($muDir, 0755, true) && !is_dir($muDir)) {
                return false;
            }
        }

        $dest = rtrim($muDir, '/\\') . '/' . self::WAF_DEST_BASENAME;

        // Content-fingerprint short-circuit: if the destination matches the
        // source byte-for-byte, do nothing.
        if (file_exists($dest) && @sha1_file($dest) === @sha1_file($source)) {
            $this->markWafInstalled();
            return true;
        }

        $bytes = @file_get_contents($source);
        if ($bytes === false) {
            return false;
        }
        $written = @file_put_contents($dest, $bytes);
        if ($written === false) {
            return false;
        }
        $this->markWafInstalled();
        return true;
    }

    /**
     * Remove the WAF mu-plugin file. Called on plugin deactivation.
     *
     * @return bool
     */
    public function uninstallWaf(): bool
    {
        $muDir = defined('WPMU_PLUGIN_DIR') ? (string) constant('WPMU_PLUGIN_DIR') : '';
        if ($muDir === '') {
            return false;
        }
        $dest = rtrim($muDir, '/\\') . '/' . self::WAF_DEST_BASENAME;
        if (!file_exists($dest)) {
            return true;
        }
        if (function_exists('delete_option')) {
            delete_option(self::OPTION_WAF_INSTALLED);
        }
        return (bool) @unlink($dest);
    }

    /**
     * Report whether the WAF mu-plugin is currently present.
     */
    public function isWafInstalled(): bool
    {
        $muDir = defined('WPMU_PLUGIN_DIR') ? (string) constant('WPMU_PLUGIN_DIR') : '';
        if ($muDir === '') {
            return false;
        }
        $dest = rtrim($muDir, '/\\') . '/' . self::WAF_DEST_BASENAME;
        return file_exists($dest);
    }

    private function markWafInstalled(): void
    {
        if (!function_exists('update_option')) {
            return;
        }
        update_option(self::OPTION_WAF_INSTALLED, time(), false);
    }

    /**
     * Remove the mu-plugin file. Called on plugin deactivation if the
     * operator wants a clean uninstall.
     *
     * @return bool
     */
    public function uninstall(): bool
    {
        $muDir = defined('WPMU_PLUGIN_DIR') ? (string) constant('WPMU_PLUGIN_DIR') : '';
        if ($muDir === '') {
            return false;
        }
        $dest = rtrim($muDir, '/\\') . '/' . self::DEST_BASENAME;
        if (!file_exists($dest)) {
            return true;
        }
        if (function_exists('delete_option')) {
            delete_option(self::OPTION_INSTALLED);
        }
        return (bool) @unlink($dest);
    }

    /**
     * Report whether the mu-plugin is currently present.
     */
    public function isInstalled(): bool
    {
        $muDir = defined('WPMU_PLUGIN_DIR') ? (string) constant('WPMU_PLUGIN_DIR') : '';
        if ($muDir === '') {
            return false;
        }
        $dest = rtrim($muDir, '/\\') . '/' . self::DEST_BASENAME;
        return file_exists($dest);
    }

    private function markInstalled(): void
    {
        if (!function_exists('update_option')) {
            return;
        }
        update_option(self::OPTION_INSTALLED, time(), false);
    }
}
