<?php
/**
 * RollbackCommandSiteLockTest - the rollback participates in the SAME per-site
 * update lock as the update command (GitHub issue #328).
 *
 * A rollback replaces the live directory wholesale, so it is the same class of
 * writer as an update. Running one while another request is driving WordPress's
 * upgrader against wp-content/upgrade/ is exactly the concurrency this lock
 * exists to remove.
 *
 * A refused rollback does NOT wait: RollbackCommand answers on the control
 * plane's own HTTP connection, so polling here would pin a PHP worker for the
 * whole of somebody else's apply. It refuses honestly, using the response's
 * existing fields.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\RollbackCommand;
use WPMgr\Agent\Support\SiteUpdateLock;
use WPMgr\Agent\Support\SnapshotManager;
use WPMgr\Agent\Support\UpdateRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\RollbackCommand
 */
final class RollbackCommandSiteLockTest extends TestCase
{
    /** @var array<string,mixed> In-memory wp-option store. */
    private array $options = [];

    /** Absolute path to the `.maintenance` marker under the test ABSPATH. */
    private string $maintenanceFile = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->options = [];
        \WP_Upgrader::$failCreateLock = false;
        SiteUpdateLock::resetForTests();

        Functions\when('get_option')->alias(function (string $name, $default = false) {
            return $this->options[$name] ?? $default;
        });
        Functions\when('update_option')->alias(function (string $name, $value) {
            $this->options[$name] = $value;
            return true;
        });
        Functions\when('delete_option')->alias(function (string $name) {
            unset($this->options[$name]);
            return true;
        });
        Functions\when('delete_site_transient')->justReturn(true);

        $abspath = defined('ABSPATH') ? rtrim((string) constant('ABSPATH'), '/\\') : '';
        if ($abspath !== '' && !is_dir($abspath)) {
            mkdir($abspath, 0755, true);
        }
        $this->maintenanceFile = $abspath . '/.maintenance';
        if (file_exists($this->maintenanceFile)) {
            unlink($this->maintenanceFile); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
        }
    }

    protected function tear_down(): void
    {
        if ($this->maintenanceFile !== '' && file_exists($this->maintenanceFile)) {
            unlink($this->maintenanceFile); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
        }
        SiteUpdateLock::resetForTests();
        \WP_Upgrader::$failCreateLock = false;
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Snapshot spy recording whether restore() was ever reached.
     *
     * @return SnapshotManager
     */
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

            public function recordedVersion(string $snapshotId): string
            {
                return '1.0.0';
            }

            public function cleanup(string $snapshotId): bool
            {
                return true;
            }
        };
    }

    /**
     * Runner spy.
     *
     * @return UpdateRunner
     */
    private function spyRunner(): UpdateRunner
    {
        return new class extends UpdateRunner {
            public function currentVersion(string $type, string $slug): string
            {
                return '1.0.0';
            }
        };
    }

    public function test_a_busy_site_refuses_the_rollback_and_never_reaches_restore(): void
    {
        // Another request holds the lock.
        $this->assertSame(SiteUpdateLock::ACQUIRED, SiteUpdateLock::acquire());
        SiteUpdateLock::resetForTests();

        $snapshots = $this->spySnapshots();

        $out = (new RollbackCommand($snapshots, $this->spyRunner()))->execute([], [
            'type'        => 'plugin',
            'slug'        => 'akismet/akismet.php',
            'snapshot_id' => 'snap_x',
        ]);

        $this->assertFalse($out['ok']);
        $this->assertSame('', $out['restored_version']);
        $this->assertStringContainsString('Nothing was attempted', $out['log']);
        $this->assertSame([], $snapshots->restored, 'restore() must be unreachable while the site is busy');
        $this->assertSame(
            ['ok', 'restored_version', 'log'],
            array_keys($out),
            'the refusal must use the existing response shape; no new wire fields'
        );
    }

    /** A refused rollback must not heal or arm anything either. */
    public function test_a_busy_refusal_does_not_touch_the_maintenance_flag(): void
    {
        file_put_contents($this->maintenanceFile, '<?php $upgrading = ' . (time() - 400) . '; ?>');

        $this->assertSame(SiteUpdateLock::ACQUIRED, SiteUpdateLock::acquire());
        SiteUpdateLock::resetForTests();

        (new RollbackCommand($this->spySnapshots(), $this->spyRunner()))->execute([], [
            'type'        => 'plugin',
            'slug'        => 'akismet/akismet.php',
            'snapshot_id' => 'snap_x',
        ]);

        $this->assertFileExists($this->maintenanceFile);
    }

    public function test_a_free_site_rolls_back_and_releases_the_lock(): void
    {
        $snapshots = $this->spySnapshots();

        $out = (new RollbackCommand($snapshots, $this->spyRunner()))->execute([], [
            'type'        => 'plugin',
            'slug'        => 'akismet/akismet.php',
            'snapshot_id' => 'snap_x',
        ]);

        $this->assertTrue($out['ok']);
        $this->assertCount(1, $snapshots->restored);
        $this->assertArrayNotHasKey(
            'wpmgr_site_update.lock',
            $this->options,
            'the rollback must not leave the site locked for its full 15-minute TTL'
        );
    }

    /** Even an invalid request must release the lock it took. */
    public function test_an_invalid_request_still_releases_the_lock(): void
    {
        $out = (new RollbackCommand($this->spySnapshots(), $this->spyRunner()))->execute([], [
            'type' => 'not-a-type',
        ]);

        $this->assertFalse($out['ok']);
        $this->assertArrayNotHasKey('wpmgr_site_update.lock', $this->options);
    }
}
