<?php
/**
 * GitHub issue #256: Plugin::maybeGcBackupRuns() regression lock.
 *
 * BackupJanitor::gcRuns() was previously reachable ONLY via its daily
 * wpmgr_backup_runs_gc cron event, so a quiet/DISABLE_WP_CRON site (or one
 * where an operator deleted a failed backup from the dashboard, which is
 * entirely control-plane-local and dispatches no agent command at all) could
 * leak the on-host `wpmgr-agent/runs/<snapshot_id>/` scratch directory
 * forever. This mirrors PluginBackupSweepTest.php's shape (GH #232) exactly,
 * one directory up the same cron-independence ladder as maybeGcSnapshots()
 * and maybeSweepStalledBackups().
 *
 * Exercises the REAL method under test via a Plugin instance built with
 * `ReflectionClass::newInstanceWithoutConstructor()` + a directly-injected
 * `Settings` instance, the same isolation PluginBackupSweepTest uses, for
 * the same reason (avoid the heavy Plugin::boot() singleton path).
 *
 * Verifies:
 *   - Gated on isEnrolled(): a not-yet-enrolled site never touches the
 *     filesystem.
 *   - Throttled via a stored option, stamped BEFORE the sweep runs.
 *   - Past the throttle window, BackupJanitor::gcRuns() actually runs
 *     (observed via a real stale run directory being swept on disk).
 *   - A thrown sweep never bubbles out of the request.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionClass;
use WPMgr\Agent\Plugin;
use WPMgr\Agent\Settings;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Plugin
 * @covers \WPMgr\Agent\Backup\BackupJanitor
 */
final class PluginBackupJanitorSweepTest extends TestCase
{
    /** @var array<string,mixed> In-memory wp-option store. */
    private array $options = [];

    private string $contentDir = '';

    private string $runsBase = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        if (!defined('WP_CONTENT_DIR')) {
            $this->contentDir = sys_get_temp_dir() . '/wpmgr-janitor-plugin-' . bin2hex(random_bytes(6));
            mkdir($this->contentDir, 0755, true);
            define('WP_CONTENT_DIR', $this->contentDir);
        } else {
            $this->contentDir = WP_CONTENT_DIR;
        }
        $this->runsBase = rtrim($this->contentDir, '/\\') . '/wpmgr-agent/runs';
        if (!is_dir($this->runsBase)) {
            mkdir($this->runsBase, 0755, true);
        }

        $this->options = [];
        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;
            return true;
        });
        Functions\when('get_option')->alias(function ($name, $default = false) {
            return $this->options[$name] ?? $default;
        });
        Functions\when('wp_delete_file')->alias(static function ($f) {
            return @unlink($f); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
        });
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Build a Plugin instance WITHOUT running its (private, heavy)
     * constructor or boot(); only `$settings` is wired, which is all
     * maybeGcBackupRuns() touches on `$this`.
     */
    private function makePlugin(): Plugin
    {
        $reflection = new ReflectionClass(Plugin::class);
        /** @var Plugin $plugin */
        $plugin = $reflection->newInstanceWithoutConstructor();

        $settingsProp = $reflection->getProperty('settings');
        $settingsProp->setValue($plugin, new Settings());

        return $plugin;
    }

    private function markEnrolled(): void
    {
        $this->options[Settings::OPTION_SITE_ID] = 'site-abc';
        $this->options[Settings::OPTION_CP_URL]  = 'https://cp.example.test';
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

    /** Seed a stale (past the 6h age gate), inactive run dir with no task row. */
    private function seedStaleRunDir(string $id): string
    {
        $dir = $this->runsBase . '/' . $id;
        mkdir($dir, 0700, true);
        file_put_contents($dir . '/database.sql.gz', 'fixture-content');
        touch($dir, time() - (7 * 3600));
        clearstatcache(true, $dir);

        return $dir;
    }

    /** Minimal $wpdb double: no row for any snapshot id (isActiveRun() -> not active). */
    private function noRowWpdb(): object
    {
        return new class {
            public string $prefix = 'wp_';

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
                return null;
            }
        };
    }

    public function test_not_enrolled_never_touches_filesystem(): void
    {
        $id  = $this->newSnapshotId();
        $dir = $this->seedStaleRunDir($id);
        $GLOBALS['wpdb'] = $this->noRowWpdb();

        // Deliberately NOT enrolled.
        $this->makePlugin()->maybeGcBackupRuns();

        $this->assertTrue(is_dir($dir), 'a not-yet-enrolled site must never sweep the filesystem');
        $this->assertArrayNotHasKey(Plugin::OPTION_BACKUP_JANITOR_LAST, $this->options, 'a not-yet-enrolled site must never stamp the throttle option');
    }

    public function test_within_throttle_window_is_a_no_op(): void
    {
        $this->markEnrolled();
        $this->options[Plugin::OPTION_BACKUP_JANITOR_LAST] = time() - 10; // well under the 3600s window.

        $id  = $this->newSnapshotId();
        $dir = $this->seedStaleRunDir($id);
        $GLOBALS['wpdb'] = $this->noRowWpdb();

        $this->makePlugin()->maybeGcBackupRuns();

        $this->assertTrue(is_dir($dir), 'a throttled call within the window must never sweep the filesystem');
        $this->assertSame(time() - 10, $this->options[Plugin::OPTION_BACKUP_JANITOR_LAST], 'a throttled call within the window must never re-stamp the option');
    }

    public function test_past_throttle_window_stamps_before_running_then_sweeps(): void
    {
        $this->markEnrolled();
        $this->options[Plugin::OPTION_BACKUP_JANITOR_LAST] = time() - 7200; // past the 3600s window.

        $id  = $this->newSnapshotId();
        $dir = $this->seedStaleRunDir($id);
        $GLOBALS['wpdb'] = $this->noRowWpdb();

        $before = time();
        $this->makePlugin()->maybeGcBackupRuns();

        $this->assertGreaterThanOrEqual(
            $before,
            (int) $this->options[Plugin::OPTION_BACKUP_JANITOR_LAST],
            'the throttle option must be stamped to "now" before the sweep runs'
        );
        $this->assertFalse(is_dir($dir), 'past the throttle window, BackupJanitor::gcRuns() must actually sweep the stale, inactive run dir');
    }

    public function test_a_thrown_sweep_never_bubbles_out_of_the_request(): void
    {
        $this->markEnrolled();

        $id = $this->newSnapshotId();
        $this->seedStaleRunDir($id);

        // A $wpdb double whose prepare() throws, proving maybeGcBackupRuns()
        // completes normally regardless of which layer inside gcRuns() ->
        // isActiveRun() contains the failure.
        $wpdb = new class {
            public string $prefix = 'wp_';

            /**
             * @param mixed ...$args
             */
            public function prepare(string $query, ...$args): string
            {
                throw new \RuntimeException('simulated DB failure');
            }
        };
        $GLOBALS['wpdb'] = $wpdb;

        $plugin = $this->makePlugin();

        $threw = false;
        try {
            $plugin->maybeGcBackupRuns();
        } catch (\Throwable $e) {
            $threw = true;
        }

        $this->assertFalse($threw, 'a thrown sweep must never bubble out of the plugins_loaded-bound request handler');
        $this->assertArrayHasKey(
            Plugin::OPTION_BACKUP_JANITOR_LAST,
            $this->options,
            'the throttle stamp must still have been written (stamp-BEFORE-run) even though the sweep itself failed'
        );
    }
}
