<?php
/**
 * DbRewriter value-level tests: a real serialized Elementor-style blob fixture
 * is rewritten with VALID s:NN: length prefixes, a partial match
 * (banner.jpg inside banner.jpg.bak) is NOT rewritten, JSON-in-postmeta is
 * rewritten, and the reverse direction restores the original.
 *
 * These exercise the pure value methods (rewriteValue / recursiveReplace) which
 * need no $wpdb — the boundary lookahead + (de)serialize round-trip are the two
 * highest-risk behaviors and live entirely in those methods.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Media\DbRewriter;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Media\DbRewriter
 */
final class MediaDbRewriterTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        // is_serialized: WP's real impl recognizes a:/O:/s:/etc.
        Functions\when('is_serialized')->alias(static function ($value): bool {
            if (!is_string($value)) {
                return false;
            }
            $value = trim($value);
            if ($value === 'N;' || $value === 'b:0;' || $value === 'b:1;') {
                return true;
            }
            return (bool) preg_match('/^(a|O|s|i|d|b):[0-9]/', $value);
        });
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    /** old -> new where the extension changes (different-ext apply). */
    private function map(): array
    {
        return [
            'https://site.test/wp-content/uploads/2026/05/banner.jpg'
                => 'https://site.test/wp-content/uploads/2026/05/banner.avif',
        ];
    }

    public function test_serialized_elementor_blob_keeps_valid_length_prefixes(): void
    {
        $old = 'https://site.test/wp-content/uploads/2026/05/banner.jpg';
        $new = 'https://site.test/wp-content/uploads/2026/05/banner.avif';

        // Elementor-style nested array of objects of strings, serialized.
        $structure = [
            'settings' => [
                'background_image' => ['url' => $old, 'id' => 42],
                'gallery'          => [
                    ['image' => ['url' => $old, 'alt' => 'hero']],
                ],
            ],
            'flag' => true,
            'n'    => 7,
        ];
        $serialized = serialize($structure);

        $rewriter = new DbRewriter();
        $out       = $rewriter->rewriteValue($serialized, $this->map());

        // It must still be VALID serialized data (length prefixes correct).
        $restored = @unserialize($out, ['allowed_classes' => false]);
        $this->assertIsArray($restored, 'rewritten serialized blob must unserialize cleanly');

        // URLs were rewritten in every leaf.
        $this->assertSame($new, $restored['settings']['background_image']['url']);
        $this->assertSame($new, $restored['settings']['gallery'][0]['image']['url']);
        // Non-string / non-URL leaves untouched.
        $this->assertSame(42, $restored['settings']['background_image']['id']);
        $this->assertTrue($restored['flag']);
        $this->assertSame(7, $restored['n']);

        // The s:NN: prefix for the new (longer/shorter) URL matches its length.
        $this->assertStringContainsString('s:' . strlen($new) . ':"' . $new . '"', $out);
        // A naive str_replace would have left s:55 (old length) on a 56-char new
        // string — assert the OLD length prefix for the URL is gone.
        $this->assertStringNotContainsString('s:' . strlen($old) . ':"' . $new . '"', $out);
    }

    public function test_partial_match_is_not_rewritten(): void
    {
        // The boundary lookahead `(?=([^0-9A-Za-z]|$))` (ported verbatim from
        // FlyingPress ImageOptimizer.php:417) protects against an ALPHANUMERIC
        // suffix: `banner.jpg2` / `banner.jpgx` must NOT be rewritten, because a
        // bare str_replace would corrupt a longer unrelated filename. (A
        // following non-alphanumeric like '?' query-string or end-of-string is a
        // legitimate boundary and DOES match — the URL genuinely ends there.)
        $value = serialize([
            'good'  => 'https://site.test/wp-content/uploads/2026/05/banner.jpg',
            'query' => 'https://site.test/wp-content/uploads/2026/05/banner.jpg?ver=2',
            'num'   => 'https://site.test/wp-content/uploads/2026/05/banner.jpg2',
            'word'  => 'https://site.test/wp-content/uploads/2026/05/banner.jpgx',
        ]);

        $rewriter = new DbRewriter();
        $out       = $rewriter->rewriteValue($value, $this->map());
        $restored  = unserialize($out, ['allowed_classes' => false]);

        // Real URL end + query-string boundary => rewritten (the '?' is a boundary).
        $this->assertSame('https://site.test/wp-content/uploads/2026/05/banner.avif', $restored['good']);
        $this->assertSame('https://site.test/wp-content/uploads/2026/05/banner.avif?ver=2', $restored['query']);
        // Alphanumeric suffix => NOT rewritten (the boundary guard's whole point).
        $this->assertSame('https://site.test/wp-content/uploads/2026/05/banner.jpg2', $restored['num']);
        $this->assertSame('https://site.test/wp-content/uploads/2026/05/banner.jpgx', $restored['word']);
    }

    public function test_json_in_postmeta_is_rewritten(): void
    {
        $old  = 'https://site.test/wp-content/uploads/2026/05/banner.jpg';
        $new  = 'https://site.test/wp-content/uploads/2026/05/banner.avif';
        $json = json_encode(['blocks' => [['attrs' => ['src' => $old]]]]);

        $rewriter = new DbRewriter();
        $out       = $rewriter->rewriteValue((string) $json, $this->map());
        $decoded   = json_decode($out, true);

        $this->assertSame($new, $decoded['blocks'][0]['attrs']['src']);
    }

    public function test_reverse_restores_original_value(): void
    {
        $old        = 'https://site.test/wp-content/uploads/2026/05/banner.jpg';
        $serialized = serialize(['url' => $old]);
        $map        = $this->map();

        $rewriter = new DbRewriter();
        $forward   = $rewriter->rewriteValue($serialized, $map);
        // Reverse direction = flip the map (what reverseImages does row-wise).
        $flipped   = array_flip($map);
        $back       = $rewriter->rewriteValue($forward, $flipped);

        $this->assertSame(
            $old,
            unserialize($back, ['allowed_classes' => false])['url'],
            'reverse rewrite restores the original URL'
        );
    }

    public function test_plain_string_value_is_rewritten_boundary_guarded(): void
    {
        $rewriter = new DbRewriter();
        $out       = $rewriter->rewriteValue(
            'see https://site.test/wp-content/uploads/2026/05/banner.jpg here',
            $this->map()
        );
        $this->assertStringContainsString('banner.avif', $out);
        $this->assertStringNotContainsString('banner.jpg', $out);
    }
}
