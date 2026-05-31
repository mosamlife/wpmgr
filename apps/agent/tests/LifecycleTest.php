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

    /**
     * Wire the teardown-observing WP stubs (deactivate_plugins, plugin_basename,
     * do_action) and the constants the revoke path needs, returning the captured
     * &$deactivated / &$revokeHook sinks by reference.
     *
     * @param array<int,string> $deactivated Out: deactivated plugin basenames.
     * @param array<int,mixed>  $revokeHook  Out: wpmgr_revoking_self reasons.
     * @return void
     */
    private function armRevokeObservers(array &$deactivated, array &$revokeHook): void
    {
        if (!defined('WPMGR_AGENT_FILE')) {
            define('WPMGR_AGENT_FILE', __FILE__);
        }
        if (!defined('ABSPATH')) {
            // Point at a non-existent path so the plugin.php require is skipped.
            define('ABSPATH', '/nonexistent-wpmgr-' . bin2hex(random_bytes(6)) . '/');
        }

        Functions\when('plugin_basename')->alias(static fn ($f) => basename((string) $f));
        Functions\when('deactivate_plugins')->alias(function ($plugin) use (&$deactivated) {
            $deactivated[] = $plugin;
            return null;
        });
        Functions\when('do_action')->alias(function ($hook, $reason = null) use (&$revokeHook) {
            if ($hook === 'wpmgr_revoking_self') {
                $revokeHook[] = $reason;
            }
            return null;
        });
    }

    /**
     * Build a Lifecycle whose signed-revoke-token verifier is the supplied
     * closure (the test seam added for Phase-6 finding B). The closure stands in
     * for Connector::verifyCommand: it returns the validated claims or throws.
     *
     * @param \Closure(string):array<string,mixed> $verifier Verifier seam.
     * @return Lifecycle
     */
    private function lifecycleWithVerifier(\Closure $verifier): Lifecycle
    {
        return new Lifecycle($this->keystore, $this->settings, $this->enrollment, $verifier);
    }

    public function test_revoke_instruction_wipes_keys_and_deactivates_plugin(): void
    {
        $deactivated = [];
        $revokeHook  = [];
        $this->armRevokeObservers($deactivated, $revokeHook);

        // A VALID signed revoke proof: the verifier accepts it and returns claims
        // bound to this site (aud) and to the revoke command (cmd).
        $seen      = [];
        $lifecycle = $this->lifecycleWithVerifier(function (string $jwt) use (&$seen) {
            $seen[] = $jwt;
            return ['cmd' => Lifecycle::REVOKE_CMD, 'aud' => $this->settings->siteId(), 'jti' => 'j1'];
        });

        // Pre-conditions: enrolled + keypair present.
        $this->assertTrue($this->settings->isEnrolled());
        $this->assertNotNull($this->keystore->getSiteKeypair());

        $lifecycle->handleInstructions(['revoke'], 'signed.revoke.jwt');

        // The verifier WAS consulted with the supplied token.
        $this->assertSame(['signed.revoke.jwt'], $seen);

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

    public function test_revoke_without_token_does_not_tear_down(): void
    {
        $deactivated = [];
        $revokeHook  = [];
        $this->armRevokeObservers($deactivated, $revokeHook);

        // The verifier must NOT even be consulted when the token is absent.
        $verifierCalled = false;
        $lifecycle = $this->lifecycleWithVerifier(function () use (&$verifierCalled) {
            $verifierCalled = true;
            return ['cmd' => Lifecycle::REVOKE_CMD, 'aud' => $this->settings->siteId()];
        });

        // Missing token (empty string) → revoke is a NO-OP.
        $lifecycle->handleInstructions(['revoke'], '');

        $this->assertFalse($verifierCalled, 'an absent token short-circuits before the verifier');
        $this->assertSame([], $revokeHook, 'no revoke hook fired');
        $this->assertSame([], $deactivated, 'plugin was NOT deactivated');
        $this->assertTrue($this->settings->isEnrolled(), 'enrollment preserved');
        $this->assertNotNull($this->keystore->getSiteKeypair(), 'keystore NOT wiped');
        $this->assertNull(Lifecycle::revokedMarker(), 'no revoked marker written');
    }

    public function test_revoke_with_invalid_token_does_not_tear_down(): void
    {
        $deactivated = [];
        $revokeHook  = [];
        $this->armRevokeObservers($deactivated, $revokeHook);

        // The verifier THROWS — mirrors Connector::verifyCommand rejecting a
        // bad signature / expired / replayed / wrong-aud token.
        $lifecycle = $this->lifecycleWithVerifier(function (): array {
            throw new \RuntimeException('WPMgr Agent: signature verification failed.');
        });

        $lifecycle->handleInstructions(['revoke'], 'forged.revoke.jwt');

        $this->assertSame([], $revokeHook);
        $this->assertSame([], $deactivated, 'a forged token must never deactivate');
        $this->assertTrue($this->settings->isEnrolled());
        $this->assertNotNull($this->keystore->getSiteKeypair());
        $this->assertNull(Lifecycle::revokedMarker());
    }

    public function test_revoke_with_wrong_cmd_claim_does_not_tear_down(): void
    {
        $deactivated = [];
        $revokeHook  = [];
        $this->armRevokeObservers($deactivated, $revokeHook);

        // Verifier returns claims for the WRONG command — the defense-in-depth
        // cmd re-assertion in the gate must reject it even though it "verified".
        $lifecycle = $this->lifecycleWithVerifier(function (): array {
            return ['cmd' => 'backup', 'aud' => $this->settings->siteId(), 'jti' => 'j2'];
        });

        $lifecycle->handleInstructions(['revoke'], 'wrong.cmd.jwt');

        $this->assertSame([], $revokeHook);
        $this->assertSame([], $deactivated, 'a non-revoke cmd must never deactivate');
        $this->assertTrue($this->settings->isEnrolled());
        $this->assertNotNull($this->keystore->getSiteKeypair());
        $this->assertNull(Lifecycle::revokedMarker());
    }

    public function test_revoke_with_aud_for_another_site_does_not_tear_down(): void
    {
        $deactivated = [];
        $revokeHook  = [];
        $this->armRevokeObservers($deactivated, $revokeHook);

        // Verifier returns claims whose aud is a DIFFERENT site — the gate's aud
        // re-assertion must reject it (a token captured for another tenant's site
        // must never tear THIS site down).
        $lifecycle = $this->lifecycleWithVerifier(function (): array {
            return ['cmd' => Lifecycle::REVOKE_CMD, 'aud' => 'some-other-site', 'jti' => 'j3'];
        });

        $lifecycle->handleInstructions(['revoke'], 'wrong.aud.jwt');

        $this->assertSame([], $revokeHook);
        $this->assertSame([], $deactivated, 'a wrong-aud token must never deactivate');
        $this->assertTrue($this->settings->isEnrolled());
        $this->assertNotNull($this->keystore->getSiteKeypair());
        $this->assertNull(Lifecycle::revokedMarker());
    }

    public function test_revoke_verified_through_real_connector_end_to_end(): void
    {
        // No injected seam: this exercises the PRODUCTION path through a real
        // Connector::verifyCommand against the stored CP public key. We mint a
        // genuine Ed25519 JWT with the CP secret to prove the wiring end-to-end.
        $deactivated = [];
        $revokeHook  = [];
        $this->armRevokeObservers($deactivated, $revokeHook);

        // Connector::verifyCommand records the jti in $wpdb; supply a fake one.
        $GLOBALS['wpdb'] = new FakeWpdb();
        \WPMgr\Agent\Connector::resetRequestCacheForTesting();

        // Re-key the keystore to a CP keypair whose SECRET we hold so we can sign.
        $cpKeypair = sodium_crypto_sign_keypair();
        $cpSecret  = sodium_crypto_sign_secretkey($cpKeypair);
        $this->keystore->storeControlPlanePublicKey(sodium_crypto_sign_publickey($cpKeypair));

        $now    = time();
        $claims = [
            'aud' => $this->settings->siteId(),
            'cmd' => 'revoke',
            'iat' => $now,
            'exp' => $now + 30,
            'jti' => 'real-revoke-' . bin2hex(random_bytes(4)),
            'iss' => 'wpmgr-cp',
        ];
        $b64 = static fn (string $d): string => rtrim(strtr(base64_encode($d), '+/', '-_'), '=');
        $signingInput = $b64((string) json_encode(['alg' => 'EdDSA', 'typ' => 'JWT']))
            . '.' . $b64((string) json_encode($claims));
        $jwt = $signingInput . '.' . $b64(sodium_crypto_sign_detached($signingInput, $cpSecret));

        // Production graph: Lifecycle with NO verifier seam (null) → real Connector.
        $lifecycle = new Lifecycle($this->keystore, $this->settings, $this->enrollment);
        $lifecycle->handleInstructions(['revoke'], $jwt);

        $this->assertSame([basename(__FILE__)], $deactivated, 'a genuine signed revoke tears down');
        $this->assertFalse($this->settings->isEnrolled());
        $this->assertNull($this->keystore->getSiteKeypair());

        unset($GLOBALS['wpdb']);
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

        $beat = $this->lifecycle->heartbeatNow();

        $this->assertSame(
            ['instructions' => [], 'revoke_token' => ''],
            $beat,
            'a failed immediate beat returns no instructions/token and does not throw'
        );
    }

    public function test_heartbeat_parses_revoke_token_alongside_instruction(): void
    {
        Functions\when('wp_remote_retrieve_body')->justReturn(json_encode([
            'ok'           => true,
            'instructions' => ['revoke'],
            'revoke_token' => 'header.payload.sig',
        ]));

        $result = $this->enrollment->sendHeartbeat();

        $this->assertSame(['revoke'], $result['instructions']);
        $this->assertSame('header.payload.sig', $result['revoke_token']);
    }

    public function test_heartbeat_revoke_token_absent_yields_empty_string(): void
    {
        Functions\when('wp_remote_retrieve_body')
            ->justReturn(json_encode(['ok' => true, 'instructions' => ['revoke']]));

        $result = $this->enrollment->sendHeartbeat();

        $this->assertSame(['revoke'], $result['instructions']);
        $this->assertSame('', $result['revoke_token'], 'no token field → empty string, gate fails closed');
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
