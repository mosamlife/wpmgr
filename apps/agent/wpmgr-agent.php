<?php
/**
 * Plugin Name:       WPMgr Agent
 * Plugin URI:        https://github.com/mosamlife/wpmgr
 * Description:        Connects this WordPress site to a WPMgr control plane for backups, updates, monitoring, and security scanning.
 * Version:           0.0.0
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

define('WPMGR_AGENT_VERSION', '0.0.0');
define('WPMGR_AGENT_FILE', __FILE__);
define('WPMGR_AGENT_DIR', plugin_dir_path(__FILE__));

// Composer autoloader (PSR-4: WPMgr\Agent\ => includes/).
$wpmgr_autoload = WPMGR_AGENT_DIR . 'vendor/autoload.php';
if (file_exists($wpmgr_autoload)) {
    require_once $wpmgr_autoload;
}

// Real bootstrap (Plugin::boot, REST routes, keystore, JWT verify) is built in
// Phase 4 by the wp-agent-engineer subagent.
