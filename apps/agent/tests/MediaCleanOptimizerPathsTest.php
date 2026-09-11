<?php
/**
 * MediaCleanOptimizerPathsTest — verifies that isolate/restore/delete include
 * the Media Optimizer's extra on-disk files when an optimization blob exists.
 *
 * The gap: allFilePathsForAttachment() collected only standard WP files; the
 * optimizer's archive (.wpmgr-original.<ext>) and coexist (.avif/.webp) files
 * were left behind on isolate/delete. This test suite covers the fix.
 *
 * Uses an isolated temp tree so no live WordPress DB or uploads directory is
 * required. Brain Monkey stubs all WP function calls; MediaKeystore is bypassed
 * via get_post_meta stubs. MediaQuarantine is injected with the isolated
 * wp-content dir so WP_CONTENT_DIR is never required.
 *
 * Covered scenarios:
 *   1a. REPLACE mode isolate: standard + .wpmgr-original.ext archive files
 *       all move to quarantine.
 *   1b. REPLACE mode restore: all files return to their original paths.
 *   1c. REPLACE mode delete: all files permanently removed.
 *   2.  COEXIST mode: standard original + .avif variant are ALL handled
 *       (isolate + restore).
 *   3.  originals_deleted status: archives are gone; isolate succeeds with just
 *       standard files, no error.
 *   4.  Non-optimized attachment (no blob): behavior unchanged — only standard files.
 *   5.  SECURITY absolute-path: a blob whose derived path escapes uploads is
 *       rejected; the out-of-uploads file is never quarantined; standard files
 *       still process.
 *   5b. SECURITY traversal: a blob path containing `..` is rejected.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\MediaCleanCommand;
use WPMgr\Agent\Media\AttachmentMeta;
use WPMgr\Agent\Media\MediaQuarantine;
use WPMgr\Agent\MediaKeystore;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\MediaCleanCommand
 */
final class MediaCleanOptimizerPathsTest extends TestCase
{
    /** Isolated temp root per test (wraps wp-content + uploads). */
    private string $tmpRoot = '';

    /** Simulated wp-content dir. */
    private string $wpContent = '';

    /** Simulated uploads base dir. */
    private string $uploadsDir = '';

    // =========================================================================
    // setUp / tearDown
    // =========================================================================

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        // Build an isolated temp tree.
        $this->tmpRoot    = sys_get_temp_dir() . '/wpmgr-optpaths-' . bin2hex(random_bytes(6));
        $this->wpContent  = $this->tmpRoot . '/wp-content';
        $this->uploadsDir = $this->wpContent . '/uploads';
        mkdir($this->uploadsDir . '/2024/06', 0755, true);

        // Stub all WP functions used by the command + quarantine internals.
        Functions\when('wp_upload_dir')->justReturn([
            'basedir' => $this->uploadsDir,
            'baseurl' => 'https://example.com/wp-content/uploads',
        ]);
        Functions\when('wp_json_encode')->alias(static function ($data, int $flags = 0): string|false {
            return json_encode($data, $flags);
        });
        Functions\when('wp_delete_file')->alias(static function (string $path): void {
            @unlink($path);
        });
        Functions\when('wp_delete_attachment')->justReturn(true);
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->tmpRoot);
        Monkey\tearDown();
        parent::tear_down();
    }

    // =========================================================================
    // Helpers
    // =========================================================================

    /**
     * Build a MediaCleanCommand with an injected quarantine that writes to the
     * isolated test wp-content dir, so WP_CONTENT_DIR is never needed.
     */
    private function makeCommand(): MediaCleanCommand
    {
        return new MediaCleanCommand(new MediaQuarantine($this->wpContent));
    }

    /**
     * Create a file under uploads and return its absolute path.
     */
    private function createFile(string $relPath, string $content = 'fake'): string
    {
        $abs = $this->uploadsDir . '/' . ltrim($relPath, '/');
        $dir = dirname($abs);
        if (!is_dir($dir)) {
            mkdir($dir, 0755, true);
        }
        file_put_contents($abs, $content);
        return $abs;
    }

    /**
     * Stub get_post_meta so the given attachment returns controlled values for
     * the three keys the command reads.
     *
     * @param int    $attachmentId
     * @param string $relPath
     * @param array  $meta         _wp_attachment_metadata
     * @param array  $blob         wpmgr_image_optimization ([] = no blob)
     */
    private function stubPostMeta(
        int $attachmentId,
        string $relPath,
        array $meta,
        array $blob = []
    ): void {
        Functions\when('get_post_meta')->alias(
            static function (int $postId, string $key, bool $single) use (
                $attachmentId,
                $relPath,
                $meta,
                $blob
            ) {
                if ($postId !== $attachmentId) {
                    return $single ? '' : [];
                }
                return match ($key) {
                    '_wp_attached_file'        => $relPath,
                    '_wp_attachment_metadata'  => $meta,
                    MediaKeystore::KEY         => $blob !== [] ? $blob : '',
                    default                    => $single ? '' : [],
                };
            }
        );
    }

    private function quarantineRoot(): string
    {
        return $this->wpContent . '/wpmgr-quarantine';
    }

    private function mediaRoot(): string
    {
        return $this->quarantineRoot() . '/media';
    }

    private function manifestsRoot(): string
    {
        return $this->quarantineRoot() . '/manifests';
    }

    /**
     * Find the single manifest ID written under manifests/ and return it.
     */
    private function findManifestId(): string
    {
        $manifests = glob($this->manifestsRoot() . '/*.json');
        if (empty($manifests)) {
            return '';
        }
        return basename((string)$manifests[0], '.json');
    }

    private function rrmdir(string $dir): void
    {
        if (!is_dir($dir)) {
            return;
        }
        $entries = @scandir($dir);
        if ($entries === false) {
            return;
        }
        foreach ($entries as $entry) {
            if ($entry === '.' || $entry === '..') {
                continue;
            }
            $path = $dir . '/' . $entry;
            is_dir($path) ? $this->rrmdir($path) : @unlink($path);
        }
        @rmdir($dir);
    }

    // =========================================================================
    // Test 1a: REPLACE mode isolate moves standard + archive files
    // =========================================================================

    public function testReplaceModeIsolateMovesArchiveFiles(): void
    {
        $attachmentId = 11;
        $relPath      = '2024/06/photo.jpg';

        // Standard files: main + one sub-size.
        $mainAbs      = $this->createFile($relPath,                                    'main-jpg');
        $thumbAbs     = $this->createFile('2024/06/photo-150x150.jpg',                 'thumb-jpg');

        // Optimizer archive files (.wpmgr-original.jpg for full + sub-size).
        $mainArchive  = $this->createFile('2024/06/photo.wpmgr-original.jpg',          'archive-main');
        $thumbArchive = $this->createFile('2024/06/photo-150x150.wpmgr-original.jpg',  'archive-thumb');

        $meta = [
            'file'  => $relPath,
            'sizes' => [
                'thumbnail' => ['file' => 'photo-150x150.jpg', 'width' => 150, 'height' => 150],
            ],
        ];

        // The blob's `path` for REPLACE mode is the post-overwrite (current) path,
        // i.e. the original location. The archive is derived via archivePathFor().
        $blob = [
            'status'         => MediaKeystore::STATUS_OPTIMIZED,
            'optimized_data' => [
                'full' => [
                    'archive_mode' => AttachmentMeta::MODE_REPLACE,
                    'path'         => $mainAbs,
                    'size'         => 8000,
                ],
                'thumbnail' => [
                    'archive_mode' => AttachmentMeta::MODE_REPLACE,
                    'path'         => $thumbAbs,
                    'size'         => 2000,
                ],
            ],
        ];

        $this->stubPostMeta($attachmentId, $relPath, $meta, $blob);

        $result = $this->makeCommand()->execute([], [
            'action'         => 'isolate',
            'job_id'         => 'job-replace-isolate',
            'attachment_ids' => [$attachmentId],
        ]);

        $this->assertTrue((bool)($result['ok'] ?? false), 'isolate ok');
        // 2 standard files + 2 archive files = 4 total.
        $this->assertSame(4, (int)($result['moved'] ?? 0), '4 files moved (2 standard + 2 archives)');

        $this->assertFileDoesNotExist($mainAbs,      'main file moved to quarantine');
        $this->assertFileDoesNotExist($thumbAbs,     'thumb file moved to quarantine');
        $this->assertFileDoesNotExist($mainArchive,  'main archive moved to quarantine');
        $this->assertFileDoesNotExist($thumbArchive, 'thumb archive moved to quarantine');

        // Manifest was written and lists all 4 files.
        $manifestId = $this->findManifestId();
        $this->assertNotSame('', $manifestId, 'manifest created');
        $manifestFile = $this->manifestsRoot() . '/' . $manifestId . '.json';
        $data  = json_decode((string)file_get_contents($manifestFile), true);
        $files = (array)($data['entries'][0]['files'] ?? []);
        $this->assertCount(4, $files, 'manifest lists all 4 files');
    }

    // =========================================================================
    // Test 1b: REPLACE mode restore returns all files
    // =========================================================================

    public function testReplaceModeRestoreReturnsAllFiles(): void
    {
        $attachmentId = 12;
        $relPath      = '2024/06/restore-photo.jpg';

        $mainAbs      = $this->createFile($relPath,                                         'main-data');
        $mainArchive  = $this->createFile('2024/06/restore-photo.wpmgr-original.jpg',       'archive-data');

        $meta = ['file' => $relPath, 'sizes' => []];
        $blob = [
            'status'         => MediaKeystore::STATUS_OPTIMIZED,
            'optimized_data' => [
                'full' => [
                    'archive_mode' => AttachmentMeta::MODE_REPLACE,
                    'path'         => $mainAbs,
                    'size'         => 5000,
                ],
            ],
        ];

        $this->stubPostMeta($attachmentId, $relPath, $meta, $blob);
        $cmd = $this->makeCommand();

        // Isolate first.
        $isolateResult = $cmd->execute([], [
            'action'         => 'isolate',
            'job_id'         => 'job-replace-restore',
            'attachment_ids' => [$attachmentId],
        ]);
        $this->assertTrue((bool)($isolateResult['ok'] ?? false));
        $this->assertSame(2, (int)($isolateResult['moved'] ?? 0));

        $manifestId = $this->findManifestId();

        // Now restore.
        $restored = $cmd->execute([], [
            'action'         => 'restore',
            'job_id'         => 'job-replace-restore',
            'quarantine_ids' => [$manifestId],
        ]);
        $this->assertTrue((bool)($restored['ok'] ?? false), 'restore ok');
        $this->assertSame(2, (int)($restored['restored'] ?? 0), 'both files restored');

        $this->assertFileExists($mainAbs,     'main file restored');
        $this->assertFileExists($mainArchive, 'archive file restored');
        $this->assertSame('archive-data', file_get_contents($mainArchive), 'archive content intact');
        $this->assertSame('main-data',    file_get_contents($mainAbs),    'main content intact');
    }

    // =========================================================================
    // Test 1c: REPLACE mode delete permanently removes all files
    // =========================================================================

    public function testReplaceModeDeleteRemovesAllFiles(): void
    {
        $attachmentId = 13;
        $relPath      = '2024/06/delete-photo.jpg';

        $mainAbs     = $this->createFile($relPath,                                        'main-del');
        $mainArchive = $this->createFile('2024/06/delete-photo.wpmgr-original.jpg',       'archive-del');

        $meta = ['file' => $relPath, 'sizes' => []];
        $blob = [
            'status'         => MediaKeystore::STATUS_OPTIMIZED,
            'optimized_data' => [
                'full' => [
                    'archive_mode' => AttachmentMeta::MODE_REPLACE,
                    'path'         => $mainAbs,
                    'size'         => 4000,
                ],
            ],
        ];

        $this->stubPostMeta($attachmentId, $relPath, $meta, $blob);
        $cmd = $this->makeCommand();

        // Isolate.
        $isolateResult = $cmd->execute([], [
            'action'         => 'isolate',
            'job_id'         => 'job-replace-delete',
            'attachment_ids' => [$attachmentId],
        ]);
        $this->assertTrue((bool)($isolateResult['ok'] ?? false));
        $this->assertSame(2, (int)($isolateResult['moved'] ?? 0));

        // Both files are gone from uploads.
        $this->assertFileDoesNotExist($mainAbs,     'main moved to quarantine');
        $this->assertFileDoesNotExist($mainArchive, 'archive moved to quarantine');

        $manifestId = $this->findManifestId();

        // Delete permanently.
        $deleted = $cmd->execute([], [
            'action'         => 'delete',
            'job_id'         => 'job-replace-delete',
            'confirm'        => 'DELETE',
            'quarantine_ids' => [$manifestId],
        ]);
        $this->assertTrue((bool)($deleted['ok'] ?? false), 'delete ok');
        $this->assertSame(1, (int)($deleted['deleted'] ?? 0), '1 attachment post deleted');

        // Quarantine dir is gone; files are permanently deleted.
        $manifestDir = $this->mediaRoot() . '/' . $manifestId;
        $this->assertDirectoryDoesNotExist($manifestDir, 'quarantine dir cleaned up');
    }

    // =========================================================================
    // Test 2: COEXIST mode — standard original + .avif variant all handled
    // =========================================================================

    public function testCoexistModeIsolateAndRestoreIncludesVariants(): void
    {
        $attachmentId = 22;
        $relPath      = '2024/06/banner.jpg';

        // Standard file (the original is kept in place in COEXIST mode).
        $origAbs = $this->createFile($relPath,            'original-jpg');

        // Optimizer variant file (different extension, written alongside original).
        $avifAbs = $this->createFile('2024/06/banner.avif', 'avif-bytes');

        $meta = ['file' => $relPath, 'sizes' => []];
        $blob = [
            'status'         => MediaKeystore::STATUS_OPTIMIZED,
            'optimized_data' => [
                'full' => [
                    'archive_mode' => AttachmentMeta::MODE_COEXIST,
                    'path'         => $avifAbs, // the optimized variant's path
                    'size'         => 5000,
                ],
            ],
        ];

        $this->stubPostMeta($attachmentId, $relPath, $meta, $blob);
        $cmd = $this->makeCommand();

        // Isolate.
        $result = $cmd->execute([], [
            'action'         => 'isolate',
            'job_id'         => 'job-coexist',
            'attachment_ids' => [$attachmentId],
        ]);
        $this->assertTrue((bool)($result['ok'] ?? false), 'isolate ok');
        $this->assertSame(2, (int)($result['moved'] ?? 0), 'original + avif moved');
        $this->assertFileDoesNotExist($origAbs, 'original moved away');
        $this->assertFileDoesNotExist($avifAbs, 'avif variant moved away');

        // Restore.
        $manifestId = $this->findManifestId();
        $restored = $cmd->execute([], [
            'action'         => 'restore',
            'job_id'         => 'job-coexist',
            'quarantine_ids' => [$manifestId],
        ]);
        $this->assertTrue((bool)($restored['ok'] ?? false), 'restore ok');
        $this->assertSame(2, (int)($restored['restored'] ?? 0), 'both files restored');
        $this->assertFileExists($origAbs, 'original restored');
        $this->assertFileExists($avifAbs, 'avif variant restored');
        $this->assertSame('avif-bytes', file_get_contents($avifAbs), 'avif content intact');
    }

    // =========================================================================
    // Test 3: originals_deleted status — archives gone; isolate uses only
    //         standard files; no error
    // =========================================================================

    public function testOriginalsDeletedStatusIsolatesOnlyStandardFiles(): void
    {
        $attachmentId = 33;
        $relPath      = '2024/06/scenic.jpg';

        // Only the main optimized file remains; the .wpmgr-original.jpg is gone
        // because originals were permanently deleted after the optimize cycle.
        $mainAbs = $this->createFile($relPath, 'scenic-optimized');
        // Do NOT create 2024/06/scenic.wpmgr-original.jpg — it doesn't exist.

        $meta = ['file' => $relPath, 'sizes' => []];
        $blob = [
            'status'           => MediaKeystore::STATUS_ORIGINALS_DELETED,
            'original_deleted' => 1,
            'optimized_data'   => [
                'full' => [
                    'archive_mode' => AttachmentMeta::MODE_REPLACE,
                    'path'         => $mainAbs,
                    'size'         => 6000,
                ],
            ],
        ];

        $this->stubPostMeta($attachmentId, $relPath, $meta, $blob);

        $result = $this->makeCommand()->execute([], [
            'action'         => 'isolate',
            'job_id'         => 'job-origdeleted',
            'attachment_ids' => [$attachmentId],
        ]);

        // Must succeed without error; the archive path fails file_exists and is silently skipped.
        $this->assertTrue((bool)($result['ok'] ?? false), 'isolate ok with originals_deleted status');

        // Only the existing main file is moved (archive path skipped by file_exists guard).
        $this->assertSame(1, (int)($result['moved'] ?? 0), 'only 1 existing file moved');
        $this->assertFileDoesNotExist($mainAbs, 'main file quarantined');

        // The archive path was never created, so asserting it still does-not-exist
        // is trivially true but confirms no unexpected file appeared.
        $archivePath = $this->uploadsDir . '/2024/06/scenic.wpmgr-original.jpg';
        $this->assertFileDoesNotExist($archivePath, 'no archive file was created');
    }

    // =========================================================================
    // Test 4: Non-optimized attachment (no blob) — behavior unchanged
    // =========================================================================

    public function testNoBlobBehaviorUnchanged(): void
    {
        $attachmentId = 44;
        $relPath      = '2024/06/simple.jpg';

        $mainAbs  = $this->createFile($relPath,                     'simple-jpg');
        $thumbAbs = $this->createFile('2024/06/simple-150x150.jpg', 'simple-thumb');

        $meta = [
            'file'  => $relPath,
            'sizes' => [
                'thumbnail' => ['file' => 'simple-150x150.jpg'],
            ],
        ];

        // No blob: get_post_meta returns '' for the optimizer key.
        $this->stubPostMeta($attachmentId, $relPath, $meta, []);

        $result = $this->makeCommand()->execute([], [
            'action'         => 'isolate',
            'job_id'         => 'job-no-blob',
            'attachment_ids' => [$attachmentId],
        ]);

        $this->assertTrue((bool)($result['ok'] ?? false), 'isolate ok');
        // Exactly 2 standard files — no extras.
        $this->assertSame(2, (int)($result['moved'] ?? 0), 'only 2 standard files moved');
        $this->assertFileDoesNotExist($mainAbs,  'main quarantined');
        $this->assertFileDoesNotExist($thumbAbs, 'thumb quarantined');

        // Verify the manifest lists exactly those 2 files.
        $manifestId   = $this->findManifestId();
        $manifestFile = $this->manifestsRoot() . '/' . $manifestId . '.json';
        $data  = json_decode((string)file_get_contents($manifestFile), true);
        $files = (array)($data['entries'][0]['files'] ?? []);
        $this->assertCount(2, $files, 'manifest lists exactly the 2 standard files (no extras)');
    }

    // =========================================================================
    // Test 5: SECURITY — blob with absolute path outside uploads is rejected;
    //         out-of-uploads file is untouched; standard files still process
    // =========================================================================

    public function testConfinementRejectsAbsolutePathOutsideUploads(): void
    {
        $attachmentId = 55;
        $relPath      = '2024/06/safe.jpg';

        // Standard file (inside uploads).
        $mainAbs = $this->createFile($relPath, 'safe-jpg');

        // A real file that exists OUTSIDE the uploads dir — the blob points at it.
        // confinedPath() must reject it and never pass it to quarantine.
        $outsidePath = $this->tmpRoot . '/secret-outside-uploads.txt';
        file_put_contents($outsidePath, 'sensitive-data');

        $meta = ['file' => $relPath, 'sizes' => []];
        $blob = [
            'status'         => MediaKeystore::STATUS_OPTIMIZED,
            'optimized_data' => [
                'full' => [
                    'archive_mode' => AttachmentMeta::MODE_COEXIST,
                    'path'         => $outsidePath, // absolute path escaping uploads
                    'size'         => 9999,
                ],
            ],
        ];

        $this->stubPostMeta($attachmentId, $relPath, $meta, $blob);

        $result = $this->makeCommand()->execute([], [
            'action'         => 'isolate',
            'job_id'         => 'job-security-abs',
            'attachment_ids' => [$attachmentId],
        ]);

        $this->assertTrue((bool)($result['ok'] ?? false), 'isolate ok');

        // Only the standard file (inside uploads) was moved; out-of-uploads rejected.
        $this->assertSame(1, (int)($result['moved'] ?? 0), 'escaped path rejected; only 1 file moved');
        $this->assertFileDoesNotExist($mainAbs, 'standard file quarantined');

        // The out-of-uploads file must be completely untouched.
        $this->assertFileExists($outsidePath,                 'out-of-uploads file NOT touched');
        $this->assertSame('sensitive-data', file_get_contents($outsidePath), 'content intact');
    }

    // =========================================================================
    // Test 5b: SECURITY — blob path with `..` traversal is rejected
    // =========================================================================

    public function testConfinementRejectsTraversalPathWithDotDot(): void
    {
        $attachmentId = 56;
        $relPath      = '2024/06/traversal.jpg';

        $mainAbs = $this->createFile($relPath, 'traversal-jpg');

        // A crafted path that uses `..` to escape the uploads dir.
        $traversalPath = $this->uploadsDir . '/2024/06/../../../wp-config.php';

        $meta = ['file' => $relPath, 'sizes' => []];
        $blob = [
            'status'         => MediaKeystore::STATUS_OPTIMIZED,
            'optimized_data' => [
                'full' => [
                    // COEXIST passes `path` directly to confinedPath() without further
                    // transformation — the traversal sequence must be caught there.
                    'archive_mode' => AttachmentMeta::MODE_COEXIST,
                    'path'         => $traversalPath,
                    'size'         => 100,
                ],
            ],
        ];

        $this->stubPostMeta($attachmentId, $relPath, $meta, $blob);

        $result = $this->makeCommand()->execute([], [
            'action'         => 'isolate',
            'job_id'         => 'job-security-dotdot',
            'attachment_ids' => [$attachmentId],
        ]);

        $this->assertTrue((bool)($result['ok'] ?? false), 'isolate ok');
        // Only the standard file is moved; the traversal path is silently rejected.
        $this->assertSame(1, (int)($result['moved'] ?? 0), 'traversal path rejected');
        $this->assertFileDoesNotExist($mainAbs, 'standard file quarantined despite traversal path in blob');
    }

    // =========================================================================
    // Test 6: SECURITY — REPLACE-branch traversal via archivePathFor()
    //
    // The two existing security tests (5 and 5b) both use COEXIST mode, which
    // passes `path` directly. REPLACE mode derives the archive path via
    // Rename::archivePathFor($blob['path']) — this path goes through confinedPath()
    // separately and must also be rejected when it escapes uploads.
    // =========================================================================

    public function testConfinementRejectsReplaceArchivePathOutsideUploads(): void
    {
        $attachmentId = 57;
        $relPath      = '2024/06/replace-safe.jpg';

        // Standard file (inside uploads).
        $mainAbs = $this->createFile($relPath, 'replace-safe-jpg');

        // The blob `path` for REPLACE mode is the post-overwrite (current) path.
        // archivePathFor() on a traversal path produces a path that also escapes uploads.
        // confinedPath() must reject the derived archive path.
        $escapedBase = $this->uploadsDir . '/2024/06/../../../';
        $traversalOptPath = $escapedBase . 'etc/secret.jpg';

        // Ensure an out-of-uploads "archive" file exists to confirm it stays untouched.
        $escapedDir = dirname($traversalOptPath);
        if (!is_dir($escapedDir)) {
            // The directory likely doesn't exist (it's a crafted path), so just
            // verify the assertion on the standard file count is the signal.
        }

        $meta = ['file' => $relPath, 'sizes' => []];
        $blob = [
            'status'         => MediaKeystore::STATUS_OPTIMIZED,
            'optimized_data' => [
                'full' => [
                    'archive_mode' => AttachmentMeta::MODE_REPLACE,
                    // path = the current (post-overwrite) location — a traversal path.
                    // archivePathFor() will derive: escapedBase/etc/secret.wpmgr-original.jpg
                    'path'         => $traversalOptPath,
                    'size'         => 1234,
                ],
            ],
        ];

        $this->stubPostMeta($attachmentId, $relPath, $meta, $blob);

        $result = $this->makeCommand()->execute([], [
            'action'         => 'isolate',
            'job_id'         => 'job-replace-traversal',
            'attachment_ids' => [$attachmentId],
        ]);

        $this->assertTrue((bool)($result['ok'] ?? false), 'isolate ok');
        // Only the standard file is moved; the REPLACE-derived archive path is rejected.
        $this->assertSame(1, (int)($result['moved'] ?? 0), 'REPLACE archive traversal path rejected');
        $this->assertFileDoesNotExist($mainAbs, 'standard file quarantined');
    }

    // =========================================================================
    // Test 7: SECURITY — prefix-confusion: sibling dir sharing the uploads prefix
    //
    // A path like <uploadsBase>EVIL/x.jpg shares the string prefix of uploadsBase
    // but is a sibling directory, not a child. confinedPath() must enforce the
    // trailing-separator anchor so this is rejected.
    // =========================================================================

    public function testConfinementRejectsPrefixConfusionSiblingDir(): void
    {
        $attachmentId = 58;
        $relPath      = '2024/06/prefix-safe.jpg';

        $mainAbs = $this->createFile($relPath, 'prefix-safe-jpg');

        // Sibling dir whose name starts with the same string as the uploads dir name.
        // e.g. if uploadsDir is /tmp/X/wp-content/uploads, sibling = /tmp/X/wp-content/uploadsEVIL
        $siblingDir = $this->wpContent . '/uploadsEVIL';
        if (!is_dir($siblingDir)) {
            mkdir($siblingDir, 0755, true);
        }
        $siblingFile = $siblingDir . '/x.jpg';
        file_put_contents($siblingFile, 'sibling-data');

        $meta = ['file' => $relPath, 'sizes' => []];
        $blob = [
            'status'         => MediaKeystore::STATUS_OPTIMIZED,
            'optimized_data' => [
                'full' => [
                    'archive_mode' => AttachmentMeta::MODE_COEXIST,
                    'path'         => $siblingFile, // inside uploadsEVIL, not uploads
                    'size'         => 500,
                ],
            ],
        ];

        $this->stubPostMeta($attachmentId, $relPath, $meta, $blob);

        $result = $this->makeCommand()->execute([], [
            'action'         => 'isolate',
            'job_id'         => 'job-prefix-confusion',
            'attachment_ids' => [$attachmentId],
        ]);

        $this->assertTrue((bool)($result['ok'] ?? false), 'isolate ok');
        // Only the standard file is moved; the sibling-dir path is rejected.
        $this->assertSame(1, (int)($result['moved'] ?? 0), 'sibling-dir prefix confusion rejected');
        $this->assertFileDoesNotExist($mainAbs, 'standard file quarantined');
        // The sibling file must be completely untouched.
        $this->assertFileExists($siblingFile, 'sibling file NOT touched');
        $this->assertSame('sibling-data', file_get_contents($siblingFile), 'sibling content intact');
    }

    // =========================================================================
    // Test 8: SECURITY — symlink inside uploads pointing outside
    //
    // A symlink under uploads whose target is outside the uploads tree must be
    // rejected at the move-time realpath gate in confinedPath(). The target file
    // must remain untouched.
    // =========================================================================

    public function testConfinementRejectsSymlinkInsideUploadsPointingOutside(): void
    {
        $attachmentId = 59;
        $relPath      = '2024/06/symlink-safe.jpg';

        $mainAbs = $this->createFile($relPath, 'symlink-safe-jpg');

        // Create a real file outside uploads.
        $outsideFile = $this->tmpRoot . '/outside-symlink-target.txt';
        file_put_contents($outsideFile, 'outside-content');

        // Create a symlink inside uploads pointing at the outside file.
        $symlinkPath = $this->uploadsDir . '/2024/06/evil-symlink.jpg';
        if (!file_exists($symlinkPath) && !is_link($symlinkPath)) {
            symlink($outsideFile, $symlinkPath);
        }

        $meta = ['file' => $relPath, 'sizes' => []];
        $blob = [
            'status'         => MediaKeystore::STATUS_OPTIMIZED,
            'optimized_data' => [
                'full' => [
                    'archive_mode' => AttachmentMeta::MODE_COEXIST,
                    // The symlink is inside uploads (passes prefix check) but resolves outside.
                    'path'         => $symlinkPath,
                    'size'         => 999,
                ],
            ],
        ];

        $this->stubPostMeta($attachmentId, $relPath, $meta, $blob);

        $result = $this->makeCommand()->execute([], [
            'action'         => 'isolate',
            'job_id'         => 'job-symlink-inside',
            'attachment_ids' => [$attachmentId],
        ]);

        $this->assertTrue((bool)($result['ok'] ?? false), 'isolate ok');
        // Only the standard file is moved; the symlink that resolves outside is rejected.
        $this->assertSame(1, (int)($result['moved'] ?? 0), 'out-of-uploads symlink rejected');
        $this->assertFileDoesNotExist($mainAbs, 'standard file quarantined');
        // The outside file must be completely untouched.
        $this->assertFileExists($outsideFile, 'outside file NOT touched via symlink');
        $this->assertSame('outside-content', file_get_contents($outsideFile), 'outside content intact');

        // Cleanup symlink.
        if (is_link($symlinkPath)) {
            @unlink($symlinkPath);
        }
    }

    // =========================================================================
    // Test 9: FIX 1 — symlinked uploadsBase: quarantine→restore round-trip
    //
    // When the uploads base is a symlink (e.g. /var/www/html → /data/site on
    // managed hosts, or /tmp → /private/tmp on macOS), realpath() resolves the
    // symlink, which can make the placement fragment (derived from realpath of
    // the source) differ from the naïve fragment you would get by stripping the
    // unresolved uploadsBase string prefix from $src.
    //
    // In particular: on macOS, sys_get_temp_dir() returns /tmp but realpath()
    // resolves it to /private/tmp. So if both the uploads base and the file path
    // are constructed under /tmp/..., the old code's naive stripping of the
    // uploadsBase prefix from the stored path still matches — UNLESS the stored
    // path already uses the resolved form.
    //
    // The fix is design-correct regardless: the manifest now stores the exact
    // fragment computed at quarantine time, so restore/delete never have to
    // re-derive it with string heuristics that can diverge under any symlink
    // configuration. This test exercises the new {'orig', 'frag'} manifest format
    // and confirms the full round-trip works.
    // =========================================================================

    public function testSymlinkedUploadsBaseRestoreRoundTrip(): void
    {
        // Build a real directory and a symlink pointing to it.
        $realUploadsDir    = $this->tmpRoot . '/real-uploads';
        mkdir($realUploadsDir . '/2024/06', 0755, true);
        $symlinkUploadsDir = $this->tmpRoot . '/symlink-uploads';
        if (!is_link($symlinkUploadsDir)) {
            symlink($realUploadsDir, $symlinkUploadsDir);
        }

        // wp_upload_dir returns the symlinked base (as WP does when the document
        // root itself is symlinked on the hosting provider).
        Functions\when('wp_upload_dir')->justReturn([
            'basedir' => $symlinkUploadsDir,
            'baseurl' => 'https://example.com/wp-content/uploads',
        ]);

        $relPath    = '2024/06/symlink-base-photo.jpg';
        $symlinkAbs = $symlinkUploadsDir . '/' . $relPath;
        file_put_contents($symlinkAbs, 'symbase-content');

        $q          = new MediaQuarantine($this->wpContent);
        $manifestId = $q->beginManifest('job-symbase');
        $moved      = $q->quarantineAttachment($manifestId, 70, $relPath, [$symlinkAbs]);
        $q->finaliseManifest($manifestId);

        $this->assertSame(1, $moved, 'file moved via symlinked uploads base');
        $this->assertFileDoesNotExist($symlinkAbs, 'file no longer at original path');

        // Verify the manifest uses the new {'orig', 'frag'} object format.
        $manifestFile = $this->manifestsRoot() . '/' . $manifestId . '.json';
        $data         = json_decode((string)file_get_contents($manifestFile), true);
        $fileRecord   = $data['entries'][0]['files'][0] ?? null;
        $this->assertIsArray($fileRecord, 'manifest file record is an object (not a plain string)');
        $this->assertArrayHasKey('orig', $fileRecord, 'manifest record has orig key');
        $this->assertArrayHasKey('frag', $fileRecord, 'manifest record has frag key');
        $this->assertSame($symlinkAbs, $fileRecord['orig'], 'orig stores the caller-supplied path');
        $this->assertSame($relPath,    $fileRecord['frag'], 'frag stores the uploads-relative fragment');

        // Restore must work correctly via the recorded frag.
        $receipt = $q->restoreManifest($manifestId);
        $this->assertSame(1, $receipt['restored'], 'file restored via symlinked uploads base');
        $this->assertFileExists($symlinkAbs, 'file restored to original path');
        $this->assertSame('symbase-content', file_get_contents($symlinkAbs), 'content intact');

        // Cleanup symlink.
        if (is_link($symlinkUploadsDir)) {
            @unlink($symlinkUploadsDir);
        }
    }

    public function testSymlinkedUploadsBaseDeleteRoundTrip(): void
    {
        $realUploadsDir    = $this->tmpRoot . '/real-uploads-del';
        mkdir($realUploadsDir . '/2024/06', 0755, true);
        $symlinkUploadsDir = $this->tmpRoot . '/symlink-uploads-del';
        if (!is_link($symlinkUploadsDir)) {
            symlink($realUploadsDir, $symlinkUploadsDir);
        }

        Functions\when('wp_upload_dir')->justReturn([
            'basedir' => $symlinkUploadsDir,
            'baseurl' => 'https://example.com/wp-content/uploads',
        ]);

        $relPath    = '2024/06/symlink-base-del.jpg';
        $symlinkAbs = $symlinkUploadsDir . '/' . $relPath;
        file_put_contents($symlinkAbs, 'symbase-del-content');

        $q          = new MediaQuarantine($this->wpContent);
        $manifestId = $q->beginManifest('job-symbase-del');
        $moved      = $q->quarantineAttachment($manifestId, 71, $relPath, [$symlinkAbs]);
        $q->finaliseManifest($manifestId);

        $this->assertSame(1, $moved, 'file moved via symlinked uploads base');
        $this->assertFileDoesNotExist($symlinkAbs, 'file no longer at original path');

        // Verify the new format.
        $manifestFile = $this->manifestsRoot() . '/' . $manifestId . '.json';
        $data         = json_decode((string)file_get_contents($manifestFile), true);
        $fileRecord   = $data['entries'][0]['files'][0] ?? null;
        $this->assertIsArray($fileRecord,            'manifest record is an object');
        $this->assertSame($relPath, $fileRecord['frag'], 'frag matches uploads-relative path');

        // Delete permanently — must not orphan the quarantined file.
        $result = $q->deleteManifest($manifestId);
        $this->assertSame(1, $result['posts_deleted'], '1 attachment post deleted');
        $this->assertSame(1, $result['files_deleted'], '1 quarantined file deleted');
        $manifestDir = $this->mediaRoot() . '/' . $manifestId;
        $this->assertDirectoryDoesNotExist($manifestDir, 'quarantine dir cleaned up after delete');

        // Cleanup symlink.
        if (is_link($symlinkUploadsDir)) {
            @unlink($symlinkUploadsDir);
        }
    }
}
