<?php
/**
 * Watchdog — recover stalled `wpmgr_backup_tasks` rows.
 *
 * M5.6 / ADR-033. Bound to the `wpmgr_backup_watchdog` action via
 * `wp_schedule_single_event(time()+120, 'wpmgr_backup_watchdog',
 * [$snapshot_id])`, scheduled by `BackupCommand::execute` when a backup
 * begins. The watchdog inspects the task row:
 *
 *   - If terminal (`completed` / `failed`), do nothing.
 *   - If active but `last_progress_at < now() - 180s`, the runner has
 *     stalled (PHP process killed, FPM worker recycled, hosting
 *     restart…). Re-enter `TaskRunner::run()` from the persisted
 *     `sub_state`. Only a resume whose previous worker saved no progress
 *     counts toward `max_resumes` (GH #983).
 *   - A task is given up on age only without progress: past 2h it runs on
 *     while its durable checkpoint advanced within the last hour or a live
 *     runner holds its run-lock, and 24h ends it regardless. Every give-up
 *     is reported to the control plane.
 *   - If active but still posting progress, reschedule the watchdog
 *     itself for another +120s. Belt-and-suspenders: even if the
 *     runner never crashes, the watchdog stays alive long enough to
 *     observe the eventual `completed`/`failed` transition.
 *
 * The watchdog runs under WP-Cron, which is fired by the FIRST visitor
 * that lands on the site after `time()` passes the scheduled timestamp.
 * Many managed hosts also have a system cron + `wp cron event run` for
 * reliability — when that's the case, the watchdog fires on schedule
 * even with zero traffic.
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

use WPMgr\Agent\Schema;
use WPMgr\Agent\Support\DebugLog;
use WPMgr\Agent\Support\LongRunningJob;

final class Watchdog
{
    /** Cron hook name — bind via `add_action(Watchdog::HOOK, …)`. */
    public const HOOK = 'wpmgr_backup_watchdog';

    /**
     * Stall threshold. If `last_progress_at` is older than this many
     * seconds AND the phase is non-terminal, the runner is presumed dead
     * and we re-enter from `sub_state`. Larger than the TaskRunner's
     * 5 s progress-throttle, smaller than the CP-side
     * `ProgressWatchdogWorker` 120 s threshold (so we get a chance to
     * recover before the CP marks the snapshot failed).
     *
     * @var int
     */
    public const STALL_THRESHOLD_SECONDS = 180;

    /**
     * Reschedule cadence — if the task is healthy (recent progress and
     * non-terminal), we re-arm the watchdog for another window. Matches
     * the initial schedule (+120 s) so the cadence is steady.
     */
    public const RESCHEDULE_SECONDS = 120;

    /** GH #983: a task this old is given up whatever its progress (zombie bound). */
    public const HARD_CEILING_SECONDS = 86400;

    /** GH #983: past this age a task runs on only while its progress is recent. */
    public const AGE_GUARD_SECONDS = 7200;

    /** GH #983: how recently the durable progress record must have advanced. */
    public const PROGRESS_WINDOW_SECONDS = 3600;

    /** Silence after which a late-phase task with no live runner is given up. */
    public const LATE_PHASE_STALL_SECONDS = 300;

    /** Reasons shown on the dashboard when the watchdog gives a backup up. */
    private const HARD_CEILING_REASON = 'Backup stopped: it was still running after 24 hours. Run it again; if backups keep taking this long, exclude large folders or file types from them.';

    private const NO_PROGRESS_REASON = 'Backup stopped: it saved no progress for over an hour. Check the site\'s free disk space and PHP error log, then run the backup again.';

    private const LATE_PHASE_REASON = 'Backup stopped: its upload stalled and the worker running it ended. Check that the site can reach the control plane, then run the backup again.';

    private const RESUME_CAP_REASON = 'Backup stopped: the host ended the backup worker %d times before it saved any progress. Raise the PHP time or memory limit, or exclude large files, then run the backup again.';

    /**
     * Cron callback. Invoked by WP-Cron with the snapshot_id passed via
     * the wp_schedule_single_event $args array. Returns `void` because
     * WP-Cron callbacks MUST NOT return a value (return-value handling is
     * undefined across WP-Cron implementations).
     *
     * GitHub issue #232: run() is now a thin wrapper around
     * resumeIfStalled() — the cron path is unchanged (no #131 regression),
     * but the resume logic is now ALSO reachable from sweepStalled(), the
     * WP-Cron-INDEPENDENT reaper bound to plugins_loaded via
     * Plugin::maybeSweepStalledBackups().
     *
     * @param string $snapshotId UUID of the snapshot to inspect.
     * @return void
     */
    public static function run(string $snapshotId): void
    {
        self::resumeIfStalled($snapshotId);
    }

    /**
     * Resume a stalled backup task, or reschedule the watchdog if it's still
     * healthy. Reusable entry point (GitHub issue #232): called by run() (the
     * `wpmgr_backup_watchdog` WP-Cron callback) AND by sweepStalled() (the
     * WP-Cron-INDEPENDENT reaper driven from ordinary request traffic).
     *
     *   - If terminal (`completed` / `failed`), do nothing (best-effort
     *     defensive DELETE).
     *   - If active but `last_progress_at < now() - STALL_THRESHOLD_SECONDS`
     *     and no live runner holds the run-lock, the runner has stalled (PHP
     *     process killed, FPM worker recycled, hosting restart…). Re-enter
     *     `TaskRunner::run()` from the persisted `sub_state`, counting the
     *     resume toward `max_resumes` only when the worker before it saved
     *     no progress (GH #983).
     *   - If active but still posting progress, reschedule the watchdog
     *     itself for another +120s. Belt-and-suspenders: even if the
     *     runner never crashes, the watchdog stays alive long enough to
     *     observe the eventual `completed`/`failed` transition.
     *
     * The give-up guards are listed on resume(). Each one reports its
     * reason to the control plane (GH #983).
     *
     * @param string $snapshotId UUID of the snapshot to inspect.
     * @return void
     */
    public static function resumeIfStalled(string $snapshotId): void
    {
        self::resume($snapshotId, false);
    }

    /**
     * GH #983: continue a task whose worker stopped itself after a saved
     * archive part, because it had used most of its time limit. Called by
     * TaskRunner::run() once it has released its guards. This is the same
     * resume path a stall takes, minus the wait for the stall: every give-up
     * guard still applies, and a hand-over never counts toward max_resumes
     * because its worker saved progress.
     *
     * @param string $snapshotId UUID of the snapshot to continue.
     * @return string|null Phase the last worker that ran reached, or null when none ran.
     */
    public static function handOver(string $snapshotId): ?string
    {
        return self::resume($snapshotId, true);
    }

    /**
     * The shared resume path behind resumeIfStalled() and handOver(): runs
     * the worker the guards of runnerToResume() allow and, for as long as a
     * worker hands over, the next one, all from this loop (GH #954). A
     * worker's frames end before the next worker's guards decode the task row
     * again, so a backup that needs many hand-overs holds one worker's state
     * at a time, not one more copy of its cursor for every hand-over.
     *
     * @param string $snapshotId UUID of the snapshot.
     * @param bool   $handedOver True when the previous worker handed over.
     * @return string|null Phase the last worker that ran reached, or null when none ran.
     */
    private static function resume(string $snapshotId, bool $handedOver): ?string
    {
        $phase = null;
        while (true) {
            $runner = self::runnerToResume($snapshotId, $handedOver);
            if ($runner === null) {
                return $phase;
            }
            // runWorker() catches all exceptions and never throws, so this
            // call cannot kill the WP-Cron worker.
            $phase = $runner->runWorker();
            if (!$runner->handedOver()) {
                return $phase;
            }
            // The worker saved a part and released its guards: the next one
            // continues at once, and this one is released before its state is
            // decoded.
            $handedOver = true;
            $runner     = null;
        }
    }

    /**
     * The runner a resume starts, or null when none should run: the task is
     * gone, finished, healthy, held by a live worker, or given up. Everything
     * decoded here for the guards is gone when this returns, before any worker
     * runs.
     *
     * Give-up guards, in order (each reclaims scratch under the run-lock,
     * deletes the row, and reports the reason to the control plane; every
     * guard but the 24h ceiling does so only while it holds the run-lock and
     * the task re-read under that lock is still due, and otherwise waits for
     * the next pass):
     *   1. Age: past HARD_CEILING_SECONDS whatever the progress; past
     *      AGE_GUARD_SECONDS only when the durable progress record has not
     *      advanced within PROGRESS_WINDOW_SECONDS and no live runner holds
     *      the run-lock.
     *   2. Late phase: encrypting_uploading / submitting_manifest silent for
     *      LATE_PHASE_STALL_SECONDS while no live runner holds the run-lock.
     *   3. Resume cap: a resume whose previous worker saved no progress
     *      counts toward max_resumes; at the cap the task is given up.
     *
     * @param string $snapshotId UUID of the snapshot.
     * @param bool   $handedOver True when the previous worker handed over.
     * @return TaskRunner|null The runner to start, or null when none should run.
     */
    private static function runnerToResume(string $snapshotId, bool $handedOver): ?TaskRunner
    {
        if ($snapshotId === '' || !preg_match('/^[a-f0-9-]{36}$/i', $snapshotId)) {
            return null;
        }

        global $wpdb;
        if (!is_object($wpdb)) {
            return null;
        }
        /** @var \wpdb $wpdb */

        $table = $wpdb->prefix . Schema::BACKUP_TASKS_TABLE;
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching,WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- direct query on plugin-owned table; identifier is prefix+constant; no caching on live task-state read
        $row = $wpdb->get_row($wpdb->prepare("SELECT * FROM {$table} WHERE snapshot_id = %s", $snapshotId), ARRAY_A);
        if (!is_array($row)) {
            return null; // Task was already cleaned up (a completed runner deletes its row).
        }

        $phase = (string) ($row['phase'] ?? '');
        if ($phase === TaskRunner::PHASE_COMPLETED || $phase === TaskRunner::PHASE_FAILED) {
            // Terminal. Best-effort DELETE the row so a future stale
            // wpmgr_backup_watchdog event can't even find it. (Defensive
            // cleanup — the TaskRunner success path now also DELETEs.)
            @$wpdb->delete($table, ['snapshot_id' => $snapshotId], ['%s']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct delete on plugin-owned table; correctness requires a live write
            return null;
        }

        $subState = self::decodeSubState($row['sub_state'] ?? '');

        // SAFETY 1 (GH #983): give up on age only without progress. A row
        // "running" for a long time used to be deleted at 2h outright, even
        // while a worker was alive and closing parts, so no large site could
        // finish. Now a task past AGE_GUARD_SECONDS continues while its
        // durable progress record keeps advancing, and only
        // HARD_CEILING_SECONDS ends a task whatever it does, so a zombie row
        // (or one whose backup completed on the CP while its local row
        // survived a killed runner) still ends. Below that ceiling a live
        // run-lock holder always wins over the age rule.
        $ageReason = self::ageGiveUpReasonUnlessLive($row, $subState);
        if ($ageReason !== null) {
            $startedAt = (int) ($row['started_at'] ?? 0);
            self::giveUp($row, $subState, sprintf(
                'WPMgr Backup: watchdog refusing to re-enter stale task for snapshot %s (started %ds ago, phase=%s); deleted row without re-entry.',
                $snapshotId, time() - $startedAt, $phase
            ), $ageReason, self::ageRecheck($ageReason));
            return null;
        }

        // SAFETY 2 (Bug 2 fix): refuse to re-enter a task whose phase has
        // been stuck at `encrypting_uploading` or `submitting_manifest` for
        // more than LATE_PHASE_STALL_SECONDS. These are the LATE phases of
        // the pipeline; if we got that far the artifacts are already
        // uploaded and the manifest is either submitted or about to be.
        // Re-entering would re-issue presignChunks calls the CP would
        // 422-reject (the observed bug).
        //
        // GH #256 finding 4: the GH #279 heartbeat is emitted BETWEEN
        // chunks, so a single large chunk upload on a slow link can exceed
        // this silence while the runner is alive. GH #983: a give-up now
        // stops a live runner at its next save, so a runner that still
        // holds the run-lock is left to finish instead.
        $lastProgress = (int) ($row['last_progress_at'] ?? 0);
        $stalledFor   = time() - $lastProgress;
        if (
            ($phase === TaskRunner::PHASE_ENCRYPTING_UPLOADING
                || $phase === TaskRunner::PHASE_SUBMITTING_MANIFEST)
            && $stalledFor > self::LATE_PHASE_STALL_SECONDS
        ) {
            if (self::runLockHeld($subState)) {
                self::schedule($snapshotId, self::RESCHEDULE_SECONDS);
                return null;
            }
            self::giveUp($row, $subState, sprintf(
                'WPMgr Backup: watchdog refusing to re-enter late-phase stalled task for snapshot %s (phase=%s, stalled %ds); deleted row.',
                $snapshotId, $phase, $stalledFor
            ), self::LATE_PHASE_REASON, static function (array $fresh): ?string {
                $freshPhase = (string) ($fresh['phase'] ?? '');
                $late = $freshPhase === TaskRunner::PHASE_ENCRYPTING_UPLOADING || $freshPhase === TaskRunner::PHASE_SUBMITTING_MANIFEST;
                return $late && time() - (int) ($fresh['last_progress_at'] ?? 0) > self::LATE_PHASE_STALL_SECONDS ? self::LATE_PHASE_REASON : null;
            });
            return null;
        }

        if (!$handedOver && $stalledFor < self::STALL_THRESHOLD_SECONDS) {
            // Runner is alive — reschedule the watchdog so it stays
            // armed across the rest of the run. No state change.
            self::schedule($snapshotId, self::RESCHEDULE_SECONDS);
            return null;
        }

        // A silent runner that still holds the run-lock is alive (a long
        // part close, say): resuming would only lose the lock, so it is
        // neither resumed nor counted.
        if (self::runLockHeld($subState)) {
            self::schedule($snapshotId, self::RESCHEDULE_SECONDS);
            return null;
        }

        // STALLED (or handed over). GH #983: only a resume whose previous
        // worker saved no progress counts toward max_resumes.
        $resumeCount = (int) ($row['resume_count'] ?? 0);
        $maxResumes  = (int) ($row['max_resumes'] ?? 6);
        $progressed  = self::workerSavedProgress($subState);
        if (!$progressed) {
            if ($resumeCount >= $maxResumes) {
                // Give up: too many workers ended without saving progress.
                //
                // If the task never left the `queued` phase, the runner was
                // never dispatched at all: strong signal that both the
                // spawn_cron loopback and the in-process fallback failed to
                // start the runner. Detect the loopback-gate scenario and
                // surface a clear, actionable reason.
                //
                // GH #256: the row is DELETEd rather than marked failed, like
                // every other give-up branch: a row left at phase=failed was
                // never revisited (sweepStalled() excludes terminal phases),
                // and a CP retry reusing this snapshot id would short-circuit
                // on it before cleanupOnFailed() could reclaim the scratch.
                $failureReason = $phase === TaskRunner::PHASE_QUEUED
                    ? self::detectQueuedStallReason($phase, $snapshotId)
                    : sprintf(self::RESUME_CAP_REASON, $maxResumes);
                self::giveUp($row, $subState, sprintf(
                    'WPMgr Backup: snapshot %s exhausted %d resume attempts without progress; reclaimed scratch and deleted row.',
                    $snapshotId, $maxResumes
                ), $failureReason, static function (array $fresh, array $freshSub) use ($handedOver, $failureReason): ?string {
                    $stalled = $handedOver || time() - (int) ($fresh['last_progress_at'] ?? 0) >= self::STALL_THRESHOLD_SECONDS;
                    $capped  = (int) ($fresh['resume_count'] ?? 0) >= (int) ($fresh['max_resumes'] ?? 6);
                    return $stalled && $capped && !self::workerSavedProgress($freshSub) ? $failureReason : null;
                });
                return null;
            }
            $resumeCount++;
        }

        @$wpdb->update($table, ['resume_count' => $resumeCount, 'last_progress_at' => time()], ['snapshot_id' => $snapshotId], ['%d', '%d'], ['%s']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct update on plugin-owned table; watchdog resume-count must be written live
        DebugLog::write(sprintf('WPMgr Backup: watchdog %s snapshot %s (%s; %d/%d resumes without progress) after %ds in phase=%s',
            $handedOver ? 'continuing handed-over' : 'resuming', $snapshotId,
            $progressed ? 'previous worker saved progress' : 'previous worker saved no progress',
            $resumeCount, $maxResumes, $stalledFor, $phase));

        // Reconstruct the runner from the persisted task row. params
        // sub_state lives in the row; the OTHER params (endpoints, db,
        // age recipient) live in sub_state.params (BackupCommand stuffed
        // them there on first run so the watchdog can rehydrate without
        // re-receiving them from the CP).
        $params = is_array($subState['params'] ?? null) ? $subState['params'] : null;
        if (!is_array($params)) {
            DebugLog::write(sprintf('WPMgr Backup: watchdog cannot resume %s — sub_state.params missing', $snapshotId));
            return null;
        }

        // Cap the next watchdog firing so this row keeps getting checked
        // until it terminates.
        self::schedule($snapshotId, self::RESCHEDULE_SECONDS);

        // The caller runs the runner once this call has returned, so the
        // decoded row and cursor above are gone before the worker starts. The
        // runner reads phase + sub_state itself and dispatches.
        return new TaskRunner($params);
    }

    /**
     * GH #983: why an old task must be given up, or null while it may run
     * on. Past HARD_CEILING_SECONDS always; past AGE_GUARD_SECONDS only when
     * the durable progress record is missing or last advanced more than
     * PROGRESS_WINDOW_SECONDS ago.
     *
     * @param array<string,mixed> $row      Task row.
     * @param array<string,mixed> $subState Decoded sub_state.
     */
    private static function ageGiveUpReason(array $row, array $subState): ?string
    {
        $startedAt = (int) ($row['started_at'] ?? 0);
        if ($startedAt <= 0) {
            return null;
        }
        $now = time();
        $age = $now - $startedAt;
        if ($age > self::HARD_CEILING_SECONDS) {
            return self::HARD_CEILING_REASON;
        }
        if ($age <= self::AGE_GUARD_SECONDS) {
            return null;
        }
        $progress = TaskRunner::progressRecord($subState['progress'] ?? null);
        if ($progress !== null && $now - $progress['at'] <= self::PROGRESS_WINDOW_SECONDS) {
            return null;
        }
        return self::NO_PROGRESS_REASON;
    }

    /**
     * GH #983: ageGiveUpReason(), except that below HARD_CEILING_SECONDS a
     * live runner holding the run-lock always wins. The progress record does
     * not move during one DB dump, one encrypt pass or the discovery walk,
     * so a quiet record never proves its runner dead; only the ceiling ends
     * a task whose runner is still alive.
     *
     * @param array<string,mixed> $row      Task row.
     * @param array<string,mixed> $subState Decoded sub_state.
     */
    private static function ageGiveUpReasonUnlessLive(array $row, array $subState): ?string
    {
        $reason = self::ageGiveUpReason($row, $subState);
        if ($reason === null || $reason === self::HARD_CEILING_REASON) {
            return $reason;
        }
        return self::runLockHeld($subState) ? null : $reason;
    }

    /**
     * The re-check of an age give-up under the run-lock: none for the 24h
     * ceiling, which ends a task whatever runs it; otherwise the age rule
     * applied again to the task re-read under the lock.
     *
     * @return (callable(array<string,mixed>, array<string,mixed>): ?string)|null
     */
    private static function ageRecheck(string $reason): ?callable
    {
        if ($reason === self::HARD_CEILING_REASON) {
            return null;
        }
        return static fn (array $fresh, array $freshSub): ?string => self::ageGiveUpReason($fresh, $freshSub);
    }

    /**
     * GH #983: whether the worker that last started saved progress before it
     * ended. A row without both records (an older agent's, or one whose
     * worker never started) has no such proof.
     *
     * @param array<string,mixed> $subState Decoded sub_state.
     */
    private static function workerSavedProgress(array $subState): bool
    {
        $progress = TaskRunner::progressRecord($subState['progress'] ?? null);
        $worker   = TaskRunner::workerRecord($subState['worker'] ?? null);
        return $progress !== null && $worker !== null && $progress['seq'] > $worker['seq'];
    }

    /**
     * Whether a live runner holds this task's run-lock. False when the row
     * carries no params to locate the lock.
     *
     * @param array<string,mixed> $subState Decoded sub_state.
     */
    private static function runLockHeld(array $subState): bool
    {
        $params = is_array($subState['params'] ?? null) ? $subState['params'] : null;
        if (!is_array($params) || !isset($params['scratch_dir']) || !is_string($params['scratch_dir']) || $params['scratch_dir'] === '') {
            return false;
        }
        try {
            return (new TaskRunner($params))->isRunLockHeld();
        } catch (\Throwable $_) {
            return false;
        }
    }

    /**
     * GH #983: give a task up. Reclaims its scratch under the run-lock (GH
     * #256), deletes the row, and reports $reason to the control plane
     * through the runner's own signed `/progress` failure channel, so the
     * backup fails on the dashboard with a reason the site owner can act
     * on.
     *
     * A give-up that spares a live worker passes $recheck: it then takes the
     * run-lock without waiting, re-reads the task under it, and reclaims the
     * scratch and deletes the row only while it holds the lock and $recheck
     * still returns a reason for the row it re-read. When another worker
     * holds the lock, or the lock cannot be taken, the row is left alone and
     * the decision waits for the next watchdog pass. Only the 24h ceiling
     * passes no $recheck: it deletes the row whatever runs it, and a worker
     * still running sees the row gone at its next save and stops without
     * reporting success.
     *
     * @param array<string,mixed>                                          $row      Task row already loaded.
     * @param array<string,mixed>                                          $subState Decoded sub_state.
     * @param string                                                       $logLine  Local log line.
     * @param string                                                       $reason   Reason shown to the site owner.
     * @param (callable(array<string,mixed>, array<string,mixed>): ?string)|null $recheck Given the task row re-read under the
     *                                                                               run-lock and its sub_state, the reason it is
     *                                                                               still to be given up for, or null.
     */
    private static function giveUp(array $row, array $subState, string $logLine, string $reason, ?callable $recheck = null): void
    {
        global $wpdb;
        $snapshotId = isset($row['snapshot_id']) && is_string($row['snapshot_id']) ? $row['snapshot_id'] : '';
        $params = is_array($subState['params'] ?? null) ? $subState['params'] : null;
        $runner = is_array($params) ? new TaskRunner($params) : null;
        if ($recheck !== null && $runner !== null && is_string($params['scratch_dir'] ?? null) && $params['scratch_dir'] !== '') {
            $given = self::giveUpUnderRunLock($snapshotId, $runner, $recheck);
            if ($given === null) {
                return;
            }
            $reason = $given;
        } else {
            // No lock to take (no params to locate it), or the 24h ceiling.
            self::reclaimRunScratch($row);
            if (is_object($wpdb) && $snapshotId !== '') {
                /** @var \wpdb $wpdb */
                @$wpdb->delete($wpdb->prefix . Schema::BACKUP_TASKS_TABLE, ['snapshot_id' => $snapshotId], ['%s']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct delete on plugin-owned table; terminal give-up, no further recovery is ever attempted for this row
            }
        }
        if ($runner !== null) {
            try {
                $runner->reportFailure((string) ($row['phase'] ?? ''), $reason);
            } catch (\Throwable $e) {
                DebugLog::write('WPMgr Backup: watchdog could not report the give-up: ' . $e->getMessage());
            }
        }
        DebugLog::write($logLine . ' ' . $reason);
    }

    /**
     * GH #983: the give-up of a task whose live worker must be spared. Takes
     * the run-lock without waiting; while holding it, re-reads the task row,
     * and only when that row is still active and $recheck still returns a
     * reason for it, reclaims the scratch and deletes the row. A worker that
     * holds the lock keeps its row: nothing is changed and the next watchdog
     * pass decides again.
     *
     * @param TaskRunner                                                 $runner  Runner built from the task row's params.
     * @param callable(array<string,mixed>, array<string,mixed>): ?string $recheck See giveUp().
     * @return string|null The reason the task was given up for, or null when it was not.
     */
    private static function giveUpUnderRunLock(string $snapshotId, TaskRunner $runner, callable $recheck): ?string
    {
        global $wpdb;
        if (!is_object($wpdb) || $snapshotId === '') {
            return null;
        }
        $given  = null;
        $locked = false;
        try {
            $locked = $runner->withRunLock(static function () use ($runner, $snapshotId, $recheck, &$given): void {
                global $wpdb;
                /** @var \wpdb $wpdb */
                $table = $wpdb->prefix . Schema::BACKUP_TASKS_TABLE;
                $fresh = $wpdb->get_row($wpdb->prepare('SELECT * FROM %i WHERE snapshot_id = %s', $table, $snapshotId), ARRAY_A); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- the live task row, read uncached under the run-lock the give-up decides with; table via the %i identifier placeholder (WP 6.2+)
                if (!is_array($fresh)) {
                    return; // Finished, or given up by another pass.
                }
                $phase = (string) ($fresh['phase'] ?? '');
                if ($phase === TaskRunner::PHASE_COMPLETED || $phase === TaskRunner::PHASE_FAILED) {
                    return;
                }
                $reason = $recheck($fresh, self::decodeSubState($fresh['sub_state'] ?? ''));
                if ($reason === null) {
                    DebugLog::write(sprintf('WPMgr Backup: watchdog kept snapshot %s: the task re-read under its run-lock is no longer due to be given up', $snapshotId));
                    return;
                }
                try {
                    $runner->reclaimScratch();
                } catch (\Throwable $e) {
                    // A reclaim failure never keeps a given-up row.
                    DebugLog::write('WPMgr Backup: watchdog scratch reclaim failed: ' . $e->getMessage());
                }
                @$wpdb->delete($table, ['snapshot_id' => $snapshotId], ['%s']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct delete on plugin-owned table, under the run-lock; terminal give-up
                $given = $reason;
            });
        } catch (\Throwable $e) {
            DebugLog::write('WPMgr Backup: watchdog give-up failed for snapshot ' . $snapshotId . ': ' . $e->getMessage());
        }
        if ($given === null) {
            // Nothing was given up: the next watchdog pass decides again.
            if (!$locked) {
                DebugLog::write(sprintf('WPMgr Backup: watchdog deferred giving up snapshot %s to its next pass: a live worker holds its run-lock, or the lock could not be taken', $snapshotId));
            }
            self::schedule($snapshotId, self::RESCHEDULE_SECONDS);
        }
        return $given;
    }

    /**
     * GH #256: reclaim a give-up path's scratch directory BEFORE the task
     * row that names it is discarded (deleted, in every branch that calls
     * this). Reuses TaskRunner::reclaimScratch() (itself a thin wrapper
     * around the existing, already-tested cleanupOnFailed() file-level
     * sweep) rather than a second, independently-duplicated implementation.
     *
     * Every give-up path that calls this already holds the freshly-SELECTed
     * $row in memory: this decodes sub_state.params from THAT row (never
     * issues a second query) and reads everything it needs before the
     * caller's own row DELETE runs.
     *
     * SAFETY (post-ship review, ship blocker, findings 1/2/3): a task row
     * reaching one of this method's four call sites (resumeIfStalled()'s
     * 7200s hard-ceiling guard, its late-phase 300s stale guard, its
     * max-resumes exhaustion, and dispatch()'s 7200s stale-task guard) is
     * judged "dead" purely from DB-column signals: started_at age,
     * last_progress_at age, resume_count. None of those signals can
     * distinguish a genuinely abandoned run from one that is still alive
     * and simply has not posted progress recently (for example, a single
     * large chunk upload on a slow link legitimately blocking for minutes
     * with no callback in between). A large site's backup can legitimately
     * run for hours. Deleting a LIVE runner's scratch directory out from
     * under it while it is actively writing chunks is far worse than the
     * disk leak this method exists to close.
     *
     * GH #256 finding 1 (TOCTOU, post-ship re-review): this used to acquire
     * the EXACT SAME run-lock guards TaskRunner::run() itself takes for this
     * snapshot via a standalone probe (TaskRunner::isRunLockFree()),
     * release them immediately, and only THEN call reclaimScratch() as a
     * separate step. That left a window between "the probe reported free"
     * and "the glob-and-unlink sweep actually finished" spanning the whole
     * cleanup (globs plus N unlinks, tens of milliseconds on a run with
     * thousands of chunk files) during which a resumed/re-entered runner
     * could start writing into the same directory again. This now calls
     * TaskRunner::withRunLock(), which acquires the exact same guards
     * (GET_LOCK then flock) and invokes the reclaim callback WHILE holding
     * both, releasing them only once the callback returns, so there is no
     * gap between "checked" and "used":
     *   - Lock acquired: no live runner is working this snapshot right now
     *     (run() itself would also be free to proceed), so the reclaim
     *     callback runs and the scratch directory is removed before the
     *     lock is ever released.
     *   - Lock held by someone else (or the flock guard could not even be
     *     attempted, GH #256 finding 2, see TaskRunner::withRunLock()'s
     *     doc): the scratch directory is left untouched.
     *     BackupJanitor::gcRuns() (its own 6h age gate, isActiveRun() veto,
     *     and, GH #256 finding 3, the same run-lock gate) remains the
     *     safe backstop that reclaims it once the run genuinely finishes or
     *     is abandoned.
     * The lock is the liveness signal here, not a second, independently
     * maintained heuristic that could disagree with the one run() itself
     * relies on for correctness. GH #983: only the 24h ceiling, and a row
     * without params to locate its lock, reach this method; their row
     * DELETE proceeds regardless and only the file reclaim is gated. Every
     * other give-up reclaims and deletes inside one hold of the run-lock
     * (giveUpUnderRunLock()).
     *
     * Best-effort and defensive: sub_state.params (or its scratch_dir) may
     * be absent (a row seeded by something other than BackupCommand, or a
     * corrupt sub_state), in which case there is no scratch_dir to resolve
     * and this is a silent no-op, exactly like every other best-effort
     * cleanup in this class. A thrown reclaim failure is swallowed so it can
     * never mask (or block) the give-up path's own row discard.
     *
     * @param array<string,mixed> $row Task row already loaded by the caller.
     * @return void
     */
    private static function reclaimRunScratch(array $row): void
    {
        $subState = self::decodeSubState($row['sub_state'] ?? '');
        $params   = is_array($subState['params'] ?? null) ? $subState['params'] : null;
        if (!is_array($params) || !isset($params['scratch_dir']) || !is_string($params['scratch_dir']) || $params['scratch_dir'] === '') {
            return;
        }

        $snapshotId = isset($row['snapshot_id']) && is_string($row['snapshot_id']) ? $row['snapshot_id'] : '';

        try {
            $runner   = new TaskRunner($params);
            $reclaimed = $runner->withRunLock(static function () use ($runner): void {
                $runner->reclaimScratch();
            });
            if (!$reclaimed) {
                DebugLog::write(sprintf(
                    'WPMgr Backup: watchdog declined to reclaim scratch for snapshot %s: the run-lock is held by a live runner (or could not be proven free); leaving the scratch directory for BackupJanitor::gcRuns()',
                    $snapshotId
                ));
            }
        } catch (\Throwable $e) {
            DebugLog::write('WPMgr Backup: watchdog scratch reclaim failed: ' . $e->getMessage());
        }
    }

    /**
     * dispatch — UNCONDITIONAL first-run TaskRunner invocation, bound to the
     * 'wpmgr_backup_run' cron hook. BackupCommand fires this immediately
     * after seeding the task row (via wp_schedule_single_event + spawn_cron),
     * so the initial backup runs in a separate FPM worker fired by wp-cron.
     *
     * Distinct from run() above (the watchdog): run() short-circuits if the
     * task isn't stalled. dispatch() doesn't check stall state — it ALWAYS
     * tries to invoke TaskRunner from sub_state.params, because at this
     * point the task is freshly queued and HASN'T been run yet.
     *
     * Idempotency: if TaskRunner has already been invoked (phase moved past
     * queued), TaskRunner::run() reads the current phase from the row and
     * dispatches to that phase — so a duplicate cron firing is safe.
     *
     * @param string $snapshotId Snapshot id passed via wp_schedule_single_event $args.
     * @return void
     */
    public static function dispatch(string $snapshotId): void
    {
        if ($snapshotId === '' || !preg_match('/^[a-f0-9-]{36}$/i', $snapshotId)) {
            return;
        }
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        /** @var \wpdb $wpdb */
        $table = $wpdb->prefix . Schema::BACKUP_TASKS_TABLE;
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching,WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- direct query on plugin-owned table; identifier is prefix+constant; no caching on live task-state read
        $row = $wpdb->get_row($wpdb->prepare("SELECT * FROM {$table} WHERE snapshot_id = %s", $snapshotId), ARRAY_A);
        if (!is_array($row)) {
            DebugLog::write(sprintf('WPMgr Backup: dispatch cannot find task row for snapshot %s', $snapshotId));
            return;
        }

        // GitHub issue #232: durable breadcrumb the very moment dispatch() is
        // entered — the FIRST possible write after BackupCommand seeds
        // phase=queued that is reachable even if TaskRunner::run() below
        // never advances phase at all (e.g. it crashes before its own first
        // saveTaskState). Distinguishes "the wpmgr_backup_run hook fired but
        // the runner made zero progress" from "the hook never fired at all"
        // — see the observability ladder in TaskRunner::run()'s GET_LOCK doc.
        self::stampStage($snapshotId, 'dispatch_entered');
        DebugLog::write(sprintf('WPMgr Backup: dispatch entered for snapshot %s', $snapshotId));

        $phase = (string) ($row['phase'] ?? '');
        if ($phase === TaskRunner::PHASE_COMPLETED || $phase === TaskRunner::PHASE_FAILED) {
            // Terminal. DELETE the row so this and any future stale
            // wpmgr_backup_run firings short-circuit at the "row missing"
            // check above. Mirrors the success-path cleanup in TaskRunner.
            @$wpdb->delete($table, ['snapshot_id' => $snapshotId], ['%s']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct delete on plugin-owned table; terminal cleanup
            return;
        }

        // SAFETY (Bug 2 fix): refuse to enter an old backup task whose phase
        // has moved past `queued`. A wpmgr_backup_run cron event scheduled
        // hours ago that fires NOW (because wp-cron only runs on visitor
        // traffic + the host has been idle) MUST NOT re-spawn TaskRunner
        // against an abandoned in-flight row — it would re-issue
        // presignChunks calls the CP would 422-reject (observed in M5.6
        // ADR-034 live QA, mid-restore). GH #983: "old" is the same rule
        // resume() applies, so a task whose progress is recent is never
        // refused here. GH #256: giveUp() reclaims scratch under the
        // run-lock before the row (the only record of where it lives) goes.
        $subState  = self::decodeSubState($row['sub_state'] ?? '');
        $ageReason = $phase !== TaskRunner::PHASE_QUEUED ? self::ageGiveUpReasonUnlessLive($row, $subState) : null;
        if ($ageReason !== null) {
            $startedAt = (int) ($row['started_at'] ?? 0);
            self::giveUp($row, $subState, sprintf(
                'WPMgr Backup: dispatch refusing to start stale task for snapshot %s (started %ds ago, phase=%s); deleted row.',
                $snapshotId, time() - $startedAt, $phase
            ), $ageReason, self::ageRecheck($ageReason));
            return;
        }

        $params = is_array($subState['params'] ?? null) ? $subState['params'] : null;
        if (!is_array($params)) {
            DebugLog::write(sprintf('WPMgr Backup: dispatch cannot extract params for snapshot %s', $snapshotId));
            return;
        }
        // Lift PHP's per-request caps — this cron worker may run for minutes.
        LongRunningJob::extendLimits();
        try {
            (new TaskRunner($params))->run();
        } catch (\Throwable $e) {
            DebugLog::write('WPMgr Backup: dispatch runner fatal: ' . $e->getMessage());
        }
    }

    /**
     * Schedule (or re-schedule) the watchdog for a snapshot. Idempotent:
     * WP-Cron dedupes identical (hook, args) pairs at the same timestamp
     * via its built-in scheduler.
     *
     * @param string $snapshotId UUID.
     * @param int    $delay      Seconds from now.
     * @return void
     */
    public static function schedule(string $snapshotId, int $delay = self::RESCHEDULE_SECONDS): void
    {
        if (!function_exists('wp_schedule_single_event')) {
            return;
        }
        wp_schedule_single_event(time() + max(1, $delay), self::HOOK, [$snapshotId]);
    }

    /**
     * GitHub issue #232 — WP-Cron-INDEPENDENT reaper. Finds every
     * wpmgr_backup_tasks row that is non-terminal AND has gone stale (no
     * progress for STALL_THRESHOLD_SECONDS) and resumes each one through
     * resumeIfStalled(). Uses the existing `KEY phase` + `KEY
     * last_progress_at` indexes already on the table (see
     * Schema::BACKUP_TASKS_TABLE) — no migration required.
     *
     * Safe to call from an ordinary request — Plugin::maybeSweepStalledBackups()
     * binds this to `plugins_loaded`, throttled to once per 60s. Each
     * resumeIfStalled() re-enters TaskRunner::run(), which now takes the GH
     * #232 advisory run-lock before doing any phase work — so a sweep that
     * races an already-live runner in a separate PHP process/DB connection is
     * a harmless no-op (the sweep's TaskRunner instance loses the lock and
     * returns without mutating phase).
     *
     * @param int $limit Maximum number of stalled rows to resume per call —
     *                    bounds the worst-case cost of a single request that
     *                    happens to trigger the sweep.
     * @return void
     */
    public static function sweepStalled(int $limit = 20): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        /** @var \wpdb $wpdb */

        $table     = $wpdb->prefix . Schema::BACKUP_TASKS_TABLE;
        $cutoff    = time() - self::STALL_THRESHOLD_SECONDS;
        $safeLimit = max(1, $limit);

        try {
            // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- interpolated identifier is prefix+constant (trusted); values bound via placeholders
            $sql = "SELECT snapshot_id FROM {$table}
                    WHERE phase NOT IN (%s, %s) AND last_progress_at < %d
                    ORDER BY started_at ASC
                    LIMIT %d";
            $prepared = $wpdb->prepare(
                // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- $sql is a hard-coded query template with placeholders only; values are bound by this very prepare() call
                $sql,
                TaskRunner::PHASE_COMPLETED,
                TaskRunner::PHASE_FAILED,
                $cutoff,
                $safeLimit
            );
            $rows = $wpdb->get_col($prepared); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching, WordPress.DB.PreparedSQL.NotPrepared -- direct read on plugin-owned table; value is the output of $wpdb->prepare()
        } catch (\Throwable $e) {
            DebugLog::write('WPMgr Backup: sweepStalled query failed: ' . $e->getMessage());
            return;
        }

        if (!is_array($rows)) {
            return;
        }

        foreach ($rows as $snapshotId) {
            if (!is_string($snapshotId) || $snapshotId === '') {
                continue;
            }
            try {
                self::resumeIfStalled($snapshotId);
            } catch (\Throwable $e) {
                DebugLog::write(sprintf('WPMgr Backup: sweepStalled resume failed for %s: %s', $snapshotId, $e->getMessage()));
            }
        }
    }

    /**
     * Stamp a durable `sub_state.stage` breadcrumb on the task row, and touch
     * `last_progress_at` to `now` (so a fired-then-killed hook is
     * distinguishable from one that never fired, and becomes
     * sweepStalled()-detectable). Feeds the observability ladder documented
     * on TaskRunner::run() and dispatch() above:
     *
     *   no stage + last_progress_at==started_at => hook never fired
     *   stage=dispatch_entered                   => hook fired, runner never advanced
     *   stage=runner_started                     => lock held, first phase failed to persist
     *   phase past 'queued'                      => advanced
     *
     * Reads the RAW sub_state column value directly (never through
     * TaskRunner's sidecar-aware loadTask(), which would rehydrate a
     * potentially large de-sidecared cursor). Merging 'stage' into whatever
     * tiny envelope already lives in the column — a genuine small sub_state
     * OR a `{"_sidecar":true,...}` pointer object — keeps this write always
     * small. Best-effort: a breadcrumb write failure must never propagate.
     *
     * @param string $snapshotId Snapshot UUID.
     * @param string $stage      Breadcrumb value to stamp.
     * @return void
     */
    private static function stampStage(string $snapshotId, string $stage): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        /** @var \wpdb $wpdb */
        $table = $wpdb->prefix . Schema::BACKUP_TASKS_TABLE;

        try {
            // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- interpolated identifier is prefix+constant (trusted); value bound via %s placeholder
            $sql = "SELECT sub_state FROM {$table} WHERE snapshot_id = %s LIMIT 1";
            /** @phpstan-ignore-next-line — dynamic wpdb. */
            $prepared = $wpdb->prepare($sql, $snapshotId); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- already prepared on the preceding line
            $raw = $wpdb->get_var($prepared); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching, WordPress.DB.PreparedSQL.NotPrepared -- direct read on plugin-owned table; value is the output of $wpdb->prepare()

            $envelope = [];
            if (is_string($raw) && $raw !== '') {
                $decoded = json_decode($raw, true);
                if (is_array($decoded)) {
                    $envelope = $decoded;
                }
            }
            $envelope['stage'] = $stage;
            $encoded = json_encode($envelope);
            if ($encoded === false) {
                return;
            }

            @$wpdb->update( // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- direct update on plugin-owned table; breadcrumb write, correctness requires a live write
                $table,
                ['sub_state' => $encoded, 'last_progress_at' => time()],
                ['snapshot_id' => $snapshotId],
                ['%s', '%d'],
                ['%s']
            );
        } catch (\Throwable $e) {
            // Swallow — a breadcrumb write failure must never break
            // dispatch()/resumeIfStalled().
        }
    }

    /**
     * When a task stalls in the `queued` phase (the runner was never
     * dispatched at all), probe the wp-cron loopback URL to determine
     * whether a membership or privacy gate is the likely cause.
     *
     * Returns an actionable human-readable message for the failure reason
     * that is stored on the task row and surfaced to the operator via the
     * CP dashboard.
     *
     * The probe uses `wp_remote_get` with `redirection=0` so a single
     * HTTP redirect is the definitive gate signal (same logic as
     * BackupCommand::isLoopbackGated, but from the watchdog context to
     * cover the case where the in-process fallback also failed — e.g. on
     * CLI-cron driven sites where PHP's shutdown-function route is also
     * unreliable).
     *
     * @param string $phase      Current task phase (used to decide whether to probe).
     * @param string $snapshotId Snapshot id (logged for traceability).
     * @return string Failure reason message.
     */
    private static function detectQueuedStallReason(string $phase, string $snapshotId): string
    {
        $generic = 'backup runner was never dispatched (stalled in queued phase with no progress)';

        // Only add loopback-gate detail when the task never left queued
        // (a stall in a later phase has a different root cause).
        if ($phase !== TaskRunner::PHASE_QUEUED) {
            return $generic;
        }

        if (!function_exists('wp_remote_get') || !function_exists('site_url')) {
            return $generic;
        }

        $cronUrl = (string) site_url('/wp-cron.php');
        if ($cronUrl === '' || $cronUrl === '/wp-cron.php') {
            return $generic;
        }

        $response = wp_remote_get(
            $cronUrl,
            [
                'timeout'     => 3,
                'redirection' => 0,
                'sslverify'   => false,
                'blocking'    => true,
                'user-agent'  => 'WPMgr-Agent/Watchdog',
            ]
        );

        if (is_wp_error($response)) {
            return sprintf(
                'backup runner was never dispatched — the WordPress cron loopback URL (%s) could not be reached (%s); verify that the site can make HTTP requests to itself',
                esc_url_raw($cronUrl),
                $response->get_error_message()
            );
        }

        $code = (int) wp_remote_retrieve_response_code($response);
        if ($code >= 300 && $code < 400) {
            $location = wp_remote_retrieve_header($response, 'location');
            DebugLog::write(sprintf(
                'WPMgr Backup Watchdog: loopback probe for snapshot %s returned %d (location: %s) — site appears to be behind a membership/privacy gate',
                $snapshotId,
                $code,
                is_string($location) ? $location : '(unknown)'
            ));
            return sprintf(
                'backup continuation request was redirected to a login page (HTTP %d) — the site appears to be behind a membership or privacy gate that blocks WordPress cron loopback requests to %s; scheduled backups cannot run until the gate allows /wp-cron.php through unauthenticated, or until the WPMgr agent cron loopback is whitelisted in the membership plugin settings',
                $code,
                esc_url_raw($cronUrl)
            );
        }

        return $generic;
    }

    /**
     * Best-effort JSON decode of a raw `sub_state` column value. Delegates
     * to TaskRunner::decodeSubStateColumn() (rather than a second,
     * independently maintained decode) so this class follows the exact
     * same sidecar-spill rehydration TaskRunner::loadTask() applies.
     *
     * GH #256 finding 5: a bare json_decode() here (the pre-fix behavior)
     * returned only the small `{"_sidecar":true,...}` pointer object for
     * any task whose cursor had spilled to the sidecar file, which
     * silently dropped `sub_state.params` for every caller of this method,
     * including reclaimRunScratch()'s own read of `scratch_dir`, on
     * exactly the largest runs (the ones with the most scratch to
     * reclaim, and the ones most likely to trip TaskRunner's
     * SUBSTATE_SIDECAR_THRESHOLD in the first place).
     *
     * Returns [] on any failure (missing column, invalid JSON, unreadable
     * sidecar file).
     */
    private static function decodeSubState($raw): array
    {
        return TaskRunner::decodeSubStateColumn($raw);
    }
}
