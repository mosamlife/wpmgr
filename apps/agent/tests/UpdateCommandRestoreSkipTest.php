<?php
/**
 * UpdateCommandRestoreSkipTest - PIECE 1, the restore-skip, end to end
 * (GitHub issue #328).
 *
 * The gate opens for exactly ONE combination out of four, and the other three
 * must behave exactly as the previous release did. Read
 * test_an_unclassified_failure_restores_exactly_as_before() first: it is the
 * strict-narrowing proof, and if it ever fails, this feature has started
 * widening the set of restores rather than shrinking it.
 *
 * Real plugin directories on disk, because the second half of the decision is
 * DestinationVerifier actually reading them.
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
final class UpdateCommandRestoreSkipTest extends TestCase
{
    /** Scratch tree for snapshot payload copies. */
    private string $scratch = '';

    /** Plugins root this process uses. */
    private string $pluginsRoot = '';

    /** @var array<int,string> Fixture directories to remove in tear-down. */
    private array $created = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->scratch = sys_get_temp_dir() . '/wpmgr-skip-' . bin2hex(random_bytes(6));
        mkdir($this->scratch, 0777, true);

        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', sys_get_temp_dir() . '/wpmgr-shared-wp-content');
        }
        if (!is_dir((string) constant('WP_CONTENT_DIR'))) {
            mkdir((string) constant('WP_CONTENT_DIR'), 0777, true);
        }
        if (!defined('WP_PLUGIN_DIR')) {
            define('WP_PLUGIN_DIR', rtrim((string) constant('WP_CONTENT_DIR'), '/\\') . '/plugins');
        }
        $this->pluginsRoot = rtrim((string) constant('WP_PLUGIN_DIR'), '/\\');
        if (!is_dir($this->pluginsRoot)) {
            mkdir($this->pluginsRoot, 0777, true);
        }
        $this->created = [];
    }

    protected function tear_down(): void
    {
        foreach ($this->created as $dir) {
            $this->deleteTree($dir);
        }
        $this->deleteTree($this->scratch);
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
     * Create a real plugin directory plus a matching snapshot payload copy.
     *
     * @param string $version Version reported in the header.
     * @return array{folder:string,slug:string,dir:string,payload:string}
     */
    private function makePluginWithPayload(string $version = '1.2.3'): array
    {
        $folder = 'wpmgr-skip-' . bin2hex(random_bytes(4));
        $dir    = $this->pluginsRoot . '/' . $folder;
        mkdir($dir, 0777, true);
        $this->created[] = $dir;

        file_put_contents(
            $dir . '/' . $folder . '.php',
            "<?php\n/**\n * Plugin Name: Skip Fixture\n * Version: " . $version . "\n */\n"
        );
        file_put_contents($dir . '/readme.txt', "=== Skip Fixture ===\n");

        $payload = $this->scratch . '/' . $folder . '-payload';
        mkdir($payload, 0777, true);
        foreach (array_diff((array) scandir($dir), ['.', '..']) as $entry) {
            copy($dir . '/' . $entry, $payload . '/' . $entry);
        }

        return [
            'folder'  => $folder,
            'slug'    => $folder . '/' . $folder . '.php',
            'dir'     => $dir,
            'payload' => $payload,
        ];
    }

    /**
     * A runner spy whose apply() returns a caller-chosen outcome array. The
     * outcome shape is the ONLY channel the decision reads, so a test can
     * express "core said it failed before touching anything" precisely.
     *
     * @param array<string,mixed> $outcome apply() return value.
     * @param string              $version Installed version reported.
     * @return UpdateRunner
     */
    private function runnerReturning(array $outcome, string $version = '1.2.3'): UpdateRunner
    {
        return new class ($outcome, $version) extends UpdateRunner {
            /** @var array<string,mixed> */
            private array $outcome;

            private string $version;

            /** @var int */
            public int $applyCalls = 0;

            /** @var array<int,array{string,bool,bool}> */
            public array $reactivations = [];

            /**
             * @param array<string,mixed> $outcome apply() return value.
             * @param string              $version Installed version.
             */
            public function __construct(array $outcome, string $version)
            {
                $this->outcome = $outcome;
                $this->version = $version;
            }

            public function currentVersion(string $type, string $slug): string
            {
                return $this->version;
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

            public function reactivateIfCoreDeactivated(string $slug, bool $wasActive, bool $wasNetworkActive): array
            {
                $this->reactivations[] = [$slug, $wasActive, $wasNetworkActive];

                return ['attempted' => true, 'ok' => true, 'message' => ''];
            }
        };
    }

    /**
     * A snapshot spy that records the calls this decision is allowed (and not
     * allowed) to make.
     *
     * @param string $snapshotId Snapshot id to hand back from capture().
     * @param string $payloadDir Payload path to hand back from payloadDir().
     * @return SnapshotManager
     */
    private function spySnapshots(string $snapshotId, string $payloadDir): SnapshotManager
    {
        return new class ($snapshotId, $payloadDir) extends SnapshotManager {
            private string $snapshotId;

            private string $payload;

            /** @var array<int,array{string,string,string}> */
            public array $restored = [];

            /** @var array<int,string> */
            public array $markedSucceeded = [];

            /** @var array<int,array{string,string,string}> */
            public array $markedRestoreSkipped = [];

            /**
             * @param string $snapshotId Snapshot id.
             * @param string $payload    Payload path.
             */
            public function __construct(string $snapshotId, string $payload)
            {
                $this->snapshotId = $snapshotId;
                $this->payload    = $payload;
            }

            public function capture(string $type, string $slug, string $fromVersion): array
            {
                return ['snapshot_id' => $this->snapshotId, 'log' => 'captured'];
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
                return $this->payload;
            }

            public function markSucceeded(string $snapshotId): void
            {
                $this->markedSucceeded[] = $snapshotId;
            }

            public function markRestoreSkipped(string $snapshotId, string $failureCode, string $verification): void
            {
                $this->markedRestoreSkipped[] = [$snapshotId, $failureCode, $verification];
            }
        };
    }

    /**
     * The outcome shape a real pre-install failure produces: the reported
     * copy_failed_ziparchive from GitHub issue #328's own report.
     *
     * @return array<string,mixed>
     */
    private function untouchedFailure(): array
    {
        return [
            'ok'                   => false,
            'log'                  => 'Update failed: Could not copy file.',
            'destination_touched'  => false,
            'failure_code'         => 'copy_failed_ziparchive',
            'failure_data'         => 'wp-seopress/vendor/psr/log/Psr/Log/Test/DummyTest.php',
            'failure_stage'        => 'unpack',
            'may_have_deactivated' => false,
            'was_active'           => false,
            'was_network_active'   => false,
        ];
    }

    // =====================================================================
    // The one combination that skips
    // =====================================================================

    /** THE #328 SHAPE, END TO END. */
    public function test_a_pre_install_failure_over_an_intact_directory_skips_the_restore(): void
    {
        $fixture   = $this->makePluginWithPayload('1.2.3');
        $snapshots = $this->spySnapshots('snap_skip_1', $fixture['payload']);

        $out = (new UpdateCommand($snapshots, $this->runnerReturning($this->untouchedFailure())))->execute([], [
            'items' => [['type' => 'plugin', 'slug' => $fixture['slug'], 'version' => 'latest']],
        ]);

        $result = $out['results'][0];

        $this->assertSame([], $snapshots->restored, 'a directory core never opened must not be restored over');
        $this->assertSame('failed', $result['status'], 'the update genuinely did not happen; only the restore is skipped');
        $this->assertSame($result['from_version'], $result['to_version'], 'nothing moved, so nothing may be reported as moved');
        $this->assertStringContainsString('was re-checked after the failure and is unchanged', $result['log']);
        $this->assertStringContainsString('copy_failed_ziparchive', $result['log']);
    }

    /**
     * THE COPY INVARIANT. "Update incomplete; auto-restored the pre-update
     * snapshot." asserts a restore happened. It must never appear when one did
     * not, and it is the sentence an operator has historically seen for this
     * entire class of failure.
     */
    public function test_a_skipped_restore_never_claims_to_have_auto_restored(): void
    {
        $fixture   = $this->makePluginWithPayload('1.2.3');
        $snapshots = $this->spySnapshots('snap_skip_2', $fixture['payload']);

        $out = (new UpdateCommand($snapshots, $this->runnerReturning($this->untouchedFailure())))->execute([], [
            'items' => [['type' => 'plugin', 'slug' => $fixture['slug'], 'version' => 'latest']],
        ]);

        $this->assertStringNotContainsString('auto-restored', $out['results'][0]['log']);
        $this->assertStringNotContainsString('auto-restore', $out['results'][0]['log']);
        $this->assertStringNotContainsString(
            'no pre-update snapshot was available to auto-restore',
            $out['results'][0]['log']
        );
    }

    /**
     * markSucceeded() flips the snapshot's reclaim threshold from 72h to 1h.
     * Calling it here would delete the one piece of evidence that could prove
     * this decision wrong, within an hour of a FAILED update. Its own docblock
     * forbids calling it before independent verification of a SUCCESS.
     */
    public function test_a_skipped_restore_never_marks_the_snapshot_succeeded(): void
    {
        $fixture   = $this->makePluginWithPayload('1.2.3');
        $snapshots = $this->spySnapshots('snap_skip_3', $fixture['payload']);

        (new UpdateCommand($snapshots, $this->runnerReturning($this->untouchedFailure())))->execute([], [
            'items' => [['type' => 'plugin', 'slug' => $fixture['slug'], 'version' => 'latest']],
        ]);

        $this->assertSame([], $snapshots->markedSucceeded);
        $this->assertCount(1, $snapshots->markedRestoreSkipped, 'the snapshot must be LABELLED, on its unchanged TTL');
        $this->assertSame('snap_skip_3', $snapshots->markedRestoreSkipped[0][0]);
        $this->assertSame('copy_failed_ziparchive', $snapshots->markedRestoreSkipped[0][1]);
    }

    /**
     * The guard is only constructed when a snapshot was captured, and this path
     * is reachable on any host where capture() failed. An unguarded call here
     * would be an Error caught by the outer handler, which returns the bare
     * "Update error." and loses the log, both versions and the entire
     * explanation, on exactly the population documented as having regressed
     * twice already.
     */
    public function test_a_skip_with_no_snapshot_does_not_touch_the_null_guard(): void
    {
        $fixture = $this->makePluginWithPayload('1.2.3');

        $snapshots = new class extends SnapshotManager {
            /** @var array<int,array{string,string,string}> */
            public array $restored = [];

            public function capture(string $type, string $slug, string $fromVersion): array
            {
                return ['snapshot_id' => '', 'log' => 'Snapshot store unavailable; proceeding without snapshot.'];
            }

            public function restore(string $type, string $slug, string $snapshotId): array
            {
                $this->restored[] = [$type, $slug, $snapshotId];

                return ['ok' => true, 'log' => 'restored'];
            }
        };

        $out = (new UpdateCommand($snapshots, $this->runnerReturning($this->untouchedFailure())))->execute([], [
            'items' => [['type' => 'plugin', 'slug' => $fixture['slug'], 'version' => 'latest']],
        ]);

        $result = $out['results'][0];
        $this->assertSame([], $snapshots->restored);
        $this->assertNotSame('Update error.', $result['log'], 'the skip path must not throw through the outer handler');
        $this->assertStringContainsString('no restore was needed', $result['log']);
        $this->assertSame('', $result['snapshot_id']);
    }

    // =====================================================================
    // The three combinations that still restore
    // =====================================================================

    /** Classification false, verification FALSE (the directory really is gone). */
    public function test_a_destroyed_directory_still_restores(): void
    {
        $fixture   = $this->makePluginWithPayload('1.2.3');
        $snapshots = $this->spySnapshots('snap_mod', $fixture['payload']);

        $this->deleteTree($fixture['dir']);

        $out = (new UpdateCommand($snapshots, $this->runnerReturning($this->untouchedFailure())))->execute([], [
            'items' => [['type' => 'plugin', 'slug' => $fixture['slug'], 'version' => 'latest']],
        ]);

        $this->assertCount(1, $snapshots->restored);
        $this->assertStringContainsString('no longer matches its pre-update state', $out['results'][0]['log']);
        $this->assertStringContainsString('auto-restored', $out['results'][0]['log']);
    }

    /**
     * Classification false, verification NULL. THE VERIFICATION-FAILS CASE:
     * an unanswerable host keeps exactly the previous release's behaviour.
     */
    public function test_an_unverifiable_directory_still_restores(): void
    {
        $fixture   = $this->makePluginWithPayload('1.2.3');
        $snapshots = $this->spySnapshots('snap_unver', $fixture['payload']);

        // An empty from_version makes the positive header match impossible, so
        // the verifier can only answer "cannot tell".
        $runner = $this->runnerReturning($this->untouchedFailure(), '');

        $out = (new UpdateCommand($snapshots, $runner))->execute([], [
            'items' => [['type' => 'plugin', 'slug' => $fixture['slug'], 'version' => 'latest']],
        ]);

        $this->assertCount(1, $snapshots->restored, 'an unverifiable host must keep restoring');
        $this->assertStringContainsString('could not verify the plugin directory', $out['results'][0]['log']);
    }

    /**
     * THE STRICT-NARROWING PROOF. An apply outcome with NO destination_touched
     * key at all (the pre-0.61.114 shape, and the shape any minimal runner
     * double produces) must restore, and its log must be byte-identical to the
     * previous release's.
     */
    public function test_an_unclassified_failure_restores_exactly_as_before(): void
    {
        $fixture   = $this->makePluginWithPayload('1.2.3');
        $snapshots = $this->spySnapshots('snap_legacy', $fixture['payload']);

        $runner = $this->runnerReturning(['ok' => false, 'log' => 'upgrader reported failure']);

        $out = (new UpdateCommand($snapshots, $runner))->execute([], [
            'items' => [['type' => 'plugin', 'slug' => $fixture['slug'], 'version' => 'latest']],
        ]);

        $this->assertCount(1, $snapshots->restored);
        $this->assertSame(
            "captured\nupgrader reported failure\nUpdate incomplete; auto-restored the pre-update snapshot.",
            $out['results'][0]['log'],
            'an unclassified failure must produce the exact log the previous release produced'
        );
    }

    /** An apply that SUCCEEDED but failed verification never opens the gate. */
    public function test_an_ok_apply_rejected_by_the_completeness_check_never_skips(): void
    {
        $fixture   = $this->makePluginWithPayload('1.2.3');
        $snapshots = $this->spySnapshots('snap_incomplete', $fixture['payload']);

        $runner = $this->runnerReturning([
            'ok'                  => true,
            'log'                 => 'applied',
            'destination_touched' => false, // Deliberately contradictory input.
            'failure_code'        => '',
            'failure_stage'       => '',
        ]);

        $out = (new UpdateCommand($snapshots, $runner))->execute([], [
            'items' => [['type' => 'plugin', 'slug' => $fixture['slug'], 'version' => 'latest']],
        ]);

        $this->assertCount(
            1,
            $snapshots->restored,
            'the gate is gated on ok === false; a half-written directory reported as ok must still restore'
        );
        $this->assertStringContainsString('auto-restored', $out['results'][0]['log']);
        $this->assertStringNotContainsString(
            'could not verify the',
            $out['results'][0]['log'],
            'a gate that was never evaluated must add no sentence at all'
        );
    }

    /**
     * `core` has no directory-level snapshot or restore by design, so the
     * decision must never even evaluate for it.
     */
    public function test_a_core_failure_never_evaluates_the_skip(): void
    {
        $snapshots = $this->spySnapshots('snap_core', '');

        $runner = $this->runnerReturning($this->untouchedFailure());

        $out = (new UpdateCommand($snapshots, $runner))->execute([], [
            'snapshot' => false,
            'items'    => [['type' => 'core', 'slug' => 'core', 'version' => 'latest']],
        ]);

        $this->assertSame([], $snapshots->restored);
        $this->assertStringNotContainsString('was re-checked after the failure', $out['results'][0]['log']);
        $this->assertStringNotContainsString('could not verify the', $out['results'][0]['log']);
        $this->assertStringNotContainsString('no longer matches its pre-update state', $out['results'][0]['log']);
        $this->assertSame('failed', $out['results'][0]['status']);
    }

    // =====================================================================
    // Reactivation
    // =====================================================================

    /**
     * Core silently deactivates an active plugin at `upgrader_pre_install` and,
     * on a non-cron request, never puts it back. A skipped restore would
     * otherwise leave the directory byte-identical and the plugin OFF.
     */
    public function test_an_active_plugin_deactivated_by_core_is_reactivated_after_a_skip(): void
    {
        Functions\when('is_plugin_active')->justReturn(false);

        $fixture   = $this->makePluginWithPayload('1.2.3');
        $snapshots = $this->spySnapshots('snap_react', $fixture['payload']);

        $outcome                         = $this->untouchedFailure();
        $outcome['failure_code']         = 'source_read_failed';
        $outcome['failure_stage']        = 'install';
        $outcome['may_have_deactivated'] = true;
        $outcome['was_active']           = true;

        $runner = $this->runnerReturning($outcome);

        $out = (new UpdateCommand($snapshots, $runner))->execute([], [
            'items' => [['type' => 'plugin', 'slug' => $fixture['slug'], 'version' => 'latest']],
        ]);

        $this->assertCount(1, $runner->reactivations);
        $this->assertSame([$fixture['slug'], true, false], $runner->reactivations[0]);
        $this->assertStringContainsString('it has been reactivated', $out['results'][0]['log']);
    }

    /**
     * A plugin that was NOT active before the update must never be switched on
     * by a failure path: activate_plugin() includes the plugin file in this
     * process, and the operator may have disabled it deliberately.
     */
    public function test_an_inactive_plugin_is_never_activated_by_the_failure_path(): void
    {
        Functions\when('is_plugin_active')->justReturn(false);

        $fixture   = $this->makePluginWithPayload('1.2.3');
        $snapshots = $this->spySnapshots('snap_noreact', $fixture['payload']);

        $outcome                         = $this->untouchedFailure();
        $outcome['may_have_deactivated'] = true;
        $outcome['was_active']           = false;
        $outcome['was_network_active']   = false;

        $runner = $this->runnerReturning($outcome);

        (new UpdateCommand($snapshots, $runner))->execute([], [
            'items' => [['type' => 'plugin', 'slug' => $fixture['slug'], 'version' => 'latest']],
        ]);

        $this->assertSame([], $runner->reactivations);
    }
}
