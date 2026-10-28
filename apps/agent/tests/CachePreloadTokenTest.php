<?php
/**
 * Preload requests are recognised by the site's own token.
 *
 * The agent generates a random per-site token, inlines it into the drop-in's
 * config, and sends it in the preload header on every self-fetch that warms a
 * page. The drop-in half (a header without the token is an ordinary visitor
 * request; with it the page is rendered fresh and stored) is pinned in
 * AdvancedCacheDropinTest. This file covers the agent half: the token, where
 * it is kept, and the self-fetches that carry it.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Cache\CacheConfig;
use WPMgr\Agent\Cache\Preload;
use WPMgr\Agent\Cache\PreloadToken;
use WPMgr\Agent\Commands\RucssComputeCommand;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Cache\PreloadToken
 * @covers \WPMgr\Agent\Cache\Preload
 * @covers \WPMgr\Agent\Cache\CacheConfig::toDropinArray
 * @covers \WPMgr\Agent\Commands\RucssComputeCommand
 */
final class CachePreloadTokenTest extends TestCase
{
    /** @var array<string,mixed> */
    private array $optionStore = [];

    /** @var list<array<string,mixed>> wp_remote_get arg arrays. */
    private array $fetchArgs = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->optionStore = [];
        $this->fetchArgs   = [];
        Functions\when('get_option')->alias(fn ($k, $d = false) => $this->optionStore[$k] ?? $d);
        Functions\when('update_option')->alias(function ($k, $v) {
            $this->optionStore[$k] = $v;
            return true;
        });
        Functions\when('home_url')->justReturn('https://example.com/');
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
        Functions\when('wp_remote_get')->alias(function ($url, $args = []) {
            $this->fetchArgs[] = is_array($args) ? $args : [];
            return ['response' => ['code' => 200]];
        });
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_token_is_random_hex_created_once_and_kept(): void
    {
        $first = PreloadToken::get();

        $this->assertMatchesRegularExpression('/^[a-f0-9]{64}$/', $first);
        $this->assertSame($first, $this->optionStore[PreloadToken::OPTION] ?? null, 'the token must be persisted');
        $this->assertSame($first, PreloadToken::get(), 'a second call must return the same token');
    }

    public function test_two_sites_get_different_tokens(): void
    {
        $first = PreloadToken::get();
        $this->optionStore = [];
        $this->assertNotSame($first, PreloadToken::get());
    }

    public function test_a_malformed_stored_token_is_replaced(): void
    {
        $this->optionStore[PreloadToken::OPTION] = 'not-a-token';

        $token = PreloadToken::get();

        $this->assertMatchesRegularExpression('/^[a-f0-9]{64}$/', $token);
        $this->assertSame($token, $this->optionStore[PreloadToken::OPTION]);
    }

    /**
     * A token that cannot be stored means no token, never an exception: the
     * drop-in is still rendered, and no request is treated as a preload.
     */
    public function test_a_token_that_cannot_be_stored_is_no_token(): void
    {
        Functions\when('update_option')->alias(static function (): bool {
            throw new \RuntimeException('options table unavailable');
        });

        $this->assertSame('', PreloadToken::get());
        $this->assertSame('', (new CacheConfig(['enabled' => true]))->toDropinArray()['preload_token'] ?? null);
    }

    public function test_the_dropin_config_carries_the_token(): void
    {
        $token = PreloadToken::get();
        $this->assertSame($token, (new CacheConfig(['enabled' => true]))->toDropinArray()['preload_token'] ?? null);
    }

    public function test_the_preloader_sends_the_token(): void
    {
        $token = PreloadToken::get();

        (new Preload(false))->warmOne('https://example.com/page/', 'desktop');

        $this->assertCount(1, $this->fetchArgs);
        $this->assertSame($token, $this->fetchArgs[0]['headers'][Preload::PRELOAD_HEADER] ?? null);
    }

    public function test_the_rucss_compute_fetch_sends_the_token(): void
    {
        $token = PreloadToken::get();
        $this->optionStore['wpmgr_perf_config'] = ['css_rucss' => true];

        $res = (new RucssComputeCommand())->execute([], ['urls' => ['https://example.com/page/']]);

        $this->assertTrue($res['ok']);
        $this->assertNotSame([], $this->fetchArgs);
        foreach ($this->fetchArgs as $args) {
            $this->assertSame($token, $args['headers'][Preload::PRELOAD_HEADER] ?? null);
        }
    }
}
