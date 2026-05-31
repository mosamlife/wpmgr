<?php
/**
 * Tests that plugin activation never fatals when the keystore master key cannot
 * be established: activation must succeed, set a persistent admin-notice option,
 * and (separately) run schema migrations.
 *
 * In-process design (no separate-process isolation):
 * This test used to run under #[RunTestsInSeparateProcesses] because it drives
 * the Plugin singleton and the keystore's master-key resolution reads
 * process-global constants (ABSPATH / WPMGR_AGENT_KEY_FILE / WP salts) that
 * other tests in the suite also define. Under PHPUnit 10.5 + PHP 8.5 the
 * isolated-test bootstrap fatals (a `rewind()` deprecation fires inside
 * `__phpunit_run_isolated_test()`, where PHPUnit's error handler can't locate
 * the TestCase on the call stack -> NoTestCaseObjectOnCallStackException).
 *
 * The fix runs in-process and forces the master-key failure DETERMINISTICALLY,
 * independent of whatever constants earlier tests defined: we pin the keystore's
 * master-key source to 'salts' (via the wpmgr_agent_master_key_source option)
 * and leave the WP secret salts unusable. With a pinned-but-unavailable salt
 * source, Keystore::resolveMasterKey() throws rather than falling back to the
 * constant/file paths — so setupKeystore() fails regardless of a leaked
 * WPMGR_AGENT_KEY_FILE constant. The asserted activation behaviour is unchanged.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Plugin;
use WPMgr\Agent\Schema;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Plugin
 */
final class PluginActivationTest extends TestCase
{
    /** @var array<string,mixed> In-memory wp-option store. */
    private array $options = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        // Pin the master-key source to 'salts'. With the salts unusable (no
        // valid AUTH_KEY/... constants in the test runtime), resolveMasterKey()
        // throws on the pinned source -> the keystore fails deterministically,
        // regardless of any WPMGR_AGENT_KEY_FILE / ABSPATH leaked from sibling
        // tests in the same process.
        $this->options = [
            Keystore::OPTION_MASTER_KEY_SOURCE => ['source' => 'salts'],
        ];

        // Hook/registration no-ops used during boot().
        foreach (['add_action', 'add_filter', 'register_activation_hook',
                  'register_deactivation_hook'] as $fn) {
            Functions\when($fn)->justReturn(true);
        }
        Functions\when('is_admin')->justReturn(false);
        Functions\when('is_multisite')->justReturn(false);

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
        // Settings::get() prefers get_site_option() when it exists (multisite
        // network options). Back it with the same in-memory store so activation's
        // markActivated() read/write round-trips deterministically in-process.
        Functions\when('get_site_option')->alias(function ($name, $default = false) {
            return $this->options[$name] ?? $default;
        });
        Functions\when('update_site_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;
            return true;
        });
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
        // Use the shared dbDelta() capture bridge (declared by SchemaTest's
        // TestDbDeltaCapture). The dbDelta() shim is a single process-global
        // function across the whole suite; routing through the bridge — rather
        // than eval'ing a divergent no-op — keeps SchemaTest's per-test capture
        // working regardless of test ordering. We don't assert on the captured
        // SQL here (only that the db-version option gets stamped), so we leave
        // the bridge's onRecord untouched.
        if (!function_exists('dbDelta')) {
            eval('function dbDelta(string $sql): array { \WPMgr\Agent\Tests\TestDbDeltaCapture::record($sql); return []; }');
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
