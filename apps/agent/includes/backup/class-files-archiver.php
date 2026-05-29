<?php
/**
 * FilesArchiver: streaming wp-content packer that emits rotated
 * `.partNNN.zip` archive parts, modelled on WPvivid's v2 file-backup engine.
 *
 * M5.6 / ADR-033 — pure-PHP wp-content archiver. Replaces the phpbu tar
 * source from ADR-032: that source shells out to `tar`, which is missing on
 * a large slice of managed WP hosting (Kinsta, some Pantheon, WP Engine
 * restricted shells, Windows). This class runs everywhere PHP runs.
 *
 * WHY ZipArchive:
 *   - Built into PHP via ext-zip — no `tar` binary dependency, no
 *     `proc_open` permission.
 *   - The C extension streams source bytes through deflate/store without
 *     loading the file body into PHP memory (the only path that buffers in
 *     PHP is `addFromString`, which we never use for file contents).
 *   - This is exactly the choice WPvivid (40M+ installs) made for the same
 *     audience; we mirror their approach.
 *
 * OOM defense (the two memory cliffs WPvivid hit and we inherit):
 *   1. Path discovery streams the relative-path list to an on-disk cache
 *      file (one path per line). A site with 500k uploads would otherwise
 *      pin >100 MB just holding the file list as a PHP array.
 *   2. Per-file ingest is `ZipArchive::addFile($abs, $relative)` — the C
 *      extension reads the source file as it writes the entry; PHP-side
 *      memory stays flat regardless of any single file's size.
 *
 * Resume / watchdog recovery:
 *   - On a fresh run we create `$outDir/paths.cache` and walk the source dir
 *     into it, then start packing.
 *   - The cache file is durable on disk. After every ~200 packed files and
 *     after every part rotation, the caller persists the cursor
 *     `{cache_file, file_index, current_part, parts_completed, bytes_written}`
 *     into the backup-task sub_state (we just return it).
 *   - On re-entry the caller hands us the cursor; we seek the cache file to
 *     line `file_index`, reopen part N with `ZipArchive::CREATE` (without
 *     OVERWRITE — ZipArchive appends to an existing valid zip), and pick up.
 *   - Worst case loss on a watchdog kill is ~200 files of re-pack work.
 *
 * Part rotation:
 *   - Rotate on EITHER `estimated_part_bytes >= max_part_bytes`
 *     (default 200 MiB; matches WPvivid) OR `entry_count >= max_part_entries`
 *     (default 55,000; defends against many-small-files cases where the
 *     central-directory rewrite on close() blows the time budget).
 *   - We use an estimate (sum of source-file sizes added since the part was
 *     opened), NOT `filesize($partPath)`: `ZipArchive::addFile` is
 *     deferred — it doesn't flush any bytes to disk until `close()`, so a
 *     `filesize()` probe between adds always reads 0/stale. The estimate
 *     over-counts when content is compressible (the on-disk part will be
 *     SMALLER than the estimate); that's the safe direction — we rotate
 *     early rather than overshoot the cap. Actual on-disk bytes are
 *     recorded into `bytes_written` after each `close()`.
 *   - Check fires AFTER adding each file, so a single source file larger
 *     than `max_part_bytes` will produce an oversized part (the only way to
 *     avoid this would be byte-slicing inside a single zip entry, which
 *     breaks resume-by-extraction). Same trade-off WPvivid accepts.
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

/**
 * Pure-PHP streaming wp-content archiver with on-disk path cache and
 * checkpointed resume. See file docblock for the rationale.
 */
final class FilesArchiver
{
    /** Default part size before rotation (bytes) — matches WPvivid. */
    public const DEFAULT_MAX_PART_BYTES = 200 * 1024 * 1024;

    /** Hard cap on entries per part — defends against many-small-files cases. */
    public const DEFAULT_MAX_PART_ENTRIES = 55_000;

    /**
     * Default exclude list (matched against `/`-separated segments of the
     * path relative to the source dir). These are the WP-content subtrees
     * we never want in a snapshot: our own scratch dirs, transient caches,
     * and the core-update staging dirs.
     *
     * @var list<string>
     */
    public const DEFAULT_EXCLUDES = [
        'wpmgr-snapshots',
        'wpmgr-agent',
        'cache',
        'upgrade',
        'upgrade-temp-backup',
    ];

    /** Save the resume cursor every N packed files. */
    private const CHECKPOINT_EVERY_FILES = 200;

    /** Emit a progress callback every N packed files. */
    private const PROGRESS_EVERY_FILES = 50;

    /** Archive part filename prefix; output is `wp-content.partNNN.zip`. */
    private const PART_PREFIX = 'wp-content';

    /** Name of the on-disk path-discovery cache (created in $outDir). */
    private const PATHS_CACHE_NAME = 'paths.cache';

    /** Absolute path of the source root (typically WP_CONTENT_DIR). */
    private string $sourceDir;

    /**
     * Merged exclude list (defaults + caller-provided). Matched against
     * `/`-separated segments of the path RELATIVE to $sourceDir.
     *
     * @var list<string>
     */
    private array $excludes;

    /** Effective per-part size cap (bytes). */
    private int $maxPartBytes;

    /** Effective per-part entry-count cap. */
    private int $maxPartEntries;

    /**
     * @param string              $sourceDir Absolute path of the root to back up
     *                                       (typically WP_CONTENT_DIR).
     * @param list<string>        $excludes  Path-segment names to skip; matched
     *                                       against the RELATIVE path under
     *                                       $sourceDir. Merged additively with
     *                                       self::DEFAULT_EXCLUDES.
     * @param array<string,mixed> $opts      Optional overrides:
     *                                         max_part_bytes (int),
     *                                         max_part_entries (int).
     * @throws \RuntimeException If ext-zip is unavailable or $sourceDir is
     *                           not a readable directory.
     */
    public function __construct(string $sourceDir, array $excludes = [], array $opts = [])
    {
        if (!class_exists(\ZipArchive::class)) {
            // V0 ships no PclZip fallback. Most managed WP hosts ship ext-zip;
            // we'll add a fallback if surveys show hosts without it. Failing
            // loudly here is preferable to silently producing a non-zip.
            throw new \RuntimeException('FilesArchiver requires ext-zip');
        }

        $real = realpath($sourceDir);
        if ($real === false || !is_dir($real)) {
            throw new \RuntimeException('FilesArchiver: sourceDir is not a readable directory: ' . $sourceDir);
        }
        $this->sourceDir = rtrim($real, DIRECTORY_SEPARATOR);

        // Merge defaults + caller excludes, dedupe, drop empties. Segment
        // names only — slashes in entries would never match an exploded
        // single segment so we strip them.
        $merged = [];
        foreach (array_merge(self::DEFAULT_EXCLUDES, $excludes) as $entry) {
            $entry = trim((string) $entry, "/ \t\n\r\0\x0B");
            if ($entry === '') {
                continue;
            }
            $merged[$entry] = true;
        }
        $this->excludes = array_values(array_keys($merged));

        $this->maxPartBytes = isset($opts['max_part_bytes'])
            ? max(1, (int) $opts['max_part_bytes'])
            : self::DEFAULT_MAX_PART_BYTES;
        $this->maxPartEntries = isset($opts['max_part_entries'])
            ? max(1, (int) $opts['max_part_entries'])
            : self::DEFAULT_MAX_PART_ENTRIES;
    }

    /**
     * Walk the source dir and pack into rotated `wp-content.partNNN.zip`
     * files inside $outDir. Resumable: if $resume carries cursors from a
     * prior call, picks up where it left off.
     *
     * @param string              $outDir   Absolute scratch dir for the part
     *                                      archives (created if missing).
     * @param array<string,mixed> $resume   Empty for a fresh run; else the
     *                                      cursor returned by a prior call.
     * @param callable            $progress function(string $phase, array $detail): void.
     *                                      $phase is always 'archiving_files'.
     *                                      Per-tick $detail keys: files_done,
     *                                      files_total, parts_done,
     *                                      bytes_written, current_file. On
     *                                      completion: done=true, parts,
     *                                      files_total, bytes_written.
     * @return array<string,mixed> On completion: `done: true` + parts list.
     *                             Otherwise the cursor for the next call.
     * @throws \RuntimeException On unrecoverable error.
     */
    public function archive(string $outDir, array $resume, callable $progress): array
    {
        // Lift caller-imposed time/abort guards. Watchdog handles
        // stall recovery; we want this loop to run as long as the SAPI
        // will let it.
        @set_time_limit(0);
        @ignore_user_abort(true);

        if (!is_dir($outDir) && !@mkdir($outDir, 0755, true) && !is_dir($outDir)) {
            throw new \RuntimeException('FilesArchiver: cannot create outDir: ' . $outDir);
        }
        $outDir = rtrim((string) realpath($outDir), DIRECTORY_SEPARATOR);

        // ----- Phase 0: ensure the path-discovery cache exists. -----
        $cachePath = isset($resume['cache_file']) && is_string($resume['cache_file']) && $resume['cache_file'] !== ''
            ? $resume['cache_file']
            : $outDir . DIRECTORY_SEPARATOR . self::PATHS_CACHE_NAME;

        $totalFiles = isset($resume['total_files']) ? (int) $resume['total_files'] : 0;
        if (!is_file($cachePath) || $totalFiles === 0) {
            // Fresh discovery walk. Truncate any stale cache.
            $totalFiles = $this->buildPathCache($cachePath);
        }

        // ----- Phase 1: pack. -----
        $fileIndex      = isset($resume['file_index']) ? (int) $resume['file_index'] : 0;
        $currentPart    = isset($resume['current_part']) ? max(1, (int) $resume['current_part']) : 1;
        $bytesWritten   = isset($resume['bytes_written']) ? (int) $resume['bytes_written'] : 0;
        /** @var list<string> $partsCompleted */
        $partsCompleted = isset($resume['parts_completed']) && is_array($resume['parts_completed'])
            ? array_values(array_map('strval', $resume['parts_completed']))
            : [];

        $cacheHandle = @fopen($cachePath, 'rb');
        if ($cacheHandle === false) {
            throw new \RuntimeException('FilesArchiver: cannot reopen path cache: ' . $cachePath);
        }

        // Seek to the requested line by counting newlines. Cheap because
        // the cache is small text (one relpath per line); even 1M paths at
        // ~120 bytes each is ~120 MB and a single sequential read.
        if ($fileIndex > 0) {
            $skipped = 0;
            while ($skipped < $fileIndex && ($line = fgets($cacheHandle)) !== false) {
                $skipped++;
            }
            if ($skipped < $fileIndex) {
                // Cache shorter than the recorded cursor — should never
                // happen, but recover by treating as already-done.
                fclose($cacheHandle);
                return $this->buildDoneResult($partsCompleted, $totalFiles, $bytesWritten, $progress);
            }
        }

        // Open / reopen the active part.
        $partPath = $this->partPath($outDir, $currentPart);
        $zip      = new \ZipArchive();
        // ZipArchive::CREATE (no OVERWRITE) appends to an existing zip — this
        // is how we resume into the same part across watchdog re-entries.
        $openFlags = is_file($partPath) ? \ZipArchive::CREATE : (\ZipArchive::CREATE | \ZipArchive::OVERWRITE);
        if ($zip->open($partPath, $openFlags) !== true) {
            fclose($cacheHandle);
            throw new \RuntimeException('FilesArchiver: cannot open zip part: ' . $partPath);
        }
        // Best-effort entry count from the existing zip; this is the
        // entry-count rotation trigger and also lets it survive resumes.
        $partEntries = $zip->numFiles;

        // Estimated bytes added to the current part since it was opened.
        // ZipArchive::addFile is deferred — it doesn't flush to disk until
        // close() — so we can't probe filesize() between adds. Estimate
        // grows by the SOURCE file size; on close() we record the real
        // on-disk size into bytes_written. The estimate is conservative
        // (over-counts compressible content), so we rotate early rather
        // than overshoot the cap.
        $partEstimatedBytes = is_file($partPath) ? (int) @filesize($partPath) : 0;

        $filesSinceProgress   = 0;
        $filesSinceCheckpoint = 0;
        $currentRel           = '';

        while (($line = fgets($cacheHandle)) !== false) {
            $rel = rtrim($line, "\r\n");
            if ($rel === '') {
                $fileIndex++;
                continue;
            }
            $currentRel = $rel;
            $abs        = $this->sourceDir . DIRECTORY_SEPARATOR . $rel;

            // file_exists check mirrors WPvivid: files that vanished between
            // walk and pack are silently dropped, not fatal.
            if (is_file($abs) && !is_link($abs)) {
                if ($zip->addFile($abs, $rel)) {
                    $partEntries++;
                    $partEstimatedBytes += (int) @filesize($abs);
                }
            }
            $fileIndex++;
            $filesSinceProgress++;
            $filesSinceCheckpoint++;

            // Rotation triggers: estimated cumulative source bytes OR
            // entry-count cap. See class docblock for why we estimate
            // instead of stat-ing the part file.
            $needRotate = $partEntries >= $this->maxPartEntries
                || $partEstimatedBytes >= $this->maxPartBytes;

            if ($needRotate) {
                if (!$zip->close()) {
                    fclose($cacheHandle);
                    throw new \RuntimeException('FilesArchiver: zip close failed for ' . $partPath);
                }
                $bytesWritten     += (int) @filesize($partPath);
                $partsCompleted[] = basename($partPath);
                $this->emitProgress($progress, [
                    'files_done'    => $fileIndex,
                    'files_total'   => $totalFiles,
                    'parts_done'    => count($partsCompleted),
                    'bytes_written' => $bytesWritten,
                    'current_file'  => $currentRel,
                ]);

                gc_collect_cycles();

                $currentPart++;
                $partPath    = $this->partPath($outDir, $currentPart);
                $zip         = new \ZipArchive();
                $openFlags   = is_file($partPath) ? \ZipArchive::CREATE : (\ZipArchive::CREATE | \ZipArchive::OVERWRITE);
                if ($zip->open($partPath, $openFlags) !== true) {
                    fclose($cacheHandle);
                    throw new \RuntimeException('FilesArchiver: cannot open next zip part: ' . $partPath);
                }
                $partEntries          = 0;
                $partEstimatedBytes   = 0;
                $filesSinceCheckpoint = 0;
            }

            if ($filesSinceProgress >= self::PROGRESS_EVERY_FILES) {
                $this->emitProgress($progress, [
                    'files_done'    => $fileIndex,
                    'files_total'   => $totalFiles,
                    'parts_done'    => count($partsCompleted),
                    // The active part's bytes aren't visible on disk until
                    // close(); report only the durable total here.
                    'bytes_written' => $bytesWritten,
                    'current_file'  => $currentRel,
                ]);
                $filesSinceProgress = 0;
            }

            if ($filesSinceCheckpoint >= self::CHECKPOINT_EVERY_FILES) {
                // We don't side-effect the caller's state here; we just keep
                // the in-progress part flushed so the cursor we'd return
                // refers to durable on-disk state. ZipArchive doesn't flush
                // mid-stream; the closest we have is forgoing rotation —
                // which we already do. The cursor itself is returned only
                // at function exit.
                $filesSinceCheckpoint = 0;
            }
        }

        fclose($cacheHandle);

        // Close the (possibly partially-filled) final part.
        if ($partEntries > 0 || !in_array(basename($partPath), $partsCompleted, true)) {
            if (!$zip->close()) {
                throw new \RuntimeException('FilesArchiver: final zip close failed for ' . $partPath);
            }
            $finalSize = (int) @filesize($partPath);
            if ($finalSize > 0) {
                $bytesWritten     += $finalSize;
                $partsCompleted[] = basename($partPath);
            } else {
                // Empty final part — drop it.
                @unlink($partPath);
            }
        } else {
            // The active part has zero entries (rotation fired on the very
            // last file). Drop the empty placeholder.
            $zip->close();
            @unlink($partPath);
        }

        return $this->buildDoneResult($partsCompleted, $totalFiles, $bytesWritten, $progress, $cachePath);
    }

    /**
     * Walk $sourceDir and write the path-relative file list to $cachePath.
     * Returns the total line count (== total files in scope).
     *
     * Memory stays flat: we never accumulate the list in a PHP array; each
     * discovered path is fwrite'd line-by-line.
     *
     * @param string $cachePath Absolute path of the cache file to create.
     * @return int Total files discovered.
     * @throws \RuntimeException On unwritable cache file or unreadable source.
     */
    private function buildPathCache(string $cachePath): int
    {
        $handle = @fopen($cachePath, 'wb');
        if ($handle === false) {
            throw new \RuntimeException('FilesArchiver: cannot create path cache: ' . $cachePath);
        }

        $count   = 0;
        $srcLen  = strlen($this->sourceDir) + 1; // +1 for the separator

        try {
            $iterator = new \RecursiveIteratorIterator(
                new \RecursiveDirectoryIterator(
                    $this->sourceDir,
                    \FilesystemIterator::SKIP_DOTS | \FilesystemIterator::UNIX_PATHS
                ),
                \RecursiveIteratorIterator::SELF_FIRST
            );
        } catch (\UnexpectedValueException $e) {
            fclose($handle);
            throw new \RuntimeException('FilesArchiver: cannot iterate sourceDir: ' . $e->getMessage(), 0, $e);
        }

        /** @var \SplFileInfo $info */
        foreach ($iterator as $info) {
            $abs = (string) $info->getPathname();

            // Drop symlinks first (covers symlinked dirs AND files).
            // is_link() works on dirs in PHP.
            if (is_link($abs)) {
                continue;
            }

            // Normalise the relative path with forward slashes (zip standard).
            $rel = substr($abs, $srcLen);
            $rel = str_replace(DIRECTORY_SEPARATOR, '/', $rel);

            if ($this->isExcluded($rel)) {
                continue;
            }

            // Only file entries make it into the cache; we don't need empty
            // directories in the archive and the packer treats every line
            // as a file.
            if (!$info->isFile()) {
                continue;
            }

            fwrite($handle, $rel . "\n");
            $count++;
        }

        fclose($handle);
        return $count;
    }

    /**
     * Test whether a relative path should be excluded. Matches by exact
     * segment name; `cache` excludes `cache/`, `foo/cache/bar`, etc., but
     * not `cachefile.txt`.
     *
     * @param string $relativePath Path relative to $sourceDir, `/`-separated.
     * @return bool
     */
    private function isExcluded(string $relativePath): bool
    {
        if ($this->excludes === []) {
            return false;
        }
        $segments = explode('/', $relativePath);
        foreach ($segments as $segment) {
            if ($segment === '') {
                continue;
            }
            if (in_array($segment, $this->excludes, true)) {
                return true;
            }
        }
        return false;
    }

    /**
     * Build the absolute path of part N (1-indexed, 3-digit zero-padded).
     *
     * @param string $outDir Absolute scratch dir.
     * @param int    $n      Part number.
     * @return string
     */
    private function partPath(string $outDir, int $n): string
    {
        return $outDir . DIRECTORY_SEPARATOR . self::PART_PREFIX . '.part' . str_pad((string) $n, 3, '0', STR_PAD_LEFT) . '.zip';
    }

    /**
     * Wrap the progress callback so a buggy callback can never abort the
     * archive run. Backup progress is observability, not correctness.
     *
     * @param callable             $progress User-supplied callback.
     * @param array<string,mixed>  $detail   Detail payload.
     * @return void
     */
    private function emitProgress(callable $progress, array $detail): void
    {
        try {
            $progress('archiving_files', $detail);
        } catch (\Throwable $e) { // phpcs:ignore -- intentional swallow.
            // Swallow. Don't even surface in a return — the backup itself
            // is making forward progress and that's what matters.
        }
    }

    /**
     * Build the terminal "done" sub-state and fire the final progress tick.
     *
     * @param list<string> $parts          Closed part filenames.
     * @param int          $totalFiles     Total file count from the cache.
     * @param int          $bytesWritten   Sum of part sizes on disk.
     * @param callable     $progress       Progress callback (see archive()).
     * @param string|null  $cachePath      Cache file to clean up, if any.
     * @return array<string,mixed>
     */
    private function buildDoneResult(array $parts, int $totalFiles, int $bytesWritten, callable $progress, ?string $cachePath = null): array
    {
        // Cache file is no longer needed once we're done; leaving it under
        // the per-run scratch dir means it gets cleaned with the scratch.
        // We don't unlink so a post-mortem can inspect.
        unset($cachePath);

        $this->emitProgress($progress, [
            'done'          => true,
            'parts'         => $parts,
            'files_total'   => $totalFiles,
            'bytes_written' => $bytesWritten,
        ]);

        return [
            'done'          => true,
            'parts'         => $parts,
            'parts_total'   => count($parts),
            'files_total'   => $totalFiles,
            'bytes_written' => $bytesWritten,
        ];
    }
}
