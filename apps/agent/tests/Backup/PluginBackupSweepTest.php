<?php
/**
 * GitHub issue #232 — Plugin::maybeSweepStalledBackups() regression lock.
 *
 * Exercises the REAL method under test via a Plugin instance built with
 * `ReflectionClass::newInstanceWithoutConstructor()` + a directly-injected
 * `Settings` instance (reflection-set onto the private `$settings`
 * property). This deliberately avoids the heavy `Plugin::boot()` singleton
 * path (schema migrations, cache manager, object cache, admin menus, error
 * monitor, …) — maybeSweepStalledBackups() only ever touches
 * `$this->settings->isEnrolled()`, `get_option()`/`update_option()`, and
 * `Watchdog::sweepStalled()`, so this is a faithful, fully-isolated test of
 * the production method. (`Plugin::boot()` was observed to leak global
 * state — e.g. a real installed error handler / static config caches —
 * that broke unrelated tests elsewhere in the suite when this test called
 * it; reflection-injecting just the one property this method touches
 * avoids that surface entirely.)
 *
 * Verifies:
 *   - Gated on isEnrolled(): a not-yet-enrolled site never touches $wpdb.
 *   - Throttled to once per 60s via a stored option, stamped BEFORE the
 *     sweep runs (a slow/failing sweep can never re-run on every request
 *     within the window).
 *   - Past the throttle window, Watchdog::sweepStalled() actually runs
 *     (observed via the shared FakeBackupTasksWpdb spy).
 *   - A thrown sweep never bubbles out of the request.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionClass;
use WPMgr\Agent\Plugin;
use WPMgr\Agent\Settings;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Plugin
 */
final class PluginBackupSweepTest extends TestCase
{
    /** @var array<string,mixed> In-memory wp-option store. */
    private array $options = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->options = [];
        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;
            return true;
        });
        Functions\when('get_option')->alias(function ($name, $default = false) {
            return $this->options[$name] ?? $default;
        });
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Build a Plugin instance WITHOUT running its (private, heavy)
     * constructor or boot() — only `$settings` is wired, which is all
     * maybeSweepStalledBackups() touches on `$this`.
     */
    private function makePlugin(): Plugin
    {
        $reflection = new ReflectionClass(Plugin::class);
        /** @var Plugin $plugin */
        $plugin = $reflection->newInstanceWithoutConstructor();

        $settingsProp = $reflection->getProperty('settings');
        $settingsProp->setValue($plugin, new Settings());

        return $plugin;
    }

    private function markEnrolled(): void
    {
        $this->options[Settings::OPTION_SITE_ID] = 'site-abc';
        $this->options[Settings::OPTION_CP_URL]  = 'https://cp.example.test';
    }

    public function test_not_enrolled_never_touches_wpdb(): void
    {
        // Deliberately NOT enrolled.
        $wpdb = new FakeBackupTasksWpdb();
        $GLOBALS['wpdb'] = $wpdb;

        $this->makePlugin()->maybeSweepStalledBackups();

        $this->assertSame(0, $wpdb->prepareCallCount, 'a not-yet-enrolled site must never reach Watchdog::sweepStalled()');
        $this->assertArrayNotHasKey(Plugin::OPTION_BACKUP_SWEEP_LAST, $this->options, 'a not-yet-enrolled site must never stamp the throttle option');
    }

    public function test_within_throttle_window_is_a_no_op(): void
    {
        $this->markEnrolled();
        $this->options[Plugin::OPTION_BACKUP_SWEEP_LAST] = time() - 10; // 10s ago, well under the 60s window.

        $wpdb = new FakeBackupTasksWpdb();
        $GLOBALS['wpdb'] = $wpdb;

        $this->makePlugin()->maybeSweepStalledBackups();

        $this->assertSame(
            time() - 10,
            $this->options[Plugin::OPTION_BACKUP_SWEEP_LAST],
            'a throttled call within the window must never re-stamp the option'
        );
        $this->assertSame(0, $wpdb->prepareCallCount, 'a throttled call within the window must never touch $wpdb');
    }

    public function test_past_throttle_window_stamps_before_running_then_sweeps(): void
    {
        $this->markEnrolled();
        $this->options[Plugin::OPTION_BACKUP_SWEEP_LAST] = time() - 3600; // long past the 60s window.

        $wpdb = new FakeBackupTasksWpdb();
        // A stale row so sweepStalled() has something to find and resume;
        // proves the sweep actually ran end-to-end, not just that the
        // throttle stamp updated.
        $wpdb->seedRow('eeeeeeee-ffff-4000-8000-000000000000', [
            'phase'            => 'queued',
            'started_at'       => time() - 200,
            'last_progress_at' => time() - 200,
            'resume_count'     => 0,
            'sub_state'        => '{}', // no params -> resumeIfStalled() bails cleanly after the resume_count bump.
        ]);
        $GLOBALS['wpdb'] = $wpdb;

        $before = time();
        $this->makePlugin()->maybeSweepStalledBackups();

        $this->assertGreaterThanOrEqual(
            $before,
            (int) $this->options[Plugin::OPTION_BACKUP_SWEEP_LAST],
            'the throttle option must be stamped to "now" before the sweep runs'
        );

        // Proof the sweep actually reached the DB: the seeded stale row's
        // resume_count was bumped from 0 to 1.
        $row = $wpdb->rows['eeeeeeee-ffff-4000-8000-000000000000'] ?? null;
        $this->assertNotNull($row);
        $this->assertSame(1, $row['resume_count'], 'sweepStalled() must have resumed the stale row past the throttle gate');
    }

    public function test_a_thrown_sweep_never_bubbles_out_of_the_request(): void
    {
        $this->markEnrolled();

        // A $wpdb double whose very first call (prepare()) throws — proves
        // maybeSweepStalledBackups() completes normally (no uncaught
        // exception) regardless of which layer contains the failure.
        $wpdb = new class {
            public string $prefix = 'wp_';

            /**
             * @param mixed ...$args
             */
            public function prepare(string $query, ...$args): string
            {
                throw new \RuntimeException('simulated DB failure');
            }
        };
        $GLOBALS['wpdb'] = $wpdb;

        $plugin = $this->makePlugin();

        $threw = false;
        try {
            $plugin->maybeSweepStalledBackups();
        } catch (\Throwable $e) {
            $threw = true;
        }

        $this->assertFalse($threw, 'a thrown sweep must never bubble out of the plugins_loaded-bound request handler');
        $this->assertArrayHasKey(
            Plugin::OPTION_BACKUP_SWEEP_LAST,
            $this->options,
            'the throttle stamp must still have been written (stamp-BEFORE-run) even though the sweep itself failed'
        );
    }
}
