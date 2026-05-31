<?php
/**
 * HtaccessInstaller idempotency: rendering the block into content twice yields
 * exactly ONE marked block, and the Accept-fallback + Vary rules are present.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Media\HtaccessInstaller;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Media\HtaccessInstaller
 */
final class MediaHtaccessInstallerTest extends TestCase
{
    public function test_block_is_idempotent(): void
    {
        $installer = new HtaccessInstaller();

        $existing = "# BEGIN WordPress\nRewriteRule . /index.php [L]\n# END WordPress\n";

        $once  = $installer->renderInto($existing);
        $twice = $installer->renderInto($once);

        // Exactly one BEGIN/END marker pair after two renders.
        $this->assertSame(1, substr_count($twice, HtaccessInstaller::BEGIN));
        $this->assertSame(1, substr_count($twice, HtaccessInstaller::END));
        // Running twice must be a no-op (stable output).
        $this->assertSame($once, $twice, 'second render must equal the first (idempotent)');

        // The original WordPress block is preserved.
        $this->assertStringContainsString('# BEGIN WordPress', $twice);
    }

    public function test_block_contains_accept_fallback_and_vary(): void
    {
        $installer = new HtaccessInstaller();
        $out        = $installer->renderInto('');

        $this->assertStringContainsString('RewriteCond %{HTTP_ACCEPT} !image/avif', $out);
        $this->assertStringContainsString('RewriteCond %{HTTP_ACCEPT} !image/webp', $out);
        // The -f existence guard must be present so a missing twin never 404s.
        $this->assertStringContainsString('-f', $out);
        $this->assertStringContainsString('Header merge Vary Accept', $out);
    }

    public function test_empty_input_produces_single_block(): void
    {
        $installer = new HtaccessInstaller();
        $out        = $installer->renderInto('');
        $this->assertSame(1, substr_count($out, HtaccessInstaller::BEGIN));
        $this->assertSame(1, substr_count($out, HtaccessInstaller::END));
    }

    public function test_nginx_snippet_is_offered(): void
    {
        $installer = new HtaccessInstaller();
        $snippet    = $installer->nginxSnippet();
        $this->assertStringContainsString('Vary Accept', $snippet);
        $this->assertStringContainsString('avif|webp', $snippet);
    }
}
