<?php
/**
 * Store/serve parity for page-cache path normalisation.
 *
 * The path a page is STORED under is computed by CacheKey::normalizePath().
 * The path it is SERVED from is computed by the advanced-cache drop-in, which
 * runs before WordPress loads and therefore cannot call that class. The two
 * implementations are duplicated source, and a silent divergence between them
 * does not fail loudly: it stores under one key and looks up under another, so
 * either the cache stops hitting or one request reaches a file keyed for a
 * different one.
 *
 * This test enforces the duplication two ways: the marked source blocks must be
 * byte-identical once dedented, and both must produce identical output over a
 * shared corpus.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Cache\CacheKey;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Cache\CacheKey::normalizePath
 */
final class CacheNormalizeParityTest extends TestCase
{
    private const BEGIN = 'WPMGR-NORMALIZE-PATH-SHARED BEGIN';
    private const END   = 'WPMGR-NORMALIZE-PATH-SHARED END';

    /**
     * Paths exercised by both implementations. Ordinary traffic first, then the
     * shapes that make a substring strip disagree with itself.
     *
     * @return list<string>
     */
    private function corpus(): array
    {
        return [
            '/', '', '/about', '/about/', '/blog/2026/09/hello-world/',
            '/index.php', '/feed/', '/page/2/', '/category/news/',
            '/shop/product-1/?utm_source=x&utm_medium=y', '/?s=hello+world',
            '/wp-json/wp/v2/posts', '/sitemap.xml', '/robots.txt',
            '/caf%C3%A9/', '/%E6%97%A5%E6%9C%AC%E8%AA%9E/page/2/',
            '/sub/site/blog/post/', '/a-very-long-slug-' . str_repeat('x', 200) . '/',
            '/.', '/..', '/...', '/....', '/.....', '/./.', '/.hidden/file',
            '/file.', '/a..b/c', '//', '///', '/a//b', '/a/./b', '/a/../b',
            '/a/b/../..', '/a/b/../../..', '/a/../../../../..',
            '/%2e%2e/x', '/%2e%2e%2fx', '/%252e%252e%252fx', '/..%2fx',
            '/x\\y', '/x\\..\\y', "/x\0/y",
            '/path?/x', '/path#/x',
        ];
    }

    /**
     * Read one marked block out of a source file, dedented to column zero.
     *
     * @param string $file Absolute path to the source file.
     * @return string Block source, one line per element, trailing newline kept.
     */
    private function sharedBlock(string $file): string
    {
        $source = file_get_contents($file);
        $this->assertIsString($source, "unreadable: $file");

        $lines = explode("\n", $source);
        $out   = [];
        $in    = false;
        foreach ($lines as $line) {
            if (!$in) {
                if (strpos($line, self::BEGIN) !== false) {
                    $in = true;
                }
                continue;
            }
            if (strpos($line, self::END) !== false) {
                $in = false;
                break;
            }
            $out[] = $line;
        }

        $this->assertNotSame([], $out, "no marked block found in $file");
        $this->assertFalse($in, "unterminated marked block in $file");

        // Dedent by the smallest indent on any non-blank line. Indentation is
        // the only licensed difference: the class copy sits inside a method
        // body and the drop-in copy at file scope.
        $indent = PHP_INT_MAX;
        foreach ($out as $line) {
            if (trim($line) === '') {
                continue;
            }
            $indent = min($indent, strlen($line) - strlen(ltrim($line)));
        }
        $this->assertLessThan(PHP_INT_MAX, $indent, "marked block in $file is blank");

        foreach ($out as $i => $line) {
            $out[$i] = trim($line) === '' ? '' : substr($line, $indent);
        }

        return implode("\n", $out) . "\n";
    }

    /**
     * The duplicated source must match byte for byte once dedented. This is the
     * check that fails when someone edits one copy and forgets the other.
     */
    public function test_shared_normalisation_blocks_are_byte_identical(): void
    {
        $classBlock  = $this->sharedBlock(dirname(__DIR__) . '/includes/cache/class-cache-key.php');
        $dropinBlock = $this->sharedBlock(dirname(__DIR__) . '/assets/wpmgr-advanced-cache.php');

        $this->assertNotSame('', trim($classBlock), 'class block must not be empty');
        $this->assertSame(
            md5($classBlock),
            md5($dropinBlock),
            'CacheKey::normalizePath and the advanced-cache drop-in must carry the '
            . 'same normalisation source; they key the same file from opposite sides.'
        );
        $this->assertSame($classBlock, $dropinBlock);
    }

    /**
     * Behavioural parity: evaluate the drop-in's own block, in isolation, over
     * the corpus and compare to normalizePath(). Source identity above proves
     * the text matches; this proves the text does what the class does when the
     * surrounding lines (query strip, decode, case fold) are applied too.
     */
    public function test_dropin_block_and_normalize_path_agree_on_corpus(): void
    {
        $block = $this->sharedBlock(dirname(__DIR__) . '/assets/wpmgr-advanced-cache.php');

        // Materialise the extracted block as a file and include it, so the
        // drop-in's own copy runs without booting WordPress and without any
        // rewriting of its source on the way.
        $tmp = sys_get_temp_dir() . '/wpmgr-normparity-' . getmypid() . '-' . uniqid('', true) . '.php';
        file_put_contents($tmp, "<?php\n" . $block);

        $runner = static function (string $raw) use ($tmp): string {
            $wpmgr_uri  = $raw;
            $wpmgr_qpos = strpos($wpmgr_uri, '?');
            if ($wpmgr_qpos !== false) {
                $wpmgr_uri = substr($wpmgr_uri, 0, $wpmgr_qpos);
            }
            $wpmgr_path = strtolower(rawurldecode($wpmgr_uri));
            include $tmp;
            return $wpmgr_path;
        };

        try {
            foreach ($this->corpus() as $raw) {
                $this->assertSame(
                    CacheKey::normalizePath($raw),
                    $runner($raw),
                    'store and serve normalisation disagree for ' . var_export($raw, true)
                );
            }
        } finally {
            @unlink($tmp);
        }
    }

    /**
     * Containment invariant, stated over the corpus: whatever comes back, no
     * segment of it is a dot segment, so joining it under a bucket directory
     * cannot name anything outside that bucket.
     */
    public function test_normalized_paths_never_carry_a_dot_segment(): void
    {
        foreach ($this->corpus() as $raw) {
            $got = CacheKey::normalizePath($raw);
            $this->assertSame(
                0,
                preg_match('#(^|/)\.+(/|$)#', $got),
                'dot segment survived normalisation of ' . var_export($raw, true) . ' -> ' . var_export($got, true)
            );
            $this->assertStringNotContainsString('//', $got, 'empty segment survived: ' . var_export($got, true));
            $this->assertStringNotContainsString('\\', $got, 'backslash survived: ' . var_export($got, true));
            if ($got !== '') {
                $this->assertStringStartsWith('/', $got);
                $this->assertStringEndsNotWith('/', $got);
            }
        }
    }

    /**
     * Containment is idempotent: feeding a normalised path back in changes
     * nothing. A strip-based sanitiser is what fails this, and failing it is
     * what let a second pass manufacture a segment the first pass had not seen.
     *
     * Inputs that still carry percent-encoding after one pass are excluded on
     * purpose. normalizePath() decodes exactly once, which is the correct
     * behaviour: re-feeding its output decodes a second time and would resolve
     * an escape sequence that was a literal name in the request. The property
     * under test is the segment resolution, not the decode.
     */
    public function test_containment_is_idempotent(): void
    {
        $checked = 0;
        foreach ($this->corpus() as $raw) {
            $once = CacheKey::normalizePath($raw);
            if (strpos($once, '%') !== false) {
                continue;
            }
            $this->assertSame($once, CacheKey::normalizePath($once), 'not idempotent for ' . var_export($raw, true));
            $checked++;
        }
        $this->assertGreaterThan(30, $checked, 'the exclusion above must not empty the corpus');
    }
}
