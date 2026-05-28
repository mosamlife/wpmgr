<?php
/**
 * Tests for BackupSource path containment: resolveWritePath must reject any
 * path that escapes wp-content and accept safe site-relative paths.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Support\BackupSource;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\BackupSource
 */
final class BackupSourceTest extends TestCase
{
    private string $root = '';

    protected function set_up(): void
    {
        parent::set_up();
        $this->root = sys_get_temp_dir() . '/wpmgr-src-' . bin2hex(random_bytes(6));
        mkdir($this->root, 0755, true);
    }

    protected function tear_down(): void
    {
        @rmdir($this->root);
        parent::tear_down();
    }

    private function source(): BackupSource
    {
        $root = $this->root;

        return new class($root) extends BackupSource {
            private string $root;

            public function __construct(string $root)
            {
                $this->root = $root;
            }

            public function contentRoot(): string
            {
                return $this->root;
            }
        };
    }

    public function test_accepts_safe_relative_paths(): void
    {
        $s = $this->source();
        $this->assertSame($this->root . '/plugins/foo/bar.php', $s->resolveWritePath('plugins/foo/bar.php'));
        $this->assertSame($this->root . '/uploads/2024/img.jpg', $s->resolveWritePath('uploads/2024/img.jpg'));
    }

    /**
     * @return array<string,array{string}>
     */
    public static function unsafePaths(): array
    {
        return [
            'parent traversal'   => ['../wp-config.php'],
            'deep traversal'     => ['plugins/../../../../etc/passwd'],
            'absolute unix'      => ['/etc/passwd'],
            'current dir'        => ['./x'],
            'windows drive'      => ['C:\\windows\\system32'],
            'null byte'          => ["ok\0/../evil"],
            'empty'              => [''],
        ];
    }

    /**
     * @dataProvider unsafePaths
     * @param string $path Unsafe path.
     */
    public function test_rejects_unsafe_paths(string $path): void
    {
        $this->assertSame('', $this->source()->resolveWritePath($path));
    }

    public function test_relative_to_returns_null_outside_root(): void
    {
        $this->assertNull(BackupSource::relativeTo('/var/www/wp-content', '/etc/passwd'));
        $this->assertSame('plugins/x.php', BackupSource::relativeTo('/var/www/wp-content', '/var/www/wp-content/plugins/x.php'));
    }
}
