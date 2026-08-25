<?php
/**
 * SearchReplaceCommand — generic serialization-safe database search-replace tool.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/search_replace
 *   Authorization: Bearer <Ed25519 JWT, cmd="search_replace", aud=<siteId>>
 *   Body: {
 *     "job_id":   "<UUID v4, required>",
 *     "search":   "<string to find, min 3 chars, required>",
 *     "replace":  "<replacement string, required>",
 *     "dry_run":  <bool, default true>,
 *     "tables":   ["<table_name>", ...] // optional allowlist; empty = all tables
 *   }
 *
 * Response (synchronous):
 *   {
 *     "ok":             true,
 *     "job_id":         "<echoed uuid>",
 *     "tables_scanned": N,
 *     "rows_matched":   N,   // rows containing at least one occurrence of search
 *     "rows_changed":   N    // 0 when dry_run=true; actual updated rows otherwise
 *   }
 *   { "ok": false, "detail": "<reason>" }  // on validation failure
 *
 * Safety invariants:
 *   1. Minimum search-string length: 3 bytes. Prevents over-broad blanket rewrites.
 *   2. All replacements are serialization-safe via UrlRewriter::rewrite_row_data.
 *      PHP-serialized blobs are unserialized, walked, rewritten, and re-serialized
 *      so s:NN: length prefixes are always recomputed — never a naive str_replace.
 *   3. Table denylist from UrlRewriter::DENYLIST_TABLES is respected.
 *   4. Binary/blob column types and posts.guid are always skipped.
 *   5. Parameterized SQL throughout — mysqli::real_escape_string for all values
 *      embedded in SQL literals; table/column identifiers are backtick-escaped.
 *   6. dry_run=true (preview): counts matches without writing. The caller MUST
 *      present the operator with the count before a dry_run=false apply.
 *
 * Auth: Router::authorize() enforces the signed JWT + anti-replay contract before
 * execute() is ever called. current_user_can('manage_options') is the WP-layer
 * defense-in-depth gate (also enforced by Router).
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Backup\UrlRewriter;
use WPMgr\Agent\Support\DebugLog;

/**
 * Generic serialization-safe database search-replace with dry-run preview mode.
 */
final class SearchReplaceCommand implements CommandInterface
{
    /**
     * Minimum bytes required in the search string. Guards against an accidental
     * empty or single-character search that would match essentially every row.
     */
    private const MIN_SEARCH_LENGTH = 3;

    /**
     * Maximum rows fetched per pagination page. Mirrors DbRestorer::rewriteAllTables.
     */
    private const ROWS_PER_PAGE = 5000;

    /**
     * Soft cap on the caller-supplied table allowlist size.
     */
    private const MAX_TABLES_ALLOWLIST = 200;

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'search_replace';
    }

    /**
     * Validate the request and run the serialization-safe search-replace (or
     * dry-run count) synchronously.
     *
     * @param array<string,mixed> $claims Validated JWT claims (auth already enforced by Router).
     * @param array<string,mixed> $params {
     *     job_id:  string  (required),
     *     search:  string  (required, min 3 bytes),
     *     replace: string  (required),
     *     dry_run: bool    (default true),
     *     tables:  string[] (optional allowlist)
     * }
     * @return array{ok:bool,job_id?:string,tables_scanned?:int,rows_matched?:int,rows_changed?:int,detail?:string}
     */
    public function execute(array $claims, array $params): array
    {
        // ------------------------------------------------------------------ //
        // Input validation                                                     //
        // ------------------------------------------------------------------ //

        $jobId = isset($params['job_id']) && is_string($params['job_id']) && $params['job_id'] !== ''
            ? $params['job_id']
            : '';
        if ($jobId === '') {
            return ['ok' => false, 'detail' => 'missing job_id'];
        }

        $search = isset($params['search']) && is_string($params['search'])
            ? $params['search']
            : '';
        if (strlen($search) < self::MIN_SEARCH_LENGTH) {
            return ['ok' => false, 'detail' => 'search must be at least ' . self::MIN_SEARCH_LENGTH . ' characters'];
        }

        if (!isset($params['replace']) || !is_string($params['replace'])) {
            return ['ok' => false, 'detail' => 'replace must be a string'];
        }
        $replace = (string) $params['replace'];

        // dry_run defaults to TRUE: the operator must explicitly opt into writing.
        $dryRun = isset($params['dry_run']) ? (bool) $params['dry_run'] : true;

        // Optional table allowlist. Entries are filtered for non-empty strings.
        $tablesAllowlist = [];
        if (isset($params['tables']) && is_array($params['tables'])) {
            foreach ($params['tables'] as $t) {
                if (is_string($t) && $t !== '') {
                    $tablesAllowlist[] = $t;
                }
            }
            if (count($tablesAllowlist) > self::MAX_TABLES_ALLOWLIST) {
                return ['ok' => false, 'detail' => 'tables allowlist exceeds maximum of ' . self::MAX_TABLES_ALLOWLIST];
            }
        }

        // ------------------------------------------------------------------ //
        // Build the serialization-safe replacement pair.                       //
        //                                                                       //
        // UrlRewriter::rewrite_row_data accepts any [$from, $to] tuple — the   //
        // serialization-safe walk (unserialize/rewrite/re-serialize) is        //
        // completely generic. We supply one pair: the operator's literal        //
        // search and replace strings.                                           //
        // ------------------------------------------------------------------ //
        /** @var array{0: list<string>, 1: list<string>} $replacements */
        $replacements = [[$search], [$replace]];

        // ------------------------------------------------------------------ //
        // Run the walk.                                                         //
        // ------------------------------------------------------------------ //
        try {
            $result = $this->runReplace($replacements, $dryRun, $tablesAllowlist);
        } catch (\Throwable $e) {
            DebugLog::write('WPMgr SearchReplaceCommand: ' . $e->getMessage());
            return ['ok' => false, 'detail' => 'internal error'];
        }

        return [
            'ok'             => true,
            'job_id'         => $jobId,
            'tables_scanned' => $result['tables_scanned'],
            'rows_matched'   => $result['rows_matched'],
            'rows_changed'   => $result['rows_changed'],
        ];
    }

    // -----------------------------------------------------------------------
    // Private: table walk
    // -----------------------------------------------------------------------

    /**
     * Walk every eligible table, apply the serialization-safe replacer row by
     * row, and return summary counts.
     *
     * When $dryRun is true the method counts matched rows but emits no UPDATE
     * statements. When false it issues batched UPDATE statements via a fresh
     * mysqli connection (separate from wpdb to avoid session-variable leakage).
     *
     * @param array{0: list<string>, 1: list<string>} $replacements
     * @param bool                                     $dryRun
     * @param list<string>                             $tablesAllowlist
     * @return array{tables_scanned:int,rows_matched:int,rows_changed:int}
     */
    private function runReplace(array $replacements, bool $dryRun, array $tablesAllowlist): array
    {
        global $wpdb;
        if (!isset($wpdb) || !is_object($wpdb)) {
            throw new \RuntimeException('wpdb not available');
        }
        /** @var \wpdb $wpdb */

        $prefix        = (string) ($wpdb->prefix ?? '');
        $mysqli        = $this->openMysqli();
        $tablesScanned = 0;
        $rowsMatched   = 0;
        $rowsChanged   = 0;

        try {
            $tables = $this->listTables($mysqli, $prefix, $tablesAllowlist);

            foreach ($tables as $tableName) {
                if (UrlRewriter::should_skip_table($tableName, $prefix)) {
                    continue;
                }

                $columns    = $this->describeColumns($mysqli, $tableName);
                $primaryKey = $this->detectPrimaryKey($mysqli, $tableName);

                if ($columns === [] || $primaryKey === '') {
                    continue;
                }

                $tablesScanned++;
                $bare = ($prefix !== '' && strpos($tableName, $prefix) === 0)
                    ? substr($tableName, strlen($prefix))
                    : $tableName;

                $offset = 0;
                while (true) {
                    $sql  = sprintf(
                        'SELECT * FROM `%s` LIMIT %d OFFSET %d',
                        $this->escIdent($tableName),
                        self::ROWS_PER_PAGE,
                        $offset
                    );
                    $rows = @$mysqli->query($sql);
                    if ($rows === false) {
                        break;
                    }

                    $batch      = '';
                    $batchBytes = 0;
                    $pageCount  = 0;

                    while ($row = $rows->fetch_assoc()) {
                        $pageCount++;
                        $sets = [];

                        foreach ($row as $colName => $value) {
                            if (!isset($columns[$colName])) {
                                continue;
                            }
                            if ($value === null) {
                                continue;
                            }
                            if (UrlRewriter::should_skip_column($tableName, $colName, $prefix, $columns[$colName])) {
                                continue;
                            }
                            if ($bare === 'posts' && $colName === 'guid') {
                                continue;
                            }

                            $rewritten = UrlRewriter::rewrite_row_data((string) $value, $replacements);
                            if ($rewritten === (string) $value) {
                                continue;
                            }
                            $sets[$colName] = $rewritten;
                        }

                        if ($sets === []) {
                            continue;
                        }

                        $pkValue = $row[$primaryKey] ?? null;
                        if ($pkValue === null) {
                            continue;
                        }

                        $rowsMatched++;

                        if ($dryRun) {
                            // Preview only — do not write.
                            continue;
                        }

                        $setSql = [];
                        foreach ($sets as $col => $val) {
                            $setSql[] = '`' . $this->escIdent($col) . "`='" . $mysqli->real_escape_string($val) . "'";
                        }
                        $stmt = sprintf(
                            "UPDATE `%s` SET %s WHERE `%s`='%s';",
                            $this->escIdent($tableName),
                            implode(',', $setSql),
                            $this->escIdent($primaryKey),
                            $mysqli->real_escape_string((string) $pkValue)
                        );
                        $stmtLen = strlen($stmt);

                        if ($batchBytes > 0 && ($batchBytes + $stmtLen) > 1_000_000) {
                            $rowsChanged += $this->flushBatch($mysqli, $batch);
                            $batch        = '';
                            $batchBytes   = 0;
                        }
                        $batch      .= $stmt . "\n";
                        $batchBytes += $stmtLen + 1;
                    }
                    $rows->free();

                    if ($batch !== '') {
                        $rowsChanged += $this->flushBatch($mysqli, $batch);
                    }

                    if ($pageCount < self::ROWS_PER_PAGE) {
                        break;
                    }
                    $offset += $pageCount;
                }
            }
        } finally {
            @$mysqli->close();
        }

        return [
            'tables_scanned' => $tablesScanned,
            'rows_matched'   => $rowsMatched,
            'rows_changed'   => $dryRun ? 0 : $rowsChanged,
        ];
    }

    /**
     * List tables to scan: caller's allowlist (validated against information_schema)
     * or all tables starting with the WP prefix.
     *
     * @param \mysqli      $mysqli
     * @param string       $prefix
     * @param list<string> $tablesAllowlist
     * @return list<string>
     */
    private function listTables(\mysqli $mysqli, string $prefix, array $tablesAllowlist): array
    {
        if ($tablesAllowlist !== []) {
            $db  = $this->currentDatabase($mysqli);
            $out = [];
            foreach ($tablesAllowlist as $table) {
                $escaped = $mysqli->real_escape_string($table);
                $dbEsc   = $mysqli->real_escape_string($db);
                $res     = @$mysqli->query(
                    "SELECT TABLE_NAME FROM information_schema.TABLES"
                    . " WHERE TABLE_SCHEMA='" . $dbEsc . "' AND TABLE_NAME='" . $escaped . "' LIMIT 1"
                );
                if ($res !== false) {
                    $row = $res->fetch_row();
                    $res->free();
                    if (is_array($row) && isset($row[0]) && is_string($row[0])) {
                        $out[] = $row[0];
                    }
                }
            }
            return $out;
        }

        if ($prefix !== '') {
            $like = $mysqli->real_escape_string($prefix . '%');
            $res  = @$mysqli->query("SHOW TABLES LIKE '" . $like . "'");
        } else {
            $res = @$mysqli->query("SHOW TABLES");
        }
        if ($res === false) {
            return [];
        }
        $out = [];
        while ($row = $res->fetch_row()) {
            if (is_array($row) && isset($row[0]) && is_string($row[0])) {
                $out[] = $row[0];
            }
        }
        $res->free();
        return $out;
    }

    /** Return the current database name (for information_schema lookups). */
    private function currentDatabase(\mysqli $mysqli): string
    {
        $res = @$mysqli->query('SELECT DATABASE()');
        if ($res === false) {
            return '';
        }
        $row = $res->fetch_row();
        $res->free();
        return (is_array($row) && isset($row[0]) && is_string($row[0])) ? $row[0] : '';
    }

    /**
     * Describe columns: returns column_name => column_type map.
     *
     * @return array<string,string>
     */
    private function describeColumns(\mysqli $mysqli, string $table): array
    {
        $res = @$mysqli->query('SHOW COLUMNS FROM `' . $this->escIdent($table) . '`');
        if ($res === false) {
            return [];
        }
        $out = [];
        while ($row = $res->fetch_assoc()) {
            $field = isset($row['Field']) ? (string) $row['Field'] : '';
            $type  = isset($row['Type'])  ? strtolower((string) $row['Type']) : '';
            if ($field !== '') {
                $out[$field] = $type;
            }
        }
        $res->free();
        return $out;
    }

    /**
     * Detect the primary-key column. Returns '' when none found (table is skipped).
     */
    private function detectPrimaryKey(\mysqli $mysqli, string $table): string
    {
        $res = @$mysqli->query('SHOW KEYS FROM `' . $this->escIdent($table) . "` WHERE Key_name = 'PRIMARY'");
        if ($res === false) {
            return '';
        }
        $pk = '';
        while ($row = $res->fetch_assoc()) {
            if ($pk === '' && isset($row['Column_name'])) {
                $pk = (string) $row['Column_name'];
            }
        }
        $res->free();
        if ($pk !== '') {
            return $pk;
        }
        // Fallback: any UNIQUE key (wp_options.option_name case).
        $res = @$mysqli->query('SHOW KEYS FROM `' . $this->escIdent($table) . "` WHERE Non_unique = 0");
        if ($res === false) {
            return '';
        }
        while ($row = $res->fetch_assoc()) {
            if ($pk === '' && isset($row['Column_name'])) {
                $pk = (string) $row['Column_name'];
            }
        }
        $res->free();
        return $pk;
    }

    /**
     * Flush a batch of UPDATE statements via multi_query and drain results.
     * Returns the number of UPDATE statements executed.
     */
    private function flushBatch(\mysqli $mysqli, string $batch): int
    {
        if ($batch === '') {
            return 0;
        }
        $count = substr_count($batch, ";\n");
        @$mysqli->multi_query($batch);
        do {
            if ($r = @$mysqli->store_result()) {
                $r->free();
            }
        } while (@$mysqli->more_results() && @$mysqli->next_result());
        return $count;
    }

    /**
     * Open a dedicated mysqli connection from the WordPress DB constants.
     * Does NOT reuse the global $wpdb connection (avoids session-variable leakage).
     *
     * @throws \RuntimeException On connection failure.
     */
    private function openMysqli(): \mysqli
    {
        if (!defined('DB_HOST') || !defined('DB_USER') || !defined('DB_PASSWORD') || !defined('DB_NAME')) {
            throw new \RuntimeException('WordPress DB constants not defined');
        }

        $host = (string) constant('DB_HOST');
        $user = (string) constant('DB_USER');
        $pass = (string) constant('DB_PASSWORD');
        $name = (string) constant('DB_NAME');
        $port = 3306;
        $sock = null;

        if (strpos($host, ':') !== false) {
            [$h, $rest] = explode(':', $host, 2);
            $host       = $h;
            if ($rest !== '' && $rest[0] === '/') {
                $sock = $rest;
            } elseif (ctype_digit($rest)) {
                $port = (int) $rest;
            }
        }

        $mysqli = @new \mysqli($host, $user, $pass, $name, $port, $sock ?? ''); // phpcs:ignore WordPress.DB.RestrictedClasses.mysql__mysqli -- dedicated streaming connection for search-replace; $wpdb buffers the full result set (OOM risk)

        if ($mysqli->connect_errno !== 0) {
            throw new \RuntimeException('SearchReplaceCommand: DB connect failed: ' . esc_html((string) $mysqli->connect_error)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to server log/SSE, not browser output
        }

        @$mysqli->query("SET SESSION sql_mode = 'ALLOW_INVALID_DATES,NO_AUTO_VALUE_ON_ZERO'");
        @$mysqli->query('SET NAMES utf8mb4');

        return $mysqli;
    }

    /**
     * Escape an identifier for safe use between backticks.
     * Doubles any existing backtick as per MySQL quoting rules.
     */
    private function escIdent(string $name): string
    {
        return str_replace('`', '``', $name);
    }
}
