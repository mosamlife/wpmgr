<?php
/**
 * UpdateInFlightTest — S4 (GitHub issue #131 adversarial review): the
 * out-of-band reconcile for a plugin/theme apply hard-killed by PHP-FPM's
 * `request_terminate_timeout` before UpdateGuard's own shutdown-function
 * backstop could ever run. ALSO covers the final-hardening-review fixes
 * layered on top of it:
 *   - B: the flock() liveness lock mark() acquires/returns is the PRIMARY
 *     signal healStaleIfPresent() uses to decide whether a marker's owning
 *     apply is still genuinely running (skip) vs. safely reconcilable.
 *   - F2: healStaleIfPresent() only ever restores when the live directory is
 *     verified genuinely incomplete AND its referenced snapshot still
 *     exists — never blindly on every stale marker.
 *   - F5: the JSON marker filename is no longer derived from type/slug (see
 *     test_repeated_mark_for_the_same_slug_leaves_exactly_one_marker for the
 *     retry-overwrite semantics this preserves despite that).
 *
 * Covers the WPMgr\Agent\Support\UpdateInFlight class in isolation (the
 * UpdateCommand integration — mark-before-apply, clear-in-finally, lock
 * release, and the start-of-execute() reconcile call — is covered in
 * UpdateCommandTest.php).
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Support\SnapshotManager;
use WPMgr\Agent\Support\UpdateInFlight;
use WPMgr\Agent\Support\UpdateRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\UpdateInFlight
 */
final class UpdateInFlightTest extends TestCase
{
    /** Temp root for this test run (removed in tear_down). */
    private string $root = '';

    /** Simulated wp-content/uploads dir. */
    private string $uploadsDir = '';

    /** Marker store under $uploadsDir. */
    private string $storeDir = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root       = sys_get_temp_dir() . '/wpmgr-inflight-' . bin2hex(random_bytes(6));
        $this->uploadsDir = $this->root . '/uploads';
        $this->storeDir   = $this->uploadsDir . '/wpmgr-update-inflight';
        mkdir($this->uploadsDir, 0755, true);

        Functions\when('wp_upload_dir')->justReturn(['basedir' => $this->uploadsDir]);
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->root);
        Monkey\tearDown();
        parent::tear_down();
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

    /**
     * A SnapshotManager spy that records restore() calls without touching
     * disk. snapshotExists() always reports true — these tests exercise the
     * restore DECISION itself (via the injected isComplete() verdict from
     * healthOverrideRunner()); the snapshot-existence gate (F2) has its own
     * dedicated test below with a purpose-built double.
     */
    private function spySnapshots(): SnapshotManager
    {
        return new class extends SnapshotManager {
            /** @var array<int,array{string,string,string}> */
            public array $restored = [];

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

    /**
     * F2 (issue #131 final-hardening review) — an UpdateRunner double whose
     * isComplete() reports a fixed, controllable health verdict, so tests can
     * exercise both sides of healStaleIfPresent()'s verify-before-restore
     * gate without depending on real get_plugins()/validate_plugin() stub
     * state (which other test files in this suite may have already frozen
     * process-wide).
     */
    private function healthOverrideRunner(bool $complete): UpdateRunner
    {
        return new class ($complete) extends UpdateRunner {
            public function __construct(private bool $complete)
            {
            }

            public function isComplete(string $type, string $slug, string $expectedVersion = ''): bool
            {
                return $this->complete;
            }
        };
    }

    /** Rewrite the sole marker file's `ts` field to simulate age. */
    private function ageTheOnlyMarker(int $ts): void
    {
        $files = glob($this->storeDir . '/*.json') ?: [];
        $this->assertCount(1, $files, 'precondition: exactly one marker file must exist');
        $data = json_decode((string) file_get_contents($files[0]), true);
        $data['ts'] = $ts;
        file_put_contents($files[0], (string) json_encode($data));
    }

    // =========================================================================
    // mark() / clear()
    // =========================================================================

    public function test_mark_then_clear_round_trip(): void
    {
        UpdateInFlight::mark('plugin', 'foo/foo.php', 'snap_abc');
        $this->assertCount(1, glob($this->storeDir . '/*.json') ?: []);

        UpdateInFlight::clear('plugin', 'foo/foo.php');
        $this->assertCount(0, glob($this->storeDir . '/*.json') ?: []);
    }

    public function test_clear_is_a_safe_noop_when_nothing_was_ever_marked(): void
    {
        UpdateInFlight::clear('plugin', 'never-marked/never-marked.php');
        $this->assertCount(0, glob($this->storeDir . '/*.json') ?: []);
    }

    public function test_mark_records_type_slug_and_snapshot_id(): void
    {
        UpdateInFlight::mark('theme', 'twentytwentyfour', 'snap_xyz');

        $files = glob($this->storeDir . '/*.json') ?: [];
        $this->assertCount(1, $files);
        $data = json_decode((string) file_get_contents($files[0]), true);

        $this->assertSame('theme', $data['type']);
        $this->assertSame('twentytwentyfour', $data['slug']);
        $this->assertSame('snap_xyz', $data['snapshot_id']);
        $this->assertIsInt($data['ts']);
    }

    public function test_different_type_slug_pairs_do_not_collide(): void
    {
        UpdateInFlight::mark('plugin', 'foo/foo.php', 'snap_a');
        UpdateInFlight::mark('theme', 'foo', 'snap_b');
        UpdateInFlight::mark('plugin', 'bar/bar.php', 'snap_c');

        $this->assertCount(3, glob($this->storeDir . '/*.json') ?: []);

        UpdateInFlight::clear('plugin', 'foo/foo.php');
        $this->assertCount(2, glob($this->storeDir . '/*.json') ?: [], 'clearing one key must not remove the others');
    }

    public function test_mark_returns_an_acquired_lock_handle(): void
    {
        $lock = UpdateInFlight::mark('plugin', 'foo/foo.php', 'snap_a');

        $this->assertNotNull($lock, 'B: mark() must acquire and return the paired liveness lock');
        $this->assertTrue(is_resource($lock));

        UpdateInFlight::release($lock);
    }

    public function test_repeated_mark_for_the_same_slug_leaves_exactly_one_marker(): void
    {
        // F5 (issue #131 final-hardening review) — the marker filename is no
        // longer deterministically derived from type/slug, so a repeat
        // mark() call for the SAME type/slug can no longer simply overwrite
        // a fixed path in place; mark() must instead explicitly purge any
        // pre-existing marker(s) for this exact type/slug so retries still
        // leave exactly one marker behind, not an ever-growing pile.
        $lock1  = UpdateInFlight::mark('plugin', 'foo/foo.php', 'snap_first');
        $files1 = glob($this->storeDir . '/*.json') ?: [];
        $this->assertCount(1, $files1);

        $lock2 = UpdateInFlight::mark('plugin', 'foo/foo.php', 'snap_second');
        $files2 = glob($this->storeDir . '/*.json') ?: [];

        $this->assertCount(
            1,
            $files2,
            'F5: a repeat mark() for the same type/slug must leave exactly one marker on disk, not accumulate'
        );
        $data = json_decode((string) file_get_contents($files2[0]), true);
        $this->assertSame(
            'snap_second',
            $data['snapshot_id'],
            'the surviving marker must be the LATEST mark() call, not the first'
        );

        UpdateInFlight::release($lock1);
        UpdateInFlight::release($lock2);
    }

    public function test_marker_filename_is_not_the_old_deterministic_slug_hash(): void
    {
        // F5 — pin that the filename is no longer sha256(type:slug)-derived
        // (which had no secret salt and was trivially recomputable by
        // anyone who already knew the public slug).
        $lock = UpdateInFlight::mark('plugin', 'akismet/akismet.php', 'snap_a');

        $files = glob($this->storeDir . '/*.json') ?: [];
        $this->assertCount(1, $files);
        $oldDeterministicName = substr(hash('sha256', 'plugin:akismet/akismet.php'), 0, 32) . '.json';

        $this->assertNotSame(
            $oldDeterministicName,
            basename($files[0]),
            'F5: the marker filename must not be the old deterministic sha256(type:slug) hash'
        );

        UpdateInFlight::release($lock);
    }

    // =========================================================================
    // healStaleIfPresent()
    // =========================================================================

    public function test_fresh_marker_is_left_untouched(): void
    {
        UpdateInFlight::mark('plugin', 'foo/foo.php', 'snap_fresh');

        $snapshots = $this->spySnapshots();
        UpdateInFlight::healStaleIfPresent($snapshots);

        $this->assertSame(
            [],
            $snapshots->restored,
            'a fresh in-flight marker may belong to a still-running apply and must not be reconciled'
        );
        $this->assertCount(1, glob($this->storeDir . '/*.json') ?: []);
    }

    public function test_stale_marker_triggers_restore_with_the_recorded_arguments_and_is_cleared(): void
    {
        $lock = UpdateInFlight::mark('plugin', 'foo/foo.php', 'snap_stale');
        UpdateInFlight::release($lock); // simulate the owning process having exited (B: lock must be free to reconcile)
        $this->ageTheOnlyMarker(time() - 1300); // > STALE_AFTER_SECONDS (1200)

        $snapshots = $this->spySnapshots();
        // F2: the live directory must be verified genuinely INCOMPLETE for a
        // restore to happen at all — this test's scenario is exactly that.
        UpdateInFlight::healStaleIfPresent($snapshots, $this->healthOverrideRunner(false));

        $this->assertSame(
            [['plugin', 'foo/foo.php', 'snap_stale']],
            $snapshots->restored,
            'a stale marker over a genuinely incomplete live directory must trigger restore() with exactly its recorded type/slug/snapshot_id'
        );
        $this->assertCount(
            0,
            glob($this->storeDir . '/*.json') ?: [],
            'a reconciled marker must be removed so it is never reconciled twice'
        );
    }

    public function test_marker_exactly_at_the_staleness_boundary_is_left_alone(): void
    {
        // Boundary: strictly LESS than the threshold counts as fresh — this
        // pins the "< STALE_AFTER_SECONDS" comparison direction.
        UpdateInFlight::mark('plugin', 'foo/foo.php', 'snap_boundary');
        $this->ageTheOnlyMarker(time() - 1199);

        $snapshots = $this->spySnapshots();
        UpdateInFlight::healStaleIfPresent($snapshots);

        $this->assertSame([], $snapshots->restored);
        $this->assertCount(1, glob($this->storeDir . '/*.json') ?: []);
    }

    public function test_corrupt_marker_is_removed_without_attempting_a_restore(): void
    {
        mkdir($this->storeDir, 0755, true);
        file_put_contents($this->storeDir . '/deadbeef00000000000000000000000.json', 'not json');

        $snapshots = $this->spySnapshots();
        UpdateInFlight::healStaleIfPresent($snapshots);

        $this->assertSame([], $snapshots->restored);
        $this->assertCount(
            0,
            glob($this->storeDir . '/*.json') ?: [],
            'an unreadable/corrupt marker cannot be reconciled to anything meaningful and must be removed'
        );
    }

    public function test_restore_throwing_during_reconcile_still_removes_the_marker(): void
    {
        $lock = UpdateInFlight::mark('plugin', 'foo/foo.php', 'snap_stale');
        UpdateInFlight::release($lock);
        $this->ageTheOnlyMarker(time() - 1300);

        $snapshots = new class extends SnapshotManager {
            public function restore(string $type, string $slug, string $snapshotId): array
            {
                throw new \RuntimeException('simulated restore failure');
            }

            public function snapshotExists(string $snapshotId): bool
            {
                return true;
            }
        };

        // Must not throw out of healStaleIfPresent() — a broken restore
        // must not wedge the reconcile sweep for every OTHER marker.
        UpdateInFlight::healStaleIfPresent($snapshots, $this->healthOverrideRunner(false));

        $this->assertCount(
            0,
            glob($this->storeDir . '/*.json') ?: [],
            'the marker must still be cleared even when the restore attempt itself throws'
        );
    }

    public function test_multiple_stale_markers_are_each_reconciled_independently(): void
    {
        $lock1 = UpdateInFlight::mark('plugin', 'foo/foo.php', 'snap_foo');
        $lock2 = UpdateInFlight::mark('theme', 'bar', 'snap_bar');
        UpdateInFlight::release($lock1);
        UpdateInFlight::release($lock2);
        foreach (glob($this->storeDir . '/*.json') ?: [] as $file) {
            $data = json_decode((string) file_get_contents($file), true);
            $data['ts'] = time() - 1300;
            file_put_contents($file, (string) json_encode($data));
        }

        $snapshots = $this->spySnapshots();
        UpdateInFlight::healStaleIfPresent($snapshots, $this->healthOverrideRunner(false));

        $this->assertCount(2, $snapshots->restored);
        $this->assertCount(0, glob($this->storeDir . '/*.json') ?: []);
    }

    public function test_heal_is_a_safe_noop_when_the_store_directory_does_not_exist(): void
    {
        // Never called mark() — the store directory itself was never created.
        $snapshots = $this->spySnapshots();
        UpdateInFlight::healStaleIfPresent($snapshots);

        $this->assertSame([], $snapshots->restored);
    }

    // =========================================================================
    // F2 — verify before restoring
    // =========================================================================

    public function test_heal_clears_without_restoring_when_the_live_directory_is_already_healthy(): void
    {
        $lock = UpdateInFlight::mark('plugin', 'foo/foo.php', 'snap_healthy');
        UpdateInFlight::release($lock);
        $this->ageTheOnlyMarker(time() - 1300);

        $snapshots = $this->spySnapshots();
        // The apply actually finished cleanly before the kill landed — the
        // live directory is reported COMPLETE.
        UpdateInFlight::healStaleIfPresent($snapshots, $this->healthOverrideRunner(true));

        $this->assertSame(
            [],
            $snapshots->restored,
            'F2: a stale marker over an already-healthy/complete live directory must NEVER trigger a restore'
        );
        $this->assertCount(
            0,
            glob($this->storeDir . '/*.json') ?: [],
            'F2: the marker must still be cleared even though no restore happened'
        );
    }

    public function test_heal_clears_without_restoring_when_the_referenced_snapshot_no_longer_exists(): void
    {
        $lock = UpdateInFlight::mark('plugin', 'foo/foo.php', 'snap_vanished');
        UpdateInFlight::release($lock);
        $this->ageTheOnlyMarker(time() - 1300);

        // Live directory verified genuinely INCOMPLETE (would normally
        // restore) — but the referenced snapshot itself is gone (already
        // pruned/consumed).
        $snapshots = new class extends SnapshotManager {
            /** @var array<int,array{string,string,string}> */
            public array $restored = [];

            public function restore(string $type, string $slug, string $snapshotId): array
            {
                $this->restored[] = [$type, $slug, $snapshotId];

                return ['ok' => true, 'log' => 'restored'];
            }

            public function snapshotExists(string $snapshotId): bool
            {
                return false;
            }
        };

        UpdateInFlight::healStaleIfPresent($snapshots, $this->healthOverrideRunner(false));

        $this->assertSame(
            [],
            $snapshots->restored,
            'F2: restore() must never be attempted once the referenced snapshot is confirmed gone'
        );
        $this->assertCount(
            0,
            glob($this->storeDir . '/*.json') ?: [],
            'F2: the marker must still be cleared when its snapshot has vanished, not left behind forever'
        );
    }

    // =========================================================================
    // B — flock liveness lock gates reconcile
    // =========================================================================

    public function test_heal_skips_a_marker_whose_liveness_lock_is_still_held_and_reconciles_once_released(): void
    {
        $lock = UpdateInFlight::mark('plugin', 'locked/locked.php', 'snap_locked');
        $this->assertNotNull($lock, 'precondition: mark() must acquire the liveness lock');
        $this->ageTheOnlyMarker(time() - 1300);

        $snapshots = $this->spySnapshots();
        $runner    = $this->healthOverrideRunner(false); // not healthy -> would restore once reachable

        // B: the lock is still held right now (simulating a still-running
        // apply, immune to the age-only heuristic) -> the marker must be
        // skipped entirely — no restore, no deletion.
        UpdateInFlight::healStaleIfPresent($snapshots, $runner);
        $this->assertSame(
            [],
            $snapshots->restored,
            'B: a marker whose liveness lock is still held must be skipped — its apply is genuinely still running'
        );
        $this->assertCount(
            1,
            glob($this->storeDir . '/*.json') ?: [],
            'a still-locked marker must not be removed either'
        );

        UpdateInFlight::release($lock);

        // Now that the lock is free, reconcile proceeds normally.
        UpdateInFlight::healStaleIfPresent($snapshots, $runner);
        $this->assertSame(
            [['plugin', 'locked/locked.php', 'snap_locked']],
            $snapshots->restored,
            'B: once the lock is released, the SAME stale marker must be reconciled normally'
        );
        $this->assertCount(0, glob($this->storeDir . '/*.json') ?: []);
    }
}
