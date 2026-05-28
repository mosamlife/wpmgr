<?php
/**
 * Tests for portable, deterministic master-key acquisition in the keystore:
 * salt derivation, file fallback (writable-candidate selection + hardening),
 * legacy-file reuse, and source pinning for cross-request determinism.
 *
 * Each test runs in a separate process because master-key resolution depends on
 * process-global constants (WPMGR_AGENT_KEY_FILE, ABSPATH, WP salts) that can
 * only be defined once per process.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use PHPUnit\Framework\Attributes\PreserveGlobalState;
use PHPUnit\Framework\Attributes\RunTestsInSeparateProcesses;
use WPMgr\Agent\Keystore;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Keystore
 */
#[RunTestsInSeparateProcesses]
#[PreserveGlobalState(false)]
final class KeystoreMasterKeyTest extends TestCase
{
    /** @var array<string,mixed> In-memory wp-option store. */
    private array $options = [];

    /** @var list<string> Temp paths/dirs to clean up. */
    private array $cleanup = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->options = [];
        $this->cleanup = [];

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
    }

    protected function tear_down(): void
    {
        foreach ($this->cleanup as $path) {
            if (is_file($path)) {
                @unlink($path);
            } elseif (is_dir($path)) {
                @array_map('unlink', glob($path . '/*') ?: []);
                @rmdir($path);
            }
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    /** A set of realistic, high-entropy salt values (64 chars each). */
    private function defineRealSalts(): void
    {
        foreach (['AUTH_KEY', 'SECURE_AUTH_KEY', 'LOGGED_IN_KEY', 'NONCE_KEY',
                  'AUTH_SALT', 'SECURE_AUTH_SALT', 'LOGGED_IN_SALT', 'NONCE_SALT'] as $i => $name) {
            if (!defined($name)) {
                define($name, str_repeat(chr(97 + $i), 8) . bin2hex(random_bytes(28)));
            }
        }
    }

    public function test_salt_derivation_is_stable_and_32_bytes(): void
    {
        $this->defineRealSalts();

        $keystore = new Keystore();

        // Round-trip exercises masterKey() twice (encrypt + decrypt).
        $envelope = $keystore->encrypt('payload-under-salts');
        $this->assertSame('payload-under-salts', $keystore->decrypt($envelope));

        // Source must be pinned to salts.
        $this->assertSame(['source' => 'salts'], $this->options[Keystore::OPTION_MASTER_KEY_SOURCE]);

        // A fresh instance (new request) must derive the SAME key and decrypt.
        $fresh = new Keystore();
        $this->assertSame('payload-under-salts', $fresh->decrypt($envelope));
    }

    public function test_placeholder_salts_are_rejected_and_fall_through_to_file(): void
    {
        // Placeholder salt poisons salt-derivation; ABSPATH parent is writable.
        define('AUTH_KEY', 'put your unique phrase here');
        define('SECURE_AUTH_KEY', str_repeat('z', 64));
        define('LOGGED_IN_KEY', str_repeat('y', 64));

        $absParent = sys_get_temp_dir() . '/wpmgr-abs-' . bin2hex(random_bytes(6));
        $abs       = $absParent . '/htdocs/';
        mkdir($abs, 0700, true);
        $this->cleanup[] = $abs;
        $this->cleanup[] = $absParent . '/.wpmgr-agent-master.key';
        $this->cleanup[] = $absParent;
        define('ABSPATH', $abs);

        $keystore = new Keystore();
        $envelope = $keystore->encrypt('payload');
        $this->assertSame('payload', $keystore->decrypt($envelope));

        // Must have fallen through to a FILE source (not salts).
        $marker = $this->options[Keystore::OPTION_MASTER_KEY_SOURCE];
        $this->assertSame('file', $marker['source']);
        $this->assertFileExists($marker['path']);
        $this->assertSame(32, strlen((string) file_get_contents($marker['path'])));
        // 0600 perms.
        $this->assertSame('0600', substr(sprintf('%o', fileperms($marker['path'])), -4));
    }

    public function test_short_salts_are_rejected(): void
    {
        // Too little combined material -> salts unusable; fall to file.
        define('AUTH_KEY', 'short');
        define('NONCE_SALT', 'tiny');

        $absParent = sys_get_temp_dir() . '/wpmgr-abs-' . bin2hex(random_bytes(6));
        $abs       = $absParent . '/htdocs/';
        mkdir($abs, 0700, true);
        $this->cleanup[] = $abs;
        $this->cleanup[] = $absParent . '/.wpmgr-agent-master.key';
        $this->cleanup[] = $absParent;
        define('ABSPATH', $abs);

        $keystore = new Keystore();
        $envelope = $keystore->encrypt('p');
        $this->assertSame('p', $keystore->decrypt($envelope));
        $this->assertSame('file', $this->options[Keystore::OPTION_MASTER_KEY_SOURCE]['source']);
    }

    public function test_file_fallback_picks_first_writable_and_hardens_webroot(): void
    {
        // No salts. ABSPATH parent is NOT writable -> skip; WP_CONTENT_DIR is.
        $root = sys_get_temp_dir() . '/wpmgr-root-' . bin2hex(random_bytes(6));
        $abs  = $root . '/site/';
        mkdir($abs, 0700, true);
        // Make ABSPATH parent ($root/site's parent = $root) read-only so the
        // first candidate (dirname(ABSPATH)) is not writable.
        $absParent = $root;
        chmod($absParent, 0500);
        define('ABSPATH', $abs);

        $content = $root . '_content';
        mkdir($content, 0700, true);
        define('WP_CONTENT_DIR', $content);

        $this->cleanup[] = $content . '/wpmgr-agent';
        $this->cleanup[] = $content;
        $this->cleanup[] = $abs;
        // restore perms so cleanup can remove it.
        register_shutdown_function(static function () use ($absParent): void {
            @chmod($absParent, 0700);
        });

        $keystore = new Keystore();
        $envelope = $keystore->encrypt('webroot-payload');
        $this->assertSame('webroot-payload', $keystore->decrypt($envelope));

        $marker = $this->options[Keystore::OPTION_MASTER_KEY_SOURCE];
        $this->assertSame('file', $marker['source']);
        $this->assertStringContainsString('/wpmgr-agent/master.key', $marker['path']);

        // Web-root directory must be hardened.
        $dir = dirname($marker['path']);
        $this->assertFileExists($dir . '/index.php');
        $this->assertFileExists($dir . '/.htaccess');
        $this->assertStringContainsString('Require all denied', (string) file_get_contents($dir . '/.htaccess'));

        @chmod($absParent, 0700);
    }

    public function test_preexisting_legacy_key_file_is_reused(): void
    {
        // No salts. A legacy key file already exists at dirname(ABSPATH).
        $absParent = sys_get_temp_dir() . '/wpmgr-legacy-' . bin2hex(random_bytes(6));
        $abs       = $absParent . '/htdocs/';
        mkdir($abs, 0700, true);
        define('ABSPATH', $abs);

        $legacyPath = $absParent . '/.wpmgr-agent-master.key';
        $knownKey   = random_bytes(32);
        file_put_contents($legacyPath, $knownKey);

        $this->cleanup[] = $legacyPath;
        $this->cleanup[] = $abs;
        $this->cleanup[] = $absParent;

        $keystore = new Keystore();
        $envelope = $keystore->encrypt('legacy-payload');

        // Pinned to the legacy file path; no new key generated.
        $marker = $this->options[Keystore::OPTION_MASTER_KEY_SOURCE];
        $this->assertSame('file', $marker['source']);
        $this->assertSame($legacyPath, $marker['path']);
        $this->assertSame($knownKey, (string) file_get_contents($legacyPath));

        // A fresh instance reads the same legacy file and decrypts.
        $fresh = new Keystore();
        $this->assertSame('legacy-payload', $fresh->decrypt($envelope));
    }

    public function test_constant_path_takes_priority_and_creates_file(): void
    {
        $path = sys_get_temp_dir() . '/wpmgr-const-' . bin2hex(random_bytes(6)) . '.key';
        define('WPMGR_AGENT_KEY_FILE', $path);
        $this->cleanup[] = $path;

        // Salts also present, but the constant wins.
        $this->defineRealSalts();

        $keystore = new Keystore();
        $envelope = $keystore->encrypt('const-payload');
        $this->assertSame('const-payload', $keystore->decrypt($envelope));

        $this->assertFileExists($path);
        $this->assertSame(32, strlen((string) file_get_contents($path)));
        $this->assertSame(['source' => 'constant'], $this->options[Keystore::OPTION_MASTER_KEY_SOURCE]);
    }
}
