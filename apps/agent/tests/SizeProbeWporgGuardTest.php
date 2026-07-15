<?php
/**
 * SizeProbeWporgGuardTest — locks down the wp.org pre-resubmission MUST-FIX:
 * the wp.org distribution build must NEVER shell out (`du`/`exec`) for
 * directory-size computation.
 *
 * `make agent-zip-wporg` injects `define('WPMGR_WPORG_BUILD', true);` into
 * the staged main file. SizeProbe::execAvailable() now short-circuits to
 * false whenever that constant is defined-true, BEFORE it ever reaches the
 * `echo wpmgrprobe` capability smoke-test or the `du -sb`/`du -sk` shell-outs
 * in duBytes(). Both duBytes() call sites (compute() and
 * preRecurseDirsizeFilter()) unconditionally gate on execAvailable() first,
 * so a false return there means duBytes() — the only method in this class
 * containing an exec() call — is categorically unreachable; this is a static
 * property of the source (see both call sites in class-size-probe.php), not
 * something that needs a live exec() interception to prove. compute() then
 * always falls through to the pure-PHP recurse_dirsize() path (phpBytes()),
 * reporting method "php".
 *
 * Deliberately does NOT intercept the real exec() PHP built-in (e.g. via
 * Patchwork's redefinable-internals + Brain\Monkey\Functions::expect()->never()).
 * exec() has two by-reference output parameters ($output, $result_code);
 * globally wrapping it for interception breaks by-reference semantics for
 * every OTHER test in the suite that spawns a real subprocess via exec()
 * (confirmed by trial: doing so turned 12 unrelated tests red with
 * "implode(): argument #2 must be of type array, null given" — the wrapped
 * exec() stopped populating $output). The reflection-based assertion below is
 * the actual regression lock; it is fully deterministic and environment-independent.
 *
 * Self-hosted installs (WPMGR_WPORG_BUILD undefined) are UNCHANGED — they
 * keep the `du` fast path via the existing function_exists/disable_functions/
 * safe_mode/open_basedir/live-smoke-test gate.
 *
 * Both tests run in a SEPARATE PROCESS: WPMGR_WPORG_BUILD and WP_CONTENT_DIR
 * are real PHP constants that, once defined, can never be undefined for the
 * rest of a PHPUnit process — see UpdateRunnerTempDirTest for the same idiom
 * used for the same reason.
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
final class SizeProbeWporgGuardTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        // Fresh per-process value — @runInSeparateProcess guarantees this is
        // never already defined by an earlier test file (see
        // UpdateRunnerTempDirTest's identical guard-define idiom).
        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', sys_get_temp_dir() . '/wpmgr-sizeprobe-test-' . bin2hex(random_bytes(6)));
        }
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * THE REGRESSION LOCK. With WPMGR_WPORG_BUILD defined true (exactly as
     * `make agent-zip-wporg` stamps the staged main file), execAvailable()
     * must return false. Since both call sites that invoke duBytes() (the
     * only method containing an exec() call) gate on this method first, a
     * false return here is dispositive: duBytes() cannot be reached. Run
     * this test against the pre-fix code and it fails: the old
     * execAvailable() never checked build identity and would run its normal
     * function_exists/disable_functions/safe_mode/open_basedir/live-smoke-test
     * chain regardless.
     *
     * @runInSeparateProcess
     */
    public function test_exec_available_is_false_in_wporg_build(): void
    {
        if (!defined('WPMGR_WPORG_BUILD')) {
            define('WPMGR_WPORG_BUILD', true);
        }

        $probe  = new SizeProbe();
        $method = new \ReflectionMethod(SizeProbe::class, 'execAvailable');

        $this->assertFalse(
            (bool) $method->invoke($probe),
            'REGRESSION: execAvailable() must return false in the wp.org build so the plugin never shells out'
        );
    }

    /**
     * End-to-end confirmation: compute() reports method "php" (never "du")
     * in the wp.org build, and the PHP recurse_dirsize() fallback produces
     * correct sizes when the `du`/exec path is skipped (the mocked
     * recurse_dirsize() return value flows straight through to the
     * persisted blob).
     *
     * @runInSeparateProcess
     */
    public function test_compute_uses_php_method_and_correct_sizes_in_wporg_build(): void
    {
        if (!defined('WPMGR_WPORG_BUILD')) {
            define('WPMGR_WPORG_BUILD', true);
        }

        Functions\when('is_dir')->justReturn(true);
        Functions\when('recurse_dirsize')->justReturn(4096);
        Functions\when('wp_get_upload_dir')->justReturn(['basedir' => WP_CONTENT_DIR . '/uploads']);
        Functions\when('disk_total_space')->justReturn(1000000000);
        Functions\when('disk_free_space')->justReturn(500000000);
        Functions\when('get_option')->justReturn(null);
        Functions\when('update_option')->justReturn(true);
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);
        Functions\when('size_format')->alias(static fn (int $bytes): string => $bytes . ' B');

        $probe = new SizeProbe();
        $blob  = $probe->compute();

        $this->assertSame(
            'php',
            $blob['method'],
            'REGRESSION: dirsize method must be "php" (never "du") in the wp.org build'
        );
        $this->assertFalse($blob['partial']);
        $this->assertArrayHasKey('wordpress_size', $blob['sizes']);
        $this->assertSame(4096, $blob['sizes']['wordpress_size']['bytes']);
    }
}
