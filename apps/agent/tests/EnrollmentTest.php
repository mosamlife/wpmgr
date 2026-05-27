<?php
/**
 * Tests for the Enrollment client: /enroll payload shape and response handling.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\MetadataCommand;
use WPMgr\Agent\Enrollment;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Enrollment
 */
final class EnrollmentTest extends TestCase
{
    private string $keyFile;

    /** @var array<string,mixed> */
    private array $options = [];

    private Keystore $keystore;

    private Settings $settings;

    private Enrollment $enrollment;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-agent-enroll-' . bin2hex(random_bytes(8)) . '.key';
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
        Functions\when('delete_option')->alias(function ($name) {
            unset($this->options[$name]);
            return true;
        });
        Functions\when('home_url')->justReturn('https://example.test');
        Functions\when('get_bloginfo')->alias(function ($key) {
            return $key === 'name' ? 'Example Site' : '6.5.0';
        });
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('wp_get_theme')->justReturn(null);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
        Functions\when('esc_url_raw')->returnArg();
        Functions\when('is_wp_error')->justReturn(false);

        $this->keystore = new Keystore();
        $this->keystore->generateSiteKeypair();
        $this->settings = new Settings();
        $signer         = new Signer($this->keystore);
        $this->enrollment = new Enrollment($this->keystore, $this->settings, $signer, new MetadataCommand());
    }

    protected function tear_down(): void
    {
        if (is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_enroll_payload_shape(): void
    {
        $payload = $this->enrollment->buildEnrollPayload('PAIR-CODE-123');

        $this->assertSame('PAIR-CODE-123', $payload['pairing_code']);
        $this->assertSame('https://example.test', $payload['site_url']);
        $this->assertSame('Example Site', $payload['name']);
        $this->assertSame(PHP_VERSION, $payload['php_version']);
        $this->assertSame([], $payload['tags']);
        $this->assertArrayHasKey('wp_version', $payload);

        // agent_public_key is base64-std of the raw 32-byte Ed25519 public key.
        $raw = base64_decode($payload['agent_public_key'], true);
        $this->assertIsString($raw);
        $this->assertSame(SODIUM_CRYPTO_SIGN_PUBLICKEYBYTES, strlen($raw));
        $this->assertSame(base64_encode($raw), $payload['agent_public_key']);
    }

    public function test_successful_enroll_persists_site_id_and_cp_key(): void
    {
        $this->settings->setControlPlaneUrl('https://cp.example.test');

        // The control plane returns its own Ed25519 public key (base64-std).
        $cpKeypair = sodium_crypto_sign_keypair();
        $cpPublic  = sodium_crypto_sign_publickey($cpKeypair);

        $body = json_encode([
            'site_id'                  => 'site-abc',
            'tenant_id'                => 'tenant-xyz',
            'control_plane_public_key' => base64_encode($cpPublic),
        ]);

        Functions\when('wp_remote_post')->justReturn(['ok']);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
        Functions\when('wp_remote_retrieve_body')->justReturn($body);

        $result = $this->enrollment->enroll('PAIR-CODE-123');

        $this->assertTrue($result['ok']);
        $this->assertSame('enrolled', $result['code']);
        $this->assertSame('site-abc', $this->settings->siteId());
        $this->assertSame('tenant-xyz', $this->settings->tenantId());
        $this->assertTrue($this->settings->isEnrolled());

        // The CP key is now the one the inbound Connector will verify against.
        $this->assertSame($cpPublic, $this->keystore->getControlPlanePublicKey());
    }

    public function test_enroll_maps_401_to_clear_message(): void
    {
        $this->settings->setControlPlaneUrl('https://cp.example.test');

        Functions\when('wp_remote_post')->justReturn(['ok']);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(401);
        Functions\when('wp_remote_retrieve_body')->justReturn('');

        $result = $this->enrollment->enroll('bad-code');

        $this->assertFalse($result['ok']);
        $this->assertSame(401, $result['status']);
        $this->assertStringContainsString('invalid or expired', $result['message']);
        $this->assertFalse($this->settings->isEnrolled());
    }

    public function test_enroll_maps_409_and_422(): void
    {
        $this->settings->setControlPlaneUrl('https://cp.example.test');
        Functions\when('wp_remote_post')->justReturn(['ok']);
        Functions\when('wp_remote_retrieve_body')->justReturn('');

        Functions\when('wp_remote_retrieve_response_code')->justReturn(409);
        $r409 = $this->enrollment->enroll('x');
        $this->assertFalse($r409['ok']);
        $this->assertSame(409, $r409['status']);

        Functions\when('wp_remote_retrieve_response_code')->justReturn(422);
        $r422 = $this->enrollment->enroll('x');
        $this->assertFalse($r422['ok']);
        $this->assertSame(422, $r422['status']);
    }

    public function test_enroll_requires_url(): void
    {
        $result = $this->enrollment->enroll('x');

        $this->assertFalse($result['ok']);
        $this->assertSame('no_url', $result['code']);
    }
}
