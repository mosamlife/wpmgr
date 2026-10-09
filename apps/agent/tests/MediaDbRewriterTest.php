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
    private bool $hadWpdb = false;

    /** @var mixed */
    private $savedWpdb = null;

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
        // Honour the flags, as WordPress does, so a test sees what an encode
        // with or without them really produces.
        Functions\when('wp_json_encode')->alias(
            static fn ($d, $flags = 0, $depth = 512) => json_encode($d, (int) $flags, (int) $depth)
        );
        $this->hadWpdb   = array_key_exists('wpdb', $GLOBALS);
        $this->savedWpdb = $GLOBALS['wpdb'] ?? null;
    }

    protected function tear_down(): void
    {
        if ($this->hadWpdb) {
            $GLOBALS['wpdb'] = $this->savedWpdb;
        } else {
            unset($GLOBALS['wpdb']);
        }
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
        // The boundary lookahead `(?=([^0-9A-Za-z]|$))` (analysis doc line 417)
        // protects against an ALPHANUMERIC
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

    // -----------------------------------------------------------------------
    // Byte preservation: a rewrite changes the URL bytes and nothing else.
    // -----------------------------------------------------------------------

    public function test_json_without_a_mapped_url_keeps_its_bytes(): void
    {
        // Unescaped slashes and raw UTF-8, as a builder that encodes with
        // JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE stores them.
        $json = '{"title":"Café","link":"https://site.test/about/","settings":{}}';

        $out = (new DbRewriter())->rewriteValue($json, $this->map());

        $this->assertSame($json, $out, 'a JSON value with no mapped URL is returned byte for byte');
    }

    public function test_json_rewrite_changes_only_the_url_bytes(): void
    {
        $json = '{"src":"https://ex.test/a.jpg","t":"é"}';

        $out = (new DbRewriter())->rewriteValue($json, ['https://ex.test/a.jpg' => 'https://ex.test/a.avif']);

        $this->assertSame('{"src":"https://ex.test/a.avif","t":"é"}', $out);
    }

    public function test_json_rewrite_keeps_escaped_slashes_and_unicode_escapes(): void
    {
        // A document stored with default flags: escaped slashes, and a Unicode
        // escape (backslash, "u", four hex digits) for the e-acute.
        $eAcute = chr(92) . 'u00e9';
        $json   = '{"src":"https:\/\/ex.test\/a.jpg","t":"' . $eAcute . '","n":1.50}';
        $this->assertSame('é', json_decode($json, true)['t'], 'fixture holds a Unicode escape');

        $out = (new DbRewriter())->rewriteValue($json, ['https://ex.test/a.jpg' => 'https://ex.test/a.avif']);

        $this->assertSame('{"src":"https:\/\/ex.test\/a.avif","t":"' . $eAcute . '","n":1.50}', $out);
    }

    public function test_json_whose_only_near_match_is_not_a_url_keeps_its_bytes(): void
    {
        // Decoded, the URL runs straight into a letter (an escaped "A"), so the
        // boundary guard rewrites nothing; in the stored text the same URL is
        // followed by a backslash. Nothing is rewritten, so nothing changes.
        $json = '{"b":"https://ex.test/a.jpg' . chr(92) . 'u0041","n":1.50}';
        $this->assertSame('https://ex.test/a.jpgA', json_decode($json, true)['b'], 'fixture holds an escaped A');

        $out = (new DbRewriter())->rewriteValue($json, ['https://ex.test/a.jpg' => 'https://ex.test/a.avif']);

        $this->assertSame($json, $out);
    }

    public function test_json_rewrite_of_both_url_forms_in_one_document(): void
    {
        $json = '{"a":"https://ex.test/a.jpg","b":"https:\/\/ex.test\/a.jpg","c":"https://ex.test/a.jpg2"}';

        $out = (new DbRewriter())->rewriteValue($json, ['https://ex.test/a.jpg' => 'https://ex.test/a.avif']);

        $this->assertSame(
            '{"a":"https://ex.test/a.avif","b":"https:\/\/ex.test\/a.avif","c":"https://ex.test/a.jpg2"}',
            $out
        );
    }

    public function test_json_rewrite_that_cannot_be_made_in_place_keeps_the_documents_style(): void
    {
        // The URL is followed by an escaped "A": decoded, the URL runs straight
        // into a letter, so the boundary guard leaves that occurrence alone, but
        // in the stored text it is followed by a backslash. An in-place edit
        // would rewrite it, so the rewriter re-encodes instead, in the style of
        // the original: unescaped slashes, raw UTF-8.
        $json = '{"a":"https://ex.test/a.jpg","b":"https://ex.test/a.jpg' . chr(92) . 'u0041","t":"é"}';
        $this->assertSame('https://ex.test/a.jpgA', json_decode($json, true)['b'], 'fixture holds an escaped A');

        $out = (new DbRewriter())->rewriteValue($json, ['https://ex.test/a.jpg' => 'https://ex.test/a.avif']);

        $this->assertSame(
            ['a' => 'https://ex.test/a.avif', 'b' => 'https://ex.test/a.jpgA', 't' => 'é'],
            json_decode($out, true)
        );
        $this->assertStringNotContainsString('\/', $out, 'unescaped slashes stay unescaped');
        $this->assertStringContainsString('"t":"é"', $out, 'raw UTF-8 stays raw');
    }

    public function test_serialized_value_without_a_mapped_url_keeps_its_bytes(): void
    {
        // A float stored at 17 significant digits: unserialize() then
        // serialize() would write it back as d:0.1, a different byte string.
        $url  = 'https://site.test/wp-content/uploads/2026/05/other.jpg';
        $blob = 'a:2:{s:5:"ratio";d:0.10000000000000001;s:3:"img";s:' . strlen($url) . ':"' . $url . '";}';
        $this->assertIsArray(unserialize($blob, ['allowed_classes' => false]), 'fixture is valid serialized data');

        $out = (new DbRewriter())->rewriteValue($blob, $this->map());

        $this->assertSame($blob, $out, 'a serialized value with no mapped URL is returned byte for byte');
    }

    public function test_postmeta_row_the_prefilter_selected_but_no_url_matches_is_not_written(): void
    {
        $wpdb = new FakeDbRewriterWpdb();
        // MySQL selects this row because its LIKE and REGEXP ignore case; the
        // mapped URL is .../banner.jpg, so nothing in it is rewritten.
        $caseOnly = '{"url":"https://site.test/wp-content/uploads/2026/05/BANNER.jpg","title":"Café"}';
        // Positive control: a row that does hold the mapped URL is written.
        $matching = '{"url":"https://site.test/wp-content/uploads/2026/05/banner.jpg","title":"Café"}';
        $wpdb->metaRows = [
            ['meta_id' => '11', 'meta_value' => $caseOnly],
            ['meta_id' => '12', 'meta_value' => $matching],
        ];
        $GLOBALS['wpdb'] = $wpdb;

        $result = (new DbRewriter())->replaceImages($this->map());

        $this->assertSame(1, $result['postmeta_rows']);
        $this->assertCount(1, $wpdb->updates, 'only the row that holds the mapped URL is written');
        $this->assertSame(['meta_id' => '12'], $wpdb->updates[0]['where']);
        $this->assertSame(
            ['meta_value' => '{"url":"https://site.test/wp-content/uploads/2026/05/banner.avif","title":"Café"}'],
            $wpdb->updates[0]['data']
        );
    }
}
