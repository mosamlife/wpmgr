<?php
/**
 * Maintenance: guarantees WordPress core's `<ABSPATH>/.maintenance` drop-file
 * never survives an agent-driven update or rollback, on any terminal path.
 *
 * WordPress core's WP_Upgrader::maintenance_mode(true) (and, for core updates,
 * update_core() in wp-admin/includes/update-core.php) writes `.maintenance` at
 * the start of an upgrade and deletes it at the end — but core has several
 * early-return paths between those two points that never reach the delete.
 * When an update/rollback the agent triggers throws, times out, or is
 * interrupted along one of those paths, `.maintenance` is orphaned and the
 * ENTIRE site serves HTTP 503 to every visitor until someone deletes it by
 * hand.
 *
 * This class is the single seam both UpdateRunner (which owns the actual
 * upgrader calls) and the update/rollback commands (which own the overall
 * request lifecycle) use to guarantee cleanup:
 *   - clear() is called from a try/finally so success, failure, a thrown
 *     exception, or an early return all reach it.
 *   - armShutdownGuard() registers a belt-and-suspenders shutdown callback so
 *     a PHP fatal error or a max_execution_time timeout — neither of which
 *     unwinds try/finally — still clears the flag once the request tears down.
 *   - healStaleIfPresent() proactively heals a `.maintenance` left behind by a
 *     PRIOR failed run the next time a managed update/rollback starts, without
 *     touching a fresh flag another in-flight process may legitimately own.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * Clears and heals the WordPress core maintenance-mode drop-file.
 */
final class Maintenance
{
    /**
     * A `.maintenance` file older than this is treated as orphaned rather
     * than possibly owned by another in-flight update/rollback. WordPress
     * upgrades of the sizes the agent handles complete in well under a
     * minute; this stays comfortably above that while still healing quickly.
     */
    private const STALE_AFTER_SECONDS = 90;

    /**
     * Resolve the absolute path to WordPress's maintenance-mode drop-file.
     *
     * @return string Absolute path, or '' when ABSPATH is unavailable.
     */
    public static function path(): string
    {
        if (!defined('ABSPATH')) {
            return '';
        }

        $abspath = rtrim((string) ABSPATH, '/\\');
        if ($abspath === '') {
            return '';
        }

        return $abspath . '/.maintenance';
    }

    /**
     * Heal a STALE `.maintenance` left behind by a prior interrupted run,
     * before starting a new managed update/rollback. A flag younger than
     * STALE_AFTER_SECONDS is left alone — it may belong to another in-flight
     * upgrade (ours or an operator's) that legitimately owns it right now.
     *
     * @return void
     */
    public static function healStaleIfPresent(): void
    {
        $file = self::path();
        if ($file === '' || !file_exists($file)) {
            return;
        }

        $mtime = @filemtime($file);
        if ($mtime === false) {
            return;
        }

        $age = time() - $mtime;
        if ($age < self::STALE_AFTER_SECONDS) {
            return;
        }

        DebugLog::write(
            'WPMgr Agent: healing stale .maintenance flag (' . $age
            . 's old) before starting a managed update/rollback.'
        );

        self::removeFile($file);
    }

    /**
     * Clear maintenance mode. Intended to be called from a `finally` block
     * wrapping every update/rollback code path, regardless of outcome.
     *
     * Prefers the WP-native upgrader call when an upgrader instance is
     * available (it also drives the upgrader skin's feedback hooks), then
     * unconditionally verifies the file itself is gone — the upgrader call
     * relies on a WP_Filesystem connection that may be uninitialized or may
     * itself have failed, so it is not trusted on its own.
     *
     * @param object|null $upgrader An upgrader instance exposing
     *                              maintenance_mode(bool), when available.
     * @return void
     */
    public static function clear(?object $upgrader = null): void
    {
        if ($upgrader !== null && method_exists($upgrader, 'maintenance_mode')) {
            try {
                $upgrader->maintenance_mode(false);
            } catch (\Throwable $e) {
                DebugLog::write(
                    'WPMgr Agent: upgrader maintenance_mode(false) threw: ' . $e->getMessage()
                );
            }
        }

        $file = self::path();
        if ($file !== '' && file_exists($file)) {
            self::removeFile($file);
        }
    }

    /**
     * Register a shutdown-time backstop that clears maintenance mode even if
     * a fatal error or a request timeout skips every try/finally (neither
     * unwinds the call stack the way a thrown Throwable does). Safe to call
     * more than once per request — PHP tolerates duplicate shutdown callbacks
     * and a redundant clear() is a cheap, idempotent no-op when there is
     * nothing left to remove.
     *
     * @return void
     */
    public static function armShutdownGuard(): void
    {
        register_shutdown_function(static function (): void {
            self::clear();
        });
    }

    /**
     * Remove the maintenance file, preferring the WP-native helper.
     *
     * @param string $file Absolute path (already resolved via self::path()).
     * @return void
     */
    private static function removeFile(string $file): void
    {
        if (function_exists('wp_delete_file')) {
            wp_delete_file($file);
        } else {
            // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- wp_delete_file() is not yet loaded this early in some contexts; best-effort removal of WP core's own maintenance drop-file (not a plugin-owned file), never fatal on a permissions error
            @unlink($file);
        }

        if (file_exists($file)) {
            DebugLog::write('WPMgr Agent: failed to remove .maintenance flag at ' . $file . '.');
        }
    }
}
