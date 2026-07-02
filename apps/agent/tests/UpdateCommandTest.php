<?php
/**
 * Tests for the update command: dry-run safety, response shape, version
 * detection, and slug sanitization.
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
 */
final class UpdateCommandTest extends TestCase
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
     * A runner spy that records calls and never touches the filesystem.
     */
    private function spyRunner(): UpdateRunner
    {
        return new class extends UpdateRunner {
            /** @var array<int,array{string,string,string}> */
            public array $applied = [];
            /** @var array<string,string> */
            public array $versions = [];
            /** @var array<string,string> */
            public array $available = [];
            /** @var array<string,bool> Explicit installed-state overrides, keyed "type:slug". */
            public array $installedOverride = [];
            /** Whether apply() should report success. */
            public bool $applySucceeds = true;

            public function currentVersion(string $type, string $slug): string
            {
                return $this->versions[$type . ':' . $slug] ?? '';
            }

            public function isInstalled(string $type, string $slug): bool
            {
                $key = $type . ':' . $slug;
                if (array_key_exists($key, $this->installedOverride)) {
                    return $this->installedOverride[$key];
                }

                // Default: known-installed only when the spy has been given a
                // version for it. Deliberately does NOT fall back to the real
                // UpdateRunner::isInstalled() here — that would consult the
                // process-wide get_plugins()/wp_get_themes() stub state, which
                // another test in this suite may have already redefined via
                // Brain Monkey (those redefinitions are not un-done between
                // tests), making this spy's behaviour depend on test order.
                return array_key_exists($key, $this->versions);
            }

            public function availableVersion(string $type, string $slug, string $requested): string
            {
                return $this->available[$type . ':' . $slug] ?? ($requested !== 'latest' ? $requested : '');
            }

            public function apply(string $type, string $slug, string $version): array
            {
                $this->applied[] = [$type, $slug, $version];

                if (!$this->applySucceeds) {
                    return ['ok' => false, 'log' => 'upgrader reported failure'];
                }

                // Simulate a successful bump.
                $this->versions[$type . ':' . $slug] = $version === 'latest' ? '9.9.9' : $version;

                return ['ok' => true, 'log' => 'applied'];
            }

            public function wpCliAvailable(): bool
            {
                return false;
            }
        };
    }

    /**
     * A snapshot spy that records captures without writing to disk.
     */
    private function spySnapshots(): SnapshotManager
    {
        return new class extends SnapshotManager {
            /** @var array<int,array{string,string,string}> */
            public array $captured = [];

            public function capture(string $type, string $slug, string $fromVersion): array
            {
                $this->captured[] = [$type, $slug, $fromVersion];

                return ['snapshot_id' => 'snap_test123', 'log' => 'captured'];
            }
        };
    }

    public function test_dry_run_performs_no_mutation_and_reports_would_update(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php'] = '5.0';
        $runner->available['plugin:akismet/akismet.php'] = '5.3';

        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'dry_run'  => true,
            'snapshot' => true,
            'items'    => [
                ['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest'],
            ],
        ]);

        // No upgrade and no snapshot may be performed in dry-run.
        $this->assertSame([], $runner->applied, 'apply() must not be invoked in dry-run');
        $this->assertSame([], $snapshots->captured, 'capture() must not be invoked in dry-run');

        $this->assertTrue($out['ok']);
        $this->assertCount(1, $out['results']);
        $r = $out['results'][0];
        $this->assertSame('would_update', $r['status']);
        $this->assertSame('5.0', $r['from_version']);
        $this->assertSame('5.3', $r['to_version']);
        $this->assertSame('', $r['snapshot_id']);
    }

    public function test_dry_run_reports_up_to_date_when_no_update_available(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:hello/hello.php'] = '1.7.2';
        // No 'available' entry => no newer version offered.

        $cmd = new UpdateCommand($this->spySnapshots(), $runner);

        $out = $cmd->execute([], [
            'dry_run' => true,
            'items'   => [['type' => 'plugin', 'slug' => 'hello/hello.php', 'version' => 'latest']],
        ]);

        $this->assertSame('up_to_date', $out['results'][0]['status']);
        $this->assertSame([], $runner->applied);
    }

    public function test_response_shape_matches_contract_exactly(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php'] = '5.0';

        $cmd = new UpdateCommand($this->spySnapshots(), $runner);

        $out = $cmd->execute([], [
            'dry_run'  => false,
            'snapshot' => true,
            'items'    => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => '5.3']],
        ]);

        $this->assertSame(['ok', 'results'], array_keys($out));
        $this->assertCount(1, $out['results']);
        $this->assertSame(
            ['type', 'slug', 'from_version', 'to_version', 'status', 'snapshot_id', 'log'],
            array_keys($out['results'][0])
        );
    }

    public function test_apply_succeeds_and_captures_snapshot(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php'] = '5.0';

        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'snapshot' => true,
            'items'    => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => '5.3']],
        ]);

        $r = $out['results'][0];
        $this->assertSame('succeeded', $r['status']);
        $this->assertSame('5.0', $r['from_version']);
        $this->assertSame('5.3', $r['to_version']);
        $this->assertSame('snap_test123', $r['snapshot_id']);
        $this->assertCount(1, $runner->applied);
        $this->assertCount(1, $snapshots->captured);
    }

    public function test_core_version_detection_uses_bloginfo(): void
    {
        Functions\when('get_bloginfo')->alias(static fn ($k) => $k === 'version' ? '6.4.3' : '');

        $runner = new UpdateRunner();
        $this->assertSame('6.4.3', $runner->currentVersion('core', 'core'));
    }

    public function test_plugin_version_detection_from_get_plugins(): void
    {
        Functions\when('get_plugins')->justReturn([
            'akismet/akismet.php' => ['Name' => 'Akismet', 'Version' => '5.3'],
        ]);

        $runner = new UpdateRunner();
        $this->assertSame('5.3', $runner->currentVersion('plugin', 'akismet/akismet.php'));
        // Folder-only slug should also resolve.
        $this->assertSame('5.3', $runner->currentVersion('plugin', 'akismet'));
    }

    public function test_theme_version_detection_from_wp_get_themes(): void
    {
        $theme = new class {
            /** @param string $k Field. @return string */
            public function get($k): string
            {
                return $k === 'Version' ? '1.0' : '';
            }
        };
        Functions\when('wp_get_themes')->justReturn(['twentytwentyfour' => $theme]);

        $runner = new UpdateRunner();
        $this->assertSame('1.0', $runner->currentVersion('theme', 'twentytwentyfour'));
    }

    public function test_invalid_type_fails_without_mutation(): void
    {
        $runner = $this->spyRunner();
        $cmd    = new UpdateCommand($this->spySnapshots(), $runner);

        $out = $cmd->execute([], ['items' => [['type' => 'bogus', 'slug' => 'x']]]);

        $this->assertFalse($out['ok']);
        $this->assertSame('failed', $out['results'][0]['status']);
        $this->assertSame([], $runner->applied);
    }

    /**
     * @dataProvider traversalSlugs
     */
    public function test_slug_sanitization_rejects_traversal(string $slug): void
    {
        $this->assertSame('', UpdateCommand::sanitizeSlug($slug));

        // And the command refuses to mutate for such slugs.
        $runner = $this->spyRunner();
        $cmd    = new UpdateCommand($this->spySnapshots(), $runner);
        $out    = $cmd->execute([], ['items' => [['type' => 'plugin', 'slug' => $slug, 'version' => 'latest']]]);

        $this->assertSame('failed', $out['results'][0]['status']);
        $this->assertSame([], $runner->applied);
    }

    /**
     * @return array<int,array{0:string}>
     */
    public static function traversalSlugs(): array
    {
        return [
            ['../evil'],
            ['../../wp-config.php'],
            ['/etc/passwd'],
            ['foo/../../bar'],
            ['C:\\Windows'],
            ['..'],
            ['foo/bar/baz'],
            ["foo\0bar"],
            [''],
        ];
    }

    public function test_slug_sanitization_accepts_valid_slugs(): void
    {
        $this->assertSame('akismet', UpdateCommand::sanitizeSlug('akismet'));
        $this->assertSame('akismet/akismet.php', UpdateCommand::sanitizeSlug('akismet/akismet.php'));
        $this->assertSame('twentytwentyfour', UpdateCommand::sanitizeSlug('twentytwentyfour'));
        $this->assertSame('woo-commerce', UpdateCommand::sanitizeSlug('woo-commerce'));
    }

    public function test_batch_continues_after_a_failure_and_sets_ok_false(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:good/good.php'] = '1.0';

        $cmd = new UpdateCommand($this->spySnapshots(), $runner);

        $out = $cmd->execute([], [
            'items' => [
                ['type' => 'plugin', 'slug' => '../bad', 'version' => 'latest'],
                ['type' => 'plugin', 'slug' => 'good/good.php', 'version' => '1.1'],
            ],
        ]);

        $this->assertFalse($out['ok']);
        $this->assertCount(2, $out['results']);
        $this->assertSame('failed', $out['results'][0]['status']);
        $this->assertSame('succeeded', $out['results'][1]['status']);
    }

    // ---- not-applicable targets (GitHub issue #126) ------------------------

    public function test_not_installed_plugin_is_skipped_not_failed(): void
    {
        $runner = $this->spyRunner();
        // No entry in $runner->versions AND explicitly marked absent — this
        // slug was never installed on the site.
        $runner->installedOverride['plugin:ghost/ghost.php'] = false;

        $cmd = new UpdateCommand($this->spySnapshots(), $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'ghost/ghost.php', 'version' => 'latest']],
        ]);

        $r = $out['results'][0];
        $this->assertSame('skipped', $r['status']);
        $this->assertTrue($out['ok'], 'a skipped (not-applicable) item must not flip ok to false');
        $this->assertSame([], $runner->applied, 'a not-installed target must never reach apply()');
    }

    public function test_not_installed_theme_is_skipped_not_failed(): void
    {
        $runner = $this->spyRunner();
        $runner->installedOverride['theme:ghost-theme'] = false;

        $cmd = new UpdateCommand($this->spySnapshots(), $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'theme', 'slug' => 'ghost-theme', 'version' => 'latest']],
        ]);

        $this->assertSame('skipped', $out['results'][0]['status']);
        $this->assertSame([], $runner->applied);
    }

    public function test_installed_but_no_pending_update_reports_up_to_date_without_apply(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php'] = '5.3';
        $runner->installedOverride['plugin:akismet/akismet.php'] = true;
        // Deliberately no 'available' entry => nothing pending for 'latest'.

        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'snapshot' => true,
            'items'    => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $r = $out['results'][0];
        $this->assertSame('up_to_date', $r['status']);
        $this->assertSame('5.3', $r['from_version']);
        $this->assertSame('5.3', $r['to_version']);
        $this->assertSame([], $runner->applied, 'nothing pending must never reach apply()');
        $this->assertSame([], $snapshots->captured, 'nothing pending must never trigger a snapshot');
        $this->assertTrue($out['ok']);
    }

    public function test_installed_with_pending_update_that_fails_to_apply_still_fails(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php'] = '5.0';
        $runner->available['plugin:akismet/akismet.php'] = '5.3';
        $runner->applySucceeds                           = false;

        $cmd = new UpdateCommand($this->spySnapshots(), $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $r = $out['results'][0];
        $this->assertSame('failed', $r['status']);
        $this->assertFalse($out['ok']);
        $this->assertCount(1, $runner->applied, 'a genuine pending update must still be attempted');
    }

    public function test_command_name(): void
    {
        $this->assertSame('update', (new UpdateCommand())->name());
    }

    // ---- version argument-injection validation ----------------------------

    /**
     * @dataProvider unsafeVersions
     */
    public function test_runner_rejects_unsafe_version_string(string $version): void
    {
        $this->assertFalse(UpdateRunner::isValidVersion($version));

        // apply() must refuse to invoke WP-CLI/upgrader for an unsafe version.
        $runner = new UpdateRunner();
        $out    = $runner->apply('plugin', 'akismet/akismet.php', $version);
        $this->assertFalse($out['ok']);

        // forceCore() (the PHP-fallback offer-URL path) must reject it too.
        $core = $runner->forceCore($version);
        $this->assertFalse($core['ok']);
    }

    /**
     * @return array<int,array{0:string}>
     */
    public static function unsafeVersions(): array
    {
        return [
            ['1.0 --activate'],
            ['latest --activate'],
            ['--activate'],
            ['1.0;rm -rf /'],
            ['1.0 && echo pwned'],
            ['1.0|whoami'],
            ['1.0`id`'],
            [' 1.0'],
            ['v1.0'], // must start with a digit
            [''],
        ];
    }

    public function test_runner_accepts_safe_versions(): void
    {
        $this->assertTrue(UpdateRunner::isValidVersion('latest'));
        $this->assertTrue(UpdateRunner::isValidVersion('1.0'));
        $this->assertTrue(UpdateRunner::isValidVersion('6.4.3'));
        $this->assertTrue(UpdateRunner::isValidVersion('5.3-beta1'));
    }

    public function test_command_marks_item_failed_for_version_with_spaces(): void
    {
        // Use a REAL runner (not the spy) so the version validation in apply()
        // is exercised; wpCliAvailable() is false outside a WP-CLI context.
        $runner = new UpdateRunner();
        $cmd    = new UpdateCommand($this->spySnapshots(), $runner);

        $out = $cmd->execute([], [
            'items' => [
                ['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => '1.0 --activate'],
            ],
        ]);

        $this->assertFalse($out['ok']);
        $this->assertSame('failed', $out['results'][0]['status']);
    }

    // ---- .maintenance guarantee (GitHub issue #127) ------------------------

    /**
     * A runner spy whose apply() mimics a real WP upgrader: it drops the
     * `.maintenance` marker (as Plugin_Upgrader/Core_Upgrader do at the start
     * of an upgrade) and then blows up before it would reach its own cleanup
     * line — the exact failure mode that orphans the flag in production.
     */
    private function spyRunnerThatLeavesMaintenanceOnFailure(string $maintenanceFile): UpdateRunner
    {
        return new class ($maintenanceFile) extends UpdateRunner {
            public function __construct(private string $maintenanceFile)
            {
            }

            public function currentVersion(string $type, string $slug): string
            {
                return '5.0';
            }

            public function apply(string $type, string $slug, string $version): array
            {
                file_put_contents($this->maintenanceFile, '<?php $upgrading = ' . time() . '; ?>');
                throw new \RuntimeException('upgrade interrupted mid-flight');
            }
        };
    }

    public function test_maintenance_flag_is_removed_after_a_failed_update(): void
    {
        $runner = $this->spyRunnerThatLeavesMaintenanceOnFailure($this->maintenanceFile);
        $cmd    = new UpdateCommand($this->spySnapshots(), $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => '5.3']],
        ]);

        $this->assertSame('failed', $out['results'][0]['status']);
        $this->assertFileDoesNotExist(
            $this->maintenanceFile,
            'a failed update must never leave the site permanently in maintenance mode'
        );
    }

    public function test_stale_maintenance_flag_is_healed_before_a_dry_run_starts(): void
    {
        file_put_contents($this->maintenanceFile, '<?php $upgrading = ' . time() . '; ?>');
        // Well past Maintenance's staleness threshold.
        touch($this->maintenanceFile, time() - 200);

        $runner = $this->spyRunner();
        $runner->versions['plugin:hello/hello.php'] = '1.7.2';
        $cmd = new UpdateCommand($this->spySnapshots(), $runner);

        // dry_run never calls Maintenance::clear() — this isolates the
        // start-of-run healStaleIfPresent() proactive heal.
        $cmd->execute([], [
            'dry_run' => true,
            'items'   => [['type' => 'plugin', 'slug' => 'hello/hello.php', 'version' => 'latest']],
        ]);

        $this->assertFileDoesNotExist(
            $this->maintenanceFile,
            'a stale flag from a prior interrupted run must be healed before new work starts'
        );
    }

    public function test_fresh_maintenance_flag_is_not_touched_by_a_dry_run(): void
    {
        file_put_contents($this->maintenanceFile, '<?php $upgrading = ' . time() . '; ?>');
        // Freshly written — well under the staleness threshold.

        $runner = $this->spyRunner();
        $runner->versions['plugin:hello/hello.php'] = '1.7.2';
        $cmd = new UpdateCommand($this->spySnapshots(), $runner);

        $cmd->execute([], [
            'dry_run' => true,
            'items'   => [['type' => 'plugin', 'slug' => 'hello/hello.php', 'version' => 'latest']],
        ]);

        $this->assertFileExists(
            $this->maintenanceFile,
            'a fresh flag may belong to another in-flight update/rollback and must not be deleted'
        );
    }
}
