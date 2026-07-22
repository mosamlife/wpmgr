<?php
/**
 * GH #268 regression: WpConfigEditor::setConstant() must be a pure no-op —
 * NEVER writing to wp-config.php — once a constant is already defined at PHP
 * runtime with the desired value, e.g. by a Roots/Bedrock install (which
 * manages constants via `Roots\WPConfig\Config::define()` and throws a
 * `ConstantAlreadyDefinedException` on any raw redefinition). Before this
 * fix, WpConfigEditor unconditionally inserted a raw `define()` at the top of
 * wp-config.php, which fataled every request on such a site.
 *
 * PHP constants cannot be undefined once defined in a process, so every test
 * here that `define()`s DISALLOW_FILE_EDIT for real must run in its own
 * separate process — the same discipline used by KeystoreMasterKeyTest.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use PHPUnit\Framework\Attributes\PreserveGlobalState;
use PHPUnit\Framework\Attributes\RunTestsInSeparateProcesses;
use WPMgr\Agent\Cache\WpConfigEditor;
use WPMgr\Agent\Security\HardeningConfig;
use WPMgr\Agent\Security\HardeningModule;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Cache\WpConfigEditor
 * @covers \WPMgr\Agent\Security\HardeningModule
 */
#[RunTestsInSeparateProcesses]
#[PreserveGlobalState(false)]
final class CacheWpConfigEditorManagedRuntimeTest extends TestCase
{
    private string $dir = '';

    private string $configPath = '';

    protected function set_up(): void
    {
        parent::set_up();
        $this->dir = sys_get_temp_dir() . '/wpmgr-cfg-rt-' . uniqid('', true);
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

    // -------------------------------------------------------------------------
    // Test 1 (the #268 regression test): already-defined-true (Bedrock) is a
    // pure no-op — nothing is written.
    // -------------------------------------------------------------------------

    public function test_set_constant_is_a_pure_noop_when_constant_already_defined_true(): void
    {
        // Simulate a Bedrock site: DISALLOW_FILE_EDIT is already defined true
        // by the time our command runs (Roots\WPConfig\Config, or any other
        // layer, resolved before this command executes).
        define('DISALLOW_FILE_EDIT', true);

        $editor = new WpConfigEditor($this->configPath);
        $before = (string) file_get_contents($this->configPath);

        $this->assertTrue($editor->setConstant('DISALLOW_FILE_EDIT', true));

        $after = (string) file_get_contents($this->configPath);
        $this->assertSame(
            $before,
            $after,
            'wp-config.php must be byte-for-byte unchanged when the constant is already correctly defined'
        );
        $this->assertStringNotContainsString(
            'DISALLOW_FILE_EDIT',
            $after,
            'no define line for this constant may be added'
        );
        $this->assertNotNull($editor->lastNotice(), 'an informational (non-error) notice must be available');
    }

    public function test_set_constant_is_a_noop_when_defined_with_a_different_value(): void
    {
        // Item 2 of the fix design: defined, but not with the value we asked
        // for. This is reachable on ordinary sites (a host or user can set
        // DISALLOW_FILE_EDIT=false to keep the editor on); the guard must refuse
        // to overwrite a constant owned by another layer rather than silently
        // reassigning it. (The runtime user_has_cap filter still enforces the
        // disable-editor toggle regardless of this file constant.)
        define('DISALLOW_FILE_EDIT', false);

        $editor = new WpConfigEditor($this->configPath);
        $before = (string) file_get_contents($this->configPath);

        $this->assertTrue($editor->setConstant('DISALLOW_FILE_EDIT', true));

        $after = (string) file_get_contents($this->configPath);
        $this->assertSame($before, $after, 'a conflicting runtime value must still never trigger a write');
        $this->assertNotNull($editor->lastNotice());
    }

    // -------------------------------------------------------------------------
    // Test 5: HardeningModule::syncWpConfigFileEdit() — the actual #268 call
    // path — returns success (no fatal, no error) when the constant is
    // already defined.
    // -------------------------------------------------------------------------

    public function test_hardening_module_sync_wp_config_file_edit_succeeds_when_already_defined(): void
    {
        define('DISALLOW_FILE_EDIT', true);

        // syncWpConfigFileEdit() constructs the DEFAULT (ABSPATH-resolved)
        // WpConfigEditor, so a real wp-config.php must exist at ABSPATH for
        // isWritable() to be true and the (skipped) write path to be
        // reachable at all. bootstrap.php only creates dirname(ABSPATH), so
        // create ABSPATH itself here (same convention as FileManagerCommandsTest).
        $abspath = rtrim((string) constant('ABSPATH'), '/\\');
        if (!is_dir($abspath)) {
            mkdir($abspath, 0755, true);
        }
        $realConfigPath = $abspath . '/wp-config.php';
        file_put_contents($realConfigPath, "<?php\n/* wp-config */\ndefine('DB_NAME', 'wp');\n");

        try {
            $config = HardeningConfig::fromArray(['config' => ['disable_file_editor' => true]]);
            $module = new HardeningModule();

            $result = $module->syncWpConfigFileEdit($config);

            $this->assertTrue($result, 'must report success — never a fatal/error — when the constant is already enforced');
            $this->assertNotNull(
                $module->lastWpConfigNotice(),
                'an informational (non-error) notice must be surfaced for the dashboard'
            );

            $after = (string) file_get_contents($realConfigPath);
            $this->assertStringNotContainsString(
                'DISALLOW_FILE_EDIT',
                $after,
                'no raw define may be inserted when the constant is already enforced elsewhere'
            );
        } finally {
            @unlink($realConfigPath);
        }
    }
}
