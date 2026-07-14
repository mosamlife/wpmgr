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
 * Phase transitions — the restore state machine, with V0 simplifications
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
 *   post_hooks           -> health_check               (always)
 *   health_check         -> maintenance_off            (always, on a pass)
 *   maintenance_off      -> health_check_live          (always)
 *   health_check_live    -> cleanup                    (always, on a pass)
 *   cleanup              -> completed
 *
 *   completed | failed: terminal (re-entry is a no-op)
 *
 * GH #146 — TWO health-check gates, for two different reasons a restore can
 * be broken:
 *
 *   Gate 1, `health_check` (Probe A / DB only) — runs right after
 *   post_hooks, BEFORE maintenance_off, so a rollback it triggers happens
 *   invisibly while the site is still in maintenance mode. Regardless of
 *   kind — files-only, db-only, and full restores all converge on
 *   post_hooks first, so this one gate covers all three.
 *
 *   Gate 2, `health_check_live` (Probe B / loopback WSOD) — runs right
 *   AFTER maintenance_off, deliberately. A loopback probe run BEFORE
 *   maintenance_off can only ever see WordPress's OWN `.maintenance` 503
 *   page, which core renders before plugins are even loaded — it physically
 *   CANNOT observe a files-side WSOD (a dropped required file fataling on
 *   plugin load, the GitHub issue #147 class this feature exists to catch).
 *   Gate 2 therefore runs the loopback probe against the REAL, live,
 *   post-maintenance site. On a definitive fatal it re-enables maintenance
 *   for the duration of the rollback (so visitors don't see the broken
 *   site being reverted), then drops it again once the revert lands.
 *
 * Either gate's definitive fatal drives the same combined files+DB rollback
 * and reports `failed` instead of ever reaching `completed`; see
 * `RestoreHealthCheck` / `RestoreGuard` / `runHealthCheck()` /
 * `runHealthCheckLive()`. The `RestoreGuard` shutdown backstop stays armed
 * (not `markClean()`'d) across BOTH gates — only Gate 2 passing marks it
 * clean, since Gate 2 is the last checkpoint before `completed`.
 *
 * V0 simplifications (deferred phases):
 *   - migrate_db (search-and-replace) is deferred. V0 is self-hosted single-
 *     site so the URL doesn't change between backup and restore — no S&R is
 *     needed. A MIGRATE_DB phase is intentionally absent from
 *     this state machine. Re-add as a follow-up if the V1 SaaS multi-site
 *     scenario lands.
 *   - rolled_back is absent (we don't yet expose an automatic rollback path;
 *     manual rollback is via the kept `.wpmgr-old-files-<id>/` dir).
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

use WPMgr\Agent\Backup\Destinations\BackupDestination;
use WPMgr\Agent\Backup\Destinations\DestinationResolver;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Phpbu\ProgressClient;
use WPMgr\Agent\Schema;
use WPMgr\Agent\Signer;
use WPMgr\Agent\Support\AgeIdentity;
use WPMgr\Agent\Support\BackupTransport;
use WPMgr\Agent\Support\Blake3;
use WPMgr\Agent\Support\LongRunningJob;

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
    // GH #146: two-gate post-restore health check + auto-rollback — see
    // class docblock. Gate 1 (DB-only) slots between post_hooks and
    // maintenance_off; Gate 2 (loopback WSOD, on the real site) slots
    // between maintenance_off and cleanup.
    public const PHASE_HEALTH_CHECK        = 'health_check';
    public const PHASE_MAINTENANCE_OFF     = 'maintenance_off';
    public const PHASE_HEALTH_CHECK_LIVE   = 'health_check_live';
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
     * whole task. We use a retry-with-backoff policy (see
     * `docs/research/async-progress-restore.md` §7) but with a longer
     * cap because we're going over a public tunnel, not just a same-rack
     * S3 endpoint: 5 attempts, exponential backoff 1s / 2s / 4s / 8s / 16s
     * (cap 30s — the 30s timeout is per attempt, see BackupTransport).
     */
    private const DOWNLOAD_CHUNK_MAX_ATTEMPTS    = 5;
    private const DOWNLOAD_CHUNK_BACKOFF_BASE_MS = 1000;
    private const DOWNLOAD_CHUNK_BACKOFF_CAP_MS  = 30000;

    /**
     * Required free disk: artifact total bytes × this multiplier. A 2× floor is
     * the safe minimum; we adopt 2.5× as a safety
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

    /**
     * GH #146 — third preflight leg: headroom for the forced pre-restore DB
     * dump `runSwapDb()` captures before the destructive DROP. 1.2× the live
     * DB's on-disk size (data_length + index_length) covers the gzip-
     * compressed dump comfortably (SQL dumps compress well below 1×) with a
     * margin for the INSERT statement overhead this dumper emits.
     */
    private const PREFLIGHT_DB_ROLLBACK_MULTIPLIER = 1.2;

    /**
     * GH #146 — default ceiling (bytes) above which the pre-restore DB dump
     * is skipped rather than attempted. Overridable via the
     * `WPMGR_RESTORE_DB_ROLLBACK_MAX_BYTES` constant. 2 GiB default: large
     * enough that the vast majority of WP sites are covered, small enough
     * that a genuinely huge DB doesn't turn every restore into a second full
     * DB dump on top of the backup's own.
     */
    private const DEFAULT_DB_ROLLBACK_MAX_BYTES = 2 * 1024 * 1024 * 1024;

    /** Filename of the forced pre-restore DB dump inside the restore's scratch dir. */
    private const PRE_RESTORE_DB_DUMP_FILENAME = 'pre-restore-db.sql.gz';

    /** @var array<string,mixed> Runner params (see class docblock). */
    private array $params;

    /** Unix-seconds of the last DB write to last_progress_at (throttle). */
    private int $lastDbUpdate = 0;

    private ?ProgressClient $progressClient = null;

    /**
     * GH #146 — shutdown-time rollback backstop. Armed right before the
     * first destructive rename (`swap_files`/`swap_db`), marked clean only
     * once Gate 2 (`health_check_live`, the LAST health-check gate) passes
     * — it stays armed across Gate 1 (`health_check`) too. Null until
     * armed — a run that never reaches a destructive phase (e.g. a
     * preflight failure) never arms one.
     */
    private ?RestoreGuard $guard = null;

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
        @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running restore loop must not hit max_execution_time; @-guarded, no-op when disabled
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
                        // GH #146: arm the shutdown-rollback backstop right
                        // before the first destructive rename this run
                        // performs.
                        $this->armGuardOnce();
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
                        // GH #146: arm the shutdown-rollback backstop right
                        // before the first destructive rename this run
                        // performs (idempotent — a no-op if swap_files
                        // already armed it for a `full` restore).
                        $this->armGuardOnce();
                        $subState = $this->runSwapDb($subState);
                        $next     = self::PHASE_POST_HOOKS;
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_POST_HOOKS:
                        $subState = $this->runPostHooks($subState);
                        $next     = self::PHASE_HEALTH_CHECK;
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_HEALTH_CHECK:
                        // GH #146 Gate 1: in-process DB probe, still under
                        // maintenance. On a definitive fatal, runHealthCheck()
                        // performs the combined files+DB rollback itself and
                        // throws — caught by this method's own top-level
                        // catch below, which reports `failed` with the
                        // rollback detail. This phase NEVER falls through to
                        // maintenance_off/health_check_live/cleanup/completed
                        // on a fatal.
                        $subState = $this->runHealthCheck($subState);
                        $next     = self::PHASE_MAINTENANCE_OFF;
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_MAINTENANCE_OFF:
                        $subState = $this->runMaintenanceOff($subState);
                        $next     = self::PHASE_HEALTH_CHECK_LIVE;
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_HEALTH_CHECK_LIVE:
                        // GH #146 Gate 2: loopback WSOD probe against the
                        // REAL, post-maintenance site (see class docblock for
                        // why this can't run under maintenance). On a
                        // definitive fatal, runHealthCheckLive() re-enables
                        // maintenance, performs the combined rollback, drops
                        // maintenance again, and throws — same top-level
                        // catch handles it as Gate 1 does.
                        $subState = $this->runHealthCheckLive($subState);
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
            $this->cleanupOnCompleted($subState);
            $this->postProgress(self::PHASE_COMPLETED, [
                'ok'      => true,
                'summary' => 'restore completed',
            ]);

            return self::PHASE_COMPLETED;
        } catch (\Throwable $e) {
            \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: phase ' . $currentPhase . ' failed: ' . $e->getMessage());

            // GH #146: RestoreHealthCheckFailed carries which rollback legs
            // already completed (runHealthCheck() performs the combined
            // files+DB rollback BEFORE throwing) — surface it on the FAILED
            // row/progress event so the operator sees the site was reverted,
            // not just that the restore failed.
            $rolledBack = $e instanceof RestoreHealthCheckFailed ? $e->rolledBack() : null;

            // Best-effort: mark failed, drop maintenance, try to clean up
            // tmp DB tables. Failures in the cleanup are swallowed.
            try {
                $this->maintenanceOff();
            } catch (\Throwable $_) {
            }

            try {
                $failState = [
                    'last_error' => substr($e->getMessage(), 0, 240),
                    'failed_in'  => $currentPhase,
                ];
                if ($rolledBack !== null) {
                    $failState['rolled_back'] = $rolledBack;
                }
                $this->saveTaskState(self::PHASE_FAILED, $failState);
            } catch (\Throwable $_) {
            }
            try {
                $progressDetail = [
                    'stage'   => $currentPhase,
                    'message' => substr($e->getMessage(), 0, 240),
                ];
                if ($rolledBack !== null) {
                    $progressDetail['rolled_back'] = $rolledBack;
                }
                $this->postProgress(self::PHASE_FAILED, $progressDetail);
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

        // ADR-049: when the CP pre-computed the winning-set estimated_bytes for
        // a chain restore, use that figure for the staging leg. The manifest
        // entries for an incremental snapshot cover only the CHANGED files so
        // totalArtifactBytes() would drastically undercount the true footprint.
        $estimatedBytes = (int) ($this->params['estimated_bytes'] ?? 0);
        $isChainRestore = (bool) ($this->params['is_chain_restore'] ?? false);

        // Two-leg disk-free precheck — see PREFLIGHT_*_MULTIPLIER constants.
        // Leg 1: enough room for the downloaded artifacts + tmp tables.
        // Leg 2: enough room for the staging tree (same size as live
        // wp-content). We require max(leg1, leg2), not sum, because the legs
        // overlap in time — when staging is being filled, the artifacts on
        // disk are smaller than the bytes already extracted out of them.
        $legArtifact = (int) ($artifactsTotal * self::PREFLIGHT_ARTIFACT_MULTIPLIER);
        if ($isChainRestore && $estimatedBytes > 0) {
            // Use CP-provided estimated_bytes for the staging leg so the disk
            // preflight reflects the full winning file set size, not just the
            // bytes in this generation's incremental snapshot.
            $legStaging = (int) ($estimatedBytes * self::PREFLIGHT_STAGING_MULTIPLIER);
        } else {
            $legStaging = (int) (FilesRestorer::estimateWpContentBytes($wpContent) * self::PREFLIGHT_STAGING_MULTIPLIER);
        }
        // GH #146 / §2 — Leg 3: headroom for the forced pre-restore DB dump
        // `runSwapDb()` captures before the destructive DROP (see
        // `capturePreRestoreDbDump()`). Only contributes to `$required` when
        // it fits under the same ceiling that phase itself enforces — a DB
        // over the ceiling skips the dump entirely, so no disk headroom is
        // needed for it (and requiring it here would wrongly fail preflight
        // for a large-DB restore that never attempts the dump at all).
        $liveDbBytes = $this->estimateLiveDbBytes();
        $dbRollbackCeiling = $this->dbRollbackMaxBytes();
        $legDbRollback = ($liveDbBytes > 0 && $liveDbBytes <= $dbRollbackCeiling)
            ? (int) ($liveDbBytes * self::PREFLIGHT_DB_ROLLBACK_MULTIPLIER)
            : 0;

        $required = max($legArtifact, $legStaging) + $legDbRollback;

        // Disk free on wp-content's volume. disk_free_space returns the
        // bytes free for the filesystem the path lives on.
        $free = $wpContent !== '' && is_dir($wpContent) ? (int) @disk_free_space($wpContent) : 0;

        if ($required > 0 && $free > 0 && $free < $required) {
            // Operator-facing message — surfaces in the SSE phase_detail and
            // the WP error_log. GB units (not bytes) because that's the unit
            // operators reason about when looking at `df -h`.
            throw new \RuntimeException(sprintf(
                'Not enough free disk. Need ~%s GB, have %s GB. Free up space and retry, or restore to a different mount.',
                esc_html(self::formatGb($required)),
                esc_html(self::formatGb($free))
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
     * download_artifacts: pull every chunk into the per-artifact
     * `<scratch>/<logical_path>` file. Idempotent — already downloaded
     * artifacts are skipped on watchdog re-entry, and an artifact partially
     * downloaded before a stall resumes at the first unwritten chunk (not
     * chunk 0).
     *
     * ADR-036 P1 storage adapter: HOW a chunk's bytes are fetched depends on
     * this restore's `destination_kind`. `local` chunks live on this same
     * webserver's disk (no presigned_url in the manifest — see
     * fetchLocalChunk() / LocalDestination::getChunk()); every other kind
     * (`cp`, `s3_compat`, or absent = older CP builds) is unchanged: a
     * presigned GET, same as today. A pure `local` restore never constructs
     * the network transport at all (see the lazy `$transport` below).
     *
     * Per-chunk GETs (cp/s3_compat only) use the v0.8.6 retry-with-backoff
     * policy: up to DOWNLOAD_CHUNK_MAX_ATTEMPTS attempts with exponential
     * backoff. Network errors and 5xx/408/425/429 are retried; terminal 4xx
     * (404, 403, 400) are not — they're fatal-by-construction (the URL is the
     * same on every attempt). The error message attached on terminal failure
     * carries the HTTP status, the host of the presigned URL, the body
     * excerpt, and the attempt count, so the SSE `phase_detail.message` + the
     * WP error_log are actually grep-able by the operator. Local disk reads
     * have no such retry policy — none of the transient-network failure
     * modes a presigned-URL GET over a tunnel has apply to a local fread.
     *
     * @param array<string,mixed> $subState
     * @return array<string,mixed>
     */
    private function runDownloadArtifacts(array $subState): array
    {
        $destinationKind  = $this->destinationKind();
        $localDestination = $destinationKind === 'local' ? $this->buildLocalDestination() : null;

        // Presigned-URL transport + age-decrypt identity are only needed
        // when at least one chunk is NOT served by the local destination —
        // constructed lazily (on first use) so a pure `local` restore never
        // pays for Keystore/Signer setup or touches the network.
        $transport = null;
        $age       = null;

        $entries = $this->chunkDownloads();
        $total   = count($entries);
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
                throw new \RuntimeException('download_artifacts: entry ' . esc_html((string) $i) . ' missing logical_path');
            }
            if (!self::isSafeLogicalPath($logical)) {
                throw new \RuntimeException('download_artifacts: unsafe logical_path: ' . esc_html($logical));
            }

            $outPath = $this->scratchDir() . DIRECTORY_SEPARATOR . $logical;
            $outDir  = dirname($outPath);
            if (!is_dir($outDir) && !@mkdir($outDir, 0700, true) && !is_dir($outDir)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir -- explicit 0700 perms on scratch dir; wp_mkdir_p would apply the wider FS_CHMOD_DIR
                throw new \RuntimeException('download_artifacts: cannot create out dir: ' . esc_html($outDir));
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
                $handle = @fopen($outPath, 'r+b'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming handle for chunked fread/fwrite over multi-GB archives; WP_Filesystem exposes only whole-file get/put which would OOM
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
                $handle     = @fopen($outPath, 'wb'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming handle for chunked fread/fwrite over multi-GB archives; WP_Filesystem exposes only whole-file get/put which would OOM
            }
            if ($handle === false) {
                throw new \RuntimeException('download_artifacts: cannot open output file: ' . esc_html($outPath));
            }

            try {
                for ($chunkIdx = $startChunk; $chunkIdx < $chunkCount; $chunkIdx++) {
                    $chunk = $chunks[$chunkIdx];
                    if (!is_array($chunk)) {
                        throw new \RuntimeException('download_artifacts: malformed chunk at ' . $logical . '[' . $chunkIdx . ']');
                    }
                    $hash = (string) ($chunk['hash'] ?? $chunk['blake3'] ?? '');
                    if ($hash === '') {
                        throw new \RuntimeException('download_artifacts: chunk missing hash at ' . $logical . '[' . $chunkIdx . ']');
                    }

                    // ADR-036 P1: a 'local' snapshot's chunks live on this
                    // same webserver's disk — read them by content hash
                    // instead of an HTTP GET. cp/s3_compat chunks are
                    // unchanged: presigned GET, same as today.
                    if ($localDestination !== null) {
                        $bytes = $this->fetchLocalChunk($localDestination, $hash, $logical, $chunkIdx);
                    } else {
                        $url = (string) ($chunk['presigned_url'] ?? $chunk['url'] ?? $chunk['get_url'] ?? '');
                        if ($url === '') {
                            throw new \RuntimeException('download_artifacts: chunk missing url at ' . $logical . '[' . $chunkIdx . ']');
                        }
                        if ($transport === null) {
                            $transport = new BackupTransport(new Signer(new Keystore()));
                        }
                        $bytes = $this->fetchChunkWithRetries($transport, $url, $hash, $logical, $chunkIdx);
                    }

                    // Verify blake3 over the CIPHERTEXT (matches the upload
                    // pipeline's content-addressing scheme) regardless of
                    // which path the bytes came from.
                    $actual = Blake3::hashHex($bytes);
                    if (!hash_equals($hash, $actual)) {
                        throw new \RuntimeException('download_artifacts: blake3 mismatch on chunk ' . $hash);
                    }

                    // Decrypt if the backup pipeline was running in age
                    // mode. EncryptAndUpload::ENCRYPT_CHUNKS is the
                    // canonical signal — match it here so the .bin
                    // plaintext path (V0 default) just writes bytes.
                    $plain = EncryptAndUpload::ENCRYPT_CHUNKS
                        ? ($age ??= new AgeIdentity(new Keystore()))->decryptChunk($bytes)
                        : $bytes;

                    $w = @fwrite($handle, $plain); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fwrite -- incremental write into a streaming handle; WP_Filesystem put_contents is whole-buffer only
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
                fclose($handle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- closes a streaming handle over multi-GB archives; WP_Filesystem has no streaming API
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
                throw new \RuntimeException('verify_artifacts: missing artifact on disk: ' . esc_html((string) $logical));
            }
            if (@filesize($abs) === 0) {
                throw new \RuntimeException('verify_artifacts: empty artifact: ' . esc_html((string) $logical));
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
        $coreZips  = []; // Track A / A2 (#187): core.gNNN.partMMM.zip → extract to ABSPATH.
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
            $kind = $this->classifyArtifactKind($logical);
            if ($kind === 'core') {
                // Track A / A2 (#187): core parts are extracted to ABSPATH, not
                // the wp-content staging dir. Collect them separately so they
                // are NOT handed to FilesRestorer::stage() (which roots everything
                // under wp-content). runStageFiles extracts them directly into a
                // staging copy of ABSPATH in a second pass below.
                $coreZips[] = $abs;
                $componentsPresent['core'] = true;
                continue;
            }
            $zips[] = $abs;
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
        // Require at least one zip when no core-only restore is happening.
        // A core-only restore (coreZips non-empty, zips empty) is valid.
        if ($zips === [] && $coreZips === []) {
            throw new \RuntimeException('stage_files: no .zip parts in artifact list');
        }

        $restoreId = $this->restoreId();
        $target    = $this->wpContentPath();
        if ($target === '' && $zips !== []) {
            throw new \RuntimeException('stage_files: wp_content_path is empty');
        }

        $restorer   = new FilesRestorer();
        $stagingDir = '';
        if ($zips !== []) {
            if ($target === '') {
                throw new \RuntimeException('stage_files: wp_content_path is empty');
            }
            $stagingDir = $restorer->stage($zips, $target, $restoreId, function (string $phase, array $detail): void {
                $this->onPhaseProgress($phase, $detail);
            });
        }

        // Track A / A2 (#187): extract core parts into a staging directory
        // rooted at ABSPATH. The staging dir is a sibling of the ABSPATH dir
        // named `.wpmgr-core-staging-<restoreId>/` so the extract is atomic-
        // rename-safe. We extract by opening each zip and copying each entry
        // (using ZipArchive) into the staging dir preserving the relative path.
        // The swap phase will rename the staging dir over ABSPATH.
        $coreStagingDir = '';
        if ($coreZips !== []) {
            $absPath = $this->wpRoot();
            if ($absPath === '' || !is_dir($absPath)) {
                // ABSPATH not available — skip core extraction (non-fatal: the
                // backup still contains the core archive; the operator can
                // manually extract it). Log and continue.
                \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: ABSPATH not available for core extraction — skipping core zip extraction');
            } else {
                $coreStagingDir = $this->extractCorePartsToStaging($coreZips, $absPath, $restoreId);
            }
        }

        // ADR-049: tombstone delete pass — runs AFTER artifact extraction, BEFORE
        // swap. Only active when is_chain_restore=true and tombstone_paths is
        // non-empty. Each path is independently sanitized; a bad path is skipped
        // (Rule 9: tombstone delete errors are non-fatal).
        $tombstonesDeleted = 0;
        $tombstoneErrors   = 0;
        $isChainRestore    = (bool) ($this->params['is_chain_restore'] ?? false);
        $tombstonePaths    = isset($this->params['tombstone_paths']) && is_array($this->params['tombstone_paths'])
            ? $this->params['tombstone_paths']
            : [];

        if ($isChainRestore && $tombstonePaths !== [] && $stagingDir !== '') {
            $stagingRoot = realpath($stagingDir);
            if ($stagingRoot !== false) {
                foreach ($tombstonePaths as $rawPath) {
                    if (!is_string($rawPath)) {
                        $tombstoneErrors++;
                        continue;
                    }
                    $safePath = $this->sanitizeTombstonePath($rawPath, $stagingRoot);
                    if ($safePath === null) {
                        // Path does not exist in staging (already absent) or
                        // was rejected by a sanitization rule — either way
                        // it is not an error that should abort the restore.
                        continue;
                    }
                    if (is_file($safePath)) {
                        if (wp_delete_file($safePath) || !file_exists($safePath)) {
                            $tombstonesDeleted++;
                        } else {
                            \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: tombstone unlink failed: ' . $safePath);
                            $tombstoneErrors++;
                        }
                    } elseif (is_dir($safePath)) {
                        // Recursively remove the directory — mirrors the cleanup
                        // helper used elsewhere in the runner.
                        $this->rrmdir($safePath);
                        // rrmdir is best-effort; count the directory as deleted
                        // if the path is now gone.
                        if (!is_dir($safePath)) {
                            $tombstonesDeleted++;
                        } else {
                            \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: tombstone rmdir failed: ' . $safePath);
                            $tombstoneErrors++;
                        }
                    }
                    // If neither file nor dir: path was already absent in
                    // staging — this is a normal no-op, not an error.
                }
            }

            $this->onPhaseProgress(self::PHASE_STAGE_FILES, [
                'tombstones_deleted' => $tombstonesDeleted,
                'tombstone_errors'   => $tombstoneErrors,
            ]);
        }

        $subState['stage'] = [
            'done'                 => true,
            'staging_dir'          => $stagingDir,
            'core_staging_dir'     => $coreStagingDir,  // Track A / A2 (#187): '' when no core parts.
            'components_present'   => array_keys($componentsPresent),
            'has_legacy_file_kind' => $hasLegacyFileEntry,
            // ADR-049: tombstone pass counts (zero for non-chain restores).
            'tombstones_deleted'   => $tombstonesDeleted,
            'tombstone_errors'     => $tombstoneErrors,
        ];
        return $subState;
    }

    /**
     * Track A / A2 (#187): extract core.gNNN.partMMM.zip archives into a
     * staging directory under ABSPATH. Returns the absolute path of the
     * staging directory (a `.wpmgr-core-staging-<restoreId>/` sibling of
     * ABSPATH). Returns '' on failure (non-fatal; caller logs and skips).
     *
     * The staging directory is then used by runSwapFiles to overlay ABSPATH
     * atomically. Each zip entry relative path (e.g. `wp-admin/foo.php`) is
     * extracted directly to `<coreStagingDir>/wp-admin/foo.php`.
     *
     * @param list<string> $coreZips  Absolute paths of core part archives.
     * @param string       $absPath   Absolute path of ABSPATH (the WordPress root).
     * @param string       $restoreId Restore run ID (used for staging dir name).
     * @return string Absolute path of the core staging dir, or '' on failure.
     */
    private function extractCorePartsToStaging(array $coreZips, string $absPath, string $restoreId): string
    {
        if (!class_exists(\ZipArchive::class)) {
            \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: ext-zip unavailable — cannot extract core parts');
            return '';
        }

        $short = substr(preg_replace('/[^a-f0-9]/i', '', $restoreId) ?? '', 0, 8);
        if ($short === '') {
            $short = substr(bin2hex(random_bytes(4)), 0, 8);
        }
        $stagingDir = dirname(rtrim($absPath, DIRECTORY_SEPARATOR)) . DIRECTORY_SEPARATOR . '.wpmgr-core-staging-' . $short;

        if (!is_dir($stagingDir) && !wp_mkdir_p($stagingDir) && !is_dir($stagingDir)) {
            \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: cannot create core staging dir: ' . $stagingDir);
            return '';
        }

        foreach ($coreZips as $zipPath) {
            if (!is_file($zipPath)) {
                continue;
            }
            $zip = new \ZipArchive();
            if ($zip->open($zipPath) !== true) {
                \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: cannot open core zip: ' . $zipPath);
                continue;
            }
            for ($i = 0; $i < $zip->numFiles; $i++) {
                $name = (string) $zip->getNameIndex($i);
                if ($name === '' || $name === false) {
                    continue;
                }
                // Sanitize: reject absolute paths, dotdot segments, NUL bytes.
                if (!self::isSafeLogicalPath($name)) {
                    \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: unsafe core zip entry rejected: ' . substr($name, 0, 120));
                    continue;
                }
                // Directories end with '/' — create them and skip file write.
                if (substr($name, -1) === '/') {
                    $dir = $stagingDir . DIRECTORY_SEPARATOR . str_replace('/', DIRECTORY_SEPARATOR, rtrim($name, '/'));
                    if (!is_dir($dir)) {
                        wp_mkdir_p($dir);
                    }
                    continue;
                }
                $destPath = $stagingDir . DIRECTORY_SEPARATOR . str_replace('/', DIRECTORY_SEPARATOR, $name);
                $destDir  = dirname($destPath);
                if (!is_dir($destDir) && !wp_mkdir_p($destDir) && !is_dir($destDir)) {
                    \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: cannot create core staging subdir: ' . $destDir);
                    continue;
                }
                $bytes = $zip->getFromIndex($i);
                if ($bytes === false) {
                    \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: failed to read core zip entry: ' . $name);
                    continue;
                }
                @file_put_contents($destPath, $bytes); // phpcs:ignore WordPress.WP.AlternativeFunctions.WriteFile.ABSPATHDetected,PluginCheck.CodeAnalysis.WriteFile.ABSPATHDetected -- restore/quarantine engine intentionally writes under ABSPATH (the live WP tree); relocating would defeat the restore
            }
            $zip->close();
        }

        return $stagingDir;
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
        $stage          = isset($subState['stage']) && is_array($subState['stage']) ? $subState['stage'] : [];
        $stagingDir     = (string) ($stage['staging_dir'] ?? '');
        $coreStagingDir = (string) ($stage['core_staging_dir'] ?? ''); // Track A / A2 (#187).
        $componentsPresent  = isset($stage['components_present']) && is_array($stage['components_present'])
            ? array_values(array_filter($stage['components_present'], 'is_string'))
            : [];
        $hasLegacyFileKind = !empty($stage['has_legacy_file_kind']);

        // Track A / A2 (#187): apply the core overlay to ABSPATH. This copies
        // staged core files (from extractCorePartsToStaging) into the live
        // ABSPATH, preserving existing files that are not in the snapshot (a
        // targeted overlay, not a full swap). wp-config.php is overlaid without
        // prompting — the operator chose include_core=true explicitly.
        $coreOverlayDone = false;
        if ($coreStagingDir !== '' && is_dir($coreStagingDir)) {
            $absPath = $this->wpRoot();
            if ($absPath !== '' && is_dir($absPath)) {
                $this->overlayCoreStaging($coreStagingDir, $absPath);
                $coreOverlayDone = true;
            }
            // Best-effort cleanup of the core staging dir after overlay.
            $this->rrmdir($coreStagingDir);
        }

        // When the restore is core-only (no wp-content zips at all), skip the
        // wp-content staging/swap phases and return early.
        if ($stagingDir === '') {
            if ($coreOverlayDone) {
                $subState['swap_files'] = [
                    'done'             => true,
                    'mode'             => 'core_only_overlay',
                    'core_overlay_done' => true,
                ];
                return $subState;
            }
            throw new \RuntimeException('swap_files: staging_dir missing from sub_state');
        }

        // Reconcile components_present (recorded by runStageFiles from artifact
        // filename prefixes) against on-disk staging. A part archive whose
        // contents were entirely DEFAULT_EXCLUDES still gets emitted as a zip,
        // so the component appears in components_present even though FilesRestorer
        // extracted nothing into the corresponding staging subdir. Without this
        // reconciliation, swapComponents() throws
        // "staging missing component subdir: …/plugins" mid-restore (the
        // 2026-05-29 v0.9.6 SSE failure).
        // Also strip 'core' from the wp-content components_present set — core
        // is handled above and must not confuse the wp-content swap logic.
        $stagingSubdirs = [
            'plugin' => 'plugins',
            'theme'  => 'themes',
            'upload' => 'uploads',
        ];
        $componentsPresent = array_values(array_filter(
            $componentsPresent,
            static function (string $c) use ($stagingDir, $stagingSubdirs): bool {
                // 'core' is handled by the ABSPATH overlay above — never pass
                // it to the wp-content swapper.
                if ($c === 'core') {
                    return false;
                }
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
        $allFour    = ['plugin', 'theme', 'upload', 'wp-content'];
        $hasAllFour = !array_diff($allFour, $componentsPresent);
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
                'done'              => true,
                'old_files_dir'     => $oldDir,
                'mode'              => $hasLegacyFileKind ? 'legacy_whole' : 'whole_all_components',
                'core_overlay_done' => $coreOverlayDone,
            ];
            return $subState;
        }

        // Path 2: per-component swap. componentsPresent is the subset the CP
        // filtered down for us.
        if ($componentsPresent === []) {
            if ($coreOverlayDone) {
                // Core-only restore already handled above; no wp-content swap needed.
                $subState['swap_files'] = [
                    'done'              => true,
                    'mode'              => 'core_overlay_only',
                    'core_overlay_done' => true,
                ];
                return $subState;
            }
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
            'done'              => true,
            'mode'              => 'per_component',
            'components'        => $componentsPresent,
            'old_dirs'          => isset($result['old_dirs']) && is_array($result['old_dirs']) ? $result['old_dirs'] : [],
            'core_overlay_done' => $coreOverlayDone,
        ];
        return $subState;
    }

    /**
     * Track A / A2 (#187): overlay the staged core files onto the live ABSPATH.
     * Copies every file from $coreStagingDir into $absPath, creating
     * subdirectories as needed. Existing ABSPATH files not in the staging dir
     * are left untouched (targeted overlay, not a full swap).
     *
     * wp-config.php is overlaid without prompting — the operator explicitly
     * requested include_core=true.
     *
     * Best-effort: individual file errors are logged and skipped rather than
     * aborting the whole restore.
     *
     * @param string $coreStagingDir Absolute path of the core staging dir.
     * @param string $absPath        Absolute path of ABSPATH (the WordPress root).
     */
    private function overlayCoreStaging(string $coreStagingDir, string $absPath): void
    {
        $srcLen  = strlen(rtrim($coreStagingDir, DIRECTORY_SEPARATOR)) + 1;
        $absPath = rtrim($absPath, DIRECTORY_SEPARATOR);

        try {
            $iterator = new \RecursiveIteratorIterator(
                new \RecursiveDirectoryIterator(
                    $coreStagingDir,
                    \FilesystemIterator::SKIP_DOTS | \FilesystemIterator::UNIX_PATHS
                )
            );
        } catch (\UnexpectedValueException $e) {
            \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: cannot iterate core staging dir: ' . $e->getMessage());
            return;
        }

        /** @var \SplFileInfo $info */
        foreach ($iterator as $info) {
            if (!$info->isFile() || is_link((string) $info->getPathname())) {
                continue;
            }
            $rel  = substr((string) $info->getPathname(), $srcLen);
            $dest = $absPath . DIRECTORY_SEPARATOR . str_replace('/', DIRECTORY_SEPARATOR, $rel);
            $dir  = dirname($dest);
            if (!is_dir($dir) && !wp_mkdir_p($dir) && !is_dir($dir)) {
                \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: cannot create ABSPATH subdir during core overlay: ' . $dir);
                continue;
            }
            if (!@copy((string) $info->getPathname(), $dest)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.WriteFile.ABSPATHDetected,PluginCheck.CodeAnalysis.WriteFile.ABSPATHDetected -- restore/quarantine engine intentionally writes under ABSPATH (the live WP tree); relocating would defeat the restore
                \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: core overlay copy failed: ' . $rel);
            }
        }
    }

    /**
     * Classify a downloaded artifact filename into the Track-5 component
     * kind. Mirror of EncryptAndUpload::entryKind for the inverse direction:
     * given a logical path on the wire, return its component bucket.
     *
     * Track A / A2 (#187): `core.gNNN.partMMM.zip` maps to 'core'. Core parts
     * are routed to ABSPATH by runStageFiles; they must NOT fall through to the
     * legacy 'file' bucket (which would trigger the whole-wp-content swap instead
     * of the ABSPATH overlay).
     *
     * @param string $logical Logical artifact path from the CP plan.
     * @return string 'plugin' | 'theme' | 'upload' | 'wp-content' | 'core' |
     *                'file' | 'db' | 'inspection' | ''
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
        // Track A / A2 (#187): CoreFilesArchiver emits `core.gNNN.partMMM.zip`.
        // Classify before the FilesArchiver generic check so 'core' is never
        // misclassified as 'file' (the legacy fallback) even when componentKindFromPartName
        // cannot match it (COMPONENT_PARTITIONS does not include 'core').
        if (preg_match('/^core\.(g\d+\.)?part\d+\.zip$/i', $lower) === 1) {
            return 'core';
        }
        // Track 5 per-component archives. FilesArchiver emits generation-
        // namespaced `<component>.gNNN.partMMM.zip`; classify via the shared
        // classifier (tolerant of both the namespaced and legacy part names)
        // so the namespaced part maps to its component on the restore overlay.
        $component = FilesArchiver::componentKindFromPartName($logical);
        if ($component !== '') {
            return $component;
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
     * Cross-environment restore: builds the serialization-safe replacement set
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
     *
     * GH #146: before the destructive DROP TABLE inside `DbRestorer::swap()`,
     * captures a forced pre-restore DB dump — the rollback source a
     * post-restore health-check failure (or the RestoreGuard shutdown
     * backstop) replays to undo this swap. See `capturePreRestoreDbDump()`.
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

        // GH #146 / §2: forced pre-restore DB dump, BEFORE the first DROP.
        // Persisted immediately (not just returned) so a crash between the
        // dump and the swap still leaves the rollback source recorded for a
        // watchdog resume, the health-check rollback, or the RestoreGuard
        // shutdown backstop to find.
        $subState = $this->capturePreRestoreDbDump($subState, $targetPrefix);
        $this->saveTaskState(self::PHASE_SWAP_DB, $subState);

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
     * GH #146 / §2: capture the live (pre-restore) database into a
     * `pre-restore-db.sql.gz` dump inside this restore's scratch dir, using
     * the same streaming `DbDumper` backups + Database Snapshots use.
     * Idempotent — a watchdog resume that already captured (or already
     * recorded as skipped/oversized) the dump does not redo it.
     *
     * ALWAYS-ON with a size ceiling (`WPMGR_RESTORE_DB_ROLLBACK_MAX_BYTES`,
     * default 2 GiB): when the live DB exceeds it, the dump is skipped and
     * `db_rollback.available=false` is recorded as a WARNING-carrying
     * marker — never silently proceeding unprotected without a trace (S8,
     * issue #131's discipline). A health-check failure downstream then
     * rolls back files ONLY.
     *
     * @param array<string,mixed> $subState
     * @return array<string,mixed>
     */
    private function capturePreRestoreDbDump(array $subState, string $targetPrefix): array
    {
        $existing = isset($subState['db_rollback']) && is_array($subState['db_rollback']) ? $subState['db_rollback'] : [];
        if (!empty($existing['done'])) {
            return $subState;
        }

        $scratch = $this->scratchDir();
        if ($scratch === '' || !is_dir($scratch)) {
            $subState['db_rollback'] = ['done' => true, 'available' => false, 'reason' => 'unavailable'];
            $this->postProgress(self::PHASE_SWAP_DB, [
                'db_rollback' => 'unavailable',
                'reason'      => 'no scratch dir available for the pre-restore dump',
            ]);
            return $subState;
        }

        $maxBytes  = $this->dbRollbackMaxBytes();
        $liveBytes = $this->estimateLiveDbBytes();
        if ($maxBytes > 0 && $liveBytes > 0 && $liveBytes > $maxBytes) {
            $subState['db_rollback'] = [
                'done'       => true,
                'available'  => false,
                'reason'     => 'unavailable',
                'live_bytes' => $liveBytes,
                'max_bytes'  => $maxBytes,
            ];
            $this->postProgress(self::PHASE_SWAP_DB, [
                'db_rollback' => 'unavailable',
                'reason'      => 'live database exceeds WPMGR_RESTORE_DB_ROLLBACK_MAX_BYTES ceiling',
                'live_bytes'  => $liveBytes,
                'max_bytes'   => $maxBytes,
            ]);
            return $subState;
        }

        $dumpPath = $scratch . DIRECTORY_SEPARATOR . self::PRE_RESTORE_DB_DUMP_FILENAME;
        try {
            $dumper = new DbDumper($this->dbCreds());
            // No incremental progress surfaced for this internal safety
            // dump — it rides inside the swap_db phase's own progress
            // budget; the noop callback keeps DbDumper's per-table
            // checkpoint cheap.
            $dumper->dump($dumpPath, [], static function (string $phase, array $detail): void {
            });

            $subState['db_rollback'] = [
                'done'      => true,
                'available' => true,
                'dump_path' => $dumpPath,
                'prefix'    => $targetPrefix,
            ];
            $this->postProgress(self::PHASE_SWAP_DB, [
                'db_rollback' => 'captured',
                'dump_bytes'  => @filesize($dumpPath) ?: 0,
            ]);
        } catch (\Throwable $e) {
            // A failed safety dump must not block the restore itself — but
            // it MUST be recorded (never silently proceed unprotected).
            \WPMgr\Agent\Support\DebugLog::write(
                'WPMgr RestoreRunner: pre-restore DB dump failed, proceeding without a DB rollback source: ' . $e->getMessage()
            );
            $subState['db_rollback'] = ['done' => true, 'available' => false, 'reason' => 'unavailable'];
            $this->postProgress(self::PHASE_SWAP_DB, [
                'db_rollback' => 'unavailable',
                'reason'      => 'pre-restore dump failed: ' . substr($e->getMessage(), 0, 160),
            ]);
        }

        return $subState;
    }

    /**
     * GH #146: `WPMGR_RESTORE_DB_ROLLBACK_MAX_BYTES` override, else the
     * 2 GiB default.
     */
    private function dbRollbackMaxBytes(): int
    {
        if (defined('WPMGR_RESTORE_DB_ROLLBACK_MAX_BYTES')) {
            $configured = (int) constant('WPMGR_RESTORE_DB_ROLLBACK_MAX_BYTES');
            if ($configured > 0) {
                return $configured;
            }
        }
        return self::DEFAULT_DB_ROLLBACK_MAX_BYTES;
    }

    /**
     * GH #146: estimate the on-disk size (bytes) of the LIVE database via
     * `information_schema.tables` — the same tables `capturePreRestoreDbDump()`
     * is about to dump (swap_db hasn't renamed anything onto them yet).
     * Returns 0 on any failure (conservative: treated as "small enough" by
     * the caller so a broken size probe never blocks the safety dump).
     */
    private function estimateLiveDbBytes(): int
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return 0;
        }
        $dbName = $this->dbCreds()['name'] ?? '';
        if ($dbName === '') {
            return 0;
        }
        try {
            /** @phpstan-ignore-next-line — $wpdb is a runtime interface. */
            $prepared = $wpdb->prepare( // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- already prepared on the preceding line
                'SELECT SUM(data_length + index_length) FROM information_schema.tables WHERE table_schema = %s',
                $dbName
            );
            /** @phpstan-ignore-next-line */
            $bytes = $wpdb->get_var($prepared); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- information_schema size probe; no core helper exists; not a cacheable value (must reflect live size right before the dump)
            return is_numeric($bytes) ? (int) $bytes : 0;
        } catch (\Throwable $e) {
            return 0;
        }
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
     * health_check (GH #146 Gate 1): in-process DB probe (Probe A ONLY —
     * see class docblock for why Probe B/the loopback WSOD gate is deferred
     * to Gate 2/`health_check_live`). Runs BEFORE maintenance_off, so a
     * rollback it triggers happens invisibly while the site is still in
     * maintenance mode. On a definitive fatal, performs the combined
     * files+DB rollback (`performRollback()`, via `routeRollback()`) BEFORE
     * throwing — this method must never return normally on a failing
     * verdict, so the dispatch loop can never fall through to
     * maintenance_off/health_check_live/cleanup/completed on an unhealthy
     * restore.
     *
     * Does NOT call `markClean()` on a pass — Gate 2 (`health_check_live`)
     * is the LAST checkpoint before `completed`, so the shutdown guard stays
     * armed across both gates; only Gate 2 passing marks it clean.
     *
     * @param array<string,mixed> $subState
     * @return array<string,mixed>
     * @throws RestoreHealthCheckFailed On a definitive health failure,
     *         AFTER the rollback has already been performed.
     */
    private function runHealthCheck(array $subState): array
    {
        $result = (new RestoreHealthCheck($this->wpRoot()))->checkDatabase();

        $this->postProgress(self::PHASE_HEALTH_CHECK, [
            'ok'       => $result['ok'],
            'failures' => $result['failures'],
        ]);

        if ($result['ok']) {
            $subState['health_check'] = ['done' => true, 'ok' => true];
            return $subState;
        }

        // Definitive DB failure — roll back BEFORE throwing (invisibly,
        // still under maintenance mode at this point in the phase order).
        $rolledBack = $this->routeRollback($subState);
        if ($rolledBack['reason'] === '') {
            $rolledBack['reason'] = 'db_unhealthy_post_restore';
        }

        throw new RestoreHealthCheckFailed(
            // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- exception message, goes to server log/SSE, never browser output
            'post-restore health check failed: ' . implode('; ', $result['failures']) . ' (' . $rolledBack['reason'] . ')',
            $rolledBack // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- not output at all; the rollback-legs array the top-level catch persists to the task row
        );
    }

    /**
     * health_check_live (GH #146 Gate 2): the loopback WSOD probe (Probe B
     * ONLY — Probe A already passed in Gate 1) against the REAL,
     * post-maintenance site. Runs right AFTER maintenance_off — see class
     * docblock for why a loopback probe run any earlier physically cannot
     * observe a files-side WSOD (WordPress's own `.maintenance` 503 page
     * renders before plugins even load).
     *
     * On a definitive fatal: re-enables maintenance for the duration of the
     * rollback (so visitors don't see the broken site being reverted — see
     * `performRollback()`'s own maintenance-toggle wrapper, which covers
     * this uniformly for both the synchronous path here and a real
     * shutdown-triggered `RestoreGuard::fire()`), then throws.
     *
     * On ok/inconclusive: marks the guard clean (this IS the last gate
     * before `completed`) and proceeds to cleanup. The brief window a
     * genuinely-broken restore was visitor-visible on the real site before
     * THIS phase caught it is an accepted trade-off — strictly better than
     * leaving it broken forever, and it never happens for a good restore.
     *
     * @param array<string,mixed> $subState
     * @return array<string,mixed>
     * @throws RestoreHealthCheckFailed On a definitive WSOD, AFTER the
     *         rollback has already been performed.
     */
    private function runHealthCheckLive(array $subState): array
    {
        $result = (new RestoreHealthCheck($this->wpRoot()))->checkLoopbackOnly();

        $this->postProgress(self::PHASE_HEALTH_CHECK_LIVE, [
            'ok'       => $result['ok'],
            'failures' => $result['failures'],
            'warnings' => $result['warnings'],
        ]);

        if ($result['ok']) {
            // Verified-good on the REAL site — the LAST gate before
            // completed. Never let a later, unrelated shutdown roll back a
            // confirmed-healthy restore (mirrors UpdateGuard::markClean()'s
            // rationale exactly).
            if ($this->guard !== null) {
                $this->guard->markClean();
            }
            $subState['health_check_live'] = [
                'done'     => true,
                'ok'       => true,
                'warnings' => $result['warnings'],
            ];
            return $subState;
        }

        // Definitive WSOD on the real, post-maintenance site (the GH #147
        // class this gate exists to catch). performRollback() itself wraps
        // the actual revert in maintenanceOn()/maintenanceOff() so visitors
        // never see the broken site mid-revert.
        $rolledBack = $this->routeRollback($subState);
        if ($rolledBack['reason'] === '') {
            $rolledBack['reason'] = 'wsod_post_restore';
        }

        throw new RestoreHealthCheckFailed(
            // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- exception message, goes to server log/SSE, never browser output
            'post-restore health check failed: ' . implode('; ', $result['failures']) . ' (' . $rolledBack['reason'] . ')',
            $rolledBack // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- not output at all; the rollback-legs array the top-level catch persists to the task row
        );
    }

    /**
     * GH #146 / Review MEDIUM-3: route a definitive health-check failure
     * (from EITHER gate) through the shutdown-guard's `fire()` when one is
     * armed, rather than calling `performRollback()` directly, so its
     * documented `fired` idempotency actually engages. Without this, the
     * guard's `fired` flag would stay false after a successful synchronous
     * rollback, and a later REAL process-shutdown invocation of the same
     * registered callback (`markClean()` is never called on a failing
     * verdict) would attempt a SECOND, redundant rollback against
     * already-consumed rollback material.
     *
     * @param array<string,mixed> $subState
     * @return array{files:bool,db:bool,reason:string}
     */
    private function routeRollback(array $subState): array
    {
        if ($this->guard !== null) {
            $fired = $this->guard->fire();
            return ['files' => $fired['files'], 'db' => $fired['db'], 'reason' => $fired['reason']];
        }
        return $this->performRollback($subState);
    }

    /**
     * GH #146 / §3+§4: combined files+DB rollback, shared by both gates'
     * synchronous failure handling (via `routeRollback()`) and a real
     * shutdown-triggered `RestoreGuard::fire()` (same closure,
     * `armGuardOnce()`). Order: a mismatch guard first (see below), then
     * DB, then files, then a quick Probe A re-check (informational only —
     * its result is logged, never thrown). Never throws — every leg is
     * independently try/catch-wrapped so one leg failing does not prevent
     * the other from being attempted.
     *
     * Review MEDIUM-4: if the live DB WAS actually swapped as part of this
     * restore (`swap_db` ran) but no rollback source is available (the dump
     * was skipped over the size ceiling, or the dump itself failed),
     * reverting FILES ALONE would run the pre-restore files against the
     * NEW (post-restore) database — a worse, incoherent "Frankenstein"
     * mismatch than simply leaving the as-restored (if unhealthy) site in
     * place. In that specific case this method performs NEITHER leg (and
     * skips the maintenance toggle below entirely — nothing is being
     * mutated, so there is nothing to hide) and reports
     * `reason: 'db_rollback_unavailable'` so the operator gets a coherent
     * (if broken) site plus whatever rollback material does exist on disk
     * to investigate by hand — never a mismatched one. This does NOT apply
     * to a `files`-kind restore (no `swap_db` ever ran, so there is nothing
     * for the files revert to mismatch against).
     *
     * GH #146 two-gate follow-up: the actual revert (DB + files) is wrapped
     * in `maintenanceOn()` / `finally maintenanceOff()` UNCONDITIONALLY —
     * this is what protects visitors during a Gate-2 (post-maintenance-off)
     * rollback, and it's a harmless no-op re-enable/immediate-disable
     * during a Gate-1 rollback (maintenance is already on there; the
     * existing top-level `run()` catch drops it again regardless). Wrapping
     * it HERE (rather than duplicating the toggle in both
     * `runHealthCheckLive()` and `armGuardOnce()`'s closure) means a real
     * shutdown-triggered `RestoreGuard::fire()` gets the SAME visitor
     * protection as the synchronous path, with no separate handling needed.
     *
     * @param array<string,mixed> $subState Freshest known sub_state. The
     *        synchronous health-check path passes the in-memory one;
     *        `RestoreGuard`'s closure reloads fresh from the DB at
     *        fire()-time since further phases may have run since arm().
     * @return array{files:bool,db:bool,reason:string} Which legs actually
     *         completed; `reason` is non-empty only when neither leg was
     *         even attempted (the mismatch guard above).
     */
    private function performRollback(array $subState): array
    {
        $dbRollback   = isset($subState['db_rollback']) && is_array($subState['db_rollback']) ? $subState['db_rollback'] : [];
        $dbWasSwapped = !empty($subState['swap_db']['done']);
        $dumpPath     = (string) ($dbRollback['dump_path'] ?? '');
        $dbRollbackAvailable = !empty($dbRollback['available']) && $dumpPath !== '' && is_file($dumpPath);

        if ($dbWasSwapped && !$dbRollbackAvailable) {
            \WPMgr\Agent\Support\DebugLog::write(
                'WPMgr RestoreRunner: skipping rollback — the live DB was swapped but no rollback source is '
                . 'available; reverting files alone would mismatch the pre-restore files against the new DB'
            );
            return ['files' => false, 'db' => false, 'reason' => 'db_rollback_unavailable'];
        }

        $dbOk    = false;
        $filesOk = false;

        // Protect visitors for the duration of the actual revert — dropped
        // again in the finally below regardless of outcome. See method doc
        // for why this is unconditional (both gates, sync AND async fire).
        $this->maintenanceOn();
        try {
            // --- 1. DB revert ------------------------------------------------
            if ($dbRollbackAvailable) {
                $dbOk = $this->revertDb($dumpPath);
            }

            // --- 2. Files revert -----------------------------------------------
            $swap = isset($subState['swap_files']) && is_array($subState['swap_files']) ? $subState['swap_files'] : [];
            try {
                $restorer = new FilesRestorer();
                if (isset($swap['old_files_dir']) && is_string($swap['old_files_dir']) && $swap['old_files_dir'] !== '') {
                    $restorer->revertSwap($this->wpContentPath(), $swap['old_files_dir'], $this->restoreId());
                    $filesOk = true;
                } elseif (isset($swap['old_dirs']) && is_array($swap['old_dirs']) && $swap['old_dirs'] !== []) {
                    /** @var array<string,string> $oldDirs */
                    $oldDirs = $swap['old_dirs'];
                    $restorer->revertSwapComponents($this->wpContentPath(), $oldDirs, $this->restoreId());
                    $filesOk = true;
                }
            } catch (\Throwable $e) {
                \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: files rollback failed: ' . $e->getMessage());
            }
        } finally {
            $this->maintenanceOff();
        }

        // --- 3. Quick Probe A re-check (informational only) ----------------
        try {
            $recheck = (new RestoreHealthCheck($this->wpRoot()))->checkDatabase();
            if (!$recheck['ok']) {
                \WPMgr\Agent\Support\DebugLog::write(
                    'WPMgr RestoreRunner: post-rollback DB re-check still unhealthy: ' . implode('; ', $recheck['failures'])
                );
            }
        } catch (\Throwable $_) {
        }

        return ['files' => $filesOk, 'db' => $dbOk, 'reason' => ''];
    }

    /**
     * GH #146: replay the pre-restore dump exactly as
     * `DbSnapshotCommand::actionRevert()` does — `new DbRestorer($creds)` ->
     * `restore()` into a fresh tmp prefix -> `swap()` back onto the live
     * prefix, `dropTmpTables()` on error. Same source/target prefix on
     * purpose: the dump was taken FROM this site's own live tables, so
     * reverting lands back onto those same tables.
     */
    private function revertDb(string $dumpPath): bool
    {
        $creds     = $this->dbCreds();
        $srcPrefix = $this->targetPrefix();
        $tmpPrefix = 'wpmrb' . substr(bin2hex(random_bytes(6)), 0, 10) . '_';
        $restorer  = new DbRestorer($creds);
        $noop      = static function (string $phase, array $detail): void {
        };

        try {
            $tmpTables = $restorer->restore($dumpPath, $tmpPrefix, $srcPrefix, $noop);
            $restorer->swap($tmpPrefix, $srcPrefix, $tmpTables, $noop);
            return true;
        } catch (\Throwable $e) {
            \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: DB rollback failed: ' . $e->getMessage());
            try {
                $restorer->dropTmpTables($tmpPrefix);
            } catch (\Throwable $_) {
            }
            return false;
        }
    }

    /**
     * GH #146 / §4: arm the RestoreGuard shutdown backstop exactly once per
     * run() invocation, right before the first destructive rename
     * (`swap_files`/`swap_db`). The rollback closure reloads sub_state
     * fresh from the DB at fire()-time (not the value captured here) since
     * later phases may have recorded MORE rollback material (e.g. the DB
     * dump path, or the files old-dir) between arming and a crash.
     */
    private function armGuardOnce(): void
    {
        if ($this->guard !== null) {
            return;
        }
        $this->guard = new RestoreGuard(function (): array {
            $task     = $this->loadTask();
            $subState = $task !== null ? $task['sub_state'] : [];
            return $this->performRollback($subState);
        });
        $this->guard->arm();
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
     * `.wpmgr-old-files-<id>/` rollback tree (+ the GH #146 pre-restore DB
     * dump, if one was captured).
     *
     * The rollback tree is the live wp-content that swap_files moved aside.
     * Pre-0.9.5 we kept it for 24h on every restore, which routinely tipped
     * small-VPS hosts into disk-red. 0.9.5 flipped the default to a
     * synchronous immediate rrmdir. GH #146 flips it again — to DEFER:
     *
     *   - `keep_old_files !== true` (DEFAULT): defer via a `<path>.expires`
     *     marker (`FilesRestorer::OLDFILES_GC_AGE_SECONDS`, ~1h) + a
     *     `wpmgr_restore_oldfiles_gc` cron sweep, rather than synchronously
     *     rrmdir-ing. The tree (and the pre-restore DB dump) must still
     *     exist for a health-check-triggered rollback to act on in the few
     *     minutes right after a restore reports its outcome — a synchronous
     *     delete here would remove the ONLY rollback source before that
     *     window even opens (health_check already runs BEFORE cleanup, so
     *     in practice a health-check rollback always precedes this method,
     *     but a manual/forensic revert shortly after `completed` needs the
     *     same material).
     *   - `keep_old_files === true`: unchanged — schedules the existing 24h
     *     GC window for operators who explicitly want a long manual-
     *     rollback window.
     *
     * Not a glob for the per-restore marker writes below — two concurrent
     * restores on the same host each write their OWN `.expires` marker
     * next to their OWN exact recorded path, so they never clobber each
     * other.
     */
    private function runCleanup(array $subState): array
    {
        // GH #146: the pre-restore DB dump (if captured) lives inside
        // scratch — carve it (and its own `.expires` marker, written below)
        // out of the artifact wipe so it survives the retention window.
        $dbRollback = isset($subState['db_rollback']) && is_array($subState['db_rollback']) ? $subState['db_rollback'] : [];
        $dumpPath   = (string) ($dbRollback['dump_path'] ?? '');
        $dumpBase   = $dumpPath !== '' && is_file($dumpPath) ? basename($dumpPath) : '';

        // 1. Remove the downloaded artifacts from scratch (keeping the
        // pre-restore DB dump, if any).
        $scratch = $this->scratchDir();
        if ($scratch !== '' && is_dir($scratch)) {
            $items = @scandir($scratch);
            if ($items !== false) {
                foreach ($items as $i) {
                    if ($i === '.' || $i === '..') {
                        continue;
                    }
                    if ($dumpBase !== '' && ($i === $dumpBase || $i === $dumpBase . '.expires')) {
                        continue;
                    }
                    $p = $scratch . DIRECTORY_SEPARATOR . $i;
                    if (is_file($p)) {
                        wp_delete_file($p);
                    } elseif (is_dir($p)) {
                        $this->rrmdir($p);
                    }
                }
            }
            // Only remove the scratch dir itself when nothing is left in
            // it — the dump, if kept, still lives here.
            if ($dumpBase === '' || !is_file($scratch . DIRECTORY_SEPARATOR . $dumpBase)) {
                @rmdir($scratch); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- removes an empty server-derived scratch dir; WP_Filesystem not initialized
            }
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

        $expiresAt = time() + ($keepOld ? FilesRestorer::OLDFILES_GC_AGE_SECONDS_LONG : FilesRestorer::OLDFILES_GC_AGE_SECONDS);

        // GH #146: mark the files rollback tree(s) with an `.expires`
        // sidecar recording exactly when `FilesRestorer::gcOldFiles()` may
        // reclaim them, then leave them in place (DEFER — no synchronous
        // rrmdir on either branch anymore).
        if ($oldFilesDir !== '' && is_dir($oldFilesDir)) {
            $this->writeGcExpiryMarker($oldFilesDir, $expiresAt);
        }
        foreach ($oldDirs as $comp => $abs) {
            if (!is_string($abs) || $abs === '') {
                continue;
            }
            // The 'wp-content' catch-all's $abs is a naming PREFIX (never a
            // real directory itself — only its `-<name>` siblings are);
            // gcOldFiles() detects this shape at sweep time and expands it.
            $this->writeGcExpiryMarker($abs, $expiresAt);
        }

        // GH #146 / §5: keep the pre-restore DB dump for the SAME window as
        // the files rollback tree, GC'd by the same cron sweep
        // (RestoreRunner::gcPreRestoreDumps(), bound to the same hook).
        if ($dumpBase !== '' && is_file($dumpPath)) {
            $this->writeGcExpiryMarker($dumpPath, $expiresAt);
        }

        if (function_exists('wp_next_scheduled') && function_exists('wp_schedule_single_event')) {
            if (!wp_next_scheduled('wpmgr_restore_oldfiles_gc')) {
                wp_schedule_single_event($expiresAt + 60, 'wpmgr_restore_oldfiles_gc');
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
            \WPMgr\Agent\Support\DebugLog::write(sprintf(
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
        throw new \RuntimeException(esc_html(substr($msg, 0, 240)));
    }

    // ==================================================================
    // ADR-036 P1 storage adapter — local destination chunk reads
    // ==================================================================

    /**
     * This restore's `destination_kind` — 'cp' | 'local' | 's3_compat'.
     * Defaults to 'cp' when absent/empty, matching BackupCommand /
     * DestinationResolver's default (older CP builds never send the field).
     */
    private function destinationKind(): string
    {
        $kind = isset($this->params['destination_kind']) && is_string($this->params['destination_kind'])
            ? $this->params['destination_kind']
            : '';
        return $kind === '' ? 'cp' : $kind;
    }

    /**
     * Build the LocalDestination this restore reads chunks from. Only
     * called once, when destinationKind() === 'local'.
     *
     * Constructs a real BackupTransport to satisfy DestinationResolver's
     * factory signature (kept uniform with the backup-side wiring in
     * EncryptAndUpload) — LocalDestination::getChunk()/prepare() never
     * actually use it; only submitManifest() (a backup-only call) does.
     *
     * prepare() resolves (and, if the guard files are missing, re-hardens)
     * the exact snapshot directory the matching backup wrote to. It is
     * idempotent — every write it performs is file_exists()-guarded — so
     * calling it here, on a read-only restore path, is safe.
     */
    private function buildLocalDestination(): BackupDestination
    {
        $config = isset($this->params['destination_config']) && is_array($this->params['destination_config'])
            ? $this->params['destination_config']
            : [];

        $transport   = new BackupTransport(new Signer(new Keystore()));
        $destination = DestinationResolver::resolve([
            'snapshot_id'        => $this->snapshotId(),
            'destination_kind'   => 'local',
            'destination_config' => $config,
            'manifest_endpoint'  => '',
            'presign_endpoint'   => '',
        ], $transport);
        $destination->prepare($this->snapshotId());

        return $destination;
    }

    /**
     * Read one chunk from the local destination by content hash, instead of
     * an HTTP GET against a presigned URL. Local snapshot chunks carry no
     * presigned_url (they never leave the webserver) — the manifest only
     * guarantees hash (+ size) for them.
     *
     * No retry-with-backoff policy here (unlike fetchChunkWithRetries): a
     * local disk read has none of the transient-network failure modes a
     * presigned-URL GET over a tunnel does. LocalDestination::getChunk()
     * itself rejects a malformed hash (path-traversal defense) and returns
     * null on any other failure (missing file, unreadable file), which we
     * surface here as a structured, operator-grep-able message.
     */
    private function fetchLocalChunk(
        BackupDestination $destination,
        string $hash,
        string $logical,
        int $chunkIdx
    ): string {
        $bytes = $destination->getChunk($hash);
        if ($bytes === null) {
            throw new \RuntimeException(esc_html(sprintf(
                'download_artifacts: local chunk %s[%d] hash=%s not found on local disk (destination_kind=local)',
                $logical,
                $chunkIdx,
                substr($hash, 0, 12)
            )));
        }
        return $bytes;
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
        if ($root === '' || !is_dir($root) || !is_writable($root)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_is_writable -- headless agent; WP_Filesystem never initialized; direct writability probe is the only option
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
            wp_delete_file($path);
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
        $prepared = $wpdb->prepare($sql, $this->snapshotId(), $this->restoreId()); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- already prepared on the preceding line
        /** @phpstan-ignore-next-line */
        $row = $wpdb->get_row($prepared, ARRAY_A); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching,WordPress.DB.DirectDatabaseQuery.SchemaChange,PluginCheck.Security.DirectDB.UnescapedDBParameter -- direct query on plugin-owned table; correctness requires a live read; already prepared above; value is the output of $wpdb->prepare()

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
            $sql, // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- identifier validated against information_schema / prefix+constant; values bound via placeholders
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
        $wpdb->query($prepared); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching,PluginCheck.Security.DirectDB.UnescapedDBParameter -- direct query on plugin-owned table; correctness requires a live read; already prepared above; value is the output of $wpdb->prepare()
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
        // JSON_INVALID_UTF8_SUBSTITUTE: a real WP site can hold file paths with
        // invalid UTF-8 bytes (e.g. latin1 filenames) in plan/tombstone cursors.
        // Plain json_encode() returns false on those, and the old `?: '{}'`
        // fallback silently WIPED the entire sub_state — including the restore
        // params (endpoints, age recipient) the watchdog needs to re-enter.
        // Substitute the bad bytes instead, and never persist '{}' over good state.
        $encoded = json_encode($subState, JSON_INVALID_UTF8_SUBSTITUTE | JSON_PARTIAL_OUTPUT_ON_ERROR);
        if ($encoded === false || $encoded === '') {
            \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: sub_state json_encode failed for phase ' . $phase . ' — skipping state write to preserve the prior cursor');
            return;
        }
        $this->lastDbUpdate = $now;

        /** @phpstan-ignore-next-line */
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct query on plugin-owned table; correctness requires a live read
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
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct query on plugin-owned table; correctness requires a live read
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
    /**
     * @param array<string,mixed> $subState Final sub_state, so the GH #146
     *        pre-restore DB dump (deliberately kept for its retention
     *        window by `runCleanup()`, which already ran as the preceding
     *        phase) is not immediately wiped out again by this method's
     *        own scratch-dir sweep.
     */
    private function cleanupOnCompleted(array $subState): void
    {
        $scratch = $this->scratchDir();
        if ($scratch !== '' && is_dir($scratch)) {
            $dbRollback = isset($subState['db_rollback']) && is_array($subState['db_rollback']) ? $subState['db_rollback'] : [];
            $dumpPath   = (string) ($dbRollback['dump_path'] ?? '');
            if ($dumpPath !== '' && is_file($dumpPath)) {
                // GH #146 / §5: the pre-restore dump (+ its `.expires`
                // marker) was deliberately kept by runCleanup() for the
                // retention window — leave the scratch dir (and just those
                // two files) alone; the GC sweep reclaims both later.
                $dumpBase = basename($dumpPath);
                $items    = @scandir($scratch);
                if ($items !== false) {
                    foreach ($items as $i) {
                        if ($i === '.' || $i === '..' || $i === $dumpBase || $i === $dumpBase . '.expires') {
                            continue;
                        }
                        $p = $scratch . DIRECTORY_SEPARATOR . $i;
                        if (is_file($p)) {
                            wp_delete_file($p);
                        } elseif (is_dir($p)) {
                            $this->rrmdir($p);
                        }
                    }
                }
            } else {
                $this->rrmdir($scratch);
            }
        }

        global $wpdb;
        if (is_object($wpdb)) {
            $runsTable = $this->prefix() . Schema::BACKUP_RESTORE_RUNS_TABLE;
            /** @phpstan-ignore-next-line */
            // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct query on plugin-owned table; correctness requires a live read
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
        if (!is_dir($dir) && !@mkdir($dir, 0700, true) && !is_dir($dir)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir -- explicit 0700 perms on scratch dir; wp_mkdir_p would apply the wider FS_CHMOD_DIR
            throw new \RuntimeException('RestoreRunner: cannot create scratch dir: ' . esc_html($dir));
        }
        @chmod($dir, 0700); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0700); WP_Filesystem would coerce to wider FS_CHMOD_DIR
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
     * the host has enough disk (conservative: treat 0 as enough disk).
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

    /**
     * ADR-049: sanitize a tombstone path and confirm it resolves inside the
     * staging root. Returns the realpath'd absolute path on success, or null
     * if the path should be skipped (absent, rejected, or a traversal attempt).
     *
     * Rules 1-10 from the ADR-049 agent_flow spec are implemented here. The
     * delete caller acts only on the non-null return — every null means either
     * "skip silently (already absent)" or "skip with security log (attack)".
     *
     * @param string $rawPath    The tombstone path as received from the CP wire.
     * @param string $stagingRoot The realpath-resolved absolute staging dir.
     * @return string|null Realpath'd safe path, or null to skip.
     */
    private function sanitizeTombstonePath(string $rawPath, string $stagingRoot): ?string
    {
        // Rule 1: reject empty.
        if ($rawPath === '') {
            return null;
        }
        // Rule 2: reject absolute paths (must not start with / or \).
        if ($rawPath[0] === '/' || $rawPath[0] === '\\') {
            \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: tombstone path escape attempt (absolute): ' . $rawPath);
            return null;
        }
        // Rule 3: reject any '..' or '.' component (split on both / and \).
        $parts = preg_split('/[\/\\\\]/', $rawPath);
        if ($parts === false) {
            return null;
        }
        foreach ($parts as $part) {
            if ($part === '..' || $part === '.') {
                \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: tombstone path escape attempt (dot-segment): ' . $rawPath);
                return null;
            }
        }
        // Rule 4: reject NUL bytes.
        if (str_contains($rawPath, "\x00")) {
            \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: tombstone path NUL byte rejected: ' . substr($rawPath, 0, 120));
            return null;
        }
        // Rule 5: build candidate path inside staging root.
        $candidate = $stagingRoot . DIRECTORY_SEPARATOR . ltrim($rawPath, '/\\');
        // Rule 6: resolve symlinks + verify containment. realpath() returns
        // false when the path does not exist — treat as "not present in
        // staging, nothing to do" and return null (skip, no action).
        $real = realpath($candidate);
        if ($real === false) {
            // Path doesn't exist in staging: already absent, no action needed.
            return null;
        }
        // Rule 7: verify the resolved path starts with staging root + separator.
        // Catches symlink escapes: a symlink inside staging pointing outside
        // staging would resolve to a path outside the prefix.
        if (!str_starts_with($real . DIRECTORY_SEPARATOR, $stagingRoot . DIRECTORY_SEPARATOR)) {
            \WPMgr\Agent\Support\DebugLog::write('WPMgr RestoreRunner: tombstone path escape attempt (symlink/resolve): ' . $rawPath);
            return null;
        }
        return $real;
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
                wp_delete_file($p);
            } elseif (is_dir($p)) {
                $this->rrmdir($p);
            }
        }
        @rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- removes an empty server-derived scratch/snapshot dir; WP_Filesystem not initialized
    }

    /**
     * GH #146 / §5: write a `<targetPath>.expires` sidecar recording the
     * unix timestamp after which `FilesRestorer::gcOldFiles()` (files) or
     * `self::gcPreRestoreDumps()` (the DB dump) may reclaim it. Best-effort
     * — a failed write just means the legacy mtime-vs-LONG-threshold
     * fallback applies instead, which is still safe (only ever LATER than
     * intended, never earlier).
     */
    private function writeGcExpiryMarker(string $targetPath, int $expiresAt): void
    {
        @file_put_contents($targetPath . '.expires', (string) $expiresAt, LOCK_EX);
    }

    /**
     * GH #146 / §5: GC pre-restore DB dumps (+ their `.expires` markers)
     * once their retention window elapses. Bound to the SAME
     * `wpmgr_restore_oldfiles_gc` cron hook `FilesRestorer::gcOldFiles()`
     * uses, so both halves of the rollback material (files tree + DB dump)
     * age out on the same sweep.
     *
     * @return void
     */
    public static function gcPreRestoreDumps(): void
    {
        if (!defined('WP_CONTENT_DIR')) {
            return;
        }
        $base = rtrim((string) WP_CONTENT_DIR, '/\\') . '/wpmgr-agent/restores';
        if (!is_dir($base)) {
            return;
        }

        $now  = time();
        $dirs = @glob($base . DIRECTORY_SEPARATOR . '*', GLOB_ONLYDIR);
        if (!is_array($dirs)) {
            return;
        }

        foreach ($dirs as $dir) {
            $markers = @glob($dir . DIRECTORY_SEPARATOR . '*.expires');
            if (!is_array($markers) || $markers === []) {
                continue;
            }
            foreach ($markers as $marker) {
                $target = substr($marker, 0, -strlen('.expires'));
                $raw    = @file_get_contents($marker);
                $expiresAt = $raw !== false && trim($raw) !== '' ? (int) trim($raw) : 0;

                if ($expiresAt <= 0) {
                    wp_delete_file($marker);
                    continue;
                }
                if ($now < $expiresAt) {
                    continue;
                }
                if (is_file($target)) {
                    wp_delete_file($target);
                }
                wp_delete_file($marker);
            }

            // Reclaim the now-empty per-restore scratch dir.
            $remaining = @scandir($dir);
            if (is_array($remaining) && count($remaining) <= 2) {
                @rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- removes an empty server-derived scratch dir; WP_Filesystem not initialized
            }
        }
    }
}
