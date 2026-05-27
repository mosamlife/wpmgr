<?php
/**
 * SnapshotManager: captures and restores pre-update snapshots of plugin/theme
 * directories so updates can be rolled back.
 *
 * Snapshots live under wp-content/uploads/wpmgr-snapshots/<snapshot_id>/ and the
 * base directory is hardened against web listing (index.php + .htaccess deny).
 * For core, only the prior version string is recorded (the directory itself is
 * not copied); rollback is then a downgrade-by-version.
 *
 * All paths derived from request input are bounded to wp-content via realpath
 * containment checks, and slugs are sanitized upstream by UpdateCommand.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * Captures and restores filesystem snapshots for safe rollback.
 */
class SnapshotManager
{
    /** Snapshot directory name under uploads. */
    private const DIR = 'wpmgr-snapshots';

    /**
     * Capture a pre-update snapshot for an item.
     *
     * For plugin/theme the live source directory is copied into the snapshot
     * store. For core only the prior version is recorded (no copy).
     *
     * @param string $type        plugin|theme|core.
     * @param string $slug        Sanitized slug.
     * @param string $fromVersion Currently installed version (recorded for core).
     * @return array{snapshot_id:string,log:string}
     */
    public function capture(string $type, string $slug, string $fromVersion): array
    {
        $snapshotId = $this->newSnapshotId();
        $base       = $this->snapshotBaseDir();
        if ($base === '') {
            return ['snapshot_id' => '', 'log' => 'Snapshot store unavailable; proceeding without snapshot.'];
        }

        $this->protectBaseDir($base);

        $dest = $base . '/' . $snapshotId;

        if ($type === 'core') {
            // Record the prior version for downgrade-by-version on rollback.
            $this->writeMeta($dest, $type, $slug, $fromVersion);

            return ['snapshot_id' => $snapshotId, 'log' => 'Recorded core version ' . $fromVersion . ' for rollback.'];
        }

        $source = $this->liveDir($type, $slug);
        if ($source === '' || !is_dir($source)) {
            return ['snapshot_id' => '', 'log' => 'Source directory not found; proceeding without snapshot.'];
        }

        if (!$this->copyDir($source, $dest . '/payload')) {
            return ['snapshot_id' => '', 'log' => 'Snapshot copy failed; proceeding without snapshot.'];
        }

        $this->writeMeta($dest, $type, $slug, $fromVersion);

        return ['snapshot_id' => $snapshotId, 'log' => 'Captured snapshot ' . $snapshotId . '.'];
    }

    /**
     * Restore a plugin/theme directory from a previously captured snapshot.
     *
     * The live directory is moved aside, the snapshot payload copied back, and
     * the set-aside copy removed on success (rolled back on failure).
     *
     * @param string $type       plugin|theme.
     * @param string $slug       Sanitized slug.
     * @param string $snapshotId Snapshot identifier.
     * @return array{ok:bool,log:string}
     */
    public function restore(string $type, string $slug, string $snapshotId): array
    {
        $base = $this->snapshotBaseDir();
        if ($base === '') {
            return ['ok' => false, 'log' => 'Snapshot store unavailable.'];
        }

        $snapshotDir = $this->resolveSnapshotDir($base, $snapshotId);
        if ($snapshotDir === '') {
            return ['ok' => false, 'log' => 'Invalid snapshot id.'];
        }

        $payload = $snapshotDir . '/payload';
        if (!is_dir($payload)) {
            return ['ok' => false, 'log' => 'Snapshot payload missing.'];
        }

        $live = $this->liveDir($type, $slug);
        if ($live === '') {
            return ['ok' => false, 'log' => 'Live directory could not be resolved.'];
        }

        $asideName = $live . '.wpmgr-old-' . $snapshotId;

        // Move the current live dir aside (if present).
        if (is_dir($live) && !@rename($live, $asideName)) {
            return ['ok' => false, 'log' => 'Could not stage live directory aside.'];
        }

        if (!$this->copyDir($payload, $live)) {
            // Roll back the move so we don't leave the site without the dir.
            if (is_dir($asideName) && !is_dir($live)) {
                @rename($asideName, $live);
            }

            return ['ok' => false, 'log' => 'Restore copy failed; original retained.'];
        }

        // Success: drop the set-aside copy.
        if (is_dir($asideName)) {
            $this->deleteDir($asideName);
        }

        return ['ok' => true, 'log' => 'Restored ' . $slug . ' from snapshot ' . $snapshotId . '.'];
    }

    /**
     * Read the recorded prior version from a snapshot's metadata.
     *
     * @param string $snapshotId Snapshot identifier.
     * @return string Recorded version, or '' when unavailable.
     */
    public function recordedVersion(string $snapshotId): string
    {
        $base = $this->snapshotBaseDir();
        if ($base === '') {
            return '';
        }
        $dir = $this->resolveSnapshotDir($base, $snapshotId);
        if ($dir === '') {
            return '';
        }
        $metaFile = $dir . '/meta.json';
        if (!is_file($metaFile)) {
            return '';
        }
        $raw = (string) @file_get_contents($metaFile);
        $meta = json_decode($raw, true);
        if (is_array($meta) && isset($meta['from_version']) && is_string($meta['from_version'])) {
            return $meta['from_version'];
        }

        return '';
    }

    /**
     * Remove a snapshot directory and its contents.
     *
     * @param string $snapshotId Snapshot identifier.
     * @return bool
     */
    public function cleanup(string $snapshotId): bool
    {
        $base = $this->snapshotBaseDir();
        if ($base === '') {
            return false;
        }
        $dir = $this->resolveSnapshotDir($base, $snapshotId);
        if ($dir === '') {
            return false;
        }

        return $this->deleteDir($dir);
    }

    // ---------------------------------------------------------------------
    // Path resolution / containment
    // ---------------------------------------------------------------------

    /**
     * Generate a unique snapshot identifier (no path-significant characters).
     *
     * @return string
     */
    protected function newSnapshotId(): string
    {
        try {
            return 'snap_' . bin2hex(random_bytes(12));
        } catch (\Throwable $e) {
            return 'snap_' . (string) time() . '_' . (string) random_int(1000, 9999);
        }
    }

    /**
     * Resolve and validate a snapshot directory inside the base, rejecting any
     * id that escapes the base directory.
     *
     * @param string $base       Snapshot base directory (absolute).
     * @param string $snapshotId Snapshot identifier.
     * @return string Absolute snapshot dir, or '' when invalid.
     */
    private function resolveSnapshotDir(string $base, string $snapshotId): string
    {
        // Snapshot ids are generated as snap_<hex>; never allow separators.
        if (preg_match('#^snap_[A-Za-z0-9_]+$#', $snapshotId) !== 1) {
            return '';
        }

        $candidate = $base . '/' . $snapshotId;
        if (!is_dir($candidate)) {
            return '';
        }

        return $this->containedRealpath($candidate, $base);
    }

    /**
     * The absolute snapshot base directory under uploads. Created if missing.
     *
     * @return string Absolute path, or '' when uploads is unavailable.
     */
    protected function snapshotBaseDir(): string
    {
        $uploads = $this->uploadsBaseDir();
        if ($uploads === '') {
            return '';
        }

        $base = rtrim($uploads, '/\\') . '/' . self::DIR;
        if (!is_dir($base)) {
            if (function_exists('wp_mkdir_p')) {
                wp_mkdir_p($base);
            } elseif (!@mkdir($base, 0755, true) && !is_dir($base)) {
                return '';
            }
        }

        return $base;
    }

    /**
     * Resolve the uploads base directory.
     *
     * @return string
     */
    protected function uploadsBaseDir(): string
    {
        if (function_exists('wp_upload_dir')) {
            $u = wp_upload_dir();
            if (is_array($u) && isset($u['basedir']) && is_string($u['basedir']) && $u['basedir'] !== '') {
                return $u['basedir'];
            }
        }
        if (defined('WP_CONTENT_DIR')) {
            return rtrim((string) WP_CONTENT_DIR, '/\\') . '/uploads';
        }

        return '';
    }

    /**
     * Resolve the live directory for a plugin/theme, bounded to wp-content.
     *
     * @param string $type plugin|theme.
     * @param string $slug Sanitized slug.
     * @return string Absolute path, or '' when it cannot be safely resolved.
     */
    protected function liveDir(string $type, string $slug): string
    {
        $contentDir = defined('WP_CONTENT_DIR')
            ? rtrim((string) WP_CONTENT_DIR, '/\\')
            : '';
        if ($contentDir === '') {
            return '';
        }

        if ($type === 'plugin') {
            $folder = strpos($slug, '/') !== false ? substr($slug, 0, strpos($slug, '/')) : $slug;
            $root   = defined('WP_PLUGIN_DIR') ? rtrim((string) WP_PLUGIN_DIR, '/\\') : $contentDir . '/plugins';
            $path   = $root . '/' . $folder;
        } elseif ($type === 'theme') {
            $root = $this->themeRoot($contentDir);
            $path = $root . '/' . $slug;
        } else {
            return '';
        }

        // Containment: the resolved path's parent must sit under wp-content.
        $parent = dirname($path);
        if ($this->containedRealpath($parent, $contentDir) === '') {
            return '';
        }

        return $path;
    }

    /**
     * Theme root directory.
     *
     * @param string $contentDir wp-content path.
     * @return string
     */
    private function themeRoot(string $contentDir): string
    {
        if (function_exists('get_theme_root')) {
            $root = get_theme_root();
            if (is_string($root) && $root !== '') {
                return rtrim($root, '/\\');
            }
        }

        return $contentDir . '/themes';
    }

    /**
     * Verify that a path resolves inside (or equal to) a trusted base directory.
     *
     * @param string $path Candidate path (may not yet exist for child dirs).
     * @param string $base Trusted base directory.
     * @return string Canonicalized path when contained, '' otherwise.
     */
    private function containedRealpath(string $path, string $base): string
    {
        $realBase = realpath($base);
        if ($realBase === false) {
            return '';
        }

        $real = realpath($path);
        if ($real === false) {
            return '';
        }

        $realBase = rtrim($realBase, '/\\');
        if ($real !== $realBase && !str_starts_with($real, $realBase . DIRECTORY_SEPARATOR)) {
            return '';
        }

        return $real;
    }

    // ---------------------------------------------------------------------
    // Filesystem primitives
    // ---------------------------------------------------------------------

    /**
     * Harden the snapshot base directory against web listing/access.
     *
     * @param string $base Absolute base directory.
     * @return void
     */
    private function protectBaseDir(string $base): void
    {
        $index = $base . '/index.php';
        if (!file_exists($index)) {
            @file_put_contents($index, "<?php\n// Silence is golden.\n");
        }
        $htaccess = $base . '/.htaccess';
        if (!file_exists($htaccess)) {
            @file_put_contents($htaccess, "Deny from all\nRequire all denied\n");
        }
    }

    /**
     * Write snapshot metadata.
     *
     * @param string $dir         Snapshot directory.
     * @param string $type        Item type.
     * @param string $slug        Slug.
     * @param string $fromVersion Recorded version.
     * @return void
     */
    private function writeMeta(string $dir, string $type, string $slug, string $fromVersion): void
    {
        if (!is_dir($dir)) {
            if (function_exists('wp_mkdir_p')) {
                wp_mkdir_p($dir);
            } else {
                @mkdir($dir, 0755, true);
            }
        }
        $meta = [
            'type'         => $type,
            'slug'         => $slug,
            'from_version' => $fromVersion,
            'created_at'   => time(),
        ];
        @file_put_contents($dir . '/meta.json', (string) json_encode($meta));
    }

    /**
     * Recursively copy a directory.
     *
     * @param string $src Source directory.
     * @param string $dst Destination directory.
     * @return bool
     */
    protected function copyDir(string $src, string $dst): bool
    {
        if (!is_dir($src)) {
            return false;
        }
        if (!is_dir($dst) && !@mkdir($dst, 0755, true) && !is_dir($dst)) {
            return false;
        }

        $items = @scandir($src);
        if ($items === false) {
            return false;
        }

        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $from = $src . '/' . $item;
            $to   = $dst . '/' . $item;

            // Do not follow symlinks out of the tree.
            if (is_link($from)) {
                continue;
            }

            if (is_dir($from)) {
                if (!$this->copyDir($from, $to)) {
                    return false;
                }
            } elseif (!@copy($from, $to)) {
                return false;
            }
        }

        return true;
    }

    /**
     * Recursively delete a directory.
     *
     * @param string $dir Directory to remove.
     * @return bool
     */
    protected function deleteDir(string $dir): bool
    {
        if (!is_dir($dir)) {
            return false;
        }
        $items = @scandir($dir);
        if ($items === false) {
            return false;
        }
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $path = $dir . '/' . $item;
            if (is_dir($path) && !is_link($path)) {
                $this->deleteDir($path);
            } else {
                @unlink($path);
            }
        }

        return @rmdir($dir);
    }
}
