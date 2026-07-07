<?php
/**
 * RestoreRollbackTest — GH #146 end-to-end coverage of RestoreRunner's
 * Gate 1 (`health_check`, the in-process DB probe)-triggered combined
 * files+DB rollback, driven through the REAL `run()` state machine dispatch
 * loop (not just `performRollback()` in isolation) so the "never Completed,
 * always reports FAILED with rolled_back detail" contract is proven at the
 * actual phase-transition level. Gate 2 (`health_check_live`, the loopback
 * WSOD probe on the real post-maintenance site)-triggered rollbacks — the
 * GitHub issue #147 files-WSOD class this whole feature exists to catch —
 * have their own dedicated coverage in RestoreHealthCheckLiveTest.
 *
 * The task row is pre-seeded directly at `health_check` (rather than
 * driving preflight -> ... -> swap_db -> post_hooks first) so this test
 * exercises exactly the phase under test without needing a working
 * FilesRestorer::stage()/DbRestorer::restore() replay to GET there — those
 * earlier phases have their own dedicated coverage elsewhere
 * (FilesRestorerExcludeAnchoringTest, RestoreRunnerIncrementalTest, etc.).
 *
 * DB-revert SQL-replay correctness is DbRestorer's own responsibility and
 * needs a live mysqli connection this suite doesn't have — mirrors
 * DbSnapshotCommandTest's identical disclaimer for `actionRevert()`, which
 * this class's `revertDb()` deliberately copies call-for-call. What THIS
 * test proves is the ORCHESTRATION: the pre-restore dump is handed to a
 * real DbRestorer (against a definitely-closed loopback port, so the
 * attempt fails fast and deterministically with no live MySQL needed), the
 * failure is caught (never aborts the files-side revert), and the
 * thrown/persisted `rolled_back` detail is correct.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Backup\RestoreRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\RestoreRunner
 * @covers \WPMgr\Agent\Backup\RestoreHealthCheck
 * @covers \WPMgr\Agent\Backup\FilesRestorer
 */
final class RestoreRollbackTest extends TestCase
{
    private const SNAPSHOT_ID = '11111111-1111-1111-1111-111111111111';
    private const RESTORE_ID  = '22222222-2222-2222-2222-222222222222';

    private string $root        = '';
    private string $targetDir   = '';
    private string $oldFilesDir = '';
    private string $scratchDir  = '';
    private string $wpRootDir   = '';
    private string $dumpPath    = '';

    private FakeRestoreRunnerWpdb $wpdb;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root        = sys_get_temp_dir() . '/wpmgr-rollback-' . bin2hex(random_bytes(6));
        $this->targetDir   = $this->root . '/wp-content';
        $this->oldFilesDir = $this->root . '/.wpmgr-old-files-222222222222';
        $this->scratchDir  = $this->root . '/scratch';
        $this->wpRootDir   = $this->root . '/wproot';

        mkdir($this->targetDir, 0755, true);
        mkdir($this->oldFilesDir, 0755, true);
        mkdir($this->scratchDir, 0755, true);
        mkdir($this->wpRootDir, 0755, true);

        // The live target currently holds the just-restored (BAD) tree.
        file_put_contents($this->targetDir . '/bad.txt', 'bad-new-content');
        // The pre-restore (GOOD) tree swap_files moved aside.
        file_put_contents($this->oldFilesDir . '/good.txt', 'good-old-content');

        // A real (never actually read) gz file standing in for the
        // pre-restore DB dump — DbRestorer::restore() fails at connect(),
        // before it ever gzopen()s this.
        $this->dumpPath = $this->scratchDir . '/pre-restore-db.sql.gz';
        file_put_contents($this->dumpPath, (string) gzencode("-- fake pre-restore dump\n"));

        $this->wpdb = new FakeRestoreRunnerWpdb();
        // Probe A fails deterministically — the headline GH #146 scenario:
        // siteurl reads back empty from the swapped live options table.
        $this->wpdb->siteurlValue = '';

        $key = self::SNAPSHOT_ID . '|' . self::RESTORE_ID;
        $this->wpdb->rows[$key]  = [
            'phase'     => RestoreRunner::PHASE_HEALTH_CHECK,
            'kind'      => 'full',
            'sub_state' => (string) json_encode([
                'tmp_prefix' => 'tmpaaaaaaaa_',
                'swap_files' => [
                    'done'          => true,
                    'old_files_dir' => $this->oldFilesDir,
                    'mode'          => 'legacy_whole',
                ],
                'db_rollback' => [
                    'done'      => true,
                    'available' => true,
                    'dump_path' => $this->dumpPath,
                    'prefix'    => 'wp_',
                ],
            ]),
            'resume_count' => 0,
            'max_resumes'  => 6,
        ];
        $GLOBALS['wpdb'] = $this->wpdb;

        Functions\when('home_url')->alias(static fn (string $path = '/') => 'https://example.test' . $path);
        Functions\when('admin_url')->alias(static fn (string $path = '') => 'https://example.test/wp-admin/' . $path);
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->root);
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_health_check_failure_rolls_back_files_and_reports_failed_never_completed(): void
    {
        $runner = $this->makeRunner();

        $phase = $runner->run();

        $this->assertSame(RestoreRunner::PHASE_FAILED, $phase, 'an unhealthy restore must NEVER report Completed');

        // --- Files ACTUALLY got reverted on disk. -------------------------
        $this->assertFileExists($this->targetDir . '/good.txt', 'the pre-restore tree must be back in place');
        $this->assertFileDoesNotExist($this->targetDir . '/bad.txt', 'the unhealthy just-restored tree must be gone');
        $this->assertDirectoryDoesNotExist($this->oldFilesDir, 'the rollback source dir is consumed by the revert');

        // --- Task row reflects FAILED + rolled_back detail. ----------------
        $finalSubState = $this->finalSubState();
        $this->assertSame(RestoreRunner::PHASE_HEALTH_CHECK, $finalSubState['failed_in']);
        $this->assertNotEmpty($finalSubState['last_error']);
        $this->assertIsArray($finalSubState['rolled_back']);
        $this->assertTrue($finalSubState['rolled_back']['files'], 'files leg must be recorded as rolled back');
        $this->assertFalse(
            $finalSubState['rolled_back']['db'],
            'the DB leg fails fast against the closed loopback port — recorded as attempted-but-failed, never silently true'
        );
        $this->assertSame(
            'db_unhealthy_post_restore',
            $finalSubState['rolled_back']['reason'],
            'Gate 1 (DB probe) failures tag the reason distinctly from Gate 2 (wsod_post_restore)'
        );

        $key = self::SNAPSHOT_ID . '|' . self::RESTORE_ID;
        $this->assertSame(RestoreRunner::PHASE_FAILED, $this->wpdb->rows[$key]['phase']);
    }

    /**
     * Regression guard for double-rollback idempotency: RestoreGuard is
     * NEVER armed in this scenario (the pre-seeded run starts AT
     * health_check, bypassing the swap_files/swap_db dispatch cases that
     * arm it), so `run()`'s SYNCHRONOUS rollback is the only one that can
     * fire. Re-running against the already-reverted state must not throw
     * or corrupt anything further (the old_files_dir is gone, so the files
     * leg of a second rollback attempt is simply a no-op).
     */
    public function test_rerunning_after_a_rollback_does_not_throw(): void
    {
        $first = $this->makeRunner()->run();
        $this->assertSame(RestoreRunner::PHASE_FAILED, $first);

        // Re-seed the row back at health_check (simulating a watchdog
        // re-entry after the FAILED terminal state — not a real production
        // path since `failed` is terminal, but proves performRollback()
        // itself tolerates being invoked with a stale/consumed old_files_dir).
        $key = self::SNAPSHOT_ID . '|' . self::RESTORE_ID;
        $this->wpdb->rows[$key]['phase'] = RestoreRunner::PHASE_HEALTH_CHECK;
        $this->wpdb->rows[$key]['sub_state'] = (string) json_encode([
            'tmp_prefix' => 'tmpaaaaaaaa_',
            'swap_files' => [
                'done'          => true,
                'old_files_dir' => $this->oldFilesDir, // no longer exists on disk.
                'mode'          => 'legacy_whole',
            ],
            'db_rollback' => ['done' => true, 'available' => false, 'reason' => 'unavailable'],
        ]);

        $second = $this->makeRunner()->run();

        $this->assertSame(RestoreRunner::PHASE_FAILED, $second);
        // Files leg: old_files_dir no longer exists, so revertSwap() throws
        // (missing rollback source) — caught by performRollback()'s own
        // try/catch, recorded as files=>false, no crash.
        $finalSubState = $this->finalSubState();
        $this->assertFalse($finalSubState['rolled_back']['files']);
        // And critically: the already-reverted good tree from the FIRST
        // rollback is left untouched — a second, consumed rollback attempt
        // must never re-corrupt an already-healthy target.
        $this->assertFileExists($this->targetDir . '/good.txt');
    }

    /**
     * Review MEDIUM-4: the DB WAS swapped as part of this restore
     * (`swap_db.done`) but no rollback source is available — reverting
     * FILES ALONE would run the pre-restore files against the NEW
     * (post-restore) database, a worse "Frankenstein" mismatch than simply
     * leaving the as-restored (if unhealthy) site in place. Neither leg
     * must be touched; the reason must be recorded.
     */
    public function test_db_rollback_unavailable_after_swap_db_skips_files_revert_too(): void
    {
        $key = self::SNAPSHOT_ID . '|' . self::RESTORE_ID;
        $this->wpdb->rows[$key]['sub_state'] = (string) json_encode([
            'tmp_prefix' => 'tmpaaaaaaaa_',
            'swap_files' => [
                'done'          => true,
                'old_files_dir' => $this->oldFilesDir,
                'mode'          => 'legacy_whole',
            ],
            'swap_db' => ['done' => true, 'tables_swapped' => 12],
            'db_rollback' => ['done' => true, 'available' => false, 'reason' => 'unavailable'],
        ]);

        $phase = $this->makeRunner()->run();

        $this->assertSame(RestoreRunner::PHASE_FAILED, $phase);

        // Neither leg touched — the as-restored (if unhealthy) state is
        // left coherent rather than mismatched.
        $this->assertFileExists($this->targetDir . '/bad.txt', 'files must NOT be reverted when the DB has no rollback source');
        $this->assertFileDoesNotExist($this->targetDir . '/good.txt');
        $this->assertDirectoryExists($this->oldFilesDir, 'the untouched rollback tree is left in place for manual investigation');

        $finalSubState = $this->finalSubState();
        $this->assertFalse($finalSubState['rolled_back']['files']);
        $this->assertFalse($finalSubState['rolled_back']['db']);
        $this->assertSame('db_rollback_unavailable', $finalSubState['rolled_back']['reason']);
    }

    /**
     * Review MEDIUM-3: the synchronous health-check failure path must
     * route through `RestoreGuard::fire()` (not call `performRollback()`
     * directly) when a guard is armed, so the guard's `fired` idempotency
     * flag actually engages. Proven end-to-end: seed the row at
     * `swap_files` (kind=files, so no live DB/mysqli is needed) and drive
     * the REAL `run()` dispatch loop through `armGuardOnce()` -> a real
     * `FilesRestorer::swap()` -> `post_hooks` -> a failing `health_check` ->
     * the rollback. Afterward, reach into the runner's now-fired guard and
     * confirm a SECOND `fire()` (simulating the eventual real
     * process-shutdown invocation of the same registered callback) is a
     * documented no-op — proof the synchronous path engaged the guard
     * itself, not a bare `performRollback()` call that would leave `fired`
     * false.
     */
    public function test_guard_armed_via_swap_files_engages_fired_idempotency_on_health_fail(): void
    {
        // Deliberately a DIFFERENT restore_id from self::RESTORE_ID: swap()'s
        // `.wpmgr-old-files-<shortId(restoreId)>` sibling path is
        // deterministic from the restore id, and set_up() already created
        // `$this->oldFilesDir` (`.wpmgr-old-files-222222222222`, from
        // self::RESTORE_ID) as a non-empty dir for the OTHER tests in this
        // class — reusing that same id here would make swap() see an
        // already-existing (unrelated) `.wpmgr-old-files-*` sibling and
        // treat this as a watchdog-resume-mid-swap, which is not the
        // scenario under test.
        $guardRestoreId = '99999999-9999-9999-9999-999999999999';

        $liveContentDir = $this->root . '/guard-live-wp-content';
        $stagingDir     = $this->root . '/guard-staging';
        mkdir($liveContentDir, 0755, true);
        mkdir($stagingDir, 0755, true);
        file_put_contents($liveContentDir . '/old-good.txt', 'pre-restore-content');
        file_put_contents($stagingDir . '/new-bad.txt', 'just-restored-content');

        $key = self::SNAPSHOT_ID . '|' . $guardRestoreId;
        $this->wpdb->rows[$key] = [
            'phase' => RestoreRunner::PHASE_SWAP_FILES,
            'kind'  => 'files',
            'sub_state' => (string) json_encode([
                'tmp_prefix' => 'tmpddddddddd_',
                'stage' => [
                    'staging_dir'          => $stagingDir,
                    'has_legacy_file_kind' => true,
                    'components_present'   => [],
                ],
            ]),
            'resume_count' => 0,
            'max_resumes'  => 6,
        ];

        $runner = new RestoreRunner([
            'snapshot_id'       => self::SNAPSHOT_ID,
            'restore_id'        => $guardRestoreId,
            'kind'              => 'files',
            'progress_endpoint' => '',
            'scratch_dir'       => $this->scratchDir,
            'wp_content_path'   => $liveContentDir,
            'wp_root'           => $this->wpRootDir,
            'db' => ['host' => '', 'user' => '', 'password' => '', 'name' => '', 'prefix' => 'wp_'],
        ]);

        $phase = $runner->run();

        $this->assertSame(RestoreRunner::PHASE_FAILED, $phase);

        // The real swap_files -> armGuardOnce() -> health_check-fail ->
        // guard->fire() -> performRollback() chain actually reverted files.
        $this->assertFileExists($liveContentDir . '/old-good.txt');
        $this->assertFileDoesNotExist($liveContentDir . '/new-bad.txt');

        $rc    = new \ReflectionClass($runner);
        $prop  = $rc->getProperty('guard');
        $guard = $prop->getValue($runner);
        $this->assertNotNull($guard, 'armGuardOnce() must have armed a guard via the swap_files dispatch case');

        $secondFire = $guard->fire();
        $this->assertFalse(
            $secondFire['fired'],
            'a second fire() must be a documented no-op — proves the synchronous rollback engaged the guard '
            . '(not a bare performRollback() call that would leave `fired` false and risk a real shutdown-time '
            . 'second rollback attempt)'
        );
    }

    /**
     * @return array<string,mixed>
     */
    private function finalSubState(): array
    {
        $key = self::SNAPSHOT_ID . '|' . self::RESTORE_ID;
        $row = $this->wpdb->rows[$key] ?? null;
        $this->assertIsArray($row);
        $decoded = json_decode((string) $row['sub_state'], true);
        $this->assertIsArray($decoded);
        return $decoded;
    }

    private function makeRunner(): RestoreRunner
    {
        return new RestoreRunner([
            'snapshot_id'       => self::SNAPSHOT_ID,
            'restore_id'        => self::RESTORE_ID,
            'kind'              => 'full',
            'progress_endpoint' => '',
            'scratch_dir'       => $this->scratchDir,
            'wp_content_path'   => $this->targetDir,
            'wp_root'           => $this->wpRootDir,
            'db' => [
                // Definitely-closed loopback port: DbRestorer::connect()
                // fails fast (ECONNREFUSED) — no live MySQL needed, and no
                // real network access either.
                'host'     => '127.0.0.1:1',
                'user'     => 'nouser',
                'password' => 'nopass',
                'name'     => 'no_such_db',
                'prefix'   => 'wp_',
            ],
        ]);
    }

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
}
