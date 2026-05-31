<?php
/**
 * MediaOptimizeCommand: handles the CP's `media_optimize` command — for each
 * attachment, collect its optimizable variants, presign-PUT the SOURCE bytes to
 * media/<tenant>/<site>/<job>/src/<name>, then signal /agent/v1/media/encode-ready.
 * It applies NOTHING on disk — that happens later in `media_apply`.
 *
 * CP contract (apps/api/internal/agentcmd/media_contract.go MediaOptimizeRequest):
 *   POST /wp-json/wpmgr/v1/command/media_optimize
 *   body: { "job_ids":[...], "target_format", "target_quality",
 *           "presign_endpoint", "ready_endpoint" }
 *   resp: MediaOptimizeResponse { "ok", "detail" }
 *
 * Presign payload (presignBody / presignVariantDTO, agent_handler.go:63-72):
 *   { "job_id", "variants":[ {"name","source_size","source_mime"} ] }
 * The CP returns { "uploads": { name -> presigned PUT URL } }, the agent PUTs each
 * variant's source bytes, then POSTs encode-ready with the SAME variants list.
 *
 * Mapping job_ids -> attachments: each CP job is for ONE attachment
 * (media_contract.go "one job per attachment"). The CP threads the WP attachment
 * id per job either as the bare job id list OR as objects; the agent accepts both
 * the flat `job_ids: ["<jobId>"]` form AND a `jobs: [{job_id, wp_attachment_id}]`
 * form so the CP can disambiguate which attachment a job targets. When only
 * job_ids are given we treat each entry as `"<jobId>:<attachmentId>"` if it
 * carries a colon-suffixed id; otherwise the attachment must be supplied via
 * `jobs`. (See deviation note in the return summary.)
 *
 * Only image/jpeg|image/png SOURCES are optimizable (FlyingPress MIME_OPTIMIZABLE).
 * Variants per attachment are capped at MaxVariantsPerJob (10, domain.go:46).
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Media\AttachmentMeta;
use WPMgr\Agent\Media\MediaUploader;

/**
 * Uploads each attachment's source variants for cloud encoding.
 */
final class MediaOptimizeCommand implements CommandInterface
{
    /** Optimizable SOURCE mimes (FlyingPress MIME_OPTIMIZABLE). */
    private const OPTIMIZABLE_MIMES = ['image/jpeg', 'image/jpg', 'image/png'];

    /** Max variants per attachment/job (mirrors media.MaxVariantsPerJob). */
    private const MAX_VARIANTS = 10;

    private MediaUploader $uploader;

    private AttachmentMeta $meta;

    public function __construct(MediaUploader $uploader, ?AttachmentMeta $meta = null)
    {
        $this->uploader = $uploader;
        $this->meta     = $meta ?? new AttachmentMeta();
    }

    public function name(): string
    {
        return 'media_optimize';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims
     * @param array<string,mixed> $params MediaOptimizeRequest fields.
     * @return array{ok:bool,detail:string}
     */
    public function execute(array $claims, array $params): array
    {
        $presign = $this->str($params, 'presign_endpoint');
        $ready   = $this->str($params, 'ready_endpoint');
        if ($presign === '' || $ready === '') {
            return ['ok' => false, 'detail' => 'missing presign/ready endpoints'];
        }

        $jobs = $this->resolveJobs($params);
        if ($jobs === []) {
            return ['ok' => false, 'detail' => 'no resolvable jobs/attachments'];
        }

        $processed = 0;
        foreach ($jobs as $job) {
            $jobId        = $job['job_id'];
            $attachmentId = $job['wp_attachment_id'];
            if ($jobId === '' || $attachmentId <= 0) {
                continue;
            }

            $variants = $this->collectVariants($attachmentId);
            if ($variants === []) {
                continue;
            }

            try {
                $uploads = $this->uploader->presign($presign, $jobId, $this->variantDtos($variants));
            } catch (\Throwable $e) {
                return ['ok' => false, 'detail' => 'presign failed for job ' . $jobId];
            }

            $uploaded = [];
            foreach ($variants as $variant) {
                $name = $variant['name'];
                if (!isset($uploads[$name])) {
                    continue;
                }
                $bytes = @file_get_contents($variant['path']);
                if ($bytes === false) {
                    continue;
                }
                if ($this->uploader->putBytes($uploads[$name], $bytes)) {
                    $uploaded[] = $variant;
                }
            }

            if ($uploaded === []) {
                continue;
            }

            if (!$this->uploader->encodeReady($ready, $jobId, $this->variantDtos($uploaded))) {
                return ['ok' => false, 'detail' => 'encode-ready failed for job ' . $jobId];
            }
            $processed++;
        }

        return ['ok' => true, 'detail' => 'uploaded sources for ' . $processed . ' attachment(s)'];
    }

    /**
     * Collect the optimizable variants (full + registered sizes) for an
     * attachment: only those whose SOURCE file exists and is image/jpeg|png.
     * Capped at MAX_VARIANTS (full first, then sizes).
     *
     * @param int $attachmentId
     * @return list<array{name:string,path:string,source_size:int,source_mime:string}>
     */
    private function collectVariants(int $attachmentId): array
    {
        if (!function_exists('wp_get_attachment_metadata')) {
            return [];
        }
        $meta = wp_get_attachment_metadata($attachmentId);
        if (!is_array($meta) || empty($meta['file'])) {
            return [];
        }

        $names = array_merge(['full'], array_keys(is_array($meta['sizes'] ?? null) ? $meta['sizes'] : []));
        $out   = [];

        foreach ($names as $name) {
            if (count($out) >= self::MAX_VARIANTS) {
                break;
            }
            $url  = function_exists('wp_get_attachment_image_url')
                ? (string) wp_get_attachment_image_url($attachmentId, $name)
                : '';
            $path = $this->meta->urlToPath($url);
            if ($path === '' || !@is_file($path)) {
                continue;
            }
            $mime = $this->mimeOf($path);
            if (!in_array($mime, self::OPTIMIZABLE_MIMES, true)) {
                continue;
            }
            $out[] = [
                'name'        => (string) $name,
                'path'        => $path,
                'source_size' => (int) @filesize($path),
                'source_mime' => $mime,
            ];
        }

        return $out;
    }

    /**
     * Map internal variants to the presignVariantDTO wire shape.
     *
     * @param list<array{name:string,path:string,source_size:int,source_mime:string}> $variants
     * @return list<array{name:string,source_size:int,source_mime:string}>
     */
    private function variantDtos(array $variants): array
    {
        $dtos = [];
        foreach ($variants as $v) {
            $dtos[] = [
                'name'        => $v['name'],
                'source_size' => $v['source_size'],
                'source_mime' => $v['source_mime'],
            ];
        }

        return $dtos;
    }

    /**
     * Resolve the (job_id, wp_attachment_id) pairs from the command params.
     * Accepts `jobs:[{job_id,wp_attachment_id}]` (preferred) OR a flat
     * `job_ids:["<jobId>:<attachmentId>"]` form.
     *
     * @param array<string,mixed> $params
     * @return list<array{job_id:string,wp_attachment_id:int}>
     */
    private function resolveJobs(array $params): array
    {
        $out = [];

        if (isset($params['jobs']) && is_array($params['jobs'])) {
            foreach ($params['jobs'] as $job) {
                if (!is_array($job)) {
                    continue;
                }
                $jobId = isset($job['job_id']) && is_string($job['job_id']) ? $job['job_id'] : '';
                $att   = isset($job['wp_attachment_id']) && is_numeric($job['wp_attachment_id'])
                    ? (int) $job['wp_attachment_id']
                    : 0;
                if ($jobId !== '' && $att > 0) {
                    $out[] = ['job_id' => $jobId, 'wp_attachment_id' => $att];
                }
            }
        }

        if ($out === [] && isset($params['job_ids']) && is_array($params['job_ids'])) {
            foreach ($params['job_ids'] as $entry) {
                if (!is_string($entry) || $entry === '') {
                    continue;
                }
                if (strpos($entry, ':') !== false) {
                    [$jobId, $att] = explode(':', $entry, 2);
                    if ($jobId !== '' && is_numeric($att)) {
                        $out[] = ['job_id' => $jobId, 'wp_attachment_id' => (int) $att];
                    }
                }
            }
        }

        return $out;
    }

    /**
     * Best-effort source mime for a path.
     *
     * @param string $path
     * @return string
     */
    private function mimeOf(string $path): string
    {
        if (function_exists('wp_get_image_mime')) {
            $mime = wp_get_image_mime($path);
            if (is_string($mime) && $mime !== '') {
                return strtolower($mime);
            }
        }
        $ext = strtolower((string) pathinfo($path, PATHINFO_EXTENSION));
        $map = ['jpg' => 'image/jpeg', 'jpeg' => 'image/jpeg', 'png' => 'image/png', 'webp' => 'image/webp', 'avif' => 'image/avif'];

        return $map[$ext] ?? '';
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
}
