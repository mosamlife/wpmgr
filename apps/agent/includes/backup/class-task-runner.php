<?php
/**
 * TaskRunner: M5.6 / ADR-033 — the state-machine driver that turns a row in
 * `wpmgr_backup_tasks` into a completed backup snapshot.
 *
 * This is the only "active" component in the state-machine pipeline: the
 * REST handler (Phase D) writes the task row, calls the SAPI-aware finish
 * (fastcgi/litespeed/fallback — see ConnectionFinisher), then invokes
 * TaskRunner::run() in the same PHP process. The watchdog hook
 * (also Phase D) calls TaskRunner::run() on re-entry. Both entry points are
 * idempotent — the state machine reads `phase` + `sub_state` and resumes from
 * wherever the last invocation left off.
 *
 * Phase transitions (closed set; matches CP backup.allowedProgressPhases):
 *
 *   queued
 *     -> dumping_db          (kind in {db, full})
 *     -> archiving_files     (kind == files)
 *
 *   dumping_db
 *     -> encrypting_uploading (kind == db; skip files)
 *     -> archiving_files      (kind == full)
 *
 *   archiving_files
 *     -> encrypting_uploading
 *
 *   encrypting_uploading
 *     -> submitting_manifest
 *
 *   submitting_manifest
 *     -> completed
 *
 *   completed | failed: terminal (re-entry is a no-op)
 *
 * ADR-051 incremental mode: an incremental backup runs through the SAME
 * phase pipeline as a full backup. The only branch is in runArchivingFiles:
 * when is_incremental=true, FilesArchiver receives the prev map built from the
 * parent snapshot's files.list (fetched via PrevFilesListChunks presigned GET
 * URLs the CP sent in the request). Only CHANGED/NEW files are packed; the
 * resulting archive + a fresh files.list are submitted through the standard
 * SubmitManifest endpoint. Deleted files become per-path tombstone manifest
 * entries (entry_kind=tombstones, mode=Delete, empty chunk list). The per-file
 * chunk scanner, the NDJSON file-index endpoint, and the three 0-files bandages
 * are all retired.
 *
 * On any uncaught exception from a phase handler we mark the task `failed`,
 * post one `failed` progress event, and return — TaskRunner::run() NEVER
 * throws. The CP watchdog notices the snapshot stalled in {dumping_db,
 * archiving_files, encrypting_uploading, submitting_manifest} and surfaces
 * the failure to the operator.
 *
 * Watchdog re-entry semantics:
 *   - Every save keeps a durable progress record in sub_state.progress: a
 *     mark built from the phase, the closed archive parts, the archive's
 *     first unfinished line, the encrypted entries and the uploaded chunks,
 *     the time it last advanced, and a sequence number bumped on each
 *     advance. Each worker records the sequence it started at in
 *     sub_state.worker. The watchdog counts a resume toward max_resumes
 *     (default 6) only when the worker before it saved no progress, and
 *     gives an old task up only when that record stopped advancing. We
 *     don't touch resume_count here. We DO update last_progress_at on every
 *     meaningful boundary so the watchdog can tell live work apart from a
 *     stalled handler.
 *   - A save that finds no task row means the task was given up: the worker
 *     stops, reports the stop, and never reports success.
 *   - A worker that has used most of its time limit stops after a closed
 *     archive part and hands the task to the watchdog's resume path.
 *
 * Persistence pattern:
 *   - $wpdb->update() with explicit prepared-arg formats. Single-row updates
 *     keyed by snapshot_id; no transactions required (the task row is the
 *     authority; sub_state is monotonically progress-only).
 *   - DB writes from inside per-chunk progress callbacks are throttled to one
 *     per 5 s (PROGRESS_DB_THROTTLE_SECONDS) to keep the per-chunk overhead
 *     bounded on big archives — the in-memory $lastDbUpdate just tracks the
 *     last write; persisted state is still correct on every phase boundary
 *     because the phase-end save is unconditional.
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

use WPMgr\Agent\Phpbu\ProgressClient;
use WPMgr\Agent\Schema;
use WPMgr\Agent\Signer;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Support\AgeCrypto;
use WPMgr\Agent\Support\BackupTransport;
use WPMgr\Agent\Support\LongRunningJob;

/**
 * State-machine driver for a single backup task row.
 *
 * Declared `final` — exactly one TaskRunner per backup invocation, instantiated
 * by BackupCommand (Phase D) and by the watchdog handler. No subclassing
 * intended; the public API is a single `run()` method.
 */
final class TaskRunner
{
    /** Closed set of phase names — matches CP /progress allowedProgressPhases. */
    public const PHASE_QUEUED               = 'queued';
    public const PHASE_DUMPING_DB           = 'dumping_db';
    public const PHASE_ARCHIVING_FILES      = 'archiving_files';
    public const PHASE_ENCRYPTING_UPLOADING = 'encrypting_uploading';
    public const PHASE_SUBMITTING_MANIFEST  = 'submitting_manifest';
    public const PHASE_COMPLETED            = 'completed';
    public const PHASE_FAILED               = 'failed';

    /** Valid snapshot kinds (mirror of the CP backup_contract.go Kind enum). */
    public const KIND_FILES = 'files';
    public const KIND_DB    = 'db';
    public const KIND_FULL  = 'full';

    /**
     * ADR-051 archive-delta tombstone contract (mirror of the CP
     * agentcmd/backup_contract.go EntryKindTombstones + TombstoneMode* enum).
     * A tombstone is a per-path manifest entry with EMPTY chunk_hashes; the
     * `mode` field carries the delete/re-add delta. The agent only ever emits
     * Delete (a re-added file is simply re-packed into a part, not a Readd
     * tombstone), but the constant set mirrors the CP for clarity.
     */
    public const ENTRY_KIND_TOMBSTONES = 'tombstones';
    public const TOMBSTONE_MODE_DELETE = 0;
    public const TOMBSTONE_MODE_READD  = 1;

    /** Minimum seconds between in-phase DB writes to last_progress_at. */
    private const PROGRESS_DB_THROTTLE_SECONDS = 5;

    /** Exception code: a save found no task row, so the task was given up. */
    public const CODE_TASK_GONE = 983001;

    /** Exception code: this worker saved a closed part and hands the task over. */
    public const CODE_HANDOVER = 983002;

    /**
     * Share of the worker's time limit after which it hands over at the next
     * closed archive part, instead of being ended by the limit mid-part.
     */
    public const HANDOVER_SHARE = 0.8;

    /** What the site owner reads when a save finds the task row gone. */
    private const TASK_GONE_MESSAGE = 'Backup stopped: its task record on this site was removed while it was running, so it cannot be resumed. Run the backup again.';

    /** Phase order inside the durable progress mark. */
    private const PHASE_RANK = [
        self::PHASE_QUEUED               => 0,
        self::PHASE_DUMPING_DB           => 1,
        self::PHASE_ARCHIVING_FILES      => 2,
        self::PHASE_ENCRYPTING_UPLOADING => 3,
        self::PHASE_SUBMITTING_MANIFEST  => 4,
    ];

    /** @var array{mark:list<int>,at:int,seq:int}|null The durable progress record, as last saved. */
    private ?array $progress = null;

    /** Progress sequence number this worker started at. */
    private int $workerStartSeq = 0;

    /** Wall clock when this worker started. */
    private float $workerWallStart = 0.0;

    /** Process CPU seconds when this worker started; null when the limit counts wall time. */
    private ?float $workerCpuStart = null;

    /** Seconds of the time limit this worker runs under: null until run() starts, 0 for none. */
    private ?float $timeLimit = null;

    /**
     * @var array{snapshot_id:string,kind:string,age_recipient:string,presign_endpoint:string,
     *            manifest_endpoint:string,progress_endpoint:string,chunk_bytes:int,
     *            scratch_dir:string,wp_content_path:string,db:array<string,string>,
     *            is_incremental?:bool,parent_snapshot_id?:string,base_snapshot_id?:string,
     *            generation?:int,prev_files_list_chunks?:list<array<string,string>>,
     *            timing?:array{received_at:int,preflight_done_at:int,claimed_at:int}}
     */
    private array $params;

    /** Unix-seconds of the last DB write to last_progress_at (throttle). */
    private int $lastDbUpdate = 0;

    private ?ProgressClient $progressClient = null;

    private ?int $workerStartedAt = null;

    private int $skippedSourcesCount = 0;

    /** @var list<string> Relative paths omitted from the snapshot. */
    private array $skippedSources = [];

    /**
     * @param array{snapshot_id:string,kind:string,age_recipient:string,
     *              presign_endpoint:string,manifest_endpoint:string,
     *              progress_endpoint:string,chunk_bytes:int,scratch_dir:string,
     *              wp_content_path:string,db:array<string,string>,
     *              timing?:array{received_at:int,preflight_done_at:int,claimed_at:int}} $params
     */
    public function __construct(array $params)
    {
        $this->params = $params;

        // ProgressClient is the existing M5.6 signed `/progress` POSTer. We
        // construct lazily so unit tests that don't touch network don't have
        // to stub Signer/Keystore.
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
                // Progress is best-effort; never let construction failure
                // (e.g. missing keystore in a degraded host) abort a backup.
                $this->progressClient = null;
            }
        }
    }

    /**
     * Drive the task to completion (or to the next checkpoint a watchdog can
     * resume from). NEVER throws — top-level catch translates any escape into
     * a `failed` phase + progress post.
     *
     * GitHub issue #232: two independent, layered mutual-exclusion guards are
     * taken before any phase work, because the now-guaranteed double-fire of
     * the in-process shutdown-function runner (BackupCommand::execute()
     * registers it unconditionally) racing the separate FPM worker
     * `wp_schedule_single_event('wpmgr_backup_run')` + `spawn_cron()` dispatch
     * (Watchdog::dispatch()) — and now also the WP-Cron-independent
     * Watchdog::sweepStalled() reaper — means two or more processes CAN
     * legitimately reach run() for the same snapshot_id at nearly the same
     * instant:
     *
     *   1. GET_LOCK(name, 0) — a fast, cheap, cross-process reject. Grants a
     *      MySQL named lock to AT MOST one session at a time; a non-blocking
     *      second caller gets '0' immediately. Adversarial review finding
     *      (post-ship): GET_LOCK is scoped to $wpdb's OWN mysqli CONNECTION
     *      and is SILENTLY released the instant that connection drops — a
     *      short `wait_timeout` on a managed host, a MySQL restart/failover,
     *      or `$wpdb->check_connection()` auto-reconnecting after "server
     *      has gone away" can all sever the connection mid-backup (e.g.
     *      during a large-table dump in DbDumper, which streams over its own
     *      separate mysqli handle and only calls back to $wpdb once per
     *      table). The winner's session-scoped lock vanishes while the
     *      winner keeps running — a SECOND caller (e.g. the reaper, once
     *      last_progress_at goes stale) can then legitimately win GET_LOCK
     *      while the first is still alive, so GET_LOCK ALONE is NOT
     *      sufficient for correctness; it is now only the fast early-reject.
     *   2. flock(LOCK_EX|LOCK_NB) on a deterministic per-snapshot lock file
     *      (see fileLockPath()) — the AUTHORITATIVE guard. It is entirely
     *      independent of $wpdb's connection state: the OS holds it for as
     *      long as this PHP process is alive, and releases it automatically
     *      the instant the process exits for ANY reason (including a
     *      SIGKILL that skips every PHP-level `finally`) — so a second
     *      caller's non-blocking acquire attempt correctly reports
     *      "contended" for as long as the first is genuinely still running,
     *      regardless of what happened to its DB connection in the
     *      meantime. flock() is a single-node primitive — correct here
     *      because a given site's backup always runs on the one WP node
     *      whose local disk holds the scratch dir.
     *
     * Guard order: GET_LOCK first (cheap, no filesystem I/O) — a '0' is a
     * fast reject with no need to even attempt the flock. Otherwise ('1' or
     * null) the flock is attempted; losing IT is authoritative and rejects
     * regardless of the GET_LOCK outcome (this is exactly what closes the
     * dead-connection gap: GET_LOCK can hand out '1' to a second caller once
     * the true owner's connection has silently dropped, but the true owner's
     * OS-level flock is untouched by that and still rejects the second
     * caller). A loser via EITHER guard writes the SAME lock_contended
     * breadcrumb and returns WITHOUT mutating phase. If flock itself cannot
     * even be attempted (path unresolvable, or the filesystem/permissions
     * prevent opening the lock file — e.g. an exotic FS without flock()
     * support), a held GET_LOCK permits the run to proceed. If neither
     * guard is held, an existing active task receives only a lock_unavailable
     * breadcrumb. The invocation never seeds, advances, fails or reclaims work.
     *
     * @return string Terminal phase reached this invocation. One of
     *                PHASE_COMPLETED, PHASE_FAILED, or — if a future phase
     *                handler yields mid-run on a soft cap — the in-progress
     *                phase. Today every handler runs the phase to completion
     *                or throws, so this returns COMPLETED or FAILED.
     */
    public function run(): string
    {
        $this->workerStartedAt = time();
        @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged, PHPCompatibility.FunctionUse.ArgumentFunctionsReportCurrentValue.NeedsInspection, Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running backup loop must not hit max_execution_time; @-guarded, no-op when disabled
        @ignore_user_abort(true);
        $this->startWorkerClock();

        $currentPhase = self::PHASE_QUEUED;
        $handedOver   = false;

        // ---- GH #232 guard 1: GET_LOCK — fast, cheap, cross-process reject.
        //   '1'  => we won the lock — proceed to the (authoritative) flock
        //           check below, and RELEASE_LOCK in the finally regardless
        //           of how this invocation ends.
        //   '0'  => another session already holds the lock and is provably
        //           making progress (or about to) — write a lock_contended
        //           breadcrumb and return WITHOUT mutating phase. Fast
        //           reject: never even attempts the flock.
        //   null => GET_LOCK() is unsupported (non-MySQL $wpdb, or the query
        //           itself failed) — fall through to the flock check, which
        //           is now the sole guard for this invocation.
        $lockToken = $this->getLock();
        if ($lockToken === '0') {
            try {
                $this->recordLockContended();
            } catch (\Throwable $_) {
                // Swallow — a breadcrumb write failure must never propagate
                // from the loser path; run() never throws.
            }
            try {
                $task = $this->loadTask();
                return $task !== null ? (string) $task['phase'] : self::PHASE_QUEUED;
            } catch (\Throwable $_) {
                return self::PHASE_QUEUED;
            }
        }
        $lockHeld = ($lockToken === '1');

        // ---- GH #232 guard 2: flock — connection-independent, AUTHORITATIVE.
        // Attempted regardless of the GET_LOCK outcome above ('1' or null):
        // this is precisely what catches the dead-$wpdb-connection scenario
        // where GET_LOCK handed out '1' to a second caller because the true
        // owner's MySQL session silently dropped mid-backup while the owner
        // (and its flock) is still alive.
        $fileLock = $this->acquireFileLock();
        if ($fileLock['contended']) {
            // Authoritative loser. Release the GET_LOCK we may have just won
            // (cleanup hygiene — this session is not doing any further work
            // that needs it) before writing the SAME lock_contended
            // breadcrumb the GET_LOCK-loser path above uses.
            if ($lockHeld) {
                $this->releaseLock();
            }
            try {
                $this->recordLockContended();
            } catch (\Throwable $_) {
                // Swallow — see the GET_LOCK-loser path above.
            }
            try {
                $task = $this->loadTask();
                return $task !== null ? (string) $task['phase'] : self::PHASE_QUEUED;
            } catch (\Throwable $_) {
                return self::PHASE_QUEUED;
            }
        }
        /** @var resource|null $fileLockHandle */
        $fileLockHandle = $fileLock['handle'];
        if (!$lockHeld && $fileLockHandle === null) {
            return $this->handleUnavailableGuards();
        }

        // GH #232 lock-file GC follow-up: set to true at every point this
        // invocation reaches a TERMINAL outcome for the snapshot (already
        // terminal on load, completed, or failed) — never on a loser return
        // above (those return before this point) and never left false by an
        // in-progress run, since every path through the try/catch below ends
        // in exactly one of the three terminal returns. Read in the
        // `finally` to decide whether OUR OWN lock file (the one this
        // invocation created/held) should be unlinked now that the snapshot
        // will never be re-entered again.
        $terminal = false;

        try {
            // ---- Seed or load the task row. ------------------------------
            $task = $this->loadTask();
            if ($task === null) {
                $this->seedTask();
                $task = $this->loadTask();
                if ($task === null) {
                    // Seed failed (unwritable schema / missing $wpdb). We
                    // can't drive a state machine without a row.
                    throw new \RuntimeException('TaskRunner: cannot create task row');
                }
            }
            $currentPhase = (string) ($task['phase'] ?? self::PHASE_QUEUED);
            $subState     = (array) ($task['sub_state'] ?? []);
            $this->retainSkippedSources((array) ($subState['files'] ?? []));

            // Terminal? Re-entry is a no-op.
            if ($currentPhase === self::PHASE_COMPLETED || $currentPhase === self::PHASE_FAILED) {
                $terminal = true;
                return $currentPhase;
            }

            // GH #232: stamp the runner_started breadcrumb now that we hold
            // at least one real exclusivity guard (GET_LOCK and/or flock) and
            // have a loaded/seeded row. Feeds the observability ladder
            // alongside Watchdog::dispatch()'s dispatch_entered stamp:
            //   no stage + last_progress_at==started_at => hook never fired
            //   stage=dispatch_entered                  => hook fired, runner never advanced
            //   stage=runner_started                     => a lock held, first phase failed to persist
            //   phase past 'queued'                      => advanced
            // A task without either guard returns before this breadcrumb.
            //
            // GH #983: the same save records where this worker started in the
            // durable progress record, so the watchdog can tell whether this
            // worker saved any progress before it ended.
            $this->progress = self::progressRecord($subState['progress'] ?? null)
                ?? ['mark' => self::progressMark($currentPhase, $subState), 'at' => time(), 'seq' => 0];
            $this->workerStartSeq = $this->progress['seq'];
            $subState['progress'] = $this->progress;
            $subState['worker']   = ['seq' => $this->workerStartSeq, 'started_at' => (int) $this->workerStartedAt];
            if ($lockHeld || $fileLockHandle !== null) {
                $subState['stage'] = 'runner_started';
                $this->saveTaskState($currentPhase, $subState);
            }

            // ---- Phase dispatch loop. ------------------------------------
            // Each branch runs ONE phase to completion (or throws), persists
            // the new sub_state, advances `phase`, and falls through to the
            // next iteration. The loop exits when phase==completed.

            while ($currentPhase !== self::PHASE_COMPLETED) {
                switch ($currentPhase) {
                    case self::PHASE_QUEUED:
                        // First entry: announce we're alive and pick the next phase.
                        $this->postProgress('queued', ['kind' => $this->kind(), 'started_at' => time()]);
                        $next = $this->nextAfterQueued();
                        \WPMgr\Agent\Support\DebugLog::write(sprintf(
                            'WPMgr TaskRunner: advancing phase %s -> %s for snapshot %s',
                            $currentPhase,
                            $next,
                            $this->snapshotId()
                        ));
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_DUMPING_DB:
                        $subState = $this->runDumpingDb($subState);
                        $next     = $this->nextAfterDumpingDb();
                        \WPMgr\Agent\Support\DebugLog::write(sprintf(
                            'WPMgr TaskRunner: advancing phase %s -> %s for snapshot %s',
                            $currentPhase,
                            $next,
                            $this->snapshotId()
                        ));
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_ARCHIVING_FILES:
                        $subState = $this->runArchivingFiles($subState);
                        \WPMgr\Agent\Support\DebugLog::write(sprintf(
                            'WPMgr TaskRunner: advancing phase %s -> %s for snapshot %s',
                            $currentPhase,
                            self::PHASE_ENCRYPTING_UPLOADING,
                            $this->snapshotId()
                        ));
                        $this->saveTaskState(self::PHASE_ENCRYPTING_UPLOADING, $subState);
                        $currentPhase = self::PHASE_ENCRYPTING_UPLOADING;
                        break;

                    case self::PHASE_ENCRYPTING_UPLOADING:
                        $subState = $this->runEncryptingUploading($subState);
                        \WPMgr\Agent\Support\DebugLog::write(sprintf(
                            'WPMgr TaskRunner: advancing phase %s -> %s for snapshot %s',
                            $currentPhase,
                            self::PHASE_SUBMITTING_MANIFEST,
                            $this->snapshotId()
                        ));
                        $this->saveTaskState(self::PHASE_SUBMITTING_MANIFEST, $subState);
                        $currentPhase = self::PHASE_SUBMITTING_MANIFEST;
                        break;

                    case self::PHASE_SUBMITTING_MANIFEST:
                        $this->runSubmittingManifest($subState);
                        \WPMgr\Agent\Support\DebugLog::write(sprintf(
                            'WPMgr TaskRunner: advancing phase %s -> %s for snapshot %s',
                            $currentPhase,
                            self::PHASE_COMPLETED,
                            $this->snapshotId()
                        ));
                        $this->saveTaskState(self::PHASE_COMPLETED, $subState);
                        $currentPhase = self::PHASE_COMPLETED;
                        break;

                    default:
                        throw new \RuntimeException('TaskRunner: unknown phase ' . $currentPhase);
                }
            }

            // ---- Completion: cleanup + ack. ------------------------------
            $this->cleanupOnCompleted();
            $this->postProgress(self::PHASE_COMPLETED, ['snapshot_id' => $this->snapshotId()]);

            $terminal = true;
            return self::PHASE_COMPLETED;
        } catch (\RuntimeException $e) {
            if ($e->getCode() === self::CODE_HANDOVER) {
                // GH #983: the closed part is saved; the next worker continues
                // from it once this one has released its guards (below).
                \WPMgr\Agent\Support\DebugLog::write('WPMgr TaskRunner: handing over snapshot ' . $this->snapshotId() . ' after a saved part: ' . $e->getMessage());
                $handedOver = true;
            } elseif ($e->getCode() === self::CODE_TASK_GONE) {
                // GH #983: the task was given up while this worker ran. Stop:
                // no further phase work, no success report, no task-row write.
                \WPMgr\Agent\Support\DebugLog::write('WPMgr TaskRunner: stopping snapshot ' . $this->snapshotId() . ' in phase ' . $currentPhase . ': ' . $e->getMessage());
                try {
                    $this->postProgress(self::PHASE_FAILED, ['stage' => $currentPhase, 'message' => self::TASK_GONE_MESSAGE]);
                } catch (\Throwable $_) {
                    // Swallow.
                }
                // This worker still holds the guards, so its scratch is reclaimed now.
                try {
                    $this->cleanupOnFailed();
                } catch (\Throwable $_) {
                    // Swallow.
                }
                $terminal = true;
                return self::PHASE_FAILED;
            } else {
                $terminal = true;
                return $this->failRun($currentPhase, $e);
            }
        } catch (\Throwable $e) {
            $terminal = true;
            return $this->failRun($currentPhase, $e);
        } finally {
            // GH #232: release BOTH guards regardless of how this invocation
            // ends (completed, failed, handed over, or an escape the outer
            // catch didn't anticipate) — never on the loser/no-lock-held
            // paths above, which already returned and hold nothing to
            // release here.
            if ($lockHeld) {
                $this->releaseLock();
            }
            if ($fileLockHandle !== null) {
                // Only the terminal owner removes its own path, while its
                // flock is still held. Losers never reclaim a lock file.
                try {
                    if ($terminal) { $this->cleanupFileLock($fileLockHandle); }
                } finally {
                    $this->releaseFileLock($fileLockHandle);
                }
            }
        }

        // GH #983: reached only by a hand-over, after both guards are released
        // so the next worker can take them. The next worker starts through the
        // watchdog's own resume path, at once: it is never left to be found
        // stalled. Its run() restarts the time limit.
        if (!$handedOver) {
            return $currentPhase;
        }
        unset($task, $subState);
        return Watchdog::handOver($this->snapshotId()) ?? $currentPhase;
    }

    /**
     * Persist and report a failed run, reclaim its scratch, and remove its
     * task row. Called from run()'s catch, while both guards are held.
     *
     * @param string     $currentPhase Phase the failure happened in.
     * @param \Throwable $e            The failure.
     * @return string PHASE_FAILED.
     */
    private function failRun(string $currentPhase, \Throwable $e): string
    {
        \WPMgr\Agent\Support\DebugLog::write('WPMgr TaskRunner: phase ' . $currentPhase . ' failed: ' . $e->getMessage());

        // Best-effort: persist `failed` + post one progress event. If
        // either of these throws we swallow and still return 'failed' —
        // the watchdog and operator will see a stale row.
        try {
            $this->saveTaskState(self::PHASE_FAILED, [
                'last_error' => substr($e->getMessage(), 0, 240),
                'failed_in'  => $currentPhase,
            ]);
        } catch (\Throwable $_) {
            // Swallow.
        }
        try {
            $failureDetail = [
                'stage'   => $currentPhase,
                'message' => substr($e->getMessage(), 0, 240),
            ];
            if ($e instanceof ZipCloseException) {
                $failureDetail['part'] = $e->part();
                $failureDetail['libzip_status'] = $e->libzipStatus();
            }
            $this->postProgress(self::PHASE_FAILED, $failureDetail);
        } catch (\Throwable $_) {
            // Swallow.
        }

        // Scratch reclamation requires a held guard. Cleanup failures
        // must never mask the failure already persisted and reported.
        try {
            $this->cleanupOnFailed();
        } catch (\Throwable $_) {
            // Swallow.
        }

        // Guarded failures remove their task row after reporting.
        // A lock-unavailable failure remains terminal in task storage.
        try {
            global $wpdb;
            if (is_object($wpdb)) {
                $tasksTable = $this->prefix() . Schema::BACKUP_TASKS_TABLE;
                /** @phpstan-ignore-next-line */
                @$wpdb->delete($tasksTable, ['snapshot_id' => $this->snapshotId()], ['%s']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- direct delete on plugin-owned table; no core helper exists
            }
        } catch (\Throwable $_) {
            // Swallow.
        }

        return self::PHASE_FAILED;
    }

    // ==================================================================
    // Advisory run-lock (GitHub issue #232)
    // ==================================================================

    /**
     * Reclaim snapshot state while holding the same guards run() uses.
     * The file lock is required; database-lock contention also vetoes the
     * callback. An unavailable database lock permits file-guarded reclamation.
     * Every acquired guard remains held throughout the callback and is
     * released even when it throws. Only the held inode's path is removed,
     * before releasing the file lock.
     *
     * @param callable(): void $callback Reclamation performed under the lock.
     * @return bool True when the callback ran; false on contention or when
     *              the file lock could not be acquired.
     */
    public function withRunLock(callable $callback): bool
    {
        $lockToken = $this->getLock();
        if ($lockToken === '0') {
            return false;
        }
        $lockHeld = ($lockToken === '1');

        $fileLock = $this->acquireFileLock();

        // GH #256 finding 2: a degraded (unattemptable) flock guard is NOT
        // "free" on this reclaim-only path; see this method's doc.
        if ($fileLock['handle'] === null && $fileLock['contended'] === false) {
            if ($lockHeld) {
                $this->releaseLock();
            }
            return false;
        }

        if ($fileLock['contended']) {
            if ($lockHeld) {
                $this->releaseLock();
            }
            return false;
        }

        /** @var resource $fileLockHandle */
        $fileLockHandle = $fileLock['handle'];

        try {
            $callback();
        } finally {
            if ($lockHeld) {
                $this->releaseLock();
            }
            // Reclaim the owner's path while its flock is still held.
            try {
                $this->cleanupFileLock($fileLockHandle);
            } finally {
                $this->releaseFileLock($fileLockHandle);
            }
        }

        return true;
    }

    /** Probe guard support without claiming work or removing a coordination path. */
    public function probeLockAvailability(): bool
    {
        $probeName = 'wpmgr_bk_probe_' . $this->snapshotId();
        $token = $this->getLock($probeName);
        $databaseAvailable = $token === '1' || $token === '0';
        $handle = null;
        try {
            $file = $this->acquireFileLock();
            $handle = $file['handle'];
            // Contention proves the guard works; existing owners remain protected.
            return $databaseAvailable || $handle !== null || $file['contended'];
        } catch (\Throwable $_) {
            return $databaseAvailable;
        } finally {
            if ($handle !== null) { $this->releaseFileLock($handle); }
            if ($token === '1') { $this->releaseLock($probeName); }
        }
    }

    /**
     * MySQL advisory-lock name for this snapshot's run. 9 + 36 = 45 chars,
     * well under MySQL's 64-character GET_LOCK() name limit.
     */
    private function lockName(): string
    {
        return 'wpmgr_bk_' . $this->snapshotId();
    }

    /**
     * Best-effort, non-blocking (timeout=0) MySQL advisory lock acquire.
     *
     * Adversarial-review correction: this is a FAST, CHEAP early-reject only
     * — it is NOT, by itself, a reliable mutual-exclusion guarantee. GET_LOCK
     * is scoped to $wpdb's own mysqli connection and is silently released the
     * instant that connection drops (a short `wait_timeout` on a managed
     * host, a MySQL restart/failover, or `$wpdb->check_connection()`
     * reconnecting after "server has gone away" — all plausible mid-backup,
     * e.g. during a large-table dump that only calls back to $wpdb once per
     * table). A dropped connection can hand a genuinely-still-alive winner's
     * '1' to a SECOND caller, so this alone can NOT promise "always exactly
     * one winner, never two" — see run()'s doc for why flock() (guard 2) is
     * the connection-independent, AUTHORITATIVE backstop that closes this
     * exact gap, and why a caller who is a fast reject here ('0') never even
     * needs to reach the more expensive flock check.
     *
     * @return string|null '1' when we won the lock, '0' when another session
     *                      already holds it (fast reject — no flock attempt
     *                      needed), null when GET_LOCK() is unsupported
     *                      (non-MySQL $wpdb driver) or the probe itself
     *                      failed — either way, run() still attempts the
     *                      authoritative flock() guard next.
     */
    private function getLock(?string $name = null): ?string
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return null;
        }
        /** @var \wpdb $wpdb */
        try {
            $result = $wpdb->get_var($wpdb->prepare('SELECT GET_LOCK(%s, 0)', $name ?? $this->lockName())); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- advisory run-lock probe; anti-double-fire correctness read, not a cacheable value
        } catch (\Throwable $e) {
            return null;
        }
        return $result === null ? null : (string) $result;
    }

    /**
     * Release the advisory lock acquired by getLock(). Best-effort — MySQL
     * releases the lock automatically when this session's connection closes,
     * so a failed RELEASE_LOCK() here is never fatal.
     */
    private function releaseLock(?string $name = null): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        /** @var \wpdb $wpdb */
        try {
            $wpdb->get_var($wpdb->prepare('SELECT RELEASE_LOCK(%s)', $name ?? $this->lockName())); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- advisory run-lock release; no core helper exists
        } catch (\Throwable $e) {
            // Swallow — MySQL releases the lock automatically on connection close.
        }
    }

    /**
     * Deterministic per-snapshot flock() lock-file path (GitHub issue #232
     * adversarial-review follow-up — see run()'s doc for the full
     * dead-connection rationale this guard closes).
     *
     * Derived from the PARENT of the per-snapshot scratch dir (the shared
     * "runs" base, e.g. `wp-content/wpmgr-agent/runs/`) rather than the
     * per-snapshot scratch dir itself, so the lock path is resolvable and
     * stable even before that per-snapshot directory has been created — a
     * watchdog resume or the reaper can race the very first
     * ensureScratchDir() call. Falls back to deriving the same "runs" base
     * from wp_content_path when scratch_dir itself is empty/malformed
     * (defensive; both params are always set together in practice).
     *
     * @return string Absolute lock-file path, or '' when neither base can be
     *                resolved at all (BackupCommand always sets scratch_dir
     *                before a runner is ever constructed, so this is only
     *                reachable via a malformed/hand-built params array).
     */
    private function fileLockPath(): string
    {
        $scratch = $this->scratchDir();
        $base    = $scratch !== '' ? dirname($scratch) : '';

        if ($base === '' || $base === '.' || $base === DIRECTORY_SEPARATOR) {
            $contentPath = $this->wpContentPath();
            $base        = $contentPath !== '' ? rtrim($contentPath, '/\\') . '/wpmgr-agent/runs' : '';
        }

        if ($base === '') {
            return '';
        }

        return rtrim($base, '/\\') . DIRECTORY_SEPARATOR . $this->snapshotId() . '.lock';
    }

    /**
     * Acquire the AUTHORITATIVE, connection-independent mutual-exclusion
     * guard for this snapshot's run (GitHub issue #232 adversarial-review
     * follow-up — see run()'s doc for the full rationale).
     *
     * Unlike getLock() (MySQL GET_LOCK, silently released if $wpdb's
     * connection drops mid-run), this flock() is held by the OS for as long
     * as this PHP process is alive and is released automatically the
     * instant it exits for ANY reason — including a SIGKILL that skips every
     * PHP-level `finally` — so a second caller's non-blocking attempt
     * correctly reports contention for as long as the first caller is
     * genuinely still running, independent of what happened to its database
     * connection in the meantime.
     *
     * Three outcomes, distinguished so a caller can tell "no one else is
     * running this" from "flock itself isn't usable right now":
     *   - ['handle' => resource, 'contended' => false]  — WON the lock. The
     *     caller must keep the handle open for the whole run and pass it to
     *     releaseFileLock() when done.
     *   - ['handle' => null,     'contended' => true]   — LOST: another live
     *     process holds this exact snapshot's lock file right now (the
     *     authoritative "someone else is really still running this" signal).
     *   - ['handle' => null,     'contended' => false]  — the guard could not
     *     even be attempted (no resolvable path, or the filesystem/
     *     permissions prevent opening the lock file — e.g. an exotic FS
     *     without flock() support). run() requires a held database guard
     *     in this case; reclamation requires a held file guard.
     *
     * @return array{handle:resource|null,contended:bool}
     */
    private function acquireFileLock(): array
    {
        $path = $this->fileLockPath();
        if ($path === '') {
            return ['handle' => null, 'contended' => false];
        }

        $dir = dirname($path);
        if (!is_dir($dir) && !wp_mkdir_p($dir) && !is_dir($dir)) {
            return ['handle' => null, 'contended' => false];
        }

        return self::acquireFileLockAtPath($path);
    }

    /**
     * Acquire an inode-checked, non-blocking file lock. A pathname change
     * retries acquisition when a fresh handle verifies the pathname's identity.
     * A fresh-handle mismatch makes the guard unavailable only while the
     * pathname retains the same identity throughout the diagnosis.
     * Existing-only acquisition never creates a janitor candidate.
     *
     * @return array{handle:resource|null,contended:bool}
     */
    public static function acquireFileLockAtPath(string $path, bool $create = true): array
    {
        for ($attempt = 0; $attempt < 3; $attempt++) {
            $handle = @fopen($path, $create ? 'c' : 'r+'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- flock requires a native file handle
            if ($handle === false) {
                return ['handle' => null, 'contended' => $attempt > 0];
            }
            $wouldBlock = 0;
            if (!@flock($handle, LOCK_EX | LOCK_NB, $wouldBlock)) {
                fclose($handle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- native flock handle
                return ['handle' => null, 'contended' => (bool) $wouldBlock];
            }
            if (self::fileLockMatchesPath($handle, $path)) {
                if ($create) { @chmod($path, 0600); } // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- private coordination file permissions
                return ['handle' => $handle, 'contended' => false];
            }
            clearstatcache(true, $path);
            $before = @stat($path);
            $fresh = @fopen($path, 'r+'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- diagnose identity using an existing pathname without creating it
            $reliable = true;
            if ($fresh !== false) {
                try { $reliable = self::fileLockMatchesPath($fresh, $path); }
                finally { fclose($fresh); } // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- close the diagnostic handle
            }
            clearstatcache(true, $path);
            $after = @stat($path);
            $stable = $before !== false && $after !== false
                && $before['dev'] === $after['dev'] && $before['ino'] === $after['ino'];
            flock($handle, LOCK_UN);
            fclose($handle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- release a stale inode before retrying
            if (!$reliable && $stable) { return ['handle' => null, 'contended' => false]; }
        }
        return ['handle' => null, 'contended' => true];
    }

    /**
     * Confirm that the pathname still names the held file's device and inode.
     *
     * @param resource $handle Locked file handle.
     */
    public static function fileLockMatchesPath($handle, string $path): bool
    {
        clearstatcache(true, $path);
        $held = @fstat($handle);
        $named = @stat($path);
        return $held !== false && $named !== false
            && $held['dev'] === $named['dev'] && $held['ino'] === $named['ino'];
    }

    /**
     * Release + close a flock handle returned by acquireFileLock(). Safe to
     * call with a non-resource (a no-op). Best-effort: a failed
     * flock(LOCK_UN) is never fatal — the OS releases the lock automatically
     * the instant this process exits for any reason.
     *
     * @param resource|null $handle
     */
    private function releaseFileLock($handle): void
    {
        if ($handle === null || !is_resource($handle)) {
            return;
        }
        flock($handle, LOCK_UN);
        fclose($handle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- releasing our own advisory lock handle; WP_Filesystem has no flock()-equivalent API and is never initialized in this headless agent context
    }

    /**
     * Reclaim only the terminal owner's lock pathname while its flock is
     * held. A different inode belongs to another owner and is retained.
     *
     * @param resource $handle This invocation's held file lock.
     */
    private function cleanupFileLock($handle): void
    {
        $path = $this->fileLockPath();
        if ($path !== '' && self::fileLockMatchesPath($handle, $path)) {
            wp_delete_file($path);
        }
    }

    /** A guardless runner may record a breadcrumb, but cannot advance or fail work. */
    private function handleUnavailableGuards(): string
    {
        try {
            $task = $this->loadTask();
            $phase = (string) ($task['phase'] ?? self::PHASE_QUEUED);
            if ($task !== null && $phase !== self::PHASE_COMPLETED && $phase !== self::PHASE_FAILED) {
                $this->recordLockContended('lock_unavailable');
            }
            return $phase;
        } catch (\Throwable $_) {
            return self::PHASE_QUEUED;
        }
    }

    /**
     * Record a lock breadcrumb without changing phase or progress time.
     * Every breadcrumb compares the raw cursor bytes before writing.
     *
     * Reads the RAW sub_state column value directly (NOT through loadTask(),
     * which follows the sidecar-spill pointer and would rehydrate a
     * potentially large de-sidecared cursor). Merging 'stage' into whatever
     * tiny envelope already lives in the column — a genuine small sub_state
     * OR a `{"_sidecar":true,...}` pointer object — keeps this write always
     * small and never risks re-writing a large cursor inline (the exact
     * @@max_allowed_packet overflow the sidecar exists to prevent).
     *
     * Best-effort: a failure here must never throw — the caller (run()'s
     * loser path) already wraps this in its own try/catch, but every DB call
     * inside is additionally guarded so nothing here can escalate on its own.
     *
     * @param string $stage Coordination breadcrumb.
     */
    private function recordLockContended(string $stage = 'lock_contended'): void
    {
        \WPMgr\Agent\Support\DebugLog::write(
            'WPMgr TaskRunner: ' . $stage . ' for snapshot ' . $this->snapshotId()
            . ' — skipping re-entry without mutating phase'
        );

        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        /** @var \wpdb $wpdb */
        $table = $this->tableName();
        if ($table === '') {
            return;
        }

        try {
            // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- interpolated identifier is prefix+constant (trusted); value bound via %s placeholder
            $sql      = "SELECT sub_state FROM {$table} WHERE snapshot_id = %s LIMIT 1";
            /** @phpstan-ignore-next-line */
            $prepared = $wpdb->prepare($sql, $this->snapshotId()); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- already prepared on the preceding line
            $raw = $wpdb->get_var($prepared); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching, WordPress.DB.PreparedSQL.NotPrepared, WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter, PluginCheck.Security.DirectDB.UnescapedDBParameter -- direct read on plugin-owned table; value is the output of $wpdb->prepare(); $table comes from tableName() (prefix+constant), opaque to static analysis but not user input

            $envelope = [];
            if (is_string($raw) && $raw !== '') {
                $decoded = self::decodeStateJson($raw);
                if (is_array($decoded)) {
                    $envelope = $decoded;
                }
            }
            $envelope['stage'] = $stage;
            $encoded = self::encodeSubState($envelope);
            if ($encoded === false) {
                return;
            }

            if (!is_string($raw)) { return; }
            // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- trusted plugin-owned table identifier; all values use placeholders
            $sql = "UPDATE {$table} SET sub_state = %s WHERE snapshot_id = %s AND BINARY sub_state = %s";
            $prepared = $wpdb->prepare($sql, $encoded, $this->snapshotId(), $raw); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- query uses placeholders for all values
            $wpdb->query($prepared); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching, WordPress.DB.PreparedSQL.NotPrepared -- byte-exact cursor compare-and-set on the plugin-owned table
        } catch (\Throwable $e) {
            // Swallow — a breadcrumb write failure must never surface from
            // the loser path.
        }
    }

    // ==================================================================
    // Phase handlers
    // ==================================================================

    /**
     * Decide the next phase after `queued`.
     *
     * The DB dump is gated by two signals (in priority order):
     *   1. include_db (explicit CP signal derived from the components allowlist).
     *      When the CP sends a components filter it derives include_db:
     *        - include_db=true  → dump the DB regardless of kind.
     *        - include_db=false → skip the DB dump regardless of kind.
     *      When include_db is absent (null) → fall back to (2).
     *   2. snapshot kind: 'files' skips the DB dump; 'db' and 'full' dump it.
     *
     * ADR-051: incremental runs use the SAME phases as a full; the only
     * difference is that runArchivingFiles loads the prevMap from the parent
     * files.list (assembled from PrevFilesListChunks in params).
     */
    private function nextAfterQueued(): string
    {
        if (!$this->shouldDumpDb()) {
            // No DB dump: go straight to file-archiving (or straight to
            // encrypt if the kind is 'db' with include_db=false — a db-only
            // selection would have include_db=true, so this case means the
            // operator deselected db from an otherwise full/files run).
            return $this->kind() === self::KIND_DB
                ? self::PHASE_ENCRYPTING_UPLOADING
                : self::PHASE_ARCHIVING_FILES;
        }
        return self::PHASE_DUMPING_DB;
    }

    /**
     * Decide the next phase after `dumping_db`.
     *
     * For DB-only snapshots with no file components: PHASE_ENCRYPTING_UPLOADING.
     * For full snapshots (and incremental): PHASE_ARCHIVING_FILES.
     *
     * When include_db=true with a files-only kind (e.g. operator selected
     * [db] component while kind=files), the dump ran first; now go to
     * encrypting_uploading since there are no file components to archive.
     */
    private function nextAfterDumpingDb(): string
    {
        // If the kind is 'db', or if a components filter has no file-archiver
        // components (i.e. the only selected component was 'db'), skip archiving.
        if ($this->kind() === self::KIND_DB) {
            return self::PHASE_ENCRYPTING_UPLOADING;
        }
        // Check if the components allowlist has any file-archiver kinds.
        // If the filter is active and contains no file kinds, go straight
        // to encrypt (db-only effect even with kind=full).
        $components = isset($this->params['components']) && is_array($this->params['components'])
            ? array_values(array_filter($this->params['components'], 'is_string'))
            : [];
        $fileKinds = ['plugin', 'theme', 'upload', 'wp-content'];
        if ($components !== [] && array_intersect($components, $fileKinds) === []) {
            // Components filter is active but contains only 'db' and/or 'core'.
            // No file archiving needed.
            return self::PHASE_ENCRYPTING_UPLOADING;
        }
        return self::PHASE_ARCHIVING_FILES;
    }

    /**
     * Determine whether the DB dump phase should run, honoring the
     * include_db signal from the CP's components allowlist.
     *
     * Rules (evaluated in priority order):
     *   1. When include_db is explicitly false → skip the dump.
     *   2. When include_db is explicitly true  → dump regardless of kind.
     *   3. When include_db is absent (null)    → follow snapshot kind:
     *      kind=files → no dump; kind=db|full → dump.
     */
    private function shouldDumpDb(): bool
    {
        // Explicit CP signal (present when a components allowlist is active).
        if (array_key_exists('include_db', $this->params) && $this->params['include_db'] !== null) {
            return (bool) $this->params['include_db'];
        }
        // Fall back to the legacy kind-based heuristic.
        return $this->kind() !== self::KIND_FILES;
    }

    /**
     * Run the DB-dump phase to completion. Writes `<scratch>/database.sql.gz`
     * and returns the new sub_state with `db.done=true`.
     *
     * @param array<string,mixed> $subState Current sub_state.
     * @return array<string,mixed> Updated sub_state.
     */
    private function runDumpingDb(array $subState): array
    {
        $outPath = $this->scratchDir() . DIRECTORY_SEPARATOR . 'database.sql.gz';
        $resume  = isset($subState['db']) && is_array($subState['db']) ? $subState['db'] : [];

        $this->ensureScratchDir();

        // Persist a fresh last_progress_at before the long-running call so
        // the watchdog sees activity even if we never callback.
        $this->saveTaskState(self::PHASE_DUMPING_DB, $subState);

        $dumper = new DbDumper($this->dbCreds());
        $result = $dumper->dump($outPath, $resume, function (string $phase, array $detail): void {
            $this->onPhaseProgress($phase, $detail);
        });

        $subState['db'] = $result;
        return $subState;
    }

    /**
     * Run the files-archive phase to completion. Writes
     * `<scratch>/<component>.partNNN.zip` files and returns sub_state with
     * `files.done=true`.
     *
     * ADR-051 incremental mode: when is_incremental=true in params, the
     * parent's files.list is assembled from PrevFilesListChunks (presigned
     * GET URLs sent by the CP) into a scratch file, then loaded into a
     * prevMap. FilesArchiver receives the prevMap so only CHANGED/NEW files
     * are packed into the archive parts. Tombstones (deleted files) are
     * returned in the result and persisted in sub_state.files.tombstones.
     *
     * @param array<string,mixed> $subState Current sub_state.
     * @return array<string,mixed> Updated sub_state.
     */
    private function runArchivingFiles(array $subState): array
    {
        $resume = isset($subState['files']) && is_array($subState['files']) ? $subState['files'] : [];

        $this->ensureScratchDir();
        $this->saveTaskState(self::PHASE_ARCHIVING_FILES, $subState);

        // ADR-051: build the prevMap for incremental change detection.
        // $prevMap===null => full mode (archive everything). $prevMap===[] =>
        // documented base signal (no parent files.list, all files are "new").
        // A non-empty map filters changed/new only.
        $prevMap        = null;
        $prevChunkCount = 0;
        $prevMapLoaded  = false;
        $prevMapSize    = 0;
        if ($this->isIncremental()) {
            $chunks = isset($this->params['prev_files_list_chunks']) && is_array($this->params['prev_files_list_chunks'])
                ? $this->params['prev_files_list_chunks']
                : [];
            $prevChunkCount = count($chunks);

            $prevMap = $this->loadPrevFilesListMap($subState);
            // ADR-051 prevMap pipeline integrity guard. The empty-vs-null
            // distinction is the difference between "carry forward correctly"
            // and "silently re-archive the whole site":
            //   - null  => fetch/decode FAILED -> full mode (safe but not delta).
            //   - []  with chunks sent => the parent's files.list parsed to ZERO
            //            entries even though chunks WERE presigned. That is a
            //            corrupt/format-mismatched prev list, NOT a legit base.
            //            Treating it as a base would re-archive everything and
            //            look like success. Force full mode (null) so it is at
            //            least correct, and surface it loudly.
            $prevMapLoaded = ($prevMap !== null);
            $prevMapSize   = is_array($prevMap) ? count($prevMap) : 0;
            if ($prevMap === [] && $prevChunkCount > 0) {
                \WPMgr\Agent\Support\DebugLog::write(sprintf(
                    'WPMgr TaskRunner: ADR-051 prevMap EMPTY despite %d prev_files_list_chunks ' .
                    '(snapshot=%s gen=%s) — parent files.list parsed to zero entries; ' .
                    'falling back to FULL re-archive. Investigate the prev files.list wire format.',
                    $prevChunkCount,
                    $this->snapshotId(),
                    isset($this->params['generation']) ? (string) $this->params['generation'] : '?'
                ));
                $prevMap       = null;
                $prevMapLoaded = false;
            }
        }

        // ADR-051: namespace the archive part filenames by generation so the
        // restore overlay never collides part names across generations (gen-0
        // and gen-1 both used to emit `plugins.part001.zip`).
        $generation = isset($this->params['generation']) && is_numeric($this->params['generation'])
            ? (int) $this->params['generation']
            : 0;

        // Track A (#187): selective component backup + exclusions. All fields
        // are optional (absent on pre-m49 CP); absent means "use agent defaults"
        // so older CP versions continue to work unchanged.
        //
        // Operator-specified extra exclude path segments merged with DEFAULT_EXCLUDES.
        $extraExcludePaths = isset($this->params['exclude_paths']) && is_array($this->params['exclude_paths'])
            ? array_values(array_filter($this->params['exclude_paths'], 'is_string'))
            : [];
        // Lowercase file extensions to skip (without leading dot).
        $excludeExtensions = isset($this->params['exclude_extensions']) && is_array($this->params['exclude_extensions'])
            ? array_values(array_filter($this->params['exclude_extensions'], 'is_string'))
            : [];
        // File size ceiling in MiB (0 = no filter).
        $excludeFileSizeMb = isset($this->params['exclude_file_size_mb']) && is_numeric($this->params['exclude_file_size_mb'])
            ? max(0, (int) $this->params['exclude_file_size_mb'])
            : 0;

        // A1 (#187): selective component backup. 'components' from the CP is the
        // full list of what to include, using the canonical SINGULAR entry_kind
        // vocabulary (plugin | theme | upload | wp-content | db | core).
        // We forward only the file-archiver relevant singular kinds to
        // FilesArchiver::include_components; 'db' and 'core' are handled by
        // separate code paths (DbDumper and CoreFilesArchiver respectively).
        // FilesArchiver's constructor normalizes the singular kinds to its
        // internal plural bucket keys via KIND_TO_BUCKET.
        // An empty/absent list means "include all file-archiver components".
        $requestedComponents = isset($this->params['components']) && is_array($this->params['components'])
            ? array_values(array_filter($this->params['components'], 'is_string'))
            : [];
        // Canonical singular file-component kinds that FilesArchiver handles.
        // Do NOT include 'db' or 'core' — they are separate pipeline steps.
        $fileKinds = ['plugin', 'theme', 'upload', 'wp-content'];
        $includeFileComponents = $requestedComponents !== []
            ? array_values(array_intersect($requestedComponents, $fileKinds))
            : [];

        $archiverOpts = [];
        if ($excludeExtensions !== []) {
            $archiverOpts['exclude_extensions'] = $excludeExtensions;
        }
        if ($excludeFileSizeMb > 0) {
            $archiverOpts['exclude_file_size_mb'] = $excludeFileSizeMb;
        }
        // A1 correctness (#187): when a components filter IS active (requestedComponents
        // is non-empty), always forward include_components to FilesArchiver — even when
        // the resolved file-archiver kinds list is empty (e.g. components=["core"] or
        // components=["core","db"]).  The presence of the key is the sentinel FilesArchiver
        // uses to distinguish "filter active, archive nothing from wp-content" from "no
        // filter present, archive everything".  An absent key still means "full backup,
        // include all components", preserving backward compatibility with pre-m49 callers.
        if ($requestedComponents !== []) {
            $archiverOpts['include_components'] = $includeFileComponents;
        }

        $archiver = new FilesArchiver($this->wpContentPath(), $extraExcludePaths, $archiverOpts, $generation);
        $result   = $archiver->archive($this->scratchDir(), $resume, function (string $phase, array $detail): void {
            $this->onPhaseProgress($phase, $detail);
        }, $prevMap, function (array $cursor) use (&$subState): void {
            // GH #954: durable archive progress. A worker the host ends
            // resumes after the last closed part instead of starting over.
            $subState['files'] = $cursor;
            try {
                $this->saveTaskState(self::PHASE_ARCHIVING_FILES, $subState);
            } catch (\Throwable $e) {
                if ($e instanceof \RuntimeException && $e->getCode() === self::CODE_TASK_GONE) {
                    // GH #983: the task was given up; never archive on for it.
                    throw $e;
                }
                // The previous checkpoint stays authoritative: a resume
                // deletes the parts closed since it and archives them again.
                \WPMgr\Agent\Support\DebugLog::write('WPMgr TaskRunner: archive checkpoint not saved for snapshot ' . $this->snapshotId() . ': ' . $e->getMessage());
                return;
            }
            // GH #983: hand over only on a saved checkpoint, so the next
            // worker loses nothing this one closed.
            if ($this->shouldHandOver()) {
                throw new \RuntimeException('worker used most of its time limit', self::CODE_HANDOVER); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- fixed literal, never output
            }
        });

        // Track A (#187): include_core — archive the WordPress core source root
        // (ABSPATH: wp-admin, wp-includes, root PHP files including wp-config.php)
        // as an additional source. Emits entry_kind="core" manifest entries alongside
        // the normal wp-content parts. Only runs when include_core=true.
        if (!empty($this->params['include_core'])) {
            $absPath = defined('ABSPATH') ? rtrim(ABSPATH, '/') : '';
            if ($absPath !== '' && is_dir($absPath)) {
                $coreArchiver = new CoreFilesArchiver($absPath, $generation);
                $coreResult   = $coreArchiver->archive($this->scratchDir(), function (string $phase, array $detail): void {
                    $this->onPhaseProgress($phase, $detail);
                });
                // Merge core result parts into the main result so they are picked
                // up by the encrypting_uploading phase (same part-list scan).
                if (isset($coreResult['parts']) && is_array($coreResult['parts'])) {
                    $result['core_parts'] = $coreResult['parts'];
                }
            }
        }

        // ADR-051 instrumentation: report the full prevMap pipeline so the next
        // live run is conclusive (prev_chunks_count / prevmap_loaded / prevmap_size
        // / files_total / files_changed / files_carried / tombstones), to BOTH
        // the CP progress payload and error_log.
        if ($this->isIncremental()) {
            $filesChanged  = isset($result['files_changed']) ? (int) $result['files_changed'] : (int) ($result['files_total'] ?? 0);
            $filesCarried  = isset($result['files_carried']) ? (int) $result['files_carried'] : 0;
            $filesTotalAll = $filesChanged + $filesCarried;
            $tombstones    = isset($result['tombstones_count']) ? (int) $result['tombstones_count'] : 0;
            $instr = [
                'prev_chunks_count' => $prevChunkCount,
                'prevmap_loaded'    => $prevMapLoaded,
                'prevmap_size'      => $prevMapSize,
                'files_total'       => $filesTotalAll,
                'files_changed'     => $filesChanged,
                'files_carried'     => $filesCarried,
                'tombstones'        => $tombstones,
            ];
            $this->postProgress('archiving_files', $instr);
            \WPMgr\Agent\Support\DebugLog::write(sprintf(
                'WPMgr TaskRunner: ADR-051 increment instrumentation snapshot=%s gen=%d ' .
                'prev_chunks_count=%d prevmap_loaded=%s prevmap_size=%d ' .
                'files_total=%d files_changed=%d files_carried=%d tombstones=%d',
                $this->snapshotId(),
                $generation,
                $prevChunkCount,
                $prevMapLoaded ? 'true' : 'false',
                $prevMapSize,
                $filesTotalAll,
                $filesChanged,
                $filesCarried,
                $tombstones
            ));
        }

        $subState['files'] = $result;
        return $subState;
    }

    /**
     * ADR-051: Fetch the parent snapshot's files.list via PrevFilesListChunks
     * (presigned GET URLs sent by the CP in the request) and build the prevMap.
     *
     * Each chunk URL is fetched in order and the bodies concatenated into a
     * local scratch file `prev_files.list`. The file is then parsed by
     * FilesArchiver::loadPrevMap() into an in-memory map never persisted to
     * sub_state.
     *
     * Return contract (the caller interprets the empty-vs-null distinction —
     * see runArchivingFiles):
     *   - []   : no prev chunks were sent (legit gen-0 base) -> archive everything
     *            as "new" but still emit a files.list. Also the literal parse of
     *            a prev list that genuinely had zero entries.
     *   - null : a fetch FAILED (auth/URL/transport) -> caller treats as full mode.
     *   - non-empty map : the parent's prev[rel]=>{size,mtime} for change detection.
     *
     * @param array<string,mixed> $subState Current sub_state (unused; future resume hook).
     * @return array<string,array{size:int,mtime:int}>|null prevMap, [] for base, or null on fetch failure.
     */
    private function loadPrevFilesListMap(array $subState): ?array
    {
        $chunks = isset($this->params['prev_files_list_chunks']) && is_array($this->params['prev_files_list_chunks'])
            ? $this->params['prev_files_list_chunks']
            : [];

        if ($chunks === []) {
            // No parent files.list chunks — treat the first incremental as a
            // base (all files are "new"). Return an empty map (not null) so the
            // archiver still emits files.list but doesn't filter anything.
            return [];
        }

        $localPath = $this->scratchDir() . DIRECTORY_SEPARATOR . 'prev_files.list';

        // Fetch and concatenate each chunk. Each entry in $chunks is a RestoreChunk:
        // at minimum: ['url' => '<presigned-get-url>', 'hash' => '<blake3>'].
        $outHandle = @fopen($localPath, 'wb'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming handle for chunked fwrite over multi-GB prev-files-list; WP_Filesystem exposes only whole-file get/put which would OOM
        if ($outHandle === false) {
            \WPMgr\Agent\Support\DebugLog::write('WPMgr TaskRunner: cannot create prev_files.list scratch file');
            return null;
        }

        try {
            foreach ($chunks as $chunk) {
                if (!is_array($chunk) || empty($chunk['url']) || !is_string($chunk['url'])) {
                    continue;
                }
                $url  = (string) $chunk['url'];
                $body = $this->fetchUrl($url);
                if ($body === null) {
                    // A failed presigned GET means we cannot diff — return null
                    // (full mode), NOT [] (which the caller reads as a legit base).
                    // The finally below closes the handle exactly once. (Closing
                    // here too would double-close: in PHP 8 fclose() on a closed
                    // resource throws a TypeError that `@` does NOT suppress, which
                    // would crash the whole archive phase instead of falling back.)
                    \WPMgr\Agent\Support\DebugLog::write('WPMgr TaskRunner: failed to fetch prev_files.list chunk from ' . $url);
                    return null;
                }
                fwrite($outHandle, $body); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fwrite -- incremental write into a streaming handle; WP_Filesystem put_contents is whole-buffer only
            }
        } finally {
            if (is_resource($outHandle)) {
                fclose($outHandle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- closes a streaming handle over multi-GB archives; WP_Filesystem has no streaming API
            }
        }

        return FilesArchiver::loadPrevMap($localPath);
    }

    /**
     * Perform a simple GET request and return the body, or null on failure.
     * Used exclusively for fetching PrevFilesListChunks (presigned GET URLs).
     * Prefers ext-curl for streaming; falls back to file_get_contents.
     *
     * @param string $url Presigned GET URL.
     * @return string|null Response body or null on error.
     *
     * `protected` is a deliberate test seam: the ADR-051 prevMap-pipeline e2e
     * drives this fetch with `file://` URLs (the same prev-list bytes the base
     * wrote) so the rest of loadPrevFilesListMap -> loadPrevMap -> archive runs
     * as production does, end to end.
     */
    protected function fetchUrl(string $url): ?string
    {
        // ext-curl is configured for the HTTP(S) presigned URLs the CP sends.
        // Any non-HTTP scheme (e.g. a local file:// the e2e uses) goes straight
        // to the stream fallback — some curl builds disable file:// entirely.
        $scheme    = strtolower((string) wp_parse_url($url, PHP_URL_SCHEME));
        $isHttpish = ($scheme === 'http' || $scheme === 'https');

        if ($isHttpish && function_exists('wp_remote_get')) {
            $response = wp_remote_get($url, ['timeout' => 60, 'sslverify' => true]);
            if (is_wp_error($response)) {
                return null;
            }
            $code = (int) wp_remote_retrieve_response_code($response);
            if ($code < 200 || $code >= 300) {
                return null;
            }
            return (string) wp_remote_retrieve_body($response);
        }

        // Fallback: only file:// URLs are permitted here. This path exists
        // exclusively for the ADR-051 e2e tests, which feed local fixture
        // archives as file:// URIs. Any other scheme returns null — if
        // wp_remote_get is unavailable we cannot make remote HTTP requests.
        if (!str_starts_with($url, 'file://')) {
            return null;
        }
        $body = @file_get_contents($url); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_get_contents_file_get_contents -- file:// local path only; wp_remote_get does not support file:// URIs; used solely for ADR-051 e2e tests
        return $body === false ? null : $body;
    }

    /**
     * Run pass 1 (encrypt) + pass 2 (upload) of the chunk pipeline. Assembles
     * the artifact list from prior sub_state, then drives EncryptAndUpload.
     *
     * @param array<string,mixed> $subState Current sub_state.
     * @return array<string,mixed> Updated sub_state (`encrypt.entries` for pass 3).
     */
    private function runEncryptingUploading(array $subState): array
    {
        $artifacts = $this->assembleArtifacts($subState);
        if ($artifacts === []) {
            throw new \RuntimeException('TaskRunner: no artifacts to encrypt');
        }

        $pipeline = new EncryptAndUpload(
            new AgeCrypto(),
            new BackupTransport(new Signer(new Keystore())),
            $this->snapshotId(),
            (string) $this->params['age_recipient'],
            (string) $this->params['presign_endpoint'],
            (string) $this->params['manifest_endpoint'],
            (int) ($this->params['chunk_bytes'] ?? EncryptAndUpload::DEFAULT_CHUNK_BYTES)
        );

        // ---- Pass 1: encrypt. ----
        $encResume = isset($subState['encrypt']) && is_array($subState['encrypt']) ? $subState['encrypt'] : [];

        $this->saveTaskState(self::PHASE_ENCRYPTING_UPLOADING, $subState);

        $encCursor = $pipeline->encryptChunks(
            $this->scratchDir(),
            $artifacts,
            $encResume,
            function (string $phase, array $detail): void {
                $this->onPhaseProgress($phase, $detail);
            }
        );
        // ADR-051: append per-path tombstone manifest entries (entry_kind=
        // tombstones, mode=Delete, empty chunk list). The CP restore overlay
        // reads them via ListManifest with no chunk fetch to resolve the
        // deleted set (newest-wins). Done AFTER encrypt so the entries list the
        // manifest submit reads (sub_state.encrypt.entries) carries them, and
        // it survives a watchdog re-entry (persisted in sub_state.encrypt).
        $encCursor['entries'] = $this->appendTombstoneEntries(
            (isset($encCursor['entries']) && is_array($encCursor['entries'])) ? $encCursor['entries'] : [],
            $subState
        );
        $subState['encrypt'] = $encCursor;
        // Checkpoint between passes so an upload-pass crash doesn't redo
        // the (CPU-expensive) encrypt pass.
        $this->saveTaskState(self::PHASE_ENCRYPTING_UPLOADING, $subState);

        // ---- Pass 2: upload. ----
        $uploadResume = isset($subState['upload']) && is_array($subState['upload']) ? $subState['upload'] : [];
        // Pass scratch_dir via the cursor so EncryptAndUpload knows where
        // to find the chunks-<hash>.age files.
        $uploadResume['scratch_dir'] = $this->scratchDir();

        $upCursor = $pipeline->uploadChunks(
            $encCursor,
            $uploadResume,
            function (string $phase, array $detail): void {
                $this->onPhaseProgress($phase, $detail);
            },
            // GH #283: durable mid-loop checkpoint. Unlike the progress
            // callback above (observability, best-effort, failures
            // swallowed), this writes the partial uploaded_hashes cursor
            // into sub_state.upload via the SAME saveTaskState() path the
            // outer phase-dispatch loop uses, so a worker kill between two
            // checkpoints resumes from a cursor that matches exactly which
            // local chunk files EncryptAndUpload has already deleted. A
            // thrown saveTaskState() failure here propagates out of
            // uploadChunks() (per its checkpoint contract) so no local file
            // is ever deleted on top of an unpersisted cursor.
            //
            // Local chunks remain until the checkpoint is acknowledged.
            // Unavailable storage or encoding failures retain them for resume.
            function (array $partialUploadCursor) use (&$subState): void {
                $subState['upload'] = $partialUploadCursor;
                $persisted = $this->saveTaskState(self::PHASE_ENCRYPTING_UPLOADING, $subState);
                if (!$persisted) {
                    throw new \RuntimeException('GH283: upload cursor persist failed; keeping local chunks for resume');
                }
            }
        );
        $subState['upload'] = $upCursor;
        return $subState;
    }

    /**
     * Run pass 3 (submit manifest). Reads the prepared entries list out of
     * `sub_state.encrypt.entries`; the runner is intentionally pass-through
     * here — EncryptAndUpload owns the entries shape end-to-end.
     *
     * @param array<string,mixed> $subState Current sub_state.
     */
    private function runSubmittingManifest(array $subState): void
    {
        $encrypt = (isset($subState['encrypt']) && is_array($subState['encrypt'])) ? $subState['encrypt'] : [];
        $entries = (isset($encrypt['entries']) && is_array($encrypt['entries'])) ? $encrypt['entries'] : [];
        if ($entries === []) {
            throw new \RuntimeException('TaskRunner: sub_state.encrypt.entries missing for manifest submit');
        }

        $pipeline = new EncryptAndUpload(
            new AgeCrypto(),
            new BackupTransport(new Signer(new Keystore())),
            $this->snapshotId(),
            (string) $this->params['age_recipient'],
            (string) $this->params['presign_endpoint'],
            (string) $this->params['manifest_endpoint'],
            (int) ($this->params['chunk_bytes'] ?? EncryptAndUpload::DEFAULT_CHUNK_BYTES)
        );

        $this->saveTaskState(self::PHASE_SUBMITTING_MANIFEST, $subState);

        $pipeline->submitManifest($entries, function (string $phase, array $detail): void {
            $this->onPhaseProgress($phase, $detail);
        });
    }

    // ==================================================================
    // Incremental helpers
    // ==================================================================

    /** Whether this run is incremental (set in params by BackupCommand). */
    private function isIncremental(): bool
    {
        return !empty($this->params['is_incremental']);
    }

    /**
     * Build the ordered artifact list (DB dump + files parts + files.list +
     * optional tombstones.list) from completed sub_state. Order matters: the
     * manifest entries reflect this order. DB is listed before zip parts by
     * convention; files.list and tombstones.list trail the zip parts so the
     * restore overlay processes them after the archive content.
     *
     * ADR-051: every backup (full and incremental alike) emits a files.list
     * artifact. An incremental additionally emits a tombstones.list when
     * any files were deleted from the parent snapshot.
     *
     * @param array<string,mixed> $subState Current sub_state.
     * @return list<array{path:string,logical:string}>
     */
    private function assembleArtifacts(array $subState): array
    {
        $artifacts = [];

        // DB artifact (only present for kind in {db, full}).
        $db = (isset($subState['db']) && is_array($subState['db'])) ? $subState['db'] : [];
        if (!empty($db['done']) && !empty($db['output_path']) && is_string($db['output_path'])) {
            $artifacts[] = [
                'path'    => (string) $db['output_path'],
                'logical' => 'database.sql.gz',
            ];
        }

        // Files artifacts (only present for kind in {files, full}). FilesArchiver
        // returns 'parts' as a list of basename strings — we map each to the
        // absolute scratch path.
        $files = (isset($subState['files']) && is_array($subState['files'])) ? $subState['files'] : [];
        if (!empty($files['done']) && isset($files['parts']) && is_array($files['parts'])) {
            foreach ($files['parts'] as $partName) {
                if (!is_string($partName) || $partName === '') {
                    continue;
                }
                $abs = $this->scratchDir() . DIRECTORY_SEPARATOR . $partName;
                $artifacts[] = [
                    'path'    => $abs,
                    'logical' => $partName,
                ];
            }
        }

        // Track A / A2 (#187): core archive parts emitted by CoreFilesArchiver.
        // Stored in files['core_parts'] as a list of basename strings and treated
        // identically to the wp-content parts above.
        if (!empty($files['core_parts']) && is_array($files['core_parts'])) {
            foreach ($files['core_parts'] as $partName) {
                if (!is_string($partName) || $partName === '') {
                    continue;
                }
                $abs = $this->scratchDir() . DIRECTORY_SEPARATOR . $partName;
                $artifacts[] = [
                    'path'    => $abs,
                    'logical' => $partName,
                ];
            }
        }

        // ADR-051: files.list sidecar — emitted by FilesArchiver alongside
        // every archive run (full and incremental). Uploaded as a normal
        // manifest entry (entry_kind=files-list) so the CP can presign it
        // for the next increment's PrevFilesListChunks.
        $filesListPath = $this->scratchDir() . DIRECTORY_SEPARATOR . FilesArchiver::FILES_LIST_NAME;
        if (is_file($filesListPath)) {
            $artifacts[] = [
                'path'    => $filesListPath,
                'logical' => FilesArchiver::FILES_LIST_NAME,
            ];
        }

        // ADR-051: tombstones are NOT shipped as a chunked `tombstones.list`
        // artifact. They are emitted as per-path manifest entries
        // (entry_kind=tombstones, mode=Delete, empty chunk list) appended to
        // the manifest after the encrypt pass — see tombstoneManifestEntries().
        // The CP restore overlay reads them via ListManifest with no chunk
        // fetch, so the bytes-on-the-wire `tombstones.list` is obsolete.

        return $artifacts;
    }

    /**
     * ADR-051: append per-path tombstone manifest entries to the encrypt-pass
     * entries list. A tombstone is the EXACT shape the CP restore overlay
     * reads via ListManifest:
     *   - entry_kind = 'tombstones' (self::ENTRY_KIND_TOMBSTONES)
     *   - path       = the deleted relpath
     *   - mode       = TOMBSTONE_MODE_DELETE (the agent never emits Readd; a
     *                  re-added file is simply re-packed into a part)
     *   - chunks     = [] (empty — no chunk fetch on restore)
     *
     * Deleted relpaths are read from the on-disk tombstones.list file whose
     * path is stored in sub_state.files.tombstones_file. This keeps sub_state
     * small even when thousands of files are deleted (a deletion-heavy
     * increment adds only two scalar fields to sub_state rather than a
     * multi-KB array). Backward-compat: if tombstones_file is absent but an
     * inline tombstones array is present (unit-test or carry-forward scenario),
     * the inline array is used instead.
     *
     * A path that fails basic sanitization (absolute, '..'/'.' segment, NUL
     * byte) is dropped here as defense-in-depth — the CP re-sanitizes and the
     * agent restore re-checks.
     *
     * Idempotent on watchdog re-entry: any tombstone entries already present in
     * $entries (matched by path) are not duplicated.
     *
     * @param list<array<string,mixed>> $entries  Encrypt-pass manifest entries.
     * @param array<string,mixed>       $subState Current sub_state.
     * @return list<array<string,mixed>> Entries with tombstone entries appended.
     */
    private function appendTombstoneEntries(array $entries, array $subState): array
    {
        $files = (isset($subState['files']) && is_array($subState['files'])) ? $subState['files'] : [];

        // Existing tombstone paths (idempotent re-entry guard).
        $already = [];
        foreach ($entries as $e) {
            if (is_array($e) && ($e['entry_kind'] ?? '') === self::ENTRY_KIND_TOMBSTONES) {
                $already[(string) ($e['path'] ?? '')] = true;
            }
        }

        // Primary: read from the on-disk tombstones.list file (flat, one per line).
        // Written incrementally by FilesArchiver — never loaded into a PHP array
        // at archiving time, so deletion of a large plugin tree costs only a
        // constant amount of RAM during the archiving phase.
        $tombstonesFile = isset($files['tombstones_file']) && is_string($files['tombstones_file'])
            ? $files['tombstones_file']
            : '';

        if ($tombstonesFile !== '' && is_file($tombstonesFile)) {
            $fh = @fopen($tombstonesFile, 'rb'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming handle for chunked fread over multi-GB tombstones list; WP_Filesystem exposes only whole-file get/put which would OOM
            if ($fh !== false) {
                while (($line = fgets($fh)) !== false) {
                    $rel = rtrim($line, "\r\n");
                    if ($rel === '' || isset($already[$rel])) {
                        continue;
                    }
                    if (!self::isSafeTombstonePath($rel)) {
                        continue;
                    }
                    $entries[] = [
                        'path'       => $rel,
                        'entry_kind' => self::ENTRY_KIND_TOMBSTONES,
                        'table_name' => '',
                        'mode'       => self::TOMBSTONE_MODE_DELETE,
                        'size'       => 0,
                        'chunks'     => [],
                    ];
                    $already[$rel] = true;
                }
                fclose($fh); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- closes a streaming handle over multi-GB archives; WP_Filesystem has no streaming API
            }
            return $entries;
        }

        // Backward-compat fallback: inline tombstones array (unit tests / carry-forward
        // from a sub_state written before the on-disk-only model was introduced).
        $tombstones = (isset($files['tombstones']) && is_array($files['tombstones'])) ? $files['tombstones'] : [];
        foreach ($tombstones as $rel) {
            if (!is_string($rel) || $rel === '') {
                continue;
            }
            if (isset($already[$rel])) {
                continue;
            }
            if (!self::isSafeTombstonePath($rel)) {
                continue;
            }
            $entries[] = [
                'path'       => $rel,
                'entry_kind' => self::ENTRY_KIND_TOMBSTONES,
                'table_name' => '',
                'mode'       => self::TOMBSTONE_MODE_DELETE,
                'size'       => 0,
                'chunks'     => [],
            ];
            $already[$rel] = true;
        }

        return $entries;
    }

    /**
     * Basic tombstone-path sanitization (defense-in-depth; the CP and the
     * agent restore engine re-check). Rejects empty, absolute, NUL-bearing, and
     * any path with a '..'/'.' segment.
     */
    private static function isSafeTombstonePath(string $p): bool
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

    // ==================================================================
    // Persistence + progress
    // ==================================================================

    /**
     * Decode a raw `sub_state` DB column value, following the sidecar-spill
     * pointer to disk when present (see SUBSTATE_SIDECAR_THRESHOLD /
     * saveTaskState()). This is the SAME rehydration loadTask() applies to
     * its own read, factored out as a shared static helper so any other
     * caller that only has the raw column value from an already-SELECTed
     * row (Watchdog's give-up paths, none of which go through loadTask()
     * because they must read sub_state.params BEFORE deciding whether to
     * re-enter run() at all) gets the exact same sidecar-aware behavior
     * instead of a second, independently duplicated (and, until GH #256
     * finding 5, silently sidecar-blind) decode.
     *
     * @param mixed $raw Raw `sub_state` column value.
     * @return array<string,mixed> Decoded sub_state, or [] on any failure
     *                             (missing column, invalid JSON, unreadable
     *                             sidecar file).
     */
    public static function decodeSubStateColumn($raw): array
    {
        if (!is_string($raw) || $raw === '') {
            return [];
        }
        $decoded = self::decodeStateJson($raw);
        if (!is_array($decoded)) {
            return [];
        }

        // Sidecar rehydration: when the DB column holds a pointer (written
        // by saveTaskState() when sub_state exceeded
        // SUBSTATE_SIDECAR_THRESHOLD), read the full cursor from disk. This
        // is exactly the case that matters most for a scratch reclaim: the
        // biggest runs (the most scratch to reclaim) are the ones most
        // likely to have spilled.
        if (!empty($decoded[self::SUBSTATE_SIDECAR_KEY]) && isset($decoded['file']) && is_string($decoded['file'])) {
            $sidecarPath = (string) $decoded['file'];
            if (is_file($sidecarPath)) {
                $sidecarRaw = @file_get_contents($sidecarPath);
                if ($sidecarRaw !== false && $sidecarRaw !== '') {
                    $full = self::decodeStateJson($sidecarRaw);
                    if (is_array($full)) {
                        $decoded = $full;
                    }
                }
            }
        }

        return $decoded;
    }

    /** Encode resume state without changing filename bytes in keys or values. */
    public static function encodeSubState(array $state): string
    {
        $json = json_encode($state);
        if ($json === false && json_last_error() === JSON_ERROR_UTF8) {
            $json = json_encode(['_wpmgr_byte_state' => 1, 'data' => self::mapStateBytes($state, true)]);
        }
        if ($json === false) { throw new \RuntimeException('TaskRunner: cannot encode task state: ' . esc_html(json_last_error_msg())); }
        return $json;
    }

    /** @return array<string,mixed>|null */
    private static function decodeStateJson(string $json): ?array
    {
        $state = json_decode($json, true);
        if (!is_array($state)) { return null; }
        if (($state['_wpmgr_byte_state'] ?? null) !== 1) { return $state; }
        if (!isset($state['data']) || !is_array($state['data'])) { return null; }
        try {
            $decoded = self::mapStateBytes($state['data'], false);
            foreach ($state as $key => $value) {
                if ($key !== '_wpmgr_byte_state' && $key !== 'data') { $decoded[$key] = $value; }
            }
            return $decoded;
        }
        catch (\UnexpectedValueException $_) { return null; }
    }

    /** @return array<mixed> Reversible base64 strings, including array keys. */
    private static function mapStateBytes(array $state, bool $encode): array
    {
        $result = [];
        foreach ($state as $key => $value) {
            if (is_string($key)) {
                if (!$encode && !str_starts_with($key, 's:')) { throw new \UnexpectedValueException('Invalid byte-state key'); }
                $key = $encode ? 's:' . base64_encode($key) : base64_decode(substr($key, 2), true);
                if ($key === false) { throw new \UnexpectedValueException('Invalid byte-state key'); }
            }
            if (is_array($value)) { $value = self::mapStateBytes($value, $encode); }
            elseif (is_string($value)) {
                $value = $encode ? base64_encode($value) : base64_decode($value, true);
                if ($value === false) { throw new \UnexpectedValueException('Invalid byte-state value'); }
            }
            $result[$key] = $value;
        }
        return $result;
    }

    /**
     * Load the task row by snapshot_id. Returns null if the row doesn't exist.
     *
     * @return array{phase:string,kind:string,sub_state:array<string,mixed>,resume_count:int,max_resumes:int,last_progress_at:int}|null
     */
    private function loadTask(): ?array
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return null;
        }
        /** @var \wpdb $wpdb */
        $table = $this->tableName();
        if ($table === '') {
            return null;
        }

        // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- interpolated identifier is prefix+constant (trusted); value bound via %s placeholder
        $sql = "SELECT phase, kind, sub_state, resume_count, max_resumes, last_progress_at FROM {$table} WHERE snapshot_id = %s LIMIT 1";
        /** @phpstan-ignore-next-line — $wpdb is a runtime interface. */
        $prepared = $wpdb->prepare($sql, $this->snapshotId()); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- already prepared on the preceding line
        $row      = $wpdb->get_row($prepared, ARRAY_A); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching, WordPress.DB.PreparedSQL.NotPrepared, WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter, PluginCheck.Security.DirectDB.UnescapedDBParameter -- direct query on plugin-owned table; value is the output of $wpdb->prepare()

        if (!is_array($row)) {
            return null;
        }

        $sub = self::decodeSubStateColumn($row['sub_state'] ?? null);

        return [
            'phase'        => (string) ($row['phase'] ?? self::PHASE_QUEUED),
            'kind'         => (string) ($row['kind'] ?? $this->kind()),
            'sub_state'    => $sub,
            'resume_count' => (int) ($row['resume_count'] ?? 0),
            'max_resumes'  => (int) ($row['max_resumes'] ?? 6),
            'last_progress_at' => (int) ($row['last_progress_at'] ?? 0),
        ];
    }

    /**
     * Insert a task in the requested phase only when its snapshot has no row.
     *
     * @param string              $phase Initial phase; queued by default.
     * @param array<string,mixed> $subState Initial task state.
     * @return bool True only when this invocation inserted the row.
     */
    private function seedTask(string $phase = self::PHASE_QUEUED, array $subState = []): bool
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return false;
        }
        /** @var \wpdb $wpdb */
        $table = $this->tableName();
        if ($table === '') {
            return false;
        }
        $now = time();

        // Use REPLACE/INSERT IGNORE semantics via $wpdb->query() with a raw
        // INSERT IGNORE — $wpdb->insert() doesn't surface the IGNORE qualifier
        // and would throw on duplicate-key collisions.
        // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- interpolated identifier is prefix+constant (trusted); values bound via placeholders
        $sql = "INSERT IGNORE INTO {$table}
                (snapshot_id, kind, phase, sub_state, started_at, last_progress_at, resume_count, max_resumes)
                VALUES (%s, %s, %s, %s, %d, %d, %d, %d)";
        $prepared = $wpdb->prepare( // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- already prepared on this line via $wpdb->prepare()
            $sql, // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- identifier validated against information_schema / prefix+constant; values bound via placeholders
            $this->snapshotId(),
            $this->kind(),
            $phase,
            $subState === [] ? '{}' : self::encodeSubState($subState),
            $now,
            $now,
            0,
            6
        );
        /** @phpstan-ignore-next-line */
        return $wpdb->query($prepared) === 1; // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching, WordPress.DB.PreparedSQL.NotPrepared, WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter, PluginCheck.Security.DirectDB.UnescapedDBParameter -- direct INSERT IGNORE on plugin-owned table; value is the output of $wpdb->prepare()
    }

    /**
     * Threshold (bytes): encoded sub_state larger than this is spilled to a
     * scratch sidecar file instead of being stored inline in the DB column.
     * Chosen to be well under MySQL's default @@max_allowed_packet of 1 MiB
     * while allowing many thousands of tombstone entries stored inline even
     * before the sidecar triggers. 48 KiB is the proven value from the
     * 0.21.2 safety net.
     */
    private const SUBSTATE_SIDECAR_THRESHOLD = 48 * 1024; // 48 KiB

    /**
     * Filename used for the sub_state sidecar inside the scratch dir.
     * Written as `<scratch_dir>/task_substate.json` using atomic
     * tempfile + rename so a crash mid-write never leaves a half-written
     * cursor on disk.
     */
    private const SUBSTATE_SIDECAR_NAME = 'task_substate.json';

    /**
     * Marker key stored in the DB sub_state column when the full cursor has
     * been spilled to the sidecar file. loadTask() reads this key and
     * rehydrates from disk.
     */
    private const SUBSTATE_SIDECAR_KEY = '_sidecar';

    /**
     * Persist phase + sub_state + bump last_progress_at. Called on every
     * phase boundary AND immediately before each long-running subprocess
     * (DbDumper / FilesArchiver / EncryptAndUpload) so the watchdog has a
     * recent timestamp even mid-phase.
     *
     * Sidecar-spill: when the JSON-encoded sub_state exceeds
     * SUBSTATE_SIDECAR_THRESHOLD bytes, the FULL cursor is written to
     * `<scratch_dir>/task_substate.json` (atomic temp+rename) and the DB
     * column holds only a small pointer
     * `{"_sidecar":true,"file":"<absolute-path>"}` plus the phase and
     * last_progress_at. loadTask() detects the pointer and rehydrates.
     * cleanupOnCompleted() unlinks the sidecar.
     *
     * This ensures $wpdb->update() can never fail silently due to
     * @@max_allowed_packet overflow regardless of how many tombstones or
     * scan-cursor entries sub_state carries.
     *
     * Throw-on-false: when $wpdb->update() returns false (not false-because-
     * no-change, which returns 0) the caller is alerted so the watchdog
     * retries instead of silently completing with a stale cursor. The check
     * is done AFTER the sidecar write to avoid a false-positive on the
     * pointer row (the pointer is tiny and should never fail).
     *
     * Filename bytes in resume fields use a reversible JSON envelope when
     * necessary. Display samples and diagnostics use UTF-8 replacement.
     * A checkpoint is acknowledged only after the database accepts it.
     *
     * @param string              $phase    New phase.
     * @param array<string,mixed> $subState New sub_state to persist.
     * @return bool True when the database accepts the checkpoint; false when
     *              task storage is unavailable. Callers requiring durability
     *              must reject a false result.
     * @throws \RuntimeException On encoding or database-write failure.
     *
     * `protected` is a deliberate test seam: the ADR-051 prevMap-pipeline e2e
     * overrides this to a no-op so runArchivingFiles can be driven without a
     * live $wpdb.
     */
    protected function saveTaskState(string $phase, array $subState): bool
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return false;
        }
        /** @var \wpdb $wpdb */
        $table = $this->tableName();
        if ($table === '') {
            return false;
        }

        $now     = time();
        if (isset($subState['files']['skipped_sources'])) {
            $subState['files']['skipped_sources'] = array_map(static fn ($path): string => json_decode(json_encode((string) $path, JSON_INVALID_UTF8_SUBSTITUTE), true), $subState['files']['skipped_sources']);
        }
        if (isset($subState['last_error'])) {
            $subState['last_error'] = json_decode(json_encode($subState['last_error'], JSON_INVALID_UTF8_SUBSTITUTE), true);
        }
        // GH #983: the durable progress record. It advances only when this
        // state's mark is later than the last saved one, and this worker
        // keeps it, so a caller's older copy of sub_state never rolls it back.
        $mark   = self::progressMark($phase, $subState);
        $record = $this->progress ?? self::progressRecord($subState['progress'] ?? null);
        if ($record === null) {
            $record = ['mark' => $mark, 'at' => $now, 'seq' => 0];
        } elseif (self::markAdvanced($mark, $record['mark'])) {
            $record = ['mark' => $mark, 'at' => $now, 'seq' => $record['seq'] + 1];
        }
        $subState['progress'] = $record;
        $encoded = self::encodeSubState($subState);

        $this->lastDbUpdate = $now;

        // ---- Sidecar-spill: keep the DB column small. ----
        // If the encoded cursor exceeds the threshold, write it to the scratch
        // sidecar and store only a pointer in the DB. This prevents
        // @@max_allowed_packet silent-write failures on any sub_state size.
        $rowEncoded = $encoded;
        $scratch    = $this->scratchDir();
        if (strlen($encoded) > self::SUBSTATE_SIDECAR_THRESHOLD && $scratch !== '') {
            $sidecarPath = $scratch . DIRECTORY_SEPARATOR . self::SUBSTATE_SIDECAR_NAME;
            $tmpPath     = $sidecarPath . '.tmp.' . getmypid();
            $written     = @file_put_contents($tmpPath, $encoded);
            // A sidecar that was not renamed into place is not this state: the
            // pointer is written only after the rename succeeds.
            if ($written !== false && $written === strlen($encoded)
                && @rename($tmpPath, $sidecarPath)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-filesystem swap; WP_Filesystem::move() is copy+delete (non-atomic) and breaks crash/watchdog-resume safety
                // DB column holds only the pointer; the full cursor is on disk.
                $pointer    = self::encodeSubState([self::SUBSTATE_SIDECAR_KEY => true, 'file' => $sidecarPath]);
                $rowEncoded = $pointer !== false ? $pointer : $encoded;
            } else {
                wp_delete_file($tmpPath);
                // Sidecar write failed — fall through to inline write (may
                // hit the packet limit, but that will be caught by the
                // throw-on-false below).
            }
        }

        $result = $wpdb->update( // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- direct update on plugin-owned table; no core helper exists; correctness requires a live write
            $table,
            [
                'phase'            => $phase,
                'sub_state'        => $rowEncoded,
                'last_progress_at' => $now,
            ],
            ['snapshot_id' => $this->snapshotId()],
            ['%s', '%s', '%d'],
            ['%s']
        );

        // $wpdb->update() returns int rows-affected on success, false on
        // genuine failure (e.g. packet overflow, DB gone away). A 0 return
        // means the row existed but the values were unchanged — that is NOT
        // a failure. Throw on false so the watchdog retries rather than
        // silently completing with a stale cursor.
        if ($result === false) {
            throw new \RuntimeException(
                'TaskRunner: DB update failed for phase ' . esc_html($phase) . ' (possible @@max_allowed_packet overflow or connection loss)'
            );
        }

        // GH #983: 0 rows is also what MySQL reports for a row whose values
        // did not change, so only a row proven missing means the task is gone.
        // Nothing was saved then, and nothing this worker does can be resumed.
        if ($result === 0 && $this->taskRowMissing()) {
            throw new \RuntimeException(
                'TaskRunner: task row for phase ' . esc_html($phase) . ' is gone; the backup was given up',
                self::CODE_TASK_GONE // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- integer code, never output
            );
        }

        $this->progress = $record;
        return true;
    }

    /**
     * Whether this task's row is provably absent. An unknown answer (no
     * database, a failed query) is never proof: the worker keeps going.
     */
    private function taskRowMissing(): bool
    {
        global $wpdb;
        $table = $this->tableName();
        if (!is_object($wpdb) || $table === '') {
            return false;
        }
        /** @var \wpdb $wpdb */
        try {
            // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- interpolated identifier is prefix+constant (trusted); value bound via %s placeholder
            $sql      = "SELECT COUNT(*) FROM {$table} WHERE snapshot_id = %s";
            $prepared = $wpdb->prepare($sql, $this->snapshotId()); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- already prepared on this line
            $count    = $wpdb->get_var($prepared); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching, WordPress.DB.PreparedSQL.NotPrepared, WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter, PluginCheck.Security.DirectDB.UnescapedDBParameter -- live existence check on the plugin-owned table; value is the output of $wpdb->prepare()
        } catch (\Throwable $_) {
            return false;
        }
        return $count !== null && (string) $count === '0';
    }

    /**
     * GH #983: the durable progress mark of a task in $phase with $subState:
     * the phase, then the closed archive parts, the archive's first
     * unfinished line, the encrypted manifest entries and the uploaded
     * chunks. Within one pass a later checkpoint never has a lower mark.
     *
     * @param array<string,mixed> $subState Task state.
     * @return list<int>
     */
    public static function progressMark(string $phase, array $subState): array
    {
        $files  = isset($subState['files']) && is_array($subState['files']) ? $subState['files'] : [];
        $parts  = $files['parts_completed'] ?? ($files['parts'] ?? []);
        $line   = $files['file_index'] ?? ($files['files_total'] ?? 0);
        $encrypt = isset($subState['encrypt']) && is_array($subState['encrypt']) ? $subState['encrypt'] : [];
        $upload  = isset($subState['upload']) && is_array($subState['upload']) ? $subState['upload'] : [];

        return [
            self::PHASE_RANK[$phase] ?? 0,
            is_array($parts) ? count($parts) : 0,
            is_int($line) ? max(0, $line) : 0,
            isset($encrypt['entries']) && is_array($encrypt['entries']) ? count($encrypt['entries']) : 0,
            isset($upload['uploaded_hashes']) && is_array($upload['uploaded_hashes']) ? count($upload['uploaded_hashes']) : 0,
        ];
    }

    /**
     * Whether $mark is later than $than, compared field by field in order.
     *
     * @param list<int> $mark Mark to test.
     * @param list<int> $than Mark it must exceed.
     */
    public static function markAdvanced(array $mark, array $than): bool
    {
        foreach (array_values($mark) as $i => $value) {
            $other = (int) ($than[$i] ?? 0);
            if ((int) $value !== $other) {
                return (int) $value > $other;
            }
        }
        return false;
    }

    /**
     * The validated durable progress record from task state, or null. The
     * task row is input like any other: every field is checked.
     *
     * @param mixed $raw sub_state.progress.
     * @return array{mark:list<int>,at:int,seq:int}|null
     */
    public static function progressRecord($raw): ?array
    {
        if (!is_array($raw) || !isset($raw['mark'], $raw['at'], $raw['seq']) || !is_array($raw['mark'])
            || !is_int($raw['at']) || !is_int($raw['seq']) || $raw['seq'] < 0) {
            return null;
        }
        $mark = [];
        foreach ($raw['mark'] as $value) {
            if (!is_int($value)) {
                return null;
            }
            $mark[] = $value;
        }
        return ['mark' => $mark, 'at' => $raw['at'], 'seq' => $raw['seq']];
    }

    /**
     * The validated record of where the last worker started, or null.
     *
     * @param mixed $raw sub_state.worker.
     * @return array{seq:int,started_at:int}|null
     */
    public static function workerRecord($raw): ?array
    {
        if (!is_array($raw) || !isset($raw['seq'], $raw['started_at']) || !is_int($raw['seq']) || !is_int($raw['started_at'])) {
            return null;
        }
        return ['seq' => $raw['seq'], 'started_at' => $raw['started_at']];
    }

    /**
     * Report a failure to the control plane through the same signed
     * `/progress` channel run() uses, so the dashboard shows $message.
     * Never throws. Used by the watchdog when it gives a task up.
     */
    public function reportFailure(string $stage, string $message): void
    {
        $this->postProgress(self::PHASE_FAILED, ['stage' => $stage, 'message' => substr($message, 0, 240)]);
    }

    /**
     * Whether a live process holds this snapshot's run-lock right now.
     * Never creates a lock file: a missing one is not held.
     */
    public function isRunLockHeld(): bool
    {
        $token = $this->getLock();
        try {
            if ($token === '0') {
                return true;
            }
            $path = $this->fileLockPath();
            if ($path === '' || !is_file($path)) {
                return false;
            }
            $file = self::acquireFileLockAtPath($path, false);
            if ($file['handle'] !== null) {
                $this->releaseFileLock($file['handle']);
            }
            return $file['contended'];
        } catch (\Throwable $_) {
            return false;
        } finally {
            if ($token === '1') {
                $this->releaseLock();
            }
        }
    }

    /** Start this worker's clock against the time limit run() just set. */
    private function startWorkerClock(): void
    {
        $this->workerWallStart = microtime(true);
        // PHP counts max_execution_time in CPU time everywhere but Windows.
        $this->workerCpuStart = PHP_OS_FAMILY === 'Windows' ? null : self::cpuSeconds();
        if ($this->timeLimit === null) {
            $this->timeLimit = (float) max(0, (int) ini_get('max_execution_time'));
        }
    }

    /** CPU seconds this process has used, or null when unavailable. */
    private static function cpuSeconds(): ?float
    {
        if (!function_exists('getrusage')) {
            return null;
        }
        $usage = @getrusage();
        if (!is_array($usage) || !isset($usage['ru_utime.tv_sec'], $usage['ru_utime.tv_usec'], $usage['ru_stime.tv_sec'], $usage['ru_stime.tv_usec'])) {
            return null;
        }
        return (float) $usage['ru_utime.tv_sec'] + $usage['ru_utime.tv_usec'] / 1e6
            + (float) $usage['ru_stime.tv_sec'] + $usage['ru_stime.tv_usec'] / 1e6;
    }

    /**
     * Whether this worker should hand over now: it saved progress of its
     * own and used at least HANDOVER_SHARE of its time limit, in CPU time
     * where the limit counts CPU and in wall time otherwise. A worker with
     * no time limit never hands over.
     */
    private function shouldHandOver(): bool
    {
        if ($this->timeLimit === null || $this->timeLimit <= 0.0 || $this->progress === null || $this->progress['seq'] <= $this->workerStartSeq) {
            return false;
        }
        $cpu  = $this->workerCpuStart !== null ? self::cpuSeconds() : null;
        $used = $cpu !== null ? $cpu - (float) $this->workerCpuStart : microtime(true) - $this->workerWallStart;
        return $used >= self::HANDOVER_SHARE * $this->timeLimit;
    }

    /**
     * Touch last_progress_at without rewriting sub_state. Used inside the
     * per-chunk progress callbacks for cheap watchdog liveness signaling.
     * Throttled to once per PROGRESS_DB_THROTTLE_SECONDS so a chatty callback
     * doesn't pound the DB.
     */
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
        /** @var \wpdb $wpdb */
        $table = $this->tableName();
        if ($table === '') {
            return;
        }
        $this->lastDbUpdate = $now;

        $wpdb->update( // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- direct update on plugin-owned table; throttled liveness ping; no core helper exists
            $table,
            ['last_progress_at' => $now],
            ['snapshot_id' => $this->snapshotId()],
            ['%d'],
            ['%s']
        );
    }

    /**
     * Per-chunk progress callback. Touches last_progress_at (throttled) and
     * fire-and-forgets a `/progress` POST to the CP.
     *
     * @param string              $phase  Phase label.
     * @param array<string,mixed> $detail Phase telemetry.
     */
    private function onPhaseProgress(string $phase, array $detail): void
    {
        $this->retainSkippedSources($detail);
        $this->touchProgressTimestamp();
        $this->postProgress($phase, $detail);
    }

    /**
     * Fire-and-forget signed POST to /progress. Failures are swallowed.
     *
     * @param string              $phase  Phase label (must be in the closed set).
     * @param array<string,mixed> $detail Phase telemetry.
     */
    private function postProgress(string $phase, array $detail): void
    {
        if ($this->progressClient === null) {
            return;
        }
        if ($this->skippedSourcesCount > 0) {
            $detail['skipped_sources_count'] = $this->skippedSourcesCount;
            $detail['skipped_sources'] = $this->skippedSources;
        }
        // The control plane keeps only the latest report, so every report
        // carries timing; worker_started_at names the worker that sent it.
        if ($this->workerStartedAt !== null && isset($this->params['timing']) && is_array($this->params['timing'])) {
            $timing = $this->params['timing'];
            $detail['timing'] = [
                'received_at'       => (int) ($timing['received_at'] ?? 0),
                'preflight_done_at' => (int) ($timing['preflight_done_at'] ?? 0),
                'claimed_at'        => (int) ($timing['claimed_at'] ?? 0),
                'worker_started_at' => $this->workerStartedAt,
            ];
        }
        try {
            $this->progressClient->post($phase, $detail);
        } catch (\Throwable $_) {
            // Swallow — progress is observability, not correctness.
        }
    }

    /** @param array<string,mixed> $detail Archive state or progress detail. */
    private function retainSkippedSources(array $detail): void
    {
        if (isset($detail['skipped_sources_count'])) {
            $this->skippedSourcesCount = max(0, (int) $detail['skipped_sources_count']);
            $this->skippedSources = array_slice(array_values(array_filter((array) ($detail['skipped_sources'] ?? []), 'is_string')), 0, 20);
        }
    }

    // ==================================================================
    // Cleanup
    // ==================================================================

    /**
     * Best-effort cleanup of the scratch dir + dedup row. Called on COMPLETED.
     * Failures are swallowed: a leaked scratch file is a disk-space concern,
     * not a correctness concern, and a swept WP cron will eventually nuke the
     * per-run dir.
     */
    private function cleanupOnCompleted(): void
    {
        $scratch = $this->scratchDir();

        // 1. Chunk files (most should already be unlinked by uploadChunks
        //    after PUT success or dedup hit; defensive sweep).
        $chunks = @glob($scratch . DIRECTORY_SEPARATOR . 'chunks-*.age');
        if (is_array($chunks)) {
            foreach ($chunks as $f) {
                wp_delete_file($f);
            }
        }
        $plainChunks = @glob($scratch . DIRECTORY_SEPARATOR . 'chunks-*.bin');
        if (is_array($plainChunks)) {
            foreach ($plainChunks as $f) {
                wp_delete_file($f);
            }
        }
        // ADR-051: remove the prev_files.list scratch file assembled from
        // PrevFilesListChunks during the archiving phase.
        $prevList = $scratch . DIRECTORY_SEPARATOR . 'prev_files.list';
        if (is_file($prevList)) {
            wp_delete_file($prevList);
        }
        // Sidecar sub_state file (written when sub_state exceeded the
        // SUBSTATE_SIDECAR_THRESHOLD in saveTaskState). Must be unlinked
        // after completion so no stale cursor survives to a future run.
        $sidecar = $scratch . DIRECTORY_SEPARATOR . self::SUBSTATE_SIDECAR_NAME;
        if (is_file($sidecar)) {
            wp_delete_file($sidecar);
        }

        // 2. Artifact files (DB dump + zip parts + files.list + tombstones.list).
        $patterns = [
            $scratch . DIRECTORY_SEPARATOR . 'database.sql.gz',
            $scratch . DIRECTORY_SEPARATOR . 'paths.cache',
            $scratch . DIRECTORY_SEPARATOR . FilesArchiver::FILES_LIST_NAME,
            $scratch . DIRECTORY_SEPARATOR . FilesArchiver::TOMBSTONES_LIST_NAME,
            $scratch . DIRECTORY_SEPARATOR . FilesArchiver::DISCOVERY_ID_NAME,
        ];
        foreach ($patterns as $p) {
            if (is_file($p)) {
                wp_delete_file($p);
            }
        }
        // Track A / A2 (#187): include 'core' in the component sweep so
        // `core.gNNN.partMMM.zip` files emitted by CoreFilesArchiver are removed.
        foreach (['plugins', 'themes', 'uploads', 'wp-content', 'core'] as $comp) {
            // Match BOTH the generation-namespaced `<comp>.gNNN.partMMM.zip`
            // and the legacy `<comp>.partMMM.zip` part filenames.
            // Also the `<part>.zip.XXXXXX` output libzip leaves when a worker ends mid-close.
            // glob() returns false on error, which must never reach wp_delete_file().
            $zips = array_merge(@glob($scratch . DIRECTORY_SEPARATOR . $comp . '.*part*.zip') ?: [], @glob($scratch . DIRECTORY_SEPARATOR . $comp . '.*part*.zip.*') ?: []);
            if (is_array($zips)) {
                foreach ($zips as $f) {
                    wp_delete_file($f);
                }
            }
        }
        // Also clean up the on-disk core-paths.cache written by CoreFilesArchiver.
        $coreCachePath = $scratch . DIRECTORY_SEPARATOR . 'core-paths.cache';
        if (is_file($coreCachePath)) {
            wp_delete_file($coreCachePath);
        }

        // 3. The scratch dir itself (rmdir refuses if not empty — that's fine).
        @rmdir($scratch); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- removes an empty server-derived scratch dir; WP_Filesystem not initialized

        // 4. The legacy `wpmgr_backup_runs` dedup row so a future backup of
        //    the same snapshot id can spawn. Best-effort: missing table or
        //    missing row are both acceptable.
        global $wpdb;
        if (is_object($wpdb)) {
            $runsTable = $this->prefix() . Schema::BACKUP_RUNS_TABLE;
            /** @phpstan-ignore-next-line */
            $wpdb->delete($runsTable, ['snapshot_id' => $this->snapshotId()], ['%s']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- direct delete on plugin-owned table; no core helper exists
        }

        // 5. DELETE the wpmgr_backup_tasks row (Bug 2 fix). Earlier design
        //    kept it at phase=completed for post-hoc debugging, but a kept
        //    row + a delayed wpmgr_backup_watchdog cron event = phantom
        //    re-entry into encrypting_uploading + a presignChunks 422 that
        //    surfaces to the UI as a misleading "encrypting_uploading
        //    failed" event during a subsequent restore. The defensive
        //    watchdog also DELETEs on the next tick if it sees a terminal
        //    row — this is the primary cleanup; the watchdog is belt &
        //    suspenders. Post-hoc debugging now relies on CP-side audit
        //    + the live-progress event log (CP DB), not the agent row.
        if (is_object($wpdb)) {
            $tasksTable = $this->prefix() . Schema::BACKUP_TASKS_TABLE;
            /** @phpstan-ignore-next-line */
            @$wpdb->delete($tasksTable, ['snapshot_id' => $this->snapshotId()], ['%s']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- direct delete on plugin-owned table; no core helper exists
        }
    }

    /**
     * GH #279: best-effort GC of the scratch dir + chunk/artifact files on a
     * genuinely terminal failure (a hard CP rejection past the watchdog's
     * hard-stall deadline, an operator cancel, or any other uncaught
     * exception the top-level catch in run() reduces to `failed`). Mirrors
     * cleanupOnCompleted()'s file-level sweep (chunks, artifacts, the
     * scratch dir itself) but intentionally does NOT touch the
     * wpmgr_backup_tasks / wpmgr_backup_runs rows: the caller already
     * DELETEs the task row right after this runs, and the legacy runs dedup
     * row expires on its own window (BackupCommand::DEDUP_WINDOW_SECONDS) or
     * is released explicitly by the command that owns it.
     *
     * Before this fix, only a genuinely COMPLETED run ever reached
     * cleanupOnCompleted(); a terminally-failed run left its scratch dir +
     * chunks-*.age/.bin files on disk until the age-gated
     * BackupJanitor::gcRuns() backstop eventually swept them. Reconciling
     * here means a terminal rejection frees the disk space (and, for the
     * "local" destination, any partially-written local chunks) immediately
     * instead of waiting on the backstop's age gate.
     *
     * Called from inside the top-level catch in run(); failures here are
     * swallowed by the caller so a second exception never masks the
     * original failure. ALSO called via the public reclaimScratch()
     * wrapper below (GH #256) by callers outside run() entirely; see that
     * method's doc.
     */
    private function cleanupOnFailed(): void
    {
        $scratch = $this->scratchDir();
        if ($scratch === '' || !is_dir($scratch)) {
            return;
        }

        // 1. Chunk files (encrypted + plaintext).
        $chunks = @glob($scratch . DIRECTORY_SEPARATOR . 'chunks-*.age');
        if (is_array($chunks)) {
            foreach ($chunks as $f) {
                wp_delete_file($f);
            }
        }
        $plainChunks = @glob($scratch . DIRECTORY_SEPARATOR . 'chunks-*.bin');
        if (is_array($plainChunks)) {
            foreach ($plainChunks as $f) {
                wp_delete_file($f);
            }
        }
        // ADR-051: remove the prev_files.list scratch file assembled from
        // PrevFilesListChunks during the archiving phase.
        $prevList = $scratch . DIRECTORY_SEPARATOR . 'prev_files.list';
        if (is_file($prevList)) {
            wp_delete_file($prevList);
        }
        // Sidecar sub_state file, if the cursor ever spilled to disk.
        $sidecar = $scratch . DIRECTORY_SEPARATOR . self::SUBSTATE_SIDECAR_NAME;
        if (is_file($sidecar)) {
            wp_delete_file($sidecar);
        }

        // 2. Artifact files (DB dump + zip parts + files.list + tombstones.list).
        $patterns = [
            $scratch . DIRECTORY_SEPARATOR . 'database.sql.gz',
            $scratch . DIRECTORY_SEPARATOR . 'paths.cache',
            $scratch . DIRECTORY_SEPARATOR . FilesArchiver::FILES_LIST_NAME,
            $scratch . DIRECTORY_SEPARATOR . FilesArchiver::TOMBSTONES_LIST_NAME,
            $scratch . DIRECTORY_SEPARATOR . FilesArchiver::DISCOVERY_ID_NAME,
        ];
        foreach ($patterns as $p) {
            if (is_file($p)) {
                wp_delete_file($p);
            }
        }
        foreach (['plugins', 'themes', 'uploads', 'wp-content', 'core'] as $comp) {
            // Match BOTH the generation-namespaced `<comp>.gNNN.partMMM.zip`
            // and the legacy `<comp>.partMMM.zip` part filenames.
            // Also the `<part>.zip.XXXXXX` output libzip leaves when a worker ends mid-close.
            // glob() returns false on error, which must never reach wp_delete_file().
            $zips = array_merge(@glob($scratch . DIRECTORY_SEPARATOR . $comp . '.*part*.zip') ?: [], @glob($scratch . DIRECTORY_SEPARATOR . $comp . '.*part*.zip.*') ?: []);
            if (is_array($zips)) {
                foreach ($zips as $f) {
                    wp_delete_file($f);
                }
            }
        }
        // Also clean up the on-disk core-paths.cache written by CoreFilesArchiver.
        $coreCachePath = $scratch . DIRECTORY_SEPARATOR . 'core-paths.cache';
        if (is_file($coreCachePath)) {
            wp_delete_file($coreCachePath);
        }

        // 3. The scratch dir itself (rmdir refuses if not empty -- that's
        // fine; BackupJanitor::gcRuns() is still the age-gated backstop for
        // any artifact this best-effort sweep missed).
        @rmdir($scratch); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- removes an empty server-derived scratch dir; WP_Filesystem not initialized
    }

    /**
     * GH #256: public wrapper around cleanupOnFailed() so a caller OUTSIDE
     * this instance's own run() (specifically, Watchdog's terminal give-up
     * paths: the 7200s hard-ceiling guard, the late-phase stale guard, the
     * max-resumes exhaustion, and dispatch()'s stale-task guard, every path
     * that discards a task row without ever entering run()'s own try/catch)
     * can reclaim this run's scratch directory through the EXACT same,
     * already-tested file-level sweep run() itself uses on a caught failure,
     * instead of a second, independently-duplicated implementation.
     *
     * Constructing a TaskRunner purely to call this has no I/O side effect
     * of its own. The constructor builds an OPTIONAL ProgressClient when
     * `progress_endpoint` is a non-empty string in $params; the caller here
     * (Watchdog::reclaimRunScratch()) reconstructs `$params` from
     * sub_state.params, the same array BackupCommand originally seeded, so
     * `progress_endpoint` is normally PRESENT and a ProgressClient IS built.
     * That construction is still side-effect free: ProgressClient's own
     * constructor only assigns properties, and this method never calls
     * postProgress()/the client's post() method, touches $wpdb, or touches
     * the task row; it is a pure filesystem sweep keyed off
     * $params['scratch_dir']. The caller (Watchdog::reclaimRunScratch(), and
     * BackupJanitor::gcRuns() for its own run-directory sweep) invokes this
     * method from INSIDE the callback it hands to withRunLock() on the SAME
     * constructed instance, never a separate probe-then-call, so this
     * runs only while both run-lock guards are held for the caller, closing
     * the TOCTOU window a standalone probe would otherwise leave open.
     *
     * Never touches the wpmgr_backup_tasks / wpmgr_backup_runs rows (same
     * contract as cleanupOnFailed()). The caller owns discarding the row
     * and must read whatever it needs from it BEFORE calling this, since
     * this reads only $this->params, never the row.
     *
     * @return void
     */
    public function reclaimScratch(): void
    {
        $this->cleanupOnFailed();
    }

    // ==================================================================
    // Helpers
    // ==================================================================

    /**
     * Ensure the per-run scratch dir exists, mkdir -p semantics.
     */
    private function ensureScratchDir(): void
    {
        $dir = $this->scratchDir();
        if ($dir === '') {
            throw new \RuntimeException('TaskRunner: scratch_dir is empty');
        }
        if (!is_dir($dir) && !wp_mkdir_p($dir) && !is_dir($dir)) {
            throw new \RuntimeException('TaskRunner: cannot create scratch dir: ' . esc_html($dir));
        }
    }

    /** Snapshot id from params (always a non-empty string by construction). */
    private function snapshotId(): string
    {
        return (string) ($this->params['snapshot_id'] ?? '');
    }

    /** Snapshot kind from params; one of {files, db, full}. */
    private function kind(): string
    {
        return (string) ($this->params['kind'] ?? '');
    }

    /** Scratch dir for this snapshot's artifacts + chunk files. */
    private function scratchDir(): string
    {
        return (string) ($this->params['scratch_dir'] ?? '');
    }

    /** WP-content root the FilesArchiver walks. */
    private function wpContentPath(): string
    {
        return (string) ($this->params['wp_content_path'] ?? '');
    }

    /**
     * DB credentials handed to DbDumper. Same shape as DbDumper's contract.
     *
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

    /** Fully-qualified wpmgr_backup_tasks table name, or '' if no $wpdb. */
    private function tableName(): string
    {
        $p = $this->prefix();
        if ($p === '') {
            return '';
        }
        return $p . Schema::BACKUP_TASKS_TABLE;
    }

    /** Current $wpdb prefix, or '' if not in a WP runtime. */
    private function prefix(): string
    {
        global $wpdb;
        if (is_object($wpdb) && isset($wpdb->prefix) && is_string($wpdb->prefix)) {
            return $wpdb->prefix;
        }
        return '';
    }
}
