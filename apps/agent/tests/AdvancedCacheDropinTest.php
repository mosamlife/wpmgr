<?php
/**
 * Advanced-cache drop-in fast-path tests.
 *
 * Proves the pre-WP drop-in honors configured bypass URLs before serving an
 * already-warmed cache file. The drop-in is rendered with a real config and
 * included against a temporary wp-content tree so it exercises the actual file
 * without needing a full WordPress bootstrap.
 *
 * Each test that includes the drop-in runs in a separate process because the
 * drop-in defines constants and reads superglobals that cannot be safely reset
 * in a single PHPUnit process.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Cache\CacheConfig;
use WPMgr\Agent\Cache\DropinInstaller;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Cache\DropinInstaller
 * @runTestsInSeparateProcesses
 * @preserveGlobalState disabled
 */
final class AdvancedCacheDropinTest extends TestCase
{
    /** Emitted by the out-of-process harness only when the drop-in returns. */
    private const MISS = '__WPMGR_DROPIN_RETURNED__';

    /** @var string */
    private string $tempDir;

    /**
     * Create an isolated temporary wp-content cache tree for each test.
     */
    protected function set_up(): void
    {
        parent::set_up();
        $this->tempDir = sys_get_temp_dir() . '/wpmgr-dropin-test-' . uniqid('', true);
        @mkdir($this->tempDir, 0o777, true);
        @mkdir($this->tempDir . '/cache/wpmgr', 0o777, true);
    }

    /**
     * Remove the temporary tree created in set_up().
     */
    protected function tear_down(): void
    {
        $this->rmdirRecursive($this->tempDir);
        parent::tear_down();
    }

    /**
     * Recursively remove a directory created by the test.
     *
     * @param string $dir Directory to remove.
     */
    private function rmdirRecursive(string $dir): void
    {
        if (!is_dir($dir)) {
            return;
        }
        $entries = new \RecursiveIteratorIterator(
            new \RecursiveDirectoryIterator($dir, \RecursiveDirectoryIterator::SKIP_DOTS),
            \RecursiveIteratorIterator::CHILD_FIRST
        );
        foreach ($entries as $entry) {
            if ($entry->isDir()) {
                @rmdir($entry->getRealPath());
            } else {
                @unlink($entry->getRealPath());
            }
        }
        @rmdir($dir);
    }

    /**
     * Render the drop-in into a temp file with the supplied config and include it
     * in a controlled environment. Returns the value produced by the drop-in
     * (false on bypass/miss, exits on hit).
     *
     * @param array<string,mixed> $config Drop-in config array.
     * @param array<string,mixed> $server $_SERVER overrides.
     * @return mixed
     */
    private function runDropin(array $config, array $server = [])
    {
        $template = dirname(__DIR__) . '/assets/wpmgr-advanced-cache.php';
        $installer = new DropinInstaller($this->tempDir, $template);
        $rendered  = $installer->render($config);
        $this->assertNotEmpty($rendered);

        $renderedPath = $this->tempDir . '/advanced-cache.php';
        file_put_contents($renderedPath, $rendered);

        // Plain-PHP guards the drop-in expects.
        if (!defined('ABSPATH')) {
            define('ABSPATH', $this->tempDir . '/');
        }
        if (!defined('WP_CACHE')) {
            define('WP_CACHE', true);
        }
        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', $this->tempDir);
        }

        $_SERVER = array_merge([
            'REQUEST_METHOD' => 'GET',
            'REQUEST_URI'    => '/',
            'HTTP_HOST'      => 'example.com',
            'HTTPS'          => 'on',
        ], $server);
        $_GET    = [];
        $_COOKIE  = [];
        $_POST    = [];

        return include $renderedPath;
    }

    /**
     * Warm a cache file for the given host/path and return its absolute path.
     *
     * @param string $host  Canonical host.
     * @param string $path  URL path (no trailing slash for root).
     * @param string $name  Cache file base name.
     * @param string $body  Marker text placed in the page body, so a test can
     *                      tell WHICH bucket answered rather than only that
     *                      something did.
     * @return string Absolute path to the warmed file.
     */
    private function warmCacheFile(string $host, string $path, string $name = 'index', string $body = 'cached'): string
    {
        $dir = $this->tempDir . '/cache/wpmgr/' . $host . ($path === '' ? '' : $path);
        if (!is_dir($dir)) {
            @mkdir($dir, 0o777, true);
        }
        $file = $dir . '/' . $name . '.html.gz';
        file_put_contents($file, gzencode('<html><body>' . $body . '</body></html>'));
        return $file;
    }

    /**
     * Create a host bucket directory without warming a page into it.
     *
     * Path resolution on POSIX walks every component, so a bucket that does not
     * exist on disk makes a lookup fail before normalisation is ever consulted.
     * A containment test that omits this passes against uncontained code.
     *
     * @param string $host Canonical host.
     * @return string Absolute path to the bucket directory.
     */
    private function makeBucketDir(string $host): string
    {
        $dir = $this->tempDir . '/cache/wpmgr/' . $host;
        @mkdir($dir, 0o777, true);
        return $dir;
    }

    /**
     * Run the drop-in in a CHILD PHP PROCESS and return everything it emitted.
     *
     * A cache HIT ends in exit(), which cannot be observed from inside the
     * PHPUnit process. Running it out-of-process lets a test assert on the
     * bytes actually served — which bucket answered, not merely that the
     * drop-in returned something. A miss appends the MISS marker below.
     *
     * @param array<string,mixed> $config Drop-in config array.
     * @param array<string,mixed> $server $_SERVER overrides.
     * @return string Combined stdout/stderr of the child process.
     */
    private function serveDropin(array $config, array $server = []): string
    {
        $template  = dirname(__DIR__) . '/assets/wpmgr-advanced-cache.php';
        $installer = new DropinInstaller($this->tempDir, $template);
        $rendered  = $installer->render($config);
        $this->assertNotEmpty($rendered);

        $renderedPath = $this->tempDir . '/advanced-cache.php';
        file_put_contents($renderedPath, $rendered);

        $server = array_merge([
            'REQUEST_METHOD' => 'GET',
            'REQUEST_URI'    => '/',
            'HTTP_HOST'      => 'example.com',
            'HTTPS'          => 'on',
        ], $server);

        $harnessPath = $this->tempDir . '/serve-harness.php';
        $harness     = "<?php\n"
            . 'define(\'ABSPATH\', ' . var_export($this->tempDir . '/', true) . ");\n"
            . "define('WP_CACHE', true);\n"
            . 'define(\'WP_CONTENT_DIR\', ' . var_export($this->tempDir, true) . ");\n"
            . '$_SERVER = ' . var_export($server, true) . ";\n"
            . "\$_GET = [];\n\$_POST = [];\n\$_COOKIE = [];\n"
            . 'include ' . var_export($renderedPath, true) . ";\n"
            . 'echo ' . var_export("\n" . self::MISS, true) . ";\n";
        file_put_contents($harnessPath, $harness);

        $cmd = escapeshellarg(PHP_BINARY) . ' -d error_reporting=E_ALL ' . escapeshellarg($harnessPath) . ' 2>&1';
        $out = shell_exec($cmd);
        $this->assertIsString($out, 'drop-in child process produced no output at all');

        // The drop-in serves the stored .gz bytes verbatim. Decode them so the
        // marker in the page body is greppable; a miss is plain text and is
        // returned unchanged.
        if (strncmp($out, "\x1f\x8b", 2) === 0) {
            $decoded = @gzdecode($out);
            if (is_string($decoded)) {
                return $decoded;
            }
        }

        return $out;
    }

    /**
     * A request whose URI contains a configured bypass URL must return false
     * BEFORE serving an existing cache file.
     */
    public function test_bypass_url_prevents_serving_existing_cache_file(): void
    {
        $this->warmCacheFile('example.com', '');

        $config = (new CacheConfig(['bypass_urls' => ['/cart']]))->toDropinArray();
        $result = $this->runDropin($config, ['REQUEST_URI' => '/cart/']);

        $this->assertFalse($result, 'bypass URL must return false before serving a warmed cache file');
    }

    /**
     * Case-insensitive substring containment: the bypass rule must match even
     * when the casing differs.
     */
    public function test_bypass_url_match_is_case_insensitive(): void
    {
        $this->warmCacheFile('example.com', '/checkout');

        $config = (new CacheConfig(['bypass_urls' => ['/CHECKOUT']]))->toDropinArray();
        $result = $this->runDropin($config, ['REQUEST_URI' => '/checkout/']);

        $this->assertFalse($result);
    }

    /**
     * A URI that resolves above the requesting host's own bucket must never
     * reach a neighbouring bucket's cached page.
     *
     * The requesting host's bucket is created FIRST and deliberately: on a
     * POSIX filesystem every component of a path must exist before the parent
     * reference in it can be followed, so without example.com/ on disk the
     * lookup fails for the wrong reason and the assertion below holds against
     * code that does not contain the path at all. With it present, unsafe
     * normalisation reaches the neighbouring bucket, serves it and exit()s the
     * process — which is what makes this test capable of going red.
     */
    public function test_traversal_generator_cannot_read_across_host_buckets(): void
    {
        // The requesting host's own bucket must exist for the lookup to be
        // decided by normalisation rather than by a missing directory.
        $this->makeBucketDir('example.com');

        // A different host's bucket, adjacent to example.com's, holding a page.
        $victim = $this->warmCacheFile('victim', '', 'index', 'VICTIM-BUCKET-BODY');
        $this->assertFileExists($victim);

        $out = $this->serveDropin(
            (new CacheConfig([]))->toDropinArray(),
            ['REQUEST_URI' => '/.../.../victim']
        );

        $this->assertStringNotContainsString(
            'VICTIM-BUCKET-BODY',
            $out,
            'traversal-generator URI reached a neighbouring host bucket'
        );
        $this->assertStringContainsString(self::MISS, $out, 'traversal-generator URI must miss');
    }

    /**
     * The same containment property against a shorter generator that survived
     * the first fix: normalisation must not be able to produce a parent
     * reference at all, including one manufactured after the last strip.
     *
     * The target here is the file every host bucket would collapse onto — one
     * shared page at the cache root, served to every site on the install.
     */
    public function test_dot_run_cannot_reach_the_shared_cache_root(): void
    {
        $this->makeBucketDir('example.com');

        // A page sitting at the cache root itself, one level above every bucket.
        $shared = $this->tempDir . '/cache/wpmgr/index.html.gz';
        file_put_contents($shared, gzencode('<html><body>SHARED-ROOT-BODY</body></html>'));

        $out = $this->serveDropin(
            (new CacheConfig([]))->toDropinArray(),
            ['REQUEST_URI' => '/....']
        );

        $this->assertStringNotContainsString(
            'SHARED-ROOT-BODY',
            $out,
            'a dot-run URI collapsed the host bucket onto the shared cache root'
        );
        $this->assertStringContainsString(self::MISS, $out, 'a dot-run URI must stay inside the host bucket');
    }

    /**
     * Ordinary traffic must still hit. A guard that also blocks correct work
     * gets switched off, and then it guards nothing.
     *
     * @dataProvider provide_ordinary_paths
     *
     * @param string $uri  Request URI as the browser sends it.
     * @param string $path Bucket-relative directory the page is stored under.
     */
    public function test_ordinary_paths_still_resolve_to_their_own_bucket(string $uri, string $path): void
    {
        $file = $this->warmCacheFile('example.com', $path, 'index', 'OWN-BUCKET-BODY');
        $this->assertFileExists($file, 'fixture must exist or the assertion below proves nothing');

        $out = $this->serveDropin((new CacheConfig([]))->toDropinArray(), ['REQUEST_URI' => $uri]);

        $this->assertStringContainsString(
            'OWN-BUCKET-BODY',
            $out,
            "ordinary URI $uri must still be served from its own bucket"
        );
    }

    /**
     * @return array<string,array{0:string,1:string}>
     */
    public static function provide_ordinary_paths(): array
    {
        return [
            'root'                 => ['/', ''],
            'trailing slash'       => ['/about/', '/about'],
            'no trailing slash'    => ['/about', '/about'],
            'nested'               => ['/blog/2026/09/hello-world/', '/blog/2026/09/hello-world'],
            'multisite subdir'     => ['/site-two/blog/post-name/', '/site-two/blog/post-name'],
            'long query string'    => ['/about/?' . str_repeat('k=v&', 200) . 'utm_source=x', '/about'],
            'percent-encoded utf8' => ['/caf%C3%A9/', '/café'],
            'literal utf8'         => ['/日本語/page/2/', '/日本語/page/2'],
            'mixed case'           => ['/About/Us/', '/about/us'],
        ];
    }

    /**
     * A symlinked cache root must work: both sides resolve THROUGH the link, so
     * the containment check must compare resolved paths, not raw ones.
     */
    public function test_symlinked_cache_root_still_serves(): void
    {
        // Move the real tree aside and put a symlink where the drop-in looks.
        $real = $this->tempDir . '/real-cache-store';
        @mkdir($real, 0o777, true);
        $link = $this->tempDir . '/cache/wpmgr';
        @rmdir($link);
        symlink($real, $link);

        @mkdir($real . '/example.com/about', 0o777, true);
        file_put_contents($real . '/example.com/about/index.html.gz', gzencode('<html><body>VIA-LINK-BODY</body></html>'));

        try {
            $out = $this->serveDropin(
                (new CacheConfig([]))->toDropinArray(),
                ['REQUEST_URI' => '/about/']
            );
            $this->assertStringContainsString(
                'VIA-LINK-BODY',
                $out,
                'a symlinked cache root must resolve through the link, not be rejected by it'
            );
        } finally {
            @unlink($link);
        }
    }

    /**
     * A host the cache does not key on must not be served from a shared
     * placeholder bucket.
     *
     * The old behaviour mapped every rejected host to one name, which made that
     * bucket readable by all of them at once: a page cached under it for one
     * rejected request would answer a different rejected request. The bucket is
     * warmed here precisely so that a fallback would be visible.
     *
     * @dataProvider provide_uncacheable_hosts
     *
     * @param string $host Host header value the cache must decline.
     */
    public function test_uncacheable_host_is_never_served_from_a_shared_bucket(string $host): void
    {
        $this->warmCacheFile('unknown-host', '', 'index', 'SHARED-PLACEHOLDER-BODY');
        $this->warmCacheFile('example.com', '', 'index', 'OWN-BUCKET-BODY');

        $out = $this->serveDropin(
            (new CacheConfig([]))->toDropinArray(),
            ['HTTP_HOST' => $host, 'REQUEST_URI' => '/']
        );

        $this->assertStringNotContainsString(
            'SHARED-PLACEHOLDER-BODY',
            $out,
            'an uncacheable host was answered from the shared placeholder bucket'
        );
        $this->assertStringNotContainsString('OWN-BUCKET-BODY', $out);
        $this->assertStringContainsString(self::MISS, $out, 'an uncacheable host must bypass and boot WordPress');
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function provide_uncacheable_hosts(): array
    {
        return [
            'empty'            => [''],
            'empty label'      => ['victim..com'],
            'leading dots'     => ['..victim.com'],
            'bare dot'         => ['.'],
            'bare dots'        => ['..'],
            'leading dot'      => ['.victim.com'],
            'trailing dot'     => ['victim.com.'],
            'separator only'   => ['-'],
            'path in host'     => ['evil.com/../..'],
            'space in host'    => ['exa mple.com'],
            'underscore'       => ['exam_ple.com'],
            'bracketed ipv6'   => ['[2001:db8::1]'],
            'port only'        => [':8443'],
            'oversized port'   => ['example.com:99999'],
        ];
    }

    /**
     * A host carrying a port must key into its own bucket and still serve.
     *
     * The fixture directory is named by the STORE side, so this also pins that
     * the two sides agree: a site on a non-default port previously stored under
     * one bucket name and looked up under another and could never hit.
     */
    public function test_host_with_port_still_serves(): void
    {
        $bucket = \WPMgr\Agent\Cache\CacheKey::normalizeHost('example.com:8443');
        $this->assertNotSame('', $bucket, 'a host with a port must remain cacheable');

        $this->warmCacheFile($bucket, '/about', 'index', 'PORT-BUCKET-BODY');

        $out = $this->serveDropin(
            (new CacheConfig([]))->toDropinArray(),
            ['HTTP_HOST' => 'example.com:8443', 'REQUEST_URI' => '/about/']
        );

        $this->assertStringContainsString('PORT-BUCKET-BODY', $out, 'a host with a port must still hit its bucket');
    }

    /**
     * An unusual but legitimate host must keep caching. A rule that declines
     * anything it does not recognise stops being a cache.
     *
     * @dataProvider provide_unusual_but_legitimate_hosts
     *
     * @param string $host Host header value that must still be cached.
     */
    public function test_unusual_but_legitimate_hosts_still_serve(string $host): void
    {
        $bucket = \WPMgr\Agent\Cache\CacheKey::normalizeHost($host);
        $this->assertNotSame('', $bucket, "host $host must remain cacheable");

        $this->warmCacheFile($bucket, '/about', 'index', 'LEGIT-HOST-BODY');

        $out = $this->serveDropin(
            (new CacheConfig([]))->toDropinArray(),
            ['HTTP_HOST' => $host, 'REQUEST_URI' => '/about/']
        );

        $this->assertStringContainsString('LEGIT-HOST-BODY', $out, "host $host must still hit its bucket");
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function provide_unusual_but_legitimate_hosts(): array
    {
        return [
            'punycode idn'      => ['xn--bcher-kva.example'],
            'deep subdomain'    => ['a.b.c.d.example.co.uk'],
            'hyphenated'        => ['my-site.example.com'],
            'single label'      => ['localhost'],
            'ipv4 literal'      => ['127.0.0.1'],
            'uppercase'         => ['EXAMPLE.COM'],
            'uppercase w/ port' => ['EXAMPLE.COM:8443'],
            'explicit port 80'  => ['example.com:80'],
            'digits only label' => ['123.example.com'],
        ];
    }

    /**
     * Regression: the host charset regex permits dots, so HTTP_HOST '..' keyed
     * into the cache root's PARENT directory. A host containing '..' must be
     * treated as unknown-host (cache bypass), matching CacheKey::sanitizeHost
     * on the write side.
     */
    public function test_dotdot_host_cannot_escape_cache_root(): void
    {
        // File OUTSIDE the cache root: <tmp>/cache/victim/index.html.gz —
        // exactly where host '..' + path '/victim' used to land.
        @mkdir($this->tempDir . '/cache/victim', 0o777, true);
        file_put_contents(
            $this->tempDir . '/cache/victim/index.html.gz',
            gzencode('<html><body>outside</body></html>')
        );

        $result = $this->runDropin(
            (new CacheConfig([]))->toDropinArray(),
            ['HTTP_HOST' => '..', 'REQUEST_URI' => '/victim']
        );

        $this->assertFalse($result, "host '..' must bypass the cache, never key outside the root");
    }

    /**
     * Containment backstop: even when every lexical sanitiser passes, a file
     * that physically resolves OUTSIDE the cache root (here: through a planted
     * symlink) must be handed to WordPress as a miss, never served.
     */
    public function test_symlinked_bucket_is_not_served(): void
    {
        $outside = $this->tempDir . '/outside-root';
        @mkdir($outside, 0o777, true);
        file_put_contents($outside . '/index.html.gz', gzencode('<html><body>leaked</body></html>'));
        symlink($outside, $this->tempDir . '/cache/wpmgr/example.com');

        try {
            $result = $this->runDropin(
                (new CacheConfig([]))->toDropinArray(),
                ['REQUEST_URI' => '/']
            );

            $this->assertFalse($result, 'a bucket resolving outside the cache root must miss, never serve');
        } finally {
            // rmdirRecursive() resolves entries via getRealPath(), which
            // returns false for a symlink whose target is already gone —
            // remove the link here so tear_down never sees it.
            @unlink($this->tempDir . '/cache/wpmgr/example.com');
        }
    }

}
