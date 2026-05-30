<?php
/**
 * Tests for FileScanner (S3 malware/integrity scan helper).
 *
 * Covers:
 *  - Cursor round-trip: two sequential scan() calls with a partial cursor resume
 *    deterministically visit all files exactly once.
 *  - Symlinks are recorded in `links` and `hashes`, but their subtree is NOT
 *    recursed into.
 *  - Unreadable file yields a hash row with error:"NOT_READABLE".
 *  - getFileContent() refuses a directory (IS_DIR) and a symlink (IS_LINK).
 *  - getFileContent() enforces max_bytes (too_large).
 *  - getFileContent() enforces ABSPATH containment (OUTSIDE_ROOT).
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use WPMgr\Agent\Support\FileScanner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Support\FileScanner
 */
final class FileScannerTest extends TestCase
{
    /** Temp root for the whole test class run. */
    private string $root = '';

    protected function set_up(): void
    {
        parent::set_up();
        $this->root = sys_get_temp_dir() . '/wpmgr-scan-' . bin2hex(random_bytes(6));
        mkdir($this->root, 0755, true);
    }

    protected function tear_down(): void
    {
        $this->rmdir_r($this->root);
        parent::tear_down();
    }

    // ------------------------------------------------------------------
    // Helpers
    // ------------------------------------------------------------------

    private function scanner(): FileScanner
    {
        return new FileScanner();
    }

    /**
     * Write a file with known content and return its absolute path.
     *
     * @param string $relPath Path relative to $this->root (forward slashes).
     * @param string $content File content.
     * @return string Absolute path.
     */
    private function writeFile(string $relPath, string $content = 'hello'): string
    {
        $abs = $this->root . '/' . $relPath;
        $dir = dirname($abs);
        if (!is_dir($dir)) {
            mkdir($dir, 0755, true);
        }
        file_put_contents($abs, $content);
        return $abs;
    }

    /** Recursively delete a directory tree. */
    private function rmdir_r(string $path): void
    {
        if (!is_dir($path)) {
            @unlink($path);
            return;
        }
        $items = @scandir($path);
        if (!is_array($items)) {
            return;
        }
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $child = $path . '/' . $item;
            if (is_link($child)) {
                @unlink($child);
            } elseif (is_dir($child)) {
                $this->rmdir_r($child);
            } else {
                @unlink($child);
            }
        }
        @rmdir($path);
    }

    // ------------------------------------------------------------------
    // Test: cursor round-trip resumes deterministically
    // ------------------------------------------------------------------

    /**
     * Two sequential scan() calls with a paths_limit of 1 must together visit
     * all files exactly once and in a deterministic order.
     */
    public function test_cursor_round_trip_visits_all_files_exactly_once(): void
    {
        // Flat directory with 3 regular files.
        $this->writeFile('alpha.php', '<?php // a');
        $this->writeFile('beta.php',  '<?php // b');
        $this->writeFile('gamma.php', '<?php // c');

        $scanner = $this->scanner();

        // First call: limit=1 → should return 1 file + partial cursor.
        $r1 = $scanner->scan(
            absRoot:           $this->root,
            relStartDir:       '',
            includeMd5:        false,
            timeBudgetS:       30.0,
            pathsLimit:        1,
            batchSize:         512,
            traversalStackMax: 10,
            resumeCursor:      null,
            excludeTopDirs:    []
        );

        $this->assertSame('partial', $r1['status'], 'First call should be partial');
        $this->assertSame(1, $r1['files_scanned']);
        $this->assertNotNull($r1['next_cursor'], 'Partial result must carry a cursor');

        // Second call: limit=1 again, using the cursor from the first call.
        $r2 = $scanner->scan(
            absRoot:           $this->root,
            relStartDir:       '',
            includeMd5:        false,
            timeBudgetS:       30.0,
            pathsLimit:        1,
            batchSize:         512,
            traversalStackMax: 10,
            resumeCursor:      $r1['next_cursor'],
            excludeTopDirs:    []
        );

        // Third call: consume the remainder.
        $r3 = $scanner->scan(
            absRoot:           $this->root,
            relStartDir:       '',
            includeMd5:        false,
            timeBudgetS:       30.0,
            pathsLimit:        10,
            batchSize:         512,
            traversalStackMax: 10,
            resumeCursor:      $r2['next_cursor'],
            excludeTopDirs:    []
        );

        $allPaths = array_merge(
            array_column($r1['hashes'], 'path'),
            array_column($r2['hashes'], 'path'),
            array_column($r3['hashes'], 'path'),
        );

        // Exactly 3 distinct paths, no duplicates.
        $this->assertCount(3, $allPaths, 'Must visit exactly 3 files across 3 calls');
        $this->assertCount(3, array_unique($allPaths), 'No file should be visited twice');
        $this->assertSame('done', $r3['status'], 'Final call should report done');
        $this->assertNull($r3['next_cursor'], 'Done result must have null next_cursor');
    }

    /**
     * A single scan() call with no limit exhausts the entire tree and returns "done".
     */
    public function test_full_scan_returns_done_and_null_cursor(): void
    {
        $this->writeFile('a.txt', 'aaa');
        $this->writeFile('sub/b.txt', 'bbb');

        $r = $this->scanner()->scan(
            absRoot:           $this->root,
            relStartDir:       '',
            includeMd5:        true,
            timeBudgetS:       30.0,
            pathsLimit:        0,
            batchSize:         512,
            traversalStackMax: 10,
            resumeCursor:      null,
            excludeTopDirs:    []
        );

        $this->assertSame('done', $r['status']);
        $this->assertNull($r['next_cursor']);
        $this->assertSame(2, $r['files_scanned']);

        // Verify md5 is present and non-empty for regular readable files.
        foreach ($r['hashes'] as $row) {
            if (!isset($row['error'])) {
                $this->assertNotEmpty($row['md5'], 'Readable file must have a non-empty md5');
                $this->assertMatchesRegularExpression('/^[0-9a-f]{32}$/', $row['md5']);
            }
        }
    }

    // ------------------------------------------------------------------
    // Test: symlinks recorded but not followed
    // ------------------------------------------------------------------

    public function test_symlink_is_recorded_in_links_and_not_recursed(): void
    {
        // Create a real directory with one file.
        $this->writeFile('real/inner.php', '<?php');
        // Create a symlink to the directory.
        $linkPath = $this->root . '/linked';
        symlink($this->root . '/real', $linkPath);

        $scanner = $this->scanner();
        $r = $scanner->scan(
            absRoot:           $this->root,
            relStartDir:       '',
            includeMd5:        false,
            timeBudgetS:       30.0,
            pathsLimit:        0,
            batchSize:         512,
            traversalStackMax: 10,
            resumeCursor:      null,
            excludeTopDirs:    []
        );

        // "linked" must appear in the links list.
        $this->assertContains('linked', $r['links'], 'Symlink must appear in links[]');

        // The symlink's TARGET contents ("inner.php") must NOT appear in hashes
        // (we never recurse into symlinked directories).
        $paths = array_column($r['hashes'], 'path');
        $this->assertNotContains('linked/inner.php', $paths, 'Symlink target must not be recursed');

        // The symlink entry itself should appear in hashes with is_link=true.
        $linkRow = null;
        foreach ($r['hashes'] as $row) {
            if ($row['path'] === 'linked') {
                $linkRow = $row;
                break;
            }
        }
        $this->assertNotNull($linkRow, 'Symlink entry must appear in hashes');
        $this->assertTrue($linkRow['is_link'], 'is_link must be true for the symlink row');
    }

    // ------------------------------------------------------------------
    // Test: unreadable file yields error:"NOT_READABLE"
    // ------------------------------------------------------------------

    public function test_unreadable_file_yields_not_readable_row(): void
    {
        $abs = $this->writeFile('secret.php', '<?php // top secret');
        // Make the file unreadable.
        chmod($abs, 0000);

        // Skip this test in root/CI environments where chmod(0) has no effect.
        if (is_readable($abs)) {
            chmod($abs, 0644);
            $this->markTestSkipped('Cannot revoke read permission in this environment (likely running as root).');
        }

        $r = $this->scanner()->scan(
            absRoot:           $this->root,
            relStartDir:       '',
            includeMd5:        false,
            timeBudgetS:       30.0,
            pathsLimit:        0,
            batchSize:         512,
            traversalStackMax: 10,
            resumeCursor:      null,
            excludeTopDirs:    []
        );

        // Restore permissions so tear_down can clean up.
        chmod($abs, 0644);

        $errorRow = null;
        foreach ($r['hashes'] as $row) {
            if (($row['path'] ?? '') === 'secret.php') {
                $errorRow = $row;
                break;
            }
        }

        $this->assertNotNull($errorRow, 'Unreadable file must appear in hashes');
        $this->assertSame('NOT_READABLE', $errorRow['error'] ?? null, 'error must be NOT_READABLE');
        $this->assertArrayNotHasKey('md5', $errorRow, 'No md5 field on NOT_READABLE row');
    }

    // ------------------------------------------------------------------
    // Test: getFileContent refuses a directory
    // ------------------------------------------------------------------

    public function test_get_file_content_refuses_directory(): void
    {
        mkdir($this->root . '/somedir', 0755, true);

        $r = $this->scanner()->getFileContent($this->root, 'somedir', 262144);

        $this->assertFalse($r['ok']);
        $this->assertTrue($r['is_dir']);
        $this->assertNull($r['content_base64']);
        $this->assertSame('IS_DIR', $r['error']);
    }

    // ------------------------------------------------------------------
    // Test: getFileContent refuses a symlink
    // ------------------------------------------------------------------

    public function test_get_file_content_refuses_symlink(): void
    {
        $this->writeFile('target.php', '<?php');
        symlink($this->root . '/target.php', $this->root . '/link.php');

        $r = $this->scanner()->getFileContent($this->root, 'link.php', 262144);

        $this->assertFalse($r['ok']);
        $this->assertTrue($r['is_link']);
        $this->assertNull($r['content_base64']);
        $this->assertSame('IS_LINK', $r['error']);
    }

    // ------------------------------------------------------------------
    // Test: getFileContent enforces max_bytes
    // ------------------------------------------------------------------

    public function test_get_file_content_rejects_file_over_max_bytes(): void
    {
        // Write a 10-byte file but cap at 5.
        $this->writeFile('big.txt', 'hello world');

        $r = $this->scanner()->getFileContent($this->root, 'big.txt', 5);

        $this->assertFalse($r['ok']);
        $this->assertNull($r['content_base64']);
        $this->assertSame('too_large', $r['error']);
    }

    /**
     * When file size is within max_bytes the content is returned as valid base64.
     */
    public function test_get_file_content_returns_base64_for_readable_file(): void
    {
        $content = 'The quick brown fox';
        $this->writeFile('readable.txt', $content);

        $r = $this->scanner()->getFileContent($this->root, 'readable.txt', 262144);

        $this->assertTrue($r['ok']);
        $this->assertFalse($r['is_dir']);
        $this->assertFalse($r['is_link']);
        $this->assertNull($r['error']);
        $this->assertIsString($r['content_base64']);
        $this->assertSame($content, base64_decode((string) $r['content_base64']));
    }

    // ------------------------------------------------------------------
    // Test: getFileContent enforces ABSPATH containment
    // ------------------------------------------------------------------

    public function test_get_file_content_refuses_traversal_outside_root(): void
    {
        // Attempt to read /etc/passwd via traversal.
        $r = $this->scanner()->getFileContent($this->root, '../../../etc/passwd', 262144);

        $this->assertFalse($r['ok']);
        $this->assertNull($r['content_base64']);
        // The resolved path does not stay within root → either OUTSIDE_ROOT or NOT_READABLE.
        $this->assertContains($r['error'], ['OUTSIDE_ROOT', 'NOT_READABLE']);
    }

    // ------------------------------------------------------------------
    // Test: excludeTopDirs skips matching top-level directories
    // ------------------------------------------------------------------

    public function test_scan_excludes_top_level_dirs(): void
    {
        $this->writeFile('kept.php', '<?php');
        $this->writeFile('cache/foo.php', '<?php'); // should be excluded

        $r = $this->scanner()->scan(
            absRoot:           $this->root,
            relStartDir:       '',
            includeMd5:        false,
            timeBudgetS:       30.0,
            pathsLimit:        0,
            batchSize:         512,
            traversalStackMax: 10,
            resumeCursor:      null,
            excludeTopDirs:    ['cache']
        );

        $paths = array_column($r['hashes'], 'path');
        $this->assertContains('kept.php', $paths);
        $this->assertNotContains('cache/foo.php', $paths);
    }

    // ------------------------------------------------------------------
    // Test: md5 is correctly computed for known content
    // ------------------------------------------------------------------

    public function test_md5_matches_known_hash(): void
    {
        $content = 'wpmgr-test-content';
        $this->writeFile('known.txt', $content);
        $expected = md5($content);

        $r = $this->scanner()->scan(
            absRoot:           $this->root,
            relStartDir:       '',
            includeMd5:        true,
            timeBudgetS:       30.0,
            pathsLimit:        0,
            batchSize:         512,
            traversalStackMax: 10,
            resumeCursor:      null,
            excludeTopDirs:    []
        );

        $row = null;
        foreach ($r['hashes'] as $h) {
            if (($h['path'] ?? '') === 'known.txt') {
                $row = $h;
                break;
            }
        }

        $this->assertNotNull($row);
        $this->assertSame($expected, $row['md5']);
    }
}
