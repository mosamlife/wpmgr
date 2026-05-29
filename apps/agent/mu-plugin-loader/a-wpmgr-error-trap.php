<?php
/**
 * Plugin Name: WPMgr Error Trap (mu-plugin loader)
 * Description: Registers a PHP error/shutdown handler at the very top of the
 *              WordPress plugin-loading pass so errors that occur DURING the
 *              bootstrap of OTHER plugins still get captured by WPMgr Agent.
 *              The main WPMgr Agent plugin drains the in-memory queue once it
 *              boots.
 * Version:     1.0.0
 * Author:      WPMgr contributors
 * License:     MIT
 *
 * Installed by:
 *   `WPMgr\Agent\Support\MuPluginInstaller::install()` — called from the agent
 *   plugin activation hook + on every `plugins_loaded` (the installer is
 *   idempotent: same content → no-op).
 *
 * Filename starts with `a-` so directory-alphabetical sort places it FIRST
 * among installed mu-plugins (WordPress loads mu-plugins via `glob()` which
 * returns alphabetical order on every platform we ship to).
 *
 * Bootstrap-safe:
 *   - Pure procedural, no autoloader, no WPMgr namespace dependency
 *   - Uses a single PHP superglobal slot `$GLOBALS['wpmgr_agent_pending_errors']`
 *     so the main plugin's `ErrorMonitor::drainBootstrapQueue()` can scoop
 *     captured errors when it boots
 *   - Caps the queue at 200 entries to bound memory growth if WPMgr Agent
 *     fails to boot at all
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/**
 * Initialize the queue if needed and append one entry.
 *
 * The agent's `ErrorMonitor::GLOBAL_PENDING` constant is `wpmgr_agent_pending_errors`;
 * we hardcode the same string here so the mu-plugin has zero dependencies on
 * the main plugin code (which may not even be loaded yet).
 *
 * @param int $code
 * @param string $message
 * @param string $file
 * @param int $line
 * @return void
 */
function wpmgr_mu_error_trap_record(int $code, string $message, string $file, int $line): void
{
    if (!isset($GLOBALS['wpmgr_agent_pending_errors']) || !is_array($GLOBALS['wpmgr_agent_pending_errors'])) {
        $GLOBALS['wpmgr_agent_pending_errors'] = [];
    }
    // Cap the queue so a flood during bootstrap can't OOM the request before
    // the main plugin even has a chance to drain it.
    if (count($GLOBALS['wpmgr_agent_pending_errors']) >= 200) {
        return;
    }
    $GLOBALS['wpmgr_agent_pending_errors'][] = [
        'code'    => $code,
        'message' => $message,
        'file'    => $file,
        'line'    => $line,
        'ts'      => time(),
    ];
}

/**
 * Non-fatal error handler. Always returns false so PHP's normal error
 * reporting runs unmodified.
 *
 * @param int $code
 * @param string $message
 * @param string $file
 * @param int $line
 * @return bool
 */
function wpmgr_mu_error_trap_handle(int $code, string $message, string $file = '', int $line = 0): bool
{
    wpmgr_mu_error_trap_record($code, $message, $file, $line);
    return false;
}

/**
 * Shutdown handler — captures fatal errors that bypass set_error_handler.
 *
 * @return void
 */
function wpmgr_mu_error_trap_shutdown(): void
{
    $err = error_get_last();
    if (!is_array($err)) {
        return;
    }
    $code = (int) ($err['type'] ?? 0);
    $fatalMask = E_ERROR | E_PARSE | E_CORE_ERROR | E_COMPILE_ERROR | E_USER_ERROR | E_RECOVERABLE_ERROR;
    if (($code & $fatalMask) === 0) {
        return;
    }
    wpmgr_mu_error_trap_record(
        $code,
        (string) ($err['message'] ?? ''),
        (string) ($err['file'] ?? ''),
        (int) ($err['line'] ?? 0)
    );
}

// Register both handlers immediately. Operating at the mu-plugin layer means
// these are armed before ANY normal plugin runs, so a fatal in another
// plugin's bootstrap is captured into our queue.
set_error_handler('wpmgr_mu_error_trap_handle', E_WARNING | E_NOTICE | E_USER_WARNING | E_USER_NOTICE | E_DEPRECATED | E_USER_DEPRECATED);
register_shutdown_function('wpmgr_mu_error_trap_shutdown');
