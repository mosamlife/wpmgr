<?php
/**
 * Tests for the rollback command: response shape, snapshot restore handling,
 * core downgrade, and input validation.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use WPMgr\Agent\Commands\RollbackCommand;
use WPMgr\Agent\Support\SnapshotManager;
use WPMgr\Agent\Support\UpdateRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\RollbackCommand
 */
final class RollbackCommandTest extends TestCase
{
    /** Absolute path to the `.maintenance` marker under the test ABSPATH. */
    private string $maintenanceFile = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

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
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Snapshot spy: records restore/cleanup and returns canned outcomes.
     */
    private function spySnapshots(bool $restoreOk = true, string $recorded = ''): SnapshotManager
    {
        return new class($restoreOk, $recorded) extends SnapshotManager {
            /** @var array<int,array{string,string,string}> */
            public array $restored = [];
            /** @var array<int,string> */
            public array $cleaned = [];

            public function __construct(private bool $restoreOk, private string $recorded)
            {
            }

            public function restore(string $type, string $slug, string $snapshotId): array
            {
                $this->restored[] = [$type, $slug, $snapshotId];

                return ['ok' => $this->restoreOk, 'log' => $this->restoreOk ? 'restored' : 'restore failed'];
            }

            public function recordedVersion(string $snapshotId): string
            {
                return $this->recorded;
            }

            public function cleanup(string $snapshotId): bool
            {
                $this->cleaned[] = $snapshotId;

                return true;
            }
        };
    }

    /**
     * Runner spy: records forceCore and returns canned versions.
     */
    private function spyRunner(bool $coreOk = true): UpdateRunner
    {
        return new class($coreOk) extends UpdateRunner {
            /** @var array<int,string> */
            public array $forced = [];
            /** @var array<string,string> */
            public array $versions = [];

            public function __construct(private bool $coreOk)
            {
            }

            public function currentVersion(string $type, string $slug): string
            {
                return $this->versions[$type . ':' . $slug] ?? '';
            }

            public function forceCore(string $version): array
            {
                $this->forced[] = $version;

                return ['ok' => $this->coreOk, 'log' => $this->coreOk ? 'core forced' : 'core failed'];
            }
        };
    }

    public function test_plugin_rollback_response_shape_and_cleanup(): void
    {
        $snapshots = $this->spySnapshots(true);
        $runner    = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php'] = '5.0';

        $cmd = new RollbackCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'type'        => 'plugin',
            'slug'        => 'akismet/akismet.php',
            'snapshot_id' => 'snap_abc',
            'to_version'  => '5.0',
        ]);

        $this->assertSame(['ok', 'restored_version', 'log'], array_keys($out));
        $this->assertTrue($out['ok']);
        $this->assertSame('5.0', $out['restored_version']);
        $this->assertSame([['plugin', 'akismet/akismet.php', 'snap_abc']], $snapshots->restored);
        $this->assertSame(['snap_abc'], $snapshots->cleaned, 'snapshot must be cleaned up on success');
    }

    public function test_failed_restore_returns_ok_false_and_no_cleanup(): void
    {
        $snapshots = $this->spySnapshots(false);
        $cmd       = new RollbackCommand($snapshots, $this->spyRunner());

        $out = $cmd->execute([], [
            'type'        => 'theme',
            'slug'        => 'twentytwentyfour',
            'snapshot_id' => 'snap_abc',
            'to_version'  => '1.0',
        ]);

        $this->assertFalse($out['ok']);
        $this->assertSame('', $out['restored_version']);
        $this->assertSame([], $snapshots->cleaned);
    }

    public function test_restored_version_falls_back_to_recorded_then_to_version(): void
    {
        // currentVersion returns '' (unknown) so it should fall back to recorded.
        $snapshots = $this->spySnapshots(true, '4.9');
        $runner    = $this->spyRunner();

        $cmd = new RollbackCommand($snapshots, $runner);
        $out = $cmd->execute([], [
            'type'        => 'plugin',
            'slug'        => 'akismet',
            'snapshot_id' => 'snap_abc',
            'to_version'  => '4.8',
        ]);

        $this->assertSame('4.9', $out['restored_version']);
    }

    public function test_core_rollback_uses_force_core(): void
    {
        $snapshots = $this->spySnapshots(true);
        $runner    = $this->spyRunner(true);
        $runner->versions['core:core'] = '6.3.2';

        $cmd = new RollbackCommand($snapshots, $runner);
        $out = $cmd->execute([], [
            'type'        => 'core',
            'slug'        => 'core',
            'snapshot_id' => 'snap_core',
            'to_version'  => '6.3.2',
        ]);

        $this->assertTrue($out['ok']);
        $this->assertSame(['6.3.2'], $runner->forced);
        $this->assertSame('6.3.2', $out['restored_version']);
        $this->assertSame(['snap_core'], $snapshots->cleaned);
    }

    public function test_core_rollback_uses_recorded_version_when_to_version_absent(): void
    {
        $snapshots = $this->spySnapshots(true, '6.2.0');
        $runner    = $this->spyRunner(true);

        $cmd = new RollbackCommand($snapshots, $runner);
        $out = $cmd->execute([], [
            'type'        => 'core',
            'snapshot_id' => 'snap_core',
        ]);

        $this->assertTrue($out['ok']);
        $this->assertSame(['6.2.0'], $runner->forced);
    }

    public function test_invalid_type_is_rejected(): void
    {
        $cmd = new RollbackCommand($this->spySnapshots(), $this->spyRunner());
        $out = $cmd->execute([], ['type' => 'bogus', 'slug' => 'x', 'snapshot_id' => 'snap_a']);

        $this->assertFalse($out['ok']);
        $this->assertSame(['ok', 'restored_version', 'log'], array_keys($out));
    }

    public function test_traversal_slug_is_rejected_without_restore(): void
    {
        $snapshots = $this->spySnapshots(true);
        $cmd       = new RollbackCommand($snapshots, $this->spyRunner());

        $out = $cmd->execute([], [
            'type'        => 'plugin',
            'slug'        => '../../wp-config',
            'snapshot_id' => 'snap_a',
        ]);

        $this->assertFalse($out['ok']);
        $this->assertSame([], $snapshots->restored, 'restore must not run for unsafe slug');
    }

    public function test_missing_snapshot_id_for_plugin_is_rejected(): void
    {
        $snapshots = $this->spySnapshots(true);
        $cmd       = new RollbackCommand($snapshots, $this->spyRunner());

        $out = $cmd->execute([], ['type' => 'plugin', 'slug' => 'akismet']);

        $this->assertFalse($out['ok']);
        $this->assertSame([], $snapshots->restored);
    }

    public function test_core_rollback_rejects_invalid_target_version(): void
    {
        $runner = $this->spyRunner(true);
        $cmd    = new RollbackCommand($this->spySnapshots(true), $runner);

        $out = $cmd->execute([], ['type' => 'core', 'to_version' => 'not a version; rm -rf']);

        $this->assertFalse($out['ok']);
        $this->assertSame([], $runner->forced, 'forceCore must not run for invalid version');
    }

    public function test_command_name(): void
    {
        $this->assertSame('rollback', (new RollbackCommand())->name());
    }

    // ---- .maintenance guarantee (GitHub issue #127) ------------------------

    /**
     * Pre-create a `.maintenance` marker as if left behind by the earlier
     * update this rollback is recovering from.
     */
    private function preCreateMaintenanceFlag(): void
    {
        file_put_contents($this->maintenanceFile, '<?php $upgrading = ' . time() . '; ?>');
    }

    public function test_maintenance_flag_is_removed_after_a_failed_plugin_rollback(): void
    {
        $this->preCreateMaintenanceFlag();

        $cmd = new RollbackCommand($this->spySnapshots(false), $this->spyRunner());
        $out = $cmd->execute([], [
            'type'        => 'plugin',
            'slug'        => 'akismet/akismet.php',
            'snapshot_id' => 'snap_abc',
        ]);

        $this->assertFalse($out['ok']);
        $this->assertFileDoesNotExist(
            $this->maintenanceFile,
            'a rollback that itself fails must not leave the site permanently in maintenance mode'
        );
    }

    public function test_maintenance_flag_is_removed_after_a_successful_plugin_rollback(): void
    {
        $this->preCreateMaintenanceFlag();

        $cmd = new RollbackCommand($this->spySnapshots(true), $this->spyRunner());
        $out = $cmd->execute([], [
            'type'        => 'plugin',
            'slug'        => 'akismet/akismet.php',
            'snapshot_id' => 'snap_abc',
        ]);

        $this->assertTrue($out['ok']);
        $this->assertFileDoesNotExist($this->maintenanceFile);
    }

    public function test_maintenance_flag_is_removed_after_a_failed_core_rollback(): void
    {
        $this->preCreateMaintenanceFlag();

        $runner = $this->spyRunner(false);
        $cmd    = new RollbackCommand($this->spySnapshots(true), $runner);

        $out = $cmd->execute([], [
            'type'        => 'core',
            'snapshot_id' => 'snap_core',
            'to_version'  => '6.3.2',
        ]);

        $this->assertFalse($out['ok']);
        $this->assertSame(['6.3.2'], $runner->forced);
        $this->assertFileDoesNotExist(
            $this->maintenanceFile,
            'exactly the reported incident: rollback FAILED must not leave the site at 503'
        );
    }

    public function test_maintenance_flag_is_removed_after_a_successful_core_rollback(): void
    {
        $this->preCreateMaintenanceFlag();

        $runner = $this->spyRunner(true);
        $runner->versions['core:core'] = '6.3.2';
        $cmd = new RollbackCommand($this->spySnapshots(true), $runner);

        $out = $cmd->execute([], [
            'type'        => 'core',
            'snapshot_id' => 'snap_core',
            'to_version'  => '6.3.2',
        ]);

        $this->assertTrue($out['ok']);
        $this->assertFileDoesNotExist($this->maintenanceFile);
    }

    public function test_stale_maintenance_flag_is_healed_even_for_a_rejected_request(): void
    {
        file_put_contents($this->maintenanceFile, '<?php $upgrading = ' . time() . '; ?>');
        // Well past Maintenance's staleness threshold.
        touch($this->maintenanceFile, time() - 200);

        // An invalid type is rejected BEFORE the mutating try/finally runs,
        // isolating the start-of-request healStaleIfPresent() proactive heal
        // from the unconditional end-of-request clear().
        $cmd = new RollbackCommand($this->spySnapshots(), $this->spyRunner());
        $out = $cmd->execute([], ['type' => 'bogus', 'slug' => 'x', 'snapshot_id' => 'snap_a']);

        $this->assertFalse($out['ok']);
        $this->assertFileDoesNotExist(
            $this->maintenanceFile,
            'a stale flag from a prior interrupted run must be healed before new work starts'
        );
    }

    public function test_fresh_maintenance_flag_is_not_touched_by_a_rejected_request(): void
    {
        file_put_contents($this->maintenanceFile, '<?php $upgrading = ' . time() . '; ?>');
        // Freshly written — well under the staleness threshold.

        $cmd = new RollbackCommand($this->spySnapshots(), $this->spyRunner());
        $out = $cmd->execute([], ['type' => 'bogus', 'slug' => 'x', 'snapshot_id' => 'snap_a']);

        $this->assertFalse($out['ok']);
        $this->assertFileExists(
            $this->maintenanceFile,
            'a fresh flag may belong to another in-flight update/rollback and must not be deleted'
        );
    }
}
