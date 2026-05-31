<?php
/**
 * End-to-end apply + restore round-trip for one attachment with a MOCKED
 * MediaUploader (fake presigned bytes) and a real temp uploads dir.
 *
 * Covers BOTH correctness branches:
 *   - different-ext (JPG -> AVIF): both files coexist, original NOT archived, a
 *     URL replacement is recorded, restore deletes the .avif and reverses.
 *   - same-ext (target_format='original'): the original is archived FIRST then
 *     overwritten; restore deletes the optimized file then un-renames the archive.
 *
 * Asserts: disk state, _wp_attachment_metadata, and the wpmgr_image_optimization
 * blob shape after apply; and that restore returns disk + metadata + blob to the
 * pre-optimization snapshot exactly.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\MediaApplyCommand;
use WPMgr\Agent\Commands\MediaRestoreCommand;
use WPMgr\Agent\Media\AttachmentMeta;
use WPMgr\Agent\Media\DbRewriter;
use WPMgr\Agent\Media\DiskWriter;
use WPMgr\Agent\Media\MediaUploader;
use WPMgr\Agent\Media\Rename;
use WPMgr\Agent\MediaKeystore;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\MediaApplyCommand
 * @covers \WPMgr\Agent\Commands\MediaRestoreCommand
 * @covers \WPMgr\Agent\Media\AttachmentMeta
 */
final class MediaApplyRestoreTest extends TestCase
{
    private string $baseDir = '';

    private string $baseUrl = 'https://site.test/wp-content/uploads';

    private int $attId = 77;

    /** @var array<int,array<string,mixed>> In-memory _wp_attachment_metadata store. */
    private array $metaStore = [];

    /** @var array<int,array<string,mixed>> In-memory postmeta store for the blob. */
    private array $blobStore = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->baseDir = sys_get_temp_dir() . '/wpmgr-media-' . bin2hex(random_bytes(6)) . '/2026/05';
        mkdir($this->baseDir, 0755, true);
        $this->metaStore = [];
        $this->blobStore = [];

        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
        Functions\when('wp_mkdir_p')->alias(static function ($d) {
            return is_dir($d) || mkdir($d, 0755, true);
        });
        Functions\when('wp_normalize_path')->alias(static fn ($p) => str_replace('\\', '/', (string) $p));
        Functions\when('wp_get_upload_dir')->alias(fn () => [
            'baseurl' => $this->baseUrl,
            'basedir' => dirname($this->baseDir, 2),
        ]);
        Functions\when('_wp_relative_upload_path')->alias(function ($path) {
            $base = dirname($this->baseDir, 2);
            return ltrim(str_replace($base, '', (string) $path), '/');
        });
        // Attachment metadata getter/setter backed by the in-memory store.
        Functions\when('wp_get_attachment_metadata')->alias(fn ($id) => $this->metaStore[$id] ?? []);
        Functions\when('wp_update_attachment_metadata')->alias(function ($id, $meta) {
            $this->metaStore[$id] = $meta;
            return true;
        });
        Functions\when('update_attached_file')->justReturn(true);
        Functions\when('get_post_field')->justReturn('');
        Functions\when('wp_delete_file')->alias(static fn ($f) => @unlink((string) $f));
        // Blob (postmeta) getter/setter.
        Functions\when('get_post_meta')->alias(fn ($id, $key, $single) => $this->blobStore[$id] ?? []);
        Functions\when('update_post_meta')->alias(function ($id, $key, $val) {
            $this->blobStore[$id] = $val;
            return true;
        });
        Functions\when('delete_post_meta')->alias(function ($id) {
            unset($this->blobStore[$id]);
            return true;
        });
        // The variant URL resolver: full -> banner.jpg, size -> banner-WxH.jpg.
        Functions\when('wp_get_attachment_image_url')->alias(function ($id, $size) {
            $meta = $this->metaStore[$id] ?? [];
            $file = (string) ($meta['file'] ?? '');
            if ($size === 'full') {
                return $this->baseUrl . '/' . $file;
            }
            $sizeFile = $meta['sizes'][$size]['file'] ?? '';
            $dir       = trim(dirname($file), '.');
            return $this->baseUrl . '/' . ($dir !== '' ? $dir . '/' : '') . $sizeFile;
        });
        Functions\when('wp_suspend_cache_addition')->justReturn(true);
    }

    protected function tear_down(): void
    {
        $root = dirname($this->baseDir, 2);
        $this->rrmdir($root);
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * Seed the original JPEG files + _wp_attachment_metadata for the attachment.
     *
     * @return array<string,mixed> the snapshot meta written.
     */
    private function seedOriginal(): array
    {
        file_put_contents($this->baseDir . '/banner.jpg', str_repeat('J', 4000)); // full
        file_put_contents($this->baseDir . '/banner-300x200.jpg', str_repeat('j', 800)); // medium

        $meta = [
            'file'     => '2026/05/banner.jpg',
            'filesize' => 4000,
            'width'    => 1200,
            'height'   => 800,
            'sizes'    => [
                'medium' => ['file' => 'banner-300x200.jpg', 'filesize' => 800, 'width' => 300, 'height' => 200, 'mime-type' => 'image/jpeg'],
            ],
        ];
        $this->metaStore[$this->attId] = $meta;

        return $meta;
    }

    /**
     * A fake uploader returning deterministic optimized bytes per variant.
     */
    private function fakeUploader(): MediaUploader
    {
        return new class extends MediaUploader {
            /** @var array<string,mixed>|null */
            public ?array $lastJobStatus = null;
            public bool $restoreReported = false;

            public function __construct()
            {
            }

            public function getBytes(string $presignedUrl): ?string
            {
                // The apply command passes get_url; we key fake bytes off the
                // variant name embedded in the fake URL.
                if (strpos($presignedUrl, 'full') !== false) {
                    return str_repeat('A', 1000); // optimized full (smaller).
                }
                if (strpos($presignedUrl, 'medium') !== false) {
                    return str_repeat('a', 200);
                }
                return str_repeat('?', 100);
            }

            public function jobStatus(string $endpoint, array $payload): array
            {
                $this->lastJobStatus = $payload;
                return ['ok' => true];
            }

            public function restoreStatus(string $endpoint, string $jobId, bool $restored, string $error = ''): bool
            {
                $this->restoreReported = $restored;
                return true;
            }
        };
    }

    /**
     * Build the MediaApplyVariant list (different-ext: target avif).
     *
     * @param string $mime
     * @return list<array<string,mixed>>
     */
    private function variants(string $mime): array
    {
        return [
            ['name' => 'full', 'get_url' => 'https://s3/out/full', 'optimized_mime' => $mime, 'optimized_size' => 1000],
            ['name' => 'medium', 'get_url' => 'https://s3/out/medium', 'optimized_mime' => $mime, 'optimized_size' => 200],
        ];
    }

    public function test_apply_different_ext_writes_avif_keeps_jpg_and_blob(): void
    {
        $snapshot = $this->seedOriginal();
        $uploader  = $this->fakeUploader();
        $apply     = new MediaApplyCommand($uploader, new AttachmentMeta(), new DbRewriter(), new MediaKeystore(), null);

        $res = $apply->execute([], [
            'job_id'           => 'job_ABC',
            'wp_attachment_id' => $this->attId,
            'target_format'    => 'avif',
            'target_quality'   => 'lossy',
            'status_endpoint'  => 'https://cp/agent/v1/media/job-status',
            'variants'         => $this->variants('image/avif'),
        ]);

        $this->assertTrue($res['ok'], $res['detail'] ?? '');

        // DISK: both the optimized .avif AND the original .jpg coexist (no archive).
        $this->assertFileExists($this->baseDir . '/banner.avif');
        $this->assertFileExists($this->baseDir . '/banner.jpg', 'original JPG is the Accept fallback, NOT archived');
        $this->assertFileExists($this->baseDir . '/banner-300x200.avif');
        $this->assertFileExists($this->baseDir . '/banner-300x200.jpg');
        $this->assertFileDoesNotExist($this->baseDir . '/banner.wpmgr-original.jpg', 'no archive in different-ext mode');

        // METADATA: now points at the avif files.
        $meta = $this->metaStore[$this->attId];
        $this->assertSame('2026/05/banner.avif', $meta['file']);
        $this->assertSame('banner-300x200.avif', $meta['sizes']['medium']['file']);
        $this->assertSame('image/avif', $meta['sizes']['medium']['mime-type']);

        // BLOB shape.
        $blob = $this->blobStore[$this->attId];
        $this->assertSame('job_ABC', $blob['wpmgr_job_id']);
        $this->assertSame(1, $blob['wpmgr_generation']);
        $this->assertSame('optimized', $blob['status']);
        $this->assertSame('avif', $blob['target_format']);
        $this->assertSame(['full', 'medium'], $blob['sizes_optimized']);
        $this->assertSame(0, $blob['original_deleted']);
        // original_data is the verbatim pre-optimize snapshot.
        $this->assertSame($snapshot, $blob['original_data']);
        // optimized_data records carry the coexist archive_mode.
        $this->assertSame(AttachmentMeta::MODE_COEXIST, $blob['optimized_data']['full']['archive_mode']);
        // replacements map old jpg url -> new avif url (drives DB rewrite + reverse).
        $this->assertSame(
            $this->baseUrl . '/2026/05/banner.avif',
            $blob['replacements'][$this->baseUrl . '/2026/05/banner.jpg']
        );

        // job-status payload matches the CP contract field names.
        $status = $uploader->lastJobStatus;
        $this->assertSame('job_ABC', $status['job_id']);
        $this->assertSame(['full', 'medium'], $status['applied_variants']);
        $this->assertSame(4800, $status['bytes_before']); // 4000 + 800.
        $this->assertSame(1200, $status['bytes_after']);  // 1000 + 200.
        $this->assertArrayHasKey('rewrite_stats', $status);
    }

    public function test_restore_different_ext_deletes_avif_and_restores(): void
    {
        $snapshot = $this->seedOriginal();
        $uploader  = $this->fakeUploader();

        (new MediaApplyCommand($uploader, new AttachmentMeta(), new DbRewriter(), new MediaKeystore(), null))
            ->execute([], [
                'job_id'           => 'job_ABC',
                'wp_attachment_id' => $this->attId,
                'target_format'    => 'avif',
                'target_quality'   => 'lossy',
                'status_endpoint'  => 'https://cp/agent/v1/media/job-status',
                'variants'         => $this->variants('image/avif'),
            ]);

        $restore = new MediaRestoreCommand($uploader, new MediaKeystore(), new AttachmentMeta(), new DbRewriter(), new Rename(), new DiskWriter());
        $res     = $restore->execute([], [
            'wp_attachment_id' => $this->attId,
            'status_endpoint'  => 'https://cp/agent/v1/media/restore-status',
            'jobs'             => [['job_id' => 'job_ABC', 'wp_attachment_id' => $this->attId]],
        ]);

        $this->assertTrue($res['ok']);
        $this->assertTrue($uploader->restoreReported);

        // DISK: optimized avif files gone; originals untouched.
        $this->assertFileDoesNotExist($this->baseDir . '/banner.avif');
        $this->assertFileDoesNotExist($this->baseDir . '/banner-300x200.avif');
        $this->assertFileExists($this->baseDir . '/banner.jpg');

        // METADATA restored exactly to the snapshot.
        $this->assertSame($snapshot, $this->metaStore[$this->attId]);

        // BLOB deleted (nothing was unoptimizable).
        $this->assertArrayNotHasKey($this->attId, $this->blobStore);
    }

    public function test_apply_same_ext_archives_original_then_overwrites(): void
    {
        $this->seedOriginal();
        $originalBytes = file_get_contents($this->baseDir . '/banner.jpg');
        $uploader      = $this->fakeUploader();

        // target_format='original' => same-ext: optimized mime == source jpeg.
        (new MediaApplyCommand($uploader, new AttachmentMeta(), new DbRewriter(), new MediaKeystore(), null))
            ->execute([], [
                'job_id'           => 'job_SAME',
                'wp_attachment_id' => $this->attId,
                'target_format'    => 'original',
                'target_quality'   => 'lossless',
                'status_endpoint'  => 'https://cp/agent/v1/media/job-status',
                'variants'         => $this->variants('image/jpeg'),
            ]);

        // The original was ARCHIVED to .wpmgr-original.jpg, then overwritten.
        $this->assertFileExists($this->baseDir . '/banner.wpmgr-original.jpg', 'original archived');
        $this->assertSame($originalBytes, file_get_contents($this->baseDir . '/banner.wpmgr-original.jpg'), 'archive holds the original bytes');
        // The live path now holds the smaller optimized bytes.
        $this->assertSame(1000, filesize($this->baseDir . '/banner.jpg'), 'live path overwritten with optimized bytes');

        $blob = $this->blobStore[$this->attId];
        $this->assertSame(AttachmentMeta::MODE_REPLACE, $blob['optimized_data']['full']['archive_mode']);
        // Same-ext => no URL change => no replacements.
        $this->assertSame([], $blob['replacements']);
    }

    public function test_restore_same_ext_unarchives_after_deleting_optimized(): void
    {
        $this->seedOriginal();
        $originalFull = file_get_contents($this->baseDir . '/banner.jpg');
        $uploader     = $this->fakeUploader();

        (new MediaApplyCommand($uploader, new AttachmentMeta(), new DbRewriter(), new MediaKeystore(), null))
            ->execute([], [
                'job_id'           => 'job_SAME',
                'wp_attachment_id' => $this->attId,
                'target_format'    => 'original',
                'target_quality'   => 'lossless',
                'status_endpoint'  => 'https://cp/agent/v1/media/job-status',
                'variants'         => $this->variants('image/jpeg'),
            ]);

        // Sanity: live path is now the optimized (smaller) bytes.
        $this->assertSame(1000, filesize($this->baseDir . '/banner.jpg'));

        (new MediaRestoreCommand($uploader, new MediaKeystore(), new AttachmentMeta(), new DbRewriter(), new Rename(), new DiskWriter()))
            ->execute([], [
                'wp_attachment_id' => $this->attId,
                'status_endpoint'  => 'https://cp/agent/v1/media/restore-status',
                'jobs'             => [['job_id' => 'job_SAME', 'wp_attachment_id' => $this->attId]],
            ]);

        // The archive was un-renamed back over the (deleted) optimized file:
        // the live path holds the ORIGINAL bytes again, and no archive remains.
        $this->assertFileExists($this->baseDir . '/banner.jpg');
        $this->assertSame($originalFull, file_get_contents($this->baseDir . '/banner.jpg'), 'original bytes restored');
        $this->assertFileDoesNotExist($this->baseDir . '/banner.wpmgr-original.jpg', 'archive consumed');
        $this->assertArrayNotHasKey($this->attId, $this->blobStore, 'blob removed on clean restore');
    }

    public function test_restore_refused_when_originals_deleted(): void
    {
        $this->blobStore[$this->attId] = ['status' => 'originals_deleted', 'original_deleted' => 1];
        $uploader = $this->fakeUploader();

        $result = (new MediaRestoreCommand($uploader, new MediaKeystore(), new AttachmentMeta(), new DbRewriter(), new Rename(), new DiskWriter()))
            ->restoreOne($this->attId);

        $this->assertFalse($result['ok']);
        $this->assertSame('originals_deleted_cannot_restore', $result['error']);
    }

    private function rrmdir(string $dir): void
    {
        if ($dir === '' || !is_dir($dir)) {
            return;
        }
        foreach ((array) scandir($dir) as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $path = $dir . DIRECTORY_SEPARATOR . $item;
            is_dir($path) ? $this->rrmdir($path) : @unlink($path);
        }
        @rmdir($dir);
    }
}
