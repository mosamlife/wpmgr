<?php
/**
 * The master key a site already uses stays its master key.
 *
 * Contract: once anything is stored in the keystore, creating a missing
 * backup key never creates a new master key and never records a different
 * one, and every key already stored still opens on the next request. A
 * request that reads a stored key keeps the key in use the same way. A key
 * that opens nothing stored is never taken for the site's key.
 *
 * Each test runs in its own process: the key sources (the wp-config.php
 * salts, WPMGR_AGENT_KEY_FILE, WP_CONTENT_DIR) are process-wide constants that
 * can be defined only once.
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
use WPMgr\Agent\Support\AgeIdentity;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Keystore
 * @covers \WPMgr\Agent\Support\AgeIdentity
 */
#[RunTestsInSeparateProcesses]
#[PreserveGlobalState(false)]
final class KeystoreMasterKeyContinuityTest extends TestCase
{
    /** @var array<string,mixed> In-memory wp-option store. */
    private array $options = [];

    /** @var list<string> Files and directories removed in tear_down. */
    private array $cleanup = [];

    /** A writable WP_CONTENT_DIR, so a new key file could always be written. */
    private string $contentDir = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->options = [];
        $this->cleanup = [];

        Functions\when('update_option')->alias(function ($name, $value, $autoload = null) {
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
        Functions\when('add_option')->alias(function ($name, $value, $deprecated = '', $autoload = null) {
            if (array_key_exists($name, $this->options)) {
                return false;
            }
            $this->options[$name] = $value;
            return true;
        });

        $this->assertFalse(defined('WP_CONTENT_DIR'), 'Precondition: WP_CONTENT_DIR is not defined yet.');
        $this->contentDir = sys_get_temp_dir() . '/wpmgr-continuity-' . bin2hex(random_bytes(6));
        mkdir($this->contentDir, 0700, true);
        define('WP_CONTENT_DIR', $this->contentDir);
        $this->cleanup[] = $this->contentDir;
        $this->assertTrue(is_writable($this->contentDir), 'Precondition: a key file could be written.');

        @unlink($this->legacyKeyPath());
        $this->cleanup[] = $this->legacyKeyPath();
    }

    protected function tear_down(): void
    {
        $source = $this->options[Keystore::OPTION_MASTER_KEY_SOURCE] ?? null;
        if (is_array($source) && isset($source['path']) && is_string($source['path'])) {
            $this->cleanup[] = $source['path'];
        }
        foreach ($this->cleanup as $path) {
            $this->removePath($path);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_creating_the_backup_key_keeps_the_database_stored_key(): void
    {
        $this->assertNoSaltsAndNoKeyFileConstant();
        $this->options[Keystore::OPTION_DB_MASTER_KEY] = base64_encode(random_bytes(32));
        $sitePublicKey = $this->connectSiteUnder(['source' => 'db']);
        unset($this->options[Keystore::OPTION_MASTER_KEY_SOURCE]);

        $this->assertBackupKeyCreatedUnderTheKeyInUse($sitePublicKey, ['source' => 'db']);
    }

    public function test_creating_the_backup_key_keeps_the_salts_key_while_the_named_key_file_does_not_exist(): void
    {
        $this->defineRealSalts();
        $constantPath = sys_get_temp_dir() . '/wpmgr-continuity-const-' . bin2hex(random_bytes(6)) . '.key';
        define('WPMGR_AGENT_KEY_FILE', $constantPath);
        $this->cleanup[] = $constantPath;
        $sitePublicKey = $this->connectSiteUnder(['source' => 'salts']);
        unset($this->options[Keystore::OPTION_MASTER_KEY_SOURCE]);
        $this->assertFileDoesNotExist($constantPath, 'Precondition: the named key file does not exist.');

        $this->assertBackupKeyCreatedUnderTheKeyInUse($sitePublicKey, ['source' => 'salts']);
        $this->assertFileDoesNotExist($constantPath);
    }

    public function test_the_next_request_opens_stored_keys_with_the_database_stored_key(): void
    {
        $this->assertNoSaltsAndNoKeyFileConstant();
        $this->options[Keystore::OPTION_DB_MASTER_KEY] = base64_encode(random_bytes(32));
        $sitePublicKey = $this->connectSiteUnder(['source' => 'db']);
        unset($this->options[Keystore::OPTION_MASTER_KEY_SOURCE]);
        $keyFiles = $this->keyFileSnapshot();

        $keypair = (new Keystore())->getSiteKeypair();

        $this->assertIsString($keypair);
        $this->assertSame($sitePublicKey, sodium_crypto_sign_publickey($keypair));
        $this->assertSame($keyFiles, $this->keyFileSnapshot(), 'Reading a stored key created or changed a key file.');
        $this->assertSame(['source' => 'db'], $this->options[Keystore::OPTION_MASTER_KEY_SOURCE] ?? null);
    }

    /** Honest case: a site that has used its salts key since before a source was recorded. */
    public function test_creating_the_backup_key_keeps_the_salts_key_when_no_source_is_recorded(): void
    {
        $this->defineRealSalts();
        $sitePublicKey = $this->connectSiteUnder(['source' => 'salts']);
        unset($this->options[Keystore::OPTION_MASTER_KEY_SOURCE]);

        $this->assertBackupKeyCreatedUnderTheKeyInUse($sitePublicKey, ['source' => 'salts']);
    }

    /** Honest case: a site whose salts key is recorded. */
    public function test_creating_the_backup_key_keeps_the_recorded_salts_key(): void
    {
        $this->defineRealSalts();
        $sitePublicKey = $this->connectSiteUnder(['source' => 'salts']);

        $this->assertBackupKeyCreatedUnderTheKeyInUse($sitePublicKey, ['source' => 'salts']);
    }

    /** Honest case: a site whose WPMGR_AGENT_KEY_FILE key is recorded and present. */
    public function test_creating_the_backup_key_keeps_the_recorded_key_file_constant(): void
    {
        $constantPath = sys_get_temp_dir() . '/wpmgr-continuity-const-' . bin2hex(random_bytes(6)) . '.key';
        file_put_contents($constantPath, random_bytes(32));
        define('WPMGR_AGENT_KEY_FILE', $constantPath);
        $this->cleanup[] = $constantPath;
        $sitePublicKey = $this->connectSiteUnder(['source' => 'constant']);

        $this->assertBackupKeyCreatedUnderTheKeyInUse($sitePublicKey, ['source' => 'constant']);
    }

    /**
     * Stored keys that no key on this site opens, next to an unrelated
     * database-stored key: that key is not recorded as the site's key, and it
     * is left as it was.
     */
    public function test_an_existing_key_that_opens_nothing_stored_is_not_adopted(): void
    {
        $this->assertNoSaltsAndNoKeyFileConstant();
        $this->options[Keystore::OPTION_DB_MASTER_KEY] = base64_encode(random_bytes(32));
        $this->options[Keystore::OPTION_SITE_KEYPAIR]  = $this->sealUnderForeignKey(random_bytes(96));
        $dbKey = $this->options[Keystore::OPTION_DB_MASTER_KEY];

        (new Keystore())->storeControlPlanePublicKey(random_bytes(SODIUM_CRYPTO_SIGN_PUBLICKEYBYTES));

        $source = $this->options[Keystore::OPTION_MASTER_KEY_SOURCE] ?? null;
        $this->assertIsArray($source, 'Precondition: storing a key recorded a source.');
        $this->assertNotSame('db', $source['source'] ?? null, 'A key that opens nothing stored was recorded as the site key.');
        $this->assertSame($dbKey, $this->options[Keystore::OPTION_DB_MASTER_KEY]);
    }

    /**
     * Create the missing backup key the way the agent does, then check the
     * contract: no key file appeared or changed, the database-stored key is
     * unchanged, the recorded source names the key already in use, and the
     * next request opens every stored key.
     *
     * @param string                            $sitePublicKey The site's Ed25519 public key.
     * @param array{source:string,path?:string} $source        The source of the key already in use.
     */
    private function assertBackupKeyCreatedUnderTheKeyInUse(string $sitePublicKey, array $source): void
    {
        $this->assertArrayNotHasKey(Keystore::OPTION_AGE_IDENTITY, $this->options, 'Precondition: no backup key is stored.');
        $keyFiles = $this->keyFileSnapshot();
        $dbKey    = $this->options[Keystore::OPTION_DB_MASTER_KEY] ?? null;

        $recipient = (new AgeIdentity(new Keystore()))->ensureRecipient();

        $this->assertSame($keyFiles, $this->keyFileSnapshot(), 'Creating the backup key created or changed a key file.');
        $this->assertSame($dbKey, $this->options[Keystore::OPTION_DB_MASTER_KEY] ?? null, 'Creating the backup key changed the database-stored key.');
        $this->assertSame($source, $this->options[Keystore::OPTION_MASTER_KEY_SOURCE] ?? null, 'The recorded source is not the key already in use.');

        $next  = new Keystore();
        $probe = $next->probe();
        $this->assertSame(Keystore::PROBE_OK, $probe['state'], 'Stored keys stopped opening: ' . implode(', ', $probe['unreadable']));
        $keypair = $next->getSiteKeypair();
        $this->assertIsString($keypair);
        $this->assertSame($sitePublicKey, sodium_crypto_sign_publickey($keypair));
        $this->assertSame($recipient, (new AgeIdentity($next))->recipient());
    }

    /**
     * Store the site keypair and the control-plane key under the given source,
     * the way enrollment does, and return the site's public key.
     *
     * @param array{source:string,path?:string} $source Source to seal under.
     */
    private function connectSiteUnder(array $source): string
    {
        $this->options[Keystore::OPTION_MASTER_KEY_SOURCE] = $source;
        $keystore      = new Keystore();
        $sitePublicKey = $keystore->generateSiteKeypair();
        $keystore->storeControlPlanePublicKey(random_bytes(SODIUM_CRYPTO_SIGN_PUBLICKEYBYTES));
        $this->assertSame($source, $this->options[Keystore::OPTION_MASTER_KEY_SOURCE], 'Precondition: the site keys were sealed under the intended source.');

        return $sitePublicKey;
    }

    /**
     * Each place a key file can live in this process, with a hash of its
     * contents, or null when there is no file.
     *
     * @return array<string,string|null>
     */
    private function keyFileSnapshot(): array
    {
        $paths = [$this->legacyKeyPath(), $this->contentDir . '/wpmgr-agent/master.key'];
        if (defined('WPMGR_AGENT_KEY_FILE')) {
            $paths[] = (string) constant('WPMGR_AGENT_KEY_FILE');
        }

        $snapshot = [];
        foreach ($paths as $path) {
            $snapshot[$path] = is_file($path) ? hash_file('sha256', $path) : null;
        }

        return $snapshot;
    }

    private function legacyKeyPath(): string
    {
        return rtrim(dirname(rtrim((string) ABSPATH, '/\\')), '/\\') . '/.wpmgr-agent-master.key';
    }

    private function assertNoSaltsAndNoKeyFileConstant(): void
    {
        $this->assertFalse(defined('AUTH_KEY'), 'Precondition: no salts in this process.');
        $this->assertFalse(defined('WPMGR_AGENT_KEY_FILE'), 'Precondition: no key-file constant in this process.');
    }

    /** Realistic, high-entropy wp-config.php salts (64 characters each). */
    private function defineRealSalts(): void
    {
        foreach (['AUTH_KEY', 'SECURE_AUTH_KEY', 'LOGGED_IN_KEY', 'NONCE_KEY',
                  'AUTH_SALT', 'SECURE_AUTH_SALT', 'LOGGED_IN_SALT', 'NONCE_SALT'] as $i => $name) {
            define($name, str_repeat(chr(97 + $i), 8) . bin2hex(random_bytes(28)));
        }
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

    /** Remove a file, or a directory and everything in it (dotfiles included). */
    private function removePath(string $path): void
    {
        if (is_file($path) || is_link($path)) {
            @unlink($path);
            return;
        }
        if (!is_dir($path)) {
            return;
        }
        foreach (scandir($path) ?: [] as $entry) {
            if ($entry !== '.' && $entry !== '..') {
                $this->removePath($path . '/' . $entry);
            }
        }
        @rmdir($path);
    }
}
