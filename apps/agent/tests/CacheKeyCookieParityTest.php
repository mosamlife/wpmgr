<?php
/**
 * Write-path / serve-path cookie-value parity for the disk-cache key.
 *
 * The serving drop-in (assets/wpmgr-advanced-cache.php) runs PRE-WordPress and
 * slugifies each include-cookie/role-cookie value with a bare
 * `strtolower()` + `preg_replace('/[^a-z0-9\-_]/', '', ...)` applied directly
 * to the RAW `$_COOKIE` value (wp_unslash()/sanitize_*() are not available
 * that early — see the drop-in's own "3. include-cookie segments" block). The
 * write path (CacheWriter::resolveContext(), post-WordPress) MUST hand
 * CacheKey::build() a byte-identical value, because CacheKey applies the
 * SAME slugifier (CacheKey::sanitizeSegment()) on the write side — so both
 * paths must compute the same on-disk file name for the same request.
 *
 * Regression this locks: resolveContext() once ran every $_COOKIE value
 * through sanitize_text_field() before handing it to CacheKey. That
 * additionally strips percent-encoded octets (e.g. "%2f" is removed
 * entirely -> "usca", not slugified to "us2fca") and HTML tags/markup that
 * the drop-in's slugifier does not touch, silently forking the two key
 * builders for any cookie value containing "%XX" or "<". The practical
 * effect: a write for `geo=us%2fca` landed under a key the serve path never
 * looks up (permanent MISS), and — worse — a different visitor's raw
 * `usca` cookie could resolve to the same bucket as another value once
 * both sides are put through mismatched transforms (cross-bucket mis-serve).
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Cache\CacheConfig;
use WPMgr\Agent\Cache\CacheKey;
use WPMgr\Agent\Cache\CacheWriter;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Cache\CacheWriter
 * @covers \WPMgr\Agent\Cache\CacheKey
 */
final class CacheKeyCookieParityTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        $_SERVER['REQUEST_URI']     = '/';
        $_SERVER['HTTP_HOST']       = 'example.com';
        $_SERVER['REQUEST_METHOD']  = 'GET';
        $_SERVER['HTTP_USER_AGENT'] = 'Mozilla/5.0 (desktop)';
        $_GET = [];
    }

    protected function tear_down(): void
    {
        unset(
            $_SERVER['REQUEST_URI'],
            $_SERVER['HTTP_HOST'],
            $_SERVER['REQUEST_METHOD'],
            $_SERVER['HTTP_USER_AGENT']
        );
        $_COOKIE = [];
        $_GET    = [];
        parent::tear_down();
    }

    /**
     * Exactly what assets/wpmgr-advanced-cache.php does to a raw include-cookie
     * value at serve time: `strtolower((string) $_COOKIE[$name])` then
     * `preg_replace('/[^a-z0-9\-_]/', '', $val)`. No wp_unslash()/sanitize_*():
     * the drop-in runs before WordPress (and wp_magic_quotes()) has loaded.
     *
     * @param string $rawCookieValue The raw $_COOKIE value as PHP parsed it.
     * @return string
     */
    private function servePathSegment(string $rawCookieValue): string
    {
        $value = strtolower($rawCookieValue);
        return (string) preg_replace('/[^a-z0-9\-_]/', '', $value);
    }

    /**
     * The write-path segment for the same raw cookie value: drives the ACTUAL
     * production code — CacheWriter::resolveContext() (private; invoked via
     * Reflection) reading the real $_COOKIE superglobal, then CacheKey::build()
     * — exactly as the live page-cache write path does on every request.
     *
     * @param string $rawCookieValue The raw $_COOKIE value to simulate.
     * @return string The include-cookie segment CacheKey::build() produced.
     */
    private function writePathSegment(string $rawCookieValue): string
    {
        $_COOKIE = ['geo' => $rawCookieValue];

        $config = new CacheConfig(['enabled' => true, 'include_cookies' => ['geo']]);
        $writer = new CacheWriter($config, sys_get_temp_dir() . '/wpmgr-cookie-parity-unused');

        $ref = new \ReflectionMethod(CacheWriter::class, 'resolveContext');
        $ctx = $ref->invoke($writer, '<!DOCTYPE html><html><body>hi</body></html>');

        $fileName = (new CacheKey())->build(
            (array) $ctx['cookies'],
            [],
            'desktop',
            false,
            false,
            ['geo'],
            []
        );
        $this->assertIsString($fileName, 'a plain include-cookie request must be cacheable');

        // File name shape is "index-{segment}.html.gz" — strip the fixed wrapper.
        return (string) preg_replace('/^index-|\.html\.gz$/', '', $fileName);
    }

    /**
     * @dataProvider provideCookieValues
     */
    public function test_write_path_segment_matches_serve_path_segment(string $rawValue, string $why): void
    {
        $serve = $this->servePathSegment($rawValue);
        $write = $this->writePathSegment($rawValue);

        $this->assertSame(
            $serve,
            $write,
            sprintf(
                'write-path and serve-path must derive the byte-identical cache-key segment '
                    . 'for cookie value %s (%s)',
                var_export($rawValue, true),
                $why
            )
        );
    }

    /**
     * @return array<string,array{0:string,1:string}>
     */
    public function provideCookieValues(): array
    {
        return [
            'percent-encoded octet is character-filtered, not stripped as an escape' => [
                'us%2fca',
                'sanitize_text_field() strips "%2f" entirely -> "usca"; the shared slugifier keeps '
                    . 'the surviving digits -> "us2fca"',
            ],
            'HTML tag is character-filtered, not tag-stripped' => [
                '<script>x',
                'sanitize_text_field() strip_tags()\'s the unterminated "<script>" down to "x"; the '
                    . 'shared slugifier only removes disallowed characters, keeping "scriptx"',
            ],
            'plain alphanumeric value is unaffected by either transform' => [
                'en',
                'control case: a correct and a buggy write path agree here, proving this is not a tautology',
            ],
        ];
    }
}
