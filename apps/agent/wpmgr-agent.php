<?php
/**
 * Plugin Name:       WPMgr Agent
 * Plugin URI:        https://github.com/mosamlife/wpmgr
 * Description:        Connects this WordPress site to a WPMgr control plane for backups, updates, monitoring, and security scanning.
 * Version:           0.5.8-dev
 * Requires at least: 6.0
 * Requires PHP:      8.0
 * Author:            WPMgr contributors
 * License:           MIT
 * License URI:       https://opensource.org/licenses/MIT
 * Text Domain:       wpmgr-agent
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

define('WPMGR_AGENT_VERSION', '0.5.8-dev');
define('WPMGR_AGENT_FILE', __FILE__);
define('WPMGR_AGENT_DIR', plugin_dir_path(__FILE__));

// Composer autoloader (dev tooling + third-party deps).
$wpmgr_autoload = WPMGR_AGENT_DIR . 'vendor/autoload.php';
if (file_exists($wpmgr_autoload)) {
    require_once $wpmgr_autoload;
}

/**
 * Lightweight autoloader mapping WPMgr\Agent\* to WordPress-style filenames
 * (class-*.php / interface-*.php) under includes/. This keeps the plugin's own
 * source compliant with WordPress file-naming conventions while remaining
 * Composer-friendly for vendor packages.
 *
 * @param string $class Fully-qualified class name.
 * @return void
 */
spl_autoload_register(static function (string $class): void {
    $prefix = 'WPMgr\\Agent\\';
    if (strpos($class, $prefix) !== 0) {
        return;
    }

    $relative = substr($class, strlen($prefix));
    $relative = str_replace('\\', '/', $relative);

    $dir  = dirname($relative);
    $base = basename($relative);

    // Convert StudlyCase short name to wordpress kebab-case file slug.
    $slug = strtolower(preg_replace('/(?<!^)[A-Z]/', '-$0', $base) ?? $base);

    $subdir = ($dir === '.' || $dir === '') ? '' : strtolower($dir) . '/';

    // Interfaces use the interface- prefix; everything else uses class-.
    $candidates = [
        WPMGR_AGENT_DIR . 'includes/' . $subdir . 'class-' . $slug . '.php',
        WPMGR_AGENT_DIR . 'includes/' . $subdir . 'interface-' . $slug . '.php',
    ];

    foreach ($candidates as $file) {
        if (file_exists($file)) {
            require_once $file;
            return;
        }
    }
});

// Boot the plugin once WordPress is present.
WPMgr\Agent\Plugin::boot();
