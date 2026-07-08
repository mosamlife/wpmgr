<?php
/**
 * Cache command-handler tests: stable names, body validation, and well-formed
 * ack envelopes. The CacheManager's wp-option reads/writes are stubbed via Brain
 * Monkey; server-side artefact writes (wp-config / drop-in / .htaccess) are not
 * exercised here (covered by the dedicated unit tests), so we assert the
 * handlers' own contract: validation + envelope shape.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Cache\CacheManager;
use WPMgr\Agent\Commands\CacheDisableCommand;
use WPMgr\Agent\Commands\CacheEnableCommand;
use WPMgr\Agent\Commands\CachePreloadCommand;
use WPMgr\Agent\Commands\CachePurgeCommand;
use WPMgr\Agent\Commands\DbCleanCommand;
use WPMgr\Agent\Commands\PerfConfigUpdateCommand;
use WPMgr\Agent\Optimizer\PerfConfig;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\CacheEnableCommand
 * @covers \WPMgr\Agent\Commands\CacheDisableCommand
 * @covers \WPMgr\Agent\Commands\CachePurgeCommand
 * @covers \WPMgr\Agent\Commands\CachePreloadCommand
 * @covers \WPMgr\Agent\Commands\PerfConfigUpdateCommand
 * @covers \WPMgr\Agent\Commands\DbCleanCommand
 */
final class CacheCommandsTest extends TestCase
{
    /** @var array<string,mixed> */
    private array $optionStore = [];

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
        Functions\when('home_url')->justReturn('https://example.com/');
        Functions\when('wp_next_scheduled')->justReturn(false);
        Functions\when('wp_schedule_single_event')->justReturn(true);
        Functions\when('wp_schedule_event')->justReturn(true);
        Functions\when('wp_clear_scheduled_hook')->justReturn(true);
        Functions\when('add_filter')->justReturn(true);
        Functions\when('add_action')->justReturn(true);

        // Task #171 — the preload warmer now enqueues into a custom table via
        // PreloadQueue. Install a minimal in-memory $wpdb double so addTask()
        // INSERTs and pendingCount() COUNTs are consistent; loopback dispatch is
        // inert here (rest_url is unstubbed, but claimable count drives dispatch
        // so it no-ops cleanly in the unit context).
        $GLOBALS['wpdb'] = new FakePreloadQueueWpdb();
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    private function manager(): CacheManager
    {
        // Default CacheManager; WP_CONTENT_DIR is undefined in tests so cacheRoot
        // resolves to '' and the file operations are inert no-ops — exactly what
        // we want for envelope-shape assertions.
        return new CacheManager();
    }

    public function test_command_names_match_cp_contract(): void
    {
        $mgr = $this->manager();
        $this->assertSame('cache_enable', (new CacheEnableCommand($mgr))->name());
        $this->assertSame('cache_disable', (new CacheDisableCommand($mgr))->name());
        $this->assertSame('cache_purge', (new CachePurgeCommand($mgr))->name());
        $this->assertSame('cache_preload', (new CachePreloadCommand($mgr))->name());
        $this->assertSame('perf_config_update', (new PerfConfigUpdateCommand($mgr))->name());
        $this->assertSame('db_clean', (new DbCleanCommand())->name());
    }

    public function test_purge_url_scope_requires_url(): void
    {
        $cmd = new CachePurgeCommand($this->manager());
        $res = $cmd->execute([], ['scope' => 'url']);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('url', $res['detail']);
    }

    public function test_purge_all_returns_envelope(): void
    {
        $cmd = new CachePurgeCommand($this->manager());
        $res = $cmd->execute([], ['scope' => 'all']);
        $this->assertArrayHasKey('ok', $res);
        $this->assertArrayHasKey('detail', $res);
        $this->assertArrayHasKey('stats', $res);
    }

    public function test_preload_rejects_non_array_urls(): void
    {
        $cmd = new CachePreloadCommand($this->manager());
        $res = $cmd->execute([], ['urls' => 'not-an-array']);
        $this->assertFalse($res['ok']);
    }

    public function test_preload_queues_urls(): void
    {
        $cmd = new CachePreloadCommand($this->manager());
        $res = $cmd->execute([], ['urls' => ['https://example.com/a', 'https://example.com/b']]);
        $this->assertTrue($res['ok']);
        $this->assertArrayHasKey('queued', $res);
        $this->assertSame(2, $res['queued']);
    }

    public function test_perf_config_update_rejects_empty_payload(): void
    {
        $cmd = new PerfConfigUpdateCommand($this->manager());
        $res = $cmd->execute([], ['unrelated' => 1]);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('recognised', $res['detail']);
    }

    public function test_perf_config_update_persists_known_fields(): void
    {
        $cmd = new PerfConfigUpdateCommand($this->manager());
        $res = $cmd->execute([], [
            'cache_mobile'     => true,
            'auto_purge'       => false,
            'refresh_interval' => 7200,
            'include_cookies'  => ['geo'],
        ]);
        $this->assertTrue($res['ok']);

        $stored = $this->optionStore[CacheManager::OPTION_CONFIG] ?? [];
        $this->assertTrue($stored['cache_mobile']);
        $this->assertFalse($stored['auto_purge']);
        $this->assertSame(7200, $stored['refresh_interval']);
        $this->assertSame(['geo'], $stored['include_cookies']);
    }

    /**
     * The command stores the four page-cache variant lists under their
     * unprefixed keys when the control plane sends them on the wire.
     */
    public function test_perf_config_update_persists_unprefixed_cache_variant_keys(): void
    {
        $cmd = new PerfConfigUpdateCommand($this->manager());
        $res = $cmd->execute([], [
            'bypass_urls'      => ['/cart', '/checkout'],
            'bypass_cookies'   => ['my_session'],
            'include_queries'  => ['sort_dir'],
            'include_cookies'  => ['geo'],
        ]);
        $this->assertTrue($res['ok']);

        $stored = $this->optionStore[CacheManager::OPTION_CONFIG] ?? [];
        $this->assertSame(['/cart', '/checkout'], $stored['bypass_urls']);
        $this->assertSame(['my_session'], $stored['bypass_cookies']);
        $this->assertSame(['sort_dir'], $stored['include_queries']);
        $this->assertSame(['geo'], $stored['include_cookies']);
    }

    /**
     * Prefixed cache_* names are not in the command whitelist, so a payload
     * using them is rejected and nothing is persisted. Guards against silently
     * accepting the old wire names again.
     */
    public function test_perf_config_update_ignores_prefixed_cache_variant_aliases(): void
    {
        $cmd = new PerfConfigUpdateCommand($this->manager());
        $res = $cmd->execute([], [
            'cache_bypass_urls'     => ['/cart'],
            'cache_bypass_cookies'  => ['my_session'],
            'cache_include_queries' => ['sort_dir'],
            'cache_include_cookies' => ['geo'],
        ]);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('recognised', $res['detail']);

        $stored = $this->optionStore[CacheManager::OPTION_CONFIG] ?? [];
        $this->assertArrayNotHasKey('bypass_urls', $stored);
        $this->assertArrayNotHasKey('bypass_cookies', $stored);
        $this->assertArrayNotHasKey('include_queries', $stored);
        $this->assertArrayNotHasKey('include_cookies', $stored);
    }

    public function test_perf_config_update_clamps_absurd_refresh_interval(): void
    {
        $cmd = new PerfConfigUpdateCommand($this->manager());
        $cmd->execute([], ['refresh_interval' => 999999999]);
        $stored = $this->optionStore[CacheManager::OPTION_CONFIG] ?? [];
        $this->assertLessThanOrEqual(2592000, $stored['refresh_interval']);
    }

    public function test_perf_config_preserves_enabled_state_when_omitted(): void
    {
        // Seed an enabled config.
        $this->optionStore[CacheManager::OPTION_CONFIG] = ['enabled' => true];
        $cmd = new PerfConfigUpdateCommand($this->manager());
        $cmd->execute([], ['cache_mobile' => true]);
        $stored = $this->optionStore[CacheManager::OPTION_CONFIG] ?? [];
        $this->assertTrue($stored['enabled'], 'enabled must be preserved when not in the payload');
    }

    public function test_db_clean_returns_ack_envelope(): void
    {
        // M38 contract: execute() returns the frozen ACK immediately.
        // job_id is REQUIRED; tasks and progress_endpoint are optional.
        $jobId = 'test-job-' . bin2hex(random_bytes(4));
        $res   = (new DbCleanCommand())->execute([], [
            'job_id' => $jobId,
            'tasks'  => ['revisions'],
        ]);
        $this->assertTrue($res['ok']);
        $this->assertSame($jobId, $res['job_id']);
        // The old 'detail' and 'cleaned' keys are NOT present in the ACK.
        $this->assertArrayNotHasKey('detail', $res);
        $this->assertArrayNotHasKey('cleaned', $res);
    }

    public function test_db_clean_rejects_missing_job_id(): void
    {
        $res = (new DbCleanCommand())->execute([], ['tasks' => ['revisions']]);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('job_id', (string) ($res['detail'] ?? ''));
    }

    public function test_enable_returns_envelope_with_stats(): void
    {
        // Pretty permalinks present, so the pre-flight guard passes.
        $this->optionStore['permalink_structure'] = '/%postname%/';
        $cmd = new CacheEnableCommand($this->manager());
        $res = $cmd->execute([], []);
        $this->assertArrayHasKey('ok', $res);
        $this->assertArrayHasKey('detail', $res);
        $this->assertArrayHasKey('stats', $res);
        // The config option is persisted with enabled=true even if artefact
        // writes are inert in the test environment.
        $stored = $this->optionStore[CacheManager::OPTION_CONFIG] ?? [];
        $this->assertTrue($stored['enabled']);
    }

    public function test_enable_returns_top_level_install_state_fields(): void
    {
        $this->optionStore['permalink_structure'] = '/%postname%/';
        $cmd = new CacheEnableCommand($this->manager());
        $res = $cmd->execute([], []);
        // These top-level fields are required by the CP dashboard "Verify" card.
        $this->assertArrayHasKey('server_software', $res);
        $this->assertArrayHasKey('dropin_installed', $res);
        $this->assertArrayHasKey('wp_cache_constant_set', $res);
        $this->assertArrayHasKey('htaccess_managed', $res);
        // Values are booleans (the exact value depends on the test environment).
        $this->assertIsBool($res['dropin_installed']);
        $this->assertIsBool($res['wp_cache_constant_set']);
        $this->assertIsBool($res['htaccess_managed']);
        $this->assertIsString($res['server_software']);
    }

    public function test_perf_config_update_returns_install_state_fields(): void
    {
        $cmd = new PerfConfigUpdateCommand($this->manager());
        $res = $cmd->execute([], [
            'cache_mobile' => true,
        ]);
        $this->assertTrue($res['ok']);
        $this->assertArrayHasKey('server_software', $res);
        $this->assertArrayHasKey('dropin_installed', $res);
        $this->assertArrayHasKey('wp_cache_constant_set', $res);
        $this->assertArrayHasKey('htaccess_managed', $res);
    }

    /**
     * Ensures CDN rewrite settings are stored under the agent keys read by the optimizer.
     */
    public function test_perf_config_update_persists_cdn_rewrite_config(): void
    {
        $cmd = new PerfConfigUpdateCommand($this->manager());
        $res = $cmd->execute([], [
            'cdn'             => true,
            'cdn_url'         => 'cdn.example.net',
            'cdn_file_types'  => 'image',
            'config_version'  => 7,
        ]);
        $this->assertTrue($res['ok']);

        $stored = $this->optionStore['wpmgr_perf_config'] ?? [];
        $this->assertTrue($stored['cdn']);
        $this->assertSame('cdn.example.net', $stored['cdn_url']);
        $this->assertSame('image', $stored['cdn_file_types']);

        // config_version lives in its own option, not the optimization option.
        $this->assertArrayHasKey('wpmgr_perf_config_version', $this->optionStore);
        $this->assertSame(7, $this->optionStore['wpmgr_perf_config_version']);
    }

    public function test_enable_refused_on_plain_permalinks(): void
    {
        // Plain permalinks (?p=123) give no URL path to key a disk cache file on,
        // so cache-enable must refuse rather than silently cache nothing.
        $this->optionStore['permalink_structure'] = '';
        $cmd = new CacheEnableCommand($this->manager());
        $res = $cmd->execute([], []);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsStringIgnoringCase('permalink', $res['detail']);
        // It must NOT have flipped the config to enabled.
        $stored = $this->optionStore[CacheManager::OPTION_CONFIG] ?? [];
        $this->assertArrayNotHasKey('stats', $res);
        $this->assertTrue(empty($stored['enabled']));
    }

    // -------------------------------------------------------------------------
    // GH #174 — merge hardening: a present-but-empty rum_beacon_key must never
    // clobber a good stored key (array_intersect_key keys on presence, not
    // value). This is defense-in-depth: the CP relies on omitempty to drop the
    // field entirely, but the agent must be robust regardless of the wire.
    // -------------------------------------------------------------------------

    public function test_perf_config_update_empty_beacon_key_does_not_clobber_stored_key(): void
    {
        $this->optionStore[PerfConfig::OPTION] = ['rum_beacon_key' => 'good-existing-key'];

        $cmd = new PerfConfigUpdateCommand($this->manager());
        $res = $cmd->execute([], ['rum_beacon_key' => '']);
        $this->assertTrue($res['ok']);

        $stored = $this->optionStore[PerfConfig::OPTION] ?? [];
        $this->assertSame(
            'good-existing-key',
            $stored['rum_beacon_key'],
            'an empty incoming rum_beacon_key must never clobber a good stored key'
        );
    }

    public function test_perf_config_update_nonempty_beacon_key_overwrites_stored_key(): void
    {
        $this->optionStore[PerfConfig::OPTION] = ['rum_beacon_key' => 'old-key'];

        $cmd = new PerfConfigUpdateCommand($this->manager());
        $res = $cmd->execute([], ['rum_beacon_key' => 'rotated-new-key']);
        $this->assertTrue($res['ok']);

        $stored = $this->optionStore[PerfConfig::OPTION] ?? [];
        $this->assertSame('rotated-new-key', $stored['rum_beacon_key'], 'a non-empty incoming key must still rotate normally');
    }

    public function test_perf_config_update_beacon_key_first_mint_from_empty(): void
    {
        // No prior key stored (fresh site): an incoming non-empty key must land
        // (the guard only protects an existing GOOD key from being blanked).
        $this->optionStore[PerfConfig::OPTION] = ['rum_beacon_key' => ''];

        $cmd = new PerfConfigUpdateCommand($this->manager());
        $res = $cmd->execute([], ['rum_beacon_key' => 'first-minted-key']);
        $this->assertTrue($res['ok']);

        $stored = $this->optionStore[PerfConfig::OPTION] ?? [];
        $this->assertSame('first-minted-key', $stored['rum_beacon_key']);
    }

    public function test_perf_config_update_omitted_beacon_key_leaves_stored_key_unchanged(): void
    {
        $this->optionStore[PerfConfig::OPTION] = ['rum_beacon_key' => 'unchanged-key'];

        $cmd = new PerfConfigUpdateCommand($this->manager());
        // Push some unrelated optimization field; rum_beacon_key is absent.
        $res = $cmd->execute([], ['cdn' => true]);
        $this->assertTrue($res['ok']);

        $stored = $this->optionStore[PerfConfig::OPTION] ?? [];
        $this->assertSame('unchanged-key', $stored['rum_beacon_key']);
    }
}
