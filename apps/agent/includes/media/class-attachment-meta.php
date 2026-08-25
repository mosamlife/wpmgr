<?php
/**
 * AttachmentMeta: resolves attachment paths/URLs, snapshots
 * _wp_attachment_metadata, applies an optimization (disk + WP meta + blob), and
 * restores from the snapshot.
 *
 * This is the class that encodes the CORRECTNESS TRAP #1 (same-ext vs
 * different-ext archive — analysis/media-postmeta-blob.md §2):
 *
 *   - DIFFERENT extension (JPG -> AVIF/WebP): the original is NEVER archived;
 *     both files coexist; the optimized file gets a NEW path/URL via
 *     Rename::changeExtension; a `replacements[old_url] = new_url` entry drives
 *     the DB rewrite; the legacy twin is the .htaccess Accept fallback.
 *   - SAME extension (target_format='original'): archive EVERY same-ext size to
 *     `.wpmgr-original.<ext>` FIRST (Rename::archive), then DiskWriter writes the
 *     new bytes AT the original path. URL is unchanged, so no DB rewrite for it.
 *
 * Restore reverses this using the blob's `original_data` snapshot + the
 * archive_mode recorded per variant: delete optimized files BEFORE un-renaming
 * archives (analysis doc lines 203-204).
 *
 * Per-variant apply/restore is split so the apply command can drive each
 * optimized output independently and accumulate the blob.
 *
 * @package WPMgr\Agent\Media
 */

declare(strict_types=1);

namespace WPMgr\Agent\Media;

use WPMgr\Agent\MediaKeystore;

/**
 * Path resolution + metadata snapshot/apply/restore for one attachment.
 */
final class AttachmentMeta
{
    /** Archive mode: optimized bytes written at the original path; original archived. */
    public const MODE_REPLACE = 'replace_in_place';

    /** Archive mode: optimized written to a new ext path; original left in place. */
    public const MODE_COEXIST = 'coexist';

    private MediaKeystore $keystore;

    private Rename $rename;

    private DiskWriter $writer;

    public function __construct(
        ?MediaKeystore $keystore = null,
        ?Rename $rename = null,
        ?DiskWriter $writer = null
    ) {
        $this->keystore = $keystore ?? new MediaKeystore();
        $this->rename   = $rename ?? new Rename();
        $this->writer   = $writer ?? new DiskWriter();
    }

    /**
     * The byte size of an attachment's FULL image file.
     *
     * This is deliberately NOT the sum of every WordPress-generated rendition.
     * Summing the sub-sizes double-counts the image: a `-scaled` upload's
     * descending intermediates (2048x2048, 1536x1536, large, ...) add up to
     * roughly a SECOND full image, which inflated a ~7MB original to ~16MB on
     * the dashboard size table and the WP-admin stats panel. Their basenames
     * are suffixed (`photo-2048x2048.jpg`), so a basename-alias guard never
     * catches them. The figure users (and WordPress's own "File size") expect
     * is the full file alone — and a full-vs-full before/after is the only
     * honest basis for the "saved %".
     *
     * WordPress 6.0+ stores the full file's bytes in `filesize`; when it is
     * absent (older metadata) we stat the file on disk. Returns 0 only when
     * neither is resolvable.
     *
     * @param array<string,mixed> $metadata _wp_attachment_metadata array.
     * @return int The full image file's bytes.
     */
    public static function fullBytes(array $metadata): int
    {
        $size = (int) ($metadata['filesize'] ?? 0);
        if ($size > 0) {
            return $size;
        }

        // Fallback for pre-6.0 metadata without `filesize`: stat the full file.
        $file = isset($metadata['file']) ? (string) $metadata['file'] : '';
        if ($file !== '' && function_exists('wp_get_upload_dir')) {
            $uploads = wp_get_upload_dir();
            if (is_array($uploads) && !empty($uploads['basedir'])) {
                $path = rtrim((string) $uploads['basedir'], '/\\') . '/' . ltrim($file, '/\\');
                if (@is_file($path)) {
                    return (int) @filesize($path);
                }
            }
        }

        return 0;
    }

    /**
     * The total bytes SAVED across every optimized variant (full + each
     * thumbnail): sum over the optimized records of (original variant bytes −
     * optimized variant bytes), counting only positive reductions.
     *
     * This is the all-variant savings the dashboard "Bytes saved" tile rolls up
     * — distinct from the per-image full-file before/after. It is computed from
     * the optimization blob's two halves: $originalData (the pre-optimize
     * _wp_attachment_metadata snapshot) and $optimizedData (size-name => record
     * with the optimized `size` AND the measured `source_size`, both set by
     * applyVariant()).
     *
     * Per-variant original bytes are taken from the record's `source_size` — the
     * bytes measured on disk at apply time, BEFORE archive/overwrite. This is the
     * authoritative source: WordPress only stores a per-sub-size `filesize` in
     * metadata on 6.0+ (and not for older/plugin-generated metadata), so relying
     * on the metadata alone undercounts thumbnail savings. When `source_size` is
     * absent (a blob written by an older agent), fall back to the metadata size:
     * fullBytes() for the full (disk fallback included), `sizes[name].filesize`
     * for a sub-size.
     *
     * @param array<string,mixed>               $originalData  Pre-optimize snapshot.
     * @param array<string,array<string,mixed>> $optimizedData size-name => record.
     * @return int Total bytes saved (>= 0).
     */
    public static function savedBytes(array $originalData, array $optimizedData): int
    {
        if ($optimizedData === []) {
            return 0;
        }
        $origSizes = is_array($originalData['sizes'] ?? null) ? $originalData['sizes'] : [];
        $saved     = 0;
        foreach ($optimizedData as $name => $rec) {
            if (!is_array($rec)) {
                continue;
            }
            $optimized = (int) ($rec['size'] ?? 0);
            if ($optimized <= 0) {
                continue;
            }
            // Prefer the measured source bytes recorded at apply time.
            $original = (int) ($rec['source_size'] ?? 0);
            if ($original <= 0) {
                // Fallback for blobs written by an older agent (no source_size).
                if ($name === 'full') {
                    $original = self::fullBytes($originalData);
                } else {
                    $sizeRec  = is_array($origSizes[$name] ?? null) ? $origSizes[$name] : [];
                    $original = (int) ($sizeRec['filesize'] ?? 0);
                }
            }
            if ($original > $optimized) {
                $saved += $original - $optimized;
            }
        }

        return $saved;
    }

    /**
     * Convert an attachment URL to an absolute filesystem path under the
     * uploads dir, or '' when it doesn't live there. Resolves path from
     * the uploads base URL/dir (analysis doc lines 858-860).
     *
     * @param string $url Attachment (variant) URL.
     * @return string Absolute path, or '' if not resolvable.
     */
    public function urlToPath(string $url): string
    {
        if ($url === '' || !function_exists('wp_get_upload_dir')) {
            return '';
        }
        $uploads = wp_get_upload_dir();
        if (!is_array($uploads) || empty($uploads['baseurl']) || empty($uploads['basedir'])) {
            return '';
        }
        $baseUrl = (string) $uploads['baseurl'];
        $baseDir = (string) $uploads['basedir'];

        if (strpos($url, $baseUrl) !== 0) {
            return '';
        }
        $path = str_replace($baseUrl, $baseDir, $url);

        return function_exists('wp_normalize_path') ? wp_normalize_path($path) : $path;
    }

    /**
     * Snapshot the live _wp_attachment_metadata verbatim — the restore bible
     * (analysis/media-postmeta-blob.md "original_data").
     *
     * @param int $attachmentId WP attachment id.
     * @return array<string,mixed> The metadata array, or [] when absent.
     */
    public function snapshotMetadata(int $attachmentId): array
    {
        if (!function_exists('wp_get_attachment_metadata')) {
            return [];
        }
        $meta = wp_get_attachment_metadata($attachmentId);

        return is_array($meta) ? $meta : [];
    }

    /**
     * Decide the archive mode for a variant from the source vs target mime.
     * SAME extension => MODE_REPLACE (archive original); DIFFERENT => MODE_COEXIST.
     *
     * @param string $sourcePath    The source file path (for its extension).
     * @param string $optimizedMime The optimized output mime (e.g. image/avif).
     * @return string One of MODE_REPLACE | MODE_COEXIST.
     */
    public function archiveModeFor(string $sourcePath, string $optimizedMime): string
    {
        $sourceExt    = $this->rename->extensionOf($sourcePath);
        $optimizedExt = $this->mimeToExt($optimizedMime);

        // Same extension (re-compress original format) => archive in place.
        return ($optimizedExt !== '' && $optimizedExt === $sourceExt)
            ? self::MODE_REPLACE
            : self::MODE_COEXIST;
    }

    /**
     * Apply ONE optimized variant on disk and return the per-variant result
     * record + any URL replacement the DB rewrite must apply.
     *
     * SAME-EXT (replace): archive the original FIRST, then write new bytes at the
     * original path (no URL change). DIFFERENT-EXT (coexist): write to the new
     * ext path; record old_url => new_url for the DB rewrite. This is the
     * data-loss-critical branch — get the ORDER right (archive BEFORE write).
     *
     * @param string      $sourceUrl     The original (pre-optimize) variant URL.
     * @param string      $sourcePath    The original variant absolute path.
     * @param string      $bytes         Optimized image bytes.
     * @param string      $optimizedMime The optimized output mime.
     * @param string|null $expectSha     Optional SHA-256 to verify the bytes.
     * @return array{ok:bool,mode:string,record:array<string,mixed>,replacement:array{from:string,to:string}|null}
     */
    public function applyVariant(
        string $sourceUrl,
        string $sourcePath,
        string $bytes,
        string $optimizedMime,
        ?string $expectSha = null
    ): array {
        $mode = $this->archiveModeFor($sourcePath, $optimizedMime);

        // Measure the ORIGINAL source bytes NOW, before any archive/overwrite. This
        // is the authoritative per-variant original size for savedBytes(): WordPress
        // only stores a per-sub-size `filesize` in metadata on 6.0+ (and not at all
        // for older/plugin-generated metadata), so relying on the metadata alone
        // silently undercounts thumbnail savings. The source file is always present
        // and statable here (collectVariants resolved it), and in replace-mode it is
        // about to be archived, so we MUST stat it before that.
        $sourceSize = ($sourcePath !== '' && @is_file($sourcePath)) ? (int) @filesize($sourcePath) : 0;

        if ($mode === self::MODE_REPLACE) {
            // SAME EXT: archive the original bytes FIRST so they're never lost,
            // then write the smaller optimized bytes at the SAME path/URL.
            $this->rename->archive($sourcePath);
            $ok = $this->writer->write($sourcePath, $bytes, $expectSha);

            $record = [
                'size'          => strlen($bytes),
                'source_size'   => $sourceSize,
                'mime_type'     => $optimizedMime,
                'url'           => $sourceUrl,
                'path'          => $sourcePath,
                'relative_path' => $this->relativeUploadPath($sourcePath),
                'archive_mode'  => self::MODE_REPLACE,
            ];

            // URL unchanged => no DB replacement needed for this variant.
            return ['ok' => $ok, 'mode' => $mode, 'record' => $record, 'replacement' => null];
        }

        // DIFFERENT EXT: write the optimized file at a NEW ext path; leave the
        // original in place (it is the Accept fallback). The public URL changes.
        $optimizedExt  = $this->mimeToExt($optimizedMime);
        $optimizedPath = $this->rename->changeExtension($sourcePath, $optimizedExt);
        $optimizedUrl  = $this->rename->changeExtension($sourceUrl, $optimizedExt);

        $ok = $this->writer->write($optimizedPath, $bytes, $expectSha);

        $record = [
            'size'          => strlen($bytes),
            'source_size'   => $sourceSize,
            'mime_type'     => $optimizedMime,
            'url'           => $optimizedUrl,
            'path'          => $optimizedPath,
            'relative_path' => $this->relativeUploadPath($optimizedPath),
            'archive_mode'  => self::MODE_COEXIST,
        ];

        $replacement = ($optimizedUrl !== '' && $optimizedUrl !== $sourceUrl)
            ? ['from' => $sourceUrl, 'to' => $optimizedUrl]
            : null;

        return ['ok' => $ok, 'mode' => $mode, 'record' => $record, 'replacement' => $replacement];
    }

    /**
     * Update WordPress's live _wp_attachment_metadata to point at the optimized
     * variants (full file + per-size file/mime/filesize). Also updates the
     * attached-file pointer + guid for the full size when its ext changed
     * (analysis doc lines 253-275).
     *
     * @param int                              $attachmentId  WP attachment id.
     * @param array<string,mixed>              $metadata      The pre-optimize metadata (snapshot).
     * @param array<string,array<string,mixed>> $optimizedData size-name => per-size record from applyVariant().
     * @return array<string,mixed> The new live metadata that was written.
     */
    public function applyOptimizedMetadata(int $attachmentId, array $metadata, array $optimizedData): array
    {
        $new = $metadata;

        // Full file pointer + guid.
        $full = $optimizedData['full'] ?? null;
        if (is_array($full) && isset($full['relative_path'], $full['url'])) {
            if (function_exists('update_attached_file')) {
                update_attached_file($attachmentId, (string) $full['relative_path']);
            }
            $this->updateGuid($attachmentId, (string) $full['url']);
            $new['file']     = (string) $full['relative_path'];
            $new['filesize'] = (int) ($full['size'] ?? ($new['filesize'] ?? 0));
        }

        // Per-size records.
        if (isset($new['sizes']) && is_array($new['sizes'])) {
            foreach ($new['sizes'] as $sizeName => $sizeData) {
                if (!isset($optimizedData[$sizeName]) || !is_array($optimizedData[$sizeName])) {
                    continue;
                }
                $rec = $optimizedData[$sizeName];
                if (!is_array($sizeData)) {
                    $sizeData = [];
                }
                if (isset($rec['relative_path'])) {
                    $sizeData['file'] = basename((string) $rec['relative_path']);
                }
                if (isset($rec['mime_type'])) {
                    $sizeData['mime-type'] = (string) $rec['mime_type'];
                }
                if (isset($rec['size'])) {
                    $sizeData['filesize'] = (int) $rec['size'];
                }
                $new['sizes'][$sizeName] = $sizeData;
            }
        }

        if (function_exists('wp_update_attachment_metadata')) {
            wp_update_attachment_metadata($attachmentId, $new);
        }

        return $new;
    }

    /**
     * Restore the live _wp_attachment_metadata from the blob's original_data
     * snapshot (analysis doc lines 207-211). Also restores the attached-file
     * pointer + guid to the original full URL.
     *
     * @param int                 $attachmentId WP attachment id.
     * @param array<string,mixed> $originalData The blob's original_data snapshot.
     * @param string              $originalFullUrl The original full-size URL (for guid).
     * @return void
     */
    public function restoreMetadata(int $attachmentId, array $originalData, string $originalFullUrl): void
    {
        if ($originalData === []) {
            return;
        }
        if (isset($originalData['file']) && function_exists('update_attached_file')) {
            update_attached_file($attachmentId, (string) $originalData['file']);
        }
        if ($originalFullUrl !== '') {
            $this->updateGuid($attachmentId, $originalFullUrl);
        }
        if (function_exists('wp_update_attachment_metadata')) {
            wp_update_attachment_metadata($attachmentId, $originalData);
        }
    }

    /**
     * Compose the full optimization blob to persist after an apply.
     *
     * @param array{
     *   job_id:string,generation:int,compression_level:string,target_format:string,
     *   sizes_optimized:list<string>,sizes_unoptimized:array<string,string>,
     *   original_data:array<string,mixed>,optimized_data:array<string,array<string,mixed>>,
     *   replacements:array<string,string>
     * } $parts
     * @return array<string,mixed>
     */
    public function composeBlob(array $parts): array
    {
        return [
            'wpmgr_job_id'      => $parts['job_id'],
            'wpmgr_generation'  => $parts['generation'],
            'status'            => MediaKeystore::STATUS_OPTIMIZED,
            'compression_level' => $parts['compression_level'],
            'target_format'     => $parts['target_format'],
            'sizes_optimized'   => array_values($parts['sizes_optimized']),
            'sizes_unoptimized' => $parts['sizes_unoptimized'],
            'original_data'     => $parts['original_data'],
            'optimized_data'    => $parts['optimized_data'],
            'replacements'      => $parts['replacements'],
            'original_deleted'  => 0,
        ];
    }

    /**
     * Persist a composed blob via the keystore.
     *
     * @param int                 $attachmentId WP attachment id.
     * @param array<string,mixed> $blob         The composed blob.
     * @return void
     */
    public function saveBlob(int $attachmentId, array $blob): void
    {
        $this->keystore->set($attachmentId, $blob);
    }

    /**
     * Map an image mime type to a file extension.
     *
     * @param string $mime e.g. 'image/avif'.
     * @return string e.g. 'avif' ('' when not an image/* mime).
     */
    public function mimeToExt(string $mime): string
    {
        $mime = strtolower(trim($mime));
        $map  = [
            'image/avif' => 'avif',
            'image/webp' => 'webp',
            'image/jpeg' => 'jpg',
            'image/jpg'  => 'jpg',
            'image/png'  => 'png',
            'image/gif'  => 'gif',
        ];

        return $map[$mime] ?? '';
    }

    /**
     * The uploads-relative path for an absolute path (for _wp_attachment_metadata
     * 'file' / per-size 'file'). Uses WP's helper when available.
     *
     * @param string $path Absolute path.
     * @return string Uploads-relative path.
     */
    public function relativeUploadPath(string $path): string
    {
        if ($path === '') {
            return '';
        }
        if (function_exists('_wp_relative_upload_path')) {
            $rel = _wp_relative_upload_path($path);
            if (is_string($rel) && $rel !== '') {
                return $rel;
            }
        }
        if (function_exists('wp_get_upload_dir')) {
            $uploads = wp_get_upload_dir();
            if (is_array($uploads) && !empty($uploads['basedir'])) {
                $base = (string) $uploads['basedir'];
                if (strpos($path, $base) === 0) {
                    return ltrim(substr($path, strlen($base)), '/\\');
                }
            }
        }

        return $path;
    }

    /**
     * Update the attachment's guid + post_mime_type when the full URL changed
     * (analysis doc lines 278-289).
     *
     * @param int    $attachmentId WP attachment id.
     * @param string $url          The new full-size URL.
     * @return void
     */
    private function updateGuid(int $attachmentId, string $url): void
    {
        if ($attachmentId <= 0 || $url === '') {
            return;
        }
        $wpdb = $GLOBALS['wpdb'] ?? null;
        if (!is_object($wpdb) || !isset($wpdb->posts)) {
            return;
        }
        if (function_exists('get_post_field') && get_post_field('guid', $attachmentId) === $url) {
            return;
        }
        $ext  = $this->rename->extensionOf($url);
        $mime = $ext === 'jpg' ? 'image/jpeg' : ('image/' . $ext);
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct update on core posts table; correctness requires live write; no plugin-helper wraps $wpdb->update for GUID+mime_type
        $wpdb->update($wpdb->posts, ['guid' => $url, 'post_mime_type' => $mime], ['ID' => $attachmentId]);
    }
}
