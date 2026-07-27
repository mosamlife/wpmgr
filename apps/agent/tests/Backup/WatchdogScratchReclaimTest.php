<?php
/**
 * GitHub issue #256 regression: every Watchdog path that gives up on a
 * `wpmgr_backup_tasks` row (discarding it via DELETE, or, pre-#256, an
 * UPDATE to phase=failed) must reclaim that snapshot's scratch directory
 * FIRST, since the row is the only record of where that scratch lives.
 * Before this fix, none of these paths touched the filesystem at all, so a
 * failed/abandoned backup's entire `wpmgr-agent/runs/<snapshot_id>/`
 * directory (DB dump, zip parts, encrypted chunk files) leaked until the
 * age-gated BackupJanitor::gcRuns() backstop eventually swept it hours
 * later.
 *
 * Covers the four give-up paths named in GH #256:
 *   - resumeIfStalled()'s 7200s hard-ceiling guard.
 *   - resumeIfStalled()'s late-phase (encrypting_uploading /
 *     submitting_manifest) stale guard: the LARGEST leak, since every zip
 *     part and encrypted chunk is already on disk by that phase.
 *   - dispatch()'s stale-task guard.
 *   - The max-resumes exhaustion branch is covered separately in
 *     WatchdogReaperTest (it changed from "mark failed" to "reclaim +
 *     delete", which is a behavior change worth keeping alongside that
 *     branch's existing coverage).
 *
 * Also covers the defensive no-op: a row whose sub_state.params (or
 * scratch_dir) is missing/corrupt must never error; reclaiming scratch is
 * best-effort, exactly like every other cleanup path in this class.
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
final class WatchdogScratchReclaimTest extends TestCase
{
    private const HARD_CEILING_ID    = '11111111-2222-4333-8444-555555555555';
    private const LATE_PHASE_ID      = '22222222-3333-4444-8555-666666666666';
    private const DISPATCH_STALE_ID  = '33333333-4444-4555-8666-777777777777';
    private const NO_PARAMS_ID       = '44444444-5555-4666-8777-888888888888';

    private string $root       = '';
    private string $scratchDir = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root       = sys_get_temp_dir() . '/wpmgr-reclaim-test-' . bin2hex(random_bytes(6));
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
            'presign_endpoint'  => 'https://cp.invalid/agent/v1/backups/reclaim-test/presign',
            'manifest_endpoint' => 'https://cp.invalid/agent/v1/backups/reclaim-test/manifest',
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

    // ------------------------------------------------------------------
    // resumeIfStalled(): 7200s hard-ceiling guard.
    // ------------------------------------------------------------------

    public function test_hard_ceiling_guard_reclaims_scratch_before_deleting_row(): void
    {
        $dir = $this->seedScratch(self::HARD_CEILING_ID);

        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->seedRow(self::HARD_CEILING_ID, [
            'phase'            => TaskRunner::PHASE_ARCHIVING_FILES,
            'started_at'       => time() - 7300, // past the 7200s hard ceiling.
            'last_progress_at' => time() - 7300,
            'sub_state'        => (string) json_encode(['params' => $this->runnerParams(self::HARD_CEILING_ID)]),
        ]);
        $GLOBALS['wpdb'] = $wpdb;

        Watchdog::resumeIfStalled(self::HARD_CEILING_ID);

        $this->assertDirectoryDoesNotExist($dir, 'the 7200s hard-ceiling guard must reclaim scratch before deleting the row');
        $this->assertArrayNotHasKey(self::HARD_CEILING_ID, $wpdb->rows, 'the row must still be deleted, exactly as before this fix');
    }

    // ------------------------------------------------------------------
    // resumeIfStalled(): late-phase (encrypting_uploading /
    // submitting_manifest) stale guard: the LARGEST leak.
    // ------------------------------------------------------------------

    public function test_late_phase_stale_guard_reclaims_scratch_before_deleting_row(): void
    {
        $dir = $this->seedScratch(self::LATE_PHASE_ID);

        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->seedRow(self::LATE_PHASE_ID, [
            'phase'            => TaskRunner::PHASE_ENCRYPTING_UPLOADING,
            'started_at'       => time() - 600,
            'last_progress_at' => time() - 400, // past the 300s late-phase threshold.
            'sub_state'        => (string) json_encode(['params' => $this->runnerParams(self::LATE_PHASE_ID)]),
        ]);
        $GLOBALS['wpdb'] = $wpdb;

        Watchdog::resumeIfStalled(self::LATE_PHASE_ID);

        $this->assertDirectoryDoesNotExist($dir, 'the late-phase stale guard must reclaim scratch: this is the reporter\'s exact screenshot shape and the largest leak (zip parts + chunks already on disk)');
        $this->assertArrayNotHasKey(self::LATE_PHASE_ID, $wpdb->rows, 'the row must still be deleted, exactly as before this fix');
    }

    // ------------------------------------------------------------------
    // dispatch(): stale-task guard.
    // ------------------------------------------------------------------

    public function test_dispatch_stale_task_guard_reclaims_scratch_before_deleting_row(): void
    {
        $dir = $this->seedScratch(self::DISPATCH_STALE_ID);

        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->seedRow(self::DISPATCH_STALE_ID, [
            'phase'            => TaskRunner::PHASE_ARCHIVING_FILES, // past `queued`.
            'started_at'       => time() - 7300, // past the 7200s ceiling.
            'last_progress_at' => time() - 7300,
            'sub_state'        => (string) json_encode(['params' => $this->runnerParams(self::DISPATCH_STALE_ID)]),
        ]);
        $GLOBALS['wpdb'] = $wpdb;

        Watchdog::dispatch(self::DISPATCH_STALE_ID);

        $this->assertDirectoryDoesNotExist($dir, 'dispatch()\'s stale-task guard must reclaim scratch before deleting the row');
        $this->assertArrayNotHasKey(self::DISPATCH_STALE_ID, $wpdb->rows, 'the row must still be deleted, exactly as before this fix');
    }

    // ------------------------------------------------------------------
    // Defensive: missing/corrupt sub_state.params must never error.
    // ------------------------------------------------------------------

    public function test_reclaim_is_a_silent_no_op_when_params_are_missing(): void
    {
        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->seedRow(self::NO_PARAMS_ID, [
            'phase'            => TaskRunner::PHASE_ARCHIVING_FILES,
            'started_at'       => time() - 7300,
            'last_progress_at' => time() - 7300,
            'sub_state'        => '{}', // no 'params' key at all.
        ]);
        $GLOBALS['wpdb'] = $wpdb;

        // Must not throw.
        Watchdog::resumeIfStalled(self::NO_PARAMS_ID);

        $this->assertArrayNotHasKey(self::NO_PARAMS_ID, $wpdb->rows, 'the row must still be deleted by the hard-ceiling guard even when scratch cannot be resolved');
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
