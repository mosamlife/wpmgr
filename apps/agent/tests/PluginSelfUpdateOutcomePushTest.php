<?php
/**
 * The other half of the CP-commanded self-update's reporting: how a FAILED
 * apply reaches the control plane.
 *
 * A SUCCESSFUL apply announces itself, because the new build boots and pushes
 * its changed version (PluginVersionChangedPushTest covers that). A failed one
 * changes no version, so nothing about it would move until the next metadata
 * cron, which runs every 30 minutes: longer than the control plane's shortest
 * confirm deadline. The site would then be recorded as having gone silent when
 * in fact it had a precise, already-written account of what went wrong.
 *
 * So the self-updater fires wpmgr_agent_self_update_recorded from shutdown once
 * it has recorded a non-applied outcome, and Plugin binds that to an immediate
 * metadata push. This file pins the binding and the swallow-everything
 * behaviour of the callback.
 *
 * Boots the REAL Plugin singleton with the same minimal stub set
 * PluginVersionChangedPushTest uses.
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
final class PluginSelfUpdateOutcomePushTest extends TestCase
{
    /** @var array<string,mixed> In-memory wp-option store. */
    private array $options = [];

    /** Temp uploads dir standing in for wp_upload_dir()'s basedir. */
    private string $uploadsDir = '';

    /** @var list<array{hook:string,priority:int,callback:mixed}> Every add_action() seen. */
    private array $actions = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->uploadsDir = sys_get_temp_dir() . '/wpmgr-plugin-outcome-' . bin2hex(random_bytes(6));
        mkdir($this->uploadsDir, 0755, true);
        $this->options = [];
        $this->actions = [];

        foreach (['add_filter', 'register_activation_hook',
                  'register_deactivation_hook', 'wp_schedule_single_event',
                  'spawn_cron', 'wp_next_scheduled', 'wp_schedule_event',
                  'wp_get_scheduled_event', 'wp_clear_scheduled_hook'] as $fn) {
            Functions\when($fn)->justReturn(true);
        }
        Functions\when('add_action')->alias(function (string $hook, $callback, int $priority = 10) {
            $this->actions[] = ['hook' => $hook, 'priority' => $priority, 'callback' => $callback];
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

    /** Mark the site enrolled and boot the real singleton. */
    private function bootEnrolledPlugin(): Plugin
    {
        $this->options[Settings::OPTION_SITE_ID] = 'site-abc';
        $this->options[Settings::OPTION_CP_URL]  = 'https://cp.example.test';

        return Plugin::boot();
    }

    /** Every registration of the named action, with its callback. */
    private function bindingsFor(string $hook): array
    {
        return array_values(array_filter(
            $this->actions,
            static fn (array $action): bool => $action['hook'] === $hook
        ));
    }

    // -------------------------------------------------------------------------
    // The binding
    // -------------------------------------------------------------------------

    /**
     * The action the self-updater fires must land on a metadata push, or a
     * failed apply is invisible until the CP has already given up on it.
     */
    public function test_a_recorded_self_update_outcome_is_bound_to_an_immediate_metadata_push(): void
    {
        $plugin   = $this->bootEnrolledPlugin();
        $bindings = $this->bindingsFor('wpmgr_agent_self_update_recorded');

        $this->assertCount(1, $bindings, 'exactly one listener, bound once per boot');
        $this->assertSame(
            [$plugin, 'pushMetadataNow'],
            $bindings[0]['callback'],
            'the recorded-outcome action must reach the metadata sender'
        );
    }

    /**
     * The binding is boot-time and gated on ONE thing: the self-updater
     * existing. It is deliberately not also gated on enrollment, because an
     * un-enrolled site cannot have been told to update itself in the first
     * place, so the listener costs it nothing and the sender's own enrollment
     * guard is the one that matters. The guard it does sit behind is what keeps
     * the wp.org distribution build, where the self-updater is not shipped at
     * all, from registering it.
     */
    public function test_the_binding_is_gated_only_on_the_self_updater_existing(): void
    {
        // Not enrolled: no site_id, no cp_url.
        Plugin::boot();

        $this->assertCount(
            1,
            $this->bindingsFor('wpmgr_agent_self_update_recorded'),
            'the listener is registered from the same guard as the rest of the self-update wiring'
        );
    }

    /**
     * The callback SWALLOWS EVERYTHING. It runs from shutdown, where a Throwable
     * abandons the rest of the queue: on this request that queue still holds the
     * rollback guard and the catch-up heartbeat. A report that cannot be
     * delivered right now is picked up by the next metadata cron; it must never
     * take anything else down with it.
     *
     * The push is deliberately left to fail here (no HTTP stub exists in this
     * fixture), which is exactly the condition being tested.
     */
    public function test_the_push_callback_never_disturbs_the_request_it_rides_on(): void
    {
        $plugin = $this->bootEnrolledPlugin();

        $plugin->pushMetadataNow();

        $this->addToAssertionCount(1);
    }
}
