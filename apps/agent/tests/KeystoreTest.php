<?php
/**
 * Tests for the AES-256-GCM keystore.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Keystore;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Keystore
 */
final class KeystoreTest extends TestCase
{
    /** @var string Path to the throwaway master-key file. */
    private string $keyFile;

    /** @var array<string,mixed> In-memory option store. */
    private array $options = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-agent-test-' . bin2hex(random_bytes(8)) . '.key';
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }

        $this->options = [];

        Functions\when('update_option')->alias(function ($name, $value) {
            $this->options[$name] = $value;
            return true;
        });
        Functions\when('get_option')->alias(function ($name, $default = false) {
            return $this->options[$name] ?? $default;
        });
    }

    protected function tear_down(): void
    {
        if (is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        foreach ($this->pinnedKeyFiles as $file) {
            if (is_file($file)) {
                @unlink($file);
            }
        }
        $this->pinnedKeyFiles = [];
        Monkey\tearDown();
        parent::tear_down();
    }

    /** @var list<string> Key files created by pinToNewKeyFile(), removed in tear_down. */
    private array $pinnedKeyFiles = [];

    /**
     * Pin the master-key source to a fresh 32-byte key file. A file pin keeps
     * these tests independent of the salt constants and WPMGR_AGENT_KEY_FILE,
     * which other tests define process-wide.
     */
    private function pinToNewKeyFile(): string
    {
        $path = sys_get_temp_dir() . '/wpmgr-agent-probe-' . bin2hex(random_bytes(8)) . '.key';
        file_put_contents($path, random_bytes(32));
        $this->pinnedKeyFiles[] = $path;
        $this->options[Keystore::OPTION_MASTER_KEY_SOURCE] = ['source' => 'file', 'path' => $path];

        return $path;
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
     * Replace the option stubs with ones that record every write attempt.
     *
     * @param list<string> $writes Receives one entry per write call.
     */
    private function recordWrites(array &$writes): void
    {
        foreach (['update_option', 'add_option', 'delete_option', 'update_site_option', 'set_transient'] as $fn) {
            Functions\when($fn)->alias(static function ($name) use (&$writes, $fn) {
                $writes[] = $fn . ':' . (string) $name;
                return true;
            });
        }
    }

    public function test_probe_reports_ok_for_every_item_that_opens_under_the_current_key(): void
    {
        $this->pinToNewKeyFile();
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
        Functions\when('delete_option')->justReturn(true);

        $keystore = new Keystore();
        $keystore->generateSiteKeypair();
        $keystore->storeControlPlanePublicKey(random_bytes(32));
        $keystore->storeAgeIdentity(random_bytes(32));
        $keystore->storeEmailSecret('smtp-password');
        $keystore->store_connection_secrets(['primary' => 'api-key']);

        $probe = (new Keystore())->probe();

        $this->assertSame(Keystore::PROBE_OK, $probe['state']);
        $this->assertSame('file', $probe['key_source']);
        $this->assertSame([
            'site_keypair'             => Keystore::ITEM_OK,
            'cp_public_key'            => Keystore::ITEM_OK,
            'age_identity'             => Keystore::ITEM_OK,
            'email_secret'             => Keystore::ITEM_OK,
            'email_connection_secrets' => Keystore::ITEM_OK,
        ], $probe['items']);
        $this->assertSame([], $probe['unreadable']);
    }

    /**
     * The state a reconnect leaves after a host move: the connection keys were
     * regenerated under the current key, the backup key was not.
     */
    public function test_probe_reports_the_split_state_item_by_item(): void
    {
        $this->pinToNewKeyFile();
        $keystore = new Keystore();
        $keystore->generateSiteKeypair();
        $this->options[Keystore::OPTION_AGE_IDENTITY] = $this->sealUnderForeignKey(random_bytes(32));

        $probe = (new Keystore())->probe();

        $this->assertSame(Keystore::PROBE_UNREADABLE, $probe['state']);
        $this->assertSame(['age_identity'], $probe['unreadable']);
        $this->assertSame(Keystore::ITEM_OK, $probe['items']['site_keypair']);
        $this->assertSame(Keystore::ITEM_UNREADABLE, $probe['items']['age_identity']);
        $this->assertSame(Keystore::ITEM_ABSENT, $probe['items']['cp_public_key']);
        $this->assertSame(Keystore::ITEM_ABSENT, $probe['items']['email_secret']);
    }

    public function test_probe_reports_key_unavailable_when_the_pinned_source_is_gone(): void
    {
        $this->options[Keystore::OPTION_MASTER_KEY_SOURCE] = [
            'source' => 'file',
            'path'   => '/nonexistent-wpmgr-probe-path/master.key',
        ];
        $this->options[Keystore::OPTION_AGE_IDENTITY] = $this->sealUnderForeignKey(random_bytes(32));

        $probe = (new Keystore())->probe();

        $this->assertSame(Keystore::PROBE_KEY_UNAVAILABLE, $probe['state']);
        $this->assertStringContainsString('pinned master key file is missing or invalid', $probe['detail']);
        $this->assertSame(['age_identity'], $probe['unreadable']);
        $this->assertSame(Keystore::ITEM_UNREADABLE, $probe['items']['age_identity']);
        $this->assertSame(Keystore::ITEM_ABSENT, $probe['items']['site_keypair']);
    }

    public function test_probe_with_nothing_stored_is_ok_without_loading_a_key(): void
    {
        $this->options[Keystore::OPTION_MASTER_KEY_SOURCE] = [
            'source' => 'file',
            'path'   => '/nonexistent-wpmgr-probe-path/master.key',
        ];

        $probe = (new Keystore())->probe();

        $this->assertSame(Keystore::PROBE_OK, $probe['state']);
        $this->assertSame('', $probe['detail']);
        $this->assertSame([Keystore::ITEM_ABSENT], array_values(array_unique($probe['items'])));
    }

    /**
     * The probe must never write: no option, no pin, no key file. Covered for
     * a pinned install in the split state and for an unpinned install, where
     * resolving the key the normal way would pin a source.
     */
    public function test_probe_never_writes(): void
    {
        $keyPath  = $this->pinToNewKeyFile();
        $keystore = new Keystore();
        $keystore->generateSiteKeypair();
        $this->options[Keystore::OPTION_AGE_IDENTITY] = $this->sealUnderForeignKey(random_bytes(32));
        $keyBytes = file_get_contents($keyPath);

        $writes = [];
        $this->recordWrites($writes);
        Functions\when('wp_upload_dir')->justReturn(['basedir' => sys_get_temp_dir() . '/wpmgr-probe-no-such-uploads']);

        // Pinned, split state.
        $before = $this->options;
        (new Keystore())->probe();
        $this->assertSame($before, $this->options, 'probe() changed an option.');
        $this->assertSame([], $writes, 'probe() attempted a write.');
        $this->assertSame($keyBytes, file_get_contents($keyPath), 'probe() changed the key file.');

        // Unpinned: the stored items remain, the source marker is gone.
        unset($this->options[Keystore::OPTION_MASTER_KEY_SOURCE]);
        $constantPath   = defined('WPMGR_AGENT_KEY_FILE') ? (string) constant('WPMGR_AGENT_KEY_FILE') : '';
        $constantExists = $constantPath !== '' && is_file($constantPath);
        $before         = $this->options;
        $probe          = (new Keystore())->probe();
        $this->assertSame($before, $this->options, 'probe() changed an option on an unpinned install.');
        $this->assertSame([], $writes, 'probe() attempted a write on an unpinned install.');
        $this->assertSame('', $probe['key_source']);
        if ($constantPath !== '' && !$constantExists) {
            $this->assertFileDoesNotExist($constantPath, 'probe() created a key file.');
        }
    }

    public function test_encrypt_decrypt_round_trip(): void
    {
        $keystore  = new Keystore();
        $plaintext = 'the quick brown fox \x00 binary \xff bytes';

        $envelope = $keystore->encrypt($plaintext);

        $this->assertNotSame($plaintext, $envelope, 'Ciphertext must differ from plaintext.');
        $this->assertSame($plaintext, $keystore->decrypt($envelope));
    }

    public function test_each_encryption_uses_a_fresh_iv(): void
    {
        $keystore = new Keystore();

        $a = $keystore->encrypt('same input');
        $b = $keystore->encrypt('same input');

        $this->assertNotSame($a, $b, 'Random IV must make ciphertexts differ.');
    }

    public function test_tampered_ciphertext_fails_authentication(): void
    {
        $keystore = new Keystore();
        $envelope = $keystore->encrypt('secret payload');

        $raw = base64_decode($envelope, true);
        $this->assertIsString($raw);
        // Flip a bit in the ciphertext body (after iv+tag).
        $raw[28] = $raw[28] ^ "\x01";
        $tampered = base64_encode($raw);

        $this->expectException(\RuntimeException::class);
        $keystore->decrypt($tampered);
    }

    public function test_control_plane_public_key_round_trip_via_options(): void
    {
        $keystore = new Keystore();
        $rawKey   = random_bytes(SODIUM_CRYPTO_SIGN_PUBLICKEYBYTES);

        $keystore->storeControlPlanePublicKey($rawKey);

        // Stored form must be encrypted, not the raw key.
        $stored = $this->options[Keystore::OPTION_CP_PUBLIC_KEY];
        $this->assertIsString($stored);
        $this->assertNotSame($rawKey, base64_decode($stored, true));

        $this->assertSame($rawKey, $keystore->getControlPlanePublicKey());
    }

    public function test_generate_site_keypair_returns_public_and_persists_encrypted(): void
    {
        $keystore = new Keystore();

        $publicKey = $keystore->generateSiteKeypair();

        $this->assertSame(SODIUM_CRYPTO_SIGN_PUBLICKEYBYTES, strlen($publicKey));

        $keypair = $keystore->getSiteKeypair();
        $this->assertIsString($keypair);
        $this->assertSame($publicKey, sodium_crypto_sign_publickey($keypair));
    }
}
