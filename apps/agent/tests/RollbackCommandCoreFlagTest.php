<?php
/**
 * GitHub issue #415: rolling back WordPress core is a forced downgrade, so the
 * agent performs one only when the request asks for it explicitly with
 * `allow_core_downgrade: true`. Plugin and theme rollbacks are unchanged.
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
 */
final class RollbackCommandCoreFlagTest extends TestCase
{
    /** The version WordPress loaded into memory when the request began. */
    private const IN_MEMORY = '7.1';

    /** Absolute path to the `.maintenance` marker under the test ABSPATH. */
    private string $maintenanceFile = '';

    /** Absolute path of <ABSPATH>/wp-includes. */
    private string $includesDir = '';

    /** Absolute path of <ABSPATH>/wp-includes/version.php. */
    private string $versionFile = '';

    /** Whether set_up() had to create wp-includes (so tear_down() removes it). */
    private bool $createdIncludesDir = false;

    /** Whether $GLOBALS['wp_version'] existed before this test. */
    private bool $hadGlobal = false;

    /** @var mixed The pre-test $GLOBALS['wp_version'], restored afterwards. */
    private $savedGlobal = null;

    /** @var array<int,string> Every delete_site_transient() key, in order. */
    private array $deletedTransients = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $abspath = rtrim((string) constant('ABSPATH'), '/\\');
        $this->assertNotSame('', $abspath, 'the test ABSPATH must be defined and non-empty');

        $this->maintenanceFile = $abspath . '/.maintenance';
        if (file_exists($this->maintenanceFile)) {
            unlink($this->maintenanceFile); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test fixture cleanup
        }

        $this->includesDir = $abspath . '/wp-includes';
        $this->versionFile = $this->includesDir . '/version.php';
        if (!is_dir($this->includesDir)) {
            mkdir($this->includesDir, 0755, true);
            $this->createdIncludesDir = true;
        }
        $this->removeVersionFile();

        $this->hadGlobal       = array_key_exists('wp_version', $GLOBALS);
        $this->savedGlobal     = $GLOBALS['wp_version'] ?? null;
        $GLOBALS['wp_version'] = self::IN_MEMORY;
        Functions\when('get_bloginfo')->alias(
            static fn ($show = '') => $show === 'version' ? $GLOBALS['wp_version'] : ''
        );

        $this->deletedTransients = [];
        Functions\when('delete_site_transient')->alias(function (string $key): bool {
            $this->deletedTransients[] = $key;
            return true;
        });
    }

    protected function tear_down(): void
    {
        if ($this->maintenanceFile !== '' && file_exists($this->maintenanceFile)) {
            unlink($this->maintenanceFile); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test fixture cleanup
        }

        $this->removeVersionFile();
        if ($this->createdIncludesDir && is_dir($this->includesDir)) {
            @rmdir($this->includesDir); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- test fixture cleanup; another test may have put files here since
        }

        if ($this->hadGlobal) {
            $GLOBALS['wp_version'] = $this->savedGlobal;
        } else {
            unset($GLOBALS['wp_version']);
        }

        Monkey\tearDown();
        parent::tear_down();
    }

    private function removeVersionFile(): void
    {
        if ($this->versionFile !== '' && file_exists($this->versionFile)) {
            unlink($this->versionFile); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test fixture cleanup
        }
    }

    /**
     * Snapshot spy: records restore/cleanup and returns canned outcomes.
     */
    private static function spySnapshots(string $recorded = ''): SnapshotManager
    {
        return new class ($recorded) extends SnapshotManager {
            /** @var array<int,array{string,string,string}> */
            public array $restored = [];
            /** @var array<int,string> */
            public array $cleaned = [];

            public function __construct(private string $recorded)
            {
            }

            public function restore(string $type, string $slug, string $snapshotId): array
            {
                $this->restored[] = [$type, $slug, $snapshotId];

                return ['ok' => true, 'log' => 'restored'];
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
     * The real UpdateRunner with only forceCore() replaced. Like a real core
     * downgrade, the fake rewrites wp-includes/version.php on disk and leaves
     * the in-memory $wp_version alone. currentVersion() is the real one.
     *
     * @param string $versionFile Path the fake downgrade writes.
     */
    private static function runnerRecordingForceCore(string $versionFile): UpdateRunner
    {
        return new class ($versionFile) extends UpdateRunner {
            /** @var array<int,string> */
            public array $forced = [];

            public function __construct(private string $versionFile)
            {
            }

            public function forceCore(string $version): array
            {
                $this->forced[] = $version;

                file_put_contents( // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- test double standing in for a core downgrade
                    $this->versionFile,
                    "<?php\n\$wp_version = '" . $version . "';\n"
                );

                return ['ok' => true, 'log' => 'core forced to ' . $version];
            }
        };
    }

    /**
     * @return array<string,array{0:array<string,mixed>}>
     */
    public static function coreRollbackRequestsWithoutPermission(): array
    {
        $base = ['type' => 'core', 'slug' => 'core', 'snapshot_id' => 'snap_core', 'to_version' => '7.0'];

        return [
            'flag absent'          => [$base],
            'flag false'           => [$base + ['allow_core_downgrade' => false]],
            'flag null'            => [$base + ['allow_core_downgrade' => null]],
            'flag string "true"'   => [$base + ['allow_core_downgrade' => 'true']],
            'flag string "1"'      => [$base + ['allow_core_downgrade' => '1']],
            'flag string "false"'  => [$base + ['allow_core_downgrade' => 'false']],
            'flag integer 1'       => [$base + ['allow_core_downgrade' => 1]],
            'flag array'           => [$base + ['allow_core_downgrade' => [true]]],
            'flag absent, no slug' => [['type' => 'core', 'to_version' => '7.0']],
        ];
    }

    /**
     * @dataProvider coreRollbackRequestsWithoutPermission
     *
     * @param array<string,mixed> $params Rollback request body.
     */
    public function test_a_core_rollback_without_explicit_permission_is_refused_and_changes_nothing(array $params): void
    {
        file_put_contents( // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- test fixture
            $this->versionFile,
            "<?php\n\$wp_version = '" . self::IN_MEMORY . "';\n"
        );

        $runner    = self::runnerRecordingForceCore($this->versionFile);
        $snapshots = self::spySnapshots('7.0');
        $cmd       = new RollbackCommand($snapshots, $runner);

        $out = $cmd->execute([], $params);

        $this->assertSame(['ok', 'restored_version', 'log'], array_keys($out));
        $this->assertFalse($out['ok']);
        $this->assertSame('', $out['restored_version']);
        $this->assertStringContainsString('allow_core_downgrade', $out['log']);
        $this->assertStringContainsString('Nothing was attempted', $out['log']);

        $this->assertSame([], $runner->forced, 'forceCore must never run without explicit permission');
        $this->assertSame([], $snapshots->cleaned, 'a refused rollback must not consume the snapshot');
        $this->assertSame([], $this->deletedTransients, 'a refused rollback must not touch update_core');
        $this->assertStringContainsString("'" . self::IN_MEMORY . "'", (string) file_get_contents($this->versionFile));
    }

    public function test_a_refused_core_rollback_leaves_a_fresh_maintenance_flag_alone(): void
    {
        file_put_contents($this->maintenanceFile, '<?php $upgrading = ' . time() . '; ?>'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- test fixture

        $runner = self::runnerRecordingForceCore($this->versionFile);
        $cmd    = new RollbackCommand(self::spySnapshots(), $runner);

        $out = $cmd->execute([], ['type' => 'core', 'to_version' => '7.0']);

        $this->assertFalse($out['ok']);
        $this->assertSame([], $runner->forced);
        $this->assertFileExists(
            $this->maintenanceFile,
            'a refused request changes nothing; a fresh flag may belong to an update still in flight'
        );
    }

    public function test_a_core_rollback_with_explicit_permission_forces_core_to_the_requested_version(): void
    {
        file_put_contents( // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- test fixture
            $this->versionFile,
            "<?php\n\$wp_version = '" . self::IN_MEMORY . "';\n"
        );

        $runner    = self::runnerRecordingForceCore($this->versionFile);
        $snapshots = self::spySnapshots();
        $cmd       = new RollbackCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'type'                 => 'core',
            'slug'                 => 'core',
            'snapshot_id'          => 'snap_core',
            'to_version'           => '7.0',
            'allow_core_downgrade' => true,
        ]);

        $this->assertTrue($out['ok']);
        $this->assertSame(['7.0'], $runner->forced);
        $this->assertSame(['snap_core'], $snapshots->cleaned);
        $this->assertSame(['update_core'], $this->deletedTransients);
    }

    /**
     * The same stale-global fault as the update path: the version reported
     * after a core downgrade is the one now on disk, not the one WordPress
     * loaded when the request began.
     */
    public function test_a_permitted_core_rollback_reports_the_version_now_on_disk(): void
    {
        file_put_contents( // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- test fixture
            $this->versionFile,
            "<?php\n\$wp_version = '" . self::IN_MEMORY . "';\n"
        );

        $runner = self::runnerRecordingForceCore($this->versionFile);
        $cmd    = new RollbackCommand(self::spySnapshots(), $runner);

        $out = $cmd->execute([], ['type' => 'core', 'to_version' => '7.0', 'allow_core_downgrade' => true]);

        $this->assertTrue($out['ok']);
        $this->assertSame(self::IN_MEMORY, get_bloginfo('version'), 'memory still says 7.1');
        $this->assertSame('7.0', $out['restored_version']);
    }

    public function test_a_permitted_core_rollback_still_rejects_an_invalid_target_version(): void
    {
        $runner = self::runnerRecordingForceCore($this->versionFile);
        $cmd    = new RollbackCommand(self::spySnapshots(), $runner);

        $out = $cmd->execute([], [
            'type'                 => 'core',
            'to_version'           => '7.0 --activate',
            'allow_core_downgrade' => true,
        ]);

        $this->assertFalse($out['ok']);
        $this->assertSame([], $runner->forced);
    }

    /**
     * @return array<string,array{0:string,1:string,2:string}>
     */
    public static function pluginAndThemeTargets(): array
    {
        return [
            'plugin' => ['plugin', 'akismet/akismet.php', 'update_plugins'],
            'theme'  => ['theme', 'twentytwentyfour', 'update_themes'],
        ];
    }

    /**
     * @dataProvider pluginAndThemeTargets
     *
     * @param string $type      plugin|theme.
     * @param string $slug      Target slug.
     * @param string $transient The update transient a successful restore clears.
     */
    public function test_plugin_and_theme_rollbacks_do_not_need_the_flag(string $type, string $slug, string $transient): void
    {
        // The real currentVersion() reads the restored item back through
        // these. Stubbed here so this test never depends on what another
        // test left declared.
        Functions\when('get_plugins')->justReturn([
            'akismet/akismet.php' => ['Name' => 'Akismet', 'Version' => '4.9'],
        ]);
        $theme = new class {
            /**
             * @param string $header Header name.
             * @return string
             */
            public function get($header): string
            {
                return $header === 'Version' ? '4.9' : '';
            }
        };
        Functions\when('wp_get_themes')->justReturn(['twentytwentyfour' => $theme]);

        $runner    = self::runnerRecordingForceCore($this->versionFile);
        $snapshots = self::spySnapshots();
        $cmd       = new RollbackCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'type'        => $type,
            'slug'        => $slug,
            'snapshot_id' => 'snap_abc',
            'to_version'  => '4.8',
        ]);

        $this->assertTrue($out['ok'], $out['log']);
        $this->assertSame('4.9', $out['restored_version']);
        $this->assertSame([[$type, $slug, 'snap_abc']], $snapshots->restored);
        $this->assertSame(['snap_abc'], $snapshots->cleaned);
        $this->assertSame([$transient], $this->deletedTransients);
        $this->assertSame([], $runner->forced);
    }
}
