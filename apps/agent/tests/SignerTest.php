<?php
/**
 * Tests for the outbound Signer: canonical message format + Ed25519 round-trip.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Signer;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Signer
 */
final class SignerTest extends TestCase
{
    private string $keyFile;

    /** @var array<string,mixed> */
    private array $options = [];

    private Keystore $keystore;

    private Signer $signer;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-agent-signer-' . bin2hex(random_bytes(8)) . '.key';
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

        $this->keystore = new Keystore();
        $this->keystore->generateSiteKeypair();
        $this->signer = new Signer($this->keystore);
    }

    protected function tear_down(): void
    {
        if (is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_canonical_message_is_exactly_method_path_ts_nonce_bodyhash(): void
    {
        $method = 'post'; // intentionally lower-case to assert upper-casing.
        $path   = '/agent/v1/metadata';
        $ts      = '1700000000';
        $nonce   = 'abc123';
        $body    = '{"hello":"world"}';

        $message = Signer::canonicalMessage($method, $path, $ts, $nonce, $body);

        $expected = "POST\n"
            . "/agent/v1/metadata\n"
            . "1700000000\n"
            . "abc123\n"
            . hash('sha256', $body);

        $this->assertSame($expected, $message);

        // Exactly 5 LF-separated lines, no trailing newline.
        $lines = explode("\n", $message);
        $this->assertCount(5, $lines);
        $this->assertSame('POST', $lines[0]);
        $this->assertSame('/agent/v1/metadata', $lines[1]);
        $this->assertSame('1700000000', $lines[2]);
        $this->assertSame('abc123', $lines[3]);
        $this->assertSame(hash('sha256', $body), $lines[4]);
        $this->assertSame(64, strlen($lines[4]), 'Body hash is 64 hex chars (sha256).');
    }

    public function test_empty_body_hashes_the_empty_string(): void
    {
        $message = Signer::canonicalMessage('POST', '/agent/v1/heartbeat', '1', 'n', '');
        $lines   = explode("\n", $message);

        $this->assertSame(hash('sha256', ''), $lines[4]);
        $this->assertSame('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855', $lines[4]);
    }

    public function test_signature_verifies_with_agent_public_key(): void
    {
        $method = 'POST';
        $path   = '/agent/v1/metadata';
        $body   = '{"a":1}';
        $now    = 1_700_000_000;

        $headers = $this->signer->signHeaders($method, $path, $body, $now);

        // Reconstruct the canonical message from the exact header strings.
        $message = Signer::canonicalMessage(
            $method,
            $path,
            $headers[Signer::HEADER_TIMESTAMP],
            $headers[Signer::HEADER_NONCE],
            $body
        );

        $publicKey = base64_decode($headers[Signer::HEADER_KEY], true);
        $signature = base64_decode($headers[Signer::HEADER_SIGNATURE], true);

        $this->assertIsString($publicKey);
        $this->assertIsString($signature);
        $this->assertSame(SODIUM_CRYPTO_SIGN_PUBLICKEYBYTES, strlen($publicKey));
        $this->assertSame(SODIUM_CRYPTO_SIGN_BYTES, strlen($signature));

        $this->assertTrue(
            sodium_crypto_sign_verify_detached($signature, $message, $publicKey),
            'Detached signature must verify against the canonical message.'
        );
    }

    public function test_headers_use_base64_std_and_timestamp_matches(): void
    {
        $now     = 1_700_000_123;
        $headers = $this->signer->signHeaders('POST', '/agent/v1/heartbeat', '', $now);

        $this->assertSame((string) $now, $headers[Signer::HEADER_TIMESTAMP]);

        // base64-std (not url-safe): the public key header decodes to 32 bytes.
        $key = base64_decode($headers[Signer::HEADER_KEY], true);
        $this->assertIsString($key);
        $this->assertSame(SODIUM_CRYPTO_SIGN_PUBLICKEYBYTES, strlen($key));

        // Nonce is 16 random bytes hex => 32 hex chars.
        $this->assertSame(32, strlen($headers[Signer::HEADER_NONCE]));
        $this->assertMatchesRegularExpression('/^[0-9a-f]{32}$/', $headers[Signer::HEADER_NONCE]);
    }

    public function test_each_request_uses_a_fresh_nonce(): void
    {
        $a = $this->signer->signHeaders('POST', '/agent/v1/heartbeat', '', 1);
        $b = $this->signer->signHeaders('POST', '/agent/v1/heartbeat', '', 1);

        $this->assertNotSame($a[Signer::HEADER_NONCE], $b[Signer::HEADER_NONCE]);
    }

    public function test_agent_public_key_base64_is_std_alphabet_32_bytes(): void
    {
        $b64 = $this->signer->agentPublicKeyBase64();
        $raw = base64_decode($b64, true);

        $this->assertIsString($raw);
        $this->assertSame(SODIUM_CRYPTO_SIGN_PUBLICKEYBYTES, strlen($raw));
        $this->assertSame(base64_encode($raw), $b64, 'Must be standard base64 with padding.');
    }
}
