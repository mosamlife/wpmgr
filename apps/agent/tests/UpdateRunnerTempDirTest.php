<?php
/**
 * UpdateRunnerTempDirTest — agent-only regression fix (0.61.21, GitHub issue
 * #131 second follow-up).
 *
 * 0.61.20 pinned WP_TEMP_DIR to wp-content/upgrade whenever that directory
 * was PRESENT and WRITABLE — but never checked whether WordPress's own
 * DEFAULT temp dir already worked. On a STANDARD host (the real production
 * report this fix is for: a plain /var/www/html install, wp-content/upgrade
 * both present AND writable, WooCommerce update), that unconditional pin was
 * itself the bug: wp-content/upgrade is the EXACT directory
 * WP_Upgrader::unpack_package() uses as its own unzip working directory, and
 * that method unconditionally DELETES every existing entry directly under
 * wp-content/upgrade/ as its first step — including the update package
 * download_package()/wp_tempnam() had JUST written there because WP_TEMP_DIR
 * was pinned to that same directory. The download was wiped before it could
 * be unzipped; upgrade() returned false with no directory copy ever having
 * started — the reported bare "Update failed." with no downstream
 * isComplete() "Reason:" suffix. See UpdateRunner::pinTempDirForUnpack()'s
 * class doc for the full mechanism (cited from WordPress core's own
 * unpack_package() source).
 *
 * UpdateRunner::pinTempDirForUnpack() now checks whether WordPress's OWN
 * default temp dir (get_temp_dir(), proven usable with a real
 * create+write+delete probe, not just is_dir()/is_writable()) already works
 * BEFORE ever considering a pin. Pinning is now a FALLBACK, applied only when
 * that default is not usable — and even then, to a dedicated directory that
 * is NEVER wp-content/upgrade or any path underneath it (a wp_upload_dir()-
 * based subdirectory instead), so the pin itself can never again collide with
 * WP_Upgrader's own use of that folder.
 *
 * All three tests below run in a SEPARATE PROCESS: WP_TEMP_DIR/WP_CONTENT_DIR
 * are real PHP constants that, once defined, can never be undefined for the
 * rest of a PHPUnit process. Running in-process after any other test in this
 * file (or elsewhere in the suite) that already pinned WP_TEMP_DIR would make
 * `defined('WP_TEMP_DIR')` trivially true regardless of what this fix
 * actually does, defeating the regression lock — see
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
     * THE REGRESSION LOCK — GitHub issue #131 second follow-up (agent
     * 0.61.21). Mirrors the real production report EXACTLY: WordPress's own
     * default temp dir is writable (a standard host, not open_basedir), AND
     * wp-content/upgrade is ALSO present + writable (0.61.20's own gate would
     * have passed). Run this test against the pre-fix (0.61.20) code and it
     * fails: that code never even looks at the default temp dir — it pins to
     * wp-content/upgrade unconditionally the moment that directory's own
     * writability check passes, which is exactly the collision that broke
     * this real-world update.
     *
     * @runInSeparateProcess
     */
    public function test_temp_dir_not_pinned_when_default_temp_usable(): void
    {
        Functions\when('get_temp_dir')->justReturn('/wpmgr-fake-default-temp/');
        // The default AND wp-content/upgrade are BOTH reported writable here
        // — proving the fix chooses the default over pinning, not merely
        // that pinning would have failed anyway.
        Functions\when('is_dir')->justReturn(true);
        Functions\when('file_put_contents')->justReturn(1);
        Functions\when('wp_delete_file')->justReturn(null);

        $runner = new UpdateRunner();
        $note   = $this->pin($runner);

        $this->assertFalse(
            defined('WP_TEMP_DIR'),
            'REGRESSION: WordPress\'s own default temp dir being writable must leave WP_TEMP_DIR UNSET — '
            . 'the pre-fix code pinned to wp-content/upgrade unconditionally whenever THAT directory was '
            . 'writable, without ever checking whether the default already worked, which is exactly the '
            . 'collision that broke every plugin/theme update on this standard host'
        );
        $this->assertStringContainsString('/wpmgr-fake-default-temp', $note);
        $this->assertStringContainsString('leaving WP_TEMP_DIR unset', $note);
    }

    /**
     * The #131 open_basedir/RunCloud-class host: WordPress's own default temp
     * dir is NOT usable here. Proves the fallback still activates — and,
     * critically, pins to a dedicated directory that is NOT wp-content/upgrade
     * itself (the exact directory whose own cleanup logic caused the
     * regression this fix exists for — see this class's doc).
     *
     * @runInSeparateProcess
     */
    public function test_temp_dir_pinned_to_dedicated_subdir_when_default_unusable(): void
    {
        Functions\when('get_temp_dir')->justReturn('/wpmgr-fake-unusable-default/');
        Functions\when('is_dir')->justReturn(true);
        Functions\when('wp_mkdir_p')->justReturn(true);
        Functions\when('wp_upload_dir')->justReturn([
            'basedir' => '/wpmgr-fake-uploads',
            'error'   => false,
        ]);
        Functions\when('wp_delete_file')->justReturn(null);

        // Differentiate writability by path: the "default" is unusable, the
        // fallback (uploads-based) candidate IS.
        Functions\when('file_put_contents')->alias(
            static function (string $path, mixed $data = ''): int|false {
                return str_starts_with($path, '/wpmgr-fake-unusable-default') ? false : 1;
            }
        );

        $runner = new UpdateRunner();
        $note   = $this->pin($runner);

        $this->assertTrue(
            defined('WP_TEMP_DIR'),
            'the #131 open_basedir/RunCloud case must still pin when the default genuinely does not work'
        );
        $this->assertSame('/wpmgr-fake-uploads/.wpmgr-tmp', WP_TEMP_DIR);
        $this->assertStringNotContainsString(
            'upgrade',
            (string) WP_TEMP_DIR,
            'the fallback pin must NEVER be wp-content/upgrade or a path underneath it — that directory\'s '
            . 'own unpack_package() cleanup is exactly what this fix exists to stop colliding with'
        );
        $this->assertStringContainsString('pinned to a dedicated fallback dir', $note);
    }

    /**
     * Respects an operator's own WP_TEMP_DIR override (or a prior call in
     * the same request): never redefine a PHP constant — that is a fatal
     * error, not merely a bug — and never second-guess an explicit choice
     * made elsewhere.
     *
     * @runInSeparateProcess
     */
    public function test_existing_wp_temp_dir_define_respected(): void
    {
        define('WP_TEMP_DIR', '/already/defined/path');

        // These must never even be consulted: an already-defined
        // WP_TEMP_DIR short-circuits before any filesystem probe runs.
        Functions\when('get_temp_dir')->justReturn('/should/never/be/read/');
        Functions\when('is_dir')->justReturn(true);
        Functions\when('file_put_contents')->justReturn(1);

        $runner = new UpdateRunner();
        $note   = $this->pin($runner);

        $this->assertSame('/already/defined/path', WP_TEMP_DIR);
        $this->assertStringContainsString('already defined', $note);
    }
}
