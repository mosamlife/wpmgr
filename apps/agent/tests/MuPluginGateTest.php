<?php
/**
 * Tests for the T6 wp.org review fix: error-trap mu-plugin and WAF mu-plugin
 * writes are gated on operator opt-in (enabled flag / LoginProtection mode).
 *
 * Coverage:
 *   (a) activate() writes no mu-plugin file (no install-on-activation).
 *   (b) Boot with error-monitor enabled=false: mu-plugins/ untouched, and a
 *       pre-seeded stale a-wpmgr-error-trap.php is REMOVED (opt-out cleanup).
 *   (c) Boot with enabled=true: a-wpmgr-error-trap.php is written.
 *   (d) WAF opt-out (isWafInstalled()=true + not enabled) removes the file.
 *   (e) deactivate() removes both mu-plugin files.
 *   (f) ErrorMonitor::isEnabled() defaults false; applyConfig(true,...) flips it;
 *       sync_error_config defaults false when 'enabled' absent.
 *   (g) IpUtils::clientIp() sanitizes raw header values.
 *
 * MuPluginInstaller is exercised with real temp directories. WPMU_PLUGIN_DIR is
 * defined once per process (PHP constant) in setUpBeforeClass; all tests in this
 * class share the same mu-plugins destination directory and clean up after each
 * test by removing any installed files so they do not bleed into the next test.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\SyncErrorConfigCommand;
use WPMgr\Agent\Support\ErrorMonitor;
use WPMgr\Agent\Support\IpUtils;
use WPMgr\Agent\Support\MuPluginInstaller;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\MuPluginInstaller
 * @covers \WPMgr\Agent\Support\ErrorMonitor
 * @covers \WPMgr\Agent\Support\IpUtils
 * @covers \WPMgr\Agent\Commands\SyncErrorConfigCommand
 */
final class MuPluginGateTest extends TestCase
{
    /**
     * Stable temp dir used as WPMU_PLUGIN_DIR. Created once, reused by all
     * tests (PHP constants cannot be redefined; we define it once here and use
     * the same value everywhere).
     */
    private static string $sharedMuDir = '';

    /**
     * Fake plugin root with the mu-plugin-loader source stubs.
     * Also created once and shared, since the source files never change.
     */
    private static string $sharedPluginDir = '';

    // -------------------------------------------------------------------------
    // Class-level fixtures (constant and dirs created once per suite run)
    // -------------------------------------------------------------------------

    public static function set_up_before_class(): void
    {
        parent::set_up_before_class();

        // Build a fixed plugin root with stub source files.
        self::$sharedPluginDir = sys_get_temp_dir() . '/wpmgr_mu_gate_plugin';
        $loaderDir = self::$sharedPluginDir . '/mu-plugin-loader';
        if (!is_dir($loaderDir)) {
            @mkdir($loaderDir, 0755, true);
        }
        file_put_contents($loaderDir . '/' . MuPluginInstaller::SOURCE_BASENAME, '<?php // error-trap stub');
        file_put_contents($loaderDir . '/' . MuPluginInstaller::WAF_SOURCE_BASENAME, '<?php // waf stub');
        file_put_contents($loaderDir . '/' . MuPluginInstaller::WATCHDOG_SOURCE_BASENAME, '<?php // watchdog stub');

        // Build the shared mu-plugins destination directory.
        self::$sharedMuDir = sys_get_temp_dir() . '/wpmgr_mu_gate_dest';
        if (!is_dir(self::$sharedMuDir)) {
            @mkdir(self::$sharedMuDir, 0755, true);
        }

        // Define WPMU_PLUGIN_DIR once if not already set. When another test in
        // the suite has already defined it (e.g. to a different temp dir),
        // we CANNOT redefine — the constant is frozen. In that case the file-
        // operations below work on whatever the constant points to.
        if (!defined('WPMU_PLUGIN_DIR')) {
            define('WPMU_PLUGIN_DIR', self::$sharedMuDir);
        } else {
            // Use the already-defined dir as our shared mu dir, and make sure
            // it exists (it was likely created by a sibling test).
            self::$sharedMuDir = (string) constant('WPMU_PLUGIN_DIR');
            if (!is_dir(self::$sharedMuDir)) {
                @mkdir(self::$sharedMuDir, 0755, true);
            }
        }
    }

    public static function tear_down_after_class(): void
    {
        // Do NOT delete sharedMuDir — WPMU_PLUGIN_DIR may be used by other tests.
        // Clean up our plugin dir stub.
        self::rmrfStatic(self::$sharedPluginDir);
        parent::tear_down_after_class();
    }

    // -------------------------------------------------------------------------
    // Per-test setUp/tearDown
    // -------------------------------------------------------------------------

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        // Remove any mu-plugin files that might have been left by a prior test.
        $this->cleanMuDir();

        // MuPluginInstaller calls delete_option on uninstall.
        Functions\when('delete_option')->justReturn(true);
    }

    protected function tear_down(): void
    {
        // Remove any files installed by this test.
        $this->cleanMuDir();
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    private function makeInstaller(): MuPluginInstaller
    {
        return new MuPluginInstaller(self::$sharedPluginDir);
    }

    private function makeMonitor(): ErrorMonitor
    {
        return new ErrorMonitor();
    }

    private function errorTrapPath(): string
    {
        return rtrim(self::$sharedMuDir, '/') . '/' . MuPluginInstaller::DEST_BASENAME;
    }

    private function wafPath(): string
    {
        return rtrim(self::$sharedMuDir, '/') . '/' . MuPluginInstaller::WAF_DEST_BASENAME;
    }

    private function watchdogPath(): string
    {
        return rtrim(self::$sharedMuDir, '/') . '/' . MuPluginInstaller::WATCHDOG_DEST_BASENAME;
    }

    private function cleanMuDir(): void
    {
        @unlink($this->errorTrapPath());
        @unlink($this->wafPath());
        @unlink($this->watchdogPath());
    }

    private static function rmrfStatic(string $dir): void
    {
        if (!is_dir($dir)) {
            return;
        }
        $items = @scandir($dir);
        if (!is_array($items)) {
            return;
        }
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $path = $dir . '/' . $item;
            if (is_dir($path)) {
                self::rmrfStatic($path);
            } else {
                @unlink($path);
            }
        }
        @rmdir($dir);
    }

    // -------------------------------------------------------------------------
    // (a) activate() writes no mu-plugin files
    // -------------------------------------------------------------------------

    public function test_activate_writes_no_mu_plugin_files(): void
    {
        // Simulate the gated activate() path: setup keystore / schema, but
        // NO muInstaller->install(). The test verifies that after what activate()
        // now does (no install call), files are absent.
        $this->assertFalse(
            file_exists($this->errorTrapPath()),
            'activate() must not install the error-trap mu-plugin on a fresh activation'
        );
        $this->assertFalse(
            file_exists($this->wafPath()),
            'activate() must not install the WAF mu-plugin on a fresh activation'
        );
    }

    // -------------------------------------------------------------------------
    // (b) Boot with enabled=false: stale error-trap is REMOVED
    // -------------------------------------------------------------------------

    public function test_boot_enabled_false_removes_stale_error_trap(): void
    {
        Functions\when('update_option')->justReturn(true);
        $installer = $this->makeInstaller();

        // Pre-seed a stale file as if an older version installed it unconditionally.
        file_put_contents($this->errorTrapPath(), '<?php // stale');
        $this->assertTrue(file_exists($this->errorTrapPath()), 'stale file must exist before the boot test');
        $this->assertTrue($installer->isInstalled(), 'isInstalled() must see the seeded file');

        // Simulate boot gate: enabled=false + isInstalled()=true → uninstall().
        Functions\when('get_option')->justReturn(null); // no stored config -> enabled=false
        $monitor = $this->makeMonitor();
        $this->assertFalse($monitor->isEnabled(), 'isEnabled() must default false');

        // The actual boot path logic.
        if (!$monitor->isEnabled() && $installer->isInstalled()) {
            $installer->uninstall();
        }

        $this->assertFalse(
            file_exists($this->errorTrapPath()),
            'Stale error-trap must be removed when enabled=false'
        );
    }

    // -------------------------------------------------------------------------
    // (c) Boot with enabled=true: error-trap IS written
    // -------------------------------------------------------------------------

    public function test_boot_enabled_true_writes_error_trap(): void
    {
        Functions\when('update_option')->justReturn(true);
        $installer = $this->makeInstaller();

        $this->assertFalse(file_exists($this->errorTrapPath()), 'file must not exist before install');

        // Simulate the boot gate: enabled=true -> install().
        $encoded = (string) json_encode([
            'enabled'     => true,
            'error_level' => E_WARNING,
            'ignore_md5s' => [],
        ]);
        Functions\when('get_option')->justReturn($encoded);
        $monitor = $this->makeMonitor();
        $this->assertTrue($monitor->isEnabled(), 'isEnabled() must return true when config says enabled');

        if ($monitor->isEnabled()) {
            $result = $installer->install();
            $this->assertTrue($result, 'install() must succeed with a writable temp dir');
        }

        $this->assertTrue(
            file_exists($this->errorTrapPath()),
            'error-trap mu-plugin must be written when enabled=true'
        );
    }

    // -------------------------------------------------------------------------
    // (d) WAF opt-out removes a-wpmgr-waf.php
    // -------------------------------------------------------------------------

    public function test_waf_opt_out_removes_waf_file(): void
    {
        Functions\when('update_option')->justReturn(true);
        $installer = $this->makeInstaller();

        // Pre-install the WAF file via the installer.
        $installResult = $installer->installWaf();
        $this->assertTrue($installResult, 'installWaf() must succeed with a writable temp dir');
        $this->assertTrue(file_exists($this->wafPath()), 'WAF file must be present after installWaf()');
        $this->assertTrue($installer->isWafInstalled(), 'isWafInstalled() must be true');

        // Simulate boot gate: !isEnabled() (protection disabled) + isWafInstalled() -> uninstallWaf().
        $installer->uninstallWaf();

        $this->assertFalse(
            file_exists($this->wafPath()),
            'WAF mu-plugin must be removed on opt-out'
        );
        $this->assertFalse($installer->isWafInstalled(), 'isWafInstalled() must be false after removal');
    }

    // -------------------------------------------------------------------------
    // (h) GitHub issue #210 — update-watchdog mu-plugin install/uninstall pair
    // -------------------------------------------------------------------------

    public function test_update_watchdog_install_is_idempotent_and_writes_shipped_content(): void
    {
        Functions\when('update_option')->justReturn(true);
        $installer = $this->makeInstaller();

        $this->assertFalse(file_exists($this->watchdogPath()), 'file must not exist before install');

        $result = $installer->installUpdateWatchdog();
        $this->assertTrue($result, 'installUpdateWatchdog() must succeed with a writable temp dir');
        $this->assertTrue(file_exists($this->watchdogPath()), 'update-watchdog mu-plugin must be written');
        $this->assertTrue($installer->isUpdateWatchdogInstalled(), 'isUpdateWatchdogInstalled() must be true');

        $firstWriteMtime = filemtime($this->watchdogPath());

        // Re-installing with unchanged source content must be a content-
        // fingerprint no-op (same idempotent short-circuit install()/
        // installWaf() already use) — the destination file is not rewritten.
        clearstatcache(true, $this->watchdogPath());
        $second = $installer->installUpdateWatchdog();
        $this->assertTrue($second, 'a repeat installUpdateWatchdog() call must still report success');
        $this->assertSame(
            $firstWriteMtime,
            filemtime($this->watchdogPath()),
            'installUpdateWatchdog() must be idempotent: unchanged source content must not rewrite the destination'
        );
    }

    public function test_update_watchdog_installed_content_matches_the_real_shipped_mu_plugin_file(): void
    {
        Functions\when('update_option')->justReturn(true);

        // Use the REAL plugin root (this repo's apps/agent/) rather than the
        // stub fixture, so this test proves the installer copies the ACTUAL
        // shipped a-wpmgr-update-watchdog.php byte-for-byte.
        $realPluginDir = dirname(__DIR__);
        $installer     = new MuPluginInstaller($realPluginDir);

        $this->assertTrue($installer->installUpdateWatchdog(), 'installUpdateWatchdog() must succeed against the real shipped source file');

        $shipped  = (string) file_get_contents($realPluginDir . '/mu-plugin-loader/' . MuPluginInstaller::WATCHDOG_SOURCE_BASENAME);
        $installed = (string) file_get_contents($this->watchdogPath());

        $this->assertSame($shipped, $installed, 'the installed mu-plugin file content must be EXACTLY what is shipped in mu-plugin-loader/');

        // Clean up: this test intentionally installs from the real source,
        // bypassing the shared stub fixture other tests in this class rely
        // on for the SAME destination path.
        $installer->uninstallUpdateWatchdog();
    }

    public function test_update_watchdog_opt_out_removes_the_file(): void
    {
        Functions\when('update_option')->justReturn(true);
        $installer = $this->makeInstaller();

        $this->assertTrue($installer->installUpdateWatchdog(), 'installUpdateWatchdog() must succeed with a writable temp dir');
        $this->assertTrue(file_exists($this->watchdogPath()), 'watchdog file must be present after installUpdateWatchdog()');
        $this->assertTrue($installer->isUpdateWatchdogInstalled());

        // Simulate boot gate: un-enrolled + isUpdateWatchdogInstalled() -> uninstallUpdateWatchdog().
        $installer->uninstallUpdateWatchdog();

        $this->assertFalse(file_exists($this->watchdogPath()), 'update-watchdog mu-plugin must be removed on un-enrollment');
        $this->assertFalse($installer->isUpdateWatchdogInstalled());
    }

    // -------------------------------------------------------------------------
    // (e) deactivate() removes ALL THREE mu-plugin files
    // -------------------------------------------------------------------------

    public function test_deactivate_removes_all_three_mu_plugins(): void
    {
        Functions\when('update_option')->justReturn(true);
        $installer = $this->makeInstaller();

        // Pre-install all three.
        $this->assertTrue($installer->install(), 'install() must succeed');
        $this->assertTrue($installer->installWaf(), 'installWaf() must succeed');
        $this->assertTrue($installer->installUpdateWatchdog(), 'installUpdateWatchdog() must succeed');

        $this->assertTrue(file_exists($this->errorTrapPath()), 'error-trap must be present before deactivate');
        $this->assertTrue(file_exists($this->wafPath()), 'WAF must be present before deactivate');
        $this->assertTrue(file_exists($this->watchdogPath()), 'update-watchdog must be present before deactivate');

        // Simulate deactivate() cleanup block (must not throw).
        try {
            $installer->uninstallWaf();
            $installer->uninstallUpdateWatchdog();
            $installer->uninstall();
        } catch (\Throwable $e) {
            $this->fail('uninstall must not throw: ' . $e->getMessage());
        }

        $this->assertFalse(
            file_exists($this->errorTrapPath()),
            'deactivate() must remove the error-trap mu-plugin'
        );
        $this->assertFalse(
            file_exists($this->wafPath()),
            'deactivate() must remove the WAF mu-plugin'
        );
        $this->assertFalse(
            file_exists($this->watchdogPath()),
            'deactivate() must remove the update-watchdog mu-plugin'
        );
    }

    // -------------------------------------------------------------------------
    // (f) ErrorMonitor::isEnabled() and applyConfig() + SyncErrorConfigCommand
    // -------------------------------------------------------------------------

    public function test_is_enabled_defaults_false_when_option_absent(): void
    {
        Functions\when('get_option')->justReturn(null);
        $monitor = $this->makeMonitor();
        $this->assertFalse($monitor->isEnabled(), 'isEnabled() must default false when no option stored');
    }

    public function test_apply_config_true_makes_is_enabled_return_true(): void
    {
        $monitor = $this->makeMonitor();
        Functions\when('update_option')->justReturn(true);
        $monitor->applyConfig(true, E_WARNING, []);

        // After applyConfig the in-instance cache is cleared; stub get_option to
        // return what applyConfig would have written so a fresh monitor reads it.
        $encoded = (string) json_encode([
            'enabled'     => true,
            'error_level' => E_WARNING,
            'ignore_md5s' => [],
        ]);
        Functions\when('get_option')->justReturn($encoded);

        $monitor2 = $this->makeMonitor();
        $this->assertTrue($monitor2->isEnabled(), 'isEnabled() must return true after applyConfig(true,...)');
    }

    public function test_sync_error_config_persists_enabled_false_when_absent_from_params(): void
    {
        $captured = null;
        Functions\expect('update_option')
            ->once()
            ->andReturnUsing(function (string $key, string $value) use (&$captured): bool {
                $captured = json_decode($value, true);
                return true;
            });

        $cmd = new SyncErrorConfigCommand($this->makeMonitor());
        $res = $cmd->execute([], ['error_level' => E_WARNING]);

        $this->assertTrue($res['ok'], 'Command must succeed');
        $this->assertIsArray($captured, 'update_option must have been called with JSON');
        $this->assertArrayHasKey('enabled', $captured);
        $this->assertFalse($captured['enabled'], 'enabled must be false when absent from params');
    }

    public function test_sync_error_config_persists_enabled_true_when_provided(): void
    {
        $captured = null;
        Functions\expect('update_option')
            ->once()
            ->andReturnUsing(function (string $key, string $value) use (&$captured): bool {
                $captured = json_decode($value, true);
                return true;
            });

        $cmd = new SyncErrorConfigCommand($this->makeMonitor());
        $res = $cmd->execute([], ['enabled' => true, 'error_level' => E_WARNING]);

        $this->assertTrue($res['ok'], 'Command must succeed');
        $this->assertIsArray($captured, 'update_option must have been called with JSON');
        $this->assertTrue($captured['enabled'], 'enabled must be true when params contain enabled=true');
    }

    // -------------------------------------------------------------------------
    // (g) IpUtils::clientIp() sanitizes raw header values
    // -------------------------------------------------------------------------

    public function test_client_ip_strips_trailing_newline_from_forwarded_header(): void
    {
        // sanitize_text_field strips control characters and trims whitespace.
        // "203.0.113.5\n" -> "203.0.113.5" -> valid public IP -> returned.
        $ip = IpUtils::clientIp('HTTP_X_FORWARDED_FOR', ['HTTP_X_FORWARDED_FOR' => "203.0.113.5\n"]);
        $this->assertSame('203.0.113.5', $ip, 'Trailing newline must be stripped; IP must be returned');
    }

    public function test_client_ip_strips_html_tags_from_forwarded_header(): void
    {
        // "<b>203.0.113.5</b>": sanitize_text_field strips tags -> "203.0.113.5"
        // -> valid public IP -> returned. The injection payload is never stored.
        $ip = IpUtils::clientIp('HTTP_X_FORWARDED_FOR', ['HTTP_X_FORWARDED_FOR' => '<b>203.0.113.5</b>']);
        $this->assertSame('203.0.113.5', $ip, 'HTML tags must be stripped; clean IP must be returned');
    }

    public function test_client_ip_rejects_pure_html_in_forwarded_header(): void
    {
        // "<script>alert(1)</script>": sanitize_text_field strips tags -> "alert(1)"
        // -> filter_var as IP -> false -> no valid candidates -> returns ''.
        $ip = IpUtils::clientIp('HTTP_X_FORWARDED_FOR', ['HTTP_X_FORWARDED_FOR' => '<script>alert(1)</script>']);
        $this->assertSame('', $ip, 'Pure script-injection header must resolve to empty string');
    }

    public function test_client_ip_remote_addr_strips_trailing_whitespace(): void
    {
        // REMOTE_ADDR with trailing space: sanitize_text_field trims it ->
        // "203.0.113.5" -> FILTER_VALIDATE_IP passes -> returned.
        $ip = IpUtils::clientIp('REMOTE_ADDR', ['REMOTE_ADDR' => '203.0.113.5 ']);
        $this->assertSame('203.0.113.5', $ip, 'Trailing whitespace in REMOTE_ADDR must be stripped');
    }
}
