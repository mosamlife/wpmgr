<?php
/**
 * GitHub issue #232 regression: Watchdog's WP-Cron-INDEPENDENT reaper
 * (sweepStalled() / resumeIfStalled()) and the dispatch_entered breadcrumb.
 *
 * Root cause recap: the ONLY pre-existing recovery path for a stalled
 * `wpmgr_backup_tasks` row was another single-shot WP-Cron event
 * (`wpmgr_backup_watchdog`) — just as WP-Cron-dependent as the original
 * dispatch, so a site with no qualifying traffic (or DISABLE_WP_CRON) could
 * never recover a stuck row. sweepStalled() is a WP-Cron-INDEPENDENT
 * reaper, driven from ordinary request traffic via
 * Plugin::maybeSweepStalledBackups() (see PluginBackupSweepTest.php),
 * that finds stale non-terminal rows via the existing `KEY phase` +
 * `KEY last_progress_at` indexes and resumes each one through
 * resumeIfStalled() — the exact same body run() (the WP-Cron callback) used
 * before, now reusable.
 *
 * Covers:
 *   3. Stale queued re-dispatch: sweepStalled() bumps resume_count and
 *      enters TaskRunner (phase advances past queued); a fresh row is left
 *      untouched.
 *   5. Breadcrumb distinguishability: dispatch()'s dispatch_entered stamp.
 *   8. resume_count cap preserved: a row already at max_resumes is marked
 *      failed, not resumed again.
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
 */
final class WatchdogReaperTest extends TestCase
{
    private const STALE_ID = 'bbbbbbbb-cccc-4ddd-8eee-ffffffffffff';
    private const FRESH_ID = 'cccccccc-dddd-4eee-8fff-000000000000';
    private const CAPPED_ID = 'dddddddd-eeee-4fff-8000-111111111111';

    private string $root       = '';
    private string $scratchDir = '';
    private string $wpContent  = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        if (!class_exists(\ZipArchive::class)) {
            self::markTestSkipped('ext-zip not available');
        }

        $this->root       = sys_get_temp_dir() . '/wpmgr-reaper-test-' . bin2hex(random_bytes(6));
        $this->scratchDir = $this->root . '/scratch';
        $this->wpContent  = $this->root . '/wp-content';
        mkdir($this->scratchDir, 0755, true);
        mkdir($this->wpContent, 0755, true);
        file_put_contents($this->wpContent . '/marker.txt', 'a real file for FilesArchiver to pack');

        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
        Functions\when('wp_schedule_single_event')->justReturn(true);
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        $this->rrmdir($this->root);
        Monkey\tearDown();
        parent::tear_down();
    }

    /** @return array<string,mixed> Minimal-but-valid TaskRunner params for kind=files. */
    private function runnerParams(string $snapshotId): array
    {
        return [
            'snapshot_id'       => $snapshotId,
            'kind'              => 'files',
            // Deliberately NOT a valid age recipient — this test only needs
            // the phase machine to LEAVE `queued`, not to complete; the
            // eventual failure (and its row DELETE) happens well after the
            // phase-advancement assertions below are captured via update()
            // history, so a real AgeCrypto identity + network stubs are
            // unnecessary overhead here.
            'age_recipient'     => 'age1' . str_repeat('q', 58),
            'presign_endpoint'  => 'https://cp.invalid/agent/v1/backups/reaper-test/presign',
            'manifest_endpoint' => 'https://cp.invalid/agent/v1/backups/reaper-test/manifest',
            'progress_endpoint' => '',
            'chunk_bytes'       => 4 * 1024 * 1024,
            'scratch_dir'       => $this->scratchDir . '/' . $snapshotId,
            'wp_content_path'   => $this->wpContent,
            'db'                => ['host' => 'localhost', 'user' => 'u', 'password' => 'p', 'name' => 'n', 'prefix' => 'wp_'],
        ];
    }

    /** Every update() call recorded for a given snapshot id, oldest first. */
    private function updatesFor(FakeBackupTasksWpdb $wpdb, string $snapshotId): array
    {
        return array_values(array_filter(
            $wpdb->updates,
            static fn (array $u): bool => ($u['where']['snapshot_id'] ?? null) === $snapshotId
        ));
    }

    // ------------------------------------------------------------------
    // Test 3: stale queued re-dispatch (sweepStalled + resumeIfStalled).
    // ------------------------------------------------------------------

    public function test_sweep_stalled_resumes_stale_row_and_leaves_fresh_row_untouched(): void
    {
        $wpdb = new FakeBackupTasksWpdb();

        // Stale: last_progress_at 200s ago (past STALL_THRESHOLD_SECONDS=180),
        // resume_count=0, valid sub_state.params.
        $wpdb->seedRow(self::STALE_ID, [
            'phase'            => TaskRunner::PHASE_QUEUED,
            'started_at'       => time() - 200,
            'last_progress_at' => time() - 200,
            'resume_count'     => 0,
            'sub_state'        => (string) json_encode(['params' => $this->runnerParams(self::STALE_ID)]),
        ]);

        // Fresh: last_progress_at just now — must NOT be touched.
        $wpdb->seedRow(self::FRESH_ID, [
            'phase'            => TaskRunner::PHASE_QUEUED,
            'started_at'       => time(),
            'last_progress_at' => time(),
            'resume_count'     => 0,
            'sub_state'        => (string) json_encode(['params' => $this->runnerParams(self::FRESH_ID)]),
        ]);

        $GLOBALS['wpdb'] = $wpdb;

        Watchdog::sweepStalled();

        // The stale row's resume_count must have been bumped to 1 (recorded
        // via update() history — the row itself may since have advanced past
        // queued, or even been deleted on eventual failure deep in the
        // pipeline, since the recipient is a shape-only fake).
        $staleUpdates = $this->updatesFor($wpdb, self::STALE_ID);
        $this->assertNotEmpty($staleUpdates, 'sweepStalled() must have resumed the stale row');
        $resumeBump = null;
        foreach ($staleUpdates as $u) {
            if (array_key_exists('resume_count', $u['data'])) {
                $resumeBump = $u['data']['resume_count'];
                break;
            }
        }
        $this->assertSame(1, $resumeBump, 'the first resume must bump resume_count from 0 to 1');

        // Phase must have advanced past `queued` at some point (proof the
        // resumed TaskRunner actually entered the phase machine, not just a
        // resume_count bump with no re-entry).
        $advancedPastQueued = false;
        foreach ($staleUpdates as $u) {
            if (($u['data']['phase'] ?? null) === TaskRunner::PHASE_ARCHIVING_FILES) {
                $advancedPastQueued = true;
                break;
            }
        }
        $this->assertTrue($advancedPastQueued, 'the resumed runner must advance phase past queued');

        // The fresh row must be completely untouched: no updates recorded.
        $this->assertEmpty($this->updatesFor($wpdb, self::FRESH_ID), 'a fresh (non-stale) row must never be resumed');
        $freshRow = $wpdb->rows[self::FRESH_ID] ?? null;
        $this->assertNotNull($freshRow);
        $this->assertSame(TaskRunner::PHASE_QUEUED, $freshRow['phase']);
        $this->assertSame(0, $freshRow['resume_count']);
    }

    // ------------------------------------------------------------------
    // Test 5: breadcrumb distinguishability — dispatch_entered.
    //
    // (The "no stage marker on a freshly seeded row" rung of the ladder is a
    // property of FakeBackupTasksWpdb::seedRow() itself, not of any
    // production code path — asserting it here would be a fixture tautology,
    // not a regression lock, so it is intentionally not a separate test. The
    // real ladder step under test is the transition BELOW: a freshly seeded
    // row that gets dispatch()-ed acquires the dispatch_entered stamp.)
    // ------------------------------------------------------------------

    public function test_dispatch_stamps_dispatch_entered_even_when_params_are_missing(): void
    {
        $wpdb = new FakeBackupTasksWpdb();
        $seededAt = time() - 500;
        $wpdb->seedRow(self::STALE_ID, [
            'phase'            => TaskRunner::PHASE_QUEUED,
            'started_at'       => $seededAt,
            'last_progress_at' => $seededAt,
            // No 'params' key — dispatch() must stamp the breadcrumb and
            // THEN bail cleanly (never construct/attempt a TaskRunner).
            'sub_state'        => '{}',
        ]);
        $GLOBALS['wpdb'] = $wpdb;

        Watchdog::dispatch(self::STALE_ID);

        $row = $wpdb->rows[self::STALE_ID] ?? null;
        $this->assertNotNull($row, 'dispatch() must not delete a non-terminal row it cannot rehydrate');

        $subState = json_decode((string) $row['sub_state'], true);
        $this->assertIsArray($subState);
        $this->assertSame('dispatch_entered', $subState['stage'] ?? null, 'dispatch() must stamp stage=dispatch_entered on entry');
        $this->assertGreaterThan($seededAt, $row['last_progress_at'], 'dispatch() must touch last_progress_at even when it cannot proceed further');
        $this->assertSame(TaskRunner::PHASE_QUEUED, $row['phase'], 'the phase itself must be untouched by the breadcrumb stamp alone');
    }

    // ------------------------------------------------------------------
    // Test 8: resume_count cap preserved.
    // ------------------------------------------------------------------

    public function test_resume_count_at_cap_is_marked_failed_not_resumed(): void
    {
        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->seedRow(self::CAPPED_ID, [
            'phase'            => TaskRunner::PHASE_QUEUED,
            'started_at'       => time() - 200,
            'last_progress_at' => time() - 200,
            'resume_count'     => 6,
            'max_resumes'      => 6,
            'sub_state'        => (string) json_encode(['params' => $this->runnerParams(self::CAPPED_ID)]),
        ]);
        $GLOBALS['wpdb'] = $wpdb;

        Watchdog::sweepStalled();

        $row = $wpdb->rows[self::CAPPED_ID] ?? null;
        $this->assertNotNull($row, 'a capped-out row is marked failed, not deleted, by this branch');
        $this->assertSame(TaskRunner::PHASE_FAILED, $row['phase'], 'a row at resume_count>=max_resumes must be marked failed, never resumed again');
        $this->assertSame(6, $row['resume_count'], 'resume_count must NOT be bumped past the cap');
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
