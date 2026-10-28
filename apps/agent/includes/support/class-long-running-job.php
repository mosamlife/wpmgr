<?php
/**
 * LongRunningJob — the shared bounded time limit for the agent's
 * long-running background loops (backup/restore/dump/media/db-clean/
 * diagnostics routines), and the guarded calls the backup and restore code
 * makes to functions a host may disable.
 *
 * S4 (issue #131 adversarial review, see UpdateCommand::APPLY_TIME_LIMIT_SECONDS)
 * established why these loops must call `set_time_limit(N)` with a bounded,
 * generous N rather than `set_time_limit(0)` (infinite): `0` throws away the
 * ONE timer whose fatal runs register_shutdown_function() callbacks
 * (max_execution_time), leaving only PHP-FPM's own request_terminate_timeout
 * (when configured) to kill a truly-hung request -- and FPM's SIGTERM does
 * NOT run shutdown functions at all, so a hang under `set_time_limit(0)` can
 * silently skip every crash-recovery/cleanup hook the agent relies on.
 *
 * 900 seconds (15 minutes) is generous for even a very large backup/restore/
 * dump/media pass, while still giving a truly-hung job a chance to hit PHP's
 * own RECOVERABLE fatal before something else (a shorter FPM timeout, if
 * configured) tears the process down with no recovery at all.
 *
 * Hosts may list set_time_limit, ignore_user_abort, disk_free_space or
 * disk_total_space in disable_functions. On PHP 8 such a function is
 * undefined, and calling it throws an Error that `@` does not silence, so the
 * backup and restore code calls them only through the methods below, which
 * check function_exists() first.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * Holds the single shared bounded time-limit constant for long-running jobs,
 * and the guarded host calls they make.
 */
final class LongRunningJob
{
    /**
     * Bounded (not infinite) execution-time cap, in seconds, for a
     * long-running backup/restore/dump/media/db-clean/diagnostics loop.
     * Mirrors UpdateCommand::APPLY_TIME_LIMIT_SECONDS (also 900) -- kept as a
     * separate constant rather than importing UpdateCommand into every one of
     * these unrelated classes, since UpdateCommand is a REST command handler,
     * not a shared utility.
     */
    public const TIME_LIMIT_SECONDS = 900;

    /**
     * Restart this worker's time limit at TIME_LIMIT_SECONDS and keep it
     * running if the client disconnects. A host that disables either
     * function keeps its own limit; the watchdog resumes a worker that limit
     * ends.
     */
    public static function extendLimits(): void
    {
        if (function_exists('set_time_limit')) {
            @set_time_limit(self::TIME_LIMIT_SECONDS); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged, Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running backup/restore loop must not hit max_execution_time; bounded, never 0
        }
        if (function_exists('ignore_user_abort')) {
            @ignore_user_abort(true); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- a background worker must outlive the request that started it
        }
    }

    /**
     * Free bytes on the filesystem holding $path, or null when the host
     * disables the probe or it fails.
     */
    public static function diskFreeSpace(string $path): ?float
    {
        if ($path === '' || !function_exists('disk_free_space')) {
            return null;
        }
        $bytes = @disk_free_space($path); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- host probe; a failure is reported as null
        return is_float($bytes) || is_int($bytes) ? (float) $bytes : null;
    }

    /**
     * Total bytes of the filesystem holding $path, or null when the host
     * disables the probe or it fails.
     */
    public static function diskTotalSpace(string $path): ?float
    {
        if ($path === '' || !function_exists('disk_total_space')) {
            return null;
        }
        $bytes = @disk_total_space($path); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- host probe; a failure is reported as null
        return is_float($bytes) || is_int($bytes) ? (float) $bytes : null;
    }
}
