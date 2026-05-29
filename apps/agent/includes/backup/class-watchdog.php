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
     * @param string $snapshotId UUID of the snapshot to inspect.
     * @return void
     */
    public static function run(string $snapshotId): void
    {
        if ($snapshotId === '' || !preg_match('/^[a-f0-9-]{36}$/i', $snapshotId)) {
            return;
        }

        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }

        $table = $wpdb->prefix . Schema::BACKUP_TASKS_TABLE;
        // @phpstan-ignore-next-line — dynamic wpdb.
        $row = $wpdb->get_row($wpdb->prepare("SELECT * FROM {$table} WHERE snapshot_id = %s", $snapshotId), ARRAY_A);
        if (!is_array($row)) {
            return; // Task was already cleaned up (a completed runner deletes its row).
        }

        $phase = (string) ($row['phase'] ?? '');
        if ($phase === TaskRunner::PHASE_COMPLETED || $phase === TaskRunner::PHASE_FAILED) {
            // Terminal. Best-effort DELETE the row so a future stale
            // wpmgr_backup_watchdog event can't even find it. (Defensive
            // cleanup — the TaskRunner success path now also DELETEs.)
            // @phpstan-ignore-next-line — dynamic wpdb.
            @$wpdb->delete($table, ['snapshot_id' => $snapshotId], ['%s']);
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
            // DELETE the row (not just mark failed) so the next watchdog
            // tick finds nothing and immediately returns. We DON'T touch
            // the CP — the snapshot's CP-side status is whatever the
            // last legitimate /progress post made it; if it's already
            // 'completed' on the CP, this guard prevents the phantom 'failed'
            // event that would otherwise overwrite it.
            // @phpstan-ignore-next-line — dynamic wpdb.
            @$wpdb->delete($table, ['snapshot_id' => $snapshotId], ['%s']);
            error_log(sprintf(
                'WPMgr Backup: watchdog refusing to re-enter stale task for snapshot %s (started %ds ago, phase=%s); deleted row without re-entry',
                $snapshotId, $age, $phase
            ));
            return;
        }

        // SAFETY 2 (Bug 2 fix): refuse to re-enter a task whose phase has
        // been stuck at `encrypting_uploading` or `submitting_manifest` for
        // more than 5 minutes. These are the LATE phases of the pipeline —
        // if we got that far the artifacts are already uploaded and the
        // manifest is either submitted or about to be. Re-entering would
        // re-issue presignChunks calls the CP would 422-reject (the
        // observed bug). Strong signal: dead/stale row regardless of
        // started_at age.
        $lastProgress = (int) ($row['last_progress_at'] ?? 0);
        $stalledFor   = time() - $lastProgress;
        if (
            ($phase === TaskRunner::PHASE_ENCRYPTING_UPLOADING
                || $phase === TaskRunner::PHASE_SUBMITTING_MANIFEST)
            && $stalledFor > 300
        ) {
            // @phpstan-ignore-next-line — dynamic wpdb.
            @$wpdb->delete($table, ['snapshot_id' => $snapshotId], ['%s']);
            error_log(sprintf(
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
            // Give up — too many resume attempts. Mark failed so the CP
            // and UI see a terminal state. The TaskRunner's own
            // bookkeeping does this normally; doing it here too is
            // defensive (the runner might be wedged in a way we can't
            // re-enter).
            // @phpstan-ignore-next-line
            @$wpdb->update($table, ['phase' => TaskRunner::PHASE_FAILED, 'last_progress_at' => time()], ['snapshot_id' => $snapshotId], ['%s', '%d'], ['%s']);
            error_log(sprintf('WPMgr Backup: snapshot %s exhausted %d resume attempts; marked failed', $snapshotId, $maxResumes));
            return;
        }

        // @phpstan-ignore-next-line
        @$wpdb->update($table, ['resume_count' => $resumeCount + 1, 'last_progress_at' => time()], ['snapshot_id' => $snapshotId], ['%d', '%d'], ['%s']);
        error_log(sprintf('WPMgr Backup: watchdog resuming snapshot %s (attempt %d/%d) — stalled for %ds in phase=%s',
            $snapshotId, $resumeCount + 1, $maxResumes, $stalledFor, $phase));

        // Reconstruct the runner from the persisted task row. params
        // sub_state lives in the row; the OTHER params (endpoints, db,
        // age recipient) live in sub_state.params (BackupCommand stuffed
        // them there on first run so the watchdog can rehydrate without
        // re-receiving them from the CP).
        $subState = self::decodeSubState($row['sub_state'] ?? '');
        $params   = is_array($subState['params'] ?? null) ? $subState['params'] : null;
        if (!is_array($params)) {
            error_log(sprintf('WPMgr Backup: watchdog cannot resume %s — sub_state.params missing', $snapshotId));
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
        $table = $wpdb->prefix . Schema::BACKUP_TASKS_TABLE;
        // @phpstan-ignore-next-line — dynamic wpdb.
        $row = $wpdb->get_row($wpdb->prepare("SELECT * FROM {$table} WHERE snapshot_id = %s", $snapshotId), ARRAY_A);
        if (!is_array($row)) {
            error_log(sprintf('WPMgr Backup: dispatch cannot find task row for snapshot %s', $snapshotId));
            return;
        }
        $phase = (string) ($row['phase'] ?? '');
        if ($phase === TaskRunner::PHASE_COMPLETED || $phase === TaskRunner::PHASE_FAILED) {
            // Terminal. DELETE the row so this and any future stale
            // wpmgr_backup_run firings short-circuit at the "row missing"
            // check above. Mirrors the success-path cleanup in TaskRunner.
            // @phpstan-ignore-next-line — dynamic wpdb.
            @$wpdb->delete($table, ['snapshot_id' => $snapshotId], ['%s']);
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
            // @phpstan-ignore-next-line — dynamic wpdb.
            @$wpdb->delete($table, ['snapshot_id' => $snapshotId], ['%s']);
            error_log(sprintf(
                'WPMgr Backup: dispatch refusing to start stale task for snapshot %s (started %ds ago, phase=%s); deleted row',
                $snapshotId, time() - $startedAt, $phase
            ));
            return;
        }

        $subState = self::decodeSubState($row['sub_state'] ?? '');
        $params   = is_array($subState['params'] ?? null) ? $subState['params'] : null;
        if (!is_array($params)) {
            error_log(sprintf('WPMgr Backup: dispatch cannot extract params for snapshot %s', $snapshotId));
            return;
        }
        // Lift PHP's per-request caps — this cron worker may run for minutes.
        @set_time_limit(0);
        @ignore_user_abort(true);
        try {
            (new TaskRunner($params))->run();
        } catch (\Throwable $e) {
            error_log('WPMgr Backup: dispatch runner fatal: ' . $e->getMessage());
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

    /** Best-effort JSON decode. Returns [] on any failure. */
    private static function decodeSubState($raw): array
    {
        if (!is_string($raw) || $raw === '') {
            return [];
        }
        $decoded = json_decode($raw, true);
        return is_array($decoded) ? $decoded : [];
    }
}
