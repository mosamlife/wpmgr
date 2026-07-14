<?php
/**
 * MediaModalInjector::renderMetaBox() escape-late regression tests (2026-07
 * wp.org review fix).
 *
 * renderMetaBox() used to `echo` StatsRenderer's assembled HTML directly,
 * with a bare `// phpcs:ignore WordPress.Security.EscapeOutput` (missing the
 * exact `.OutputNotEscaped` sniff code) relying entirely on StatsRenderer
 * having esc_html()'d each dynamic value at assembly time (escape-early). It
 * now wraps that same echo in `wp_kses($html, self::allowedStatsKses())` --
 * the visible escape-at-output-boundary pass -- while StatsRenderer's own
 * esc_html() calls remain as belt-and-suspenders.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Media\StatsRenderer;
use WPMgr\Agent\MediaKeystore;
use WPMgr\Agent\Webhooks\MediaModalInjector;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Webhooks\MediaModalInjector
 */
final class MediaModalInjectorTest extends TestCase
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

    private function makeInjectorWithBlob(array $blob): MediaModalInjector
    {
        Functions\when('get_post_meta')->justReturn($blob);
        $renderer = new StatsRenderer(new MediaKeystore());

        return new MediaModalInjector($renderer);
    }

    private function makeAttachmentPost(int $id, string $mime = 'image/jpeg'): \stdClass
    {
        $post                  = new \stdClass();
        $post->ID              = $id;
        $post->post_mime_type  = $mime;
        return $post;
    }

    /**
     * @return array<string,array<string,bool>>
     */
    private function allowedStatsKses(): array
    {
        $ref = new \ReflectionMethod(MediaModalInjector::class, 'allowedStatsKses');
        return $ref->invoke(null);
    }

    public function test_render_meta_box_passes_output_through_wp_kses_with_the_explicit_allowlist(): void
    {
        $blob = [
            'status'          => 'optimized',
            'sizes_optimized' => ['full'],
            'original_data'   => ['file' => '2026/05/banner.jpg', 'filesize' => 200000, 'sizes' => []],
        ];
        $injector = $this->makeInjectorWithBlob($blob);

        $captured = [];
        Functions\when('wp_kses')->alias(function ($html, $allowed = []) use (&$captured) {
            $captured[] = ['html' => $html, 'allowed' => $allowed];
            return $html;
        });

        ob_start();
        $injector->renderMetaBox($this->makeAttachmentPost(1));
        $out = (string) ob_get_clean();

        $this->assertCount(1, $captured, 'renderMetaBox() must call wp_kses() exactly once');
        $this->assertSame(
            $this->allowedStatsKses(),
            $captured[0]['allowed'],
            'renderMetaBox() must pass the module\'s own explicit allowlist'
        );
        $this->assertStringContainsString('wpmgr-media-stats', $out);
    }

    public function test_allowed_stats_kses_is_narrow_and_has_no_interactive_elements(): void
    {
        $allowed = $this->allowedStatsKses();

        // This panel is read-only (no forms/inputs, unlike the 2FA module).
        $this->assertArrayNotHasKey('form', $allowed);
        $this->assertArrayNotHasKey('input', $allowed);
        $this->assertArrayNotHasKey('script', $allowed);

        $this->assertArrayHasKey('div', $allowed);
        $this->assertArrayHasKey('span', $allowed);
        $this->assertArrayHasKey('strong', $allowed);
        $this->assertArrayHasKey('code', $allowed);
        $this->assertArrayHasKey('class', $allowed['div']);
        $this->assertArrayHasKey('class', $allowed['span']);
    }

    public function test_render_meta_box_output_is_fully_covered_by_allowlist(): void
    {
        // Exercise the "sizes not optimized" branch too, for full tag coverage.
        $blob = [
            'status'            => 'optimized',
            'sizes_optimized'   => ['full'],
            'sizes_unoptimized' => ['large' => 'too big'],
            'original_data'     => [
                'file'     => '2026/05/banner.jpg',
                'filesize' => 200000,
                'sizes'    => ['large' => ['width' => 1024, 'height' => 768, 'filesize' => 50000]],
            ],
        ];
        $injector = $this->makeInjectorWithBlob($blob);

        Functions\when('wp_kses')->returnArg();

        ob_start();
        $injector->renderMetaBox($this->makeAttachmentPost(1));
        $html = (string) ob_get_clean();

        $this->assertNotSame('', $html);

        $doc = new \DOMDocument();
        libxml_use_internal_errors(true);
        $doc->loadHTML('<!DOCTYPE html><html><body>' . $html . '</body></html>', LIBXML_NOERROR | LIBXML_NOWARNING);
        libxml_clear_errors();

        $allowed = $this->allowedStatsKses();
        $body    = $doc->getElementsByTagName('body')->item(0);
        $this->assertNotNull($body);

        $seenAnyTag = false;
        $walker     = function (\DOMNode $node) use (&$walker, &$seenAnyTag, $allowed): void {
            if ($node instanceof \DOMElement) {
                $tag        = strtolower($node->tagName);
                $seenAnyTag = true;
                $this->assertArrayHasKey($tag, $allowed, "tag <$tag> is emitted but missing from allowedStatsKses()");
                foreach ($node->attributes as $attr) {
                    $attrLow = strtolower($attr->name);
                    $this->assertArrayHasKey(
                        $attrLow,
                        $allowed[$tag],
                        "attribute \"$attrLow\" on <$tag> is emitted but missing from allowedStatsKses()['$tag']"
                    );
                }
            }
            foreach ($node->childNodes as $child) {
                $walker($child);
            }
        };
        foreach ($body->childNodes as $child) {
            $walker($child);
        }

        $this->assertTrue($seenAnyTag);
    }

    public function test_render_meta_box_returns_early_for_invalid_post(): void
    {
        $injector = $this->makeInjectorWithBlob([]);

        $ranWpKses = false;
        Functions\when('wp_kses')->alias(function ($html, $allowed = []) use (&$ranWpKses) {
            $ranWpKses = true;
            return $html;
        });

        ob_start();
        $injector->renderMetaBox(new \stdClass());
        $out = (string) ob_get_clean();

        $this->assertSame('', $out);
        $this->assertFalse($ranWpKses, 'must never call wp_kses() when there is no valid attachment id');
    }
}
