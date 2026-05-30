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
 * S1.2 — error config (ignore-list + level):
 *   - `wpmgr_error_config` wp-option holds `{ "error_level": <int>, "ignore_md5s": [...] }`.
 *   - `error_level`: bitmask of non-fatal codes to capture; fatals always captured.
 *   - `ignore_md5s`: fingerprints to drop entirely (no INSERT/UPDATE).
 *   - Config is written by the `sync_error_config` signed command and read on
 *     every request via a static per-request cache (one get_option() per boot).
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

    /**
     * wp-options key for the ship timestamp cursor (max last_seen confirmed by
     * CP). Used by the occurrence-count drift fix: rows whose last_seen is
     * greater than this value are re-included in the next ship batch even if
     * their id is below OPTION_SHIP_CURSOR.
     */
    public const OPTION_SHIP_TS = 'wpmgr_agent_errors_ship_ts';

    /** Global flag the mu-plugin sets to indicate it has trapped errors. */
    public const GLOBAL_PENDING = 'wpmgr_agent_pending_errors';

    /**
     * wp-options key for the per-site error config pushed by the control plane.
     * Holds JSON: { "error_level": <int E_* bitmask>, "ignore_md5s": ["<32hex>", ...] }
     */
    public const OPTION_CONFIG = 'wpmgr_error_config';

    /**
     * The full set of non-fatal codes this class can intercept via
     * set_error_handler. Never widened by config — only narrowed.
     */
    private const HANDLEABLE_NON_FATAL = E_WARNING | E_NOTICE | E_USER_WARNING | E_USER_NOTICE | E_DEPRECATED | E_USER_DEPRECATED;

    /**
     * Fatal-class codes. The shutdown handler matches against this mask.
     * Config can NEVER disable fatal capture.
     */
    private const FATAL_MASK = E_ERROR | E_PARSE | E_CORE_ERROR | E_COMPILE_ERROR | E_USER_ERROR | E_RECOVERABLE_ERROR;

    /**
     * Per-instance config cache. Populated on first call to loadConfig() and
     * cleared by applyConfig() so subsequent record() calls in the same request
     * (e.g. during unit tests) pick up the updated config without re-reading
     * wp-options. In production each PHP worker services one request and exits,
     * so "same request" is the only case where invalidation matters.
     *
     * @var array{error_level:int,ignore_md5s:array<int,string>}|null
     */
    private ?array $configCache = null;

    /**
     * Load and validate the error config from wp-options. Cached per-instance
     * (one get_option() per request lifetime).
     *
     * Returns safe defaults when the option is absent, not valid JSON, or
     * contains out-of-range values:
     *   - error_level: HANDLEABLE_NON_FATAL (captures everything, preserving
     *     pre-S1.2 behaviour until an operator narrows the mask).
     *   - ignore_md5s: [] (nothing ignored by default).
     *
     * Validation rules (mirror WP Remote's isValidErrorLevel):
     *   - error_level must satisfy `($level & E_ALL) === $level` (only known
     *     E_* bits are set).
     *   - ignore_md5s entries must be exactly 32 lowercase hex characters;
     *     invalid entries are silently dropped rather than rejecting the whole
     *     config.
     *
     * @return array{error_level:int,ignore_md5s:array<int,string>}
     */
    private function loadConfig(): array
    {
        if ($this->configCache !== null) {
            return $this->configCache;
        }

        $defaults = [
            'error_level' => self::HANDLEABLE_NON_FATAL,
            'ignore_md5s' => [],
        ];

        $raw = function_exists('get_option') ? get_option(self::OPTION_CONFIG, null) : null;
        if (!is_string($raw) || $raw === '') {
            $this->configCache = $defaults;
            return $this->configCache;
        }

        $decoded = json_decode($raw, true);
        if (!is_array($decoded)) {
            $this->configCache = $defaults;
            return $this->configCache;
        }

        // Validate error_level: must be an integer whose bits are all within E_ALL.
        $level = isset($decoded['error_level']) ? $decoded['error_level'] : self::HANDLEABLE_NON_FATAL;
        if (!is_int($level) || ($level & E_ALL) !== $level || $level < 0) {
            $level = self::HANDLEABLE_NON_FATAL;
        }

        // Validate ignore_md5s: keep only 32-char hex strings.
        $rawMd5s = isset($decoded['ignore_md5s']) && is_array($decoded['ignore_md5s'])
            ? $decoded['ignore_md5s']
            : [];
        $ignoreMd5s = [];
        foreach ($rawMd5s as $entry) {
            if (is_string($entry) && preg_match('/^[0-9a-f]{32}$/', $entry) === 1) {
                $ignoreMd5s[] = $entry;
            }
        }

        $this->configCache = [
            'error_level' => $level,
            'ignore_md5s' => $ignoreMd5s,
        ];

        return $this->configCache;
    }

    /**
     * Install the error + shutdown handlers. Idempotent — repeated calls are
     * safe (set_error_handler stacks; we keep a static flag so we install at
     * most once per request).
     *
     * The set_error_handler mask is computed from the stored config, intersected
     * with HANDLEABLE_NON_FATAL so config can only NARROW the captured set, never
     * widen it beyond what the handler can actually intercept. If the intersected
     * mask is 0 (operator silenced all non-fatals), we still register the shutdown
     * handler so fatals are always captured.
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

        // Derive the set_error_handler mask from config, narrowed to the
        // handleable non-fatal set. Fatals are always captured via shutdown.
        $config    = $this->loadConfig();
        $errorMask = $config['error_level'] & self::HANDLEABLE_NON_FATAL;

        if ($errorMask !== 0) {
            set_error_handler([$this, 'handleError'], $errorMask);
        }

        // Always register the shutdown handler regardless of config — fatals
        // (E_ERROR, E_PARSE, E_CORE_ERROR, E_COMPILE_ERROR, E_USER_ERROR,
        // E_RECOVERABLE_ERROR) are NEVER suppressible via error_level config.
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
        $bt = $this->captureBacktrace();
        $this->record($code, $message, $file, $line, $this->severityForCode($code), $bt);
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
     * Early-return gates (applied in order before any DB work):
     *   1. md5 ignore-list: if the fingerprint appears in the CP-pushed
     *      `ignore_md5s` config list, the error is dropped entirely.
     *   2. Level mask: if the error code is non-fatal AND is not within the
     *      configured `error_level` bitmask, the error is dropped. Fatal codes
     *      (shutdown handler path) bypass this check — they are ALWAYS recorded.
     *
     * @param int $code PHP error code (E_* constant).
     * @param string $message Error message.
     * @param string $file Source file path.
     * @param int $line Line number.
     * @param string $severity 'fatal'|'warning'|'notice'|'deprecated'|'bootstrap'
     * @param string|null $backtraceCompressed gzcompress(json_encode($frames)) from
     *     captureBacktrace(), or null when no backtrace is available (shutdown
     *     fatals, bootstrap-queue drains).
     * @return void
     */
    public function record(int $code, string $message, string $file, int $line, string $severity, ?string $backtraceCompressed = null): void
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        $md5 = md5($code . ':' . $file . ':' . $line . ':' . $message);

        // --- S1.2 ignore-list + level gating ---
        $config = $this->loadConfig();

        // Gate 1: md5 ignore-list — drop entirely if fingerprint is silenced.
        if (in_array($md5, $config['ignore_md5s'], true)) {
            return;
        }

        // Gate 2: level mask — non-fatal codes must be within the configured
        // bitmask. Fatal codes (shutdown handler) always pass through.
        $isFatal = ($code & self::FATAL_MASK) !== 0;
        if (!$isFatal && ($code & $config['error_level']) === 0) {
            return;
        }
        // --- end S1.2 gating ---

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
                    'md5'                  => $md5,
                    'code'                 => $code,
                    'message'              => substr($message, 0, 4096),
                    'file'                 => substr($file, 0, 1024),
                    'line'                 => $line,
                    'request_path'         => $requestPath,
                    'request_id'           => $this->requestId(),
                    'backtrace_compressed' => $backtraceCompressed,
                    'first_seen'           => $now,
                    'last_seen'            => $now,
                    'occurrence_count'     => 1,
                    'severity'             => $severity,
                    'silenced'             => 0,
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
     * Validate and persist the CP-pushed error config. Called by the
     * `sync_error_config` command handler after the JWT has been verified.
     *
     * Validation mirrors loadConfig(): error_level must satisfy
     * `($level & E_ALL) === $level`; ignore_md5s entries must be 32 hex chars.
     * Invalid entries are silently dropped (best-effort). The resulting config
     * is written to wp-options as JSON under OPTION_CONFIG.
     *
     * Best-effort cleanup: when fingerprints are newly added to ignore_md5s the
     * corresponding rows are deleted from the capture table so they disappear
     * from the dashboard immediately (mirrors WP Remote's intent — the operator
     * silenced them, so showing historical data is misleading). Failure to
     * delete is silently ignored; the rows will be suppressed from the next ship
     * batch anyway by loadConfig()'s ignore gate.
     *
     * The static loadConfig() cache is invalidated by clearing it so the new
     * config is picked up for the remainder of this request.
     *
     * @param int            $errorLevel  Desired non-fatal capture mask.
     * @param array<mixed>   $ignoreMd5s  Fingerprints to silence.
     * @return void
     */
    public function applyConfig(int $errorLevel, array $ignoreMd5s): void
    {
        // Validate and clamp error_level.
        if (($errorLevel & E_ALL) !== $errorLevel || $errorLevel < 0) {
            $errorLevel = self::HANDLEABLE_NON_FATAL;
        }

        // Validate ignore_md5s: keep only 32-char lowercase hex.
        $cleanMd5s = [];
        foreach ($ignoreMd5s as $entry) {
            if (is_string($entry) && preg_match('/^[0-9a-f]{32}$/', $entry) === 1) {
                $cleanMd5s[] = $entry;
            }
        }

        // Persist.
        $encoded = (string) json_encode([
            'error_level' => $errorLevel,
            'ignore_md5s' => $cleanMd5s,
        ]);
        if (function_exists('update_option')) {
            update_option(self::OPTION_CONFIG, $encoded, false);
        }

        // Invalidate the per-instance config cache so subsequent record() calls
        // in the same request pick up the new config without re-reading wp-options.
        // In production this matters primarily in unit tests; in a live request the
        // PHP worker exits after the REST response is sent.
        $this->configCache = null;

        // Best-effort: delete existing rows whose md5 is now ignored, so they
        // disappear from the dashboard immediately.
        if ($cleanMd5s !== []) {
            $this->deleteIgnoredRows($cleanMd5s);
        }
    }

    /**
     * Delete rows whose md5 fingerprint is in the given ignore list.
     * Best-effort — failure is silently swallowed.
     *
     * @param array<int,string> $md5s Fingerprints to remove.
     * @return void
     */
    private function deleteIgnoredRows(array $md5s): void
    {
        global $wpdb;
        if (!is_object($wpdb) || $md5s === []) {
            return;
        }
        $table = $wpdb->prefix . self::TABLE;
        try {
            // Build a safe IN(...) clause using prepare() placeholders.
            $placeholders = implode(', ', array_fill(0, count($md5s), '%s'));
            $wpdb->query(
                $wpdb->prepare(
                    "DELETE FROM {$table} WHERE md5 IN ({$placeholders})",
                    ...$md5s
                )
            );
        } catch (\Throwable $e) {
            // Never let cleanup fatal the request.
        }
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
     * SHIP_BATCH NEWEST unsilenced rows whose id is above the stored
     * `since_id` cursor OR whose last_seen is above the stored `since_ts`
     * cursor (occurrence-count drift fix: re-ships recently-active rows so
     * the CP count does not freeze at the value seen on first-ship).
     *
     * Each returned row includes a `backtrace` key holding the decompressed
     * JSON array of frame objects ([] when no backtrace was recorded). The raw
     * `backtrace_compressed` column is NOT present in the returned array.
     *
     * @return array<int,array<string,mixed>>
     */
    public function shipBatch(): array
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return [];
        }
        $table    = $wpdb->prefix . self::TABLE;
        $sinceId  = (int) (function_exists('get_option') ? get_option(self::OPTION_SHIP_CURSOR, 0) : 0);
        $sinceTs  = (int) (function_exists('get_option') ? get_option(self::OPTION_SHIP_TS, 0) : 0);
        $rows = $wpdb->get_results(
            $wpdb->prepare(
                "SELECT id, md5, code, message, file, line, request_path, request_id,
                        first_seen, last_seen, occurrence_count, severity,
                        backtrace_compressed
                 FROM {$table}
                 WHERE (id > %d OR last_seen > %d) AND silenced = 0
                 ORDER BY last_seen DESC
                 LIMIT %d",
                $sinceId,
                $sinceTs,
                self::SHIP_BATCH
            ),
            ARRAY_A
        );
        if (!is_array($rows)) {
            return [];
        }

        // Decompress the backtrace into wire form and strip the raw column.
        foreach ($rows as &$row) {
            $compressed = isset($row['backtrace_compressed']) ? (string) $row['backtrace_compressed'] : '';
            unset($row['backtrace_compressed']);

            $backtrace = [];
            if ($compressed !== '') {
                try {
                    $json = @gzuncompress($compressed);
                    if (is_string($json) && $json !== '') {
                        $decoded = json_decode($json, true);
                        if (is_array($decoded)) {
                            $backtrace = $decoded;
                        }
                    }
                } catch (\Throwable $e) {
                    // Corrupt or empty blob — ship an empty array.
                }
            }
            $row['backtrace'] = $backtrace;
        }
        unset($row);

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

    /**
     * Advance the ship-timestamp cursor after the CP confirms the batch. Mirrors
     * advanceCursor() but tracks max last_seen rather than max id, enabling the
     * occurrence-count drift fix in shipBatch() to re-include rows whose count
     * has ticked up since they were last shipped.
     *
     * @param int $maxLastSeen The highest last_seen timestamp across the shipped batch.
     * @return void
     */
    public function advanceShipTs(int $maxLastSeen): void
    {
        if (!function_exists('update_option')) {
            return;
        }
        $current = (int) (function_exists('get_option') ? get_option(self::OPTION_SHIP_TS, 0) : 0);
        if ($maxLastSeen > $current) {
            update_option(self::OPTION_SHIP_TS, $maxLastSeen, false);
        }
    }

    /**
     * Capture a meaningful backtrace for live set_error_handler calls. Drops
     * frames originating in this class (captureBacktrace / handleError /
     * record internals), keeps the outermost 10 application frames, and
     * returns gzcompress(json_encode($frames)) for compact DB storage.
     *
     * Returns null when: no application frames remain, json_encode fails,
     * gzcompress fails, or any exception is thrown — never propagates.
     *
     * @return string|null gzcompress'd JSON, or null.
     */
    private function captureBacktrace(): ?string
    {
        try {
            // 12 raw frames gives headroom to drop the 2 internal ones
            // (captureBacktrace + handleError) and still have up to 10.
            $raw = debug_backtrace(DEBUG_BACKTRACE_IGNORE_ARGS, 12);
            $frames = [];
            foreach ($raw as $frame) {
                // Drop frames that originate inside this class file.
                $frameFile = isset($frame['file']) ? (string) $frame['file'] : '';
                if ($frameFile === __FILE__) {
                    continue;
                }
                $frames[] = [
                    'file'     => $frameFile,
                    'line'     => isset($frame['line']) ? (int) $frame['line'] : 0,
                    'function' => isset($frame['function']) ? (string) $frame['function'] : '',
                ];
                if (count($frames) >= 10) {
                    break;
                }
            }
            if ($frames === []) {
                return null;
            }
            $json = json_encode($frames);
            if (!is_string($json) || $json === '') {
                return null;
            }
            $compressed = gzcompress($json);
            return is_string($compressed) ? $compressed : null;
        } catch (\Throwable $e) {
            return null;
        }
    }
}
