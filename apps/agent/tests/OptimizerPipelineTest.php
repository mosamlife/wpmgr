<?php
/**
 * Pipeline integration tests: the Optimizer orchestrator runs the enabled stages
 * in order and is a no-op when inert / on non-HTML; the cache writer runs the
 * optimizer on a cacheable MISS so the OPTIMIZED bytes are what get gzipped +
 * written + served; and the DbCleanCommand handler returns the engine's counts.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Cache\CacheConfig;
use WPMgr\Agent\Cache\CacheWriter;
use WPMgr\Agent\Commands\DbCleanCommand;
use WPMgr\Agent\Optimizer\DbCleanup;
use WPMgr\Agent\Optimizer\Optimizer;
use WPMgr\Agent\Optimizer\PerfConfig;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Optimizer\Optimizer
 * @covers \WPMgr\Agent\Cache\CacheWriter
 * @covers \WPMgr\Agent\Commands\DbCleanCommand
 */
final class OptimizerPipelineTest extends TestCase
{
    private const DOC = '<!DOCTYPE html><html><head></head><body>'
        . '<iframe src="https://www.youtube.com/embed/abcdefg"></iframe>'
        . '<img src="/lazy1.jpg"><img src="/lazy2.jpg"><img src="/lazy3.jpg">'
        . '</body></html>';

    private string $root = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->root = sys_get_temp_dir() . '/wpmgr-opt-' . uniqid('', true) . '/cache/wpmgr';
        // ImagesHtml builds a UrlHelper that reads site_url(); stub it so the
        // pipeline runs cleanly under unit tests.
        Functions\when('site_url')->justReturn('https://example.com');
        Functions\when('home_url')->justReturn('https://example.com');
        Functions\when('get_option')->justReturn([]);
    }

    protected function tear_down(): void
    {
        $base = dirname(dirname($this->root));
        $this->rrmdir($base);
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
            is_dir($p) ? $this->rrmdir($p) : @unlink($p);
        }
        @rmdir($dir);
    }

    public function test_orchestrator_is_noop_when_no_flag_enabled(): void
    {
        $opt = new Optimizer(new PerfConfig([]));
        $this->assertFalse($opt->isActive());
        $this->assertSame(self::DOC, $opt->run(self::DOC));
    }

    public function test_orchestrator_is_noop_on_non_html(): void
    {
        $opt = new Optimizer(new PerfConfig(['lazy_load' => true]));
        $json = '{"not":"html"}';
        $this->assertSame($json, $opt->run($json));
    }

    public function test_orchestrator_runs_enabled_stages(): void
    {
        $opt = new Optimizer(new PerfConfig([
            'lazy_load'           => true,
            'youtube_placeholder' => true,
            'cache_link_prefetch' => true,
        ]));
        $this->assertTrue($opt->isActive());
        $out = $opt->run(self::DOC);

        // Images lazified, YouTube facaded, speculation rules injected.
        $this->assertStringContainsString('loading=', $out);
        $this->assertStringContainsString('wpmgr-yt', $out);
        $this->assertStringNotContainsString('<iframe', $out);
        $this->assertStringContainsString('speculationrules', $out);
    }

    public function test_cache_writer_writes_optimized_bytes_on_miss(): void
    {
        $config = new CacheConfig(['enabled' => true]);
        // Inject an optimizer that uppercases a marker so we can prove the
        // OPTIMIZED bytes are what get cached + returned.
        $optimizer = new Optimizer(new PerfConfig(['lazy_load' => true, 'youtube_placeholder' => true]));
        $writer = new CacheWriter($config, $this->root, 'example.com/post/index.html.gz', null, $optimizer);

        // Simulate the request superglobals the writer's resolveContext reads.
        $_SERVER['REQUEST_URI']    = '/post/';
        $_SERVER['HTTP_HOST']      = 'example.com';
        $_SERVER['REQUEST_METHOD'] = 'GET';
        $_SERVER['HTTP_USER_AGENT'] = 'Mozilla/5.0';
        $_GET = [];
        $_COOKIE = [];

        $returned = $writer->handle(self::DOC, PHP_OUTPUT_HANDLER_FINAL);

        // The live response was optimized (facade present, iframe gone).
        $this->assertStringContainsString('wpmgr-yt', $returned);
        $this->assertStringNotContainsString('<iframe', $returned);

        // The cached file holds the optimized bytes PLUS the footprint marker.
        // The live response ($returned) is the optimized HTML; the cached file
        // additionally contains the appended footprint comment.
        $path = $this->root . '/example.com/post/index.html.gz';
        $this->assertFileExists($path);
        $cached = (string) gzdecode((string) file_get_contents($path));
        $this->assertStringStartsWith($returned, $cached, 'cached bytes must start with the served optimized bytes');
        $this->assertStringContainsString(\WPMgr\Agent\Cache\CacheWriter::FOOTPRINT_MARKER, $cached,
            'cached bytes must include the footprint marker');
        $this->assertStringContainsString('(optimized)', $cached,
            'cached bytes for an optimized write must include the optimized suffix');

        unset($_SERVER['REQUEST_URI'], $_SERVER['HTTP_HOST'], $_SERVER['REQUEST_METHOD'], $_SERVER['HTTP_USER_AGENT']);
    }

    public function test_cache_writer_inactive_optimizer_caches_verbatim(): void
    {
        $config = new CacheConfig(['enabled' => true]);
        $optimizer = new Optimizer(new PerfConfig([])); // inactive
        $writer = new CacheWriter($config, $this->root, 'example.com/p2/index.html.gz', null, $optimizer);

        $_SERVER['REQUEST_URI']    = '/p2/';
        $_SERVER['HTTP_HOST']      = 'example.com';
        $_SERVER['REQUEST_METHOD'] = 'GET';
        $_SERVER['HTTP_USER_AGENT'] = 'Mozilla/5.0';
        $_GET = [];
        $_COOKIE = [];

        $returned = $writer->handle(self::DOC, PHP_OUTPUT_HANDLER_FINAL);
        $this->assertSame(self::DOC, $returned, 'inactive optimizer leaves the buffer verbatim');

        $path = $this->root . '/example.com/p2/index.html.gz';
        $this->assertFileExists($path);
        // The cached file has the footprint appended; the live response does not.
        $cached = (string) gzdecode((string) file_get_contents($path));
        $this->assertStringStartsWith(self::DOC, $cached,
            'cached bytes must start with the original body');
        $this->assertStringContainsString(\WPMgr\Agent\Cache\CacheWriter::FOOTPRINT_MARKER, $cached,
            'cached bytes must include the footprint marker');

        unset($_SERVER['REQUEST_URI'], $_SERVER['HTTP_HOST'], $_SERVER['REQUEST_METHOD'], $_SERVER['HTTP_USER_AGENT']);
    }

    public function test_db_clean_command_returns_ack(): void
    {
        // M38 contract: execute() returns the frozen ACK {ok, job_id} immediately;
        // the actual cleanup + progress POSTs run asynchronously via shutdown hook.
        $wpdb = new FakeCleanupWpdb();
        $wpdb->idResults = [1, 2];
        $engine = new DbCleanup(new PerfConfig(['db_post_revisions' => true]), $wpdb);

        $jobId = 'pipeline-test-job-id';
        $res   = (new DbCleanCommand($engine))->execute([], [
            'job_id' => $jobId,
            'tasks'  => ['revisions'],
        ]);

        $this->assertTrue($res['ok']);
        $this->assertSame($jobId, $res['job_id']);
        // The ACK carries NO result data — those come via progress pushes.
        $this->assertArrayNotHasKey('detail', $res);
        $this->assertArrayNotHasKey('cleaned', $res);
    }

    public function test_db_clean_command_rejects_missing_job_id(): void
    {
        $engine = new DbCleanup(new PerfConfig([]), null);
        $res    = (new DbCleanCommand($engine))->execute([], ['tasks' => ['revisions']]);
        $this->assertFalse($res['ok']);
        $this->assertArrayHasKey('detail', $res);
    }
}
