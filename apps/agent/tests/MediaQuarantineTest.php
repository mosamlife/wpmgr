<?php
/**
 * MediaQuarantineTest — verifies the quarantine directory lifecycle.
 *
 * All filesystem operations use an isolated temp directory so tests are fully
 * self-contained and clean up after themselves.
 *
 * Covers:
 *   - beginManifest() creates quarantine root + .htaccess + index.php + manifest dir.
 *   - quarantineAttachment() refuses src files outside the uploads root (containment).
 *   - quarantineAttachment() moves the file and records the original path.
 *   - quarantineAttachment() records a manifest entry even when 0 files are moved
 *     (broken/missing-file attachment) so the post can always be deleted later.
 *   - finaliseManifest() writes a valid JSON manifest outside the media/ tree.
 *   - finaliseManifest() — media/ subtree contains only image files (no JSON).
 *   - restoreManifest() returns 0 for an unknown manifest_id.
 *   - restoreManifest() moves files back and removes the manifest dir.
 *   - restoreManifest() never moves a quarantined file onto an original path that
 *     something else now occupies, and reports the skip with a reason code.
 *   - restoreManifest() does not delete a quarantined file it refused to restore —
 *     cleanup runs only on a complete restore, so a partial one keeps every copy.
 *   - restoreManifest() withholds `complete` while the quarantine tree still holds
 *     anything the counters cannot see: an unrecorded file, an entry no stat can
 *     classify, or a stray beside files/ that cleanup would delete unseen.
 *   - restoreManifest() never reports `complete` for a manifest id that named nothing.
 *   - restoreManifest() handles entries with empty files[] cleanly (0 files back, no error).
 *   - deleteManifest() returns a zero-count result for an unknown manifest_id.
 *   - deleteManifest() removes quarantined files, calls wp_delete_attachment,
 *     and removes the manifest dir; returns posts_deleted=1 files_deleted=1.
 *   - deleteManifest() on a manifest with 0-file entry still calls wp_delete_attachment
 *     and reports posts_deleted=1, files_deleted=0.
 *   - loadManifest() (via restoreManifest) rejects path-traversal manifest_ids.
 *   - normalisePath() rejects paths containing "/.." (belt-and-braces traversal guard).
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Media\MediaQuarantine;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Media\MediaQuarantine
 */
final class MediaQuarantineTest extends TestCase
{
    /** Temp root that acts as wp-content for this test run. */
    private string $wpContent = '';

    /** Temp uploads dir (simulates wp-content/uploads). */
    private string $uploadsDir = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        // Create an isolated temp tree per test.
        $tmp = sys_get_temp_dir() . '/wpmgr-qt-' . bin2hex(random_bytes(6));
        $this->wpContent  = $tmp . '/wp-content';
        $this->uploadsDir = $this->wpContent . '/uploads';
        mkdir($this->uploadsDir . '/2024/01', 0755, true);

        // Stub wp_upload_dir for quarantine internals.
        Functions\when('wp_upload_dir')->justReturn([
            'basedir' => $this->uploadsDir,
            'baseurl' => 'https://example.com/wp-content/uploads',
        ]);

        // wp_json_encode — used by finaliseManifest().
        Functions\when('wp_json_encode')->alias(static function ($data, int $flags = 0): string|false {
            return json_encode($data, $flags);
        });

        // wp_delete_file — used by deleteManifest().
        Functions\when('wp_delete_file')->alias(static function (string $path): void {
            @unlink($path);
        });

        // wp_delete_attachment — used by deleteManifest().
        Functions\when('wp_delete_attachment')->justReturn(true);
    }

    protected function tear_down(): void
    {
        $this->rrmdir(dirname($this->wpContent));
        Monkey\tearDown();
        parent::tear_down();
    }

    // =========================================================================
    // Helpers
    // =========================================================================

    /**
     * Build a MediaQuarantine instance that writes to this test's isolated
     * wp-content directory, regardless of whether WP_CONTENT_DIR is defined
     * by another test in the suite.
     */
    private function makeQuarantine(): MediaQuarantine
    {
        return new MediaQuarantine($this->wpContent);
    }

    private function createUploadFile(string $relPath, string $content = 'fake-image-data'): string
    {
        $abs = $this->uploadsDir . '/' . ltrim($relPath, '/');
        $dir = dirname($abs);
        if (!is_dir($dir)) {
            mkdir($dir, 0755, true);
        }
        file_put_contents($abs, $content);
        return $abs;
    }

    private function quarantineRoot(): string
    {
        return $this->wpContent . '/' . MediaQuarantine::QUARANTINE_DIR;
    }

    private function mediaRoot(): string
    {
        return $this->quarantineRoot() . '/' . MediaQuarantine::MEDIA_SUBDIR;
    }

    private function manifestsRoot(): string
    {
        return $this->quarantineRoot() . '/' . MediaQuarantine::MANIFESTS_SUBDIR;
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
    // beginManifest() — creates root + guards + manifest dir
    // =========================================================================

    public function testBeginManifestCreatesQuarantineRootWithGuards(): void
    {
        $q          = $this->makeQuarantine();
        $manifestId = $q->beginManifest('job-001');

        // Quarantine root exists.
        $this->assertDirectoryExists($this->quarantineRoot());

        // .htaccess blocks web access.
        $htaccess = $this->quarantineRoot() . '/.htaccess';
        $this->assertFileExists($htaccess);
        $this->assertStringContainsString('Deny', (string)file_get_contents($htaccess));

        // PHP silence guard.
        $index = $this->quarantineRoot() . '/index.php';
        $this->assertFileExists($index);
        $this->assertStringContainsString('Silence', (string)file_get_contents($index));

        // Media root exists.
        $this->assertDirectoryExists($this->mediaRoot());

        // Manifests root exists (outside media/).
        $this->assertDirectoryExists($this->manifestsRoot());

        // Media files sub-dir exists (only image files here, no manifest JSON).
        $this->assertDirectoryExists($this->mediaRoot() . '/' . $manifestId . '/files');

        // manifest_id is a 32-char lowercase hex string.
        $this->assertMatchesRegularExpression('/^[0-9a-f]{32}$/', $manifestId);
    }

    // =========================================================================
    // quarantineAttachment() — containment: refuses src outside uploads root
    // =========================================================================

    public function testQuarantineAttachmentRefusesSrcOutsideUploadsRoot(): void
    {
        $q          = $this->makeQuarantine();
        $manifestId = $q->beginManifest('job-contain');

        // Create a file that is in /tmp — outside the uploads root.
        $outsideFile = sys_get_temp_dir() . '/outside-' . bin2hex(random_bytes(4)) . '.jpg';
        file_put_contents($outsideFile, 'not-in-uploads');

        $moved = $q->quarantineAttachment($manifestId, 1, '2024/01/outside.jpg', [$outsideFile]);

        @unlink($outsideFile);

        $this->assertSame(0, $moved, 'Files outside the uploads root must NOT be moved.');

        // Finalise and verify: the entry IS recorded (with empty files) so the
        // attachment post can still be deleted later. The file itself is NOT moved.
        $q->finaliseManifest($manifestId);
        $manifestFile = $this->manifestsRoot() . '/' . $manifestId . '.json';
        $data = json_decode((string)file_get_contents($manifestFile), true);
        $this->assertIsArray($data);
        $this->assertCount(1, $data['entries'], 'Entry must be recorded even when all files failed containment.');
        $this->assertSame(1, $data['entries'][0]['attachment_id']);
        $this->assertEmpty($data['entries'][0]['files'], 'files[] must be empty: no files passed containment.');
        // Confirm nothing was quarantined: the media files directory is empty.
        $quarantinedFilesDir = $this->mediaRoot() . '/' . $manifestId . '/files';
        $dirEntries = array_diff((array)scandir($quarantinedFilesDir), ['.', '..']);
        $this->assertEmpty($dirEntries, 'No files must appear in the quarantine media dir after containment rejection.');
    }

    // =========================================================================
    // quarantineAttachment() — moves file + records original path
    // =========================================================================

    public function testQuarantineAttachmentMovesFileAndRecordsPath(): void
    {
        $relPath = '2024/01/hero.jpg';
        $srcAbs  = $this->createUploadFile($relPath, 'jpeg-bytes');

        $q          = $this->makeQuarantine();
        $manifestId = $q->beginManifest('job-move');
        $moved      = $q->quarantineAttachment($manifestId, 42, $relPath, [$srcAbs]);

        $this->assertSame(1, $moved);

        // Source file is gone from uploads.
        $this->assertFileDoesNotExist($srcAbs);

        // File exists in the quarantine tree, preserving the sub-path.
        $quarantined = $this->mediaRoot() . '/' . $manifestId . '/files/' . $relPath;
        $this->assertFileExists($quarantined);
        $this->assertSame('jpeg-bytes', file_get_contents($quarantined));
    }

    // =========================================================================
    // finaliseManifest() — writes valid JSON manifest outside media/ tree
    // =========================================================================

    public function testFinaliseManifestWritesValidJson(): void
    {
        $relPath = '2024/01/banner.jpg';
        $srcAbs  = $this->createUploadFile($relPath, 'img-data');

        $q          = $this->makeQuarantine();
        $manifestId = $q->beginManifest('job-json');
        $q->quarantineAttachment($manifestId, 77, $relPath, [$srcAbs]);
        $q->finaliseManifest($manifestId);

        // Manifest JSON must live in manifests/ (outside media/).
        $manifestFile = $this->manifestsRoot() . '/' . $manifestId . '.json';
        $this->assertFileExists($manifestFile);

        $data = json_decode((string)file_get_contents($manifestFile), true);
        $this->assertIsArray($data);
        $this->assertSame(1, $data['v']);
        $this->assertSame($manifestId, $data['id']);
        $this->assertSame('job-json', $data['job_id']);
        $this->assertIsInt($data['ts']);
        $this->assertCount(1, $data['entries']);

        $entry = $data['entries'][0];
        $this->assertSame(77, $entry['attachment_id']);
        $this->assertSame($relPath, $entry['rel_path']);
        // Each file record is now {"orig": <abs-path>, "frag": <relative-fragment>}.
        $this->assertCount(1, $entry['files']);
        $this->assertSame($srcAbs, $entry['files'][0]['orig']);
        $this->assertSame($relPath, $entry['files'][0]['frag']);
    }

    public function testFinaliseManifestDoesNotPlaceJsonInsideMediaTree(): void
    {
        $relPath = '2024/01/nginx-safe.jpg';
        $srcAbs  = $this->createUploadFile($relPath, 'img-data');

        $q          = $this->makeQuarantine();
        $manifestId = $q->beginManifest('job-nginx');
        $q->quarantineAttachment($manifestId, 99, $relPath, [$srcAbs]);
        $q->finaliseManifest($manifestId);

        // The media/<manifest_id>/ directory must contain only files/ — no .json.
        $mediaManifestDir = $this->mediaRoot() . '/' . $manifestId;
        $this->assertDirectoryExists($mediaManifestDir);
        $entries = array_diff((array)scandir($mediaManifestDir), ['.', '..']);
        foreach ($entries as $entry) {
            $this->assertFalse(
                str_ends_with((string)$entry, '.json'),
                "No JSON files must exist inside the media/<manifest_id>/ directory (nginx-safety). Found: {$entry}"
            );
        }
    }

    // =========================================================================
    // restoreManifest() — unknown manifest_id returns 0
    // =========================================================================

    public function testRestoreManifestUnknownIdReturnsZero(): void
    {
        $q = $this->makeQuarantine();
        // Ensure the quarantine root exists (needed for realpath in loadManifest).
        $q->beginManifest('warmup');

        $receipt = $q->restoreManifest('00000000000000000000000000000000');

        $this->assertSame(0, $receipt['restored']);
    }

    // =========================================================================
    // restoreManifest() — moves files back + removes manifest dir
    // =========================================================================

    public function testRestoreManifestMovesFilesBackAndRemovesDir(): void
    {
        $relPath = '2024/01/hero.jpg';
        $srcAbs  = $this->createUploadFile($relPath, 'restore-me');

        $q          = $this->makeQuarantine();
        $manifestId = $q->beginManifest('job-restore');
        $q->quarantineAttachment($manifestId, 42, $relPath, [$srcAbs]);
        $q->finaliseManifest($manifestId);

        // Confirm file is gone from uploads.
        $this->assertFileDoesNotExist($srcAbs);

        $receipt = $q->restoreManifest($manifestId);

        $this->assertSame(1, $receipt['restored']);
        $this->assertTrue($receipt['complete'], 'a restore that put everything back is complete');

        // File is back in uploads.
        $this->assertFileExists($srcAbs);
        $this->assertSame('restore-me', file_get_contents($srcAbs));

        // Manifest dir is removed after successful restore.
        $manifestDir = $this->mediaRoot() . '/' . $manifestId;
        $this->assertDirectoryDoesNotExist($manifestDir);
    }

    // =========================================================================
    // restoreManifest() — never moves a quarantined file over a file that
    // occupies the original path
    // =========================================================================

    public function testRestoreManifestDoesNotOverwriteANewerFileAtTheOriginalPath(): void
    {
        $relPath = '2024/01/hero.jpg';
        $srcAbs  = $this->createUploadFile($relPath, 'quarantined-bytes');

        $q          = $this->makeQuarantine();
        $manifestId = $q->beginManifest('job-restore-occupied');
        $q->quarantineAttachment($manifestId, 42, $relPath, [$srcAbs]);
        $q->finaliseManifest($manifestId);

        // The original path is empty: quarantineAttachment() MOVES the bytes,
        // so the quarantined copy is the only copy that exists.
        $this->assertFileDoesNotExist($srcAbs);

        // A different file comes to occupy that exact path while the
        // attachment is isolated.
        $this->createUploadFile($relPath, 'newer-bytes-written-after-isolation');

        $receipt = $q->restoreManifest($manifestId);

        // Load-bearing: the occupant is the only copy of its own bytes.
        $this->assertSame(
            'newer-bytes-written-after-isolation',
            file_get_contents($srcAbs),
            'restore must not move a quarantined file onto an occupied original path'
        );

        // The refused file is likewise the only copy of itself, so it must
        // survive the refusal rather than be cleaned up behind it.
        $quarantinedCopy = $this->mediaRoot() . '/' . $manifestId . '/files/' . $relPath;
        $this->assertFileExists($quarantinedCopy, 'the refused file stays in quarantine');
        $this->assertSame('quarantined-bytes', file_get_contents($quarantinedCopy));

        $this->assertFileExists(
            $this->manifestsRoot() . '/' . $manifestId . '.json',
            'the manifest survives an incomplete restore so the operator can retry'
        );

        $this->assertSame(0, $receipt['restored'], 'nothing was restored');
        $this->assertSame(1, $receipt['skipped'], 'the contested file counts as skipped');
        $this->assertSame(
            1,
            $receipt['reasons'][MediaQuarantine::RESTORE_SKIP_DESTINATION_OCCUPIED] ?? 0,
            'the skip is attributed to the occupied destination'
        );
        $this->assertFalse($receipt['complete'], 'an incomplete restore never reports complete');
    }

    // =========================================================================
    // restoreManifest() — a file it refused to restore is never then deleted
    // by the cleanup gate
    // =========================================================================

    public function testRestoreManifestDoesNotDeleteAQuarantinedFileItRefusedToRestore(): void
    {
        $mainRel  = '2024/01/gallery.jpg';
        $thumbRel = '2024/01/gallery-150x150.jpg';

        $mainAbs  = $this->createUploadFile($mainRel,  'main-bytes');
        $thumbAbs = $this->createUploadFile($thumbRel, 'thumb-bytes');

        // One entry, two files — the shape isolate produces for an attachment
        // with sub-sizes.
        $q          = $this->makeQuarantine();
        $manifestId = $q->beginManifest('job-restore-partial');
        $q->quarantineAttachment($manifestId, 42, $mainRel, [$mainAbs, $thumbAbs]);
        $q->finaliseManifest($manifestId);

        $this->assertFileDoesNotExist($mainAbs);
        $this->assertFileDoesNotExist($thumbAbs);

        // Only the main file's original path is taken.
        $this->createUploadFile($mainRel, 'newer-main-bytes');

        $receipt = $q->restoreManifest($manifestId);

        $this->assertSame(
            'newer-main-bytes',
            file_get_contents($mainAbs),
            'the occupant survives'
        );

        // The guard must not block correct work: the uncontested file still
        // goes back, with its original content.
        $this->assertFileExists($thumbAbs, 'the uncontested file is still restored');
        $this->assertSame('thumb-bytes', file_get_contents($thumbAbs), 'restored content intact');

        // Load-bearing: cleanup must not delete the copy restore declined to
        // move. A partial restore that then removes the manifest directory
        // destroys the only remaining copy of the refused file.
        $refusedCopy = $this->mediaRoot() . '/' . $manifestId . '/files/' . $mainRel;
        $this->assertFileExists(
            $refusedCopy,
            'a quarantined file restore refused to move must not then be deleted'
        );
        $this->assertSame('main-bytes', file_get_contents($refusedCopy));

        $this->assertFileExists(
            $this->manifestsRoot() . '/' . $manifestId . '.json',
            'the manifest survives so the refused file can still be recovered'
        );

        $this->assertSame(1, $receipt['restored'], 'the uncontested file counts as restored');
        $this->assertSame(1, $receipt['skipped'], 'the contested file counts as skipped');
        $this->assertFalse($receipt['complete'], 'a partial restore never reports complete');
    }

    // =========================================================================
    // restoreManifest() — the physical scan, the half of `complete` the
    // restore counters cannot see
    //
    // Every test below leaves something in the quarantine tree that no
    // manifest entry describes, so the counters read clean and only the scan
    // can withhold `complete`. Delete the hasRemainingFiles() term from
    // restoreReceipt() and each of them fails.
    // =========================================================================

    public function testRestoreManifestIsNotCompleteWhileAnUnrecordedFileRemains(): void
    {
        $relPath = '2024/01/hero.jpg';
        $srcAbs  = $this->createUploadFile($relPath, 'restore-me');

        $q          = $this->makeQuarantine();
        $manifestId = $q->beginManifest('job-unrecorded');
        $q->quarantineAttachment($manifestId, 42, $relPath, [$srcAbs]);
        $q->finaliseManifest($manifestId);

        // A file under files/ that no manifest entry names. A fragment
        // derivation that went down a different branch leaves exactly this:
        // bytes in the tree with no countable record anywhere.
        $orphan = $this->mediaRoot() . '/' . $manifestId . '/files/2024/01/orphan.jpg';
        file_put_contents($orphan, 'orphan-bytes');

        $receipt = $q->restoreManifest($manifestId);

        // The counters are clean: every file the manifest described went back.
        $this->assertSame(1, $receipt['restored'], 'the recorded file is restored');
        $this->assertSame(0, $receipt['skipped'], 'nothing was skipped');
        $this->assertSame(0, $receipt['failed'], 'nothing failed');
        $this->assertFileExists($srcAbs, 'the guard does not block correct work');

        // Load-bearing: only the physical scan knows the orphan is there.
        $this->assertFalse(
            $receipt['complete'],
            'a tree that still holds an unrecorded file is not a complete restore'
        );
        $this->assertFileExists($orphan, 'the unrecorded file is not deleted behind the counters');
        $this->assertSame('orphan-bytes', file_get_contents($orphan));
        $this->assertFileExists(
            $this->manifestsRoot() . '/' . $manifestId . '.json',
            'the manifest stays, so the leftover still has something naming it'
        );
    }

    public function testRestoreManifestIsNotCompleteWhileAnUnclassifiableEntryRemains(): void
    {
        $relPath = '2024/01/hero.jpg';
        $srcAbs  = $this->createUploadFile($relPath, 'restore-me');

        $q          = $this->makeQuarantine();
        $manifestId = $q->beginManifest('job-unclassifiable');
        $q->quarantineAttachment($manifestId, 42, $relPath, [$srcAbs]);
        $q->finaliseManifest($manifestId);

        // An entry scandir() lists but that is neither link, file nor
        // directory. A FIFO is the portable way to produce one: unlike the
        // read-bit-without-search-bit directory that motivated this rule, it
        // behaves the same for root, so it is reproducible in a CI container.
        $oddball = $this->mediaRoot() . '/' . $manifestId . '/files/2024/01/oddball';
        if (!function_exists('posix_mkfifo') || !@posix_mkfifo($oddball, 0644)) {
            $this->markTestSkipped('posix_mkfifo() unavailable: cannot create an unclassifiable entry');
        }

        // Refuse to pass vacuously: if the entry is classifiable after all,
        // this test proves nothing about the hazard and must not report green.
        $this->assertTrue(file_exists($oddball), 'the planted entry exists');
        $this->assertFalse(is_link($oddball), 'precondition: not a link');
        $this->assertFalse(is_file($oddball), 'precondition: not a file');
        $this->assertFalse(is_dir($oddball), 'precondition: not a directory');

        $receipt = $q->restoreManifest($manifestId);

        $this->assertSame(1, $receipt['restored'], 'the recorded file is restored');
        $this->assertSame(0, $receipt['skipped'], 'nothing was skipped');
        $this->assertSame(0, $receipt['failed'], 'nothing failed');

        // Load-bearing: an entry no stat can classify is not an empty tree.
        $this->assertFalse(
            $receipt['complete'],
            'an entry that cannot be positively classified counts as remaining'
        );
        $this->assertTrue(file_exists($oddball), 'the unclassified entry is not deleted behind the scan');
    }

    public function testRestoreManifestIsNotCompleteWhileAStrayOutsideFilesRemains(): void
    {
        $relPath = '2024/01/hero.jpg';
        $srcAbs  = $this->createUploadFile($relPath, 'restore-me');

        $q          = $this->makeQuarantine();
        $manifestId = $q->beginManifest('job-stray');
        $q->quarantineAttachment($manifestId, 42, $relPath, [$srcAbs]);
        $q->finaliseManifest($manifestId);

        // Beside files/, not under it. removeManifestDir() deletes the whole
        // media/<manifest_id> tree, so the scan has to cover the whole of it
        // rather than the one sub-directory today's layout happens to use.
        $stray = $this->mediaRoot() . '/' . $manifestId . '/stray.dat';
        file_put_contents($stray, 'stray-bytes');

        $receipt = $q->restoreManifest($manifestId);

        $this->assertSame(1, $receipt['restored'], 'the recorded file is restored');
        $this->assertSame(0, $receipt['skipped'], 'nothing was skipped');
        $this->assertSame(0, $receipt['failed'], 'nothing failed');

        $this->assertFalse(
            $receipt['complete'],
            'a stray beside files/ is inside what the cleanup deletes, so it withholds complete'
        );
        $this->assertFileExists($stray, 'the stray is not deleted unseen');
        $this->assertSame('stray-bytes', file_get_contents($stray));
    }

    public function testRestoreManifestUnknownIdIsNeverComplete(): void
    {
        $q = $this->makeQuarantine();
        $q->beginManifest('warmup');

        $receipt = $q->restoreManifest('00000000000000000000000000000000');

        $this->assertSame(0, $receipt['files_seen'], 'an unknown id describes no files');
        $this->assertSame(0, $receipt['restored'], 'and restores none');

        // `complete` is public receipt API. A caller that gates a delete on it
        // must never read "this id means nothing to me" as "everything was
        // put back".
        $this->assertFalse(
            $receipt['complete'],
            'a receipt for a manifest that never existed is not a complete restore'
        );
    }

    // =========================================================================
    // deleteManifest() — unknown manifest_id returns 0
    // =========================================================================

    public function testDeleteManifestUnknownIdReturnsZeroCounts(): void
    {
        $q = $this->makeQuarantine();
        $q->beginManifest('warmup');

        $result = $q->deleteManifest('00000000000000000000000000000000');
        $this->assertIsArray($result);
        $this->assertSame(0, $result['posts_deleted']);
        $this->assertSame(0, $result['posts_failed']);
        $this->assertSame(0, $result['files_deleted']);
        $this->assertSame(0, $result['entries_processed']);
        $this->assertSame([], $result['results']);
    }

    // =========================================================================
    // deleteManifest() — removes quarantined files + manifest dir
    // =========================================================================

    public function testDeleteManifestRemovesQuarantinedFilesAndDir(): void
    {
        $relPath = '2024/01/delete-me.jpg';
        $srcAbs  = $this->createUploadFile($relPath, 'to-be-deleted');

        $q          = $this->makeQuarantine();
        $manifestId = $q->beginManifest('job-del');
        $q->quarantineAttachment($manifestId, 55, $relPath, [$srcAbs]);
        $q->finaliseManifest($manifestId);

        $quarantined = $this->mediaRoot() . '/' . $manifestId . '/files/' . $relPath;
        $this->assertFileExists($quarantined);

        $result = $q->deleteManifest($manifestId);

        $this->assertSame(1, $result['posts_deleted'], 'One attachment post must be reported deleted.');
        $this->assertSame(0, $result['posts_failed']);
        $this->assertSame(1, $result['files_deleted'], 'One quarantined file must be reported deleted.');
        $this->assertSame(1, $result['entries_processed']);
        $this->assertCount(1, $result['results']);
        $this->assertSame(55, $result['results'][0]['attachment_id']);
        $this->assertTrue($result['results'][0]['post_deleted']);
        $this->assertSame(1, $result['results'][0]['files_deleted']);

        // Quarantined file is permanently removed.
        $this->assertFileDoesNotExist($quarantined);

        // Manifest dir is removed.
        $manifestDir = $this->mediaRoot() . '/' . $manifestId;
        $this->assertDirectoryDoesNotExist($manifestDir);
    }

    // =========================================================================
    // quarantineAttachment() — entry always recorded even when 0 files moved
    // =========================================================================

    public function testQuarantineAttachmentWithNoFilesStillRecordsEntry(): void
    {
        $q          = $this->makeQuarantine();
        $manifestId = $q->beginManifest('job-broken');

        // Pass an attachment whose files do not exist on disk (broken attachment).
        // quarantineAttachment must still record an entry with the attachment_id
        // so that delete can call wp_delete_attachment() later.
        $moved = $q->quarantineAttachment($manifestId, 99, '2024/01/missing.jpg', []);

        $this->assertSame(0, $moved, 'Zero files must be moved for a missing-file attachment.');

        $q->finaliseManifest($manifestId);

        $manifestFile = $this->manifestsRoot() . '/' . $manifestId . '.json';
        $data         = json_decode((string)file_get_contents($manifestFile), true);

        $this->assertIsArray($data);
        $this->assertCount(1, $data['entries'], 'Entry must be recorded even when moved=0.');
        $this->assertSame(99, $data['entries'][0]['attachment_id']);
        $this->assertSame([], $data['entries'][0]['files']);
    }

    // =========================================================================
    // deleteManifest() — 0-file entry still calls wp_delete_attachment
    // =========================================================================

    public function testDeleteManifestWithZeroFileEntryStillDeletesPost(): void
    {
        // Track how many times wp_delete_attachment is called.
        $callCount = 0;
        Functions\when('wp_delete_attachment')->alias(static function (int $id, bool $force) use (&$callCount) {
            $callCount++;
            return new \stdClass(); // truthy non-null/non-false value
        });

        $q          = $this->makeQuarantine();
        $manifestId = $q->beginManifest('job-nofiles-del');

        // Quarantine with empty file list (simulates broken/already-absent attachment).
        $q->quarantineAttachment($manifestId, 77, '2024/01/ghost.jpg', []);
        $q->finaliseManifest($manifestId);

        $result = $q->deleteManifest($manifestId);

        $this->assertSame(1, $callCount, 'wp_delete_attachment must be called exactly once.');
        $this->assertSame(1, $result['posts_deleted']);
        $this->assertSame(0, $result['posts_failed']);
        $this->assertSame(0, $result['files_deleted'], 'No quarantined files to delete.');
        $this->assertSame(1, $result['entries_processed']);
        $this->assertCount(1, $result['results']);
        $this->assertSame(77, $result['results'][0]['attachment_id']);
        $this->assertTrue($result['results'][0]['post_deleted']);
        $this->assertSame(0, $result['results'][0]['files_deleted']);
    }

    // =========================================================================
    // restoreManifest() — empty files[] entry restores cleanly with 0 files back
    // =========================================================================

    public function testRestoreManifestWithEmptyFilesEntryReturnsZeroWithNoError(): void
    {
        $q          = $this->makeQuarantine();
        $manifestId = $q->beginManifest('job-restore-empty');

        // Record an entry with no files (broken attachment with no on-disk files).
        $q->quarantineAttachment($manifestId, 11, '2024/01/ghost.jpg', []);
        $q->finaliseManifest($manifestId);

        // restoreManifest must handle empty files[] without error and return 0.
        $receipt = $q->restoreManifest($manifestId);

        $this->assertSame(0, $receipt['restored'], 'Nothing to restore when files array is empty.');
    }

    // =========================================================================
    // loadManifest() — path-traversal manifest_id is rejected
    // =========================================================================

    public function testLoadManifestRejectsPathTraversalIds(): void
    {
        $q = $this->makeQuarantine();

        // Exercise loadManifest() indirectly via restoreManifest().
        $traversalIds = ['../../etc/passwd', '..', '/etc/shadow', '../secret', '.hidden'];

        foreach ($traversalIds as $id) {
            $result = $q->restoreManifest($id);
            $this->assertSame(0, $result['restored'], "Path-traversal id '{$id}' must be rejected (restores 0).");
        }
    }

    // =========================================================================
    // normalisePath() — rejects paths containing "/.." (belt-and-braces)
    // =========================================================================

    public function testNormalisePathRejectsDoubleDotTraversal(): void
    {
        // Craft a manifest with a crafted entry whose stored path contains "/.."
        // and verify that restoreManifest() skips it (returns 0).
        // We do this by writing a manifest JSON directly to the manifests/ dir.
        $q          = $this->makeQuarantine();
        $manifestId = $q->beginManifest('job-dotdot');

        // Close the in-progress manifest normally (no files moved).
        $q->finaliseManifest($manifestId);

        // Overwrite the manifest JSON with a crafted entry containing "/.." in
        // the stored original path.
        $manifestFile = $this->manifestsRoot() . '/' . $manifestId . '.json';
        $crafted = json_encode([
            'v'       => 1,
            'id'      => $manifestId,
            'job_id'  => 'job-dotdot',
            'ts'      => time(),
            'entries' => [[
                'attachment_id' => 1,
                'rel_path'      => '2024/01/traversal.jpg',
                // Crafted path with "/..": normalisePath must reject this.
                'files'         => [$this->uploadsDir . '/2024/01/../../../etc/passwd'],
            ]],
        ]);
        file_put_contents($manifestFile, $crafted);

        // restoreManifest must skip the crafted entry (returns 0 restored).
        $receipt = $q->restoreManifest($manifestId);
        $this->assertSame(
            0,
            $receipt['restored'],
            'normalisePath() must reject paths containing "/.." — 0 files restored.'
        );
        $this->assertSame(
            1,
            $receipt['reasons'][MediaQuarantine::RESTORE_SKIP_OUTSIDE_UPLOADS] ?? 0,
            'the rejected entry is attributed to the containment check, not silently dropped'
        );
    }
}
