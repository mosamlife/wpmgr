<?php
/**
 * What the page cache stores, decided at the moment the capture buffer is
 * flushed.
 *
 * A page is stored only when it is a whole HTML page that the visitor actually
 * received, rendered through the template stage, and nothing about the
 * response says it must not be cached:
 *
 *  - output produced before template_redirect (an age gate, a coming-soon
 *    page, a download answered on init) is passed through and never stored;
 *  - a response that is not HTML, carries Content-Disposition, or grows past
 *    the size limit is passed through as it is produced, so it is never held
 *    in memory in full, and is not stored;
 *  - a buffer a plugin throws away (ob_end_clean, ob_get_clean) is not stored;
 *  - Cache-Control no-store or private, and DONOTCACHEPAGE, mean not stored;
 *  - the Bypass URLs and the enabled flag are read again when the page is
 *    stored, so a page bypassed or a cache switched off while the request ran
 *    is not stored; a push that switches the cache off clears it.
 *
 * Each test runs in its own process: the drop-in's key and DONOTCACHEPAGE are
 * constants, and WP_CONTENT_DIR is defined per test.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Cache\CacheManager;
use WPMgr\Agent\Cache\CacheWriter;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Cache\CacheWriter
 * @covers \WPMgr\Agent\Cache\CacheManager
 * @runTestsInSeparateProcesses
 * @preserveGlobalState disabled
 */
final class CacheWriterStoreGatesTest extends TestCase
{
    private const PAGE = '<!DOCTYPE html><html><body>An ordinary page</body></html>';

    private const KEY = 'example.com/about/index.html.gz';

    private string $base = '';

    private string $root = '';

    /** @var array<string,mixed> */
    private array $optionStore = [];

    /** @var list<string> What headers_list() reports for the response. */
    private array $headers = ['Content-Type: text/html; charset=UTF-8'];

    private bool $templateStage = true;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->base = sys_get_temp_dir() . '/wpmgr-store-gates-' . uniqid('', true);
        $this->root = $this->base . '/cache/wpmgr';
        @mkdir($this->root, 0o777, true);
        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', $this->base);
        }

        $this->optionStore = [CacheManager::OPTION_CONFIG => ['enabled' => true]];
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
        Functions\when('did_action')->alias(fn (string $hook): int => $hook === 'template_redirect' && $this->templateStage ? 1 : 0);
        Functions\when('headers_list')->alias(fn (): array => $this->headers);

        $_SERVER['REQUEST_METHOD'] = 'GET';
        $_SERVER['HTTP_HOST']      = 'example.com';
        $_SERVER['REQUEST_URI']    = '/about/';
        $_COOKIE                   = [];
        $_GET                      = [];

        define(CacheWriter::KEY_CONSTANT, self::KEY); // the drop-in's key for /about/
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

    /**
     * Run output through the capture buffer the way WordPress does (the buffer
     * opens on init; the shutdown flush closes it) and return what reached the
     * visitor.
     *
     * @param callable $render Produces the response; may open or close buffers.
     */
    private function respond(callable $render): string
    {
        $manager = new CacheManager();
        ob_start();
        $levelsBefore = ob_get_level();
        $manager->startBuffer();
        $render();
        while (ob_get_level() > $levelsBefore) {
            ob_end_flush();
        }
        return (string) ob_get_clean();
    }

    private function stored(): bool
    {
        return is_file($this->root . '/' . self::KEY);
    }

    /** Positive control: everything below must be able to go red. */
    public function test_an_ordinary_page_is_stored(): void
    {
        $visitor = $this->respond(static function (): void {
            echo self::PAGE;
        });

        $this->assertSame(self::PAGE, $visitor);
        $this->assertTrue($this->stored(), 'an ordinary page must be stored, or no test below proves anything');
    }

    // ---------------------------------------------------------------------
    // Item: only pages that reached template_redirect.
    // ---------------------------------------------------------------------

    /**
     * An age gate, a coming-soon page, a login screen included on init: a
     * response answered before the template stage is never stored.
     */
    public function test_a_page_answered_before_the_template_stage_is_not_stored(): void
    {
        $this->templateStage = false;

        $visitor = $this->respond(static function (): void {
            echo '<!DOCTYPE html><html><body>GATE: confirm you are over 18</body></html>';
        });

        $this->assertStringContainsString('GATE', $visitor, 'the visitor still receives the gate');
        $this->assertFalse($this->stored(), 'a page answered before template_redirect must not be stored');
    }

    // ---------------------------------------------------------------------
    // Item: large or non-HTML responses are not held in memory.
    // ---------------------------------------------------------------------

    /**
     * A download streamed on init through a front-end GET, with no buffer
     * clearing: it passes through as it is produced. Memory held by the
     * capture stays near one chunk instead of the whole file, every byte
     * reaches the visitor, and nothing is stored.
     */
    public function test_a_download_streamed_on_init_is_not_held_in_memory(): void
    {
        ini_set('memory_limit', '1024M');
        $this->templateStage = false;
        $this->headers       = ['Content-Type: application/zip'];
        $manager             = new CacheManager();

        $sent = 0;
        ob_start(static function (string $chunk) use (&$sent): string {
            $sent += strlen($chunk);
            return '';
        }, 1); // the network: takes each byte as it comes
        $levelsBefore = ob_get_level();
        $manager->startBuffer();

        $block  = str_repeat('Z', 1 << 20);
        $before = memory_get_usage();
        for ($i = 0; $i < 64; $i++) {
            echo $block;
        }
        $held = memory_get_usage() - $before;

        while (ob_get_level() > $levelsBefore) {
            ob_end_flush();
        }
        ob_end_clean();

        $this->assertLessThan(4 << 20, $held, sprintf('the capture held %.1f MB of a 64 MB download', $held / 1048576));
        $this->assertSame(64 << 20, $sent, 'every byte must reach the visitor');
        $this->assertFalse($this->stored());
    }

    /**
     * After the template stage, a response that is not HTML is passed through
     * as soon as it outgrows one chunk.
     */
    public function test_a_non_html_response_after_the_template_stage_is_passed_through(): void
    {
        ini_set('memory_limit', '1024M');
        $this->headers = ['Content-Type: application/octet-stream'];
        $manager       = new CacheManager();

        ob_start(static fn (string $chunk): string => '', 1);
        $levelsBefore = ob_get_level();
        $manager->startBuffer();
        $block  = str_repeat('Z', 1 << 20);
        $before = memory_get_usage();
        for ($i = 0; $i < 32; $i++) {
            echo $block;
        }
        $held = memory_get_usage() - $before;
        while (ob_get_level() > $levelsBefore) {
            ob_end_flush();
        }
        ob_end_clean();

        $this->assertLessThan(4 << 20, $held, sprintf('the capture held %.1f MB of a non-HTML response', $held / 1048576));
        $this->assertFalse($this->stored());
    }

    /**
     * A response that carries Content-Disposition is not a page, however small
     * and whatever its type.
     */
    public function test_a_response_with_content_disposition_is_not_stored(): void
    {
        $this->headers = ['Content-Type: text/html; charset=UTF-8', 'Content-Disposition: attachment; filename="report.html"'];

        $visitor = $this->respond(static function (): void {
            echo self::PAGE;
        });

        $this->assertSame(self::PAGE, $visitor);
        $this->assertFalse($this->stored());
    }

    /**
     * A non-HTML response that fits in one chunk is not stored either.
     */
    public function test_a_small_non_html_response_is_not_stored(): void
    {
        $this->headers = ['Content-Type: application/json; charset=UTF-8'];

        $this->respond(static function (): void {
            echo self::PAGE;
        });

        $this->assertFalse($this->stored());
    }

    /**
     * application/xhtml+xml is a page (WordPress's html_type option allows it),
     * and so is a response with no Content-Type header, which PHP sends as
     * text/html.
     *
     * @dataProvider provide_html_content_types
     *
     * @param list<string> $headers Response headers.
     */
    public function test_html_content_types_are_stored(array $headers): void
    {
        $this->headers = $headers;

        $this->respond(static function (): void {
            echo self::PAGE;
        });

        $this->assertTrue($this->stored());
    }

    /**
     * @return array<string,array{0:list<string>}>
     */
    public static function provide_html_content_types(): array
    {
        return [
            'xhtml'           => [['Content-Type: application/xhtml+xml; charset=UTF-8']],
            'no content type' => [[]],
            'upper case'      => [['content-type: TEXT/HTML']],
        ];
    }

    /**
     * A large page, bigger than one chunk but under the limit, is still held
     * whole, stored whole and delivered whole.
     */
    public function test_a_large_page_under_the_limit_is_stored_whole(): void
    {
        $body = '<!DOCTYPE html><html><body>' . str_repeat('<p>large page</p>', 200000) . '</body></html>';
        $this->assertGreaterThan(CacheWriter::CHUNK_BYTES, strlen($body), 'the page must span several chunks');
        $this->assertLessThan(CacheWriter::MAX_PAGE_BYTES, strlen($body));

        $visitor = $this->respond(static function () use ($body): void {
            foreach (str_split($body, 65536) as $piece) {
                echo $piece;
            }
        });

        $this->assertSame($body, $visitor, 'the visitor must receive the whole page, in order');
        $this->assertTrue($this->stored());
        $stored = (string) gzdecode((string) file_get_contents($this->root . '/' . self::KEY));
        $this->assertStringStartsWith($body, $stored, 'the stored page must be the whole page');
    }

    /**
     * A page past the limit is passed through and not stored, and the visitor
     * still receives all of it, in order.
     */
    public function test_a_page_past_the_limit_is_delivered_but_not_stored(): void
    {
        ini_set('memory_limit', '1024M');
        $body = '<!DOCTYPE html><html><body>' . str_repeat('x', CacheWriter::MAX_PAGE_BYTES + (2 << 20)) . '</body></html>';

        $visitor = $this->respond(static function () use ($body): void {
            foreach (str_split($body, 1 << 20) as $piece) {
                echo $piece;
            }
        });

        $this->assertSame(strlen($body), strlen($visitor));
        $this->assertSame(md5($body), md5($visitor), 'the visitor must receive every byte, in order');
        $this->assertFalse($this->stored());
    }

    // ---------------------------------------------------------------------
    // Item: discarded output is never stored.
    // ---------------------------------------------------------------------

    /**
     * A plugin that throws the buffer away (to replace the page, or before a
     * redirect) means the visitor received something else: the discarded page
     * is not stored.
     *
     * @dataProvider provide_discards
     *
     * @param string $discard ob_end_clean or ob_get_clean.
     */
    public function test_a_discarded_page_is_not_stored(string $discard): void
    {
        $visitor = $this->respond(static function () use ($discard): void {
            echo '<!DOCTYPE html><html><body>DISCARDED DRAFT</body></html>';
            $discard();
            echo 'WHAT THE VISITOR GOT';
        });

        $this->assertSame('WHAT THE VISITOR GOT', $visitor);
        $this->assertFalse($this->stored(), 'a page the visitor never received must not be stored');
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function provide_discards(): array
    {
        return [
            'ob_end_clean' => ['ob_end_clean'],
            'ob_get_clean' => ['ob_get_clean'],
        ];
    }

    /**
     * ob_clean() keeps the buffer open: what follows is the whole response the
     * visitor receives, so it is stored.
     */
    public function test_a_page_rendered_after_ob_clean_is_stored(): void
    {
        $visitor = $this->respond(static function (): void {
            echo 'partial output thrown away';
            ob_clean();
            echo self::PAGE;
        });

        $this->assertSame(self::PAGE, $visitor);
        $this->assertTrue($this->stored());
        $this->assertStringNotContainsString('thrown away', (string) gzdecode((string) file_get_contents($this->root . '/' . self::KEY)));
    }

    /**
     * Output flushed early with ob_flush() has left before the page is
     * complete: what reaches the final flush is only the rest of the page.
     */
    public function test_a_page_flushed_part_way_is_not_stored(): void
    {
        $visitor = $this->respond(static function (): void {
            echo '<!DOCTYPE html><html><head></head>';
            ob_flush();
            echo '<body>rest</body></html>';
        });

        $this->assertSame('<!DOCTYPE html><html><head></head><body>rest</body></html>', $visitor);
        $this->assertFalse($this->stored());
    }

    // ---------------------------------------------------------------------
    // Item: a response that says it must not be cached.
    // ---------------------------------------------------------------------

    /**
     * @dataProvider provide_cache_control_that_forbids_storing
     *
     * @param string $value Cache-Control value.
     */
    public function test_cache_control_no_store_or_private_is_honoured(string $value): void
    {
        $this->headers = ['Content-Type: text/html; charset=UTF-8', 'Cache-Control: ' . $value];

        $this->respond(static function (): void {
            echo self::PAGE;
        });

        $this->assertFalse($this->stored(), "Cache-Control: $value must not be stored");
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function provide_cache_control_that_forbids_storing(): array
    {
        return [
            'no-store'               => ['no-store'],
            'private'                => ['private'],
            'wordpress nocache 6.8+' => ['no-cache, must-revalidate, max-age=0, no-store, private'],
            'upper case'             => ['NO-STORE'],
            'private with fields'    => ['private="Set-Cookie", max-age=60'],
        ];
    }

    /**
     * Other directives leave the page storable.
     *
     * @dataProvider provide_cache_control_that_allows_storing
     *
     * @param list<string> $headers Response headers.
     */
    public function test_other_cache_control_values_and_cookies_do_not_stop_storing(array $headers): void
    {
        $this->headers = $headers;

        $this->respond(static function (): void {
            echo self::PAGE;
        });

        $this->assertTrue($this->stored());
    }

    /**
     * @return array<string,array{0:list<string>}>
     */
    public static function provide_cache_control_that_allows_storing(): array
    {
        return [
            'public max-age'   => [['Cache-Control: public, max-age=600']],
            'not a directive'  => [['Cache-Control: max-age=60, x-not-private']],
            'language cookie'  => [['Set-Cookie: pll_language=en; path=/']],
            'other header'     => [['X-Private: no-store']],
        ];
    }

    public function test_donotcachepage_is_honoured(): void
    {
        define('DONOTCACHEPAGE', true);

        $this->respond(static function (): void {
            echo self::PAGE;
        });

        $this->assertFalse($this->stored());
    }

    public function test_donotcachepage_defined_false_still_stores(): void
    {
        define('DONOTCACHEPAGE', false);

        $this->respond(static function (): void {
            echo self::PAGE;
        });

        $this->assertTrue($this->stored());
    }

    // ---------------------------------------------------------------------
    // Item: Bypass URLs and the enabled flag are read again at store time.
    // ---------------------------------------------------------------------

    /**
     * A Bypass URL added while the request ran (the purge that comes with it
     * has already happened) stops the page being stored.
     */
    public function test_a_bypass_url_added_while_the_request_ran_stops_the_store(): void
    {
        $this->respond(function (): void {
            echo self::PAGE;
            $this->optionStore[CacheManager::OPTION_CONFIG] = ['enabled' => true, 'bypass_urls' => ['/about']];
        });

        $this->assertFalse($this->stored());
    }

    public function test_a_cache_switched_off_while_the_request_ran_stores_nothing(): void
    {
        $this->respond(function (): void {
            echo self::PAGE;
            $this->optionStore[CacheManager::OPTION_CONFIG] = ['enabled' => false];
        });

        $this->assertFalse($this->stored());
    }

    /**
     * A push that switches the cache off leaves the drop-in and the server
     * rules in place, and they would keep serving what is stored, so the push
     * clears it.
     */
    public function test_a_push_that_switches_the_cache_off_clears_it(): void
    {
        @mkdir(dirname($this->root . '/' . self::KEY), 0o777, true);
        file_put_contents($this->root . '/' . self::KEY, gzencode(self::PAGE));

        (new CacheManager())->applyConfig(['enabled' => false]);

        $this->assertFalse($this->stored());
    }

    /**
     * And a push that leaves an already-disabled cache disabled does not purge
     * (a purge also clears host and edge caches).
     */
    public function test_a_push_to_an_already_disabled_cache_does_not_purge(): void
    {
        $this->optionStore[CacheManager::OPTION_CONFIG] = ['enabled' => false];
        Monkey\Actions\expectDone('wpmgr_purge_everything:before')->never();

        (new CacheManager())->applyConfig(['enabled' => false, 'cache_mobile' => true]);

        $this->assertTrue(true); // the expectation above is the assertion
    }
}
