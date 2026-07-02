<?php
/**
 * DbTableActionCommand — per-table database actions (optimize, repair, drop, empty, analyze, convert_innodb).
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/db_table_action
 *   Authorization: Bearer <Ed25519 JWT, cmd="db_table_action", aud=<siteId>>
 *   Content-Type: application/json
 *
 * Request body:
 *   {
 *     "job_id": "<UUID v4, required>",
 *     "action": "optimize" | "repair" | "drop" | "empty" | "analyze" | "convert_innodb",
 *     "tables": ["wp_full_table_name", ...]
 *   }
 *
 * Response (synchronous):
 *   {
 *     "ok":     true,
 *     "job_id": "<echoed>",
 *     "action": "<echoed>",
 *     "results": [
 *       { "table": "wp_foo", "status": "done",  "detail": "" },
 *       ...
 *     ]
 *   }
 *   On early validation failure:
 *   { "ok": false, "detail": "<reason>" }
 *
 * Safety model (DROP / EMPTY only):
 *   LAYER 1 — Re-runs classifyTable() at action time.
 *             DROP: refuses WP-core and unclassified tables (owner_type must not
 *                   be "core" or "unknown"); plugin/theme/orphan are allowed.
 *             EMPTY: refuses WP-core tables (owner_type must not be "core");
 *                    plugin/theme/orphan/unknown tables are all allowed.
 *             OPTIMIZE/REPAIR/ANALYZE/CONVERT_INNODB: no gate (any table allowed).
 *   LAYER 2 — Exact-match validation against live information_schema.TABLES
 *             using a prepared statement; table name used in SQL comes from the
 *             DB result, NOT the raw input.
 *   LAYER 3 — Operator type-to-confirm (enforced at the CP handler layer).
 *   LAYER 4 — PermSiteManage required at the CP route layer.
 *
 * Auth: the Router's permission_callback already enforced the signed JWT +
 * anti-replay contract (Connector::verifyCommand) before execute() runs.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Optimizer\DbCleanup;

/**
 * Per-table database action command (synchronous, full result in ACK).
 */
final class DbTableActionCommand implements CommandInterface
{
    /** Allowed action values. */
    private const ALLOWED_ACTIONS = ['optimize', 'repair', 'drop', 'empty', 'analyze', 'convert_innodb'];

    /** Maximum number of tables accepted in a single request. */
    private const MAX_TABLES = 200;

    private ?DbCleanup $cleanup;

    /** @var object|null Injected $wpdb (tests); defaults to global. */
    private ?object $wpdb;

    /**
     * @param DbCleanup|null $cleanup Injected for tests; defaults to a live engine.
     * @param object|null    $wpdb    Injected for tests; defaults to global $wpdb.
     */
    public function __construct(?DbCleanup $cleanup = null, ?object $wpdb = null)
    {
        $this->cleanup = $cleanup;
        $this->wpdb    = $wpdb ?? (isset($GLOBALS['wpdb']) && is_object($GLOBALS['wpdb']) ? $GLOBALS['wpdb'] : null);
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'db_table_action';
    }

    /**
     * Validate the request and execute the action on each listed table.
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused here).
     * @param array<string,mixed> $params {
     *   job_id: string — required UUID v4.
     *   action: string — one of optimize|repair|drop|empty.
     *   tables: string[] — full table names (with prefix).
     * }
     * @return array<string,mixed>
     */
    public function execute(array $claims, array $params): array
    {
        // --- Validate job_id --------------------------------------------------
        $jobId = isset($params['job_id']) && is_string($params['job_id']) && $params['job_id'] !== ''
            ? $params['job_id']
            : '';

        if ($jobId === '') {
            return ['ok' => false, 'detail' => 'missing job_id'];
        }

        // --- Validate action --------------------------------------------------
        $action = isset($params['action']) && is_string($params['action'])
            ? $params['action']
            : '';

        if (!in_array($action, self::ALLOWED_ACTIONS, true)) {
            return ['ok' => false, 'detail' => 'unknown action: must be one of optimize, repair, drop, empty, analyze, convert_innodb'];
        }

        // --- Validate tables[] ------------------------------------------------
        if (!isset($params['tables']) || !is_array($params['tables']) || $params['tables'] === []) {
            return ['ok' => false, 'detail' => 'tables must be a non-empty array'];
        }

        $tables = [];
        foreach ($params['tables'] as $t) {
            if (is_string($t) && $t !== '') {
                $tables[] = $t;
            }
        }

        if ($tables === []) {
            return ['ok' => false, 'detail' => 'tables must contain at least one non-empty string'];
        }

        // Reject (do NOT silently truncate) an over-cap batch: the operator's
        // type-to-confirm token said "DROP N TABLES" for a specific N, so quietly
        // dropping only the first 200 would mismatch what they confirmed. Fail the
        // whole call so they retry within the cap. The CP enforces the same cap up
        // front, so this is defence in depth.
        if (count($tables) > self::MAX_TABLES) {
            return [
                'ok'     => false,
                'detail' => sprintf('too many tables in one call (%d, max %d)', count($tables), self::MAX_TABLES),
            ];
        }

        // --- Run action on each table ----------------------------------------
        $results = [];
        foreach ($tables as $tableName) {
            $results[] = $this->applyAction($action, $tableName);
        }

        return [
            'ok'      => true,
            'job_id'  => $jobId,
            'action'  => $action,
            'results' => $results,
        ];
    }

    // -------------------------------------------------------------------------
    // Per-table action dispatcher
    // -------------------------------------------------------------------------

    /**
     * Apply one action to one table. Returns the per-table result array.
     *
     * LAYER 1 safety gates:
     *   - drop  → owner_type must NOT be "core" or "unknown"
     *             (plugin/theme/orphan are allowed; the owning plugin will recreate
     *             its schema on next activation/run if needed).
     *   - empty → owner_type must NOT be "core" (plugin/theme/orphan/unknown allowed).
     *   - optimize / repair / analyze / convert_innodb → no gate (any table allowed).
     *
     * @param string $action    One of optimize|repair|drop|empty|analyze|convert_innodb.
     * @param string $tableName Full table name as provided by the caller.
     * @return array{table:string,status:string,detail:string}
     */
    private function applyAction(string $action, string $tableName): array
    {
        // LAYER 2: Exact-match validation against live information_schema.
        $validatedName = $this->validateTableName($tableName);
        if ($validatedName === null) {
            return [
                'table'  => $tableName,
                'status' => 'not_found',
                'detail' => 'table not found in information_schema',
            ];
        }

        // LAYER 1 (DROP / EMPTY only): re-classify at action time.
        //
        // DROP is non-core: refuses 'core' (would destroy the site) AND 'unknown'
        // (an unclassified table should not be silently removed — refuse it for
        // safety). plugin/theme/orphan tables are all allowed; an owning plugin
        // will recreate its schema on next activation/run if the operator chooses
        // to remove a log table (e.g. wp_digits_failed_login_logs).
        //
        // EMPTY is non-core: allows operators to truncate log/data tables that
        // belong to active plugins (e.g. wp_digits_failed_login_logs,
        // wp_wpmgr_activity_log) while still refusing all WordPress core tables
        // (wp_posts, wp_users, wp_options, etc.) which would destroy the site.
        if ($action === 'drop') {
            [$ownerType] = $this->classifyLive($validatedName);
            if ($ownerType === 'core' || $ownerType === 'unknown') {
                return [
                    'table'  => $validatedName,
                    'status' => 'skipped',
                    'detail' => $ownerType === 'core'
                        ? 'table belongs to WordPress core and cannot be dropped'
                        : 'table could not be positively classified; refusing to drop',
                ];
            }
        } elseif ($action === 'empty') {
            // EMPTY requires a POSITIVE plugin/theme/orphan classification. Refuse
            // both 'core' (would destroy the site) AND 'unknown' (an unclassified
            // table could be a mis-detected core/system table — for a destructive
            // TRUNCATE we never act on a table we could not positively identify).
            [$ownerType] = $this->classifyLive($validatedName);
            if ($ownerType === 'core' || $ownerType === 'unknown') {
                return [
                    'table'  => $validatedName,
                    'status' => 'skipped',
                    'detail' => $ownerType === 'core'
                        ? 'table belongs to WordPress core and cannot be emptied'
                        : 'table could not be positively classified; refusing to empty',
                ];
            }
        }

        // Execute the SQL action. The table name in SQL comes from the
        // information_schema result (validatedName), never raw user input.
        return match ($action) {
            'optimize'        => $this->runOptimize($validatedName),
            'repair'          => $this->runRepair($validatedName),
            'drop'            => $this->runDrop($validatedName),
            'empty'           => $this->runEmpty($validatedName),
            'analyze'         => $this->runAnalyze($validatedName),
            'convert_innodb'  => $this->runConvertInnodb($validatedName),
            default           => ['table' => $validatedName, 'status' => 'error', 'detail' => 'internal: unknown action'],
        };
    }

    // -------------------------------------------------------------------------
    // LAYER 2: information_schema name validation
    // -------------------------------------------------------------------------

    /**
     * Validate a table name against the live information_schema.TABLES.
     *
     * Returns the information_schema TABLE_NAME string (the authoritative name
     * for use in SQL) if found, or null if not present.
     *
     * This eliminates SQL injection via table name: the raw input is used only
     * as the prepared-statement bind parameter; the identifier in the actual
     * SQL statement comes from the DB's own catalog.
     *
     * @param string $tableName Raw table name from request.
     * @return string|null
     */
    private function validateTableName(string $tableName): ?string
    {
        if ($this->wpdb === null || !method_exists($this->wpdb, 'prepare') || !method_exists($this->wpdb, 'get_var')) {
            return null;
        }

        $prepared = $this->wpdb->prepare(
            'SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = %s LIMIT 1',
            $tableName
        );

        if (!is_string($prepared)) {
            return null;
        }

        $result = $this->wpdb->get_var($prepared); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- already prepared on the preceding line; information_schema table-name validation; no caching appropriate
        return is_string($result) && $result !== '' ? $result : null;
    }

    // -------------------------------------------------------------------------
    // LAYER 1: live classification for DROP/EMPTY safety gate
    // -------------------------------------------------------------------------

    /**
     * Re-run classification at action time (not at scan time) so a table that
     * had its owning plugin installed between scan and action is not dropped.
     *
     * Uses the cached plugin map transient (built during the most recent db_scan).
     *
     * @param string $validatedName Full table name (from information_schema).
     * @return array{string,string} [owner_type, belongs_to]
     */
    private function classifyLive(string $validatedName): array
    {
        $engine = $this->cleanup ?? new DbCleanup();

        $prefix = $this->prefix();

        // Collect plugin/theme metadata (same as scanTableInventory does).
        $allPluginMeta    = $this->getAllPluginMeta();
        $allThemeMeta     = $this->getAllThemeMeta();
        $activePluginSlugs = $this->getActivePluginSlugs();
        $activeThemeSlugs  = $this->getActiveThemeSlugs();

        return $engine->classifyTable(
            $validatedName,
            $prefix,
            $activePluginSlugs,
            $allPluginMeta,
            $activeThemeSlugs,
            $allThemeMeta
        );
    }

    // -------------------------------------------------------------------------
    // SQL runners
    // -------------------------------------------------------------------------

    /**
     * Run OPTIMIZE TABLE on the validated table name.
     *
     * @param string $table Validated table name from information_schema.
     * @return array{table:string,status:string,detail:string}
     */
    private function runOptimize(string $table): array
    {
        if ($this->wpdb === null || !method_exists($this->wpdb, 'query')) {
            return ['table' => $table, 'status' => 'error', 'detail' => 'wpdb unavailable'];
        }

        $result = $this->wpdb->query($this->wpdb->prepare('OPTIMIZE TABLE %i', $table)); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- table identifier validated against information_schema, parameterized with the %i identifier placeholder (WP 6.2+); DDL statement has no bind values

        if ($result === false) {
            // $wpdb->last_error is a public PROPERTY, not a method, so the old
            // method_exists() check was always false and collapsed every failure to
            // "unknown error". Read the property directly so a failed DROP/REPAIR/
            // OPTIMIZE surfaces the real MySQL error.
            $lastError = isset($this->wpdb->last_error)
                && is_string($this->wpdb->last_error)
                && $this->wpdb->last_error !== ''
                ? $this->wpdb->last_error
                : 'unknown error';
            return ['table' => $table, 'status' => 'error', 'detail' => $lastError];
        }

        // Follow-up ANALYZE TABLE to refresh information_schema statistics.
        $this->wpdb->query($this->wpdb->prepare('ANALYZE TABLE %i', $table)); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- table identifier validated against information_schema, parameterized with the %i identifier placeholder (WP 6.2+); DDL statement has no bind values

        return ['table' => $table, 'status' => 'done', 'detail' => ''];
    }

    /**
     * Run REPAIR TABLE on the validated table name.
     *
     * @param string $table Validated table name from information_schema.
     * @return array{table:string,status:string,detail:string}
     */
    private function runRepair(string $table): array
    {
        if ($this->wpdb === null || !method_exists($this->wpdb, 'query')) {
            return ['table' => $table, 'status' => 'error', 'detail' => 'wpdb unavailable'];
        }

        $result = $this->wpdb->query($this->wpdb->prepare('REPAIR TABLE %i', $table)); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- table identifier validated against information_schema, parameterized with the %i identifier placeholder (WP 6.2+); DDL statement has no bind values

        if ($result === false) {
            // $wpdb->last_error is a public PROPERTY, not a method, so the old
            // method_exists() check was always false and collapsed every failure to
            // "unknown error". Read the property directly so a failed DROP/REPAIR/
            // OPTIMIZE surfaces the real MySQL error.
            $lastError = isset($this->wpdb->last_error)
                && is_string($this->wpdb->last_error)
                && $this->wpdb->last_error !== ''
                ? $this->wpdb->last_error
                : 'unknown error';
            return ['table' => $table, 'status' => 'error', 'detail' => $lastError];
        }

        $this->wpdb->query($this->wpdb->prepare('ANALYZE TABLE %i', $table)); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- table identifier validated against information_schema, parameterized with the %i identifier placeholder (WP 6.2+); DDL statement has no bind values

        return ['table' => $table, 'status' => 'done', 'detail' => ''];
    }

    /**
     * Run DROP TABLE IF EXISTS on the validated table name.
     * Only reached after passing the non-core safety gate (LAYER 1): any table
     * whose owner_type is plugin, theme, or orphan is allowed; WP-core tables
     * (owner_type=core) and unclassified tables (owner_type=unknown) are refused
     * before this method is ever called.
     *
     * @param string $table Validated table name from information_schema.
     * @return array{table:string,status:string,detail:string}
     */
    private function runDrop(string $table): array
    {
        if ($this->wpdb === null || !method_exists($this->wpdb, 'query')) {
            return ['table' => $table, 'status' => 'error', 'detail' => 'wpdb unavailable'];
        }

        $result = $this->wpdb->query($this->wpdb->prepare('DROP TABLE IF EXISTS %i', $table)); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching,WordPress.DB.DirectDatabaseQuery.SchemaChange -- table identifier validated against information_schema, parameterized with the %i identifier placeholder (WP 6.2+); DDL statement has no bind values

        if ($result === false) {
            // $wpdb->last_error is a public PROPERTY, not a method, so the old
            // method_exists() check was always false and collapsed every failure to
            // "unknown error". Read the property directly so a failed DROP/REPAIR/
            // OPTIMIZE surfaces the real MySQL error.
            $lastError = isset($this->wpdb->last_error)
                && is_string($this->wpdb->last_error)
                && $this->wpdb->last_error !== ''
                ? $this->wpdb->last_error
                : 'unknown error';
            return ['table' => $table, 'status' => 'error', 'detail' => $lastError];
        }

        return ['table' => $table, 'status' => 'done', 'detail' => ''];
    }

    /**
     * Run TRUNCATE TABLE on the validated table name.
     * Only reached after passing the non-core safety gate (LAYER 1): any table
     * whose owner_type is plugin, theme, orphan, or unknown is allowed; WP-core
     * tables (owner_type=core) are refused before this method is ever called.
     *
     * The table name used in the SQL statement is the information_schema-validated
     * $table value, never the raw request input.
     *
     * @param string $table Validated table name from information_schema.
     * @return array{table:string,status:string,detail:string}
     */
    private function runEmpty(string $table): array
    {
        if ($this->wpdb === null || !method_exists($this->wpdb, 'query')) {
            return ['table' => $table, 'status' => 'error', 'detail' => 'wpdb unavailable'];
        }

        $result = $this->wpdb->query($this->wpdb->prepare('TRUNCATE TABLE %i', $table)); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- table identifier validated against information_schema, parameterized with the %i identifier placeholder (WP 6.2+); DDL statement has no bind values

        if ($result === false) {
            // $wpdb->last_error is a public PROPERTY, not a method, so the old
            // method_exists() check was always false and collapsed every failure to
            // "unknown error". Read the property directly so a failed DROP/REPAIR/
            // OPTIMIZE surfaces the real MySQL error.
            $lastError = isset($this->wpdb->last_error)
                && is_string($this->wpdb->last_error)
                && $this->wpdb->last_error !== ''
                ? $this->wpdb->last_error
                : 'unknown error';
            return ['table' => $table, 'status' => 'error', 'detail' => $lastError];
        }

        $this->wpdb->query($this->wpdb->prepare('ANALYZE TABLE %i', $table)); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- table identifier validated against information_schema, parameterized with the %i identifier placeholder (WP 6.2+); DDL statement has no bind values

        return ['table' => $table, 'status' => 'done', 'detail' => ''];
    }

    /**
     * Run ANALYZE TABLE on the validated table name.
     *
     * Issues ANALYZE TABLE to refresh index statistics and TABLE_ROWS in
     * information_schema. Safe on any table (InnoDB, MyISAM, or other engine),
     * instantaneous on most workloads. No LAYER-1 owner_type gate — analyzing a
     * WP-core table is a legitimate, non-destructive operation.
     *
     * @param string $table Validated table name from information_schema.
     * @return array{table:string,status:string,detail:string}
     */
    private function runAnalyze(string $table): array
    {
        if ($this->wpdb === null || !method_exists($this->wpdb, 'query')) {
            return ['table' => $table, 'status' => 'error', 'detail' => 'wpdb unavailable'];
        }

        $result = $this->wpdb->query($this->wpdb->prepare('ANALYZE TABLE %i', $table)); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- table identifier validated against information_schema, parameterized with the %i identifier placeholder (WP 6.2+); DDL statement has no bind values

        if ($result === false) {
            $lastError = isset($this->wpdb->last_error)
                && is_string($this->wpdb->last_error)
                && $this->wpdb->last_error !== ''
                ? $this->wpdb->last_error
                : 'unknown error';
            return ['table' => $table, 'status' => 'error', 'detail' => $lastError];
        }

        return ['table' => $table, 'status' => 'done', 'detail' => ''];
    }

    /**
     * Run ALTER TABLE ... ENGINE=InnoDB on the validated table name.
     *
     * Converts the table to the InnoDB storage engine. Data is preserved
     * (non-destructive), but the table is briefly locked while being rebuilt.
     * If the table is already InnoDB the ALTER is a harmless rebuild and still
     * returns done. On success, a follow-up ANALYZE TABLE refreshes statistics,
     * mirroring the pattern in runOptimize() and runRepair().
     *
     * No LAYER-1 owner_type gate — converting a WP-core table (e.g. wp_posts)
     * from MyISAM to InnoDB is a legitimate, safe improvement.
     *
     * @param string $table Validated table name from information_schema.
     * @return array{table:string,status:string,detail:string}
     */
    private function runConvertInnodb(string $table): array
    {
        if ($this->wpdb === null || !method_exists($this->wpdb, 'query')) {
            return ['table' => $table, 'status' => 'error', 'detail' => 'wpdb unavailable'];
        }

        $result = $this->wpdb->query($this->wpdb->prepare('ALTER TABLE %i ENGINE=InnoDB', $table)); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- table identifier validated against information_schema, parameterized with the %i identifier placeholder (WP 6.2+); DDL statement has no bind values

        if ($result === false) {
            $lastError = isset($this->wpdb->last_error)
                && is_string($this->wpdb->last_error)
                && $this->wpdb->last_error !== ''
                ? $this->wpdb->last_error
                : 'unknown error';
            return ['table' => $table, 'status' => 'error', 'detail' => $lastError];
        }

        // Follow-up ANALYZE TABLE to refresh information_schema statistics after
        // the engine conversion, mirroring the pattern in runOptimize/runRepair.
        $this->wpdb->query($this->wpdb->prepare('ANALYZE TABLE %i', $table)); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- table identifier validated against information_schema, parameterized with the %i identifier placeholder (WP 6.2+); DDL statement has no bind values

        return ['table' => $table, 'status' => 'done', 'detail' => ''];
    }

    // -------------------------------------------------------------------------
    // WP helpers (mirrors DbCleanup's private helpers; duplicated here so the
    // command can classify tables without requiring a DbCleanup subclass).
    // -------------------------------------------------------------------------

    private function prefix(): string
    {
        return ($this->wpdb !== null && isset($this->wpdb->prefix) && is_string($this->wpdb->prefix))
            ? $this->wpdb->prefix
            : 'wp_';
    }

    /**
     * @return array<string,string>
     */
    private function getAllPluginMeta(): array
    {
        if (!function_exists('get_plugins')) {
            return [];
        }
        $plugins = get_plugins();
        if (!is_array($plugins)) {
            return [];
        }
        $meta = [];
        foreach ($plugins as $path => $data) {
            if (!is_string($path) || $path === '') {
                continue;
            }
            $parts = explode('/', $path, 2);
            $slug  = $parts[0];
            if ($slug === '') {
                continue;
            }
            $name        = is_array($data) && isset($data['Name']) && is_string($data['Name']) && $data['Name'] !== ''
                ? $data['Name']
                : $slug;
            $meta[$slug] = $name;
        }
        return $meta;
    }

    /**
     * @return array<string,string>
     */
    private function getAllThemeMeta(): array
    {
        if (!function_exists('wp_get_themes')) {
            return [];
        }
        $themes = wp_get_themes();
        if (!is_array($themes)) {
            return [];
        }
        $meta = [];
        foreach ($themes as $slug => $theme) {
            if (!is_string($slug) || $slug === '') {
                continue;
            }
            $name = $slug;
            if (is_object($theme) && method_exists($theme, 'get')) {
                $n = $theme->get('Name');
                if (is_string($n) && $n !== '') {
                    $name = $n;
                }
            } elseif (is_array($theme) && isset($theme['Name']) && is_string($theme['Name']) && $theme['Name'] !== '') {
                $name = $theme['Name'];
            }
            $meta[$slug] = $name;
        }
        return $meta;
    }

    /**
     * @return list<string>
     */
    private function getActivePluginSlugs(): array
    {
        if (!function_exists('get_option')) {
            return [];
        }
        $active = get_option('active_plugins');
        if (!is_array($active)) {
            return [];
        }
        $slugs = [];
        foreach ($active as $path) {
            if (!is_string($path) || $path === '') {
                continue;
            }
            $parts = explode('/', $path, 2);
            $slug  = $parts[0];
            if ($slug !== '') {
                $slugs[] = $slug;
            }
        }
        return $slugs;
    }

    /**
     * @return list<string>
     */
    private function getActiveThemeSlugs(): array
    {
        $slugs = [];
        if (function_exists('get_stylesheet')) {
            $s = get_stylesheet();
            if (is_string($s) && $s !== '') {
                $slugs[] = $s;
            }
        }
        if (function_exists('get_template')) {
            $t = get_template();
            if (is_string($t) && $t !== '' && !in_array($t, $slugs, true)) {
                $slugs[] = $t;
            }
        }
        return $slugs;
    }
}
