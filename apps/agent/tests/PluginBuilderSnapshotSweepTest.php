<?php
/**
 * The hourly sweep of expired page-edit page copies is wired into the
 * plugin: its cron event is bound to BuilderDocumentSnapshot's callback,
 * scheduled when the recurring events are re-armed and on activation, and
 * cleared on deactivation.
 *
 * Boots the real Plugin singleton with the minimal stub set
 * PluginMaybeGcSnapshotsTest uses, capturing the hook bindings and the cron
 * calls.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\BuilderDocumentSnapshot;
use WPMgr\Agent\Plugin;
use WPMgr\Agent\Settings;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Plugin
 */
final class PluginBuilderSnapshotSweepTest extends TestCase
{
    /** @var array<string,mixed> In-memory wp-option store. */
    private array $options = [];

    /** @var list<array{0:string,1:mixed}> Hook bindings, in order. */
    private array $actions = [];

    /** @var array<string,array{0:int,1:string}> Scheduled recurring events by hook. */
    private array $events = [];

    /** @var list<string> Hooks cleared, in order. */
    private array $cleared = [];

    private string $uploadsDir = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->uploadsDir = sys_get_temp_dir() . '/wpmgr-plugin-sweep-' . bin2hex(random_bytes(6));
        mkdir($this->uploadsDir, 0755, true);
        $this->options = [];
        $this->actions = [];
        $this->events  = [];
        $this->cleared = [];

        foreach (['add_filter', 'register_activation_hook', 'register_deactivation_hook',
                  'wp_schedule_single_event', 'spawn_cron', 'wp_get_scheduled_event'] as $fn) {
            Functions\when($fn)->justReturn(true);
        }
        Functions\when('add_action')->alias(function ($hook, $callback) {
            $this->actions[] = [(string) $hook, $callback];

            return true;
        });
        Functions\when('wp_next_scheduled')->alias(fn ($hook) => isset($this->events[$hook]) ? $this->events[$hook][0] : false);
        Functions\when('wp_schedule_event')->alias(function ($timestamp, $recurrence, $hook): bool {
            $this->events[(string) $hook] = [(int) $timestamp, (string) $recurrence];

            return true;
        });
        Functions\when('wp_clear_scheduled_hook')->alias(function ($hook): int {
            $this->cleared[] = (string) $hook;
            unset($this->events[$hook]);

            return 0;
        });
        Functions\when('is_admin')->justReturn(false);
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('plugin_basename')->returnArg();
        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;

            return true;
        });
        Functions\when('get_option')->alias(fn ($name, $default = false) => $this->options[$name] ?? $default);
        Functions\when('delete_option')->alias(function ($name) {
            unset($this->options[$name]);

            return true;
        });
        Functions\when('get_site_option')->alias(fn ($name, $default = false) => $this->options[$name] ?? $default);
        Functions\when('update_site_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;

            return true;
        });
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $this->uploadsDir]);
    }

    protected function tear_down(): void
    {
        Plugin::resetForTesting();
        if (is_dir($this->uploadsDir)) {
            @rmdir($this->uploadsDir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- test-only fixture cleanup
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_the_sweep_event_is_bound_scheduled_on_rearm_and_cleared_on_deactivation(): void
    {
        $this->options[Settings::OPTION_SITE_ID] = 'site-abc';
        $this->options[Settings::OPTION_CP_URL]  = 'https://cp.example.test';

        $plugin = Plugin::boot();
        $this->assertContains(
            [BuilderDocumentSnapshot::HOOK_SWEEP, [BuilderDocumentSnapshot::class, 'sweepScheduled']],
            $this->actions,
            'the cron event runs the bounded sweep'
        );

        // The recurring events went missing (the heartbeat canary is absent).
        $plugin->maybeRescheduleCron();
        $this->assertSame('hourly', $this->events[BuilderDocumentSnapshot::HOOK_SWEEP][1] ?? null, 'the sweep is re-armed with the recurring events');

        $plugin->deactivate();
        $this->assertContains(BuilderDocumentSnapshot::HOOK_SWEEP, $this->cleared, 'deactivation clears the sweep');
    }
}
