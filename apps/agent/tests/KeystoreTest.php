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
        Monkey\tearDown();
        parent::tear_down();
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
