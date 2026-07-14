<?php
/**
 * LongRunningJob — the shared bounded time limit for the agent's
 * long-running background loops (backup/restore/dump/media/db-clean/
 * diagnostics routines).
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
 * Every call site keeps its existing `@`-guard and in-method placement
 * (this constant only replaces the literal `0` argument) -- `set_time_limit()`
 * is a no-op under most CLI SAPIs and when disabled via `disable_functions`,
 * so the `@` suppression remains load-bearing.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * Holds the single shared bounded time-limit constant for long-running jobs.
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
}
