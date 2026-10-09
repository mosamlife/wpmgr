<?php
/**
 * GitHub issue #893: a rollback request that is turned down (unsafe slug,
 * missing snapshot id, the agent itself, no valid core target) leaves a fresh
 * `.maintenance` flag another updater set exactly as it was, during the
 * request and after it ends. A stale flag is still healed, and a flag this
 * request set is still cleared when the request dies part way.
 *
 * The request's shutdown callbacks are captured and run after execute()
 * returns, so the shutdown phase is part of every run here.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\RollbackCommand;
use WPMgr\Agent\Support\SnapshotManager;
use WPMgr\Agent\Support\UpdateRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\RollbackCommand
 * @covers \WPMgr\Agent\Support\Maintenance
 */
final class RollbackCommandMaintenanceOwnershipTest extends TestCase
{
    /** Absolute path to the `.maintenance` marker under the test ABSPATH. */
    private string $maintenanceFile = '';

    /** @var array<int,array{0:callable,1:array<int,mixed>}> Captured shutdown callbacks, in order. */
    private array $shutdownCallbacks = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->shutdownCallbacks = [];

        $abspath = rtrim((string) constant('ABSPATH'), '/\\');
        $this->assertNotSame('', $abspath, 'the test ABSPATH must be defined and non-empty');
        if (!is_dir($abspath)) {
            mkdir($abspath, 0755, true);
        }
        $this->maintenanceFile = $abspath . '/.maintenance';
        $this->removeFlag();

        Functions\when('delete_site_transient')->justReturn(true);
        Functions\when('register_shutdown_function')->alias(
            function (callable $callback, ...$args): void {
                $this->shutdownCallbacks[] = [$callback, $args];
            }
        );
    }

    protected function tear_down(): void
    {
        $this->removeFlag();
        Monkey\tearDown();
        parent::tear_down();
    }

    private function removeFlag(): void
    {
        if ($this->maintenanceFile !== '' && file_exists($this->maintenanceFile)) {
            unlink($this->maintenanceFile); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test fixture cleanup
        }
    }

    /**
     * Put the site into maintenance mode the way another updater does.
     *
     * @param int $at The timestamp WordPress writes into the flag.
     * @return string The flag's bytes.
     */
    private function writeFlag(int $at): string
    {
        $flag = '<?php $upgrading = ' . $at . '; ?>';
        file_put_contents($this->maintenanceFile, $flag); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- test fixture

        return $flag;
    }

    /**
     * End the request the way PHP does: run every captured shutdown callback
     * in registration order.
     *
     * @return int How many callbacks ran.
     */
    private function runShutdownCallbacks(): int
    {
        $ran = 0;
        while ($this->shutdownCallbacks !== []) {
            [$callback, $args] = array_shift($this->shutdownCallbacks);
            $callback(...$args);
            ++$ran;
        }

        return $ran;
    }

    /**
     * @return array<string,array{0:array<string,mixed>}>
     */
    public static function turnedDownRequests(): array
    {
        return [
            'unsafe slug'         => [['type' => 'plugin', 'slug' => 'akismet/../../wp-config.php', 'snapshot_id' => 'snap_1']],
            'missing snapshot_id' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php']],
            'self-target'         => [['type' => 'plugin', 'slug' => 'wpmgr-agent/wpmgr-agent.php', 'snapshot_id' => 'snap_1']],
            'core, bad target'    => [['type' => 'core', 'slug' => 'core', 'allow_core_downgrade' => true, 'to_version' => 'not a version']],
        ];
    }

    /**
     * @dataProvider turnedDownRequests
     *
     * @param array<string,mixed> $params Rollback request body.
     */
    public function test_a_turned_down_rollback_leaves_another_updaters_flag_alone(array $params): void
    {
        $flag = $this->writeFlag(time());

        // Seeing this run proves the shutdown phase below ran what the request
        // registered, so the assertions after it cannot pass by default.
        $sentinelRan = false;
        register_shutdown_function(static function () use (&$sentinelRan): void {
            $sentinelRan = true;
        });

        $snapshots = self::recordingSnapshots();
        $runner    = self::runnerRecordingForceCore();

        $out = (new RollbackCommand($snapshots, $runner))->execute([], $params);

        $this->assertFalse($out['ok'], 'the request is turned down');
        $this->assertSame([], $runner->forced, 'no core downgrade ran');
        $this->assertSame([], $snapshots->restored, 'the snapshot store is never reached');
        $this->assertSame($flag, (string) file_get_contents($this->maintenanceFile), 'the request itself leaves the flag as it was');

        $this->runShutdownCallbacks();

        $this->assertTrue($sentinelRan, 'the shutdown phase did not run what this request registered');
        $this->assertFileExists($this->maintenanceFile, 'a turned-down request must not clear another updater\'s flag when it ends');
        $this->assertSame($flag, (string) file_get_contents($this->maintenanceFile), 'the flag is byte for byte the one another updater wrote');
    }

    public function test_a_stale_flag_is_still_healed(): void
    {
        $this->writeFlag(time() - 600);
        touch($this->maintenanceFile, time() - 600);
        clearstatcache(true, $this->maintenanceFile);

        $out = (new RollbackCommand(self::recordingSnapshots(), self::runnerRecordingForceCore()))
            ->execute([], ['type' => 'plugin', 'slug' => 'akismet/akismet.php']);

        $this->assertFalse($out['ok']);
        $this->assertFileDoesNotExist($this->maintenanceFile, 'a flag older than the stale threshold is healed');
    }

    /**
     * @return array<string,array{0:bool}>
     */
    public static function flagInPlaceWhenArmed(): array
    {
        return [
            'no flag when armed'              => [false],
            'another updater\'s flag when armed' => [true],
        ];
    }

    /**
     * @dataProvider flagInPlaceWhenArmed
     *
     * @param bool $foreignFirst Whether another updater's fresh flag is in place when the request arms.
     */
    public function test_a_flag_this_rollback_set_is_cleared_when_the_request_dies(bool $foreignFirst): void
    {
        if ($foreignFirst) {
            $this->writeFlag(time());
        }

        $snapshots = self::snapshotsDyingMidRestore(
            $this->maintenanceFile,
            fn (): int => $this->runShutdownCallbacks()
        );

        (new RollbackCommand($snapshots, self::runnerRecordingForceCore()))
            ->execute([], ['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'snapshot_id' => 'snap_1']);

        $this->assertTrue($snapshots->maintenanceWasSet, 'the restore put the site into maintenance mode');
        $this->assertGreaterThan(0, $snapshots->shutdownCallbacksRun, 'the request registered a shutdown backstop');
        $this->assertFalse($snapshots->maintenanceSurvivedShutdown, 'the backstop clears the flag this request set');
    }

    /**
     * Snapshot store that records restore() calls; a turned-down request
     * never makes one.
     */
    private static function recordingSnapshots(): SnapshotManager
    {
        return new class extends SnapshotManager {
            /** @var array<int,array{string,string,string}> */
            public array $restored = [];

            public function __construct()
            {
            }

            public function restore(string $type, string $slug, string $snapshotId): array
            {
                $this->restored[] = [$type, $slug, $snapshotId];

                return ['ok' => false, 'log' => 'Snapshot not found.'];
            }

            public function recordedVersion(string $snapshotId): string
            {
                return '';
            }

            public function cleanup(string $snapshotId): bool
            {
                return true;
            }
        };
    }

    /**
     * Snapshot store whose restore() puts the site into maintenance mode, as
     * this request's own work would, and then reaches the point where a fatal
     * error ends the request: $endRequest runs the shutdown phase, and the
     * fake records what that phase left behind.
     *
     * @param string   $maintenanceFile Path of the `.maintenance` marker.
     * @param \Closure $endRequest      Runs the shutdown phase and returns how many callbacks ran.
     */
    private static function snapshotsDyingMidRestore(string $maintenanceFile, \Closure $endRequest): SnapshotManager
    {
        return new class ($maintenanceFile, $endRequest) extends SnapshotManager {
            public bool $maintenanceWasSet = false;

            public int $shutdownCallbacksRun = 0;

            public ?bool $maintenanceSurvivedShutdown = null;

            public function __construct(private string $maintenanceFile, private \Closure $endRequest)
            {
            }

            public function restore(string $type, string $slug, string $snapshotId): array
            {
                // A later write than any flag already in place, as WordPress's
                // own timestamp would be.
                file_put_contents($this->maintenanceFile, '<?php $upgrading = ' . (time() + 1) . '; ?>'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- test double standing in for this request entering maintenance mode
                $this->maintenanceWasSet = file_exists($this->maintenanceFile);

                $this->shutdownCallbacksRun = ($this->endRequest)();
                clearstatcache(true, $this->maintenanceFile);
                $this->maintenanceSurvivedShutdown = file_exists($this->maintenanceFile);

                throw new \RuntimeException('the request ended here');
            }

            public function recordedVersion(string $snapshotId): string
            {
                return '';
            }

            public function cleanup(string $snapshotId): bool
            {
                return true;
            }
        };
    }

    /**
     * The real UpdateRunner with only forceCore() replaced, recording calls.
     */
    private static function runnerRecordingForceCore(): UpdateRunner
    {
        return new class extends UpdateRunner {
            /** @var array<int,string> */
            public array $forced = [];

            public function __construct()
            {
            }

            public function forceCore(string $version): array
            {
                $this->forced[] = $version;

                return ['ok' => true, 'log' => 'core forced to ' . $version];
            }
        };
    }
}
