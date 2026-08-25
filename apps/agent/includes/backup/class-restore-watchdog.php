<?php
/**
 * RestoreWatchdog — recover stalled `wpmgr_restore_tasks` rows.
 *
 * M5.6 / ADR-034. Clone of `Watchdog` (the backup-side equivalent) but keyed
 * by (snapshot_id, restore_id) instead of (snapshot_id) — `restore_id` is the
 * uniqueness because the same snapshot may be restored multiple times.
 *
 * Two cron actions are bound to this class via Plugin::registerHooks:
 *
 *   - `wpmgr_restore_run`        -> dispatch()  — UNCONDITIONAL first-run
 *                                    entry from RestoreCommand. Always
 *                                    invokes RestoreRunner.
 *   - `wpmgr_restore_watchdog`   -> run()       — stall-detection re-entry.
 *                                    Reschedules itself if the task is alive;
 *                                    bumps resume_count + re-enters
 *                                    RestoreRunner if stalled.
 *
 * See the backup-side `Watchdog` class docblock for the full rationale of
 * this pattern (FPM worker decoupling, openresty 60 s upstream-timeout
 * mitigation, etc.). This class mirrors that file 1:1 for restore.
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

use WPMgr\Agent\Schema;
use WPMgr\Agent\Support\LongRunningJob;

final class RestoreWatchdog
{
    /** Stall-detection cron hook. */
    public const HOOK = 'wpmgr_restore_watchdog';

    /** First-run dispatch hook (bound separately by Plugin). */
    public const HOOK_RUN = 'wpmgr_restore_run';

    /**
     * Stall threshold. Same semantics + same value as the backup watchdog.
     */
    public const STALL_THRESHOLD_SECONDS = 180;

    /**
     * Reschedule cadence for the watchdog itself.
     */
    public const RESCHEDULE_SECONDS = 120;

    /**
     * Stall-detection cron callback. Invoked by WP-Cron with
     * [snapshot_id, restore_id] passed via wp_schedule_single_event $args.
     *
     * @param string $snapshotId UUID of the snapshot being restored.
     * @param string $restoreId  UUID of the restore run.
     * @return void
     */
    public static function run(string $snapshotId, string $restoreId): void
    {
        if (!self::validIds($snapshotId, $restoreId)) {
            return;
        }
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        /** @var \wpdb $wpdb */
        $table = $wpdb->prefix . Schema::BACKUP_RESTORE_TASKS_TABLE;
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct query on plugin-owned table; no core $wpdb helper exists; correctness requires a live read (anti-replay/locking)
        $row = $wpdb->get_row($wpdb->prepare(
            "SELECT * FROM {$table} WHERE snapshot_id = %s AND restore_id = %s", // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- interpolated identifier is prefix+constant (trusted); values bound via placeholders
            $snapshotId,
            $restoreId
        ), ARRAY_A);
        if (!is_array($row)) {
            return;
        }

        $phase = (string) ($row['phase'] ?? '');
        if ($phase === RestoreRunner::PHASE_COMPLETED || $phase === RestoreRunner::PHASE_FAILED) {
            return;
        }

        $lastProgress = (int) ($row['last_progress_at'] ?? 0);
        $stalledFor   = time() - $lastProgress;

        if ($stalledFor < self::STALL_THRESHOLD_SECONDS) {
            // Runner alive — re-arm the watchdog and bail.
            self::schedule($snapshotId, $restoreId, self::RESCHEDULE_SECONDS);
            return;
        }

        $resumeCount = (int) ($row['resume_count'] ?? 0);
        $maxResumes  = (int) ($row['max_resumes'] ?? 6);
        if ($resumeCount >= $maxResumes) {
            // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct query on plugin-owned table; no core $wpdb helper exists; correctness requires a live read (anti-replay/locking)
            @$wpdb->update(
                $table,
                ['phase' => RestoreRunner::PHASE_FAILED, 'last_progress_at' => time()],
                ['snapshot_id' => $snapshotId, 'restore_id' => $restoreId],
                ['%s', '%d'],
                ['%s', '%s']
            );
            \WPMgr\Agent\Support\DebugLog::write(sprintf(
                'WPMgr Restore: %s/%s exhausted %d resume attempts; marked failed',
                $snapshotId,
                $restoreId,
                $maxResumes
            ));
            return;
        }

        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct query on plugin-owned table; no core $wpdb helper exists; correctness requires a live read (anti-replay/locking)
        @$wpdb->update(
            $table,
            ['resume_count' => $resumeCount + 1, 'last_progress_at' => time()],
            ['snapshot_id' => $snapshotId, 'restore_id' => $restoreId],
            ['%d', '%d'],
            ['%s', '%s']
        );
        \WPMgr\Agent\Support\DebugLog::write(sprintf(
            'WPMgr Restore: watchdog resuming %s/%s (attempt %d/%d) - stalled %ds in phase=%s',
            $snapshotId,
            $restoreId,
            $resumeCount + 1,
            $maxResumes,
            $stalledFor,
            $phase
        ));

        $subState = self::decodeSubState($row['sub_state'] ?? '');
        $params   = is_array($subState['params'] ?? null) ? $subState['params'] : null;
        if (!is_array($params)) {
            \WPMgr\Agent\Support\DebugLog::write(sprintf('WPMgr Restore: watchdog cannot resume %s/%s - sub_state.params missing', $snapshotId, $restoreId));
            return;
        }

        // Keep the watchdog armed until the row reaches a terminal phase.
        self::schedule($snapshotId, $restoreId, self::RESCHEDULE_SECONDS);

        $runner = new RestoreRunner($params);
        $runner->run();
    }

    /**
     * UNCONDITIONAL first-run dispatch. Bound to `wpmgr_restore_run`. Mirrors
     * Watchdog::dispatch on the backup side: doesn't gate on stall state,
     * always invokes the runner from sub_state.params.
     *
     * @param string $snapshotId UUID.
     * @param string $restoreId  UUID.
     * @return void
     */
    public static function dispatch(string $snapshotId, string $restoreId): void
    {
        if (!self::validIds($snapshotId, $restoreId)) {
            return;
        }
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        /** @var \wpdb $wpdb */
        $table = $wpdb->prefix . Schema::BACKUP_RESTORE_TASKS_TABLE;
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct query on plugin-owned table; no core $wpdb helper exists; correctness requires a live read (anti-replay/locking)
        $row = $wpdb->get_row($wpdb->prepare(
            "SELECT * FROM {$table} WHERE snapshot_id = %s AND restore_id = %s", // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- interpolated identifier is prefix+constant (trusted); values bound via placeholders
            $snapshotId,
            $restoreId
        ), ARRAY_A);
        if (!is_array($row)) {
            \WPMgr\Agent\Support\DebugLog::write(sprintf('WPMgr Restore: dispatch cannot find task row for %s/%s', $snapshotId, $restoreId));
            return;
        }
        $phase = (string) ($row['phase'] ?? '');
        if ($phase === RestoreRunner::PHASE_COMPLETED || $phase === RestoreRunner::PHASE_FAILED) {
            return;
        }
        $subState = self::decodeSubState($row['sub_state'] ?? '');
        $params   = is_array($subState['params'] ?? null) ? $subState['params'] : null;
        if (!is_array($params)) {
            \WPMgr\Agent\Support\DebugLog::write(sprintf('WPMgr Restore: dispatch cannot extract params for %s/%s', $snapshotId, $restoreId));
            return;
        }
        @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running backup/restore loop must not hit max_execution_time; @-guarded
        @ignore_user_abort(true);
        try {
            (new RestoreRunner($params))->run();
        } catch (\Throwable $e) {
            \WPMgr\Agent\Support\DebugLog::write('WPMgr Restore: dispatch runner fatal: ' . $e->getMessage());
        }
    }

    /**
     * Schedule (or re-schedule) the watchdog for a restore.
     */
    public static function schedule(string $snapshotId, string $restoreId, int $delay = self::RESCHEDULE_SECONDS): void
    {
        if (!function_exists('wp_schedule_single_event')) {
            return;
        }
        if (!self::validIds($snapshotId, $restoreId)) {
            return;
        }
        wp_schedule_single_event(time() + max(1, $delay), self::HOOK, [$snapshotId, $restoreId]);
    }

    /**
     * Validate the snapshot/restore ID pair shape (UUID-ish).
     */
    private static function validIds(string $snapshotId, string $restoreId): bool
    {
        if ($snapshotId === '' || !preg_match('/^[a-f0-9-]{36}$/i', $snapshotId)) {
            return false;
        }
        if ($restoreId === '' || !preg_match('/^[a-f0-9-]{36}$/i', $restoreId)) {
            return false;
        }
        return true;
    }

    /**
     * Best-effort JSON decode. Returns [] on any failure.
     *
     * @param mixed $raw
     * @return array<string,mixed>
     */
    private static function decodeSubState($raw): array
    {
        if (!is_string($raw) || $raw === '') {
            return [];
        }
        $decoded = json_decode($raw, true);
        return is_array($decoded) ? $decoded : [];
    }
}
