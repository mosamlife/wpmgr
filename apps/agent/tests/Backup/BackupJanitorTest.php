<?php
/**
 * BackupJanitorTest — GitHub issue #151 (backup scratch-dir leak on the
 * runner FAILURE path): TaskRunner::cleanupOnCompleted() only runs on the
 * `completed` phase transition, so a failed backup's whole
 * `wpmgr-agent/runs/<snapshot_id>/` scratch directory (DB dump, zip parts,
 * chunks) is never reclaimed. BackupJanitor is the age-gated GC backstop
 * that reclaims it.
 *
 * All filesystem operations use an isolated temp directory per test.
 * BackupJanitor::runsBaseDir() (protected) is overridden in an anonymous
 * subclass so every test here is independent of ambient WP_CONTENT_DIR state
 * other test files in this suite may have already frozen as a global PHP
 * constant (constants cannot be redefined once set). `$GLOBALS['wpdb']` is a
 * minimal, self-contained fake built fresh per test (no shared Patchwork
 * redefinition) — it only implements the exact surface isActiveRun() touches
 * (prepare()+get_row()).
 *
 * Covers:
 *   - A stale run dir (past RUNS_GC_AGE_SECONDS) with no `wpmgr_backup_tasks`
 *     row is swept — the just-failed-backup leak this class exists to fix.
 *   - A stale run dir whose task row reports a live, recently-progressing
 *     phase is NOT swept, even though it is past the age threshold — the
 *     task-row veto is a belt-and-suspenders guard layered on top of, never
 *     a substitute for, the age gate.
 *   - A fresh run dir (under the age threshold) is kept regardless of the
 *     task row.
 *   - A non-run entry (neither a UUID-shaped file nor directory) is never
 *     touched by the sweep.
 *   - scheduleGc() is idempotent: an already-scheduled event is left alone.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Backup\BackupJanitor;
use WPMgr\Agent\Backup\TaskRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\BackupJanitor
 */
final class BackupJanitorTest extends TestCase
{
    /** Temp root for this test run (removed in tear_down). */
    private string $root = '';

    /** Simulated wp-content/wpmgr-agent/runs dir. */
    private string $runsBase = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root     = sys_get_temp_dir() . '/wpmgr-janitor-test-' . bin2hex(random_bytes(6));
        $this->runsBase = $this->root . '/wpmgr-agent/runs';
        mkdir($this->runsBase, 0755, true);

        Functions\when('wp_delete_file')->alias(static function ($f) {
            return @unlink($f); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
        });
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->root);
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    // =========================================================================
    // Test doubles / helpers
    // =========================================================================

    /**
     * A BackupJanitor subclass whose runsBaseDir() is fully test-controlled
     * (never touches WP_CONTENT_DIR), exercised via `new static()` late
     * static binding inside gcRuns() itself — the same mechanism
     * SnapshotManagerTest uses for gcExpired().
     *
     * @return class-string<BackupJanitor>
     */
    private function janitorClassWithBase(string $base): string
    {
        $class = new class extends BackupJanitor {
            public static string $testBase = '';

            protected function runsBaseDir(): string
            {
                return self::$testBase;
            }
        };
        $class::$testBase = $base;

        return get_class($class);
    }

    /**
     * Minimal in-memory $wpdb double supporting only the surface
     * BackupJanitor::isActiveRun() touches: prepare() (passthrough envelope)
     * + get_row() (returns the configured canned row, or null for "no row").
     *
     * @param array{phase:string,last_progress_at:int}|null $row
     */
    private function fakeWpdb(?array $row): object
    {
        return new class($row) {
            public string $prefix = 'wp_';

            /** @var array{phase:string,last_progress_at:int}|null */
            private ?array $row;

            /**
             * @param array{phase:string,last_progress_at:int}|null $row
             */
            public function __construct(?array $row)
            {
                $this->row = $row;
            }

            /**
             * @param mixed ...$args
             */
            public function prepare(string $query, ...$args): string
            {
                return (string) json_encode(['sql' => $query, 'args' => $args]);
            }

            /**
             * @param mixed $mode
             * @return array<string,mixed>|null
             */
            public function get_row(string $prepared, $mode = null): ?array
            {
                return $this->row;
            }
        };
    }

    /** Seed a run scratch dir with a representative artifact file. */
    private function seedRunDir(string $id): string
    {
        $dir = $this->runsBase . '/' . $id;
        mkdir($dir, 0700, true);
        file_put_contents($dir . '/database.sql.gz', 'fixture-content');

        return $dir;
    }

    /**
     * Seed a bare `<snapshot_id>.lock` coordination file (TaskRunner's
     * flock() guard — see class-task-runner.php's fileLockPath()) directly
     * in the runs base, alongside (never inside) any run directory.
     */
    private function seedLockFile(string $id): string
    {
        $file = $this->runsBase . '/' . $id . '.lock';
        touch($file);

        return $file;
    }

    /** A 36-character UUID-shaped snapshot id matching BackupJanitor's run-id regex. */
    private function newSnapshotId(): string
    {
        return sprintf(
            '%08x-%04x-%04x-%04x-%012x',
            random_int(0, 0xffffffff),
            random_int(0, 0xffff),
            random_int(0, 0xffff),
            random_int(0, 0xffff),
            random_int(0, 0xffffffffffff)
        );
    }

    /** Recursive delete used only for test fixture cleanup. */
    private function rrmdir(string $dir): void
    {
        if (!is_dir($dir)) {
            return;
        }
        $items = @scandir($dir);
        if ($items === false) {
            return;
        }
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $path = $dir . '/' . $item;
            if (is_dir($path) && !is_link($path)) {
                $this->rrmdir($path);
            } else {
                @unlink($path); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
            }
        }
        @rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- test-only fixture cleanup
    }

    // =========================================================================
    // gcRuns()
    // =========================================================================

    public function test_stale_run_with_no_task_row_is_swept(): void
    {
        $id  = $this->newSnapshotId();
        $dir = $this->seedRunDir($id);
        touch($dir, time() - (7 * 3600)); // past RUNS_GC_AGE_SECONDS (6h)
        clearstatcache(true, $dir);

        // No wpmgr_backup_tasks row — simulates the run having just FAILED
        // (TaskRunner's failure path DELETEs the row without cleaning
        // scratch) or having completed and already been cleaned up. Either
        // way, a missing row must never be the reason for deletion by
        // itself — but it must never BLOCK deletion once the age gate says
        // this directory is old enough to consider.
        $GLOBALS['wpdb'] = $this->fakeWpdb(null);

        $class = $this->janitorClassWithBase($this->runsBase);
        $class::gcRuns();

        $this->assertFalse(
            is_dir($dir),
            'a run dir past RUNS_GC_AGE_SECONDS with no task row must be swept — this is the #151 failed-backup scratch leak'
        );
    }

    public function test_active_run_is_not_swept_even_when_old(): void
    {
        $id  = $this->newSnapshotId();
        $dir = $this->seedRunDir($id);
        touch($dir, time() - (7 * 3600)); // past the age gate
        clearstatcache(true, $dir);

        // A live task row: non-terminal phase, progress posted just now —
        // this is the belt-and-suspenders veto that must override the age
        // gate.
        $GLOBALS['wpdb'] = $this->fakeWpdb([
            'phase'            => TaskRunner::PHASE_ARCHIVING_FILES,
            'last_progress_at' => time(),
        ]);

        $class = $this->janitorClassWithBase($this->runsBase);
        $class::gcRuns();

        $this->assertTrue(
            is_dir($dir),
            'a task row reporting an active, recently-progressing phase must veto the sweep regardless of directory age'
        );
    }

    public function test_fresh_dir_under_the_age_threshold_is_kept(): void
    {
        $id  = $this->newSnapshotId();
        $dir = $this->seedRunDir($id);
        touch($dir, time());
        clearstatcache(true, $dir);

        $GLOBALS['wpdb'] = $this->fakeWpdb(null);

        $class = $this->janitorClassWithBase($this->runsBase);
        $class::gcRuns();

        $this->assertTrue(
            is_dir($dir),
            'a run dir younger than RUNS_GC_AGE_SECONDS must never be swept, task row or not'
        );
    }

    public function test_non_run_entries_are_ignored(): void
    {
        file_put_contents($this->runsBase . '/notauuid', 'not a run directory');
        mkdir($this->runsBase . '/restores', 0755, true);
        touch($this->runsBase . '/notauuid', time() - (7 * 3600));
        touch($this->runsBase . '/restores', time() - (7 * 3600));
        clearstatcache(true, $this->runsBase);

        $GLOBALS['wpdb'] = $this->fakeWpdb(null);

        $class = $this->janitorClassWithBase($this->runsBase);
        $class::gcRuns();

        $this->assertTrue(
            is_file($this->runsBase . '/notauuid'),
            'an entry that does not match the strict UUID run-id pattern must never be touched by the sweep'
        );
        $this->assertTrue(
            is_dir($this->runsBase . '/restores'),
            'a sibling non-run directory (e.g. restores/) under the same base must never be touched'
        );
    }

    // =========================================================================
    // gcRuns() — GH #232 follow-up: sweeping <snapshot_id>.lock FILES.
    // =========================================================================

    public function test_aged_orphan_lock_file_is_swept(): void
    {
        $id   = $this->newSnapshotId();
        $file = $this->seedLockFile($id);
        touch($file, time() - (7 * 3600)); // past RUNS_GC_AGE_SECONDS (6h)
        clearstatcache(true, $file);

        // No wpmgr_backup_tasks row — the run this lock file belonged to
        // crashed before reaching its own `finally` cleanup (fatal/OOM/
        // SIGKILL), or completed/failed and its row is already gone.
        $GLOBALS['wpdb'] = $this->fakeWpdb(null);

        $class = $this->janitorClassWithBase($this->runsBase);
        $class::gcRuns();

        $this->assertFalse(
            is_file($file),
            'an aged orphan <snapshot_id>.lock file left by a crashed run must be swept — the same leak class as #151, for the sibling lock-file artifact'
        );
    }

    public function test_active_runs_lock_file_is_not_swept_even_when_old(): void
    {
        $id   = $this->newSnapshotId();
        $file = $this->seedLockFile($id);
        touch($file, time() - (7 * 3600)); // past the age gate
        clearstatcache(true, $file);

        // A live task row: non-terminal phase, progress posted just now —
        // the same belt-and-suspenders veto the directory sweep uses.
        $GLOBALS['wpdb'] = $this->fakeWpdb([
            'phase'            => TaskRunner::PHASE_ARCHIVING_FILES,
            'last_progress_at' => time(),
        ]);

        $class = $this->janitorClassWithBase($this->runsBase);
        $class::gcRuns();

        $this->assertTrue(
            is_file($file),
            'a lock file whose task row reports an active, recently-progressing phase must be spared regardless of age'
        );
    }

    public function test_fresh_lock_file_under_the_age_threshold_is_kept(): void
    {
        $id   = $this->newSnapshotId();
        $file = $this->seedLockFile($id);
        touch($file, time());
        clearstatcache(true, $file);

        $GLOBALS['wpdb'] = $this->fakeWpdb(null);

        $class = $this->janitorClassWithBase($this->runsBase);
        $class::gcRuns();

        $this->assertTrue(
            is_file($file),
            'a lock file younger than RUNS_GC_AGE_SECONDS must never be swept, task row or not'
        );
    }

    // =========================================================================
    // scheduleGc()
    // =========================================================================

    public function test_schedule_gc_is_idempotent(): void
    {
        Functions\when('wp_next_scheduled')->justReturn(time() + 3600);
        Functions\expect('wp_schedule_event')->never();

        BackupJanitor::scheduleGc(time());

        // Brain Monkey verifies the ->never() expectation at teardown; this
        // explicit count satisfies PHPUnit's "at least one assertion" check.
        $this->addToAssertionCount(1);
    }
}
