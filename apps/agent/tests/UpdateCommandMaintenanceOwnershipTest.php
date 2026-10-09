<?php
/**
 * GitHub issue #893: an update request that applies nothing (every item is
 * refused, or already current) leaves a fresh `.maintenance` flag another
 * updater set exactly as it was, during the request and after it ends. A
 * stale flag is still healed, and a flag this request set during an apply is
 * still cleared when the request dies part way.
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
use WPMgr\Agent\Commands\UpdateCommand;
use WPMgr\Agent\Support\SnapshotManager;
use WPMgr\Agent\Support\UpdateRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\UpdateCommand
 * @covers \WPMgr\Agent\Support\Maintenance
 */
final class UpdateCommandMaintenanceOwnershipTest extends TestCase
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
     * @return array<string,array{0:array<int,array<string,string>>,1:array<int,string>}>
     */
    public static function requestsThatApplyNothing(): array
    {
        $invalidType = ['type' => 'widget', 'slug' => 'akismet/akismet.php', 'version' => 'latest'];
        $unsafeSlug  = ['type' => 'plugin', 'slug' => 'akismet/../../wp-config.php', 'version' => 'latest'];
        $selfTarget  = ['type' => 'plugin', 'slug' => 'wpmgr-agent/wpmgr-agent.php', 'version' => 'latest'];
        $current     = ['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest'];

        return [
            'invalid type'    => [[$invalidType], ['failed']],
            'unsafe slug'     => [[$unsafeSlug], ['failed']],
            'self-target'     => [[$selfTarget], ['skipped']],
            'already current' => [[$current], ['up_to_date']],
            'all of them'     => [[$invalidType, $unsafeSlug, $selfTarget, $current], ['failed', 'failed', 'skipped', 'up_to_date']],
        ];
    }

    /**
     * @dataProvider requestsThatApplyNothing
     *
     * @param array<int,array<string,string>> $items    Update request items.
     * @param array<int,string>               $statuses Expected per-item statuses.
     */
    public function test_an_update_that_applies_nothing_leaves_another_updaters_flag_alone(array $items, array $statuses): void
    {
        $flag = $this->writeFlag(time());

        // Seeing this run proves the shutdown phase below ran what the request
        // registered, so the assertions after it cannot pass by default.
        $sentinelRan = false;
        register_shutdown_function(static function () use (&$sentinelRan): void {
            $sentinelRan = true;
        });

        $runner = self::runner($this->maintenanceFile, null, '5.0', '');

        $out = (new UpdateCommand(self::snapshotsWithoutCapture(), $runner))->execute([], ['items' => $items]);

        $this->assertSame($statuses, array_column($out['results'], 'status'));
        $this->assertSame([], $runner->applied, 'nothing was applied');
        $this->assertSame($flag, (string) file_get_contents($this->maintenanceFile), 'the request itself leaves the flag as it was');

        $this->runShutdownCallbacks();

        $this->assertTrue($sentinelRan, 'the shutdown phase did not run what this request registered');
        $this->assertFileExists($this->maintenanceFile, 'an update that applied nothing must not clear another updater\'s flag when it ends');
        $this->assertSame($flag, (string) file_get_contents($this->maintenanceFile), 'the flag is byte for byte the one another updater wrote');
    }

    public function test_a_stale_flag_is_still_healed(): void
    {
        $this->writeFlag(time() - 600);
        touch($this->maintenanceFile, time() - 600);
        clearstatcache(true, $this->maintenanceFile);

        (new UpdateCommand(self::snapshotsWithoutCapture(), self::runner($this->maintenanceFile, null, '5.0', '')))
            ->execute([], ['items' => [['type' => 'widget', 'slug' => 'x', 'version' => 'latest']]]);

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
    public function test_a_flag_this_update_set_is_cleared_when_the_request_dies(bool $foreignFirst): void
    {
        if ($foreignFirst) {
            $this->writeFlag(time());
        }

        $runner = self::runner($this->maintenanceFile, fn (): int => $this->runShutdownCallbacks(), '5.0', '5.3');

        (new UpdateCommand(self::snapshotsWithoutCapture(), $runner))
            ->execute([], ['items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']]]);

        $this->assertSame([['plugin', 'akismet/akismet.php', 'latest']], $runner->applied, 'the item reached its apply');
        $this->assertTrue($runner->maintenanceWasSet, 'the apply put the site into maintenance mode');
        $this->assertGreaterThan(0, $runner->shutdownCallbacksRun, 'the request registered a shutdown backstop');
        $this->assertFalse($runner->maintenanceSurvivedShutdown, 'the backstop clears the flag this request set');
    }

    /**
     * Snapshot store that captures nothing, so no restore guard or in-flight
     * marker is involved.
     */
    private static function snapshotsWithoutCapture(): SnapshotManager
    {
        return new class extends SnapshotManager {
            public function __construct()
            {
            }

            public function capture(string $type, string $slug, string $fromVersion): array
            {
                return ['snapshot_id' => '', 'log' => ''];
            }
        };
    }

    /**
     * Runner double. Every plugin is installed at $installed and WordPress
     * offers $available. When $endRequest is set, apply() puts the site into
     * maintenance mode, as WordPress's upgrader does, and then reaches the
     * point where a fatal error ends the request: $endRequest runs the
     * shutdown phase, and the double records what that phase left behind.
     *
     * @param string        $maintenanceFile Path of the `.maintenance` marker.
     * @param \Closure|null $endRequest      Runs the shutdown phase and returns how many callbacks ran.
     * @param string        $installed       Installed version of every plugin.
     * @param string        $available       Version WordPress offers ('' for none).
     */
    private static function runner(string $maintenanceFile, ?\Closure $endRequest, string $installed, string $available): UpdateRunner
    {
        return new class ($maintenanceFile, $endRequest, $installed, $available) extends UpdateRunner {
            /** @var array<int,array{string,string,string}> */
            public array $applied = [];

            public bool $maintenanceWasSet = false;

            public int $shutdownCallbacksRun = 0;

            public ?bool $maintenanceSurvivedShutdown = null;

            public function __construct(
                private string $maintenanceFile,
                private ?\Closure $endRequest,
                private string $installed,
                private string $available
            ) {
            }

            public function isInstalled(string $type, string $slug): bool
            {
                return true;
            }

            public function currentVersion(string $type, string $slug): string
            {
                return $this->installed;
            }

            public function availableVersion(string $type, string $slug, string $requested): ?string
            {
                return $this->available;
            }

            public function apply(string $type, string $slug, string $version): array
            {
                $this->applied[] = [$type, $slug, $version];

                if ($this->endRequest !== null) {
                    // A later write than any flag already in place, as
                    // WordPress's own timestamp would be.
                    file_put_contents($this->maintenanceFile, '<?php $upgrading = ' . (time() + 1) . '; ?>'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- test double standing in for the upgrader entering maintenance mode
                    $this->maintenanceWasSet = file_exists($this->maintenanceFile);

                    $this->shutdownCallbacksRun = ($this->endRequest)();
                    clearstatcache(true, $this->maintenanceFile);
                    $this->maintenanceSurvivedShutdown = file_exists($this->maintenanceFile);

                    throw new \RuntimeException('the request ended here');
                }

                return ['ok' => true, 'log' => 'applied'];
            }
        };
    }
}
