<?php
/**
 * StatsRenderer: HTML for never-optimized / optimized / partial / originals-
 * deleted states, with output-escaping assertions (no XSS).
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Media\StatsRenderer;
use WPMgr\Agent\MediaKeystore;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Media\StatsRenderer
 */
final class MediaStatsRendererTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('esc_html')->alias(static fn ($s) => htmlspecialchars((string) $s, ENT_QUOTES, 'UTF-8'));
        Functions\when('__')->alias(static fn ($s) => $s);
        Functions\when('size_format')->alias(static fn ($b) => round($b / 1024, 1) . ' KB');
        Functions\when('wp_get_attachment_metadata')->justReturn([]);
        Functions\when('get_post_mime_type')->justReturn('image/jpeg');
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Configure get_post_meta (read by the REAL MediaKeystore — which is final)
     * to return a fixed blob, then return a real keystore-backed renderer.
     */
    private function rendererFor(array $blob): StatsRenderer
    {
        Functions\when('get_post_meta')->justReturn($blob);

        return new StatsRenderer(new MediaKeystore());
    }

    public function test_never_optimized_shows_status_only(): void
    {
        $renderer = $this->rendererFor([]);
        $html      = $renderer->renderForAttachment(1, 'image/jpeg');

        $this->assertStringContainsString('Not Optimized yet', $html);
        $this->assertStringNotContainsString('Total Size', $html);
    }

    public function test_optimized_shows_before_after_and_saved_percent(): void
    {
        // before 200KB (full) ; live meta returns after (full 100KB).
        Functions\when('wp_get_attachment_metadata')->justReturn([
            'file'     => '2026/05/banner.avif',
            'filesize' => 100000,
            'sizes'    => [],
        ]);
        $blob = [
            'status'          => 'optimized',
            'sizes_optimized' => ['full'],
            'original_data'   => ['file' => '2026/05/banner.jpg', 'filesize' => 200000, 'sizes' => []],
        ];
        $renderer = $this->rendererFor($blob);
        $html      = $renderer->renderForAttachment(1, 'image/jpeg');

        $this->assertStringContainsString('Optimized', $html);
        $this->assertStringContainsString('Total Size', $html);
        $this->assertStringContainsString('&rarr;', $html);
        $this->assertStringContainsString('50% saved', $html); // (200-100)/200.
        $this->assertStringContainsString('Managed by WPMgr', $html);
    }

    public function test_partial_renders_unoptimized_reasons_escaped(): void
    {
        // A reason carrying an HTML/script payload MUST be escaped.
        $blob = [
            'status'            => 'optimized',
            'sizes_optimized'   => ['full'],
            'sizes_unoptimized' => ['large' => '<script>alert(1)</script> too big'],
            'original_data'     => [
                'file'     => '2026/05/banner.jpg',
                'filesize' => 100,
                'sizes'    => ['large' => ['width' => 1024, 'height' => 768, 'filesize' => 50]],
            ],
        ];
        Functions\when('wp_get_attachment_metadata')->justReturn(['file' => '2026/05/banner.avif', 'filesize' => 80, 'sizes' => []]);
        $renderer = $this->rendererFor($blob);
        $html      = $renderer->renderForAttachment(1, 'image/jpeg');

        $this->assertStringContainsString('Sizes not optimized', $html);
        $this->assertStringContainsString('1024x768', $html);
        // The XSS payload must be escaped (no raw <script>).
        $this->assertStringNotContainsString('<script>alert(1)</script>', $html);
        $this->assertStringContainsString('&lt;script&gt;', $html);
    }

    public function test_originals_deleted_state(): void
    {
        $blob = [
            'status'           => 'originals_deleted',
            'original_deleted' => 1,
            'sizes_optimized'  => ['full'],
            'original_data'    => ['file' => '2026/05/banner.jpg', 'filesize' => 100, 'sizes' => []],
        ];
        Functions\when('wp_get_attachment_metadata')->justReturn(['file' => '2026/05/banner.avif', 'filesize' => 80, 'sizes' => []]);
        $renderer = $this->rendererFor($blob);
        $html      = $renderer->renderForAttachment(1, 'image/jpeg');

        $this->assertStringContainsString('originals deleted', $html);
    }

    public function test_non_optimizable_mime_renders_nothing(): void
    {
        // Live + blob both point at a non-optimizable source (gif).
        Functions\when('get_post_mime_type')->justReturn('image/gif');
        $renderer = $this->rendererFor([]);
        $this->assertSame('', $renderer->renderForAttachment(1, 'image/gif'));
    }
}
