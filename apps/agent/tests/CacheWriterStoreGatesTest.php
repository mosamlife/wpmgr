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
 *  - a response that is not HTML, or carries Content-Disposition, is not
 *    stored; a response that reaches one chunk is passed through as it is
 *    produced, so it is never held in memory in full, and is not stored;
 *  - the chunk is an eighth of the memory the request has left, between 512 KB
 *    and 4 MB (4 MB when memory_limit is -1), and a page smaller than the chunk
 *    is stored whatever the chunk is;
 *  - output is never held back across chunks, so a plugin that takes the
 *    buffer (ob_get_clean) loses nothing that was already on its way;
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
use WPMgr\Agent\Cache\CacheConfig;
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

    /** The smallest chunk the capture buffer opens with: 512 KB. */
    private const FLOOR = 524288;

    /** The largest chunk the capture buffer opens with: 4 MB. */
    private const CEILING = 4194304;

    /** Memory left when the buffer opens that is tight: 3 MB, an eighth of which is under the floor. */
    private const TIGHT = 3145728;

    /** Memory left when the buffer opens that is plentiful: 1 GB. */
    private const PLENTY = 1073741824;

    /** The memory_limit the tests run under, wide enough for a 64 MB download. */
    private const ROOM = '1024M';

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

        // The chunk is sized from the memory left when the buffer opens, so a
        // test about what is stored must not depend on how much the process
        // running it has used: every test starts with room for a 4 MB chunk.
        ini_set('memory_limit', self::ROOM);

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
     * @param callable                    $render Produces the response; may open or close buffers.
     * @param callable(CacheManager):void|null $open   Opens the capture buffer; startBuffer() when omitted.
     */
    private function respond(callable $render, ?callable $open = null): string
    {
        $manager = new CacheManager();
        ob_start();
        $levelsBefore = ob_get_level();
        $open === null ? $manager->startBuffer() : $open($manager);
        $render();
        while (ob_get_level() > $levelsBefore) {
            ob_end_flush();
        }
        return (string) ob_get_clean();
    }

    /**
     * Open the capture buffer through CacheManager::startBuffer(), as init
     * does, in a request that has the given amount of memory left, and return
     * the chunk size it opened with. The request gets its room back as soon as
     * the buffer is open: the chunk is chosen when the buffer opens, and what
     * the limit is afterwards does not change it.
     *
     * @param CacheManager    $manager The manager whose buffer opens.
     * @param int|string|null $left    Bytes left under memory_limit when the buffer
     *                                 opens; a string is memory_limit as it is
     *                                 written in php.ini ("1G"); null is -1.
     */
    private function openCapture(CacheManager $manager, int|string|null $left): int
    {
        // Load everything startBuffer() touches while the limit is still wide,
        // so the window below holds the buffer opening and nothing else.
        $warm = new CacheManager();
        $warm->startBuffer();
        ob_end_clean();

        if ($left === null) {
            $limit = '-1';
        } elseif (is_string($left)) {
            $limit = $left;
        } else {
            $limit = (string) (memory_get_usage(true) + $left);
        }

        ini_set('memory_limit', $limit);
        try {
            $manager->startBuffer();
        } finally {
            ini_set('memory_limit', self::ROOM);
        }

        return (int) ob_get_status()['chunk_size'];
    }

    /**
     * The chunk size the capture buffer opens with in a request that has the
     * given amount of memory left (see openCapture()), with the buffer closed
     * again.
     *
     * @param int|string|null $left Bytes left, memory_limit as written, or null for -1.
     */
    private function chunkOpenedWith(int|string|null $left): int
    {
        $levelBefore = ob_get_level();
        $chunk       = $this->openCapture(new CacheManager(), $left);
        $this->assertSame($levelBefore + 1, ob_get_level(), 'the capture buffer must have opened');
        ob_end_clean();

        return $chunk;
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
     * capture stays within a few chunks instead of the whole file, every byte
     * reaches the visitor, and nothing is stored.
     */
    public function test_a_download_streamed_on_init_is_not_held_in_memory(): void
    {
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

        $this->assertLessThan(3 * self::CEILING, $held, sprintf('the capture held %.1f MB of a 64 MB download', $held / 1048576));
        $this->assertSame(64 << 20, $sent, 'every byte must reach the visitor');
        $this->assertFalse($this->stored());
    }

    /**
     * After the template stage, a response that is not HTML is passed through
     * as soon as it outgrows one chunk.
     */
    public function test_a_non_html_response_after_the_template_stage_is_passed_through(): void
    {
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

        $this->assertLessThan(3 * self::CEILING, $held, sprintf('the capture held %.1f MB of a non-HTML response', $held / 1048576));
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
     * A large page, over a megabyte and under one chunk, reaches the final
     * flush in one piece: it is delivered whole and stored whole.
     */
    public function test_a_large_page_under_one_chunk_is_stored_whole(): void
    {
        $body = '<!DOCTYPE html><html><body>' . str_repeat('<p>large page</p>', 200000) . '</body></html>';
        $this->assertGreaterThan(1 << 20, strlen($body), 'the page must be larger than a megabyte');
        $this->assertLessThan(self::CEILING, strlen($body));

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
     * A page of one chunk or more is passed through as it is produced and not
     * stored, and the visitor still receives all of it, in order. The largest
     * page that is stored is one byte short of a chunk, whichever chunk the
     * memory the request has left gave the buffer.
     *
     * @dataProvider provide_page_sizes_around_one_chunk
     *
     * @param int  $left       Bytes of memory left when the buffer opens.
     * @param int  $chunk      The chunk the buffer opens with then.
     * @param int  $size       Page size in bytes.
     * @param bool $stored     Whether a page of that size is stored.
     * @param int  $pieceBytes Size of the pieces the page is echoed in; 0 echoes it in one piece.
     */
    public function test_only_a_page_smaller_than_one_chunk_is_stored(int $left, int $chunk, int $size, bool $stored, int $pieceBytes): void
    {
        $head = '<!DOCTYPE html><html><body>';
        $tail = '</body></html>';
        $body = $head . str_repeat('x', $size - strlen($head) - strlen($tail)) . $tail;
        $this->assertSame($size, strlen($body));

        $opened  = 0;
        $visitor = $this->respond(
            static function () use ($body, $pieceBytes): void {
                self::echoPage($body, $pieceBytes);
            },
            function (CacheManager $manager) use ($left, &$opened): void {
                $opened = $this->openCapture($manager, $left);
            }
        );

        $this->assertSame($chunk, $opened, 'the buffer must open with the chunk that this much free memory gives');
        $this->assertSame(strlen($body), strlen($visitor));
        $this->assertSame(md5($body), md5($visitor), 'the visitor must receive every byte, in order');
        $this->assertSame($stored, $this->stored());
    }

    /**
     * Page sizes around the chunk a request gets, for a request with plenty of
     * memory left (a 4 MB chunk) and for one with little (the 512 KB floor), each
     * echoed in pieces and in one echo.
     *
     * @return array<string,array{0:int,1:int,2:int,3:bool,4:int}>
     */
    public static function provide_page_sizes_around_one_chunk(): array
    {
        $requests = [
            'plenty of memory left' => [self::PLENTY, self::CEILING],
            'little memory left'    => [self::TIGHT, self::FLOOR],
        ];

        $cases = [];
        foreach ($requests as $request => [$left, $chunk]) {
            $sizes = [
                'one byte short of a chunk' => [$chunk - 1, true],
                'exactly one chunk'         => [$chunk, false],
                'two chunks and a bit'      => [(2 * $chunk) + 4097, false],
            ];
            foreach ($sizes as $label => [$size, $stored]) {
                foreach (self::provide_how_a_page_is_echoed() as $shape => [$pieceBytes]) {
                    $cases[$request . ', ' . $label . ', ' . $shape] = [$left, $chunk, $size, $stored, $pieceBytes];
                }
            }
        }

        return $cases;
    }

    /**
     * The two shapes a template's output takes: many pieces, as a theme
     * renders a page, and a single echo of the whole page.
     *
     * @return array<string,array{0:int}> Piece size in bytes; 0 is one piece.
     */
    public static function provide_how_a_page_is_echoed(): array
    {
        return [
            'in 64 KB pieces' => [65536],
            'in one echo'     => [0],
        ];
    }

    /**
     * Echo a page in pieces of the given size, or whole when the size is 0.
     *
     * @param string $page       The page.
     * @param int    $pieceBytes Piece size in bytes; 0 echoes the page in one piece.
     * @return void
     */
    private static function echoPage(string $page, int $pieceBytes): void
    {
        foreach (str_split($page, $pieceBytes > 0 ? $pieceBytes : max(1, strlen($page))) as $piece) {
            echo $piece;
        }
    }

    /**
     * Output is never held across chunk boundaries: whatever the buffer hands
     * over before the final flush is returned whole, for any response. A
     * plugin that takes the buffer with ob_get_clean() therefore discards
     * nothing that was already on its way to the visitor.
     */
    public function test_a_chunk_is_returned_whole_and_nothing_is_held(): void
    {
        $writer = new CacheWriter(new CacheConfig(['enabled' => true]), $this->root, self::KEY);
        $chunk  = '<!DOCTYPE html><html><body>' . str_repeat('x', 4096);

        $this->assertSame($chunk, $writer->handle($chunk, PHP_OUTPUT_HANDLER_START), 'the first chunk must be returned whole');
        $this->assertSame('second', $writer->handle('second', PHP_OUTPUT_HANDLER_WRITE));
        $this->assertSame('rest</body></html>', $writer->handle('rest</body></html>', PHP_OUTPUT_HANDLER_FINAL));
        $this->assertFalse($this->stored(), 'a response that was handed on in chunks is not a page that is stored');
    }

    /**
     * A plugin that opened its own buffer before init takes the top buffer,
     * which is the capture, with ob_get_clean() at shutdown and sends the page
     * on. The visitor receives the page whole, and a page the plugin took is
     * not stored. The page here is larger than a megabyte and smaller than one
     * chunk.
     *
     * @dataProvider provide_how_a_page_is_echoed
     *
     * @param int $pieceBytes Size of the pieces the page is echoed in; 0 echoes it in one piece.
     */
    public function test_a_page_a_plugin_takes_from_the_capture_buffer_is_delivered_whole(int $pieceBytes): void
    {
        $page = '<!DOCTYPE html><html><body>' . str_repeat('<p>row</p>', 220000) . '<span>END-MARKER</span></body></html>';
        $this->assertGreaterThan(1 << 20, strlen($page));
        $this->assertLessThan(self::CEILING, strlen($page));

        $visitor = $this->respondWithAPluginTakingTheTopBuffer(
            $page,
            $pieceBytes,
            static fn (string $taken): string => strtoupper(substr($taken, 0, 15)) . substr($taken, 15)
        );

        $this->assertSame(strtoupper(substr($page, 0, 15)) . substr($page, 15), $visitor, 'the page the plugin rewrote must reach the visitor whole');
        $this->assertFalse($this->stored(), 'a page the plugin took from the buffer must not be stored');
    }

    /**
     * The same takeover on a page of more than one chunk: the part already
     * handed on and the part the plugin takes arrive in order, every byte of
     * them, and the page is not stored.
     *
     * @dataProvider provide_how_a_page_is_echoed
     *
     * @param int $pieceBytes Size of the pieces the page is echoed in; 0 echoes it in one piece.
     */
    public function test_a_page_over_one_chunk_a_plugin_takes_from_the_capture_buffer_is_delivered_whole(int $pieceBytes): void
    {
        $page = '<!DOCTYPE html><html><body>' . str_repeat('<p>row</p>', 520000) . '<span>END-MARKER</span></body></html>';
        $this->assertGreaterThan(self::CEILING, strlen($page));

        $visitor = $this->respondWithAPluginTakingTheTopBuffer($page, $pieceBytes, static fn (string $taken): string => $taken);

        $this->assertSame(strlen($page), strlen($visitor));
        $this->assertSame(md5($page), md5($visitor), 'the visitor must receive every byte, in order');
        $this->assertFalse($this->stored());
    }

    /**
     * Render a page through the capture buffer with a plugin's own buffer below
     * it, the way a plugin that opened a buffer before init leaves things: at
     * shutdown the plugin calls ob_get_clean(), which takes the top buffer (the
     * capture), and sends on what it got, after $rewrite.
     *
     * @param string                  $page       The page.
     * @param int                     $pieceBytes Size of the pieces the page is echoed in; 0 echoes it in one piece.
     * @param callable(string):string $rewrite    What the plugin sends on for what it took.
     */
    private function respondWithAPluginTakingTheTopBuffer(string $page, int $pieceBytes, callable $rewrite): string
    {
        $manager = new CacheManager();
        ob_start();                       // the network
        $network = ob_get_level();
        ob_start();                       // the plugin's buffer, opened before init
        $manager->startBuffer();          // the capture, on top from init on
        self::echoPage($page, $pieceBytes); // the template renders
        $taken = (string) ob_get_clean(); // the plugin, at shutdown
        echo $rewrite($taken);
        while (ob_get_level() > $network) {
            ob_end_flush();
        }

        return (string) ob_get_clean();
    }

    // ---------------------------------------------------------------------
    // Item: the chunk is sized from the memory the request has left.
    // ---------------------------------------------------------------------

    /**
     * PHP reserves the whole chunk against memory_limit when the buffer opens.
     * The chunk is an eighth of the memory the request has left, at least
     * 512 KB and at most 4 MB, and 4 MB when there is no limit.
     *
     * @dataProvider provide_memory_and_the_chunk_it_gives
     *
     * @param int $limit Parsed memory_limit in bytes; -1 is no limit.
     * @param int $used  Memory in use when the buffer opens.
     * @param int $chunk The chunk size the buffer opens with.
     */
    public function test_the_chunk_is_an_eighth_of_the_memory_left_between_512_kb_and_4_mb(int $limit, int $used, int $chunk): void
    {
        $this->assertSame($chunk, CacheWriter::chunkBytesFor($limit, $used));
    }

    /**
     * @return array<string,array{0:int,1:int,2:int}>
     */
    public static function provide_memory_and_the_chunk_it_gives(): array
    {
        $mb = 1048576;

        return [
            'nothing left'                           => [40 * $mb, 40 * $mb, self::FLOOR],
            'more in use than the limit'             => [40 * $mb, 41 * $mb, self::FLOOR],
            'a limit that could not be read'         => [0, 20 * $mb, self::FLOOR],
            '3 MB left'                              => [40 * $mb, 37 * $mb, self::FLOOR],
            '4 MB left: an eighth is the floor'      => [40 * $mb, 36 * $mb, self::FLOOR],
            '4 MB and 8 bytes left'                  => [(40 * $mb) + 8, 36 * $mb, self::FLOOR + 1],
            '8 MB left'                              => [40 * $mb, 32 * $mb, $mb],
            '16 MB left'                             => [40 * $mb, 24 * $mb, 2 * $mb],
            '32 MB left: an eighth is the ceiling'   => [64 * $mb, 32 * $mb, self::CEILING],
            '32 MB less 8 bytes left'                => [(64 * $mb) - 8, 32 * $mb, self::CEILING - 1],
            '33 MB left'                             => [65 * $mb, 32 * $mb, self::CEILING],
            '1 GB left'                              => [1024 * $mb, 24 * $mb, self::CEILING],
            'no limit'                               => [-1, 24 * $mb, self::CEILING],
            'no limit, a lot in use'                 => [-1, 900 * $mb, self::CEILING],
            'the largest limit there is'             => [PHP_INT_MAX, 24 * $mb, self::CEILING],
        ];
    }

    /**
     * A request with little memory left opens the smallest chunk, 512 KB, so
     * that the buffer does not take what the page needs to render. Through
     * the same code path init uses.
     */
    public function test_a_request_with_little_free_memory_opens_the_512_kb_chunk(): void
    {
        $this->assertSame(self::FLOOR, $this->chunkOpenedWith(self::TIGHT));
    }

    /**
     * One with plenty opens the largest, 4 MB.
     */
    public function test_a_request_with_plenty_of_free_memory_opens_the_4_mb_chunk(): void
    {
        $this->assertSame(self::CEILING, $this->chunkOpenedWith(self::PLENTY));
    }

    /**
     * Between the two the chunk follows the memory left: an eighth of 16 MB is
     * 2 MB. The buffer itself is not counted in what is left, and neither is
     * anything a few allocations between the limit being set and the buffer
     * opening may take, hence the margin.
     */
    public function test_a_request_with_some_free_memory_opens_a_chunk_of_an_eighth_of_it(): void
    {
        $this->assertEqualsWithDelta(2097152, $this->chunkOpenedWith(16 * 1048576), 307200);
    }

    /**
     * memory_limit -1 means there is no limit to be near: 4 MB.
     */
    public function test_a_request_without_a_memory_limit_opens_the_4_mb_chunk(): void
    {
        $this->assertSame(self::CEILING, $this->chunkOpenedWith(null));
    }

    /**
     * memory_limit is read the way WordPress reads it, so a limit written with
     * a unit is the number of bytes it says: each of these is 1 GB.
     *
     * @dataProvider provide_a_gigabyte_written_three_ways
     *
     * @param string $limit memory_limit as php.ini writes it.
     */
    public function test_a_memory_limit_written_with_a_unit_is_read_the_way_wordpress_reads_it(string $limit): void
    {
        $this->assertSame(self::CEILING, $this->chunkOpenedWith($limit));
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function provide_a_gigabyte_written_three_ways(): array
    {
        return [
            'in gigabytes' => ['1G'],
            'in megabytes' => ['1024M'],
            'in kilobytes' => ['1048576K'],
        ];
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
