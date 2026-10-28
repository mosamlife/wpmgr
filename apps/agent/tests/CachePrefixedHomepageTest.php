<?php
/**
 * A homepage served under a path prefix (a language such as /en/, a subsite)
 * is a page of its own: it is stored under its own key, answered from that key,
 * and covered by Bypass URLs written for it, whichever way the visitor spells
 * the path.
 *
 * The drop-in half of this contract (serving, and the Bypass URL check that
 * runs before serving) is pinned in AdvancedCacheDropinTest. This file covers
 * the write side and the configuration side:
 *
 *  - the writer keys on the request path as it arrived, not on whatever
 *    $_SERVER['REQUEST_URI'] holds by the time the buffer is flushed;
 *  - the capture buffer opens before the init callbacks of other plugins, so
 *    it is the OUTER buffer and stores the bytes the visitor receives, not the
 *    bytes a later output-buffer filter (a translator, a minifier) rewrites;
 *  - Bypass URL matching tolerates the trailing slash, in the PHP path and in
 *    the pre-WordPress drop-in alike;
 *  - applying new Bypass URLs removes the pages already cached for them, so a
 *    web-server fast path that never reads the list cannot keep serving them.
 *
 * Each test runs in its own process: the buffer tests need WP_CONTENT_DIR
 * defined, and a constant cannot be undefined between tests.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Cache\CacheConfig;
use WPMgr\Agent\Cache\CacheManager;
use WPMgr\Agent\Cache\CacheWriter;
use WPMgr\Agent\Cache\Cacheability;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Cache\CacheManager
 * @covers \WPMgr\Agent\Cache\CacheWriter
 * @covers \WPMgr\Agent\Cache\Cacheability::matchesBypassUrl
 * @runTestsInSeparateProcesses
 * @preserveGlobalState disabled
 */
final class CachePrefixedHomepageTest extends TestCase
{
    private const HTML_IT = '<!DOCTYPE html><html><body>Benvenuti nella home</body></html>';

    private string $base = '';

    private string $root = '';

    /** @var array<string,mixed> */
    private array $optionStore = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->base = sys_get_temp_dir() . '/wpmgr-prefixed-home-' . uniqid('', true);
        $this->root = $this->base . '/cache/wpmgr';
        @mkdir($this->root, 0o777, true);

        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', $this->base);
        }

        $this->optionStore = [];
        Functions\when('get_option')->alias(fn ($k, $d = false) => $this->optionStore[$k] ?? $d);
        Functions\when('update_option')->alias(function ($k, $v) {
            $this->optionStore[$k] = $v;
            return true;
        });
        Functions\when('delete_option')->justReturn(true);
        Functions\when('home_url')->justReturn('https://example.com/');
        Functions\when('wp_next_scheduled')->justReturn(false);
        Functions\when('wp_schedule_single_event')->justReturn(true);
        Functions\when('wp_schedule_event')->justReturn(true);
        Functions\when('wp_clear_scheduled_hook')->justReturn(true);
        // The page reached the template stage, as every page rendered by the
        // front controller does.
        Functions\when('did_action')->alias(static fn (string $hook): int => $hook === 'template_redirect' ? 1 : 0);

        $_SERVER['REQUEST_METHOD'] = 'GET';
        $_SERVER['HTTP_HOST']      = 'example.com';
        $_SERVER['REQUEST_URI']    = '/';
        $_COOKIE                   = [];
        $_GET                      = [];
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->base);
        Monkey\tearDown();
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
            is_dir($p) && !is_link($p) ? $this->rrmdir($p) : @unlink($p);
        }
        @rmdir($dir);
    }

    private function enabledManager(): CacheManager
    {
        $this->optionStore[CacheManager::OPTION_CONFIG] = ['enabled' => true];
        return new CacheManager();
    }

    /**
     * Run a page through the capture buffer the way WordPress does and return
     * what the visitor would receive. The sink buffer swallows the flush so the
     * test runner sees no output.
     *
     * @param callable $render Echoes the page; may open further buffers.
     */
    private function renderThroughCaptureBuffer(CacheManager $manager, callable $render): string
    {
        ob_start();
        $levelsBefore = ob_get_level();
        $manager->startBuffer();
        $render();
        while (ob_get_level() > $levelsBefore) {
            ob_end_flush();
        }
        return (string) ob_get_clean();
    }

    private function storedBody(string $relative): string
    {
        $file = $this->root . '/' . $relative;
        $this->assertFileExists($file);
        return (string) gzdecode((string) file_get_contents($file));
    }

    /**
     * The key the drop-in worked out for this request, as it hands it over.
     */
    private function dropinChose(string $key): void
    {
        define('WPMGR_PAGE_CACHE_KEY', $key);
    }

    /**
     * @return list<string> Every stored page, relative to the cache root.
     */
    private function storedPages(): array
    {
        $out = [];
        if (!is_dir($this->root)) {
            return $out;
        }
        $it = new \RecursiveIteratorIterator(new \RecursiveDirectoryIterator($this->root, \FilesystemIterator::SKIP_DOTS));
        foreach ($it as $file) {
            if (substr((string) $file, -8) === '.html.gz') {
                $out[] = substr((string) $file, strlen($this->root) + 1);
            }
        }
        sort($out);
        return $out;
    }

    // ---------------------------------------------------------------------
    // Write side: the key is the one the drop-in chose.
    // ---------------------------------------------------------------------

    /**
     * The writer stores the page under exactly the key the drop-in serves it
     * from, whatever the path looks like: a prefixed homepage, a
     * percent-encoded or non-ASCII slug (#981). A sanitised copy of the URI
     * would drop the %XX sequences and file the page under another page's
     * key, often the homepage.
     *
     * @dataProvider provide_requests_and_the_keys_the_dropin_chose
     *
     * @param string $uri Request URI as the browser sends it.
     * @param string $key Key the drop-in handed over for it.
     */
    public function test_writer_stores_under_the_key_the_dropin_chose(string $uri, string $key): void
    {
        $manager = $this->enabledManager();
        $_SERVER['REQUEST_URI'] = $uri;
        $this->dropinChose($key);

        $this->renderThroughCaptureBuffer($manager, static function (): void {
            echo self::HTML_IT;
        });

        $this->assertSame([$key], $this->storedPages(), "$uri must be stored under the drop-in's key and nowhere else");
    }

    /**
     * @return array<string,array{0:string,1:string}>
     */
    public static function provide_requests_and_the_keys_the_dropin_chose(): array
    {
        return [
            'prefixed homepage'        => ['/en/', 'example.com/en/index.html.gz'],
            'prefixed, no slash'       => ['/en', 'example.com/en/index.html.gz'],
            'percent-encoded utf8'     => ['/caf%C3%A9/', 'example.com/café/index.html.gz'],
            'percent-encoded cyrillic' => ['/%D0%BC%D0%B8%D1%80/', 'example.com/мир/index.html.gz'],
            'mobile variant'           => ['/about/', 'example.com/about/index-mobile.html.gz'],
        ];
    }

    /**
     * No key from the drop-in (it is not installed, it declined the request,
     * or it is a release that hands none over) means nothing is stored: the
     * writer has no second opinion about where a page belongs.
     */
    public function test_writer_stores_nothing_without_a_key_from_the_dropin(): void
    {
        $manager = $this->enabledManager();
        $_SERVER['REQUEST_URI'] = '/about/';

        $visitor = $this->renderThroughCaptureBuffer($manager, static function (): void {
            echo self::HTML_IT;
        });

        $this->assertSame(self::HTML_IT, $visitor, 'the visitor still receives the page');
        $this->assertSame([], $this->storedPages());
    }

    /**
     * A key that is not the shape the drop-in produces is refused rather than
     * written: it is read from a constant, and the writer trusts no input.
     *
     * @dataProvider provide_malformed_keys
     *
     * @param string $key Key as found in the constant.
     */
    public function test_writer_refuses_a_malformed_key(string $key): void
    {
        $manager = $this->enabledManager();
        $_SERVER['REQUEST_URI'] = '/about/';
        $this->dropinChose($key);

        $this->renderThroughCaptureBuffer($manager, static function (): void {
            echo self::HTML_IT;
        });

        $this->assertSame([], $this->storedPages(), "key '$key' must be refused");
        $this->assertFileDoesNotExist($this->base . '/escaped/index.html.gz');
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function provide_malformed_keys(): array
    {
        return [
            'empty'             => [''],
            'no file name'      => ['example.com/about/'],
            'not a cache file'  => ['example.com/about/index.php'],
            'absolute'          => ['/example.com/about/index.html.gz'],
            'parent segment'    => ['example.com/../../escaped/index.html.gz'],
            'dot segment'       => ['example.com/./index.html.gz'],
            'empty segment'     => ['example.com//index.html.gz'],
            'uppercase host'    => ['Example.com/index.html.gz'],
            'bad host'          => ['victim..com/index.html.gz'],
            'backslash'         => ['example.com\\about/index.html.gz'],
            'control character' => ["example.com/a\nb/index.html.gz"],
        ];
    }

    /**
     * A plugin that rewrites REQUEST_URI before init (a language plugin that
     * strips the /en prefix on plugins_loaded, say) does not move the
     * translated page into the root bucket: the key was worked out before any
     * plugin ran.
     */
    public function test_a_uri_rewritten_before_init_does_not_move_the_page(): void
    {
        $manager = $this->enabledManager();
        $this->dropinChose('example.com/en/index.html.gz'); // the drop-in saw /en/
        $_SERVER['REQUEST_URI'] = '/';                         // plugins_loaded: the prefix is stripped

        $this->renderThroughCaptureBuffer($manager, static function (): void {
            ob_start(static fn (string $page): string => str_replace('Benvenuti nella home', 'Welcome home', $page));
            echo self::HTML_IT;
        });

        $this->assertSame(['example.com/en/index.html.gz'], $this->storedPages(), 'the root homepage bucket must not receive the /en/ page');
        $this->assertStringContainsString('Welcome home', $this->storedBody('example.com/en/index.html.gz'));
    }

    /**
     * The path the cache keys on is the one the request ARRIVED with. A plugin
     * that rewrites $_SERVER['REQUEST_URI'] while the page is built (to hand
     * WordPress a prefix-less URL, say) must not move the page into another
     * page's bucket: the drop-in already looked the request up under the
     * original path, so a page stored under the rewritten one could never be
     * read back, and would overwrite whatever lives at the rewritten path.
     */
    public function test_buffer_keys_on_the_request_path_it_was_opened_with(): void
    {
        $manager = $this->enabledManager();
        $this->dropinChose('example.com/en/index.html.gz');

        $_SERVER['REQUEST_URI'] = '/en/';
        $this->renderThroughCaptureBuffer($manager, function (): void {
            echo self::HTML_IT;
            // Some plugin hands WordPress a prefix-less URL while the page is built.
            $_SERVER['REQUEST_URI'] = '/';
        });

        $this->assertFileExists($this->root . '/example.com/en/index.html.gz', 'the page must be stored under the path it was requested at');
        $this->assertFileDoesNotExist(
            $this->root . '/example.com/index.html.gz',
            'a rewritten REQUEST_URI must not move the page into the root homepage bucket'
        );
    }

    // ---------------------------------------------------------------------
    // Write side: the capture buffer holds the final bytes.
    // ---------------------------------------------------------------------

    /**
     * Output buffers unwind innermost first, so a buffer opened EARLIER wraps
     * one opened later and sees the later one's output. A translator that
     * opens its buffer at init and rewrites the page when it flushes is
     * therefore only visible to a capture buffer that opened before it. Opened
     * after, the capture stores the untranslated page and a cache hit serves
     * the default language on a translated URL.
     */
    public function test_a_buffer_opened_later_by_another_plugin_is_inside_the_capture(): void
    {
        $manager = $this->enabledManager();
        $_SERVER['REQUEST_URI'] = '/en/';
        $this->dropinChose('example.com/en/index.html.gz');

        $this->renderThroughCaptureBuffer($manager, function (): void {
            // A translator opening its filter buffer at init, after the capture.
            ob_start(static fn (string $page): string => str_replace('Benvenuti nella home', 'Welcome home', $page));
            echo self::HTML_IT;
        });

        $stored = $this->storedBody('example.com/en/index.html.gz');
        $this->assertStringContainsString('Welcome home', $stored, 'the cache must hold what the visitor receives');
        $this->assertStringNotContainsString('Benvenuti nella home', $stored, 'the pre-filter page must not be stored');
    }

    /**
     * The failure the test above rules out, executed the other way round: a
     * translator whose buffer is already open when the capture opens (the
     * order a capture bound to template_redirect has with a translator bound
     * to init) is OUTSIDE the capture. The visitor on a cache miss still sees
     * the translated page, because the translator runs after the capture has
     * handed its bytes on, but what the capture stored is the page before the
     * translator touched it, and that is what every later hit would serve.
     */
    public function test_a_capture_opened_after_a_translator_stores_the_untranslated_page(): void
    {
        $manager = $this->enabledManager();
        $_SERVER['REQUEST_URI'] = '/en/';
        $this->dropinChose('example.com/en/index.html.gz');

        ob_start();
        $levelsBefore = ob_get_level();
        ob_start(static fn (string $page): string => str_replace('Benvenuti nella home', 'Welcome home', $page));
        $manager->startBuffer();
        echo self::HTML_IT;
        while (ob_get_level() > $levelsBefore) {
            ob_end_flush();
        }
        $visitor = (string) ob_get_clean();

        $this->assertStringContainsString('Welcome home', $visitor, 'the visitor on a miss still sees the translated page');
        $stored = $this->storedBody('example.com/en/index.html.gz');
        $this->assertStringContainsString('Benvenuti nella home', $stored, 'the capture inside the translator stores the page before it is translated');
        $this->assertStringNotContainsString('Welcome home', $stored);
    }

    /**
     * The capture buffer has to open ahead of every init callback a plugin
     * registers at the default priority, which is what puts it on the outside.
     */
    public function test_capture_buffer_is_registered_ahead_of_default_priority_init_callbacks(): void
    {
        $manager = $this->enabledManager();

        Monkey\Actions\expectAdded('init')
            ->once()
            ->with(\Mockery::on(static fn ($cb): bool => is_array($cb) && ($cb[1] ?? '') === 'startBuffer'), PHP_INT_MIN);

        $manager->registerHooks();
        $this->assertTrue(true); // the expectation above is the assertion
    }

    /**
     * Requests that can never be stored do not get a buffer at all: opening one
     * before init would otherwise hold back streaming output (admin downloads,
     * REST responses, exports) for no benefit.
     *
     * @dataProvider provide_requests_that_never_get_a_buffer
     *
     * @param string $method HTTP method.
     * @param string $uri    Request URI.
     */
    public function test_requests_that_can_never_be_stored_get_no_buffer(string $method, string $uri): void
    {
        $manager = $this->enabledManager();
        $_SERVER['REQUEST_METHOD'] = $method;
        $_SERVER['REQUEST_URI']    = $uri;

        ob_start();
        $levelsBefore = ob_get_level();
        $manager->startBuffer();
        $opened = ob_get_level() - $levelsBefore;
        while (ob_get_level() > $levelsBefore) {
            ob_end_clean();
        }
        ob_end_clean();

        $this->assertSame(0, $opened, "$method $uri must not open a capture buffer");
    }

    /**
     * @return array<string,array{0:string,1:string}>
     */
    public static function provide_requests_that_never_get_a_buffer(): array
    {
        return [
            'post'       => ['POST', '/en/'],
            'rest route' => ['GET', '/wp-json/wp/v2/posts'],
            'rest query' => ['GET', '/?rest_route=/wp/v2/posts'],
            'login'      => ['GET', '/wp-login.php'],
        ];
    }

    /**
     * And an ordinary front-end GET still does, so the guard above does not
     * quietly switch the writer off.
     */
    public function test_a_front_end_get_gets_a_buffer(): void
    {
        $manager = $this->enabledManager();
        $_SERVER['REQUEST_URI'] = '/en/';

        ob_start();
        $levelsBefore = ob_get_level();
        $manager->startBuffer();
        $opened = ob_get_level() - $levelsBefore;
        while (ob_get_level() > $levelsBefore) {
            ob_end_clean();
        }
        ob_end_clean();

        $this->assertSame(1, $opened);
    }

    /**
     * Number of output buffers startBuffer() leaves open; the sink keeps the
     * test runner's own buffering out of the count.
     */
    private function buffersOpenedBy(CacheManager $manager): int
    {
        ob_start();
        $levelsBefore = ob_get_level();
        $manager->startBuffer();
        $opened = ob_get_level() - $levelsBefore;
        while (ob_get_level() > $levelsBefore) {
            ob_end_clean();
        }
        ob_end_clean();

        return $opened;
    }

    /**
     * The scripts that load WordPress without going through the front
     * controller (the multisite signup and activation pages, the editor
     * helpers) never reached template_redirect, where the buffer used to open,
     * so they were never stored. Opening it at init must not start storing
     * them: a signup form carries a nonce that only its own visitor can use.
     *
     * @dataProvider provide_scripts_that_are_not_the_front_controller
     *
     * @param string $script Script name as WordPress reports it in $pagenow.
     */
    public function test_a_script_other_than_the_front_controller_gets_no_buffer(string $script): void
    {
        $manager = $this->enabledManager();
        $GLOBALS['pagenow']     = $script;
        $_SERVER['REQUEST_URI'] = '/' . $script;

        $this->assertSame(0, $this->buffersOpenedBy($manager), "$script must not open a capture buffer");
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function provide_scripts_that_are_not_the_front_controller(): array
    {
        return [
            'multisite signup'     => ['wp-signup.php'],
            'multisite activation' => ['wp-activate.php'],
            'cron'                 => ['wp-cron.php'],
        ];
    }

    /**
     * The front controller is the one script that does get the buffer.
     */
    public function test_the_front_controller_gets_a_buffer(): void
    {
        $manager = $this->enabledManager();
        $GLOBALS['pagenow']     = 'index.php';
        $_SERVER['REQUEST_URI'] = '/en/';

        $this->assertSame(1, $this->buffersOpenedBy($manager));
    }

    // ---------------------------------------------------------------------
    // Bypass URLs: matching.
    // ---------------------------------------------------------------------

    /**
     * Same matrix as the drop-in test: the PHP path (used by the writer, the
     * preloader and the purge planner) and the pre-WordPress drop-in must
     * agree about what a Bypass URL covers.
     *
     * @dataProvider provide_prefixed_homepage_bypass
     *
     * @param string $entry Bypass URL entry.
     * @param string $uri   Request URI.
     */
    public function test_cacheability_bypass_covers_the_prefixed_homepage_however_it_is_spelled(string $entry, string $uri): void
    {
        $this->assertTrue((new Cacheability([], [$entry]))->matchesBypassUrl($uri), "entry '$entry' must cover $uri");
    }

    /**
     * @return array<string,array{0:string,1:string}>
     */
    public static function provide_prefixed_homepage_bypass(): array
    {
        return AdvancedCacheDropinTest::provide_prefixed_homepage_bypass();
    }

    /**
     * @dataProvider provide_unrelated_paths
     *
     * @param string $uri Request URI a bypass entry for /en/ must not catch.
     */
    public function test_cacheability_bypass_for_the_prefixed_homepage_leaves_other_pages_alone(string $uri): void
    {
        $this->assertFalse((new Cacheability([], ['/en/']))->matchesBypassUrl($uri), "entry '/en/' must not cover $uri");
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function provide_unrelated_paths(): array
    {
        return [
            'root homepage'          => ['/'],
            'unrelated page'         => ['/contact/'],
            'segment that starts en' => ['/enterprise/'],
            'segment that ends en'   => ['/men'],
        ];
    }

    /**
     * The two implementations of the matching rule are duplicated source, for
     * the same reason the path normaliser is: the drop-in runs before the
     * plugin loads. The marked blocks must stay byte-identical.
     */
    public function test_bypass_subject_block_is_identical_in_dropin_and_cacheability(): void
    {
        $marker  = 'WPMGR-BYPASS-URL-SHARED';
        $dropin  = $this->sharedBlock(dirname(__DIR__) . '/assets/wpmgr-advanced-cache.php', $marker);
        $classes = $this->sharedBlock(dirname(__DIR__) . '/includes/cache/class-cacheability.php', $marker);

        $this->assertNotSame('', trim($dropin));
        $this->assertSame($classes, $dropin, 'the drop-in and Cacheability must carry the same bypass-URL subject rule');
    }

    /**
     * @return string Block source between the BEGIN and END marker lines, dedented.
     */
    private function sharedBlock(string $file, string $marker): string
    {
        $source = file_get_contents($file);
        $this->assertIsString($source, "unreadable: $file");

        $out = [];
        $in  = false;
        foreach (explode("\n", $source) as $line) {
            if (!$in) {
                $in = strpos($line, $marker . ' BEGIN') !== false;
                continue;
            }
            if (strpos($line, $marker . ' END') !== false) {
                $in = false;
                break;
            }
            $out[] = $line;
        }
        $this->assertNotSame([], $out, "no marked block found in $file");
        $this->assertFalse($in, "unterminated marked block in $file");

        $indent = PHP_INT_MAX;
        foreach ($out as $line) {
            if (trim($line) !== '') {
                $indent = min($indent, strlen($line) - strlen(ltrim($line)));
            }
        }
        foreach ($out as $i => $line) {
            $out[$i] = trim($line) === '' ? '' : substr($line, $indent);
        }

        return implode("\n", $out) . "\n";
    }

    // ---------------------------------------------------------------------
    // Bypass URLs: applying them.
    // ---------------------------------------------------------------------

    private function warm(string $relative, string $body): string
    {
        $file = $this->root . '/' . $relative;
        @mkdir(dirname($file), 0o777, true);
        file_put_contents($file, gzencode($body));
        return $file;
    }

    /**
     * A page cached before its Bypass URL was added is still on disk when the
     * entry is applied, and a web server serving it straight from disk never
     * reads the entry. Applying the list therefore has to remove the pages the
     * new entries cover.
     */
    public function test_applying_new_bypass_urls_removes_pages_cached_before_them(): void
    {
        $manager = $this->enabledManager();
        $stale   = $this->warm('example.com/en/index.html.gz', self::HTML_IT);
        $other   = $this->warm('example.com/index.html.gz', self::HTML_IT);

        $manager->applyConfig(['enabled' => true, 'bypass_urls' => ['/en/']]);

        $this->assertFileDoesNotExist($stale, 'the page a new Bypass URL covers must not stay on disk to be served');
        $this->assertFileDoesNotExist($other, 'the purge on a new Bypass URL clears the cache, which re-warms on its own');
    }

    /**
     * Pushing the same configuration again, or one that adds no Bypass URL,
     * must not throw the cache away: settings are pushed for many reasons.
     */
    public function test_applying_config_without_new_bypass_urls_keeps_the_cache(): void
    {
        $manager = $this->enabledManager();
        $manager->applyConfig(['enabled' => true, 'bypass_urls' => ['/cart']]);

        $kept = $this->warm('example.com/en/index.html.gz', self::HTML_IT);

        // Same list again, and a pure removal: neither adds an entry.
        $manager->applyConfig(['enabled' => true, 'bypass_urls' => ['/cart'], 'cache_mobile' => true]);
        $manager->applyConfig(['enabled' => true, 'bypass_urls' => []]);

        $this->assertFileExists($kept, 'a configuration push that adds no Bypass URL must keep the cache');
    }
}
