<?php
/**
 * Tests that plugin activation never fatals when the keystore master key cannot
 * be established: activation must succeed, set a persistent admin-notice option,
 * and retry lazily on later admin loads.
 *
 * Runs in a separate process because it drives the Plugin singleton and depends
 * on process-global constants used by master-key resolution.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\Attributes\PreserveGlobalState;
use PHPUnit\Framework\Attributes\RunTestsInSeparateProcesses;
use WPMgr\Agent\Plugin;
use WPMgr\Agent\Schema;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Plugin
 */
#[RunTestsInSeparateProcesses]
#[PreserveGlobalState(false)]
final class PluginActivationTest extends TestCase
{
    /** @var array<string,mixed> In-memory wp-option store. */
    private array $options = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->options = [];

        // Hook/registration no-ops used during boot().
        foreach (['add_action', 'add_filter', 'register_activation_hook',
                  'register_deactivation_hook'] as $fn) {
            Functions\when($fn)->justReturn(true);
        }
        Functions\when('is_admin')->justReturn(false);

        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;
            return true;
        });
        Functions\when('get_option')->alias(function ($name, $default = false) {
            return $this->options[$name] ?? $default;
        });
        Functions\when('delete_option')->alias(function ($name) {
            unset($this->options[$name]);
            return true;
        });

        // Force every master-key source to fail:
        //  - no WPMGR_AGENT_KEY_FILE constant,
        //  - no usable salts,
        //  - ABSPATH parent is a non-existent, non-creatable path.
        if (!defined('ABSPATH')) {
            // A path under /proc cannot be created or written on Linux/macOS.
            define('ABSPATH', '/nonexistent-wpmgr-' . bin2hex(random_bytes(6)) . '/x/y/z/site/');
        }
        // No WP_CONTENT_DIR, no wp_upload_dir stub -> those candidates are skipped.
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_activation_does_not_throw_and_sets_notice_when_keystore_fails(): void
    {
        $plugin = Plugin::boot();

        // Must NOT throw, even though no master-key source is available.
        $plugin->activate();

        $this->assertArrayHasKey(
            Plugin::OPTION_KEYSTORE_ERROR,
            $this->options,
            'A persistent keystore-error option must be set.'
        );
        $this->assertIsString($this->options[Plugin::OPTION_KEYSTORE_ERROR]);
        $this->assertStringContainsString(
            'WPMGR_AGENT_KEY_FILE',
            $this->options[Plugin::OPTION_KEYSTORE_ERROR]
        );

        // Activation still recorded its timestamp -> activation succeeded.
        $this->assertArrayHasKey('wpmgr_agent_activated_at', $this->options);
    }

    public function test_keypair_is_not_persisted_when_master_key_unavailable(): void
    {
        $plugin = Plugin::boot();
        $plugin->activate();

        // No site keypair should have been written (encrypt would have failed).
        $this->assertArrayNotHasKey('wpmgr_agent_site_keypair', $this->options);
    }

    public function test_activation_runs_schema_migrations_and_stamps_db_version(): void
    {
        // Provide a $wpdb double + a dbDelta() shim so Schema::ensureCurrent
        // can complete and bump the schema-version option. Without these,
        // Schema bails silently (correct production behavior outside WP).
        $GLOBALS['wpdb'] = new class {
            public string $prefix = 'wp_';
            public function get_charset_collate(): string
            {
                return '';
            }
        };
        if (!function_exists('dbDelta')) {
            eval('function dbDelta(string $sql): array { return []; }');
        }

        $plugin = Plugin::boot();
        $plugin->activate();

        $this->assertArrayHasKey(
            Schema::OPTION_DB_VERSION,
            $this->options,
            'Activation must invoke Schema::ensureCurrent (sets the db-version option).'
        );
        $this->assertSame(Schema::CURRENT_VERSION, $this->options[Schema::OPTION_DB_VERSION]);

        unset($GLOBALS['wpdb']);
    }
}
