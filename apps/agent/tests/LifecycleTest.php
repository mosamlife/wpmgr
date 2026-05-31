<?php
/**
 * Tests for the connection-lifecycle behaviours (ADR-039/040/041):
 *   - the light 60s heartbeat payload shape,
 *   - a heartbeat "revoke" instruction wiping keys + deactivating the plugin,
 *   - the deactivate last-will posting a SIGNED disconnect with a 3s timeout,
 *   - the 410 enroll response surfacing the correct user-facing message.
 *
 * WP functions are stubbed with Brain Monkey exactly as the sibling tests do.
 * These tests run IN-PROCESS (no separate-process isolation) so they are stable
 * under PHP 8.5 + PHPUnit 10.
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
use WPMgr\Agent\Lifecycle;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Lifecycle
 * @covers \WPMgr\Agent\Enrollment
 */
final class LifecycleTest extends TestCase
{
    private string $keyFile;

    /** @var array<string,mixed> In-memory wp-option store. */
    private array $options = [];

    /** @var list<array{0:string,1:array<string,mixed>}> Captured wp_remote_post calls. */
    private array $posts = [];

    private Keystore $keystore;

    private Settings $settings;

    private Enrollment $enrollment;

    private Lifecycle $lifecycle;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-agent-lifecycle-' . bin2hex(random_bytes(8)) . '.key';
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }

        $this->options = [];
        $this->posts   = [];

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
        Functions\when('get_site_option')->alias(function ($name, $default = false) {
            return $this->options[$name] ?? $default;
        });

        Functions\when('is_multisite')->justReturn(false);
        Functions\when('home_url')->justReturn('https://example.test');
        Functions\when('get_bloginfo')->alias(static function ($key) {
            return $key === 'name' ? 'Example Site' : '6.7.0';
        });
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
        Functions\when('esc_url_raw')->returnArg();
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('get_plugins')->justReturn([
            'akismet/akismet.php' => ['Version' => '5.3'],
            'hello.php'           => ['Version' => '1.7.2'],
        ]);
        Functions\when('get_site_transient')->justReturn(false);
        // MetadataCommand::collect() (reached via enroll -> buildEnrollPayload)
        // probes the theme inventory; stub it so the full-suite run does not
        // trip Brain Monkey's strict "unmocked function" guard.
        Functions\when('wp_get_theme')->justReturn(null);
        Functions\when('wp_get_themes')->justReturn([]);

        // Capture every signed POST so we can assert path / timeout / body.
        Functions\when('wp_remote_post')->alias(function ($url, $args) {
            $this->posts[] = [(string) $url, (array) $args];
            return ['ok'];
        });
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
        Functions\when('wp_remote_retrieve_body')->justReturn('');

        $this->keystore = new Keystore();
        $this->keystore->generateSiteKeypair();
        $this->keystore->storeControlPlanePublicKey(
            sodium_crypto_sign_publickey(sodium_crypto_sign_keypair())
        );
        $this->settings = new Settings();
        $this->settings->setControlPlaneUrl('https://cp.example.test');
        $this->settings->setEnrollment('site-abc', 'tenant-xyz');

        $signer            = new Signer($this->keystore);
        $this->enrollment  = new Enrollment(
            $this->keystore,
            $this->settings,
            $signer,
            new MetadataCommand()
        );
        $this->lifecycle = new Lifecycle($this->keystore, $this->settings, $this->enrollment);
    }

    protected function tear_down(): void
    {
        if (is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_heartbeat_payload_is_valid_json_with_expected_keys(): void
    {
        $payload = $this->enrollment->buildHeartbeatPayload();

        // Must serialise to valid JSON.
        $json = json_encode($payload);
        $this->assertIsString($json);
        $this->assertNotFalse(json_decode($json));

        foreach (['site_id', 'ts', 'status', 'wp_version', 'php_memory',
                  'plugin_versions', 'installed_updates_count', 'multisite'] as $key) {
            $this->assertArrayHasKey($key, $payload, "heartbeat payload missing $key");
        }

        $this->assertSame('site-abc', $payload['site_id']);
        $this->assertSame('ok', $payload['status']);
        $this->assertSame(ini_get('memory_limit'), $payload['php_memory']);
        $this->assertIsInt($payload['installed_updates_count']);
        $this->assertIsArray($payload['plugin_versions']);
        $this->assertSame('5.3', $payload['plugin_versions']['akismet/akismet.php']);
        $this->assertFalse($payload['multisite']);
    }

    public function test_heartbeat_parses_revoke_instruction_from_200_json(): void
    {
        Functions\when('wp_remote_retrieve_body')
            ->justReturn(json_encode(['ok' => true, 'instructions' => ['revoke']]));

        $result = $this->enrollment->sendHeartbeat();

        $this->assertTrue($result['ok']);
        $this->assertSame(['revoke'], $result['instructions']);
    }

    public function test_heartbeat_tolerates_legacy_204_empty_body(): void
    {
        Functions\when('wp_remote_retrieve_response_code')->justReturn(204);
        Functions\when('wp_remote_retrieve_body')->justReturn('');

        $result = $this->enrollment->sendHeartbeat();

        // 204 is a 2xx -> ok, with no instructions and no error.
        $this->assertTrue($result['ok']);
        $this->assertSame([], $result['instructions']);
    }

    public function test_revoke_instruction_wipes_keys_and_deactivates_plugin(): void
    {
        if (!defined('WPMGR_AGENT_FILE')) {
            define('WPMGR_AGENT_FILE', __FILE__);
        }
        if (!defined('ABSPATH')) {
            // Point at a non-existent path so the plugin.php require is skipped.
            define('ABSPATH', '/nonexistent-wpmgr-' . bin2hex(random_bytes(6)) . '/');
        }

        $deactivated = [];
        Functions\when('plugin_basename')->alias(static fn ($f) => basename((string) $f));
        Functions\when('deactivate_plugins')->alias(function ($plugin) use (&$deactivated) {
            $deactivated[] = $plugin;
            return null;
        });
        $revokeHook = [];
        Functions\when('do_action')->alias(function ($hook, $reason = null) use (&$revokeHook) {
            if ($hook === 'wpmgr_revoking_self') {
                $revokeHook[] = $reason;
            }
            return null;
        });

        // Pre-conditions: enrolled + keypair present.
        $this->assertTrue($this->settings->isEnrolled());
        $this->assertNotNull($this->keystore->getSiteKeypair());

        $this->lifecycle->handleInstructions(['revoke']);

        // The revoke hook fired with the dashboard reason.
        $this->assertSame([Lifecycle::REASON_REVOKED], $revokeHook);

        // Plugin was deactivated.
        $this->assertSame([basename(__FILE__)], $deactivated);

        // Keystore site identity wiped (CP key + site keypair gone).
        $this->assertNull($this->keystore->getSiteKeypair());
        $this->assertNull($this->keystore->getControlPlanePublicKey());

        // Enrollment cleared.
        $this->assertFalse($this->settings->isEnrolled());

        // Persistent marker recorded for the admin UI.
        $marker = Lifecycle::revokedMarker();
        $this->assertIsArray($marker);
        $this->assertSame(Lifecycle::REASON_REVOKED, $marker['reason']);
        $this->assertGreaterThan(0, $marker['at']);
    }

    public function test_deactivate_posts_signed_disconnect_with_3s_timeout(): void
    {
        $this->lifecycle->onDeactivate();

        // Exactly one POST, to the disconnect path, with the 3s budget.
        $this->assertCount(1, $this->posts);
        [$url, $args] = $this->posts[0];

        $this->assertStringEndsWith(Enrollment::PATH_DISCONNECT, $url);
        $this->assertSame(Enrollment::DISCONNECT_TIMEOUT, $args['timeout']);
        $this->assertSame(3, $args['timeout']);

        // Body is the signed last-will with reason=deactivated.
        $body = json_decode((string) $args['body'], true);
        $this->assertIsArray($body);
        $this->assertSame('deactivated', $body['reason']);
        $this->assertSame('site-abc', $body['site_id']);

        // It went through the SIGNED path: the four X-WPMgr-* headers are present.
        $headers = $args['headers'];
        $this->assertArrayHasKey(Signer::HEADER_KEY, $headers);
        $this->assertArrayHasKey(Signer::HEADER_TIMESTAMP, $headers);
        $this->assertArrayHasKey(Signer::HEADER_NONCE, $headers);
        $this->assertArrayHasKey(Signer::HEADER_SIGNATURE, $headers);
    }

    public function test_deactivate_is_noop_when_not_enrolled(): void
    {
        $this->settings->clearEnrollment();

        $this->lifecycle->onDeactivate();

        $this->assertCount(0, $this->posts, 'no last-will should be sent when not enrolled');
    }

    public function test_uninstall_reason_maps_through_disconnect(): void
    {
        $result = $this->enrollment->disconnect('uninstalled');

        $this->assertTrue($result['ok']);
        $this->assertCount(1, $this->posts);
        [$url, $args] = $this->posts[0];
        $this->assertStringEndsWith(Enrollment::PATH_DISCONNECT, $url);
        $body = json_decode((string) $args['body'], true);
        $this->assertSame('uninstalled', $body['reason']);
    }

    public function test_disconnect_coerces_unknown_reason_to_user_initiated(): void
    {
        $this->enrollment->disconnect('not-a-valid-reason');

        $this->assertCount(1, $this->posts);
        $body = json_decode((string) $this->posts[0][1]['body'], true);
        $this->assertSame('user_initiated', $body['reason']);
    }

    public function test_enroll_maps_410_to_expired_or_consumed_message(): void
    {
        Functions\when('wp_remote_retrieve_response_code')->justReturn(410);
        Functions\when('wp_remote_retrieve_body')->justReturn('');

        $result = $this->enrollment->enroll('some-consumed-code');

        $this->assertFalse($result['ok']);
        $this->assertSame(410, $result['status']);
        $this->assertStringContainsString('expired or was already used', $result['message']);
        $this->assertStringContainsString('Request a new code', $result['message']);
    }

    public function test_heartbeat_now_swallows_failure(): void
    {
        // Force the outbound call to look like a hard failure (WP_Error).
        Functions\when('is_wp_error')->justReturn(true);

        $instructions = $this->lifecycle->heartbeatNow();

        $this->assertSame([], $instructions, 'a failed immediate beat returns no instructions and does not throw');
    }

    public function test_heartbeat_now_fires_one_signed_beat(): void
    {
        $this->posts = [];

        $this->lifecycle->heartbeatNow();

        $this->assertCount(1, $this->posts, 'one immediate post-enroll heartbeat is sent');
        [$url] = $this->posts[0];
        $this->assertStringEndsWith(Enrollment::PATH_HEARTBEAT, $url);
    }

    public function test_reenroll_sequence_wipes_then_establishes_fresh_identity(): void
    {
        // Capture the ORIGINAL site keypair so we can prove it was rotated.
        $originalKeypair = $this->keystore->getSiteKeypair();
        $this->assertIsString($originalKeypair);

        // --- The wipe half of Re-enroll (what Admin::handleReenroll does). ---
        $this->keystore->clearSiteIdentity();
        $this->settings->clearEnrollment();
        $this->settings->clearLastSyncTimestamps();

        $this->assertNull($this->keystore->getSiteKeypair());
        $this->assertFalse($this->settings->isEnrolled());

        // --- The enroll half against a fresh code. ---
        // A fresh keypair must exist for the enroll request to be signable; the
        // Signer regenerates it lazily, mirroring the real enroll flow.
        $signer        = new Signer($this->keystore);
        $newPublicB64  = $signer->agentPublicKeyBase64();
        $this->assertNotSame('', $newPublicB64);

        $cpKeypair = sodium_crypto_sign_keypair();
        $cpPublic  = sodium_crypto_sign_publickey($cpKeypair);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
        Functions\when('wp_remote_retrieve_body')->justReturn(json_encode([
            'site_id'                  => 'site-NEW',
            'tenant_id'                => 'tenant-NEW',
            'control_plane_public_key' => base64_encode($cpPublic),
        ]));

        $result = $this->enrollment->enroll('fresh-code');

        $this->assertTrue($result['ok']);
        $this->assertSame('site-NEW', $this->settings->siteId());
        $this->assertTrue($this->settings->isEnrolled());

        // Identity rotated: the new keypair differs from the wiped one.
        $newKeypair = $this->keystore->getSiteKeypair();
        $this->assertIsString($newKeypair);
        $this->assertNotSame($originalKeypair, $newKeypair, 'Re-enroll must rotate the site keypair');

        // The CP key persisted is the freshly returned one.
        $this->assertSame($cpPublic, $this->keystore->getControlPlanePublicKey());
    }
}
