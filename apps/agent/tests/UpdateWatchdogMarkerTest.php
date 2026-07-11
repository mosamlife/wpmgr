<?php
/**
 * UpdateWatchdogMarkerTest — GitHub issue #210 (update-watchdog mu-plugin).
 *
 * Covers the ARM-side marker writer: schema, storage location, the TTL
 * bound (< SnapshotManager::MIN_KEEP_AGE_SECONDS), per-slug refresh
 * semantics across a multi-item batch, opportunistic pruning of expired
 * sibling entries, self-protection against arming for the running agent's
 * own plugin directory, and stale-claim-file GC.
 *
 * Also covers MEDIUM-1b (GitHub issue #210 security review): clearSlug()'s
 * single-entry removal, and disarmHealthy()'s fail-closed healthy-boot
 * disarm (active + on-disk at the armed to_version clears; not-yet-updated,
 * absent, or inactive stays armed; sibling entries always untouched).
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Support\SnapshotManager;
use WPMgr\Agent\Support\UpdateWatchdogMarker;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\UpdateWatchdogMarker
 */
final class UpdateWatchdogMarkerTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->cleanState();
    }

    protected function tear_down(): void
    {
        $this->cleanState();
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Guard-define WP_CONTENT_DIR (may already be a frozen global PHP
     * constant from another test file in this same PHPUnit process — same
     * idiom as SnapshotManagerTest::ensurePluginRootConstants()) and return
     * the watchdog state directory the marker file lives under.
     */
    private function stateDir(): string
    {
        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', sys_get_temp_dir() . '/wpmgr-shared-wp-content');
        }
        if (!is_dir(WP_CONTENT_DIR)) {
            mkdir(WP_CONTENT_DIR, 0755, true);
        }

        return rtrim((string) WP_CONTENT_DIR, '/\\') . '/wpmgr-update-watchdog';
    }

    private function markerFile(): string
    {
        return $this->stateDir() . '/watchdog-marker.json';
    }

    private function cleanState(): void
    {
        $dir = $this->stateDir();
        if (!is_dir($dir)) {
            return;
        }
        $items = scandir($dir) ?: [];
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            unlink($dir . '/' . $item); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test fixture cleanup
        }
    }

    /**
     * @return array<string,mixed>
     */
    private function readMarkerFile(): array
    {
        $raw = file_get_contents($this->markerFile());
        $decoded = json_decode((string) $raw, true);

        return is_array($decoded) ? $decoded : [];
    }

    // =========================================================================
    // Schema, storage location, TTL
    // =========================================================================

    public function test_arm_writes_the_marker_file_at_the_wp_content_based_state_dir_with_correct_schema(): void
    {
        // arm() persists these as opaque, already-validated strings — it
        // does not require them to exist on disk (SnapshotManager::
        // resolvedRestorePaths() is what enforces that BEFORE arm() is ever
        // called; see UpdateCommandTest's ARM tests for that end-to-end path).
        $live    = sys_get_temp_dir() . '/wpmgr-watchdog-marker-test-live-' . bin2hex(random_bytes(6));
        $payload = sys_get_temp_dir() . '/wpmgr-watchdog-marker-test-payload-' . bin2hex(random_bytes(6));

        $before = time();
        UpdateWatchdogMarker::arm('plugin', 'demo/demo.php', 'snap_test123', $live, $payload, '5.3.0');
        $after = time();

        $this->assertFileExists($this->markerFile(), 'arm() must write the marker file at the WP_CONTENT_DIR-based state dir');

        $decoded = $this->readMarkerFile();
        $this->assertArrayHasKey('markers', $decoded);
        $this->assertCount(1, $decoded['markers']);

        $entry = $decoded['markers'][0];
        $this->assertSame('plugin', $entry['type']);
        $this->assertSame('demo/demo.php', $entry['slug']);
        $this->assertSame('snap_test123', $entry['snapshot_id']);
        $this->assertSame(rtrim($live, '/\\'), $entry['live_dir']);
        $this->assertSame(rtrim($payload, '/\\'), $entry['payload_dir']);
        $this->assertSame('5.3.0', $entry['to_version'], 'the armed target version (MEDIUM-1b disarm input) must be persisted');
        $this->assertGreaterThanOrEqual($before, $entry['applied_at']);
        $this->assertLessThanOrEqual($after, $entry['applied_at']);

        $ttl = $entry['expires_at'] - $entry['applied_at'];
        $this->assertSame(UpdateWatchdogMarker::TTL_SECONDS, $ttl, 'the persisted TTL must exactly match TTL_SECONDS');
        $this->assertLessThan(
            600,
            $ttl,
            'TTL MUST stay strictly less than SnapshotManager::MIN_KEEP_AGE_SECONDS (600) so the marker can never outlive the snapshot retention floor'
        );
        $this->assertGreaterThanOrEqual(180, $ttl, 'TTL must still comfortably overlap the ~1-minute CP post-update health-probe window');
    }

    public function test_arm_no_ops_silently_on_missing_arguments(): void
    {
        UpdateWatchdogMarker::arm('', 'slug', 'snap_x', '/live', '/payload');
        UpdateWatchdogMarker::arm('plugin', '', 'snap_x', '/live', '/payload');
        UpdateWatchdogMarker::arm('plugin', 'slug', '', '/live', '/payload');
        UpdateWatchdogMarker::arm('plugin', 'slug', 'snap_x', '', '/payload');
        UpdateWatchdogMarker::arm('plugin', 'slug', 'snap_x', '/live', '');
        UpdateWatchdogMarker::arm('core', 'core', 'snap_x', '/live', '/payload');

        $this->assertFileDoesNotExist($this->markerFile(), 'arm() must never write a file for an incomplete or non-plugin/theme call');
    }

    // =========================================================================
    // Multi-item batch: per-slug refresh, sibling entries preserved
    // =========================================================================

    public function test_arming_a_second_slug_preserves_the_first_slugs_entry(): void
    {
        $tmp = sys_get_temp_dir();
        $liveA = $tmp . '/wpmgr-watchdog-a';
        $liveB = $tmp . '/wpmgr-watchdog-b';
        $payloadA = $tmp . '/wpmgr-watchdog-snap-a-payload';
        $payloadB = $tmp . '/wpmgr-watchdog-snap-b-payload';

        UpdateWatchdogMarker::arm('plugin', 'a/a.php', 'snap_a', $liveA, $payloadA);
        UpdateWatchdogMarker::arm('plugin', 'b/b.php', 'snap_b', $liveB, $payloadB);

        $decoded = $this->readMarkerFile();
        $this->assertCount(2, $decoded['markers'], 'both items from the same batch must have their own surviving entry');

        $slugs = array_column($decoded['markers'], 'slug');
        $this->assertContains('a/a.php', $slugs);
        $this->assertContains('b/b.php', $slugs);
    }

    public function test_rearming_the_same_slug_refreshes_in_place_rather_than_duplicating(): void
    {
        $tmp = sys_get_temp_dir();
        $live = $tmp . '/wpmgr-watchdog-demo';
        $payload1 = $tmp . '/wpmgr-watchdog-snap-1-payload';
        $payload2 = $tmp . '/wpmgr-watchdog-snap-2-payload';

        UpdateWatchdogMarker::arm('plugin', 'demo/demo.php', 'snap_1', $live, $payload1);
        UpdateWatchdogMarker::arm('plugin', 'demo/demo.php', 'snap_2', $live, $payload2);

        $decoded = $this->readMarkerFile();
        $this->assertCount(1, $decoded['markers'], 're-arming the exact same type/slug must refresh in place, not accumulate');
        $this->assertSame('snap_2', $decoded['markers'][0]['snapshot_id'], 'the LATEST apply for a slug must win');
    }

    public function test_arm_opportunistically_prunes_an_already_expired_sibling_entry(): void
    {
        $tmp = sys_get_temp_dir();
        $liveExpired  = $tmp . '/wpmgr-watchdog-expired';
        $liveFresh    = $tmp . '/wpmgr-watchdog-fresh';
        $payloadStale = $tmp . '/wpmgr-watchdog-snap-stale-payload';
        $payloadFresh = $tmp . '/wpmgr-watchdog-snap-fresh-payload';

        // Seed a marker file directly with an already-expired entry (as if
        // arm() had written it long ago and its TTL has since elapsed).
        $dir = $this->stateDir();
        if (!is_dir($dir)) {
            mkdir($dir, 0755, true);
        }
        file_put_contents($this->markerFile(), (string) json_encode([
            'markers' => [[
                'type'        => 'plugin',
                'slug'        => 'expired/expired.php',
                'snapshot_id' => 'snap_stale',
                'live_dir'    => $liveExpired,
                'payload_dir' => $payloadStale,
                'applied_at'  => time() - 1000,
                'expires_at'  => time() - 700,
            ]],
        ]));

        UpdateWatchdogMarker::arm('plugin', 'fresh/fresh.php', 'snap_fresh', $liveFresh, $payloadFresh);

        $decoded = $this->readMarkerFile();
        $slugs = array_column($decoded['markers'], 'slug');
        $this->assertNotContains('expired/expired.php', $slugs, 'an expired sibling entry must be pruned opportunistically');
        $this->assertContains('fresh/fresh.php', $slugs);
    }

    // =========================================================================
    // Self-protection: never arm against the running agent's own plugin dir
    // =========================================================================

    public function test_arm_refuses_to_arm_against_the_running_agents_own_plugin_directory(): void
    {
        $this->assertTrue(defined('WPMGR_AGENT_DIR'), 'precondition: WPMGR_AGENT_DIR must be defined by the test bootstrap');
        $selfDir = rtrim((string) constant('WPMGR_AGENT_DIR'), '/\\');

        $payload = sys_get_temp_dir() . '/wpmgr-watchdog-snap-self-payload';

        UpdateWatchdogMarker::arm('plugin', 'wpmgr-agent/wpmgr-agent.php', 'snap_self', $selfDir, $payload);

        $this->assertFileDoesNotExist(
            $this->markerFile(),
            'arm() must refuse to write ANY marker when live_dir resolves to the running agent\'s own plugin directory'
        );
    }

    // =========================================================================
    // Stale claim-file GC
    // =========================================================================

    public function test_arm_sweeps_stale_claim_sentinels_but_keeps_recent_ones(): void
    {
        $dir = $this->stateDir();
        if (!is_dir($dir)) {
            mkdir($dir, 0755, true);
        }

        $staleClaim  = $dir . '/watchdog-claim-snap_old.claim';
        $recentClaim = $dir . '/watchdog-claim-snap_new.claim';
        touch($staleClaim, time() - 7200); // 2h — past the 1h stale threshold
        touch($recentClaim, time() - 60);  // 1m — must survive

        $live    = sys_get_temp_dir() . '/wpmgr-watchdog-claim-sweep-demo';
        $payload = sys_get_temp_dir() . '/wpmgr-watchdog-claim-sweep-demo-payload';

        UpdateWatchdogMarker::arm('plugin', 'demo/demo.php', 'snap_demo', $live, $payload);

        $this->assertFileDoesNotExist($staleClaim, 'a claim sentinel older than the stale threshold must be swept on the next arm()');
        $this->assertFileExists($recentClaim, 'a recent claim sentinel must survive the sweep');
    }

    // =========================================================================
    // SnapshotManager::resolvedRestorePaths() -> UpdateWatchdogMarker::arm()
    // end-to-end wiring sanity (real SnapshotManager, real disk paths)
    // =========================================================================

    public function test_end_to_end_resolved_paths_from_a_real_snapshot_manager_arm_successfully(): void
    {
        $uploadsDir = sys_get_temp_dir() . '/wpmgr-watchdog-e2e-' . bin2hex(random_bytes(6));
        mkdir($uploadsDir, 0755, true);
        \Brain\Monkey\Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploadsDir]);

        $mgr  = new class extends SnapshotManager {
            public string $liveOverride = '';

            protected function liveDir(string $type, string $slug): string
            {
                return $this->liveOverride;
            }
        };
        $live = sys_get_temp_dir() . '/wpmgr-watchdog-e2e-live-' . bin2hex(random_bytes(6));
        mkdir($live, 0755, true);
        file_put_contents($live . '/marker.txt', 'v1');
        $mgr->liveOverride = $live;

        $snap = $mgr->capture('plugin', 'e2e-demo/e2e-demo.php', '1.0');
        $this->assertNotSame('', $snap['snapshot_id']);

        $paths = $mgr->resolvedRestorePaths('plugin', 'e2e-demo/e2e-demo.php', $snap['snapshot_id']);
        $this->assertNotSame('', $paths['live']);
        $this->assertNotSame('', $paths['payload']);

        UpdateWatchdogMarker::arm('plugin', 'e2e-demo/e2e-demo.php', $snap['snapshot_id'], $paths['live'], $paths['payload']);

        $decoded = $this->readMarkerFile();
        $this->assertCount(1, $decoded['markers']);
        $this->assertSame($paths['live'], $decoded['markers'][0]['live_dir']);
        $this->assertSame($paths['payload'], $decoded['markers'][0]['payload_dir']);

        $this->rrmdir($uploadsDir);
        $this->rrmdir($live);
    }

    // =========================================================================
    // MEDIUM-1b (GitHub issue #210 security review) — clearSlug() / disarmHealthy()
    // =========================================================================

    public function test_clear_slug_removes_only_the_matching_entry_and_leaves_other_slugs_entries_untouched(): void
    {
        $tmp = sys_get_temp_dir();
        UpdateWatchdogMarker::arm('plugin', 'a/a.php', 'snap_a', $tmp . '/wpmgr-watchdog-live-a', $tmp . '/wpmgr-watchdog-payload-a', '2.0');
        UpdateWatchdogMarker::arm('plugin', 'b/b.php', 'snap_b', $tmp . '/wpmgr-watchdog-live-b', $tmp . '/wpmgr-watchdog-payload-b', '3.0');

        UpdateWatchdogMarker::clearSlug('plugin', 'a/a.php');

        $decoded = $this->readMarkerFile();
        $this->assertCount(1, $decoded['markers'], 'clearSlug() must remove ONLY the matching entry');
        $this->assertSame('b/b.php', $decoded['markers'][0]['slug'], "other slugs' entries must be left completely untouched");
        $this->assertSame('3.0', $decoded['markers'][0]['to_version']);
    }

    public function test_clear_slug_deletes_the_marker_file_entirely_once_the_last_entry_is_removed(): void
    {
        $tmp = sys_get_temp_dir();
        UpdateWatchdogMarker::arm('plugin', 'solo/solo.php', 'snap_solo', $tmp . '/wpmgr-watchdog-live-solo', $tmp . '/wpmgr-watchdog-payload-solo', '1.5');
        $this->assertFileExists($this->markerFile());

        UpdateWatchdogMarker::clearSlug('plugin', 'solo/solo.php');

        $this->assertFileDoesNotExist(
            $this->markerFile(),
            'clearing the only entry must delete the marker file, not leave an empty {"markers":[]} behind'
        );
    }

    public function test_clear_slug_is_a_silent_no_op_when_no_marker_file_exists(): void
    {
        UpdateWatchdogMarker::clearSlug('plugin', 'never-armed/never-armed.php');
        $this->assertFileDoesNotExist($this->markerFile());
    }

    public function test_disarm_healthy_clears_a_slug_that_is_active_and_on_disk_at_the_armed_version(): void
    {
        $tmp = sys_get_temp_dir();
        UpdateWatchdogMarker::arm('plugin', 'demo/demo.php', 'snap_demo', $tmp . '/wpmgr-watchdog-live-demo', $tmp . '/wpmgr-watchdog-payload-demo', '2.0');

        Functions\when('get_plugins')->justReturn(['demo/demo.php' => ['Version' => '2.0']]);
        Functions\when('is_plugin_active')->justReturn(true);

        UpdateWatchdogMarker::disarmHealthy();

        $this->assertFileDoesNotExist(
            $this->markerFile(),
            'a healthy boot with the slug active and at the armed version must clear that entry'
        );
    }

    public function test_disarm_healthy_leaves_a_slug_armed_when_the_on_disk_version_has_not_reached_the_armed_target(): void
    {
        $tmp = sys_get_temp_dir();
        UpdateWatchdogMarker::arm('plugin', 'demo/demo.php', 'snap_demo', $tmp . '/wpmgr-watchdog-live-demo2', $tmp . '/wpmgr-watchdog-payload-demo2', '2.0');

        // On-disk version is STILL the pre-update one — not yet at the armed target.
        Functions\when('get_plugins')->justReturn(['demo/demo.php' => ['Version' => '1.9']]);
        Functions\when('is_plugin_active')->justReturn(true);

        UpdateWatchdogMarker::disarmHealthy();

        $decoded = $this->readMarkerFile();
        $this->assertCount(1, $decoded['markers'], 'a slug NOT yet at the armed version must be left armed');
        $this->assertSame('demo/demo.php', $decoded['markers'][0]['slug']);
    }

    public function test_disarm_healthy_leaves_a_slug_armed_when_it_is_absent_from_get_plugins(): void
    {
        $tmp = sys_get_temp_dir();
        UpdateWatchdogMarker::arm('plugin', 'demo/demo.php', 'snap_demo', $tmp . '/wpmgr-watchdog-live-demo3', $tmp . '/wpmgr-watchdog-payload-demo3', '2.0');

        // The slug is simply not present at all.
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('is_plugin_active')->justReturn(true);

        UpdateWatchdogMarker::disarmHealthy();

        $decoded = $this->readMarkerFile();
        $this->assertCount(1, $decoded['markers'], 'a slug absent from get_plugins() must be left armed, not disarmed');
    }

    public function test_disarm_healthy_leaves_a_slug_armed_when_installed_at_the_right_version_but_inactive(): void
    {
        $tmp = sys_get_temp_dir();
        UpdateWatchdogMarker::arm('plugin', 'demo/demo.php', 'snap_demo', $tmp . '/wpmgr-watchdog-live-demo4', $tmp . '/wpmgr-watchdog-payload-demo4', '2.0');

        Functions\when('get_plugins')->justReturn(['demo/demo.php' => ['Version' => '2.0']]);
        Functions\when('is_plugin_active')->justReturn(false);
        Functions\when('is_plugin_active_for_network')->justReturn(false);

        UpdateWatchdogMarker::disarmHealthy();

        $decoded = $this->readMarkerFile();
        $this->assertCount(1, $decoded['markers'], 'an installed-but-INACTIVE plugin proves nothing about whether its code loads cleanly — must stay armed');
    }

    public function test_disarm_healthy_clears_only_the_healthy_slug_and_leaves_other_armed_slugs_entries_untouched(): void
    {
        $tmp = sys_get_temp_dir();
        UpdateWatchdogMarker::arm('plugin', 'healthy/healthy.php', 'snap_healthy', $tmp . '/wpmgr-watchdog-live-healthy', $tmp . '/wpmgr-watchdog-payload-healthy', '2.0');
        UpdateWatchdogMarker::arm('plugin', 'still-stale/still-stale.php', 'snap_stale', $tmp . '/wpmgr-watchdog-live-stale', $tmp . '/wpmgr-watchdog-payload-stale', '2.0');

        Functions\when('get_plugins')->justReturn([
            'healthy/healthy.php'         => ['Version' => '2.0'],
            'still-stale/still-stale.php' => ['Version' => '1.0'], // not yet at its armed target
        ]);
        Functions\when('is_plugin_active')->alias(static fn ($slug) => $slug === 'healthy/healthy.php');

        UpdateWatchdogMarker::disarmHealthy();

        $decoded = $this->readMarkerFile();
        $this->assertCount(1, $decoded['markers'], "other slugs' entries must be untouched — only the healthy one is cleared");
        $this->assertSame('still-stale/still-stale.php', $decoded['markers'][0]['slug']);
    }

    public function test_disarm_healthy_clears_a_theme_slug_that_is_the_active_stylesheet_at_the_armed_version(): void
    {
        $tmp = sys_get_temp_dir();
        UpdateWatchdogMarker::arm('theme', 'twentytwentyfour', 'snap_theme', $tmp . '/wpmgr-watchdog-live-theme', $tmp . '/wpmgr-watchdog-payload-theme', '1.2');

        $theme = new class {
            /** @param string $k Field. @return string */
            public function get($k): string
            {
                return $k === 'Version' ? '1.2' : '';
            }
        };
        Functions\when('wp_get_themes')->justReturn(['twentytwentyfour' => $theme]);
        Functions\when('get_stylesheet')->justReturn('twentytwentyfour');
        Functions\when('get_template')->justReturn('twentytwentyfour');

        UpdateWatchdogMarker::disarmHealthy();

        $this->assertFileDoesNotExist($this->markerFile(), 'the active theme at its armed version must be disarmed too');
    }

    public function test_disarm_healthy_is_a_cheap_no_op_when_no_marker_file_exists(): void
    {
        // No arm() call at all — the file must not exist, and disarmHealthy()
        // must not create one or throw.
        UpdateWatchdogMarker::disarmHealthy();
        $this->assertFileDoesNotExist($this->markerFile());
    }

    private function rrmdir(string $dir): void
    {
        if (!is_dir($dir)) {
            return;
        }
        $items = scandir($dir) ?: [];
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $path = $dir . '/' . $item;
            if (is_dir($path)) {
                $this->rrmdir($path);
            } else {
                unlink($path); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test fixture cleanup
            }
        }
        rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- test fixture cleanup
    }
}
