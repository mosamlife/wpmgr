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

    /** @var list<array{url:string,headers:array<string,string>}> Every request the HTTP layer double saw, redirects included. */
    private array $requests = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->optionStore = [];
        $this->fetchArgs   = [];
        $this->requests    = [];
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

    // ---------------------------------------------------------------------
    // The token stays on the site: a redirect is never followed.
    // ---------------------------------------------------------------------

    /**
     * Every request that carries the token is made without following
     * redirects.
     */
    public function test_requests_that_carry_the_token_do_not_follow_redirects(): void
    {
        PreloadToken::get();
        $this->optionStore['wpmgr_perf_config'] = ['css_rucss' => true];

        (new Preload(false))->warmOne('https://example.com/page/', 'desktop');
        (new RucssComputeCommand())->execute([], ['urls' => ['https://example.com/page/']]);

        $this->assertGreaterThanOrEqual(2, count($this->fetchArgs), 'both the preloader and the RUCSS pass must have fetched');
        foreach ($this->fetchArgs as $args) {
            $this->assertArrayHasKey(Preload::PRELOAD_HEADER, $args['headers'] ?? []);
            $this->assertSame(0, $args['redirection'] ?? null, 'a request that carries the token must not follow a redirect');
        }
    }

    /**
     * A redirect from a page of the site, off the site or to another page of
     * it, is not followed by the preloader: the destination is never
     * requested, so the token goes nowhere but the page that was asked for,
     * and the redirect is nothing to preload (not an error to retry).
     *
     * @dataProvider provide_redirects
     *
     * @param string $destination Where the page redirects to.
     */
    public function test_the_preloader_does_not_follow_a_redirect(string $destination): void
    {
        PreloadToken::get();
        $this->httpLayerFollowingRedirects(['https://example.com/old/' => $destination]);

        (new Preload(false))->warmOne('https://example.com/old/', 'desktop');

        $this->assertSame(['https://example.com/old/'], array_column($this->requests, 'url'), 'only the page that was asked for may be requested');
    }

    /**
     * The same for the RUCSS compute pass, which counts a page only when it
     * was rendered: a redirect rendered nothing.
     *
     * @dataProvider provide_redirects
     *
     * @param string $destination Where the page redirects to.
     */
    public function test_the_rucss_compute_pass_does_not_follow_a_redirect(string $destination): void
    {
        PreloadToken::get();
        $this->optionStore['wpmgr_perf_config'] = ['css_rucss' => true];
        $this->httpLayerFollowingRedirects(['https://example.com/old/' => $destination]);

        $res = (new RucssComputeCommand())->execute([], ['urls' => ['https://example.com/old/']]);

        $this->assertSame(['https://example.com/old/'], array_column($this->requests, 'url'), 'only the page that was asked for may be requested');
        $this->assertSame(0, $res['queued'], 'a redirect rendered nothing, so nothing was queued');
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function provide_redirects(): array
    {
        return [
            'to another site'       => ['https://elsewhere.example/landing/'],
            'to another site, http' => ['http://elsewhere.example/landing/'],
            'to a page of the site' => ['https://example.com/new/'],
        ];
    }

    /**
     * Positive control for the two tests above: the same HTTP layer does
     * follow the redirect when a request allows it, so a request count of one
     * means something.
     */
    public function test_the_http_layer_double_follows_redirects_when_allowed(): void
    {
        $this->httpLayerFollowingRedirects(['https://example.com/old/' => 'https://elsewhere.example/landing/']);

        wp_remote_get('https://example.com/old/', ['redirection' => 2, 'headers' => ['x-wpmgr-preload' => 'token']]);

        $this->assertSame(['https://example.com/old/', 'https://elsewhere.example/landing/'], array_column($this->requests, 'url'));
        $this->assertSame('token', $this->requests[1]['headers']['x-wpmgr-preload'] ?? null, 'the redirect carries the headers along');
    }

    /**
     * Replace wp_remote_get() with a double that behaves as core's HTTP API
     * does: a response that redirects is followed, with the same headers, up
     * to the request's 'redirection' limit; with a limit of 0 the redirect
     * response itself is returned.
     *
     * @param array<string,string> $redirects Page URL => the URL it redirects to.
     */
    private function httpLayerFollowingRedirects(array $redirects): void
    {
        $this->requests = [];
        Functions\when('wp_remote_retrieve_response_code')->alias(static fn ($response): int => (int) ($response['response']['code'] ?? 0));
        Functions\when('wp_remote_get')->alias(function ($url, $args = []) use ($redirects) {
            $args  = is_array($args) ? $args : [];
            $limit = (int) ($args['redirection'] ?? 5);
            for ($hops = 0; ; $hops++) {
                $this->requests[] = ['url' => (string) $url, 'headers' => $args['headers'] ?? []];
                if (!isset($redirects[$url])) {
                    return ['response' => ['code' => 200]];
                }
                if ($hops >= $limit) {
                    return ['response' => ['code' => 301], 'headers' => ['location' => $redirects[$url]]];
                }
                $url = $redirects[$url];
            }
        });
    }
}
