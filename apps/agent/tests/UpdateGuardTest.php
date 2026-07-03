<?php
/**
 * UpdateGuardTest — direct unit coverage of WPMgr\Agent\Support\UpdateGuard,
 * focused on the F2 fix (issue #131 final-hardening review): markClean() must
 * ALSO clear this item's UpdateInFlight out-of-band reconcile marker
 * immediately, closing the narrow window between "apply confirmed good" and
 * UpdateCommand::processItem()'s own `finally` (where UpdateInFlight::clear()
 * was previously the ONLY place a marker was ever removed on a success path).
 *
 * The arm()/fire()/idempotency lifecycle itself is exercised end-to-end via
 * UpdateCommandTest.php's integration tests; this file isolates markClean()'s
 * NEW side effect.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Support\SnapshotManager;
use WPMgr\Agent\Support\UpdateGuard;
use WPMgr\Agent\Support\UpdateInFlight;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\UpdateGuard
 */
final class UpdateGuardTest extends TestCase
{
    /** Temp root for this test run (removed in tear_down). */
    private string $root = '';

    /** Simulated wp-content/uploads dir. */
    private string $uploadsDir = '';

    /** UpdateInFlight marker store under $uploadsDir. */
    private string $storeDir = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root       = sys_get_temp_dir() . '/wpmgr-guard-' . bin2hex(random_bytes(6));
        $this->uploadsDir = $this->root . '/uploads';
        $this->storeDir   = $this->uploadsDir . '/wpmgr-update-inflight';
        mkdir($this->uploadsDir, 0755, true);

        Functions\when('wp_upload_dir')->justReturn(['basedir' => $this->uploadsDir]);
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->root);
        Monkey\tearDown();
        parent::tear_down();
    }

    /** Recursive delete used only for test fixture cleanup. */
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
                unlink($path); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
            }
        }
        rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- test-only fixture cleanup
    }

    /** A SnapshotManager spy that records restore() calls without touching disk. */
    private function spySnapshots(): SnapshotManager
    {
        return new class extends SnapshotManager {
            /** @var array<int,array{string,string,string}> */
            public array $restored = [];

            public function restore(string $type, string $slug, string $snapshotId): array
            {
                $this->restored[] = [$type, $slug, $snapshotId];

                return ['ok' => true, 'log' => 'restored'];
            }
        };
    }

    public function test_markClean_clears_this_items_update_inflight_marker_immediately(): void
    {
        $lock = UpdateInFlight::mark('plugin', 'foo/foo.php', 'snap_x');
        $this->assertCount(1, glob($this->storeDir . '/*.json') ?: [], 'precondition: mark() wrote a marker');

        $guard = new UpdateGuard($this->spySnapshots(), 'plugin', 'foo/foo.php', 'snap_x');
        $guard->markClean();

        $this->assertCount(
            0,
            glob($this->storeDir . '/*.json') ?: [],
            'F2: markClean() must clear this type/slug\'s UpdateInFlight marker immediately, not wait for a later finally'
        );

        UpdateInFlight::release($lock);
    }

    public function test_markClean_is_a_safe_noop_when_no_marker_was_ever_written(): void
    {
        $guard = new UpdateGuard($this->spySnapshots(), 'plugin', 'never-marked/never-marked.php', 'snap_x');

        // Must not throw or warn — clear() is a no-op when nothing matches.
        $guard->markClean();

        $this->assertCount(0, glob($this->storeDir . '/*.json') ?: []);
    }

    public function test_markClean_only_clears_the_marker_for_its_own_type_and_slug(): void
    {
        $lockA = UpdateInFlight::mark('plugin', 'foo/foo.php', 'snap_a');
        $lockB = UpdateInFlight::mark('theme', 'bar', 'snap_b');
        $this->assertCount(2, glob($this->storeDir . '/*.json') ?: []);

        $guard = new UpdateGuard($this->spySnapshots(), 'plugin', 'foo/foo.php', 'snap_a');
        $guard->markClean();

        $this->assertCount(
            1,
            glob($this->storeDir . '/*.json') ?: [],
            'markClean() must only clear ITS OWN type/slug marker, never an unrelated one'
        );

        UpdateInFlight::release($lockA);
        UpdateInFlight::release($lockB);
    }

    public function test_fire_still_restores_when_never_marked_clean(): void
    {
        $snapshots = $this->spySnapshots();
        $guard     = new UpdateGuard($snapshots, 'plugin', 'foo/foo.php', 'snap_x');

        $result = $guard->fire();

        $this->assertTrue($result['fired']);
        $this->assertSame([['plugin', 'foo/foo.php', 'snap_x']], $snapshots->restored);
    }

    public function test_fire_is_a_noop_after_markClean(): void
    {
        $snapshots = $this->spySnapshots();
        $guard     = new UpdateGuard($snapshots, 'plugin', 'foo/foo.php', 'snap_x');

        $guard->markClean();
        $result = $guard->fire();

        $this->assertFalse($result['fired'], 'a verified-clean guard must never restore, even if fire() is later invoked');
        $this->assertSame([], $snapshots->restored);
    }

    public function test_fire_is_idempotent(): void
    {
        $snapshots = $this->spySnapshots();
        $guard     = new UpdateGuard($snapshots, 'plugin', 'foo/foo.php', 'snap_x');

        $first  = $guard->fire();
        $second = $guard->fire();

        $this->assertTrue($first['fired']);
        $this->assertFalse($second['fired'], 'a second fire() call must be a no-op, never a second restore');
        $this->assertCount(1, $snapshots->restored);
    }
}
