<?php
/**
 * MediaKeystore: typed accessor over the per-attachment `wpmgr_image_optimization`
 * postmeta blob — the single on-site source of truth for an attachment's
 * optimization state and the restore bible.
 *
 * Shape + lifecycle are defined in analysis/media-postmeta-blob.md. The blob is
 * a verbatim WPMgr-naming analogue of FlyingPress's
 * `flying_press_image_optimizer_data` (reference/flying-press/src/ImageOptimizer.php:8),
 * but with two explicit fields FlyingPress lacks:
 *   - `wpmgr_job_id`     — CP media_optimization_jobs.id audit cross-ref.
 *   - `wpmgr_generation` — (re)optimization counter mirrored to asset.generation
 *     (FlyingPress overloads compression_level as its regen trigger; we keep an
 *     explicit counter — see analysis doc lines 11-14).
 *
 * This class only reads/writes the meta — it does NOT carry optimization logic.
 * All values pass straight through update_post_meta / get_post_meta. No secrets
 * ever touch this blob.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent;

/**
 * Get/set/delete/has helpers for the per-attachment optimization blob.
 */
final class MediaKeystore
{
    /**
     * Postmeta key for the optimization blob. Mirrors FlyingPress's META_KEY
     * (ImageOptimizer.php:8) under WPMgr naming. This EXACT string is also in
     * DbRewriter::SKIP_META_KEYS — the rewriter must never string-rewrite the
     * blob (it is the restore anchor).
     */
    public const KEY = 'wpmgr_image_optimization';

    /** Status: the attachment has optimized variants on disk. */
    public const STATUS_OPTIMIZED = 'optimized';

    /** Status: the attachment is excluded from optimization. */
    public const STATUS_EXCLUDED = 'excluded';

    /** Status: the archived originals were irreversibly deleted. */
    public const STATUS_ORIGINALS_DELETED = 'originals_deleted';

    /**
     * Read the optimization blob for an attachment.
     *
     * @param int $attachmentId WP attachment (post) id.
     * @return array<string,mixed> The blob, or [] when absent/malformed.
     */
    public function get(int $attachmentId): array
    {
        if (!function_exists('get_post_meta')) {
            return [];
        }
        $value = get_post_meta($attachmentId, self::KEY, true);

        return is_array($value) ? $value : [];
    }

    /**
     * Whether an attachment has a non-empty optimization blob.
     *
     * @param int $attachmentId WP attachment (post) id.
     * @return bool
     */
    public function has(int $attachmentId): bool
    {
        return $this->get($attachmentId) !== [];
    }

    /**
     * Whether the attachment is currently optimized (status === 'optimized').
     *
     * @param int $attachmentId WP attachment (post) id.
     * @return bool
     */
    public function isOptimized(int $attachmentId): bool
    {
        $blob = $this->get($attachmentId);

        return ($blob['status'] ?? '') === self::STATUS_OPTIMIZED;
    }

    /**
     * Whether the archived originals have been deleted (restore impossible).
     *
     * @param int $attachmentId WP attachment (post) id.
     * @return bool
     */
    public function originalsDeleted(int $attachmentId): bool
    {
        $blob = $this->get($attachmentId);

        return (int) ($blob['original_deleted'] ?? 0) === 1;
    }

    /**
     * Persist the full optimization blob.
     *
     * @param int                 $attachmentId WP attachment (post) id.
     * @param array<string,mixed> $blob         The blob to store.
     * @return void
     */
    public function set(int $attachmentId, array $blob): void
    {
        if (!function_exists('update_post_meta')) {
            return;
        }
        update_post_meta($attachmentId, self::KEY, $blob);
    }

    /**
     * Delete the optimization blob entirely (restore lifecycle shape #2 when
     * nothing was unoptimizable — analysis doc lines 22-24).
     *
     * @param int $attachmentId WP attachment (post) id.
     * @return void
     */
    public function delete(int $attachmentId): void
    {
        if (!function_exists('delete_post_meta')) {
            return;
        }
        delete_post_meta($attachmentId, self::KEY);
    }

    /**
     * Reduce the blob to the post-restore stub (analysis doc lifecycle shape #2)
     * — keep only the compression_level + sizes_unoptimized so we don't retry
     * known-bad sizes under the same profile. If sizes_unoptimized is empty the
     * blob is deleted instead (mirrors FlyingPress restore_single_image,
     * ImageOptimizer.php:213-221).
     *
     * @param int                  $attachmentId     WP attachment (post) id.
     * @param string               $compressionLevel The compression level at last optimize.
     * @param array<string,string> $sizesUnoptimized size-name => human reason.
     * @return void
     */
    public function reduceAfterRestore(int $attachmentId, string $compressionLevel, array $sizesUnoptimized): void
    {
        if ($sizesUnoptimized === []) {
            $this->delete($attachmentId);

            return;
        }

        $this->set($attachmentId, [
            'compression_level' => $compressionLevel,
            'sizes_unoptimized' => $sizesUnoptimized,
        ]);
    }

    /**
     * Mark the archived originals as deleted (analysis doc lifecycle shape #3 /
     * FlyingPress delete_original_image, ImageOptimizer.php:439-448).
     *
     * @param int $attachmentId WP attachment (post) id.
     * @return void
     */
    public function markOriginalsDeleted(int $attachmentId): void
    {
        $blob = $this->get($attachmentId);
        if ($blob === []) {
            return;
        }
        $blob['original_deleted'] = 1;
        $blob['status']          = self::STATUS_ORIGINALS_DELETED;
        $this->set($attachmentId, $blob);
    }
}
