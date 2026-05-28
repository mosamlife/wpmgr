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
    public const CURRENT_VERSION = '2';

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

        $jtiTable    = $prefix . Connector::JTI_TABLE;
        $replayTable = $prefix . ReplayCache::TABLE;

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
        ];
    }
}
