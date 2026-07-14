<?php
/**
 * TaskRunner: M5.6 / ADR-033 — the state-machine driver that turns a row in
 * `wpmgr_backup_tasks` into a completed backup snapshot.
 *
 * This is the only "active" component in the state-machine pipeline: the
 * REST handler (Phase D) writes the task row, calls fastcgi_finish_request(),
 * then invokes TaskRunner::run() in the same PHP process. The watchdog hook
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
 *   - Re-entries are gated by `resume_count < max_resumes` (default cap 6).
 *     The cap is enforced by the watchdog handler in Phase D before it calls
 *     us; we don't touch resume_count here. But we DO update last_progress_at
 *     on every meaningful boundary so the watchdog can tell live work apart
 *     from a stalled handler.
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

    /**
     * @var array{snapshot_id:string,kind:string,age_recipient:string,presign_endpoint:string,
     *            manifest_endpoint:string,progress_endpoint:string,chunk_bytes:int,
     *            scratch_dir:string,wp_content_path:string,db:array<string,string>,
     *            is_incremental?:bool,parent_snapshot_id?:string,base_snapshot_id?:string,
     *            generation?:int,prev_files_list_chunks?:list<array<string,string>>}
     */
    private array $params;

    /** Unix-seconds of the last DB write to last_progress_at (throttle). */
    private int $lastDbUpdate = 0;

    private ?ProgressClient $progressClient = null;

    /**
     * @param array{snapshot_id:string,kind:string,age_recipient:string,
     *              presign_endpoint:string,manifest_endpoint:string,
     *              progress_endpoint:string,chunk_bytes:int,scratch_dir:string,
     *              wp_content_path:string,db:array<string,string>} $params
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
     * @return string Terminal phase reached this invocation. One of
     *                PHASE_COMPLETED, PHASE_FAILED, or — if a future phase
     *                handler yields mid-run on a soft cap — the in-progress
     *                phase. Today every handler runs the phase to completion
     *                or throws, so this returns COMPLETED or FAILED.
     */
    public function run(): string
    {
        @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged, PHPCompatibility.FunctionUse.ArgumentFunctionsReportCurrentValue.NeedsInspection, Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running backup loop must not hit max_execution_time; @-guarded, no-op when disabled
        @ignore_user_abort(true);

        $currentPhase = self::PHASE_QUEUED;

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
            $currentPhase = (string) $task['phase'];
            $subState     = (array) $task['sub_state'];

            // Terminal? Re-entry is a no-op.
            if ($currentPhase === self::PHASE_COMPLETED || $currentPhase === self::PHASE_FAILED) {
                return $currentPhase;
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
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_DUMPING_DB:
                        $subState = $this->runDumpingDb($subState);
                        $next     = $this->nextAfterDumpingDb();
                        $this->saveTaskState($next, $subState);
                        $currentPhase = $next;
                        break;

                    case self::PHASE_ARCHIVING_FILES:
                        $subState = $this->runArchivingFiles($subState);
                        $this->saveTaskState(self::PHASE_ENCRYPTING_UPLOADING, $subState);
                        $currentPhase = self::PHASE_ENCRYPTING_UPLOADING;
                        break;

                    case self::PHASE_ENCRYPTING_UPLOADING:
                        $subState = $this->runEncryptingUploading($subState);
                        $this->saveTaskState(self::PHASE_SUBMITTING_MANIFEST, $subState);
                        $currentPhase = self::PHASE_SUBMITTING_MANIFEST;
                        break;

                    case self::PHASE_SUBMITTING_MANIFEST:
                        $this->runSubmittingManifest($subState);
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

            return self::PHASE_COMPLETED;
        } catch (\Throwable $e) {
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
                $this->postProgress(self::PHASE_FAILED, [
                    'stage'   => $currentPhase,
                    'message' => substr($e->getMessage(), 0, 240),
                ]);
            } catch (\Throwable $_) {
                // Swallow.
            }

            // Bug 2 fix: also DELETE the failed row so a delayed watchdog
            // cron event can't re-enter and re-emit a stale phantom
            // /presign call. The CP-side audit + the just-posted `failed`
            // progress event have already recorded the failure for ops.
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
        }, $prevMap);

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
     * Load the task row by snapshot_id. Returns null if the row doesn't exist.
     *
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

        // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- interpolated identifier is prefix+constant (trusted); value bound via %s placeholder
        $sql = "SELECT phase, kind, sub_state, resume_count, max_resumes FROM {$table} WHERE snapshot_id = %s LIMIT 1";
        /** @phpstan-ignore-next-line — $wpdb is a runtime interface. */
        $prepared = $wpdb->prepare($sql, $this->snapshotId()); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- already prepared on the preceding line
        /** @phpstan-ignore-next-line */
        $row      = $wpdb->get_row($prepared, ARRAY_A); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching, WordPress.DB.PreparedSQL.NotPrepared, WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter, PluginCheck.Security.DirectDB.UnescapedDBParameter -- direct query on plugin-owned table; value is the output of $wpdb->prepare()

        if (!is_array($row)) {
            return null;
        }

        $sub = [];
        if (isset($row['sub_state']) && is_string($row['sub_state']) && $row['sub_state'] !== '') {
            $decoded = json_decode($row['sub_state'], true);
            if (is_array($decoded)) {
                // Sidecar rehydration: when the DB column holds a pointer
                // (written by saveTaskState when sub_state exceeded the
                // SUBSTATE_SIDECAR_THRESHOLD), read the full cursor from disk.
                if (!empty($decoded[self::SUBSTATE_SIDECAR_KEY]) && isset($decoded['file']) && is_string($decoded['file'])) {
                    $sidecarPath = (string) $decoded['file'];
                    if (is_file($sidecarPath)) {
                        $raw = @file_get_contents($sidecarPath);
                        if ($raw !== false && $raw !== '') {
                            $full = json_decode($raw, true);
                            if (is_array($full)) {
                                $decoded = $full;
                            }
                        }
                    }
                }
                $sub = $decoded;
            }
        }

        return [
            'phase'        => (string) ($row['phase'] ?? self::PHASE_QUEUED),
            'kind'         => (string) ($row['kind'] ?? $this->kind()),
            'sub_state'    => $sub,
            'resume_count' => (int) ($row['resume_count'] ?? 0),
            'max_resumes'  => (int) ($row['max_resumes'] ?? 6),
        ];
    }

    /**
     * Insert a fresh task row in `queued` state. Caller-safe under retry: if
     * the row already exists (snapshot_id PK) the INSERT is a no-op.
     */
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

        // Use REPLACE/INSERT IGNORE semantics via $wpdb->query() with a raw
        // INSERT IGNORE — $wpdb->insert() doesn't surface the IGNORE qualifier
        // and would throw on duplicate-key collisions.
        // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- interpolated identifier is prefix+constant (trusted); values bound via placeholders
        $sql = "INSERT IGNORE INTO {$table}
                (snapshot_id, kind, phase, sub_state, started_at, last_progress_at, resume_count, max_resumes)
                VALUES (%s, %s, %s, %s, %d, %d, %d, %d)";
        /** @phpstan-ignore-next-line */
        $prepared = $wpdb->prepare( // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- already prepared on this line via $wpdb->prepare()
            $sql, // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared -- identifier validated against information_schema / prefix+constant; values bound via placeholders
            $this->snapshotId(),
            $this->kind(),
            self::PHASE_QUEUED,
            '{}',
            $now,
            $now,
            0,
            6
        );
        /** @phpstan-ignore-next-line */
        $wpdb->query($prepared); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching, WordPress.DB.PreparedSQL.NotPrepared, WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter, PluginCheck.Security.DirectDB.UnescapedDBParameter -- direct INSERT IGNORE on plugin-owned table; value is the output of $wpdb->prepare()
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
     * @param string              $phase    New phase.
     * @param array<string,mixed> $subState New sub_state to persist.
     * @throws \RuntimeException When the DB write fails AND we cannot fall
     *                           back to the sidecar.
     *
     * `protected` is a deliberate test seam: the ADR-051 prevMap-pipeline e2e
     * overrides this to a no-op so runArchivingFiles can be driven without a
     * live $wpdb.
     */
    protected function saveTaskState(string $phase, array $subState): void
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
        if ($encoded === false || $encoded === '') {
            // Last-resort: skip the write so the watchdog re-enters from the
            // last good state. json_encode should never fail for the small
            // part-cursor state we persist; if it does, logging is better
            // than wiping the prior cursor with '{}'.
            \WPMgr\Agent\Support\DebugLog::write('WPMgr TaskRunner: sub_state json_encode failed for phase ' . $phase . ' — skipping state write to preserve the prior cursor');
            return;
        }

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
            if ($written !== false && $written === strlen($encoded)) {
                @rename($tmpPath, $sidecarPath); // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic same-filesystem swap; WP_Filesystem::move() is copy+delete (non-atomic) and breaks crash/watchdog-resume safety
                // DB column holds only the pointer; the full cursor is on disk.
                $pointer    = json_encode([self::SUBSTATE_SIDECAR_KEY => true, 'file' => $sidecarPath]);
                $rowEncoded = $pointer !== false ? $pointer : $encoded;
            } else {
                wp_delete_file($tmpPath);
                // Sidecar write failed — fall through to inline write (may
                // hit the packet limit, but that will be caught by the
                // throw-on-false below).
            }
        }

        /** @phpstan-ignore-next-line */
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
        $table = $this->tableName();
        if ($table === '') {
            return;
        }
        $this->lastDbUpdate = $now;

        /** @phpstan-ignore-next-line */
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
        try {
            $this->progressClient->post($phase, $detail);
        } catch (\Throwable $_) {
            // Swallow — progress is observability, not correctness.
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
            $zips = @glob($scratch . DIRECTORY_SEPARATOR . $comp . '.*part*.zip');
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
