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

        // Pin the master-key source to 'file' with a path that does not and
        // cannot exist. resolveMasterKey() will try readKeyFile() on that path,
        // find nothing, and throw — making the keystore fail deterministically.
        //
        // We previously pinned to 'salts', but other tests in the suite
        // (FileManagerP3CommandsTest::testVersionRestoreSwapsContentAndCreatesPreRestoreVersion)
        // define AUTH_KEY / SECURE_AUTH_KEY / ... as real PHP process-global
        // constants. Once defined, constants cannot be undefined, so keyFromSalts()
        // begins succeeding for the remainder of the process — defeating the
        // 'salts' pin strategy. A non-existent file path is immune to that leak.
        $this->options = [
            Keystore::OPTION_MASTER_KEY_SOURCE => [
                'source' => 'file',
                'path'   => '/nonexistent-wpmgr-test-master-key-path/master.key',
            ],
        ];

        // Hook/registration no-ops used during boot().
        //
        // wp_schedule_single_event / spawn_cron are activation-side no-ops here:
        // activate() arms a +30s diagnostics prime + size probe via
        // wp_schedule_single_event (guarded by function_exists). Brain Monkey's
        // Patchwork makes function_exists('wp_schedule_single_event') return true
        // for the rest of the PROCESS once ANY sibling test defines it (e.g.
        // MediaAsyncTest, which legitimately exercises the media background-run
        // scheduling), so this test must stub it too or the guarded call throws
        // "not defined nor mocked" depending on file order. Stubbing both keeps
        // activation deterministic regardless of suite ordering.
        foreach (['add_action', 'add_filter', 'register_activation_hook',
                  'register_deactivation_hook', 'wp_schedule_single_event',
                  'spawn_cron', 'wp_next_scheduled', 'wp_schedule_event',
                  'wp_get_scheduled_event', 'wp_clear_scheduled_hook'] as $fn) {
            Functions\when($fn)->justReturn(true);
        }
        Functions\when('is_admin')->justReturn(false);
        Functions\when('is_multisite')->justReturn(false);
        // plugin_basename() is called under a function_exists guard inside
        // AdminBarPurge during Plugin::boot(); Brain Monkey makes the guard pass.
        Functions\when('plugin_basename')->returnArg();

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

    /** @var list<string> Key files created by pinToNewKeyFile(), removed in tear_down. */
    private array $keyFiles = [];

    /**
     * Pin the master-key source to a real 32-byte key file. A file pin, not
     * the salts: salt constants other tests define leak across the suite.
     */
    private function pinToNewKeyFile(): void
    {
        $path = sys_get_temp_dir() . '/wpmgr-agent-activation-' . bin2hex(random_bytes(8)) . '.key';
        file_put_contents($path, random_bytes(32));
        $this->keyFiles[] = $path;
        $this->options[Keystore::OPTION_MASTER_KEY_SOURCE] = ['source' => 'file', 'path' => $path];
    }

    /** An AES-256-GCM envelope in the keystore's layout, sealed under a random key that is not this site's. */
    private function sealUnderForeignKey(string $plaintext): string
    {
        $iv  = random_bytes(12);
        $tag = '';
        $ct  = openssl_encrypt($plaintext, 'aes-256-gcm', random_bytes(32), OPENSSL_RAW_DATA, $iv, $tag, '', 16);
        $this->assertIsString($ct);

        return base64_encode($iv . $tag . $ct);
    }

    /**
     * GH #753. After a host move the site keypair is regenerated under the new
     * key on reconnect, while the backup key is still sealed under the old
     * one. Activation must flag that state in plain words, and must leave the
     * unreadable backup key exactly as it is rather than replace it.
     */
    public function test_activation_flags_age_identity_sealed_under_a_different_key(): void
    {
        $this->pinToNewKeyFile();
        (new Keystore())->generateSiteKeypair();
        $sealed = $this->sealUnderForeignKey(random_bytes(32));
        $this->options[Keystore::OPTION_AGE_IDENTITY] = $sealed;
        $this->options[Plugin::OPTION_KEYSTORE_ERROR] = 'stale notice from an earlier run';

        Plugin::boot()->activate();

        $this->assertArrayHasKey(
            Plugin::OPTION_KEYSTORE_ERROR,
            $this->options,
            'An unreadable backup key must leave a notice set.'
        );
        $message = $this->options[Plugin::OPTION_KEYSTORE_ERROR];
        $this->assertIsString($message);
        $this->assertStringContainsString('The backup key saved on this site cannot be opened', $message);
        $this->assertStringContainsString('Most often', $message);
        $this->assertStringContainsString('put back the encryption key file', $message);
        $this->assertStringContainsString('backups of this site will fail', $message);
        $this->assertStringNotContainsString('WPMGR_AGENT_KEY_FILE', $message);
        $this->assertSame('unreadable', $this->options[Plugin::OPTION_KEYSTORE_ERROR_KIND] ?? null);
        $this->assertSame(
            $sealed,
            $this->options[Keystore::OPTION_AGE_IDENTITY],
            'The unreadable backup key must never be regenerated or replaced.'
        );
    }

    /** Over-fire guard: a keystore whose stored keys all open clears a stale notice. */
    public function test_activation_clears_the_notice_when_every_stored_key_opens(): void
    {
        $this->pinToNewKeyFile();
        (new Keystore())->generateSiteKeypair();
        $this->options[Plugin::OPTION_KEYSTORE_ERROR]      = 'stale notice from an earlier run';
        $this->options[Plugin::OPTION_KEYSTORE_ERROR_KIND] = 'unreadable';

        Plugin::boot()->activate();

        $this->assertArrayNotHasKey(Plugin::OPTION_KEYSTORE_ERROR, $this->options);
        $this->assertArrayNotHasKey(Plugin::OPTION_KEYSTORE_ERROR_KIND, $this->options);
        // The absent backup key was generated, under the key that opens the rest.
        $this->assertArrayHasKey(Keystore::OPTION_AGE_IDENTITY, $this->options);
        $this->assertSame(Keystore::PROBE_OK, (new Keystore())->probe()['state']);
    }

    /**
     * An already-broken site is flagged on an admin load, without
     * re-activation, and the probe runs at most once per throttle window.
     */
    public function test_admin_load_flags_unreadable_keys_at_most_once_per_window(): void
    {
        $this->pinToNewKeyFile();
        (new Keystore())->generateSiteKeypair();
        $this->options[Keystore::OPTION_AGE_IDENTITY] = $this->sealUnderForeignKey(random_bytes(32));

        $transients = [];
        Functions\when('current_user_can')->justReturn(true);
        Functions\when('get_transient')->alias(static function ($name) use (&$transients) {
            return $transients[$name] ?? false;
        });
        Functions\when('set_transient')->alias(static function ($name, $value) use (&$transients) {
            $transients[$name] = $value;
            return true;
        });

        $plugin = Plugin::boot();
        $plugin->ensureKeystoreReady();

        $this->assertArrayHasKey(Plugin::TRANSIENT_KEYSTORE_PROBE, $transients);
        $this->assertSame('unreadable', $this->options[Plugin::OPTION_KEYSTORE_ERROR_KIND] ?? null);

        // Inside the window, with no notice set, nothing is probed again.
        unset($this->options[Plugin::OPTION_KEYSTORE_ERROR], $this->options[Plugin::OPTION_KEYSTORE_ERROR_KIND]);
        $plugin->ensureKeystoreReady();
        $this->assertArrayNotHasKey(Plugin::OPTION_KEYSTORE_ERROR, $this->options);
    }

    /** A user who cannot manage options never triggers the admin-load probe. */
    public function test_admin_load_probe_is_limited_to_users_who_manage_options(): void
    {
        $this->pinToNewKeyFile();
        (new Keystore())->generateSiteKeypair();
        $this->options[Keystore::OPTION_AGE_IDENTITY] = $this->sealUnderForeignKey(random_bytes(32));
        Functions\when('current_user_can')->justReturn(false);
        Functions\when('get_transient')->justReturn(false);
        Functions\when('set_transient')->justReturn(true);

        Plugin::boot()->ensureKeystoreReady();

        $this->assertArrayNotHasKey(Plugin::OPTION_KEYSTORE_ERROR, $this->options);
    }

    protected function tear_down(): void
    {
        foreach ($this->keyFiles as $file) {
            if (is_file($file)) {
                @unlink($file);
            }
        }
        $this->keyFiles = [];
        // Reset the Plugin singleton so this test's constructed instance (with its
        // Keystore and Brain Monkey stubs) does not leak into subsequent tests.
        Plugin::resetForTesting();
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
        // The pinned 'file' source (a nonexistent path) is unavailable: the
        // message surfaces that specific pinned-source-drift cause (GH #257
        // cause-specific messaging) rather than a generic fixed string.
        $this->assertStringContainsString(
            'pinned master key file is missing or invalid',
            $this->options[Plugin::OPTION_KEYSTORE_ERROR]
        );
        $this->assertStringContainsString(
            'active but inactive',
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
