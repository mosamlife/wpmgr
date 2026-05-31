<?php
/**
 * StatsRenderer: builds the per-attachment optimization stats HTML for the
 * Media Library modal panel + the edit-screen meta box (one renderer, two
 * surfaces — reference/flying-press-patterns.md §4d,
 * reference/flying-press/src/ImageOptimizer.php:676-738).
 *
 * Rendered states (top-to-bottom):
 *   1. Never optimized / excluded -> a single Status line.
 *   2. Status line (Optimized | Not Optimized | Originals deleted).
 *   3. Total size before -> after + saved% (the analysis/CP "bytes_before /
 *      bytes_after"), only when something was optimized.
 *   4. Sizes not optimized -> per-size <code>{dim}</code>: {reason} rows.
 *   5. A "View in WPMgr" footer.
 *
 * SECURITY: every dynamic value is escaped (esc_html). The reasons come from the
 * CP/encoder + WP and are treated as untrusted. No raw HTML interpolation of any
 * attacker-influenced string.
 *
 * @package WPMgr\Agent\Media
 */

declare(strict_types=1);

namespace WPMgr\Agent\Media;

use WPMgr\Agent\MediaKeystore;

/**
 * Renders the (escaped) stats panel + decides if an attachment is optimizable.
 */
final class StatsRenderer
{
    /** Source mimes we can optimize (FlyingPress MIME_OPTIMIZABLE). */
    private const OPTIMIZABLE_MIMES = ['image/jpeg', 'image/jpg', 'image/png'];

    private MediaKeystore $keystore;

    public function __construct(?MediaKeystore $keystore = null)
    {
        $this->keystore = $keystore ?? new MediaKeystore();
    }

    /**
     * Whether the attachment's ORIGINAL source mime is one we can optimize. Reads
     * the original mime from the blob's snapshot when present (so an already-
     * converted AVIF still reports its original JPEG mime), else the live mime.
     *
     * @param int    $attachmentId WP attachment id.
     * @param string $liveMime     The attachment's current post_mime_type.
     * @return bool
     */
    public function isOptimizable(int $attachmentId, string $liveMime = ''): bool
    {
        $original = $this->originalMime($attachmentId, $liveMime);

        return in_array($original, self::OPTIMIZABLE_MIMES, true);
    }

    /**
     * Render the escaped stats HTML for an attachment.
     *
     * @param int    $attachmentId WP attachment id.
     * @param string $liveMime     The attachment's current post_mime_type.
     * @return string HTML (escaped); '' when not optimizable.
     */
    public function renderForAttachment(int $attachmentId, string $liveMime = ''): string
    {
        if (!$this->isOptimizable($attachmentId, $liveMime)) {
            return '';
        }

        $blob = $this->keystore->get($attachmentId);

        // 1. Never optimized / excluded.
        if (($blob['status'] ?? '') === '') {
            return $this->wrap($this->statusLine($this->t('Not Optimized yet')));
        }
        if (($blob['status'] ?? '') === MediaKeystore::STATUS_EXCLUDED) {
            return $this->wrap($this->statusLine($this->t('Excluded from Optimization')));
        }

        $originalsDeleted = (int) ($blob['original_deleted'] ?? 0) === 1
            || ($blob['status'] ?? '') === MediaKeystore::STATUS_ORIGINALS_DELETED;
        $sizesOptimized   = is_array($blob['sizes_optimized'] ?? null) ? $blob['sizes_optimized'] : [];

        // 2. Status line.
        $statusText = $sizesOptimized === [] ? $this->t('Not Optimized') : $this->t('Optimized');
        if ($originalsDeleted) {
            $statusText = $this->t('Optimized (originals deleted)');
        }
        $html = $this->statusLine($statusText);

        // 3. Before -> after + saved%.
        if ($sizesOptimized !== []) {
            $before = $this->totalBytes(is_array($blob['original_data'] ?? null) ? $blob['original_data'] : []);
            $after  = $this->totalBytes($this->liveMetadata($attachmentId));
            $html  .= $this->sizeLine($before, $after);
        }

        // 4. Sizes not optimized.
        $unoptimized = is_array($blob['sizes_unoptimized'] ?? null) ? $blob['sizes_unoptimized'] : [];
        if ($unoptimized !== []) {
            $html .= $this->unoptimizedRows($unoptimized, is_array($blob['original_data'] ?? null) ? $blob['original_data'] : []);
        }

        // 5. Footer.
        $html .= $this->footer();

        return $this->wrap($html);
    }

    // ------------------------------------------------------------------
    // building blocks (all escaped)
    // ------------------------------------------------------------------

    /**
     * @param string $html Inner HTML (already escaped piece-by-piece).
     * @return string
     */
    private function wrap(string $html): string
    {
        return '<div class="wpmgr-media-stats">' . $html . '</div>';
    }

    private function statusLine(string $value): string
    {
        return '<div class="wpmgr-status"><strong>' . $this->esc($this->t('Status')) . ':</strong> '
            . $this->esc($value) . '</div>';
    }

    private function sizeLine(int $before, int $after): string
    {
        $saved = $before > 0 ? max(0, (int) round((($before - $after) / $before) * 100)) : 0;

        return '<div class="wpmgr-total-size"><strong>' . $this->esc($this->t('Total Size')) . ':</strong> '
            . $this->esc($this->formatBytes($before)) . ' &rarr; ' . $this->esc($this->formatBytes($after))
            . ' <span class="wpmgr-saved">(' . $this->esc((string) $saved) . '% ' . $this->esc($this->t('saved')) . ')</span></div>';
    }

    /**
     * @param array<string,string> $unoptimized
     * @param array<string,mixed>  $originalData
     * @return string
     */
    private function unoptimizedRows(array $unoptimized, array $originalData): string
    {
        $sizes = is_array($originalData['sizes'] ?? null) ? $originalData['sizes'] : [];

        $html = '<div class="wpmgr-sizes-unoptimized"><strong>' . $this->esc($this->t('Sizes not optimized')) . ':</strong></div>';
        foreach ($unoptimized as $size => $reason) {
            if ($size === 'full') {
                $label = $this->t('Full');
            } else {
                $meta   = is_array($sizes[$size] ?? null) ? $sizes[$size] : [];
                $width  = (int) ($meta['width'] ?? 0);
                $height = (int) ($meta['height'] ?? 0);
                $label  = $width . 'x' . $height;
            }
            $html .= '<div class="wpmgr-size-row"><code>' . $this->esc((string) $label) . '</code>: '
                . $this->esc((string) $reason) . '</div>';
        }

        return $html;
    }

    private function footer(): string
    {
        return '<div class="wpmgr-footer">' . $this->esc($this->t('Managed by WPMgr')) . '</div>';
    }

    // ------------------------------------------------------------------
    // data helpers
    // ------------------------------------------------------------------

    /**
     * Total bytes of an _wp_attachment_metadata-shaped array (full + each size).
     *
     * @param array<string,mixed> $metadata
     * @return int
     */
    private function totalBytes(array $metadata): int
    {
        if (empty($metadata['file'])) {
            return 0;
        }
        $total = (int) ($metadata['filesize'] ?? 0);
        $sizes = is_array($metadata['sizes'] ?? null) ? $metadata['sizes'] : [];
        foreach ($sizes as $meta) {
            if (is_array($meta)) {
                $total += (int) ($meta['filesize'] ?? 0);
            }
        }

        return $total;
    }

    /**
     * @param int $attachmentId
     * @return array<string,mixed>
     */
    private function liveMetadata(int $attachmentId): array
    {
        if (!function_exists('wp_get_attachment_metadata')) {
            return [];
        }
        $meta = wp_get_attachment_metadata($attachmentId);

        return is_array($meta) ? $meta : [];
    }

    /**
     * The attachment's ORIGINAL source mime (from the blob snapshot, else live).
     *
     * @param int    $attachmentId
     * @param string $liveMime
     * @return string
     */
    private function originalMime(int $attachmentId, string $liveMime): string
    {
        $blob = $this->keystore->get($attachmentId);
        $file = $blob['original_data']['file'] ?? '';
        if (is_string($file) && $file !== '') {
            $ext = strtolower((string) pathinfo($file, PATHINFO_EXTENSION));
            if ($ext === 'jpg' || $ext === 'jpeg') {
                return 'image/jpeg';
            }
            if ($ext !== '') {
                return 'image/' . $ext;
            }
        }
        if ($liveMime !== '') {
            return strtolower($liveMime);
        }
        if (function_exists('get_post_mime_type')) {
            $m = get_post_mime_type($attachmentId);

            return is_string($m) ? strtolower($m) : '';
        }

        return '';
    }

    // ------------------------------------------------------------------
    // escaping + i18n shims
    // ------------------------------------------------------------------

    private function esc(string $value): string
    {
        return function_exists('esc_html') ? esc_html($value) : htmlspecialchars($value, ENT_QUOTES, 'UTF-8');
    }

    private function t(string $text): string
    {
        return function_exists('__') ? (string) __($text, 'wpmgr-agent') : $text;
    }

    private function formatBytes(int $bytes): string
    {
        if (function_exists('size_format')) {
            $out = size_format($bytes, 1);
            if (is_string($out) && $out !== '') {
                return $out;
            }
        }
        $units = ['B', 'KB', 'MB', 'GB'];
        $i     = 0;
        $value = (float) $bytes;
        while ($value >= 1024 && $i < count($units) - 1) {
            $value /= 1024;
            $i++;
        }

        return round($value, 1) . ' ' . $units[$i];
    }
}
