<?php
/**
 * GitHub issue #256 post-ship review, findings 1/2/3: a Watchdog give-up
 * path (the 7200s hard-ceiling guard, the late-phase 300s stale guard, the
 * max-resumes exhaustion, and dispatch()'s stale-task guard) reclaims a
 * snapshot's scratch directory purely from DB-column signals (row age,
 * last_progress_at age, resume_count). None of those signals can prove the
 * runner that owns the snapshot has actually stopped: a real backup of a
 * large site can legitimately run for hours, and a single large chunk
 * upload on a slow link can legitimately go quiet for minutes at a time
 * (see WatchdogReclaimSubstateSidecarTest for the sidecar-spill half of
 * this review).
 *
 * Before this fix, none of the four give-up paths took any lock at all
 * before deleting scratch files, so a genuinely live runner racing a
 * give-up path (holding TaskRunner::run()'s own advisory lock while this
 * class independently decided the row was dead) could have its in-flight
 * chunk/artifact files deleted out from under it.
 *
 * This suite proves the fix by simulating a live runner: it opens and
 * flock()s the exact lock-file path TaskRunner::run() itself would use for
 * a given snapshot (see TaskRunner::fileLockPath()) BEFORE invoking a
 * Watchdog give-up path, then asserts the scratch directory survives.
 * PHP's flock() contends across two separate file handles to the same path
 * even within a single process/test, which is exactly what makes this a
 * faithful, deterministic simulation without spinning a second process.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Backup\TaskRunner;
use WPMgr\Agent\Backup\Watchdog;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\Watchdog
 * @covers \WPMgr\Agent\Backup\TaskRunner
 */
final class WatchdogReclaimRunLockTest extends TestCase
{
    private const HARD_CEILING_ID   = '55555555-6666-4777-8888-999999999999';
    private const LATE_PHASE_ID     = '66666666-7777-4888-8999-aaaaaaaaaaaa';
    private const MAX_RESUMES_ID    = '77777777-8888-4999-8aaa-bbbbbbbbbbbb';
    private const DISPATCH_STALE_ID = '88888888-9999-4aaa-8bbb-cccccccccccc';

    private string $root       = '';
    private string $scratchDir = '';

    /** @var resource|null */
    private $liveRunnerLockHandle = null;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root       = sys_get_temp_dir() . '/wpmgr-reclaim-lock-test-' . bin2hex(random_bytes(6));
        $this->scratchDir = $this->root . '/scratch';
        mkdir($this->scratchDir, 0755, true);

        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
        Functions\when('wp_schedule_single_event')->justReturn(true);
        Functions\when('wp_delete_file')->alias(static function ($f) {
            return @unlink($f); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
        });
    }

    protected function tear_down(): void
    {
        if (is_resource($this->liveRunnerLockHandle)) {
            flock($this->liveRunnerLockHandle, LOCK_UN);
            fclose($this->liveRunnerLockHandle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- test-only fixture cleanup
            $this->liveRunnerLockHandle = null;
        }
        unset($GLOBALS['wpdb']);
        $this->rrmdir($this->root);
        Monkey\tearDown();
        parent::tear_down();
    }

    /** @return array<string,mixed> Minimal-but-valid TaskRunner params. */
    private function runnerParams(string $snapshotId): array
    {
        return [
            'snapshot_id'       => $snapshotId,
            'kind'              => 'files',
            'age_recipient'     => 'age1' . str_repeat('q', 58),
            'presign_endpoint'  => 'https://cp.invalid/agent/v1/backups/reclaim-lock-test/presign',
            'manifest_endpoint' => 'https://cp.invalid/agent/v1/backups/reclaim-lock-test/manifest',
            'progress_endpoint' => '',
            'chunk_bytes'       => 4 * 1024 * 1024,
            'scratch_dir'       => $this->scratchDir . '/' . $snapshotId,
            'wp_content_path'   => $this->root . '/wp-content',
            'db'                => ['host' => 'localhost', 'user' => 'u', 'password' => 'p', 'name' => 'n', 'prefix' => 'wp_'],
        ];
    }

    /** Seed a run's scratch dir with a representative artifact file. */
    private function seedScratch(string $snapshotId): string
    {
        $dir = $this->scratchDir . '/' . $snapshotId;
        mkdir($dir, 0700, true);
        file_put_contents($dir . '/database.sql.gz', 'fixture-bytes');
        file_put_contents($dir . '/wp-content.g000.part001.zip', 'fixture-zip-bytes');

        return $dir;
    }

    /**
     * Hold the exact lock file TaskRunner::run() would hold for this
     * snapshot, simulating a live runner still in flight. The lock path is
     * derived from the PARENT of the scratch dir (see
     * TaskRunner::fileLockPath()), which in this fixture is $this->scratchDir
     * itself, shared by every snapshot under test.
     */
    private function simulateLiveRunnerHoldingLock(string $snapshotId): void
    {
        $lockPath = $this->scratchDir . DIRECTORY_SEPARATOR . $snapshotId . '.lock';
        $handle   = fopen($lockPath, 'c'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- test-only fixture simulating a live runner's own flock handle
        $this->assertNotFalse($handle, 'test fixture must be able to open the lock file');
        $this->assertTrue(flock($handle, LOCK_EX | LOCK_NB), 'test fixture must be able to acquire the lock before the code under test attempts it');
        $this->liveRunnerLockHandle = $handle;
    }

    // ------------------------------------------------------------------
    // resumeIfStalled(): 7200s hard-ceiling guard.
    // ------------------------------------------------------------------

    /**
     * FAILS against the pre-fix code: before GH #256 finding 3, none of the
     * four give-up paths took any lock at all, so reclaimRunScratch() would
     * delete the scratch directory even while a live runner (simulated here
     * by holding the exact same lock file TaskRunner::run() would hold)
     * still owns it.
     */
    public function test_hard_ceiling_guard_does_not_delete_scratch_while_a_live_runner_holds_the_lock(): void
    {
        $dir = $this->seedScratch(self::HARD_CEILING_ID);
        $this->simulateLiveRunnerHoldingLock(self::HARD_CEILING_ID);

        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->seedRow(self::HARD_CEILING_ID, [
            'phase'            => TaskRunner::PHASE_ARCHIVING_FILES,
            'started_at'       => time() - 7300, // past the 7200s hard ceiling.
            'last_progress_at' => time() - 7300,
            'sub_state'        => (string) json_encode(['params' => $this->runnerParams(self::HARD_CEILING_ID)]),
        ]);
        $GLOBALS['wpdb'] = $wpdb;

        Watchdog::resumeIfStalled(self::HARD_CEILING_ID);

        $this->assertDirectoryExists($dir, 'a live runner still holds the run-lock; its scratch directory must NOT be deleted out from under it');
        $this->assertFileExists($dir . '/database.sql.gz', 'the live runner\'s in-flight artifact file must survive the give-up path');
        $this->assertArrayNotHasKey(self::HARD_CEILING_ID, $wpdb->rows, 'the task row discard is unaffected by the lock gate: only the file reclaim is gated');
    }

    // ------------------------------------------------------------------
    // resumeIfStalled(): late-phase (encrypting_uploading /
    // submitting_manifest) stale guard.
    // ------------------------------------------------------------------

    /**
     * GH #256 finding 4: the 300s late-phase threshold can legitimately fire
     * while a single large chunk upload is still in flight on a slow link.
     * This is the scenario that guard's threshold alone cannot distinguish
     * from a genuinely dead runner; the run-lock probe is what makes the
     * file reclaim safe regardless of the threshold value.
     */
    public function test_late_phase_guard_does_not_delete_scratch_while_a_live_runner_holds_the_lock(): void
    {
        $dir = $this->seedScratch(self::LATE_PHASE_ID);
        $this->simulateLiveRunnerHoldingLock(self::LATE_PHASE_ID);

        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->seedRow(self::LATE_PHASE_ID, [
            'phase'            => TaskRunner::PHASE_ENCRYPTING_UPLOADING,
            'started_at'       => time() - 600,
            'last_progress_at' => time() - 400, // past the 300s late-phase threshold.
            'sub_state'        => (string) json_encode(['params' => $this->runnerParams(self::LATE_PHASE_ID)]),
        ]);
        $GLOBALS['wpdb'] = $wpdb;

        Watchdog::resumeIfStalled(self::LATE_PHASE_ID);

        $this->assertDirectoryExists($dir, 'a live runner still holds the run-lock (e.g. mid-upload of one large chunk); its scratch directory must NOT be deleted');
        $this->assertFileExists($dir . '/wp-content.g000.part001.zip', 'the live runner\'s in-flight archive part must survive the give-up path');
        $this->assertArrayNotHasKey(self::LATE_PHASE_ID, $wpdb->rows, 'the task row discard is unaffected by the lock gate: only the file reclaim is gated');
    }

    // ------------------------------------------------------------------
    // resumeIfStalled(): max-resumes exhaustion guard.
    // ------------------------------------------------------------------

    /**
     * FAILS against the pre-fix code for the same reason as the two guards
     * above: none of the four give-up paths took any lock before this fix,
     * including this one (reached only after $resumeCount >= $maxResumes
     * separate stall detections).
     */
    public function test_max_resumes_exhaustion_guard_does_not_delete_scratch_while_a_live_runner_holds_the_lock(): void
    {
        $dir = $this->seedScratch(self::MAX_RESUMES_ID);
        $this->simulateLiveRunnerHoldingLock(self::MAX_RESUMES_ID);

        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->seedRow(self::MAX_RESUMES_ID, [
            'phase'            => TaskRunner::PHASE_ARCHIVING_FILES,
            'started_at'       => time() - 600,  // well under the 7200s hard ceiling.
            'last_progress_at' => time() - 200,  // past STALL_THRESHOLD_SECONDS (180s).
            'resume_count'     => 6,
            'max_resumes'      => 6,
            'sub_state'        => (string) json_encode(['params' => $this->runnerParams(self::MAX_RESUMES_ID)]),
        ]);
        $GLOBALS['wpdb'] = $wpdb;

        Watchdog::resumeIfStalled(self::MAX_RESUMES_ID);

        $this->assertDirectoryExists($dir, 'a live runner still holds the run-lock; its scratch directory must NOT be deleted even after resume attempts are exhausted');
        $this->assertFileExists($dir . '/database.sql.gz', 'the live runner\'s in-flight artifact file must survive the give-up path');
        $this->assertArrayNotHasKey(self::MAX_RESUMES_ID, $wpdb->rows, 'the task row discard is unaffected by the lock gate: only the file reclaim is gated');
    }

    // ------------------------------------------------------------------
    // dispatch(): 7200s stale-task guard.
    // ------------------------------------------------------------------

    /**
     * FAILS against the pre-fix code for the same reason as the other three
     * give-up paths: dispatch()'s own stale-task guard took no lock before
     * this fix either.
     */
    public function test_dispatch_stale_task_guard_does_not_delete_scratch_while_a_live_runner_holds_the_lock(): void
    {
        $dir = $this->seedScratch(self::DISPATCH_STALE_ID);
        $this->simulateLiveRunnerHoldingLock(self::DISPATCH_STALE_ID);

        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->seedRow(self::DISPATCH_STALE_ID, [
            'phase'            => TaskRunner::PHASE_ARCHIVING_FILES,
            'started_at'       => time() - 7300, // past dispatch()'s own 7200s guard.
            'last_progress_at' => time() - 7300,
            'sub_state'        => (string) json_encode(['params' => $this->runnerParams(self::DISPATCH_STALE_ID)]),
        ]);
        $GLOBALS['wpdb'] = $wpdb;

        Watchdog::dispatch(self::DISPATCH_STALE_ID);

        $this->assertDirectoryExists($dir, 'a live runner still holds the run-lock; its scratch directory must NOT be deleted by dispatch()\'s stale-task guard');
        $this->assertFileExists($dir . '/wp-content.g000.part001.zip', 'the live runner\'s in-flight archive part must survive the give-up path');
        $this->assertArrayNotHasKey(self::DISPATCH_STALE_ID, $wpdb->rows, 'the task row discard is unaffected by the lock gate: only the file reclaim is gated');
    }

    // ------------------------------------------------------------------
    // Once the lock is released, the SAME give-up decision reclaims safely
    // (this is the pre-existing, already-covered behavior in
    // WatchdogScratchReclaimTest.php; asserted once more here, in the same
    // fixture shape, purely to show the gate is a probe-and-release, not a
    // permanent block).
    // ------------------------------------------------------------------

    public function test_reclaim_proceeds_once_the_run_lock_is_released(): void
    {
        $dir = $this->seedScratch(self::HARD_CEILING_ID);
        $this->simulateLiveRunnerHoldingLock(self::HARD_CEILING_ID);

        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->seedRow(self::HARD_CEILING_ID, [
            'phase'            => TaskRunner::PHASE_ARCHIVING_FILES,
            'started_at'       => time() - 7300,
            'last_progress_at' => time() - 7300,
            'sub_state'        => (string) json_encode(['params' => $this->runnerParams(self::HARD_CEILING_ID)]),
        ]);
        $GLOBALS['wpdb'] = $wpdb;

        // First attempt: lock held, files must survive; row is still deleted.
        Watchdog::resumeIfStalled(self::HARD_CEILING_ID);
        $this->assertDirectoryExists($dir);
        $this->assertArrayNotHasKey(self::HARD_CEILING_ID, $wpdb->rows);

        // The live runner finishes (or crashes) and releases its lock.
        flock($this->liveRunnerLockHandle, LOCK_UN);
        fclose($this->liveRunnerLockHandle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- test-only fixture cleanup
        $this->liveRunnerLockHandle = null;

        // A later reaper tick discovers the same still-orphaned row (a fresh
        // seed here stands in for whatever re-discovers it, e.g. a stale
        // BackupJanitor sweep candidate) with the lock now free.
        $wpdb->seedRow(self::HARD_CEILING_ID, [
            'phase'            => TaskRunner::PHASE_ARCHIVING_FILES,
            'started_at'       => time() - 7300,
            'last_progress_at' => time() - 7300,
            'sub_state'        => (string) json_encode(['params' => $this->runnerParams(self::HARD_CEILING_ID)]),
        ]);

        Watchdog::resumeIfStalled(self::HARD_CEILING_ID);

        $this->assertDirectoryDoesNotExist($dir, 'with the lock free, the give-up path must reclaim the scratch directory exactly as before this fix');
        $this->assertArrayNotHasKey(self::HARD_CEILING_ID, $wpdb->rows);
    }

    /** Recursive delete used only for test fixture cleanup. */
    private function rrmdir(string $dir): void
    {
        if ($dir === '' || !is_dir($dir)) {
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
}
