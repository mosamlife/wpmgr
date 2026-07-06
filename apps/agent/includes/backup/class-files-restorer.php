<?php
/**
 * FilesRestorer: M5.6 / ADR-034 — staged file extract + atomic directory swap.
 *
 * Pattern (staged-extract + atomic-swap, §7.2 fix #1 — adds the safety nets
 * that a direct in-place extract lacks):
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
 * Extracting directly into live wp-content (as some plugins do) leaves the site
 * half-merged on a crash with no rollback. We pay one staging-tree copy of disk
 * for atomicity + rollback — worth it.
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
     * wp-content-ROOT drop-ins / config we never overwrite during a restore.
     * Matched by EXACT full-path equality against the normalized entry name
     * (i.e. the entry must sit at the top of the archive, with no directory
     * prefix) — these filenames only carry drop-in/config semantics when
     * WordPress finds them at the wp-content root. A same-named file nested
     * inside a plugin/theme (e.g. `includes/modules/redirections/class-db.php`,
     * or a bundled `redis-cache/includes/object-cache.php` template) is
     * ordinary plugin content and MUST be restored — see GH #147, where a
     * prior unanchored substring match dropped every nested `*db.php` file
     * silently at extract time.
     *
     * `wp-config.php`, `.htaccess`, and `.user.ini` live in ABSPATH, OUTSIDE
     * wp-content, so a well-formed wp-content archive should never contain
     * them at all; they're listed here purely as belt-and-suspenders in case
     * one ever appears at the root of a wp-content zip.
     *
     * @var list<string>
     */
    private const EXCLUDE_EXACT_PATHS = [
        'wp-config.php',
        'db.php',
        'object-cache.php',
        'advanced-cache.php',
        '.htaccess',
        '.user.ini',
    ];

    /**
     * WPMgr's own scratch/secret asset names, excluded wherever they appear —
     * matched by EXACT segment equality against ANY `/`-separated component
     * of the normalized entry name (unlike EXCLUDE_EXACT_PATHS, depth doesn't
     * matter here). Safe to match at any depth because these names are
     * unique to the agent and never legitimately collide with third-party
     * plugin/theme file or directory names.
     *
     * - `wpmgr-agent`      — the agent scratch + keystore root (either at the
     *                        wp-content root, `wp-content/wpmgr-agent/`, or
     *                        the plugin directory `plugins/wpmgr-agent/`).
     * - `wpmgr-agent.php`  — the single-file plugin variant / main plugin
     *                        file, wherever it lands in the tree.
     * - `wpmgr-snapshots`  — UpdateCommand pre-update snapshot store; lives
     *                        under `uploads/wpmgr-snapshots/` by default or
     *                        the legacy `wp-content/wpmgr-snapshots/` on
     *                        read-only-uploads hosts — either way it must
     *                        never be restored from a stale backup.
     * - `wpmgr-object-cache-config.php` — object-cache credentials file
     *                        (plaintext Redis password); the live credential
     *                        must remain intact across a restore.
     * - `.wpmgr-oc-state.json` — FD-6 object-cache cool-down state; must not
     *                        be restored so a stale window cannot suppress
     *                        Redis on a healthy site.
     *
     * @var list<string>
     */
    private const EXCLUDE_SEGMENTS = [
        'wpmgr-agent',
        'wpmgr-agent.php',
        'wpmgr-snapshots',
        'wpmgr-object-cache-config.php',
        '.wpmgr-oc-state.json',
    ];

    /** Emit a progress event every N extracted entries. */
    private const PROGRESS_EVERY_FILES = 50;

    /**
     * Default GC threshold for `.wpmgr-old-files-*` directories: 1 h.
     *
     * V0 originally kept old files for 24 h so the operator had a long window
     * to roll back by hand. In practice, on small VPS hosts (5–10 GB free)
     * keeping a full wp-content copy for a day routinely tipped the disk into
     * red. The 0.9.5 restore-safety release flips the default to 1 h
     * (sync-cleaned at the end of `cleanup`) and gates the historical 24 h
     * behavior behind the explicit `keep_old_files=true` task param. See
     * `RestoreRunner::runCleanup()`.
     */
    public const OLDFILES_GC_AGE_SECONDS = 3600;

    /**
     * Opt-in GC threshold when the caller asks to keep the rollback tree
     * around (task param `keep_old_files=true`). 24 h matches the pre-0.9.5
     * baseline so an operator who explicitly wants the old behavior can still
     * get it without redeploying.
     */
    public const OLDFILES_GC_AGE_SECONDS_LONG = 86400;

    /**
     * Anchored path-prefix denylist applied at zip-extract time. Any entry
     * whose normalized name starts with one of these prefixes is silently
     * dropped before it reaches staging. This is a THIRD, belt-and-suspenders
     * layer on top of `EXCLUDE_SEGMENTS` — the segment check above already
     * catches these paths at any depth (a `wpmgr-agent` or `wpmgr-snapshots`
     * segment anywhere in the tree), but an anchored prefix is stricter still
     * for the specific top-level locations these trees are known to live at,
     * so it keeps working even if a future change ever narrowed the segment
     * list.
     *
     * Backslashes in entry names are normalized to `/` before matching so a
     * Windows-packed zip can't bypass the check with `plugins\wpmgr-agent\`.
     *
     * @var list<string>
     */
    private const ENTRY_DENYLIST_PREFIXES = [
        'plugins/wpmgr-agent/',
        'plugins/wpmgr-agent.php',
        'wpmgr-agent/',
        'wpmgr-snapshots/',
    ];

    /**
     * Live-tree paths copied INTO staging just before the swap, so a restore
     * never deletes the running agent or active drop-ins by replacing them
     * with stale content from a months-old backup.
     *
     * Each entry is a wp-content-relative path (file or directory). Copies
     * are recursive for directories. Entries already present in staging are
     * NOT overwritten — only missing entries are pulled forward from live.
     *
     * `db.php`, `object-cache.php`, and `advanced-cache.php` are excluded
     * from staging entirely (see `EXCLUDE_EXACT_PATHS`) because a snapshot
     * from a different host/config (e.g. a Memcached-era backup restored
     * onto a Redis host) must not clobber the live drop-in — so staging will
     * NEVER already contain them, and the entries below unconditionally pull
     * the live copy forward across the swap (GH #147 fix — previously these
     * were excluded from staging but NOT preserved-from-live, so a restore
     * silently deleted the running drop-in with no replacement).
     *
     * Note on scope: `swap()` operates on wp-content only, so `wp-config.php`,
     * `.htaccess`, and `.user.ini` (which live in ABSPATH, OUTSIDE wp-content)
     * are not in scope here. They're already filtered by the backup-side
     * excludes + by `EXCLUDE_EXACT_PATHS` above; they were never written to
     * staging to begin with, so they continue to live at their original
     * ABSPATH location across a restore.
     *
     * @var list<string>
     */
    private const PRESERVE_FROM_LIVE = [
        // Agent plugin itself (live binary that drives the restore — losing
        // this mid-restore = self-DoS).
        'plugins/wpmgr-agent',
        'plugins/wpmgr-agent.php',
        // Agent scratch + keystore root at wp-content/wpmgr-agent/. Holds
        // restores/<id>/database.sql.gz (read by PHASE_RESTORE_DB AFTER this
        // swap completes), runs/<id>/ (backup-side scratch), and
        // .wpmgr-agent-master.key (keystore). v0.9.7 swap_files destroyed
        // this dir; v0.9.9 explicitly preserves it from live → staging so the
        // dump file survives the swap and restore_db finds it. Same pattern
        // leading backup plugins use for their own scratch dirs.
        'wpmgr-agent',
        // Host-state drop-ins: keep the LIVE copy across a restore — GH #147.
        'db.php',
        'object-cache.php',
        'advanced-cache.php',
    ];

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
            throw new \RuntimeException('FilesRestorer: targetDir does not exist: ' . esc_html($targetDir)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
        }
        $parent = dirname($targetDir);
        if (!is_dir($parent) || !is_writable($parent)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_is_writable -- headless agent; WP_Filesystem never initialized; direct writability probe is the only option
            throw new \RuntimeException('FilesRestorer: target parent not writable: ' . esc_html($parent)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
        }

        // --- PREFLIGHT: every zip must open + contain entries -------------
        $totalFiles = 0;
        foreach ($zipPaths as $z) {
            if (!is_file($z)) {
                throw new \RuntimeException('FilesRestorer: missing zip part: ' . esc_html($z)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
            }
            $zip = new ZipArchive();
            $rc  = $zip->open($z);
            if ($rc !== true) {
                throw new \RuntimeException('FilesRestorer: cannot open zip part: ' . esc_html($z) . ' (code=' . esc_html((string) $rc) . ')'); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
            }
            if ($zip->numFiles <= 0) {
                $zip->close();
                throw new \RuntimeException('FilesRestorer: empty zip part: ' . esc_html($z)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
            }
            $totalFiles += $zip->numFiles;
            $zip->close();
        }

        // --- Create staging dir (idempotent across watchdog resume) -------
        $short      = self::shortId($restoreId);
        $stagingDir = $parent . DIRECTORY_SEPARATOR . '.wpmgr-staging-' . $short;
        if (!is_dir($stagingDir) && !@mkdir($stagingDir, 0700, true) && !is_dir($stagingDir)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir -- explicit 0700 perms on staging scratch dir; wp_mkdir_p would apply the wider FS_CHMOD_DIR
            throw new \RuntimeException('FilesRestorer: cannot create staging dir: ' . esc_html($stagingDir)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
        }
        @chmod($stagingDir, 0700); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0700); WP_Filesystem would coerce to wider FS_CHMOD_DIR

        // Canonical staging real-path (used for traversal containment check).
        $stagingReal = self::canonical($stagingDir);
        if ($stagingReal === '') {
            throw new \RuntimeException('FilesRestorer: cannot resolve staging real path'); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
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
                throw new \RuntimeException('FilesRestorer: cannot reopen zip: ' . $zipPath); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to server log/SSE, not browser output
            }

            try {
                $num = $zip->numFiles;
                for ($i = 0; $i < $num; $i++) {
                    $entryName = (string) $zip->getNameIndex($i);
                    if ($entryName === '') {
                        continue;
                    }

                    // Drop-in / config / WPMgr-scratch exclude list — anchored
                    // match (exact path or exact segment; never substring).
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
                    if (!is_dir($targetParent) && !wp_mkdir_p($targetParent) && !is_dir($targetParent)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir -- converted to wp_mkdir_p
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
                        // and continue — log and skip semantics, never
                        // abort the whole run on one entry.
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
     * Track 5 — per-component subset of the wp-content top-level layout that
     * `swapComponents()` knows how to swap independently. Keys match the
     * Track-5 manifest entry_kind values; values are the wp-content-relative
     * subdirectory for the component.
     *
     * The `wp-content` entry isn't here because it's the catch-all: any
     * top-level item in staging that's NOT one of these subdirs falls under
     * the "wp-content others" component and is swapped item-by-item.
     *
     * @var array<string,string>
     */
    private const COMPONENT_SUBDIRS = [
        'plugin' => 'plugins',
        'theme'  => 'themes',
        'upload' => 'uploads',
    ];

    /**
     * Atomic directory swap. Moves the live target dir aside (preserving its
     * tree under `.wpmgr-old-files-<short>/`) then renames staging into place.
     *
     * This is the WHOLE-wp-content swap path — used when:
     *   (a) the snapshot is a pre-Track-5 (legacy) one with entry_kind='file'
     *       on its parts, OR
     *   (b) the operator selected ALL components (the "Everything" case —
     *       still faster + simpler than per-component swap).
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
            throw new \RuntimeException('FilesRestorer: staging dir missing for swap: ' . esc_html($stagingDir)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
        }
        if (!is_dir($targetDir)) {
            throw new \RuntimeException('FilesRestorer: target dir missing for swap: ' . esc_html($targetDir)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
        }

        $parent     = dirname($targetDir);
        $short      = self::shortId($restoreId);
        $oldFiles   = $parent . DIRECTORY_SEPARATOR . '.wpmgr-old-files-' . $short;

        // 0) PRESERVE-FROM-LIVE: copy a small allowlist of paths from the live
        // tree into staging if staging doesn't already contain them. The
        // canonical case is the wpmgr-agent plugin itself — the running agent
        // is on disk in `wp-content/plugins/wpmgr-agent/`, and a restore from
        // a backup taken before the agent was installed would otherwise erase
        // it (the very process that's running). Drop-ins (`db.php`,
        // `object-cache.php`, etc.) get the same treatment because they're
        // host-state-specific (a Redis drop-in on the live host should NOT be
        // replaced by a snapshot from a Memcached-era backup).
        //
        // Scope reminder: this only covers paths inside wp-content; ABSPATH-
        // level files (`wp-config.php`, `.htaccess`, `.user.ini`) live above
        // the swap surface and are preserved by virtue of not being touched.
        self::preserveFromLive($targetDir, $stagingDir);

        // 1) Move live target aside. If $oldFiles already exists (a watchdog
        // mid-swap re-entry), the previous attempt already moved the live
        // dir but failed before renaming staging — so the current "target"
        // is actually a leftover. Skip the first rename in that case.
        if (!is_dir($oldFiles)) {
            if (!@rename($targetDir, $oldFiles)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-filesystem swap; WP_Filesystem::move() is copy+delete (non-atomic) and breaks crash/watchdog-resume safety
                throw new \RuntimeException('FilesRestorer: cannot move live dir aside: ' . esc_html($targetDir) . ' -> ' . esc_html($oldFiles)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
            }
        }

        // 2) Move staging into place.
        // Safety: if target exists at this point (e.g. someone mkdir'd it
        // between step 1 and now), rmdir-attempt it first; if it's not empty
        // we can't rename — abort.
        if (is_dir($targetDir) && @rmdir($targetDir) === false) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- removes an empty server-derived scratch dir; WP_Filesystem not initialized
            throw new \RuntimeException('FilesRestorer: target dir reappeared and is not empty: ' . esc_html($targetDir)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
        }
        if (!@rename($stagingDir, $targetDir)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-filesystem swap; WP_Filesystem::move() is copy+delete (non-atomic) and breaks crash/watchdog-resume safety
            // Best-effort rollback: put the old tree back.
            @rename($oldFiles, $targetDir); // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-filesystem rollback swap; WP_Filesystem::move() is non-atomic
            throw new \RuntimeException('FilesRestorer: cannot promote staging dir: ' . esc_html($stagingDir) . ' -> ' . esc_html($targetDir)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
        }

        self::safeProgress($progress, 'swap_files', [
            'done'          => true,
            'old_files_dir' => $oldFiles,
            'target_dir'    => $targetDir,
        ]);

        return $oldFiles;
    }

    /**
     * Track 5 — per-component swap. Replaces ONLY the wp-content subdirectories
     * listed in $components, leaving the rest of the live wp-content untouched.
     *
     * Routing — what each component swaps:
     *   'plugin'     -> wp-content/plugins/
     *   'theme'      -> wp-content/themes/
     *   'upload'     -> wp-content/uploads/
     *   'wp-content' -> every top-level item in wp-content that is NOT
     *                   plugins/themes/uploads (mu-plugins, languages,
     *                   drop-ins, custom dirs)
     *
     * Per-component algorithm:
     *   For each component:
     *     1. Compute target subdir + the corresponding staging subdir.
     *     2. Atomically rename live subdir → `.wpmgr-old-<comp>-<short>/`
     *        (sibling of the target dir).
     *     3. Atomically rename staging subdir → live subdir.
     *   For the 'wp-content' (others) component, do this for EACH non-managed
     *   top-level entry in staging.
     *
     * PRESERVE_FROM_LIVE re-application: the whole-swap path calls
     * preserveFromLive() to pull a small allowlist of paths (the wpmgr-agent
     * plugin itself + drop-ins) from live into staging BEFORE the swap, so a
     * months-old snapshot can't erase the running agent. The per-component
     * path applies the same allowlist filtered to the components actually
     * being swapped — e.g. when swapping only `plugin` we preserve
     * `plugins/wpmgr-agent`, but when swapping only `upload` there's nothing
     * to preserve from that allowlist.
     *
     * Returns the absolute path of the staging dir if it still exists (so the
     * caller can clean it up); also returns the rollback dirs as a map of
     * component => absolute old-dir path.
     *
     * @param string       $stagingDir Absolute staging dir (returned by stage()).
     * @param string       $targetDir  Live wp-content dir.
     * @param list<string> $components Component names to swap. Must be a
     *                                 non-empty subset of
     *                                 {'plugin','theme','upload','wp-content'}.
     * @param string       $restoreId  Restore UUID — for the old-* dir names.
     * @param callable     $progress   function(string $phase, array $detail): void
     * @return array{old_dirs:array<string,string>,staging_dir:string}
     * @throws \RuntimeException On rename failure.
     */
    public function swapComponents(string $stagingDir, string $targetDir, array $components, string $restoreId, callable $progress): array
    {
        $targetDir = rtrim($targetDir, DIRECTORY_SEPARATOR);
        if (!is_dir($stagingDir)) {
            throw new \RuntimeException('FilesRestorer: staging dir missing for swap: ' . esc_html($stagingDir)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
        }
        if (!is_dir($targetDir)) {
            throw new \RuntimeException('FilesRestorer: target dir missing for swap: ' . esc_html($targetDir)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
        }
        if ($components === []) {
            throw new \RuntimeException('FilesRestorer: swapComponents called with empty component list');
        }

        $stagingDir = rtrim($stagingDir, DIRECTORY_SEPARATOR);
        $short      = self::shortId($restoreId);
        $oldDirs    = [];

        // PRESERVE_FROM_LIVE for per-component: only the wpmgr-agent plugin
        // tree (plugins/wpmgr-agent) is in scope, and only when 'plugin' is
        // being swapped. The drop-ins (db.php, object-cache.php) live at the
        // wp-content root and are only relevant when the 'wp-content'
        // (catch-all others) component is being swapped — we handle them
        // there.
        self::preserveFromLiveScoped($targetDir, $stagingDir, $components);

        foreach ($components as $comp) {
            if (isset(self::COMPONENT_SUBDIRS[$comp])) {
                // plugin / theme / upload — swap the top-level subdir.
                $subdir       = self::COMPONENT_SUBDIRS[$comp];
                $liveSub      = $targetDir . DIRECTORY_SEPARATOR . $subdir;
                $stagingSub   = $stagingDir . DIRECTORY_SEPARATOR . $subdir;
                $oldSub       = $targetDir . DIRECTORY_SEPARATOR . '.wpmgr-old-' . $subdir . '-' . $short;

                if (!is_dir($stagingSub)) {
                    // The staging side has no payload for this component.
                    // That's a contract violation — the CP's selectEntries
                    // already validated the snapshot contains entries for the
                    // requested component. Treat as a hard fail rather than
                    // silently leave the live subdir in place.
                    throw new \RuntimeException('FilesRestorer: staging missing component subdir: ' . esc_html($stagingSub)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
                }

                // Move live subdir aside (skip if a watchdog mid-swap re-entry
                // already did this leg).
                if (is_dir($liveSub) && !is_dir($oldSub)) {
                    if (!@rename($liveSub, $oldSub)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-filesystem swap; WP_Filesystem::move() is copy+delete (non-atomic) and breaks crash/watchdog-resume safety
                        throw new \RuntimeException('FilesRestorer: cannot move live ' . esc_html($subdir) . ' aside: ' . esc_html($liveSub)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
                    }
                }

                // Safety: if a non-empty dir reappeared at $liveSub between
                // the move-aside and now, abort — we'd otherwise lose
                // whatever just appeared.
                if (is_dir($liveSub) && @rmdir($liveSub) === false) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- removes an empty server-derived scratch dir; WP_Filesystem not initialized
                    throw new \RuntimeException('FilesRestorer: live ' . esc_html($subdir) . ' reappeared and is not empty: ' . esc_html($liveSub)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
                }

                if (!@rename($stagingSub, $liveSub)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-filesystem swap; WP_Filesystem::move() is copy+delete (non-atomic) and breaks crash/watchdog-resume safety
                    // Best-effort rollback: put the old subdir back.
                    @rename($oldSub, $liveSub); // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-filesystem rollback swap; WP_Filesystem::move() is non-atomic
                    throw new \RuntimeException('FilesRestorer: cannot promote staging ' . esc_html($subdir) . ': ' . esc_html($stagingSub)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
                }

                $oldDirs[$comp] = $oldSub;
                self::safeProgress($progress, 'swap_files', [
                    'component'      => $comp,
                    'subdir'         => $subdir,
                    'old_subdir_dir' => $oldSub,
                ]);
                continue;
            }

            if ($comp === 'wp-content') {
                // Catch-all "others" component — swap each top-level entry in
                // staging that is NOT a managed subdir (plugins/themes/uploads).
                // We iterate the STAGING root because the snapshot defines
                // what "others" payload exists; anything in live that's not
                // in staging is preserved by design (no swap touches it).
                $managed = [];
                foreach (self::COMPONENT_SUBDIRS as $cfg) {
                    $managed[$cfg] = true;
                }
                $items = @scandir($stagingDir);
                if ($items === false) {
                    throw new \RuntimeException('FilesRestorer: cannot read staging dir for wp-content swap: ' . esc_html($stagingDir)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
                }
                $swappedAny = false;
                foreach ($items as $name) {
                    if ($name === '.' || $name === '..') {
                        continue;
                    }
                    if (isset($managed[$name])) {
                        // Reserved for the plugin/theme/upload components.
                        continue;
                    }
                    $stagingItem = $stagingDir . DIRECTORY_SEPARATOR . $name;
                    $liveItem    = $targetDir . DIRECTORY_SEPARATOR . $name;
                    $oldItem     = $targetDir . DIRECTORY_SEPARATOR . '.wpmgr-old-wpcontent-' . $short . '-' . $name;

                    if (file_exists($liveItem) && !file_exists($oldItem)) {
                        if (!@rename($liveItem, $oldItem)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-filesystem swap; WP_Filesystem::move() is copy+delete (non-atomic) and breaks crash/watchdog-resume safety
                            throw new \RuntimeException('FilesRestorer: cannot move live wp-content item aside: ' . esc_html($liveItem)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
                        }
                    }
                    if (file_exists($liveItem) && is_dir($liveItem) && @rmdir($liveItem) === false) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- removes an empty server-derived scratch dir; WP_Filesystem not initialized
                        throw new \RuntimeException('FilesRestorer: live wp-content item reappeared and is not empty: ' . esc_html($liveItem)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
                    }
                    if (!@rename($stagingItem, $liveItem)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-filesystem swap; WP_Filesystem::move() is copy+delete (non-atomic) and breaks crash/watchdog-resume safety
                        @rename($oldItem, $liveItem); // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-filesystem rollback swap; WP_Filesystem::move() is non-atomic
                        throw new \RuntimeException('FilesRestorer: cannot promote staging wp-content item: ' . esc_html($stagingItem)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
                    }
                    $swappedAny = true;
                }
                // Record a synthetic per-component old-dir entry: there are
                // many of them (one per top-level item) but the caller only
                // needs to know "the wp-content component was swapped". We
                // surface the staging dir itself as a marker — the actual
                // rollback paths follow the `.wpmgr-old-wpcontent-<short>-*`
                // glob.
                $oldDirs[$comp] = $targetDir . DIRECTORY_SEPARATOR . '.wpmgr-old-wpcontent-' . $short;
                self::safeProgress($progress, 'swap_files', [
                    'component'  => $comp,
                    'swapped_any' => $swappedAny,
                ]);
                continue;
            }

            throw new \RuntimeException('FilesRestorer: unknown swap component: ' . esc_html($comp)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to log/SSE, not browser
        }

        // Caller is responsible for rrmdir'ing the staging dir after we're
        // done; staging may still contain the subdirs for components that
        // weren't swapped this round (e.g. when only 'plugin' was selected,
        // the themes/uploads/everything-else trees are dead weight in
        // staging). We return its path so the caller can clean it up.
        self::safeProgress($progress, 'swap_files', [
            'done'        => true,
            'components'  => $components,
            'old_dirs'    => $oldDirs,
            'staging_dir' => $stagingDir,
        ]);

        return [
            'old_dirs'    => $oldDirs,
            'staging_dir' => $stagingDir,
        ];
    }

    /**
     * Garbage-collect `.wpmgr-old-files-*` dirs older than the GC age.
     * Bound to the `wpmgr_restore_oldfiles_gc` cron action.
     *
     * Threshold note: under the 0.9.5 default policy the synchronous cleanup
     * path in `RestoreRunner::runCleanup()` removes the rollback tree at the
     * end of the restore, so this cron sweep is only relevant for the opt-in
     * `keep_old_files=true` path (or for leftovers from a crashed run that
     * never reached cleanup). We therefore sweep against the LONG threshold —
     * the operator who opted in explicitly asked for a 24h rollback window,
     * and we don't want to GC their rollback tree out from under them at the
     * 1h SHORT threshold.
     *
     * @return void
     */
    public static function gcOldFiles(): void
    {
        if (!defined('WP_CONTENT_DIR')) {
            return;
        }
        // The old-files dirs live as siblings of the directory we replaced.
        // We sweep: ABSPATH (the canonical WP root, works even on relocated
        // wp-content installs), dirname(WP_CONTENT_DIR) (equals ABSPATH on
        // standard installs; differs when wp-content is relocated), and
        // WP_CONTENT_DIR itself (for any dirs staged one level inside wp-content).
        $candidates = array_unique([
            defined('ABSPATH') ? rtrim((string) constant('ABSPATH'), '/\\') : '',
            dirname(WP_CONTENT_DIR),
            WP_CONTENT_DIR,
        ]);
        $now       = time();
        $threshold = self::OLDFILES_GC_AGE_SECONDS_LONG;
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
                if ($mtime === false || ($now - (int) $mtime) < $threshold) {
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
                if ($mtime === false || ($now - (int) $mtime) < $threshold) {
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
     * Whether an entry name should be skipped at extract time. GH #147:
     * previously this used an UNANCHORED substring match
     * (`strpos($entryName, $sub) !== false`), so `db.php` matched inside
     * `.../class-db.php`, silently dropping unrelated plugin files at
     * extract time — before extraction, so the loss was never even logged as
     * a skip-with-reason. Every check below is now anchored (exact-path,
     * exact-segment, or anchored-prefix — never a bare substring), mirroring
     * the already-correct discipline in `FilesArchiver::isExcluded()`.
     *
     * Combines three anchored filters, most-specific first:
     *
     *   1. EXCLUDE_EXACT_PATHS — the normalized entry name must equal the
     *      excluded value EXACTLY (i.e. the entry sits at the top of the
     *      archive with no directory prefix). Catches wp-content-root
     *      drop-ins/config without over-matching same-named nested files.
     *   2. EXCLUDE_SEGMENTS — one `/`-separated segment of the normalized
     *      entry name must equal the excluded value exactly, at any depth.
     *      Reserved for WPMgr's own uniquely-named scratch/secret assets,
     *      which never legitimately collide with third-party file names.
     *   3. ENTRY_DENYLIST_PREFIXES — anchored-prefix match against the
     *      normalized name. Belt-and-suspenders on top of (2) for the
     *      specific top-level locations the wpmgr-agent tree and scratch
     *      dirs are known to live at.
     *
     * Backslashes are normalized to `/` first (and any leading `/` is
     * stripped) so a zip packed on Windows (entries like
     * `plugins\wpmgr-agent\foo.php`) is still matched by all three checks.
     */
    private static function isExcluded(string $entryName): bool
    {
        $normalized = str_replace('\\', '/', $entryName);
        $normalized = ltrim($normalized, '/');
        if ($normalized === '') {
            return false;
        }

        // 1) Exact top-level path match.
        if (in_array($normalized, self::EXCLUDE_EXACT_PATHS, true)) {
            return true;
        }

        // 2) Exact segment match at any depth.
        $segments = explode('/', $normalized);
        foreach ($segments as $segment) {
            if ($segment !== '' && in_array($segment, self::EXCLUDE_SEGMENTS, true)) {
                return true;
            }
        }

        // 3) Anchored-prefix denylist.
        foreach (self::ENTRY_DENYLIST_PREFIXES as $prefix) {
            if (strncmp($normalized, $prefix, strlen($prefix)) === 0) {
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
     * Track 5 — preserve-from-live for the per-component swap path. Same
     * semantics as preserveFromLive() but only copies allowlist entries whose
     * top-level segment is in scope for one of the components being swapped.
     *
     * Scope rules:
     *   - 'plugin'     swap   ->  pulls plugins/* allowlist entries.
     *   - 'theme'      swap   ->  pulls themes/* allowlist entries (none today).
     *   - 'upload'     swap   ->  pulls uploads/* allowlist entries (none today).
     *   - 'wp-content' swap   ->  pulls wp-content-root allowlist entries
     *                              (db.php, object-cache.php, etc.).
     *
     * @param string       $liveRoot    Absolute live wp-content path.
     * @param string       $stagingRoot Absolute staging dir.
     * @param list<string> $components  Component keys being swapped.
     */
    private static function preserveFromLiveScoped(string $liveRoot, string $stagingRoot, array $components): void
    {
        $liveRoot    = rtrim($liveRoot, DIRECTORY_SEPARATOR);
        $stagingRoot = rtrim($stagingRoot, DIRECTORY_SEPARATOR);
        if ($liveRoot === '' || $stagingRoot === '' || !is_dir($liveRoot) || !is_dir($stagingRoot)) {
            return;
        }
        if ($components === []) {
            return;
        }

        $compSet = [];
        foreach ($components as $c) {
            $compSet[$c] = true;
        }

        foreach (self::PRESERVE_FROM_LIVE as $rel) {
            $rel = ltrim($rel, '/');
            if ($rel === '') {
                continue;
            }
            // Map the allowlist entry to the component(s) responsible for it.
            // First segment of the rel path determines the bucket:
            //   plugins/*  -> 'plugin'
            //   themes/*   -> 'theme'
            //   uploads/*  -> 'upload'
            //   (any other top-level item) -> 'wp-content'
            $firstSeg = explode('/', $rel)[0] ?? '';
            switch ($firstSeg) {
                case 'plugins':
                    $bucket = 'plugin';
                    break;
                case 'themes':
                    $bucket = 'theme';
                    break;
                case 'uploads':
                    $bucket = 'upload';
                    break;
                default:
                    $bucket = 'wp-content';
            }
            if (!isset($compSet[$bucket])) {
                continue;
            }

            $src = $liveRoot . DIRECTORY_SEPARATOR . $rel;
            $dst = $stagingRoot . DIRECTORY_SEPARATOR . $rel;

            if (!file_exists($src)) {
                continue;
            }
            if (file_exists($dst)) {
                continue;
            }

            $dstParent = dirname($dst);
            if (!is_dir($dstParent) && !wp_mkdir_p($dstParent) && !is_dir($dstParent)) {
                \WPMgr\Agent\Support\DebugLog::write('WPMgr FilesRestorer: preserveFromLiveScoped cannot create parent: ' . $dstParent);
                continue;
            }

            try {
                if (is_dir($src) && !is_link($src)) {
                    self::copyRecursive($src, $dst);
                } else {
                    @copy($src, $dst);
                }
            } catch (\Throwable $e) {
                \WPMgr\Agent\Support\DebugLog::write('WPMgr FilesRestorer: preserveFromLiveScoped failed for ' . $rel . ': ' . $e->getMessage());
            }
        }
    }

    /**
     * Copy PRESERVE_FROM_LIVE entries from the live tree into staging if (and
     * only if) staging is missing them. Recursive copy for directories,
     * straight file copy for individual files. Best-effort: a failure on any
     * single entry is logged via error_log but does NOT abort the restore —
     * the operator's manual rollback path (`.wpmgr-old-files-<id>/`) is still
     * intact, and aborting the swap because we couldn't preserve a drop-in is
     * worse than completing the swap without it.
     *
     * @param string $liveRoot    Absolute path of the live wp-content dir.
     * @param string $stagingRoot Absolute path of the per-restore staging dir.
     */
    private static function preserveFromLive(string $liveRoot, string $stagingRoot): void
    {
        $liveRoot    = rtrim($liveRoot, DIRECTORY_SEPARATOR);
        $stagingRoot = rtrim($stagingRoot, DIRECTORY_SEPARATOR);
        if ($liveRoot === '' || $stagingRoot === '' || !is_dir($liveRoot) || !is_dir($stagingRoot)) {
            return;
        }

        foreach (self::PRESERVE_FROM_LIVE as $rel) {
            $rel = ltrim($rel, '/');
            if ($rel === '') {
                continue;
            }
            $src = $liveRoot . DIRECTORY_SEPARATOR . $rel;
            $dst = $stagingRoot . DIRECTORY_SEPARATOR . $rel;

            // Live doesn't have it — nothing to preserve.
            if (!file_exists($src)) {
                continue;
            }
            // Staging already has it (because the backup did contain it AND
            // it survived the exclude/denylist pass). Trust staging — the
            // operator explicitly opted into a restore of these paths.
            if (file_exists($dst)) {
                continue;
            }

            // Ensure the destination parent exists.
            $dstParent = dirname($dst);
            if (!is_dir($dstParent) && !wp_mkdir_p($dstParent) && !is_dir($dstParent)) {
                \WPMgr\Agent\Support\DebugLog::write('WPMgr FilesRestorer: preserveFromLive cannot create parent: ' . $dstParent);
                continue;
            }

            try {
                if (is_dir($src) && !is_link($src)) {
                    self::copyRecursive($src, $dst);
                } else {
                    @copy($src, $dst);
                }
            } catch (\Throwable $e) {
                \WPMgr\Agent\Support\DebugLog::write('WPMgr FilesRestorer: preserveFromLive failed for ' . $rel . ': ' . $e->getMessage());
            }
        }
    }

    /**
     * Recursive copy used by `preserveFromLive`. Skips symlinks (we never
     * follow a symlink out of the live tree into wherever it points) and
     * silently swallows individual `copy()` failures — the caller logs the
     * outer entry failure.
     */
    private static function copyRecursive(string $src, string $dst): void
    {
        if (!is_dir($src)) {
            return;
        }
        if (!is_dir($dst) && !wp_mkdir_p($dst) && !is_dir($dst)) {
            return;
        }
        $items = @scandir($src);
        if ($items === false) {
            return;
        }
        foreach ($items as $i) {
            if ($i === '.' || $i === '..') {
                continue;
            }
            $sp = $src . DIRECTORY_SEPARATOR . $i;
            $dp = $dst . DIRECTORY_SEPARATOR . $i;
            if (is_link($sp)) {
                // Don't follow symlinks — preserving a symlink target outside
                // the live tree would be a footgun.
                continue;
            }
            if (is_dir($sp)) {
                self::copyRecursive($sp, $dp);
            } elseif (is_file($sp)) {
                @copy($sp, $dp);
            }
        }
    }

    /**
     * Estimate the on-disk size of wp-content. Used by `RestoreRunner`'s
     * two-leg disk-free precheck to size the "we need room for a staging tree
     * the same size as live wp-content" leg. Caps at 5 wall-clock seconds —
     * on a giant wp-content (millions of files) a full walk would itself eat
     * the cron budget. On a cap-hit OR an iterator failure we fall back to
     * `disk_total_space - disk_free_space`, which over-estimates (counts the
     * entire volume, not just wp-content) but is safer than under-estimating.
     *
     * @param string $wpContent Absolute wp-content path.
     * @return int Estimated bytes, or 0 if neither path nor fallback is usable.
     */
    public static function estimateWpContentBytes(string $wpContent): int
    {
        if ($wpContent === '' || !is_dir($wpContent)) {
            return 0;
        }
        $deadline = microtime(true) + 5.0;
        $total    = 0;

        try {
            // SKIP_DOTS avoids `.` and `..`. We deliberately don't follow
            // symlinks — a symlink loop in wp-content (rare but real on some
            // hosts) would otherwise burn the whole 5s budget on nothing.
            $it = new \RecursiveIteratorIterator(
                new \RecursiveDirectoryIterator(
                    $wpContent,
                    \FilesystemIterator::SKIP_DOTS
                ),
                \RecursiveIteratorIterator::LEAVES_ONLY,
                \RecursiveIteratorIterator::CATCH_GET_CHILD
            );
            foreach ($it as $file) {
                if (microtime(true) > $deadline) {
                    // Cap hit — fall through to the disk-based fallback.
                    return self::diskUsageFallback($wpContent);
                }
                /** @var \SplFileInfo $file */
                if ($file->isFile()) {
                    $total += (int) $file->getSize();
                }
            }
        } catch (\Throwable $_) {
            return self::diskUsageFallback($wpContent);
        }
        return $total;
    }

    /**
     * Fallback for `estimateWpContentBytes` when the walk caps or errors —
     * report the volume's used bytes (`total - free`). This OVER-estimates
     * (counts the whole volume, not just wp-content) which means a noisy
     * shared host will trip the precheck before a small wp-content really
     * needed it to — but a false positive ("not enough disk, abort") is far
     * safer than a false negative ("plenty of disk, proceed").
     */
    private static function diskUsageFallback(string $path): int
    {
        $total = @disk_total_space($path);
        $free  = @disk_free_space($path);
        if (!is_numeric($total) || !is_numeric($free) || $total <= 0) {
            return 0;
        }
        $used = (int) $total - (int) $free;
        return $used > 0 ? $used : 0;
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
                wp_delete_file($p);
            } elseif (is_dir($p)) {
                self::rrmdir($p);
            }
        }
        @rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- removes an empty server-derived scratch dir; WP_Filesystem not initialized
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
