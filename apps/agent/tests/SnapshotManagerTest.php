<?php
/**
 * SnapshotManagerTest — adversarial-review fixes for GitHub issue #131 (agent
 * update atomicity): M1 (bounded snapshot GC) and S5 (restore()'s dead
 * rollback-on-failure code).
 *
 * All filesystem operations use an isolated temp directory per test, and
 * SnapshotManager's `liveDir()` (protected) is overridden in an anonymous
 * subclass wherever a test needs to control the "live" plugin/theme source
 * path deterministically — this keeps every test in this file independent of
 * ambient WP_CONTENT_DIR / WP_PLUGIN_DIR state that OTHER test files in this
 * suite may have already frozen as global PHP constants (constants cannot be
 * redefined once set, and several other test files legitimately do define
 * them for their own purposes).
 *
 * Covers:
 *   - M1: capture() prunes a slug's older snapshots beyond the count cap,
 *     the most-recent-two survive across a capture cycle (the CP
 *     post-update health-probe window guarantee), a TTL-expired snapshot is
 *     pruned even while still within the count cap, and pruning one slug
 *     never touches another slug's snapshots.
 *   - M1: gcExpired() (the recurring cron backstop) sweeps any snapshot
 *     directory older than the backstop TTL, including one with no readable
 *     meta.json at all (mtime fallback), while leaving recent ones alone.
 *   - S5: restore() rolls back to the pre-restore-attempt state (not the
 *     misleading "original retained" no-op) when the copy-back fails
 *     mid-write, correctly handles the case where there was nothing to roll
 *     back to, and the happy path still works unchanged.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Support\SnapshotManager;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\SnapshotManager
 */
final class SnapshotManagerTest extends TestCase
{
    /** Temp root for this test run (removed in tear_down). */
    private string $root = '';

    /** Simulated wp-content/uploads dir. */
    private string $uploadsDir = '';

    /** wpmgr-snapshots store under $uploadsDir. */
    private string $snapshotsBase = '';

    /**
     * In-memory wp-option store backing get_option()/update_option() —
     * needed for maybeGc()'s throttle timestamp (GitHub issue #226).
     *
     * @var array<string,mixed>
     */
    private array $options = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root          = sys_get_temp_dir() . '/wpmgr-snaptest-' . bin2hex(random_bytes(6));
        $this->uploadsDir    = $this->root . '/uploads';
        $this->snapshotsBase = $this->uploadsDir . '/wpmgr-snapshots';
        mkdir($this->uploadsDir, 0755, true);

        Functions\when('wp_upload_dir')->justReturn(['basedir' => $this->uploadsDir]);

        $this->options = [];
        Functions\when('get_option')->alias(function ($name, $default = false) {
            return $this->options[$name] ?? $default;
        });
        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;
            return true;
        });
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->root);
        Monkey\tearDown();
        parent::tear_down();
    }

    // =========================================================================
    // Test doubles / helpers
    // =========================================================================

    /**
     * A SnapshotManager whose liveDir() is fully test-controlled (never
     * touches WP_CONTENT_DIR/WP_PLUGIN_DIR), and whose copyDir() can be
     * forced to fail AFTER mimicking the real method's mkdir-destination-first
     * side effect — the exact precondition S5's bug required.
     */
    private function manager(): object
    {
        return new class extends SnapshotManager {
            public string $liveOverride = '';
            public bool $failCopy       = false;

            /**
             * Optional hook invoked at the very start of copyDir() — lets a
             * test observe on-disk state at the exact moment restore()'s
             * copy-back step begins (the narrow "aside is staged, copy is
             * about to run" window Fix 1 (issue #131 final-hardening review,
             * asides not GC-safe) targets).
             *
             * @var (callable(): void)|null
             */
            public $onCopyDir = null;

            protected function liveDir(string $type, string $slug): string
            {
                return $this->liveOverride;
            }

            protected function copyDir(string $src, string $dst): bool
            {
                if ($this->onCopyDir !== null) {
                    ($this->onCopyDir)();
                }

                if ($this->failCopy) {
                    // Mirror copyDir()'s real first step (mkdir the
                    // destination) even though the copy is about to fail —
                    // this is the exact precondition that defeated the
                    // original `is_dir($live)` rollback guard (S5).
                    if (!is_dir($dst)) {
                        mkdir($dst, 0755, true);
                    }
                    return false;
                }

                return parent::copyDir($src, $dst);
            }
        };
    }

    /**
     * A SnapshotManager whose liveDir() always returns '' (no live source) —
     * isolates prune-at-capture behavior from any real directory copy.
     */
    private function managerWithNoLiveSource(): SnapshotManager
    {
        return new class extends SnapshotManager {
            protected function liveDir(string $type, string $slug): string
            {
                return '';
            }
        };
    }

    /**
     * Seed a snapshot directory directly on disk (bypassing capture()) with a
     * controlled created_at, for prune/GC tests that need precise ages.
     *
     * @param bool $succeeded GitHub issue #226 — when true, stamps the same
     *                         `succeeded_at` marker markSucceeded() writes, so
     *                         GC tests can seed a two-tier scenario directly
     *                         without a real capture()/markSucceeded() round trip.
     */
    private function seedSnapshot(string $type, string $slug, int $createdAt, bool $succeeded = false): string
    {
        $id  = 'snap_' . bin2hex(random_bytes(12));
        $dir = $this->snapshotsBase . '/' . $id;
        mkdir($dir, 0755, true);
        $meta = [
            'type'         => $type,
            'slug'         => $slug,
            'from_version' => '1.0',
            'created_at'   => $createdAt,
        ];
        if ($succeeded) {
            $meta['succeeded_at'] = $createdAt;
        }
        file_put_contents($dir . '/meta.json', (string) json_encode($meta));

        return $id;
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
                @unlink($path); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
            }
        }
        @rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- test-only fixture cleanup
    }

    // =========================================================================
    // M1 — prune-at-capture
    // =========================================================================

    public function test_repeated_capture_bounds_snapshot_count_and_keeps_the_most_recent_for_cp_rollback(): void
    {
        $source = $this->root . '/plugins-src/demo';
        mkdir($source, 0755, true);
        file_put_contents($source . '/demo.php', "<?php\n// v1\n");

        $mgr               = $this->manager();
        $mgr->liveOverride = $source;

        // Two EXISTING entries for this slug from well-spaced-out prior
        // update cycles — comfortably past MIN_KEEP_AGE_SECONDS (10 min) so
        // the COUNT-based retention cap is what's under test here, not the
        // min-age floor (a real back-to-back capture sequence within 10
        // minutes is exactly the race the floor now protects against — see
        // test_prune_never_removes_a_snapshot_younger_than_the_min_keep_age_floor_regardless_of_rank
        // below for that case).
        $r1 = $this->seedSnapshot('plugin', 'demo/demo.php', time() - 2400); // 40 min ago
        $r2 = $this->seedSnapshot('plugin', 'demo/demo.php', time() - 1800); // 30 min ago

        $r3 = $mgr->capture('plugin', 'demo/demo.php', '1.2');
        $this->assertNotSame('', $r3['snapshot_id']);

        // The THIRD capture's own prune pass must have removed the OLDEST
        // (r1) — but never the two most recent (the CP's post-update
        // health-probe window guarantee M1 exists for).
        $this->assertFalse(
            is_dir($this->snapshotsBase . '/' . $r1),
            'M1: the oldest snapshot for this slug must be pruned once the per-slug count cap is exceeded'
        );
        $this->assertTrue(is_dir($this->snapshotsBase . '/' . $r2));
        $this->assertTrue(is_dir($this->snapshotsBase . '/' . $r3['snapshot_id']));
    }

    public function test_prune_never_removes_a_snapshot_younger_than_the_min_keep_age_floor_regardless_of_rank(): void
    {
        $mgr = $this->managerWithNoLiveSource();
        $mgr->capture('plugin', 'warmup/warmup.php', '1.0');

        // Two EXISTING entries for the SAME slug, both well under the
        // 10-minute floor — simulating a concurrent-update race (a second
        // in-flight `update` request for the SAME slug capturing its own
        // snapshot moments after the first; the CP's cross-run dedup for
        // this is being fixed separately, but the agent must not depend on
        // it). The count cap alone (keep newest 1 existing) would normally
        // prune the older of the two right here; the floor must override
        // that and keep BOTH regardless of rank.
        $olderId = $this->seedSnapshot('plugin', 'racey/racey.php', time() - 5);
        $newerId = $this->seedSnapshot('plugin', 'racey/racey.php', time() - 2);

        $mgr->capture('plugin', 'racey/racey.php', '1.0');

        $this->assertTrue(
            is_dir($this->snapshotsBase . '/' . $olderId),
            'belt: a snapshot younger than MIN_KEEP_AGE_SECONDS must never be pruned, even when it is past the per-slug count cap'
        );
        $this->assertTrue(
            is_dir($this->snapshotsBase . '/' . $newerId),
            'belt: a snapshot younger than MIN_KEEP_AGE_SECONDS must never be pruned'
        );
    }

    public function test_capture_prunes_a_ttl_expired_slug_snapshot_even_within_the_count_cap(): void
    {
        $mgr = $this->managerWithNoLiveSource();

        // Force-create the snapshot base directory.
        $mgr->capture('plugin', 'warmup/warmup.php', '1.0');

        // Only ONE existing entry for this slug — well within the count cap
        // (MAX_SNAPSHOTS_PER_SLUG - 1 = 1) — but 25h old, past the 24h
        // capture-time TTL.
        $staleId = $this->seedSnapshot('plugin', 'stale/stale.php', time() - (25 * 3600));

        $mgr->capture('plugin', 'stale/stale.php', '1.0');

        $this->assertFalse(
            is_dir($this->snapshotsBase . '/' . $staleId),
            'M1: a slug snapshot past the 24h capture-time TTL must be pruned even though it is within the count cap'
        );
    }

    public function test_capture_keeps_a_recent_slug_snapshot_within_both_bounds(): void
    {
        $mgr = $this->managerWithNoLiveSource();
        $mgr->capture('plugin', 'warmup/warmup.php', '1.0');

        $recentId = $this->seedSnapshot('plugin', 'recent/recent.php', time() - 60);

        $mgr->capture('plugin', 'recent/recent.php', '1.0');

        $this->assertTrue(
            is_dir($this->snapshotsBase . '/' . $recentId),
            'a snapshot within both the count cap and the TTL must survive pruning'
        );
    }

    public function test_prune_never_touches_a_different_slugs_snapshots(): void
    {
        $mgr = $this->managerWithNoLiveSource();
        $mgr->capture('plugin', 'warmup/warmup.php', '1.0');

        // Ages kept comfortably past MIN_KEEP_AGE_SECONDS (10 min) so the
        // per-slug isolation under test here isn't masked by the min-age
        // floor (which would otherwise keep everything regardless of slug).
        $idA1 = $this->seedSnapshot('plugin', 'alpha/alpha.php', time() - 900);
        $idA2 = $this->seedSnapshot('plugin', 'alpha/alpha.php', time() - 800);
        $idB1 = $this->seedSnapshot('plugin', 'beta/beta.php', time() - 700);

        // Pruning triggered for 'alpha' only.
        $mgr->capture('plugin', 'alpha/alpha.php', '1.0');

        $this->assertFalse(is_dir($this->snapshotsBase . '/' . $idA1), 'alpha oldest must be pruned');
        $this->assertTrue(is_dir($this->snapshotsBase . '/' . $idA2), 'alpha newest must survive');
        $this->assertTrue(
            is_dir($this->snapshotsBase . '/' . $idB1),
            'M1: pruning one slug must never remove a DIFFERENT slug\'s snapshots'
        );
    }

    public function test_prune_never_touches_snapshots_of_a_different_type_with_the_same_slug_name(): void
    {
        // A plugin folder and a theme stylesheet could coincidentally share
        // the same slug string; type must be part of the match, not just slug.
        // Two PLUGIN entries (so the plugin bucket genuinely exceeds the
        // count cap and a real prune happens) plus one THEME entry sharing
        // the same slug string proves type-isolation: if type were NOT part
        // of the match, the combined "shared" bucket would already be at 3
        // entries against a cap of 1, and the theme entry could be swept too.
        $mgr = $this->managerWithNoLiveSource();
        $mgr->capture('plugin', 'warmup/warmup.php', '1.0');

        // Ages kept comfortably past MIN_KEEP_AGE_SECONDS (10 min) — see the
        // comment on the alpha/beta test above for why.
        $pluginOld = $this->seedSnapshot('plugin', 'shared', time() - 900);
        $pluginNew = $this->seedSnapshot('plugin', 'shared', time() - 800);
        $themeId   = $this->seedSnapshot('theme', 'shared', time() - 700);

        $mgr->capture('plugin', 'shared', '1.0');

        $this->assertFalse(is_dir($this->snapshotsBase . '/' . $pluginOld), 'the older plugin:shared snapshot must be pruned');
        $this->assertTrue(is_dir($this->snapshotsBase . '/' . $pluginNew), 'the newer plugin:shared snapshot must survive');
        $this->assertTrue(
            is_dir($this->snapshotsBase . '/' . $themeId),
            'M1: a theme:shared snapshot must never be pruned by a plugin:shared capture — type must be part of the match, not just slug'
        );
    }

    // =========================================================================
    // M1 — GC cron backstop (gcExpired())
    // =========================================================================

    public function test_gc_expired_sweeps_snapshots_older_than_the_backstop_ttl(): void
    {
        $mgr = $this->managerWithNoLiveSource();
        $mgr->capture('plugin', 'warmup/warmup.php', '1.0');

        $oldId    = $this->seedSnapshot('plugin', 'orphan/orphan.php', time() - (73 * 3600)); // > 72h
        $recentId = $this->seedSnapshot('plugin', 'recent/recent.php', time() - 3600); // 1h

        SnapshotManager::gcExpired();

        $this->assertFalse(
            is_dir($this->snapshotsBase . '/' . $oldId),
            'M1: the GC backstop must sweep any snapshot older than 72h, independent of the per-slug prune'
        );
        $this->assertTrue(
            is_dir($this->snapshotsBase . '/' . $recentId),
            'a recent snapshot must survive the GC backstop sweep'
        );
    }

    public function test_gc_expired_uses_mtime_fallback_for_a_snapshot_with_no_readable_meta(): void
    {
        $mgr = $this->managerWithNoLiveSource();
        $mgr->capture('plugin', 'warmup/warmup.php', '1.0');

        // Simulates a crashed capture that never got as far as writeMeta().
        $orphan = $this->snapshotsBase . '/snap_' . bin2hex(random_bytes(12));
        mkdir($orphan, 0755, true);
        touch($orphan, time() - (73 * 3600));

        SnapshotManager::gcExpired();

        $this->assertFalse(
            is_dir($orphan),
            'a crashed-capture directory with no meta.json must still be swept via the mtime fallback'
        );
    }

    public function test_gc_expired_never_touches_the_cp_health_probe_window(): void
    {
        $mgr               = $this->manager();
        $source            = $this->root . '/plugins-src/live';
        mkdir($source, 0755, true);
        file_put_contents($source . '/live.php', "<?php\n");
        $mgr->liveOverride = $source;

        $r = $mgr->capture('plugin', 'live/live.php', '1.0');
        $this->assertNotSame('', $r['snapshot_id']);

        // Immediately run the GC backstop, simulating an unlucky cron tick
        // landing right after a fresh capture.
        SnapshotManager::gcExpired();

        $this->assertTrue(
            is_dir($this->snapshotsBase . '/' . $r['snapshot_id']),
            'M1: a freshly captured snapshot must never be swept by the GC backstop — it is nowhere near the 72h TTL'
        );
    }

    // =========================================================================
    // GitHub issue #226 — maybeGc() WP-Cron-independent throttled trigger
    // =========================================================================

    /**
     * A SnapshotManager subclass whose gcExpired() is fully test-controlled
     * (a static call counter, optionally throwing) — exercised via maybeGc()'s
     * `static::gcExpired()` late-static-binding call, exactly the same seam
     * gcExpired() itself already documents for strandedAsideRoots() overrides.
     *
     * @param bool $throws Whether gcExpired() should throw after recording the call.
     * @return class-string<SnapshotManager>
     */
    private function managerClassCountingGcExpiredCalls(bool $throws = false): string
    {
        $class = new class extends SnapshotManager {
            public static int $calls  = 0;
            public static bool $throws = false;

            public static function gcExpired(): void
            {
                self::$calls++;
                if (self::$throws) {
                    throw new \RuntimeException('simulated GC sweep failure');
                }
            }
        };
        $class::$calls  = 0;
        $class::$throws = $throws;

        return get_class($class);
    }

    public function test_maybe_gc_runs_on_first_call_skips_within_the_throttle_window_and_runs_again_after_it_elapses(): void
    {
        $now = 1_700_000_000;
        Functions\when('time')->alias(static function () use (&$now) {
            return $now;
        });

        $class = $this->managerClassCountingGcExpiredCalls();

        // No stored throttle timestamp yet -> must run.
        $class::maybeGc();
        $this->assertSame(1, $class::$calls, 'the first ever call must run gcExpired()');

        // Well within GC_THROTTLE_SECONDS (1h) -> must be skipped.
        $now += 10;
        $class::maybeGc();
        $this->assertSame(1, $class::$calls, 'a call within the throttle window must skip gcExpired()');

        // Past GC_THROTTLE_SECONDS since the stamped timestamp -> must run again.
        $now += 3700;
        $class::maybeGc();
        $this->assertSame(2, $class::$calls, 'a call past the throttle window must run gcExpired() again');
    }

    public function test_maybe_gc_stamps_the_throttle_before_running_so_a_throwing_gc_expired_never_propagates_or_retries_every_request(): void
    {
        $class = $this->managerClassCountingGcExpiredCalls(throws: true);

        // Must not throw even though the wrapped gcExpired() itself throws.
        $class::maybeGc();
        $this->assertSame(1, $class::$calls);

        // Calling again immediately must NOT re-invoke gcExpired() — proving
        // the throttle timestamp was stamped BEFORE gcExpired() ran (not only
        // after a successful return), so a failing sweep cannot be retried on
        // every single subsequent request within the same throttle window.
        $class::maybeGc();
        $this->assertSame(
            1,
            $class::$calls,
            'the throttle timestamp must be stamped before gcExpired() runs, so a throw does not cause a retry on every request'
        );
    }

    // =========================================================================
    // GitHub issue #226 — two-tier reclaim decision (shouldReclaim()) + gcExpired()
    // =========================================================================

    public function test_should_reclaim_two_tier_decision_matrix(): void
    {
        $mgr    = new SnapshotManager();
        $method = new \ReflectionMethod(SnapshotManager::class, 'shouldReclaim');

        // Succeeded-marked, aged 90 minutes: past the 1h SUCCESS_RECLAIM_TTL_SECONDS -> reclaim.
        $this->assertTrue(
            (bool) $method->invoke($mgr, 90 * 60, true),
            'a succeeded-marked snapshot past 1h must be reclaimed'
        );

        // Unmarked, aged 90 minutes: well under the 72h backstop -> kept.
        $this->assertFalse(
            (bool) $method->invoke($mgr, 90 * 60, false),
            'an UNMARKED snapshot under 72h must be kept — only a success marker shortens the TTL'
        );

        // Unmarked, aged past 72h -> reclaim (the unchanged orphan backstop).
        $this->assertTrue(
            (bool) $method->invoke($mgr, 73 * 3600, false),
            'an unmarked snapshot past the 72h backstop must still be reclaimed'
        );

        // Succeeded-marked but younger than MIN_KEEP_AGE_SECONDS (600s) — the
        // defensive floor must override the marker regardless.
        $this->assertFalse(
            (bool) $method->invoke($mgr, 599, true),
            'DEFENSIVE FLOOR: nothing younger than MIN_KEEP_AGE_SECONDS may ever be reclaimed, even when marked succeeded'
        );

        // Unmarked and younger than MIN_KEEP_AGE_SECONDS -> kept either way.
        $this->assertFalse(
            (bool) $method->invoke($mgr, 599, false),
            'an unmarked snapshot younger than MIN_KEEP_AGE_SECONDS must also be kept'
        );
    }

    public function test_gc_expired_reclaims_an_unmarked_snapshot_past_72h_and_a_succeeded_marked_snapshot_past_1h_with_no_wp_cron_involved(): void
    {
        $mgr = $this->managerWithNoLiveSource();
        $mgr->capture('plugin', 'warmup/warmup.php', '1.0');

        $unmarkedOrphan = $this->seedSnapshot('plugin', 'orphan/orphan.php', time() - (73 * 3600));
        $succeededOld   = $this->seedSnapshot('plugin', 'done/done.php', time() - (61 * 60), true);

        // Direct call, exactly as Plugin::maybeGcSnapshots() -> SnapshotManager::maybeGc()
        // reaches it on plugins_loaded — no WP-Cron event involved at all.
        SnapshotManager::gcExpired();

        $this->assertFalse(
            is_dir($this->snapshotsBase . '/' . $unmarkedOrphan),
            'GitHub issue #226: an unmarked orphan past the 72h backstop must reclaim via a direct gcExpired() call, cron or not'
        );
        $this->assertFalse(
            is_dir($this->snapshotsBase . '/' . $succeededOld),
            'GitHub issue #226: a succeeded-marked snapshot past the 1h SUCCESS_RECLAIM_TTL must reclaim long before the 72h backstop'
        );
    }

    public function test_gc_expired_keeps_a_succeeded_marked_snapshot_younger_than_the_min_keep_age_floor(): void
    {
        $mgr = $this->managerWithNoLiveSource();
        $mgr->capture('plugin', 'warmup/warmup.php', '1.0');

        // Marked succeeded but only 5 minutes old — well under
        // MIN_KEEP_AGE_SECONDS (600s) — must survive regardless of the marker.
        $freshSucceeded = $this->seedSnapshot('plugin', 'fresh/fresh.php', time() - 300, true);

        SnapshotManager::gcExpired();

        $this->assertTrue(
            is_dir($this->snapshotsBase . '/' . $freshSucceeded),
            'a snapshot younger than MIN_KEEP_AGE_SECONDS must never be reclaimed, even when marked succeeded'
        );
    }

    public function test_gc_expired_keeps_an_unmarked_snapshot_between_1h_and_72h_but_reclaims_a_succeeded_marked_one_at_the_same_age(): void
    {
        $mgr = $this->managerWithNoLiveSource();
        $mgr->capture('plugin', 'warmup/warmup.php', '1.0');

        $ageSeconds       = 90 * 60; // 90 minutes: past the 1h SUCCESS TTL, well under the 72h backstop.
        $unmarkedSameAge  = $this->seedSnapshot('plugin', 'still-unresolved/still-unresolved.php', time() - $ageSeconds);
        $succeededSameAge = $this->seedSnapshot('plugin', 'succeeded/succeeded.php', time() - $ageSeconds, true);

        SnapshotManager::gcExpired();

        $this->assertTrue(
            is_dir($this->snapshotsBase . '/' . $unmarkedSameAge),
            'an UNMARKED snapshot aged 90 minutes (< 72h) must be KEPT'
        );
        $this->assertFalse(
            is_dir($this->snapshotsBase . '/' . $succeededSameAge),
            'a succeeded-MARKED snapshot at the SAME 90-minute age must be RECLAIMED — only the marker changes the outcome'
        );
    }

    // =========================================================================
    // GitHub issue #226 — markSucceeded()
    // =========================================================================

    public function test_mark_succeeded_stamps_the_meta_and_the_snapshot_then_reclaims_after_the_success_ttl(): void
    {
        $source = $this->root . '/plugins-src/demo-succeed';
        mkdir($source, 0755, true);
        file_put_contents($source . '/demo-succeed.php', "<?php\n// v1\n");

        $mgr               = $this->manager();
        $mgr->liveOverride = $source;

        $snap = $mgr->capture('plugin', 'demo-succeed/demo-succeed.php', '1.0');
        $this->assertNotSame('', $snap['snapshot_id']);

        $dir = $this->snapshotsBase . '/' . $snap['snapshot_id'];

        $mgr->markSucceeded($snap['snapshot_id']);

        $meta = json_decode((string) file_get_contents($dir . '/meta.json'), true);
        $this->assertArrayHasKey('succeeded_at', $meta, 'markSucceeded() must stamp the meta with a success marker');

        // Age the snapshot past SUCCESS_RECLAIM_TTL_SECONDS (1h) but still
        // well under GC_BACKSTOP_TTL_SECONDS (72h) — only the success
        // marker's shorter TTL can explain an early reclaim here.
        $meta['created_at'] = time() - (90 * 60);
        file_put_contents($dir . '/meta.json', (string) json_encode($meta));

        SnapshotManager::gcExpired();

        $this->assertFalse(
            is_dir($dir),
            'a snapshot marked succeeded by markSucceeded() must reclaim ~1h after success, long before the 72h backstop'
        );
    }

    public function test_mark_succeeded_is_a_safe_no_op_for_an_unknown_snapshot_id(): void
    {
        $mgr = $this->managerWithNoLiveSource();
        $mgr->capture('plugin', 'warmup/warmup.php', '1.0'); // force the store to exist

        // Must not throw even though this snapshot id was never captured.
        $mgr->markSucceeded('snap_' . bin2hex(random_bytes(12)));

        $this->addToAssertionCount(1);
    }

    public function test_mark_succeeded_is_a_safe_no_op_when_the_snapshot_store_is_unavailable(): void
    {
        $mgr = new class extends SnapshotManager {
            protected function snapshotBaseDir(): string
            {
                return '';
            }
        };

        // Must not throw even though the store cannot be resolved at all.
        $mgr->markSucceeded('snap_' . bin2hex(random_bytes(12)));

        $this->addToAssertionCount(1);
    }

    // =========================================================================
    // Prong A (agent-only regression fix, follow-up to GitHub issue #131) —
    // liveDir()'s open_basedir / symlinked-wp-content anchored-containment
    // fallback. Drives the REAL (unoverridden) liveDir() via reflection —
    // deliberately NOT the `manager()`/`managerWithNoLiveSource()` doubles
    // above, which override liveDir() itself and so could never exercise
    // this fix. WP_CONTENT_DIR/WP_PLUGIN_DIR are guard-defined per the same
    // "may already be a frozen global PHP constant from another test file in
    // this suite" idiom used elsewhere (e.g. RestoreRunnerLocalDestinationTest)
    // — every path resolved under them here uses a fresh, unique per-test
    // subdirectory, so whichever directory the constants already point at is
    // fine.
    // =========================================================================

    /**
     * Invoke the REAL, protected `liveDir()` via reflection — no test double
     * involved — so `realpath()` mocked false below actually exercises
     * liveDir()'s own fallback logic rather than an overridden stand-in.
     */
    private function realLiveDir(SnapshotManager $mgr, string $type, string $slug): string
    {
        // No setAccessible() call needed — ReflectionMethod::invoke() has
        // been able to call non-public methods directly since PHP 8.1.
        $method = new \ReflectionMethod(SnapshotManager::class, 'liveDir');

        return (string) $method->invoke($mgr, $type, $slug);
    }

    /**
     * Guard-define WP_CONTENT_DIR/WP_PLUGIN_DIR (may already be frozen by an
     * earlier test file in this same PHPUnit process) and ensure both exist
     * on disk, returning the resolved plugin root so the caller can create a
     * live plugin folder under it.
     */
    private function ensurePluginRootConstants(): string
    {
        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', sys_get_temp_dir() . '/wpmgr-shared-wp-content');
        }
        if (!is_dir(WP_CONTENT_DIR)) {
            mkdir(WP_CONTENT_DIR, 0755, true);
        }
        if (!defined('WP_PLUGIN_DIR')) {
            define('WP_PLUGIN_DIR', rtrim((string) WP_CONTENT_DIR, '/\\') . '/plugins');
        }
        if (!is_dir(WP_PLUGIN_DIR)) {
            mkdir(WP_PLUGIN_DIR, 0755, true);
        }

        return rtrim((string) WP_PLUGIN_DIR, '/\\');
    }

    public function test_live_dir_resolves_a_valid_plugin_source_when_realpath_containment_fails(): void
    {
        $pluginRoot = $this->ensurePluginRootConstants();

        $slug   = 'prong-a-plugin-' . bin2hex(random_bytes(6));
        $folder = $pluginRoot . '/' . $slug;
        mkdir($folder, 0755, true);
        file_put_contents($folder . '/' . $slug . '.php', "<?php\n// live plugin file\n");

        // Force EVERY realpath() call to fail, deterministically simulating
        // the open_basedir / symlinked-wp-content case that made
        // containedRealpath() reject a perfectly real, on-disk directory.
        Functions\when('realpath')->justReturn(false);

        $mgr    = new SnapshotManager();
        $result = $this->realLiveDir($mgr, 'plugin', $slug . '/' . $slug . '.php');

        $this->assertSame(
            $folder,
            $result,
            'Prong A: a real, on-disk plugin directory must still resolve when realpath()-based containment fails'
        );
    }

    public function test_live_dir_resolves_a_valid_theme_source_when_realpath_containment_fails(): void
    {
        $this->ensurePluginRootConstants();
        $themeRoot = rtrim((string) WP_CONTENT_DIR, '/\\') . '/themes';
        if (!is_dir($themeRoot)) {
            mkdir($themeRoot, 0755, true);
        }

        $slug   = 'prong-a-theme-' . bin2hex(random_bytes(6));
        $folder = $themeRoot . '/' . $slug;
        mkdir($folder, 0755, true);
        file_put_contents($folder . '/style.css', "/*\nTheme Name: Prong A Test Theme\n*/\n");

        Functions\when('realpath')->justReturn(false);

        $mgr    = new SnapshotManager();
        $result = $this->realLiveDir($mgr, 'theme', $slug);

        $this->assertSame(
            $folder,
            $result,
            'Prong A: a real, on-disk theme directory must still resolve when realpath()-based containment fails'
        );
    }

    public function test_live_dir_still_rejects_a_path_escape_even_when_realpath_containment_fails(): void
    {
        // Defense-in-depth: even with the anchored-containment fallback
        // active, a traversal slug fed DIRECTLY to liveDir() (bypassing
        // UpdateCommand::sanitizeSlug(), which would already reject it
        // upstream in production) must never resolve outside wp-content.
        $this->ensurePluginRootConstants();

        Functions\when('realpath')->justReturn(false);

        $mgr = new SnapshotManager();

        $this->assertSame(
            '',
            $this->realLiveDir($mgr, 'theme', '../../etc/passwd'),
            'Prong A: the anchored-containment fallback must still reject a traversal slug, never resolve it'
        );
        $this->assertSame(
            '',
            $this->realLiveDir($mgr, 'plugin', '../escape/escape.php'),
            'Prong A: a plugin folder derived as ".." must still be rejected by the fallback, never resolved'
        );
    }

    public function test_live_dir_returns_empty_for_a_source_that_genuinely_does_not_exist_even_with_the_fallback(): void
    {
        // The fallback must not turn a genuinely WRONG/missing path into a
        // false positive — only a real, on-disk directory may resolve.
        $this->ensurePluginRootConstants();

        Functions\when('realpath')->justReturn(false);

        $mgr = new SnapshotManager();

        $this->assertSame(
            '',
            $this->realLiveDir($mgr, 'plugin', 'never-installed-' . bin2hex(random_bytes(6))),
            'Prong A: a plugin folder that does not exist on disk must still resolve to \'\', fallback or not'
        );
    }

    // =========================================================================
    // F4 — stranded `.wpmgr-old-<id>` aside sweep (gcExpired())
    // =========================================================================

    /**
     * A SnapshotManager subclass whose strandedAsideRoots() is fully
     * test-controlled (never touches the real WP_PLUGIN_DIR/WP_CONTENT_DIR
     * constants), exercised via `new static()` late static binding inside
     * gcExpired() itself — see gcExpired()'s doc for why this works.
     *
     * The configured root is held in a STATIC property, not a constructor
     * argument: gcExpired() constructs its own fresh instance internally via
     * `new static()` (no args), so any per-instance constructor parameter
     * would never reach that instance. A static property is shared by every
     * instance of this same anonymous class, including that internal one.
     *
     * @return class-string<SnapshotManager>
     */
    private function managerClassWithPluginRoot(string $pluginsRoot): string
    {
        $class = new class extends SnapshotManager {
            public static string $testPluginsRoot = '';

            protected function strandedAsideRoots(): array
            {
                return [self::$testPluginsRoot];
            }
        };
        $class::$testPluginsRoot = $pluginsRoot;

        return get_class($class);
    }

    public function test_gc_expired_sweeps_a_stranded_wpmgr_old_aside_past_the_backstop_ttl(): void
    {
        $pluginsRoot = $this->root . '/plugins-root';
        mkdir($pluginsRoot . '/demo', 0755, true);

        $strandedAside = $pluginsRoot . '/demo.wpmgr-old-snap_' . bin2hex(random_bytes(12));
        mkdir($strandedAside, 0755, true);
        file_put_contents($strandedAside . '/marker.txt', 'stranded original');
        touch($strandedAside, time() - (73 * 3600)); // > 72h backstop TTL

        $recentAside = $pluginsRoot . '/other.wpmgr-old-snap_' . bin2hex(random_bytes(12));
        mkdir($recentAside, 0755, true);
        touch($recentAside, time() - 3600); // 1h — must survive

        $class = $this->managerClassWithPluginRoot($pluginsRoot);
        $class::gcExpired();

        $this->assertFalse(
            is_dir($strandedAside),
            'F4: a stranded .wpmgr-old-<id> aside past the GC backstop TTL must be swept'
        );
        $this->assertTrue(is_dir($recentAside), 'a recent stranded aside must survive the sweep');
        $this->assertTrue(
            is_dir($pluginsRoot . '/demo'),
            'F4 must never touch the LIVE plugin directory itself, only the stranded aside sibling'
        );
    }

    public function test_gc_expired_ignores_a_directory_that_only_partially_matches_the_stranded_aside_pattern(): void
    {
        $pluginsRoot = $this->root . '/plugins-root2';
        mkdir($pluginsRoot, 0755, true);

        // A plain plugin folder that happens to contain the substring but
        // does not END in the exact `.wpmgr-old-snap_<id>` suffix pattern —
        // must never be swept.
        $unrelated = $pluginsRoot . '/wpmgr-old-lookalike';
        mkdir($unrelated, 0755, true);
        touch($unrelated, time() - (73 * 3600));

        $class = $this->managerClassWithPluginRoot($pluginsRoot);
        $class::gcExpired();

        $this->assertTrue(
            is_dir($unrelated),
            'F4 must only match the exact stranded-aside suffix pattern, never a lookalike plugin folder name'
        );
    }

    /**
     * MEDIUM-2 (GitHub issue #210 security review) — a stranded aside left
     * behind by an INTERRUPTED update-watchdog restore
     * (`wpmgr_watchdog_swap_directories()`'s `.wpmgr-watchdog-old-<snap_id>`
     * naming, not SnapshotManager's own `.wpmgr-old-<snap_id>`) must be
     * reclaimed by the SAME GC backstop, or it leaks as a permanent ghost
     * plugin-directory duplicate.
     */
    public function test_gc_expired_sweeps_a_stranded_watchdog_aside_past_the_backstop_ttl(): void
    {
        $pluginsRoot = $this->root . '/plugins-root-watchdog';
        mkdir($pluginsRoot . '/demo', 0755, true);

        $strandedWatchdogAside = $pluginsRoot . '/demo.wpmgr-watchdog-old-snap_' . bin2hex(random_bytes(12));
        mkdir($strandedWatchdogAside, 0755, true);
        file_put_contents($strandedWatchdogAside . '/demo.php', "<?php\n// Plugin Name: Demo\n");
        touch($strandedWatchdogAside, time() - (73 * 3600)); // > 72h backstop TTL

        $recentWatchdogAside = $pluginsRoot . '/other.wpmgr-watchdog-old-snap_' . bin2hex(random_bytes(12));
        mkdir($recentWatchdogAside, 0755, true);
        touch($recentWatchdogAside, time() - 3600); // 1h — must survive

        $class = $this->managerClassWithPluginRoot($pluginsRoot);
        $class::gcExpired();

        $this->assertFalse(
            is_dir($strandedWatchdogAside),
            'MEDIUM-2: a stranded .wpmgr-watchdog-old-<id> aside past the GC backstop TTL must be swept, same as the original .wpmgr-old-<id> naming'
        );
        $this->assertTrue(is_dir($recentWatchdogAside), 'a recent stranded watchdog aside must survive the sweep');
        $this->assertTrue(
            is_dir($pluginsRoot . '/demo'),
            'the sweep must never touch the LIVE plugin directory itself, only the stranded aside sibling'
        );
    }

    // =========================================================================
    // F1 — recovery from a re-interrupted restore (deterministic aside name)
    // =========================================================================

    public function test_restore_recovers_from_a_previously_interrupted_restore_for_the_same_snapshot(): void
    {
        $mgr  = $this->manager();
        $live = $this->root . '/plugins-src/demo5';
        mkdir($live, 0755, true);
        file_put_contents($live . '/marker.txt', 'GOOD-PRE-UPDATE-CONTENT');
        $mgr->liveOverride = $live;

        // Capture while marker.txt holds the GOOD (pre-update) content — this
        // is exactly what a correctly-completed restore must end up with.
        $snap = $mgr->capture('plugin', 'demo5/demo5.php', '1.0');
        $this->assertNotSame('', $snap['snapshot_id']);
        $asideName = $live . '.wpmgr-old-' . $snap['snapshot_id'];

        // Construct the exact end state a FIRST restore() call would leave
        // behind if hard-killed mid-copy: $live was successfully moved aside
        // (holding whatever the post-update/possibly-broken state was), and
        // copyDir() got partway through writing the good payload back into a
        // freshly (re)created $live before the process died. This is built
        // directly here (mirroring restore()'s own rename()+copyDir()
        // sequence) rather than via an actual kill, which cannot be
        // deterministically triggered in a unit test.
        rename($live, $asideName);
        mkdir($live, 0755, true);
        file_put_contents($live . '/partial.txt', 'PARTIAL-COPY-FROM-INTERRUPTED-RESTORE');
        $this->assertTrue(is_dir($asideName), 'precondition: the interrupted first attempt left an aside behind');
        $this->assertTrue(is_dir($live), 'precondition: the interrupted first attempt left a partial live dir behind');

        // A SECOND restore() call for the SAME snapshot must recover cleanly
        // — it must never bail with "Could not stage live directory aside"
        // (the ENOTEMPTY wedge F1 fixes: rename() onto a non-empty existing
        // $asideName fails outright) and must never leave the good pre-update
        // snapshot payload stranded/unreachable.
        $mgr->failCopy = false;
        $result        = $mgr->restore('plugin', 'demo5/demo5.php', $snap['snapshot_id']);

        $this->assertTrue($result['ok'], 'F1: a re-interrupted restore must recover, not wedge: ' . $result['log']);
        $this->assertTrue(is_dir($live));
        $this->assertSame(
            'GOOD-PRE-UPDATE-CONTENT',
            file_get_contents($live . '/marker.txt'),
            'F1: the second restore attempt must end with the GOOD pre-update snapshot content actually in place — the live directory must be genuinely restored'
        );
        $this->assertFalse(
            is_file($live . '/partial.txt'),
            'F1: the partial/junk content from the interrupted first attempt must not survive'
        );
        $this->assertFalse(
            is_dir($asideName),
            'F1: no stranded aside may remain on disk after a successful recovery'
        );
    }

    public function test_restore_second_attempt_is_not_wedged_even_when_the_first_attempts_aside_is_the_only_thing_left(): void
    {
        // A narrower variant: the interrupted first attempt died before
        // copyDir() even got as far as (re)creating $live at all — only the
        // aside survives, $live is entirely absent. The second attempt must
        // still recover cleanly (this is really just the ordinary
        // "live missing, aside present" case routed through the SAME heal
        // branch, since is_dir($asideName) is checked unconditionally).
        $mgr  = $this->manager();
        $live = $this->root . '/plugins-src/demo6';
        mkdir($live, 0755, true);
        file_put_contents($live . '/marker.txt', 'GOOD-PRE-UPDATE-CONTENT');
        $mgr->liveOverride = $live;

        $snap = $mgr->capture('plugin', 'demo6/demo6.php', '1.0');
        $this->assertNotSame('', $snap['snapshot_id']);
        $asideName = $live . '.wpmgr-old-' . $snap['snapshot_id'];

        rename($live, $asideName);
        $this->assertFalse(is_dir($live));
        $this->assertTrue(is_dir($asideName));

        $result = $mgr->restore('plugin', 'demo6/demo6.php', $snap['snapshot_id']);

        $this->assertTrue($result['ok'], 'F1: recovery must succeed even when only the aside survived: ' . $result['log']);
        $this->assertSame('GOOD-PRE-UPDATE-CONTENT', file_get_contents($live . '/marker.txt'));
        $this->assertFalse(is_dir($asideName));
    }

    // =========================================================================
    // Fix 1 (issue #131 final-hardening review) — staged aside is GC-safe
    // =========================================================================

    public function test_restore_stamps_the_staged_asides_mtime_so_the_gc_backstop_cannot_reap_it_during_the_copy_back(): void
    {
        $mgr  = $this->manager();
        $live = $this->root . '/plugins-src/demo7';
        mkdir($live, 0755, true);
        file_put_contents($live . '/marker.txt', 'GOOD-PRE-UPDATE-CONTENT');
        $mgr->liveOverride = $live;

        $snap = $mgr->capture('plugin', 'demo7/demo7.php', '1.0');
        $this->assertNotSame('', $snap['snapshot_id']);
        $asideName = $live . '.wpmgr-old-' . $snap['snapshot_id'];

        // The LIVE directory's own mtime is already older than the GC
        // backstop TTL (72h) BEFORE restore() ever runs — e.g. a plugin
        // whose files simply have not changed recently. rename() (used to
        // stage the aside) PRESERVES this old mtime, so without Fix 1's
        // @touch() the aside would be "born" already past
        // GC_BACKSTOP_TTL_SECONDS the instant it is carved out — eligible
        // for sweepStrandedAsides() to reap it while this very restore() is
        // still using it.
        touch($live, time() - (73 * 3600));
        clearstatcache(true, $live);

        // Observe the aside's mtime at the exact moment restore()'s
        // copy-back step (copyDir($payload, $live)) begins — this is the
        // narrow window between staging the aside and the copy finishing
        // (or failing) that Fix 1 protects.
        $observedMtime = null;
        $mgr->onCopyDir = function () use ($asideName, &$observedMtime): void {
            $observedMtime = @filemtime($asideName);
        };

        $result = $mgr->restore('plugin', 'demo7/demo7.php', $snap['snapshot_id']);

        $this->assertTrue($result['ok'], 'precondition: the happy-path restore must still succeed: ' . $result['log']);
        $this->assertNotNull($observedMtime, 'precondition: the aside must already exist when the copy-back step starts');
        $this->assertGreaterThanOrEqual(
            time() - 5,
            $observedMtime,
            'Fix 1: restore() must stamp the staged aside\'s mtime to staging time (not inherit the old live-dir mtime via rename()), so sweepStrandedAsides()\'s age-based GC gives it the full 72h grace period even if interrupted right after staging'
        );
    }

    public function test_a_freshly_staged_restore_aside_survives_a_gc_backstop_tick_mid_copy_even_when_the_live_dir_was_already_old(): void
    {
        // End-to-end variant of the mtime test above, exercised through the
        // ACTUAL GC backstop sweep (gcExpired() -> sweepStrandedAsides()),
        // using the same static-property seam as managerClassWithPluginRoot()
        // so gcExpired()'s internal `new static()` instance (see gcExpired()'s
        // doc) resolves the same stranded-aside root this test stages into.
        //
        // The copy-back is forced to FAIL so restore() must roll back to the
        // staged aside — this is what actually proves the fix: without it,
        // a GC tick landing mid-copy deletes the aside out from under the
        // in-progress restore(), and the subsequent rollback attempt finds
        // nothing to roll back to (data loss); with the fix, the aside's
        // mtime was stamped fresh at staging time, so the very same GC tick
        // leaves it alone and the rollback succeeds.
        $class = new class extends SnapshotManager {
            public static string $testPluginsRoot = '';
            public string $liveOverride            = '';
            public bool $failCopy                  = false;

            /** @var (callable(): void)|null */
            public $onCopyDir = null;

            protected function liveDir(string $type, string $slug): string
            {
                return $this->liveOverride;
            }

            protected function copyDir(string $src, string $dst): bool
            {
                if ($this->onCopyDir !== null) {
                    ($this->onCopyDir)();
                }

                if ($this->failCopy) {
                    // Mirror copyDir()'s real first step (mkdir the
                    // destination) even though the copy is about to fail —
                    // the S5 precondition (see manager()'s helper above).
                    if (!is_dir($dst)) {
                        mkdir($dst, 0755, true);
                    }
                    return false;
                }

                return parent::copyDir($src, $dst);
            }

            protected function strandedAsideRoots(): array
            {
                return [self::$testPluginsRoot];
            }
        };

        $pluginsRoot = $this->root . '/plugins-root3';
        mkdir($pluginsRoot, 0755, true);
        $class::$testPluginsRoot = $pluginsRoot;

        $live = $pluginsRoot . '/demo8';
        mkdir($live, 0755, true);
        file_put_contents($live . '/marker.txt', 'SNAPSHOT-CONTENT');

        $mgr               = new $class();
        $mgr->liveOverride = $live;

        $snap = $mgr->capture('plugin', 'demo8/demo8.php', '1.0');
        $this->assertNotSame('', $snap['snapshot_id']);

        // Mutate live to the pre-restore-attempt state, and age the
        // directory past the GC backstop TTL BEFORE restore() ever runs —
        // rename() (used to stage the aside) preserves this age onto the
        // aside it carves out.
        file_put_contents($live . '/marker.txt', 'PRE-RESTORE-ATTEMPT-CONTENT');
        touch($live, time() - (73 * 3600));
        clearstatcache(true, $live);

        // Run the GC backstop from INSIDE the copy-back step — the exact
        // "restore in progress" window — before the (forced) copy failure
        // is even reported back to restore().
        $mgr->failCopy  = true;
        $mgr->onCopyDir = static function () use ($class): void {
            $class::gcExpired();
        };

        $result = $mgr->restore('plugin', 'demo8/demo8.php', $snap['snapshot_id']);

        $this->assertFalse($result['ok']);
        $this->assertTrue(is_dir($live), 'the live directory must exist again — never left missing');
        $this->assertSame(
            'PRE-RESTORE-ATTEMPT-CONTENT',
            file_get_contents($live . '/marker.txt'),
            'Fix 1: a GC backstop tick landing mid-copy must never reap the staged aside — the rollback must still find it and restore the pre-restore-attempt state, not report "no pre-existing directory to roll back to"'
        );
        $this->assertStringContainsString('rolled back to the pre-restore original', $result['log']);
    }

    // =========================================================================
    // S5 — restore()'s rollback-on-copy-failure
    // =========================================================================

    public function test_restore_rolls_back_to_the_pre_restore_state_when_copy_fails_mid_write(): void
    {
        $mgr  = $this->manager();
        $live = $this->root . '/plugins-src/demo2';
        mkdir($live, 0755, true);
        file_put_contents($live . '/marker.txt', 'SNAPSHOT-CONTENT');
        $mgr->liveOverride = $live;

        // Capture while marker.txt still holds the "good" content.
        $snap = $mgr->capture('plugin', 'demo2/demo2.php', '1.0');
        $this->assertNotSame('', $snap['snapshot_id']);

        // Mutate live to whatever is on disk right BEFORE the restore() call
        // under test — this is the state a correctly-fixed failed restore
        // must roll back TO.
        file_put_contents($live . '/marker.txt', 'PRE-RESTORE-ATTEMPT-CONTENT');
        $mgr->failCopy = true;

        $result = $mgr->restore('plugin', 'demo2/demo2.php', $snap['snapshot_id']);

        $this->assertFalse($result['ok']);
        $this->assertTrue(is_dir($live), 'S5: the live directory must exist again — never left missing');
        $this->assertSame(
            'PRE-RESTORE-ATTEMPT-CONTENT',
            file_get_contents($live . '/marker.txt'),
            'S5: a mid-copy restore failure must roll back to the state that existed right before the restore attempt, not strand it aside nor leave a half-written directory'
        );
        $this->assertFalse(
            is_dir($live . '.wpmgr-old-' . $snap['snapshot_id']),
            'S5: the set-aside copy must be renamed back, not left stranded forever'
        );
        $this->assertStringContainsString('rolled back to the pre-restore original', $result['log']);
    }

    public function test_restore_reports_no_original_when_live_did_not_exist_before_the_attempt(): void
    {
        $mgr  = $this->manager();
        $live = $this->root . '/plugins-src/demo3';
        mkdir($live, 0755, true);
        file_put_contents($live . '/marker.txt', 'v1');
        $mgr->liveOverride = $live;

        $snap = $mgr->capture('plugin', 'demo3/demo3.php', '1.0');
        $this->assertNotSame('', $snap['snapshot_id']);

        // Live directory removed entirely before the restore attempt begins
        // — there is nothing for restore() to have moved aside.
        $this->rrmdir($live);
        $this->assertFalse(is_dir($live));

        $mgr->failCopy = true;
        $result = $mgr->restore('plugin', 'demo3/demo3.php', $snap['snapshot_id']);

        $this->assertFalse($result['ok']);
        $this->assertStringContainsString('no pre-existing directory to roll back to', $result['log']);
    }

    public function test_restore_succeeds_and_removes_the_set_aside_copy_on_the_happy_path(): void
    {
        $mgr  = $this->manager();
        $live = $this->root . '/plugins-src/demo4';
        mkdir($live, 0755, true);
        file_put_contents($live . '/marker.txt', 'v1');
        $mgr->liveOverride = $live;

        $snap = $mgr->capture('plugin', 'demo4/demo4.php', '1.0');
        $this->assertNotSame('', $snap['snapshot_id']);

        // Simulate the "current" (post-update) live state.
        file_put_contents($live . '/marker.txt', 'v2');

        $result = $mgr->restore('plugin', 'demo4/demo4.php', $snap['snapshot_id']);

        $this->assertTrue($result['ok']);
        $this->assertSame('v1', file_get_contents($live . '/marker.txt'));
        $this->assertFalse(is_dir($live . '.wpmgr-old-' . $snap['snapshot_id']));
    }

    // =========================================================================
    // GitHub issue #210 — resolvedRestorePaths() (update-watchdog ARM step)
    // =========================================================================

    public function test_resolved_restore_paths_returns_the_live_and_payload_dirs_for_a_real_snapshot(): void
    {
        $mgr  = $this->manager();
        $live = $this->root . '/plugins-src/watchdog-demo';
        mkdir($live, 0755, true);
        file_put_contents($live . '/marker.txt', 'v1');
        $mgr->liveOverride = $live;

        $snap = $mgr->capture('plugin', 'watchdog-demo/watchdog-demo.php', '1.0');
        $this->assertNotSame('', $snap['snapshot_id']);

        $paths = $mgr->resolvedRestorePaths('plugin', 'watchdog-demo/watchdog-demo.php', $snap['snapshot_id']);

        $this->assertSame($live, $paths['live'], 'resolvedRestorePaths() must return the exact liveDir() resolution');
        // resolveSnapshotDir() returns a realpath()'d value (e.g. macOS
        // resolves /var -> /private/var), so compare through realpath() on
        // both sides rather than asserting exact string identity against a
        // manually-concatenated (non-canonicalized) expectation.
        $this->assertSame(
            realpath($this->snapshotsBase . '/' . $snap['snapshot_id']) . '/payload',
            $paths['payload'],
            'resolvedRestorePaths() must return the exact snapshot payload directory'
        );
        $this->assertTrue(is_dir($paths['payload']), 'the returned payload dir must actually exist on disk');
    }

    public function test_resolved_restore_paths_returns_empty_for_an_unknown_snapshot_id(): void
    {
        $mgr  = $this->manager();
        $mgr->liveOverride = $this->root . '/plugins-src/never-captured';

        $paths = $mgr->resolvedRestorePaths('plugin', 'never-captured/never-captured.php', 'snap_' . bin2hex(random_bytes(12)));

        $this->assertSame(['live' => '', 'payload' => ''], $paths);
    }

    public function test_resolved_restore_paths_returns_empty_when_the_live_dir_cannot_be_resolved(): void
    {
        $mgr  = $this->manager();
        $live = $this->root . '/plugins-src/watchdog-demo2';
        mkdir($live, 0755, true);
        file_put_contents($live . '/marker.txt', 'v1');
        $mgr->liveOverride = $live;

        $snap = $mgr->capture('plugin', 'watchdog-demo2/watchdog-demo2.php', '1.0');
        $this->assertNotSame('', $snap['snapshot_id']);

        // Simulate liveDir() being unable to resolve (e.g. the live source
        // has since been removed) — resolvedRestorePaths() must propagate
        // '' rather than fabricate a path.
        $mgr->liveOverride = '';

        $paths = $mgr->resolvedRestorePaths('plugin', 'watchdog-demo2/watchdog-demo2.php', $snap['snapshot_id']);

        $this->assertSame(['live' => '', 'payload' => ''], $paths);
    }
}
