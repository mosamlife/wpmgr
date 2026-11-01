<?php
/**
 * Tests for HeartbeatCatchup: overdue + no lock → send + write lock;
 * lock fresh → no send; not overdue → no send; not enrolled → no send.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\PingCommand;
use WPMgr\Agent\Enrollment;
use WPMgr\Agent\HeartbeatCatchup;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;
use WPMgr\Agent\Commands\MetadataCommand;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\HeartbeatCatchup
 */
final class HeartbeatCatchupTest extends TestCase
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

        $this->keyFile = sys_get_temp_dir() . '/wpmgr-hbc-' . bin2hex(random_bytes(8)) . '.key';

        $this->options = [];

        // Option store backed by in-memory array.
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

        // Stubs required by Settings::get() two-tier resolution.
        Functions\when('get_site_option')->alias(function (string $name, $default = false) {
            $sentinel = '__wpmgr_settings_missing__';
            if ($default === $sentinel) {
                return $sentinel; // Signal "not in site options" so get_option fallback runs.
            }
            return $this->options[$name] ?? $default;
        });
        Functions\when('is_multisite')->justReturn(false);

        // Stubs required by Enrollment::buildHeartbeatPayload().
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
        Functions\when('esc_url_raw')->returnArg();
        Functions\when('wp_parse_url')->alias(static fn ($url, $c = -1) => parse_url($url, $c));
        Functions\when('home_url')->justReturn('https://example.test');
        Functions\when('get_bloginfo')->justReturn('6.5.0');
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
        Functions\when('wp_remote_retrieve_body')->justReturn('{}');

        // Stubs needed by MetadataCommand::collect() through Enrollment.
        Functions\when('is_multisite')->justReturn(false);
        Functions\when('wp_get_themes')->justReturn([]);
        Functions\when('get_plugins')->justReturn([]);
        Functions\when('get_stylesheet')->justReturn('');
        Functions\when('get_template')->justReturn('');
        Functions\when('wp_get_theme')->justReturn(null);
        Functions\when('get_users')->justReturn([]);

        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }

        $keystore   = new Keystore();
        \WPMgr\Agent\Tests\OptionsBackedKeyRowWpdb::around(fn () => $keystore->generateSiteKeypair());
        $this->settings   = new Settings();
        $signer     = new Signer($keystore);
        $this->enrollment = new Enrollment($keystore, $this->settings, $signer, new MetadataCommand());
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
    // Helpers
    // -------------------------------------------------------------------------

    /**
     * Enroll the site so Settings::isEnrolled() returns true.
     */
    private function simulateEnrolled(): void
    {
        $this->settings->setControlPlaneUrl('https://cp.example.test');
        $this->options[Settings::OPTION_SITE_ID] = 'site-abc';
    }

    // -------------------------------------------------------------------------
    // Not enrolled → no send
    // -------------------------------------------------------------------------

    public function test_does_not_send_when_not_enrolled(): void
    {
        // wp_remote_post must NOT be called.
        Functions\expect('wp_remote_post')->never();

        $catchup = new HeartbeatCatchup($this->settings, $this->enrollment);
        $catchup->maybeSend();

        // Brain Monkey verifies the ->never() expectation at teardown; this
        // explicit count satisfies PHPUnit's "at least one assertion" check.
        $this->addToAssertionCount(1);
    }

    // -------------------------------------------------------------------------
    // Enrolled, overdue, no lock → sends one heartbeat and writes lock
    // -------------------------------------------------------------------------

    public function test_sends_heartbeat_when_overdue_and_no_lock(): void
    {
        $this->simulateEnrolled();

        // Mark as overdue: option is 300 s ago (well past the 120 s threshold).
        $this->options[PingCommand::OPTION_LAST_HEARTBEAT_AT] = time() - 300;

        // Expect wp_remote_post called exactly once (the heartbeat POST) with
        // the short shutdown-path timeout so a slow CP can never hold the FPM
        // worker long after output.
        $capturedArgs = null;
        Functions\expect('wp_remote_post')
            ->once()
            ->andReturnUsing(function ($url, $args) use (&$capturedArgs) {
                $capturedArgs = $args;
                return ['ok' => true];
            });

        $catchup = new HeartbeatCatchup($this->settings, $this->enrollment);
        $catchup->maybeSend();

        $this->assertIsArray($capturedArgs);
        $this->assertSame(5, $capturedArgs['timeout'], 'Catch-up heartbeat must use the 5 s shutdown timeout');

        // Lock must have been written.
        $this->assertArrayHasKey(HeartbeatCatchup::OPTION_LOCK, $this->options);
        $lockAge = time() - (int) $this->options[HeartbeatCatchup::OPTION_LOCK];
        $this->assertLessThan(5, $lockAge, 'Lock timestamp should be within 5 s of now');
    }

    // -------------------------------------------------------------------------
    // Fresh lock → no send
    // -------------------------------------------------------------------------

    public function test_does_not_send_when_lock_is_fresh(): void
    {
        $this->simulateEnrolled();

        // Mark as overdue.
        $this->options[PingCommand::OPTION_LAST_HEARTBEAT_AT] = time() - 300;

        // Write a fresh lock (10 s ago — within the 60 s window).
        $this->options[HeartbeatCatchup::OPTION_LOCK] = time() - 10;

        // wp_remote_post must NOT be called.
        Functions\expect('wp_remote_post')->never();

        $catchup = new HeartbeatCatchup($this->settings, $this->enrollment);
        $catchup->maybeSend();

        $this->addToAssertionCount(1);
    }

    // -------------------------------------------------------------------------
    // Not overdue → no send
    // -------------------------------------------------------------------------

    public function test_does_not_send_when_heartbeat_not_overdue(): void
    {
        $this->simulateEnrolled();

        // Last heartbeat was 30 s ago — not overdue (threshold is 120 s).
        $this->options[PingCommand::OPTION_LAST_HEARTBEAT_AT] = time() - 30;

        // wp_remote_post must NOT be called.
        Functions\expect('wp_remote_post')->never();

        $catchup = new HeartbeatCatchup($this->settings, $this->enrollment);
        $catchup->maybeSend();

        $this->addToAssertionCount(1);
    }

    // -------------------------------------------------------------------------
    // Expired lock → sends (lock older than 60 s is treated as stale)
    // -------------------------------------------------------------------------

    public function test_sends_when_lock_is_expired(): void
    {
        $this->simulateEnrolled();

        // Mark as overdue.
        $this->options[PingCommand::OPTION_LAST_HEARTBEAT_AT] = time() - 300;

        // Write a stale lock (90 s ago — outside the 60 s window).
        $this->options[HeartbeatCatchup::OPTION_LOCK] = time() - 90;

        Functions\expect('wp_remote_post')
            ->once()
            ->andReturn(['ok' => true]);

        $catchup = new HeartbeatCatchup($this->settings, $this->enrollment);
        $catchup->maybeSend();

        // Lock must have been refreshed.
        $lockAge = time() - (int) $this->options[HeartbeatCatchup::OPTION_LOCK];
        $this->assertLessThan(5, $lockAge, 'Lock timestamp should be refreshed to near-now');
    }

    // -------------------------------------------------------------------------
    // Missing last-heartbeat option counts as overdue
    // -------------------------------------------------------------------------

    public function test_sends_when_heartbeat_option_missing(): void
    {
        $this->simulateEnrolled();

        // Do not set OPTION_LAST_HEARTBEAT_AT at all — treat as overdue.
        Functions\expect('wp_remote_post')
            ->once()
            ->andReturn(['ok' => true]);

        $catchup = new HeartbeatCatchup($this->settings, $this->enrollment);
        $catchup->maybeSend();

        // Lock must have been written (confirms the send path executed).
        $this->assertArrayHasKey(HeartbeatCatchup::OPTION_LOCK, $this->options);
    }
}
