<?php
/**
 * UpdateRunner: resolves installed versions and applies plugin/theme/core
 * updates, preferring WP-CLI when available and falling back to the WordPress
 * upgrader APIs.
 *
 * This class is the single seam between the command logic and the WordPress /
 * shell runtime, which keeps the command itself unit-testable.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * Executes updates via WP-CLI or the WP upgrader APIs.
 */
class UpdateRunner
{
    /**
     * Validate an untrusted version string before it reaches WP-CLI argv or an
     * upgrader offer URL. Accepts only the literal "latest" or a token that
     * starts with a digit and contains only [0-9A-Za-z.-] (no spaces, no flag
     * separators), which blocks argument injection such as "latest --activate"
     * or "1.0 --activate".
     *
     * @param string $version Raw requested version.
     * @return bool True when safe to use.
     */
    public static function isValidVersion(string $version): bool
    {
        if ($version === 'latest') {
            return true;
        }

        return preg_match('#^[0-9][0-9A-Za-z.\-]*$#', $version) === 1;
    }

    /**
     * Resolve the currently installed version of an item.
     *
     * @param string $type plugin|theme|core.
     * @param string $slug Sanitized slug (ignored for core).
     * @return string Installed version, or '' when unknown.
     */
    public function currentVersion(string $type, string $slug): string
    {
        switch ($type) {
            case 'core':
                if (function_exists('get_bloginfo')) {
                    $v = get_bloginfo('version');
                    if (is_string($v) && $v !== '') {
                        return $v;
                    }
                }

                return isset($GLOBALS['wp_version']) && is_scalar($GLOBALS['wp_version'])
                    ? (string) $GLOBALS['wp_version']
                    : '';

            case 'plugin':
                return $this->pluginVersion($slug);

            case 'theme':
                return $this->themeVersion($slug);
        }

        return '';
    }

    /**
     * Resolve the version that an update would move the item to.
     *
     * Used only by dry-run. For an explicit version request we return it as-is.
     * For 'latest' we consult the WordPress update transients / version check.
     *
     * @param string $type      plugin|theme|core.
     * @param string $slug      Sanitized slug.
     * @param string $requested 'latest' or an explicit x.y.z.
     * @return string Target version, or '' when none is available.
     */
    public function availableVersion(string $type, string $slug, string $requested): string
    {
        if ($requested !== 'latest') {
            return $requested;
        }

        switch ($type) {
            case 'plugin':
                return $this->pluginUpdateVersion($slug);
            case 'theme':
                return $this->themeUpdateVersion($slug);
            case 'core':
                return $this->coreUpdateVersion();
        }

        return '';
    }

    /**
     * Apply the update. Prefers WP-CLI; falls back to the upgrader APIs.
     *
     * @param string $type    plugin|theme|core.
     * @param string $slug    Sanitized slug.
     * @param string $version 'latest' or an explicit x.y.z.
     * @return array{ok:bool,log:string}
     */
    public function apply(string $type, string $slug, string $version): array
    {
        if (!self::isValidVersion($version)) {
            return ['ok' => false, 'log' => 'Rejected unsafe version string.'];
        }

        if ($this->wpCliAvailable()) {
            return $this->applyViaWpCli($type, $slug, $version);
        }

        return $this->applyViaUpgrader($type, $slug, $version);
    }

    /**
     * Force a core downgrade/upgrade to an explicit version (rollback).
     *
     * Prefers WP-CLI `core update --version=<v> --force`; falls back to
     * Core_Upgrader with a forced offer for the requested version.
     *
     * @param string $version Explicit target version (x.y.z).
     * @return array{ok:bool,log:string}
     */
    public function forceCore(string $version): array
    {
        // forceCore always targets an explicit version; "latest" is not a valid
        // rollback target and the literal must never reach the offer URL / argv.
        if ($version === 'latest' || !self::isValidVersion($version)) {
            return ['ok' => false, 'log' => 'Rejected unsafe version string.'];
        }

        if ($this->wpCliAvailable()) {
            return $this->runWpCli([
                'core',
                'update',
                '--version=' . $version,
                '--force',
                '--skip-plugins',
                '--skip-themes',
            ]);
        }

        return $this->forceCoreViaUpgrader($version);
    }

    /**
     * Force a core version via Core_Upgrader using a synthetic offer URL.
     *
     * @param string $version Explicit target version.
     * @return array{ok:bool,log:string}
     */
    protected function forceCoreViaUpgrader(string $version): array
    {
        $this->loadUpgraderApi();

        if (!class_exists('\Core_Upgrader') || !class_exists('\WP_Ajax_Upgrader_Skin')) {
            return ['ok' => false, 'log' => 'Core upgrader API unavailable.'];
        }

        $locale = function_exists('get_locale') ? (string) get_locale() : 'en_US';

        $offer                  = new \stdClass();
        $offer->response        = 'upgrade';
        $offer->current         = $version;
        $offer->version         = $version;
        $offer->download        = 'https://downloads.wordpress.org/release/wordpress-' . $version . '.zip';
        $offer->locale          = $locale;
        $offer->packages        = (object) [
            'full'        => $offer->download,
            'no_content'  => false,
            'new_bundled' => false,
            'partial'     => false,
            'rollback'    => false,
        ];
        $offer->php_version     = '5.6.20';
        $offer->mysql_version   = '5.0';
        $offer->new_bundled     = '';
        $offer->partial_version = '';

        $upgrader = new \Core_Upgrader(new \WP_Ajax_Upgrader_Skin());
        $result   = $upgrader->upgrade($offer);

        return $this->upgraderOutcome($result);
    }

    // ---------------------------------------------------------------------
    // WP-CLI path
    // ---------------------------------------------------------------------

    /**
     * Is a WP-CLI execution context available?
     *
     * @return bool
     */
    public function wpCliAvailable(): bool
    {
        return defined('WP_CLI') && WP_CLI;
    }

    /**
     * Apply an update through the WP-CLI runner.
     *
     * @param string $type    plugin|theme|core.
     * @param string $slug    Sanitized slug.
     * @param string $version 'latest' or x.y.z.
     * @return array{ok:bool,log:string}
     */
    private function applyViaWpCli(string $type, string $slug, string $version): array
    {
        $args = match ($type) {
            'plugin' => ['plugin', 'update', $slug, '--skip-themes'],
            'theme'  => ['theme', 'update', $slug, '--skip-plugins'],
            'core'   => array_merge(
                ['core', 'update', '--skip-plugins', '--skip-themes'],
                $version !== 'latest' ? ['--version=' . $version] : []
            ),
            default  => [],
        };

        if ($args === []) {
            return ['ok' => false, 'log' => 'Unsupported type for WP-CLI update.'];
        }

        if ($type !== 'core' && $version !== 'latest') {
            $args[] = '--version=' . $version;
        }

        return $this->runWpCli($args);
    }

    /**
     * Invoke WP-CLI's runcommand and capture its output. Isolated so tests can
     * stub it.
     *
     * @param array<int,string> $args Command argument vector.
     * @return array{ok:bool,log:string}
     */
    protected function runWpCli(array $args): array
    {
        if (!class_exists('\WP_CLI')) {
            return ['ok' => false, 'log' => 'WP-CLI not loadable.'];
        }

        $command = implode(' ', $args);

        try {
            /** @var array{stdout?:string,stderr?:string,return_code?:int} $res */
            $res = \WP_CLI::runcommand(
                $command,
                ['return' => 'all', 'exit_error' => false, 'launch' => false]
            );

            $code   = isset($res['return_code']) ? (int) $res['return_code'] : 0;
            $stdout = isset($res['stdout']) ? (string) $res['stdout'] : '';
            $stderr = isset($res['stderr']) ? (string) $res['stderr'] : '';

            return [
                'ok'  => $code === 0,
                'log' => trim($stdout . "\n" . $stderr),
            ];
        } catch (\Throwable $e) {
            return ['ok' => false, 'log' => 'WP-CLI execution error.'];
        }
    }

    // ---------------------------------------------------------------------
    // Upgrader (PHP fallback) path
    // ---------------------------------------------------------------------

    /**
     * Apply an update through the WordPress upgrader APIs under a quiet skin.
     *
     * @param string $type    plugin|theme|core.
     * @param string $slug    Sanitized slug.
     * @param string $version 'latest' or x.y.z.
     * @return array{ok:bool,log:string}
     */
    protected function applyViaUpgrader(string $type, string $slug, string $version): array
    {
        $this->loadUpgraderApi();

        if (function_exists('wp_update_plugins') && $type === 'plugin') {
            wp_update_plugins();
        }
        if (function_exists('wp_update_themes') && $type === 'theme') {
            wp_update_themes();
        }

        switch ($type) {
            case 'plugin':
                if (!class_exists('\Plugin_Upgrader') || !class_exists('\WP_Ajax_Upgrader_Skin')) {
                    return ['ok' => false, 'log' => 'Upgrader API unavailable.'];
                }

                // Capture active state BEFORE upgrade. WordPress's
                // Plugin_Upgrader::upgrade() registers an upgrader_pre_install
                // hook that calls deactivate_plugins($plugin, silent=true) and
                // does NOT re-activate after the upgrade finishes. WP-CLI's
                // `wp plugin update` preserves active state; we mirror that
                // behaviour here so the PHP-fallback path doesn't strand an
                // active plugin inactive after a successful upgrade.
                $pluginsFilePath  = WPMGR_AGENT_DIR; // unused — silence linters about the var
                $wasActive        = function_exists('is_plugin_active')             ? \is_plugin_active($slug) : false;
                $wasNetworkActive = function_exists('is_plugin_active_for_network') ? \is_plugin_active_for_network($slug) : false;

                $upgrader = new \Plugin_Upgrader(new \WP_Ajax_Upgrader_Skin());
                $result   = $upgrader->upgrade($slug);
                $outcome  = $this->upgraderOutcome($result);

                if ($outcome['ok'] && ($wasActive || $wasNetworkActive) && function_exists('activate_plugin')) {
                    // Refresh plugin caches before reactivating: the upgrade
                    // may have changed the main plugin file (slug stays the
                    // same but the metadata cache is stale).
                    if (function_exists('wp_clean_plugins_cache')) {
                        \wp_clean_plugins_cache(true);
                    }
                    $activated = \activate_plugin($slug, '', $wasNetworkActive, true);
                    if (\is_wp_error($activated)) {
                        $outcome['log'] .= "\n[wpmgr] upgrade succeeded but reactivation failed: "
                            . $activated->get_error_message();
                        error_log('WPMgr Agent: post-upgrade reactivation failed for '
                            . $slug . ': ' . $activated->get_error_message());
                    }
                }

                return $outcome;

            case 'theme':
                if (!class_exists('\Theme_Upgrader') || !class_exists('\WP_Ajax_Upgrader_Skin')) {
                    return ['ok' => false, 'log' => 'Upgrader API unavailable.'];
                }
                $upgrader = new \Theme_Upgrader(new \WP_Ajax_Upgrader_Skin());
                $result   = $upgrader->upgrade($slug);

                return $this->upgraderOutcome($result);

            case 'core':
                return $this->applyCoreUpgrade($version);
        }

        return ['ok' => false, 'log' => 'Unsupported type.'];
    }

    /**
     * Run a core upgrade via Core_Upgrader to the requested (or latest) version.
     *
     * @param string $version 'latest' or x.y.z.
     * @return array{ok:bool,log:string}
     */
    private function applyCoreUpgrade(string $version): array
    {
        if (function_exists('wp_version_check')) {
            wp_version_check([], true);
        }

        if (!class_exists('\Core_Upgrader') || !function_exists('get_core_updates')) {
            return ['ok' => false, 'log' => 'Core upgrader API unavailable.'];
        }

        $updates = get_core_updates();
        if (!is_array($updates) || $updates === []) {
            return ['ok' => false, 'log' => 'No core update offer available.'];
        }

        $offer = null;
        foreach ($updates as $candidate) {
            if (!is_object($candidate)) {
                continue;
            }
            $candidateVersion = isset($candidate->version) ? (string) $candidate->version : '';
            if ($version === 'latest' || $candidateVersion === $version) {
                $offer = $candidate;
                break;
            }
        }

        if ($offer === null) {
            return ['ok' => false, 'log' => 'Requested core version not offered.'];
        }

        if (!class_exists('\WP_Ajax_Upgrader_Skin')) {
            return ['ok' => false, 'log' => 'Upgrader skin unavailable.'];
        }

        $upgrader = new \Core_Upgrader(new \WP_Ajax_Upgrader_Skin());
        $result   = $upgrader->upgrade($offer);

        return $this->upgraderOutcome($result);
    }

    /**
     * Normalize an upgrader return value into the ok/log shape.
     *
     * @param mixed $result Upgrader result (bool|array|WP_Error|null).
     * @return array{ok:bool,log:string}
     */
    private function upgraderOutcome($result): array
    {
        if (is_object($result) && method_exists($result, 'get_error_message')) {
            /** @var \WP_Error $result */
            return ['ok' => false, 'log' => (string) $result->get_error_message()];
        }

        if ($result === false || $result === null) {
            return ['ok' => false, 'log' => 'Update failed.'];
        }

        return ['ok' => true, 'log' => 'Update applied via upgrader.'];
    }

    /**
     * Ensure the wp-admin upgrade/plugin/theme/file APIs are loaded.
     *
     * @return void
     */
    private function loadUpgraderApi(): void
    {
        if (!defined('ABSPATH')) {
            return;
        }
        $base = ABSPATH . 'wp-admin/includes/';
        foreach (['plugin.php', 'theme.php', 'file.php', 'misc.php', 'class-wp-upgrader.php', 'update.php'] as $file) {
            if (file_exists($base . $file)) {
                require_once $base . $file;
            }
        }
    }

    // ---------------------------------------------------------------------
    // Version helpers
    // ---------------------------------------------------------------------

    /**
     * Installed version of a plugin by its basename (folder/file.php) or folder.
     *
     * @param string $slug Plugin basename or folder.
     * @return string
     */
    private function pluginVersion(string $slug): string
    {
        $this->loadPluginApi();
        if (!function_exists('get_plugins')) {
            return '';
        }

        $all = get_plugins();
        if (!is_array($all)) {
            return '';
        }

        if (isset($all[$slug]) && is_array($all[$slug]) && isset($all[$slug]['Version'])) {
            return (string) $all[$slug]['Version'];
        }

        // Allow a folder-only slug to match its "folder/..." basename.
        foreach ($all as $file => $meta) {
            if (!is_string($file) || !is_array($meta)) {
                continue;
            }
            if (str_starts_with($file, $slug . '/') && isset($meta['Version'])) {
                return (string) $meta['Version'];
            }
        }

        return '';
    }

    /**
     * Installed version of a theme by stylesheet.
     *
     * @param string $slug Theme stylesheet.
     * @return string
     */
    private function themeVersion(string $slug): string
    {
        if (!function_exists('wp_get_themes')) {
            return '';
        }

        $themes = wp_get_themes();
        if (!is_array($themes) || !isset($themes[$slug])) {
            return '';
        }

        $theme = $themes[$slug];
        if (is_object($theme) && method_exists($theme, 'get')) {
            return (string) $theme->get('Version');
        }

        return '';
    }

    /**
     * Pending update version for a plugin from the update transient.
     *
     * @param string $slug Plugin basename.
     * @return string
     */
    private function pluginUpdateVersion(string $slug): string
    {
        if (!function_exists('get_site_transient')) {
            return '';
        }
        $transient = get_site_transient('update_plugins');
        if (!is_object($transient) || !isset($transient->response) || !is_array($transient->response)) {
            return '';
        }
        $entry = $transient->response[$slug] ?? null;
        if (is_object($entry) && isset($entry->new_version)) {
            return (string) $entry->new_version;
        }

        return '';
    }

    /**
     * Pending update version for a theme from the update transient.
     *
     * @param string $slug Theme stylesheet.
     * @return string
     */
    private function themeUpdateVersion(string $slug): string
    {
        if (!function_exists('get_site_transient')) {
            return '';
        }
        $transient = get_site_transient('update_themes');
        if (!is_object($transient) || !isset($transient->response) || !is_array($transient->response)) {
            return '';
        }
        $entry = $transient->response[$slug] ?? null;
        if (is_array($entry) && isset($entry['new_version'])) {
            return (string) $entry['new_version'];
        }

        return '';
    }

    /**
     * Latest offered core version from the update transient.
     *
     * @return string
     */
    private function coreUpdateVersion(): string
    {
        if (!function_exists('get_site_transient')) {
            return '';
        }
        $transient = get_site_transient('update_core');
        if (!is_object($transient) || !isset($transient->updates) || !is_array($transient->updates)) {
            return '';
        }
        foreach ($transient->updates as $update) {
            if (is_object($update) && isset($update->response) && $update->response === 'upgrade' && isset($update->version)) {
                return (string) $update->version;
            }
        }

        return '';
    }

    /**
     * Load the wp-admin plugin helper so get_plugins() is available.
     *
     * @return void
     */
    private function loadPluginApi(): void
    {
        if (!function_exists('get_plugins') && defined('ABSPATH') && file_exists(ABSPATH . 'wp-admin/includes/plugin.php')) {
            require_once ABSPATH . 'wp-admin/includes/plugin.php';
        }
    }
}
