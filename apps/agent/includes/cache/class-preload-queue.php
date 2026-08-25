<?php
/**
 * PreloadQueue — WPMgr's custom-MySQL-table cache-warm queue.
 *
 * This is WPMgr's own self-dispatching queue: rows live in a dedicated table
 * (wpmgr_preload_queue), are claimed atomically with a per-claim lock token,
 * warmed via a same-host loopback fetch, and DELETED on success (there is no
 * `completed` status). A failed warm is parked for retry with exponential
 * backoff (a FUTURE locked_at timestamp), then marked permanently `failed` once
 * the retry budget is exhausted.
 *
 * Concurrency: dispatchRunners() computes the number of free runner slots
 * (capped at MAX_CONCURRENCY) and fires that many non-blocking loopback POSTs to
 * a dedicated REST runner route. Each runner drains within a bounded time window
 * (RUNNER_WINDOW_SECONDS) and self-chains a fresh runner when work remains. A
 * wp-cron watchdog re-kicks any queue whose loopback chain died.
 *
 * Auth on the runner route is a self-HMAC handshake (NOT the Ed25519 Connector
 * signing the CP uses): this endpoint is a fire-and-forget LOOPBACK kick from the
 * agent to itself, carries no command authority, and only drains an already-
 * queued, SSRF-filtered, same-host URL set. See registerPreloadRunRoute() and the
 * §1.10 security-review checklist in the spec.
 *
 * All timestamps are stored as UTC DATETIME and every comparison uses
 * UTC_TIMESTAMP(). All table I/O is owned here; the warmer (Preload) only enqueues
 * and exposes a single-URL warm method this queue's runner invokes.
 *
 * @package WPMgr\Agent\Cache
 */

declare(strict_types=1);

namespace WPMgr\Agent\Cache;

use WPMgr\Agent\Schema;

/**
 * Custom-table preload queue with atomic claim/lock, retry/backoff, and
 * concurrency-bounded loopback runners.
 */
final class PreloadQueue
{
    /** REST namespace for the loopback runner route. */
    public const REST_NAMESPACE = 'wpmgr/v1';

    /** Route path (full: /wp-json/wpmgr/v1/preload/run). */
    public const REST_RUN_ROUTE = '/preload/run';

    /** Default queue group (kept as a column for future multi-group reuse). */
    public const GROUP_NAME = 'preload-urls';

    /** Default callback name (the warm action this group dispatches). */
    public const CALLBACK = 'preload_url';

    /** Stale-lock timeout (sec): stale-requeue + active-runner window. */
    public const TASK_LOCK_TIMEOUT = 120;

    /** A single runner drains for at most this many seconds, then self-chains. */
    public const RUNNER_WINDOW_SECONDS = 20;

    /** Default max retries before a task is marked permanently `failed`. */
    public const DEFAULT_MAX_RETRIES = 3;

    /** Default concurrency (strictly serial — preserves today's behavior). */
    public const DEFAULT_CONCURRENCY = 1;

    /** Hard concurrency cap (loopback runner amplification bound). */
    public const MAX_CONCURRENCY = 4;

    /** Backoff base (sec); doubles per attempt. */
    public const RETRY_BACKOFF_BASE_SECONDS = 30;

    /** Backoff ceiling (sec). */
    public const RETRY_BACKOFF_MAX_SECONDS = 300;

    /** wp-cron hook the watchdog runs under. */
    public const WATCHDOG_HOOK = 'wpmgr_preload_queue_watchdog';

    /** Max stalled groups the watchdog re-kicks per tick. */
    public const WATCHDOG_BATCH = 10;

    /** Preload marker header (mirrors Preload::PRELOAD_HEADER). */
    public const PRELOAD_HEADER = Preload::PRELOAD_HEADER;

    /** Queue group for this instance. */
    private string $group;

    /** Callback name for this instance. */
    private string $callback;

    /** Max retries before permanent failure (clamped >= 0). */
    private int $maxRetries;

    /** Concurrency (clamped to 1..MAX_CONCURRENCY). */
    private int $concurrency;

    /** Inter-request warm delay in microseconds. */
    private int $delayUs;

    /** Informational batch size (cron run() slice; time-boxed runner ignores). */
    private int $batchSize;

    /** 1-min load-average-per-core ceiling (0 = disabled). */
    private float $maxLoadPerCore;

    /**
     * @param string $group          Queue group (default 'preload-urls').
     * @param string $callback       Callback name (default 'preload_url').
     * @param int    $maxRetries     Max retries before permanent failure.
     * @param int    $concurrency    Parallel loopback drain workers (1..4).
     * @param int    $delayUs        Inter-request warm delay in microseconds.
     * @param int    $batchSize      Informational cron-slice size.
     * @param float  $maxLoadPerCore 1-min load-per-core defer ceiling (0=off).
     */
    public function __construct(
        string $group = self::GROUP_NAME,
        string $callback = self::CALLBACK,
        int $maxRetries = self::DEFAULT_MAX_RETRIES,
        int $concurrency = self::DEFAULT_CONCURRENCY,
        int $delayUs = 500000,
        int $batchSize = 50,
        float $maxLoadPerCore = 0.0
    ) {
        $this->group          = $group !== '' ? $group : self::GROUP_NAME;
        $this->callback       = $callback !== '' ? $callback : self::CALLBACK;
        $this->maxRetries     = max(0, $maxRetries);
        $this->concurrency    = min(self::MAX_CONCURRENCY, max(1, $concurrency));
        $this->delayUs        = max(0, $delayUs);
        $this->batchSize      = max(1, $batchSize);
        $this->maxLoadPerCore = max(0.0, $maxLoadPerCore);
    }

    /**
     * Reconstruct a PreloadQueue from the persisted PerfConfig. The loopback
     * runner runs in a FRESH request where instance state does not survive, so
     * config — not constructor args — is the single source of truth for the
     * runner's throttle values.
     *
     * @return PreloadQueue
     */
    public static function fromConfig(): self
    {
        $concurrency    = self::DEFAULT_CONCURRENCY;
        $delayUs        = 500000;
        $batchSize      = 50;
        $maxLoadPerCore = 0.0;

        if (class_exists(\WPMgr\Agent\Optimizer\PerfConfig::class)) {
            $cfg            = \WPMgr\Agent\Optimizer\PerfConfig::load();
            $concurrency    = $cfg->preloadConcurrency;
            $delayUs        = $cfg->preloadDelayMs * 1000;
            $batchSize      = $cfg->preloadBatchSize;
            $maxLoadPerCore = $cfg->preloadMaxLoad;
        }

        // The existing operator filters remain the final override; their
        // effective DEFAULT is now the PerfConfig-derived value.
        if (function_exists('apply_filters')) {
            $delayUs        = (int) apply_filters('wpmgr_preload_delay_us', $delayUs);
            $maxLoadPerCore = (float) apply_filters('wpmgr_preload_max_load_per_core', $maxLoadPerCore);
            $batchSize      = (int) apply_filters('wpmgr_preload_batch_size', $batchSize);
        }

        return new self(
            self::GROUP_NAME,
            self::CALLBACK,
            self::DEFAULT_MAX_RETRIES,
            $concurrency,
            $delayUs,
            $batchSize,
            $maxLoadPerCore
        );
    }

    // -------------------------------------------------------------------------
    // Table lifecycle
    // -------------------------------------------------------------------------

    /**
     * Ensure the queue table exists. Thin pass-through to Schema::ensureCurrent()
     * (the real creation path); present for symmetry/tests. Never the primary
     * creation route — dbDelta runs on plugins_loaded / activation.
     *
     * @return void
     */
    public function createTable(): void
    {
        if (class_exists(Schema::class)) {
            Schema::ensureCurrent();
        }
    }

    // -------------------------------------------------------------------------
    // Enqueue
    // -------------------------------------------------------------------------

    // phpcs:disable WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching,WordPress.DB.DirectDatabaseQuery.SchemaChange,WordPress.DB.PreparedSQL.InterpolatedNotPrepared,WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter,WordPress.DB.PreparedSQL.NotPrepared -- direct queries on plugin-owned table (wpmgr_preload_queue); no core helper exists; $table is prefix+constant (trusted); correctness requires live reads (anti-replay/locking window)

    /**
     * Enqueue (or revive) a (url, device) warm task. Idempotent upsert keyed on
     * (group_name, task_hash): a re-queue of the same (url, device) is a no-op
     * that keeps the more-urgent priority and revives a `failed` row to `pending`.
     *
     * @param string $url      Absolute, same-host URL to warm.
     * @param string $device   'desktop' | 'mobile' (participates in the hash).
     * @param int    $priority Ascending-urgency (lower = warmed first).
     * @return int The row id (existing row id on conflict), or 0 on failure.
     */
    public function addTask(string $url, string $device = 'desktop', int $priority = 20): int
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return 0;
        }
        /** @var \wpdb $wpdb */
        $url = trim($url);
        if ($url === '') {
            return 0;
        }

        $device   = $this->normalizeDevice($device);
        $priority = max(0, $priority);
        $hash     = $this->taskHash($url, $device);
        $table    = $this->table();

        try {
            $wpdb->query( // phpcs:ignore PluginCheck.Security.DirectDB.UnescapedDBParameter -- value is the output of $wpdb->prepare(); not attacker-controlled
                $wpdb->prepare(
                    "INSERT INTO {$table}
                        (group_name, callback, url, device, task_hash, priority, status, attempts, created_at, updated_at)
                     VALUES
                        (%s, %s, %s, %s, %s, %d, 'pending', 0, UTC_TIMESTAMP(), UTC_TIMESTAMP())
                     ON DUPLICATE KEY UPDATE
                        id        = LAST_INSERT_ID(id),
                        priority  = LEAST(priority, VALUES(priority)),
                        status    = IF(status='failed','pending',status),
                        updated_at = UTC_TIMESTAMP()",
                    $this->group,
                    $this->callback,
                    $url,
                    $device,
                    $hash,
                    $priority
                )
            );
        } catch (\Throwable $e) {
            return 0;
        }

        return (int) $wpdb->insert_id;
    }

    // -------------------------------------------------------------------------
    // Claim / complete / fail
    // -------------------------------------------------------------------------

    /**
     * Atomically claim the next due task. Two UPDATEs + a SELECT, no transaction:
     *   STEP 1 — release locks held longer than TASK_LOCK_TIMEOUT (stale-requeue).
     *   STEP 2 — claim the highest-priority due row with a fresh lock token.
     * `attempts` is incremented AT claim time, so a task's first execution sees
     * attempts == 1.
     *
     * @return array<string,mixed>|null The claimed row, or null when none due.
     */
    public function claimNext(): ?array
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return null;
        }
        /** @var \wpdb $wpdb */
        $table = $this->table();

        try {
            // STEP 1 — stale-requeue.
            $wpdb->query( // phpcs:ignore PluginCheck.Security.DirectDB.UnescapedDBParameter -- value is the output of $wpdb->prepare(); not attacker-controlled
                $wpdb->prepare(
                    "UPDATE {$table}
                        SET status='pending', lock_token=NULL, locked_at=NULL, updated_at=UTC_TIMESTAMP()
                      WHERE status='processing'
                        AND locked_at < UTC_TIMESTAMP() - INTERVAL %d SECOND",
                    self::TASK_LOCK_TIMEOUT
                )
            );

            // STEP 2 — atomic claim of the highest-priority due row.
            $token = $this->newToken();
            $wpdb->query( // phpcs:ignore PluginCheck.Security.DirectDB.UnescapedDBParameter -- value is the output of $wpdb->prepare(); not attacker-controlled
                $wpdb->prepare(
                    "UPDATE {$table}
                        SET status='processing', lock_token=%s, locked_at=UTC_TIMESTAMP(),
                            attempts=attempts+1, updated_at=UTC_TIMESTAMP()
                      WHERE group_name=%s AND callback=%s
                        AND status='pending'
                        AND (locked_at IS NULL OR locked_at <= UTC_TIMESTAMP())
                      ORDER BY priority ASC, created_at ASC, id ASC
                      LIMIT 1",
                    $token,
                    $this->group,
                    $this->callback
                )
            );

            if ((int) $wpdb->rows_affected === 0) {
                return null;
            }

            $row = $wpdb->get_row( // phpcs:ignore PluginCheck.Security.DirectDB.UnescapedDBParameter -- value is the output of $wpdb->prepare(); not attacker-controlled
                $wpdb->prepare("SELECT * FROM {$table} WHERE lock_token=%s LIMIT 1", $token),
                ARRAY_A
            );
        } catch (\Throwable $e) {
            return null;
        }

        return is_array($row) ? $row : null;
    }

    /**
     * Success path: throttle, then DELETE the row. The lock_token guard prevents
     * deleting a row another runner re-claimed after a stale-requeue.
     *
     * @param array<string,mixed> $task The claimed row.
     * @return void
     */
    public function complete(array $task): void
    {
        global $wpdb;
        if ($this->delayUs > 0) {
            usleep($this->delayUs);
        }
        if (!is_object($wpdb)) {
            return;
        }
        $id    = (int) ($task['id'] ?? 0);
        $token = (string) ($task['lock_token'] ?? '');
        if ($id <= 0 || $token === '') {
            return;
        }
        $table = $this->table();
        try {
            $wpdb->query( // phpcs:ignore PluginCheck.Security.DirectDB.UnescapedDBParameter -- value is the output of $wpdb->prepare(); not attacker-controlled
                $wpdb->prepare(
                    "DELETE FROM {$table} WHERE id=%d AND lock_token=%s",
                    $id,
                    $token
                )
            );
        } catch (\Throwable $e) {
            // Best-effort; a stale row is swept by the stale-requeue path.
        }
    }

    /**
     * Failure path: PARK for retry with a future locked_at (exponential backoff)
     * while attempts < maxRetries, else mark permanently `failed`. Then throttle.
     *
     * @param array<string,mixed> $task  The claimed row.
     * @param string              $error Short error text (truncated to 1000).
     * @return void
     */
    public function fail(array $task, string $error): void
    {
        global $wpdb;
        $id       = (int) ($task['id'] ?? 0);
        $token    = (string) ($task['lock_token'] ?? '');
        $attempts = (int) ($task['attempts'] ?? 0);
        $err      = substr($error, 0, 1000);
        $table    = $this->table();

        if (is_object($wpdb) && $id > 0 && $token !== '') {
            try {
                if ($attempts < $this->maxRetries) {
                    // PARK for retry: a FUTURE locked_at excludes this row from
                    // claimNext() until the backoff window elapses.
                    $backoff = $this->retryBackoffSeconds($attempts);
                    $wpdb->query( // phpcs:ignore PluginCheck.Security.DirectDB.UnescapedDBParameter -- value is the output of $wpdb->prepare(); not attacker-controlled
                        $wpdb->prepare(
                            "UPDATE {$table}
                                SET status='pending', lock_token=NULL,
                                    locked_at = UTC_TIMESTAMP() + INTERVAL %d SECOND,
                                    last_error=%s, updated_at=UTC_TIMESTAMP()
                              WHERE id=%d AND lock_token=%s",
                            $backoff,
                            $err,
                            $id,
                            $token
                        )
                    );
                } else {
                    $wpdb->query( // phpcs:ignore PluginCheck.Security.DirectDB.UnescapedDBParameter -- value is the output of $wpdb->prepare(); not attacker-controlled
                        $wpdb->prepare(
                            "UPDATE {$table}
                                SET status='failed', lock_token=NULL, locked_at=NULL,
                                    last_error=%s, updated_at=UTC_TIMESTAMP()
                              WHERE id=%d AND lock_token=%s",
                            $err,
                            $id,
                            $token
                        )
                    );
                }
            } catch (\Throwable $e) {
                // Best-effort.
            }
        }

        if ($this->delayUs > 0) {
            usleep($this->delayUs);
        }
    }

    /**
     * Exponential backoff for the given (claim-incremented) attempt count.
     * attempts=1 -> 30s, attempts=2 -> 60s, capped at RETRY_BACKOFF_MAX_SECONDS.
     *
     * @param int $attempts The (already-incremented) attempt count.
     * @return int Backoff seconds.
     */
    public function retryBackoffSeconds(int $attempts): int
    {
        $exp = max(0, $attempts - 1);
        $val = self::RETRY_BACKOFF_BASE_SECONDS * (2 ** $exp);
        return (int) min(self::RETRY_BACKOFF_MAX_SECONDS, max(1, (int) round($val)));
    }

    // -------------------------------------------------------------------------
    // Counters (dashboard)
    // -------------------------------------------------------------------------

    /**
     * Pending + processing tally (the dashboard denominator).
     *
     * @return int
     */
    public function pendingCount(): int
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return 0;
        }
        /** @var \wpdb $wpdb */
        $table = $this->table();
        try {
            return (int) $wpdb->get_var( // phpcs:ignore PluginCheck.Security.DirectDB.UnescapedDBParameter -- value is the output of $wpdb->prepare(); not attacker-controlled
                $wpdb->prepare(
                    "SELECT COUNT(*) FROM {$table}
                      WHERE group_name=%s AND callback=%s
                        AND status IN ('pending','processing')",
                    $this->group,
                    $this->callback
                )
            );
        } catch (\Throwable $e) {
            return 0;
        }
    }

    /**
     * Permanently-failed row tally.
     *
     * @return int
     */
    public function failedCount(): int
    {
        return $this->countByStatus('failed');
    }

    /**
     * In-flight (processing) row tally.
     *
     * @return int
     */
    public function processingCount(): int
    {
        return $this->countByStatus('processing');
    }

    /**
     * Count rows for this group+callback in a single status.
     *
     * @param string $status Status value.
     * @return int
     */
    private function countByStatus(string $status): int
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return 0;
        }
        /** @var \wpdb $wpdb */
        $table = $this->table();
        try {
            return (int) $wpdb->get_var( // phpcs:ignore PluginCheck.Security.DirectDB.UnescapedDBParameter -- value is the output of $wpdb->prepare(); not attacker-controlled
                $wpdb->prepare(
                    "SELECT COUNT(*) FROM {$table}
                      WHERE group_name=%s AND callback=%s AND status=%s",
                    $this->group,
                    $this->callback,
                    $status
                )
            );
        } catch (\Throwable $e) {
            return 0;
        }
    }

    /**
     * Fetch a bounded page of rows for the dashboard viewer (newest first).
     *
     * @param int $limit Max rows (clamped 1..200).
     * @return list<array<string,mixed>>
     */
    public function listRows(int $limit = 50): array
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return [];
        }
        /** @var \wpdb $wpdb */
        $limit = min(200, max(1, $limit));
        $table = $this->table();
        try {
            $rows = $wpdb->get_results( // phpcs:ignore PluginCheck.Security.DirectDB.UnescapedDBParameter -- value is the output of $wpdb->prepare(); not attacker-controlled
                $wpdb->prepare(
                    "SELECT id, url, device, priority, status, attempts, last_error, created_at, locked_at, updated_at
                       FROM {$table}
                      WHERE group_name=%s AND callback=%s
                      ORDER BY id DESC
                      LIMIT %d",
                    $this->group,
                    $this->callback,
                    $limit
                ),
                ARRAY_A
            );
        } catch (\Throwable $e) {
            return [];
        }
        return is_array($rows) ? array_values($rows) : [];
    }

    /**
     * Revive every permanently-`failed` row to `pending` (clears locked_at so the
     * row is immediately due). Returns rows revived.
     *
     * @return int
     */
    public function retryFailed(): int
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return 0;
        }
        /** @var \wpdb $wpdb */
        $table = $this->table();
        try {
            $wpdb->query( // phpcs:ignore PluginCheck.Security.DirectDB.UnescapedDBParameter -- value is the output of $wpdb->prepare(); not attacker-controlled
                $wpdb->prepare(
                    "UPDATE {$table}
                        SET status='pending', lock_token=NULL, locked_at=NULL,
                            updated_at=UTC_TIMESTAMP()
                      WHERE group_name=%s AND callback=%s AND status='failed'",
                    $this->group,
                    $this->callback
                )
            );
        } catch (\Throwable $e) {
            return 0;
        }
        return (int) $wpdb->rows_affected;
    }

    /**
     * Delete every row for this group+callback. Returns rows deleted.
     *
     * @return int
     */
    public function clearQueue(): int
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return 0;
        }
        /** @var \wpdb $wpdb */
        $table = $this->table();
        try {
            $wpdb->query( // phpcs:ignore PluginCheck.Security.DirectDB.UnescapedDBParameter -- value is the output of $wpdb->prepare(); not attacker-controlled
                $wpdb->prepare(
                    "DELETE FROM {$table} WHERE group_name=%s AND callback=%s",
                    $this->group,
                    $this->callback
                )
            );
        } catch (\Throwable $e) {
            return 0;
        }
        return (int) $wpdb->rows_affected;
    }

    // -------------------------------------------------------------------------
    // Dispatch (concurrency)
    // -------------------------------------------------------------------------

    /**
     * Fire-and-forget loopback dispatch up to the number of free runner slots.
     * Never spawns more runners than free slots or claimable rows, so a flood of
     * dispatch calls cannot fork unbounded runners (amplification bound).
     *
     * @return void
     */
    public function dispatchRunners(): void
    {
        $active = $this->activeRunnerCount();
        $availableSlots = $this->concurrency - $active;
        if ($availableSlots <= 0) {
            return;
        }
        $claimable = $this->claimablePendingCount();
        if ($claimable <= 0) {
            return;
        }
        $dispatchCount = min($availableSlots, $claimable);
        for ($i = 0; $i < $dispatchCount; $i++) {
            $this->dispatchOne();
        }
    }

    /**
     * Count in-flight runners (rows locked within the active-runner window).
     *
     * @return int
     */
    private function activeRunnerCount(): int
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return 0;
        }
        /** @var \wpdb $wpdb */
        $table = $this->table();
        try {
            return (int) $wpdb->get_var( // phpcs:ignore PluginCheck.Security.DirectDB.UnescapedDBParameter -- value is the output of $wpdb->prepare(); not attacker-controlled
                $wpdb->prepare(
                    "SELECT COUNT(*) FROM {$table}
                      WHERE group_name=%s AND callback=%s
                        AND status='processing'
                        AND locked_at >= UTC_TIMESTAMP() - INTERVAL %d SECOND",
                    $this->group,
                    $this->callback,
                    self::TASK_LOCK_TIMEOUT
                )
            );
        } catch (\Throwable $e) {
            return 0;
        }
    }

    /**
     * Count rows that are immediately claimable (pending + due).
     *
     * @return int
     */
    private function claimablePendingCount(): int
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return 0;
        }
        /** @var \wpdb $wpdb */
        $table = $this->table();
        try {
            return (int) $wpdb->get_var( // phpcs:ignore PluginCheck.Security.DirectDB.UnescapedDBParameter -- value is the output of $wpdb->prepare(); not attacker-controlled
                $wpdb->prepare(
                    "SELECT COUNT(*) FROM {$table}
                      WHERE group_name=%s AND callback=%s
                        AND status='pending'
                        AND (locked_at IS NULL OR locked_at <= UTC_TIMESTAMP())",
                    $this->group,
                    $this->callback
                )
            );
        } catch (\Throwable $e) {
            return 0;
        }
    }

    /**
     * Whether ANY work remains: an in-flight runner, or a claimable pending row.
     *
     * @return bool
     */
    private function hasMore(): bool
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return false;
        }
        /** @var \wpdb $wpdb */
        $table = $this->table();
        try {
            $count = (int) $wpdb->get_var( // phpcs:ignore PluginCheck.Security.DirectDB.UnescapedDBParameter -- value is the output of $wpdb->prepare(); not attacker-controlled
                $wpdb->prepare(
                    "SELECT COUNT(*) FROM {$table}
                      WHERE group_name=%s AND callback=%s
                        AND ( status='processing'
                              OR ( status='pending'
                                   AND (locked_at IS NULL OR locked_at <= UTC_TIMESTAMP()) ) )",
                    $this->group,
                    $this->callback
                )
            );
        } catch (\Throwable $e) {
            return false;
        }
        return $count > 0;
    }

    /**
     * Fire a single non-blocking loopback POST to the runner route. The request
     * never blocks the caller (timeout 0.01, blocking false). sslverify is false
     * because this is a strictly same-host loopback to our own REST endpoint — the
     * URL is derived from home_url() (never request input) and the request never
     * follows redirects off-host (redirection 0).
     *
     * @return void
     */
    private function dispatchOne(): void
    {
        if (!function_exists('wp_remote_post') || !function_exists('rest_url')) {
            return;
        }
        $url = (string) rest_url(ltrim(self::REST_NAMESPACE . self::REST_RUN_ROUTE, '/'));
        if ($url === '') {
            return;
        }
        $body = function_exists('wp_json_encode')
            ? (string) wp_json_encode([
                'group'    => $this->group,
                'callback' => $this->callback,
                'token'    => $this->buildRunnerToken($this->group, $this->callback),
            ])
            : '';

        wp_remote_post($url, [
            'timeout'           => 0.01,
            'blocking'          => false,
            'redirection'       => 0,
            'sslverify'         => false,
            'reject_unsafe_urls' => true,
            'headers'           => [
                'Content-Type' => 'application/json',
                self::PRELOAD_HEADER => '1',
            ],
            'body'              => $body,
        ]);
    }

    // -------------------------------------------------------------------------
    // Runner (time-boxed REST entry) + self-HMAC auth
    // -------------------------------------------------------------------------

    /**
     * Build the self-HMAC runner token. Derived from a per-site secret
     * (wp_salt('auth')) — never logged, never derivable off-site.
     *
     * @param string $group    Queue group.
     * @param string $callback Callback name.
     * @return string Hex HMAC token.
     */
    public function buildRunnerToken(string $group, string $callback): string
    {
        $secret = function_exists('wp_salt') ? (string) wp_salt('auth') : '';
        return hash_hmac('sha256', $group . '|' . $callback, $secret);
    }

    /**
     * Timing-safe verify of a runner token.
     *
     * @param string $group    Queue group.
     * @param string $callback Callback name.
     * @param string $token    Presented token.
     * @return bool
     */
    public function verifyRunnerToken(string $group, string $callback, string $token): bool
    {
        if ($group === '' || $callback === '' || $token === '') {
            return false;
        }
        return hash_equals($this->buildRunnerToken($group, $callback), $token);
    }

    /**
     * REST callback for POST /wpmgr/v1/preload/run. Verifies the self-HMAC, then
     * runs a time-boxed claim/process loop and self-chains a fresh runner when
     * work remains. The permission_callback verifies the same self-HMAC
     * (verifyRunnerToken); this handler re-verifies as the authoritative gate.
     *
     * @param \WP_REST_Request<array<string,mixed>> $request Incoming request.
     * @return \WP_REST_Response|\WP_Error
     */
    public function runFromRest(\WP_REST_Request $request)
    {
        $group    = $this->sanitize((string) $request->get_param('group'));
        $callback = $this->sanitize((string) $request->get_param('callback'));
        $token    = $this->sanitize((string) $request->get_param('token'));

        if (!$this->verifyRunnerToken($group, $callback, $token)) {
            return new \WP_Error(
                'wpmgr/invalid-queue-runner',
                'Forbidden.',
                ['status' => 403]
            );
        }

        // Load-gate: defer (do not warm) while the server is overloaded.
        if ($this->isServerOverloaded()) {
            return new \WP_REST_Response(['success' => true, 'processed' => 0, 'has_more' => true], 200);
        }

        $preload   = new Preload(false); // device is decided per-row, not by ctor.
        $started   = microtime(true);
        $processed = 0;

        while ((microtime(true) - $started) < self::RUNNER_WINDOW_SECONDS) {
            $task = $this->claimNext();
            if ($task === null) {
                break;
            }
            $device = $this->normalizeDevice((string) ($task['device'] ?? 'desktop'));
            $url    = (string) ($task['url'] ?? '');
            try {
                $preload->warmOne($url, $device);
                $this->complete($task);
            } catch (\Throwable $e) {
                $this->fail($task, $e->getMessage());
            }
            $processed++;
        }

        $hasMore = $this->hasMore();

        // SELF-CHAIN — a fresh loopback runner picks up where the window ended.
        if ($hasMore) {
            $this->dispatchRunners();
        }

        // Keep the dashboard SSE bar moving across chained runners.
        $this->reportStats();

        return new \WP_REST_Response([
            'success'   => true,
            'processed' => $processed,
            'has_more'  => $hasMore,
        ], 200);
    }

    /**
     * Push a stats frame so the dashboard's live preload bar advances across
     * chained runners. Fire-and-forget — never breaks the runner.
     *
     * @return void
     */
    private function reportStats(): void
    {
        try {
            $reporter = (new CacheManager())->makePerfReporter();
            if ($reporter === null) {
                return;
            }
            $pending = $this->pendingCount();
            $total   = (int) (function_exists('get_option')
                ? get_option(PerfReporter::OPTION_PRELOAD_TOTAL, $pending)
                : $pending);
            $lastAt  = null;
            if ($pending === 0 && $total > 0) {
                PerfReporter::persistLastPreloadAt(time());
                $lastAt = time();
            }
            $reporter->reportStats($pending, $total, $lastAt);
        } catch (\Throwable $e) {
            // Fire-and-forget.
        }
    }

    // -------------------------------------------------------------------------
    // Watchdog (wp-cron)
    // -------------------------------------------------------------------------

    /**
     * Re-kick any queue whose loopback chain died (the non-blocking POST was
     * dropped). Selects stalled groups (no live runner, but has work or a stale
     * lock) and dispatches a fresh runner for each.
     *
     * @return void
     */
    public function runWatchdog(): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        /** @var \wpdb $wpdb */
        $table = $this->table();

        // Table-missing guard: a fresh install before dbDelta ran.
        try {
            $exists = $wpdb->get_var(
                $wpdb->prepare('SHOW TABLES LIKE %s', $table)
            );
            if ($exists !== $table) {
                return;
            }

            $stalled = $wpdb->get_results( // phpcs:ignore PluginCheck.Security.DirectDB.UnescapedDBParameter -- value is the output of $wpdb->prepare(); not attacker-controlled
                $wpdb->prepare(
                    "SELECT group_name, callback
                       FROM {$table}
                      GROUP BY group_name, callback
                     HAVING SUM(status='processing' AND locked_at >= UTC_TIMESTAMP() - INTERVAL %d SECOND) = 0
                        AND ( SUM(status='pending') > 0
                              OR SUM(status='processing' AND locked_at < UTC_TIMESTAMP() - INTERVAL %d SECOND) > 0 )
                      LIMIT %d",
                    self::TASK_LOCK_TIMEOUT,
                    self::TASK_LOCK_TIMEOUT,
                    self::WATCHDOG_BATCH
                ),
                ARRAY_A
            );
        } catch (\Throwable $e) {
            return;
        }

        if (!is_array($stalled)) {
            return;
        }

        foreach ($stalled as $row) {
            $group    = (string) ($row['group_name'] ?? '');
            $callback = (string) ($row['callback'] ?? '');
            if ($group === '' || $callback === '') {
                continue;
            }
            // Re-kick this specific group via a fresh queue bound to its
            // group/callback but sharing this instance's throttle config.
            $queue = new self(
                $group,
                $callback,
                $this->maxRetries,
                $this->concurrency,
                $this->delayUs,
                $this->batchSize,
                $this->maxLoadPerCore
            );
            $queue->dispatchRunners();
        }
    }

    // phpcs:enable WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching,WordPress.DB.DirectDatabaseQuery.SchemaChange,WordPress.DB.PreparedSQL.InterpolatedNotPrepared,WordPress.DB.DirectDatabaseQuery.UnescapedDBParameter,WordPress.DB.PreparedSQL.NotPrepared

    // -------------------------------------------------------------------------
    // Load gate
    // -------------------------------------------------------------------------

    /**
     * Whether the server's 1-min load average PER CORE exceeds the configured
     * ceiling. Returns false (never defer) when load cannot be sampled or the
     * ceiling is disabled (<= 0). The ceiling comes from the injected
     * maxLoadPerCore (PerfConfig.preload_max_load).
     *
     * @return bool
     */
    public function isServerOverloaded(): bool
    {
        if (!function_exists('sys_getloadavg')) {
            return false;
        }
        if ($this->maxLoadPerCore <= 0) {
            return false;
        }
        $load = @sys_getloadavg();
        if (!is_array($load) || !isset($load[0]) || !is_numeric($load[0])) {
            return false;
        }
        $cores   = $this->cpuCores();
        $perCore = $cores > 0 ? ((float) $load[0] / $cores) : (float) $load[0];
        return $perCore > $this->maxLoadPerCore;
    }

    /**
     * Best-effort CPU core count for the load-per-core calc. Tunable via
     * `wpmgr_preload_cpu_cores`; falls back to parsing /proc/cpuinfo, then 1.
     *
     * @return int
     */
    private function cpuCores(): int
    {
        $cores = function_exists('apply_filters')
            ? (int) apply_filters('wpmgr_preload_cpu_cores', 0)
            : 0;
        if ($cores > 0) {
            return $cores;
        }
        if (@is_readable('/proc/cpuinfo')) {
            $info = @file_get_contents('/proc/cpuinfo');
            if (is_string($info)) {
                $n = preg_match_all('/^processor\s*:/mi', $info);
                if (is_int($n) && $n > 0) {
                    return $n;
                }
            }
        }
        return 1;
    }

    // -------------------------------------------------------------------------
    // Identity / normalization helpers
    // -------------------------------------------------------------------------

    /**
     * The fully-qualified table name.
     *
     * @return string
     */
    private function table(): string
    {
        global $wpdb;
        $prefix = (is_object($wpdb) && isset($wpdb->prefix)) ? (string) $wpdb->prefix : 'wp_';
        return $prefix . Schema::PRELOAD_QUEUE_TABLE;
    }

    /**
     * Generate a 32-char alnum claim/lock token.
     *
     * @return string
     */
    private function newToken(): string
    {
        if (function_exists('wp_generate_password')) {
            return (string) wp_generate_password(32, false, false);
        }
        // Fallback for non-WP test contexts.
        return substr(bin2hex(random_bytes(16)), 0, 32);
    }

    /**
     * Normalize a device label to the known set.
     *
     * @param string $device Candidate device.
     * @return string 'desktop' | 'mobile'.
     */
    private function normalizeDevice(string $device): string
    {
        $d = strtolower(trim($device));
        return $d === 'mobile' ? 'mobile' : 'desktop';
    }

    /**
     * The sha256 dedup key over the canonical (url, device) payload. Because
     * `device` participates in the hash, (url, desktop) and (url, mobile) never
     * collide.
     *
     * @param string $url    Absolute URL.
     * @param string $device Device label.
     * @return string 64-char sha256 hex.
     */
    private function taskHash(string $url, string $device): string
    {
        $canonical = $this->canonical(['url' => $url, 'device' => $this->normalizeDevice($device)]);
        $encoded   = function_exists('wp_json_encode')
            ? (string) wp_json_encode($canonical)
            : (string) json_encode($canonical);
        return hash('sha256', $this->group . '|' . $encoded);
    }

    /**
     * Recursively ksort every associative array so key order never changes the
     * hash.
     *
     * @param mixed $value Candidate payload.
     * @return mixed Canonicalized payload.
     */
    private function canonical($value)
    {
        if (is_array($value)) {
            $out = [];
            foreach ($value as $k => $v) {
                $out[$k] = $this->canonical($v);
            }
            // Only ksort associative arrays (list arrays keep positional order).
            if ($out !== [] && array_keys($out) !== range(0, count($out) - 1)) {
                ksort($out);
            }
            return $out;
        }
        return $value;
    }

    /**
     * Sanitize a runner-route body field.
     *
     * @param string $value Raw field value.
     * @return string
     */
    private function sanitize(string $value): string
    {
        if (function_exists('sanitize_text_field')) {
            return (string) sanitize_text_field($value);
        }
        return trim($value);
    }
}
