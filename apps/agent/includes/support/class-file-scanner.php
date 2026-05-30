<?php
/**
 * FileScanner: resumable DFS file-system walker for the malware/integrity scan.
 *
 * Ports scanFilesDfs, fileStat, calculateMd5, getFilesContent, seekDirectoryHandle,
 * and the $cwAllowedFiles seed from wpremote/callback/wings/fs.php into the WPMgr
 * Agent PSR-4 / namespace style.
 *
 * Design constraints (from docs/research/s3-malware-scan-design.md):
 *  - Cursor shape is byte-identical to the wpremote shape so the CP can resume
 *    across agent upgrades.
 *  - Symlinks are recorded in `links`, NEVER recursed.
 *  - An unreadable or odd file never aborts the walk; it produces a row with
 *    error:"NOT_READABLE".
 *  - md5_file() is used (NOT Blake3) because wp.org manifests use md5.
 *  - Time budget enforced between files via microtime(); also honours paths_limit.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * Pure DFS file-system walker with resumable cursor support.
 *
 * Instantiate inside a command; carries no shared state between requests.
 */
final class FileScanner
{
    /**
     * Files that are always considered "allowed" in core-dir unknown-injection
     * diffing (seeded from wpremote $cwAllowedFiles + WP conventional extras).
     *
     * @var list<string>
     */
    public const ALLOWED_FILES = [
        '.htaccess',
        '.user.ini',
        'malcare-waf.php',
        'wp-config.php',
        '.maintenance',
        'object-cache.php',
        'advanced-cache.php',
        'wp-content/',
    ];

    /**
     * Directories excluded when scanning the files (wp-content) subtree.
     * Mirrors BackupSource::EXCLUDE_DIRS exactly.
     *
     * @var list<string>
     */
    private const EXCLUDE_DIRS = ['wpmgr-snapshots', 'wpmgr-agent', 'cache', 'upgrade'];

    // ------------------------------------------------------------------
    // Public API
    // ------------------------------------------------------------------

    /**
     * Run a resumable DFS walk.
     *
     * The walk honours $timeBudgetS (wall-clock seconds, checked between
     * files) and $pathsLimit (total files emitted). When either limit is
     * reached the walk returns status="partial" and a non-null next_cursor.
     * When the DFS is exhausted it returns status="done" with next_cursor=null.
     *
     * @param string              $absRoot           Absolute scan root (e.g. ABSPATH), no trailing slash.
     * @param string              $relStartDir       Site-relative start directory (e.g. "" or "wp-content/").
     *                                               Must end with "/" if non-empty, or be "" for root.
     * @param bool                $includeMd5        Whether to compute md5 of each file.
     * @param float               $timeBudgetS       Max wall-clock seconds before returning partial.
     * @param int                 $pathsLimit        Max files to emit before returning partial (0 = unlimited).
     * @param int                 $batchSize         Number of file entries accumulated before flushing to
     *                                               output array (kept for cursor bookkeeping; all entries
     *                                               returned inline).
     * @param int                 $traversalStackMax Max depth of the in-memory traversal stack.
     * @param array<string,mixed>|null $resumeCursor Cursor from a prior partial response, or null to start fresh.
     * @param list<string>        $excludeTopDirs    Top-level directory names (relative to absRoot) to skip.
     *
     * @return array{
     *   status: "partial"|"done",
     *   files_scanned: int,
     *   next_cursor: array{dir:string, traversal_stack:list<list<mixed>>, folder_offset:int}|null,
     *   links: list<string>,
     *   hashes: list<array<string,mixed>>
     * }
     */
    public function scan(
        string $absRoot,
        string $relStartDir,
        bool $includeMd5,
        float $timeBudgetS,
        int $pathsLimit,
        int $batchSize,
        int $traversalStackMax,
        ?array $resumeCursor,
        array $excludeTopDirs = []
    ): array {
        $deadline = microtime(true) + $timeBudgetS;

        // Normalise absRoot: resolve symlinked mounts (e.g. macOS /tmp → /private/tmp)
        // so that path containment checks are consistent across all platforms.
        $absRoot = rtrim(str_replace('\\', '/', $absRoot), '/');
        $resolvedAbsRoot = realpath($absRoot);
        if ($resolvedAbsRoot !== false) {
            $absRoot = str_replace('\\', '/', rtrim($resolvedAbsRoot, '/'));
        }

        // Unpack resume cursor or start fresh.
        $dir            = $relStartDir;
        $traversalStack = [];
        $folderOffset   = 0;

        if ($resumeCursor !== null) {
            $dir = isset($resumeCursor['dir']) && is_string($resumeCursor['dir'])
                ? $resumeCursor['dir'] : $relStartDir;
            $rawStack       = $resumeCursor['traversal_stack'] ?? [];
            $traversalStack = is_array($rawStack) ? array_values($rawStack) : [];
            $folderOffset   = isset($resumeCursor['folder_offset']) && is_int($resumeCursor['folder_offset'])
                ? $resumeCursor['folder_offset'] : 0;
        }

        $links        = [];
        $hashes       = [];
        $filesScanned = 0;

        // Compute the current directory path from dir + traversal_stack.
        $basePath  = $this->buildBasePath($dir, $traversalStack);
        $dirHandle = @opendir($absRoot . '/' . $basePath);

        if ($dirHandle !== false) {
            $this->seekDirHandle($dirHandle, $folderOffset);
        }

        $status = 'done';

        while (true) {
            // Check time budget first (checked at entry of each file, not mid-read).
            if (microtime(true) >= $deadline) {
                $status = 'partial';
                break;
            }

            // Check paths limit.
            if ($pathsLimit > 0 && $filesScanned >= $pathsLimit) {
                $status = 'partial';
                break;
            }

            if ($dirHandle === false || ($file = @readdir($dirHandle)) === false) {
                // No more entries in this directory — backtrack.
                if ($dirHandle !== false) {
                    closedir($dirHandle);
                    $dirHandle = false;
                }

                if ($traversalStack === []) {
                    // DFS exhausted.
                    $status = 'done';
                    break;
                }

                /** @var list<mixed> $currentInfo */
                $currentInfo  = array_pop($traversalStack);
                $basePath     = $this->buildBasePath($dir, $traversalStack);
                $dirHandle    = @opendir($absRoot . '/' . $basePath);
                $folderOffset = is_int($currentInfo[1]) ? (int) $currentInfo[1] : 0;

                if ($dirHandle !== false) {
                    $this->seekDirHandle($dirHandle, $folderOffset);
                }

                continue;
            }

            if ($file === '.' || $file === '..') {
                continue;
            }

            // Build the site-relative path (no leading slash).
            // basePath is either "" (root) or "dir/" with trailing slash,
            // so concatenating the filename is always safe.
            $relativePath = $basePath . $file;
            $absolutePath = $absRoot . '/' . $relativePath;

            $folderOffset++;

            // Apply top-level exclude list. The top-level name is the first
            // path component (everything up to the first "/", or the whole
            // string if there is no slash).
            if ($excludeTopDirs !== []) {
                $slashPos = strpos($relativePath, '/');
                $topDir   = $slashPos !== false ? substr($relativePath, 0, $slashPos) : $relativePath;
                if (in_array($topDir, $excludeTopDirs, true)) {
                    continue;
                }
            }

            $isLink = is_link($absolutePath);
            $isDir  = !$isLink && is_dir($absolutePath);

            if ($isLink) {
                $links[]  = $relativePath;
                $hashes[] = $this->fileStat($absRoot, $relativePath, $includeMd5);
                $filesScanned++;
                continue;
            }

            if ($isDir) {
                if (count($traversalStack) >= $traversalStackMax) {
                    // Depth guard: skip this subtree rather than OOM.
                    continue;
                }

                closedir($dirHandle);

                $traversalStack[] = [$file, $folderOffset];
                $basePath         = $this->buildBasePath($dir, $traversalStack);
                $dirHandle        = @opendir($absRoot . '/' . $basePath);
                $folderOffset     = 0;

                continue;
            }

            // Regular file.
            $hashes[] = $this->fileStat($absRoot, $relativePath, $includeMd5);
            $filesScanned++;
        }

        // Clean up handle if still open.
        if ($dirHandle !== false) {
            closedir($dirHandle);
        }

        $nextCursor = null;
        if ($status === 'partial') {
            $nextCursor = [
                'dir'             => $dir,
                'traversal_stack' => $traversalStack,
                'folder_offset'   => $folderOffset,
            ];
        }

        return [
            'status'        => $status,
            'files_scanned' => $filesScanned,
            'next_cursor'   => $nextCursor,
            'links'         => $links,
            'hashes'        => $hashes,
        ];
    }

    /**
     * Read a single file and return its content as a base64-encoded string,
     * enforcing containment within $absRoot and refusing dirs/symlinks/over-cap.
     *
     * This is the getFilesContent equivalent, used by GetFileCommand.
     *
     * @param string $absRoot   Absolute filesystem root (e.g. ABSPATH), no trailing slash.
     * @param string $relPath   Site-relative path (forward slashes, may not have leading slash).
     * @param int    $maxBytes  Maximum file size in bytes to read (0 = no limit).
     *
     * @return array{ok:bool, path:string, size:int, is_dir:bool, is_link:bool, content_base64:string|null, error:string|null}
     */
    public function getFileContent(string $absRoot, string $relPath, int $maxBytes): array
    {
        $relPath = str_replace('\\', '/', $relPath);
        $absRoot = rtrim(str_replace('\\', '/', $absRoot), '/');

        // Resolve absRoot itself through realpath so that macOS /tmp → /private/tmp
        // and similar symlinked mount points do not break the containment check.
        $resolvedRoot = realpath($absRoot);
        if ($resolvedRoot !== false) {
            $absRoot = str_replace('\\', '/', rtrim($resolvedRoot, '/'));
        }

        $base = [
            'ok'             => false,
            'path'           => $relPath,
            'size'           => 0,
            'is_dir'         => false,
            'is_link'        => false,
            'content_base64' => null,
            'error'          => null,
        ];

        // Reject obvious traversal segments before touching disk.
        $stripped = ltrim($relPath, '/');
        foreach (explode('/', $stripped) as $segment) {
            if ($segment === '..' || $segment === '.') {
                $base['error'] = 'OUTSIDE_ROOT';
                return $base;
            }
        }

        if ($stripped === '' || str_contains($stripped, "\0")) {
            $base['error'] = 'NOT_READABLE';
            return $base;
        }

        $joined = $absRoot . '/' . $stripped;

        // Containment: resolve and verify the path stays inside absRoot.
        // Use lstat-based check BEFORE realpath to detect symlinks (realpath follows them).
        $lstatResult = @lstat($joined);

        if ($lstatResult === false) {
            // Does not exist or is truly unreadable.
            $base['error'] = 'NOT_READABLE';
            return $base;
        }

        // Check if it is a symlink BEFORE realpath follows it.
        if (is_link($joined)) {
            $base['is_link'] = true;
            $base['error']   = 'IS_LINK';
            return $base;
        }

        if (is_dir($joined)) {
            $base['is_dir'] = true;
            $base['error']  = 'IS_DIR';
            return $base;
        }

        // Now resolve the real path and perform containment check.
        $real = realpath($joined);
        if ($real === false) {
            $base['error'] = 'NOT_READABLE';
            return $base;
        }
        $real = str_replace('\\', '/', $real);
        if (strncmp($real, $absRoot . '/', strlen($absRoot) + 1) !== 0) {
            $base['error'] = 'OUTSIDE_ROOT';
            return $base;
        }

        $size = @filesize($real);
        if ($size === false) {
            $base['error'] = 'NOT_READABLE';
            return $base;
        }
        $base['size'] = $size;

        if ($maxBytes > 0 && $size > $maxBytes) {
            $base['error'] = 'too_large';
            return $base;
        }

        $content = @file_get_contents($real);
        if ($content === false) {
            $base['error'] = 'NOT_READABLE';
            return $base;
        }

        $base['ok']             = true;
        $base['content_base64'] = base64_encode($content);

        return $base;
    }

    // ------------------------------------------------------------------
    // Private helpers
    // ------------------------------------------------------------------

    /**
     * Build the current directory path WITH trailing slash from the DFS cursor
     * components. Returns "" for the top-level root.
     *
     * Mirrors getDirectoryPath() in fs.php: the result is used as a prefix for
     * concatenating filenames, so "sub/" + "b.txt" = "sub/b.txt" (correct),
     * while "" + "a.txt" = "a.txt" (also correct for root-level files).
     *
     * @param string            $dir            Base scan dir (may be "" or have trailing slash; stripped).
     * @param list<list<mixed>> $traversalStack Stack of [name, offset] frames.
     * @return string Directory path with trailing slash (e.g. "wp-admin/css/"), or "" for root.
     */
    private function buildBasePath(string $dir, array $traversalStack): string
    {
        // Strip trailing slash from base dir; treat "" as the filesystem root.
        $base = rtrim($dir, '/');

        if ($traversalStack === []) {
            // Root-level: return "" so file names are NOT prefixed with a slash.
            return $base === '' ? '' : ($base . '/');
        }

        $sub = implode('/', array_column($traversalStack, 0));

        if ($base === '') {
            return $sub . '/';
        }

        return $base . '/' . $sub . '/';
    }

    /**
     * Advance a directory handle past $offset real entries (skip . and ..).
     * Mirrors seekDirectoryHandle() in fs.php exactly.
     *
     * @param resource $handle Dir handle.
     * @param int      $offset Number of real entries to skip.
     * @return void
     */
    private function seekDirHandle($handle, int $offset): void
    {
        while ($offset > 0 && ($file = @readdir($handle)) !== false) {
            if ($file === '.' || $file === '..') {
                continue;
            }
            $offset--;
        }
    }

    /**
     * Collect stat metadata for a single filesystem entry, optionally computing md5.
     *
     * Mirrors fileStat() in fs.php. On an unreadable path returns {path, error:"NOT_READABLE"}.
     * Symlinks: stat fields are recorded but the link is NOT followed for md5.
     *
     * @param string $absRoot      Absolute scan root (no trailing slash).
     * @param string $relativePath Path relative to absRoot (forward slashes, no leading slash).
     * @param bool   $includeMd5   Whether to compute md5 for regular files.
     *
     * @return array<string,mixed>
     */
    private function fileStat(string $absRoot, string $relativePath, bool $includeMd5): array
    {
        $absFile = $absRoot . '/' . $relativePath;

        $isLink = is_link($absFile);

        $row = [
            'path'    => str_replace('\\', '/', $relativePath),
            'size'    => 0,
            'md5'     => '',
            'mtime'   => 0,
            'mode'    => 0,
            'is_link' => $isLink,
        ];

        if (@is_readable($absFile) === false) {
            $row['error'] = 'NOT_READABLE';
            // Remove fields that have no meaning on unreadable entries.
            unset($row['md5']);
            return $row;
        }

        // Use lstat for symlinks (does not follow), stat for regular files.
        $stats = $isLink ? @lstat($absFile) : @stat($absFile);
        if ($stats === false) {
            $row['error'] = 'NOT_READABLE';
            unset($row['md5']);
            return $row;
        }

        // Mirror the preg_grep('#size|uid|gid|mode|mtime#i') from fs.php,
        // but use exact named-key matching to avoid blksize/blkcnt pollution.
        $wantedKeys = ['size', 'uid', 'gid', 'mode', 'mtime'];
        foreach ($wantedKeys as $key) {
            if (array_key_exists($key, $stats)) {
                $row[$key] = $stats[$key];
            }
        }

        // Wire-contract scalar types (always present).
        $row['size']  = (int) ($stats['size']  ?? 0);
        $row['mtime'] = (int) ($stats['mtime'] ?? 0);
        $row['mode']  = (int) ($stats['mode']  ?? 0);

        // Compute md5 only for regular (non-link, non-dir) files.
        if ($includeMd5 && !$isLink && !is_dir($absFile)) {
            $row['md5'] = $this->calculateMd5($absFile);
        }

        return $row;
    }

    /**
     * Compute the md5 of a file using md5_file() (whole-file, O(1) memory via
     * the C runtime). Returns 32-char hex on success, empty string on failure.
     *
     * Mirrors calculateMd5($offset=0,$limit=0) in fs.php.
     *
     * @param string $absFile Absolute file path.
     * @return string 32-char hex md5, or "" if unreadable.
     */
    private function calculateMd5(string $absFile): string
    {
        try {
            $result = @md5_file($absFile);
            return $result !== false ? $result : '';
        } catch (\Throwable $e) {
            return '';
        }
    }
}
