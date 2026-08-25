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
 *     restart…). Increment `resume_count` (cap at `max_resumes`) and
 *     re-enter `TaskRunner::run()` from the persisted `sub_state`.
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
     *   - If active but `last_progress_at < now() - STALL_THRESHOLD_SECONDS`,
     *     the runner has stalled (PHP process killed, FPM worker recycled,
     *     hosting restart…). Increment `resume_count` (cap at `max_resumes`)
     *     and re-enter `TaskRunner::run()` from the persisted `sub_state`.
     *   - If active but still posting progress, reschedule the watchdog
     *     itself for another +120s. Belt-and-suspenders: even if the
     *     runner never crashes, the watchdog stays alive long enough to
     *     observe the eventual `completed`/`failed` transition.
     *
     * Every guard below is preserved VERBATIM from the pre-#232 run(): the
     * 7200s hard-ceiling delete, the late-phase (encrypting_uploading /
     * submitting_manifest) delete, the STALL_THRESHOLD_SECONDS staleness
     * check, and the resume_count >= max_resumes -> mark-failed guard.
     *
     * @param string $snapshotId UUID of the snapshot to inspect.
     * @return void
     */
    public static function resumeIfStalled(string $snapshotId): void
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
            return; // Task was already cleaned up (a completed runner deletes its row).
        }

        $phase = (string) ($row['phase'] ?? '');
        if ($phase === TaskRunner::PHASE_COMPLETED || $phase === TaskRunner::PHASE_FAILED) {
            // Terminal. Best-effort DELETE the row so a future stale
            // wpmgr_backup_watchdog event can't even find it. (Defensive
            // cleanup — the TaskRunner success path now also DELETEs.)
            @$wpdb->delete($table, ['snapshot_id' => $snapshotId], ['%s']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct delete on plugin-owned table; correctness requires a live write
            return;
        }

        // SAFETY: refuse to re-enter a task that's been "running" for an
        // implausibly long time. The longest legit backup on a real WP host
        // is ~30 min; >2h means the row is stale state from a process that
        // died without cleanup, OR (the bug we shipped this guard for) a
        // task row from a backup that COMPLETED on the CP side but whose
        // local row never got DELETEd by a crashing/killed runner. Either
        // way, re-entering it now would re-run TaskRunner against an
        // already-completed snapshot, triggering presignChunks calls the
        // CP would 422-reject (observed in M5.6 ADR-034 live QA, mid-restore).
        $startedAt = (int) ($row['started_at'] ?? 0);
        $age       = time() - $startedAt;
        if ($startedAt > 0 && $age > 7200) {
            // GH #256: reclaim this run's scratch directory BEFORE the row
            // (the only record of where that scratch lives) is deleted
            // below. A row "running" for over 2h with no completion is a
            // strong signal by itself, but not a certain one: an unusually
            // large site can legitimately take that long, so
            // reclaimRunScratch() does not treat this age alone as proof
            // the run is dead. It reclaims the file only from INSIDE
            // TaskRunner's own run-lock (TaskRunner::withRunLock()), and
            // skips the file reclaim entirely (deferring to
            // BackupJanitor::gcRuns()) if a live runner still holds it. See
            // reclaimRunScratch()'s own doc for the full rationale.
            self::reclaimRunScratch($row);
            // DELETE the row (not just mark failed) so the next watchdog
            // tick finds nothing and immediately returns. We DON'T touch
            // the CP — the snapshot's CP-side status is whatever the
            // last legitimate /progress post made it; if it's already
            // 'completed' on the CP, this guard prevents the phantom 'failed'
            // event that would otherwise overwrite it.
            @$wpdb->delete($table, ['snapshot_id' => $snapshotId], ['%s']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct delete on plugin-owned table; stale-row cleanup
            DebugLog::write(sprintf(
                'WPMgr Backup: watchdog refusing to re-enter stale task for snapshot %s (started %ds ago, phase=%s); deleted row without re-entry',
                $snapshotId, $age, $phase
            ));
            return;
        }

        // SAFETY 2 (Bug 2 fix): refuse to re-enter a task whose phase has
        // been stuck at `encrypting_uploading` or `submitting_manifest` for
        // more than 5 minutes. These are the LATE phases of the pipeline;
        // if we got that far the artifacts are already uploaded and the
        // manifest is either submitted or about to be. Re-entering would
        // re-issue presignChunks calls the CP would 422-reject (the
        // observed bug).
        //
        // GH #256 finding 4 (post-ship review): this 300s figure is NOT a
        // reliable dead-run signal on its own. The GH #279 heartbeat is
        // emitted BETWEEN chunks, so a single large chunk upload on a slow
        // link can legitimately exceed 300s of database silence while the
        // runner is completely alive. Kept at 300s deliberately, for two
        // separate reasons rather than one:
        //   - As the trigger for the ROW discard below, 300s is fine: the
        //     row is only bookkeeping, and every other give-up branch in
        //     this method already accepts that a live runner can lose its
        //     row (see the class doc's re-entry contract) without harming
        //     the in-flight run itself.
        //   - As the trigger for the FILE reclaim, the number itself no
        //     longer needs to be conservative, because reclaimRunScratch()
        //     no longer trusts this threshold as proof of death: it
        //     reclaims files only from inside TaskRunner's own run-lock
        //     (TaskRunner::withRunLock()) and skips deleting any file
        //     when a live runner still holds it, deferring to
        //     BackupJanitor::gcRuns() instead. A slow-but-alive upload that
        //     trips this 300s gate simply has its reclaim attempt safely
        //     no-op.
        $lastProgress = (int) ($row['last_progress_at'] ?? 0);
        $stalledFor   = time() - $lastProgress;
        if (
            ($phase === TaskRunner::PHASE_ENCRYPTING_UPLOADING
                || $phase === TaskRunner::PHASE_SUBMITTING_MANIFEST)
            && $stalledFor > 300
        ) {
            // GH #256: reclaim scratch BEFORE discarding the row. This is
            // the LARGEST leak this fix closes: at this late phase every
            // zip part and every encrypted chunk file is already on disk
            // (see the class doc's "reporter's screenshot shape" note).
            // See reclaimRunScratch()'s own doc and the SAFETY 2 note above
            // for why this is now gated on the run-lock rather than on
            // $stalledFor alone.
            self::reclaimRunScratch($row);
            @$wpdb->delete($table, ['snapshot_id' => $snapshotId], ['%s']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct delete on plugin-owned table; late-phase stale-row cleanup
            DebugLog::write(sprintf(
                'WPMgr Backup: watchdog refusing to re-enter late-phase stalled task for snapshot %s (phase=%s, stalled %ds); deleted row',
                $snapshotId, $phase, $stalledFor
            ));
            return;
        }

        if ($stalledFor < self::STALL_THRESHOLD_SECONDS) {
            // Runner is alive — reschedule the watchdog so it stays
            // armed across the rest of the run. No state change.
            self::schedule($snapshotId, self::RESCHEDULE_SECONDS);
            return;
        }

        // STALLED. Bump resume_count + re-enter the runner.
        $resumeCount = (int) ($row['resume_count'] ?? 0);
        $maxResumes  = (int) ($row['max_resumes'] ?? 6);
        if ($resumeCount >= $maxResumes) {
            // Give up: too many resume attempts. The runner might be
            // wedged in a way we can't re-enter.
            //
            // If the task never left the `queued` phase (phase=queued AND
            // resume_count already at max), the runner was never dispatched
            // at all: strong signal that both the spawn_cron loopback and
            // the in-process fallback failed to start the runner. Detect the
            // loopback-gate scenario and surface a clear, actionable reason.
            $failureReason = self::detectQueuedStallReason($phase, $snapshotId);

            // GH #256: reclaim scratch BEFORE discarding the row. This
            // branch is only reached after $resumeCount >= $maxResumes
            // (default 6) SEPARATE stall detections, each already requiring
            // STALL_THRESHOLD_SECONDS (180s) of silence, a stronger dead-run
            // signal than either single-stall guard above, but still not a
            // certain one on its own for the reason explained in
            // reclaimRunScratch()'s own doc: the file reclaim inside that
            // call only ever runs from inside TaskRunner's own run-lock
            // (TaskRunner::withRunLock()) and skips deleting any file
            // when a live runner still holds it.
            //
            // DELETE the row rather than marking it failed (the pre-#256
            // behavior): a row left at phase=failed here was never revisited
            // by anything else in the agent (sweepStalled()'s own query
            // excludes terminal phases, so the row leaked forever), and
            // WORSE, a later re-entry into TaskRunner::run() for this exact
            // snapshot_id (e.g. a CP retry reusing the same id) would find
            // phase=failed and short-circuit at the "terminal? no-op" check
            // BEFORE ever reaching the catch block that calls
            // cleanupOnFailed(), so the scratch could never be reclaimed for
            // that snapshot again. Deleting the row here, mirroring every
            // other give-up branch in this method, closes both gaps at
            // once and matches BackupJanitor::isActiveRun()'s own existing
            // assumption that no row for a snapshot means "not active,
            // defer to the age gate" (never a live run). Nothing else in the
            // agent reads wpmgr_backup_tasks once a snapshot is terminal;
            // the failure reason is still captured below via DebugLog for
            // local troubleshooting, and the CP's own progress-watchdog
            // independently times the snapshot out on its side regardless.
            self::reclaimRunScratch($row);
            @$wpdb->delete($table, ['snapshot_id' => $snapshotId], ['%s']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct delete on plugin-owned table; terminal give-up, no further recovery is ever attempted for this row
            DebugLog::write(sprintf('WPMgr Backup: snapshot %s exhausted %d resume attempts; reclaimed scratch and deleted row. %s', $snapshotId, $maxResumes, $failureReason));
            return;
        }

        @$wpdb->update($table, ['resume_count' => $resumeCount + 1, 'last_progress_at' => time()], ['snapshot_id' => $snapshotId], ['%d', '%d'], ['%s']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct update on plugin-owned table; watchdog resume-count must be written live
        DebugLog::write(sprintf('WPMgr Backup: watchdog resuming snapshot %s (attempt %d/%d) — stalled for %ds in phase=%s',
            $snapshotId, $resumeCount + 1, $maxResumes, $stalledFor, $phase));

        // Reconstruct the runner from the persisted task row. params
        // sub_state lives in the row; the OTHER params (endpoints, db,
        // age recipient) live in sub_state.params (BackupCommand stuffed
        // them there on first run so the watchdog can rehydrate without
        // re-receiving them from the CP).
        $subState = self::decodeSubState($row['sub_state'] ?? '');
        $params   = is_array($subState['params'] ?? null) ? $subState['params'] : null;
        if (!is_array($params)) {
            DebugLog::write(sprintf('WPMgr Backup: watchdog cannot resume %s — sub_state.params missing', $snapshotId));
            return;
        }

        // Cap the next watchdog firing so this row keeps getting checked
        // until it terminates.
        self::schedule($snapshotId, self::RESCHEDULE_SECONDS);

        // Enter the runner. run() catches all exceptions and never
        // throws — so this call cannot kill the WP-Cron worker. The
        // runner reads phase + sub_state and dispatches.
        $runner = new TaskRunner($params);
        $runner->run();
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
     * relies on for correctness. This does not change the caller's row
     * DELETE, which proceeds regardless: only the file reclaim is gated.
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

        // SAFETY (Bug 2 fix): refuse to enter a backup task whose phase has
        // moved past `queued` AND whose started_at is >2h ago. A
        // wpmgr_backup_run cron event scheduled hours ago that fires NOW
        // (because wp-cron only runs on visitor traffic + the host has been
        // idle) MUST NOT re-spawn TaskRunner against an old in-flight row —
        // it would re-issue presignChunks calls the CP would 422-reject
        // (observed in M5.6 ADR-034 live QA, mid-restore).
        $startedAt = (int) ($row['started_at'] ?? 0);
        if ($startedAt > 0 && (time() - $startedAt) > 7200 && $phase !== TaskRunner::PHASE_QUEUED) {
            // GH #256: reclaim scratch BEFORE the row (the only record of
            // where it lives) is deleted below. Same 7200s dead-run premise
            // as resumeIfStalled()'s own hard-ceiling guard above, and the
            // same caveat: an unusually large site can legitimately still
            // be running at this age, so the file reclaim inside
            // reclaimRunScratch() does not rely on this age check alone. It
            // only ever reclaims files from inside TaskRunner's own
            // run-lock (TaskRunner::withRunLock()) and skips deleting any
            // file when a live runner still holds it, deferring to
            // BackupJanitor::gcRuns(). See reclaimRunScratch()'s own doc.
            self::reclaimRunScratch($row);
            @$wpdb->delete($table, ['snapshot_id' => $snapshotId], ['%s']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct delete on plugin-owned table; stale dispatch cleanup
            DebugLog::write(sprintf(
                'WPMgr Backup: dispatch refusing to start stale task for snapshot %s (started %ds ago, phase=%s); deleted row',
                $snapshotId, time() - $startedAt, $phase
            ));
            return;
        }

        $subState = self::decodeSubState($row['sub_state'] ?? '');
        $params   = is_array($subState['params'] ?? null) ? $subState['params'] : null;
        if (!is_array($params)) {
            DebugLog::write(sprintf('WPMgr Backup: dispatch cannot extract params for snapshot %s', $snapshotId));
            return;
        }
        // Lift PHP's per-request caps — this cron worker may run for minutes.
        @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged,Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running backup dispatch must not hit max_execution_time; @-guarded
        @ignore_user_abort(true);
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
