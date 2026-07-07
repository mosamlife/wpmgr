<?php
/**
 * RestoreHealthPassTest — GH #146 coverage of the health-check PASS path
 * through BOTH gates: Gate 1 (`health_check`, Probe A / DB, via the
 * FakeRestoreRunnerWpdb's default healthy answers) and Gate 2
 * (`health_check_live`, Probe B / loopback, via the universal 200+html
 * `wp_remote_get` mock below — Gate 1 never calls `wp_remote_get` at all
 * under the two-gate restructure, so this one mock serves Gate 2 only).
 * Drives all the way to `completed`, marks the guard clean (at Gate 2, the
 * LAST gate — see `RestoreGuard::markClean()`'s doc), and confirms §5's
 * deferred cleanup actually KEEPS the files rollback tree + the pre-restore
 * DB dump (with `.expires` GC markers + a scheduled sweep), rather than the
 * pre-#146 behavior of synchronously deleting them.
 *
 * Like RestoreRollbackTest, the task row is pre-seeded directly at
 * `health_check` so this test exercises exactly post_hooks-onward without
 * needing a full stage_files/swap_files/restore_db/swap_db replay. Gate
 * 2's OWN failure path (the GitHub issue #147 files-WSOD class) has its
 * dedicated coverage in RestoreHealthCheckLiveTest.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Backup\FilesRestorer;
use WPMgr\Agent\Backup\RestoreRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\RestoreRunner
 * @covers \WPMgr\Agent\Backup\RestoreHealthCheck
 */
final class RestoreHealthPassTest extends TestCase
{
    private const SNAPSHOT_ID = '33333333-3333-3333-3333-333333333333';
    private const RESTORE_ID  = '44444444-4444-4444-4444-444444444444';

    private string $root        = '';
    private string $targetDir   = '';
    private string $oldFilesDir = '';
    private string $scratchDir  = '';
    private string $wpRootDir   = '';
    private string $dumpPath    = '';

    private FakeRestoreRunnerWpdb $wpdb;

    /** @var list<array{ts:int,hook:string}> */
    private array $scheduled = [];

    /** Number of wp_remote_get() calls — proves Gate 2 actually probed. */
    private int $loopbackCalls = 0;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root        = sys_get_temp_dir() . '/wpmgr-healthpass-' . bin2hex(random_bytes(6));
        $this->targetDir   = $this->root . '/wp-content';
        $this->oldFilesDir = $this->root . '/.wpmgr-old-files-444444444444';
        $this->scratchDir  = $this->root . '/scratch';
        $this->wpRootDir   = $this->root . '/wproot';

        mkdir($this->targetDir, 0755, true);
        mkdir($this->oldFilesDir, 0755, true);
        mkdir($this->scratchDir, 0755, true);
        mkdir($this->wpRootDir, 0755, true);

        file_put_contents($this->targetDir . '/current.txt', 'the-now-healthy-live-tree');
        file_put_contents($this->oldFilesDir . '/good.txt', 'pre-restore-tree');

        $this->dumpPath = $this->scratchDir . '/pre-restore-db.sql.gz';
        file_put_contents($this->dumpPath, (string) gzencode("-- fake pre-restore dump\n"));

        $this->wpdb = new FakeRestoreRunnerWpdb();
        // Probe A passes (defaults: siteurl set, no table errors).
        $key = self::SNAPSHOT_ID . '|' . self::RESTORE_ID;
        $this->wpdb->rows[$key] = [
            'phase'     => RestoreRunner::PHASE_HEALTH_CHECK,
            'kind'      => 'full',
            'sub_state' => (string) json_encode([
                'tmp_prefix' => 'tmpbbbbbbbb_',
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
        // Every loopback probe (home/admin) answers 2xx — Probe B passes
        // outright. This mock ONLY serves Gate 2 (health_check_live) —
        // Gate 1 (health_check) never calls wp_remote_get() at all under the
        // two-gate restructure — so counting calls here doubles as proof
        // Gate 2 actually ran (asserted below), not just Gate 1.
        Functions\when('wp_remote_get')->alias(function (string $url, array $args = []): array {
            $this->loopbackCalls++;
            return ['response' => ['code' => 200], 'body' => '<html>ok</html>'];
        });
        Functions\when('wp_remote_retrieve_body')->alias(static fn ($r) => is_array($r) ? (string) ($r['body'] ?? '') : '');

        Functions\when('wp_next_scheduled')->justReturn(false);
        Functions\when('wp_schedule_single_event')->alias(function (int $ts, string $hook, array $args = []): bool {
            $this->scheduled[] = ['ts' => $ts, 'hook' => $hook];
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

    public function test_health_check_pass_completes_and_keeps_rollback_material_for_the_retention_window(): void
    {
        $runner = new RestoreRunner([
            'snapshot_id'       => self::SNAPSHOT_ID,
            'restore_id'        => self::RESTORE_ID,
            'kind'              => 'full',
            'progress_endpoint' => '',
            'scratch_dir'       => $this->scratchDir,
            'wp_content_path'   => $this->targetDir,
            'wp_root'           => $this->wpRootDir,
            'db' => [
                'host' => '', 'user' => '', 'password' => '', 'name' => '', 'prefix' => 'wp_',
            ],
        ]);

        $phase = $runner->run();

        $this->assertSame(RestoreRunner::PHASE_COMPLETED, $phase);

        // --- The files rollback tree is KEPT (deferred, not rrmdir'd). -----
        $this->assertDirectoryExists($this->oldFilesDir);
        $this->assertFileExists($this->oldFilesDir . '/good.txt');
        $this->assertFileExists($this->oldFilesDir . '.expires', 'gcOldFiles() needs this marker to know when it may reclaim the tree');
        $this->assertGreaterThan(time(), (int) trim((string) file_get_contents($this->oldFilesDir . '.expires')));

        // --- The pre-restore DB dump is ALSO kept, same window. -----------
        $this->assertFileExists($this->dumpPath);
        $this->assertFileExists($this->dumpPath . '.expires');
        $this->assertGreaterThan(time(), (int) trim((string) file_get_contents($this->dumpPath . '.expires')));

        // --- GC sweep was scheduled on the shared hook. --------------------
        $this->assertNotEmpty($this->scheduled);
        $this->assertSame('wpmgr_restore_oldfiles_gc', $this->scheduled[0]['hook']);
        $this->assertGreaterThanOrEqual(
            time() + FilesRestorer::OLDFILES_GC_AGE_SECONDS,
            $this->scheduled[0]['ts'],
            'the DEFAULT (non keep_old_files) path must schedule at least the SHORT window out, not the 24h LONG one'
        );

        // --- Task row reflects Completed. -----------------------------------
        $key = self::SNAPSHOT_ID . '|' . self::RESTORE_ID;
        $this->assertSame(RestoreRunner::PHASE_COMPLETED, $this->wpdb->rows[$key]['phase']);

        // --- Gate 2 (health_check_live) actually ran the loopback probe. ---
        // Gate 1 never calls wp_remote_get() at all under the two-gate
        // restructure, so any call here proves Gate 2 executed (not just
        // Gate 1) — and since PHASE_COMPLETED is only reachable if BOTH
        // gates passed (runHealthCheckLive() throws on anything but
        // ok/inconclusive), reaching Completed already proves Gate 1 passed.
        $this->assertGreaterThan(0, $this->loopbackCalls, 'Gate 2 must have actually probed the loopback URLs');
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
