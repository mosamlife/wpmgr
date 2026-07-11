<?php
/**
 * SchedulerTest — GitHub issue #212: refreshUpdateTransients()'s `$force`
 * flag only ever bypassed WPMgr's OWN 5-minute REFRESH_LOCK_KEY throttle; it
 * never bypassed WordPress core's own ~12h `update_plugins`/`update_themes`/
 * `update_core` transient throttle, so a human-initiated "Refresh" on the
 * dashboard would often silently no-op against a still-fresh core transient
 * and require several clicks to actually reach wp.org.
 *
 * These tests exercise Scheduler::refreshUpdateTransients() directly (via
 * ReflectionClass::newInstanceWithoutConstructor() — the method under test
 * never touches $settings/$enrollment/$lifecycle, only the WP transient/
 * update-check functions, so constructing the real (final, heavy-dependency)
 * collaborators would add nothing; see RouterTest.php for the identical
 * precedent) and prove:
 *   1. force=true deletes ALL THREE site transients (update_plugins,
 *      update_themes, update_core) BEFORE re-checking, and passes `true` as
 *      wp_version_check()'s second (bypass-cache) argument — even when
 *      WPMgr's own REFRESH_LOCK_KEY lock is still held, i.e. the human
 *      "Refresh" click is not throttled by a refresh that happened <5 min ago.
 *   2. force=false (the 30-min cron / event-hook path) does NONE of the
 *      transient deletes and passes `false` as wp_version_check()'s second
 *      argument, so a bulk plugin/theme toggle (which fires several soft
 *      refreshes back-to-back via upgrader_process_complete/switch_theme/
 *      activated_plugin/deactivated_plugin) never hammers wp.org.
 *
 * Note: the existing RefreshInventoryCommandTest injects closures for its
 * collaborators and therefore never exercises the real
 * Scheduler::refreshUpdateTransients() body — it cannot catch this class of
 * regression. This suite calls the real method.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Scheduler;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Scheduler
 */
final class SchedulerTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Builds a real Scheduler instance without calling __construct — the
     * constructor requires Settings/Enrollment/Lifecycle (all final, with
     * their own heavy dependency chains) that refreshUpdateTransients()
     * never touches.
     */
    private function makeScheduler(): Scheduler
    {
        return (new \ReflectionClass(Scheduler::class))->newInstanceWithoutConstructor();
    }

    public function test_force_true_deletes_all_three_transients_and_hard_forces_core_check(): void
    {
        // The WPMgr REFRESH_LOCK_KEY is still held (as it would be if a human
        // clicks Refresh twice within 5 minutes) — force=true must proceed
        // anyway; get_transient() must not even gate the outcome.
        Functions\when('get_transient')->justReturn(1);
        Functions\when('set_transient')->justReturn(true);

        $deletedKeys = [];
        Functions\expect('delete_site_transient')
            ->times(3)
            ->andReturnUsing(function (string $key) use (&$deletedKeys): bool {
                $deletedKeys[] = $key;
                return true;
            });

        Functions\expect('wp_update_plugins')->once();
        Functions\expect('wp_update_themes')->once();
        Functions\expect('wp_version_check')->once()->with([], true);

        $this->makeScheduler()->refreshUpdateTransients(true);

        $this->assertSame(
            ['update_plugins', 'update_themes', 'update_core'],
            $deletedKeys,
            'force=true must delete all three site transients before re-checking'
        );
    }

    public function test_force_false_never_deletes_transients_and_soft_checks_core(): void
    {
        // Lock not held -> the soft path proceeds to the actual (throttled)
        // WP.org re-checks.
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);

        Functions\expect('delete_site_transient')->never();
        Functions\expect('wp_update_plugins')->once();
        Functions\expect('wp_update_themes')->once();
        Functions\expect('wp_version_check')->once()->with([], false);

        $this->makeScheduler()->refreshUpdateTransients(false);

        // The real proof is the mock call-count expectations above (verified
        // on Monkey\tearDown()); this keeps the test itself non-risky.
        $this->addToAssertionCount(1);
    }

    public function test_force_defaults_to_false_when_omitted(): void
    {
        // The 30-min cron (runMetadata -> refreshUpdateTransients()) and the
        // upgrader_process_complete/switch_theme/activated_plugin/
        // deactivated_plugin event hooks all call with NO argument — the
        // default must stay soft (false), never hard-forcing.
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('wp_update_plugins')->justReturn(null);
        Functions\when('wp_update_themes')->justReturn(null);

        Functions\expect('delete_site_transient')->never();
        Functions\expect('wp_version_check')->once()->with([], false);

        $this->makeScheduler()->refreshUpdateTransients();

        $this->addToAssertionCount(1);
    }
}
