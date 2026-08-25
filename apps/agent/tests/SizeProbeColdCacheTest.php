<?php
/**
 * GH #521 regression: SizeProbe's `pre_recurse_dirsize` callback must accept
 * the FALSE that WordPress core passes as the fifth argument when the
 * `dirsize_cache` transient is cold.
 *
 * Core seeds that argument from a transient and does not normalise it first:
 *
 *     // wp-includes/functions.php, recurse_dirsize()
 *     if ( ! isset( $directory_cache ) ) {
 *         $directory_cache = get_transient( 'dirsize_cache' );   // false when cold
 *     }
 *     ...
 *     $size = apply_filters( 'pre_recurse_dirsize', false, $directory, $exclude, $max_execution_time, $directory_cache );
 *     ...
 *     if ( ! is_array( $directory_cache ) ) {   // normalised AFTER the filter
 *         $directory_cache = array();
 *     }
 *
 * get_transient() returns false when the transient is missing or expired, so an
 * `array` hint on that parameter was an uncaught TypeError. This one is the
 * worst of the set because it needs no setting at all: Plugin::registerHooks()
 * registers the filter unconditionally on every boot, and it could not
 * self-heal — the transient is only written after the directory walk that the
 * fatal prevented, so it repeated on every request. Triggers included a fresh
 * install, any transient or object-cache flush, Site Health -> Info ->
 * Directory sizes, the agent's own daily size walk, and on multisite every
 * media upload via get_space_used().
 *
 * The cases below bind exactly what core binds. No existing test constructed
 * the cold-cache call at all.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Diagnostics\SizeProbe;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Diagnostics\SizeProbe
 */
final class SizeProbeColdCacheTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        // '0' short-circuits execAvailable() to false, so the callback returns
        // false and lets core do its own recursion. That keeps these tests
        // deterministic and off the du(1) shell-out entirely: what is under
        // test is argument binding, not the byte count.
        Functions\when('get_transient')->justReturn('0');
        Functions\when('set_transient')->justReturn(true);
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * The reported fatal. Before the fix this dies with
     *   TypeError: ...preRecurseDirsizeFilter(): Argument #5 ($directoryCache)
     *   must be of type array, bool given
     */
    public function test_cold_dirsize_transient_does_not_fatal(): void
    {
        $probe = new SizeProbe();

        $result = $probe->preRecurseDirsizeFilter(false, sys_get_temp_dir(), null, 30, false);

        $this->assertFalse(
            $result,
            'with exec unavailable the callback must hand the walk back to core, not throw'
        );
    }

    /** A warm cache is an array, and that path must keep working unchanged. */
    public function test_warm_dirsize_cache_still_works(): void
    {
        $probe = new SizeProbe();

        $result = $probe->preRecurseDirsizeFilter(
            false,
            sys_get_temp_dir(),
            null,
            30,
            ['/some/dir' => 1234]
        );

        $this->assertFalse($result);
    }

    /**
     * A value already filtered upstream is passed straight through, cold cache
     * or not — this callback must never clobber another plugin's answer.
     */
    public function test_upstream_size_is_returned_untouched_on_a_cold_cache(): void
    {
        $probe = new SizeProbe();

        $this->assertSame(
            4242,
            $probe->preRecurseDirsizeFilter(4242, sys_get_temp_dir(), null, 30, false)
        );
    }

    /**
     * Core marks arguments 3-5 optional. A third-party plugin applying the
     * filter with fewer arguments must not produce an ArgumentCountError.
     */
    public function test_filter_tolerates_being_applied_with_fewer_arguments(): void
    {
        $probe = new SizeProbe();

        $this->assertFalse($probe->preRecurseDirsizeFilter(false, sys_get_temp_dir()));
    }
}
