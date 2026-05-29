<?php
/**
 * RestoreRunner: M5.6 / ADR-034 — state-machine driver that turns a row in
 * `wpmgr_restore_tasks` into a completed restore.
 *
 * Mirror of the backup-side TaskRunner. Both entry points (the cron-dispatch
 * `wpmgr_restore_run` and the watchdog `wpmgr_restore_watchdog`) eventually
 * call `RestoreRunner::run()`, which reads `phase` + `sub_state` from the row
 * and resumes from wherever the last invocation left off.
 *
 * Phase transitions — closed set lifted from the dossier
 * (`docs/research/wpvivid-restore-deep-dive.md` §7.3) with V0 simplifications
 * called out at the bottom:
 *
 *   preflight            -> download_artifacts        (always)
 *   download_artifacts   -> verify_artifacts          (always)
 *   verify_artifacts     -> maintenance_on            (always)
 *   maintenance_on       -> stage_files               (kind in {files, full})
 *                       -> restore_db                 (kind == db)
 *   stage_files          -> swap_files                (kind in {files, full})
 *   swap_files           -> restore_db                (kind == full)
 *                       -> post_hooks                 (kind == files)
 *   restore_db           -> swap_db                   (kind in {db, full})
 *   swap_db              -> post_hooks                (always)
 *   post_hooks           -> maintenance_off
 *   maintenance_off      -> cleanup
 *   cleanup              -> completed
 *
 *   completed | failed: terminal (re-entry is a no-op)
 *
 * V0 deviations from the dossier's full enum:
 *   - migrate_db (search-and-replace) is deferred. V0 is self-hosted single-
 *     site so the URL doesn't change between backup and restore — no S&R is
 *     needed. The dossier's MIGRATE_DB phase is intentionally absent from
 *     this state machine. Re-add as a follow-up if the V1 SaaS multi-site
 *     scenario lands.
 *   - rolled_back is absent (we don't yet expose an automatic rollback path;
 *     manual rollback is via the kept `.wpmgr-old-files-<id>/` dir).
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

use WPMgr\Agent\Keystore;
use WPMgr\Agent\Phpbu\ProgressClient;
use WPMgr\Agent\Schema;
use WPMgr\Agent\Signer;
use WPMgr\Agent\Support\AgeIdentity;
use WPMgr\Agent\Support\BackupTransport;
use WPMgr\Agent\Support\Blake3;

/**
 * State-machine driver for a single restore task row. Declared `final` —
 * exactly one RestoreRunner per restore invocation, instantiated by
 * RestoreCommand (Phase D dispatch) and by RestoreWatchdog (stall recovery).
 */
final class RestoreRunner
{
    /** Closed set of phase names — see class docblock for the enum. */
    public const PHASE_PREFLIGHT           = 'preflight';
    public const PHASE_DOWNLOAD_ARTIFACTS  = 'download_artifacts';
    public const PHASE_VERIFY_ARTIFACTS    = 'verify_artifacts';
    public const PHASE_MAINTENANCE_ON      = 'maintenance_on';
    public const PHASE_STAGE_FILES         = 'stage_files';
    public const PHASE_SWAP_FILES          = 'swap_files';
    public const PHASE_RESTORE_DB          = 'restore_db';
    public const PHASE_SWAP_DB             = 'swap_db';
    public const PHASE_POST_HOOKS          = 'post_hooks';
    public const PHASE_MAINTENANCE_OFF     = 'maintenance_off';
    public const PHASE_CLEANUP             = 'cleanup';
    public const PHASE_COMPLETED           = 'completed';
    public const PHASE_FAILED              = 'failed';

    /** Valid restore kinds (mirror of the CP RestoreRequest Kind enum). */
    public const KIND_FILES = 'files';
    public const KIND_DB    = 'db';
    public const KIND_FULL  = 'full';

    /** Minimum seconds between in-phase DB writes to last_progress_at. */
    private const PROGRESS_DB_THROTTLE_SECONDS = 5;

    /**
     * download_artifacts per-chunk retry policy. The presigned-GET path runs
     * through a Cloudflare tunnel to a self-hosted SeaweedFS in V0 deployments;
     * a single transient blip on a 381-chunk restore previously killed the
     * whole task. We mirror WPvivid's "remote connect retry" semantics
     * (`WPVIVID_REMOTE_CONNECT_RETRY_TIMES=3`, see
     * `docs/research/wpvivid-async-progress-restore.md` §7) but with a longer
     * cap because we're going over a public tunnel, not just a same-rack
     * S3 endpoint: 5 attempts, exponential backoff 1s / 2s / 4s / 8s / 16s
     * (cap 30s — the 30s timeout is per attempt, see BackupTransport).
     */
    private const DOWNLOAD_CHUNK_MAX_ATTEMPTS    = 5;
    private const DOWNLOAD_CHUNK_BACKOFF_BASE_MS = 1000;
    private const DOWNLOAD_CHUNK_BACKOFF_CAP_MS  = 30000;

    /**
     * Required free disk: artifact total bytes × this multiplier. WPvivid
     * recommends 2× in the dossier (§7.2 fix #2); we adopt 2.5× as a safety
     * margin (1× for downloads, 1× for staged extract, 0.5× for tmp tables).
     */
    private const PREFLIGHT_DISK_MULTIPLIER = 2.5;

    /** @var array<string,mixed> Runner params (see class docblock). */
    private array $params;

    /** Unix-seconds of the last DB write to last_progress_at (throttle). */
    private int $lastDbUpdate = 0;

    private ?ProgressClient $progressClient = null;

    /**
     * Test seam: the sleeper invoked between download retry attempts. Tests
     * override this with a no-op so the retry-loop test doesn't actually
     * pause 1+2+4+8 = 15s of wall clock. Default is `usleep` so production
     * gets the real backoff. Signature: (int $milliseconds) => void.
     *
     * @var callable(int):void
     */
    private $sleeper;

    /**
     * @param array<string,mixed> $params Same shape as $runnerParams in
     *                                    RestoreCommand::execute.
     */
    public function __construct(array $params)
    {
        $this->params = $params;
        // Default sleeper: real usleep. Tests override via setSleeper().
        $this->sleeper = static function (int $ms): void {
            if ($ms > 0) {
                @usleep($ms * 1000);
            }
        };

        if (
            class_exists(ProgressClient::class)
            && class_exists(Signer::class)
            && class_exists(Keystore::class)
            && ($this->params['progress_endpoint'] ?? '') !== ''
            && ($this->params['snapshot_id'] ?? '') !== ''
        ) {
            try {
                $this->progressClient = new ProgressClient(
                    (string) $this->params['progress_endpoint'],
                    (string) $this->params['snapshot_id'],
                    new Signer(new Keystore())
                );
            } catch (\Throwable $_) {
                $this->progressClient = null;
            }
        }
    }

    /**
     * Drive the task to completion (or to the next checkpoint a watchdog can
     * resume from). NEVER throws — top-level catch translates any escape
     * into a `failed` phase + progress post.
     *
     * @return string Terminal phase reached this invocation.
     */
    public function run(): string
    {
        @set_time_limit(0);
        @ignore_user_abort(true);

        $currentPhase = self::PHASE_PREFLIGHT;

        try {
            $task = $this->loadTask();
            if ($task === null) {
                $this->seedTask();
                $task = $this->loadTask();
                if ($task === null) {
                    throw new \RuntimeException('RestoreRunner: cannot create task row');
                }
            }
            $currentPhase = (string) $task['phase'];
            $subState     = (array) $task['sub_state'];

            if ($currentPhase === self::PHASE_COMPLETED || $currentPhase === self::PHASE_FAILED) {
                return $currentPhase;
            }

            // Compute the tmp prefix once + persist it into sub_state so
            // every phase agrees + a watchdog re-entry sees the same value.
            if (!isset($subState['tmp_prefix']) || !is_string($subState['tmp_prefix']) || $subState['tmp_prefix'] === '') {
                $subState['tmp_prefix'] = $this->makeTmpPrefix();
            }

            // ---- Phase dispatch loop. ------------------------------------
            while ($currentPhase !== self::PHASE_COMPLETED) {
                switch ($currentPhase) {
                    case self::PHASE_PREFLIGHT:
                        $subState = $this->runPreflight($subState);
                        $next     = self::PHASE_DOWNLOAD_ARTIFACTS;
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_DOWNLOAD_ARTIFACTS:
                        $subState = $this->runDownloadArtifacts($subState);
                        $next     = self::PHASE_VERIFY_ARTIFACTS;
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_VERIFY_ARTIFACTS:
                        $subState = $this->runVerifyArtifacts($subState);
                        $next     = self::PHASE_MAINTENANCE_ON;
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_MAINTENANCE_ON:
                        $subState = $this->runMaintenanceOn($subState);
                        $next     = $this->nextAfterMaintenanceOn();
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_STAGE_FILES:
                        $subState = $this->runStageFiles($subState);
                        $next     = self::PHASE_SWAP_FILES;
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_SWAP_FILES:
                        $subState = $this->runSwapFiles($subState);
                        $next     = $this->nextAfterSwapFiles();
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_RESTORE_DB:
                        $subState = $this->runRestoreDb($subState);
                        $next     = self::PHASE_SWAP_DB;
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_SWAP_DB:
                        $subState = $this->runSwapDb($subState);
                        $next     = self::PHASE_POST_HOOKS;
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_POST_HOOKS:
                        $subState = $this->runPostHooks($subState);
                        $next     = self::PHASE_MAINTENANCE_OFF;
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_MAINTENANCE_OFF:
                        $subState = $this->runMaintenanceOff($subState);
                        $next     = self::PHASE_CLEANUP;
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_CLEANUP:
                        $subState = $this->runCleanup($subState);
                        $this->saveTaskState(self::PHASE_COMPLETED, $subState);
                        $currentPhase = self::PHASE_COMPLETED;
                        break;

                    default:
                        throw new \RuntimeException('RestoreRunner: unknown phase ' . $currentPhase);
                }
            }

            // ---- Completion: cleanup + ack. ------------------------------
            $this->cleanupOnCompleted();
            $this->postProgress(self::PHASE_COMPLETED, [
                'ok'      => true,
                'summary' => 'restore completed',
            ]);

            return self::PHASE_COMPLETED;
        } catch (\Throwable $e) {
            error_log('WPMgr RestoreRunner: phase ' . $currentPhase . ' failed: ' . $e->getMessage());

            // Best-effort: mark failed, drop maintenance, try to clean up
            // tmp DB tables. Failures in the cleanup are swallowed.
            try {
                $this->maintenanceOff();
            } catch (\Throwable $_) {
            }

            try {
                $this->saveTaskState(self::PHASE_FAILED, [
                    'last_error' => substr($e->getMessage(), 0, 240),
                    'failed_in'  => $currentPhase,
                ]);
            } catch (\Throwable $_) {
            }
            try {
                $this->postProgress(self::PHASE_FAILED, [
                    'stage'   => $currentPhase,
                    'message' => substr($e->getMessage(), 0, 240),
                ]);
            } catch (\Throwable $_) {
            }

            return self::PHASE_FAILED;
        }
    }

    // ==================================================================
    // Phase handlers
    // ==================================================================

    /**
     * preflight: disk-space, DB-connectivity, scratch-dir checks. Posts a
     * single progress event with the resulting numbers.
     *
     * @param array<string,mixed> $subState
     * @return array<string,mixed>
     */
    private function runPreflight(array $subState): array
    {
        $this->ensureScratchDir();

        $artifactsTotal = $this->totalArtifactBytes();
        $required       = (int) ($artifactsTotal * self::PREFLIGHT_DISK_MULTIPLIER);

        // Disk free on wp-content's volume. disk_free_space returns the
        // bytes free for the filesystem the path lives on.
        $wpContent = $this->wpContentPath();
        $free      = $wpContent !== '' && is_dir($wpContent) ? (int) @disk_free_space($wpContent) : 0;

        if ($required > 0 && $free > 0 && $free < $required) {
            throw new \RuntimeException(sprintf(
                'preflight: insufficient disk space: have %d bytes, need ~%d',
                $free,
                $required
            ));
        }

        // DB connectivity smoke test. We don't open a real mysqli here —
        // just check that wpdb is available so a later phase doesn't fail
        // on a "no database" host config.
        global $wpdb;
        $dbOk = is_object($wpdb);

        $this->postProgress(self::PHASE_PREFLIGHT, [
            'disk_free_bytes' => $free,
            'disk_required'   => $required,
            'db_ok'           => $dbOk,
        ]);

        $subState['preflight'] = [
            'done'            => true,
            'disk_free_bytes' => $free,
            'disk_required'   => $required,
        ];
        return $subState;
    }

    /**
     * download_artifacts: pull every chunk from its presigned URL into the
     * per-artifact `<scratch>/<logical_path>` file. Idempotent — already
     * downloaded artifacts are skipped on watchdog re-entry, and an artifact
     * partially downloaded before a stall resumes at the first unwritten
     * chunk (not chunk 0).
     *
     * Per-chunk GETs use the v0.8.6 retry-with-backoff policy: up to
     * DOWNLOAD_CHUNK_MAX_ATTEMPTS attempts with exponential backoff. Network
     * errors and 5xx/408/425/429 are retried; terminal 4xx (404, 403, 400)
     * are not — they're fatal-by-construction (the URL is the same on every
     * attempt). The error message attached on terminal failure carries the
     * HTTP status, the host of the presigned URL, the body excerpt, and the
     * attempt count, so the SSE `phase_detail.message` + the WP error_log are
     * actually grep-able by the operator.
     *
     * @param array<string,mixed> $subState
     * @return array<string,mixed>
     */
    private function runDownloadArtifacts(array $subState): array
    {
        $transport = new BackupTransport(new Signer(new Keystore()));
        $age       = new AgeIdentity(new Keystore());
        $entries   = $this->chunkDownloads();
        $total     = count($entries);
        if ($total === 0) {
            throw new \RuntimeException('download_artifacts: no chunk_downloads supplied');
        }

        $resume     = isset($subState['download']) && is_array($subState['download']) ? $subState['download'] : [];
        $artifactDone = (int) ($resume['artifact_index'] ?? 0);
        $bytesTotal   = (int) ($resume['bytes_downloaded'] ?? 0);
        // Per-chunk resume offset within the in-flight artifact. Persisted
        // after every successful chunk write so a stall mid-artifact picks
        // up at chunk N+1 (not chunk 0). Cleared when an artifact finishes.
        $chunkResume  = (int) ($resume['chunk_index'] ?? 0);

        $artifactPaths = isset($resume['artifact_paths']) && is_array($resume['artifact_paths'])
            ? $resume['artifact_paths']
            : [];

        for ($i = $artifactDone; $i < $total; $i++) {
            $entry   = $entries[$i];
            $logical = (string) ($entry['logical_path'] ?? '');
            $chunks  = isset($entry['chunks']) && is_array($entry['chunks']) ? $entry['chunks'] : [];
            if ($logical === '') {
                throw new \RuntimeException('download_artifacts: entry ' . $i . ' missing logical_path');
            }
            if (!self::isSafeLogicalPath($logical)) {
                throw new \RuntimeException('download_artifacts: unsafe logical_path: ' . $logical);
            }

            $outPath = $this->scratchDir() . DIRECTORY_SEPARATOR . $logical;
            $outDir  = dirname($outPath);
            if (!is_dir($outDir) && !@mkdir($outDir, 0700, true) && !is_dir($outDir)) {
                throw new \RuntimeException('download_artifacts: cannot create out dir: ' . $outDir);
            }

            // Resume policy: which chunks (if any) of this artifact have
            // already been written. $chunkResume is non-zero ONLY for the
            // artifact at index $artifactDone (the one we were mid-way
            // through when a stall hit). For later artifacts we always
            // start from chunk 0.
            $startChunk = ($i === $artifactDone) ? $chunkResume : 0;
            $chunkCount = count($chunks);

            // Open in r+b ("read + write, don't truncate") when resuming
            // partway, write-mode otherwise. The r+b case truncates the file
            // to the expected partial offset before writing the next chunk —
            // this defends against a torn resume where the on-disk file has
            // MORE bytes than chunk_index says (a process kill between
            // fflush and saveTaskState would otherwise leave duplicate-chunk
            // garbage at the tail and corrupt the artifact).
            //
            // V0 ships with ENCRYPT_CHUNKS=false, so ciphertext bytes == plain
            // bytes — the on-disk size matches sum(chunks[0..startChunk-1].size)
            // exactly. (When/if we flip ENCRYPT_CHUNKS=true the per-chunk
            // ciphertext size is still authoritative because we write the
            // DECRYPTED bytes only; this seek/truncate calc would need a
            // separate plain-size accumulator. Tracked as a TODO for then.)
            if ($startChunk > 0 && is_file($outPath)) {
                $expectedOffset = 0;
                for ($s = 0; $s < $startChunk; $s++) {
                    $c = $chunks[$s] ?? null;
                    if (is_array($c) && isset($c['size']) && is_numeric($c['size'])) {
                        $expectedOffset += (int) $c['size'];
                    }
                }
                $handle = @fopen($outPath, 'r+b');
                if ($handle !== false) {
                    // ftruncate to expectedOffset then seek to it, so the next
                    // fwrite appends at exactly the right byte.
                    if ($expectedOffset > 0) {
                        @ftruncate($handle, $expectedOffset);
                        @fseek($handle, $expectedOffset, SEEK_SET);
                    } else {
                        @ftruncate($handle, 0);
                        @fseek($handle, 0, SEEK_SET);
                        $startChunk = 0;
                    }
                }
            } else {
                $startChunk = 0;
                $handle     = @fopen($outPath, 'wb');
            }
            if ($handle === false) {
                throw new \RuntimeException('download_artifacts: cannot open output file: ' . $outPath);
            }

            try {
                for ($chunkIdx = $startChunk; $chunkIdx < $chunkCount; $chunkIdx++) {
                    $chunk = $chunks[$chunkIdx];
                    if (!is_array($chunk)) {
                        throw new \RuntimeException('download_artifacts: malformed chunk at ' . $logical . '[' . $chunkIdx . ']');
                    }
                    $url   = (string) ($chunk['presigned_url'] ?? $chunk['url'] ?? $chunk['get_url'] ?? '');
                    $hash  = (string) ($chunk['hash'] ?? $chunk['blake3'] ?? '');
                    if ($url === '' || $hash === '') {
                        throw new \RuntimeException('download_artifacts: chunk missing url/hash at ' . $logical . '[' . $chunkIdx . ']');
                    }

                    $bytes = $this->fetchChunkWithRetries($transport, $url, $hash, $logical, $chunkIdx);

                    // Verify blake3 over the CIPHERTEXT (matches the upload
                    // pipeline's content-addressing scheme).
                    $actual = Blake3::hashHex($bytes);
                    if (!hash_equals($hash, $actual)) {
                        throw new \RuntimeException('download_artifacts: blake3 mismatch on chunk ' . $hash);
                    }

                    // Decrypt if the backup pipeline was running in age
                    // mode. EncryptAndUpload::ENCRYPT_CHUNKS is the
                    // canonical signal — match it here so the .bin
                    // plaintext path (V0 default) just writes bytes.
                    $plain = EncryptAndUpload::ENCRYPT_CHUNKS
                        ? $age->decryptChunk($bytes)
                        : $bytes;

                    $w = @fwrite($handle, $plain);
                    if ($w === false || $w !== strlen($plain)) {
                        throw new \RuntimeException('download_artifacts: write failed for ' . $logical);
                    }
                    $bytesTotal += strlen($bytes);

                    // Per-chunk checkpoint: persist the cursor so a watchdog
                    // re-entry resumes at chunkIdx+1, not at chunk 0. We
                    // throttle the actual DB write inside saveTaskState via
                    // PROGRESS_DB_THROTTLE_SECONDS, so this is cheap even on
                    // a 381-chunk restore.
                    if (($chunkIdx & 0x07) === 0 || $chunkIdx === $chunkCount - 1) {
                        $subState['download'] = [
                            'artifact_index'   => $i,
                            'chunk_index'      => $chunkIdx + 1,
                            'bytes_downloaded' => $bytesTotal,
                            'artifact_paths'   => $artifactPaths,
                        ];
                        // Sync to the open file so a hard kill doesn't lose
                        // the bytes we just wrote (and trigger a restart from
                        // chunk_index that doesn't match on-disk size).
                        @fflush($handle);
                        $this->saveTaskState(self::PHASE_DOWNLOAD_ARTIFACTS, $subState);

                        // Throttle progress posts: roughly every 8 chunks.
                        $this->onPhaseProgress(self::PHASE_DOWNLOAD_ARTIFACTS, [
                            'artifacts_done'    => $i,
                            'artifacts_total'   => $total,
                            'chunks_done'       => $chunkIdx + 1,
                            'chunks_total'      => $chunkCount,
                            'bytes_downloaded'  => $bytesTotal,
                            'current_artifact'  => $logical,
                        ]);
                    }
                }
            } finally {
                fclose($handle);
            }

            $artifactPaths[$logical] = $outPath;
            $artifactDone = $i + 1;
            // Artifact finished — reset the per-chunk cursor for the next
            // artifact (so a fresh resume of artifact i+1 starts at chunk 0).
            $chunkResume  = 0;

            // Persist after each artifact so a stall doesn't redo it.
            $subState['download'] = [
                'artifact_index'   => $artifactDone,
                'chunk_index'      => 0,
                'bytes_downloaded' => $bytesTotal,
                'artifact_paths'   => $artifactPaths,
            ];
            $this->saveTaskState(self::PHASE_DOWNLOAD_ARTIFACTS, $subState);
        }

        $subState['download'] = [
            'done'             => true,
            'artifact_index'   => $artifactDone,
            'bytes_downloaded' => $bytesTotal,
            'artifact_paths'   => $artifactPaths,
        ];

        $this->postProgress(self::PHASE_DOWNLOAD_ARTIFACTS, [
            'artifacts_done'   => $artifactDone,
            'artifacts_total'  => $total,
            'bytes_downloaded' => $bytesTotal,
        ]);

        return $subState;
    }

    /**
     * verify_artifacts: ensure each on-disk artifact exists + has non-zero
     * size. Per-chunk blake3 was already verified during download; we don't
     * need a second pass over the reconstructed plaintext (no stable
     * plaintext hash is in the manifest today).
     *
     * @param array<string,mixed> $subState
     * @return array<string,mixed>
     */
    private function runVerifyArtifacts(array $subState): array
    {
        $download = isset($subState['download']) && is_array($subState['download']) ? $subState['download'] : [];
        $paths    = isset($download['artifact_paths']) && is_array($download['artifact_paths']) ? $download['artifact_paths'] : [];
        if ($paths === []) {
            throw new \RuntimeException('verify_artifacts: no artifact_paths recorded');
        }

        $count = 0;
        $total = count($paths);
        foreach ($paths as $logical => $abs) {
            if (!is_string($abs) || !is_file($abs)) {
                throw new \RuntimeException('verify_artifacts: missing artifact on disk: ' . (string) $logical);
            }
            if (@filesize($abs) === 0) {
                throw new \RuntimeException('verify_artifacts: empty artifact: ' . (string) $logical);
            }
            $count++;
        }

        $this->postProgress(self::PHASE_VERIFY_ARTIFACTS, [
            'artifacts_done'  => $count,
            'artifacts_total' => $total,
        ]);

        $subState['verify'] = ['done' => true, 'artifacts' => $count];
        return $subState;
    }

    /**
     * maintenance_on: drop `.maintenance` in ABSPATH so visitors see the
     * standard WP maintenance page. Best-effort — a non-writable ABSPATH
     * doesn't fail the restore.
     */
    private function runMaintenanceOn(array $subState): array
    {
        $this->maintenanceOn();
        $this->postProgress(self::PHASE_MAINTENANCE_ON, []);
        $subState['maintenance'] = ['on' => true];
        return $subState;
    }

    /**
     * Decide the next phase after maintenance_on based on kind.
     */
    private function nextAfterMaintenanceOn(): string
    {
        $kind = $this->kind();
        if ($kind === self::KIND_DB) {
            return self::PHASE_RESTORE_DB;
        }
        // files OR full -> stage_files first
        return self::PHASE_STAGE_FILES;
    }

    /**
     * Decide the next phase after swap_files based on kind.
     */
    private function nextAfterSwapFiles(): string
    {
        $kind = $this->kind();
        if ($kind === self::KIND_FILES) {
            return self::PHASE_POST_HOOKS;
        }
        return self::PHASE_RESTORE_DB;
    }

    /**
     * stage_files: extract every part zip into the staging dir.
     */
    private function runStageFiles(array $subState): array
    {
        $download = isset($subState['download']) && is_array($subState['download']) ? $subState['download'] : [];
        $paths    = isset($download['artifact_paths']) && is_array($download['artifact_paths']) ? $download['artifact_paths'] : [];
        $zips     = [];
        foreach ($paths as $logical => $abs) {
            if (!is_string($logical) || !is_string($abs)) {
                continue;
            }
            // Anything that ends with .zip is a files-part artifact.
            if (substr(strtolower($logical), -4) === '.zip') {
                $zips[] = $abs;
            }
        }
        if ($zips === []) {
            throw new \RuntimeException('stage_files: no .zip parts in artifact list');
        }

        $restoreId = $this->restoreId();
        $target    = $this->wpContentPath();
        if ($target === '') {
            throw new \RuntimeException('stage_files: wp_content_path is empty');
        }

        $restorer   = new FilesRestorer();
        $stagingDir = $restorer->stage($zips, $target, $restoreId, function (string $phase, array $detail): void {
            $this->onPhaseProgress($phase, $detail);
        });

        $subState['stage'] = ['done' => true, 'staging_dir' => $stagingDir];
        return $subState;
    }

    /**
     * swap_files: atomically move staging into place + old aside.
     */
    private function runSwapFiles(array $subState): array
    {
        $stage      = isset($subState['stage']) && is_array($subState['stage']) ? $subState['stage'] : [];
        $stagingDir = (string) ($stage['staging_dir'] ?? '');
        if ($stagingDir === '') {
            throw new \RuntimeException('swap_files: staging_dir missing from sub_state');
        }

        $restorer = new FilesRestorer();
        $oldDir   = $restorer->swap(
            $stagingDir,
            $this->wpContentPath(),
            $this->restoreId(),
            function (string $phase, array $detail): void {
                $this->onPhaseProgress($phase, $detail);
            }
        );

        $subState['swap_files'] = ['done' => true, 'old_files_dir' => $oldDir];
        return $subState;
    }

    /**
     * restore_db: replay the SQL dump into tmp tables.
     */
    private function runRestoreDb(array $subState): array
    {
        $download = isset($subState['download']) && is_array($subState['download']) ? $subState['download'] : [];
        $paths    = isset($download['artifact_paths']) && is_array($download['artifact_paths']) ? $download['artifact_paths'] : [];
        $sqlPath  = '';
        foreach ($paths as $logical => $abs) {
            if (!is_string($logical) || !is_string($abs)) {
                continue;
            }
            $lower = strtolower($logical);
            if (substr($lower, -7) === '.sql.gz' || substr($lower, -4) === '.sql') {
                $sqlPath = $abs;
                break;
            }
        }
        if ($sqlPath === '') {
            throw new \RuntimeException('restore_db: no .sql/.sql.gz artifact in artifact list');
        }

        $tmpPrefix    = (string) ($subState['tmp_prefix'] ?? '');
        $sourcePrefix = $this->sourcePrefix();
        if ($tmpPrefix === '' || $sourcePrefix === '') {
            throw new \RuntimeException('restore_db: missing tmp_prefix or source_prefix');
        }

        $restorer  = new DbRestorer($this->dbCreds());
        $tmpTables = $restorer->restore(
            $sqlPath,
            $tmpPrefix,
            $sourcePrefix,
            function (string $phase, array $detail): void {
                $this->onPhaseProgress($phase, $detail);
            }
        );

        $subState['restore_db'] = ['done' => true, 'tmp_tables' => $tmpTables];
        return $subState;
    }

    /**
     * swap_db: atomic per-table swap.
     */
    private function runSwapDb(array $subState): array
    {
        $r          = isset($subState['restore_db']) && is_array($subState['restore_db']) ? $subState['restore_db'] : [];
        $tmpTables  = isset($r['tmp_tables']) && is_array($r['tmp_tables']) ? $r['tmp_tables'] : [];
        $tmpPrefix  = (string) ($subState['tmp_prefix'] ?? '');

        // Target prefix comes from the live wpdb — NOT from the params,
        // because a restore should always land in the LIVE site's prefix
        // (not the prefix the backup was taken under).
        $targetPrefix = $this->targetPrefix();
        if ($tmpPrefix === '' || $targetPrefix === '') {
            throw new \RuntimeException('swap_db: missing tmp/target prefix');
        }

        // Coerce to list<string>.
        $list = [];
        foreach ($tmpTables as $t) {
            if (is_string($t) && $t !== '') {
                $list[] = $t;
            }
        }

        $restorer = new DbRestorer($this->dbCreds());
        $restorer->swap($tmpPrefix, $targetPrefix, $list, function (string $phase, array $detail): void {
            $this->onPhaseProgress($phase, $detail);
        });

        $subState['swap_db'] = ['done' => true, 'tables_swapped' => count($list)];
        return $subState;
    }

    /**
     * post_hooks: flush rewrite rules, drop opcache, etc. Best-effort.
     */
    private function runPostHooks(array $subState): array
    {
        // Cache flush.
        if (function_exists('wp_cache_flush')) {
            @wp_cache_flush();
        }
        // OPcache reset (so any PHP files we replaced get reread).
        if (function_exists('opcache_reset')) {
            @opcache_reset();
        }
        // Rewrite rules: best effort. flush_rewrite_rules is a no-op when
        // we're not in an admin context, but calling it costs nothing.
        if (function_exists('flush_rewrite_rules')) {
            @flush_rewrite_rules(false);
        }

        $this->postProgress(self::PHASE_POST_HOOKS, []);
        $subState['post_hooks'] = ['done' => true];
        return $subState;
    }

    /**
     * maintenance_off: drop the `.maintenance` file.
     */
    private function runMaintenanceOff(array $subState): array
    {
        $this->maintenanceOff();
        $this->postProgress(self::PHASE_MAINTENANCE_OFF, []);
        $subState['maintenance']['on'] = false;
        return $subState;
    }

    /**
     * cleanup: drop downloaded artifacts and schedule the old-files GC.
     */
    private function runCleanup(array $subState): array
    {
        // 1. Remove the downloaded artifacts from scratch.
        $scratch = $this->scratchDir();
        if ($scratch !== '' && is_dir($scratch)) {
            $items = @scandir($scratch);
            if ($items !== false) {
                foreach ($items as $i) {
                    if ($i === '.' || $i === '..') {
                        continue;
                    }
                    $p = $scratch . DIRECTORY_SEPARATOR . $i;
                    if (is_file($p)) {
                        @unlink($p);
                    } elseif (is_dir($p)) {
                        $this->rrmdir($p);
                    }
                }
            }
            @rmdir($scratch);
        }

        // 2. Schedule the 24 h GC of the .wpmgr-old-files-<id>/ dir.
        if (function_exists('wp_next_scheduled') && function_exists('wp_schedule_single_event')) {
            if (!wp_next_scheduled('wpmgr_restore_oldfiles_gc')) {
                wp_schedule_single_event(
                    time() + FilesRestorer::OLDFILES_GC_AGE_SECONDS + 60,
                    'wpmgr_restore_oldfiles_gc'
                );
            }
        }

        $this->postProgress(self::PHASE_CLEANUP, []);
        $subState['cleanup'] = ['done' => true];
        return $subState;
    }

    // ==================================================================
    // Download retry
    // ==================================================================

    /**
     * Test seam: replace the inter-attempt sleeper so a retry-loop unit test
     * does not actually pause real wall-clock seconds. Production code never
     * calls this; the default sleeper is real usleep.
     *
     * @param callable(int):void $sleeper
     */
    public function setSleeper(callable $sleeper): void
    {
        $this->sleeper = $sleeper;
    }

    /**
     * Fetch a single chunk with retry-with-exponential-backoff. Throws a
     * RuntimeException carrying HTTP status + host + body excerpt + attempt
     * count on terminal failure so the SSE detail and the WP error_log are
     * actually grep-able by the operator.
     *
     * Retry semantics — see DOWNLOAD_CHUNK_MAX_ATTEMPTS / *_BACKOFF_* and
     * BackupTransport::getChunkWithStatus for the per-attempt classification.
     *
     * @param BackupTransport $transport
     * @param string $url        Presigned GET URL (NEVER logged).
     * @param string $hash       blake3 of the chunk (logged on error).
     * @param string $logical    Logical artifact path (logged on error).
     * @param int    $chunkIdx   Chunk index within the artifact (logged).
     * @return string Ciphertext bytes on success.
     * @throws \RuntimeException with a structured message on terminal failure.
     */
    private function fetchChunkWithRetries(
        BackupTransport $transport,
        string $url,
        string $hash,
        string $logical,
        int $chunkIdx
    ): string {
        $last = null;
        for ($attempt = 1; $attempt <= self::DOWNLOAD_CHUNK_MAX_ATTEMPTS; $attempt++) {
            $res = $transport->getChunkWithStatus($url);
            if ($res['ok']) {
                return (string) $res['body'];
            }
            $last = $res;
            if (!$res['retryable'] || $attempt >= self::DOWNLOAD_CHUNK_MAX_ATTEMPTS) {
                break;
            }
            // Exponential backoff: 1s, 2s, 4s, 8s, 16s (cap at 30s).
            $delayMs = (int) min(
                self::DOWNLOAD_CHUNK_BACKOFF_CAP_MS,
                self::DOWNLOAD_CHUNK_BACKOFF_BASE_MS * (1 << ($attempt - 1))
            );
            // Log the transient so the operator can grep wp-content/debug.log
            // for "wpmgr restore retry" if the restore eventually succeeds
            // (so they know it wasn't a smooth ride). We log host + status +
            // attempt; we NEVER log the presigned URL itself.
            error_log(sprintf(
                'WPMgr RestoreRunner: download retry chunk %s[%d] hash=%s attempt=%d/%d host=%s status=%d err=%s next_delay_ms=%d',
                $logical,
                $chunkIdx,
                substr($hash, 0, 12),
                $attempt,
                self::DOWNLOAD_CHUNK_MAX_ATTEMPTS,
                (string) $res['host'],
                (int) $res['status'],
                substr((string) ($res['error'] !== '' ? $res['error'] : $res['body_excerpt']), 0, 80),
                $delayMs
            ));
            ($this->sleeper)($delayMs);
        }

        // Terminal failure — assemble the structured message that the SSE
        // detail surface (phase_detail.message) and the WP error_log need.
        $status      = is_array($last) ? (int) ($last['status'] ?? 0) : 0;
        $host        = is_array($last) ? (string) ($last['host'] ?? '') : '';
        $bodyExcerpt = is_array($last) ? (string) ($last['body_excerpt'] ?? '') : '';
        $errMsg      = is_array($last) ? (string) ($last['error'] ?? '') : '';

        $tail = '';
        if ($status > 0) {
            $tail = sprintf('HTTP %d from %s', $status, $host !== '' ? $host : 'unknown');
            if ($bodyExcerpt !== '') {
                $tail .= '; body: ' . $bodyExcerpt;
            }
        } else {
            $tail = sprintf('transport error from %s', $host !== '' ? $host : 'unknown');
            if ($errMsg !== '') {
                $tail .= ': ' . $errMsg;
            }
        }

        $msg = sprintf(
            'download_artifacts: chunk %s[%d] hash=%s failed after %d attempts. last: %s',
            $logical,
            $chunkIdx,
            substr($hash, 0, 12),
            self::DOWNLOAD_CHUNK_MAX_ATTEMPTS,
            $tail
        );
        // Cap at 240 chars to match the saveTaskState last_error column limit
        // and the postProgress phase_detail message budget.
        throw new \RuntimeException(substr($msg, 0, 240));
    }

    // ==================================================================
    // Maintenance file
    // ==================================================================

    /**
     * Drop a `.maintenance` file in ABSPATH per WP's convention. The file
     * must `<?php` set `$upgrading = time();` for core to render the
     * maintenance page.
     */
    private function maintenanceOn(): void
    {
        $root = $this->wpRoot();
        if ($root === '' || !is_dir($root) || !is_writable($root)) {
            return;
        }
        $path = rtrim($root, DIRECTORY_SEPARATOR) . DIRECTORY_SEPARATOR . '.maintenance';
        $body = "<?php\n\$upgrading = " . time() . ';';
        @file_put_contents($path, $body, LOCK_EX);
    }

    private function maintenanceOff(): void
    {
        $root = $this->wpRoot();
        if ($root === '') {
            return;
        }
        $path = rtrim($root, DIRECTORY_SEPARATOR) . DIRECTORY_SEPARATOR . '.maintenance';
        if (is_file($path)) {
            @unlink($path);
        }
    }

    // ==================================================================
    // Persistence
    // ==================================================================

    /**
     * @return array{phase:string,kind:string,sub_state:array<string,mixed>,resume_count:int,max_resumes:int}|null
     */
    private function loadTask(): ?array
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return null;
        }
        $table = $this->tableName();
        if ($table === '') {
            return null;
        }

        $sql = "SELECT phase, kind, sub_state, resume_count, max_resumes
                FROM {$table}
                WHERE snapshot_id = %s AND restore_id = %s LIMIT 1";
        /** @phpstan-ignore-next-line — $wpdb is a runtime interface. */
        $prepared = $wpdb->prepare($sql, $this->snapshotId(), $this->restoreId());
        /** @phpstan-ignore-next-line */
        $row = $wpdb->get_row($prepared, ARRAY_A);

        if (!is_array($row)) {
            return null;
        }

        $sub = [];
        if (isset($row['sub_state']) && is_string($row['sub_state']) && $row['sub_state'] !== '') {
            $decoded = json_decode($row['sub_state'], true);
            if (is_array($decoded)) {
                $sub = $decoded;
            }
        }

        return [
            'phase'        => (string) ($row['phase'] ?? self::PHASE_PREFLIGHT),
            'kind'         => (string) ($row['kind'] ?? $this->kind()),
            'sub_state'    => $sub,
            'resume_count' => (int) ($row['resume_count'] ?? 0),
            'max_resumes'  => (int) ($row['max_resumes'] ?? 6),
        ];
    }

    private function seedTask(): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        $table = $this->tableName();
        if ($table === '') {
            return;
        }
        $now = time();

        $sql = "INSERT IGNORE INTO {$table}
                (snapshot_id, restore_id, kind, phase, sub_state, started_at, last_progress_at, resume_count, max_resumes)
                VALUES (%s, %s, %s, %s, %s, %d, %d, %d, %d)";
        /** @phpstan-ignore-next-line */
        $prepared = $wpdb->prepare(
            $sql,
            $this->snapshotId(),
            $this->restoreId(),
            $this->kind(),
            self::PHASE_PREFLIGHT,
            '{}',
            $now,
            $now,
            0,
            6
        );
        /** @phpstan-ignore-next-line */
        $wpdb->query($prepared);
    }

    /**
     * @param array<string,mixed> $subState
     */
    private function saveTaskState(string $phase, array $subState): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        $table = $this->tableName();
        if ($table === '') {
            return;
        }

        $now     = time();
        $encoded = json_encode($subState);
        if ($encoded === false) {
            $encoded = '{}';
        }
        $this->lastDbUpdate = $now;

        /** @phpstan-ignore-next-line */
        $wpdb->update(
            $table,
            [
                'phase'            => $phase,
                'sub_state'        => $encoded,
                'last_progress_at' => $now,
            ],
            [
                'snapshot_id' => $this->snapshotId(),
                'restore_id'  => $this->restoreId(),
            ],
            ['%s', '%s', '%d'],
            ['%s', '%s']
        );
    }

    private function touchProgressTimestamp(): void
    {
        $now = time();
        if ($now - $this->lastDbUpdate < self::PROGRESS_DB_THROTTLE_SECONDS) {
            return;
        }
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        $table = $this->tableName();
        if ($table === '') {
            return;
        }
        $this->lastDbUpdate = $now;

        /** @phpstan-ignore-next-line */
        $wpdb->update(
            $table,
            ['last_progress_at' => $now],
            [
                'snapshot_id' => $this->snapshotId(),
                'restore_id'  => $this->restoreId(),
            ],
            ['%d'],
            ['%s', '%s']
        );
    }

    private function onPhaseProgress(string $phase, array $detail): void
    {
        $this->touchProgressTimestamp();
        $this->postProgress($phase, $detail);
    }

    /**
     * @param array<string,mixed> $detail
     */
    private function postProgress(string $phase, array $detail): void
    {
        if ($this->progressClient === null) {
            return;
        }
        try {
            $this->progressClient->post($phase, $detail);
        } catch (\Throwable $_) {
            // Swallow — progress is observability, not correctness.
        }
    }

    // ==================================================================
    // Cleanup on completion
    // ==================================================================

    /**
     * Best-effort cleanup of the per-run scratch dir + dedup row. Called on
     * COMPLETED.
     */
    private function cleanupOnCompleted(): void
    {
        $scratch = $this->scratchDir();
        if ($scratch !== '' && is_dir($scratch)) {
            $this->rrmdir($scratch);
        }

        global $wpdb;
        if (is_object($wpdb)) {
            $runsTable = $this->prefix() . Schema::BACKUP_RESTORE_RUNS_TABLE;
            /** @phpstan-ignore-next-line */
            $wpdb->delete(
                $runsTable,
                [
                    'snapshot_id' => $this->snapshotId(),
                    'restore_id'  => $this->restoreId(),
                ],
                ['%s', '%s']
            );
        }
    }

    // ==================================================================
    // Helpers
    // ==================================================================

    private function ensureScratchDir(): void
    {
        $dir = $this->scratchDir();
        if ($dir === '') {
            throw new \RuntimeException('RestoreRunner: scratch_dir is empty');
        }
        if (!is_dir($dir) && !@mkdir($dir, 0700, true) && !is_dir($dir)) {
            throw new \RuntimeException('RestoreRunner: cannot create scratch dir: ' . $dir);
        }
        @chmod($dir, 0700);
    }

    private function snapshotId(): string
    {
        return (string) ($this->params['snapshot_id'] ?? '');
    }

    private function restoreId(): string
    {
        return (string) ($this->params['restore_id'] ?? '');
    }

    private function kind(): string
    {
        $k = (string) ($this->params['kind'] ?? '');
        return $k === '' ? self::KIND_FULL : $k;
    }

    private function scratchDir(): string
    {
        return (string) ($this->params['scratch_dir'] ?? '');
    }

    private function wpContentPath(): string
    {
        return (string) ($this->params['wp_content_path'] ?? '');
    }

    private function wpRoot(): string
    {
        return (string) ($this->params['wp_root'] ?? (defined('ABSPATH') ? ABSPATH : ''));
    }

    /**
     * The prefix the backup was taken under. We fall back to the live
     * wpdb prefix if not supplied — but in V0 self-hosted single-site,
     * the two are always equal.
     */
    private function sourcePrefix(): string
    {
        $db = isset($this->params['db']) && is_array($this->params['db']) ? $this->params['db'] : [];
        $p  = (string) ($db['prefix'] ?? '');
        if ($p === '') {
            $p = $this->targetPrefix();
        }
        return $p;
    }

    /**
     * The prefix the LIVE site uses (the target of the swap). Always read
     * from the live wpdb — we want restore to land in the live prefix.
     */
    private function targetPrefix(): string
    {
        global $wpdb;
        if (is_object($wpdb) && isset($wpdb->prefix) && is_string($wpdb->prefix)) {
            return $wpdb->prefix;
        }
        $db = isset($this->params['db']) && is_array($this->params['db']) ? $this->params['db'] : [];
        return (string) ($db['prefix'] ?? 'wp_');
    }

    /**
     * @return array{host:string,user:string,password:string,name:string,prefix:string}
     */
    private function dbCreds(): array
    {
        $db = isset($this->params['db']) && is_array($this->params['db']) ? $this->params['db'] : [];
        return [
            'host'     => (string) ($db['host'] ?? ''),
            'user'     => (string) ($db['user'] ?? ''),
            'password' => (string) ($db['password'] ?? ''),
            'name'     => (string) ($db['name'] ?? ''),
            'prefix'   => (string) ($db['prefix'] ?? ''),
        ];
    }

    /**
     * @return list<array{logical_path:string,chunks:list<array<string,mixed>>}>
     */
    private function chunkDownloads(): array
    {
        $d = isset($this->params['chunk_downloads']) && is_array($this->params['chunk_downloads'])
            ? $this->params['chunk_downloads']
            : [];
        $out = [];
        foreach ($d as $entry) {
            if (!is_array($entry)) {
                continue;
            }
            $logical = (string) ($entry['logical_path'] ?? '');
            $chunks  = isset($entry['chunks']) && is_array($entry['chunks']) ? $entry['chunks'] : [];
            if ($logical === '' || $chunks === []) {
                continue;
            }
            $out[] = ['logical_path' => $logical, 'chunks' => $chunks];
        }
        return $out;
    }

    /**
     * Best-effort sum of declared chunk sizes (for preflight disk check).
     * Returns 0 if no size hints are present; preflight then trusts that
     * the host has enough disk (matches WPvivid's "no preflight" baseline).
     */
    private function totalArtifactBytes(): int
    {
        $total = 0;
        foreach ($this->chunkDownloads() as $entry) {
            foreach ($entry['chunks'] as $c) {
                if (is_array($c) && isset($c['size']) && is_numeric($c['size'])) {
                    $total += (int) $c['size'];
                }
            }
        }
        return $total;
    }

    /**
     * Compute the tmp table prefix. Short enough to keep table names under
     * MySQL's 64-char limit even for the longest WP table name.
     */
    private function makeTmpPrefix(): string
    {
        $clean = preg_replace('/[^a-f0-9]/i', '', $this->restoreId()) ?? '';
        $short = substr($clean, 0, 8);
        if ($short === '') {
            $short = substr(bin2hex(random_bytes(4)), 0, 8);
        }
        return 'tmp' . $short . '_';
    }

    /**
     * Whether a manifest logical_path is safe to write inside the scratch
     * dir — no traversal, no NULs, no absolute paths.
     */
    private static function isSafeLogicalPath(string $p): bool
    {
        if ($p === '' || strpos($p, "\0") !== false) {
            return false;
        }
        if ($p[0] === '/' || $p[0] === '\\') {
            return false;
        }
        $parts = preg_split('#[/\\\\]+#', $p);
        if ($parts === false) {
            return false;
        }
        foreach ($parts as $part) {
            if ($part === '..' || $part === '.') {
                return false;
            }
        }
        return true;
    }

    private function tableName(): string
    {
        $p = $this->prefix();
        return $p === '' ? '' : $p . Schema::BACKUP_RESTORE_TASKS_TABLE;
    }

    private function prefix(): string
    {
        global $wpdb;
        if (is_object($wpdb) && isset($wpdb->prefix) && is_string($wpdb->prefix)) {
            return $wpdb->prefix;
        }
        return '';
    }

    /**
     * Recursive rmdir — best effort, never throws.
     */
    private function rrmdir(string $dir): void
    {
        if (!is_dir($dir)) {
            return;
        }
        $items = @scandir($dir);
        if ($items === false) {
            return;
        }
        foreach ($items as $i) {
            if ($i === '.' || $i === '..') {
                continue;
            }
            $p = $dir . DIRECTORY_SEPARATOR . $i;
            if (is_link($p) || is_file($p)) {
                @unlink($p);
            } elseif (is_dir($p)) {
                $this->rrmdir($p);
            }
        }
        @rmdir($dir);
    }
}
