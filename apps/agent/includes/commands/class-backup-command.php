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

        $runnerParams = [
            'snapshot_id'       => $snapshotId,
            'kind'              => $kind,
            'age_recipient'     => $recipient,
            'presign_endpoint'  => $presign,
            'manifest_endpoint' => $manifestEp,
            'progress_endpoint' => $progressEp,
            'chunk_bytes'       => $chunkBytes,
            'scratch_dir'       => $scratchDir,
            'wp_content_path'   => defined('WP_CONTENT_DIR') ? WP_CONTENT_DIR : '',
            'db'                => $this->dbCreds(),
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
     * Create wp-content/wpmgr-agent/runs/{snapshot_id}/ with restrictive
     * permissions. Returns absolute path. Idempotent — if the dir already
     * exists (a watchdog resume), returns it.
     */
    private function prepareScratchDir(string $snapshotId): string
    {
        if (!defined('WP_CONTENT_DIR')) {
            throw new \RuntimeException('WP_CONTENT_DIR is not defined');
        }
        $base = WP_CONTENT_DIR . '/wpmgr-agent/runs';
        if (!is_dir($base) && !mkdir($base, 0700, true) && !is_dir($base)) {
            throw new \RuntimeException('cannot create scratch base');
        }
        $dir = $base . '/' . $snapshotId;
        if (!is_dir($dir) && !mkdir($dir, 0700) && !is_dir($dir)) {
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
}
