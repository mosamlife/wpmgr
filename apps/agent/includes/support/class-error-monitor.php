<?php
/**
 * ErrorMonitor (ADR-037 Sprint 2): PHP-error capture, dedup, rate-limit, ship.
 *
 * What it does:
 *   - Installs `set_error_handler(...)` covering E_WARNING|E_NOTICE|E_DEPRECATED.
 *   - Installs `register_shutdown_function(...)` to capture FATAL errors
 *     (`error_get_last()` only surfaces these at shutdown).
 *   - Records every captured error into `wpmgr_php_errors` with the dedup key
 *     `md5(code:file:line:message)`. Existing fingerprint → INCREMENT
 *     `occurrence_count` + bump `last_seen`. New fingerprint → INSERT.
 *
 * Rate limit:
 *   - Hard cap = 10 000 rows. On cap, evict OLDEST by `last_seen ASC`.
 *
 * Ship cadence:
 *   - Heartbeat (every 5 min, runs in Scheduler::runHeartbeat) calls
 *     `ErrorMonitor::shipBatch()` to upload up to 50 NEWEST unsilenced rows
 *     since the `since_id` cursor stored in wp-options. CP dedupes server-side
 *     on `(site_id, md5)`.
 *
 * Hand-off to the mu-plugin loader:
 *   - The mu-plugin (`a-wpmgr-error-trap.php`) is installed into
 *     `wp-content/mu-plugins/` by `MuPluginInstaller` on plugin activation. It
 *     runs at priority `-PHP_INT_MAX` so it traps errors that occur DURING the
 *     bootstrap of OTHER plugins (the exact failure mode WPRemote's
 *     `bv_php_error_store` is designed to catch). The mu-plugin sets up a tiny
 *     in-memory queue; this class drains the queue when the agent boots.
 *
 * Memory budget:
 *   - One INSERT per unique error, one UPDATE per repeat — no logging through
 *     the WP query cache. Backtrace storage is OPTIONAL and gzipped via
 *     `gzcompress` so a single recurring fatal does not blow out InnoDB row
 *     size.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

/**
 * Records PHP errors into the wpmgr_php_errors table with md5 dedup, rate
 * limit, and a heartbeat-driven ship cadence.
 */
final class ErrorMonitor
{
    /** Unprefixed table name for php-error capture. */
    public const TABLE = 'wpmgr_php_errors';

    /** Hard row cap; evicts oldest by last_seen on overflow. */
    public const ROW_CAP = 10000;

    /** Heartbeat-batch size (newest unsilenced rows). */
    public const SHIP_BATCH = 50;

    /** wp-options key for the ship-cursor (highest id confirmed by CP). */
    public const OPTION_SHIP_CURSOR = 'wpmgr_agent_errors_ship_cursor';

    /** Global flag the mu-plugin sets to indicate it has trapped errors. */
    public const GLOBAL_PENDING = 'wpmgr_agent_pending_errors';

    /**
     * Install the error + shutdown handlers. Idempotent — repeated calls are
     * safe (set_error_handler stacks; we keep a static flag so we install at
     * most once per request).
     *
     * @return void
     */
    public function install(): void
    {
        static $installed = false;
        if ($installed) {
            return;
        }
        $installed = true;

        // Drain any errors the mu-plugin captured before plugins_loaded.
        $this->drainBootstrapQueue();

        // Cover NOTICE/WARNING/DEPRECATED. Fatals are caught by the shutdown
        // function below (error_get_last is the only way to see them).
        set_error_handler([$this, 'handleError'], E_WARNING | E_NOTICE | E_USER_WARNING | E_USER_NOTICE | E_DEPRECATED | E_USER_DEPRECATED);
        register_shutdown_function([$this, 'handleShutdown']);
    }

    /**
     * Pull whatever the mu-plugin trapped (queued in a global the loader sets
     * up) and record each entry. The mu-plugin loader can be loaded long
     * before the main plugin's autoloader is ready, so it can only queue —
     * not record. This drain is the bridge between the two.
     *
     * @return void
     */
    public function drainBootstrapQueue(): void
    {
        if (!isset($GLOBALS[self::GLOBAL_PENDING]) || !is_array($GLOBALS[self::GLOBAL_PENDING])) {
            return;
        }
        $queue = $GLOBALS[self::GLOBAL_PENDING];
        $GLOBALS[self::GLOBAL_PENDING] = [];
        foreach ($queue as $entry) {
            if (!is_array($entry)) {
                continue;
            }
            $this->record(
                (int) ($entry['code'] ?? E_WARNING),
                (string) ($entry['message'] ?? ''),
                (string) ($entry['file'] ?? ''),
                (int) ($entry['line'] ?? 0),
                'bootstrap'
            );
        }
    }

    /**
     * set_error_handler() callback signature.
     *
     * Return value: false propagates to the next handler (PHP's default).
     * We always return false so we add to but don't suppress PHP's normal
     * error reporting.
     *
     * @param int $code
     * @param string $message
     * @param string $file
     * @param int $line
     * @return bool false → let PHP continue normal error handling.
     */
    public function handleError(int $code, string $message, string $file = '', int $line = 0): bool
    {
        $this->record($code, $message, $file, $line, $this->severityForCode($code));
        return false;
    }

    /**
     * register_shutdown_function() callback. Captures fatals.
     *
     * @return void
     */
    public function handleShutdown(): void
    {
        $err = error_get_last();
        if (!is_array($err)) {
            return;
        }
        $code = (int) ($err['type'] ?? 0);
        // Only act on fatal-class errors. Non-fatals were already handled by
        // handleError.
        $fatalMask = E_ERROR | E_PARSE | E_CORE_ERROR | E_COMPILE_ERROR | E_USER_ERROR | E_RECOVERABLE_ERROR;
        if (($code & $fatalMask) === 0) {
            return;
        }
        $this->record(
            $code,
            (string) ($err['message'] ?? ''),
            (string) ($err['file'] ?? ''),
            (int) ($err['line'] ?? 0),
            'fatal'
        );
    }

    /**
     * Insert-or-update one error row.
     *
     * @param int $code PHP error code (E_* constant).
     * @param string $message Error message.
     * @param string $file Source file path.
     * @param int $line Line number.
     * @param string $severity 'fatal'|'warning'|'notice'|'deprecated'|'bootstrap'
     * @return void
     */
    public function record(int $code, string $message, string $file, int $line, string $severity): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        $md5 = md5($code . ':' . $file . ':' . $line . ':' . $message);
        $table = $wpdb->prefix . self::TABLE;
        $now = time();

        // Best-effort capture of the request URI for context. Truncated to
        // bound the row size — InnoDB byte limits matter for the (md5)
        // unique index page count.
        $requestPath = '';
        if (isset($_SERVER['REQUEST_URI']) && is_string($_SERVER['REQUEST_URI'])) {
            $requestPath = substr((string) $_SERVER['REQUEST_URI'], 0, 512);
        }

        try {
            // Try to UPDATE first — fast path for repeat occurrences.
            $updated = $wpdb->query(
                $wpdb->prepare(
                    "UPDATE {$table} SET occurrence_count = occurrence_count + 1, last_seen = %d WHERE md5 = %s",
                    $now,
                    $md5
                )
            );
            if (is_int($updated) && $updated > 0) {
                return;
            }
            // First-seen — INSERT.
            $wpdb->insert(
                $table,
                [
                    'md5'              => $md5,
                    'code'             => $code,
                    'message'          => substr($message, 0, 4096),
                    'file'             => substr($file, 0, 1024),
                    'line'             => $line,
                    'request_path'     => $requestPath,
                    'request_id'       => $this->requestId(),
                    'backtrace_compressed' => null,
                    'first_seen'       => $now,
                    'last_seen'        => $now,
                    'occurrence_count' => 1,
                    'severity'         => $severity,
                    'silenced'         => 0,
                ],
                ['%s', '%d', '%s', '%s', '%d', '%s', '%s', '%s', '%d', '%d', '%d', '%s', '%d']
            );
            $this->enforceRowCap();
        } catch (\Throwable $e) {
            // Never let the error monitor itself fatal the request.
        }
    }

    /**
     * Translate PHP error codes to a coarse severity bucket.
     */
    private function severityForCode(int $code): string
    {
        if ($code & (E_ERROR | E_PARSE | E_CORE_ERROR | E_COMPILE_ERROR | E_USER_ERROR | E_RECOVERABLE_ERROR)) {
            return 'fatal';
        }
        if ($code & (E_WARNING | E_USER_WARNING | E_CORE_WARNING | E_COMPILE_WARNING)) {
            return 'warning';
        }
        if ($code & (E_NOTICE | E_USER_NOTICE)) {
            return 'notice';
        }
        if ($code & (E_DEPRECATED | E_USER_DEPRECATED)) {
            return 'deprecated';
        }
        return 'unknown';
    }

    /**
     * Generate a per-request id for correlation.
     */
    private function requestId(): string
    {
        if (function_exists('wp_unique_id')) {
            return (string) wp_unique_id('req_');
        }
        return bin2hex(random_bytes(8));
    }

    /**
     * Enforce the 10k row cap by evicting oldest by last_seen ASC.
     */
    private function enforceRowCap(): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        $table = $wpdb->prefix . self::TABLE;
        $count = (int) $wpdb->get_var("SELECT COUNT(*) FROM {$table}");
        if ($count <= self::ROW_CAP) {
            return;
        }
        $overflow = $count - self::ROW_CAP;
        // DELETE the $overflow oldest rows by last_seen ASC. We cap delete
        // batches at 200 per call to keep the eviction work bounded.
        $batch = min($overflow, 200);
        $wpdb->query(
            $wpdb->prepare(
                "DELETE FROM {$table} ORDER BY last_seen ASC LIMIT %d",
                $batch
            )
        );
    }

    /**
     * Toggle silenced flag on a fingerprint (the operator action from the UI).
     *
     * @param string $md5 The fingerprint.
     * @param bool $silenced Whether to silence.
     * @return int Rows affected.
     */
    public function setSilenced(string $md5, bool $silenced): int
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return 0;
        }
        $table = $wpdb->prefix . self::TABLE;
        $result = $wpdb->update(
            $table,
            ['silenced' => $silenced ? 1 : 0],
            ['md5' => $md5],
            ['%d'],
            ['%s']
        );
        return is_int($result) ? $result : 0;
    }

    /**
     * Build the batch the heartbeat ships to /agent/v1/errors. Returns up to
     * SHIP_BATCH NEWEST unsilenced rows above the stored `since_id` cursor.
     *
     * @return array<int,array<string,mixed>>
     */
    public function shipBatch(): array
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return [];
        }
        $table = $wpdb->prefix . self::TABLE;
        $since = (int) (function_exists('get_option') ? get_option(self::OPTION_SHIP_CURSOR, 0) : 0);
        $rows = $wpdb->get_results(
            $wpdb->prepare(
                "SELECT id, md5, code, message, file, line, request_path, request_id,
                        first_seen, last_seen, occurrence_count, severity
                 FROM {$table}
                 WHERE id > %d AND silenced = 0
                 ORDER BY id DESC
                 LIMIT %d",
                $since,
                self::SHIP_BATCH
            ),
            ARRAY_A
        );
        if (!is_array($rows)) {
            return [];
        }
        return $rows;
    }

    /**
     * Advance the ship cursor after the CP confirms it consumed up to $highestId.
     *
     * @param int $highestId The highest id the CP confirmed.
     * @return void
     */
    public function advanceCursor(int $highestId): void
    {
        if (!function_exists('update_option')) {
            return;
        }
        $current = (int) (function_exists('get_option') ? get_option(self::OPTION_SHIP_CURSOR, 0) : 0);
        if ($highestId > $current) {
            update_option(self::OPTION_SHIP_CURSOR, $highestId, false);
        }
    }
}
