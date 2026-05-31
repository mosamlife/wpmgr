<?php
/**
 * MediaRestoreCommand: handles the CP's `media_restore` command — revert each
 * attachment behind job_ids to its pre-optimization state using the
 * wpmgr_image_optimization blob, then POST /agent/v1/media/restore-status.
 *
 * CP contract (apps/api/internal/agentcmd/media_contract.go MediaRestoreRequest):
 *   POST /wp-json/wpmgr/v1/command/media_restore
 *   body: { "job_ids":[...], "status_endpoint" }
 *   resp: MediaRestoreResponse { "ok", "detail" }
 *
 * restore-status payload (restoreStatusBody, agent_handler.go:93-97):
 *   { "job_id", "restored":bool, "error":string }
 *
 * Restore decision logic (analysis/media-postmeta-blob.md "Restore decision
 * logic" + FlyingPress ImageOptimizer.php:164-226), per optimized size, keyed
 * off the per-variant archive_mode recorded at apply time:
 *   - MODE_REPLACE (same ext): delete the optimized file at the original path,
 *     then Rename::restore() the .wpmgr-original.<ext> archive back. No URL change.
 *   - MODE_COEXIST (different ext): delete the optimized .avif/.webp file; the
 *     original .jpg was never touched and is already in place. Reverse the URL
 *     replacement so the DB points back at the .jpg.
 * ORDER MATTERS: delete optimized files BEFORE un-renaming archives so no window
 * has two files claiming one URL (ImageOptimizer.php:203-204).
 *
 * If original_deleted==1 the restore is REFUSED (restore is impossible —
 * archives are gone). Always restore _wp_attachment_metadata from original_data,
 * reverse the URL rewrite, then delete/reduce the blob (lifecycle shape #2).
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Media\AttachmentMeta;
use WPMgr\Agent\Media\DbRewriter;
use WPMgr\Agent\Media\DiskWriter;
use WPMgr\Agent\Media\MediaUploader;
use WPMgr\Agent\Media\Rename;
use WPMgr\Agent\MediaKeystore;

/**
 * Reverses an optimization back to the pre-optimize on-disk + DB state.
 */
final class MediaRestoreCommand implements CommandInterface
{
    private MediaUploader $uploader;

    private MediaKeystore $keystore;

    private AttachmentMeta $meta;

    private DbRewriter $rewriter;

    private Rename $rename;

    private DiskWriter $writer;

    public function __construct(
        MediaUploader $uploader,
        ?MediaKeystore $keystore = null,
        ?AttachmentMeta $meta = null,
        ?DbRewriter $rewriter = null,
        ?Rename $rename = null,
        ?DiskWriter $writer = null
    ) {
        $this->uploader = $uploader;
        $this->keystore = $keystore ?? new MediaKeystore();
        $this->meta     = $meta ?? new AttachmentMeta();
        $this->rewriter = $rewriter ?? new DbRewriter();
        $this->rename   = $rename ?? new Rename();
        $this->writer   = $writer ?? new DiskWriter();
    }

    public function name(): string
    {
        return 'media_restore';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims
     * @param array<string,mixed> $params MediaRestoreRequest fields.
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

        $restored = 0;
        foreach ($jobs as $job) {
            $jobId        = $job['job_id'];
            $attachmentId = $job['wp_attachment_id'];
            if ($jobId === '' || $attachmentId <= 0) {
                continue;
            }

            $result = $this->restoreOne($attachmentId);
            $this->uploader->restoreStatus($statusEp, $jobId, $result['ok'], $result['error']);
            if ($result['ok']) {
                $restored++;
            }
        }

        return ['ok' => true, 'detail' => 'restored ' . $restored . ' attachment(s)'];
    }

    /**
     * Restore a single attachment from its blob. Public + returning a structured
     * result so the round-trip test can drive it directly.
     *
     * @param int $attachmentId
     * @return array{ok:bool,error:string}
     */
    public function restoreOne(int $attachmentId): array
    {
        $blob = $this->keystore->get($attachmentId);
        if ($blob === []) {
            return ['ok' => true, 'error' => '']; // Nothing to restore — idempotent success.
        }
        if ((int) ($blob['original_deleted'] ?? 0) === 1) {
            return ['ok' => false, 'error' => 'originals_deleted_cannot_restore'];
        }

        $originalData  = is_array($blob['original_data'] ?? null) ? $blob['original_data'] : [];
        $optimizedData = is_array($blob['optimized_data'] ?? null) ? $blob['optimized_data'] : [];
        $replacements  = is_array($blob['replacements'] ?? null) ? $blob['replacements'] : [];

        $optimizedFiles = [];
        $archives       = [];

        foreach ($optimizedData as $sizeName => $record) {
            if (!is_array($record)) {
                continue;
            }
            $optimizedPath = isset($record['path']) ? (string) $record['path'] : '';
            $mode          = isset($record['archive_mode']) ? (string) $record['archive_mode'] : AttachmentMeta::MODE_COEXIST;

            if ($optimizedPath !== '') {
                // Always queue the optimized file for deletion.
                $optimizedFiles[] = $optimizedPath;
            }

            if ($mode === AttachmentMeta::MODE_REPLACE) {
                // Same-ext: the original was archived to .wpmgr-original.<ext>;
                // un-rename it back to the original path (which == optimizedPath
                // here, since the optimized bytes overwrote the original path).
                $archives[] = $this->rename->archivePathFor($optimizedPath);
            }
        }

        // ORDER: delete optimized files FIRST, then un-rename archives, so no
        // window has two files claiming one URL.
        foreach (array_unique($optimizedFiles) as $file) {
            $this->writer->delete($file);
        }
        foreach (array_unique($archives) as $archive) {
            $this->rename->restore($archive);
        }

        // Restore the live metadata from the snapshot.
        $originalFullUrl = $this->originalFullUrl($attachmentId, $originalData);
        $this->meta->restoreMetadata($attachmentId, $originalData, $originalFullUrl);

        // Reverse the URL rewrite (different-ext variants only).
        if ($replacements !== []) {
            $this->rewriter->reverseImages($this->stringMap($replacements));
        }

        // Delete or reduce the blob (lifecycle shape #2).
        $compression = isset($blob['compression_level']) ? (string) $blob['compression_level'] : '';
        $unoptimized = is_array($blob['sizes_unoptimized'] ?? null) ? $blob['sizes_unoptimized'] : [];
        $this->keystore->reduceAfterRestore($attachmentId, $compression, $this->stringMap($unoptimized));

        return ['ok' => true, 'error' => ''];
    }

    /**
     * The original full-size URL (for the guid restore). Derived from the
     * snapshot's relative `file` via the uploads base URL.
     *
     * @param int                 $attachmentId
     * @param array<string,mixed> $originalData
     * @return string
     */
    private function originalFullUrl(int $attachmentId, array $originalData): string
    {
        $file = isset($originalData['file']) ? (string) $originalData['file'] : '';
        if ($file === '' || !function_exists('wp_get_upload_dir')) {
            return '';
        }
        $uploads = wp_get_upload_dir();
        if (!is_array($uploads) || empty($uploads['baseurl'])) {
            return '';
        }

        return rtrim((string) $uploads['baseurl'], '/') . '/' . ltrim($file, '/');
    }

    /**
     * Coerce a mixed map to array<string,string>.
     *
     * @param array<mixed,mixed> $map
     * @return array<string,string>
     */
    private function stringMap(array $map): array
    {
        $out = [];
        foreach ($map as $k => $v) {
            if (is_string($k) && is_string($v)) {
                $out[$k] = $v;
            }
        }

        return $out;
    }

    /**
     * Resolve (job_id, wp_attachment_id) pairs. Mirrors the optimize command:
     * `jobs:[{job_id,wp_attachment_id}]` OR `job_ids:["<jobId>:<attachmentId>"]`.
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
