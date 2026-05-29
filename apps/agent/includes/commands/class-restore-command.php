<?php
/**
 * Restore command: M5.6 / ADR-034 — WPvivid-pattern restore engine.
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
     * @return array{ok:bool,detail:string}
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

        $chunkDownloads = $this->parseChunkDownloads($params);
        if ($chunkDownloads === []) {
            return $this->refuse('no chunk_downloads / manifest entries supplied');
        }

        // --- 2. Dedup claim ------------------------------------------------
        Schema::ensureCurrent();
        if (!$this->tryClaimDedup($snapshotId, $restoreId)) {
            return $this->refuse('runner already in flight for this restore');
        }

        // --- 3. Prepare scratch dir + assemble runner params --------------
        try {
            $scratchDir = $this->prepareScratchDir($snapshotId, $restoreId);
        } catch (\Throwable $e) {
            $this->releaseDedup($snapshotId, $restoreId);
            return $this->refuse('scratch dir creation failed');
        }

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
     * @return list<array<string,mixed>>
     */
    private function parseChunkDownloads(array $params): array
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
            $norm = [];
            foreach ($chunks as $c) {
                if (!is_array($c)) {
                    continue;
                }
                $hash = (string) ($c['hash'] ?? $c['blake3'] ?? '');
                $url  = (string) ($c['presigned_url'] ?? $c['url'] ?? $c['get_url'] ?? '');
                if ($hash === '' || $url === '') {
                    continue;
                }
                $row = ['hash' => $hash, 'presigned_url' => $url];
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
        $table  = $wpdb->prefix . Schema::BACKUP_RESTORE_RUNS_TABLE;
        $now    = time();
        $cutoff = $now - self::DEDUP_WINDOW_SECONDS;

        // @phpstan-ignore-next-line
        $existing = $wpdb->get_row($wpdb->prepare(
            "SELECT pid, started_at FROM {$table} WHERE snapshot_id = %s AND restore_id = %s",
            $snapshotId,
            $restoreId
        ));
        if (is_object($existing) && (int) $existing->started_at > $cutoff) {
            return false;
        }
        if (is_object($existing)) {
            // @phpstan-ignore-next-line
            $wpdb->update(
                $table,
                ['pid' => getmypid() ?: 0, 'started_at' => $now],
                ['snapshot_id' => $snapshotId, 'restore_id' => $restoreId],
                ['%d', '%d'],
                ['%s', '%s']
            );
            return true;
        }
        // @phpstan-ignore-next-line
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
        $table = $wpdb->prefix . Schema::BACKUP_RESTORE_RUNS_TABLE;
        // @phpstan-ignore-next-line
        @$wpdb->delete($table, ['snapshot_id' => $snapshotId, 'restore_id' => $restoreId], ['%s', '%s']);
    }

    /**
     * Create wp-content/wpmgr-agent/restores/{snapshot_id}-{restore_id}/.
     * Idempotent — watchdog resume returns the same dir.
     */
    private function prepareScratchDir(string $snapshotId, string $restoreId): string
    {
        if (!defined('WP_CONTENT_DIR')) {
            throw new \RuntimeException('WP_CONTENT_DIR is not defined');
        }
        $base = WP_CONTENT_DIR . '/wpmgr-agent/restores';
        if (!is_dir($base) && !mkdir($base, 0700, true) && !is_dir($base)) {
            throw new \RuntimeException('cannot create restore scratch base');
        }
        // Short-id to keep the dir name reasonable.
        $clean = preg_replace('/[^a-f0-9]/i', '', $restoreId) ?? '';
        $short = substr($clean, 0, 12);
        $dir   = $base . '/' . $snapshotId . '-' . $short;
        if (!is_dir($dir) && !mkdir($dir, 0700) && !is_dir($dir)) {
            throw new \RuntimeException('cannot create restore scratch dir');
        }
        @chmod($dir, 0700);
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
        $table = $wpdb->prefix . Schema::BACKUP_RESTORE_TASKS_TABLE;
        $now   = time();
        $subState = (string) wp_json_encode(['params' => $runnerParams]);

        // @phpstan-ignore-next-line
        $wpdb->query($wpdb->prepare(
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
     * @return array{ok:bool,detail:string}
     */
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
