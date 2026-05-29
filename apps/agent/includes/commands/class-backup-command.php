<?php
/**
 * Backup command: M5.6 / ADR-033 — WPvivid-pattern in-process state machine.
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
 *   5. `fastcgi_finish_request()` to release the HTTP response to the CP in
 *      well under a second.
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

/**
 * Accepts a `backup` command from the CP, seeds the task row, and drives
 * the WPvivid-pattern state machine. The HTTP response is released early via
 * `fastcgi_finish_request()` so the CP sees the ACK in well under a second
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

    public function __construct(AgeIdentity $identity)
    {
        $this->identity = $identity;
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
     * @return array{ok:bool,detail:string}
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
            return $this->refuse('runner already in flight for this snapshot');
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

        // --- 6. Release the HTTP response, then continue working ----------
        //
        // Pattern: register a shutdown function that flushes the response and
        // does the heavy work AFTER PHP has sent the body. Why this matters:
        //
        //   - WordPress REST framework runs my execute() inside a nested
        //     output buffer. Anything I echo here goes into WP's buffer,
        //     NOT directly to FPM. Calling fastcgi_finish_request()
        //     immediately + exit() leaves the buffer unflushed — the
        //     client sees no body, openresty waits for upstream, fires a
        //     60s 504 Gateway Timeout, the CP retries, the agent's dedup
        //     correctly refuses the retry, snapshot marked failed.
        //     (Exactly what we saw on the first files-backup attempt:
        //     27f20756-…, 1.5 GB archived but snapshot=failed because the
        //     CP never got the ACK.)
        //
        //   - With register_shutdown_function: I return the ACK normally,
        //     WP REST builds the WP_REST_Response, WP closes all the
        //     output buffers cleanly, the response goes to FPM, FPM
        //     responds to nginx/openresty/Cloudflare, the client sees a
        //     fast 200. Only THEN PHP runs shutdown handlers. Inside the
        //     handler we call fastcgi_finish_request() (defensive — most
        //     of the close already happened) and then TaskRunner.
        //
        //   - This matches WPvivid's pattern (40M+ installs of evidence
        //     that it works on every FPM-based WP host).
        //
        // On non-FPM SAPIs (mod_php, cli-server) the shutdown function
        // still fires but fastcgi_finish_request doesn't exist; the work
        // runs synchronously and the CP's 10-min HTTPTimeout accommodates.
        // --- 6. Decouple the work into a SEPARATE FPM request ----------
        //
        // We learned the hard way (v0.7.4-dev + v0.7.5-dev) that
        // `register_shutdown_function` + `fastcgi_finish_request` does NOT
        // reliably release the FCGI response on 1panel's openresty config.
        // The script keeps the FCGI connection alive while TaskRunner runs,
        // openresty's 60 s upstream-timeout fires, the CP sees 504 (or
        // HTTP/2 INTERNAL_ERROR over Cloudflare), River retries, the agent
        // dedup refuses the retry → snapshot marked failed even though the
        // runner is happily archiving in the background.
        //
        // The bulletproof fix (also what WPvivid uses): hand the work off
        // to a SEPARATE FPM request entirely.
        //
        //   1. Schedule the cron event for now (`time()`) bound to
        //      'wpmgr_backup_run' with the snapshot_id as the sole arg.
        //   2. Call `spawn_cron()` — WordPress's built-in loopback that
        //      makes a non-blocking wp_remote_post to /wp-cron.php on this
        //      same site. The loopback IS the trigger; it returns
        //      immediately without waiting for cron to actually run.
        //   3. Return ACK. The REST request exits cleanly in ms — no
        //      pending shutdown work, no buffer drama, no upstream timeout.
        //   4. /wp-cron.php fires in a fresh FPM worker, picks up our
        //      scheduled event, calls the 'wpmgr_backup_run' handler,
        //      which dispatches TaskRunner. THIS worker can run for
        //      minutes without affecting the original REST request.
        //
        // The watchdog (`wpmgr_backup_watchdog`, scheduled at +120s
        // above) remains the recovery net if the cron worker also dies
        // mid-run — it re-enters from `sub_state` via `TaskRunner::run`.
        if (function_exists('wp_schedule_single_event')) {
            wp_schedule_single_event(time(), 'wpmgr_backup_run', [$snapshotId]);
        }
        // spawn_cron lives in wp-includes/cron.php; available wherever wp-load
        // has run (which is always, in a REST request).
        if (function_exists('spawn_cron')) {
            @spawn_cron();
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
        $existing = $wpdb->get_row($wpdb->prepare("SELECT pid, started_at FROM {$table} WHERE snapshot_id = %s", $snapshotId));
        if (is_object($existing) && (int) $existing->started_at > $cutoff) {
            return false;
        }
        if (is_object($existing)) {
            // @phpstan-ignore-next-line
            $wpdb->update($table, ['pid' => getmypid() ?: 0, 'started_at' => $now], ['snapshot_id' => $snapshotId], ['%d', '%d'], ['%s']);
            return true;
        }
        // @phpstan-ignore-next-line
        $inserted = $wpdb->insert($table, ['snapshot_id' => $snapshotId, 'pid' => getmypid() ?: 0, 'started_at' => $now], ['%s', '%d', '%d']);
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
        @$wpdb->delete($table, ['snapshot_id' => $snapshotId], ['%s']);
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
            throw new \RuntimeException('cannot create backup scratch base: ' . $base);
        }
        $dir = $base . DIRECTORY_SEPARATOR . $snapshotId;
        if (!is_dir($dir) && !@mkdir($dir, 0700) && !is_dir($dir)) {
            throw new \RuntimeException('cannot create scratch dir');
        }
        @chmod($dir, 0700);
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
        $wpdb->query($wpdb->prepare(
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

    /** Refusal response — the agent didn't accept the job. */
    private function refuse(string $detail): array
    {
        return ['ok' => false, 'detail' => $detail];
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
     * WPvivid + WPRemote operators see in the wild:
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
                $probe = $wpdb->get_var('SELECT 1');
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
        // packet limit is tight. WPvivid's documented floor for managed-host
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
            if (!is_dir($scratchBase) && is_dir($parent) && !is_writable($parent)) {
                $failures[] = 'scratch base parent not writable: ' . $parent;
            } elseif (is_dir($scratchBase) && !is_writable($scratchBase)) {
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
            $value = $wpdb->get_var('SELECT @@max_allowed_packet');
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
                $rows = $wpdb->get_results('SHOW TABLE STATUS', ARRAY_A);
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
            $wpdb->query($wpdb->prepare(
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
