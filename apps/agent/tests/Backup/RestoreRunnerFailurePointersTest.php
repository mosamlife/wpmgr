<?php
/**
 * GH #538: a restore that fails after swapping files keeps its rollback
 * pointers, so the shutdown rollback can still undo the swap.
 *
 * The RestoreGuard armed before the first destructive rename fires at
 * shutdown unless the run was confirmed healthy, and it reads the rollback
 * pointers (swap_files, swap_db, db_rollback) from the task row. Each case
 * drives the real run() dispatch loop, captures the shutdown callbacks
 * instead of letting PHP run them at process exit, and then invokes them:
 *
 *   - A full restore that swaps files and then fails in restore_db: the
 *     FAILED row keeps swap_files next to last_error and failed_in, and the
 *     shutdown rollback puts the pre-restore files back.
 *   - A failure in swap_db, after the pre-restore dump decision was recorded
 *     mid-phase: the FAILED row keeps that db_rollback record, which the
 *     run's own phase-by-phase copy never held.
 *   - A row whose sub_state is overwritten after the run: the shutdown
 *     rollback still acts on the pointers the run last persisted.
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
use WPMgr\Agent\Backup\RestoreGuard;
use WPMgr\Agent\Backup\RestoreRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\RestoreRunner
 * @covers \WPMgr\Agent\Backup\RestoreGuard
 */
final class RestoreRunnerFailurePointersTest extends TestCase
{
    private const SNAPSHOT_ID = '53853853-8538-4538-8538-538538538538';
    private const RESTORE_ID  = '64664664-6646-4646-8646-646646646646';

    private string $root       = '';
    private string $liveDir    = '';
    private string $stagingDir = '';
    private string $scratchDir = '';
    private string $wpRootDir  = '';

    private FakeRestoreRunnerWpdb $wpdb;

    /** @var list<mixed> Every callback the run registered for shutdown. */
    private array $shutdown = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root       = sys_get_temp_dir() . '/wpmgr-538-' . bin2hex(random_bytes(6));
        $this->liveDir    = $this->root . '/wp-content';
        $this->stagingDir = $this->root . '/staging';
        $this->scratchDir = $this->root . '/scratch';
        $this->wpRootDir  = $this->root . '/wproot';
        foreach ([$this->liveDir, $this->stagingDir, $this->scratchDir, $this->wpRootDir] as $dir) {
            mkdir($dir, 0755, true);
        }

        $this->wpdb      = new FakeRestoreRunnerWpdb();
        $GLOBALS['wpdb'] = $this->wpdb;

        $this->shutdown = [];
        Functions\when('register_shutdown_function')->alias(function ($callback): void {
            $this->shutdown[] = $callback;
        });
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->root);
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_full_restore_failing_in_restore_db_keeps_swap_files_and_is_rolled_back_at_shutdown(): void
    {
        file_put_contents($this->liveDir . '/old-good.txt', 'pre-restore');
        file_put_contents($this->stagingDir . '/new-bad.txt', 'from the snapshot');
        $this->seed(RestoreRunner::PHASE_SWAP_FILES, [
            'tmp_prefix' => 'tmpaaaaaaaa_',
            'stage'      => [
                'staging_dir'          => $this->stagingDir,
                'has_legacy_file_kind' => true,
                'components_present'   => [],
            ],
            // No SQL artifact: restore_db throws once swap_files has run.
            'download'   => ['artifact_paths' => []],
        ]);

        $this->assertSame(RestoreRunner::PHASE_FAILED, $this->runner()->run());

        // The swap happened and the run failed after it.
        $this->assertFileExists($this->liveDir . '/new-bad.txt', 'precondition: swap_files put the snapshot tree live');
        $sub = $this->rowSubState();
        $this->assertSame(RestoreRunner::PHASE_RESTORE_DB, $sub['failed_in'] ?? null);
        $this->assertStringContainsString('restore_db', (string) ($sub['last_error'] ?? ''));
        $this->assertIsArray($sub['swap_files'] ?? null, 'the FAILED row lost swap_files, the pointer the shutdown rollback reverts with');
        $oldDir = (string) ($sub['swap_files']['old_files_dir'] ?? '');
        $this->assertNotSame('', $oldDir);
        $this->assertDirectoryExists($oldDir);
        $this->assertSame('tmpaaaaaaaa_', $sub['tmp_prefix'] ?? null, 'the FAILED row must keep the rest of the sub_state too');

        $this->assertSame(1, $this->fireShutdown());

        $this->assertFileExists($this->liveDir . '/old-good.txt', 'the shutdown rollback did not put the pre-restore files back');
        $this->assertFileDoesNotExist($this->liveDir . '/new-bad.txt');
        $this->assertDirectoryDoesNotExist($oldDir, 'the revert consumes the old-files dir it recorded');
    }

    public function test_failure_in_swap_db_keeps_the_db_rollback_recorded_mid_phase(): void
    {
        $oldDir = $this->root . '/.wpmgr-old-files-earlier';
        mkdir($oldDir, 0755, true);
        file_put_contents($oldDir . '/old-good.txt', 'pre-restore');
        file_put_contents($this->liveDir . '/new-bad.txt', 'from the snapshot');
        // A resumed run at swap_db: files were swapped by an earlier pass.
        $this->seed(RestoreRunner::PHASE_SWAP_DB, [
            'tmp_prefix' => 'tmpbbbbbbbb_',
            'restore_db' => ['done' => true, 'tmp_tables' => ['tmpbbbbbbbb_options']],
            'swap_files' => [
                'done'          => true,
                'old_files_dir' => $oldDir,
                'mode'          => 'legacy_whole',
            ],
        ]);

        $this->assertSame(RestoreRunner::PHASE_FAILED, $this->runner()->run());

        $sub = $this->rowSubState();
        $this->assertSame(RestoreRunner::PHASE_SWAP_DB, $sub['failed_in'] ?? null);
        $this->assertNotSame('', (string) ($sub['last_error'] ?? ''));
        $this->assertIsArray($sub['db_rollback'] ?? null, 'the FAILED row lost the db_rollback record swap_db persisted before its swap');
        $this->assertTrue($sub['db_rollback']['done'] ?? false);
        $this->assertFalse($sub['db_rollback']['available'] ?? true, 'the dump fails against the closed port and is recorded as unavailable');
        $this->assertSame($oldDir, $sub['swap_files']['old_files_dir'] ?? null);
        $this->assertSame(['tmpbbbbbbbb_options'], $sub['restore_db']['tmp_tables'] ?? null);
        $this->assertArrayNotHasKey('swap_db', $sub, 'the DB swap never happened');

        $this->assertSame(1, $this->fireShutdown());

        // No DB swap happened, so the files revert alone is coherent.
        $this->assertFileExists($this->liveDir . '/old-good.txt', 'the shutdown rollback did not put the pre-restore files back');
        $this->assertFileDoesNotExist($this->liveDir . '/new-bad.txt');
    }

    public function test_shutdown_rollback_uses_the_last_persisted_pointers_when_the_row_is_overwritten(): void
    {
        file_put_contents($this->liveDir . '/old-good.txt', 'pre-restore');
        file_put_contents($this->stagingDir . '/new-bad.txt', 'from the snapshot');
        $this->seed(RestoreRunner::PHASE_SWAP_FILES, [
            'tmp_prefix' => 'tmpcccccccc_',
            'stage'      => [
                'staging_dir'          => $this->stagingDir,
                'has_legacy_file_kind' => true,
                'components_present'   => [],
            ],
            'download'   => ['artifact_paths' => []],
        ]);

        $this->assertSame(RestoreRunner::PHASE_FAILED, $this->runner()->run());
        $this->assertFileExists($this->liveDir . '/new-bad.txt', 'precondition: swap_files put the snapshot tree live');

        // Something writes the row after the run, without the pointers.
        $this->wpdb->rows[self::SNAPSHOT_ID . '|' . self::RESTORE_ID]['sub_state'] = (string) json_encode(['last_error' => 'overwritten']);

        $this->assertSame(1, $this->fireShutdown());

        $this->assertFileExists($this->liveDir . '/old-good.txt', 'the shutdown rollback lost its pointers with the row');
        $this->assertFileDoesNotExist($this->liveDir . '/new-bad.txt');
    }

    /**
     * Invoke every RestoreGuard the run registered for shutdown, as PHP
     * would at process exit.
     *
     * @return int How many guards fired.
     */
    private function fireShutdown(): int
    {
        $fired = 0;
        foreach ($this->shutdown as $callback) {
            if (is_array($callback) && ($callback[0] ?? null) instanceof RestoreGuard) {
                $result = $callback();
                $fired += !empty($result['fired']) ? 1 : 0;
            }
        }

        return $fired;
    }

    /**
     * @param array<string,mixed> $subState
     */
    private function seed(string $phase, array $subState): void
    {
        $this->wpdb->rows[self::SNAPSHOT_ID . '|' . self::RESTORE_ID] = [
            'phase'        => $phase,
            'kind'         => 'full',
            'sub_state'    => (string) json_encode($subState),
            'resume_count' => 0,
            'max_resumes'  => 6,
        ];
    }

    /**
     * @return array<string,mixed>
     */
    private function rowSubState(): array
    {
        $row = $this->wpdb->rows[self::SNAPSHOT_ID . '|' . self::RESTORE_ID] ?? null;
        $this->assertIsArray($row);
        $this->assertSame(RestoreRunner::PHASE_FAILED, $row['phase']);
        $decoded = json_decode((string) $row['sub_state'], true);
        $this->assertIsArray($decoded);

        return $decoded;
    }

    private function runner(): RestoreRunner
    {
        return new RestoreRunner([
            'snapshot_id'       => self::SNAPSHOT_ID,
            'restore_id'        => self::RESTORE_ID,
            'kind'              => 'full',
            'progress_endpoint' => '',
            'scratch_dir'       => $this->scratchDir,
            'wp_content_path'   => $this->liveDir,
            'wp_root'           => $this->wpRootDir,
            'db'                => [
                // A closed loopback port: every connection fails fast.
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
