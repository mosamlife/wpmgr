<?php
/**
 * MediaQuarantine: manages the uploads/wpmgr-quarantine/ directory (legacy:
 * wp-content/wpmgr-quarantine/) where isolated (unused) attachment files are
 * temporarily stored.
 *
 * Base path resolution is uploads-first (wp.org Guideline compliance, see
 * {@see \WPMgr\Agent\Support\StoragePaths::dataBase()}): the resolved root is
 * normally wp_upload_dir()['basedir'] . '/wpmgr-quarantine', falling back to
 * WP_CONTENT_DIR . '/wpmgr-quarantine' only when the uploads directory is
 * unavailable. Existing self-hosted installs that already have data under the
 * legacy wp-content location keep working via {@see $legacyRoot} read-fallback
 * and a one-time rename in {@see ensureQuarantineRoot()}.
 *
 * Layout (relative to the resolved root, described above):
 *   wpmgr-quarantine/
 *     .htaccess              — deny all web access (Apache/LiteSpeed)
 *     index.php              — empty PHP guard
 *     manifests/             — manifest JSONs; NEVER web-reachable (outside media/)
 *       <manifest_id>.json   — reversible quarantine manifest
 *     media/
 *       <manifest_id>/       — 128-bit random hex directory
 *         files/             — the moved attachment files (preserving sub-path)
 *                              (only image files here; no path-disclosing JSON)
 *
 * Nginx-safety: manifest.json (which contains absolute server paths) is stored
 * in the manifests/ sub-directory alongside — not inside — the media/ tree.
 * The media/ sub-tree contains ONLY the quarantined image files (already public
 * at their uploads URL). The unguessable 128-bit random manifest_id directory
 * name is a defence-in-depth layer; the primary guard is that path-disclosing
 * manifests are outside the served subtree entirely.
 *
 * manifest.json schema:
 *   {
 *     "v":      1,
 *     "id":     "<manifest_id>",
 *     "job_id": "<job_id>",
 *     "ts":     <unix timestamp>,
 *     "entries": [
 *       {
 *         "attachment_id": <int>,
 *         "rel_path":      "<uploads-relative main file path>",
 *         "files": [
 *           {
 *             "orig": "<unresolved-absolute-path>",  // restore destination
 *             "frag": "<resolved-uploads-relative-fragment>"  // quarantine location key
 *           }, ...
 *         ]
 *       }, ...
 *     ]
 *   }
 *
 * Schema note: "files" entries are {"orig","frag"} objects (v1 since agent 0.25.9).
 * Older manifests stored plain strings; restore/delete fall back gracefully.
 *
 * Security:
 *   - The resolved quarantine root (uploads/wpmgr-quarantine, or the legacy
 *     wp-content/wpmgr-quarantine fallback) is a directory no attachment URL
 *     ever points into. Because the uploads-first root sits inside the
 *     web-served uploads/ tree, direct web access is blocked explicitly by
 *     the .htaccess + index.php guard files ensureQuarantineRoot() writes on
 *     first use (Deny all / Require all denied) rather than by directory
 *     placement alone.
 *   - manifest_id values are 128-bit random hex strings. Callers treat them as
 *     opaque; the class never accepts a manifest_id from untrusted input —
 *     callers receive IDs from beginManifest() or from the manifest list, and
 *     all file operations are contained via realpath checks to the quarantine root.
 *   - normalisePath() rejects any path containing "/.." as belt-and-braces
 *     against directory traversal in the stored original-path list.
 *
 * @package WPMgr\Agent\Media
 */

declare(strict_types=1);

namespace WPMgr\Agent\Media;

use WPMgr\Agent\Support\StoragePaths;

/**
 * Manages the quarantine directory lifecycle.
 */
final class MediaQuarantine
{
    /** Directory name under wp-content (legacy) / uploads (new). */
    public const QUARANTINE_DIR = 'wpmgr-quarantine';

    /** Sub-directory under QUARANTINE_DIR for quarantined image files. */
    public const MEDIA_SUBDIR = 'media';

    /**
     * Sub-directory under QUARANTINE_DIR for manifest JSON files.
     * Kept OUTSIDE the media/ tree so path-disclosing JSON is not
     * web-reachable on nginx/Caddy/LiteSpeed (which ignore .htaccess).
     */
    public const MANIFESTS_SUBDIR = 'manifests';

    /** Manifest schema version. */
    private const MANIFEST_VERSION = 1;

    /** Absolute path to the quarantine root (uploads/wpmgr-quarantine; legacy: wp-content/wpmgr-quarantine). */
    private string $quarantineRoot;

    /** Absolute path to the media quarantine dir (wpmgr-quarantine/media). */
    private string $mediaRoot;

    /** Absolute path to the manifests dir (wpmgr-quarantine/manifests). */
    private string $manifestsRoot;

    /**
     * Legacy quarantine root (wp-content/wpmgr-quarantine) kept for read-fallback
     * so existing self-hosted installs are not orphaned after the directory is
     * relocated under uploads/. Only set when it differs from quarantineRoot.
     */
    private string $legacyRoot = '';

    /** In-progress manifest data keyed by manifest_id. */
    private array $openManifests = [];

    /**
     * Whether the constructor resolved a safe wp-content base path.
     * False means no write operations may proceed; the instance is read-only.
     */
    private bool $basePathResolved = false;

    /**
     * @param string|null $contentDir Override for the quarantine base path (testing only).
     *                                Pass null (default) to use the runtime wp_upload_dir() / WP_CONTENT_DIR.
     */
    public function __construct(?string $contentDir = null)
    {
        // Base-path resolution order:
        //   1. Explicit $contentDir passed by the caller (test override or known path).
        //   2. StoragePaths::dataBase('quarantine') — uploads-first; honors relocatable
        //      upload_path + multisite per-site subdirs (wp.org Guideline compliance).
        //      Internally falls back to WP_CONTENT_DIR/wpmgr-quarantine when
        //      wp_upload_dir() is unavailable (CLI/headless contexts) — see
        //      StoragePaths::dataBase() for that fallback's own definition.
        //   3. Unresolved — instance is marked unavailable; writes are blocked before
        //      any mkdir call so nothing is ever created at the filesystem root. In
        //      a real WordPress boot this is unreachable (WP core always defines
        //      WP_CONTENT_DIR very early, well before any plugin loads), so there is
        //      no separate ABSPATH-guessing fallback here — StoragePaths::dataBase()
        //      already covers every path a normal WP request can take.
        if ($contentDir === '') {
            // Explicit empty string is the "unavailable" signal used by tests and by
            // callers that already know no base could be resolved. Treat as unresolved
            // immediately — do NOT fall through to StoragePaths/wp_upload_dir, which
            // might return a real path and mask the guard.
            $base                   = '';
            $this->basePathResolved = false;
        } elseif (is_string($contentDir)) {
            // Non-empty explicit override: $contentDir is a wp-content surrogate; append
            // QUARANTINE_DIR exactly as the old runtime path did with WP_CONTENT_DIR.
            $base                   = rtrim($contentDir, '/\\') . '/' . self::QUARANTINE_DIR;
            $this->basePathResolved = true;
        } else {
            // Runtime path: uploads-first (wp.org Guideline compliance).
            $resolved = StoragePaths::dataBase('quarantine');
            if ($resolved !== '') {
                $base                   = $resolved;
                $this->basePathResolved = true;

                // Record the legacy wp-content location so reads can fall back to
                // it — existing self-hosted installs keep working with no migration.
                $legacy = StoragePaths::legacyBase('quarantine');
                if ($legacy !== '' && $legacy !== $base) {
                    $this->legacyRoot = $legacy;
                }
            } else {
                // No safe base available. Set the *Root properties to non-empty strings
                // so that the readonly scandir callers (quarantinedAttachmentIds,
                // listManifests, listManifestsDetailed) simply return empty arrays on a
                // non-existent path — their existing @scandir()/is_dir() guards handle this.
                // ensureQuarantineRoot() will throw before any mkdir when $basePathResolved
                // is false, so these paths are never written to.
                $base                   = '';
                $this->basePathResolved = false;
            }
        }

        $this->quarantineRoot  = $base !== '' ? $base : '/__wpmgr-quarantine-unresolved__';
        $this->mediaRoot       = $this->quarantineRoot . '/' . self::MEDIA_SUBDIR;
        $this->manifestsRoot   = $this->quarantineRoot . '/' . self::MANIFESTS_SUBDIR;
    }

    // =========================================================================
    // Public API
    // =========================================================================

    /**
     * Create a fresh manifest and return its ID. Call quarantineAttachment()
     * for each attachment, then finaliseManifest() to flush the JSON.
     *
     * @param string $jobId CP-side job UUID (stored for audit).
     * @return string manifest_id (128-bit random hex string)
     */
    public function beginManifest(string $jobId): string
    {
        $this->ensureQuarantineRoot();

        $manifestId = $this->generateId();

        $this->openManifests[$manifestId] = [
            'v'       => self::MANIFEST_VERSION,
            'id'      => $manifestId,
            'job_id'  => $jobId,
            'ts'      => time(),
            'entries' => [],
        ];

        // Create the media files directory only (no manifest.json here).
        // manifest.json is written to manifests/<id>.json (outside media/).
        $filesDir = $this->mediaRoot . '/' . $manifestId . '/files';
        if (!is_dir($filesDir)) {
            wp_mkdir_p($filesDir);
        }

        return $manifestId;
    }

    /**
     * Move the given attachment's files into the quarantine directory and append
     * an entry to the open manifest.
     *
     * @param string   $manifestId    ID returned by beginManifest().
     * @param int      $attachmentId  Attachment post ID.
     * @param string   $relPath       Upload-relative main file path.
     * @param string[] $absPaths      Absolute paths of ALL files to move (main + sizes).
     * @return int Number of files successfully moved.
     */
    public function quarantineAttachment(
        string $manifestId,
        int $attachmentId,
        string $relPath,
        array $absPaths
    ): int {
        if (!isset($this->openManifests[$manifestId])) {
            return 0;
        }

        $filesRoot   = $this->mediaRoot . '/' . $manifestId . '/files';
        $uploadsBase = $this->uploadsBase();
        $movedPaths  = [];
        $moved       = 0;

        foreach ($absPaths as $src) {
            // Containment: only move files that are inside the uploads dir.
            $real = realpath($src);
            if ($real === false) {
                continue;
            }
            $uploadsReal = realpath($uploadsBase);
            if ($uploadsReal === false || strpos($real, $uploadsReal . DIRECTORY_SEPARATOR) !== 0) {
                continue;
            }

            // Derive the relative path within uploads to preserve sub-structure.
            $fragment = ltrim(str_replace($uploadsReal, '', $real), '/\\');
            $dest     = $filesRoot . '/' . $fragment;

            // Ensure destination directory exists.
            $destDir = dirname($dest);
            if (!is_dir($destDir)) {
                wp_mkdir_p($destDir);
            }

            if (@rename($src, $dest)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-filesystem swap; WP_Filesystem::move() is copy+delete (non-atomic) and breaks crash-resume safety
                // Store both the unresolved original path (restore destination) and
                // the resolved fragment (quarantine location). The fragment is derived
                // from the realpath-resolved source, so it always matches the actual
                // placement under filesRoot/ regardless of whether the uploads base or
                // any path component is reached via a symlink.
                $movedPaths[] = ['orig' => $src, 'frag' => $fragment];
                $moved++;
            }
        }

        // Record the manifest entry for every attachment, even when no files were
        // physically moved (e.g. already-broken attachment, files already absent,
        // or an optimized attachment whose on-disk paths were not found). An empty
        // "files" array is valid: restore has nothing to move back, and delete will
        // still call wp_delete_attachment() to remove the WP post record.
        $this->openManifests[$manifestId]['entries'][] = [
            'attachment_id' => $attachmentId,
            'rel_path'      => $relPath,
            'files'         => $movedPaths,
        ];

        return $moved;
    }

    /**
     * Write the manifest JSON to disk and close the in-progress manifest.
     *
     * The manifest is written to manifests/<manifest_id>.json — outside the
     * media/ directory tree — so that path-disclosing JSON is not web-reachable
     * on nginx/Caddy/LiteSpeed sites (which do not honour .htaccess).
     *
     * @param string $manifestId
     * @return void
     */
    public function finaliseManifest(string $manifestId): void
    {
        if (!isset($this->openManifests[$manifestId])) {
            return;
        }

        $data         = $this->openManifests[$manifestId];
        // Write to manifests/ (outside media/), NOT inside the served media tree.
        $manifestPath = $this->manifestsRoot . '/' . $manifestId . '.json';

        $json = wp_json_encode($data, JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES);
        if ($json !== false) {
            file_put_contents($manifestPath, $json, LOCK_EX);
        }

        unset($this->openManifests[$manifestId]);
    }

    /**
     * Move all files in a manifest back to their original paths.
     *
     * @param string $manifestId
     * @return int Number of files successfully restored.
     */
    public function restoreManifest(string $manifestId): int
    {
        $manifest = $this->loadManifest($manifestId);
        if ($manifest === null) {
            return 0;
        }

        $filesRoot   = $this->mediaRoot . '/' . $manifestId . '/files';
        $uploadsBase = $this->uploadsBase();
        $restored    = 0;

        foreach ($manifest['entries'] as $entry) {
            $files = is_array($entry['files']) ? $entry['files'] : [];

            foreach ($files as $fileRecord) {
                // Support both the current {"orig","frag"} object format and the
                // legacy plain-string format written by agent versions < 0.25.9.
                if (is_array($fileRecord)) {
                    $originalAbsPath = (string)($fileRecord['orig'] ?? '');
                    $fragment        = (string)($fileRecord['frag'] ?? '');
                } else {
                    $originalAbsPath = (string)$fileRecord;
                    $fragment        = '';
                }

                // Containment: restore target must be inside uploads.
                $normalised = $this->normalisePath($originalAbsPath, $uploadsBase);
                if ($normalised === '') {
                    continue;
                }

                // Resolve the quarantined-file location.
                // For current manifests the fragment was recorded at quarantine time from
                // the realpath-resolved source, so it matches the actual placement even
                // when the uploads base or any path component is a symlink.
                // For legacy manifests (plain string entries) we fall back to re-deriving
                // the fragment from the stored unresolved path.
                if ($fragment === '') {
                    $uploadsReal = realpath($uploadsBase);
                    if ($uploadsReal === false) {
                        continue;
                    }
                    $uploadsBaseNorm = rtrim(str_replace('\\', '/', $uploadsBase), '/');
                    $normalisedFwd   = str_replace('\\', '/', $normalised);
                    if (strpos($normalisedFwd, $uploadsBaseNorm . '/') === 0) {
                        $fragment = substr($normalisedFwd, strlen($uploadsBaseNorm) + 1);
                    } else {
                        $fragment = ltrim(str_replace($uploadsReal, '', $normalisedFwd), '/');
                    }
                }

                $src = $filesRoot . '/' . $fragment;

                if (!file_exists($src)) {
                    continue;
                }

                // Ensure destination directory exists.
                $destDir = dirname($normalised);
                if (!is_dir($destDir)) {
                    wp_mkdir_p($destDir);
                }

                if (@rename($src, $normalised)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename,WordPress.WP.AlternativeFunctions.file_system_operations_rename,PluginCheck.CodeAnalysis.WriteFile.ABSPATHDetected -- restore/quarantine engine intentionally writes under ABSPATH (the live WP tree); relocating would defeat the restore
                    $restored++;
                }
            }
        }

        // Remove the (now-empty) manifest directory if everything was restored.
        if ($restored > 0) {
            $this->removeManifestDir($manifestId);
        }

        return $restored;
    }

    /**
     * Permanently delete all quarantined files for a manifest and delete the
     * attachment posts via wp_delete_attachment().
     *
     * Returns a detailed result array so callers can surface per-attachment
     * outcomes and build instrumented ACK payloads:
     *
     *   [
     *     'posts_deleted'      => int,  // wp_delete_attachment returned truthy
     *     'posts_failed'       => int,  // attachment_id>0 but wp_delete_attachment returned false/null
     *     'files_deleted'      => int,  // quarantined files successfully unlinked
     *     'entries_processed'  => int,  // total manifest entries seen
     *     'results'            => list<array{attachment_id:int,post_deleted:bool,files_deleted:int}>,
     *   ]
     *
     * wp_delete_attachment($id, true) also removes any WP-tracked sub-sizes that
     * were not in quarantine (e.g. if isolate missed some) — this is the correct
     * cleanup behaviour.
     *
     * @param string $manifestId
     * @return array{posts_deleted:int,posts_failed:int,files_deleted:int,entries_processed:int,results:list<array{attachment_id:int,post_deleted:bool,files_deleted:int}>}
     */
    public function deleteManifest(string $manifestId): array
    {
        $manifest = $this->loadManifest($manifestId);
        if ($manifest === null) {
            return [
                'posts_deleted'     => 0,
                'posts_failed'      => 0,
                'files_deleted'     => 0,
                'entries_processed' => 0,
                'results'           => [],
            ];
        }

        $filesRoot   = $this->mediaRoot . '/' . $manifestId . '/files';
        $uploadsBase = $this->uploadsBase();

        $postsDeleted     = 0;
        $postsFailed      = 0;
        $totalFilesDeleted = 0;
        $results          = [];

        foreach ($manifest['entries'] as $entry) {
            $attachmentId    = (int)($entry['attachment_id'] ?? 0);
            $files           = is_array($entry['files']) ? $entry['files'] : [];
            $entryFilesCount = 0;

            // Delete the quarantined files for this entry.
            foreach ($files as $fileRecord) {
                // Support both the current {"orig","frag"} object format and the
                // legacy plain-string format written by agent versions < 0.25.9.
                if (is_array($fileRecord)) {
                    $originalAbsPath = (string)($fileRecord['orig'] ?? '');
                    $fragment        = (string)($fileRecord['frag'] ?? '');
                } else {
                    $originalAbsPath = (string)$fileRecord;
                    $fragment        = '';
                }

                if ($fragment === '') {
                    // Legacy path: re-derive the fragment from the stored unresolved path.
                    $uploadsReal = realpath($uploadsBase);
                    if ($uploadsReal === false) {
                        continue;
                    }
                    $uploadsBaseNorm = rtrim(str_replace('\\', '/', $uploadsBase), '/');
                    $origFwd         = str_replace('\\', '/', $originalAbsPath);
                    if (strpos($origFwd, $uploadsBaseNorm . '/') === 0) {
                        $fragment = substr($origFwd, strlen($uploadsBaseNorm) + 1);
                    } else {
                        $fragment = ltrim(str_replace($uploadsReal, '', $origFwd), '/');
                    }
                }

                $quarantinedPath = $filesRoot . '/' . $fragment;
                if ($quarantinedPath !== $filesRoot . '/' && file_exists($quarantinedPath)) {
                    wp_delete_file($quarantinedPath);
                    $entryFilesCount++;
                }
            }

            $totalFilesDeleted += $entryFilesCount;

            // Delete the attachment post + its remaining WP-tracked sub-sizes.
            // This runs for every entry with a valid attachment_id, regardless of
            // whether any quarantined files existed (handles broken/zero-file entries).
            $postDeleted = false;
            if ($attachmentId > 0) {
                $wpResult    = wp_delete_attachment($attachmentId, true);
                $postDeleted = ($wpResult !== false && $wpResult !== null);
                if ($postDeleted) {
                    $postsDeleted++;
                } else {
                    $postsFailed++;
                }
            }

            $results[] = [
                'attachment_id' => $attachmentId,
                'post_deleted'  => $postDeleted,
                'files_deleted' => $entryFilesCount,
            ];
        }

        $this->removeManifestDir($manifestId);

        return [
            'posts_deleted'     => $postsDeleted,
            'posts_failed'      => $postsFailed,
            'files_deleted'     => $totalFilesDeleted,
            'entries_processed' => count($results),
            'results'           => $results,
        ];
    }

    /**
     * Return a set of all attachment IDs that are currently recorded in at
     * least one quarantine manifest on disk. The returned array uses attachment
     * ID as key with true as value, giving O(1) membership tests.
     *
     * Used by the scan action to exclude already-quarantined attachments from
     * the candidate list so they do not resurface after a prior isolation run.
     *
     * A missing or unreadable manifests directory returns an empty array —
     * no exclusions occur in that case, which preserves the original scan
     * behaviour when no quarantine has ever been created.
     *
     * Corrupt or unreadable individual manifest files are silently skipped so
     * a single bad file cannot break a full-library scan.
     *
     * @return array<int,true>
     */
    public function quarantinedAttachmentIds(): array
    {
        $mRoot = $this->effectiveManifestsRoot();
        if (!is_dir($mRoot)) {
            return [];
        }

        $dirEntries = @scandir($mRoot);
        if ($dirEntries === false) {
            return [];
        }

        $ids = [];

        foreach ($dirEntries as $dirEntry) {
            if ($dirEntry === '.' || $dirEntry === '..') {
                continue;
            }
            if (!str_ends_with($dirEntry, '.json')) {
                continue;
            }

            $manifestId = substr($dirEntry, 0, -5);
            $manifest   = $this->loadManifest($manifestId);
            if ($manifest === null) {
                continue;
            }

            $entries = is_array($manifest['entries'] ?? null) ? $manifest['entries'] : [];
            foreach ($entries as $entry) {
                $attachmentId = (int)($entry['attachment_id'] ?? 0);
                if ($attachmentId > 0) {
                    $ids[$attachmentId] = true;
                }
            }
        }

        return $ids;
    }

    /**
     * Return a list of all manifests in the quarantine manifests directory.
     *
     * @return list<array{id:string,ts:int,entries_count:int}>
     */
    public function listManifests(): array
    {
        $mRoot = $this->effectiveManifestsRoot();
        if (!is_dir($mRoot)) {
            return [];
        }

        $manifests = [];
        $entries   = @scandir($mRoot);
        if ($entries === false) {
            return [];
        }

        foreach ($entries as $entry) {
            if ($entry === '.' || $entry === '..') {
                continue;
            }
            // Entry is "<manifest_id>.json"; strip the .json suffix to get the ID.
            if (!str_ends_with($entry, '.json')) {
                continue;
            }
            $manifestId = substr($entry, 0, -5);
            $manifest   = $this->loadManifest($manifestId);
            if ($manifest !== null) {
                $manifests[] = [
                    'id'            => (string)($manifest['id']  ?? $manifestId),
                    'job_id'        => (string)($manifest['job_id'] ?? ''),
                    'ts'            => (int)($manifest['ts'] ?? 0),
                    'entries_count' => count((array)($manifest['entries'] ?? [])),
                ];
            }
        }

        usort($manifests, fn ($a, $b) => $b['ts'] - $a['ts']);

        return $manifests;
    }

    /**
     * Return a detailed list of all quarantine manifests, including per-entry
     * attachment titles and file counts. Sorted newest-first by isolated_at.
     *
     * Each manifest record in the returned array has:
     *   - manifest_id  (string)
     *   - job_id       (string)
     *   - isolated_at  (int, unix seconds — the manifest's "ts" field)
     *   - total_files  (int, sum of each entry's file count across the manifest)
     *   - entries      (list of {attachment_id, title, file_count})
     *
     * File-count is format-agnostic: both the current {"orig","frag"} object
     * format and the legacy plain-string format are counted the same way — we
     * simply count the elements of each entry's "files" array.
     *
     * The attachment title is resolved via get_the_title() when the function is
     * available (i.e. WordPress is loaded). If the attachment post no longer
     * exists, or if get_the_title() is not available, an empty string is used.
     *
     * @return list<array{manifest_id:string,job_id:string,isolated_at:int,total_files:int,entries:list<array{attachment_id:int,title:string,file_count:int}>}>
     */
    public function listManifestsDetailed(): array
    {
        $mRoot = $this->effectiveManifestsRoot();
        if (!is_dir($mRoot)) {
            return [];
        }

        $dirEntries = @scandir($mRoot);
        if ($dirEntries === false) {
            return [];
        }

        $result = [];

        foreach ($dirEntries as $dirEntry) {
            if ($dirEntry === '.' || $dirEntry === '..') {
                continue;
            }
            if (!str_ends_with($dirEntry, '.json')) {
                continue;
            }

            $manifestId = substr($dirEntry, 0, -5);
            $manifest   = $this->loadManifest($manifestId);
            if ($manifest === null) {
                continue;
            }

            $rawEntries = is_array($manifest['entries'] ?? null) ? $manifest['entries'] : [];
            $entries    = [];
            $totalFiles = 0;

            foreach ($rawEntries as $rawEntry) {
                $attachmentId = (int)($rawEntry['attachment_id'] ?? 0);

                // Count files format-agnostically: each element of the "files"
                // array is one file regardless of whether it is the current
                // {"orig","frag"} object or the legacy plain-string form.
                $files     = is_array($rawEntry['files'] ?? null) ? $rawEntry['files'] : [];
                $fileCount = count($files);
                $totalFiles += $fileCount;

                // Resolve the attachment title; guard against WP not being loaded.
                $title = '';
                if ($attachmentId > 0 && function_exists('get_the_title')) {
                    $fetched = get_the_title($attachmentId);
                    $title   = is_string($fetched) ? $fetched : '';
                }

                $entries[] = [
                    'attachment_id' => $attachmentId,
                    'title'         => $title,
                    'file_count'    => $fileCount,
                ];
            }

            $result[] = [
                'manifest_id' => (string)($manifest['id'] ?? $manifestId),
                'job_id'      => (string)($manifest['job_id'] ?? ''),
                'isolated_at' => (int)($manifest['ts'] ?? 0),
                'total_files' => $totalFiles,
                'entries'     => $entries,
            ];
        }

        // Sort newest-first by isolated_at.
        usort($result, fn ($a, $b) => $b['isolated_at'] - $a['isolated_at']);

        return $result;
    }

    // =========================================================================
    // Private helpers
    // =========================================================================

    /**
     * Ensure the quarantine root + .htaccess + index.php guards exist, and
     * create the manifests/ and media/ sub-directories.
     *
     * The .htaccess blocks Apache/LiteSpeed. On nginx/Caddy, the .htaccess is
     * ineffective; the primary protection for manifests is that they live in
     * the manifests/ directory (outside the media/ web tree), not inside a
     * directory that is ever served.
     *
     * @throws \RuntimeException When no safe wp-content base could be resolved at
     *   construction time. The guard here is the single write gateway — it blocks
     *   every code path that would otherwise mkdir or write files, preventing any
     *   directory from being created at the filesystem root ('/').
     */
    private function ensureQuarantineRoot(): void
    {
        if (!$this->basePathResolved) {
            throw new \RuntimeException(
                'WPMgr media quarantine unavailable: could not resolve a safe base path ' .
                '(uploads dir unavailable, WP_CONTENT_DIR undefined, and ABSPATH unavailable). ' .
                'Refusing to write at the filesystem root.'
            );
        }

        // One-time migration: if the legacy wp-content location has data and the
        // new uploads-based location does not yet exist, atomically rename the
        // legacy directory to the new location so existing quarantines are preserved.
        if ($this->legacyRoot !== ''
            && is_dir($this->legacyRoot)
            && !is_dir($this->quarantineRoot)
        ) {
            // Ensure the parent directory under uploads exists before renaming.
            $newParent = dirname($this->quarantineRoot);
            if (!is_dir($newParent)) {
                wp_mkdir_p($newParent);
            }
            // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic directory rename; WP_Filesystem::move() is non-atomic and would leave partial state on failure
            @rename($this->legacyRoot, $this->quarantineRoot);
        }

        if (!is_dir($this->quarantineRoot)) {
            wp_mkdir_p($this->quarantineRoot);
        }
        if (!is_dir($this->mediaRoot)) {
            wp_mkdir_p($this->mediaRoot);
        }
        // Manifests directory: outside media/, holds the path-disclosing JSON files.
        if (!is_dir($this->manifestsRoot)) {
            wp_mkdir_p($this->manifestsRoot);
        }

        // Web-access guard (.htaccess) — effective on Apache/LiteSpeed.
        $htaccess = $this->quarantineRoot . '/.htaccess';
        if (!file_exists($htaccess)) {
            file_put_contents($htaccess, "Order Deny,Allow\nDeny from all\n", LOCK_EX);
        }

        // PHP silence guard.
        $index = $this->quarantineRoot . '/index.php';
        if (!file_exists($index)) {
            file_put_contents($index, "<?php // Silence is golden.\n", LOCK_EX);
        }
    }

    /**
     * Return the manifests root to scan for read operations.
     *
     * Uses the primary (uploads-based) manifests root when it exists, otherwise
     * falls back to the legacy wp-content location so self-hosted installs that
     * have not yet been migrated continue to work without data loss.
     */
    private function effectiveManifestsRoot(): string
    {
        if (is_dir($this->manifestsRoot)) {
            return $this->manifestsRoot;
        }
        if ($this->legacyRoot !== '') {
            $legacyManifests = $this->legacyRoot . '/' . self::MANIFESTS_SUBDIR;
            if (is_dir($legacyManifests)) {
                return $legacyManifests;
            }
        }
        return $this->manifestsRoot; // return primary even if absent; callers check is_dir()
    }

    /**
     * Load and decode a manifest JSON file. Returns null on failure.
     *
     * Manifests are read from manifests/<manifest_id>.json (outside media/).
     *
     * @param string $manifestId
     * @return array|null
     */
    private function loadManifest(string $manifestId): ?array
    {
        // Validate manifest ID to prevent path traversal.
        if (!preg_match('/^[a-zA-Z0-9_-]{1,64}$/', $manifestId)) {
            return null;
        }

        // Try primary (uploads-based) manifests root, then fall back to legacy.
        $manifestsRoots = [$this->manifestsRoot];
        if ($this->legacyRoot !== '') {
            $manifestsRoots[] = $this->legacyRoot . '/' . self::MANIFESTS_SUBDIR;
        }

        foreach ($manifestsRoots as $mRoot) {
            $path = $mRoot . '/' . $manifestId . '.json';

            // Containment: the manifest file must be inside its manifests root.
            $real = realpath($path);
            if ($real === false) {
                continue;
            }
            $rootReal = realpath($mRoot);
            if ($rootReal === false || strpos($real, $rootReal . DIRECTORY_SEPARATOR) !== 0) {
                continue;
            }

            $json = @file_get_contents($real);
            if ($json === false || $json === '') {
                continue;
            }

            $data = json_decode($json, true);
            if (!is_array($data)) {
                continue;
            }

            return $data;
        }

        return null;
    }

    /**
     * Remove the media directory and the manifest JSON for a given manifest.
     * Cleans up both the media/<manifest_id>/ tree and manifests/<manifest_id>.json.
     */
    private function removeManifestDir(string $manifestId): void
    {
        if (!preg_match('/^[a-zA-Z0-9_-]{1,64}$/', $manifestId)) {
            return;
        }
        // Remove the media files directory.
        $dir = $this->mediaRoot . '/' . $manifestId;
        $this->rmdirRecursive($dir);

        // Remove the manifest JSON from manifests/.
        $jsonPath = $this->manifestsRoot . '/' . $manifestId . '.json';
        if (file_exists($jsonPath)) {
            wp_delete_file($jsonPath);
        }
    }

    private function rmdirRecursive(string $dir): void
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
            if (is_dir($path)) {
                $this->rmdirRecursive($path);
            } else {
                wp_delete_file($path);
            }
        }
        @rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- removes an empty server-derived quarantine scratch dir; WP_Filesystem not initialized
    }

    /**
     * Validate and return the absolute path, confirming it is inside the uploads
     * directory. Returns '' if the path fails containment.
     *
     * Belt-and-braces: also rejects any path containing "/.." (directory
     * traversal) before the prefix check, guarding against crafted entries in
     * a tampered manifest file.
     */
    private function normalisePath(string $absPath, string $uploadsBase): string
    {
        // We cannot realpath() a path that doesn't exist (file was moved to
        // quarantine). Instead, normalise manually and check the prefix.
        $normalised = str_replace(['\\', '//'], ['/', '/'], $absPath);

        // Reject directory traversal sequences regardless of platform encoding.
        if (strpos($normalised, '/..') !== false) {
            return '';
        }

        $uploadsNorm = rtrim(str_replace('\\', '/', $uploadsBase), '/') . '/';
        if (strpos($normalised, $uploadsNorm) !== 0) {
            return '';
        }
        return $normalised;
    }

    /**
     * Absolute filesystem path to the uploads root.
     */
    private function uploadsBase(): string
    {
        $dir = wp_upload_dir();
        return rtrim((string)($dir['basedir'] ?? ''), '/\\');
    }

    /**
     * Generate a random manifest ID (16 hex bytes = 32 chars, URL-safe).
     */
    private function generateId(): string
    {
        return bin2hex(random_bytes(16));
    }
}
