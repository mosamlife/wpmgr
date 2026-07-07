<?php
/**
 * RestoreHealthCheckLiveTest — GH #146 two-gate restructure: dedicated
 * coverage of Gate 2 (`health_check_live`, the loopback WSOD probe on the
 * REAL post-maintenance site).
 *
 * This is the GitHub issue #147 class this whole feature exists to catch:
 * a dropped required file fataling on plugin load. Gate 1 (`health_check`,
 * the in-process DB probe, still under maintenance) CANNOT see this at
 * all — the DB is perfectly fine; only a real, post-maintenance HTTP
 * request to the live site can observe a plugin-load fatal. Every test here
 * drives the REAL `run()` dispatch loop, seeded at `health_check` (Gate 1)
 * with Probe A passing by default, so `run()` actually crosses
 * maintenance_off into `health_check_live` (Gate 2) — proving the phase
 * ORDER, not just Gate 2's probe logic in isolation (that's
 * RestoreHealthCheckTest's job).
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
 */
final class RestoreHealthCheckLiveTest extends TestCase
{
    private const SNAPSHOT_ID = '55555555-5555-5555-5555-555555555555';
    private const RESTORE_ID  = '66666666-6666-6666-6666-666666666666';

    private string $root        = '';
    private string $targetDir   = '';
    private string $oldFilesDir = '';
    private string $scratchDir  = '';
    private string $wpRootDir   = '';
    private string $dumpPath    = '';

    private FakeRestoreRunnerWpdb $wpdb;

    /** @var list<array{ts:int,hook:string}> */
    private array $scheduled = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root        = sys_get_temp_dir() . '/wpmgr-hclive-' . bin2hex(random_bytes(6));
        $this->targetDir   = $this->root . '/wp-content';
        $this->oldFilesDir = $this->root . '/.wpmgr-old-files-666666666666';
        $this->scratchDir  = $this->root . '/scratch';
        $this->wpRootDir   = $this->root . '/wproot';

        mkdir($this->targetDir, 0755, true);
        mkdir($this->oldFilesDir, 0755, true);
        mkdir($this->scratchDir, 0755, true);
        mkdir($this->wpRootDir, 0755, true);

        // The live target currently holds the just-restored tree — a
        // FILES-side WSOD (the #147 class: e.g. a dropped required file)
        // fatals on plugin load even though the DB is perfectly fine.
        file_put_contents($this->targetDir . '/bad.txt', 'just-restored-content-with-a-broken-plugin');
        file_put_contents($this->oldFilesDir . '/good.txt', 'pre-restore-working-content');

        $this->dumpPath = $this->scratchDir . '/pre-restore-db.sql.gz';
        file_put_contents($this->dumpPath, (string) gzencode("-- fake pre-restore dump\n"));

        $this->wpdb = new FakeRestoreRunnerWpdb();
        // Probe A (Gate 1) passes by default — the whole point of this
        // files-WSOD class is that the DB is fine; only the real site (Gate
        // 2) can see the plugin-load fatal.

        $key = self::SNAPSHOT_ID . '|' . self::RESTORE_ID;
        $this->wpdb->rows[$key] = [
            'phase'     => RestoreRunner::PHASE_HEALTH_CHECK,
            'kind'      => 'full',
            'sub_state' => (string) json_encode([
                'tmp_prefix' => 'tmpeeeeeeee_',
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

    /**
     * The headline GH #147-class scenario: Gate 1 (DB) passes, but Gate 2
     * (the real post-maintenance site) gets a definitive fatal (a real 5xx
     * here — a fatal-body-signature case is covered in
     * RestoreHealthCheckTest). Must roll back files+DB and report FAILED
     * with reason wsod_post_restore — NEVER Completed.
     */
    public function test_files_wsod_on_the_real_site_rolls_back_and_reports_failed(): void
    {
        Functions\when('wp_remote_get')->alias(static fn (string $url, array $args = []) => [
            'response' => ['code' => 500],
            'body'     => 'Internal Server Error — Fatal error: Uncaught Error in wp-content/plugins/broken/broken.php',
        ]);

        $phase = $this->makeRunner()->run();

        $this->assertSame(RestoreRunner::PHASE_FAILED, $phase, 'a files-WSOD restore must NEVER report Completed');

        // --- Files ACTUALLY got reverted on disk. -------------------------
        $this->assertFileExists($this->targetDir . '/good.txt', 'the pre-restore (working) tree must be back in place');
        $this->assertFileDoesNotExist($this->targetDir . '/bad.txt', 'the WSOD-causing just-restored tree must be gone');
        $this->assertDirectoryDoesNotExist($this->oldFilesDir, 'the rollback source dir is consumed by the revert');

        // --- Task row reflects FAILED + the Gate-2-specific reason. --------
        $finalSubState = $this->finalSubState();
        $this->assertSame(RestoreRunner::PHASE_HEALTH_CHECK_LIVE, $finalSubState['failed_in']);
        $this->assertNotEmpty($finalSubState['last_error']);
        $this->assertTrue($finalSubState['rolled_back']['files'], 'files leg must be recorded as rolled back');
        $this->assertFalse(
            $finalSubState['rolled_back']['db'],
            'the DB leg fails fast against the closed loopback port — recorded as attempted-but-failed, never silently true'
        );
        $this->assertSame(
            'wsod_post_restore',
            $finalSubState['rolled_back']['reason'],
            'Gate 2 (loopback WSOD) failures tag the reason distinctly from Gate 1 (db_unhealthy_post_restore)'
        );

        $key = self::SNAPSHOT_ID . '|' . self::RESTORE_ID;
        $this->assertSame(RestoreRunner::PHASE_FAILED, $this->wpdb->rows[$key]['phase']);
    }

    /**
     * Same files-WSOD scenario, but via a fatal-body-signature 200 (a
     * swallowed fatal under display_errors=Off) instead of a real 5xx —
     * proves the body-signature classification path also drives the Gate-2
     * rollback, not just a raw 5xx.
     */
    public function test_fatal_body_signature_on_the_real_site_rolls_back_and_reports_failed(): void
    {
        Functions\when('wp_remote_get')->alias(static fn (string $url, array $args = []) => [
            'response' => ['code' => 200],
            'body'     => '<b>Fatal error</b>: Uncaught Error: Class "Broken_Plugin" not found in wp-content/plugins/broken/broken.php',
        ]);

        $phase = $this->makeRunner()->run();

        $this->assertSame(RestoreRunner::PHASE_FAILED, $phase);
        $this->assertFileExists($this->targetDir . '/good.txt');
        $this->assertFileDoesNotExist($this->targetDir . '/bad.txt');

        $finalSubState = $this->finalSubState();
        $this->assertSame(RestoreRunner::PHASE_HEALTH_CHECK_LIVE, $finalSubState['failed_in']);
        $this->assertSame('wsod_post_restore', $finalSubState['rolled_back']['reason']);
    }

    /**
     * GH #146 review CRITICAL-2 safety, exercised end-to-end through Gate
     * 2: every loopback probe target connect-refuses (a transient blip,
     * post-maintenance-off — the site is live, this isn't a maintenance-mode
     * artifact) — this MUST be inconclusive/fail-open, proceeding all the
     * way to Completed with NO rollback. Proves a blip after maintenance is
     * lifted can never roll back a good restore.
     */
    public function test_connect_refused_on_the_real_site_is_inconclusive_completes_with_no_rollback(): void
    {
        Functions\when('wp_remote_get')->alias(static fn (string $url, array $args = []) => new \WP_Error('http_request_failed', 'Connection refused'));

        $phase = $this->makeRunner()->run();

        $this->assertSame(RestoreRunner::PHASE_COMPLETED, $phase, 'mere unreachability post-maintenance-off must never roll back a good restore');

        // --- Nothing was reverted — the as-restored tree is untouched. -----
        $this->assertFileExists($this->targetDir . '/bad.txt', 'the as-restored tree must be left exactly as-is');
        $this->assertFileDoesNotExist($this->targetDir . '/good.txt');
        // --- And the rollback material is still kept (deferred cleanup). --
        $this->assertDirectoryExists($this->oldFilesDir);
        $this->assertFileExists($this->oldFilesDir . '.expires');

        $key = self::SNAPSHOT_ID . '|' . self::RESTORE_ID;
        $this->assertSame(RestoreRunner::PHASE_COMPLETED, $this->wpdb->rows[$key]['phase']);
    }

    /**
     * Same fail-open invariant, but for a blank/marker-less 200 (the
     * Risk-2 hardening) rather than a connect failure — also must complete
     * with no rollback.
     */
    public function test_blank_200_on_the_real_site_is_inconclusive_completes_with_no_rollback(): void
    {
        Functions\when('wp_remote_get')->alias(static fn (string $url, array $args = []) => ['response' => ['code' => 200], 'body' => '']);

        $phase = $this->makeRunner()->run();

        $this->assertSame(RestoreRunner::PHASE_COMPLETED, $phase);
        $this->assertFileExists($this->targetDir . '/bad.txt');
        $this->assertFileDoesNotExist($this->targetDir . '/good.txt');
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
