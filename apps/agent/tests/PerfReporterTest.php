<?php
/**
 * PerfReporterTest — validates the reporter envelope shape and option-persistence
 * helpers without actually POSTing to any network endpoint. All WP functions are
 * stubbed via Brain Monkey.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Cache\CacheManager;
use WPMgr\Agent\Cache\PerfReporter;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Optimizer\PerfConfig;
use WPMgr\Agent\Settings;
use WPMgr\Agent\Signer;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Cache\PerfReporter
 */
final class PerfReporterTest extends TestCase
{
    /** @var array<string,mixed> */
    private array $optionStore = [];

    private string $keyFile;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->optionStore = [];
        Functions\when('get_option')->alias(fn ($k, $d = false) => $this->optionStore[$k] ?? $d);
        Functions\when('update_option')->alias(function ($k, $v) {
            $this->optionStore[$k] = $v;
            return true;
        });
        Functions\when('delete_option')->alias(function ($k) {
            unset($this->optionStore[$k]);
            return true;
        });

        // GH #174 ack tests exercise the real signed-post path (reportInstallState).
        unset($_SERVER['SERVER_SOFTWARE']);
        $this->keyFile = sys_get_temp_dir() . '/wpmgr-perf-reporter-' . bin2hex(random_bytes(8)) . '.key';
        if (!defined('WPMGR_AGENT_KEY_FILE')) {
            define('WPMGR_AGENT_KEY_FILE', $this->keyFile);
        }
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
    }

    protected function tear_down(): void
    {
        unset($_SERVER['SERVER_SOFTWARE']);
        if (is_file($this->keyFile)) {
            @unlink($this->keyFile);
        }
        Monkey\tearDown();
        parent::tear_down();
    }

    // -------------------------------------------------------------------------
    // GH #174 — config-ack carries rum_beacon_present (never the plaintext key)
    // -------------------------------------------------------------------------

    /**
     * Builds an enrolled PerfReporter wired to a real Signer/Keystore so
     * reportInstallState() runs its full signed-post path, and captures the
     * exact JSON body handed to wp_remote_post.
     *
     * @return array<string,mixed> Decoded config-ack body.
     */
    private function captureConfigAckBody(): array
    {
        $this->optionStore[Settings::OPTION_CP_URL]  = 'https://cp.example.test';
        $this->optionStore[Settings::OPTION_SITE_ID] = 'site-abc';

        $keystore = new Keystore();
        $keystore->generateSiteKeypair();
        $settings = new Settings();
        $signer   = new Signer($keystore);
        $cache    = new CacheManager();
        $reporter = new PerfReporter($settings, $signer, $cache);

        $captured = null;
        Functions\expect('wp_remote_post')
            ->once()
            ->andReturnUsing(function ($url, $args) use (&$captured) {
                $captured = $args;
                return ['ok' => true];
            });

        $reporter->reportInstallState();

        $this->assertIsArray($captured, 'reportInstallState() must POST when enrolled');
        $decoded = json_decode((string) $captured['body'], true);
        $this->assertIsArray($decoded);

        return $decoded;
    }

    public function test_config_ack_reports_rum_beacon_present_true_when_key_held(): void
    {
        $this->optionStore[PerfConfig::OPTION] = ['rum_beacon_key' => 'plaintext-beacon-key-value'];

        $body = $this->captureConfigAckBody();

        $this->assertArrayHasKey('rum_beacon_present', $body);
        $this->assertTrue($body['rum_beacon_present']);
    }

    public function test_config_ack_reports_rum_beacon_present_false_when_key_empty(): void
    {
        $this->optionStore[PerfConfig::OPTION] = ['rum_beacon_key' => ''];

        $body = $this->captureConfigAckBody();

        $this->assertArrayHasKey('rum_beacon_present', $body);
        $this->assertFalse($body['rum_beacon_present']);
    }

    public function test_config_ack_reports_rum_beacon_present_false_when_option_absent(): void
    {
        // No PerfConfig::OPTION seeded at all — PerfConfig::load() defaults to ''.
        $body = $this->captureConfigAckBody();

        $this->assertArrayHasKey('rum_beacon_present', $body);
        $this->assertFalse($body['rum_beacon_present']);
    }

    public function test_config_ack_never_sends_plaintext_beacon_key(): void
    {
        $this->optionStore[PerfConfig::OPTION] = ['rum_beacon_key' => 'super-secret-plaintext-key'];

        $body = $this->captureConfigAckBody();

        $this->assertArrayNotHasKey('rum_beacon_key', $body);
        $encoded = (string) wp_json_encode($body);
        $this->assertStringNotContainsString('super-secret-plaintext-key', $encoded);
    }

    public function test_persist_config_version_stores_option(): void
    {
        PerfReporter::persistConfigVersion(42);
        $this->assertSame(42, $this->optionStore[PerfReporter::OPTION_PERF_CONFIG_VERSION]);
    }

    public function test_persist_preload_total_stores_option(): void
    {
        PerfReporter::persistPreloadTotal(100);
        $this->assertSame(100, $this->optionStore[PerfReporter::OPTION_PRELOAD_TOTAL]);
    }

    public function test_persist_last_preload_at_stores_option(): void
    {
        $ts = time();
        PerfReporter::persistLastPreloadAt($ts);
        $this->assertSame($ts, $this->optionStore[PerfReporter::OPTION_LAST_PRELOAD_AT]);
    }

    public function test_persist_last_purge_stores_timestamp_and_kind(): void
    {
        $ts = time();
        PerfReporter::persistLastPurge($ts, 'all');
        $this->assertSame($ts, $this->optionStore[PerfReporter::OPTION_LAST_PURGED_AT]);
        $this->assertSame('all', $this->optionStore[PerfReporter::OPTION_LAST_PURGE_KIND]);
    }

    public function test_persist_last_purge_defaults_kind_to_all(): void
    {
        PerfReporter::persistLastPurge(time());
        $this->assertSame('all', $this->optionStore[PerfReporter::OPTION_LAST_PURGE_KIND]);
    }

    public function test_option_keys_are_non_empty_strings(): void
    {
        $this->assertNotEmpty(PerfReporter::OPTION_PERF_CONFIG_VERSION);
        $this->assertNotEmpty(PerfReporter::OPTION_PRELOAD_TOTAL);
        $this->assertNotEmpty(PerfReporter::OPTION_LAST_PRELOAD_AT);
    }
}
