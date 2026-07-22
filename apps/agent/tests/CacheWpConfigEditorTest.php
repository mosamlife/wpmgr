<?php
/**
 * WpConfigEditor tests on a temp-dir wp-config.php fixture: idempotent add
 * (WP_CACHE added exactly once), clean removal, atomic write never corrupts,
 * and a foreign existing define is replaced rather than duplicated.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Cache\WpConfigEditor;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Cache\WpConfigEditor
 */
final class CacheWpConfigEditorTest extends TestCase
{
    private string $dir = '';

    private string $configPath = '';

    protected function set_up(): void
    {
        parent::set_up();
        $this->dir = sys_get_temp_dir() . '/wpmgr-cfg-' . uniqid('', true);
        mkdir($this->dir, 0o777, true);
        $this->configPath = $this->dir . '/wp-config.php';
        file_put_contents(
            $this->configPath,
            "<?php\n/* wp-config */\ndefine('DB_NAME', 'wp');\n\nrequire_once ABSPATH . 'wp-settings.php';\n"
        );
    }

    protected function tear_down(): void
    {
        foreach (glob($this->dir . '/*') ?: [] as $f) {
            @unlink($f);
        }
        @rmdir($this->dir);
        parent::tear_down();
    }

    private function editor(): WpConfigEditor
    {
        return new WpConfigEditor($this->configPath);
    }

    public function test_set_constant_adds_define_once(): void
    {
        $editor = $this->editor();
        $this->assertTrue($editor->setConstant('WP_CACHE', true));

        $content = (string) file_get_contents($this->configPath);
        $this->assertSame(1, substr_count($content, "define('WP_CACHE'"));
        $this->assertStringContainsString("define('WP_CACHE', true);", $content);

        // The original content survives intact.
        $this->assertStringContainsString("define('DB_NAME', 'wp');", $content);
        $this->assertStringContainsString("require_once ABSPATH . 'wp-settings.php';", $content);
    }

    public function test_set_constant_is_idempotent(): void
    {
        $editor = $this->editor();
        $editor->setConstant('WP_CACHE', true);
        $first = (string) file_get_contents($this->configPath);

        // Second call is a no-op: same value already present.
        $this->assertTrue($editor->setConstant('WP_CACHE', true));
        $second = (string) file_get_contents($this->configPath);

        $this->assertSame($first, $second, 'idempotent set must not rewrite the file');
        $this->assertSame(1, substr_count($second, "define('WP_CACHE'"));
    }

    public function test_set_constant_replaces_existing_value(): void
    {
        // Seed a pre-existing WP_CACHE=false define.
        file_put_contents(
            $this->configPath,
            "<?php\ndefine('WP_CACHE', false);\ndefine('DB_NAME', 'wp');\n"
        );
        $editor = $this->editor();
        $this->assertTrue($editor->setConstant('WP_CACHE', true));

        $content = (string) file_get_contents($this->configPath);
        $this->assertSame(1, substr_count($content, "define('WP_CACHE'"), 'must not duplicate the define');
        $this->assertStringContainsString("define('WP_CACHE', true);", $content);
        $this->assertStringNotContainsString('false', $content);
    }

    public function test_remove_constant_is_clean_and_idempotent(): void
    {
        $editor = $this->editor();
        $editor->setConstant('WP_CACHE', true);

        $this->assertTrue($editor->removeConstant('WP_CACHE'));
        $content = (string) file_get_contents($this->configPath);
        $this->assertStringNotContainsString('WP_CACHE', $content);
        // Other defines remain.
        $this->assertStringContainsString("define('DB_NAME', 'wp');", $content);

        // Removing again is a no-op success.
        $this->assertTrue($editor->removeConstant('WP_CACHE'));
    }

    public function test_round_trip_returns_to_original(): void
    {
        $original = (string) file_get_contents($this->configPath);
        $editor   = $this->editor();
        $editor->setConstant('WP_CACHE', true);
        $editor->removeConstant('WP_CACHE');
        $after = (string) file_get_contents($this->configPath);

        $this->assertStringNotContainsString('WP_CACHE', $after);
        // The original define + require lines are preserved.
        $this->assertStringContainsString("define('DB_NAME', 'wp');", $after);
        $this->assertStringContainsString("wp-settings.php", $after);
    }

    public function test_is_writable_reflects_file_state(): void
    {
        $this->assertTrue($this->editor()->isWritable());

        $missing = new WpConfigEditor($this->dir . '/does-not-exist.php');
        $this->assertFalse($missing->isWritable());
    }

    public function test_atomic_write_leaves_no_temp_files(): void
    {
        $editor = $this->editor();
        $editor->setConstant('WP_CACHE', true);
        $temps = glob($this->dir . '/*wpmgr-tmp*');
        $this->assertSame([], $temps ?: [], 'no temp files should remain after an atomic write');
    }

    public function test_invalid_constant_name_is_rejected(): void
    {
        $editor = $this->editor();
        $this->assertFalse($editor->setConstant('BAD NAME', true));
        $this->assertFalse($editor->setConstant('1BAD', true));
    }

    public function test_set_constant_disallow_file_editor_writes_on_standard_wp(): void
    {
        // GH #268 baseline: standard WP (no framework markers, constant not
        // defined) must be entirely unaffected by the managed-config guard —
        // the define is inserted exactly as before.
        $editor = $this->editor();
        $this->assertTrue($editor->setConstant('DISALLOW_FILE_EDIT', true));

        $content = (string) file_get_contents($this->configPath);
        $this->assertStringContainsString("define('DISALLOW_FILE_EDIT', true); // Added by WPMgr Cache", $content);
        $this->assertNull($editor->lastNotice(), 'a real write must not carry an informational notice');
    }

    // -------------------------------------------------------------------------
    // GH #268 — Roots/Bedrock managed wp-config.php: never insert a raw define
    // -------------------------------------------------------------------------

    public function test_is_managed_wp_config_via_application_php_require_skips_write(): void
    {
        // Bedrock's own web/wp-config.php shape: no raw defines for WP
        // constants at all — they live in the required config/application.php,
        // which this test never actually requires (only the STRING signal
        // matters to isManagedWpConfig()).
        file_put_contents(
            $this->configPath,
            "<?php\nrequire_once __DIR__ . '/../config/application.php';\n"
        );

        $editor = $this->editor();
        $before = (string) file_get_contents($this->configPath);

        $this->assertTrue($editor->setConstant('DISALLOW_FILE_EDIT', true));

        $after = (string) file_get_contents($this->configPath);
        $this->assertSame($before, $after, 'a Bedrock-managed wp-config.php must be left byte-for-byte unchanged');
        $this->assertStringNotContainsString('DISALLOW_FILE_EDIT', $after);
        $this->assertNotNull($editor->lastNotice(), 'a managed-config skip must surface an informational notice');
    }

    public function test_is_managed_wp_config_via_roots_wpconfig_class_skips_write(): void
    {
        file_put_contents(
            $this->configPath,
            "<?php\nuse Roots\\WPConfig\\Config;\nConfig::define('WP_ENV', 'production');\n"
        );

        $editor = $this->editor();
        $before = (string) file_get_contents($this->configPath);

        $this->assertTrue($editor->setConstant('DISALLOW_FILE_EDIT', true));

        $after = (string) file_get_contents($this->configPath);
        $this->assertSame($before, $after, 'a Roots\\WPConfig\\Config-managed wp-config.php must be left unchanged');
        $this->assertStringNotContainsString('DISALLOW_FILE_EDIT', $after);
    }

    public function test_remove_constant_on_managed_config_without_our_marker_is_a_safe_noop(): void
    {
        $managedContent = "<?php\nrequire_once __DIR__ . '/../config/application.php';\n";
        file_put_contents($this->configPath, $managedContent);

        $editor = $this->editor();

        // Never had our marker (or any raw define for this name) — must be a
        // clean no-op: no throw, file untouched.
        $this->assertTrue($editor->removeConstant('DISALLOW_FILE_EDIT'));

        $after = (string) file_get_contents($this->configPath);
        $this->assertSame($managedContent, $after, 'removeConstant must not touch a managed wp-config.php that never had our define');
    }
}
