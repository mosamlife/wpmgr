<?php
/**
 * BEAT 2 (APPLY) of the CP-commanded agent self-update, second entry point: the
 * WP-Cron-INDEPENDENT backstop bound to plugins_loaded.
 *
 * The cron event used to be the only way beat 2 ever ran. A site whose loopback
 * cron is blocked, whose wp_schedule_single_event was short-circuited by another
 * plugin, or that simply never gets a tick, staged an update that nothing was
 * left to apply, and then sat at "scheduled" until the control plane's confirm
 * window ran out. This is the same treatment GitHub issues #226 and #232 gave
 * the snapshot reclaim and the stalled-backup reaper.
 *
 * THE INVARIANT THIS FILE EXISTS TO PIN: beat 2 must run in a DIFFERENT REQUEST
 * from the arm. The swap must never happen inside the request that has to report
 * the outcome, and the hook choice is the whole reason it does not.
 * plugins_loaded fires during WordPress's plugin-loading phase in wp-settings.php;
 * a REST route callback runs from parse_request, far later in the SAME request.
 * So on the arm's own request the gate runs BEFORE the signed command writes the
 * staged record, finds nothing, and arms nothing. Moving this binding to any
 * hook that fires after the REST callback would silently break the isolation the
 * three-beat design is built on, and the ordering test below is what fails when
 * somebody does.
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
use WPMgr\Agent\Commands\AgentSelfUpdateCommand;
use WPMgr\Agent\Plugin;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Support\UpdateChecker;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Plugin
 * @covers \WPMgr\Agent\Support\UpdateChecker
 */
final class PluginSelfUpdateBackstopTest extends TestCase
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

        $this->uploadsDir = sys_get_temp_dir() . '/wpmgr-plugin-b2-' . bin2hex(random_bytes(6));
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
        Functions\when('delete_site_transient')->justReturn(true);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => $this->uploadsDir]);

        // Neutralise ConnectionFinisher's last-rung flush for the CLI SAPI.
        // Only real PHP functions are stubbed here, never the finish-request
        // pair: DEFINING fastcgi_finish_request/litespeed_finish_request would
        // flip function_exists() process-wide and change which rung unrelated
        // tests observe.
        Functions\when('headers_sent')->justReturn(true);
        Functions\when('ob_get_level')->justReturn(0);
        Functions\when('flush')->justReturn(null);

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

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    /** Mark the site enrolled and boot the real singleton. */
    private function bootEnrolledPlugin(): Plugin
    {
        $this->options[Settings::OPTION_SITE_ID] = 'site-abc';
        $this->options[Settings::OPTION_CP_URL]  = 'https://cp.example.test';

        return Plugin::boot();
    }

    /**
     * Invoke every registered callback for $hook whose method name is $method,
     * and return how many fired. Deliberately narrow: firing EVERY
     * plugins_loaded callback would drag in unrelated sweeps this test says
     * nothing about, while asserting the count pins that the self-update apply
     * really is bound to the hook named here.
     *
     * @param string $hook   Hook name the callback must be registered on.
     * @param string $method Plugin method name.
     * @return int Callbacks fired.
     */
    private function fireBoundMethod(string $hook, string $method): int
    {
        $fired = 0;
        foreach ($this->actions as $action) {
            if ($action['hook'] !== $hook) {
                continue;
            }
            $callback = $action['callback'];
            if (!is_array($callback) || count($callback) !== 2 || $callback[1] !== $method) {
                continue;
            }
            $callback();
            $fired++;
        }

        return $fired;
    }

    /** Count registrations of a given Plugin method on a given hook. */
    private function boundCount(string $hook, string $method): int
    {
        $count = 0;
        foreach ($this->actions as $action) {
            if ($action['hook'] !== $hook) {
                continue;
            }
            $callback = $action['callback'];
            if (is_array($callback) && count($callback) === 2 && $callback[1] === $method) {
                $count++;
            }
        }

        return $count;
    }

    /** A live staged record for the version one segment above the on-disk core. */
    private function stageRecord(): array
    {
        $target = (string) preg_replace('/[-+].*$/', '', (string) constant('WPMGR_AGENT_VERSION')) . '.1';

        return [
            'from_version' => (string) constant('WPMGR_AGENT_VERSION'),
            'to_version'   => $target,
            'staged_at'    => time(),
            'expires_at'   => time() + UpdateChecker::STAGED_TTL_SECONDS,
            'token'        => 'live-token',
        ];
    }

    // -------------------------------------------------------------------------
    // The binding itself
    // -------------------------------------------------------------------------

    /**
     * The apply backstop must be bound to plugins_loaded and to nothing later.
     * The hook name is the entire mechanism by which the arm's own request
     * cannot apply, so it is asserted as a literal rather than inferred.
     */
    public function test_the_apply_backstop_is_bound_to_plugins_loaded(): void
    {
        $this->bootEnrolledPlugin();

        $this->assertSame(
            1,
            $this->boundCount('plugins_loaded', 'maybeApplyStagedSelfUpdate'),
            'beat 2 must have a request-bound entry point, and it must sit on plugins_loaded'
        );
    }

    /**
     * The common case, which is every site on essentially every request: no
     * staged record, so nothing is armed and no work is queued.
     */
    public function test_a_request_with_nothing_staged_arms_nothing(): void
    {
        $plugin = $this->bootEnrolledPlugin();

        $this->assertSame(1, $this->fireBoundMethod('plugins_loaded', 'maybeApplyStagedSelfUpdate'));
        $this->assertSame(
            0,
            $this->boundCount('shutdown', 'applyStagedSelfUpdateOnShutdown'),
            'a boot with nothing staged must not queue an apply'
        );
        $this->assertFalse($plugin->maybeApplyStagedSelfUpdate());
    }

    /**
     * A site with no control plane is never told to update itself.
     */
    public function test_an_unenrolled_site_never_arms_the_apply(): void
    {
        // Deliberately NOT enrolled: no site_id/cp_url in the option store.
        $plugin = Plugin::boot();
        $this->options[UpdateChecker::OPTION_STAGED] = $this->stageRecord();

        $this->assertFalse($plugin->maybeApplyStagedSelfUpdate());
        $this->assertSame(0, $this->boundCount('shutdown', 'applyStagedSelfUpdateOnShutdown'));
    }

    // -------------------------------------------------------------------------
    // The isolation invariant
    // -------------------------------------------------------------------------

    /**
     * THE ARM REQUEST MUST NEVER APPLY.
     *
     * Replays one request in WordPress's own order. plugins_loaded fires during
     * the plugin-loading phase of wp-settings.php; the signed REST callback that
     * writes the staged record runs from parse_request, far later in that same
     * request. So the gate runs FIRST and finds nothing, and the record it would
     * have applied only comes into existence afterwards.
     *
     * This is the constraint the whole three-beat design rests on: the swap must
     * not happen inside the request that has to report the outcome. If this
     * binding is ever moved to a hook that fires after the REST callback (init
     * is still safe, wp_loaded and later are not), this test is what fails.
     */
    public function test_the_arm_request_itself_never_applies(): void
    {
        $plugin = $this->bootEnrolledPlugin();

        // 1. plugins_loaded, the real position of the gate in the request.
        $this->assertSame(1, $this->fireBoundMethod('plugins_loaded', 'maybeApplyStagedSelfUpdate'));

        // 2. Much later in the SAME request: the signed agent_self_update
        //    command runs and stages the record.
        $record = $this->stageRecord();
        $this->options[UpdateChecker::OPTION_STAGED] = $record;

        // 3. shutdown. Nothing was armed, so nothing applies.
        $this->assertSame(
            0,
            $this->boundCount('shutdown', 'applyStagedSelfUpdateOnShutdown'),
            'the request that answers the control plane must not queue the swap it just armed'
        );
        $this->assertSame(0, $this->fireBoundMethod('shutdown', 'applyStagedSelfUpdateOnShutdown'));

        $this->assertSame(
            $record,
            $this->options[UpdateChecker::OPTION_STAGED] ?? null,
            'the record the arm just wrote must survive the arm request untouched'
        );
        $this->assertArrayNotHasKey(
            AgentSelfUpdateCommand::OPTION_RESULT,
            $this->options,
            'no apply ran, so no apply outcome may exist'
        );
    }

    /**
     * The next request applies it, with no cron event anywhere in the picture.
     * This is the whole point of the backstop: every request the agent serves,
     * including the control plane's own heartbeat and uptime hits, is a far more
     * reliable clock than WP-Cron.
     *
     * The apply fails here for want of a Plugin_Upgrader, which this process
     * does not have. That failure IS the proof: reaching it means the record was
     * claimed and the upgrade attempted, which is exactly what never happened on
     * the arm request above.
     */
    public function test_a_later_request_applies_the_staged_record_without_cron(): void
    {
        $plugin = $this->bootEnrolledPlugin();
        $this->options[UpdateChecker::OPTION_STAGED] = $this->stageRecord();

        $this->assertSame(1, $this->fireBoundMethod('plugins_loaded', 'maybeApplyStagedSelfUpdate'));
        $this->assertSame(
            1,
            $this->boundCount('shutdown', 'applyStagedSelfUpdateOnShutdown'),
            'a request that finds a staged record must queue exactly one apply'
        );

        $this->assertSame(1, $this->fireBoundMethod('shutdown', 'applyStagedSelfUpdateOnShutdown'));

        $this->assertArrayNotHasKey(
            UpdateChecker::OPTION_STAGED,
            $this->options,
            'the apply claims the record before any upgrade work, so it must be gone'
        );

        $result = $this->options[AgentSelfUpdateCommand::OPTION_RESULT] ?? null;
        $this->assertIsArray($result, 'the apply must record an outcome for the next metadata push');
        $this->assertSame('failed', $result['status']);
        $this->assertArrayNotHasKey(
            'wpmgr_agent_self_update_applying',
            $this->options,
            'the in-flight marker is released on every terminal path'
        );
    }

    /**
     * The apply is deferred to shutdown rather than run inline on
     * plugins_loaded, and that is not a nicety. The work downloads a package and
     * overwrites this plugin's own directory; doing that part-way through the
     * boot of the request executing it would let anything autoloaded later in
     * that request come from the new build while the loaded classes came from
     * the old one.
     */
    public function test_the_apply_is_deferred_to_shutdown_not_run_inline(): void
    {
        $plugin = $this->bootEnrolledPlugin();
        $this->options[UpdateChecker::OPTION_STAGED] = $this->stageRecord();

        $this->assertTrue($plugin->maybeApplyStagedSelfUpdate());

        $this->assertArrayHasKey(
            UpdateChecker::OPTION_STAGED,
            $this->options,
            'the gate only arms; nothing may be claimed or applied during plugins_loaded itself'
        );
        $this->assertArrayNotHasKey(AgentSelfUpdateCommand::OPTION_RESULT, $this->options);
        $this->assertSame(1, $this->boundCount('shutdown', 'applyStagedSelfUpdateOnShutdown'));
    }
}
