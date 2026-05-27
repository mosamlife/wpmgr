<?php
/**
 * Tests for Ed25519 JWT verification + anti-replay in the Connector.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Connector;
use WPMgr\Agent\Keystore;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Connector
 */
final class ConnectorTest extends TestCase
{
    private string $keyFile;

    /** @var array<string,mixed> */
    private array $options = [];

    /** @var string Control-plane Ed25519 secret key. */
    private string $cpSecret;

    /** @var string Control-plane Ed25519 public key. */
    private string $cpPublic;

    private Keystore $keystore;

    private Connector $connector;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-agent-conn-' . bin2hex(random_bytes(8)) . '.key';
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

        // Generate the control-plane keypair and provision its public key.
        $keypair        = sodium_crypto_sign_keypair();
        $this->cpSecret = sodium_crypto_sign_secretkey($keypair);
        $this->cpPublic = sodium_crypto_sign_publickey($keypair);

        $this->keystore = new Keystore();
        $this->keystore->storeControlPlanePublicKey($this->cpPublic);

        $this->connector = new Connector($this->keystore);

        // Fresh fake $wpdb per test.
        $GLOBALS['wpdb'] = new FakeWpdb();
    }

    protected function tear_down(): void
    {
        if (is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Build a compact Ed25519-signed JWT.
     *
     * @param array<string,mixed> $claims Payload claims.
     * @param string|null         $secret Override signing key (for invalid sig).
     * @param array<string,mixed> $header Override header.
     * @return string
     */
    private function makeJwt(array $claims, ?string $secret = null, array $header = ['alg' => 'EdDSA', 'typ' => 'JWT']): string
    {
        $secret = $secret ?? $this->cpSecret;

        $segments = [
            $this->b64(json_encode($header) ?: ''),
            $this->b64(json_encode($claims) ?: ''),
        ];
        $signingInput = implode('.', $segments);
        $signature    = sodium_crypto_sign_detached($signingInput, $secret);
        $segments[]   = $this->b64($signature);

        return implode('.', $segments);
    }

    private function b64(string $data): string
    {
        return rtrim(strtr(base64_encode($data), '+/', '-_'), '=');
    }

    public function test_accepts_a_valid_signed_token(): void
    {
        $now = 1_700_000_000;
        $jwt = $this->makeJwt(['jti' => 'unique-1', 'exp' => $now + 30]);

        $claims = $this->connector->verify($jwt, $now);

        $this->assertSame('unique-1', $claims['jti']);
    }

    public function test_rejects_an_invalid_signature(): void
    {
        $now = 1_700_000_000;

        // Sign with a DIFFERENT (attacker) key.
        $attacker = sodium_crypto_sign_keypair();
        $jwt = $this->makeJwt(
            ['jti' => 'forged-1', 'exp' => $now + 30],
            sodium_crypto_sign_secretkey($attacker)
        );

        $this->expectException(\RuntimeException::class);
        $this->connector->verify($jwt, $now);
    }

    public function test_rejects_tampered_payload(): void
    {
        $now = 1_700_000_000;
        $jwt = $this->makeJwt(['jti' => 'tamper-1', 'exp' => $now + 30]);

        // Swap the payload segment for one with escalated claims.
        $parts    = explode('.', $jwt);
        $parts[1] = $this->b64(json_encode(['jti' => 'tamper-1', 'exp' => $now + 30, 'role' => 'admin']) ?: '');
        $tampered = implode('.', $parts);

        $this->expectException(\RuntimeException::class);
        $this->connector->verify($tampered, $now);
    }

    public function test_rejects_an_expired_token(): void
    {
        $now = 1_700_000_000;
        $jwt = $this->makeJwt(['jti' => 'expired-1', 'exp' => $now - 1]);

        $this->expectException(\RuntimeException::class);
        $this->connector->verify($jwt, $now);
    }

    public function test_rejects_exp_too_far_in_future(): void
    {
        $now = 1_700_000_000;
        // exp more than MAX_FUTURE_EXP (60s) ahead.
        $jwt = $this->makeJwt(['jti' => 'future-1', 'exp' => $now + 61]);

        $this->expectException(\RuntimeException::class);
        $this->connector->verify($jwt, $now);
    }

    public function test_rejects_a_replayed_jti(): void
    {
        $now = 1_700_000_000;
        $jwt = $this->makeJwt(['jti' => 'replay-1', 'exp' => $now + 30]);

        // First use succeeds.
        $this->connector->verify($jwt, $now);

        // Second presentation of the same token must be rejected.
        $this->expectException(\RuntimeException::class);
        $this->connector->verify($jwt, $now + 1);
    }

    public function test_rejects_token_with_no_provisioned_key(): void
    {
        // Wipe the provisioned key.
        $this->options = [];
        $now = 1_700_000_000;
        $jwt = $this->makeJwt(['jti' => 'nokey-1', 'exp' => $now + 30]);

        $this->expectException(\RuntimeException::class);
        $this->connector->verify($jwt, $now);
    }

    public function test_rejects_malformed_token(): void
    {
        $this->expectException(\RuntimeException::class);
        $this->connector->verify('not-a-jwt');
    }
}
