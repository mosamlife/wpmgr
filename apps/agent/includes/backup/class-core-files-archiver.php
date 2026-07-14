<?php
/**
 * CoreFilesArchiver: archives the WordPress core source root (ABSPATH).
 *
 * Track A / A2 (#187) — ABSPATH archiving.
 *
 * FilesArchiver covers WP_CONTENT_DIR. A full site backup is not restorable
 * if the core installation is corrupted or the wp-config.php is lost, because
 * the content archive alone cannot bring those back. This archiver fills that
 * gap: it walks ABSPATH and packs the WordPress core files (wp-admin/,
 * wp-includes/, and the root PHP files including wp-config.php) into one
 * rotating part-sequence with entry_kind "core" so restore can stage them
 * separately and independently of the content archive.
 *
 * ABSPATH typically looks like:
 *   <docroot>/
 *     wp-admin/         <- packed
 *     wp-includes/      <- packed
 *     wp-config.php     <- packed
 *     wp-login.php      <- packed
 *     index.php         <- packed
 *     wp-content/       <- SKIPPED (covered by FilesArchiver)
 *     <other non-php>   <- SKIPPED (robots.txt, .htaccess etc. intentionally
 *                          excluded — they are hosting-environment artefacts)
 *
 * WHY ZipArchive: same rationale as FilesArchiver — ext-zip is available on
 * all target hosts; no binary dependencies; streams file content without
 * PHP-side buffering.
 *
 * OOM defence: path discovery streams to an on-disk cache file exactly as
 * FilesArchiver does — never accumulates the list in PHP memory.
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

use WPMgr\Agent\Support\LongRunningJob;

/**
 * Pure-PHP streaming WordPress core archiver.
 *
 * Archives the content of ABSPATH into a rotating `core.gNNN.partMMM.zip`
 * sequence. The manifest entry_kind for every emitted part is "core".
 *
 * The archive() method is intentionally simpler than FilesArchiver because:
 *   - there is only ONE component bucket ("core") — no per-component split
 *   - no incremental-delta logic (the restore path for core is always a full
 *     overlay replace; a core file changed between two increments means the
 *     whole core install may have been updated, and partial overlay is risky)
 *   - no files.list sidecar (incremental core is deferred to a later phase)
 *
 * The caller (TaskRunner::runArchivingFiles) receives the returned parts list
 * and merges it into the overall manifest under the "core" kind.
 */
final class CoreFilesArchiver
{
    /** entry_kind written on every manifest part emitted by this archiver. */
    public const ENTRY_KIND = 'core';

    /** Default per-part size cap (bytes). Matches FilesArchiver default. */
    public const DEFAULT_MAX_PART_BYTES = 200 * 1024 * 1024;

    /** Hard cap on entries per part. */
    public const DEFAULT_MAX_PART_ENTRIES = 55_000;

    /** On-disk path-cache filename (in outDir). */
    private const PATHS_CACHE_NAME = 'core-paths.cache';

    /**
     * ABSPATH subtrees and root-level PHP files to include. We pack:
     *   wp-admin/     — admin application directory
     *   wp-includes/  — core library directory
     *   *.php         — root-level PHP files (wp-config.php, wp-login.php, …)
     *
     * wp-content/ is explicitly excluded even though it lives under ABSPATH —
     * it is covered by FilesArchiver.
     *
     * Hidden-file roots (.htaccess, robots.txt) are hosting-env artefacts and
     * not part of the WordPress source tree; they are not packed.
     */
    private const INCLUDE_DIRS = ['wp-admin', 'wp-includes'];

    /** Emit a progress callback every N packed files. */
    private const PROGRESS_EVERY_FILES = 50;

    /** Absolute path of ABSPATH (trailing slash stripped). */
    private string $absPath;

    /** Snapshot generation (0 = base full). Namespaces part filenames. */
    private int $generation;

    /** Per-part size cap (bytes). */
    private int $maxPartBytes;

    /** Per-part entry-count cap. */
    private int $maxPartEntries;

    /**
     * @param string $absPath    Absolute path to ABSPATH (WordPress root).
     * @param int    $generation Snapshot generation (0 = full base, >0 = increment).
     * @param array<string,mixed> $opts Optional overrides: max_part_bytes, max_part_entries.
     * @throws \RuntimeException If ext-zip is unavailable or $absPath is not a directory.
     */
    public function __construct(string $absPath, int $generation = 0, array $opts = [])
    {
        if (!class_exists(\ZipArchive::class)) {
            throw new \RuntimeException('CoreFilesArchiver requires ext-zip');
        }

        $real = realpath($absPath);
        if ($real === false || !is_dir($real)) {
            throw new \RuntimeException('CoreFilesArchiver: absPath is not a readable directory: ' . esc_html($absPath)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
        }
        $this->absPath    = rtrim($real, DIRECTORY_SEPARATOR);
        $this->generation = max(0, $generation);

        $this->maxPartBytes = isset($opts['max_part_bytes'])
            ? max(1, (int) $opts['max_part_bytes'])
            : self::DEFAULT_MAX_PART_BYTES;
        $this->maxPartEntries = isset($opts['max_part_entries'])
            ? max(1, (int) $opts['max_part_entries'])
            : self::DEFAULT_MAX_PART_ENTRIES;
    }

    /**
     * Walk ABSPATH and pack the WordPress core files into rotating part archives
     * inside $outDir.
     *
     * Returns an array with:
     *   done       => true
     *   parts      => list<string>   part basenames
     *   part_kinds => list<string>   all 'core' (parallel to parts)
     *   files_total => int
     *   bytes_written => int
     *
     * On a site without a readable ABSPATH (should not happen in practice but
     * defends against mis-configuration) returns done=true with empty parts.
     *
     * @param string   $outDir   Absolute scratch dir (created if missing).
     * @param callable $progress function(string $phase, array $detail): void.
     * @return array<string,mixed>
     * @throws \RuntimeException On unrecoverable I/O error.
     */
    public function archive(string $outDir, callable $progress): array
    {
        @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running backup/restore loop must not hit max_execution_time; @-guarded
        @ignore_user_abort(true);

        if (!is_dir($outDir) && !wp_mkdir_p($outDir) && !is_dir($outDir)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir -- converted to wp_mkdir_p
            throw new \RuntimeException('CoreFilesArchiver: cannot create outDir: ' . esc_html($outDir)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
        }
        $outDir = rtrim((string) realpath($outDir), DIRECTORY_SEPARATOR);

        // Phase 0: build path cache.
        $cachePath  = $outDir . DIRECTORY_SEPARATOR . self::PATHS_CACHE_NAME;
        $totalFiles = $this->buildPathCache($cachePath);

        if ($totalFiles === 0) {
            $progress('archiving_core', ['done' => true, 'parts' => [], 'part_kinds' => [], 'files_total' => 0, 'bytes_written' => 0]);
            return ['done' => true, 'parts' => [], 'part_kinds' => [], 'files_total' => 0, 'bytes_written' => 0];
        }

        // Phase 1: pack.
        $partsCompleted = [];
        $partKinds      = [];
        $bytesWritten   = 0;
        $fileIndex      = 0;
        $filesSinceProgress = 0;

        $currentPart      = 1;
        $partEntries      = 0;
        $partEstBytes     = 0;
        /** @var \ZipArchive|null $zip */
        $zip              = null;
        $currentPartPath  = '';

        $cacheHandle = @fopen($cachePath, 'r'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming handle for chunked line-by-line reads over a large path-cache file; WP_Filesystem exposes only whole-file get/put which would OOM
        if ($cacheHandle === false) {
            throw new \RuntimeException('CoreFilesArchiver: cannot read path cache: ' . esc_html($cachePath)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
        }

        while (($line = fgets($cacheHandle)) !== false) {
            $rel = rtrim($line, "\n\r");
            if ($rel === '') {
                continue;
            }

            $abs = $this->absPath . DIRECTORY_SEPARATOR . str_replace('/', DIRECTORY_SEPARATOR, $rel);
            if (!is_file($abs)) {
                continue;
            }

            // Lazy-open first / rotated part.
            if ($zip === null) {
                [$zip, $currentPartPath] = $this->openPart($outDir, $currentPart);
            }

            if ($zip->addFile($abs, $rel) === false) {
                throw new \RuntimeException('CoreFilesArchiver: addFile failed for ' . esc_html($rel)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
            }
            $partEntries++;
            $partEstBytes += (int) @filesize($abs);
            $fileIndex++;
            $filesSinceProgress++;

            // Rotation.
            $needRotate = $partEntries >= $this->maxPartEntries || $partEstBytes >= $this->maxPartBytes;
            if ($needRotate) {
                $closed = $this->closePart($zip, $currentPartPath, $partEntries);
                if ($closed['size'] > 0) {
                    $bytesWritten     += $closed['size'];
                    $partsCompleted[] = $closed['name'];
                    $partKinds[]      = self::ENTRY_KIND;
                }
                $currentPart++;
                $partEntries  = 0;
                $partEstBytes = 0;
                $zip          = null;
                $currentPartPath = '';
                gc_collect_cycles();
            }

            if ($filesSinceProgress >= self::PROGRESS_EVERY_FILES) {
                $progress('archiving_core', [
                    'files_done'    => $fileIndex,
                    'files_total'   => $totalFiles,
                    'parts_done'    => count($partsCompleted),
                    'bytes_written' => $bytesWritten,
                ]);
                $filesSinceProgress = 0;
            }
        }
        fclose($cacheHandle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- closes a streaming handle over a large path-cache file; WP_Filesystem has no streaming API

        // Close the final open part.
        if ($zip !== null) {
            $closed = $this->closePart($zip, $currentPartPath, $partEntries);
            if ($closed['size'] > 0) {
                $bytesWritten     += $closed['size'];
                $partsCompleted[] = $closed['name'];
                $partKinds[]      = self::ENTRY_KIND;
            }
        }

        wp_delete_file($cachePath);

        $result = [
            'done'          => true,
            'parts'         => $partsCompleted,
            'part_kinds'    => $partKinds,
            'files_total'   => $totalFiles,
            'bytes_written' => $bytesWritten,
        ];
        $progress('archiving_core', $result);
        return $result;
    }

    // -------------------------------------------------------------------------
    // Private helpers
    // -------------------------------------------------------------------------

    /**
     * Walk ABSPATH and write all includable file relative-paths to $cachePath.
     * Returns the count of files written.
     *
     * Included:
     *   - Files directly under ABSPATH matching *.php (core root PHP files).
     *   - All files recursively under wp-admin/ and wp-includes/.
     *
     * Excluded:
     *   - wp-content/ (covered by FilesArchiver).
     *   - Symlinks.
     */
    private function buildPathCache(string $cachePath): int
    {
        $handle = @fopen($cachePath, 'w'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming handle for incremental line-by-line writes to a large path-cache file; WP_Filesystem exposes only whole-file put_contents which would OOM
        if ($handle === false) {
            throw new \RuntimeException('CoreFilesArchiver: cannot write path cache: ' . esc_html($cachePath)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
        }

        $count   = 0;
        $srcLen  = strlen($this->absPath) + 1; // +1 for the trailing separator

        // 1. Root-level PHP files.
        $rootPhpFiles = @glob($this->absPath . DIRECTORY_SEPARATOR . '*.php');
        if (is_array($rootPhpFiles)) {
            foreach ($rootPhpFiles as $abs) {
                if (!is_file($abs) || is_link($abs)) {
                    continue;
                }
                $rel = str_replace(DIRECTORY_SEPARATOR, '/', substr($abs, $srcLen));
                fwrite($handle, $rel . "\n"); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fwrite -- incremental write into a streaming handle; WP_Filesystem put_contents is whole-buffer only
                $count++;
            }
        }

        // 2. Recursive subtrees (wp-admin, wp-includes).
        foreach (self::INCLUDE_DIRS as $dir) {
            $subDir = $this->absPath . DIRECTORY_SEPARATOR . $dir;
            if (!is_dir($subDir)) {
                continue;
            }
            try {
                $iterator = new \RecursiveIteratorIterator(
                    new \RecursiveDirectoryIterator(
                        $subDir,
                        \FilesystemIterator::SKIP_DOTS | \FilesystemIterator::UNIX_PATHS
                    )
                );
            } catch (\UnexpectedValueException $e) {
                // Unreadable subtree — skip silently (permissions may restrict).
                continue;
            }

            /** @var \SplFileInfo $info */
            foreach ($iterator as $info) {
                if (is_link((string) $info->getPathname())) {
                    continue;
                }
                if (!$info->isFile()) {
                    continue;
                }
                $rel = str_replace(DIRECTORY_SEPARATOR, '/', substr((string) $info->getPathname(), $srcLen));
                fwrite($handle, $rel . "\n"); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fwrite -- incremental write into a streaming handle; WP_Filesystem put_contents is whole-buffer only
                $count++;
            }
        }

        fclose($handle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- closes a streaming handle over a large path-cache file; WP_Filesystem has no streaming API
        return $count;
    }

    /**
     * Open part N for writing. Returns [ZipArchive, absolutePartPath].
     *
     * @return array{\ZipArchive,string}
     */
    private function openPart(string $outDir, int $n): array
    {
        $name = sprintf('core.g%03d.part%03d.zip', $this->generation, $n);
        $path = $outDir . DIRECTORY_SEPARATOR . $name;
        $zip  = new \ZipArchive();
        if ($zip->open($path, \ZipArchive::CREATE | \ZipArchive::OVERWRITE) !== true) {
            throw new \RuntimeException('CoreFilesArchiver: cannot open zip part: ' . esc_html($path)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
        }
        return [$zip, $path];
    }

    /**
     * Close the given part. Unlinks empty parts (no entries). Returns the
     * on-disk filename and byte-size (both '' / 0 for empty parts).
     *
     * @return array{name:string,size:int}
     */
    private function closePart(\ZipArchive $zip, string $partPath, int $entries): array
    {
        if ($entries === 0) {
            $zip->close();
            if (is_file($partPath)) {
                wp_delete_file($partPath);
            }
            return ['name' => '', 'size' => 0];
        }
        if (!$zip->close()) {
            throw new \RuntimeException('CoreFilesArchiver: zip close failed for ' . esc_html($partPath)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
        }
        $size = (int) @filesize($partPath);
        return ['name' => basename($partPath), 'size' => $size];
    }
}
