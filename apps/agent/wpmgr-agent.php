<?php
/**
 * Plugin Name:       WPMgr Agent
 * Plugin URI:        https://github.com/mosamlife/wpmgr
 * Description:        Connects this WordPress site to a WPMgr control plane for backups, updates, monitoring, and security scanning.
 * Version:           0.61.112
 * Requires at least: 6.2
 * Requires PHP:      8.1
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

define('WPMGR_AGENT_VERSION', '0.61.112');

// Display name shown on the admin settings screen (menu label, page title).
// Kept as its own constant (rather than hard-coded strings in class-admin.php)
// so the wp.org build can rewrite it to match that build's listing identity
// ("Fleet Agent Site Manager") without touching the self-hosted build, which
// keeps "WPMgr Agent". See the agent-zip-wporg Makefile target.
define('WPMGR_AGENT_DISPLAY_NAME', 'WPMgr Agent');

define('WPMGR_AGENT_FILE', __FILE__);
define('WPMGR_AGENT_DIR', plugin_dir_path(__FILE__));

// Composer autoloader (dev tooling + third-party deps). When present, its
// classmap autoloader is tried first by PHP's autoload chain.
$wpmgr_autoload = WPMGR_AGENT_DIR . 'vendor/autoload.php';
if (file_exists($wpmgr_autoload)) {
    require_once $wpmgr_autoload;
}

// The plugin's own class resolver. Required directly (not autoloaded) so it
// is available even on installs that ship without a Composer vendor/
// directory (a git clone or a GitHub source ZIP without `composer install`),
// where it becomes the sole autoloader for the plugin's own classes. See
// includes/class-autoloader.php for the resolution algorithm.
require_once WPMGR_AGENT_DIR . 'includes/class-autoloader.php';

/**
 * Maps WPMgr\Agent\* class names to their WordPress-style filenames
 * (class-*.php / interface-*.php) under includes/ via Autoloader::resolve().
 * This keeps the plugin's own source compliant with WordPress file-naming
 * conventions while remaining Composer-friendly for vendor packages.
 *
 * @param string $class Fully-qualified class name.
 * @return void
 */
spl_autoload_register(static function (string $class): void {
    $file = WPMgr\Agent\Autoloader::resolve($class);
    if ($file !== null) {
        require_once $file;
    }
});

// Relocate this plugin's own signed Authorization bearer token out of
// $_SERVER, when present, before any other plugin's early REST auth hooks
// (determine_current_user / rest_authentication_errors) get a chance to run.
// Some third-party plugins install a global handler that tries to decode ANY
// Authorization: Bearer value as one of their own JWTs and can fatal on a
// token in a format they don't recognize; those hooks fire strictly AFTER
// every active plugin's main file has been included, so doing this here,
// synchronously at include time, preempts them unconditionally. See
// includes/support/class-auth-header-shield.php.
WPMgr\Agent\Support\AuthHeaderShield::protect();

// Boot the plugin once WordPress is present.
WPMgr\Agent\Plugin::boot();
