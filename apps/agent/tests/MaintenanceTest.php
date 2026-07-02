<?php
/**
 * Tests for the Maintenance helper: guaranteed `.maintenance` cleanup on
 * every terminal path, and safe stale-flag healing.
 *
 * Strategy: ABSPATH is defined in bootstrap.php as
 * sys_get_temp_dir()/wpmgr_wp_abspath/site/. This test writes the real
 * `.maintenance` marker file directly under that directory (mirroring where
 * WordPress core itself puts it) and asserts on its presence/absence.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use WPMgr\Agent\Support\Maintenance;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\Maintenance
 */
final class MaintenanceTest extends TestCase
{
    /** Absolute path to the ABSPATH directory used by the test suite. */
    private string $abspathDir = '';

    /** Absolute path to the `.maintenance` file under ABSPATH. */
    private string $maintenanceFile = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $abspath = defined('ABSPATH') ? rtrim((string) constant('ABSPATH'), '/\\') : '';
        $this->abspathDir = $abspath;
        if ($abspath !== '' && !is_dir($abspath)) {
            mkdir($abspath, 0755, true);
        }
        $this->maintenanceFile = $abspath . '/.maintenance';

        // Never start a test with a stray marker from a previous test.
        $this->removeIfExists($this->maintenanceFile);
    }

    protected function tear_down(): void
    {
        $this->removeIfExists($this->maintenanceFile);
        Monkey\tearDown();
        parent::tear_down();
    }

    private function removeIfExists(string $file): void
    {
        if ($file !== '' && file_exists($file)) {
            unlink($file); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only cleanup of a fixture file
        }
    }

    /**
     * @param int $ageSeconds How far in the past to backdate mtime.
     */
    private function writeMaintenanceFile(int $ageSeconds = 0): void
    {
        file_put_contents($this->maintenanceFile, '<?php $upgrading = ' . time() . '; ?>');
        if ($ageSeconds > 0) {
            touch($this->maintenanceFile, time() - $ageSeconds);
        }
    }

    // -----------------------------------------------------------------
    // path()
    // -----------------------------------------------------------------

    public function test_path_resolves_under_abspath(): void
    {
        $this->assertSame($this->maintenanceFile, Maintenance::path());
    }

    // -----------------------------------------------------------------
    // clear()
    // -----------------------------------------------------------------

    public function test_clear_removes_an_existing_file_with_no_upgrader(): void
    {
        $this->writeMaintenanceFile();
        $this->assertFileExists($this->maintenanceFile);

        Maintenance::clear();

        $this->assertFileDoesNotExist($this->maintenanceFile);
    }

    public function test_clear_is_a_safe_noop_when_no_file_is_present(): void
    {
        $this->assertFileDoesNotExist($this->maintenanceFile);

        // Must not throw or warn.
        Maintenance::clear();

        $this->assertFileDoesNotExist($this->maintenanceFile);
    }

    public function test_clear_prefers_the_upgrader_native_call_when_available(): void
    {
        $this->writeMaintenanceFile();

        $upgrader = new class ($this->maintenanceFile) {
            /** @var array<int,bool> */
            public array $calls = [];

            public function __construct(private string $file)
            {
            }

            public function maintenance_mode(bool $enable = false): void
            {
                $this->calls[] = $enable;
                // Mirror WP_Upgrader::maintenance_mode(false): actually
                // deletes the file itself.
                if (file_exists($this->file)) {
                    unlink($this->file); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test double
                }
            }
        };

        Maintenance::clear($upgrader);

        $this->assertSame([false], $upgrader->calls, 'upgrader->maintenance_mode(false) must be invoked');
        $this->assertFileDoesNotExist($this->maintenanceFile);
    }

    public function test_clear_still_removes_file_when_upgrader_call_is_a_noop(): void
    {
        $this->writeMaintenanceFile();

        // Simulates a real-world failure mode: $wp_filesystem was never
        // connected, so the upgrader's own maintenance_mode(false) silently
        // does nothing. The direct-delete backstop must still run.
        $upgrader = new class {
            public function maintenance_mode(bool $enable = false): void
            {
                // Intentionally does nothing.
            }
        };

        Maintenance::clear($upgrader);

        $this->assertFileDoesNotExist($this->maintenanceFile);
    }

    public function test_clear_still_removes_file_when_upgrader_call_throws(): void
    {
        $this->writeMaintenanceFile();

        $upgrader = new class {
            public function maintenance_mode(bool $enable = false): void
            {
                throw new \RuntimeException('filesystem credentials unavailable');
            }
        };

        // Must not propagate the upgrader's exception.
        Maintenance::clear($upgrader);

        $this->assertFileDoesNotExist($this->maintenanceFile);
    }

    public function test_clear_ignores_an_object_without_maintenance_mode(): void
    {
        $this->writeMaintenanceFile();

        $notAnUpgrader = new class {
        };

        Maintenance::clear($notAnUpgrader);

        $this->assertFileDoesNotExist($this->maintenanceFile, 'the file-based backstop must still run');
    }

    // -----------------------------------------------------------------
    // healStaleIfPresent()
    // -----------------------------------------------------------------

    public function test_heal_removes_a_stale_flag(): void
    {
        // Well past the 90s staleness threshold.
        $this->writeMaintenanceFile(200);
        $this->assertFileExists($this->maintenanceFile);

        Maintenance::healStaleIfPresent();

        $this->assertFileDoesNotExist($this->maintenanceFile);
    }

    public function test_heal_does_not_touch_a_fresh_flag(): void
    {
        // Freshly written — well under the 90s threshold.
        $this->writeMaintenanceFile();
        $this->assertFileExists($this->maintenanceFile);

        Maintenance::healStaleIfPresent();

        $this->assertFileExists(
            $this->maintenanceFile,
            'a fresh flag may belong to another in-flight update/rollback and must not be deleted'
        );
    }

    public function test_heal_is_a_safe_noop_when_no_file_is_present(): void
    {
        $this->assertFileDoesNotExist($this->maintenanceFile);

        Maintenance::healStaleIfPresent();

        $this->assertFileDoesNotExist($this->maintenanceFile);
    }

    // -----------------------------------------------------------------
    // armShutdownGuard()
    // -----------------------------------------------------------------

    public function test_arm_shutdown_guard_does_not_throw(): void
    {
        // We cannot observe PHP's real shutdown sequence from within a
        // running test, but registering the callback must never throw and
        // must be safe to call repeatedly (every command execute() call
        // arms it).
        Maintenance::armShutdownGuard();
        Maintenance::armShutdownGuard();

        $this->addToAssertionCount(1);
    }
}
