<?php
/**
 * UpdateCommandManagedCoreTest - core updates on Composer-owned or
 * file-change-locked installs, and the wording of a core update that stopped
 * before touching anything (GitHub issue #367).
 *
 * The reported failure, end to end: on a Roots/Bedrock site the agent ran a
 * WordPress-side core update, WordPress stopped while unpacking into the
 * content directory (mkdir_failed_ziparchive), and the item log ended with
 * "Update incomplete; no pre-update snapshot was available to auto-restore.",
 * which reads as a half-updated, unprotected site when nothing had changed.
 *
 * Detector trees are real directories on disk. The runner is a spy, so a test
 * can say exactly what WordPress reported and count whether an apply was ever
 * attempted.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use WPMgr\Agent\Commands\UpdateCommand;
use WPMgr\Agent\Support\ManagedCore;
use WPMgr\Agent\Support\SnapshotManager;
use WPMgr\Agent\Support\UpdateRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\UpdateCommand
 */
final class UpdateCommandManagedCoreTest extends TestCase
{
    /** The sentence #367 reported for a core update that changed nothing. */
    private const NO_SNAPSHOT_SENTENCE = 'no pre-update snapshot was available to auto-restore';

    /** Private scratch root for detector trees. */
    private string $root = '';

    /** @var array<int,string> Plugin fixture directories to remove in tear-down. */
    private array $created = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root = sys_get_temp_dir() . '/wpmgr-managed-cmd-' . bin2hex(random_bytes(6));
        mkdir($this->root, 0777, true);

        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', sys_get_temp_dir() . '/wpmgr-shared-wp-content');
        }
        if (!is_dir((string) constant('WP_CONTENT_DIR'))) {
            mkdir((string) constant('WP_CONTENT_DIR'), 0777, true);
        }
        if (!defined('WP_PLUGIN_DIR')) {
            define('WP_PLUGIN_DIR', rtrim((string) constant('WP_CONTENT_DIR'), '/\\') . '/plugins');
        }
        if (!is_dir((string) constant('WP_PLUGIN_DIR'))) {
            mkdir((string) constant('WP_PLUGIN_DIR'), 0777, true);
        }
        $this->created = [];
    }

    protected function tear_down(): void
    {
        foreach ($this->created as $dir) {
            $this->deleteTree($dir);
        }
        $this->deleteTree($this->root);
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * @param string $dir Directory to remove recursively.
     * @return void
     */
    private function deleteTree(string $dir): void
    {
        if (!is_dir($dir)) {
            return;
        }
        foreach (array_diff((array) scandir($dir), ['.', '..']) as $entry) {
            $path = $dir . '/' . $entry;
            if (is_dir($path)) {
                $this->deleteTree($path);
            } else {
                unlink($path); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
            }
        }
        rmdir($dir);
    }

    /**
     * A detector over a project tree under the scratch root: WordPress at
     * web/wp, and a composer.json at the project root requiring $require.
     *
     * @param array<string,string>|null $require         composer.json `require`, or null for no manifest.
     * @param bool                      $fileModsAllowed Answer to the file-changes question.
     * @return ManagedCore
     */
    private function project(?array $require, bool $fileModsAllowed = true): ManagedCore
    {
        mkdir($this->root . '/web/wp', 0777, true);
        if ($require !== null) {
            file_put_contents(
                $this->root . '/composer.json',
                (string) json_encode(['name' => 'example/site', 'type' => 'project', 'require' => $require], JSON_UNESCAPED_SLASHES)
            );
        }

        return new ManagedCore($this->root . '/web/wp/', static function () use ($fileModsAllowed): bool {
            return $fileModsAllowed;
        });
    }

    /**
     * The outcome WordPress produced in #367: an unpack failure classified as
     * stopping before anything was installed.
     *
     * @return array<string,mixed>
     */
    private function reportedUnpackFailure(): array
    {
        return [
            'ok'                   => false,
            'log'                  => 'WP_Error (mkdir_failed_ziparchive): Could not create directory.',
            'destination_touched'  => false,
            'failure_code'         => 'mkdir_failed_ziparchive',
            'failure_data'         => '/srv/www/example/current/web/app/upgrade/wordpress-7.0.2-no-content',
            'failure_stage'        => 'unpack',
            'may_have_deactivated' => false,
            'was_active'           => false,
            'was_network_active'   => false,
        ];
    }

    /**
     * A runner spy whose apply() returns a caller-chosen outcome and counts
     * every call that would reach WordPress.
     *
     * @param array<string,mixed> $outcome   apply() return value.
     * @param bool                $installed What isInstalled() reports.
     * @return UpdateRunner
     */
    private function spyRunner(array $outcome, bool $installed = true): UpdateRunner
    {
        return new class ($outcome, $installed) extends UpdateRunner {
            /** @var array<string,mixed> */
            private array $outcome;

            private bool $installed;

            public int $applyCalls = 0;

            public int $availabilityCalls = 0;

            /**
             * @param array<string,mixed> $outcome   apply() return value.
             * @param bool                $installed What isInstalled() reports.
             */
            public function __construct(array $outcome, bool $installed)
            {
                $this->outcome   = $outcome;
                $this->installed = $installed;
            }

            public function currentVersion(string $type, string $slug): string
            {
                return $type === 'core' ? '6.9.5' : '1.2.3';
            }

            public function isInstalled(string $type, string $slug): bool
            {
                return $this->installed;
            }

            public function availableVersion(string $type, string $slug, string $requested): ?string
            {
                ++$this->availabilityCalls;

                return $type === 'core' ? '7.0.2' : '9.9.9';
            }

            public function apply(string $type, string $slug, string $version): array
            {
                ++$this->applyCalls;

                return $this->outcome;
            }

            public function wpCliAvailable(): bool
            {
                return false;
            }

            public function isComplete(string $type, string $slug, string $expectedVersion = ''): bool
            {
                return false;
            }

            public function lastIncompleteReason(): string
            {
                return '';
            }
        };
    }

    /**
     * A snapshot spy that records restores and never touches disk.
     *
     * @return SnapshotManager
     */
    private function spySnapshots(): SnapshotManager
    {
        return new class extends SnapshotManager {
            /** @var array<int,array{string,string,string}> */
            public array $restored = [];

            /** @var array<int,string> */
            public array $captured = [];

            public function __construct()
            {
            }

            public function capture(string $type, string $slug, string $fromVersion): array
            {
                $this->captured[] = $type . ':' . $slug;

                return ['snapshot_id' => 'snap_managed_core', 'log' => 'captured'];
            }

            public function restore(string $type, string $slug, string $snapshotId): array
            {
                $this->restored[] = [$type, $slug, $snapshotId];

                return ['ok' => true, 'log' => 'restored'];
            }

            public function snapshotExists(string $snapshotId): bool
            {
                return true;
            }

            public function payloadDir(string $snapshotId): string
            {
                return '';
            }

            public function markSucceeded(string $snapshotId): void
            {
            }

            public function markRestoreSkipped(string $snapshotId, string $failureCode, string $verification): void
            {
            }
        };
    }

    /**
     * @param array<string,mixed> $params Command parameters.
     * @param UpdateRunner        $runner Runner spy.
     * @param ManagedCore         $core   Detector.
     * @param SnapshotManager|null $snapshots Snapshot spy.
     * @return array{ok:bool,results:array<int,array<string,mixed>>}
     */
    private function run(array $params, UpdateRunner $runner, ManagedCore $core, ?SnapshotManager $snapshots = null): array
    {
        return (new UpdateCommand($snapshots ?? $this->spySnapshots(), $runner, $core))->execute([], $params);
    }

    /**
     * @return array<string,mixed>
     */
    private function coreItem(): array
    {
        return ['type' => 'core', 'slug' => 'core', 'version' => 'latest'];
    }

    // =====================================================================
    // Composer-managed core is skipped before anything is attempted
    // =====================================================================

    /** THE #367 SHAPE: a Bedrock project's core update is never attempted. */
    public function test_a_composer_managed_core_is_skipped_before_any_apply(): void
    {
        $runner = $this->spyRunner($this->reportedUnpackFailure());

        $out = $this->run(
            ['snapshot' => true, 'items' => [$this->coreItem()]],
            $runner,
            $this->project(['roots/wordpress' => '^7.0'])
        );

        $result = $out['results'][0];
        $this->assertSame('skipped', $result['status'], 'a core update WordPress must not apply here is skipped, not failed');
        $this->assertSame('core_managed', $result['skip_reason'] ?? null);
        $this->assertStringContainsString('managed by Composer', $result['log']);
        $this->assertStringContainsString('requires roots/wordpress', $result['log']);
        $this->assertSame(0, $runner->applyCalls, 'nothing may be downloaded or unpacked on a Composer-managed core');
        $this->assertTrue($out['ok'], 'a skip is not a failed command');
    }

    /** A dry run must not report an update the real run will refuse. */
    public function test_a_composer_managed_core_dry_run_is_skipped_too(): void
    {
        $runner = $this->spyRunner($this->reportedUnpackFailure());

        $out = $this->run(
            ['dry_run' => true, 'items' => [$this->coreItem()]],
            $runner,
            $this->project(['johnpbloch/wordpress' => '*'])
        );

        $this->assertSame('skipped', $out['results'][0]['status']);
        $this->assertSame('core_managed', $out['results'][0]['skip_reason'] ?? null);
        $this->assertSame(0, $runner->availabilityCalls, 'not even an update check runs for a managed core');
        $this->assertSame(0, $runner->applyCalls);
    }

    /**
     * File changes disallowed with no Composer signal: skipped with its own
     * reason and its own words, because "managed by Composer" would be false.
     */
    public function test_disallowed_file_changes_skip_core_with_their_own_reason(): void
    {
        $runner = $this->spyRunner($this->reportedUnpackFailure());

        $out = $this->run(['items' => [$this->coreItem()]], $runner, $this->project(null, false));

        $result = $out['results'][0];
        $this->assertSame('skipped', $result['status']);
        $this->assertSame('file_mods_disallowed', $result['skip_reason'] ?? null);
        $this->assertStringContainsString('file changes', $result['log']);
        $this->assertStringNotContainsString('Composer', $result['log']);
        $this->assertSame(0, $runner->applyCalls);
    }

    // =====================================================================
    // Over-fire guards: ordinary sites and other item types are untouched
    // =====================================================================

    /** A Composer project that manages only plugins still gets core updates. */
    public function test_a_manifest_without_a_core_package_leaves_the_core_path_unchanged(): void
    {
        $runner = $this->spyRunner($this->reportedUnpackFailure());

        $out = $this->run(
            ['items' => [$this->coreItem()]],
            $runner,
            $this->project(['wpackagist-plugin/akismet' => '^5.0'])
        );

        $this->assertSame(1, $runner->applyCalls, 'the core update must still be attempted');
        $this->assertSame('failed', $out['results'][0]['status']);
        $this->assertArrayNotHasKey('skip_reason', $out['results'][0]);
    }

    /** No manifest and file changes allowed: an ordinary site. */
    public function test_an_ordinary_site_still_attempts_the_core_update(): void
    {
        $runner = $this->spyRunner($this->reportedUnpackFailure());

        $this->run(['items' => [$this->coreItem()]], $runner, $this->project(null, true));

        $this->assertSame(1, $runner->applyCalls);
    }

    /** The managed-core check is about core: a plugin on a Bedrock site still updates. */
    public function test_a_plugin_on_a_composer_managed_site_is_still_applied(): void
    {
        $folder = 'wpmgr-managed-' . bin2hex(random_bytes(4));
        $dir    = rtrim((string) constant('WP_PLUGIN_DIR'), '/\\') . '/' . $folder;
        mkdir($dir, 0777, true);
        $this->created[] = $dir;
        file_put_contents($dir . '/' . $folder . '.php', "<?php\n/**\n * Plugin Name: Managed Fixture\n * Version: 1.2.3\n */\n");

        $runner    = $this->spyRunner(['ok' => false, 'log' => 'upgrader reported failure']);
        $snapshots = $this->spySnapshots();

        $out = $this->run(
            ['items' => [['type' => 'plugin', 'slug' => $folder . '/' . $folder . '.php', 'version' => 'latest']]],
            $runner,
            $this->project(['roots/wordpress' => '^7.0']),
            $snapshots
        );

        $this->assertSame(1, $runner->applyCalls, 'a plugin update must never be refused by the core check');
        $this->assertNotSame('skipped', $out['results'][0]['status']);
        $this->assertArrayNotHasKey('skip_reason', $out['results'][0]);
    }

    // =====================================================================
    // A core update that stopped before touching anything says so
    // =====================================================================

    /** THE #367 COPY: core was not changed, and the log must say so plainly. */
    public function test_an_untouched_core_failure_says_core_was_not_changed(): void
    {
        $out = $this->run(
            ['items' => [$this->coreItem()]],
            $this->spyRunner($this->reportedUnpackFailure()),
            $this->project(null)
        );

        $log = $out['results'][0]['log'];
        $this->assertSame('failed', $out['results'][0]['status'], 'the update genuinely did not happen');
        $this->assertStringNotContainsString(self::NO_SNAPSHOT_SENTENCE, $log);
        $this->assertStringContainsString('WordPress core was not changed', $log);
        $this->assertStringContainsString('unpack', $log);
        $this->assertStringContainsString('mkdir_failed_ziparchive', $log);
        $this->assertStringContainsString('/srv/www/example/current/web/app/upgrade/wordpress-7.0.2-no-content', $log);
        $this->assertStringContainsString(rtrim((string) constant('WP_CONTENT_DIR'), '/\\') . '/upgrade/', $log);
    }

    /**
     * A core failure WordPress may have got past the unpack with keeps the
     * previous release's wording exactly: this release narrows, never widens.
     */
    public function test_a_touched_core_failure_keeps_the_previous_wording(): void
    {
        $touched = [
            'ok'                  => false,
            'log'                 => 'WP_Error (files_not_writable): The update cannot be installed.',
            'destination_touched' => true,
            'failure_code'        => 'files_not_writable',
            'failure_data'        => '',
            'failure_stage'       => 'install',
        ];

        $out = $this->run(['items' => [$this->coreItem()]], $this->spyRunner($touched), $this->project(null));

        $this->assertSame(
            "WP_Error (files_not_writable): The update cannot be installed.\nUpdate incomplete; no pre-update snapshot was available to auto-restore.",
            $out['results'][0]['log']
        );
    }

    /** An unclassified core failure (no destination verdict) also keeps the previous wording. */
    public function test_an_unclassified_core_failure_keeps_the_previous_wording(): void
    {
        $out = $this->run(
            ['items' => [$this->coreItem()]],
            $this->spyRunner(['ok' => false, 'log' => 'Update failed.']),
            $this->project(null)
        );

        $this->assertSame(
            "Update failed.\nUpdate incomplete; no pre-update snapshot was available to auto-restore.",
            $out['results'][0]['log']
        );
    }

    // =====================================================================
    // skip_reason on the existing skips (the control plane maps these)
    // =====================================================================

    /** A target that is not installed carries its reason. */
    public function test_a_not_installed_skip_carries_its_reason(): void
    {
        $out = $this->run(
            ['items' => [['type' => 'plugin', 'slug' => 'absent-plugin/absent-plugin.php', 'version' => 'latest']]],
            $this->spyRunner($this->reportedUnpackFailure(), false),
            $this->project(null)
        );

        $this->assertSame('skipped', $out['results'][0]['status']);
        $this->assertSame('not_installed', $out['results'][0]['skip_reason'] ?? null);
    }

    /** The agent's own plugin carries its reason. */
    public function test_a_self_target_skip_carries_its_reason(): void
    {
        $out = $this->run(
            ['items' => [['type' => 'plugin', 'slug' => 'wpmgr-agent/wpmgr-agent.php', 'version' => 'latest']]],
            $this->spyRunner($this->reportedUnpackFailure()),
            $this->project(null)
        );

        $this->assertSame('skipped', $out['results'][0]['status']);
        $this->assertSame('self_target', $out['results'][0]['skip_reason'] ?? null);
    }
}
