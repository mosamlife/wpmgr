<?php
/**
 * CacheKey builder tests: anonymous / logged-in+role / mobile / query variants,
 * marketing-param stripping (utm_* don't fragment, lang/currency do), and path
 * resolution. Pure string logic — no WP bootstrap needed.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Cache\CacheKey;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Cache\CacheKey
 */
final class CacheKeyTest extends TestCase
{
    private CacheKey $key;

    protected function set_up(): void
    {
        parent::set_up();
        $this->key = new CacheKey();
    }

    public function test_anonymous_request_is_plain_index(): void
    {
        $name = $this->key->build([], [], 'Mozilla/5.0 (desktop)', false, false);
        $this->assertSame('index.html.gz', $name);
    }

    public function test_logged_in_without_logged_in_caching_returns_null(): void
    {
        $cookies = ['wordpress_logged_in_abc' => 'token'];
        $name = $this->key->build($cookies, [], 'desktop', false, false);
        $this->assertNull($name, 'logged-in request must not produce a disk key when logged-in caching is off');
    }

    public function test_logged_in_with_role_segment(): void
    {
        $cookies = [
            'wordpress_logged_in_abc'  => 'token',
            'wpmgr_logged_in_roles'    => 'editor',
        ];
        $name = $this->key->build($cookies, [], 'desktop', true, false);
        $this->assertSame('index-logged-in-editor.html.gz', $name);
    }

    public function test_role_segment_is_sanitised(): void
    {
        // A hostile role cookie cannot inject path separators.
        $cookies = [
            'wordpress_logged_in_abc' => 'token',
            'wpmgr_logged_in_roles'   => '../../etc/passwd',
        ];
        $name = $this->key->build($cookies, [], 'desktop', true, false);
        $this->assertStringNotContainsString('/', (string) $name);
        $this->assertStringNotContainsString('..', (string) $name);
    }

    public function test_mobile_segment_only_when_mobile_caching_on_and_mobile_ua(): void
    {
        $mobileUa = 'Mozilla/5.0 (iPhone; CPU iPhone OS 16_0 like Mac OS X)';

        // Mobile UA but mobile caching off → no -mobile.
        $this->assertSame('index.html.gz', $this->key->build([], [], $mobileUa, false, false));

        // Mobile UA + mobile caching on → -mobile.
        $this->assertSame('index-mobile.html.gz', $this->key->build([], [], $mobileUa, false, true));

        // Desktop UA + mobile caching on → no -mobile.
        $this->assertSame('index.html.gz', $this->key->build([], [], 'Mozilla/5.0 (desktop)', false, true));
    }

    public function test_marketing_params_do_not_fragment_the_cache(): void
    {
        $clean   = $this->key->build([], [], 'desktop', false, false);
        $tracked = $this->key->build(
            [],
            ['utm_source' => 'newsletter', 'utm_campaign' => 'spring', 'gclid' => 'xyz', 'fbclid' => 'abc'],
            'desktop',
            false,
            false
        );
        $this->assertSame($clean, $tracked, 'utm_*/gclid/fbclid must not change the cache key');
        $this->assertSame('index.html.gz', $tracked);
    }

    public function test_cache_varying_params_do_fragment_the_cache(): void
    {
        $en = $this->key->build([], ['lang' => 'en'], 'desktop', false, false);
        $fr = $this->key->build([], ['lang' => 'fr'], 'desktop', false, false);

        $this->assertNotSame($en, $fr, 'lang must vary the cache key');
        $this->assertStringStartsWith('index-', (string) $en);
        $this->assertStringEndsWith('.html.gz', (string) $en);
    }

    public function test_query_hash_is_order_independent(): void
    {
        $a = $this->key->queryHash(['currency' => 'usd', 'lang' => 'en']);
        $b = $this->key->queryHash(['lang' => 'en', 'currency' => 'usd']);
        $this->assertSame($a, $b, 'query hash must be independent of param arrival order');
        $this->assertNotSame('', $a);
    }

    public function test_query_hash_empty_when_only_marketing_params(): void
    {
        $this->assertSame('', $this->key->queryHash(['utm_source' => 'x', 'gclid' => 'y']));
    }

    public function test_include_cookies_segment(): void
    {
        $name = $this->key->build(
            [],
            [],
            'desktop',
            false,
            false,
            ['geo_country'],
            []
        );
        $this->assertSame('index.html.gz', $name, 'cookie absent → no segment');

        $name = $this->key->build(
            ['geo_country' => 'de'],
            [],
            'desktop',
            false,
            false,
            ['geo_country'],
            []
        );
        $this->assertSame('index-de.html.gz', $name);
    }

    public function test_too_many_query_keys_make_request_non_cacheable(): void
    {
        // 13 distinct cache-varying params (> the cap of 12) => non-cacheable.
        $query = [];
        for ($i = 0; $i <= CacheKey::MAX_QUERY_KEYS; $i++) {
            $query['k' . $i] = (string) $i;
        }
        $this->assertGreaterThan(CacheKey::MAX_QUERY_KEYS, count($query));

        $name = $this->key->build([], $query, 'desktop', false, false);
        $this->assertNull($name, 'over the query-key cap the request must not produce a disk key');

        // queryHash in isolation also refuses to fan out over the cap.
        $this->assertSame('', $this->key->queryHash($query));
    }

    public function test_exactly_cap_query_keys_is_still_cacheable(): void
    {
        // Exactly the cap (12) distinct params => still cacheable.
        $query = [];
        for ($i = 0; $i < CacheKey::MAX_QUERY_KEYS; $i++) {
            $query['k' . $i] = (string) $i;
        }
        $this->assertCount(CacheKey::MAX_QUERY_KEYS, $query);

        $name = $this->key->build([], $query, 'desktop', false, false);
        $this->assertIsString($name);
        $this->assertStringStartsWith('index-', (string) $name);
        $this->assertStringEndsWith('.html.gz', (string) $name);
        $this->assertNotSame('', $this->key->queryHash($query));
    }

    public function test_marketing_params_do_not_count_toward_the_cap(): void
    {
        // Many recognised tracking params + a couple of real ones: tracking ones
        // are dropped first, so the request stays cacheable (only 2 kept).
        $marketing = [
            'utm_source', 'utm_medium', 'utm_campaign', 'utm_term', 'utm_content',
            'utm_id', 'utm_expid', 'gclid', 'gclsrc', 'gad_source', 'gbraid',
            'wbraid', 'fbclid', 'msclkid', 'ttclid', 'mc_cid', 'mc_eid',
        ];
        $query = ['lang' => 'en', 'currency' => 'usd'];
        foreach ($marketing as $i => $name) {
            $query[$name] = (string) $i;
        }
        $this->assertGreaterThan(CacheKey::MAX_QUERY_KEYS, count($query));

        $name = $this->key->build([], $query, 'desktop', false, false);
        $this->assertIsString($name, 'marketing params must not push a request over the cap');
    }

    public function test_path_resolution_for_root_and_subpath(): void
    {
        $root = $this->key->path('/var/cache/wpmgr', 'example.com', '/', 'index.html.gz');
        $this->assertSame('/var/cache/wpmgr/example.com/index.html.gz', $root);

        $sub = $this->key->path('/var/cache/wpmgr', 'Example.COM', '/Blog/Post/?x=1', 'index.html.gz');
        $this->assertSame('/var/cache/wpmgr/example.com/blog/post/index.html.gz', $sub);
    }

    public function test_path_strips_traversal(): void
    {
        $p = $this->key->path('/var/cache/wpmgr', 'evil.com/../..', '/a/../../etc', 'index.html.gz');
        $this->assertStringNotContainsString('..', $p);
    }

    /**
     * Normalisation resolves segments, so no arrangement of dot segments can
     * name anything above the bucket the path belongs to.
     *
     * Each case pins the exact result, not merely the absence of a substring:
     * a containment assertion that only checks for '../' is satisfied by code
     * that has already collapsed two segments into one.
     */
    public function test_normalize_path_resolves_dot_segments(): void
    {
        $cases = [
            '/.//.../x'        => '/x',
            '/.../.../victim'  => '/victim',
            '/.../...//victim' => '/victim',
            // A dot run is not a name, and the last one must not survive to be
            // re-read as a parent reference after the leading separator is put
            // back on.
            '/....'            => '',
            '/..../x'          => '/x',
            // Neighbouring segments must never be fused into one another.
            '/a/....//b'       => '/a/b',
            '/aa/.//.../b'     => '/aa/b',
            // Parent references resolve, and stop at the root.
            '/a/../b'          => '/b',
            '/a/b/../..'       => '',
            '/a/../../../../b' => '/b',
            '/..'              => '',
            '/../..'           => '',
            // Ordinary paths are untouched.
            '/'                => '',
            '/about/'          => '/about',
            '/blog/2026/post/' => '/blog/2026/post',
            '/.hidden/file'    => '/.hidden/file',
            '/a..b/c'          => '/a..b/c',
        ];
        foreach ($cases as $input => $want) {
            $got = CacheKey::normalizePath((string) $input);
            $this->assertSame($want, $got, "normalizePath('$input')");
            $this->assertSame(
                0,
                preg_match('#(^|/)\.+(/|$)#', $got),
                "dot segment survived normalizePath('$input')"
            );
        }
    }

    /**
     * Containment stated as an invariant rather than a case list: joining any
     * normalised path under a bucket directory must resolve inside it.
     */
    public function test_normalized_path_never_escapes_its_bucket(): void
    {
        $inputs = [
            '/....', '/.....', '/..%2f..%2fx', '/%2e%2e/%2e%2e/x', '/%252e%252e%252fx',
            '/..%2fx', '/....//x', '\\..\\..\\x', '/a\\..\\..\\x', "/a\0/../x",
            '/a/../../../../..', '..', '.', '', '/', '//', '///',
            '/' . str_repeat('../', 500) . 'x',
            '/' . str_repeat('.', 4096),
        ];
        foreach ($inputs as $input) {
            $got = CacheKey::normalizePath($input);
            $joined = '/cache/wpmgr/example.com' . $got;
            $this->assertStringStartsWith(
                '/cache/wpmgr/example.com',
                $joined,
                'join must stay under the bucket for ' . var_export($input, true)
            );
            $this->assertSame(
                0,
                preg_match('#(^|/)\.+(/|$)#', $got),
                'dot segment survived for ' . var_export($input, true) . ' -> ' . var_export($got, true)
            );
        }
    }
}
