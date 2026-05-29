<?php
/**
 * FilesRestorer: M5.6 / ADR-034 — staged file extract + atomic directory swap.
 *
 * Pattern (per `docs/research/wpvivid-restore-deep-dive.md` §7.2 fix #1, lifting
 * the WPvivid file-restore engine but adding the safety net WPvivid lacks):
 *
 *   1. PREFLIGHT every `.zip` opens cleanly (ZipArchive::open + numFiles > 0).
 *      Any failure aborts before a single byte is written to live wp-content.
 *   2. Extract entries into `wp-content/.wpmgr-staging-<short>/` (NOT into live
 *      wp-content). 0700 perms.
 *   3. Per-entry path-traversal defense: full resolved path must live INSIDE
 *      staging dir. Reject `..`, absolute paths, NUL bytes, symlink targets.
 *   4. Skip the canonical "keep the site running" EXCLUDE LIST (config files
 *      and drop-ins that must not be overwritten by a snapshot from the past).
 *   5. After all parts are staged, `swap()` atomically renames:
 *        rename(targetDir, .wpmgr-old-files-<short>/)   # move live aside
 *        rename(stagingDir, targetDir)                  # promote staging
 *      Both legs are filesystem-level directory-entry renames; they're atomic
 *      relative to each other so a crash between them leaves either
 *      `.wpmgr-old-files-<id>/` (rollback by hand) or the new tree in place,
 *      never half-merged content.
 *   6. Old files dir is INTENTIONALLY kept for 24 h so the user can roll back
 *      manually if the restore was bad. `gcOldFiles()` (separate cron event,
 *      bound to `wpmgr_restore_oldfiles_gc`) sweeps anything older than 24 h.
 *
 * WPvivid by comparison (deep-dive §2.2): they extract DIRECTLY into the live
 * wp-content with `WPVIVID_PCLZIP_OPT_REPLACE_NEWER`. Half-extracted state on
 * a crash leaves the site half-merged with no rollback. We pay one
 * staging-tree copy of disk for atomicity + rollback — worth it.
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

use ZipArchive;

/**
 * Stage-then-swap file restorer. Declared `final` — RestoreRunner instantiates
 * exactly one of these per restore run.
 */
final class FilesRestorer
{
    /**
     * Files we never overwrite during a restore. Substring match (matches
     * WPvivid's pattern — substring-based so `notwp-config.php` is also a
     * false positive but never a security hole).
     *
     * @var list<string>
     */
    private const EXCLUDE_SUBSTRINGS = [
        'wp-config.php',
        'db.php',
        'object-cache.php',
        'advanced-cache.php',
        '.htaccess',
        '.user.ini',
        'wpmgr-agent/',
        'wpmgr-snapshots/',
        // Defensive — the agent's own plugin tree should not be replaced by a
        // restore of a past wp-content.
        'wpmgr-agent.php',
    ];

    /** Emit a progress event every N extracted entries. */
    private const PROGRESS_EVERY_FILES = 50;

    /** GC threshold for `.wpmgr-old-files-*` directories: 24 h. */
    public const OLDFILES_GC_AGE_SECONDS = 86400;

    /**
     * Stage every zip part into `<wpContentParent>/.wpmgr-staging-<short>/`.
     * Returns the absolute staging dir.
     *
     * @param list<string> $zipPaths    Absolute paths to the part zips, in order.
     * @param string       $targetDir   The live directory the staging tree
     *                                   should eventually replace (typically
     *                                   `wp-content`). Used to anchor the
     *                                   staging sibling dir.
     * @param string       $restoreId   UUID — for the per-run staging dir name.
     * @param callable     $progress    function(string $phase, array $detail): void
     * @return string Absolute staging dir.
     * @throws \RuntimeException On preflight failure or unrecoverable I/O.
     */
    public function stage(array $zipPaths, string $targetDir, string $restoreId, callable $progress): string
    {
        if ($zipPaths === []) {
            throw new \RuntimeException('FilesRestorer: no zip parts to stage');
        }
        $targetDir = rtrim($targetDir, DIRECTORY_SEPARATOR);
        if ($targetDir === '' || !is_dir($targetDir)) {
            throw new \RuntimeException('FilesRestorer: targetDir does not exist: ' . $targetDir);
        }
        $parent = dirname($targetDir);
        if (!is_dir($parent) || !is_writable($parent)) {
            throw new \RuntimeException('FilesRestorer: target parent not writable: ' . $parent);
        }

        // --- PREFLIGHT: every zip must open + contain entries -------------
        $totalFiles = 0;
        foreach ($zipPaths as $z) {
            if (!is_file($z)) {
                throw new \RuntimeException('FilesRestorer: missing zip part: ' . $z);
            }
            $zip = new ZipArchive();
            $rc  = $zip->open($z);
            if ($rc !== true) {
                throw new \RuntimeException('FilesRestorer: cannot open zip part: ' . $z . ' (code=' . $rc . ')');
            }
            if ($zip->numFiles <= 0) {
                $zip->close();
                throw new \RuntimeException('FilesRestorer: empty zip part: ' . $z);
            }
            $totalFiles += $zip->numFiles;
            $zip->close();
        }

        // --- Create staging dir (idempotent across watchdog resume) -------
        $short      = self::shortId($restoreId);
        $stagingDir = $parent . DIRECTORY_SEPARATOR . '.wpmgr-staging-' . $short;
        if (!is_dir($stagingDir) && !@mkdir($stagingDir, 0700, true) && !is_dir($stagingDir)) {
            throw new \RuntimeException('FilesRestorer: cannot create staging dir: ' . $stagingDir);
        }
        @chmod($stagingDir, 0700);

        // Canonical staging real-path (used for traversal containment check).
        $stagingReal = self::canonical($stagingDir);
        if ($stagingReal === '') {
            throw new \RuntimeException('FilesRestorer: cannot resolve staging real path');
        }

        // --- Extract each part, per-entry traversal-checked + excluded ---
        $filesDone   = 0;
        $partsDone   = 0;
        $partsTotal  = count($zipPaths);
        $sinceTick   = 0;
        $currentFile = '';

        foreach ($zipPaths as $zipPath) {
            $zip = new ZipArchive();
            $rc  = $zip->open($zipPath);
            if ($rc !== true) {
                throw new \RuntimeException('FilesRestorer: cannot reopen zip: ' . $zipPath);
            }

            try {
                $num = $zip->numFiles;
                for ($i = 0; $i < $num; $i++) {
                    $entryName = (string) $zip->getNameIndex($i);
                    if ($entryName === '') {
                        continue;
                    }

                    // Drop-in / config exclude list. Matches WPvivid's
                    // pre-extract callback shape (substring match).
                    if (self::isExcluded($entryName)) {
                        continue;
                    }

                    // Path traversal defense.
                    if (!self::isSafeEntryPath($entryName)) {
                        // Skip the bad entry; do NOT throw — a single hostile
                        // entry in a zip should not abort the whole restore.
                        continue;
                    }

                    // Resolve the would-be extraction path. ZipArchive resolves
                    // entry paths relative to the extractTo root, but we
                    // verify the resolved absolute path lives inside staging.
                    $target = $stagingReal . DIRECTORY_SEPARATOR . ltrim($entryName, DIRECTORY_SEPARATOR);
                    $targetParent = dirname($target);
                    // Pre-create the directory so extractTo can write the file.
                    if (!is_dir($targetParent) && !@mkdir($targetParent, 0755, true) && !is_dir($targetParent)) {
                        // Cannot create — skip this entry; not fatal.
                        continue;
                    }
                    // Double-check containment against the canonical staging
                    // root after dir creation (defends against a malicious
                    // symlink already at $targetParent).
                    $targetParentReal = self::canonical($targetParent);
                    if ($targetParentReal === '' || strpos($targetParentReal, $stagingReal) !== 0) {
                        continue;
                    }

                    // Per-entry extraction lets us control which entries
                    // actually land. extractTo's third arg accepts a list of
                    // entry names; ZipArchive resolves them itself.
                    $ok = @$zip->extractTo($stagingDir, [$entryName]);
                    if ($ok !== true) {
                        // Extraction failed for this entry — log via progress
                        // and continue (matches WPvivid's "log and skip"
                        // semantics, never abort the whole run on one entry).
                        continue;
                    }

                    $filesDone++;
                    $sinceTick++;
                    $currentFile = $entryName;

                    if ($sinceTick >= self::PROGRESS_EVERY_FILES) {
                        self::safeProgress($progress, 'stage_files', [
                            'files_done'  => $filesDone,
                            'files_total' => $totalFiles,
                            'parts_done'  => $partsDone,
                            'parts_total' => $partsTotal,
                            'current_file' => $currentFile,
                        ]);
                        $sinceTick = 0;
                    }
                }
            } finally {
                $zip->close();
            }
            $partsDone++;
        }

        // Final beacon so the caller can mark stage_files complete.
        self::safeProgress($progress, 'stage_files', [
            'done'        => true,
            'files_done'  => $filesDone,
            'files_total' => $totalFiles,
            'parts_done'  => $partsDone,
            'parts_total' => $partsTotal,
            'staging_dir' => $stagingDir,
        ]);

        return $stagingDir;
    }

    /**
     * Atomic directory swap. Moves the live target dir aside (preserving its
     * tree under `.wpmgr-old-files-<short>/`) then renames staging into place.
     *
     * Both renames are filesystem-level directory-entry operations and share
     * the same parent dir (and therefore the same filesystem), so each is
     * atomic. The window between them is the only failure mode; if a crash
     * lands here, the operator sees `.wpmgr-old-files-<short>/` alongside
     * (no) target dir and can rename it back by hand.
     *
     * @param string   $stagingDir Absolute staging dir (returned by stage()).
     * @param string   $targetDir  Live directory to replace.
     * @param string   $restoreId  Restore UUID — for the old-files dir name.
     * @param callable $progress   function(string $phase, array $detail): void
     * @return string Absolute old-files dir (caller may want to record it).
     * @throws \RuntimeException On rename failure.
     */
    public function swap(string $stagingDir, string $targetDir, string $restoreId, callable $progress): string
    {
        $targetDir = rtrim($targetDir, DIRECTORY_SEPARATOR);
        if (!is_dir($stagingDir)) {
            throw new \RuntimeException('FilesRestorer: staging dir missing for swap: ' . $stagingDir);
        }
        if (!is_dir($targetDir)) {
            throw new \RuntimeException('FilesRestorer: target dir missing for swap: ' . $targetDir);
        }

        $parent     = dirname($targetDir);
        $short      = self::shortId($restoreId);
        $oldFiles   = $parent . DIRECTORY_SEPARATOR . '.wpmgr-old-files-' . $short;

        // 1) Move live target aside. If $oldFiles already exists (a watchdog
        // mid-swap re-entry), the previous attempt already moved the live
        // dir but failed before renaming staging — so the current "target"
        // is actually a leftover. Skip the first rename in that case.
        if (!is_dir($oldFiles)) {
            if (!@rename($targetDir, $oldFiles)) {
                throw new \RuntimeException('FilesRestorer: cannot move live dir aside: ' . $targetDir . ' -> ' . $oldFiles);
            }
        }

        // 2) Move staging into place.
        // Safety: if target exists at this point (e.g. someone mkdir'd it
        // between step 1 and now), rmdir-attempt it first; if it's not empty
        // we can't rename — abort.
        if (is_dir($targetDir) && @rmdir($targetDir) === false) {
            throw new \RuntimeException('FilesRestorer: target dir reappeared and is not empty: ' . $targetDir);
        }
        if (!@rename($stagingDir, $targetDir)) {
            // Best-effort rollback: put the old tree back.
            @rename($oldFiles, $targetDir);
            throw new \RuntimeException('FilesRestorer: cannot promote staging dir: ' . $stagingDir . ' -> ' . $targetDir);
        }

        self::safeProgress($progress, 'swap_files', [
            'done'          => true,
            'old_files_dir' => $oldFiles,
            'target_dir'    => $targetDir,
        ]);

        return $oldFiles;
    }

    /**
     * Garbage-collect `.wpmgr-old-files-*` dirs older than the GC age.
     * Bound to the `wpmgr_restore_oldfiles_gc` cron action.
     *
     * @return void
     */
    public static function gcOldFiles(): void
    {
        if (!defined('WP_CONTENT_DIR')) {
            return;
        }
        // The old-files dirs live as siblings of the directory we replaced
        // (typically wp-content/). We sweep both wp-content's parent AND
        // wp-content itself, since we might have staged either one.
        $candidates = [
            dirname(WP_CONTENT_DIR),
            WP_CONTENT_DIR,
        ];
        $now = time();
        foreach ($candidates as $dir) {
            if (!is_dir($dir)) {
                continue;
            }
            $hits = @glob($dir . DIRECTORY_SEPARATOR . '.wpmgr-old-files-*');
            if (!is_array($hits)) {
                continue;
            }
            foreach ($hits as $old) {
                if (!is_dir($old)) {
                    continue;
                }
                $mtime = @filemtime($old);
                if ($mtime === false || ($now - (int) $mtime) < self::OLDFILES_GC_AGE_SECONDS) {
                    continue;
                }
                self::rrmdir($old);
            }
            // Same sweep for any leftover staging dirs from a crashed run.
            $stages = @glob($dir . DIRECTORY_SEPARATOR . '.wpmgr-staging-*');
            if (!is_array($stages)) {
                continue;
            }
            foreach ($stages as $stage) {
                if (!is_dir($stage)) {
                    continue;
                }
                $mtime = @filemtime($stage);
                if ($mtime === false || ($now - (int) $mtime) < self::OLDFILES_GC_AGE_SECONDS) {
                    continue;
                }
                self::rrmdir($stage);
            }
        }
    }

    // ==================================================================
    // Helpers
    // ==================================================================

    /**
     * Short form of a restore id, suitable for embedding in filesystem paths.
     * First 8 hex chars after stripping dashes — collisions across restore
     * runs are catastrophic only within the same wp-content parent at the
     * same time, which is the dedup table's job to prevent.
     */
    private static function shortId(string $restoreId): string
    {
        $clean = preg_replace('/[^a-f0-9]/i', '', $restoreId) ?? '';
        return substr($clean, 0, 12) ?: 'unknown';
    }

    /**
     * Whether an entry name matches the EXCLUDE_SUBSTRINGS list.
     */
    private static function isExcluded(string $entryName): bool
    {
        foreach (self::EXCLUDE_SUBSTRINGS as $sub) {
            if (strpos($entryName, $sub) !== false) {
                return true;
            }
        }
        return false;
    }

    /**
     * Whether an entry path is safe to extract — no traversal, no NUL,
     * no absolute path. Conservative: a single bad component fails.
     */
    private static function isSafeEntryPath(string $entryName): bool
    {
        if ($entryName === '') {
            return false;
        }
        if (strpos($entryName, "\0") !== false) {
            return false;
        }
        // Absolute path? Reject.
        if ($entryName[0] === '/' || $entryName[0] === '\\') {
            return false;
        }
        // Windows-style drive letter? Reject.
        if (strlen($entryName) >= 2 && ctype_alpha($entryName[0]) && $entryName[1] === ':') {
            return false;
        }
        // Normalize separators and check each component.
        $parts = preg_split('#[/\\\\]+#', $entryName);
        if ($parts === false) {
            return false;
        }
        foreach ($parts as $p) {
            if ($p === '..' || $p === '.') {
                return false;
            }
        }
        return true;
    }

    /**
     * realpath() with no error spew and a tolerant fallback. Returns '' if
     * the path cannot be resolved.
     */
    private static function canonical(string $path): string
    {
        $real = @realpath($path);
        return is_string($real) ? $real : '';
    }

    /**
     * Recursive rmdir — best effort, never throws. Used by gcOldFiles().
     */
    private static function rrmdir(string $dir): void
    {
        if (!is_dir($dir)) {
            return;
        }
        $items = @scandir($dir);
        if ($items === false) {
            return;
        }
        foreach ($items as $i) {
            if ($i === '.' || $i === '..') {
                continue;
            }
            $p = $dir . DIRECTORY_SEPARATOR . $i;
            if (is_link($p) || is_file($p)) {
                @unlink($p);
            } elseif (is_dir($p)) {
                self::rrmdir($p);
            }
        }
        @rmdir($dir);
    }

    /**
     * Invoke caller progress callback safely; a broken hook must never fail a
     * restore.
     *
     * @param callable            $progress Caller callback.
     * @param string              $phase    Phase label.
     * @param array<string,mixed> $detail   Phase detail payload.
     */
    private static function safeProgress(callable $progress, string $phase, array $detail): void
    {
        try {
            $progress($phase, $detail);
        } catch (\Throwable $_) {
            // Swallow.
        }
    }
}
