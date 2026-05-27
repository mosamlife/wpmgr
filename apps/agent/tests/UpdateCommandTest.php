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
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
    }

    protected function tear_down(): void
    {
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

            public function currentVersion(string $type, string $slug): string
            {
                return $this->versions[$type . ':' . $slug] ?? '';
            }

            public function availableVersion(string $type, string $slug, string $requested): string
            {
                return $this->available[$type . ':' . $slug] ?? ($requested !== 'latest' ? $requested : '');
            }

            public function apply(string $type, string $slug, string $version): array
            {
                $this->applied[] = [$type, $slug, $version];
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
}
