<?php
/**
 * BackupJanitor: GH #151 — recurring GC backstop for the backup pipeline's
 * per-run scratch directory, `wp-content/wpmgr-agent/runs/<snapshot_id>/`.
 *
 * TaskRunner::cleanupOnCompleted() removes that directory, but ONLY on the
 * `completed` phase transition — the failure path deletes the
 * `wpmgr_backup_tasks` row without cleaning scratch, leaking the DB dump,
 * zip parts, and chunk files of every failed run forever. This class sweeps
 * those leaked directories on an age-gated cadence. GH #232 follow-up: the
 * same sweep also reclaims the sibling `<snapshot_id>.lock` coordination
 * FILE TaskRunner's flock() guard creates alongside each run directory —
 * TaskRunner::run() reclaims its own on a normal terminal exit, but a run
 * that crashes before reaching that cleanup (fatal/OOM/SIGKILL) leaks the
 * lock file exactly like it always leaked the run directory. Full design
 * rationale (the "a just-failed run has no task row" subtlety, the
 * age-threshold reasoning, and the task-row veto) is in the design notes at
 * the bottom of this file — kept there so the ABSPATH direct-access guard
 * below stays within the first ~50 lines of the file, which is what Plugin
 * Check's Direct_File_Access_Check regex fallback scans.
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

use WPMgr\Agent\Schema;

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/**
 * Recurring GC backstop for the backup scratch directory
 * (`wp-content/wpmgr-agent/runs/`). See the design notes at the bottom of
 * this file for the full leak/age-gate/task-row-veto rationale.
 *
 * Not `final` — mirrors SnapshotManager's testability shape: a test
 * subclass overrides runsBaseDir() (and, where needed, isActiveRun()) to
 * fully control on-disk state without depending on ambient
 * WP_CONTENT_DIR/$wpdb, and `new static()` inside gcRuns() (late static
 * binding, not `new self()`) lets that subclass's overrides run through the
 * exact same production code path a real `BackupJanitor::gcRuns()` call
 * takes.
 */
class BackupJanitor
{
    /** Recurring cron hook name — bind via `add_action(self::HOOK_GC, …)`. */
    public const HOOK_GC = 'wpmgr_backup_runs_gc';

    /**
     * GC age threshold. A `runs/<snapshot_id>/` directory is only ever
     * considered for sweeping once it has been untouched (by mtime) for at
     * least this long. See the design notes below for why 6h is
     * comfortably clear of every legitimate in-flight window.
     */
    public const RUNS_GC_AGE_SECONDS = 21600; // 6h

    /**
     * Cron callback — sweep `runs/` for any old, inactive per-snapshot
     * scratch directory. See the design notes at the bottom of this file
     * for the full age-gate + task-row-veto design.
     *
     * `new static()`, not `new self()` — lets a test double subclass
     * override runsBaseDir()/isActiveRun() and still exercise this exact
     * production code path via `SubclassName::gcRuns()`.
     *
     * @return void
     */
    public static function gcRuns(): void
    {
        $j    = new static();
        $base = $j->runsBaseDir();
        if ($base === '' || !is_dir($base)) {
            return;
        }

        $items = @scandir($base);
        if (!is_array($items)) {
            return;
        }

        $now = time();
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }

            // GH #232 follow-up: TaskRunner's flock() coordination file,
            // `<snapshot_id>.lock` — created alongside (never inside) the
            // per-snapshot run directory, so it is resolvable even before
            // that directory exists (see TaskRunner::fileLockPath()'s doc).
            // TaskRunner::run()'s own `finally` reclaims a run's OWN lock
            // file once it reaches a genuinely terminal outcome, but a run
            // that crashes before ever reaching that `finally` (fatal/OOM/
            // SIGKILL) leaks it forever — the exact same class of leak GH
            // #151 exists to fix for the run DIRECTORY, now for this
            // sibling artifact. Same age gate + isActiveRun() veto as the
            // directory sweep below; a file, not a directory, so it is
            // reclaimed with a plain unlink rather than deleteDir().
            if (preg_match('/^[0-9a-fA-F-]{36}\.lock$/', $item) === 1) {
                $lockFile = $base . '/' . $item;
                if (!is_file($lockFile) || is_link($lockFile)) {
                    continue;
                }

                $lockMtime = @filemtime($lockFile);
                if ($lockMtime === false) {
                    // Fail-safe: cannot determine age — retain rather than
                    // risk deleting a lock a still-running process holds.
                    continue;
                }
                if (($now - $lockMtime) < self::RUNS_GC_AGE_SECONDS) {
                    continue; // Too fresh — could still belong to an active run.
                }

                $snapshotId = substr($item, 0, -5); // Strip the '.lock' suffix.
                if ($j->isActiveRun($snapshotId)) {
                    continue; // Belt-and-suspenders: the task row says it's still live.
                }

                wp_delete_file($lockFile);
                continue;
            }

            // Run directories are named after BackupCommand's snapshot_id —
            // a UUID. No path separators or traversal sequences can match
            // this pattern, so there is no way for a crafted entry name to
            // escape $base.
            if (preg_match('/^[0-9a-fA-F-]{36}$/', $item) !== 1) {
                continue;
            }

            $dir = $base . '/' . $item;
            if (!is_dir($dir) || is_link($dir)) {
                continue;
            }

            $mtime = @filemtime($dir);
            if ($mtime === false) {
                // Fail-safe: cannot determine age — retain rather than risk
                // deleting a run that may still be live.
                continue;
            }
            if (($now - $mtime) < self::RUNS_GC_AGE_SECONDS) {
                continue; // Too fresh — could still be an active run.
            }

            if ($j->isActiveRun($item)) {
                continue; // Belt-and-suspenders: the task row says it's still live.
            }

            $j->deleteDir($dir);
        }
    }

    /**
     * The absolute backup-runs scratch base directory:
     * `wp-content/wpmgr-agent/runs`. Mirrors the exact literal
     * BackupCommand::prepareScratchDir() pins. Overridable so tests never
     * depend on the real, process-global WP_CONTENT_DIR constant.
     *
     * @return string Absolute path, or '' when WP_CONTENT_DIR is unavailable.
     */
    protected function runsBaseDir(): string
    {
        return defined('WP_CONTENT_DIR')
            ? rtrim((string) WP_CONTENT_DIR, '/\\') . '/wpmgr-agent/runs'
            : '';
    }

    /**
     * Whether $snapshotId's `wpmgr_backup_tasks` row says the run is still
     * live. This is a NO-ONLY veto: it can prevent a deletion the age gate
     * in gcRuns() already decided to consider, but a missing row must never
     * be treated as a "yes, delete" signal on its own — see the design
     * notes below for the "just-failed run has no row" subtlety.
     *
     * Mirrors Watchdog::run()'s own terminal-row check (class-watchdog.php
     * ~89-96): no row (completed and already cleaned up, OR the failure
     * path's row DELETE already ran) => not active, defer entirely to the
     * age gate; a terminal phase => not active; otherwise active only while
     * `last_progress_at` is within Watchdog::STALL_THRESHOLD_SECONDS (the
     * same staleness window the watchdog itself uses to decide whether a
     * runner is still alive).
     *
     * @param string $snapshotId UUID of the run to check.
     * @return bool
     */
    protected function isActiveRun(string $snapshotId): bool
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return false;
        }

        $table = $wpdb->prefix . Schema::BACKUP_TASKS_TABLE;
        // @phpstan-ignore-next-line — dynamic wpdb.
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching, WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- direct read on plugin-owned task-state table; identifier is prefix+constant; live liveness check, not cacheable
        $row = $wpdb->get_row($wpdb->prepare("SELECT phase, last_progress_at FROM {$table} WHERE snapshot_id = %s", $snapshotId), ARRAY_A);
        if (!is_array($row)) {
            // No row: either the run finished cleanly (already cleaned up)
            // or it just FAILED (the failure path deletes the row without
            // cleaning scratch — the exact bug this class fixes). Either
            // way, this is never a "delete now" signal by itself — the age
            // gate in gcRuns() is the sole decision-maker for this case.
            return false;
        }

        $phase = (string) ($row['phase'] ?? '');
        if ($phase === TaskRunner::PHASE_COMPLETED || $phase === TaskRunner::PHASE_FAILED) {
            return false;
        }

        $lastProgressAt = (int) ($row['last_progress_at'] ?? 0);

        return $lastProgressAt > 0 && (time() - $lastProgressAt) < Watchdog::STALL_THRESHOLD_SECONDS;
    }

    /**
     * Recursively delete a directory. Self-contained (not shared with
     * SnapshotManager) so this class has no cross-subsystem coupling.
     * A file that won't unlink leaves the directory non-empty, so the
     * trailing rmdir() fails and the directory is simply retained and
     * retried on the next cron tick — never a crash.
     *
     * @param string $dir Directory to remove.
     * @return bool
     */
    private function deleteDir(string $dir): bool
    {
        if (!is_dir($dir)) {
            return false;
        }
        $items = @scandir($dir);
        if ($items === false) {
            return false;
        }
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $path = $dir . '/' . $item;
            if (is_dir($path) && !is_link($path)) {
                $this->deleteDir($path);
            } else {
                wp_delete_file($path);
            }
        }

        return @rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- removes an empty server-derived scratch dir; WP_Filesystem not initialized in the headless agent
    }

    /**
     * Schedule the recurring GC backstop (gcRuns()). Safe to call on every
     * activation / reschedule pass — a no-op when already scheduled.
     * Mirrors SnapshotManager::scheduleGc()'s idempotent-schedule shape.
     *
     * @param int $now Current time.
     * @return void
     */
    public static function scheduleGc(int $now): void
    {
        if (!function_exists('wp_next_scheduled') || !function_exists('wp_schedule_event')) {
            return;
        }
        if (wp_next_scheduled(self::HOOK_GC) !== false) {
            return;
        }
        wp_schedule_event($now + 3600, 'daily', self::HOOK_GC);
    }
}

/*
 * Design notes (referenced from the class docblock above; kept down here so
 * the ABSPATH direct-access guard stays within the first ~50 lines of the
 * file, which is what Plugin Check's Direct_File_Access_Check regex
 * fallback scans — see class-restore-guard.php for the same convention).
 *
 * The leak (GitHub issue #151): BackupCommand::prepareScratchDir() creates
 * `runs/<snapshot_id>/` for every backup run and writes the DB dump,
 * per-component zip parts, chunk files, and checkpoint sidecars into it.
 * TaskRunner::run()'s success path calls cleanupOnCompleted(), which removes
 * that directory — but the `catch (\Throwable $e)` failure path only
 * DELETEs the `wpmgr_backup_tasks` row and posts a `failed` progress event;
 * it never calls cleanupOnCompleted(). Every failed run therefore leaks its
 * entire scratch directory forever. Reporters observed 1.5GB and 287MB
 * leaked this way.
 *
 * `runs/` is pure scratch, never archived (FilesArchiver::DEFAULT_EXCLUDES
 * includes 'wpmgr-agent') and never read by restore (restore uses
 * `restores/` scratch + fresh R2 pulls), so reclaiming it here is safe.
 *
 * CRITICAL subtlety this class is built around: a run that just FAILED has
 * NO `wpmgr_backup_tasks` row (TaskRunner deletes it as part of the failure
 * path) but its scratch dir is fresh. "No row" therefore must NEVER be read
 * as "safe to delete right now" — a just-started run also briefly has no
 * row (the row is seeded, but a race between mkdir and the INSERT is
 * possible) and a resumed/watchdog-recovered run's row can also churn. The
 * sweep is PRIMARILY age-based (RUNS_GC_AGE_SECONDS): only directories
 * untouched for a long time are even considered. The task-row check
 * (isActiveRun()) is an ADDITIONAL veto layered on top of the age gate — it
 * can only ever say "no, keep this", never "yes, delete this" — so a
 * missing row on an old-enough directory still falls through to deletion
 * (the just-failed case this class exists to fix), while a stale-looking
 * directory that a live/resuming task row says is still active is protected
 * regardless of age.
 *
 * RUNS_GC_AGE_SECONDS (6h) is deliberately far larger than any legitimate
 * in-flight window: 3x Watchdog::STALL_THRESHOLD_SECONDS's sibling hard
 * re-entry ceiling (class-watchdog.php's 7200s/2h "refuse to re-enter an
 * implausibly long-running task" guard) and roughly 12x the ~30-minute
 * longest legitimate backup observed on a real WP host. No sweepable
 * directory can therefore ever host a run that is still legitimately
 * running.
 */
