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
 * Ownership: armShutdownGuard() returns the fresh flag it found in place, if
 * any, which belongs to another updater. The shutdown backstop, and a clear()
 * handed that value, leave such a flag alone for as long as its bytes and
 * modification time are unchanged, so a request that never put the site into
 * maintenance mode never takes it out of another updater's. A flag that has
 * been rewritten since is treated as this request's own and cleared. A rewrite
 * with identical bytes in the same second cannot be told apart and is left in
 * place; WordPress stops honouring a flag after ten minutes, and the next
 * managed update or rollback heals it once it is stale.
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
     * When $foreign is the value armShutdownGuard() returned and the flag is
     * still exactly that one, nothing is removed and the upgrader is not
     * asked to leave maintenance mode: the flag belongs to another updater.
     *
     * @param object|null                      $upgrader An upgrader instance exposing
     *                                                   maintenance_mode(bool), when available.
     * @param array{mtime:int,hash:string}|null $foreign  The fresh flag found when arming, or null.
     * @return void
     */
    public static function clear(?object $upgrader = null, ?array $foreign = null): void
    {
        if ($foreign !== null && self::isUnchanged($foreign)) {
            DebugLog::write(
                'WPMgr Agent: left a fresh .maintenance flag in place; it was set before this request armed and is unchanged.'
            );
            return;
        }

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
     * Records the fresh flag already in place, if any. The registered callback
     * leaves that flag alone while it is unchanged (see the class doc), and
     * the caller passes the returned value to its own clear() for the same
     * reason. Arm once per request, right before the work that can set the
     * flag: a later arm would record this request's own flag as another's.
     *
     * @return array{mtime:int,hash:string}|null The fresh flag found in place, or null.
     */
    public static function armShutdownGuard(): ?array
    {
        $foreign = self::freshFlag();
        register_shutdown_function(static function () use ($foreign): void {
            self::clear(null, $foreign);
        });

        return $foreign;
    }

    /**
     * The flag in place now, when it is younger than STALE_AFTER_SECONDS.
     *
     * @return array{mtime:int,hash:string}|null
     */
    private static function freshFlag(): ?array
    {
        $file = self::path();
        if ($file === '') {
            return null;
        }

        $seen = self::fingerprint($file);
        if ($seen === null || time() - $seen['mtime'] >= self::STALE_AFTER_SECONDS) {
            return null;
        }

        return $seen;
    }

    /**
     * Whether the flag in place is still exactly the one recorded.
     *
     * @param array{mtime:int,hash:string} $recorded A value freshFlag() returned.
     * @return bool
     */
    private static function isUnchanged(array $recorded): bool
    {
        $file = self::path();
        if ($file === '') {
            return false;
        }

        $seen = self::fingerprint($file);

        return $seen !== null
            && $seen['mtime'] === $recorded['mtime']
            && hash_equals($recorded['hash'], $seen['hash']);
    }

    /**
     * Modification time and content hash of a file, or null when it cannot be read.
     *
     * @param string $file Absolute path (already resolved via self::path()).
     * @return array{mtime:int,hash:string}|null
     */
    private static function fingerprint(string $file): ?array
    {
        clearstatcache(true, $file);
        if (!file_exists($file)) {
            return null;
        }

        $mtime = @filemtime($file);
        $hash  = @hash_file('sha256', $file);
        if ($mtime === false || !is_string($hash)) {
            return null;
        }

        return ['mtime' => $mtime, 'hash' => $hash];
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
