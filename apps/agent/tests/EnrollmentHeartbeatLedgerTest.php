<?php
/**
 * Tests for the heartbeat ledger: Enrollment::sendHeartbeat writes
 * wpmgr_last_heartbeat_at on a successful send, and does NOT write it on
 * a failed send.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\MetadataCommand;
use WPMgr\Agent\Commands\PingCommand;
use WPMgr\Agent\Enrollment;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Enrollment::sendHeartbeat
 */
final class EnrollmentHeartbeatLedgerTest extends TestCase
{
    private string $keyFile;

    /** @var array<string,mixed> */
    private array $options = [];

    private Settings $settings;

    private Enrollment $enrollment;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-hbl-' . bin2hex(random_bytes(8)) . '.key';

        $this->options = [];

        Functions\when('update_option')->alias(function (string $name, $value, $autoload = null) {
            $this->options[$name] = $value;
            return true;
        });
        Functions\when('get_option')->alias(function (string $name, $default = false) {
            return $this->options[$name] ?? $default;
        });
        Functions\when('delete_option')->alias(function (string $name) {
            unset($this->options[$name]);
            return true;
        });

        // Settings two-tier resolution stubs.
        Functions\when('get_site_option')->alias(function (string $name, $default = false) {
            $sentinel = '__wpmgr_settings_missing__';
            if ($default === $sentinel) {
                return $sentinel;
            }
            return $this->options[$name] ?? $default;
        });
        Functions\when('is_multisite')->justReturn(false);

        // Network / WP stubs required by Enrollment.
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
        Functions\when('esc_url_raw')->returnArg();
        Functions\when('wp_parse_url')->alias(static fn ($url, $c = -1) => parse_url($url, $c));
        Functions\when('home_url')->justReturn('https://example.test');
        Functions\when('get_bloginfo')->justReturn('6.5.0');
        Functions\when('is_wp_error')->justReturn(false);

        // MetadataCommand stubs.
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_stylesheet')->justReturn('');
        Functions\when('get_template')->justReturn('');
        Functions\when('wp_get_theme')->justReturn(null);
        Functions\when('get_users')->justReturn([]);

        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }

        $keystore         = new Keystore();
        \WPMgr\Agent\Tests\OptionsBackedKeyRowWpdb::around(fn () => $keystore->generateSiteKeypair());
        $this->settings   = new Settings();
        $signer           = new Signer($keystore);
        $this->enrollment = new Enrollment($keystore, $this->settings, $signer, new MetadataCommand());

        // Enroll the site so sendHeartbeat() does not short-circuit.
        $this->settings->setControlPlaneUrl('https://cp.example.test');
        $this->options[Settings::OPTION_SITE_ID] = 'site-abc';
    }

    protected function tear_down(): void
    {
        if (is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // Successful send → writes wpmgr_last_heartbeat_at
    // -------------------------------------------------------------------------

    public function test_successful_send_updates_last_heartbeat_at(): void
    {
        Functions\when('wp_remote_post')->justReturn(['ok' => true]);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
        Functions\when('wp_remote_retrieve_body')->justReturn('{"ok":true,"instructions":[],"revoke_token":""}');

        $before = time();
        $this->enrollment->sendHeartbeat();

        $this->assertArrayHasKey(
            PingCommand::OPTION_LAST_HEARTBEAT_AT,
            $this->options,
            'wpmgr_last_heartbeat_at must be written after a successful heartbeat'
        );

        $stored = (int) $this->options[PingCommand::OPTION_LAST_HEARTBEAT_AT];
        $this->assertGreaterThanOrEqual($before, $stored);
        $this->assertLessThanOrEqual(time(), $stored);
    }

    // -------------------------------------------------------------------------
    // Failed send (non-2xx) → does NOT write wpmgr_last_heartbeat_at
    // -------------------------------------------------------------------------

    public function test_failed_send_does_not_update_last_heartbeat_at(): void
    {
        Functions\when('wp_remote_post')->justReturn(['ok' => true]);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(503);
        Functions\when('wp_remote_retrieve_body')->justReturn('');

        $this->enrollment->sendHeartbeat();

        $this->assertArrayNotHasKey(
            PingCommand::OPTION_LAST_HEARTBEAT_AT,
            $this->options,
            'wpmgr_last_heartbeat_at must NOT be written when the heartbeat POST fails'
        );
    }

    // -------------------------------------------------------------------------
    // WP_Error (unreachable CP) → does NOT write wpmgr_last_heartbeat_at
    // -------------------------------------------------------------------------

    public function test_wp_error_send_does_not_update_last_heartbeat_at(): void
    {
        // is_wp_error returns true → signedPost treats this as "unreachable".
        Functions\when('wp_remote_post')->justReturn(new \WP_Error('http_request_failed', 'cURL error'));
        Functions\when('is_wp_error')->justReturn(true);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(0);
        Functions\when('wp_remote_retrieve_body')->justReturn('');

        $this->enrollment->sendHeartbeat();

        $this->assertArrayNotHasKey(
            PingCommand::OPTION_LAST_HEARTBEAT_AT,
            $this->options,
            'wpmgr_last_heartbeat_at must NOT be written when the CP is unreachable'
        );
    }
}
