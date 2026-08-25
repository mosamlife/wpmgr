<?php
/**
 * Schema: centralized DB-schema definitions + idempotent migration runner.
 *
 * Why this exists:
 *   WordPress' `register_activation_hook` only fires on a true activation
 *   transition (inactive -> active). Same-version re-uploads, in-place
 *   replacements, and many "Update Now" flows do NOT trigger it. Plugins that
 *   only create tables in the activation hook end up missing tables on those
 *   paths, producing runtime 500s (e.g. `wpmgr_replay_mark_failed` for the
 *   M5.5 autologin replay table that never got created on a re-upload).
 *
 * Fix pattern (the canonical WP "plugin upgrade routine"):
 *   - Store the agent's intended schema version in a wp_options row.
 *   - On every `plugins_loaded` (cheap option-read shortcut), compare it to
 *     CURRENT_VERSION; if different, run `dbDelta` for every agent table
 *     definition and bump the option.
 *   - The activation hook ALSO calls into this so fresh installs still work.
 *
 * `dbDelta` is idempotent — it only emits ALTER/CREATE statements for the
 * deltas — so re-running it on already-correct tables is a no-op.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent;

/**
 * Centralized schema/migrations for the agent's DB tables.
 *
 * Not declared `final` so tests can subclass for assertions if needed; nothing
 * in production inherits from it.
 */
class Schema
{
    /**
     * The agent's current DB schema version.
     *
     * Bump this whenever a table definition in self::definitions() changes
     * (add a column, add an index, etc). The migration runner reads the
     * stored option and compares it to this value; mismatch => run dbDelta.
     */
    public const CURRENT_VERSION = '11';

    /** Name of the M5.6 phpbu-runner dedup table (unprefixed). */
    public const BACKUP_RUNS_TABLE = 'wpmgr_backup_runs';

    /** Name of the M5.6/ADR-033 backup task-state table (unprefixed). */
    public const BACKUP_TASKS_TABLE = 'wpmgr_backup_tasks';

    /** Name of the M5.6/ADR-034 restore dedup table (unprefixed). */
    public const BACKUP_RESTORE_RUNS_TABLE = 'wpmgr_restore_runs';

    /** Name of the M5.6/ADR-034 restore task-state table (unprefixed). */
    public const BACKUP_RESTORE_TASKS_TABLE = 'wpmgr_restore_tasks';

    /** ADR-037 Sprint 2 — PHP-error capture table (unprefixed). */
    public const PHP_ERRORS_TABLE = 'wpmgr_php_errors';

    /** ADR-037 Sprint 2 — diagnostics-run history table (unprefixed). */
    public const DIAGNOSTICS_RUNS_TABLE = 'wpmgr_diagnostics_runs';

    /** ADR-037 Sprint 3 — hash-chained WP activity log table (unprefixed). */
    public const ACTIVITY_LOG_TABLE = 'wpmgr_activity_log';

    /** S2 — login-event capture table (unprefixed). */
    public const LOGIN_EVENTS_TABLE = 'wpmgr_login_events';

    /**
     * Task #171 — preload-queue table (unprefixed). A custom MySQL-table queue
     * with atomic claim/lock, retry/backoff, and concurrency-bounded loopback
     * runners that warms same-host URLs into the page cache. Replaces the single
     * wp-option queue that the cache warmer previously used.
     */
    public const PRELOAD_QUEUE_TABLE = 'wpmgr_preload_queue';

    /**
     * Per-site email send-event log table (unprefixed).
     *
     * Local buffer for every wp_mail() attempt routed through WPMgr's provider
     * pipeline. Body is stored only when store_body=true (default false).
     * Designed for a keyset-cursor CP ingest (Phase 3): rows are ordered by
     * (created_at DESC, id DESC) and the composite (created_at, id) pair serves
     * as the cursor so the CP can page through all rows without re-fetching.
     *
     * Auto-increment `id` is the PK (cursor-friendly); `created_at` is UTC
     * DATETIME (matches every other agent table).
     */
    public const EMAIL_LOG_TABLE = 'wpmgr_email_log';

    /** Option key storing the last-installed schema version. */
    public const OPTION_DB_VERSION = 'wpmgr_agent_db_version';

    /**
     * Ensure the DB schema matches CURRENT_VERSION.
     *
     * Cheap path: a single get_option() call when already current.
     * Migration path: requires upgrade.php (for dbDelta), iterates the
     * definitions map, and bumps the option.
     *
     * @param bool $force If true, run dbDelta unconditionally (used by the
     *                    autologin fallback retry to self-heal a broken
     *                    install on the spot regardless of the option value).
     * @return void
     */
    public static function ensureCurrent(bool $force = false): void
    {
        if (!function_exists('get_option') || !function_exists('update_option')) {
            // Not in a WP runtime; nothing we can do (and nothing we should).
            return;
        }

        $stored = (string) get_option(self::OPTION_DB_VERSION, '0');
        if (!$force && hash_equals(self::CURRENT_VERSION, $stored)) {
            return;
        }

        global $wpdb;
        if (!is_object($wpdb)) {
            return;
        }
        /** @var \wpdb $wpdb */

        // dbDelta lives in wp-admin/includes/upgrade.php and is not loaded by
        // default on the frontend. require_once is safe to call multiple times.
        if (defined('ABSPATH') && file_exists(ABSPATH . 'wp-admin/includes/upgrade.php')) {
            require_once ABSPATH . 'wp-admin/includes/upgrade.php';
        }

        if (!function_exists('dbDelta')) {
            // In test environments we may not have dbDelta; bail without
            // bumping the option so the next request retries.
            return;
        }

        foreach (self::definitions() as $sql) {
            dbDelta($sql);
        }

        update_option(self::OPTION_DB_VERSION, self::CURRENT_VERSION, false);
    }

    /**
     * Map of unqualified-name => CREATE TABLE SQL for every agent table.
     *
     * Adding a new table here + bumping CURRENT_VERSION is the entire
     * migration ceremony. Existing rows are preserved because dbDelta only
     * emits the deltas required to reach the declared shape.
     *
     * @return array<string,string>
     */
    public static function definitions(): array
    {
        global $wpdb;
        $prefix  = (is_object($wpdb) && isset($wpdb->prefix)) ? (string) $wpdb->prefix : 'wp_';
        $charset = (is_object($wpdb) && method_exists($wpdb, 'get_charset_collate'))
            ? (string) $wpdb->get_charset_collate()
            : '';

        $jtiTable          = $prefix . Connector::JTI_TABLE;
        $replayTable       = $prefix . ReplayCache::TABLE;
        $backupRunsTable   = $prefix . self::BACKUP_RUNS_TABLE;
        $backupTasksTable  = $prefix . self::BACKUP_TASKS_TABLE;
        $restoreRunsTable  = $prefix . self::BACKUP_RESTORE_RUNS_TABLE;
        $restoreTasksTable = $prefix . self::BACKUP_RESTORE_TASKS_TABLE;
        $phpErrorsTable    = $prefix . self::PHP_ERRORS_TABLE;
        $diagnosticsRunsTable = $prefix . self::DIAGNOSTICS_RUNS_TABLE;
        $activityLogTable  = $prefix . self::ACTIVITY_LOG_TABLE;
        $loginEventsTable  = $prefix . self::LOGIN_EVENTS_TABLE;
        $preloadQueueTable = $prefix . self::PRELOAD_QUEUE_TABLE;
        $emailLogTable     = $prefix . self::EMAIL_LOG_TABLE;

        return [
            // M2: Connector anti-replay table (short window, per-token jti).
            Connector::JTI_TABLE => "CREATE TABLE {$jtiTable} (
                id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
                jti_hash CHAR(64) NOT NULL,
                expires_at BIGINT UNSIGNED NOT NULL,
                created_at BIGINT UNSIGNED NOT NULL,
                PRIMARY KEY  (id),
                UNIQUE KEY jti_hash (jti_hash),
                KEY expires_at (expires_at)
            ) {$charset};",

            // M5.5: Autologin single-use replay table (long window).
            ReplayCache::TABLE => "CREATE TABLE {$replayTable} (
                jti_hash CHAR(64) NOT NULL,
                expires_at BIGINT UNSIGNED NOT NULL,
                PRIMARY KEY  (jti_hash),
                KEY expires_at (expires_at)
            ) {$charset};",

            // M5.6: phpbu runner dedup. Defeats lost-ACK retry storms: if the
            // CP retries the `backup` command for the SAME snapshot_id while a
            // runner is already in flight, we refuse to spawn a second one.
            // `snapshot_id` is the PK; the active row is the one with
            // started_at within the dedup window. A finished runner clears its
            // row on exit (best effort), and the watchdog GC sweeps stale rows.
            self::BACKUP_RUNS_TABLE => "CREATE TABLE {$backupRunsTable} (
                snapshot_id CHAR(36) NOT NULL,
                pid INT UNSIGNED NOT NULL,
                started_at BIGINT UNSIGNED NOT NULL,
                PRIMARY KEY  (snapshot_id),
                KEY started_at (started_at)
            ) {$charset};",

            // M5.6/ADR-033: checkpointed task state for the backup
            // backup runner. `sub_state` JSON carries per-phase resume cursors
            // (file walker offset, mysqldump table+row position, multipart
            // upload id/parts, etc.); the watchdog re-enters from this row on
            // stall and resumes from the last checkpoint instead of restarting.
            // Co-exists with BACKUP_RUNS_TABLE: runs is legacy dedup (PID-keyed
            // in-flight guard), tasks is the new persistent state machine.
            self::BACKUP_TASKS_TABLE => "CREATE TABLE {$backupTasksTable} (
                snapshot_id CHAR(36) NOT NULL,
                kind VARCHAR(16) NOT NULL,
                phase VARCHAR(32) NOT NULL DEFAULT 'queued',
                sub_state LONGTEXT NOT NULL,
                started_at BIGINT UNSIGNED NOT NULL,
                last_progress_at BIGINT UNSIGNED NOT NULL,
                resume_count INT UNSIGNED NOT NULL DEFAULT 0,
                max_resumes INT UNSIGNED NOT NULL DEFAULT 6,
                PRIMARY KEY  (snapshot_id),
                KEY phase (phase),
                KEY last_progress_at (last_progress_at)
            ) {$charset};",

            // M5.6/ADR-034: restore dedup. Parallels BACKUP_RUNS_TABLE but is
            // keyed by (snapshot_id, restore_id): a single snapshot can be
            // restored repeatedly (each attempt is a fresh restore_id), but a
            // given (snapshot, restore_id) pair must execute at most once. The
            // CP retrying a lost-ACK is the failure mode this defeats. Active
            // row is one whose started_at is within DEDUP_WINDOW_SECONDS of
            // now; a finished runner deletes its row.
            self::BACKUP_RESTORE_RUNS_TABLE => "CREATE TABLE {$restoreRunsTable} (
                snapshot_id CHAR(36) NOT NULL,
                restore_id CHAR(36) NOT NULL,
                pid INT UNSIGNED NOT NULL,
                started_at BIGINT UNSIGNED NOT NULL,
                PRIMARY KEY  (snapshot_id, restore_id),
                KEY started_at (started_at)
            ) {$charset};",

            // M5.6/ADR-034: checkpointed task state for the restore
            // restore runner. Mirrors BACKUP_TASKS_TABLE shape. PK is
            // (snapshot_id, restore_id) so a customer can re-attempt a failed
            // restore with a new restore_id without colliding with the old
            // row. sub_state.params holds the runner config (endpoints, db
            // creds, chunk download maps) so the watchdog can rehydrate the
            // runner without re-receiving them from the CP.
            self::BACKUP_RESTORE_TASKS_TABLE => "CREATE TABLE {$restoreTasksTable} (
                snapshot_id CHAR(36) NOT NULL,
                restore_id CHAR(36) NOT NULL,
                kind VARCHAR(16) NOT NULL,
                phase VARCHAR(32) NOT NULL DEFAULT 'preflight',
                sub_state LONGTEXT NOT NULL,
                started_at BIGINT UNSIGNED NOT NULL,
                last_progress_at BIGINT UNSIGNED NOT NULL,
                resume_count INT UNSIGNED NOT NULL DEFAULT 0,
                max_resumes INT UNSIGNED NOT NULL DEFAULT 6,
                PRIMARY KEY  (snapshot_id, restore_id),
                KEY phase (phase),
                KEY last_progress_at (last_progress_at)
            ) {$charset};",

            // ADR-037 Sprint 2 — PHP error capture. `md5` is the dedup key
            // (md5(code:file:line:message)) and carries a UNIQUE index so the
            // ErrorMonitor's UPDATE-then-INSERT fast path can run as a single
            // index seek. Row cap (10k) is enforced application-side by
            // ErrorMonitor::enforceRowCap (eviction by last_seen ASC).
            // `backtrace_compressed` is intentionally a BLOB so a stacktrace
            // can be gzipped before storage; null when we don't capture one.
            self::PHP_ERRORS_TABLE => "CREATE TABLE {$phpErrorsTable} (
                id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
                md5 CHAR(32) NOT NULL,
                code INT NOT NULL,
                message TEXT NOT NULL,
                file TEXT NOT NULL,
                line INT NOT NULL DEFAULT 0,
                request_path TEXT NOT NULL,
                request_id VARCHAR(64) NOT NULL DEFAULT '',
                backtrace_compressed LONGBLOB NULL,
                first_seen BIGINT UNSIGNED NOT NULL,
                last_seen BIGINT UNSIGNED NOT NULL,
                occurrence_count INT UNSIGNED NOT NULL DEFAULT 1,
                severity VARCHAR(16) NOT NULL DEFAULT 'warning',
                silenced TINYINT(1) NOT NULL DEFAULT 0,
                PRIMARY KEY  (id),
                UNIQUE KEY md5 (md5),
                KEY last_seen (last_seen),
                KEY silenced (silenced)
            ) {$charset};",

            // ADR-037 Sprint 2 — diagnostics-run history. The CP polls the
            // latest row per (site, category) for its Health-tab cards;
            // older rows are pruned by the agent once a per-category cap
            // (default 24 rows ~= ~24 days at one push/day) is exceeded.
            // For V1 we just keep INSERTs without pruning — daily cadence
            // for ~1 yr is ~365 rows, well below any concerning footprint.
            self::DIAGNOSTICS_RUNS_TABLE => "CREATE TABLE {$diagnosticsRunsTable} (
                id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
                category VARCHAR(32) NOT NULL,
                payload LONGTEXT NOT NULL,
                collected_at BIGINT UNSIGNED NOT NULL,
                PRIMARY KEY  (id),
                KEY category_collected (category, collected_at)
            ) {$charset};",

            // ADR-037 Sprint 3 — hash-chained WP activity log. `seq` is a
            // per-site monotonic counter (option wpmgr_agent_activity_seq) with
            // a UNIQUE index — it orders the SHA-256 hash chain independently of
            // the AUTO_INCREMENT id. Each row's `this_hash` folds in the prior
            // row's `this_hash` (genesis = 64 zeros), so editing or deleting any
            // historical row breaks every subsequent hash — the CP verifies the
            // chain on ingest. `shipped_idx (shipped, id)` backs the batch
            // shipper (unshipped rows, seq ASC) and the oldest-shipped-first
            // eviction once the 10k row cap is exceeded (ActivityLog::enforceRowCap).
            // `meta` is the canonical compact JSON that was hashed.
            self::ACTIVITY_LOG_TABLE => "CREATE TABLE {$activityLogTable} (
                id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
                seq BIGINT UNSIGNED NOT NULL,
                event_type VARCHAR(64) NOT NULL,
                object_type VARCHAR(32) NOT NULL,
                object_id VARCHAR(255) NOT NULL DEFAULT '',
                object_label VARCHAR(255) NOT NULL DEFAULT '',
                actor_user_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
                actor_login VARCHAR(191) NOT NULL DEFAULT '',
                actor_ip VARCHAR(64) NOT NULL DEFAULT '',
                summary VARCHAR(255) NOT NULL DEFAULT '',
                meta LONGTEXT NULL,
                prev_hash CHAR(64) NOT NULL,
                this_hash CHAR(64) NOT NULL,
                occurred_at DATETIME NOT NULL,
                shipped TINYINT(1) NOT NULL DEFAULT 0,
                PRIMARY KEY  (id),
                UNIQUE KEY seq (seq),
                KEY shipped_idx (shipped, id)
            ) {$charset};",

            // S2 — login-event capture. `status` stores the event type:
            //   1 = failure (failed login attempt),
            //   2 = success (successful login),
            //   3 = blocked (login attempt blocked by the protection engine).
            // `category` identifies which rule fired (captcha_block, temp_block,
            // all_blocked, blacklisted, allowed, private_ip, bypassed).
            // `request_id` is the per-request correlation ID generated by
            // LoginProtection (used for dedup and incident tracing on the CP).
            // `occurred_at` is a Unix timestamp; the composite index on
            // (status, occurred_at) backs the time-windowed COUNT(*) queries
            // in getLoginCount() without a full table scan on high-volume sites.
            // Row cap (100k) is enforced application-side by
            // LoginProtection::enforceRowCap (eviction by occurred_at ASC).
            self::LOGIN_EVENTS_TABLE => "CREATE TABLE {$loginEventsTable} (
                id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
                ip VARCHAR(64) NOT NULL DEFAULT '',
                status TINYINT NOT NULL DEFAULT 1,
                category VARCHAR(64) NOT NULL DEFAULT '',
                username VARCHAR(191) NOT NULL DEFAULT '',
                request_id VARCHAR(64) NOT NULL DEFAULT '',
                occurred_at BIGINT UNSIGNED NOT NULL,
                PRIMARY KEY  (id),
                KEY occurred_at (occurred_at),
                KEY status_occurred (status, occurred_at)
            ) {$charset};",

            // Task #171 — preload-queue. WPMgr's own self-dispatching warm queue:
            // rows are claimed atomically (a 32-char lock_token + future-parked
            // backoff via locked_at), warmed via a same-host loopback fetch, then
            // DELETEd on success (there is no `completed` status). `task_hash` is
            // the sha256 dedup key over (url, device), so (url,desktop) and
            // (url,mobile) are two distinct rows. `priority` is ascending-urgency
            // (lower = warmed first). All timestamps are UTC DATETIME and every
            // comparison uses UTC_TIMESTAMP(). Indexes: uniq_group_task is the
            // upsert/dedup target; idx_runner covers the claim ORDER BY; idx_lock
            // backs the stale-requeue + active-runner scans.
            self::PRELOAD_QUEUE_TABLE => "CREATE TABLE {$preloadQueueTable} (
                id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
                group_name VARCHAR(32) NOT NULL,
                callback VARCHAR(64) NOT NULL,
                url TEXT NOT NULL,
                device VARCHAR(10) NOT NULL DEFAULT 'desktop',
                task_hash CHAR(64) NOT NULL,
                priority INT NOT NULL DEFAULT 20,
                status VARCHAR(20) NOT NULL DEFAULT 'pending',
                lock_token VARCHAR(64) NULL,
                attempts SMALLINT UNSIGNED NOT NULL DEFAULT 0,
                last_error TEXT NULL,
                created_at DATETIME NOT NULL,
                locked_at DATETIME NULL,
                updated_at DATETIME NOT NULL,
                PRIMARY KEY  (id),
                UNIQUE KEY uniq_group_task (group_name, task_hash),
                KEY idx_runner (group_name, callback, status, priority, created_at, id),
                KEY idx_lock (status, locked_at)
            ) {$charset};",

            // Email (Phase 2 / v11) — per-site outgoing-mail send-event buffer.
            // `id` AUTO_INCREMENT is the primary cursor for CP ingest
            // (keyset: WHERE (created_at, id) > (cursor_created_at, cursor_id)).
            // `status` is an enum-like VARCHAR: 'sent' | 'failed'.
            // `body` is NULL unless store_body=true (default false, GDPR-friendly).
            // `retries` counts fallback attempts (0 = first attempt only).
            // `resent_count` is incremented by the resend command.
            // `connection_key` records which named connection was used ('default' = primary).
            // `attachments` stores [{name,size_bytes}] metadata (TEXT NULL, JSON).
            // Indexes:
            //   PRIMARY (id)         — keyset cursor.
            //   idx_created (created_at DESC, id DESC) — time-range queries.
            //   idx_status (status, created_at DESC)   — failures-only view.
            self::EMAIL_LOG_TABLE => "CREATE TABLE {$emailLogTable} (
                id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
                message_id VARCHAR(255) NOT NULL DEFAULT '',
                mail_to VARCHAR(500) NOT NULL DEFAULT '',
                mail_from VARCHAR(255) NOT NULL DEFAULT '',
                subject VARCHAR(500) NOT NULL DEFAULT '',
                provider VARCHAR(32) NOT NULL DEFAULT '',
                status VARCHAR(16) NOT NULL DEFAULT 'sent',
                response TEXT NULL,
                error TEXT NULL,
                retries TINYINT UNSIGNED NOT NULL DEFAULT 0,
                resent_count SMALLINT UNSIGNED NOT NULL DEFAULT 0,
                body_stored TINYINT(1) NOT NULL DEFAULT 0,
                body LONGTEXT NULL,
                connection_key VARCHAR(32) NOT NULL DEFAULT '',
                attachments TEXT NULL,
                created_at DATETIME NOT NULL,
                PRIMARY KEY  (id),
                KEY idx_created (created_at, id),
                KEY idx_status (status, created_at)
            ) {$charset};",
        ];
    }
}
