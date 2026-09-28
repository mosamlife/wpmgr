<?php
/**
 * DebugLog: a single diagnostic channel that only writes when debugging is
 * explicitly enabled, so production installs stay quiet.
 *
 * It replaces scattered error_log() calls throughout the agent. Output is
 * emitted only when WordPress debug logging is on (WP_DEBUG && WP_DEBUG_LOG)
 * or the agent's own WPMGR_DEBUG constant is truthy; otherwise the call is a
 * no-op. Callers pass a plain message string (the agent's existing call sites
 * already build a "WPMgr <Component>: <detail>" string).
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/**
 * Gated diagnostic logger.
 */
final class DebugLog
{
    /**
     * Whether this channel would write at all: WordPress debug logging is on
     * (WP_DEBUG && WP_DEBUG_LOG) or the agent's own WPMGR_DEBUG constant is
     * truthy. Callers with an expensive message to build — formatting,
     * escaping — check this first, so that work only ever happens when the
     * result would actually be written.
     *
     * @return bool
     */
    public static function isEnabled(): bool
    {
        $wpDebug = defined('WP_DEBUG') && WP_DEBUG
            && defined('WP_DEBUG_LOG') && WP_DEBUG_LOG;
        $agentDebug = defined('WPMGR_DEBUG') && WPMGR_DEBUG;

        return $wpDebug || $agentDebug;
    }

    /**
     * Write a diagnostic line, but only when debugging is enabled.
     *
     * @param string $message Pre-formatted diagnostic message.
     * @return void
     */
    public static function write(string $message): void
    {
        if (!self::isEnabled()) {
            return;
        }

        // phpcs:ignore WordPress.PHP.DevelopmentFunctions.error_log_error_log -- single debug-gated diagnostic channel; only writes under WP_DEBUG_LOG or WPMGR_DEBUG
        error_log($message);
    }
}
