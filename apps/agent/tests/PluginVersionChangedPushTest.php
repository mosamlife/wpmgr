<?php
/**
 * The CONFIRM beat of the CP-commanded agent self-update.
 *
 * A "scheduled" acknowledgement from the ARM is never success, and the apply
 * that follows it is busy replacing the very code that would report its own
 * outcome, on a connection it has already released. The only trustworthy
 * success signal is the NEW code phoning home, so
 * the freshly-installed build pushes metadata once on its first boot rather
 * than leaving the control plane to wait out the 30-minute metadata cron.
 *
 * "Once and only once per version" is the load-bearing property: this fires on
 * plugins_loaded, i.e. on EVERY request, so a boot that failed to stamp would
 * turn one confirmation into a CP call per page view.
 *
 * Boots the REAL Plugin singleton with the same minimal stub set
 * PluginMaybeGcSnapshotsTest uses.
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
final class PluginVersionChangedPushTest extends TestCase
{
    /** @var array<string,mixed> In-memory wp-option store. */
    private array $options = [];

    /** Temp uploads dir standing in for wp_upload_dir()'s basedir. */
    private string $uploadsDir = '';

    /** @var list<array{hook:string,priority:int}> Every add_action() seen after boot. */
    private array $actions = [];

    /** Set true once boot() has finished, so only post-boot hooks are recorded. */
    private bool $recordActions = false;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->uploadsDir    = sys_get_temp_dir() . '/wpmgr-plugin-b3-' . bin2hex(random_bytes(6));
        mkdir($this->uploadsDir, 0755, true);
        $this->options       = [];
        $this->actions       = [];
        $this->recordActions = false;

        // Same minimal boot-stub set as PluginMaybeGcSnapshotsTest::set_up().
        foreach (['add_filter', 'register_activation_hook',
                  'register_deactivation_hook', 'wp_schedule_single_event',
                  'spawn_cron', 'wp_next_scheduled', 'wp_schedule_event',
                  'wp_get_scheduled_event', 'wp_clear_scheduled_hook'] as $fn) {
            Functions\when($fn)->justReturn(true);
        }
        Functions\when('add_action')->alias(function (string $hook, $callback, int $priority = 10) {
            if ($this->recordActions) {
                $this->actions[] = ['hook' => $hook, 'priority' => $priority];
            }
            return true;
        });
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

        // The version under test. Guard-defined because PHP constants are
        // process-global and another test file in this (non-isolated) suite may
        // already have defined it, so the assertions read the live value back
        // rather than assuming this literal won.
        if (!defined('WPMGR_AGENT_VERSION')) {
            define('WPMGR_AGENT_VERSION', '0.10.5');
        }
    }

    protected function tear_down(): void
    {
        Plugin::resetForTesting();
        $this->rrmdir($this->uploadsDir);
        Monkey\tearDown();
        parent::tear_down();
    }

    /** Recursive delete used only for test fixture cleanup. */
    private function rrmdir(string $dir): void
    {
        if ($dir === '' || !is_dir($dir)) {
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

    /** Boot the real singleton and start recording hook registrations. */
    private function bootPlugin(): Plugin
    {
        $plugin              = Plugin::boot();
        $this->recordActions = true;

        return $plugin;
    }

    /** Count the 'shutdown' registrations captured since boot finished. */
    private function shutdownHookCount(): int
    {
        $count = 0;
        foreach ($this->actions as $action) {
            if ($action['hook'] === 'shutdown') {
                $count++;
            }
        }

        return $count;
    }

    /**
     * The whole point: the confirmation push arms EXACTLY ONCE for a given
     * version, no matter how many requests boot on it. This runs on
     * plugins_loaded, so a missing stamp would mean a CP round trip per page
     * view.
     */
    public function test_confirmation_push_arms_once_and_only_once_per_version(): void
    {
        $this->options[Settings::OPTION_SITE_ID] = 'site-abc';
        $this->options[Settings::OPTION_CP_URL]  = 'https://cp.example.test';

        $plugin = $this->bootPlugin();

        $this->assertTrue(
            $plugin->maybePushVersionChangedMetadata(),
            'the first boot on an unreported version must arm the confirmation push'
        );
        $this->assertSame(1, $this->shutdownHookCount());
        $this->assertSame(
            (string) constant('WPMGR_AGENT_VERSION'),
            $this->options[Plugin::OPTION_REPORTED_VERSION] ?? null,
            'the reported-version stamp must be written by the arming call'
        );

        $this->assertFalse($plugin->maybePushVersionChangedMetadata());
        $this->assertFalse($plugin->maybePushVersionChangedMetadata());
        $this->assertSame(
            1,
            $this->shutdownHookCount(),
            'later boots on the same version must not arm a second push'
        );
    }

    /**
     * The stamp is written BEFORE the network push, not after: a control plane
     * that is unreachable must not make every subsequent request re-attempt the
     * confirmation. The 30-minute metadata cron is the backstop.
     */
    public function test_a_later_version_change_arms_the_push_again(): void
    {
        $this->options[Settings::OPTION_SITE_ID]         = 'site-abc';
        $this->options[Settings::OPTION_CP_URL]          = 'https://cp.example.test';
        $this->options[Plugin::OPTION_REPORTED_VERSION]  = '0.0.1-some-older-build';

        $plugin = $this->bootPlugin();

        $this->assertTrue($plugin->maybePushVersionChangedMetadata());
        $this->assertSame(1, $this->shutdownHookCount());
        $this->assertFalse($plugin->maybePushVersionChangedMetadata());
        $this->assertSame(1, $this->shutdownHookCount());
    }

    /**
     * A site that is not enrolled has no control plane to confirm anything to,
     * and must not even stamp (so its first push after enrollment still fires).
     */
    public function test_an_unenrolled_site_never_arms_the_push(): void
    {
        // Deliberately NOT enrolled: no site_id/cp_url in the option store.
        $plugin = $this->bootPlugin();

        $this->assertFalse($plugin->maybePushVersionChangedMetadata());
        $this->assertSame(0, $this->shutdownHookCount());
        $this->assertArrayNotHasKey(Plugin::OPTION_REPORTED_VERSION, $this->options);
    }
}
