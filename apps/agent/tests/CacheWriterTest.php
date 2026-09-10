<?php
/**
 * CacheWriter tests on a temp-dir cache fixture: a cacheable anonymous HTML GET
 * is written as a gzip file at the deterministic key path; non-cacheable
 * requests (logged-in w/o logged-in caching, non-200, non-HTML) write nothing;
 * the written bytes are valid gzip of the original body.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Cache\CacheConfig;
use WPMgr\Agent\Cache\CacheWriter;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Cache\CacheWriter
 */
final class CacheWriterTest extends TestCase
{
    private const HTML = '<!DOCTYPE html><html><body>hello world</body></html>';

    private string $root = '';

    protected function set_up(): void
    {
        parent::set_up();
        $this->root = sys_get_temp_dir() . '/wpmgr-writer-' . uniqid('', true) . '/cache/wpmgr';
    }

    protected function tear_down(): void
    {
        $base = dirname(dirname($this->root));
        $this->rrmdir($base);
        parent::tear_down();
    }

    private function rrmdir(string $dir): void
    {
        if (!is_dir($dir)) {
            return;
        }
        foreach (scandir($dir) ?: [] as $e) {
            if ($e === '.' || $e === '..') {
                continue;
            }
            $p = $dir . '/' . $e;
            is_dir($p) ? $this->rrmdir($p) : @unlink($p);
        }
        @rmdir($dir);
    }

    private function writer(array $configOver = []): CacheWriter
    {
        $config = new CacheConfig(array_merge(['enabled' => true], $configOver));
        return new CacheWriter($config, $this->root);
    }

    private function ctx(array $over = []): array
    {
        return array_merge([
            'url'               => '/about/',
            'uri_path'          => '/about/',
            'host'              => 'example.com',
            'method'            => 'GET',
            'user_agent'        => 'Mozilla/5.0 (desktop)',
            'cookies'           => [],
            'query'             => [],
            'is_admin'          => false,
            'is_ajax'           => false,
            'status'            => 200,
            'logged_in'         => false,
            'cache_logged_in'   => false,
            'password_required' => false,
        ], $over);
    }

    public function test_writes_gzip_file_for_cacheable_request(): void
    {
        $written = $this->writer()->maybeWrite(self::HTML, $this->ctx());
        $this->assertTrue($written);

        $path = $this->root . '/example.com/about/index.html.gz';
        $this->assertFileExists($path);

        $raw      = (string) file_get_contents($path);
        $decoded  = (string) gzdecode($raw);

        // Body must begin with the original HTML.
        $this->assertStringStartsWith(self::HTML, $decoded, 'stored bytes must start with the original body');
        // Footprint marker must be present.
        $this->assertStringContainsString(\WPMgr\Agent\Cache\CacheWriter::FOOTPRINT_MARKER, $decoded,
            'footprint marker must be appended to the cached bytes');
        // Non-optimized write must NOT include the "(optimized)" suffix.
        $this->assertStringNotContainsString('(optimized)', $decoded,
            'non-optimized write must not include the optimized suffix');
    }

    public function test_footprint_marker_includes_utc_timestamp(): void
    {
        $this->writer()->maybeWrite(self::HTML, $this->ctx());
        $path    = $this->root . '/example.com/about/index.html.gz';
        $decoded = (string) gzdecode((string) file_get_contents($path));
        // The marker must close with --> after a UTC ISO8601 timestamp.
        $this->assertMatchesRegularExpression(
            '/<!-- Optimized and cached by WPMgr[^>]*\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z -->/',
            $decoded,
            'footprint must include an ISO8601 UTC timestamp'
        );
    }

    public function test_footprint_marker_with_optimized_flag(): void
    {
        $written = $this->writer()->maybeWrite(self::HTML, $this->ctx(), true);
        $this->assertTrue($written);
        $path    = $this->root . '/example.com/about/index.html.gz';
        $decoded = (string) gzdecode((string) file_get_contents($path));
        $this->assertStringContainsString('(optimized)', $decoded,
            'optimized write must include the (optimized) suffix in the footprint');
    }

    public function test_disabled_config_writes_nothing(): void
    {
        $written = $this->writer(['enabled' => false])->maybeWrite(self::HTML, $this->ctx());
        $this->assertFalse($written);
        $this->assertFileDoesNotExist($this->root . '/example.com/about/index.html.gz');
    }

    public function test_logged_in_without_logged_in_caching_writes_nothing(): void
    {
        $ctx = $this->ctx(['logged_in' => true, 'cookies' => ['wordpress_logged_in_x' => '1']]);
        $written = $this->writer()->maybeWrite(self::HTML, $ctx);
        $this->assertFalse($written);
    }

    public function test_non_200_writes_nothing(): void
    {
        $written = $this->writer()->maybeWrite(self::HTML, $this->ctx(['status' => 404]));
        $this->assertFalse($written);
    }

    public function test_non_html_writes_nothing(): void
    {
        $written = $this->writer()->maybeWrite('{"json":true}', $this->ctx());
        $this->assertFalse($written);
    }

    public function test_query_variant_path(): void
    {
        $ctx = $this->ctx(['query' => ['lang' => 'fr'], 'url' => '/about/?lang=fr']);
        $written = $this->writer()->maybeWrite(self::HTML, $ctx);
        $this->assertTrue($written);

        // The file name carries a query hash segment, not a bare index.
        $files = glob($this->root . '/example.com/about/index-*.html.gz');
        $this->assertNotEmpty($files);
    }

    public function test_mobile_variant_path(): void
    {
        $ctx = $this->ctx(['user_agent' => 'Mozilla/5.0 (iPhone; CPU iPhone OS 16_0 like Mac OS X)']);
        $written = $this->writer(['cache_mobile' => true])->maybeWrite(self::HTML, $ctx);
        $this->assertTrue($written);
        $this->assertFileExists($this->root . '/example.com/about/index-mobile.html.gz');
    }

    /**
     * Write containment: the directory a page is about to be written into must
     * physically resolve inside the cache root.
     *
     * The bucket here is a symlink pointing out of the tree, which every
     * lexical check upstream passes — the key string is ordinary and contains
     * nothing to sanitise. Only comparing the RESOLVED directory against the
     * resolved root can refuse it, so this is the case that distinguishes a
     * containment guard from its absence.
     */
    public function test_write_into_a_bucket_resolving_outside_the_root_is_refused(): void
    {
        $outside = dirname(dirname($this->root)) . '/outside-the-cache-root';
        @mkdir($outside, 0o777, true);
        @mkdir($this->root, 0o777, true);
        $this->assertTrue(symlink($outside, $this->root . '/example.com'), 'fixture symlink must be created');

        $written = $this->writer()->maybeWrite(self::HTML, $this->ctx());

        $this->assertFalse($written, 'a write whose directory resolves outside the cache root must be refused');
        $this->assertFileDoesNotExist(
            $outside . '/about/index.html.gz',
            'no cache file may be created outside the cache root'
        );
        $this->assertSame(
            [],
            glob($outside . '/about/*') ?: [],
            'nothing at all may be written outside the cache root'
        );

        @unlink($this->root . '/example.com');
    }

    /**
     * A host the cache does not key on must not be stored under a placeholder
     * bucket. The serve side declines the same hosts, so anything written here
     * could only ever be read back by an unrelated request.
     *
     * @dataProvider provide_uncacheable_hosts
     *
     * @param string $host Host the writer must decline.
     */
    public function test_uncacheable_host_writes_nothing(string $host): void
    {
        $written = $this->writer()->maybeWrite(self::HTML, $this->ctx(['host' => $host]));

        $this->assertFalse($written, "host '$host' must not be cached at all");
        $this->assertSame(
            [],
            glob($this->root . '/*') ?: [],
            'no bucket may be created for an uncacheable host'
        );
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function provide_uncacheable_hosts(): array
    {
        return [
            'empty'          => [''],
            'empty label'    => ['victim..com'],
            'leading dots'   => ['..victim.com'],
            'bare dot'       => ['.'],
            'bare dots'      => ['..'],
            'trailing dot'   => ['victim.com.'],
            'separator only' => ['-'],
            'path in host'   => ['evil.com/../..'],
            'bracketed ipv6' => ['[2001:db8::1]'],
        ];
    }

    /**
     * The bucket a page is written into must be the one the serve side would
     * look in. Both sides run the same host rule, so this pins the agreement
     * rather than a hard-coded spelling.
     *
     * @dataProvider provide_cacheable_hosts
     *
     * @param string $host Host that must be cached.
     */
    public function test_cacheable_host_writes_into_the_bucket_both_sides_agree_on(string $host): void
    {
        $bucket = \WPMgr\Agent\Cache\CacheKey::normalizeHost($host);
        $this->assertNotSame('', $bucket, "host $host must remain cacheable");

        $written = $this->writer()->maybeWrite(self::HTML, $this->ctx(['host' => $host]));

        $this->assertTrue($written, "host $host must still be cached");
        $this->assertFileExists($this->root . '/' . $bucket . '/about/index.html.gz');
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function provide_cacheable_hosts(): array
    {
        return [
            'plain'             => ['example.com'],
            'with port'         => ['example.com:8443'],
            'explicit port 80'  => ['example.com:80'],
            'uppercase'         => ['EXAMPLE.COM'],
            'punycode idn'      => ['xn--bcher-kva.example'],
            'single label'      => ['localhost'],
            'ipv4 literal'      => ['127.0.0.1'],
            'hyphenated'        => ['my-site.example.com'],
        ];
    }

    /**
     * Containment must be decided BEFORE the directory is created.
     *
     * wp_mkdir_p() follows symlinks, so a link in an existing parent segment
     * redirects the creation outside the cache root. A check that runs after
     * the mkdir refuses the file but leaves the directories behind — the write
     * is contained and the filesystem is not. Nothing may appear outside.
     */
    public function test_no_directories_are_created_outside_the_root(): void
    {
        $outside = dirname(dirname($this->root)) . '/outside-the-cache-root';
        @mkdir($outside, 0o777, true);
        @mkdir($this->root, 0o777, true);
        $this->assertTrue(symlink($outside, $this->root . '/example.com'), 'fixture symlink must be created');

        $written = $this->writer()->maybeWrite(self::HTML, $this->ctx());

        $this->assertFalse($written, 'a write redirected outside the cache root must be refused');
        $this->assertDirectoryDoesNotExist(
            $outside . '/about',
            'no directory may be created outside the cache root'
        );
        $this->assertSame(
            [],
            glob($outside . '/*') ?: [],
            'nothing at all may appear outside the cache root'
        );

        @unlink($this->root . '/example.com');
    }

    /**
     * A cache root that is itself reached through a symlink must still be
     * writable: both the root and the target directory resolve through the same
     * link, so a containment check comparing resolved paths agrees with itself.
     * A guard that compared raw paths would refuse every write on such a host.
     */
    public function test_write_through_a_symlinked_cache_root_still_succeeds(): void
    {
        $base = dirname(dirname($this->root));
        $real = $base . '/real-cache-store';
        @mkdir($real, 0o777, true);
        @mkdir(dirname($this->root), 0o777, true);
        @rmdir($this->root);
        $this->assertTrue(symlink($real, $this->root), 'fixture symlink must be created');

        $written = $this->writer()->maybeWrite(self::HTML, $this->ctx());

        $this->assertTrue($written, 'a symlinked cache root must remain writable');
        $this->assertFileExists($real . '/example.com/about/index.html.gz');

        @unlink($this->root);
    }
}
