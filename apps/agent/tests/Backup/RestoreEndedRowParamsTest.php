<?php
/**
 * GH #538: a restore's task row keeps its run params only while the run can
 * still resume.
 *
 * RestoreCommand seeds the run params (database credentials, destination
 * config, chunk download URLs, the progress endpoint) into the row so the
 * watchdog can resume a stalled run. Once the run has ended, the row keeps
 * none of them and keeps everything else. One case per way a run ends, each
 * through the production code path:
 *
 *   - completed: run() passes both health gates, cleans up and completes.
 *   - failed by the runner: a phase throws and run()'s catch records it.
 *   - rolled back: a health gate fails, the run reverts and records it.
 *   - failed by the watchdog: a stalled run that has used its last resume.
 *
 * The other side of the contract: every write of a run still in progress
 * keeps the params, because the watchdog resumes from them. And the
 * watchdog ends only the run it read: a row a runner wrote to after that
 * read is left as the runner wrote it.
 *
 * No live MySQL is needed: the DB credentials point at a closed loopback
 * port, so every DB connection fails fast.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Backup\RestoreRunner;
use WPMgr\Agent\Backup\RestoreWatchdog;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\RestoreRunner
 * @covers \WPMgr\Agent\Backup\RestoreWatchdog
 */
final class RestoreEndedRowParamsTest extends TestCase
{
    private const SNAPSHOT_ID = '53853853-0538-4538-8538-000000000538';
    private const RESTORE_ID  = '64664664-0538-4646-8646-000000000538';

    /** The seeded database password. No ended row may hold it. */
    private const DB_PASSWORD = 'db-pass-538-must-not-outlive-the-run';

    private string $root        = '';
    private string $liveDir     = '';
    private string $oldFilesDir = '';
    private string $scratchDir  = '';
    private string $wpRootDir   = '';
    private string $dumpPath    = '';

    private FakeRestoreRunnerWpdb $wpdb;

    /** @var list<array{hook:string,args:array<int,mixed>}> */
    private array $scheduled = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root        = sys_get_temp_dir() . '/wpmgr-538-ended-' . bin2hex(random_bytes(6));
        $this->liveDir     = $this->root . '/wp-content';
        $this->oldFilesDir = $this->root . '/.wpmgr-old-files-000000000538';
        $this->scratchDir  = $this->root . '/scratch';
        $this->wpRootDir   = $this->root . '/wproot';
        foreach ([$this->liveDir, $this->oldFilesDir, $this->scratchDir, $this->wpRootDir] as $dir) {
            mkdir($dir, 0755, true);
        }
        file_put_contents($this->liveDir . '/restored.txt', 'from the snapshot');
        file_put_contents($this->oldFilesDir . '/pre-restore.txt', 'pre-restore');
        $this->dumpPath = $this->scratchDir . '/pre-restore-db.sql.gz';
        file_put_contents($this->dumpPath, (string) gzencode("-- pre-restore dump\n"));

        $this->wpdb      = new FakeRestoreRunnerWpdb();
        $GLOBALS['wpdb'] = $this->wpdb;

        $this->scheduled = [];
        Functions\when('home_url')->alias(static fn (string $path = '/') => 'https://example.test' . $path);
        Functions\when('admin_url')->alias(static fn (string $path = '') => 'https://example.test/wp-admin/' . $path);
        // Gate 2's loopback probe sees a healthy site.
        Functions\when('wp_remote_get')->alias(static fn (string $url, array $args = []): array => ['response' => ['code' => 200], 'body' => '<html>ok</html>']);
        Functions\when('wp_remote_retrieve_body')->alias(static fn ($r) => is_array($r) ? (string) ($r['body'] ?? '') : '');
        Functions\when('wp_next_scheduled')->justReturn(false);
        Functions\when('wp_schedule_single_event')->alias(function (int $ts, string $hook, array $args = []): bool {
            $this->scheduled[] = ['hook' => $hook, 'args' => $args];
            return true;
        });
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->root);
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_a_completed_restore_keeps_no_run_params(): void
    {
        $seeded = $this->seed(RestoreRunner::PHASE_HEALTH_CHECK, $this->rollbackPointers());

        $this->assertSame(RestoreRunner::PHASE_COMPLETED, $this->runner()->run());

        $sub = $this->endedSubState(RestoreRunner::PHASE_COMPLETED);
        $this->assertSame($seeded['swap_files'], $sub['swap_files'] ?? null);
        $this->assertSame($seeded['db_rollback'], $sub['db_rollback'] ?? null);
        $this->assertSame($seeded['tmp_prefix'], $sub['tmp_prefix'] ?? null);
        $this->assertTrue($sub['health_check']['ok'] ?? false, 'the COMPLETED row keeps the Gate 1 result');
        $this->assertTrue($sub['health_check_live']['ok'] ?? false, 'the COMPLETED row keeps the Gate 2 result');
        $this->assertTrue($sub['cleanup']['done'] ?? false);
    }

    public function test_a_restore_failed_by_the_runner_keeps_no_run_params(): void
    {
        // A resumed full restore at restore_db whose files were already
        // swapped, with no SQL artifact: restore_db throws, and run()'s
        // catch records the failure.
        $seeded = $this->seed(RestoreRunner::PHASE_RESTORE_DB, [
            'swap_files' => $this->rollbackPointers()['swap_files'],
            'download'   => ['artifact_paths' => []],
        ]);

        $this->assertSame(RestoreRunner::PHASE_FAILED, $this->runner()->run());

        $sub = $this->endedSubState(RestoreRunner::PHASE_FAILED);
        $this->assertSame(RestoreRunner::PHASE_RESTORE_DB, $sub['failed_in'] ?? null);
        $this->assertStringContainsString('restore_db', (string) ($sub['last_error'] ?? ''));
        $this->assertSame($seeded['swap_files'], $sub['swap_files'] ?? null, 'the FAILED row lost the pointer the shutdown rollback reverts the files with');
        $this->assertSame($seeded['tmp_prefix'], $sub['tmp_prefix'] ?? null);
        $this->assertArrayNotHasKey('rolled_back', $sub);
    }

    public function test_a_restore_rolled_back_by_a_health_gate_keeps_no_run_params(): void
    {
        // Gate 1 sees a broken site: siteurl reads back empty.
        $this->wpdb->siteurlValue = '';
        $seeded = $this->seed(RestoreRunner::PHASE_HEALTH_CHECK, $this->rollbackPointers());

        $this->assertSame(RestoreRunner::PHASE_FAILED, $this->runner()->run());

        $this->assertFileExists($this->liveDir . '/pre-restore.txt', 'precondition: the run rolled the files back');
        $sub = $this->endedSubState(RestoreRunner::PHASE_FAILED);
        $this->assertSame(RestoreRunner::PHASE_HEALTH_CHECK, $sub['failed_in'] ?? null);
        $this->assertNotSame('', (string) ($sub['last_error'] ?? ''));
        $this->assertTrue($sub['rolled_back']['files'] ?? false, 'the rolled-back row keeps which legs were reverted');
        $this->assertSame('db_unhealthy_post_restore', $sub['rolled_back']['reason'] ?? null);
        $this->assertSame($seeded['swap_files'], $sub['swap_files'] ?? null);
        $this->assertSame($seeded['db_rollback'], $sub['db_rollback'] ?? null);
    }

    public function test_a_restore_failed_by_the_watchdog_keeps_no_run_params(): void
    {
        // A stalled run at swap_db that has used its last resume.
        $seeded = $this->seed(
            RestoreRunner::PHASE_SWAP_DB,
            $this->rollbackPointers() + ['restore_db' => ['done' => true, 'tmp_tables' => ['tmpaaaaaaaa_options']]],
            6,
            time() - 1000
        );

        RestoreWatchdog::run(self::SNAPSHOT_ID, self::RESTORE_ID);

        $sub      = $this->endedSubState(RestoreRunner::PHASE_FAILED);
        $expected = $seeded;
        unset($expected['params']);
        $this->assertSame($expected, $sub, 'the FAILED row must keep everything but the run params');
        $this->assertSame([], $this->scheduled, 'a run the watchdog ended is not watched again');
    }

    public function test_the_watchdog_leaves_a_row_a_runner_wrote_after_the_watchdog_read_it(): void
    {
        $this->seed(RestoreRunner::PHASE_SWAP_DB, $this->rollbackPointers(), 6, time() - 1000);
        $key = self::SNAPSHOT_ID . '|' . self::RESTORE_ID;

        // A runner that is still alive persists its next phase between the
        // watchdog's read of the row and the watchdog's write.
        $runnerWrite = [
            'phase'            => RestoreRunner::PHASE_POST_HOOKS,
            'sub_state'        => (string) json_encode(
                ['params' => $this->runParams(), 'tmp_prefix' => 'tmpaaaaaaaa_']
                + $this->rollbackPointers()
                + ['swap_db' => ['done' => true, 'tables_swapped' => 12]]
            ),
            'last_progress_at' => time(),
        ];
        $this->wpdb->afterGetRow = static function (FakeRestoreRunnerWpdb $wpdb) use ($key, $runnerWrite): void {
            $wpdb->afterGetRow = null;
            $wpdb->rows[$key]  = array_merge($wpdb->rows[$key], $runnerWrite);
        };

        RestoreWatchdog::run(self::SNAPSHOT_ID, self::RESTORE_ID);

        $row = $this->wpdb->rows[$key];
        $this->assertSame(RestoreRunner::PHASE_POST_HOOKS, $row['phase'], 'the watchdog ended a run that had moved on since it was read');
        $this->assertSame($runnerWrite['sub_state'], $row['sub_state'], 'the watchdog replaced the sub_state the runner wrote');
        $this->assertSame(
            [['hook' => RestoreWatchdog::HOOK, 'args' => [self::SNAPSHOT_ID, self::RESTORE_ID]]],
            $this->scheduled,
            'the watchdog must look at a run it did not end again later'
        );
    }

    public function test_a_run_still_in_progress_keeps_its_params_on_every_write(): void
    {
        $this->seed(RestoreRunner::PHASE_HEALTH_CHECK, $this->rollbackPointers());

        $this->assertSame(RestoreRunner::PHASE_COMPLETED, $this->runner()->run());

        $inProgress = [];
        foreach ($this->wpdb->updates as $update) {
            $phase = $update['data']['phase'] ?? null;
            if (!is_string($phase) || $phase === RestoreRunner::PHASE_COMPLETED || $phase === RestoreRunner::PHASE_FAILED) {
                continue;
            }
            $inProgress[] = $phase;
            $sub = json_decode((string) ($update['data']['sub_state'] ?? ''), true);
            $this->assertIsArray($sub);
            $this->assertSame(
                self::DB_PASSWORD,
                $sub['params']['db']['password'] ?? null,
                'the ' . $phase . ' write dropped the run params the watchdog resumes from'
            );
        }
        $this->assertContains(RestoreRunner::PHASE_CLEANUP, $inProgress, 'the last write before the run ends is covered');
    }

    /**
     * What RestoreCommand seeds as the run params.
     *
     * @return array<string,mixed>
     */
    private function runParams(): array
    {
        return [
            'snapshot_id'        => self::SNAPSHOT_ID,
            'restore_id'         => self::RESTORE_ID,
            'kind'               => 'full',
            'progress_endpoint'  => 'https://cp.example.test/v1/agent/progress',
            'chunk_downloads'    => [[
                'logical_path' => 'database.sql.gz',
                'chunks'       => [[
                    'hash'          => str_repeat('a', 64),
                    'size'          => 1,
                    'presigned_url' => 'https://objects.example.test/chunk?X-Amz-Signature=538',
                ]],
            ]],
            'scratch_dir'        => $this->scratchDir,
            'wp_content_path'    => $this->liveDir,
            'wp_root'            => $this->wpRootDir,
            'db'                 => [
                // A closed loopback port: every connection fails fast.
                'host'     => '127.0.0.1:1',
                'user'     => 'nouser',
                'password' => self::DB_PASSWORD,
                'name'     => 'no_such_db',
                'prefix'   => 'wp_',
            ],
            'destination_kind'   => 'cp',
            'destination_config' => [],
        ];
    }

    /**
     * The runner the watchdog builds from the row's params, minus the
     * progress endpoint so a test posts nothing.
     */
    private function runner(): RestoreRunner
    {
        return new RestoreRunner(array_merge($this->runParams(), ['progress_endpoint' => '']));
    }

    /**
     * @return array{swap_files:array<string,mixed>,db_rollback:array<string,mixed>}
     */
    private function rollbackPointers(): array
    {
        return [
            'swap_files'  => ['done' => true, 'old_files_dir' => $this->oldFilesDir, 'mode' => 'legacy_whole'],
            'db_rollback' => ['done' => true, 'available' => true, 'dump_path' => $this->dumpPath, 'prefix' => 'wp_'],
        ];
    }

    /**
     * Seed the row as RestoreCommand does (the params in sub_state), plus
     * what earlier phases of the run persisted.
     *
     * @param array<string,mixed> $progress
     * @return array<string,mixed> The seeded sub_state.
     */
    private function seed(string $phase, array $progress, int $resumeCount = 0, ?int $lastProgressAt = null): array
    {
        $subState = ['params' => $this->runParams(), 'tmp_prefix' => 'tmpaaaaaaaa_'] + $progress;
        $this->wpdb->rows[self::SNAPSHOT_ID . '|' . self::RESTORE_ID] = [
            'snapshot_id'      => self::SNAPSHOT_ID,
            'restore_id'       => self::RESTORE_ID,
            'phase'            => $phase,
            'kind'             => 'full',
            'sub_state'        => (string) json_encode($subState),
            'last_progress_at' => $lastProgressAt ?? time(),
            'resume_count'     => $resumeCount,
            'max_resumes'      => 6,
        ];

        return $subState;
    }

    /**
     * The row's sub_state, after asserting the run ended in $phase and the
     * row holds neither the params nor the password anywhere.
     *
     * @return array<string,mixed>
     */
    private function endedSubState(string $phase): array
    {
        $row = $this->wpdb->rows[self::SNAPSHOT_ID . '|' . self::RESTORE_ID] ?? null;
        $this->assertIsArray($row);
        $this->assertSame($phase, $row['phase']);
        $this->assertStringNotContainsString(self::DB_PASSWORD, (string) json_encode($row), 'the ended restore row still holds the database password');
        $sub = json_decode((string) $row['sub_state'], true);
        $this->assertIsArray($sub);
        $this->assertArrayNotHasKey('params', $sub, 'the ended restore row still holds the run params');

        return $sub;
    }

    private function rrmdir(string $dir): void
    {
        if ($dir === '' || !is_dir($dir)) {
            return;
        }
        foreach (scandir($dir) ?: [] as $item) {
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
