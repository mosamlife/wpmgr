<?php
/**
 * UpdateCommandSiteLockTest - PIECE 2, per-site serialisation, at the command
 * boundary (GitHub issue #328).
 *
 * The safety assertion these tests exist for is not "the update did not
 * happen". It is "NOTHING WAS WRITTEN": a command refused because the site is
 * busy must be indistinguishable on disk from a command that never arrived.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\UpdateCommand;
use WPMgr\Agent\Support\SiteUpdateLock;
use WPMgr\Agent\Support\SnapshotManager;
use WPMgr\Agent\Support\UpdateRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\UpdateCommand
 */
final class UpdateCommandSiteLockTest extends TestCase
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
     * Pretend another REQUEST holds the site lock: take it, then drop this
     * process's in-memory ownership while leaving the option store alone.
     *
     * @return void
     */
    private function anotherRequestHoldsTheLock(): void
    {
        $this->assertSame(SiteUpdateLock::ACQUIRED, SiteUpdateLock::acquire());
        SiteUpdateLock::resetForTests();
    }

    /**
     * A runner spy that records every call it is asked to make.
     *
     * @return UpdateRunner
     */
    private function spyRunner(): UpdateRunner
    {
        return new class extends UpdateRunner {
            /** @var array<int,array{string,string,string}> */
            public array $applied = [];
            /** @var array<string,string> */
            public array $versions = [];
            /** @var int */
            public int $reconciled = 0;

            public function currentVersion(string $type, string $slug): string
            {
                return $this->versions[$type . ':' . $slug] ?? '';
            }

            public function isInstalled(string $type, string $slug): bool
            {
                return array_key_exists($type . ':' . $slug, $this->versions);
            }

            public function availableVersion(string $type, string $slug, string $requested): ?string
            {
                return $requested !== 'latest' ? $requested : '9.9.9';
            }

            public function apply(string $type, string $slug, string $version): array
            {
                $this->applied[] = [$type, $slug, $version];
                $this->versions[$type . ':' . $slug] = $version === 'latest' ? '9.9.9' : $version;

                return ['ok' => true, 'log' => 'applied', 'destination_touched' => true];
            }

            public function wpCliAvailable(): bool
            {
                return false;
            }

            public function isComplete(string $type, string $slug, string $expectedVersion = ''): bool
            {
                return true;
            }
        };
    }

    /**
     * A snapshot spy that records everything and touches no disk.
     *
     * @return SnapshotManager
     */
    private function spySnapshots(): SnapshotManager
    {
        return new class extends SnapshotManager {
            /** @var array<int,array{string,string,string}> */
            public array $captured = [];
            /** @var array<int,array{string,string,string}> */
            public array $restored = [];
            /** @var int */
            public int $gcCalls = 0;

            public function capture(string $type, string $slug, string $fromVersion): array
            {
                $this->captured[] = [$type, $slug, $fromVersion];

                return ['snapshot_id' => 'snap_lock_test', 'log' => 'captured'];
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
        };
    }

    // ---- the refusal -----------------------------------------------------

    public function test_a_busy_site_refuses_every_item_with_the_site_busy_status(): void
    {
        $this->anotherRequestHoldsTheLock();

        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php'] = '5.0';
        $runner->versions['plugin:hello/hello.php']     = '1.0';

        $out = (new UpdateCommand($this->spySnapshots(), $runner))->execute([], [
            'items' => [
                ['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest'],
                ['type' => 'plugin', 'slug' => 'hello/hello.php', 'version' => 'latest'],
            ],
        ]);

        $this->assertFalse(
            $out['ok'],
            'ok MUST be false so an older control plane that does not know this status takes its safe '
            . 'rejected-command path instead of the post-probe branch, which could record a never-run update as '
            . 'succeeded'
        );
        $this->assertCount(2, $out['results']);
        foreach ($out['results'] as $result) {
            $this->assertSame('site_busy', $result['status']);
            $this->assertSame('', $result['from_version']);
            $this->assertSame('', $result['to_version']);
            $this->assertSame('', $result['snapshot_id']);
            $this->assertStringContainsString('Nothing was attempted', $result['log']);
            $this->assertStringContainsString('nothing on this site was changed', $result['log']);
        }
        $this->assertSame(
            ['type', 'slug', 'from_version', 'to_version', 'status', 'snapshot_id', 'log'],
            array_keys($out['results'][0]),
            'the refusal must use the existing item shape; no new wire fields'
        );
    }

    /** THE SAFETY ASSERTION: a refused command writes nothing whatsoever. */
    public function test_a_busy_refusal_touches_nothing_on_disk(): void
    {
        file_put_contents($this->maintenanceFile, '<?php $upgrading = ' . (time() - 400) . '; ?>');

        $this->anotherRequestHoldsTheLock();

        $runner    = $this->spyRunner();
        $snapshots = $this->spySnapshots();
        $runner->versions['plugin:akismet/akismet.php'] = '5.0';

        (new UpdateCommand($snapshots, $runner))->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $this->assertSame([], $snapshots->captured, 'no snapshot may be captured for a refused command');
        $this->assertSame([], $snapshots->restored, 'no restore may run for a refused command');
        $this->assertSame([], $runner->applied, 'no apply may run for a refused command');
        $this->assertFileExists(
            $this->maintenanceFile,
            'a refused command must not heal a stale flag either: the heal is below the lock precisely so a command '
            . 'with nothing to do cannot strip a flag another in-flight upgrade owns'
        );
    }

    /** The lock is released on every synchronous path, including a throw. */
    public function test_the_lock_is_released_after_a_successful_command(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php'] = '5.0';

        (new UpdateCommand($this->spySnapshots(), $runner))->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $this->assertArrayNotHasKey('wpmgr_site_update.lock', $this->options);
        $this->assertArrayNotHasKey('wpmgr_site_update_owner', $this->options);
    }

    public function test_the_lock_is_released_even_when_an_item_throws(): void
    {
        $runner = new class extends UpdateRunner {
            /** @var array<string,string> */
            public array $versions = ['plugin:akismet/akismet.php' => '5.0'];

            public function currentVersion(string $type, string $slug): string
            {
                return $this->versions[$type . ':' . $slug] ?? '';
            }

            public function isInstalled(string $type, string $slug): bool
            {
                return true;
            }

            public function availableVersion(string $type, string $slug, string $requested): ?string
            {
                return '9.9.9';
            }

            public function apply(string $type, string $slug, string $version): array
            {
                throw new \RuntimeException('apply exploded');
            }

            public function wpCliAvailable(): bool
            {
                return false;
            }
        };

        $out = (new UpdateCommand($this->spySnapshots(), $runner))->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $this->assertSame('failed', $out['results'][0]['status']);
        $this->assertArrayNotHasKey(
            'wpmgr_site_update.lock',
            $this->options,
            'a thrown item must not leave the site locked for 15 minutes'
        );
    }

    /**
     * ONE COMMAND, ONE HOLD, restamped between items. Without the renewal a
     * legitimately long multi-item command could expire its own lock mid-run
     * and let a second writer in.
     */
    public function test_a_multi_item_command_renews_the_lock_between_items(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:a/a.php'] = '1.0';
        $runner->versions['plugin:b/b.php'] = '1.0';
        $runner->versions['plugin:c/c.php'] = '1.0';

        $stamps = [];
        Functions\when('update_option')->alias(function (string $name, $value) use (&$stamps) {
            if ($name === 'wpmgr_site_update.lock') {
                $stamps[] = $value;
            }
            $this->options[$name] = $value;
            return true;
        });

        (new UpdateCommand($this->spySnapshots(), $runner))->execute([], [
            'items' => [
                ['type' => 'plugin', 'slug' => 'a/a.php', 'version' => 'latest'],
                ['type' => 'plugin', 'slug' => 'b/b.php', 'version' => 'latest'],
                ['type' => 'plugin', 'slug' => 'c/c.php', 'version' => 'latest'],
            ],
        ]);

        // One write when the lock was taken plus one renewal per item.
        $this->assertCount(4, $stamps);
    }

    // ---- the dry-run contract -------------------------------------------

    /**
     * THE F9 CONTRACT FIX UNDER TEST. A dry run takes no lock (so it can never
     * block a real apply, and can never be blocked by one) and mutates nothing.
     */
    public function test_a_dry_run_takes_no_lock_and_still_answers_while_the_site_is_busy(): void
    {
        $this->anotherRequestHoldsTheLock();
        $lockStampBefore = $this->options['wpmgr_site_update.lock'];

        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php'] = '5.0';

        $out = (new UpdateCommand($this->spySnapshots(), $runner))->execute([], [
            'dry_run' => true,
            'items'   => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $this->assertSame(
            'would_update',
            $out['results'][0]['status'],
            'a preview must not be refused just because the site is applying something else'
        );
        $this->assertSame([], $runner->applied);
        $this->assertSame(
            $lockStampBefore,
            $this->options['wpmgr_site_update.lock'],
            'a dry run must neither take nor disturb the site lock'
        );
    }

    /**
     * A dry run on an IDLE site must not take the lock either, and must not
     * reach any of the four writes that used to run before `dry_run` was even
     * parsed.
     */
    public function test_a_dry_run_on_an_idle_site_writes_nothing_at_all(): void
    {
        file_put_contents($this->maintenanceFile, '<?php $upgrading = ' . (time() - 400) . '; ?>');

        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php'] = '5.0';

        $out = (new UpdateCommand($this->spySnapshots(), $runner))->execute([], [
            'dry_run' => true,
            'items'   => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $this->assertSame('would_update', $out['results'][0]['status']);
        $this->assertSame(
            [],
            $this->options,
            'a dry run must not write a single wp-option, including the site lock'
        );
        $this->assertFileExists(
            $this->maintenanceFile,
            'a dry run must not delete a stale maintenance flag; deleting a file is a mutation'
        );
    }

    // ---- the fail-open branch -------------------------------------------

    /**
     * An UNAVAILABLE lock must PROCEED, not refuse. Core's own helper reports
     * "locked" when its INSERT failed and no lock row is readable, and refusing
     * on that reading would break every update on the affected site forever
     * with no in-product remedy.
     */
    public function test_an_unavailable_lock_proceeds_rather_than_refusing(): void
    {
        \WP_Upgrader::$failCreateLock = true;

        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php'] = '5.0';

        $out = (new UpdateCommand($this->spySnapshots(), $runner))->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $this->assertSame('succeeded', $out['results'][0]['status']);
        $this->assertCount(1, $runner->applied);
    }
}
