<?php
/**
 * Restore command: M5.6 / ADR-034 — cron-dispatched restore engine.
 *
 * Mirror of the M5.6 BackupCommand cron+spawn_cron pattern. Replaces the
 * legacy in-process M4 restore (which downloaded chunks + decrypted + wrote
 * files inline inside the REST request — fine for tiny snapshots, would 504
 * on real sites). The new flow:
 *
 *   1. Validate JWT-signed request (Connector did the crypto; we sanity check
 *      shape + recipient — there's no age_recipient in restore though).
 *   2. Atomically claim (snapshot_id, restore_id) in `wpmgr_restore_runs` so a
 *      retry of a lost-ACK doesn't spawn a second runner.
 *   3. Seed `wpmgr_restore_tasks` row with all runner params nested in
 *      `sub_state.params` so the watchdog can rehydrate.
 *   4. Schedule the RestoreWatchdog cron for +120 s (stall detection).
 *   5. `wp_schedule_single_event(time(), 'wpmgr_restore_run', [...])` + call
 *      `spawn_cron()` to fire wp-cron in a fresh FPM worker.
 *   6. Return ACK in milliseconds.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/restore
 *   {
 *     "snapshot_id":       "uuid",
 *     "restore_id":        "uuid",      // CP-generated; unique per restore run
 *     "kind":              "files"|"db"|"full",
 *     "progress_endpoint": "https://cp/.../progress",
 *     "destination_kind":   "cp"|"local"|"s3_compat",  // ADR-036 P1; absent/empty = "cp"
 *     "destination_config": { "local_path_prefix": "..." },  // only for "local"; omitempty
 *     "manifest": {
 *       "entries": [
 *         { "logical_path": "database.sql.gz",
 *           "chunks": [ { "hash": "...", "presigned_url": "...", "size": N }, ... ] },
 *         { "logical_path": "wp-content.part001.zip", "chunks": [ ... ] },
 *         ...
 *       ]
 *     },
 *     "chunk_bytes": 4194304   // hint only (presented in preflight telemetry)
 *   }
 *   response: { "ok": bool, "detail": string }
 *
 * ADR-036 P1 storage adapter: a `destination_kind=local` snapshot's chunks
 * carry Hash + Size but NO presigned_url in the manifest above — they live on
 * this same webserver's disk (written by the matching local-destination
 * backup) and RestoreRunner reads them by content hash instead of an HTTP
 * GET. `cp`/`s3_compat` chunks are unchanged: presigned GET, same as today.
 *
 * The agent's REPLY means "accepted & runner started". Completion happens
 * when RestoreRunner posts the `completed` progress event to /progress.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Backup\RestoreRunner;
use WPMgr\Agent\Backup\RestoreWatchdog;
use WPMgr\Agent\Schema;

/**
 * Accepts a `restore` command from the CP, seeds the task row, and hands
 * off to the cron-dispatched RestoreRunner via wp_schedule_single_event +
 * spawn_cron(). Returns ACK in well under a second.
 */
final class RestoreCommand implements CommandInterface
{
    /** Valid restore kinds (mirror of CP RestoreRequest.Kind). */
    private const KINDS = ['files', 'db', 'full'];

    /** Default plaintext chunk size (matches CP agentcmd.ChunkBytes). */
    private const DEFAULT_CHUNK_BYTES = 4 << 20;

    /**
     * Dedup window: refuse to seed a second task for the same
     * (snapshot_id, restore_id) within this many seconds of a previous claim.
     */
    private const DEDUP_WINDOW_SECONDS = 300;

    public function __construct()
    {
        // No collaborators — RestoreRunner instantiates its own seams when
        // the cron worker picks it up. Matches BackupCommand.
    }

    public function name(): string
    {
        return 'restore';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused).
     * @param array<string,mixed> $params RestoreRequest fields.
     * @return array{ok:bool,detail:string,code?:string}
     */
    public function execute(array $claims, array $params): array
    {
        $snapshotId = $this->str($params, 'snapshot_id');
        $restoreId  = $this->str($params, 'restore_id');
        $kind       = $this->str($params, 'kind');
        $progressEp = $this->str($params, 'progress_endpoint');

        $chunkBytes = isset($params['chunk_bytes']) && is_numeric($params['chunk_bytes']) && (int) $params['chunk_bytes'] > 0
            ? (int) $params['chunk_bytes']
            : self::DEFAULT_CHUNK_BYTES;

        // ADR-036 P1 storage adapter: mirrors BackupCommand's destination_kind
        // / destination_config wiring. Absent/empty destination_kind means
        // 'cp' — the existing presigned-URL restore path, bit-for-bit
        // unchanged for every CP build that predates this feature.
        $destinationKind = isset($params['destination_kind']) && is_string($params['destination_kind']) && $params['destination_kind'] !== ''
            ? $params['destination_kind']
            : 'cp';
        $destinationConfig = isset($params['destination_config']) && is_array($params['destination_config'])
            ? $params['destination_config']
            : [];

        // --- 1. Input validation -------------------------------------------
        if ($snapshotId === '' || $restoreId === '') {
            return $this->refuse('missing snapshot_id or restore_id');
        }
        if (!preg_match('/^[a-f0-9-]{36}$/i', $snapshotId)) {
            return $this->refuse('invalid snapshot id');
        }
        if (!preg_match('/^[a-f0-9-]{36}$/i', $restoreId)) {
            return $this->refuse('invalid restore id');
        }
        if (!in_array($kind, self::KINDS, true)) {
            return $this->refuse('invalid kind');
        }

        $chunkDownloads = $this->parseChunkDownloads($params, $destinationKind);
        if ($chunkDownloads === []) {
            return $this->refuse('no chunk_downloads / manifest entries supplied');
        }

        // --- 1.5. Restore-readiness preflight (ADR-037 Sprint 1, 1B) ------
        //
        // Mirror of BackupCommand's preflight. Restore also needs disk for the
        // staging tree, a writable scratch base, ZipArchive for the per-
        // component extract, and a healthy DB connection for the SQL restore
        // phase. Defense-in-depth: if any of these fail later, the operator
        // gets a long delay before the failure surfaces; preflight closes
        // that perception gap.
        $preflight = $this->preflightChecks();
        if (!$preflight['ok']) {
            $this->writePreflightFailure($snapshotId, $restoreId, $preflight['failures']);
            return $this->refuse('preflight_failed: ' . implode(', ', $preflight['failures']));
        }

        // --- 2. Dedup claim ------------------------------------------------
        Schema::ensureCurrent();
        if (!$this->tryClaimDedup($snapshotId, $restoreId)) {
            return $this->refuse('runner already in flight for this restore', 'runner_in_flight');
        }

        // --- 3. Prepare scratch dir + assemble runner params --------------
        try {
            $scratchDir = $this->prepareScratchDir($snapshotId, $restoreId);
        } catch (\Throwable $e) {
            $this->releaseDedup($snapshotId, $restoreId);
            return $this->refuse('scratch dir creation failed');
        }

        // P0 URL rewriter: extract target_* URLs from the RestoreRequest so
        // the URL_REWRITE phase can rewrite siteurl/home/content/upload
        // references in the tmp tables before swap. When the CP doesn't pass
        // them (older CP, or a same-environment restore) we fall back to the
        // live site's values inside RestoreRunner::runUrlRewrite — same-env
        // restore then short-circuits to a no-op.
        $targetSiteUrl    = $this->str($params, 'target_site_url');
        $targetHomeUrl    = $this->str($params, 'target_home_url');
        $targetContentUrl = $this->str($params, 'target_content_url');
        $targetUploadUrl  = $this->str($params, 'target_upload_url');
        $sourceSiteUrl    = $this->str($params, 'source_site_url');
        $sourceHomeUrl    = $this->str($params, 'source_home_url');
        $sourceContentUrl = $this->str($params, 'source_content_url');
        $sourceUploadUrl  = $this->str($params, 'source_upload_url');

        // ADR-049: incremental chain restore wire fields. All are backward-
        // compatible: absent = non-chain restore (zero/empty defaults).
        $isChainRestore   = isset($params['is_chain_restore']) && (bool) $params['is_chain_restore'];
        $targetGeneration = isset($params['target_generation']) && is_numeric($params['target_generation'])
            ? (int) $params['target_generation'] : 0;
        $estimatedBytes   = isset($params['estimated_bytes']) && is_numeric($params['estimated_bytes'])
            ? (int) $params['estimated_bytes'] : 0;
        $tombstonePaths   = isset($params['tombstone_paths']) && is_array($params['tombstone_paths'])
            ? array_values(array_filter($params['tombstone_paths'], 'is_string'))
            : [];

        $runnerParams = [
            'snapshot_id'       => $snapshotId,
            'restore_id'        => $restoreId,
            'kind'              => $kind,
            'progress_endpoint' => $progressEp,
            'chunk_downloads'   => $chunkDownloads,
            'chunk_bytes'       => $chunkBytes,
            'scratch_dir'       => $scratchDir,
            'wp_content_path'   => defined('WP_CONTENT_DIR') ? WP_CONTENT_DIR : '',
            'wp_root'           => defined('ABSPATH') ? ABSPATH : '',
            'db'                => $this->dbCreds(),
            // ADR-036 P1 storage adapter: which destination RestoreRunner
            // reads chunks from. 'local' chunks carry no presigned_url (see
            // parseChunkDownloads) — the runner reads them off local disk by
            // hash instead of an HTTP GET.
            'destination_kind'   => $destinationKind,
            'destination_config' => $destinationConfig,
            // P0 URL rewriter: target/source URLs for cross-env restore.
            'target_site_url'    => $targetSiteUrl,
            'target_home_url'    => $targetHomeUrl,
            'target_content_url' => $targetContentUrl,
            'target_upload_url'  => $targetUploadUrl,
            'source_site_url'    => $sourceSiteUrl,
            'source_home_url'    => $sourceHomeUrl,
            'source_content_url' => $sourceContentUrl,
            'source_upload_url'  => $sourceUploadUrl,
            // ADR-037 Sprint 1, 1B: preflight warnings ride along with the
            // first progress event (same surface as BackupCommand).
            'preflight_warnings' => $preflight['warnings'],
            // ADR-049: chain restore fields (sanitized again inside runner).
            'is_chain_restore'   => $isChainRestore,
            'target_generation'  => $targetGeneration,
            'estimated_bytes'    => $estimatedBytes,
            'tombstone_paths'    => $tombstonePaths,
        ];

        // --- 4. Seed the task row ------------------------------------------
        $this->seedTaskRow($snapshotId, $restoreId, $kind, $runnerParams);

        // --- 5. Schedule the watchdog -------------------------------------
        RestoreWatchdog::schedule($snapshotId, $restoreId, RestoreWatchdog::RESCHEDULE_SECONDS);

        // --- 6. Hand off to cron in a separate FPM worker -----------------
        if (function_exists('wp_schedule_single_event')) {
            wp_schedule_single_event(time(), RestoreWatchdog::HOOK_RUN, [$snapshotId, $restoreId]);
        }
        if (function_exists('spawn_cron')) {
            @spawn_cron();
        }

        return ['ok' => true, 'detail' => 'accepted'];
    }

    /**
     * Parse the chunk_downloads array out of the request. Accepts both:
     *   - flat top-level "chunk_downloads": [ { logical_path, chunks }, ... ]
     *   - nested "manifest": { "entries": [ ... ] } per the wire contract
     *
     * @param array<string,mixed> $params
     * @param string              $destinationKind 'cp' | 'local' | 's3_compat'
     *                                              (already defaulted by the
     *                                              caller). Only 'local' chunk
     *                                              entries are allowed to omit
     *                                              presigned_url.
     * @return list<array<string,mixed>>
     */
    private function parseChunkDownloads(array $params, string $destinationKind): array
    {
        $candidates = [];
        if (isset($params['chunk_downloads']) && is_array($params['chunk_downloads'])) {
            $candidates = $params['chunk_downloads'];
        } elseif (isset($params['manifest']) && is_array($params['manifest'])
            && isset($params['manifest']['entries']) && is_array($params['manifest']['entries'])
        ) {
            $candidates = $params['manifest']['entries'];
        }

        $out = [];
        foreach ($candidates as $entry) {
            if (!is_array($entry)) {
                continue;
            }
            $logical = isset($entry['logical_path']) && is_string($entry['logical_path'])
                ? $entry['logical_path']
                : (isset($entry['path']) && is_string($entry['path']) ? $entry['path'] : '');
            $chunks  = isset($entry['chunks']) && is_array($entry['chunks']) ? $entry['chunks'] : [];
            if ($logical === '' || $chunks === []) {
                continue;
            }
            // Normalize each chunk to the runner's expected key names
            // (`hash`, `presigned_url`). Accept either {hash, presigned_url}
            // or {blake3, url}/{blake3, get_url} for compatibility with the
            // M4 manifest shape.
            //
            // ADR-036 P1: a `destination_kind=local` snapshot's chunks live
            // on this webserver's own disk (RestoreRunner reads them by
            // hash — see fetchLocalChunk), so the CP never mints a
            // presigned_url for them. Every other destination kind still
            // requires one — a cp/s3_compat chunk with no URL is malformed.
            $norm = [];
            foreach ($chunks as $c) {
                if (!is_array($c)) {
                    continue;
                }
                $hash = (string) ($c['hash'] ?? $c['blake3'] ?? '');
                $url  = (string) ($c['presigned_url'] ?? $c['url'] ?? $c['get_url'] ?? '');
                if ($hash === '') {
                    continue;
                }
                if ($url === '' && $destinationKind !== 'local') {
                    continue;
                }
                $row = ['hash' => $hash];
                if ($url !== '') {
                    $row['presigned_url'] = $url;
                }
                if (isset($c['size']) && is_numeric($c['size'])) {
                    $row['size'] = (int) $c['size'];
                }
                $norm[] = $row;
            }
            if ($norm !== []) {
                $out[] = ['logical_path' => $logical, 'chunks' => $norm];
            }
        }
        return $out;
    }

    /**
     * Atomically claim (snapshot_id, restore_id) in wpmgr_restore_runs.
     * Returns true if we won the race.
     */
    private function tryClaimDedup(string $snapshotId, string $restoreId): bool
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return true;
        }
        /** @var \wpdb $wpdb */
        $table  = $wpdb->prefix . Schema::BACKUP_RESTORE_RUNS_TABLE;
        $now    = time();
        $cutoff = $now - self::DEDUP_WINDOW_SECONDS;

        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct query on plugin-owned table; no core $wpdb helper exists; correctness requires a live read (anti-replay/locking)
        $existing = $wpdb->get_row($wpdb->prepare(
            "SELECT pid, started_at FROM {$table} WHERE snapshot_id = %s AND restore_id = %s", // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- interpolated identifier is prefix+constant (trusted); values bound via placeholders
            $snapshotId,
            $restoreId
        ));
        if (is_object($existing) && (int) $existing->started_at > $cutoff) {
            return false;
        }
        if (is_object($existing)) {
            // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct query on plugin-owned table; no core $wpdb helper exists
            $wpdb->update(
                $table,
                ['pid' => getmypid() ?: 0, 'started_at' => $now],
                ['snapshot_id' => $snapshotId, 'restore_id' => $restoreId],
                ['%d', '%d'],
                ['%s', '%s']
            );
            return true;
        }
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery -- direct query on plugin-owned table; no core $wpdb helper exists
        $inserted = $wpdb->insert(
            $table,
            [
                'snapshot_id' => $snapshotId,
                'restore_id'  => $restoreId,
                'pid'         => getmypid() ?: 0,
                'started_at'  => $now,
            ],
            ['%s', '%s', '%d', '%d']
        );
        return $inserted !== false;
    }

    private function releaseDedup(string $snapshotId, string $restoreId): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        /** @var \wpdb $wpdb */
        $table = $wpdb->prefix . Schema::BACKUP_RESTORE_RUNS_TABLE;
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct query on plugin-owned table; no core $wpdb helper exists
        @$wpdb->delete($table, ['snapshot_id' => $snapshotId, 'restore_id' => $restoreId], ['%s', '%s']);
    }

    /**
     * Create the per-restore scratch directory inside wp-content.
     *
     * Path: `wp-content/wpmgr-agent/restores/<snapshot_id>-<short_restore_id>/`.
     *
     * Why inside wp-content (vs ABSPATH or its parent):
     *   `wp-content/` is the ONLY directory PHP-FPM is guaranteed to be writable
     *   on every WordPress host (because uploads land there). Parent-of-wp-content
     *   (ABSPATH) is read-only at runtime on WP Engine, Pantheon, WP VIP, and
     *   often Kinsta. v0.9.8 briefly relocated scratch to dirname(WP_CONTENT_DIR)
     *   to dodge the swap_files bug but that traded one failure mode for another.
     *
     * Why the swap_files phase doesn't destroy it (v0.9.9+):
     *   FilesRestorer::PRESERVE_FROM_LIVE now includes 'wpmgr-agent' alongside
     *   'plugins/wpmgr-agent'. The whole-wp-content swap copies the live
     *   wpmgr-agent dir (scratch + keystore + restores subdir) into staging
     *   before the rename, so the post-swap wp-content carries everything
     *   forward and PHASE_RESTORE_DB finds <scratch>/database.sql.gz exactly
     *   where it left it. Uses the same allowlist pattern as common backup plugins.
     *
     * Idempotent — a watchdog resume returns the same dir.
     */
    private function prepareScratchDir(string $snapshotId, string $restoreId): string
    {
        $base = WP_CONTENT_DIR . '/wpmgr-agent/restores';
        if (!is_dir($base) && !wp_mkdir_p($base) && !is_dir($base)) {
            throw new \RuntimeException('cannot create restore scratch base: ' . esc_html($base));
        }
        // Short-id to keep the dir name reasonable.
        $clean = preg_replace('/[^a-f0-9]/i', '', $restoreId) ?? '';
        $short = substr($clean, 0, 12);
        $dir   = $base . DIRECTORY_SEPARATOR . $snapshotId . '-' . $short;
        if (!is_dir($dir) && !@mkdir($dir, 0700) && !is_dir($dir)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir -- explicit 0700 perms on restore scratch dir; wp_mkdir_p would apply the wider FS_CHMOD_DIR
            throw new \RuntimeException('cannot create restore scratch dir');
        }
        @chmod($dir, 0700); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0700); WP_Filesystem would coerce to wider FS_CHMOD_DIR
        return $dir;
    }

    /**
     * Seed the wpmgr_restore_tasks row with INSERT IGNORE so a concurrent
     * runner doesn't race us.
     *
     * @param array<string,mixed> $runnerParams
     */
    private function seedTaskRow(string $snapshotId, string $restoreId, string $kind, array $runnerParams): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        /** @var \wpdb $wpdb */
        $table = $wpdb->prefix . Schema::BACKUP_RESTORE_TASKS_TABLE;
        $now   = time();
        $subState = (string) wp_json_encode(['params' => $runnerParams]);

        // @phpstan-ignore-next-line
        $wpdb->query($wpdb->prepare( // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct query on plugin-owned table; no core helper exists
            // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- interpolated identifier is prefix+constant (trusted); values bound via placeholders
            "INSERT IGNORE INTO {$table}
             (snapshot_id, restore_id, kind, phase, sub_state, started_at, last_progress_at, resume_count, max_resumes)
             VALUES (%s, %s, %s, %s, %s, %d, %d, %d, %d)",
            $snapshotId,
            $restoreId,
            $kind,
            RestoreRunner::PHASE_PREFLIGHT,
            $subState,
            $now,
            $now,
            0,
            6
        ));
    }

    /**
     * Pull DB credentials from the WP runtime constants.
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
     * Refusal response.
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
     * ADR-037 Sprint 1, 1B — Restore-readiness preflight.
     *
     * Symmetric with BackupCommand::preflightChecks. Restore needs the same
     * environment guarantees as backup: disk for the staging tree, a writable
     * scratch base, ZipArchive for the per-component extracts, a healthy DB
     * for the SQL restore phase, and a sane MySQL max_allowed_packet for the
     * largest row in the dump.
     *
     * The headroom estimate differs slightly: at restore time we don't have a
     * cheap "expected payload" signal (the manifest's total_size is on the
     * CP, not threaded through to the agent's command params), so the disk
     * check uses a fixed 1 GiB floor — small enough that managed-host scratch
     * almost always has it, large enough to catch "0 bytes free" / "200 MB
     * left" failure modes before scratch creation.
     *
     * @return array{ok:bool,failures:list<string>,warnings:list<string>}
     */
    private function preflightChecks(): array
    {
        $failures = [];
        $warnings = [];

        // --- ZipArchive (HARD fail) ---
        if (!class_exists('ZipArchive') || !method_exists('ZipArchive', 'addFile')) {
            $failures[] = 'ZipArchive class or addFile method missing (rebuild PHP with ext-zip)';
        }

        // --- DB ping (HARD fail) ---
        global $wpdb;
        if (is_object($wpdb)) {
            try {
                // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- connectivity probe on core WP connection; no core helper exists; adding cache would defeat the purpose
                $probe = $wpdb->get_var('SELECT 1');
                if ((string) $probe !== '1') {
                    $failures[] = 'DB ping (SELECT 1) returned unexpected: ' . substr((string) $probe, 0, 80);
                }
            } catch (\Throwable $e) {
                $failures[] = 'DB ping threw: ' . substr($e->getMessage(), 0, 200);
            }
        }

        // --- max_allowed_packet (HARD fail < 1 MiB, WARN < 16 MiB) ---
        $maxPacket = $this->readMaxAllowedPacket();
        if ($maxPacket !== null) {
            if ($maxPacket < (1 << 20)) {
                $failures[] = 'max_allowed_packet is ' . $maxPacket . ' bytes; need at least 1 MiB';
            } elseif ($maxPacket < (16 << 20)) {
                $warnings[] = 'max_allowed_packet is ' . round($maxPacket / (1 << 20), 1) . ' MiB; 16+ MiB recommended for wide rows';
            }
        }

        // --- Scratch base writable + disk headroom (HARD fail) ---
        $scratchBase = (defined('WP_CONTENT_DIR') ? WP_CONTENT_DIR : '') . '/wpmgr-agent/restores';
        if ($scratchBase === '/wpmgr-agent/restores') {
            $failures[] = 'WP_CONTENT_DIR is undefined; cannot resolve scratch base';
        } else {
            $parent = dirname($scratchBase);
            if (!is_dir($scratchBase) && is_dir($parent) && !is_writable($parent)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_is_writable -- headless agent; WP_Filesystem never initialized; direct writability probe is the only option
                $failures[] = 'scratch base parent not writable: ' . $parent;
            } elseif (is_dir($scratchBase) && !is_writable($scratchBase)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_is_writable -- headless agent; WP_Filesystem never initialized; direct writability probe is the only option
                $failures[] = 'scratch base not writable: ' . $scratchBase;
            }

            $probeDir = is_dir($scratchBase) ? $scratchBase : (is_dir($parent) ? $parent : '');
            if ($probeDir !== '' && function_exists('disk_free_space')) {
                $free = @disk_free_space($probeDir);
                if ($free === false) {
                    $warnings[] = 'disk_free_space() returned false; cannot probe headroom';
                } else {
                    // 1 GiB floor — restore staging + DB scratch on a typical
                    // WP site fits inside this with comfortable margin; below
                    // it the swap_files phase is going to fail anyway.
                    $needed = 1 << 30;
                    if ((int) $free < $needed) {
                        $failures[] = 'insufficient disk: free=' . (int) $free . ' bytes, need at least 1 GiB';
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
     * Read MySQL @@max_allowed_packet (bytes). Returns null on read failure.
     */
    private function readMaxAllowedPacket(): ?int
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return null;
        }
        /** @var \wpdb $wpdb */
        try {
            // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- reads a MySQL system variable; no caching applicable; no core $wpdb helper exists
            $value = $wpdb->get_var('SELECT @@max_allowed_packet');
        } catch (\Throwable $e) {
            return null;
        }
        if ($value === null || !is_numeric($value)) {
            return null;
        }
        return (int) $value;
    }

    /**
     * Record preflight_failed onto the restore task row.
     *
     * @param list<string> $failures
     */
    private function writePreflightFailure(string $snapshotId, string $restoreId, array $failures): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        /** @var \wpdb $wpdb */
        $table = $wpdb->prefix . Schema::BACKUP_RESTORE_TASKS_TABLE;
        $now   = time();
        $subState = (string) wp_json_encode([
            'reason_code'  => 'preflight_failed',
            'phase_detail' => ['failures' => $failures],
        ]);
        try {
            // @phpstan-ignore-next-line
            $wpdb->query($wpdb->prepare( // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- direct query on plugin-owned table; no core helper exists
                // phpcs:ignore WordPress.DB.PreparedSQL.InterpolatedNotPrepared -- interpolated identifier is prefix+constant (trusted); values bound via placeholders
                "INSERT IGNORE INTO {$table}
                 (snapshot_id, restore_id, kind, phase, sub_state, started_at, last_progress_at, resume_count, max_resumes)
                 VALUES (%s, %s, %s, %s, %s, %d, %d, %d, %d)",
                $snapshotId,
                $restoreId,
                'full',
                'failed',
                $subState,
                $now,
                $now,
                0,
                0
            ));
        } catch (\Throwable $e) {
            // best-effort.
        }
    }
}
