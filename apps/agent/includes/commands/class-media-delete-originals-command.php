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
 * Which file to delete, per the blob's per-variant archive_mode
 * (analysis doc lines 810-843):
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

use WPMgr\Agent\Keystore;
use WPMgr\Agent\Media\AttachmentMeta;
use WPMgr\Agent\Media\DiskWriter;
use WPMgr\Agent\Media\MediaRunStore;
use WPMgr\Agent\Media\MediaUploader;
use WPMgr\Agent\Media\Rename;
use WPMgr\Agent\MediaKeystore;
use WPMgr\Agent\Signer;

/**
 * Irreversibly deletes archived originals for optimized attachments.
 *
 * SCALE: bulk delete-originals has the identical synchronous-batch shape as
 * media_optimize/media_restore — a per-job deleteOne() + signed job-status
 * callback in a loop — so on a mega library it hits the SAME CP timeout. It ACKs
 * IMMEDIATELY ('accepted') after persisting the batch and drains it in bounded
 * background chunks via wp-cron (MediaRunStore), mirroring BackupCommand. The
 * wire contract and the job-status callbacks are UNCHANGED.
 */
final class MediaDeleteOriginalsCommand implements CommandInterface
{
    /** Cron hook the background worker is bound to in Plugin::registerHooks. */
    public const RUN_HOOK = 'wpmgr_media_delete_originals_run';

    private MediaUploader $uploader;

    private MediaKeystore $keystore;

    private Rename $rename;

    private DiskWriter $writer;

    private MediaRunStore $store;

    public function __construct(
        MediaUploader $uploader,
        ?MediaKeystore $keystore = null,
        ?Rename $rename = null,
        ?DiskWriter $writer = null,
        ?MediaRunStore $store = null
    ) {
        $this->uploader = $uploader;
        $this->keystore = $keystore ?? new MediaKeystore();
        $this->rename   = $rename ?? new Rename();
        $this->writer   = $writer ?? new DiskWriter();
        $this->store    = $store ?? new MediaRunStore();
    }

    public function name(): string
    {
        return 'media_delete_originals';
    }

    /**
     * Effect: permanently deletes the original image files left behind after optimization. Once gone,
     * media_restore can no longer put the attachment back.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Destructive;
    }

    /**
     * Repeatability: each delivery mints a new run id, schedules another event, and fires another status
     * callback even for attachments already carrying original_deleted=1. An original already deleted stays
     * deleted, so nothing extra is destroyed -- but the run is not free.
     *
     * @return CommandRepeatability
     */
    public function repeatability(): CommandRepeatability
    {
        return CommandRepeatability::Repeatable;
    }

    /**
     * {@inheritDoc}
     *
     * Persists the batch under a fresh run id, schedules the background worker,
     * and returns 'accepted' immediately. The irreversible per-attachment delete
     * happens in runBackground() under wp-cron.
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

        $runId   = MediaRunStore::newRunId();
        $payload = [
            'status_endpoint' => $statusEp,
            'jobs'            => $jobs,
        ];
        if (!$this->store->claim($runId, $payload)) {
            $this->runInline($payload);
            return ['ok' => true, 'detail' => 'processed inline (no background store)'];
        }

        $this->store->scheduleNext(self::RUN_HOOK, $runId);

        return ['ok' => true, 'detail' => 'accepted'];
    }

    /**
     * Cron callback (bound to RUN_HOOK). Drains one bounded chunk, reschedules
     * until empty.
     *
     * @param string $runId Run id passed via wp_schedule_single_event $args.
     * @return void
     */
    public static function runBackground(string $runId): void
    {
        if ($runId === '') {
            return;
        }
        $store = new MediaRunStore();
        if ($store->get($runId) === null) {
            return;
        }
        $command = self::fromRuntime();
        if ($command === null) {
            return;
        }
        $store->drain(
            self::RUN_HOOK,
            $runId,
            MediaRunStore::DEFAULT_CHUNK,
            static function (array $job, array $r) use ($command): void {
                $command->processJob($job, $r);
            }
        );
    }

    /**
     * Process a single job: deleteOne() + the signed job-status callback. Public
     * so the cron worker and unit tests can drive one job.
     *
     * @param array{job_id:string,wp_attachment_id:int} $job
     * @param array<string,mixed>                       $run Full run payload (status_endpoint).
     * @return bool True when the originals were deleted + the blob flipped.
     */
    public function processJob(array $job, array $run): bool
    {
        $statusEp = isset($run['status_endpoint']) && is_string($run['status_endpoint']) ? $run['status_endpoint'] : '';
        $jobId    = isset($job['job_id']) && is_string($job['job_id']) ? $job['job_id'] : '';
        $attachmentId = isset($job['wp_attachment_id']) && is_numeric($job['wp_attachment_id']) ? (int) $job['wp_attachment_id'] : 0;

        if ($statusEp === '' || $jobId === '' || $attachmentId <= 0) {
            return false;
        }

        $ok = $this->deleteOne($attachmentId);
        $this->reportStatus($statusEp, $jobId, $ok);

        return $ok;
    }

    /**
     * Build a command instance with a freshly-constructed signed MediaUploader.
     * Returns null when the crypto seam is unavailable (tests drive processJob
     * directly).
     */
    private static function fromRuntime(): ?self
    {
        if (!class_exists(Signer::class) || !class_exists(Keystore::class)) {
            return null;
        }
        try {
            return new self(new MediaUploader(new Signer(new Keystore())));
        } catch (\Throwable $e) {
            return null;
        }
    }

    /**
     * Synchronous fallback when no transient backend is available. Processes
     * every job inline (the old behaviour).
     *
     * @param array<string,mixed> $payload Run payload (status_endpoint + jobs).
     * @return void
     */
    private function runInline(array $payload): void
    {
        $jobs = isset($payload['jobs']) && is_array($payload['jobs']) ? $payload['jobs'] : [];
        foreach ($jobs as $job) {
            if (is_array($job)) {
                $this->processJob($job, $payload);
            }
        }
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

        foreach ($this->originalPathsFor($blob) as $path) {
            $this->writer->delete($path);
        }

        $this->keystore->markOriginalsDeleted($attachmentId);

        return true;
    }

    /**
     * Enumerate WPMgr's UNTRACKED deletable originals for an optimization blob.
     *
     * For each optimized_data[size] record with an absolute path P and an
     * archive_mode M:
     *   - MODE_REPLACE: the SAME-EXT archive sits beside P as
     *     Rename::archivePathFor(P) — the *.wpmgr-original.<ext> file.
     *   - MODE_COEXIST: the DIFFERENT-EXT twin shares P's basename with the
     *     ORIGINAL extension — Rename::changeExtension(P, originalExt) — and is
     *     only emitted when the original extension is non-empty.
     * The result is array_unique'd. WP itself owns the WP-TRACKED files (the
     * in-place optimized for REPLACE, the .avif for COEXIST); this list is ONLY
     * the files WPMgr created that WordPress does not know about.
     *
     * Shared by deleteOne() (the media_delete_originals command) AND
     * Plugin::onDeleteAttachment() (the delete_attachment hook) so the two paths
     * can never compute a different set of files to remove.
     *
     * @param array<string,mixed> $blob The wpmgr_image_optimization blob.
     * @return list<string> Absolute paths of WPMgr's untracked originals.
     */
    public function originalPathsFor(array $blob): array
    {
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

        return array_values(array_unique($paths));
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
