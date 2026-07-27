<?php
/**
 * GitHub issue #256: Plugin::maybeGcRestoreArtifacts() regression lock.
 *
 * FilesRestorer::gcOldFiles() + RestoreRunner::gcPreRestoreDumps() were
 * previously reachable ONLY via a wp_schedule_single_event ONE-SHOT
 * scheduled at the end of a successful RestoreRunner::runCleanup(). A
 * crashed/aborted restore never scheduled it at all, and even a
 * scheduled-but-unfired event (WP-Cron never ticked) permanently suppressed
 * every later restore's attempt to schedule a replacement (see
 * RestoreOldFilesGcRescheduleTest for that half of the fix). This mirrors
 * PluginBackupJanitorSweepTest.php's shape (GH #256, backup side) for the
 * restore side.
 *
 * Verifies:
 *   - Gated on isEnrolled(): a not-yet-enrolled site never touches the
 *     filesystem.
 *   - Throttled via a stored option, stamped BEFORE the sweep runs.
 *   - Past the throttle window, BOTH gcOldFiles() and gcPreRestoreDumps()
 *     actually run (observed via real expired `.expires`-marked fixtures
 *     being reclaimed on disk).
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
 * @covers \WPMgr\Agent\Backup\FilesRestorer
 * @covers \WPMgr\Agent\Backup\RestoreRunner
 */
final class PluginRestoreGcSweepTest extends TestCase
{
    /** @var array<string,mixed> In-memory wp-option store. */
    private array $options = [];

    private string $contentDir = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        if (!defined('WP_CONTENT_DIR')) {
            $this->contentDir = sys_get_temp_dir() . '/wpmgr-restore-gc-plugin-' . bin2hex(random_bytes(6));
            mkdir($this->contentDir, 0755, true);
            define('WP_CONTENT_DIR', $this->contentDir);
        } else {
            $this->contentDir = WP_CONTENT_DIR;
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
        Monkey\tearDown();
        parent::tear_down();
    }

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

    /**
     * Seed an already-expired `.wpmgr-old-files-<id>` rollback tree directly
     * under WP_CONTENT_DIR (one of gcOldFiles()'s three candidate dirs).
     *
     * @return array{dir:string,marker:string}
     */
    private function seedExpiredOldFilesFixture(): array
    {
        $id     = bin2hex(random_bytes(6));
        $dir    = rtrim($this->contentDir, '/\\') . '/.wpmgr-old-files-' . $id;
        $marker = $dir . '.expires';
        mkdir($dir, 0755, true);
        file_put_contents($dir . '/good.txt', 'pre-restore-tree');
        file_put_contents($marker, (string) (time() - 10));

        return ['dir' => $dir, 'marker' => $marker];
    }

    /**
     * Seed an already-expired pre-restore DB dump directly under
     * `<WP_CONTENT_DIR>/wpmgr-agent/restores/<restore-id>/`.
     *
     * @return array{dump:string,marker:string,dir:string}
     */
    private function seedExpiredDumpFixture(): array
    {
        $restoreId = bin2hex(random_bytes(6));
        $dir       = rtrim($this->contentDir, '/\\') . '/wpmgr-agent/restores/' . $restoreId;
        $dump      = $dir . '/pre-restore-db.sql.gz';
        $marker    = $dump . '.expires';
        mkdir($dir, 0755, true);
        file_put_contents($dump, 'fixture-dump-bytes');
        file_put_contents($marker, (string) (time() - 10));

        return ['dump' => $dump, 'marker' => $marker, 'dir' => $dir];
    }

    public function test_not_enrolled_never_touches_filesystem(): void
    {
        $oldFiles = $this->seedExpiredOldFilesFixture();
        $dump     = $this->seedExpiredDumpFixture();

        // Deliberately NOT enrolled.
        $this->makePlugin()->maybeGcRestoreArtifacts();

        $this->assertTrue(is_dir($oldFiles['dir']), 'a not-yet-enrolled site must never sweep gcOldFiles() targets');
        $this->assertTrue(is_file($dump['dump']), 'a not-yet-enrolled site must never sweep gcPreRestoreDumps() targets');
        $this->assertArrayNotHasKey(Plugin::OPTION_RESTORE_GC_LAST, $this->options, 'a not-yet-enrolled site must never stamp the throttle option');
    }

    public function test_within_throttle_window_is_a_no_op(): void
    {
        $this->markEnrolled();
        $this->options[Plugin::OPTION_RESTORE_GC_LAST] = time() - 10; // well under the 900s window.

        $oldFiles = $this->seedExpiredOldFilesFixture();
        $dump     = $this->seedExpiredDumpFixture();

        $this->makePlugin()->maybeGcRestoreArtifacts();

        $this->assertTrue(is_dir($oldFiles['dir']), 'a throttled call within the window must never sweep the filesystem');
        $this->assertTrue(is_file($dump['dump']), 'a throttled call within the window must never sweep the filesystem');
        $this->assertSame(time() - 10, $this->options[Plugin::OPTION_RESTORE_GC_LAST], 'a throttled call within the window must never re-stamp the option');
    }

    public function test_past_throttle_window_stamps_before_running_then_sweeps_both_targets(): void
    {
        $this->markEnrolled();
        $this->options[Plugin::OPTION_RESTORE_GC_LAST] = time() - 1000; // past the 900s window.

        $oldFiles = $this->seedExpiredOldFilesFixture();
        $dump     = $this->seedExpiredDumpFixture();

        $before = time();
        $this->makePlugin()->maybeGcRestoreArtifacts();

        $this->assertGreaterThanOrEqual(
            $before,
            (int) $this->options[Plugin::OPTION_RESTORE_GC_LAST],
            'the throttle option must be stamped to "now" before the sweep runs'
        );
        $this->assertFalse(is_dir($oldFiles['dir']), 'past the throttle window, gcOldFiles() must reclaim the expired rollback tree');
        $this->assertFalse(is_file($oldFiles['marker']), 'the expired .expires marker must also be removed');
        $this->assertFalse(is_file($dump['dump']), 'past the throttle window, gcPreRestoreDumps() must reclaim the expired pre-restore dump');
        $this->assertFalse(is_file($dump['marker']), 'the expired dump .expires marker must also be removed');
    }
}
