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
use WPMgr\Agent\Support\UpdateInFlight;
use WPMgr\Agent\Support\UpdateRunner;
use WPMgr\Agent\Support\UpdateWatchdogMarker;
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
            /**
             * @var array<string,bool> Keys ("type:slug") for which
             *     availableVersion() must return null — i.e. availability
             *     could not be determined even after a forced fresh check
             *     (GitHub issue #208) — rather than '' (genuinely no
             *     update) or a real version string.
             */
            public array $availableUnknown = [];
            /** @var array<string,bool> Explicit installed-state overrides, keyed "type:slug". */
            public array $installedOverride = [];
            /** Whether apply() should report success. */
            public bool $applySucceeds = true;
            /**
             * @var array<string,bool> Explicit isComplete() overrides, keyed
             *     "type:slug". Absent = complete (true) — matches the
             *     production fail-open default so tests that don't care about
             *     the completeness check are unaffected.
             */
            public array $completeOverride = [];
            /** @var array<int,array{string,string}> Args of each isComplete() call. */
            public array $completeChecked = [];
            /**
             * @var array<string,string> Explicit lastIncompleteReason()
             *     overrides, keyed "type:slug" — agent-only, 0.61.19 (GitHub
             *     issue #182's sibling visibility fix). Absent = '' (matches
             *     the real UpdateRunner's default when isComplete() returned
             *     true, or was never called).
             */
            public array $incompleteReasonOverride = [];
            /**
             * The "type:slug" key of the MOST RECENT isComplete() call, so
             * lastIncompleteReason() mirrors the real UpdateRunner's
             * single-mutable-property semantics (the reason belongs to
             * whichever call happened last, not to every key ever checked).
             */
            private string $lastCompleteCheckedKey = '';

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

            public function availableVersion(string $type, string $slug, string $requested): ?string
            {
                $key = $type . ':' . $slug;
                if (isset($this->availableUnknown[$key])) {
                    return null;
                }

                return $this->available[$key] ?? ($requested !== 'latest' ? $requested : '');
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

            public function isComplete(string $type, string $slug, string $expectedVersion = ''): bool
            {
                $this->completeChecked[]      = [$type, $slug];
                $this->lastCompleteCheckedKey = $type . ':' . $slug;

                return $this->completeOverride[$type . ':' . $slug] ?? true;
            }

            /** Agent-only, 0.61.19 (GitHub issue #182's sibling visibility fix). */
            public function lastIncompleteReason(): string
            {
                return $this->incompleteReasonOverride[$this->lastCompleteCheckedKey] ?? '';
            }
        };
    }

    /**
     * A snapshot spy that records captures/restores without touching disk.
     */
    private function spySnapshots(): SnapshotManager
    {
        return new class extends SnapshotManager {
            /** @var array<int,array{string,string,string}> */
            public array $captured = [];
            /** @var array<int,array{string,string,string}> Args of each restore() call. */
            public array $restored = [];
            /** Whether restore() should report success. */
            public bool $restoreSucceeds = true;
            /**
             * @var array<int,string> Snapshot ids passed to markSucceeded()
             *     (GitHub issue #226).
             */
            public array $markedSucceeded = [];

            public function capture(string $type, string $slug, string $fromVersion): array
            {
                $this->captured[] = [$type, $slug, $fromVersion];

                return ['snapshot_id' => 'snap_test123', 'log' => 'captured'];
            }

            public function restore(string $type, string $slug, string $snapshotId): array
            {
                $this->restored[] = [$type, $slug, $snapshotId];

                return $this->restoreSucceeds
                    ? ['ok' => true, 'log' => 'restored']
                    : ['ok' => false, 'log' => 'restore failed'];
            }

            public function snapshotExists(string $snapshotId): bool
            {
                // F2 (issue #131 final-hardening review) — these tests exercise
                // the restore DECISION via the runner spy's completeOverride;
                // always report the snapshot present so that gate never blocks
                // a restore this test otherwise expects.
                return true;
            }

            public function markSucceeded(string $snapshotId): void
            {
                $this->markedSucceeded[] = $snapshotId;
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

    // ---- undetermined availability (GitHub issue #208) ---------------------

    /**
     * Dry run: a real pending update went silently unreported before this
     * fix because a stale/expired/never-populated update transient was read
     * as-is, indistinguishable from "genuinely no update available".
     * UpdateRunner::availableVersion() now returns null (rather than '')
     * when it cannot determine availability at all — even after forcing a
     * fresh check — and the dry-run reporter must surface that as a status
     * distinct from both `up_to_date` and `would_update`, never guessing.
     */
    public function test_dry_run_reports_unknown_status_when_availability_cannot_be_determined(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:hello/hello.php'] = '1.7.2';
        $runner->availableUnknown['plugin:hello/hello.php'] = true;

        $cmd = new UpdateCommand($this->spySnapshots(), $runner);

        $out = $cmd->execute([], [
            'dry_run' => true,
            'items'   => [['type' => 'plugin', 'slug' => 'hello/hello.php', 'version' => 'latest']],
        ]);

        $r = $out['results'][0];
        $this->assertSame(
            'unknown',
            $r['status'],
            'undetermined availability must never be silently folded into up_to_date (or falsely reported as would_update)'
        );
        $this->assertNotSame('up_to_date', $r['status']);
        $this->assertNotSame('would_update', $r['status']);
        $this->assertSame([], $runner->applied, 'a dry run must never mutate regardless of availability');
    }

    /**
     * Real (non-dry-run) apply: when availability is undetermined, the
     * caller must NOT silently report up_to_date and skip the item — it
     * must fall through to the normal snapshot+apply path, whose own
     * apply() call performs its own forced fresh check before mutating
     * anything. A genuine pending update (simulated here by the spy's
     * apply() successfully bumping the version) is still applied and
     * reported succeeded, proving the item was never silently skipped.
     */
    public function test_undetermined_availability_is_not_reported_up_to_date_and_the_update_is_still_attempted(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php'] = '5.0';
        $runner->availableUnknown['plugin:akismet/akismet.php'] = true;

        $cmd = new UpdateCommand($this->spySnapshots(), $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $r = $out['results'][0];
        $this->assertNotSame(
            'up_to_date',
            $r['status'],
            'undetermined availability must never be silently reported as up_to_date on a real apply'
        );
        $this->assertCount(
            1,
            $runner->applied,
            'undetermined availability must fall through to the normal apply path, not skip the item'
        );
        $this->assertSame('succeeded', $r['status'], 'a genuine pending update behind an undetermined pre-check must still be applied');
        $this->assertSame('9.9.9', $r['to_version']);
    }

    /**
     * The complementary case: when availability is undetermined but the
     * apply's own forced re-check (simulated by the spy's apply() being a
     * true no-op — applySucceeds stays true but currentVersion never moves
     * because 'latest' bumps to a fixed '9.9.9' in the spy, so this test
     * instead pins an explicit version equal to from_version) turns out to
     * genuinely have nothing to do, the result must still resolve
     * accurately rather than reporting a false failure or false success.
     */
    public function test_undetermined_availability_that_turns_out_genuinely_current_reports_up_to_date_after_apply(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php'] = '5.3';
        $runner->availableUnknown['plugin:akismet/akismet.php'] = true;

        $cmd = new UpdateCommand($this->spySnapshots(), $runner);

        // Pin an explicit version equal to what's already installed — the
        // spy's apply() only bumps the recorded version to '9.9.9' for a
        // 'latest' request; for an explicit version it sets it to that exact
        // string, so requesting the currently-installed version simulates
        // an apply() that ends up changing nothing.
        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => '5.3']],
        ]);

        $r = $out['results'][0];
        $this->assertCount(1, $runner->applied, 'the apply must still be attempted for an undetermined pre-check');
        $this->assertSame(
            'up_to_date',
            $r['status'],
            'once the apply itself confirms nothing changed, the result must accurately settle on up_to_date, not a false failure'
        );
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

    /**
     * GitHub issue #328 - DELIBERATE BEHAVIOUR CHANGE, and the old expectation
     * is what was wrong.
     *
     * This test previously asserted that a DRY RUN heals a stale `.maintenance`
     * flag. Deleting a file is a mutation, and the control-plane contract's
     * hard rule is that `dry_run` must not mutate the site at all. The heal was
     * one of four writes a dry run performed before `dry_run` was even parsed
     * (the others: arming a shutdown callback that deletes any `.maintenance`
     * it later finds, an in-flight reconcile that can perform a FULL DIRECTORY
     * RESTORE, and a snapshot GC sweep that deletes). The heal itself is not
     * lost: it now happens on every REAL run, under the site lock, which is the
     * only kind of run that can have left a flag behind in the first place. The
     * companion test below pins that.
     */
    public function test_a_dry_run_never_heals_a_stale_maintenance_flag(): void
    {
        file_put_contents($this->maintenanceFile, '<?php $upgrading = ' . time() . '; ?>');
        // Well past Maintenance's staleness threshold.
        touch($this->maintenanceFile, time() - 200);

        $runner = $this->spyRunner();
        $runner->versions['plugin:hello/hello.php'] = '1.7.2';
        $cmd = new UpdateCommand($this->spySnapshots(), $runner);

        $cmd->execute([], [
            'dry_run' => true,
            'items'   => [['type' => 'plugin', 'slug' => 'hello/hello.php', 'version' => 'latest']],
        ]);

        $this->assertFileExists(
            $this->maintenanceFile,
            'a dry run must not delete anything, including a stale flag it would be entitled to heal on a real run'
        );
    }

    /** The heal a dry run no longer performs still happens on a real run. */
    public function test_stale_maintenance_flag_is_healed_before_a_real_run_starts(): void
    {
        file_put_contents($this->maintenanceFile, '<?php $upgrading = ' . time() . '; ?>');
        touch($this->maintenanceFile, time() - 200);

        $runner = $this->spyRunner();
        $runner->versions['plugin:hello/hello.php'] = '1.7.2';
        $cmd = new UpdateCommand($this->spySnapshots(), $runner);

        $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'hello/hello.php', 'version' => 'latest']],
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

    // ---- forced snapshot + auto-restore on incomplete apply (GitHub issue #131, D2/D3) ----

    /**
     * A runner spy whose apply() ALWAYS throws — mirrors a resource kill
     * severe enough that install_package()/copy_dir() never returns.
     */
    private function spyRunnerThatThrowsOnApply(): UpdateRunner
    {
        return new class extends UpdateRunner {
            public function currentVersion(string $type, string $slug): string
            {
                return '5.0';
            }

            public function isInstalled(string $type, string $slug): bool
            {
                return true;
            }

            public function availableVersion(string $type, string $slug, string $requested): string
            {
                return '5.3';
            }

            public function apply(string $type, string $slug, string $version): array
            {
                throw new \RuntimeException('simulated mid-copy resource kill');
            }

            public function wpCliAvailable(): bool
            {
                return false;
            }
        };
    }

    public function test_plugin_snapshot_is_captured_even_without_the_request_snapshot_flag(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php']   = '5.0';
        $runner->available['plugin:akismet/akismet.php']  = '5.3';

        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            // 'snapshot' deliberately omitted (defaults false).
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $this->assertSame('succeeded', $out['results'][0]['status']);
        $this->assertCount(
            1,
            $snapshots->captured,
            'plugin/theme items must always snapshot before applying (D2), regardless of the request flag'
        );
    }

    public function test_core_snapshot_still_respects_the_request_flag(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['core:core']  = '6.4';
        $runner->available['core:core'] = '6.5';

        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            // 'snapshot' omitted (defaults false) — core must NOT snapshot.
            'items' => [['type' => 'core', 'slug' => '', 'version' => 'latest']],
        ]);

        $this->assertSame('succeeded', $out['results'][0]['status']);
        $this->assertSame(
            [],
            $snapshots->captured,
            'core keeps the original opt-in snapshot behavior (D3) — only plugin/theme are forced'
        );
    }

    public function test_ok_false_apply_triggers_auto_restore_and_reports_failed(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php']  = '5.0';
        $runner->available['plugin:akismet/akismet.php'] = '5.3';
        $runner->applySucceeds                           = false;

        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $r = $out['results'][0];
        $this->assertSame('failed', $r['status']);
        $this->assertFalse($out['ok']);
        $this->assertCount(1, $snapshots->restored, 'an ok:false apply must trigger an auto-restore');
        $this->assertSame(['plugin', 'akismet/akismet.php', 'snap_test123'], $snapshots->restored[0]);
    }

    public function test_apply_throwing_fires_the_guard_and_restores(): void
    {
        $runner    = $this->spyRunnerThatThrowsOnApply();
        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => '5.3']],
        ]);

        $this->assertSame('failed', $out['results'][0]['status']);
        $this->assertCount(
            1,
            $snapshots->restored,
            'a thrown apply() must still trigger the same restore path the shutdown guard uses'
        );
        $this->assertSame(['plugin', 'akismet/akismet.php', 'snap_test123'], $snapshots->restored[0]);
    }

    public function test_half_write_detected_by_completeness_check_is_restored_and_reported_failed(): void
    {
        // Mirrors the reported incident exactly: apply() bumps the version
        // (the main plugin file WAS replaced) but the rest of the directory
        // is missing, so the completeness check must catch what the old
        // version-header-only test would have reported as 'succeeded'.
        $runner = $this->spyRunner();
        $runner->versions['plugin:kadence-blocks/kadence-blocks.php']  = '3.2.0';
        $runner->available['plugin:kadence-blocks/kadence-blocks.php'] = '3.2.1';
        $runner->completeOverride['plugin:kadence-blocks/kadence-blocks.php'] = false;

        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'kadence-blocks/kadence-blocks.php', 'version' => 'latest']],
        ]);

        $r = $out['results'][0];
        $this->assertSame(
            'failed',
            $r['status'],
            'a version bump alone must never be reported as succeeded when the completeness check fails'
        );
        $this->assertCount(1, $runner->completeChecked, 'the completeness check must run for an ok:true apply');
        $this->assertCount(1, $snapshots->restored, 'a half-written apply must be auto-restored');
    }

    // ---- failure-reason visibility (agent-only, 0.61.19, GitHub issue #182's sibling fix) ----

    public function test_rollback_log_surfaces_the_validate_plugin_error_reason(): void
    {
        // The "digits 9.1.0.5 -> 9.1.0.5 Failed + rollback" class of report:
        // the reason UpdateRunner::isComplete() decided `false` (here, a
        // validate_plugin() WP_Error — the new package genuinely did not
        // land) must now reach the CP-visible item log, not just DebugLog.
        $runner = $this->spyRunner();
        $runner->versions['plugin:digits/digits.php']  = '9.1.0.4';
        $runner->available['plugin:digits/digits.php'] = '9.1.0.5';
        $runner->completeOverride['plugin:digits/digits.php']         = false;
        $runner->incompleteReasonOverride['plugin:digits/digits.php'] =
            'validate_plugin: plugin file does not exist; on-disk main file present, raw Version 9.1.0.4 (expected 9.1.0.5)';

        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'digits/digits.php', 'version' => 'latest']],
        ]);

        $r = $out['results'][0];
        $this->assertSame('failed', $r['status']);
        $this->assertStringContainsString(
            'Reason: validate_plugin: plugin file does not exist',
            $r['log'],
            'the concrete isComplete() reason must appear in the CP-visible item log, not only DebugLog'
        );
        $this->assertStringContainsString('9.1.0.4', $r['log'], 'the on-disk-vs-expected diagnostic must be included');
        $this->assertStringContainsString('9.1.0.5', $r['log']);
    }

    public function test_rollback_log_surfaces_the_basename_unresolved_reason(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:ghost-plugin/ghost-plugin.php']  = '1.0.0';
        $runner->available['plugin:ghost-plugin/ghost-plugin.php'] = '1.1.0';
        $runner->completeOverride['plugin:ghost-plugin/ghost-plugin.php']         = false;
        $runner->incompleteReasonOverride['plugin:ghost-plugin/ghost-plugin.php'] =
            'basename-unresolved (no installed plugin found for this slug); on-disk main file NOT found at "ghost-plugin/ghost-plugin.php" (expected version 1.1.0)';

        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'ghost-plugin/ghost-plugin.php', 'version' => 'latest']],
        ]);

        $r = $out['results'][0];
        $this->assertSame('failed', $r['status']);
        $this->assertStringContainsString(
            'Reason: basename-unresolved',
            $r['log'],
            'the basename-unresolved reason must appear in the CP-visible item log'
        );
    }

    public function test_rollback_log_has_no_reason_suffix_when_apply_itself_reported_failure(): void
    {
        // isComplete() is never invoked when $applied['ok'] is already false
        // (short-circuit, unchanged) — there is no reason to surface here;
        // $applied['log'] (already appended above) is the real explanation.
        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php']  = '5.0';
        $runner->available['plugin:akismet/akismet.php'] = '5.3';
        $runner->applySucceeds                           = false;

        $cmd = new UpdateCommand($this->spySnapshots(), $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $r = $out['results'][0];
        $this->assertSame('failed', $r['status']);
        $this->assertSame([], $runner->completeChecked, 'isComplete() must never be invoked after an ok:false apply');
        $this->assertStringNotContainsString(
            'Reason:',
            $r['log'],
            'no isComplete() reason exists for a genuine upgrader-reported failure'
        );
    }

    public function test_verified_good_apply_is_never_rolled_back(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php']  = '5.0';
        $runner->available['plugin:akismet/akismet.php'] = '5.3';
        // completeOverride left unset => the spy's default (true), the same
        // fail-open default UpdateRunner::isComplete() itself uses.

        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $r = $out['results'][0];
        $this->assertSame('succeeded', $r['status']);
        $this->assertCount(1, $snapshots->captured, 'a snapshot is still taken up front (D2)');
        $this->assertSame(
            [],
            $snapshots->restored,
            'a genuinely good, verified-complete apply must never be rolled back — guard against false-rollback'
        );
    }

    public function test_core_apply_failure_never_attempts_a_directory_restore(): void
    {
        // D3: core has no directory-level rollback here at all — even with
        // snapshot=true (which only records the prior version for
        // RollbackCommand's downgrade-by-version), a failed core apply must
        // never call SnapshotManager::restore().
        $runner = $this->spyRunner();
        $runner->versions['core:core']  = '6.4';
        $runner->available['core:core'] = '6.5';
        $runner->applySucceeds          = false;

        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'snapshot' => true,
            'items'    => [['type' => 'core', 'slug' => '', 'version' => 'latest']],
        ]);

        $this->assertSame('failed', $out['results'][0]['status']);
        $this->assertSame(
            [],
            $snapshots->restored,
            'core relies on B1 (resource guard) + Core_Upgrader temp-backup, never a directory restore (D3)'
        );
    }

    // ---- S8, revised: graceful degraded-mode proceed on a failed snapshot capture ----
    // (originally a hard refusal — GitHub issue #131 adversarial review;
    // downgraded to a best-effort proceed by an agent-only regression fix:
    // that refusal was itself the root cause of GH report "all plugin/theme
    // updates fail with a snapshot error" on any open_basedir/symlinked-
    // wp-content host whose realpath()-based containment check failed.)

    public function test_snapshot_capture_failure_still_applies_unprotected_for_plugin_theme(): void
    {
        // INVERTS the old S8 "refuses the apply" test, which encoded the
        // now-fixed regression as intended behavior: a plugin/theme item
        // whose pre-update snapshot capture FAILED must no longer be
        // refused outright. It must PROCEED with a best-effort, unprotected
        // apply — no UpdateGuard armed, no in-flight marker, nothing to roll
        // back to, but the apply itself must run and a genuinely successful
        // result must be reported succeeded, not failed.
        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php']  = '5.0';
        $runner->available['plugin:akismet/akismet.php'] = '5.3';

        $snapshots = new class extends SnapshotManager {
            /** @var array<int,array{string,string,string}> */
            public array $restored = [];

            public function capture(string $type, string $slug, string $fromVersion): array
            {
                return ['snapshot_id' => '', 'log' => 'Source directory not found; proceeding without snapshot.'];
            }

            public function restore(string $type, string $slug, string $snapshotId): array
            {
                $this->restored[] = [$type, $slug, $snapshotId];

                return ['ok' => false, 'log' => 'unreachable'];
            }
        };
        $cmd = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $r = $out['results'][0];
        $this->assertSame(
            'succeeded',
            $r['status'],
            'a failed snapshot capture must no longer refuse the apply — the item must still succeed'
        );
        $this->assertStringContainsString('Applied without a pre-update snapshot', $r['log']);
        $this->assertCount(
            1,
            $runner->applied,
            'a snapshot capture failure must no longer block apply() from ever being invoked'
        );
        $this->assertSame(
            [],
            $snapshots->restored,
            'without a captured snapshot id there is nothing to restore from — the guard must never arm'
        );
    }

    public function test_normal_update_still_applies_when_snapshot_cannot_be_captured(): void
    {
        // Faithful regression test for the reported symptom: "after updating
        // to the latest agent, ALL plugin AND theme updates fail with a
        // snapshot error, while core updates still work." Simulates a
        // SnapshotManager whose capture() always fails (mirroring
        // liveDir()'s realpath()-containment false-negative on an
        // open_basedir/symlinked-wp-content host) across an ACTIVE plugin,
        // an INACTIVE plugin, a theme, and core, all in one batch. Every
        // plugin/theme item must now actually apply() and report succeeded
        // (previously 'failed' with "refusing to apply unprotected"); core
        // is unaffected either way (D3 exemption, unchanged).
        $runner = $this->spyRunner();
        $runner->versions['plugin:active-plugin/active-plugin.php']    = '1.0';
        $runner->available['plugin:active-plugin/active-plugin.php']   = '1.1';
        $runner->versions['plugin:inactive-plugin/inactive-plugin.php'] = '2.0';
        $runner->available['plugin:inactive-plugin/inactive-plugin.php'] = '2.1';
        $runner->versions['theme:twentytwentyfour']  = '1.0';
        $runner->available['theme:twentytwentyfour'] = '1.1';
        $runner->versions['core:core']  = '6.4';
        $runner->available['core:core'] = '6.5';

        $neverCaptures = new class extends SnapshotManager {
            /** @var array<int,array{string,string,string}> */
            public array $restored = [];

            public function capture(string $type, string $slug, string $fromVersion): array
            {
                return ['snapshot_id' => '', 'log' => 'Source directory not found; proceeding without snapshot.'];
            }

            public function restore(string $type, string $slug, string $snapshotId): array
            {
                $this->restored[] = [$type, $slug, $snapshotId];

                return ['ok' => false, 'log' => 'unreachable'];
            }
        };
        $cmd = new UpdateCommand($neverCaptures, $runner);

        $out = $cmd->execute([], [
            'snapshot' => true,
            'items'    => [
                ['type' => 'plugin', 'slug' => 'active-plugin/active-plugin.php', 'version' => 'latest'],
                ['type' => 'plugin', 'slug' => 'inactive-plugin/inactive-plugin.php', 'version' => 'latest'],
                ['type' => 'theme', 'slug' => 'twentytwentyfour', 'version' => 'latest'],
                ['type' => 'core', 'slug' => '', 'version' => 'latest'],
            ],
        ]);

        $this->assertTrue($out['ok'], 'a snapshot-capture-unavailable host must not fail an otherwise-good batch');
        $this->assertCount(4, $out['results']);

        foreach ($out['results'] as $r) {
            $this->assertSame(
                'succeeded',
                $r['status'],
                $r['type'] . ':' . $r['slug'] . ' must apply successfully even though its snapshot could not be captured'
            );
        }

        $this->assertCount(
            4,
            $runner->applied,
            'every item — active plugin, inactive plugin, theme, and core — must reach apply()'
        );
        $this->assertSame(
            [],
            $neverCaptures->restored,
            'no restore may ever be attempted when no snapshot was captured to begin with'
        );
    }

    public function test_core_apply_proceeds_even_when_its_opt_in_snapshot_capture_fails(): void
    {
        // D3 exemption: core has no directory-level snapshot/restore
        // protection by design, so S8's refusal must be scoped to
        // plugin/theme only — a core snapshot capture failure (only
        // reachable when `snapshot=true` was requested) must never block a
        // core apply that never depended on it.
        $runner = $this->spyRunner();
        $runner->versions['core:core']  = '6.4';
        $runner->available['core:core'] = '6.5';

        $snapshots = new class extends SnapshotManager {
            public function capture(string $type, string $slug, string $fromVersion): array
            {
                return ['snapshot_id' => '', 'log' => 'Snapshot store unavailable; proceeding without snapshot.'];
            }
        };
        $cmd = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'snapshot' => true,
            'items'    => [['type' => 'core', 'slug' => '', 'version' => 'latest']],
        ]);

        $this->assertSame('succeeded', $out['results'][0]['status']);
        $this->assertCount(
            1,
            $runner->applied,
            'S8: core must proceed to apply even when its opt-in snapshot capture fails (D3 exemption)'
        );
    }

    // ---- S7: a THROW during post-apply verification must never false-rollback a good update ----

    /**
     * A runner spy whose currentVersion() returns the FROM version on its
     * first call (the read at the top of processItem()) and then THROWS on
     * every subsequent call — i.e. the post-apply re-read blows up.
     */
    private function spyRunnerThrowsOnSecondVersionRead(): UpdateRunner
    {
        return new class extends UpdateRunner {
            public array $applied = [];
            private int $versionCalls = 0;

            public function isInstalled(string $type, string $slug): bool
            {
                return true;
            }

            public function availableVersion(string $type, string $slug, string $requested): string
            {
                return '5.3';
            }

            public function currentVersion(string $type, string $slug): string
            {
                $this->versionCalls++;
                if ($this->versionCalls >= 2) {
                    throw new \RuntimeException('simulated post-apply verification blow-up');
                }

                return '5.0';
            }

            public function apply(string $type, string $slug, string $version): array
            {
                $this->applied[] = [$type, $slug, $version];

                return ['ok' => true, 'log' => 'applied'];
            }

            public function isComplete(string $type, string $slug, string $expectedVersion = ''): bool
            {
                return true;
            }

            public function wpCliAvailable(): bool
            {
                return false;
            }
        };
    }

    /**
     * A runner spy whose apply() succeeds and currentVersion() correctly
     * reports the bump, but isComplete() itself THROWS.
     */
    private function spyRunnerThrowsOnIsComplete(): UpdateRunner
    {
        return new class extends UpdateRunner {
            public array $applied = [];
            private int $versionCalls = 0;

            public function isInstalled(string $type, string $slug): bool
            {
                return true;
            }

            public function availableVersion(string $type, string $slug, string $requested): string
            {
                return '5.3';
            }

            public function currentVersion(string $type, string $slug): string
            {
                $this->versionCalls++;

                return $this->versionCalls === 1 ? '5.0' : '5.3';
            }

            public function apply(string $type, string $slug, string $version): array
            {
                $this->applied[] = [$type, $slug, $version];

                return ['ok' => true, 'log' => 'applied'];
            }

            public function isComplete(string $type, string $slug, string $expectedVersion = ''): bool
            {
                throw new \RuntimeException('simulated isComplete() blow-up');
            }

            public function wpCliAvailable(): bool
            {
                return false;
            }
        };
    }

    public function test_iscomplete_throw_after_a_good_apply_keeps_the_update_succeeded_not_rolled_back(): void
    {
        $runner    = $this->spyRunnerThrowsOnIsComplete();
        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $r = $out['results'][0];
        $this->assertSame(
            'succeeded',
            $r['status'],
            'S7: isComplete() throwing must be treated as inconclusive, not incomplete — the update is KEPT'
        );
        $this->assertSame('5.3', $r['to_version']);
        $this->assertSame([], $snapshots->restored, 'S7: a throw during verification must never trigger a restore');
        $this->assertStringContainsString('KEPT', $r['log']);
    }

    public function test_currentversion_throw_after_a_good_apply_never_triggers_a_false_rollback(): void
    {
        $runner    = $this->spyRunnerThrowsOnSecondVersionRead();
        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $r = $out['results'][0];
        // The exact reported status is secondary here — the post-apply
        // version could not be re-read at all, so it conservatively falls
        // back to from_version. The CONTRACT S7 guards is that a
        // verification throw must NEVER be treated as an affirmative
        // "incomplete" result and must NEVER trigger a restore of a
        // genuinely good apply.
        $this->assertNotSame('failed', $r['status']);
        $this->assertSame([], $snapshots->restored, 'S7: a throw during verification must never trigger a restore');
        $this->assertStringContainsString('KEPT', $r['log']);
    }

    public function test_verification_throw_after_a_genuinely_failed_apply_still_proceeds_to_restore(): void
    {
        // S7 must not be exploitable to KEEP a genuinely bad apply: when
        // applied['ok'] is already false, a throw while trying to re-read
        // the resulting version must still fall through to the normal
        // incomplete/failed/restore path, not be upgraded to "complete".
        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php']  = '5.0';
        $runner->available['plugin:akismet/akismet.php'] = '5.3';
        $runner->applySucceeds                           = false;

        $throwingRunner = new class ($runner) extends UpdateRunner {
            public function __construct(private UpdateRunner $inner)
            {
            }

            public function isInstalled(string $type, string $slug): bool
            {
                return $this->inner->isInstalled($type, $slug);
            }

            public function availableVersion(string $type, string $slug, string $requested): string
            {
                return $this->inner->availableVersion($type, $slug, $requested);
            }

            public function apply(string $type, string $slug, string $version): array
            {
                return $this->inner->apply($type, $slug, $version);
            }

            public function currentVersion(string $type, string $slug): string
            {
                static $calls = 0;
                $calls++;
                if ($calls >= 2) {
                    throw new \RuntimeException('simulated post-apply verification blow-up');
                }

                return $this->inner->currentVersion($type, $slug);
            }

            public function wpCliAvailable(): bool
            {
                return false;
            }
        };

        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $throwingRunner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $r = $out['results'][0];
        $this->assertSame(
            'failed',
            $r['status'],
            'S7 must not mask a genuine upgrader failure: a throw AFTER an already-failed apply must still restore'
        );
        $this->assertCount(1, $snapshots->restored, 'a genuinely failed apply must still be auto-restored despite the verification throw');
    }

    // ---- S4: bounded apply time limit + out-of-band in-flight reconcile (GitHub issue #131 adversarial review) ----

    public function test_apply_time_limit_is_bounded_not_infinite(): void
    {
        // The original bug (set_time_limit(0), infinite) is exactly what
        // made the shutdown guard non-functional for an FPM hard-kill; the
        // fix must set a finite, generous bound instead.
        $seenLimit = null;
        Functions\expect('set_time_limit')
            ->once()
            ->andReturnUsing(function (int $seconds) use (&$seenLimit): bool {
                $seenLimit = $seconds;

                return true;
            });

        $runner = $this->spyRunner();
        $cmd    = new UpdateCommand($this->spySnapshots(), $runner);

        $cmd->execute([], ['items' => []]);

        $this->assertSame(900, $seenLimit, 'S4: the apply time limit must be bounded (900s), not infinite (0)');
    }

    public function test_stale_update_in_flight_marker_is_reconciled_at_the_start_of_a_fresh_run(): void
    {
        $tmp = sys_get_temp_dir() . '/wpmgr-s4-heal-' . bin2hex(random_bytes(6));
        mkdir($tmp, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $tmp]);

        UpdateInFlight::mark('plugin', 'ghost/ghost.php', 'snap_ghost');

        // Age the marker well past the staleness threshold (1200s) so it is
        // treated as orphaned by a prior hard-killed request, not a
        // currently-running apply.
        $markerFiles = glob($tmp . '/wpmgr-update-inflight/*.json') ?: [];
        $this->assertCount(1, $markerFiles, 'precondition: mark() must have written exactly one marker file');
        $data = json_decode((string) file_get_contents($markerFiles[0]), true);
        $data['ts'] = time() - 1300;
        file_put_contents($markerFiles[0], (string) json_encode($data));

        $snapshots = $this->spySnapshots();
        $runner    = $this->spyRunner();
        // F2 (final-hardening review) — the reconcile only restores when the
        // live directory is verified genuinely INCOMPLETE; this test's
        // scenario (a hard-killed apply that never finished) is exactly that.
        $runner->completeOverride['plugin:ghost/ghost.php'] = false;
        $cmd = new UpdateCommand($snapshots, $runner);

        // No items in THIS request — isolates the start-of-run reconcile
        // from any per-item mark()/clear() this run might otherwise do.
        $cmd->execute([], ['items' => []]);

        $this->assertSame(
            [['plugin', 'ghost/ghost.php', 'snap_ghost']],
            $snapshots->restored,
            'S4: a stale in-flight marker over a genuinely incomplete live directory must be auto-restored at the start of the next update command'
        );
        $this->assertCount(
            0,
            glob($tmp . '/wpmgr-update-inflight/*.json') ?: [],
            'S4: a reconciled marker must be cleared so it is never reconciled twice'
        );

        $this->rrmdir($tmp);
    }

    public function test_stale_marker_over_a_complete_live_directory_is_cleared_without_restoring(): void
    {
        // F2 (issue #131 final-hardening review) — a marker can legitimately
        // survive to the start of the NEXT run even for a SUCCESSFUL update
        // (a kill landing in the old markClean()-to-`finally` window, now
        // closed at the source — this integration test exercises the
        // defense-in-depth backstop regardless). The live directory here is
        // verified COMPLETE, so the reconcile must clear the marker WITHOUT
        // ever calling restore() — blindly restoring here would revert a
        // perfectly good, already-finished update.
        $tmp = sys_get_temp_dir() . '/wpmgr-f2-healthy-' . bin2hex(random_bytes(6));
        mkdir($tmp, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $tmp]);

        UpdateInFlight::mark('plugin', 'healthy/healthy.php', 'snap_healthy');

        $markerFiles = glob($tmp . '/wpmgr-update-inflight/*.json') ?: [];
        $this->assertCount(1, $markerFiles, 'precondition: mark() must have written exactly one marker file');
        $data = json_decode((string) file_get_contents($markerFiles[0]), true);
        $data['ts'] = time() - 1300;
        file_put_contents($markerFiles[0], (string) json_encode($data));

        $snapshots = $this->spySnapshots();
        $runner    = $this->spyRunner();
        // Default completeOverride is TRUE (healthy) — see spyRunner()'s doc.
        $cmd = new UpdateCommand($snapshots, $runner);

        $cmd->execute([], ['items' => []]);

        $this->assertSame(
            [],
            $snapshots->restored,
            'F2: a stale marker over an already-healthy/complete live directory must NEVER trigger a restore'
        );
        $this->assertCount(
            0,
            glob($tmp . '/wpmgr-update-inflight/*.json') ?: [],
            'F2: the marker must still be cleared even though no restore happened'
        );

        $this->rrmdir($tmp);
    }

    public function test_fresh_update_in_flight_marker_is_not_touched_by_the_reconcile(): void
    {
        $tmp = sys_get_temp_dir() . '/wpmgr-s4-fresh-' . bin2hex(random_bytes(6));
        mkdir($tmp, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $tmp]);

        // Freshly written — well under the staleness threshold. May belong
        // to a still-running apply and must be left alone.
        UpdateInFlight::mark('plugin', 'busy/busy.php', 'snap_busy');

        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $this->spyRunner());

        $cmd->execute([], ['items' => []]);

        $this->assertSame(
            [],
            $snapshots->restored,
            'S4: a fresh in-flight marker may belong to a still-running apply and must never be reconciled'
        );
        $this->assertCount(
            1,
            glob($tmp . '/wpmgr-update-inflight/*.json') ?: [],
            'a fresh marker must not be removed either'
        );

        $this->rrmdir($tmp);
    }

    // ---- C: opportunistic snapshot-store GC (issue #131 final-hardening review) ----

    public function test_snapshot_gc_backstop_runs_opportunistically_at_the_start_of_every_update_command(): void
    {
        $tmp = sys_get_temp_dir() . '/wpmgr-c-gc-' . bin2hex(random_bytes(6));
        mkdir($tmp, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $tmp]);

        $snapshotsBase = $tmp . '/wpmgr-snapshots';
        mkdir($snapshotsBase, 0755, true);

        // An orphaned snapshot well past the 72h GC backstop TTL — the exact
        // class of orphan SnapshotManager::gcExpired() exists to reclaim
        // (e.g. a since-uninstalled/renamed slug that will never again
        // trigger the per-slug prune-at-capture path for itself). WP-Cron is
        // unreliable on DISABLE_WP_CRON/dormant sites, so this must ALSO run
        // opportunistically here, not cron-only.
        $orphan = $snapshotsBase . '/snap_' . bin2hex(random_bytes(12));
        mkdir($orphan, 0755, true);
        file_put_contents($orphan . '/meta.json', (string) json_encode([
            'type'         => 'plugin',
            'slug'         => 'long-gone/long-gone.php',
            'from_version' => '1.0',
            'created_at'   => time() - (73 * 3600),
        ]));

        $cmd = new UpdateCommand($this->spySnapshots(), $this->spyRunner());

        // No items in THIS request — isolates the opportunistic GC call from
        // any per-item capture()/restore() this run might otherwise do.
        $cmd->execute([], ['items' => []]);

        $this->assertFalse(
            is_dir($orphan),
            'C: SnapshotManager::gcExpired() must run opportunistically at the start of every `update` command, not cron-only'
        );

        $this->rrmdir($tmp);
    }

    public function test_in_flight_marker_is_written_before_apply_and_cleared_after_a_synchronous_success(): void
    {
        $tmp = sys_get_temp_dir() . '/wpmgr-s4-lifecycle-' . bin2hex(random_bytes(6));
        mkdir($tmp, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $tmp]);

        $markerDir = $tmp . '/wpmgr-update-inflight';

        // A runner whose apply() inspects the filesystem to prove the marker
        // was written BEFORE apply() is invoked.
        $runner = new class ($markerDir) extends UpdateRunner {
            public bool $markerPresentDuringApply = false;
            private bool $applied                 = false;

            public function __construct(private string $markerDir)
            {
            }

            public function isInstalled(string $type, string $slug): bool
            {
                return true;
            }

            public function currentVersion(string $type, string $slug): string
            {
                return $this->applied ? '5.3' : '5.0';
            }

            public function availableVersion(string $type, string $slug, string $requested): string
            {
                return '5.3';
            }

            public function apply(string $type, string $slug, string $version): array
            {
                $files = glob($this->markerDir . '/*.json') ?: [];
                $this->markerPresentDuringApply = count($files) === 1;
                $this->applied                  = true;

                return ['ok' => true, 'log' => 'applied'];
            }

            public function isComplete(string $type, string $slug, string $expectedVersion = ''): bool
            {
                return true;
            }

            public function wpCliAvailable(): bool
            {
                return false;
            }
        };

        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $this->assertTrue(
            $runner->markerPresentDuringApply,
            'S4: the in-flight marker must be written BEFORE apply() is invoked'
        );
        $this->assertSame('succeeded', $out['results'][0]['status']);
        $this->assertCount(
            0,
            glob($markerDir . '/*.json') ?: [],
            'S4: a synchronously successful item must clear its own in-flight marker before returning'
        );

        $this->rrmdir($tmp);
    }

    // =========================================================================
    // GitHub issue #210 — update-watchdog ARM step
    // =========================================================================

    /**
     * Guard-define WP_CONTENT_DIR (same idiom used elsewhere in this suite —
     * see SnapshotManagerTest::ensurePluginRootConstants()) and return the
     * update-watchdog marker file's absolute path.
     */
    private function watchdogMarkerFile(): string
    {
        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', sys_get_temp_dir() . '/wpmgr-shared-wp-content');
        }
        if (!is_dir(WP_CONTENT_DIR)) {
            mkdir(WP_CONTENT_DIR, 0755, true);
        }

        return rtrim((string) WP_CONTENT_DIR, '/\\') . '/wpmgr-update-watchdog/watchdog-marker.json';
    }

    private function cleanWatchdogMarker(): void
    {
        $file = $this->watchdogMarkerFile();
        if (file_exists($file)) {
            unlink($file); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
        }
    }

    /**
     * A snapshot spy whose resolvedRestorePaths() returns fixed, controlled
     * absolute paths (rather than the real WP_PLUGIN_DIR/uploads-dependent
     * resolution), so the ARM step under test can be exercised deterministically.
     */
    private function spySnapshotsWithResolvedPaths(string $live, string $payload): SnapshotManager
    {
        return new class ($live, $payload) extends SnapshotManager {
            /** @var array<int,array{string,string,string}> */
            public array $captured = [];
            /**
             * @var array<int,string> Snapshot ids passed to markSucceeded()
             *     (GitHub issue #226).
             */
            public array $markedSucceeded = [];

            public function __construct(private string $live, private string $payload)
            {
            }

            public function capture(string $type, string $slug, string $fromVersion): array
            {
                $this->captured[] = [$type, $slug, $fromVersion];

                return ['snapshot_id' => 'snap_test123', 'log' => 'captured'];
            }

            public function restore(string $type, string $slug, string $snapshotId): array
            {
                return ['ok' => true, 'log' => 'restored'];
            }

            public function snapshotExists(string $snapshotId): bool
            {
                return true;
            }

            public function resolvedRestorePaths(string $type, string $slug, string $snapshotId): array
            {
                return ['live' => $this->live, 'payload' => $this->payload];
            }

            public function markSucceeded(string $snapshotId): void
            {
                $this->markedSucceeded[] = $snapshotId;
            }
        };
    }

    public function test_successful_apply_arms_the_update_watchdog_marker_with_the_resolved_absolute_paths(): void
    {
        $this->cleanWatchdogMarker();

        $live    = sys_get_temp_dir() . '/wpmgr-watchdog-arm-live-' . bin2hex(random_bytes(6));
        $payload = sys_get_temp_dir() . '/wpmgr-watchdog-arm-payload-' . bin2hex(random_bytes(6));
        mkdir($live, 0755, true);
        mkdir($payload, 0755, true);

        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php'] = '5.0';

        $snapshots = $this->spySnapshotsWithResolvedPaths($live, $payload);
        $cmd       = new UpdateCommand($snapshots, $runner);

        $before = time();
        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => '5.3']],
        ]);
        $after = time();

        $this->assertSame('succeeded', $out['results'][0]['status']);
        $this->assertFileExists($this->watchdogMarkerFile(), 'a succeeded plugin apply must arm the update-watchdog marker');

        $decoded = json_decode((string) file_get_contents($this->watchdogMarkerFile()), true);
        $this->assertCount(1, $decoded['markers']);
        $entry = $decoded['markers'][0];
        $this->assertSame('plugin', $entry['type']);
        $this->assertSame('akismet/akismet.php', $entry['slug']);
        $this->assertSame('snap_test123', $entry['snapshot_id']);
        $this->assertSame($live, $entry['live_dir']);
        $this->assertSame($payload, $entry['payload_dir']);
        $this->assertSame('5.3', $entry['to_version'], 'MEDIUM-1b: the armed to_version must be threaded through from UpdateCommand');

        $ttl = $entry['expires_at'] - $entry['applied_at'];
        $this->assertSame(UpdateWatchdogMarker::TTL_SECONDS, $ttl);
        $this->assertLessThan(600, $ttl, 'ARM test: TTL must be strictly less than SnapshotManager::MIN_KEEP_AGE_SECONDS (600)');
        $this->assertGreaterThanOrEqual($before, $entry['applied_at']);
        $this->assertLessThanOrEqual($after, $entry['applied_at']);

        $this->rrmdir($live);
        $this->rrmdir($payload);
        $this->cleanWatchdogMarker();
    }

    public function test_failed_apply_does_not_arm_the_update_watchdog_marker(): void
    {
        $this->cleanWatchdogMarker();

        $live    = sys_get_temp_dir() . '/wpmgr-watchdog-arm-live-' . bin2hex(random_bytes(6));
        $payload = sys_get_temp_dir() . '/wpmgr-watchdog-arm-payload-' . bin2hex(random_bytes(6));
        mkdir($live, 0755, true);
        mkdir($payload, 0755, true);

        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php'] = '5.0';
        $runner->applySucceeds = false;

        $snapshots = $this->spySnapshotsWithResolvedPaths($live, $payload);
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => '5.3']],
        ]);

        $this->assertSame('failed', $out['results'][0]['status']);
        $this->assertFileDoesNotExist($this->watchdogMarkerFile(), 'a failed apply must never arm the update-watchdog marker');

        $this->rrmdir($live);
        $this->rrmdir($payload);
    }

    public function test_skipped_apply_does_not_arm_the_update_watchdog_marker(): void
    {
        $this->cleanWatchdogMarker();

        $live    = sys_get_temp_dir() . '/wpmgr-watchdog-arm-live-' . bin2hex(random_bytes(6));
        $payload = sys_get_temp_dir() . '/wpmgr-watchdog-arm-payload-' . bin2hex(random_bytes(6));
        mkdir($live, 0755, true);
        mkdir($payload, 0755, true);

        $runner = $this->spyRunner();
        // No version seeded -> isInstalled() reports false -> skipped.
        $snapshots = $this->spySnapshotsWithResolvedPaths($live, $payload);
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'never-installed/never-installed.php', 'version' => 'latest']],
        ]);

        $this->assertSame('skipped', $out['results'][0]['status']);
        $this->assertFileDoesNotExist($this->watchdogMarkerFile(), 'a skipped (not-installed) item must never arm the update-watchdog marker');

        $this->rrmdir($live);
        $this->rrmdir($payload);
    }

    public function test_up_to_date_apply_does_not_arm_the_update_watchdog_marker(): void
    {
        $this->cleanWatchdogMarker();

        $live    = sys_get_temp_dir() . '/wpmgr-watchdog-arm-live-' . bin2hex(random_bytes(6));
        $payload = sys_get_temp_dir() . '/wpmgr-watchdog-arm-payload-' . bin2hex(random_bytes(6));
        mkdir($live, 0755, true);
        mkdir($payload, 0755, true);

        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php']  = '5.3';
        $runner->available['plugin:akismet/akismet.php'] = '5.3';

        $snapshots = $this->spySnapshotsWithResolvedPaths($live, $payload);
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => '5.3']],
        ]);

        $this->assertSame('up_to_date', $out['results'][0]['status']);
        $this->assertFileDoesNotExist($this->watchdogMarkerFile(), 'an up_to_date item must never arm the update-watchdog marker — nothing changed to guard against');

        $this->rrmdir($live);
        $this->rrmdir($payload);
    }

    public function test_core_succeeded_apply_never_arms_the_update_watchdog_marker(): void
    {
        $this->cleanWatchdogMarker();

        $live    = sys_get_temp_dir() . '/wpmgr-watchdog-arm-live-' . bin2hex(random_bytes(6));
        $payload = sys_get_temp_dir() . '/wpmgr-watchdog-arm-payload-' . bin2hex(random_bytes(6));
        mkdir($live, 0755, true);
        mkdir($payload, 0755, true);

        $runner = $this->spyRunner();
        $runner->versions['core:core'] = '6.4.2';

        $snapshots = $this->spySnapshotsWithResolvedPaths($live, $payload);
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'snapshot' => true,
            'items'    => [['type' => 'core', 'slug' => 'core', 'version' => '6.4.3']],
        ]);

        $this->assertSame('succeeded', $out['results'][0]['status']);
        $this->assertFileDoesNotExist(
            $this->watchdogMarkerFile(),
            'core has no directory-level snapshot (D3) and must never arm the update-watchdog marker, even when the apply itself succeeded'
        );

        $this->rrmdir($live);
        $this->rrmdir($payload);
    }

    public function test_arm_is_skipped_when_resolved_restore_paths_cannot_be_determined(): void
    {
        $this->cleanWatchdogMarker();

        // resolvedRestorePaths() returning empty strings (e.g. the live
        // directory could not be resolved) must never write a marker.
        $snapshots = $this->spySnapshotsWithResolvedPaths('', '');

        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php'] = '5.0';
        $cmd = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => '5.3']],
        ]);

        $this->assertSame('succeeded', $out['results'][0]['status'], 'the apply itself must still succeed');
        $this->assertFileDoesNotExist($this->watchdogMarkerFile(), 'an unresolvable live/payload path must silently skip arming, never affect the response');
    }

    // =========================================================================
    // GitHub issue #226 — SnapshotManager::markSucceeded() call site
    // =========================================================================

    public function test_successful_apply_marks_the_snapshot_succeeded_without_disturbing_the_watchdog_arm(): void
    {
        $this->cleanWatchdogMarker();

        $live    = sys_get_temp_dir() . '/wpmgr-mark-succeeded-live-' . bin2hex(random_bytes(6));
        $payload = sys_get_temp_dir() . '/wpmgr-mark-succeeded-payload-' . bin2hex(random_bytes(6));
        mkdir($live, 0755, true);
        mkdir($payload, 0755, true);

        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php'] = '5.0';

        $snapshots = $this->spySnapshotsWithResolvedPaths($live, $payload);
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => '5.3']],
        ]);

        $this->assertSame('succeeded', $out['results'][0]['status']);
        $this->assertSame(
            ['snap_test123'],
            $snapshots->markedSucceeded,
            'GitHub issue #226: a genuinely successful apply must mark its captured snapshot succeeded'
        );
        $this->assertFileExists(
            $this->watchdogMarkerFile(),
            'marking the snapshot succeeded must not disturb the watchdog arm — the snapshot must still exist for it'
        );

        $this->rrmdir($live);
        $this->rrmdir($payload);
        $this->cleanWatchdogMarker();
    }

    public function test_failed_apply_does_not_mark_the_snapshot_succeeded(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php']  = '5.0';
        $runner->available['plugin:akismet/akismet.php'] = '5.3';
        $runner->applySucceeds                           = false;

        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => 'latest']],
        ]);

        $this->assertSame('failed', $out['results'][0]['status']);
        $this->assertSame(
            [],
            $snapshots->markedSucceeded,
            'GitHub issue #226: a failed apply must never mark its snapshot succeeded'
        );
    }

    public function test_incomplete_apply_that_is_auto_restored_does_not_mark_the_snapshot_succeeded(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:kadence-blocks/kadence-blocks.php']  = '3.2.0';
        $runner->available['plugin:kadence-blocks/kadence-blocks.php'] = '3.2.1';
        $runner->completeOverride['plugin:kadence-blocks/kadence-blocks.php'] = false;

        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'plugin', 'slug' => 'kadence-blocks/kadence-blocks.php', 'version' => 'latest']],
        ]);

        $this->assertSame('failed', $out['results'][0]['status']);
        $this->assertSame(
            [],
            $snapshots->markedSucceeded,
            'GitHub issue #226: an incomplete apply that gets auto-restored must never mark its snapshot succeeded'
        );
    }

    public function test_up_to_date_apply_does_not_mark_any_snapshot_succeeded(): void
    {
        $runner = $this->spyRunner();
        $runner->versions['plugin:akismet/akismet.php']  = '5.3';
        $runner->available['plugin:akismet/akismet.php'] = '5.3';

        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'snapshot' => true,
            'items'    => [['type' => 'plugin', 'slug' => 'akismet/akismet.php', 'version' => '5.3']],
        ]);

        $this->assertSame('up_to_date', $out['results'][0]['status']);
        $this->assertSame(
            [],
            $snapshots->markedSucceeded,
            'an up_to_date item (nothing changed) must never mark a snapshot succeeded'
        );
    }

    public function test_core_succeeded_apply_never_marks_a_snapshot_succeeded(): void
    {
        // D3: core has no directory-level snapshot at all — snapshotId is
        // always '' for core — so there is nothing for markSucceeded() to
        // mark, even on a genuinely successful core apply.
        $runner = $this->spyRunner();
        $runner->versions['core:core']  = '6.4';
        $runner->available['core:core'] = '6.5';

        $snapshots = $this->spySnapshots();
        $cmd       = new UpdateCommand($snapshots, $runner);

        $out = $cmd->execute([], [
            'items' => [['type' => 'core', 'slug' => '', 'version' => 'latest']],
        ]);

        $this->assertSame('succeeded', $out['results'][0]['status']);
        $this->assertSame([], $snapshots->markedSucceeded);
    }

    /** Recursive delete used only for test fixture cleanup. */
    private function rrmdir(string $dir): void
    {
        if (!is_dir($dir)) {
            return;
        }
        $items = @scandir($dir);
        if ($items === false) {
            return;
        }
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $path = $dir . '/' . $item;
            if (is_dir($path) && !is_link($path)) {
                $this->rrmdir($path);
            } else {
                unlink($path); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
            }
        }
        rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- test-only fixture cleanup
    }
}
