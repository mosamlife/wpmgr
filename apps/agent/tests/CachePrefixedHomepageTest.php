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

    /** The options table the key-scheme claim writes to; its rows are $optionStore. */
    private FakeKeySchemeWpdb $wpdb;

    /**
     * Runs once, right after a request has read which key scheme is recorded
     * and before it does anything with that: another request lands in that
     * window.
     */
    private ?\Closure $afterSchemeRead = null;

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
        $this->afterSchemeRead = null;
        $this->wpdb            = new FakeKeySchemeWpdb($this->optionStore);
        $GLOBALS['wpdb']       = $this->wpdb;
        Functions\when('get_option')->alias(function ($k, $d = false) {
            $value = $this->optionStore[$k] ?? $d;
            if ($k === CacheManager::OPTION_PURGED_FOR_KEYS && $this->afterSchemeRead !== null) {
                $otherRequest          = $this->afterSchemeRead;
                $this->afterSchemeRead = null;
                $otherRequest();
            }

            return $value;
        });
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
        unset($GLOBALS['wpdb']);
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
     * percent-encoded or non-ASCII slug (#981).
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
     * The drop-in does not run (WP_CACHE is off, another plugin owns
     * advanced-cache.php, ours sits under another file name), so it hands no
     * key over. The page is still stored, under the key the drop-in's rule
     * gives for the request as it arrived: the file the web-server rules look
     * for. A path the browser percent-encodes, a path that is not ASCII and a
     * path that is not lower case each get the key of their own path.
     *
     * @dataProvider provide_requests_and_the_keys_the_dropin_rule_gives
     *
     * @param string               $uri Request URI as the browser sends it.
     * @param array<string,string> $get $_GET for it.
     * @param string               $key Key the drop-in's rule gives.
     */
    public function test_without_a_dropin_key_the_page_is_stored_under_the_key_the_dropin_rule_gives(string $uri, array $get, string $key): void
    {
        $manager = $this->enabledManager();
        $_SERVER['REQUEST_URI'] = $uri;
        $_GET                   = $get;

        $visitor = $this->renderThroughCaptureBuffer($manager, static function (): void {
            echo self::HTML_IT;
        });

        $this->assertSame(self::HTML_IT, $visitor, 'the visitor still receives the page');
        $this->assertSame([$key], $this->storedPages(), "$uri must be stored under the drop-in rule's key and nowhere else");
        if ($key !== 'example.com/index.html.gz') {
            $this->assertFileDoesNotExist($this->root . '/example.com/index.html.gz', "$uri must not be stored under the homepage key");
        }
    }

    /**
     * @return array<string,array{0:string,1:array<string,string>,2:string}>
     */
    public static function provide_requests_and_the_keys_the_dropin_rule_gives(): array
    {
        return [
            'root'                     => ['/', [], 'example.com/index.html.gz'],
            'page'                     => ['/about/', [], 'example.com/about/index.html.gz'],
            'prefixed homepage'        => ['/en', [], 'example.com/en/index.html.gz'],
            'percent-encoded utf8'     => ['/caf%C3%A9/', [], 'example.com/café/index.html.gz'],
            'literal utf8'             => ['/café/', [], 'example.com/café/index.html.gz'],
            'percent-encoded cyrillic' => ['/%D0%BC%D0%B8%D1%80/', [], 'example.com/мир/index.html.gz'],
            'mixed case'               => ['/About/Us/', [], 'example.com/about/us/index.html.gz'],
            'marketing query'          => ['/about/?utm_source=x', ['utm_source' => 'x'], 'example.com/about/index.html.gz'],
        ];
    }

    /**
     * A key the drop-in handed over always wins: the page goes where the
     * drop-in serves it from, not where the rule would put the request now in
     * front of the writer.
     */
    public function test_a_key_from_the_dropin_wins_over_the_derived_one(): void
    {
        $manager = $this->enabledManager();
        $_SERVER['REQUEST_URI'] = '/about/';
        $this->dropinChose('example.com/en/index.html.gz');

        $this->renderThroughCaptureBuffer($manager, static function (): void {
            echo self::HTML_IT;
        });

        $this->assertSame(['example.com/en/index.html.gz'], $this->storedPages(), 'the derived key must not be used, or added, next to the drop-in\'s');
    }

    /**
     * The derived key follows the request as it was when the buffer opened,
     * the same as the drop-in's key follows the request as it arrived: a URI
     * rewritten while the page is built does not change where the page is
     * stored.
     */
    public function test_a_derived_key_is_fixed_when_the_buffer_opens(): void
    {
        $manager = $this->enabledManager();
        $_SERVER['REQUEST_URI'] = '/%D0%BC%D0%B8%D1%80/';

        $this->renderThroughCaptureBuffer($manager, function (): void {
            echo self::HTML_IT;
            $_SERVER['REQUEST_URI'] = '/';
        });

        $this->assertSame(['example.com/мир/index.html.gz'], $this->storedPages());
    }

    /**
     * Without a key from the drop-in the rule can still decline a request, as
     * the drop-in does, and then nothing is stored: a host that names no
     * bucket, a URI longer than the drop-in reads, more cache-varying query
     * keys than a variant may carry, a Bypass URL written the way the browser
     * sends the path, a cookie that marks a personalised page.
     *
     * @dataProvider provide_requests_the_dropin_rule_declines
     *
     * @param array<string,mixed>  $config Page-cache settings.
     * @param array<string,string> $server $_SERVER overrides.
     * @param array<string,string> $get    $_GET.
     * @param array<string,string> $cookie $_COOKIE.
     */
    public function test_without_a_dropin_key_a_request_the_rule_declines_stores_nothing(array $config, array $server, array $get, array $cookie): void
    {
        $this->optionStore[CacheManager::OPTION_CONFIG] = ['enabled' => true] + $config;
        $manager = new CacheManager();
        $_SERVER = $server + $_SERVER;
        $_GET    = $get;
        $_COOKIE = $cookie;

        $visitor = $this->renderThroughCaptureBuffer($manager, static function (): void {
            echo self::HTML_IT;
        });

        $this->assertSame(self::HTML_IT, $visitor, 'the visitor still receives the page');
        $this->assertSame([], $this->storedPages());
    }

    /**
     * @return array<string,array{0:array<string,mixed>,1:array<string,string>,2:array<string,string>,3:array<string,string>}>
     */
    public static function provide_requests_the_dropin_rule_declines(): array
    {
        $manyKeys = [];
        foreach (range('a', 'm') as $letter) {
            $manyKeys['filter_' . $letter] = '1'; // a known cache-varying prefix, so only the key cap refuses it
        }

        return [
            'uncacheable host'           => [[], ['REQUEST_URI' => '/about/', 'HTTP_HOST' => 'victim..com'], [], []],
            'overlong uri'               => [[], ['REQUEST_URI' => '/' . str_repeat('a', 2100) . '/'], [], []],
            'too many query keys'        => [[], ['REQUEST_URI' => '/shop/?' . http_build_query($manyKeys)], $manyKeys, []],
            'bypass url, percent-encoded' => [['bypass_urls' => ['/%d0%bc%d0%b8%d1%80']], ['REQUEST_URI' => '/%D0%BC%D0%B8%D1%80/'], [], []],
            'bypass cookie'              => [[], ['REQUEST_URI' => '/about/'], [], ['woocommerce_cart_hash' => 'abc']],
        ];
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
     * Without a key from the drop-in, the key is worked out from the request
     * path when the buffer opens. A path that is no longer the one the plugin
     * loaded with (a language plugin that strips its prefix on plugins_loaded,
     * say) is not the path the drop-in and the web-server rules look a page up
     * by, so nothing is stored for the request, under the new path's key or any
     * other. The visitor still receives the page.
     *
     * @dataProvider provide_paths_changed_after_the_plugin_loaded
     *
     * @param string $loaded Request URI when the plugin loaded.
     * @param string $later  Request URI when the buffer opens.
     */
    public function test_without_a_dropin_key_a_uri_changed_after_the_plugin_loaded_stores_nothing(string $loaded, string $later): void
    {
        $manager = $this->enabledManager();
        $_SERVER['REQUEST_URI'] = $loaded;
        $manager->registerHooks();          // the plugin loads
        $_SERVER['REQUEST_URI'] = $later;   // plugins_loaded: another plugin changes the path

        $visitor = $this->renderThroughCaptureBuffer($manager, static function (): void {
            echo self::HTML_IT;
        });

        $this->assertSame(self::HTML_IT, $visitor, 'the visitor still receives the page');
        $this->assertSame([], $this->storedPages(), "$loaded changed to $later must store nothing, not even under the key of $later");
    }

    /**
     * @return array<string,array{0:string,1:string}>
     */
    public static function provide_paths_changed_after_the_plugin_loaded(): array
    {
        return [
            'prefix stripped'      => ['/en/', '/'],
            'prefix replaced'      => ['/en/', '/de/'],
            'trailing slash added' => ['/en', '/en/'],
        ];
    }

    /**
     * The same request with its path left alone is stored under the key the
     * rule gives. WordPress adds slashes to the superglobals once the plugins
     * have loaded; that is not a change of path, including for a character it
     * slashes.
     *
     * @dataProvider provide_paths_left_alone
     *
     * @param string               $uri Request URI as the browser sends it.
     * @param array<string,string> $get $_GET for it.
     * @param string               $key Key the drop-in's rule gives.
     */
    public function test_without_a_dropin_key_an_unchanged_uri_is_stored_under_the_derived_key(string $uri, array $get, string $key): void
    {
        $manager = $this->enabledManager();
        $_SERVER['REQUEST_URI'] = $uri;
        $_GET                   = $get;
        $manager->registerHooks();                          // the plugin loads
        $_SERVER['REQUEST_URI'] = addslashes($uri);         // once the plugins have loaded, WordPress slashes the superglobals

        $visitor = $this->renderThroughCaptureBuffer($manager, static function (): void {
            echo self::HTML_IT;
        });

        $this->assertSame(self::HTML_IT, $visitor, 'the visitor still receives the page');
        $this->assertSame([$key], $this->storedPages(), "$uri must be stored under the derived key");
    }

    /**
     * @return array<string,array{0:string,1:array<string,string>,2:string}>
     */
    public static function provide_paths_left_alone(): array
    {
        return [
            'prefixed homepage'       => ['/en/', [], 'example.com/en/index.html.gz'],
            'apostrophe in the query' => ["/about/?utm_source=it's", ['utm_source' => "it's"], 'example.com/about/index.html.gz'],
        ];
    }

    /**
     * The path is read once, when the hooks are first registered. Registering
     * them again after another plugin changed the path does not take the
     * changed path as the one the request arrived with.
     */
    public function test_registering_the_hooks_again_keeps_the_path_the_plugin_loaded_with(): void
    {
        $manager = $this->enabledManager();
        $_SERVER['REQUEST_URI'] = '/en/';
        $manager->registerHooks();          // the plugin loads
        $_SERVER['REQUEST_URI'] = '/';      // plugins_loaded: the prefix is stripped
        $manager->registerHooks();

        $this->renderThroughCaptureBuffer($manager, static function (): void {
            echo self::HTML_IT;
        });

        $this->assertSame([], $this->storedPages(), 'the path the plugin loaded with must still be the one compared against');
    }

    /**
     * A key from the drop-in is the file it serves the request from, whatever
     * path the plugin loaded with: the path captured at load plays no part.
     */
    public function test_a_key_from_the_dropin_does_not_depend_on_the_path_the_plugin_loaded_with(): void
    {
        $manager = $this->enabledManager();
        $this->dropinChose('example.com/en/index.html.gz'); // the drop-in ran first and saw /en/
        $_SERVER['REQUEST_URI'] = '/en/';
        $manager->registerHooks();                          // the plugin loads
        $_SERVER['REQUEST_URI'] = '/';                      // plugins_loaded: the prefix is stripped

        $visitor = $this->renderThroughCaptureBuffer($manager, static function (): void {
            echo self::HTML_IT;
        });

        $this->assertSame(self::HTML_IT, $visitor, 'the visitor still receives the page');
        $this->assertSame(['example.com/en/index.html.gz'], $this->storedPages(), 'the drop-in\'s key stands even though the path changed after the plugin loaded');
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

    // ---------------------------------------------------------------------
    // Upgrade: pages stored by an earlier release are removed, once.
    // ---------------------------------------------------------------------

    /**
     * The first boot of this release removes every page stored before it,
     * once: a page stored after that is kept by every later boot.
     */
    public function test_first_boot_of_this_release_purges_once(): void
    {
        $manager = $this->enabledManager();
        $old     = $this->warm('example.com/index.html.gz', self::HTML_IT);

        $manager->maybeRefreshDropin();
        $this->assertFileDoesNotExist($old, 'a page stored by an earlier release must be removed on the first boot');

        $new = $this->warm('example.com/en/index.html.gz', self::HTML_IT);
        $manager->maybeRefreshDropin();
        (new CacheManager())->maybeRefreshDropin();
        $this->assertFileExists($new, 'later boots must not purge again');
    }

    /**
     * Requests that find the scheme not recorded at the same moment: the second
     * lands between the first one's read of the record and its claim. Exactly
     * one of them purges, and the purge reaches the host and edge caches once.
     */
    public function test_requests_that_arrive_together_on_the_first_boot_purge_once(): void
    {
        $manager = $this->enabledManager();
        $old     = $this->warm('example.com/index.html.gz', self::HTML_IT);
        Monkey\Actions\expectDone('wpmgr_purge_everything:before')->once();
        $this->afterSchemeRead = static function (): void {
            (new CacheManager())->maybeRefreshDropin();
        };

        $manager->maybeRefreshDropin();

        $this->assertNull($this->afterSchemeRead, 'the second request must have run in that window');
        $this->assertFileDoesNotExist($old);
        $this->assertSame(CacheManager::KEY_SCHEME, $this->optionStore[CacheManager::OPTION_PURGED_FOR_KEYS] ?? null);
    }

    /**
     * A request that arrives while the first is still purging finds the scheme
     * already recorded, and purges nothing.
     */
    public function test_a_request_that_arrives_during_the_purge_does_not_purge_again(): void
    {
        $manager = $this->enabledManager();
        $this->warm('example.com/index.html.gz', self::HTML_IT);
        $arrived = false;
        Monkey\Actions\expectDone('wpmgr_purge_everything:before')->once()->whenHappen(function () use (&$arrived): void {
            if ($arrived) {
                return;
            }
            $arrived = true;
            (new CacheManager())->maybeRefreshDropin();
        });

        $manager->maybeRefreshDropin();

        $this->assertTrue($arrived, 'the second request must have run during the purge');
    }

    /**
     * A recorded scheme older than the current one is replaced by exactly one
     * request, which purges; the same race on it purges once.
     */
    public function test_a_newer_key_scheme_purges_once_more(): void
    {
        $this->optionStore[CacheManager::OPTION_PURGED_FOR_KEYS] = 'older-' . CacheManager::KEY_SCHEME;
        $manager = $this->enabledManager();
        $old     = $this->warm('example.com/index.html.gz', self::HTML_IT);
        Monkey\Actions\expectDone('wpmgr_purge_everything:before')->once();
        $this->afterSchemeRead = static function (): void {
            (new CacheManager())->maybeRefreshDropin();
        };

        $manager->maybeRefreshDropin();

        $this->assertNull($this->afterSchemeRead, 'the second request must have run in that window');
        $this->assertFileDoesNotExist($old);
        $this->assertSame(CacheManager::KEY_SCHEME, $this->optionStore[CacheManager::OPTION_PURGED_FOR_KEYS] ?? null);
    }

    /**
     * When the record can never be written (a read-only database) no request
     * claims the purge, so none purges: the purge is not repeated on every
     * request for as long as the write keeps failing.
     */
    public function test_an_option_that_cannot_be_written_causes_no_purge_loop(): void
    {
        $this->enabledManager();
        $this->warm('example.com/index.html.gz', self::HTML_IT);
        $this->wpdb->writesFail = true;
        Functions\when('update_option')->justReturn(false);
        $purges = 0;
        Monkey\Actions\expectDone('wpmgr_purge_everything:before')->zeroOrMoreTimes()->whenHappen(function () use (&$purges): void {
            $purges++;
        });

        for ($request = 0; $request < 4; $request++) {
            (new CacheManager())->maybeRefreshDropin();
        }

        $this->assertSame(0, $purges, 'no request may purge while the record cannot be written');
        $this->assertArrayNotHasKey(CacheManager::OPTION_PURGED_FOR_KEYS, $this->optionStore);
        $this->assertCount(4, $this->wpdb->writes, 'every request must have tried to claim the purge');
    }

    /**
     * The claim is written past the options cache, so the cache entries that
     * would still answer with the old record are dropped; a boot that finds the
     * scheme recorded drops nothing.
     */
    public function test_a_claim_drops_the_cached_options_it_made_stale(): void
    {
        $dropped = [];
        Functions\when('wp_cache_delete')->alias(function ($key, $group = '') use (&$dropped): bool {
            $dropped[] = $group . ':' . $key;

            return true;
        });

        $this->enabledManager()->maybeRefreshDropin();
        sort($dropped);
        $this->assertSame(['options:alloptions', 'options:notoptions', 'options:' . CacheManager::OPTION_PURGED_FOR_KEYS], $dropped);

        $dropped = [];
        (new CacheManager())->maybeRefreshDropin();
        $this->assertSame([], $dropped, 'a recorded scheme needs no claim and drops nothing');
    }

    /**
     * Control for the tests above: the options table double has the unique key
     * and the conditional UPDATE that make a claim a claim.
     */
    public function test_the_options_table_double_admits_one_claim(): void
    {
        $insert = 'INSERT IGNORE INTO %i (option_name, option_value, autoload) VALUES (%s, %s, %s)';
        $update = 'UPDATE %i SET option_value = %s WHERE option_name = %s AND option_value = %s';
        $table  = $this->wpdb->options;

        $this->assertSame(1, $this->wpdb->query($this->wpdb->prepare($insert, $table, 'name', 'a', 'yes')));
        $this->assertSame(0, $this->wpdb->query($this->wpdb->prepare($insert, $table, 'name', 'a', 'yes')), 'the second insert of a name changes nothing');
        $this->assertSame(1, $this->wpdb->query($this->wpdb->prepare($update, $table, 'b', 'name', 'a')));
        $this->assertSame(0, $this->wpdb->query($this->wpdb->prepare($update, $table, 'c', 'name', 'a')), 'an update from a value that is no longer stored changes nothing');
        $this->assertSame('b', $this->optionStore['name']);
    }

    /**
     * A site whose page cache is off and whose drop-in is gone has nothing
     * that serves stored pages: it is not purged (a purge also clears host and
     * edge caches through the integrations), and it is not asked again.
     */
    public function test_a_site_without_the_page_cache_is_not_purged_on_upgrade(): void
    {
        $this->optionStore[CacheManager::OPTION_CONFIG] = ['enabled' => false];
        $kept = $this->warm('example.com/index.html.gz', self::HTML_IT);
        Monkey\Actions\expectDone('wpmgr_purge_everything:before')->never();

        (new CacheManager())->maybeRefreshDropin();

        $this->assertFileExists($kept);
        $this->assertArrayHasKey(CacheManager::OPTION_PURGED_FOR_KEYS, $this->optionStore, 'the upgrade is recorded as handled');
    }

    /**
     * A page cache switched off by a pushed setting leaves the drop-in in
     * place, still serving what is stored, so that site is purged too.
     */
    public function test_a_site_whose_dropin_is_still_installed_is_purged_on_upgrade(): void
    {
        $this->optionStore[CacheManager::OPTION_CONFIG] = ['enabled' => false];
        file_put_contents($this->base . '/advanced-cache.php', "<?php\n// WPMgr Page Cache drop-in (advanced-cache.php).\n");
        $old = $this->warm('example.com/index.html.gz', self::HTML_IT);

        (new CacheManager())->maybeRefreshDropin();

        $this->assertFileDoesNotExist($old);
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
