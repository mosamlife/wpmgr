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
    // P0 URL rewriter: ADR-036 phase. Slots between RESTORE_DB and SWAP_DB so
    // the tmp tables exist (we have something to rewrite) but the live tables
    // haven't been swapped yet (rewriting the live site would be a footgun).
    public const PHASE_URL_REWRITE         = 'url_rewrite';
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
     *
     * @deprecated Replaced by the two-leg precheck below (artifact leg vs.
     *             staging leg, take the max). Kept for one release so an
     *             out-of-tree call site doesn't break on upgrade.
     */
    private const PREFLIGHT_DISK_MULTIPLIER = 2.5;

    /**
     * Two-leg disk-free precheck multipliers.
     *
     * Leg 1 (artifact): the downloaded artifacts (.zip parts + .sql.gz) sit
     * in scratch while the restore runs. 1.5× covers the raw bytes plus a
     * margin for tmp tables created during the DB replay phase.
     *
     * Leg 2 (staging): the staged wp-content tree is the same on-disk size
     * as live wp-content (we extract every file twice — once into staging,
     * once into the post-swap target). 1.0× is the floor.
     *
     * Required = max(legArtifact, legStaging), NOT the sum: by the time
     * staging is full we've already freed the artifact bytes (well, not
     * really — cleanup runs after swap — but the two legs overlap on disk
     * for only a short window mid-extract, and on a host that has enough
     * for the LARGER leg the smaller leg fits in the headroom). Using max()
     * over sum() trades a little safety for not nagging operators of small
     * VPSes who actually do have room to restore.
     */
    private const PREFLIGHT_ARTIFACT_MULTIPLIER = 1.5;
    private const PREFLIGHT_STAGING_MULTIPLIER  = 1.0;

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
                        // P0 URL rewriter: route through URL_REWRITE so a
                        // cross-environment restore rewrites siteurl/home/
                        // content/upload references in the tmp tables BEFORE
                        // the atomic swap. The phase itself short-circuits
                        // when source and target URLs match (the common
                        // same-environment case), so this is zero-cost for
                        // non-migrating restores.
                        $next     = self::PHASE_URL_REWRITE;
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_URL_REWRITE:
                        // P0 URL rewriter: ADR-036.
                        $subState = $this->runUrlRewrite($subState);
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
        $wpContent      = $this->wpContentPath();

        // Two-leg disk-free precheck — see PREFLIGHT_*_MULTIPLIER constants.
        // Leg 1: enough room for the downloaded artifacts + tmp tables.
        // Leg 2: enough room for the staging tree (same size as live
        // wp-content). We require max(leg1, leg2), not sum, because the legs
        // overlap in time — when staging is being filled, the artifacts on
        // disk are smaller than the bytes already extracted out of them.
        $legArtifact = (int) ($artifactsTotal * self::PREFLIGHT_ARTIFACT_MULTIPLIER);
        $legStaging  = (int) (FilesRestorer::estimateWpContentBytes($wpContent) * self::PREFLIGHT_STAGING_MULTIPLIER);
        $required    = max($legArtifact, $legStaging);

        // Disk free on wp-content's volume. disk_free_space returns the
        // bytes free for the filesystem the path lives on.
        $free = $wpContent !== '' && is_dir($wpContent) ? (int) @disk_free_space($wpContent) : 0;

        if ($required > 0 && $free > 0 && $free < $required) {
            // Operator-facing message — surfaces in the SSE phase_detail and
            // the WP error_log. GB units (not bytes) because that's the unit
            // operators reason about when looking at `df -h`.
            throw new \RuntimeException(sprintf(
                'Not enough free disk. Need ~%s GB, have %s GB. Free up space and retry, or restore to a different mount.',
                self::formatGb($required),
                self::formatGb($free)
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
     *
     * Track 5 — the staging tree mirrors live wp-content's shape (zip entry
     * names are wp-content-relative, e.g. `plugins/foo/foo.php`), so a
     * snapshot containing only `plugins.partNNN.zip` parts populates
     * `<stagingDir>/plugins/...` and leaves the staging tree's `themes/`,
     * `uploads/`, etc. EMPTY. This is the per-component split's contract:
     * whatever the manifest carries is what staging gets.
     *
     * We also classify the parts (by filename prefix) and record the set of
     * component kinds present in this snapshot. The swap_files phase reads
     * this set to decide between the legacy whole-swap path (entry_kind=
     * 'file' OR all 4 components present) and the per-component swap path.
     */
    private function runStageFiles(array $subState): array
    {
        $download = isset($subState['download']) && is_array($subState['download']) ? $subState['download'] : [];
        $paths    = isset($download['artifact_paths']) && is_array($download['artifact_paths']) ? $download['artifact_paths'] : [];
        $zips     = [];
        $componentsPresent = [];
        $hasLegacyFileEntry = false;
        foreach ($paths as $logical => $abs) {
            if (!is_string($logical) || !is_string($abs)) {
                continue;
            }
            // Anything that ends with .zip is a files-part artifact.
            if (substr(strtolower($logical), -4) !== '.zip') {
                continue;
            }
            $zips[] = $abs;
            $kind   = $this->classifyArtifactKind($logical);
            if ($kind === 'file') {
                // Pre-Track-5 snapshot: a single `wp-content.partNNN.zip`
                // sequence whose manifest entry_kind is 'file'. The swap path
                // for this case is the whole-wp-content swap (preserves the
                // legacy contract).
                $hasLegacyFileEntry = true;
            } elseif ($kind === 'plugin' || $kind === 'theme' || $kind === 'upload' || $kind === 'wp-content') {
                $componentsPresent[$kind] = true;
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

        $subState['stage'] = [
            'done'                 => true,
            'staging_dir'          => $stagingDir,
            'components_present'   => array_keys($componentsPresent),
            'has_legacy_file_kind' => $hasLegacyFileEntry,
        ];
        return $subState;
    }

    /**
     * swap_files: atomically move staging into place + old aside.
     *
     * Track 5 — chooses one of two paths based on the manifest:
     *   1. Legacy / "Everything" — whole-wp-content swap. Used when:
     *        a. The snapshot has any 'file' entry_kind (pre-Track-5), OR
     *        b. The selection covers all 4 file components and no per-
     *           component subset is needed.
     *      Faster, single atomic rename, single rollback dir.
     *
     *   2. Per-component — call swapComponents() with the actual components
     *      present in the snapshot. Used when only a subset of file
     *      components is being restored (e.g. CP selected `plugin` only;
     *      only `plugins.partNNN.zip` parts came down).
     *
     * The CP's selectEntries upstream filters the manifest to ONLY the
     * components the operator asked for, so by the time we're here the set
     * of components actually downloaded == the components to swap. We do not
     * need a separate "which components were selected" param — the staging
     * tree carries that information.
     */
    private function runSwapFiles(array $subState): array
    {
        $stage      = isset($subState['stage']) && is_array($subState['stage']) ? $subState['stage'] : [];
        $stagingDir = (string) ($stage['staging_dir'] ?? '');
        if ($stagingDir === '') {
            throw new \RuntimeException('swap_files: staging_dir missing from sub_state');
        }
        $componentsPresent  = isset($stage['components_present']) && is_array($stage['components_present'])
            ? array_values(array_filter($stage['components_present'], 'is_string'))
            : [];
        $hasLegacyFileKind = !empty($stage['has_legacy_file_kind']);

        // Reconcile components_present (recorded by runStageFiles from artifact
        // filename prefixes) against on-disk staging. A part archive whose
        // contents were entirely DEFAULT_EXCLUDES still gets emitted as a zip,
        // so the component appears in components_present even though FilesRestorer
        // extracted nothing into the corresponding staging subdir. Without this
        // reconciliation, swapComponents() throws
        // "staging missing component subdir: …/plugins" mid-restore (the
        // 2026-05-29 v0.9.6 SSE failure).
        $stagingSubdirs = [
            'plugin' => 'plugins',
            'theme'  => 'themes',
            'upload' => 'uploads',
        ];
        $componentsPresent = array_values(array_filter(
            $componentsPresent,
            static function (string $c) use ($stagingDir, $stagingSubdirs): bool {
                // The catch-all "wp-content" component is handled by
                // swapComponents itself by iterating the staging root for
                // non-managed top-level items; it does not require a
                // staging subdir of its own, so always keep it.
                if ($c === 'wp-content') {
                    return true;
                }
                $sub = $stagingSubdirs[$c] ?? '';
                if ($sub === '') {
                    return false;
                }
                $path = $stagingDir . DIRECTORY_SEPARATOR . $sub;
                return is_dir($path);
            }
        ));

        $restorer = new FilesRestorer();

        // Path 1: legacy snapshot (entry_kind='file') OR all 4 components
        // present (the "Everything" case). Whole-wp-content swap.
        $allFour      = ['plugin', 'theme', 'upload', 'wp-content'];
        $presentSet   = array_flip($componentsPresent);
        $hasAllFour   = !array_diff($allFour, $componentsPresent);
        if ($hasLegacyFileKind || $hasAllFour) {
            $oldDir = $restorer->swap(
                $stagingDir,
                $this->wpContentPath(),
                $this->restoreId(),
                function (string $phase, array $detail): void {
                    $this->onPhaseProgress($phase, $detail);
                }
            );
            $subState['swap_files'] = [
                'done'          => true,
                'old_files_dir' => $oldDir,
                'mode'          => $hasLegacyFileKind ? 'legacy_whole' : 'whole_all_components',
            ];
            return $subState;
        }

        // Path 2: per-component swap. componentsPresent is the subset the CP
        // filtered down for us.
        if ($componentsPresent === []) {
            throw new \RuntimeException('swap_files: no components present to swap and no legacy entry detected');
        }
        $result = $restorer->swapComponents(
            $stagingDir,
            $this->wpContentPath(),
            $componentsPresent,
            $this->restoreId(),
            function (string $phase, array $detail): void {
                $this->onPhaseProgress($phase, $detail);
            }
        );

        // Best-effort cleanup of any leftover staging payload that wasn't
        // promoted (e.g. unused subdirs for components NOT selected).
        $leftoverStaging = (string) ($result['staging_dir'] ?? '');
        if ($leftoverStaging !== '' && is_dir($leftoverStaging)) {
            $this->rrmdir($leftoverStaging);
        }

        $subState['swap_files'] = [
            'done'        => true,
            'mode'        => 'per_component',
            'components'  => $componentsPresent,
            'old_dirs'    => isset($result['old_dirs']) && is_array($result['old_dirs']) ? $result['old_dirs'] : [],
        ];
        return $subState;
    }

    /**
     * Classify a downloaded artifact filename into the Track-5 component
     * kind. Mirror of EncryptAndUpload::entryKind for the inverse direction:
     * given a logical path on the wire, return its component bucket.
     *
     * @param string $logical Logical artifact path from the CP plan.
     * @return string 'plugin' | 'theme' | 'upload' | 'wp-content' | 'file' |
     *                'db' | 'inspection' | ''
     */
    private function classifyArtifactKind(string $logical): string
    {
        $lower = strtolower($logical);
        if ($lower === 'sql-inspection.json') {
            return 'inspection';
        }
        if (str_ends_with($lower, '.sql') || str_ends_with($lower, '.sql.gz') || str_contains($lower, 'database.sql')) {
            return 'db';
        }
        if (str_starts_with($lower, 'plugins.part') && str_ends_with($lower, '.zip')) {
            return 'plugin';
        }
        if (str_starts_with($lower, 'themes.part') && str_ends_with($lower, '.zip')) {
            return 'theme';
        }
        if (str_starts_with($lower, 'uploads.part') && str_ends_with($lower, '.zip')) {
            return 'upload';
        }
        if (str_starts_with($lower, 'wp-content.part') && str_ends_with($lower, '.zip')) {
            return 'wp-content';
        }
        // Anything else (legacy or unrecognized) — treat as the legacy 'file'
        // entry_kind so the whole-wp-content swap path covers it.
        if (str_ends_with($lower, '.zip')) {
            return 'file';
        }
        return '';
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
     * P0 URL rewriter (ADR-036): rewrite siteurl/home/content/upload URL
     * references in the tmp tables before the atomic swap.
     *
     * Short-circuits to a no-op when the source URLs (read from the dump
     * banner and/or the CP-supplied `source_*` params) all equal the target
     * URLs. This is the common self-hosted same-environment restore — no
     * URLs changed, no rewrite needed, zero rows touched.
     *
     * Cross-environment restore: builds the WPvivid-pattern replacement set
     * and walks every tmp table paginated (5000 rows/page). Sub-state is
     * persisted per page so a watchdog re-entry resumes at the last
     * checkpointed offset rather than restarting the table.
     */
    private function runUrlRewrite(array $subState): array
    {
        $tmpPrefix    = (string) ($subState['tmp_prefix'] ?? '');
        $sourcePrefix = $this->sourcePrefix();
        if ($tmpPrefix === '' || $sourcePrefix === '') {
            // No tmp prefix means restore_db didn't actually create tmp
            // tables (e.g. a kind=files restore). Skip cleanly.
            $subState['url_rewrite'] = ['done' => true, 'skipped' => 'no_tmp_prefix'];
            return $subState;
        }

        // Resolve source URLs. Precedence:
        //   1. CP-supplied `source_*` params (manifest-recorded, authoritative
        //      when the snapshot has them).
        //   2. Banner comments in the actual dump file (defense — survives a
        //      missing/stale manifest).
        $sourceFromParams = [
            'site'    => (string) ($this->params['source_site_url']    ?? ''),
            'home'    => (string) ($this->params['source_home_url']    ?? ''),
            'content' => (string) ($this->params['source_content_url'] ?? ''),
            'upload'  => (string) ($this->params['source_upload_url']  ?? ''),
        ];
        $sourceFromDump = $this->extractDumpUrlsFromSubState($subState);

        $oldSite   = $sourceFromParams['site']    !== '' ? $sourceFromParams['site']    : $sourceFromDump['old_site_url'];
        $oldHome   = $sourceFromParams['home']    !== '' ? $sourceFromParams['home']    : $sourceFromDump['old_home_url'];
        $oldContent = $sourceFromParams['content'] !== '' ? $sourceFromParams['content'] : $sourceFromDump['old_content_url'];
        $oldUpload = $sourceFromParams['upload']  !== '' ? $sourceFromParams['upload']  : $sourceFromDump['old_upload_url'];

        // Resolve target URLs. Same precedence — CP params first, then the
        // live site values as fallback (so a same-environment restore lands
        // a no-op).
        $newSite   = (string) ($this->params['target_site_url']    ?? '');
        $newHome   = (string) ($this->params['target_home_url']    ?? '');
        $newContent = (string) ($this->params['target_content_url'] ?? '');
        $newUpload = (string) ($this->params['target_upload_url']  ?? '');
        if ($newSite === '' && function_exists('site_url')) {
            $newSite = rtrim((string) site_url(), '/');
        }
        if ($newHome === '' && function_exists('home_url')) {
            $newHome = rtrim((string) home_url(), '/');
        }
        if ($newContent === '' && defined('WP_CONTENT_URL')) {
            $newContent = rtrim((string) WP_CONTENT_URL, '/');
        }
        if ($newContent === '' && $newSite !== '') {
            // V1 simplification: derive from new site URL if not supplied.
            $newContent = $newSite . '/wp-content';
        }
        if ($newUpload === '' && function_exists('wp_upload_dir')) {
            $upload = wp_upload_dir();
            if (is_array($upload) && isset($upload['baseurl']) && is_string($upload['baseurl'])) {
                $newUpload = rtrim($upload['baseurl'], '/');
            }
        }
        if ($newUpload === '' && $newContent !== '') {
            $newUpload = $newContent . '/uploads';
        }

        // Fast-exit: if nothing changed, skip the whole phase.
        $sameUrls =
            ($oldSite === '' || $oldSite === $newSite) &&
            ($oldHome === '' || $oldHome === $newHome) &&
            ($oldContent === '' || $oldContent === $newContent) &&
            ($oldUpload === '' || $oldUpload === $newUpload);
        if ($sameUrls) {
            $this->postProgress(self::PHASE_URL_REWRITE, [
                'skipped'    => 'same_urls',
                'source_site' => $oldSite,
                'target_site' => $newSite,
            ]);
            $subState['url_rewrite'] = ['done' => true, 'skipped' => 'same_urls'];
            return $subState;
        }

        $replacements = \WPMgr\Agent\Backup\UrlRewriter::build_replacements(
            $oldSite,
            $newSite,
            $oldHome,
            $newHome,
            $oldContent,
            $newContent,
            $oldUpload,
            $newUpload
        );
        $fromCount = is_array($replacements[0] ?? null) ? count($replacements[0]) : 0;

        $this->postProgress(self::PHASE_URL_REWRITE, [
            'started'           => true,
            'source_site'       => $oldSite,
            'target_site'       => $newSite,
            'replacements_count' => $fromCount,
        ]);

        $resume = isset($subState['url_rewrite']) && is_array($subState['url_rewrite']) ? $subState['url_rewrite'] : [];

        $restorer = new DbRestorer($this->dbCreds());
        // The checkpoint callback persists the running url_rewrite progress
        // straight to the task row so a watchdog re-entry resumes at the
        // last seen offset (not the table head). We re-read the existing
        // sub-state inside the closure so the checkpoint payload is merged
        // atomically rather than clobbering other phases' state.
        $self = $this;
        $tmpPrefixCap = $tmpPrefix;
        $fromCountCap = $fromCount;
        $oldSiteCap   = $oldSite;
        $newSiteCap   = $newSite;
        $result = $restorer->rewriteAllTables(
            $tmpPrefix,
            $sourcePrefix,
            $replacements,
            $resume,
            function (array $pageState) use ($self, $tmpPrefixCap, $fromCountCap, $oldSiteCap, $newSiteCap): void {
                // Merge the per-page cursor into a snapshot of the runner's
                // current sub-state and persist. We don't update $subState
                // by reference here because PHP closures can't capture the
                // outer sub-state by reference across multiple invocations
                // safely — instead we re-save the row each page so a watchdog
                // re-entry reads the latest cursor.
                $self->checkpointUrlRewrite($tmpPrefixCap, $pageState, $fromCountCap, $oldSiteCap, $newSiteCap);
            },
            function (string $phase, array $detail): void {
                $this->onPhaseProgress($phase, $detail);
            }
        );

        $subState['url_rewrite'] = [
            'done'                => true,
            'replacements_count'  => $fromCount,
            'total_updates'       => (int) ($result['total_updates'] ?? 0),
            'tables_done'         => (int) (is_array($result['tables_done'] ?? null) ? count($result['tables_done']) : 0),
            'tables_total'        => (int) ($result['tables_total'] ?? 0),
            'source_site_url'     => $oldSite,
            'target_site_url'     => $newSite,
        ];
        return $subState;
    }

    /**
     * P0 URL rewriter: persist a per-page checkpoint while the URL rewrite
     * phase is in flight. The callback closure in `runUrlRewrite()` invokes
     * this for each table page so the running cursor (table_offset map +
     * tables_done list + cumulative update count) is written through to
     * `wpmgr_restore_tasks.sub_state` immediately. A watchdog re-entry then
     * reads the latest cursor and resumes mid-table.
     *
     * Public so the closure can call it; not part of the runner's external
     * contract.
     *
     * @param array<string,mixed> $pageState From DbRestorer::rewriteAllTables's checkpoint callback.
     */
    public function checkpointUrlRewrite(string $tmpPrefix, array $pageState, int $replacementsCount, string $sourceSite, string $targetSite): void
    {
        // Re-load current sub_state from the DB so we merge instead of clobber.
        $task = $this->loadTask();
        if ($task === null) {
            return;
        }
        $subState = (array) ($task['sub_state'] ?? []);
        $url      = isset($subState['url_rewrite']) && is_array($subState['url_rewrite']) ? $subState['url_rewrite'] : [];
        $url = array_merge($url, $pageState, [
            'replacements_count' => $replacementsCount,
            'source_site_url'    => $sourceSite,
            'target_site_url'    => $targetSite,
        ]);
        // Don't accidentally flip 'done' true on a mid-table checkpoint:
        // rewriteAllTables only sets finished=true on its terminal call,
        // which is also when the runner exits the closure loop.
        $subState['url_rewrite']  = $url;
        $subState['tmp_prefix']   = $tmpPrefix; // ensure preserved
        $this->saveTaskState(self::PHASE_URL_REWRITE, $subState);
    }

    /**
     * P0 URL rewriter: lazily extract source URLs from the dump file. Result
     * is memoised in sub-state so repeated runUrlRewrite() invocations on
     * watchdog resume don't re-parse the dump head.
     *
     * @return array{old_site_url:string,old_home_url:string,old_content_url:string,old_upload_url:string,old_table_prefix:string}
     */
    private function extractDumpUrlsFromSubState(array &$subState): array
    {
        if (isset($subState['url_rewrite']['dump_urls']) && is_array($subState['url_rewrite']['dump_urls'])) {
            $cached = $subState['url_rewrite']['dump_urls'];
            return [
                'old_site_url'     => (string) ($cached['old_site_url']     ?? ''),
                'old_home_url'     => (string) ($cached['old_home_url']     ?? ''),
                'old_content_url' => (string) ($cached['old_content_url'] ?? ''),
                'old_upload_url'   => (string) ($cached['old_upload_url']   ?? ''),
                'old_table_prefix' => (string) ($cached['old_table_prefix'] ?? ''),
            ];
        }
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
            return [
                'old_site_url'     => '',
                'old_home_url'     => '',
                'old_content_url' => '',
                'old_upload_url'   => '',
                'old_table_prefix' => '',
            ];
        }
        $extracted = DbRestorer::extractDumpUrls($sqlPath);
        if (!isset($subState['url_rewrite']) || !is_array($subState['url_rewrite'])) {
            $subState['url_rewrite'] = [];
        }
        $subState['url_rewrite']['dump_urls'] = $extracted;
        return $extracted;
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
     * cleanup: drop downloaded artifacts, then deal with the per-run
     * `.wpmgr-old-files-<id>/` rollback tree.
     *
     * The rollback tree is the live wp-content that swap_files moved aside.
     * Pre-0.9.5 we kept it for 24h on every restore, which routinely tipped
     * small-VPS hosts into disk-red. 0.9.5 flips the default:
     *
     *   - `keep_old_files !== true` (DEFAULT): synchronously rrmdir the
     *     exact dir recorded in sub_state during swap_files. No glob — two
     *     concurrent restores on the same host would otherwise clobber each
     *     other's rollback trees.
     *   - `keep_old_files === true`: schedule the 24h GC the way pre-0.9.5
     *     did, for operators who explicitly want a long manual-rollback
     *     window.
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

        // 2. Old-files disposition. Track 5 — two shapes:
        //    Legacy / whole-swap: single `old_files_dir`.
        //    Per-component swap : map `old_dirs[component => abs_dir]`, plus
        //                         a glob of `.wpmgr-old-wpcontent-<short>-*`
        //                         siblings for the catch-all component.
        $keepOld     = !empty($this->params['keep_old_files']) && $this->params['keep_old_files'] === true;
        $swap        = isset($subState['swap_files']) && is_array($subState['swap_files']) ? $subState['swap_files'] : [];
        $oldFilesDir = (string) ($swap['old_files_dir'] ?? '');
        $oldDirs     = isset($swap['old_dirs']) && is_array($swap['old_dirs']) ? $swap['old_dirs'] : [];

        if ($keepOld) {
            // Operator opted into the long-window manual-rollback path.
            // Schedule a 24h GC + 60s grace to sweep the exact dir later.
            if (function_exists('wp_next_scheduled') && function_exists('wp_schedule_single_event')) {
                if (!wp_next_scheduled('wpmgr_restore_oldfiles_gc')) {
                    wp_schedule_single_event(
                        time() + FilesRestorer::OLDFILES_GC_AGE_SECONDS_LONG + 60,
                        'wpmgr_restore_oldfiles_gc'
                    );
                }
            }
        } else {
            // Default path: synchronous rrmdir of the EXACT dirs recorded in
            // sub_state. Not a glob — two concurrent restores on the same
            // host would lose each other's rollback trees on a glob sweep.
            if ($oldFilesDir !== '' && is_dir($oldFilesDir)) {
                $this->rrmdir($oldFilesDir);
            }
            foreach ($oldDirs as $comp => $abs) {
                if (!is_string($abs) || $abs === '') {
                    continue;
                }
                if ($comp === 'wp-content') {
                    // The catch-all component's rollback is a glob of
                    // `.wpmgr-old-wpcontent-<short>-*` siblings — the marker
                    // path recorded in sub_state is the common prefix; expand
                    // it. This is the ONE place we use a glob, and it's
                    // bounded to a per-restore prefix, so concurrent restores
                    // don't clobber each other.
                    $hits = @glob($abs . '-*');
                    if (is_array($hits)) {
                        foreach ($hits as $h) {
                            if (is_dir($h)) {
                                $this->rrmdir($h);
                            } elseif (is_file($h) || is_link($h)) {
                                @unlink($h);
                            }
                        }
                    }
                    continue;
                }
                if (is_dir($abs)) {
                    $this->rrmdir($abs);
                }
            }
        }

        $this->postProgress(self::PHASE_CLEANUP, []);
        $subState['cleanup'] = ['done' => true, 'kept_old_files' => $keepOld];
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
     * Format a byte count as a 1-decimal GB string for the operator-facing
     * preflight error message. Always rounds up to at least 0.1 GB so a
     * sub-100MB value doesn't render as "0.0 GB" (which reads as a bug).
     */
    private static function formatGb(int $bytes): string
    {
        if ($bytes <= 0) {
            return '0.0';
        }
        $gb = $bytes / (1024 * 1024 * 1024);
        if ($gb < 0.1) {
            $gb = 0.1;
        }
        return number_format($gb, 1);
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
