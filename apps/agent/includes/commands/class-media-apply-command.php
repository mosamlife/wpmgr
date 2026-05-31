<?php
/**
 * MediaApplyCommand: handles the CP's `media_apply` command — the encoder
 * finished an attachment's variants; the agent downloads each out/<variant> via
 * its presigned GET URL, applies it on disk (same-ext/different-ext rules ->
 * DiskWriter atomic write), updates _wp_attachment_metadata, writes the
 * wpmgr_image_optimization blob, runs the DB URL rewrite, then POSTs
 * /agent/v1/media/job-status.
 *
 * CP contract (apps/api/internal/agentcmd/media_contract.go MediaApplyRequest):
 *   POST /wp-json/wpmgr/v1/command/media_apply
 *   body: { "job_id", "target_format", "target_quality", "status_endpoint",
 *           "variants":[ {"name","get_url","optimized_mime","optimized_size"} ] }
 *   resp: MediaApplyResponse { "ok", "detail" }
 *
 * job-status payload (jobStatusBody, agent_handler.go:79-91):
 *   { "job_id", "applied_variants":[...], "sizes_unoptimized":{...},
 *     "current_format", "current_size_bytes", "bytes_before", "bytes_after",
 *     "compression_level", "target_format", "rewrite_stats":{...}, "error" }
 *
 * The CORRECTNESS TRAP (same-ext vs different-ext) is enforced in
 * AttachmentMeta::applyVariant: target_format='original' (same ext as source) =>
 * archive the original FIRST then write in place (no URL change); avif/webp
 * (different ext) => write the new file, leave the original as the .htaccess
 * fallback, and record old_url=>new_url for the DB rewrite. The attachment id is
 * resolved exactly as in MediaOptimizeCommand (jobs[]/job_id:attachment form).
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Media\AttachmentMeta;
use WPMgr\Agent\Media\DbRewriter;
use WPMgr\Agent\Media\HtaccessInstaller;
use WPMgr\Agent\Media\MediaUploader;
use WPMgr\Agent\MediaKeystore;

/**
 * Downloads optimized outputs and applies them on disk + in the DB.
 */
final class MediaApplyCommand implements CommandInterface
{
    private MediaUploader $uploader;

    private AttachmentMeta $meta;

    private DbRewriter $rewriter;

    private MediaKeystore $keystore;

    private ?HtaccessInstaller $htaccess;

    public function __construct(
        MediaUploader $uploader,
        ?AttachmentMeta $meta = null,
        ?DbRewriter $rewriter = null,
        ?MediaKeystore $keystore = null,
        ?HtaccessInstaller $htaccess = null
    ) {
        $this->uploader = $uploader;
        $this->meta     = $meta ?? new AttachmentMeta();
        $this->rewriter = $rewriter ?? new DbRewriter();
        $this->keystore = $keystore ?? new MediaKeystore();
        $this->htaccess = $htaccess; // null in unit tests; real one wired in Plugin.
    }

    public function name(): string
    {
        return 'media_apply';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims
     * @param array<string,mixed> $params MediaApplyRequest fields.
     * @return array{ok:bool,detail:string}
     */
    public function execute(array $claims, array $params): array
    {
        $jobId        = $this->str($params, 'job_id');
        $statusEp     = $this->str($params, 'status_endpoint');
        $targetFormat = $this->str($params, 'target_format');
        $quality      = $this->str($params, 'target_quality');
        $attachmentId = $this->resolveAttachmentId($params, $jobId);

        if ($jobId === '' || $statusEp === '') {
            return ['ok' => false, 'detail' => 'missing job_id/status_endpoint'];
        }
        if ($attachmentId <= 0) {
            $this->reportError($statusEp, $jobId, $targetFormat, $quality, 'unresolved attachment');

            return ['ok' => false, 'detail' => 'unresolved attachment'];
        }

        $variants = $this->resolveVariants($params);
        if ($variants === []) {
            $this->reportError($statusEp, $jobId, $targetFormat, $quality, 'no variants');

            return ['ok' => false, 'detail' => 'no variants'];
        }

        // Snapshot the pre-optimization metadata FIRST — the restore bible.
        $originalData = $this->meta->snapshotMetadata($attachmentId);
        if ($originalData === [] || empty($originalData['file'])) {
            $this->reportError($statusEp, $jobId, $targetFormat, $quality, 'missing source metadata');

            return ['ok' => false, 'detail' => 'missing source metadata'];
        }
        $bytesBefore = $this->totalBytes($originalData);

        $optimizedData    = [];
        $sizesUnoptimized = [];
        $replacements     = [];
        $appliedVariants  = [];

        foreach ($variants as $variant) {
            $name = $variant['name'];

            // Resolve this size's SOURCE url/path from the (still-original) meta.
            $sourceUrl = function_exists('wp_get_attachment_image_url')
                ? (string) wp_get_attachment_image_url($attachmentId, $name)
                : '';
            $sourcePath = $this->meta->urlToPath($sourceUrl);
            if ($sourcePath === '' || $sourceUrl === '') {
                $sizesUnoptimized[$name] = 'Source path unresolved';
                continue;
            }

            $bytes = $this->uploader->getBytes($variant['get_url']);
            if ($bytes === null || $bytes === '') {
                $sizesUnoptimized[$name] = 'Optimized output unavailable';
                continue;
            }

            // Verify the downloaded output matches the CP's contracted size +
            // format BEFORE trusting it on disk (ADR-043 §14). The bytes come from
            // a CP-minted presigned GET, but a truncated transfer or a format/size
            // mismatch must never be written over (or beside) the original. On
            // mismatch, record the size as unoptimized and skip it.
            $expectSize = isset($variant['optimized_size']) ? (int) $variant['optimized_size'] : 0;
            if ($expectSize > 0 && strlen($bytes) !== $expectSize) {
                $sizesUnoptimized[$name] = 'Optimized output size mismatch';
                continue;
            }
            if (!self::bytesMatchMime($bytes, (string) ($variant['optimized_mime'] ?? ''))) {
                $sizesUnoptimized[$name] = 'Optimized output format mismatch';
                continue;
            }

            $applied = $this->meta->applyVariant($sourceUrl, $sourcePath, $bytes, $variant['optimized_mime']);
            if (!$applied['ok']) {
                $sizesUnoptimized[$name] = 'Disk write failed';
                continue;
            }

            $optimizedData[$name] = $applied['record'];
            $appliedVariants[]    = $name;
            if ($applied['replacement'] !== null) {
                $replacements[$applied['replacement']['from']] = $applied['replacement']['to'];
            }
        }

        if ($appliedVariants === []) {
            $this->reportError($statusEp, $jobId, $targetFormat, $quality, 'no variants applied');

            return ['ok' => false, 'detail' => 'no variants applied'];
        }

        // Update the live _wp_attachment_metadata to point at the optimized files.
        $newMeta     = $this->meta->applyOptimizedMetadata($attachmentId, $originalData, $optimizedData);
        $bytesAfter  = $this->totalBytes($newMeta);

        // Persist the blob (the restore bible).
        $generation = (int) ($this->keystore->get($attachmentId)['wpmgr_generation'] ?? 0) + 1;
        $blob       = $this->meta->composeBlob([
            'job_id'            => $jobId,
            'generation'        => $generation,
            'compression_level' => $quality !== '' ? $quality : 'lossy',
            'target_format'     => $targetFormat !== '' ? $targetFormat : 'original',
            'sizes_optimized'   => $appliedVariants,
            'sizes_unoptimized' => $sizesUnoptimized,
            'original_data'     => $originalData,
            'optimized_data'    => $optimizedData,
            'replacements'      => $replacements,
        ]);
        $this->meta->saveBlob($attachmentId, $blob);

        // Rewrite DB URLs (different-ext variants only — same-ext produces none).
        $rewriteStats = ['post_content_rows' => 0, 'postmeta_rows' => 0];
        if ($replacements !== []) {
            $stats        = $this->rewriter->replaceImages($replacements);
            $rewriteStats = [
                'post_content_rows' => $stats['post_content_rows'],
                'postmeta_rows'     => $stats['postmeta_rows'],
                'needs_more'        => $stats['needs_more'],
            ];
            // Different-ext (coexist) variants rely on the .htaccess Accept
            // fallback — ensure the block exists on first such apply.
            $this->ensureHtaccess();
        }

        // Report to the CP.
        $currentFormat = $this->currentFormat($optimizedData);
        $this->uploader->jobStatus($statusEp, [
            'job_id'             => $jobId,
            'applied_variants'   => $appliedVariants,
            'sizes_unoptimized'  => $sizesUnoptimized,
            'current_format'     => $currentFormat,
            'current_size_bytes' => $bytesAfter,
            'bytes_before'       => $bytesBefore,
            'bytes_after'        => $bytesAfter,
            'compression_level'  => $quality !== '' ? $quality : 'lossy',
            'target_format'      => $targetFormat !== '' ? $targetFormat : 'original',
            'rewrite_stats'      => $rewriteStats,
            'error'              => '',
        ]);

        return ['ok' => true, 'detail' => 'applied ' . count($appliedVariants) . ' variant(s)'];
    }

    /**
     * Parse the MediaApplyVariant list from the request.
     *
     * @param array<string,mixed> $params
     * @return list<array{name:string,get_url:string,optimized_mime:string,optimized_size:int}>
     */
    private function resolveVariants(array $params): array
    {
        if (!isset($params['variants']) || !is_array($params['variants'])) {
            return [];
        }
        $out = [];
        foreach ($params['variants'] as $v) {
            if (!is_array($v)) {
                continue;
            }
            $name   = isset($v['name']) && is_string($v['name']) ? $v['name'] : '';
            $getUrl = isset($v['get_url']) && is_string($v['get_url']) ? $v['get_url'] : '';
            $mime   = isset($v['optimized_mime']) && is_string($v['optimized_mime']) ? $v['optimized_mime'] : '';
            if ($name === '' || $getUrl === '' || $mime === '') {
                continue;
            }
            $out[] = [
                'name'           => $name,
                'get_url'        => $getUrl,
                'optimized_mime' => $mime,
                'optimized_size' => isset($v['optimized_size']) && is_numeric($v['optimized_size']) ? (int) $v['optimized_size'] : 0,
            ];
        }

        return $out;
    }

    /**
     * Resolve the WP attachment id. Prefers an explicit `wp_attachment_id`,
     * falls back to a `<jobId>:<attachmentId>` suffix on the job id.
     *
     * @param array<string,mixed> $params
     * @param string              $jobId
     * @return int
     */
    private function resolveAttachmentId(array $params, string $jobId): int
    {
        if (isset($params['wp_attachment_id']) && is_numeric($params['wp_attachment_id'])) {
            return (int) $params['wp_attachment_id'];
        }
        if (strpos($jobId, ':') !== false) {
            [, $att] = explode(':', $jobId, 2);
            if (is_numeric($att)) {
                return (int) $att;
            }
        }

        return 0;
    }

    /**
     * The dominant output format (from the full variant, else the first).
     *
     * @param array<string,array<string,mixed>> $optimizedData
     * @return string
     */
    private function currentFormat(array $optimizedData): string
    {
        $rec = $optimizedData['full'] ?? (reset($optimizedData) ?: []);
        $mime = is_array($rec) && isset($rec['mime_type']) ? (string) $rec['mime_type'] : '';

        return $this->meta->mimeToExt($mime) ?: $mime;
    }

    /**
     * Total bytes of an _wp_attachment_metadata-shaped array.
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
     * Ensure the .htaccess Accept-fallback block is installed (idempotent).
     *
     * @return void
     */
    private function ensureHtaccess(): void
    {
        try {
            ($this->htaccess ?? new HtaccessInstaller())->install();
        } catch (\Throwable $e) {
            // Best-effort — a non-writable .htaccess never fails the apply.
        }
    }

    /**
     * Report a hard error to the CP via job-status.
     *
     * @param string $statusEp
     * @param string $jobId
     * @param string $targetFormat
     * @param string $quality
     * @param string $error
     * @return void
     */
    private function reportError(string $statusEp, string $jobId, string $targetFormat, string $quality, string $error): void
    {
        $this->uploader->jobStatus($statusEp, [
            'job_id'             => $jobId,
            'applied_variants'   => [],
            'sizes_unoptimized'  => [],
            'current_format'     => '',
            'current_size_bytes' => 0,
            'bytes_before'       => null,
            'bytes_after'        => null,
            'compression_level'  => $quality !== '' ? $quality : 'lossy',
            'target_format'      => $targetFormat !== '' ? $targetFormat : 'original',
            'rewrite_stats'      => ['post_content_rows' => 0, 'postmeta_rows' => 0],
            'error'              => $error,
        ]);
    }

    /**
     * @param array<string,mixed> $params
     * @param string              $key
     * @return string
     */
    private function str(array $params, string $key): string
    {
        return isset($params[$key]) && is_string($params[$key]) ? $params[$key] : '';
    }

    /**
     * Magic-byte check that $bytes match the claimed $mime. Defends against a
     * truncated/garbage optimized output being written to disk before we trust
     * the CP-supplied mime (ADR-043 §14). An unknown/empty mime passes (no
     * assertion); too-short input fails.
     *
     * @param string $bytes Downloaded optimized bytes.
     * @param string $mime  The contracted optimized mime.
     * @return bool
     */
    private static function bytesMatchMime(string $bytes, string $mime): bool
    {
        if (strlen($bytes) < 12) {
            return false;
        }
        switch ($mime) {
            case 'image/jpeg':
                return substr($bytes, 0, 3) === "\xFF\xD8\xFF";
            case 'image/png':
                return substr($bytes, 0, 8) === "\x89PNG\r\n\x1A\n";
            case 'image/webp':
                return substr($bytes, 0, 4) === 'RIFF' && substr($bytes, 8, 4) === 'WEBP';
            case 'image/avif':
                // ISO-BMFF: a 'ftyp' box at offset 4 with an 'avif'/'avis' brand.
                return substr($bytes, 4, 4) === 'ftyp'
                    && strpos(substr($bytes, 8, 16), 'avif') !== false;
            default:
                return true; // unknown/empty mime — nothing to assert
        }
    }
}
