<?php
/**
 * Backup command: M5.6 / ADR-033 — cron-dispatched backup state machine.
 *
 * Replaces the ADR-032 phpbu+proc_open design (which failed on the
 * curvabykerline.in 1panel host because phpbu shells out to `mysqldump` and
 * `tar`, neither present in the WP container, and many hosts disable
 * `proc_open` regardless). The new flow:
 *
 *   1. Validate the JWT-signed request (Connector already did the crypto
 *      verification; we just sanity-check the body shape + recipient match).
 *   2. Atomically claim the snapshot_id in `wpmgr_backup_runs` (legacy dedup
 *      table — a winning REST request gets the slot; a duplicate is refused).
 *   3. Persist the TaskRunner params into `wpmgr_backup_tasks.sub_state.params`
 *      so the watchdog can rehydrate the runner on a stall recovery.
 *   4. Schedule the watchdog cron event for +120s.
 *   5. The SAPI-aware finish (fastcgi/litespeed/fallback — see
 *      ConnectionFinisher) to release the HTTP response to the CP in well
 *      under a second, regardless of whether the host runs PHP-FPM or
 *      OpenLiteSpeed (GH #274).
 *   6. Continue running under `ignore_user_abort(true)`: invoke
 *      `TaskRunner::run()` which drives the dumping_db → archiving_files →
 *      encrypting_uploading → submitting_manifest pipeline, persisting
 *      sub_state after every phase boundary.
 *
 * Contract (CP → agent), unchanged on the wire:
 *   POST /wp-json/wpmgr/v1/command/backup
 *   request:  { "snapshot_id", "kind" in {files|db|full}, "age_recipient",
 *               "chunk_bytes", "presign_endpoint", "manifest_endpoint",
 *               "progress_endpoint" }
 *   response: { "ok": bool, "detail": string }
 *
 * The agent's REPLY means "accepted & runner started" — NOT "completed".
 * Completion happens when the runner's `SubmitManifest` POST lands at the CP.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Backup\TaskRunner;
use WPMgr\Agent\Backup\Watchdog;
use WPMgr\Agent\Schema;
use WPMgr\Agent\Support\AgeIdentity;
use WPMgr\Agent\Support\ConnectionFinisher;
use WPMgr\Agent\Support\DebugLog;
use WPMgr\Agent\Support\LongRunningJob;

/**
 * Accepts a `backup` command from the CP, seeds the task row, and drives
 * the backup state machine. The HTTP response is released early via the
 * SAPI-aware ConnectionFinisher (fastcgi/litespeed/fallback) so the CP sees
 * the ACK in well under a second, on PHP-FPM or OpenLiteSpeed (GH #274),
 * while the real work continues under `ignore_user_abort(true)`.
 */
final class BackupCommand implements CommandInterface
{
    /** Valid snapshot kinds (mirror of the CP backup_contract.go Kind enum). */
    private const KINDS = ['files', 'db', 'full'];

    /** Default plaintext chunk size (matches CP agentcmd.ChunkBytes). */
    private const DEFAULT_CHUNK_BYTES = 4 << 20;

    /**
     * Dedup window: refuse to seed a second task for the same snapshot_id
     * within this many seconds of a previous claim. Long enough to cover
     * the CP retrying a lost-ACK; short enough that a crashed runner can
     * be re-claimed in reasonable time.
     */
    private const DEDUP_WINDOW_SECONDS = 300;

    private AgeIdentity $identity;

    private ConnectionFinisher $finisher;

    /**
     * @param AgeIdentity             $identity Backup-encryption identity/recipient checks.
     * @param ConnectionFinisher|null $finisher Injected for tests; defaults to a real ladder.
     */
    public function __construct(AgeIdentity $identity, ?ConnectionFinisher $finisher = null)
    {
        $this->identity = $identity;
        $this->finisher = $finisher ?? new ConnectionFinisher();
    }

    public function name(): string
    {
        return 'backup';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused — Connector verified them).
     * @param array<string,mixed> $params BackupRequest fields.
     * @return array{ok:bool,detail:string,code?:string}
     */
    public function execute(array $claims, array $params): array
    {
        $snapshotId = $this->str($params, 'snapshot_id');
        $kind       = $this->str($params, 'kind');
        $recipient  = $this->str($params, 'age_recipient');
        $presign    = $this->str($params, 'presign_endpoint');
        $manifestEp = $this->str($params, 'manifest_endpoint');
        $progressEp = $this->str($params, 'progress_endpoint');

        $chunkBytes = isset($params['chunk_bytes']) && is_numeric($params['chunk_bytes']) && (int) $params['chunk_bytes'] > 0
            ? (int) $params['chunk_bytes']
            : self::DEFAULT_CHUNK_BYTES;

        // --- ADR-051 incremental fields (absent/false on a full-base run) ----
        $isIncremental       = !empty($params['is_incremental']);
        $parentSnapshotId    = isset($params['parent_snapshot_id']) && is_string($params['parent_snapshot_id']) ? $params['parent_snapshot_id'] : '';
        $baseSnapshotId      = isset($params['base_snapshot_id']) && is_string($params['base_snapshot_id']) ? $params['base_snapshot_id'] : '';
        $generation          = isset($params['generation']) && is_numeric($params['generation']) ? (int) $params['generation'] : 0;
        // ADR-051: PrevFilesListChunks replaces the retired file_index_endpoint.
        // The CP presigns the parent's files.list chunks and sends them here so
        // the agent can build the change-detection map without a separate round-trip.
        $prevFilesListChunks = isset($params['prev_files_list_chunks']) && is_array($params['prev_files_list_chunks'])
            ? $params['prev_files_list_chunks']
            : [];

        // --- Track A (#187) — selective component backup + exclusions --------
        // All fields are optional (absent on pre-m49 CP versions); absent means
        // "use the agent default" so older CP versions are unaffected.
        //
        // components: which high-level components to archive. null/[] = all.
        // Values use the canonical singular entry_kind vocabulary:
        // plugin | theme | upload | wp-content | db | core.
        $components = isset($params['components']) && is_array($params['components'])
            ? array_values(array_filter($params['components'], 'is_string'))
            : [];
        // include_db: explicit DB-inclusion signal derived from the components
        // allowlist by the CP (backup_contract.go deriveIncludeDB). When
        // components is non-empty the CP sets this to &true (db is selected)
        // or &false (db not selected). Absent when components is empty (no
        // filter); the agent then follows the snapshot kind for DB inclusion.
        // We store null (absent/unset) vs true/false so TaskRunner can
        // distinguish "no filter" from "explicitly excluded".
        $includeDb = null;
        if (isset($params['include_db'])) {
            // json_decode produces bool directly for JSON true/false.
            $includeDb = (bool) $params['include_db'];
        }
        // include_core: when true, archive ABSPATH (wp-admin, wp-includes, root
        // PHP files) as an additional source emitting entry_kind="core".
        $includeCore = !empty($params['include_core']);
        // exclude_paths: additional path-segment names merged into FilesArchiver's
        // DEFAULT_EXCLUDES. Silently drop non-strings.
        $excludePaths = isset($params['exclude_paths']) && is_array($params['exclude_paths'])
            ? array_values(array_filter($params['exclude_paths'], 'is_string'))
            : [];
        // exclude_extensions: lowercase extension names (without dot, e.g. 'log').
        $excludeExtensions = isset($params['exclude_extensions']) && is_array($params['exclude_extensions'])
            ? array_values(array_filter($params['exclude_extensions'], 'is_string'))
            : [];
        // exclude_file_size_mb: skip files > N MiB; 0 = no filter.
        $excludeFileSizeMb = isset($params['exclude_file_size_mb']) && is_numeric($params['exclude_file_size_mb'])
            ? max(0, (int) $params['exclude_file_size_mb'])
            : 0;

        // --- 1. Input validation -------------------------------------------
        if ($snapshotId === '' || $presign === '' || $manifestEp === '') {
            return $this->refuse('missing snapshot or callback endpoints');
        }
        if (!preg_match('/^[a-f0-9-]{36}$/i', $snapshotId)) {
            return $this->refuse('invalid snapshot id');
        }
        if (!in_array($kind, self::KINDS, true)) {
            return $this->refuse('invalid kind');
        }
        if ($recipient === '') {
            return $this->refuse('missing age recipient');
        }
        if (!$this->identity->recipientMatches($recipient)) {
            return $this->refuse('age recipient mismatch');
        }

        // --- 1.5. Backup-readiness preflight (ADR-037 Sprint 1, 1B) -------
        //
        // Cheap defense-in-depth gate ahead of scratch creation + dedup claim.
        // Catches the "your disk is full" / "max_allowed_packet too small" /
        // "ZipArchive missing" / "DB unreachable" / "scratch base not writable"
        // failure modes BEFORE we burn a snapshot slot. Failures fail-fast
        // with reason_code=preflight_failed; warnings ride along with the
        // first progress event so the operator sees them in the UI without
        // blocking the run.
        $preflight = $this->preflightChecks();
        if (!$preflight['ok']) {
            $this->writePreflightFailure($snapshotId, $preflight['failures']);
            return $this->refuse('preflight_failed: ' . implode(', ', $preflight['failures']));
        }

        // --- 2. Dedup claim -----------------------------------------------
        // Belt-and-suspenders: ensure the schema is current (cheap when up to
        // date) before touching the dedup table; same pattern the autologin
        // command uses to self-heal a stale install.
        Schema::ensureCurrent();
        if (!$this->tryClaimDedup($snapshotId)) {
            return $this->refuse('runner already in flight for this snapshot', 'runner_in_flight');
        }

        // --- 3. Prepare scratch + assemble runner params ------------------
        try {
            $scratchDir = $this->prepareScratchDir($snapshotId);
        } catch (\Throwable $e) {
            $this->releaseDedup($snapshotId);
            return $this->refuse('scratch dir creation failed');
        }

        // ADR-036 P1 storage adapter: the CP threads `destination_kind` (one of
        // 'cp' | 'local' | 's3_compat') and an optional `destination_config`
        // through the BackupRequest so the runner can route chunks to the
        // right backend. When absent, the resolver defaults to 'cp' — the
        // existing 0.9.6 control-plane bucket path, bit-for-bit unchanged.
        $destinationKind   = isset($params['destination_kind']) && is_string($params['destination_kind'])
            ? $params['destination_kind']
            : 'cp';
        $destinationConfig = isset($params['destination_config']) && is_array($params['destination_config'])
            ? $params['destination_config']
            : [];

        $runnerParams = [
            'snapshot_id'        => $snapshotId,
            'kind'               => $kind,
            'age_recipient'      => $recipient,
            'presign_endpoint'   => $presign,
            'manifest_endpoint'  => $manifestEp,
            'progress_endpoint'  => $progressEp,
            'chunk_bytes'        => $chunkBytes,
            'scratch_dir'        => $scratchDir,
            'wp_content_path'    => defined('WP_CONTENT_DIR') ? WP_CONTENT_DIR : '',
            'db'                 => $this->dbCreds(),
            'destination_kind'   => $destinationKind,
            'destination_config' => $destinationConfig,
            // ADR-037 Sprint 1, 1B: preflight warnings — soft signals (e.g.
            // max_allowed_packet < 16 MiB) that don't block the run but should
            // be surfaced to the operator. TaskRunner attaches these to the
            // first progress event as `preflight_warnings`.
            'preflight_warnings' => $preflight['warnings'],
            // ADR-051 incremental backup fields. All default to no-op values
            // when is_incremental=false so the full-backup pipeline is unchanged.
            'is_incremental'         => $isIncremental,
            'parent_snapshot_id'     => $parentSnapshotId,
            'base_snapshot_id'       => $baseSnapshotId,
            'generation'             => $generation,
            // PrevFilesListChunks: presigned GET URLs for the parent's files.list
            // (ADR-051 W1 wiring). Empty for a full-base run.
            'prev_files_list_chunks' => $prevFilesListChunks,
            // Track A (#187): selective component backup + exclusions.
            // components uses singular entry_kind vocab (plugin|theme|upload|wp-content|db|core).
            'components'             => $components,
            // include_db: null = follow snapshot kind (no filter active),
            // true = dump DB, false = skip DB dump (components filter excludes db).
            'include_db'             => $includeDb,
            'include_core'           => $includeCore,
            'exclude_paths'          => $excludePaths,
            'exclude_extensions'     => $excludeExtensions,
            'exclude_file_size_mb'   => $excludeFileSizeMb,
        ];

        // --- 4. Seed the task row (with params nested in sub_state) -------
        // The watchdog rehydrates the runner from sub_state.params on a
        // stall recovery, so the params MUST be persisted at seed time.
        // TaskRunner itself also seeds the row on first run() if missing —
        // but doing it here as well lets us hand off a fully-formed row
        // even before the runner gets CPU time (matters for the watchdog
        // schedule, which fires +120 s from THIS moment).
        $this->seedTaskRow($snapshotId, $kind, $runnerParams);

        // --- 5. Schedule the watchdog cron event --------------------------
        Watchdog::schedule($snapshotId, Watchdog::RESCHEDULE_SECONDS);

        // --- 6. Trigger the backup runner ---------------------------------
        //
        // GitHub issue #232: scheduled backups intermittently stalled
        // PERMANENTLY at phase=queued because the ONLY cron-independent
        // starter — the in-process shutdown-function runner below — used to
        // register ONLY when isLoopbackGated() detected a membership/privacy
        // redirect. On a non-gated site with no visitor traffic in the next
        // few seconds (or DISABLE_WP_CRON, where spawn_cron() is a documented
        // no-op), NOTHING invoked TaskRunner::run() for the first time, so
        // the freshly-seeded row never left `queued` (started_at ==
        // last_progress_at, resume_count=0 — no cron tick ever arrived to
        // even attempt a resume). The fix: register the in-process runner
        // UNCONDITIONALLY on every trigger — it is a durable, cron-
        // INDEPENDENT starter that runs in the SAME FPM worker that already
        // produced the ACK response to the CP, so it fires regardless of
        // cron health.
        //
        //   1. A PHP shutdown handler (fires after execute() returns and WP
        //      REST flushes the response body) calls the SAPI-aware
        //      ConnectionFinisher (defensive — WP has usually already closed
        //      its output buffers) then runs TaskRunner::run() directly in
        //      the same PHP process. ConnectionFinisher tries
        //      fastcgi_finish_request() (PHP-FPM), then
        //      litespeed_finish_request() (OpenLiteSpeed — GH #274), then a
        //      portable best-effort flush.
        //   2. execute() returns the ACK normally. The CP sees 200 < 1 s.
        //   3. The worker runs the backup while the connection has been
        //      released by ConnectionFinisher. On hosts where none of the
        //      finish-request functions fully close the upstream connection,
        //      ignore_user_abort(true) + a bounded set_time_limit() ensure
        //      the runner is not killed mid-backup.
        //
        // wp_schedule_single_event('wpmgr_backup_run') + spawn_cron() below
        // are KEPT as secondary triggers (they preserve the #131 watchdog +
        // the separate-FPM-worker FCGI-timeout fix for hosts where the
        // in-process path is unreliable on an unusual SAPI configuration).
        // The now-guaranteed double-fire this creates — the in-process
        // runner AND the separate wp-cron-dispatched worker both calling
        // TaskRunner::run() for the same snapshot — is handled by
        // TaskRunner::run()'s own GH #232 MySQL advisory run-lock: whichever
        // process arrives second loses the lock and returns without
        // mutating phase.
        if (function_exists('wp_schedule_single_event')) {
            wp_schedule_single_event(time(), 'wpmgr_backup_run', [$snapshotId]);
        }

        // isLoopbackGated() is still probed — its own DebugLog side effect
        // surfaces likely membership/privacy-gate hosts for diagnostics —
        // but its result no longer decides whether the in-process runner
        // below registers (see the trigger doc above).
        if ($this->isLoopbackGated()) {
            DebugLog::write(sprintf(
                'WPMgr Backup: snapshot %s loopback probe reports gated; the always-on in-process shutdown runner is the primary starter for this run',
                $snapshotId
            ));
        }

        // Capture the params in a local variable so the closure does not
        // hold a reference to $this (which would keep the entire
        // BackupCommand object alive through the shutdown chain). $finisher
        // is captured too — it is a small, stateless collaborator (no $this
        // reference of its own), so this doesn't reintroduce the leak.
        $shutdownParams = $runnerParams;
        $finisher       = $this->finisher;
        register_shutdown_function(static function () use ($shutdownParams, $finisher): void {
            // The SAPI-aware finish. Called defensively: WP has already
            // closed its own output buffers before shutdown handlers run, so
            // the response was most likely already sent. This is a
            // belt-and-suspenders flush that also covers OpenLiteSpeed
            // (GH #274), which has no fastcgi_finish_request().
            $finisher->finish();
            @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged,Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running backup runner must not hit max_execution_time; @-guarded
            @ignore_user_abort(true);
            try {
                (new TaskRunner($shutdownParams))->run();
            } catch (\Throwable $e) {
                \WPMgr\Agent\Support\DebugLog::write('WPMgr Backup: in-process runner fatal: ' . $e->getMessage());
            }
        });

        // Always fire spawn_cron: it is the primary trigger on non-gated
        // sites and a harmless no-op on gated ones (the loopback redirects
        // away) — the always-on in-process shutdown runner above covers the
        // gap either way.
        if (function_exists('spawn_cron')) {
            @spawn_cron(); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- spawn_cron may emit a harmless notice when DISABLE_WP_CRON is set; @-suppressed intentionally
        }

        return ['ok' => true, 'detail' => 'accepted'];
    }

    /**
     * Atomically claim the snapshot_id in wpmgr_backup_runs. Returns true if
     * we won the race. Loses if an active row exists within the dedup window.
     */
    private function tryClaimDedup(string $snapshotId): bool
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return true; // No DB seam — allow the run (single-process dev).
        }
        $table  = $wpdb->prefix . Schema::BACKUP_RUNS_TABLE;
        $now    = time();
        $cutoff = $now - self::DEDUP_WINDOW_SECONDS;

        // @phpstan-ignore-next-line
        $existing = $wpdb->get_row($wpdb->prepare("SELECT pid, started_at FROM {$table} WHERE snapshot_id = %s", $snapshotId)); // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- interpolated identifier is prefix+constant (trusted); direct query on plugin-owned dedup table; no core helper exists
        if (is_object($existing) && (int) $existing->started_at > $cutoff) {
            return false;
        }
        if (is_object($existing)) {
            // @phpstan-ignore-next-line
            $wpdb->update($table, ['pid' => getmypid() ?: 0, 'started_at' => $now], ['snapshot_id' => $snapshotId], ['%d', '%d'], ['%s']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- direct update on plugin-owned dedup table; no core helper exists
            return true;
        }
        // @phpstan-ignore-next-line
        $inserted = $wpdb->insert($table, ['snapshot_id' => $snapshotId, 'pid' => getmypid() ?: 0, 'started_at' => $now], ['%s', '%d', '%d']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery -- direct insert on plugin-owned dedup table; no core helper exists
        return $inserted !== false;
    }

    private function releaseDedup(string $snapshotId): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        $table = $wpdb->prefix . Schema::BACKUP_RUNS_TABLE;
        // @phpstan-ignore-next-line
        @$wpdb->delete($table, ['snapshot_id' => $snapshotId], ['%s']); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- direct delete on plugin-owned dedup table; no core helper exists
    }

    /**
     * Create the per-snapshot scratch directory inside wp-content.
     *
     * Path: `wp-content/wpmgr-agent/runs/<snapshot_id>/`.
     *
     * Same rationale as RestoreCommand::prepareScratchDir — wp-content is the
     * only directory guaranteed writable on every WordPress host (uploads land
     * there). The whole-wp-content swap during a concurrent restore would
     * normally destroy this dir, but FilesRestorer::PRESERVE_FROM_LIVE includes
     * 'wpmgr-agent' so the scratch survives the swap. The backup pipeline's
     * task runner cleans this dir up on completion via cleanupOnCompleted.
     *
     * Idempotent — a watchdog resume returns the same dir.
     */
    private function prepareScratchDir(string $snapshotId): string
    {
        $base = WP_CONTENT_DIR . '/wpmgr-agent/runs';
        if (!is_dir($base) && !wp_mkdir_p($base) && !is_dir($base)) {
            throw new \RuntimeException('cannot create backup scratch base: ' . esc_html($base));
        }
        $dir = $base . DIRECTORY_SEPARATOR . $snapshotId;
        if (!is_dir($dir) && !@mkdir($dir, 0700) && !is_dir($dir)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir -- explicit 0700 perms on scratch dir; wp_mkdir_p would apply the wider FS_CHMOD_DIR
            throw new \RuntimeException('cannot create scratch dir');
        }
        @chmod($dir, 0700); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0700); WP_Filesystem would coerce to wider FS_CHMOD_DIR
        return $dir;
    }

    /**
     * Seed the wpmgr_backup_tasks row with INSERT IGNORE so a concurrent
     * runner doesn't race us. sub_state.params holds the runner config so
     * the watchdog can rehydrate without re-receiving it from the CP.
     */
    private function seedTaskRow(string $snapshotId, string $kind, array $runnerParams): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        $table = $wpdb->prefix . Schema::BACKUP_TASKS_TABLE;
        $now   = time();
        $subState = (string) wp_json_encode(['params' => $runnerParams]);

        // @phpstan-ignore-next-line
        $wpdb->query($wpdb->prepare( // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct INSERT IGNORE on plugin-owned table; no core helper exists
            // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- interpolated identifier is prefix+constant (trusted); values bound via placeholders
            "INSERT IGNORE INTO {$table} (snapshot_id, kind, phase, sub_state, started_at, last_progress_at, resume_count, max_resumes) VALUES (%s, %s, %s, %s, %d, %d, %d, %d)",
            $snapshotId,
            $kind,
            TaskRunner::PHASE_QUEUED,
            $subState,
            $now,
            $now,
            0,
            6
        ));
    }

    /**
     * Pull DB credentials from the WP runtime constants. ifsnop/mysqldump-php
     * connects via PDO using these.
     *
     * @return array{host:string,user:string,password:string,name:string,prefix:string}
     */
    private function dbCreds(): array
    {
        global $wpdb;
        return [
            'host'     => defined('DB_HOST') ? (string) DB_HOST : 'localhost',
            'user'     => defined('DB_USER') ? (string) DB_USER : '',
            'password' => defined('DB_PASSWORD') ? (string) DB_PASSWORD : '',
            'name'     => defined('DB_NAME') ? (string) DB_NAME : '',
            'prefix'   => is_object($wpdb) && isset($wpdb->prefix) ? (string) $wpdb->prefix : 'wp_',
        ];
    }

    /**
     * Probe whether the wp-cron loopback URL is gated by a membership or
     * privacy plugin.
     *
     * Sends a non-redirect-following GET to /wp-cron.php with a 3-second
     * timeout. Returns true when the response is a 3xx redirect (the
     * front-end redirects unauthenticated requests to a login page) or when
     * the request fails entirely (WP_Error — DNS, connect, TLS). Returns
     * false for any other status code (2xx, 4xx, 5xx) — the loopback is
     * reachable even if it returns an error, so spawn_cron will work.
     *
     * Why we only follow 0 redirects: a single HTTP redirect is the
     * definitive sign that the gate is active. Multi-hop chains are not
     * considered; we treat any redirect as gated.
     *
     * DISABLE_WP_CRON / ALTERNATE_WP_CRON short-circuit: when WP cron is
     * disabled the loopback probe is irrelevant (spawn_cron is a no-op too),
     * so we return false and let the normal path proceed.
     *
     * `protected` is a deliberate test seam.
     */
    protected function isLoopbackGated(): bool
    {
        // If WP cron is disabled entirely, spawn_cron is a no-op regardless
        // of whether the loopback is gated — nothing to probe.
        if (defined('DISABLE_WP_CRON') && (bool) constant('DISABLE_WP_CRON')) {
            return false;
        }

        if (!function_exists('wp_remote_get') || !function_exists('site_url')) {
            return false;
        }

        $cronUrl = (string) site_url('/wp-cron.php');
        if ($cronUrl === '' || $cronUrl === '/wp-cron.php') {
            return false;
        }

        $response = wp_remote_get(
            $cronUrl,
            [
                'timeout'     => 3,
                'redirection' => 0,  // Do NOT follow redirects — a redirect IS the gate signal.
                'sslverify'   => false,
                'blocking'    => true,
                'user-agent'  => 'WPMgr-Agent/' . (defined('WPMGR_AGENT_VERSION') ? (string) constant('WPMGR_AGENT_VERSION') : '0'),
            ]
        );

        if (is_wp_error($response)) {
            // Cannot reach the loopback at all (DNS failure, TLS error, etc.).
            // Treat as gated — in-process fallback is safer than spawning into
            // a black hole.
            DebugLog::write(sprintf(
                'WPMgr Backup: loopback probe failed (%s); using in-process runner as fallback',
                $response->get_error_message()
            ));
            return true;
        }

        $code = (int) wp_remote_retrieve_response_code($response);
        if ($code >= 300 && $code < 400) {
            // Redirect — the front-end gate is active.
            $location = wp_remote_retrieve_header($response, 'location');
            DebugLog::write(sprintf(
                'WPMgr Backup: loopback probe returned %d (redirect to: %s); site appears to be behind a membership/privacy gate — using in-process runner as fallback',
                $code,
                is_string($location) ? $location : '(unknown)'
            ));
            return true;
        }

        return false;
    }

    /**
     * Refusal response — the agent didn't accept the job.
     *
     * @param string $detail Human-readable reason (unchanged wire field).
     * @param string $code   Optional stable machine code for the CP to branch
     *                       on (e.g. 'runner_in_flight'). Empty string omits
     *                       the field entirely — most refusals carry no code.
     * @return array{ok:bool,detail:string,code?:string}
     */
    private function refuse(string $detail, string $code = ''): array
    {
        $response = ['ok' => false, 'detail' => $detail];
        if ($code !== '') {
            $response['code'] = $code;
        }
        return $response;
    }

    /** @param array<string,mixed> $params */
    private function str(array $params, string $key): string
    {
        return isset($params[$key]) && is_string($params[$key]) ? $params[$key] : '';
    }

    /**
     * ADR-037 Sprint 1, 1B — Backup-readiness preflight.
     *
     * Cheap, fail-fast environment probes run BEFORE we burn a scratch dir or
     * a dedup slot. Catches the most common "snapshot starts then 5 minutes
     * later fails on something we could've checked in 50 ms" failure modes
     * operators of site-management and backup plugins see in the wild:
     *
     *   - Disk full at scratch base (need ~2x estimated backup size headroom)
     *   - MySQL max_allowed_packet too small for SQL dump rows
     *   - ZipArchive class/method missing (some PHP builds strip ext-zip)
     *   - DB unreachable (creds wrong, server down)
     *   - Scratch base not writable
     *
     * The contract:
     *   - 'failures' (non-empty) → set ok=false → caller refuses the run and
     *     records reason_code=preflight_failed on the snapshot row.
     *   - 'warnings' (non-empty, ok=true) → ride along with the first progress
     *     event as preflight_warnings — visible to the operator but not
     *     blocking.
     *
     * @return array{ok:bool,failures:list<string>,warnings:list<string>}
     */
    private function preflightChecks(): array
    {
        $failures = [];
        $warnings = [];

        // --- ZipArchive (HARD fail) ---
        // Required by FilesArchiver for the per-component part zips. Missing
        // on minimal PHP builds (no `--with-zip` at configure time).
        if (!class_exists('ZipArchive') || !method_exists('ZipArchive', 'addFile')) {
            $failures[] = 'ZipArchive class or addFile method missing (rebuild PHP with ext-zip)';
        }

        // --- DB ping (HARD fail) ---
        // Cheap SELECT 1 confirms wpdb's persistent connection actually works.
        // A DB outage now would surface 5+ minutes later inside the dump phase.
        global $wpdb;
        if (is_object($wpdb)) {
            try {
                // @phpstan-ignore-next-line — runtime wpdb seam.
                $probe = $wpdb->get_var('SELECT 1'); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- preflight DB connectivity probe; no suitable cached alternative
                if ((string) $probe !== '1') {
                    $failures[] = 'DB ping (SELECT 1) returned unexpected: ' . substr((string) $probe, 0, 80);
                }
            } catch (\Throwable $e) {
                $failures[] = 'DB ping threw: ' . substr($e->getMessage(), 0, 200);
            }
        }

        // --- max_allowed_packet (HARD fail < 1 MiB, WARN < 16 MiB) ---
        // Bound rows (e.g. base64-encoded uploads in postmeta, serialized
        // session data in transients) blow up the dump if the server's
        // packet limit is tight. The documented floor for managed-host
        // safety is 16 MiB; below 1 MiB the dump fails for basically every
        // real WP site.
        $maxPacket = $this->readMaxAllowedPacket();
        if ($maxPacket !== null) {
            if ($maxPacket < (1 << 20)) {
                $failures[] = 'max_allowed_packet is ' . $maxPacket . ' bytes; need at least 1 MiB';
            } elseif ($maxPacket < (16 << 20)) {
                $warnings[] = 'max_allowed_packet is ' . round($maxPacket / (1 << 20), 1) . ' MiB; 16+ MiB recommended for fat row dumps';
            }
        }

        // --- Scratch base writable (HARD fail) ---
        // prepareScratchDir runs immediately after preflight; if the base dir
        // can't be created or isn't writable we want to surface that with
        // operator-readable context instead of a generic "scratch dir creation
        // failed" further down.
        $scratchBase = (defined('WP_CONTENT_DIR') ? WP_CONTENT_DIR : '') . '/wpmgr-agent/runs';
        if ($scratchBase === '/wpmgr-agent/runs') {
            $failures[] = 'WP_CONTENT_DIR is undefined; cannot resolve scratch base';
        } else {
            $parent = dirname($scratchBase);
            // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_is_writable -- headless agent; WP_Filesystem never initialized; direct writability probe is the only option
            if (!is_dir($scratchBase) && is_dir($parent) && !is_writable($parent)) {
                $failures[] = 'scratch base parent not writable: ' . $parent;
            } elseif (is_dir($scratchBase) && !is_writable($scratchBase)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_is_writable -- headless agent; WP_Filesystem never initialized; direct writability probe is the only option
                $failures[] = 'scratch base not writable: ' . $scratchBase;
            }
        }

        // --- Disk headroom (HARD fail) ---
        // disk_free_space(scratchBase) >= 2x estimateBackupSize() — gives
        // enough room for the dump AND a generation of ciphertext chunks
        // before the upload pass starts unlinking finished files. Skipped
        // (with a soft warn) if disk_free_space() is disabled on this host.
        if ($scratchBase !== '/wpmgr-agent/runs') {
            $probeDir = is_dir($scratchBase) ? $scratchBase : (is_dir(dirname($scratchBase)) ? dirname($scratchBase) : '');
            if ($probeDir !== '' && function_exists('disk_free_space')) {
                $free = @disk_free_space($probeDir);
                if ($free === false) {
                    $warnings[] = 'disk_free_space() returned false; cannot probe headroom';
                } else {
                    $needed = 2 * $this->estimateBackupSize();
                    if ((int) $free < $needed) {
                        $failures[] = 'insufficient disk: free=' . (int) $free . ' bytes, need 2x estimated backup (' . $needed . ')';
                    }
                }
            }
        }

        return [
            'ok'       => $failures === [],
            'failures' => $failures,
            'warnings' => $warnings,
        ];
    }

    /**
     * Read MySQL @@max_allowed_packet (bytes). Returns null when the variable
     * cannot be read (cheap wpdb fail — we don't want preflight to false-fail
     * a backup because of a permissions quirk).
     */
    private function readMaxAllowedPacket(): ?int
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return null;
        }
        try {
            // @phpstan-ignore-next-line
            $value = $wpdb->get_var('SELECT @@max_allowed_packet'); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- preflight max_allowed_packet probe; informational session variable read; no suitable cached alternative
        } catch (\Throwable $e) {
            return null;
        }
        if ($value === null) {
            return null;
        }
        if (!is_numeric($value)) {
            return null;
        }
        return (int) $value;
    }

    /**
     * Estimate the total backup payload in bytes for headroom checks. Sum of:
     *   - DB plaintext bytes from `SHOW TABLE STATUS` (Data_length + Index_length)
     *   - wp-content disk usage, capped to a 5s wall-clock walk so a giant
     *     uploads/ tree doesn't make preflight slow.
     *
     * Approximate by design — the disk-headroom check multiplies by 2x so a
     * 30-50% under-estimate from the 5s walk cap still keeps us safe.
     */
    private function estimateBackupSize(): int
    {
        $bytes = 0;

        global $wpdb;
        if (is_object($wpdb)) {
            try {
                // @phpstan-ignore-next-line — runtime wpdb seam.
                $rows = $wpdb->get_results('SHOW TABLE STATUS', ARRAY_A); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- preflight size estimate; informational SHOW query; no suitable cached alternative
                if (is_array($rows)) {
                    foreach ($rows as $row) {
                        if (!is_array($row)) {
                            continue;
                        }
                        $bytes += isset($row['Data_length']) ? (int) $row['Data_length'] : 0;
                        $bytes += isset($row['Index_length']) ? (int) $row['Index_length'] : 0;
                    }
                }
            } catch (\Throwable $e) {
                // best-effort estimate; ignore failures.
            }
        }

        if (defined('WP_CONTENT_DIR') && is_dir(WP_CONTENT_DIR)) {
            $bytes += $this->duCapped(WP_CONTENT_DIR, 5);
        }

        return $bytes;
    }

    /**
     * Capped recursive disk-usage walk. Returns the sum of file sizes under
     * $path or 0 on error. Bails as soon as $maxSeconds elapses — useful for
     * preflight where we want a representative sample, not an exhaustive scan.
     */
    private function duCapped(string $path, int $maxSeconds): int
    {
        $bytes = 0;
        $deadline = microtime(true) + $maxSeconds;

        try {
            $it = new \RecursiveIteratorIterator(
                new \RecursiveDirectoryIterator($path, \FilesystemIterator::SKIP_DOTS | \FilesystemIterator::FOLLOW_SYMLINKS),
                \RecursiveIteratorIterator::LEAVES_ONLY,
                \RecursiveIteratorIterator::CATCH_GET_CHILD
            );
            foreach ($it as $file) {
                if (microtime(true) > $deadline) {
                    break;
                }
                if ($file instanceof \SplFileInfo && $file->isFile()) {
                    $bytes += (int) $file->getSize();
                }
            }
        } catch (\Throwable $e) {
            // Silently swallow — preflight headroom is an estimate, not a contract.
        }

        return $bytes;
    }

    /**
     * Record preflight_failed onto the snapshot's task row so the watchdog
     * (and the CP via the progress callback on the next attempt) can read
     * the failure reason. Best-effort write — a missing schema/table doesn't
     * block the refusal.
     *
     * @param list<string> $failures
     */
    private function writePreflightFailure(string $snapshotId, array $failures): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        $table = $wpdb->prefix . Schema::BACKUP_TASKS_TABLE;
        $now   = time();
        $subState = (string) wp_json_encode([
            'reason_code'    => 'preflight_failed',
            'phase_detail'   => ['failures' => $failures],
        ]);
        try {
            // @phpstan-ignore-next-line
            $wpdb->query($wpdb->prepare( // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct INSERT IGNORE on plugin-owned table; no core helper exists
                // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- interpolated identifier is prefix+constant (trusted); values bound via placeholders
                "INSERT IGNORE INTO {$table}
                 (snapshot_id, kind, phase, sub_state, started_at, last_progress_at, resume_count, max_resumes)
                 VALUES (%s, %s, %s, %s, %d, %d, %d, %d)",
                $snapshotId,
                'full',
                'failed',
                $subState,
                $now,
                $now,
                0,
                0
            ));
        } catch (\Throwable $e) {
            // best-effort — refusal path proceeds either way.
        }
    }
}
