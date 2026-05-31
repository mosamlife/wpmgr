<?php
/**
 * MediaDeleteOriginalsCommand: handles the CP's `media_delete_originals`
 * command — IRREVERSIBLY delete the archived originals behind job_ids, then
 * report via /agent/v1/media/job-status.
 *
 * CP contract (apps/api/internal/agentcmd/media_contract.go
 * MediaDeleteOriginalsRequest):
 *   POST /wp-json/wpmgr/v1/command/media_delete_originals
 *   body: { "job_ids":[...], "status_endpoint" }
 *   resp: MediaDeleteOriginalsResponse { "ok", "detail" }
 *   reports back via job-status (jobStatusBody, agent_handler.go:79-91).
 *
 * Which file to delete, per the blob's per-variant archive_mode (FlyingPress
 * get_original_image_paths, ImageOptimizer.php:810-843):
 *   - MODE_REPLACE (same ext): delete the .wpmgr-original.<ext> ARCHIVE — the
 *     live optimized file at the original path stays.
 *   - MODE_COEXIST (different ext): delete the SAME-NAMED original (the .jpg
 *     twin) — the optimized .avif/.webp stays; the .htaccess fallback no longer
 *     has a twin (acceptable: originals were explicitly purged).
 * Then set blob original_deleted=1 (status -> originals_deleted): restore is now
 * impossible.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Media\AttachmentMeta;
use WPMgr\Agent\Media\DiskWriter;
use WPMgr\Agent\Media\MediaUploader;
use WPMgr\Agent\Media\Rename;
use WPMgr\Agent\MediaKeystore;

/**
 * Irreversibly deletes archived originals for optimized attachments.
 */
final class MediaDeleteOriginalsCommand implements CommandInterface
{
    private MediaUploader $uploader;

    private MediaKeystore $keystore;

    private Rename $rename;

    private DiskWriter $writer;

    public function __construct(
        MediaUploader $uploader,
        ?MediaKeystore $keystore = null,
        ?Rename $rename = null,
        ?DiskWriter $writer = null
    ) {
        $this->uploader = $uploader;
        $this->keystore = $keystore ?? new MediaKeystore();
        $this->rename   = $rename ?? new Rename();
        $this->writer   = $writer ?? new DiskWriter();
    }

    public function name(): string
    {
        return 'media_delete_originals';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims
     * @param array<string,mixed> $params MediaDeleteOriginalsRequest fields.
     * @return array{ok:bool,detail:string}
     */
    public function execute(array $claims, array $params): array
    {
        $statusEp = $this->str($params, 'status_endpoint');
        if ($statusEp === '') {
            return ['ok' => false, 'detail' => 'missing status_endpoint'];
        }

        $jobs = $this->resolveJobs($params);
        if ($jobs === []) {
            return ['ok' => false, 'detail' => 'no resolvable jobs/attachments'];
        }

        $deleted = 0;
        foreach ($jobs as $job) {
            $jobId        = $job['job_id'];
            $attachmentId = $job['wp_attachment_id'];
            if ($jobId === '' || $attachmentId <= 0) {
                continue;
            }

            $ok = $this->deleteOne($attachmentId);
            $this->reportStatus($statusEp, $jobId, $ok);
            if ($ok) {
                $deleted++;
            }
        }

        return ['ok' => true, 'detail' => 'deleted originals for ' . $deleted . ' attachment(s)'];
    }

    /**
     * Delete the archived/twin originals for one attachment and flip the blob.
     * Public + returning a bool so it is directly unit-testable.
     *
     * @param int $attachmentId
     * @return bool True when a blob existed and was flipped.
     */
    public function deleteOne(int $attachmentId): bool
    {
        $blob = $this->keystore->get($attachmentId);
        if ($blob === [] || (int) ($blob['original_deleted'] ?? 0) === 1) {
            return false;
        }

        $optimizedData = is_array($blob['optimized_data'] ?? null) ? $blob['optimized_data'] : [];
        $originalExt   = strtolower((string) pathinfo(
            (string) ($blob['original_data']['file'] ?? ''),
            PATHINFO_EXTENSION
        ));

        $paths = [];
        foreach ($optimizedData as $record) {
            if (!is_array($record)) {
                continue;
            }
            $optimizedPath = isset($record['path']) ? (string) $record['path'] : '';
            $mode          = isset($record['archive_mode']) ? (string) $record['archive_mode'] : AttachmentMeta::MODE_COEXIST;
            if ($optimizedPath === '') {
                continue;
            }

            if ($mode === AttachmentMeta::MODE_REPLACE) {
                // Same ext: the archive is .wpmgr-original.<ext> next to the path.
                $paths[] = $this->rename->archivePathFor($optimizedPath);
            } else {
                // Different ext: the original twin shares the basename with the
                // original extension (e.g. banner.jpg next to banner.avif).
                if ($originalExt !== '') {
                    $paths[] = $this->rename->changeExtension($optimizedPath, $originalExt);
                }
            }
        }

        foreach (array_unique($paths) as $path) {
            $this->writer->delete($path);
        }

        $this->keystore->markOriginalsDeleted($attachmentId);

        return true;
    }

    /**
     * Report a delete result via job-status.
     *
     * @param string $statusEp
     * @param string $jobId
     * @param bool   $ok
     * @return void
     */
    private function reportStatus(string $statusEp, string $jobId, bool $ok): void
    {
        $this->uploader->jobStatus($statusEp, [
            'job_id'             => $jobId,
            'applied_variants'   => [],
            'sizes_unoptimized'  => [],
            'current_format'     => '',
            'current_size_bytes' => 0,
            'bytes_before'       => null,
            'bytes_after'        => null,
            'compression_level'  => '',
            'target_format'      => '',
            'rewrite_stats'      => ['post_content_rows' => 0, 'postmeta_rows' => 0],
            'error'              => $ok ? '' : 'originals_already_deleted_or_no_blob',
        ]);
    }

    /**
     * Resolve (job_id, wp_attachment_id) pairs (same form as the other commands).
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
                $att   = isset($job['wp_attachment_id']) && is_numeric($job['wp_attachment_id']) ? (int) $job['wp_attachment_id'] : 0;
                if ($jobId !== '' && $att > 0) {
                    $out[] = ['job_id' => $jobId, 'wp_attachment_id' => $att];
                }
            }
        }
        if ($out === [] && isset($params['job_ids']) && is_array($params['job_ids'])) {
            foreach ($params['job_ids'] as $entry) {
                if (is_string($entry) && strpos($entry, ':') !== false) {
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
     * @param array<string,mixed> $params
     * @param string              $key
     * @return string
     */
    private function str(array $params, string $key): string
    {
        return isset($params[$key]) && is_string($params[$key]) ? $params[$key] : '';
    }
}
