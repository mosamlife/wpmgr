<?php
/**
 * FilesArchiver: streaming wp-content packer that emits rotated
 * `<component>.partNNN.zip` archive parts, modelled on WPvivid's v2 file-backup
 * engine.
 *
 * Track 5 / 0.9.6 — PER-COMPONENT split. Previously this class emitted a single
 * rotating sequence `wp-content.partNNN.zip` lumping the entire wp-content tree.
 * Now each well-known wp-content subtree gets its own rotating sequence so
 * operators can restore plugins / themes / uploads independently:
 *
 *   plugins/        -> plugins.part001.zip, plugins.part002.zip, ...
 *   themes/         -> themes.part001.zip, ...
 *   uploads/        -> uploads.part001.zip, uploads.part002.zip, ...
 *   wp-content/*    -> wp-content.part001.zip, ...  (EVERYTHING ELSE — mu-plugins,
 *                                                    languages, drop-ins, custom dirs)
 *
 * The component buckets are mutually exclusive: an entry under `plugins/` is
 * NEVER also in `wp-content.partNNN.zip`. The "wp-content" bucket is defined
 * as "all wp-content entries that don't fall into a more specific bucket".
 *
 * Each emitted part archive's manifest entry carries an `entry_kind` matching
 * the component name ('plugin' | 'theme' | 'upload' | 'wp-content'). The
 * restore side routes per-component and stages + swaps each subdirectory
 * independently when only some components are selected.
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
 *     into it, then start packing. The cache has every entry tagged with the
 *     component bucket it belongs to (one tagged path per line).
 *   - On re-entry the caller hands us the cursor; we seek the cache to line
 *     `file_index` and resume.
 *   - Worst case loss on a watchdog kill is ~200 files of re-pack work.
 *
 * Part rotation (per component, independently):
 *   - Same triggers as before: `estimated_part_bytes >= max_part_bytes`
 *     (default 200 MiB) OR `entry_count >= max_part_entries`
 *     (default 55,000).
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

    /**
     * Component partition definitions, in match-order. The first matching
     * `root` prefix claims the entry; anything that matches no listed root
     * falls into the `wp-content` (others) bucket.
     *
     * Each component emits its own rotating part archive sequence with a
     * filename prefix matching the array key. The `entry_kind` on the
     * resulting manifest entry also matches the array key.
     *
     * Note on `wp-content`: it is the catch-all bucket — its root is the
     * empty string, which never matches as a prefix; anything that fell
     * through the more specific roots ends up here.
     *
     * Note on the `wpmgr-agent` self-exclude: the DEFAULT_EXCLUDES segment
     * filter ALSO drops anything containing a `wpmgr-agent` segment, so the
     * plugin's own tree never makes it into the plugins bucket. This is
     * defense in depth — the segment filter is checked first.
     *
     * Track 5 ordering rationale: `plugin`/`theme`/`upload` first, exact
     * prefix match. Operator intent is "restore my plugins" — they want a
     * narrow, deterministic bucket; the catch-all only sweeps everything
     * else.
     *
     * @var array<string,array{root:string,kind:string}>
     */
    private const COMPONENT_PARTITIONS = [
        'plugins'    => ['root' => 'plugins/',    'kind' => 'plugin'],
        'themes'     => ['root' => 'themes/',     'kind' => 'theme'],
        'uploads'    => ['root' => 'uploads/',    'kind' => 'upload'],
        'wp-content' => ['root' => '',            'kind' => 'wp-content'],
    ];

    /** Save the resume cursor every N packed files. */
    private const CHECKPOINT_EVERY_FILES = 200;

    /** Emit a progress callback every N packed files. */
    private const PROGRESS_EVERY_FILES = 50;

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
     * Walk the source dir and pack into per-component rotated part files
     * inside $outDir. Resumable: if $resume carries cursors from a prior
     * call, picks up where it left off.
     *
     * Returns a flat list of part filenames (one rotating sequence per
     * component) AND a parallel list of per-part component kinds so the
     * caller can tag the manifest entries.
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
     *                                      part_kinds, files_total,
     *                                      bytes_written.
     * @return array<string,mixed> On completion: `done: true` + parts list +
     *                              parallel `part_kinds` list.
     *                              Otherwise the cursor for the next call.
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
        // The cache file format is one line per file: "<component>\t<relpath>".
        // The tab keeps the relpath bytes intact (no escaping needed; we never
        // pack literal tabs in WP filenames in practice, and a stray tab in a
        // user-uploaded filename would still parse — only the FIRST tab is the
        // delimiter).
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
        $bytesWritten   = isset($resume['bytes_written']) ? (int) $resume['bytes_written'] : 0;
        /** @var list<string> $partsCompleted */
        $partsCompleted = isset($resume['parts_completed']) && is_array($resume['parts_completed'])
            ? array_values(array_map('strval', $resume['parts_completed']))
            : [];
        /** @var list<string> $partKinds — parallel to $partsCompleted, holds the entry_kind for each */
        $partKinds      = isset($resume['part_kinds']) && is_array($resume['part_kinds'])
            ? array_values(array_map('strval', $resume['part_kinds']))
            : [];

        // Per-component current-part trackers. Each component gets its OWN
        // rotating sequence: `<component>.partNNN.zip`. The trackers carry the
        // current part number (1-indexed) and the open ZipArchive handle.
        //
        // We open lazily: the first time a component sees an entry, we open
        // its first part. A component with zero matching entries emits zero
        // part files (e.g. a site with no `mu-plugins`/`languages` ends up
        // with no `wp-content.partNNN.zip`).
        $compState = [];
        foreach (self::COMPONENT_PARTITIONS as $compName => $_) {
            $compState[$compName] = [
                'current_part'         => 1,
                'part_entries'         => 0,
                'part_estimated_bytes' => 0,
                'zip'                  => null,
                'part_path'            => '',
                'kind'                 => self::COMPONENT_PARTITIONS[$compName]['kind'],
            ];
            // Restore from the prior cursor if present.
            if (isset($resume['components'][$compName]) && is_array($resume['components'][$compName])) {
                $cs = $resume['components'][$compName];
                $compState[$compName]['current_part'] = isset($cs['current_part']) ? max(1, (int) $cs['current_part']) : 1;
            }
        }

        $cacheHandle = @fopen($cachePath, 'rb');
        if ($cacheHandle === false) {
            throw new \RuntimeException('FilesArchiver: cannot reopen path cache: ' . $cachePath);
        }

        // Seek to the requested line by counting newlines.
        if ($fileIndex > 0) {
            $skipped = 0;
            while ($skipped < $fileIndex && ($line = fgets($cacheHandle)) !== false) {
                $skipped++;
            }
            if ($skipped < $fileIndex) {
                // Cache shorter than the recorded cursor — should never
                // happen, but recover by treating as already-done.
                fclose($cacheHandle);
                return $this->buildDoneResult($partsCompleted, $partKinds, $totalFiles, $bytesWritten, $progress);
            }
        }

        $filesSinceProgress   = 0;
        $filesSinceCheckpoint = 0;
        $currentRel           = '';

        while (($line = fgets($cacheHandle)) !== false) {
            $line = rtrim($line, "\r\n");
            if ($line === '') {
                $fileIndex++;
                continue;
            }
            // Split "<component>\t<relpath>". Defensive: a malformed line
            // without a tab is treated as a wp-content (catch-all) entry.
            $tabPos = strpos($line, "\t");
            if ($tabPos === false) {
                $compName = 'wp-content';
                $rel      = $line;
            } else {
                $compName = substr($line, 0, $tabPos);
                $rel      = substr($line, $tabPos + 1);
            }
            if (!isset($compState[$compName])) {
                $compName = 'wp-content';
            }
            if ($rel === '') {
                $fileIndex++;
                continue;
            }
            $currentRel = $rel;
            $abs        = $this->sourceDir . DIRECTORY_SEPARATOR . $rel;

            // file_exists check mirrors WPvivid: files that vanished between
            // walk and pack are silently dropped, not fatal.
            if (is_file($abs) && !is_link($abs)) {
                // Lazy-open the active part for this component.
                if ($compState[$compName]['zip'] === null) {
                    $this->openActivePart($compState[$compName], $compName, $outDir);
                }
                /** @var \ZipArchive $zip */
                $zip = $compState[$compName]['zip'];
                if ($zip->addFile($abs, $rel)) {
                    $compState[$compName]['part_entries']++;
                    $compState[$compName]['part_estimated_bytes'] += (int) @filesize($abs);
                }
            }
            $fileIndex++;
            $filesSinceProgress++;
            $filesSinceCheckpoint++;

            // Per-component rotation triggers.
            if ($compState[$compName]['zip'] !== null) {
                $needRotate = $compState[$compName]['part_entries'] >= $this->maxPartEntries
                    || $compState[$compName]['part_estimated_bytes'] >= $this->maxPartBytes;
                if ($needRotate) {
                    $closed = $this->closeActivePart($compState[$compName]);
                    if ($closed['size'] > 0) {
                        $bytesWritten     += $closed['size'];
                        $partsCompleted[] = $closed['name'];
                        $partKinds[]      = $compState[$compName]['kind'];
                    }
                    $this->emitProgress($progress, [
                        'files_done'    => $fileIndex,
                        'files_total'   => $totalFiles,
                        'parts_done'    => count($partsCompleted),
                        'bytes_written' => $bytesWritten,
                        'current_file'  => $currentRel,
                        'component'     => $compName,
                    ]);
                    gc_collect_cycles();

                    $compState[$compName]['current_part']++;
                    $compState[$compName]['part_entries']         = 0;
                    $compState[$compName]['part_estimated_bytes'] = 0;
                    $filesSinceCheckpoint = 0;
                }
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
                // Same semantics as before: we don't side-effect the caller's
                // state; ZipArchive doesn't flush mid-stream; the cursor itself
                // is returned only at function exit.
                $filesSinceCheckpoint = 0;
            }
        }

        fclose($cacheHandle);

        // Close every still-open per-component active part.
        foreach ($compState as $compName => &$state) {
            if ($state['zip'] === null) {
                continue;
            }
            $closed = $this->closeActivePart($state);
            if ($closed['size'] > 0) {
                $bytesWritten     += $closed['size'];
                $partsCompleted[] = $closed['name'];
                $partKinds[]      = $state['kind'];
            }
        }
        unset($state);

        return $this->buildDoneResult($partsCompleted, $partKinds, $totalFiles, $bytesWritten, $progress, $cachePath);
    }

    /**
     * Open (or reopen for resume) the active part for the given component
     * tracker. Mutates $state in-place.
     *
     * Resume policy: ZipArchive::CREATE (no OVERWRITE) appends to an existing
     * valid zip — that's how we survive watchdog re-entry into the same part
     * for the same component.
     *
     * @param array<string,mixed> $state Per-component tracker (by reference).
     * @param string              $compName Component key (filename prefix).
     * @param string              $outDir   Absolute scratch dir.
     * @throws \RuntimeException On zip open failure.
     */
    private function openActivePart(array &$state, string $compName, string $outDir): void
    {
        $partPath  = $this->partPath($outDir, $compName, (int) $state['current_part']);
        $zip       = new \ZipArchive();
        $openFlags = is_file($partPath) ? \ZipArchive::CREATE : (\ZipArchive::CREATE | \ZipArchive::OVERWRITE);
        if ($zip->open($partPath, $openFlags) !== true) {
            throw new \RuntimeException('FilesArchiver: cannot open zip part: ' . $partPath);
        }
        $state['zip']                  = $zip;
        $state['part_path']            = $partPath;
        // If reopening on resume, the existing zip's numFiles is the
        // entry-rotation cursor. Same for filesize as the byte estimate.
        $state['part_entries']         = $zip->numFiles;
        $state['part_estimated_bytes'] = is_file($partPath) ? (int) @filesize($partPath) : 0;
    }

    /**
     * Close the currently-open part for the given component tracker. Returns
     * the on-disk size + part filename so the caller can record the durable
     * bytes-written sum and the completed-parts list.
     *
     * Empty parts (`part_entries === 0`) are unlinked rather than recorded;
     * an empty zip never enters the manifest.
     *
     * @param array<string,mixed> $state Per-component tracker (by reference).
     * @return array{name:string,size:int}
     * @throws \RuntimeException On zip close failure.
     */
    private function closeActivePart(array &$state): array
    {
        /** @var \ZipArchive $zip */
        $zip      = $state['zip'];
        $partPath = (string) $state['part_path'];
        $entries  = (int) $state['part_entries'];

        if ($entries === 0) {
            // Best-effort drop the empty placeholder. ZipArchive::close on an
            // empty new zip can still write a tiny central-directory stub;
            // unlink covers that.
            $zip->close();
            if (is_file($partPath)) {
                @unlink($partPath);
            }
            $state['zip']       = null;
            $state['part_path'] = '';
            return ['name' => '', 'size' => 0];
        }

        if (!$zip->close()) {
            throw new \RuntimeException('FilesArchiver: zip close failed for ' . $partPath);
        }
        $size = (int) @filesize($partPath);
        $state['zip']       = null;
        $state['part_path'] = '';
        return ['name' => basename($partPath), 'size' => $size];
    }

    /**
     * Walk $sourceDir and write the path-relative file list to $cachePath.
     * Returns the total line count (== total files in scope).
     *
     * Format: one line per file as "<component>\t<relpath>". Tagging at
     * discovery time means the pack loop is a pure dispatch — it never has
     * to recompute the component for an entry it's already seen.
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

            // Only file entries make it into the cache.
            if (!$info->isFile()) {
                continue;
            }

            $component = $this->classifyComponent($rel);
            fwrite($handle, $component . "\t" . $rel . "\n");
            $count++;
        }

        fclose($handle);
        return $count;
    }

    /**
     * Map a wp-content-relative path to its component bucket name. Path-
     * prefix match against COMPONENT_PARTITIONS roots; the `wp-content`
     * catch-all takes anything that didn't match a specific root.
     *
     * @param string $relativePath Path relative to $sourceDir, `/`-separated.
     * @return string Component key (one of the COMPONENT_PARTITIONS keys).
     */
    private function classifyComponent(string $relativePath): string
    {
        // Normalize: strip any leading slash so prefix match is deterministic.
        $p = ltrim($relativePath, '/');
        foreach (self::COMPONENT_PARTITIONS as $compName => $cfg) {
            $root = (string) $cfg['root'];
            if ($root === '') {
                // Catch-all — always matches; we save it for last by virtue
                // of being the last entry in COMPONENT_PARTITIONS.
                continue;
            }
            if (strncmp($p, $root, strlen($root)) === 0) {
                return $compName;
            }
        }
        return 'wp-content';
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
     * Build the absolute path of part N for the given component (1-indexed,
     * 3-digit zero-padded). Each component has its own filename prefix:
     * `plugins.partNNN.zip`, `themes.partNNN.zip`, etc.
     *
     * @param string $outDir   Absolute scratch dir.
     * @param string $compName Component key (used as the filename prefix).
     * @param int    $n        Part number.
     * @return string
     */
    private function partPath(string $outDir, string $compName, int $n): string
    {
        return $outDir . DIRECTORY_SEPARATOR . $compName . '.part' . str_pad((string) $n, 3, '0', STR_PAD_LEFT) . '.zip';
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
     * @param list<string> $partKinds      Parallel list of per-part entry_kinds.
     * @param int          $totalFiles     Total file count from the cache.
     * @param int          $bytesWritten   Sum of part sizes on disk.
     * @param callable     $progress       Progress callback (see archive()).
     * @param string|null  $cachePath      Cache file to clean up, if any.
     * @return array<string,mixed>
     */
    private function buildDoneResult(array $parts, array $partKinds, int $totalFiles, int $bytesWritten, callable $progress, ?string $cachePath = null): array
    {
        // Cache file is no longer needed once we're done; leaving it under
        // the per-run scratch dir means it gets cleaned with the scratch.
        // We don't unlink so a post-mortem can inspect.
        unset($cachePath);

        $this->emitProgress($progress, [
            'done'          => true,
            'parts'         => $parts,
            'part_kinds'    => $partKinds,
            'files_total'   => $totalFiles,
            'bytes_written' => $bytesWritten,
        ]);

        return [
            'done'          => true,
            'parts'         => $parts,
            'part_kinds'    => $partKinds,
            'parts_total'   => count($parts),
            'files_total'   => $totalFiles,
            'bytes_written' => $bytesWritten,
        ];
    }
}
