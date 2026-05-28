<?php
/**
 * Metadata command: collects the full site inventory pushed to the control
 * plane's /agent/v1/metadata endpoint.
 *
 * Gathers WP/PHP versions, server software, the active theme, all installed
 * plugins + versions, all installed themes + versions, and the multisite flag.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

/**
 * Builds the site metadata payload.
 */
final class MetadataCommand implements CommandInterface
{
    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'metadata';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused).
     * @param array<string,mixed> $params Request parameters (unused).
     * @return array<string,mixed>
     */
    public function execute(array $claims, array $params): array
    {
        return $this->collect();
    }

    /**
     * Collect the full metadata payload.
     *
     * @return array{
     *     wp_version:string,
     *     php_version:string,
     *     server_info:string,
     *     multisite:bool,
     *     active_theme:array{name:string,version:string,template:string,stylesheet:string},
     *     plugins:array<int,array{slug:string,name:string,version:string,active:bool}>,
     *     themes:array<int,array{slug:string,name:string,version:string,active:bool}>
     * }
     */
    public function collect(): array
    {
        return [
            'wp_version'   => $this->wpVersion(),
            'php_version'  => PHP_VERSION,
            'server_info'  => $this->serverSoftware(),
            'multisite'    => function_exists('is_multisite') ? is_multisite() : false,
            'active_theme' => $this->activeTheme(),
            'plugins'      => $this->plugins(),
            'themes'       => $this->themes(),
        ];
    }

    /**
     * Resolve the WordPress core version.
     *
     * @return string
     */
    private function wpVersion(): string
    {
        if (function_exists('get_bloginfo')) {
            $version = get_bloginfo('version');
            if (is_string($version) && $version !== '') {
                return $version;
            }
        }

        return isset($GLOBALS['wp_version']) && is_scalar($GLOBALS['wp_version'])
            ? (string) $GLOBALS['wp_version']
            : 'unknown';
    }

    /**
     * Read the web-server software string (never trusted as input).
     *
     * @return string
     */
    private function serverSoftware(): string
    {
        $value = $_SERVER['SERVER_SOFTWARE'] ?? '';

        return is_string($value) ? $value : '';
    }

    /**
     * Ensure get_plugins()/wp_get_themes() are available by loading the admin
     * plugin helpers when running outside an admin request.
     *
     * @return void
     */
    private function ensurePluginApi(): void
    {
        if (!function_exists('get_plugins') && defined('ABSPATH') && file_exists(ABSPATH . 'wp-admin/includes/plugin.php')) {
            require_once ABSPATH . 'wp-admin/includes/plugin.php';
        }
    }

    /**
     * Collect all installed plugins with version + active flag.
     *
     * @return array<int,array{slug:string,name:string,version:string,active:bool}>
     */
    private function plugins(): array
    {
        $this->ensurePluginApi();

        $active = function_exists('get_option') ? get_option('active_plugins') : [];
        if (!is_array($active)) {
            $active = [];
        }
        $activeSet = [];
        foreach ($active as $a) {
            if (is_string($a)) {
                $activeSet[$a] = true;
            }
        }

        $all = function_exists('get_plugins') ? get_plugins() : [];
        if (!is_array($all)) {
            $all = [];
        }

        $out = [];
        foreach ($all as $file => $meta) {
            if (!is_string($file)) {
                continue;
            }
            $meta  = is_array($meta) ? $meta : [];
            $out[] = [
                'slug'    => $file,
                'name'    => isset($meta['Name']) ? (string) $meta['Name'] : $file,
                'version' => isset($meta['Version']) ? (string) $meta['Version'] : 'unknown',
                'active'  => isset($activeSet[$file]),
            ];
        }

        return $out;
    }

    /**
     * Collect all installed themes with versions.
     *
     * @return array<int,array{slug:string,name:string,version:string,active:bool}>
     */
    private function themes(): array
    {
        if (!function_exists('wp_get_themes')) {
            return [];
        }

        $themes = wp_get_themes();
        if (!is_array($themes)) {
            return [];
        }

        $activeStylesheet = function_exists('get_stylesheet') ? (string) get_stylesheet() : '';

        $out = [];
        foreach ($themes as $stylesheet => $theme) {
            if (!is_object($theme) || !method_exists($theme, 'get')) {
                continue;
            }
            $slug  = (string) $stylesheet;
            $out[] = [
                'slug'    => $slug,
                'name'    => (string) $theme->get('Name'),
                'version' => (string) $theme->get('Version'),
                'active'  => $slug === $activeStylesheet,
            ];
        }

        return $out;
    }

    /**
     * Describe the active theme.
     *
     * @return array{name:string,version:string,template:string,stylesheet:string}
     */
    private function activeTheme(): array
    {
        $unknown = ['name' => 'unknown', 'version' => 'unknown', 'template' => 'unknown', 'stylesheet' => 'unknown'];

        if (!function_exists('wp_get_theme')) {
            return $unknown;
        }

        $theme = wp_get_theme();
        if (!is_object($theme) || !method_exists($theme, 'get')) {
            return $unknown;
        }

        return [
            'name'       => (string) $theme->get('Name'),
            'version'    => (string) $theme->get('Version'),
            'template'   => (string) $theme->get_template(),
            'stylesheet' => (string) $theme->get_stylesheet(),
        ];
    }
}
