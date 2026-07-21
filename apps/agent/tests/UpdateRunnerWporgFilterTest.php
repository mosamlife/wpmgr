<?php
/**
 * UpdateRunnerWporgFilterTest — wp.org pre-resubmission review response
 * (T15): the `upgrader_package_options` filter UpdateRunner::applyViaUpgrader()
 * registers to force a clean working directory for the current upgrade must
 * never be registered in the wp.org distribution build, so that build never
 * alters WordPress core's own upgrader behaviour and instead degrades to
 * core's default working-directory handling.
 *
 * `make agent-zip-wporg` injects `define('WPMGR_WPORG_BUILD', true);` into
 * the staged main file — exactly what
 * test_filter_not_registered_in_wporg_build() below reproduces. The closure
 * definition and the `finally`-block `remove_filter()` call are unchanged by
 * this fix and are intentionally NOT under test here: `remove_filter()` of a
 * callback that was never registered is a harmless no-op, so self-hosted
 * behaviour (the default build, WPMGR_WPORG_BUILD undefined) stays
 * bit-for-bit identical — see test_filter_registered_in_default_build().
 *
 * Both tests invoke the protected applyViaUpgrader() via reflection with a
 * deliberately unsupported $type ('wpmgr-test-unsupported-type'), which
 * short-circuits the switch in the method's `try` block with none of its
 * plugin/theme/core cases matching. That reaches — and passes through — the
 * add_filter()/remove_filter() pair under test without needing the real
 * Plugin_Upgrader/Theme_Upgrader machinery (not stubbed anywhere in this
 * suite) to actually run an upgrade.
 *
 * Both tests run in a SEPARATE PROCESS: WPMGR_WPORG_BUILD and WP_TEMP_DIR are
 * real PHP constants that, once defined, can never be undefined for the rest
 * of a PHPUnit process — see SizeProbeWporgGuardTest and UpdateRunnerTempDirTest
 * for the same idiom used for the same reason. WP_TEMP_DIR is pre-defined in
 * set_up() so pinTempDirForUnpack() short-circuits at its very first check,
 * keeping this test isolated from the filesystem-writability probing that
 * method otherwise performs (irrelevant to what's under test here).
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Filters;
use PHPUnit\Framework\Attributes\PreserveGlobalState;
use PHPUnit\Framework\Attributes\RunTestsInSeparateProcesses;
use WPMgr\Agent\Support\UpdateRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\UpdateRunner
 */
#[RunTestsInSeparateProcesses]
#[PreserveGlobalState(false)]
final class UpdateRunnerWporgFilterTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        if (!defined('WP_TEMP_DIR')) {
            define('WP_TEMP_DIR', sys_get_temp_dir());
        }
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Invoke the protected applyViaUpgrader() with an unsupported $type via
     * reflection. No setAccessible() call needed — ReflectionMethod::invoke()
     * has been able to call non-public methods directly since PHP 8.1 (see
     * UpdateRunnerTempDirTest::pin() for the same idiom).
     *
     * @return array{ok:bool,log:string}
     */
    private function applyWithUnsupportedType(UpdateRunner $runner): array
    {
        $method = new \ReflectionMethod(UpdateRunner::class, 'applyViaUpgrader');

        /** @var array{ok:bool,log:string} $result */
        $result = $method->invoke($runner, 'wpmgr-test-unsupported-type', 'irrelevant-slug', 'latest');

        return $result;
    }

    /**
     * THE REGRESSION LOCK. With WPMGR_WPORG_BUILD defined true (exactly as
     * `make agent-zip-wporg` stamps the staged main file), applyViaUpgrader()
     * must never register the upgrader_package_options filter. Run this test
     * against the pre-fix code and it fails: the old add_filter() call ran
     * unconditionally regardless of build identity.
     */
    public function test_filter_not_registered_in_wporg_build(): void
    {
        define('WPMGR_WPORG_BUILD', true);

        Filters\expectAdded('upgrader_package_options')->never();

        $runner = new UpdateRunner();
        $result = $this->applyWithUnsupportedType($runner);

        $this->assertFalse($result['ok'], 'Unsupported type must still resolve to a failed outcome, not a fatal.');
    }

    /**
     * Self-hosted installs (WPMGR_WPORG_BUILD undefined) are UNCHANGED — the
     * filter is still registered for THIS upgrade to force a clean working
     * directory, exactly as before this fix.
     */
    public function test_filter_registered_in_default_build(): void
    {
        // WPMGR_WPORG_BUILD intentionally left undefined in this process —
        // mirrors a self-hosted install.
        Filters\expectAdded('upgrader_package_options')->once();

        $runner = new UpdateRunner();
        $result = $this->applyWithUnsupportedType($runner);

        $this->assertFalse($result['ok'], 'Unsupported type must still resolve to a failed outcome, not a fatal.');
    }
}
