<?php
/**
 * Info command: reports environment metadata to the control plane.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

/**
 * Returns WP/PHP versions, active plugins/themes (with versions), multisite.
 */
final class InfoCommand implements CommandInterface
{
    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'info';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims.
     * @param array<string,mixed> $params Request parameters.
     * @return array<string,mixed>
     */
    public function execute(array $claims, array $params): array
    {
        return [
            'wp_version'     => $this->wpVersion(),
            'php_version'    => PHP_VERSION,
            'is_multisite'   => function_exists('is_multisite') ? is_multisite() : false,
            'active_plugins' => $this->activePlugins(),
            'active_theme'   => $this->activeTheme(),
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

        // $wp_version is a WordPress core global, not described by the stubs.
        return isset($GLOBALS['wp_version']) && is_scalar($GLOBALS['wp_version'])
            ? (string) $GLOBALS['wp_version']
            : 'unknown';
    }

    /**
     * Collect active plugins with their versions.
     *
     * @return array<int,array{plugin:string,name:string,version:string}>
     */
    private function activePlugins(): array
    {
        if (!function_exists('get_option') || !function_exists('get_plugins')) {
            // get_plugins lives in wp-admin/includes/plugin.php; load it if we can.
            if (function_exists('get_option') && defined('ABSPATH') && file_exists(ABSPATH . 'wp-admin/includes/plugin.php')) {
                require_once ABSPATH . 'wp-admin/includes/plugin.php';
            }
        }

        $active = function_exists('get_option') ? get_option('active_plugins') : [];
        if (!is_array($active)) {
            $active = [];
        }

        $all = function_exists('get_plugins') ? get_plugins() : [];
        if (!is_array($all)) {
            $all = [];
        }

        $out = [];
        foreach ($active as $file) {
            if (!is_string($file)) {
                continue;
            }
            $meta = isset($all[$file]) && is_array($all[$file]) ? $all[$file] : [];
            $out[] = [
                'plugin'  => $file,
                'name'    => isset($meta['Name']) ? (string) $meta['Name'] : $file,
                'version' => isset($meta['Version']) ? (string) $meta['Version'] : 'unknown',
            ];
        }

        return $out;
    }

    /**
     * Describe the active theme.
     *
     * @return array{name:string,version:string,template:string}
     */
    private function activeTheme(): array
    {
        if (!function_exists('wp_get_theme')) {
            return ['name' => 'unknown', 'version' => 'unknown', 'template' => 'unknown'];
        }

        $theme = wp_get_theme();

        return [
            'name'     => (string) $theme->get('Name'),
            'version'  => (string) $theme->get('Version'),
            'template' => (string) $theme->get_template(),
        ];
    }
}
