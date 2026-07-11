<?php
/**
 * UpdateWatchdogRestoreTest — GitHub issue #210 (update-watchdog mu-plugin).
 *
 * Exercises the REAL functions defined in
 * `mu-plugin-loader/a-wpmgr-update-watchdog.php` directly (never a
 * hand-copied replica — mirrors WafGateHardeningTest.php's own convention).
 *
 * Two groups of coverage:
 *   1. Restore behavior: given a fresh, unconsumed marker entry + a
 *      simulated fatal, the live dir is replaced by the snapshot payload and
 *      the entry is marked consumed (one-shot, via an atomic claim file).
 *      No-op when: no marker; marker expired; marker already consumed;
 *      error is null/non-fatal; a real apply is in flight for the same
 *      slug; the fatal's file cannot be attributed to any entry.
 *   2. Path-safety regression suite mirroring the GH #147 restore-anchoring
 *      discipline: exact-path/segment containment, no globbing, an escape
 *      attempt rejected, the running agent's own plugin dir refused as a
 *      restore target.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

// Guard: define the testing constant BEFORE requiring the mu-plugin so the
// top-level register_shutdown_function() call at the bottom of the file is
// skipped (mirrors a-wpmgr-waf.php's own WPMGR_WAF_TESTING convention).
if (!defined('WPMGR_WATCHDOG_TESTING')) {
    define('WPMGR_WATCHDOG_TESTING', true);
}

// Load the real mu-plugin — defines every wpmgr_watchdog_*() function.
// function_exists() guard makes this idempotent if another test already
// loaded it.
if (!function_exists('wpmgr_watchdog_path_is_within')) {
    require_once dirname(__DIR__) . '/mu-plugin-loader/a-wpmgr-update-watchdog.php';
}

/**
 * @covers wpmgr_watchdog_path_is_within
 * @covers wpmgr_watchdog_anchored_containment
 * @covers wpmgr_watchdog_slug_is_safe
 * @covers wpmgr_watchdog_expected_live_dir
 * @covers wpmgr_watchdog_expected_payload_dir
 * @covers wpmgr_watchdog_try_lock_inflight
 * @covers wpmgr_watchdog_storage_data_base
 * @covers wpmgr_watchdog_copy_dir
 * @covers wpmgr_watchdog_delete_dir
 * @covers wpmgr_watchdog_swap_directories
 * @covers wpmgr_watchdog_is_self_path
 * @covers wpmgr_watchdog_attempt_restore
 * @covers wpmgr_watchdog_process_fatal
 * @covers wpmgr_watchdog_is_resource_exhaustion_message
 */
final class UpdateWatchdogRestoreTest extends TestCase
{
    /** Temp root for this test run (removed in tear_down). */
    private string $root = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root = sys_get_temp_dir() . '/wpmgr-watchdog-test-' . bin2hex(random_bytes(6));
        mkdir($this->root, 0755, true);

        // The watchdog state dir (where the marker file AND one-shot claim
        // sentinels live) is normally created by UpdateWatchdogMarker::arm()
        // via StoragePaths::ensureHardenedPath(). Tests here call
        // wpmgr_watchdog_attempt_restore()/wpmgr_watchdog_process_fatal()
        // directly, bypassing arm() entirely, so it must exist up front for
        // fopen($claimFile, 'x') to succeed.
        $this->ensurePluginRootConstants();
        $stateDir = wpmgr_watchdog_state_dir();
        if ($stateDir !== '' && !is_dir($stateDir)) {
            mkdir($stateDir, 0755, true);
        }
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->root);
        $this->cleanSharedWatchdogStateDir();
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Several tests write directly into the SHARED (WP_CONTENT_DIR-based)
     * watchdog state directory — the marker file and/or one-shot claim
     * sentinels. Sweep both after every test so no leftover file from one
     * test can be mistaken for state by a later test in this same process
     * (each test uses its own randomly-generated snapshot ids, so this is
     * pure hygiene, not a correctness dependency).
     */
    private function cleanSharedWatchdogStateDir(): void
    {
        $dir = wpmgr_watchdog_state_dir();
        if ($dir === '' || !is_dir($dir)) {
            return;
        }
        $items = @scandir($dir) ?: [];
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            @unlink($dir . '/' . $item); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test fixture cleanup
        }
    }

    // =========================================================================
    // Fixtures / helpers
    // =========================================================================

    /**
     * Guard-define WP_CONTENT_DIR/WP_PLUGIN_DIR (may already be frozen by an
     * earlier test file in this same PHPUnit process — same idiom as
     * SnapshotManagerTest::ensurePluginRootConstants()).
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

    /**
     * Build a real on-disk plugin directory with a main file, a "loader.php"
     * (the file the simulated fatal is reported in), and a plain-text
     * `marker.txt` carrying $content — read back by content assertions so
     * they never have to account for PHP-file wrapper syntax.
     *
     * @return array{slug:string,live:string}
     */
    private function makeLivePlugin(string $content = 'BROKEN-UPDATE-CONTENT'): array
    {
        $pluginRoot = $this->ensurePluginRootConstants();
        $folder     = 'watchdog-plugin-' . bin2hex(random_bytes(6));
        $live       = $pluginRoot . '/' . $folder;
        mkdir($live, 0755, true);
        file_put_contents($live . '/' . $folder . '.php', "<?php\n// plugin main file\n");
        file_put_contents($live . '/loader.php', "<?php\n// loader\n");
        file_put_contents($live . '/marker.txt', $content);

        return ['slug' => $folder . '/' . $folder . '.php', 'live' => $live];
    }

    /**
     * Build a real on-disk snapshot payload directory (mirrors
     * SnapshotManager's own `<uploads>/wpmgr-snapshots/<id>/payload/` layout)
     * with one file, and point wp_upload_dir() at $uploadsDir for the
     * duration of the test.
     *
     * @return array{snapshot_id:string,payload:string}
     */
    private function makeSnapshotPayload(string $uploadsDir, string $content = 'GOOD-PRE-UPDATE-CONTENT'): array
    {
        $snapshotId = 'snap_' . bin2hex(random_bytes(12));
        $payload    = $uploadsDir . '/wpmgr-snapshots/' . $snapshotId . '/payload';
        mkdir($payload, 0755, true);
        file_put_contents($payload . '/marker.txt', $content);

        // Canonicalize (e.g. macOS resolves /var -> /private/var): a real
        // marker's payload_dir is always the realpath()'d value produced by
        // SnapshotManager::resolveSnapshotDir() (via
        // resolvedRestorePaths()) — tests that hand-construct a marker
        // entry must match that exactly, since
        // wpmgr_watchdog_expected_payload_dir() independently re-derives
        // and realpath()'s it too.
        return ['snapshot_id' => $snapshotId, 'payload' => realpath($payload)];
    }

    /**
     * @param array<string,mixed> $overrides
     * @return array<string,mixed>
     */
    private function markerEntry(string $type, string $live, string $snapshotId, string $payload, array $overrides = []): array
    {
        $now = time();

        return array_merge([
            'type'        => $type,
            'slug'        => self::relativeSlugFor($live),
            'snapshot_id' => $snapshotId,
            'live_dir'    => $live,
            'payload_dir' => $payload,
            'applied_at'  => $now,
            'expires_at'  => $now + 300,
        ], $overrides);
    }

    /** Derive the plugin slug ("folder/folder.php") a live dir under WP_PLUGIN_DIR corresponds to. */
    private static function relativeSlugFor(string $live): string
    {
        $folder = basename($live);

        return $folder . '/' . $folder . '.php';
    }

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
                @unlink($path); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test fixture cleanup
            }
        }
        @rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- test fixture cleanup
    }

    // =========================================================================
    // wpmgr_watchdog_path_is_within() — anchored containment (GH #147 style)
    // =========================================================================

    public function test_path_is_within_matches_the_exact_directory_and_nested_files(): void
    {
        $this->assertTrue(wpmgr_watchdog_path_is_within('/a/b', '/a/b'));
        $this->assertTrue(wpmgr_watchdog_path_is_within('/a/b/c.php', '/a/b'));
        $this->assertTrue(wpmgr_watchdog_path_is_within('/a/b/c/d.php', '/a/b'));
    }

    public function test_path_is_within_rejects_a_sibling_with_a_shared_string_prefix(): void
    {
        // GH #147 discipline: /a/bc must NOT be considered "within" /a/b
        // merely because it shares a string prefix — this is the exact class
        // of bug an unanchored strpos() check introduced.
        $this->assertFalse(wpmgr_watchdog_path_is_within('/a/bc/evil.php', '/a/b'));
        $this->assertFalse(wpmgr_watchdog_path_is_within('/a/bcd.php', '/a/b'));
    }

    public function test_path_is_within_normalizes_backslashes(): void
    {
        $this->assertTrue(wpmgr_watchdog_path_is_within('C:\\a\\b\\c.php', 'C:\\a\\b'));
    }

    public function test_path_is_within_rejects_empty_inputs(): void
    {
        $this->assertFalse(wpmgr_watchdog_path_is_within('', '/a/b'));
        $this->assertFalse(wpmgr_watchdog_path_is_within('/a/b/c.php', ''));
    }

    // =========================================================================
    // wpmgr_watchdog_anchored_containment()
    // =========================================================================

    public function test_anchored_containment_accepts_root_and_descendants(): void
    {
        $this->assertTrue(wpmgr_watchdog_anchored_containment('/plugins', '/plugins'));
        $this->assertTrue(wpmgr_watchdog_anchored_containment('/plugins', '/plugins/demo'));
        $this->assertTrue(wpmgr_watchdog_anchored_containment('/plugins', '/plugins/demo/sub/file.php'));
    }

    public function test_anchored_containment_rejects_traversal_segments(): void
    {
        $this->assertFalse(wpmgr_watchdog_anchored_containment('/plugins', '/plugins/../evil'));
        $this->assertFalse(wpmgr_watchdog_anchored_containment('/plugins', '/plugins/demo/../../evil'));
    }

    public function test_anchored_containment_rejects_a_sibling_with_a_shared_string_prefix(): void
    {
        $this->assertFalse(wpmgr_watchdog_anchored_containment('/plugins', '/plugins-evil/x'));
    }

    // =========================================================================
    // wpmgr_watchdog_slug_is_safe()
    // =========================================================================

    public function test_slug_is_safe_accepts_valid_slugs(): void
    {
        $this->assertTrue(wpmgr_watchdog_slug_is_safe('akismet'));
        $this->assertTrue(wpmgr_watchdog_slug_is_safe('akismet/akismet.php'));
        $this->assertTrue(wpmgr_watchdog_slug_is_safe('woo-commerce'));
    }

    /**
     * @dataProvider unsafeSlugs
     */
    public function test_slug_is_safe_rejects_traversal_and_malformed_slugs(string $slug): void
    {
        $this->assertFalse(wpmgr_watchdog_slug_is_safe($slug));
    }

    /**
     * @return array<int,array{0:string}>
     */
    public static function unsafeSlugs(): array
    {
        return [
            [''],
            ['../evil'],
            ['../../wp-config.php'],
            ['/etc/passwd'],
            ['foo/../../bar'],
            ['C:\\Windows'],
            ['..'],
            ['foo/bar/baz'],
            ["foo\0bar"],
        ];
    }

    public function test_slug_is_safe_accepts_a_bare_dot_the_same_way_upstream_sanitize_slug_does(): void
    {
        // A bare "." satisfies UpdateCommand::sanitizeSlug()'s character-class
        // regex too — this mu-plugin does NOT rely on slug_is_safe() alone to
        // catch it; wpmgr_watchdog_expected_live_dir() has its OWN additional
        // guard for exactly this case (see the next test group).
        $this->assertTrue(wpmgr_watchdog_slug_is_safe('.'));
    }

    // =========================================================================
    // wpmgr_watchdog_expected_live_dir() — mirrors SnapshotManager::liveDir()
    // =========================================================================

    public function test_expected_live_dir_resolves_a_real_plugin_directory(): void
    {
        $plugin = $this->makeLivePlugin();

        $result = wpmgr_watchdog_expected_live_dir('plugin', $plugin['slug']);

        $this->assertSame($plugin['live'], $result);
    }

    public function test_expected_live_dir_resolves_a_real_theme_directory(): void
    {
        $pluginRoot = $this->ensurePluginRootConstants();
        $themeRoot  = rtrim((string) WP_CONTENT_DIR, '/\\') . '/themes';
        if (!is_dir($themeRoot)) {
            mkdir($themeRoot, 0755, true);
        }
        $slug   = 'watchdog-theme-' . bin2hex(random_bytes(6));
        $folder = $themeRoot . '/' . $slug;
        mkdir($folder, 0755, true);
        file_put_contents($folder . '/style.css', "/*\nTheme Name: Watchdog Test\n*/\n");

        $result = wpmgr_watchdog_expected_live_dir('theme', $slug);

        $this->assertSame($folder, $result);
    }

    public function test_expected_live_dir_rejects_a_dot_folder_segment_even_though_the_slug_regex_allows_it(): void
    {
        $this->ensurePluginRootConstants();

        // A lone "." would otherwise collapse to WP_PLUGIN_DIR itself via
        // dirname()/realpath() — see this file's own doc for why this extra
        // guard exists beyond what SnapshotManager::liveDir() checks.
        $this->assertSame('', wpmgr_watchdog_expected_live_dir('plugin', '.'));
        $this->assertSame('', wpmgr_watchdog_expected_live_dir('theme', '.'));
    }

    public function test_expected_live_dir_still_rejects_a_path_escape_even_when_realpath_containment_fails(): void
    {
        // Mirrors SnapshotManagerTest's own
        // test_live_dir_still_rejects_a_path_escape_even_when_realpath_containment_fails.
        $this->ensurePluginRootConstants();
        Functions\when('realpath')->justReturn(false);

        $this->assertSame('', wpmgr_watchdog_expected_live_dir('theme', '../../etc/passwd'));
        $this->assertSame('', wpmgr_watchdog_expected_live_dir('plugin', '../escape/escape.php'));
    }

    public function test_expected_live_dir_returns_empty_for_an_unknown_type(): void
    {
        $this->assertSame('', wpmgr_watchdog_expected_live_dir('core', 'core'));
        $this->assertSame('', wpmgr_watchdog_expected_live_dir('bogus', 'x'));
    }

    public function test_expected_live_dir_returns_empty_for_a_source_that_does_not_exist_even_with_the_fallback(): void
    {
        $this->ensurePluginRootConstants();
        Functions\when('realpath')->justReturn(false);

        $this->assertSame(
            '',
            wpmgr_watchdog_expected_live_dir('plugin', 'never-installed-' . bin2hex(random_bytes(6)))
        );
    }

    // =========================================================================
    // wpmgr_watchdog_expected_payload_dir()
    // =========================================================================

    public function test_expected_payload_dir_resolves_a_real_snapshot_payload(): void
    {
        $uploadsDir = $this->root . '/uploads';
        mkdir($uploadsDir, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploadsDir]);

        $snap = $this->makeSnapshotPayload($uploadsDir);

        // wpmgr_watchdog_expected_payload_dir() returns a realpath()'d value
        // (e.g. macOS resolves /var -> /private/var) — compare through
        // realpath() on both sides rather than asserting exact string
        // identity against a manually-concatenated expectation.
        $this->assertSame(
            realpath(dirname($snap['payload'])) . '/payload',
            wpmgr_watchdog_expected_payload_dir($snap['snapshot_id'])
        );
    }

    public function test_expected_payload_dir_rejects_a_malformed_snapshot_id(): void
    {
        $uploadsDir = $this->root . '/uploads';
        mkdir($uploadsDir, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploadsDir]);

        $this->assertSame('', wpmgr_watchdog_expected_payload_dir('../../etc/passwd'));
        $this->assertSame('', wpmgr_watchdog_expected_payload_dir('snap_ok/../../escape'));
        $this->assertSame('', wpmgr_watchdog_expected_payload_dir(''));
    }

    public function test_expected_payload_dir_returns_empty_when_the_payload_subdir_is_missing(): void
    {
        $uploadsDir = $this->root . '/uploads';
        mkdir($uploadsDir, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploadsDir]);

        $snapshotId = 'snap_' . bin2hex(random_bytes(12));
        // Snapshot dir exists but has no payload/ subdir (e.g. a core-only
        // meta snapshot).
        mkdir($uploadsDir . '/wpmgr-snapshots/' . $snapshotId, 0755, true);

        $this->assertSame('', wpmgr_watchdog_expected_payload_dir($snapshotId));
    }

    // =========================================================================
    // wpmgr_watchdog_try_lock_inflight()
    // =========================================================================

    public function test_try_lock_inflight_succeeds_when_no_apply_is_in_progress(): void
    {
        $uploadsDir = $this->root . '/uploads';
        mkdir($uploadsDir . '/wpmgr-update-inflight', 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploadsDir]);

        $handle = wpmgr_watchdog_try_lock_inflight('plugin', 'demo/demo.php');

        $this->assertIsResource($handle);
        wpmgr_watchdog_unlock_inflight($handle);
    }

    public function test_try_lock_inflight_stands_down_when_a_real_apply_holds_the_lock(): void
    {
        $uploadsDir = $this->root . '/uploads';
        $dir        = $uploadsDir . '/wpmgr-update-inflight';
        mkdir($dir, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploadsDir]);

        $key  = substr(hash('sha256', 'plugin:demo/demo.php'), 0, 32);
        $lockFile = $dir . '/' . $key . '.lock';
        $externalHandle = fopen($lockFile, 'c');
        $this->assertIsResource($externalHandle);
        $this->assertTrue(flock($externalHandle, LOCK_EX | LOCK_NB), 'precondition: the external handle must hold the lock');

        $result = wpmgr_watchdog_try_lock_inflight('plugin', 'demo/demo.php');

        $this->assertFalse($result, 'a genuinely in-progress apply must make the watchdog stand down');

        flock($externalHandle, LOCK_UN);
        fclose($externalHandle);
    }

    /**
     * LOW-1 (GitHub issue #210 security review) — when wp_upload_dir()
     * returns no usable basedir, the in-flight lock path MUST fall back to
     * `WP_CONTENT_DIR/wpmgr-update-inflight/...` (mirroring
     * StoragePaths::dataBase()'s own fallback, which UpdateInFlight itself
     * uses) — NOT `WP_CONTENT_DIR/uploads/wpmgr-update-inflight/...` (the
     * DIFFERENT fallback wpmgr_watchdog_uploads_base()/SnapshotManager use
     * for the snapshot-payload side). Before this fix the two diverged in
     * exactly this scenario, so a lock UpdateInFlight::mark() genuinely
     * held at the correct (no-/uploads) path was invisible to the watchdog.
     */
    public function test_try_lock_inflight_uses_the_storagepaths_fallback_when_wp_upload_dir_returns_no_basedir(): void
    {
        $this->ensurePluginRootConstants(); // guard-defines WP_CONTENT_DIR if not already set.
        Functions\when('wp_upload_dir')->justReturn([]); // No usable 'basedir' key at all.

        $expectedDir = rtrim((string) WP_CONTENT_DIR, '/\\') . '/wpmgr-update-inflight';
        if (!is_dir($expectedDir)) {
            mkdir($expectedDir, 0755, true);
        }

        // A lock genuinely held at the CORRECT (StoragePaths::dataBase()-style,
        // no "/uploads") fallback path — exactly where UpdateInFlight::mark()
        // itself would create it in this same scenario.
        $key      = substr(hash('sha256', 'plugin:fallback-demo/fallback-demo.php'), 0, 32);
        $lockFile = $expectedDir . '/' . $key . '.lock';
        $externalHandle = fopen($lockFile, 'c');
        $this->assertIsResource($externalHandle);
        $this->assertTrue(flock($externalHandle, LOCK_EX | LOCK_NB), 'precondition: the external handle must hold the lock at the StoragePaths-style fallback path');

        $result = wpmgr_watchdog_try_lock_inflight('plugin', 'fallback-demo/fallback-demo.php');

        $this->assertFalse(
            $result,
            'LOW-1: in the wp_upload_dir()-unavailable fallback, the watchdog must resolve the SAME lock path StoragePaths::dataBase()/UpdateInFlight use and correctly detect the held lock'
        );

        flock($externalHandle, LOCK_UN);
        fclose($externalHandle);
        unlink($lockFile); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test fixture cleanup
    }

    // =========================================================================
    // wpmgr_watchdog_copy_dir() / wpmgr_watchdog_delete_dir()
    // =========================================================================

    public function test_copy_dir_recursively_copies_files_and_subdirectories(): void
    {
        $src = $this->root . '/copy-src';
        $dst = $this->root . '/copy-dst';
        mkdir($src . '/nested', 0755, true);
        file_put_contents($src . '/top.txt', 'top');
        file_put_contents($src . '/nested/deep.txt', 'deep');

        $this->assertTrue(wpmgr_watchdog_copy_dir($src, $dst));
        $this->assertSame('top', file_get_contents($dst . '/top.txt'));
        $this->assertSame('deep', file_get_contents($dst . '/nested/deep.txt'));
    }

    public function test_copy_dir_skips_symlinks(): void
    {
        $src = $this->root . '/copy-src-sym';
        $dst = $this->root . '/copy-dst-sym';
        mkdir($src, 0755, true);
        file_put_contents($this->root . '/outside.txt', 'outside');
        file_put_contents($src . '/real.txt', 'real');
        @symlink($this->root . '/outside.txt', $src . '/link.txt');

        $this->assertTrue(wpmgr_watchdog_copy_dir($src, $dst));
        $this->assertSame('real', file_get_contents($dst . '/real.txt'));
        $this->assertFalse(file_exists($dst . '/link.txt'), 'a symlink entry must never be followed/copied');
    }

    public function test_delete_dir_recursively_removes_everything(): void
    {
        $dir = $this->root . '/delete-me';
        mkdir($dir . '/nested', 0755, true);
        file_put_contents($dir . '/top.txt', 'top');
        file_put_contents($dir . '/nested/deep.txt', 'deep');

        $this->assertTrue(wpmgr_watchdog_delete_dir($dir));
        $this->assertFalse(is_dir($dir));
    }

    // =========================================================================
    // wpmgr_watchdog_swap_directories() — the core swap, mirrors
    // SnapshotManager::restore()'s rename-based staging + S5 rollback.
    // =========================================================================

    public function test_swap_directories_replaces_live_with_payload_on_the_happy_path(): void
    {
        $live = $this->root . '/live-demo';
        mkdir($live, 0755, true);
        file_put_contents($live . '/marker.txt', 'BROKEN');

        $payload = $this->root . '/payload-demo';
        mkdir($payload, 0755, true);
        file_put_contents($payload . '/marker.txt', 'GOOD');

        wpmgr_watchdog_swap_directories($live, $payload, 'snap_test');

        $this->assertSame('GOOD', file_get_contents($live . '/marker.txt'));
        $this->assertFalse(is_dir($live . '.wpmgr-watchdog-old-snap_test'), 'the aside must be dropped on success');
    }

    public function test_swap_directories_does_nothing_when_the_aside_already_exists(): void
    {
        $live = $this->root . '/live-demo2';
        mkdir($live, 0755, true);
        file_put_contents($live . '/marker.txt', 'CURRENT');

        $payload = $this->root . '/payload-demo2';
        mkdir($payload, 0755, true);
        file_put_contents($payload . '/marker.txt', 'GOOD');

        $aside = $live . '.wpmgr-watchdog-old-snap_test2';
        mkdir($aside, 0755, true);

        wpmgr_watchdog_swap_directories($live, $payload, 'snap_test2');

        $this->assertSame('CURRENT', file_get_contents($live . '/marker.txt'), 'an unexpected pre-existing aside must leave live untouched');
    }

    public function test_swap_directories_rolls_back_when_the_copy_fails_mid_write(): void
    {
        $live = $this->root . '/live-demo3';
        mkdir($live, 0755, true);
        file_put_contents($live . '/marker.txt', 'PRE-RESTORE-ATTEMPT');

        // Payload directory does not exist at all -> wpmgr_watchdog_copy_dir()
        // fails immediately (is_dir($src) check).
        $payload = $this->root . '/payload-missing';

        wpmgr_watchdog_swap_directories($live, $payload, 'snap_test3');

        $this->assertTrue(is_dir($live), 'the live directory must exist again — never left missing');
        $this->assertSame(
            'PRE-RESTORE-ATTEMPT',
            file_get_contents($live . '/marker.txt'),
            'a failed copy must roll back to the pre-restore-attempt state'
        );
        $this->assertFalse(is_dir($live . '.wpmgr-watchdog-old-snap_test3'));
    }

    // =========================================================================
    // wpmgr_watchdog_attempt_restore() — full integration
    // =========================================================================

    public function test_attempt_restore_performs_the_swap_for_a_fresh_unconsumed_entry(): void
    {
        $uploadsDir = $this->root . '/uploads';
        mkdir($uploadsDir, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploadsDir]);
        mkdir($uploadsDir . '/wpmgr-update-inflight', 0755, true);

        $plugin = $this->makeLivePlugin('BROKEN-UPDATE-CONTENT');
        $snap   = $this->makeSnapshotPayload($uploadsDir);

        $entry = $this->markerEntry('plugin', $plugin['live'], $snap['snapshot_id'], $snap['payload']);
        $entry['slug'] = $plugin['slug'];

        wpmgr_watchdog_attempt_restore($entry, time());

        $this->assertSame(
            'GOOD-PRE-UPDATE-CONTENT',
            file_get_contents($plugin['live'] . '/marker.txt'),
            'a fresh, unconsumed, valid entry must actually restore the snapshot payload over the live dir'
        );
    }

    public function test_attempt_restore_is_one_shot_a_second_call_for_the_same_snapshot_does_nothing(): void
    {
        $uploadsDir = $this->root . '/uploads';
        mkdir($uploadsDir, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploadsDir]);
        mkdir($uploadsDir . '/wpmgr-update-inflight', 0755, true);

        $plugin = $this->makeLivePlugin('BROKEN-UPDATE-CONTENT');
        $snap   = $this->makeSnapshotPayload($uploadsDir);

        $entry = $this->markerEntry('plugin', $plugin['live'], $snap['snapshot_id'], $snap['payload']);
        $entry['slug'] = $plugin['slug'];

        wpmgr_watchdog_attempt_restore($entry, time());
        $this->assertSame('GOOD-PRE-UPDATE-CONTENT', file_get_contents($plugin['live'] . '/marker.txt'));

        // Simulate a second, concurrently-fataling request re-processing the
        // SAME marker entry — must be a complete no-op (the claim file from
        // the first call already exists).
        file_put_contents($plugin['live'] . '/marker.txt', 'MUTATED-AFTER-FIRST-RESTORE');
        wpmgr_watchdog_attempt_restore($entry, time());

        $this->assertSame(
            'MUTATED-AFTER-FIRST-RESTORE',
            file_get_contents($plugin['live'] . '/marker.txt'),
            'a SECOND attempt for the exact same snapshot must never restore again — one-shot'
        );
    }

    public function test_attempt_restore_does_nothing_for_an_expired_entry(): void
    {
        $uploadsDir = $this->root . '/uploads';
        mkdir($uploadsDir, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploadsDir]);
        mkdir($uploadsDir . '/wpmgr-update-inflight', 0755, true);

        $plugin = $this->makeLivePlugin('BROKEN-UPDATE-CONTENT');
        $snap   = $this->makeSnapshotPayload($uploadsDir);

        $now   = time();
        $entry = $this->markerEntry('plugin', $plugin['live'], $snap['snapshot_id'], $snap['payload'], [
            'applied_at' => $now - 1000,
            'expires_at' => $now - 700,
        ]);
        $entry['slug'] = $plugin['slug'];

        wpmgr_watchdog_attempt_restore($entry, $now);

        $this->assertSame('BROKEN-UPDATE-CONTENT', file_get_contents($plugin['live'] . '/marker.txt'));
    }

    public function test_attempt_restore_does_nothing_when_the_recorded_live_dir_does_not_match_the_independently_derived_expectation(): void
    {
        // Simulates a tampered/corrupt marker: the recorded live_dir is some
        // OTHER absolute path entirely, not the one the slug independently
        // resolves to. Must be rejected outright, never trusted.
        $uploadsDir = $this->root . '/uploads';
        mkdir($uploadsDir, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploadsDir]);
        mkdir($uploadsDir . '/wpmgr-update-inflight', 0755, true);

        $plugin  = $this->makeLivePlugin('BROKEN-UPDATE-CONTENT');
        $snap    = $this->makeSnapshotPayload($uploadsDir);
        $decoyLive = $this->root . '/totally-unrelated-dir';
        mkdir($decoyLive, 0755, true);
        file_put_contents($decoyLive . '/should-not-be-touched.txt', 'DECOY');

        $entry = $this->markerEntry('plugin', $decoyLive, $snap['snapshot_id'], $snap['payload']);
        $entry['slug'] = $plugin['slug']; // slug resolves to $plugin['live'], NOT $decoyLive.

        wpmgr_watchdog_attempt_restore($entry, time());

        $this->assertSame('DECOY', file_get_contents($decoyLive . '/should-not-be-touched.txt'), 'the decoy dir recorded in the marker must never be touched');
        $this->assertSame(
            'BROKEN-UPDATE-CONTENT',
            file_get_contents($plugin['live'] . '/marker.txt'),
            'the slug\'s REAL live dir must also be left untouched — the mismatch aborts the whole restore'
        );
    }

    public function test_is_self_path_matches_only_the_running_agents_own_plugin_directory(): void
    {
        $this->assertTrue(defined('WPMGR_AGENT_DIR'), 'precondition: WPMGR_AGENT_DIR must be defined by the test bootstrap');
        $selfDir = rtrim((string) constant('WPMGR_AGENT_DIR'), '/\\');

        $this->assertTrue(wpmgr_watchdog_is_self_path($selfDir));
        $this->assertFalse(wpmgr_watchdog_is_self_path($selfDir . '-lookalike'), 'a sibling with a shared string prefix must not match (anchored, not substring)');
        $this->assertFalse(wpmgr_watchdog_is_self_path('/some/totally/unrelated/dir'));
        $this->assertFalse(wpmgr_watchdog_is_self_path(''));
    }

    public function test_attempt_restore_stands_down_when_the_self_path_guard_fires(): void
    {
        // Integration-level proof that wpmgr_watchdog_attempt_restore()
        // actually consults wpmgr_watchdog_is_self_path() for every
        // candidate restore — forces the guard true via the same
        // Brain-Monkey/Patchwork interception WafGateHardeningTest already
        // relies on for calls made from this namespace-free mu-plugin file.
        $uploadsDir = $this->root . '/uploads';
        mkdir($uploadsDir, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploadsDir]);
        mkdir($uploadsDir . '/wpmgr-update-inflight', 0755, true);

        $plugin = $this->makeLivePlugin('BROKEN-UPDATE-CONTENT');
        $snap   = $this->makeSnapshotPayload($uploadsDir);

        $entry = $this->markerEntry('plugin', $plugin['live'], $snap['snapshot_id'], $snap['payload']);
        $entry['slug'] = $plugin['slug'];

        Functions\when('wpmgr_watchdog_is_self_path')->justReturn(true);

        wpmgr_watchdog_attempt_restore($entry, time());

        $this->assertSame(
            'BROKEN-UPDATE-CONTENT',
            file_get_contents($plugin['live'] . '/marker.txt'),
            'when the self-path guard fires, the restore must be a complete no-op, even for an otherwise fully valid entry'
        );
    }

    public function test_attempt_restore_stands_down_when_a_real_apply_is_in_flight_for_the_same_slug(): void
    {
        $uploadsDir = $this->root . '/uploads';
        $inflightDir = $uploadsDir . '/wpmgr-update-inflight';
        mkdir($inflightDir, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploadsDir]);

        $plugin = $this->makeLivePlugin('BROKEN-UPDATE-CONTENT');
        $snap   = $this->makeSnapshotPayload($uploadsDir);

        $entry = $this->markerEntry('plugin', $plugin['live'], $snap['snapshot_id'], $snap['payload']);
        $entry['slug'] = $plugin['slug'];

        // Hold the SAME in-flight lock UpdateInFlight::mark() would hold for
        // a real, currently-running apply of this exact slug.
        $key  = substr(hash('sha256', 'plugin:' . $plugin['slug']), 0, 32);
        $lockFile = $inflightDir . '/' . $key . '.lock';
        $externalHandle = fopen($lockFile, 'c');
        $this->assertTrue(flock($externalHandle, LOCK_EX | LOCK_NB));

        wpmgr_watchdog_attempt_restore($entry, time());

        $this->assertSame(
            'BROKEN-UPDATE-CONTENT',
            file_get_contents($plugin['live'] . '/marker.txt'),
            'a genuinely in-progress apply for the same slug must never be raced by the watchdog'
        );

        flock($externalHandle, LOCK_UN);
        fclose($externalHandle);
    }

    // =========================================================================
    // wpmgr_watchdog_process_fatal() — the shutdown-callback core
    // =========================================================================

    public function test_process_fatal_does_nothing_for_a_null_error(): void
    {
        $plugin = $this->makeLivePlugin();
        wpmgr_watchdog_process_fatal(null);
        $this->assertSame('BROKEN-UPDATE-CONTENT', file_get_contents($plugin['live'] . '/marker.txt'));
    }

    /**
     * @dataProvider nonFatalErrorCodes
     */
    public function test_process_fatal_does_nothing_for_a_non_fatal_error_code(int $code): void
    {
        $result = wpmgr_watchdog_process_fatal(['type' => $code, 'message' => 'x', 'file' => '/tmp/x.php', 'line' => 1]);
        $this->assertNull($result);
    }

    /**
     * @return array<int,array{0:int}>
     */
    public static function nonFatalErrorCodes(): array
    {
        return [
            [E_WARNING],
            [E_NOTICE],
            [E_USER_WARNING],
            [E_USER_NOTICE],
            [E_DEPRECATED],
            [E_USER_ERROR],        // Deliberately narrower mask than error-trap's.
            [E_RECOVERABLE_ERROR],
        ];
    }

    public function test_process_fatal_restores_the_entry_whose_live_dir_contains_the_fatals_file(): void
    {
        $uploadsDir = $this->root . '/uploads';
        mkdir($uploadsDir, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploadsDir]);
        mkdir($uploadsDir . '/wpmgr-update-inflight', 0755, true);

        $broken = $this->makeLivePlugin('BROKEN-UPDATE-CONTENT');
        $healthy = $this->makeLivePlugin('HEALTHY-CONTENT');
        $snapBroken  = $this->makeSnapshotPayload($uploadsDir, 'GOOD-BROKEN-PLUGIN-CONTENT');
        $snapHealthy = $this->makeSnapshotPayload($uploadsDir, 'GOOD-HEALTHY-PLUGIN-CONTENT');

        $stateDir = wpmgr_watchdog_state_dir();
        if (!is_dir($stateDir)) {
            mkdir($stateDir, 0755, true);
        }
        file_put_contents($stateDir . '/watchdog-marker.json', (string) json_encode([
            'markers' => [
                $this->markerEntry('plugin', $broken['live'], $snapBroken['snapshot_id'], $snapBroken['payload'], ['slug' => $broken['slug']]),
                $this->markerEntry('plugin', $healthy['live'], $snapHealthy['snapshot_id'], $snapHealthy['payload'], ['slug' => $healthy['slug']]),
            ],
        ]));

        // The fatal happened inside the BROKEN plugin's own loader file.
        $err = [
            'type'    => E_ERROR,
            'message' => "Uncaught Error: Class 'Foo' not found",
            'file'    => $broken['live'] . '/loader.php',
            'line'    => 3,
        ];

        wpmgr_watchdog_process_fatal($err);

        $this->assertSame(
            'GOOD-BROKEN-PLUGIN-CONTENT',
            file_get_contents($broken['live'] . '/marker.txt'),
            'the broken plugin (whose file the fatal was reported in) must be restored'
        );
        $this->assertSame(
            'HEALTHY-CONTENT',
            file_get_contents($healthy['live'] . '/marker.txt'),
            'a healthy sibling from the SAME batch must never be touched — precise attribution, not blast-radius restore-everything'
        );

        $this->rrmdir($stateDir);
    }

    // =========================================================================
    // MEDIUM-1a (GitHub issue #210 security review) — resource-exhaustion
    // fatals (OOM / execution-timeout) are excluded from attribution.
    // =========================================================================

    /**
     * @dataProvider resourceExhaustionMessages
     */
    public function test_is_resource_exhaustion_message_matches_oom_and_timeout(string $message): void
    {
        $this->assertTrue(wpmgr_watchdog_is_resource_exhaustion_message($message));
    }

    /**
     * @return array<int,array{0:string}>
     */
    public static function resourceExhaustionMessages(): array
    {
        return [
            ['Allowed memory size of 134217728 bytes exhausted (tried to allocate 20971520 bytes)'],
            ['allowed memory size of 268435456 bytes exhausted'], // case-insensitive
            ['Maximum execution time of 30 seconds exceeded'],
            ['maximum execution time of 60 seconds exceeded'], // case-insensitive
        ];
    }

    public function test_is_resource_exhaustion_message_rejects_a_genuine_code_fatal(): void
    {
        $this->assertFalse(wpmgr_watchdog_is_resource_exhaustion_message("Uncaught Error: Class 'Foo' not found"));
        $this->assertFalse(wpmgr_watchdog_is_resource_exhaustion_message(''));
    }

    public function test_process_fatal_does_not_restore_on_an_allowed_memory_size_fatal_attributable_to_the_armed_slug(): void
    {
        $uploadsDir = $this->root . '/uploads';
        mkdir($uploadsDir, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploadsDir]);
        mkdir($uploadsDir . '/wpmgr-update-inflight', 0755, true);

        $plugin = $this->makeLivePlugin('BROKEN-UPDATE-CONTENT');
        $snap   = $this->makeSnapshotPayload($uploadsDir);

        $stateDir = wpmgr_watchdog_state_dir();
        if (!is_dir($stateDir)) {
            mkdir($stateDir, 0755, true);
        }
        file_put_contents($stateDir . '/watchdog-marker.json', (string) json_encode([
            'markers' => [$this->markerEntry('plugin', $plugin['live'], $snap['snapshot_id'], $snap['payload'], ['slug' => $plugin['slug']])],
        ]));

        // The fatal's file DOES fall within the armed slug's live_dir — the
        // ONLY reason this must not restore is the OOM message itself.
        $err = [
            'type'    => E_ERROR,
            'message' => 'Allowed memory size of 134217728 bytes exhausted (tried to allocate 20971520 bytes)',
            'file'    => $plugin['live'] . '/loader.php',
            'line'    => 42,
        ];

        wpmgr_watchdog_process_fatal($err);

        $this->assertSame(
            'BROKEN-UPDATE-CONTENT',
            file_get_contents($plugin['live'] . '/marker.txt'),
            'MEDIUM-1a: an OOM fatal must never trigger a restore, even when it is otherwise attributable to the armed slug'
        );

        $this->rrmdir($stateDir);
    }

    public function test_process_fatal_does_nothing_when_the_fatals_file_cannot_be_attributed_to_any_entry(): void
    {
        $uploadsDir = $this->root . '/uploads';
        mkdir($uploadsDir, 0755, true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $uploadsDir]);

        $plugin = $this->makeLivePlugin('BROKEN-UPDATE-CONTENT');
        $snap   = $this->makeSnapshotPayload($uploadsDir);

        $stateDir = wpmgr_watchdog_state_dir();
        if (!is_dir($stateDir)) {
            mkdir($stateDir, 0755, true);
        }
        file_put_contents($stateDir . '/watchdog-marker.json', (string) json_encode([
            'markers' => [$this->markerEntry('plugin', $plugin['live'], $snap['snapshot_id'], $snap['payload'], ['slug' => $plugin['slug']])],
        ]));

        // The fatal is reported inside some UNRELATED file, not under any
        // recorded live_dir — ambiguous, must not guess.
        $err = [
            'type'    => E_ERROR,
            'message' => 'unrelated fatal',
            'file'    => $this->root . '/somewhere-else/core-file.php',
            'line'    => 1,
        ];

        wpmgr_watchdog_process_fatal($err);

        $this->assertSame(
            'BROKEN-UPDATE-CONTENT',
            file_get_contents($plugin['live'] . '/marker.txt'),
            'an unattributable fatal must never trigger a guessed restore'
        );

        $this->rrmdir($stateDir);
    }

    public function test_process_fatal_does_nothing_when_no_marker_file_exists(): void
    {
        $stateDir = wpmgr_watchdog_state_dir();
        if (is_dir($stateDir)) {
            $this->rrmdir($stateDir);
        }

        // Must not throw even though the state dir/marker do not exist.
        wpmgr_watchdog_process_fatal(['type' => E_ERROR, 'message' => 'x', 'file' => '/tmp/x.php', 'line' => 1]);
        $this->assertTrue(true, 'no exception was thrown');
    }

    public function test_shutdown_check_never_throws_even_with_no_environment_set_up(): void
    {
        // wpmgr_watchdog_shutdown_check() calls the REAL error_get_last();
        // in a clean PHPUnit run that is typically null/empty. This test's
        // only assertion is that calling it is always safe.
        wpmgr_watchdog_shutdown_check();
        $this->assertTrue(true, 'no exception was thrown');
    }
}
