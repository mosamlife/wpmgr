<?php
/**
 * DiskWriter: atomic image-byte writes (temp file + rename) with optional
 * SHA-256 verification.
 *
 * Optimized bytes arrive from the CP's presigned-GET (the media-encoder wrote
 * them to media/.../out/<variant>). We MUST NOT half-write the public path: a
 * truncated write would leave a corrupt image at a live URL. So we write to a
 * sibling temp file in the SAME directory (so rename() is atomic on POSIX —
 * same filesystem, no cross-device copy) and rename over the target.
 *
 * This is the single seam that ever creates the optimized file on disk. The
 * SAME-EXT archive ordering (Rename::archive BEFORE this write) is the caller's
 * responsibility (AttachmentMeta / the apply command), not this class's.
 *
 * @package WPMgr\Agent\Media
 */

declare(strict_types=1);

namespace WPMgr\Agent\Media;

/**
 * Atomic writer for optimized image bytes.
 */
final class DiskWriter
{
    /**
     * Atomically write $bytes to $path. Creates the parent directory if needed,
     * writes to a temp sibling, fsyncs-by-rename, and chmods to 0644 so the web
     * server can serve it. Optionally verifies a SHA-256 of the bytes first.
     *
     * @param string      $path       Absolute destination path.
     * @param string      $bytes      Raw optimized image bytes.
     * @param string|null $expectSha  Optional lower-hex SHA-256 to verify before writing.
     * @return bool True on success.
     */
    public function write(string $path, string $bytes, ?string $expectSha = null): bool
    {
        if ($path === '' || $bytes === '') {
            return false;
        }

        if ($expectSha !== null && $expectSha !== '') {
            $actual = hash('sha256', $bytes);
            if (!hash_equals(strtolower($expectSha), $actual)) {
                return false;
            }
        }

        $dir = dirname($path);
        if (!is_dir($dir)) {
            if (function_exists('wp_mkdir_p')) {
                wp_mkdir_p($dir);
            } else {
                @mkdir($dir, 0755, true);
            }
            if (!is_dir($dir)) {
                return false;
            }
        }

        // Temp sibling in the SAME directory so the rename is atomic (same
        // filesystem; no cross-device copy that could be observed half-done).
        $tmp = $dir . DIRECTORY_SEPARATOR . '.wpmgr-tmp-' . bin2hex(random_bytes(8));

        $written = @file_put_contents($tmp, $bytes, LOCK_EX);
        if ($written === false || $written !== strlen($bytes)) {
            @unlink($tmp);

            return false;
        }

        if (!@rename($tmp, $path)) {
            @unlink($tmp);

            return false;
        }

        @chmod($path, 0644);

        return true;
    }

    /**
     * Delete a file if it exists, preferring wp_delete_file (which honors WP
     * filesystem filters) and falling back to unlink.
     *
     * @param string $path Absolute path.
     * @return void
     */
    public function delete(string $path): void
    {
        if ($path === '' || !@file_exists($path)) {
            return;
        }
        if (function_exists('wp_delete_file')) {
            wp_delete_file($path);

            return;
        }
        @unlink($path);
    }
}
