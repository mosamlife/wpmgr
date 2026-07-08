<?php
/**
 * UpdateRunnerTempDirTest — agent-only regression fix (0.61.20, GitHub issue
 * #131 follow-up).
 *
 * B3 (#131) pinned WP_TEMP_DIR to wp-content/upgrade guarded ONLY by
 * is_dir() — no writability check. On a host where wp-content/upgrade
 * EXISTS but is NOT writable in the request's execution context
 * (open_basedir/RunCloud-style hosts), that unconditional pin broke EVERY
 * plugin/theme update there: WordPress's download_url()/unzip_file() then
 * tried to write INTO the pinned-but-unwritable directory instead of
 * falling back to whatever location (sys_get_temp_dir()/upload_tmp_dir)
 * actually worked before #131 — the apply failed, and the #131
 * isComplete() guard correctly (but unhelpfully) rolled the failed apply
 * back, exactly the "worked before #131, fails after" symptom reported.
 *
 * UpdateRunner::pinTempDirForUnpack() (extracted from applyViaUpgrader() as
 * part of this fix) now checks is_writable() too and skips the define()
 * entirely when the directory is not writable, leaving WP_TEMP_DIR
 * undefined so WordPress's own get_temp_dir() resolves the fallback
 * itself — the pre-#131 behaviour that worked on this class of host.
 *
 * Both tests below run in a SEPARATE PROCESS: WP_TEMP_DIR/WP_CONTENT_DIR
 * are real PHP constants that, once defined, can never be undefined for
 * the rest of a PHPUnit process. Running in-process after any other test
 * in this file (or elsewhere in the suite) that already pinned WP_TEMP_DIR
 * would make `defined('WP_TEMP_DIR')` trivially true regardless of what
 * this fix actually does, defeating the regression lock — see
 * Security/HideBackendModuleTest.php for the same idiom used for the same
 * reason (a process-lifetime latch/constant).
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Support\UpdateRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\UpdateRunner
 */
final class UpdateRunnerTempDirTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        // Fresh per-process value — @runInSeparateProcess guarantees this is
        // never already defined by an earlier test file, but guard it the
        // same way the rest of this suite does for constants shared across
        // files (see SnapshotManagerTest's guard-define idiom).
        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', sys_get_temp_dir() . '/wpmgr-tempdir-test-' . bin2hex(random_bytes(6)));
        }
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    /** Invoke the private pinTempDirForUnpack() via reflection. */
    private function pin(UpdateRunner $runner): string
    {
        // No setAccessible() call needed — ReflectionMethod::invoke() has
        // been able to call non-public methods directly since PHP 8.1 (see
        // SnapshotManagerTest::realLiveDir() for the same idiom).
        $method = new \ReflectionMethod(UpdateRunner::class, 'pinTempDirForUnpack');

        return (string) $method->invoke($runner);
    }

    /**
     * CONTROL — the writable-host case is UNCHANGED from #131: when
     * wp-content/upgrade exists AND is writable in this execution context,
     * the pin is still applied exactly as before. Proves the fix only ADDS
     * a fallback; it never weakens the #131 hardening on a host where it
     * already worked.
     *
     * @runInSeparateProcess
     */
    public function test_temp_dir_pinned_when_upgrade_dir_writable(): void
    {
        Functions\when('is_dir')->justReturn(true);
        Functions\when('is_writable')->justReturn(true);

        $runner = new UpdateRunner();
        $note   = $this->pin($runner);

        $expected = rtrim((string) WP_CONTENT_DIR, '/\\') . '/upgrade';

        $this->assertTrue(
            defined('WP_TEMP_DIR'),
            'a writable upgrade dir must still be pinned — unchanged #131 behaviour'
        );
        $this->assertSame($expected, WP_TEMP_DIR);
        $this->assertStringContainsString('pinned to', $note);
    }

    /**
     * THE REGRESSION LOCK — GitHub issue #131 follow-up (agent 0.61.20).
     * Before this fix, wp-content/upgrade merely satisfying is_dir()=true
     * was enough to pin WP_TEMP_DIR, REGARDLESS of writability — exactly
     * the scenario that broke every plugin/theme update (WooCommerce free
     * + a premium plugin both failing + rolling back) on an
     * open_basedir/RunCloud host where wp-content/upgrade exists but isn't
     * writable in this request's execution context.
     *
     * Proves the fix: is_writable()=false must leave WP_TEMP_DIR UNDEFINED
     * so WordPress's own get_temp_dir() fallback
     * (sys_get_temp_dir()/upload_tmp_dir) — the pre-#131 behaviour that
     * worked on this class of host — is used instead of a broken pin. Run
     * this test against the pre-fix (is_dir-only) code and it fails: that
     * code pins WP_TEMP_DIR unconditionally once is_dir() is true, with no
     * writability check at all.
     *
     * @runInSeparateProcess
     */
    public function test_temp_dir_pin_skipped_when_not_writable(): void
    {
        Functions\when('is_dir')->justReturn(true);
        Functions\when('is_writable')->justReturn(false);

        $runner = new UpdateRunner();
        $note   = $this->pin($runner);

        $this->assertFalse(
            defined('WP_TEMP_DIR'),
            'REGRESSION: wp-content/upgrade existing (is_dir=true) but NOT writable must NOT pin '
            . 'WP_TEMP_DIR — the pre-fix is_dir()-only check pinned regardless of writability, '
            . 'breaking every plugin/theme update on this class of host'
        );
        $this->assertStringContainsString('not writable', $note);
        $this->assertStringContainsString('skipped', $note);
    }

    /**
     * Respects an operator's own WP_TEMP_DIR override (or a prior call in
     * the same request): never redefine a PHP constant — that is a fatal
     * error, not merely a bug — and never second-guess an explicit choice
     * made elsewhere.
     *
     * @runInSeparateProcess
     */
    public function test_temp_dir_pin_skipped_when_wp_temp_dir_already_defined(): void
    {
        define('WP_TEMP_DIR', '/already/defined/path');

        // These must never even be consulted: an already-defined
        // WP_TEMP_DIR short-circuits before any filesystem probe runs.
        Functions\when('is_dir')->justReturn(true);
        Functions\when('is_writable')->justReturn(true);

        $runner = new UpdateRunner();
        $note   = $this->pin($runner);

        $this->assertSame('/already/defined/path', WP_TEMP_DIR);
        $this->assertStringContainsString('already defined', $note);
    }
}
