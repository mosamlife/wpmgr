<?php
/**
 * Rename round-trip: archive($path) then restore(archive) must equal identity,
 * and change_extension must behave like FlyingPress change_extesion().
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Media\Rename;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Media\Rename
 */
final class MediaRenameTest extends TestCase
{
    private string $dir = '';

    protected function set_up(): void
    {
        parent::set_up();
        $this->dir = sys_get_temp_dir() . '/wpmgr-rename-' . bin2hex(random_bytes(6));
        mkdir($this->dir, 0700, true);
    }

    protected function tear_down(): void
    {
        foreach ((array) glob($this->dir . '/*') as $f) {
            @unlink((string) $f);
        }
        @rmdir($this->dir);
        parent::tear_down();
    }

    public function test_archive_then_restore_is_identity(): void
    {
        $rename = new Rename();
        $path   = $this->dir . '/banner.jpg';
        file_put_contents($path, 'ORIGINAL-BYTES');

        $archive = $rename->archive($path);
        $this->assertSame($this->dir . '/banner.wpmgr-original.jpg', $archive);
        $this->assertFileDoesNotExist($path, 'original moved aside');
        $this->assertFileExists($archive);

        $restored = $rename->restore($archive);
        $this->assertSame($path, $restored);
        $this->assertFileExists($path);
        $this->assertFileDoesNotExist($archive);
        $this->assertSame('ORIGINAL-BYTES', file_get_contents($path), 'bytes preserved through round-trip');
    }

    public function test_archive_path_for_predicts_without_touching_disk(): void
    {
        $rename = new Rename();
        $this->assertSame(
            '/x/y/banner.wpmgr-original.png',
            $rename->archivePathFor('/x/y/banner.png')
        );
    }

    public function test_change_extension_replaces_trailing_segment_only(): void
    {
        $rename = new Rename();
        $this->assertSame('/x/banner.avif', $rename->changeExtension('/x/banner.jpg', 'avif'));
        $this->assertSame('https://s/banner.webp', $rename->changeExtension('https://s/banner.jpg', 'webp'));
        // Already matching => unchanged.
        $this->assertSame('/x/banner.jpg', $rename->changeExtension('/x/banner.jpg', 'jpg'));
        // Double-extension archive name.
        $this->assertSame('/x/banner.wpmgr-original.jpg', $rename->changeExtension('/x/banner.jpg', 'wpmgr-original.jpg'));
    }

    public function test_missing_source_is_a_noop_returning_target(): void
    {
        $rename  = new Rename();
        $archive = $rename->archive($this->dir . '/nope.jpg');
        $this->assertSame($this->dir . '/nope.wpmgr-original.jpg', $archive);
        $this->assertFileDoesNotExist($archive);
    }
}
