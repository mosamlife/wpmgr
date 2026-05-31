<?php
/**
 * Rename: the `.wpmgr-original.*` archive/restore engine.
 *
 * Mirrors FlyingPress's §2 rename trick (reference/flying-press-patterns.md §2,
 * reference/flying-press/src/ImageOptimizer.php:766-781 `rename_files()` +
 * :871-884 `change_extesion()`) under WPMgr naming. The marker is
 * `wpmgr-original` (vs FlyingPress's `flying-press-original`).
 *
 * The whole same-ext archive scheme hinges on this class:
 *   archive('/x/banner.jpg')  -> renames to '/x/banner.wpmgr-original.jpg'
 *   restore('/x/banner.wpmgr-original.jpg') -> renames back to '/x/banner.jpg'
 *
 * Pure filesystem ops — no WP, no DB. Every method is idempotent-friendly:
 * a missing source is a no-op (returns the would-be target), so a partial
 * archive can be retried without error. The archive name is a DOUBLE
 * extension: `change_extension($path, 'wpmgr-original.' . $ext)`
 * (FlyingPress ImageOptimizer.php:777).
 *
 * @package WPMgr\Agent\Media
 */

declare(strict_types=1);

namespace WPMgr\Agent\Media;

/**
 * Archive an original image to a double-extension name and reverse it.
 */
final class Rename
{
    /**
     * The archive marker inserted before the extension. Renaming
     * `banner.jpg` to `banner.wpmgr-original.jpg` makes the archive name a
     * "double extension" so a restore can strip the marker unambiguously.
     */
    public const SUFFIX = 'wpmgr-original';

    /**
     * Archive an original file by renaming it to its double-extension name.
     * `/x/banner.jpg` -> `/x/banner.wpmgr-original.jpg`. Used in the SAME-EXT
     * (target_format='original') branch BEFORE writing the optimized bytes at
     * the original path (FlyingPress ImageOptimizer.php:90, :126).
     *
     * @param string $path Absolute path to the original file.
     * @return string The archive path (whether or not the source existed).
     */
    public function archive(string $path): string
    {
        $ext     = $this->extensionOf($path);
        $archive = $this->changeExtension($path, self::SUFFIX . '.' . $ext);

        if ($path !== '' && @is_file($path)) {
            @rename($path, $archive);
        }

        return $archive;
    }

    /**
     * Restore an archived original by stripping the marker.
     * `/x/banner.wpmgr-original.jpg` -> `/x/banner.jpg`. The reverse of
     * archive(); mirrors FlyingPress's `$restore` branch (ImageOptimizer.php:776).
     *
     * @param string $archived Absolute path to the archived (double-extension) file.
     * @return string The restored path (whether or not the archive existed).
     */
    public function restore(string $archived): string
    {
        $restored = str_replace('.' . self::SUFFIX, '', $archived);

        if ($archived !== '' && $archived !== $restored && @is_file($archived)) {
            @rename($archived, $restored);
        }

        return $restored;
    }

    /**
     * Compute the archive path for an original WITHOUT touching the disk.
     * Used to predict where the archive lives during restore/delete-originals.
     *
     * @param string $path Absolute path to the original file.
     * @return string The archive path.
     */
    public function archivePathFor(string $path): string
    {
        $ext = $this->extensionOf($path);

        return $this->changeExtension($path, self::SUFFIX . '.' . $ext);
    }

    /**
     * Swap a path/URL's trailing extension. Pure string op; mirrors
     * FlyingPress `change_extesion()` (ImageOptimizer.php:871-884, note the
     * source's misspelling). Returns the input unchanged when the extension
     * already matches; otherwise replaces the trailing `.<ext>` segment.
     *
     * @param string $pathOrUrl Path or URL.
     * @param string $newExt    New extension (no leading dot), e.g. 'avif' or
     *                          'wpmgr-original.jpg' for the synthetic archive name.
     * @return string The path/URL with the new trailing extension.
     */
    public function changeExtension(string $pathOrUrl, string $newExt): string
    {
        if ($pathOrUrl === '' || $newExt === '') {
            return $pathOrUrl;
        }

        $currentExt = $this->extensionOf($pathOrUrl);
        if ($currentExt === $newExt) {
            return $pathOrUrl;
        }

        // Replace the trailing `.<ext>` only (FlyingPress ImageOptimizer.php:883).
        $result = preg_replace('/\.[^\.\/]+$/', '.' . $newExt, $pathOrUrl);

        return is_string($result) ? $result : $pathOrUrl;
    }

    /**
     * The lower-cased trailing extension of a path/URL ('' when none).
     *
     * @param string $pathOrUrl Path or URL.
     * @return string
     */
    public function extensionOf(string $pathOrUrl): string
    {
        $ext = pathinfo($pathOrUrl, PATHINFO_EXTENSION);

        return is_string($ext) ? strtolower($ext) : '';
    }
}
