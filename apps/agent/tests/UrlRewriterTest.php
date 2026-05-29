<?php
/**
 * UrlRewriterTest — focused PHPUnit coverage for the P0/ADR-036 URL rewriter.
 *
 * Covers the three signatures of WordPress's "URL hiding in the DB" problem:
 *   1. Plain string substring rewrite (the trivial case).
 *   2. URL-encoded form rewrite (the case the naive str_replace misses).
 *   3. PHP-serialized blob — the safety-critical case that MUST re-emit a
 *      correct `s:NN:` length prefix or the option silently breaks.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use PHPUnit\Framework\TestCase;
use WPMgr\Agent\Backup\UrlRewriter;

final class UrlRewriterTest extends TestCase
{
    public function testPlainStringRewrite(): void
    {
        $replacements = UrlRewriter::build_replacements(
            'https://staging.example.com', 'https://example.com',
            'https://staging.example.com', 'https://example.com',
            'https://staging.example.com/wp-content', 'https://example.com/wp-content',
            'https://staging.example.com/wp-content/uploads', 'https://example.com/wp-content/uploads'
        );

        $input  = 'Click https://staging.example.com/foo here.';
        $output = UrlRewriter::rewrite_row_data($input, $replacements);

        $this->assertSame('Click https://example.com/foo here.', $output);
    }

    public function testUrlEncodedRewrite(): void
    {
        $replacements = UrlRewriter::build_replacements(
            'https://staging.example.com', 'https://example.com',
            'https://staging.example.com', 'https://example.com',
            'https://staging.example.com/wp-content', 'https://example.com/wp-content',
            'https://staging.example.com/wp-content/uploads', 'https://example.com/wp-content/uploads'
        );

        // URL-encoded form: a URL stored inside another URL's query string,
        // or inside a JSON-encoded REST payload stored in wp_options.
        $input  = 'redirect=https%3A%2F%2Fstaging.example.com%2Ffoo';
        $output = UrlRewriter::rewrite_row_data($input, $replacements);

        $this->assertSame('redirect=https%3A%2F%2Fexample.com%2Ffoo', $output);
    }

    /**
     * The safety-critical case: a PHP-serialized blob with a hard-coded
     * string-length prefix must come back out with a CORRECT length prefix
     * after rewrite. A naive str_replace would change the bytes but leave
     * the original `s:30:` prefix in place; `unserialize()` would then
     * return `false` and the option would silently brick.
     */
    public function testSerializedBlobRewrite(): void
    {
        $replacements = UrlRewriter::build_replacements(
            'https://staging.example.com', 'https://example.com',
            'https://staging.example.com', 'https://example.com',
            'https://staging.example.com/wp-content', 'https://example.com/wp-content',
            'https://staging.example.com/wp-content/uploads', 'https://example.com/wp-content/uploads'
        );

        $original = [
            'siteurl' => 'https://staging.example.com',
            'home'    => 'https://staging.example.com',
            'count'   => 7, // non-string scalar must survive unchanged.
        ];
        $serializedInput = serialize($original);

        $serializedOutput = UrlRewriter::rewrite_row_data($serializedInput, $replacements);

        // 1. The output must be valid serialized PHP (unserialize must NOT
        //    return false). This is the regression a naive rewrite would hit.
        $decoded = @unserialize($serializedOutput, ['allowed_classes' => false]);
        $this->assertNotFalse(
            $decoded,
            'rewrite produced an unserialize-fail blob — length prefix bug'
        );

        // 2. The decoded structure must have the new URLs.
        $this->assertIsArray($decoded);
        $this->assertSame('https://example.com', $decoded['siteurl']);
        $this->assertSame('https://example.com', $decoded['home']);

        // 3. The non-string scalar must survive bit-perfect.
        $this->assertSame(7, $decoded['count']);

        // 4. Spot-check the length prefix is correct for the new URL.
        // "https://example.com" is 19 chars → expect `s:19:`.
        $this->assertStringContainsString('s:19:"https://example.com"', $serializedOutput);
    }

    /**
     * Same-environment restore (source URL == target URL): build_replacements
     * must return empty arrays so rewrite_row_data is a no-op. This is the
     * common self-hosted-single-site case and we don't want to pay any
     * rewrite cost (or risk any rewrite bugs) for it.
     */
    public function testSameEnvironmentNoOp(): void
    {
        $replacements = UrlRewriter::build_replacements(
            'https://example.com', 'https://example.com',
            'https://example.com', 'https://example.com',
            'https://example.com/wp-content', 'https://example.com/wp-content',
            'https://example.com/wp-content/uploads', 'https://example.com/wp-content/uploads'
        );
        $this->assertSame([], $replacements[0]);
        $this->assertSame([], $replacements[1]);

        $input  = 'a:1:{s:7:"siteurl";s:19:"https://example.com";}';
        $output = UrlRewriter::rewrite_row_data($input, $replacements);
        $this->assertSame($input, $output);
    }
}
