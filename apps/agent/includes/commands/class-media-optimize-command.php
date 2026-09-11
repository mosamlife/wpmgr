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
 * Only image/jpeg|image/png SOURCES are optimizable (optimizable MIME list).
 * Variants per attachment are capped at MaxVariantsPerJob (10, domain.go:46).
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Keystore;
use WPMgr\Agent\Media\AttachmentMeta;
use WPMgr\Agent\Media\MediaUploader;
use WPMgr\Agent\Media\MediaRunStore;
use WPMgr\Agent\Signer;

/**
 * Uploads each attachment's source variants for cloud encoding.
 *
 * SCALE: the per-job work (enumerate sizes -> CP /presign -> PUT source bytes to
 * S3 -> /encode-ready) is far too slow to run synchronously inside the CP->agent
 * REST request for a non-trivial batch — the CP's HTTP client times out and marks
 * every job failed even though the agent keeps working. So this command ACKs
 * IMMEDIATELY ('accepted') after persisting the batch, then drains it in bounded
 * background chunks via wp-cron, mirroring BackupCommand (ADR-033). See
 * MediaRunStore for the orchestration. The CP wire contract is UNCHANGED: the
 * command still returns {ok, detail}; encode-ready callbacks are unchanged.
 */
final class MediaOptimizeCommand implements CommandInterface
{
    /** Cron hook the background worker is bound to in Plugin::registerHooks. */
    public const RUN_HOOK = 'wpmgr_media_optimize_run';

    /** Optimizable SOURCE mimes (JPEG, PNG, GIF). */
    private const OPTIMIZABLE_MIMES = ['image/jpeg', 'image/jpg', 'image/png', 'image/gif'];

    /** Max variants per attachment/job (mirrors media.MaxVariantsPerJob). */
    private const MAX_VARIANTS = 10;

    private MediaUploader $uploader;

    private AttachmentMeta $meta;

    private MediaRunStore $store;

    public function __construct(MediaUploader $uploader, ?AttachmentMeta $meta = null, ?MediaRunStore $store = null)
    {
        $this->uploader = $uploader;
        $this->meta     = $meta ?? new AttachmentMeta();
        $this->store    = $store ?? new MediaRunStore();
    }

    public function name(): string
    {
        return 'media_optimize';
    }

    /**
     * Effect: claims a batch under a fresh run id and encodes optimized variants in the background.
     * Originals are kept for media_restore.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Write;
    }

    /**
     * Repeatability: its own code notes this: a retry mints a NEW run id and per-attachment encode-ready is
     * deduped control-plane side, so the only cost is repeated encoding.
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
     * Validates the batch, persists it under a fresh run id, schedules the
     * background worker, and returns 'accepted' in milliseconds — BEFORE any
     * presign/upload. The heavy work happens in runBackground() under wp-cron.
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

        // Persist the batch under a fresh run id and hand off to the background
        // worker. The run id is freshly minted here (never CP-supplied), so each
        // delivery is its own run; a CP retry of a LOST ack would re-enter
        // execute() with the SAME params but mint a NEW run id — which is still
        // safe because per-attachment encode-ready is idempotent on the CP side
        // (one job per attachment), and the bounded chunked worker means neither
        // run can wedge a request.
        $runId = MediaRunStore::newRunId();
        $payload = [
            'presign_endpoint' => $presign,
            'ready_endpoint'   => $ready,
            'jobs'             => $jobs,
        ];
        if (!$this->store->claim($runId, $payload)) {
            // Could not persist (no transient backend / id collision). Fall back
            // to running inline so we don't silently drop the batch on a host
            // without a working transient store.
            $this->runInline($payload);
            return ['ok' => true, 'detail' => 'processed inline (no background store)'];
        }

        $this->store->scheduleNext(self::RUN_HOOK, $runId);

        return ['ok' => true, 'detail' => 'accepted'];
    }

    /**
     * Cron callback (bound to RUN_HOOK in Plugin::registerHooks). Drains one
     * bounded chunk of the run, then reschedules itself until the batch is
     * empty. Static so the cron binding needs no live instance — it rebuilds the
     * collaborators from the plugin signer the same way the REST dispatch does.
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
        $run   = $store->get($runId);
        if ($run === null) {
            return; // Already drained / never existed — idempotent no-op.
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
     * Build a command instance with a freshly-constructed signed MediaUploader,
     * mirroring how RestoreRunner rebuilds its BackupTransport inside the cron
     * worker (new Signer(new Keystore())). Returns null when the crypto seam is
     * unavailable (e.g. unit tests, which drive processJob() directly instead).
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
     * Process a SINGLE job: enumerate variants, presign, PUT source bytes, signal
     * encode-ready. Public so the cron worker (runBackground) and unit tests can
     * drive one job without the chunking machinery. Per-job network bytes are
     * freed as we go (never hold all sources in memory) so a mega batch can't OOM.
     *
     * @param array{job_id:string,wp_attachment_id:int} $job
     * @param array<string,mixed>                       $run Full run payload (endpoints).
     * @return bool True when the source uploads + encode-ready succeeded.
     */
    public function processJob(array $job, array $run): bool
    {
        $presign = isset($run['presign_endpoint']) && is_string($run['presign_endpoint']) ? $run['presign_endpoint'] : '';
        $ready   = isset($run['ready_endpoint']) && is_string($run['ready_endpoint']) ? $run['ready_endpoint'] : '';
        $jobId   = isset($job['job_id']) && is_string($job['job_id']) ? $job['job_id'] : '';
        $attachmentId = isset($job['wp_attachment_id']) && is_numeric($job['wp_attachment_id']) ? (int) $job['wp_attachment_id'] : 0;

        if ($presign === '' || $ready === '' || $jobId === '' || $attachmentId <= 0) {
            return false;
        }

        $variants = $this->collectVariants($attachmentId);
        if ($variants === []) {
            return false;
        }

        try {
            $uploads = $this->uploader->presign($presign, $jobId, $this->variantDtos($variants));
        } catch (\Throwable $e) {
            // A presign failure for ONE job must not abort the rest of the batch
            // (the drain loop catches throws too, but returning cleanly here is
            // tidier and lets the loop continue without logging a throw).
            return false;
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
            $ok = $this->uploader->putBytes($uploads[$name], $bytes);
            // Free the source bytes immediately — never hold all sources in
            // memory across a job, let alone across the batch.
            unset($bytes);
            if ($ok) {
                $uploaded[] = $variant;
            }
        }

        if ($uploaded === []) {
            return false;
        }

        return $this->uploader->encodeReady($ready, $jobId, $this->variantDtos($uploaded));
    }

    /**
     * Synchronous fallback when no transient backend is available to persist the
     * run. Processes every job inline (the old behaviour). Only reached on hosts
     * without a working transient store; the bounded background path is the norm.
     *
     * @param array<string,mixed> $payload Run payload (endpoints + jobs).
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
