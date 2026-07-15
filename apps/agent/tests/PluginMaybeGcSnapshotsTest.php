<?php
/**
 * GitHub issue #226 — Plugin::maybeGcSnapshots() boot-wiring regression lock.
 *
 * Verifies the WP-Cron-independent GC trigger registerHooks() binds to
 * `plugins_loaded` actually reaches SnapshotManager::maybeGc() -> gcExpired()
 * when the site is enrolled, and is a cheap no-op (never touches the
 * snapshot store) otherwise — mirroring the isEnrolled() gate already used by
 * maybeRescheduleCron() / maybeDisarmUpdateWatchdog().
 *
 * Boots the REAL Plugin singleton with the same minimal stub set
 * PluginActivationTest uses for boot() (no master-key setup needed here —
 * this test never calls activate()).
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Plugin;
use WPMgr\Agent\Settings;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Plugin
 */
final class PluginMaybeGcSnapshotsTest extends TestCase
{
    /** @var array<string,mixed> In-memory wp-option store. */
    private array $options = [];

    /** Temp uploads dir standing in for wp_upload_dir()'s basedir. */
    private string $uploadsDir = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->uploadsDir = sys_get_temp_dir() . '/wpmgr-plugin-gc-' . bin2hex(random_bytes(6));
        mkdir($this->uploadsDir, 0755, true);

        $this->options = [];

        // Same minimal boot-stub set as PluginActivationTest::set_up() — see
        // its own doc for why each of these is needed for Plugin::boot() to
        // complete without throwing.
        foreach (['add_action', 'add_filter', 'register_activation_hook',
                  'register_deactivation_hook', 'wp_schedule_single_event',
                  'spawn_cron', 'wp_next_scheduled', 'wp_schedule_event',
                  'wp_get_scheduled_event', 'wp_clear_scheduled_hook'] as $fn) {
            Functions\when($fn)->justReturn(true);
        }
        Functions\when('is_admin')->justReturn(false);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('plugin_basename')->returnArg();

        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;
            return true;
        });
        Functions\when('get_option')->alias(function ($name, $default = false) {
            return $this->options[$name] ?? $default;
        });
        Functions\when('delete_option')->alias(function ($name) {
            unset($this->options[$name]);
            return true;
        });
        Functions\when('get_site_option')->alias(function ($name, $default = false) {
            return $this->options[$name] ?? $default;
        });
        Functions\when('update_site_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;
            return true;
        });

        Functions\when('wp_upload_dir')->justReturn(['basedir' => $this->uploadsDir]);
    }

    protected function tear_down(): void
    {
        // Reset the Plugin singleton so this test's constructed instance does
        // not leak into subsequent tests in the suite — same reasoning as
        // PluginActivationTest::tear_down().
        Plugin::resetForTesting();
        $this->rrmdir($this->uploadsDir);
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
                @unlink($path); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
            }
        }
        @rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- test-only fixture cleanup
    }

    /**
     * Seed an orphaned snapshot directory well past the 72h GC backstop TTL —
     * the exact class of orphan gcExpired() exists to reclaim.
     *
     * @return string Absolute path to the seeded snapshot directory.
     */
    private function seedOrphanSnapshot(): string
    {
        $snapshotsBase = $this->uploadsDir . '/wpmgr-snapshots';
        if (!is_dir($snapshotsBase)) {
            mkdir($snapshotsBase, 0755, true);
        }
        $dir = $snapshotsBase . '/snap_' . bin2hex(random_bytes(12));
        mkdir($dir, 0755, true);
        file_put_contents($dir . '/meta.json', (string) json_encode([
            'type'         => 'plugin',
            'slug'         => 'long-gone/long-gone.php',
            'from_version' => '1.0',
            'created_at'   => time() - (73 * 3600),
        ]));

        return $dir;
    }

    public function test_maybe_gc_snapshots_reclaims_an_orphan_via_snapshot_manager_maybe_gc_when_enrolled(): void
    {
        $this->options[Settings::OPTION_SITE_ID] = 'site-abc';
        $this->options[Settings::OPTION_CP_URL]  = 'https://cp.example.test';

        $orphan = $this->seedOrphanSnapshot();

        $plugin = Plugin::boot();
        $plugin->maybeGcSnapshots();

        $this->assertFalse(
            is_dir($orphan),
            'an enrolled site must reclaim an orphaned snapshot via the plugins_loaded-bound maybeGcSnapshots() -> SnapshotManager::maybeGc() -> gcExpired() chain'
        );
    }

    public function test_maybe_gc_snapshots_is_a_cheap_no_op_when_not_enrolled(): void
    {
        // Deliberately NOT enrolled: no site_id/cp_url set in the option store.
        $orphan = $this->seedOrphanSnapshot();

        $plugin = Plugin::boot();
        $plugin->maybeGcSnapshots();

        $this->assertTrue(
            is_dir($orphan),
            'a not-yet-enrolled site must never run the snapshot GC — maybeGcSnapshots() must be a cheap no-op'
        );
    }
}
