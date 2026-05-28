<?php
/**
 * PHPStan bootstrap: declare plugin-defined constants so static analysis can
 * reason about them without a running WordPress.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

if (!defined('WPMGR_AGENT_VERSION')) {
    define('WPMGR_AGENT_VERSION', '0.5.5-dev');
}
if (!defined('WPMGR_AGENT_FILE')) {
    define('WPMGR_AGENT_FILE', __FILE__);
}
if (!defined('WPMGR_AGENT_DIR')) {
    define('WPMGR_AGENT_DIR', __DIR__ . '/');
}
if (!defined('WPMGR_AGENT_KEY_FILE')) {
    define('WPMGR_AGENT_KEY_FILE', '/var/keys/wpmgr-agent-master.key');
}
