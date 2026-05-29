<?php
/**
 * DbRestorer: M5.6 / ADR-034 — WPvivid-pattern DB restorer.
 *
 * Replays a `.sql.gz` dump into the live database under a temporary table
 * prefix (`tmp<short>_X`), then atomically swaps each tmp table over the live
 * one with `DROP TABLE IF EXISTS wp_X; RENAME TABLE tmp_X TO wp_X;`. The whole
 * point is documented in `docs/research/wpvivid-restore-deep-dive.md` §1.3:
 *
 *   "Every CREATE/INSERT/DROP rewrites the table name from `wp_X` to
 *    `tmp<id>_X` before execution. So WordPress keeps reading and writing
 *    `wp_options` ... the whole time. Only at the very end ... does
 *    `rename_db()` swap things atomically per-table."
 *
 * Specifically we adopt from WPvivid (§7.1) and FIX from WPvivid (§7.2):
 *
 *   KEEP:
 *     - tmp-prefix + per-table RENAME swap under SET FOREIGN_KEY_CHECKS=0
 *     - SET SESSION sql_mode = 'ALLOW_INVALID_DATES,NO_AUTO_VALUE_ON_ZERO,...'
 *       (makes 5+ year old WP dumps replay on strict-mode MySQL 8)
 *     - Permissive per-statement error handling — log+continue, never abort
 *
 *   FIX:
 *     - Proper SQL statement parser. State machine tracks the quote and
 *       comment regions (single/double-quoted strings, backtick identifiers,
 *       block comments, dash-dash line comments) so a `;` inside any of those
 *       is NOT a statement terminator.
 *       WPvivid splits on lines ending in `;`, which breaks on multi-line
 *       statements with embedded ; inside strings or comments. We need this
 *       because Phase 2 SubmitManifest may include user-edited dumps and
 *       arbitrary-payload string columns.
 *     - Identifier-only prefix rewrite (not blanket str_replace): only catch
 *       table identifiers at statement-start position, never inside string
 *       literals.
 *
 * Wire pattern with the RestoreRunner:
 *   `restore($sqlGzPath, $tmpPrefix, $sourcePrefix, $progress) -> tmp tables list`
 *   `swap($tmpPrefix, $targetPrefix, $tmpTables, $progress) -> void`
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

/**
 * SQL dump replayer + per-table atomic swap. Constructs its own mysqli
 * connection from the supplied credentials — same shape as DbDumper, and
 * for the same reason: keep the long-running restore transaction off the
 * shared $wpdb connection.
 */
final class DbRestorer
{
    /** Emit a progress event every N executed statements. */
    private const PROGRESS_EVERY_STATEMENTS = 200;

    /**
     * Read chunk size for the gzopen stream. 256 KiB is the sweet spot — big
     * enough that a single gzread amortises the gzip header overhead, small
     * enough that a 256 MiB dump never holds more than 256 KiB of plaintext
     * in memory at once.
     */
    private const READ_CHUNK_BYTES = 262144;

    /**
     * @var array{host:string,user:string,password:string,name:string,prefix:string}
     */
    private array $db;

    /**
     * @param array{host:string,user:string,password:string,name:string,prefix:string} $db
     *      WordPress DB credentials.
     */
    public function __construct(array $db)
    {
        $this->db = $db;
    }

    /**
     * Replay a `.sql.gz` dump into tmp tables.
     *
     * @param string   $sqlGzPath    Absolute path to the dump.
     * @param string   $tmpPrefix    Target tmp prefix (e.g. `tmpAB12_`). MUST
     *                                end with `_`.
     * @param string   $sourcePrefix The prefix the dump was made with (e.g.
     *                                `wp_`). Any identifier starting with
     *                                this prefix will be rewritten to
     *                                $tmpPrefix.
     * @param callable $progress     function(string $phase, array $detail): void
     * @return list<string> Names of the tmp tables that ended up populated.
     * @throws \RuntimeException On fatal connection / read failure.
     */
    public function restore(string $sqlGzPath, string $tmpPrefix, string $sourcePrefix, callable $progress): array
    {
        if (!is_file($sqlGzPath)) {
            throw new \RuntimeException('DbRestorer: dump file missing: ' . $sqlGzPath);
        }
        if ($tmpPrefix === '' || substr($tmpPrefix, -1) !== '_') {
            throw new \RuntimeException('DbRestorer: tmpPrefix must end with "_"');
        }
        if ($sourcePrefix === '') {
            throw new \RuntimeException('DbRestorer: sourcePrefix empty');
        }

        @set_time_limit(0);
        @ignore_user_abort(true);

        $mysqli = $this->connect();

        try {
            $this->configureSession($mysqli);

            $handle = @gzopen($sqlGzPath, 'rb');
            if ($handle === false) {
                throw new \RuntimeException('DbRestorer: cannot gzopen dump: ' . $sqlGzPath);
            }

            $stmtBuffer  = '';
            $statements  = 0;
            $errors      = 0;
            $sinceTick   = 0;
            $currentTbl  = '';
            $touchedTbls = []; // hash set: tmp table name => 1

            // Wrap the whole replay in a transaction. Per-statement errors are
            // LOGGED and continued; the COMMIT at the end finalises whatever
            // landed. Matches WPvivid's amortised-fsync approach (deep-dive
            // §1.4): commit is unconditional, even after errors.
            @$mysqli->query('START TRANSACTION');

            try {
                while (!gzeof($handle)) {
                    $buf = gzread($handle, self::READ_CHUNK_BYTES);
                    if ($buf === false) {
                        throw new \RuntimeException('DbRestorer: gzread failed');
                    }
                    if ($buf === '') {
                        break;
                    }
                    $stmtBuffer .= $buf;

                    // Parse out complete statements from the buffer. The
                    // parser hands us a list of complete statements + the
                    // tail (incomplete) bytes to carry into the next read.
                    [$complete, $tail] = self::splitStatements($stmtBuffer);
                    $stmtBuffer        = $tail;

                    foreach ($complete as $rawStmt) {
                        $stmt = self::rewritePrefix($rawStmt, $sourcePrefix, $tmpPrefix, $currentTbl);
                        if ($stmt === '') {
                            continue;
                        }

                        // Track tmp tables that we end up touching, so swap()
                        // doesn't need a SHOW TABLES round-trip.
                        if ($currentTbl !== '' && strpos($currentTbl, $tmpPrefix) === 0) {
                            $touchedTbls[$currentTbl] = 1;
                        }

                        $ok = @$mysqli->query($stmt);
                        if ($ok === false) {
                            // Log-and-continue — WPvivid §1.4 semantics.
                            error_log(sprintf(
                                'WPMgr DbRestorer: statement #%d failed: %s',
                                $statements + 1,
                                substr((string) $mysqli->error, 0, 240)
                            ));
                            $errors++;
                        }

                        $statements++;
                        $sinceTick++;

                        if ($sinceTick >= self::PROGRESS_EVERY_STATEMENTS) {
                            self::safeProgress($progress, 'restore_db', [
                                'statements_done' => $statements,
                                'errors'          => $errors,
                                'current_table'   => $currentTbl,
                                'tables_touched'  => count($touchedTbls),
                            ]);
                            $sinceTick = 0;
                        }
                    }
                }

                // Flush any final unterminated statement (typically just
                // trailing whitespace / a comment with no `;`).
                $tail = trim($stmtBuffer);
                if ($tail !== '' && !self::looksLikeCommentOnly($tail)) {
                    $stmt = self::rewritePrefix($tail, $sourcePrefix, $tmpPrefix, $currentTbl);
                    if ($stmt !== '') {
                        $ok = @$mysqli->query($stmt);
                        if ($ok === false) {
                            $errors++;
                        }
                        $statements++;
                        if ($currentTbl !== '' && strpos($currentTbl, $tmpPrefix) === 0) {
                            $touchedTbls[$currentTbl] = 1;
                        }
                    }
                }

                @$mysqli->query('COMMIT');
            } finally {
                gzclose($handle);
            }

            self::safeProgress($progress, 'restore_db', [
                'done'            => true,
                'statements_done' => $statements,
                'errors'          => $errors,
                'tables_touched'  => count($touchedTbls),
            ]);

            return array_keys($touchedTbls);
        } finally {
            // Reset session flags before releasing the connection.
            @$mysqli->query('SET FOREIGN_KEY_CHECKS=1');
            @$mysqli->close();
        }
    }

    /**
     * Atomically swap each tmp table over the live target table.
     *
     * Per-table SQL:
     *   SET FOREIGN_KEY_CHECKS=0;
     *   DROP TABLE IF EXISTS `wp_X`;
     *   RENAME TABLE `tmp<id>_X` TO `wp_X`;
     *   SET FOREIGN_KEY_CHECKS=1;
     *
     * @param string       $tmpPrefix    The prefix the restored data lives under.
     * @param string       $targetPrefix The prefix the live site uses.
     * @param list<string> $tmpTables    Tmp table names returned by restore().
     * @param callable     $progress     function(string $phase, array $detail): void
     * @return void
     * @throws \RuntimeException On a swap failure for any table.
     */
    public function swap(string $tmpPrefix, string $targetPrefix, array $tmpTables, callable $progress): void
    {
        if ($tmpTables === []) {
            // Nothing to swap (e.g. a db-restore that touched zero tables) —
            // emit a done event and bail.
            self::safeProgress($progress, 'swap_db', [
                'done'         => true,
                'tables_done'  => 0,
                'tables_total' => 0,
            ]);
            return;
        }
        if ($tmpPrefix === '' || $targetPrefix === '') {
            throw new \RuntimeException('DbRestorer::swap: empty prefix');
        }

        @set_time_limit(0);
        @ignore_user_abort(true);

        $mysqli = $this->connect();
        try {
            @$mysqli->query('SET FOREIGN_KEY_CHECKS=0');

            $total      = count($tmpTables);
            $done       = 0;
            $sinceTick  = 0;

            foreach ($tmpTables as $tmpTable) {
                if (!is_string($tmpTable) || $tmpTable === '' || strpos($tmpTable, $tmpPrefix) !== 0) {
                    continue;
                }
                $bare       = substr($tmpTable, strlen($tmpPrefix));
                $targetTable = $targetPrefix . $bare;

                // DROP IF EXISTS the live table.
                $dropSql = 'DROP TABLE IF EXISTS `' . $this->escIdent($targetTable) . '`';
                if (@$mysqli->query($dropSql) === false) {
                    throw new \RuntimeException('DbRestorer::swap: DROP failed for ' . $targetTable . ': ' . $mysqli->error);
                }

                // RENAME tmp_X TO wp_X.
                $renameSql = 'RENAME TABLE `' . $this->escIdent($tmpTable) . '` TO `' . $this->escIdent($targetTable) . '`';
                if (@$mysqli->query($renameSql) === false) {
                    throw new \RuntimeException('DbRestorer::swap: RENAME failed for ' . $tmpTable . ' -> ' . $targetTable . ': ' . $mysqli->error);
                }

                $done++;
                $sinceTick++;

                if ($sinceTick >= 8) {
                    self::safeProgress($progress, 'swap_db', [
                        'tables_done'  => $done,
                        'tables_total' => $total,
                        'current_table' => $targetTable,
                    ]);
                    $sinceTick = 0;
                }
            }

            self::safeProgress($progress, 'swap_db', [
                'done'         => true,
                'tables_done'  => $done,
                'tables_total' => $total,
            ]);
        } finally {
            @$mysqli->query('SET FOREIGN_KEY_CHECKS=1');
            @$mysqli->close();
        }
    }

    /**
     * Drop any stray tmp tables left over from a failed restore. Used by
     * RestoreRunner during cleanup on a failed run.
     *
     * @param string $tmpPrefix Prefix to sweep (e.g. `tmpAB12_`).
     * @return int Number of tables dropped.
     */
    public function dropTmpTables(string $tmpPrefix): int
    {
        if ($tmpPrefix === '') {
            return 0;
        }
        $mysqli = $this->connect();
        try {
            $like = $mysqli->real_escape_string($tmpPrefix . '%');
            $res  = @$mysqli->query("SHOW TABLES LIKE '" . $like . "'");
            if ($res === false) {
                return 0;
            }
            $dropped = 0;
            while ($row = $res->fetch_row()) {
                $name = is_array($row) ? (string) ($row[0] ?? '') : '';
                if ($name === '' || strpos($name, $tmpPrefix) !== 0) {
                    continue;
                }
                if (@$mysqli->query('DROP TABLE IF EXISTS `' . $this->escIdent($name) . '`') !== false) {
                    $dropped++;
                }
            }
            $res->close();
            return $dropped;
        } finally {
            @$mysqli->close();
        }
    }

    // ==================================================================
    // Connection + session
    // ==================================================================

    /**
     * Open a fresh mysqli connection. Mirrors DbDumper::connect — we do NOT
     * reuse the global $wpdb connection because the restore session needs its
     * own sql_mode / FOREIGN_KEY_CHECKS flags and shouldn't leak them.
     *
     * @throws \RuntimeException On connection failure.
     */
    private function connect(): \mysqli
    {
        $host = (string) ($this->db['host'] ?? 'localhost');
        $user = (string) ($this->db['user'] ?? '');
        $pass = (string) ($this->db['password'] ?? '');
        $name = (string) ($this->db['name'] ?? '');
        $port = 3306;
        $sock = null;

        // host may be "host:port" or "localhost:/path/to/socket". Same
        // parsing rules as WordPress's wpdb::parse_db_host.
        if (strpos($host, ':') !== false) {
            [$h, $rest] = explode(':', $host, 2);
            $host       = $h;
            if ($rest !== '' && $rest[0] === '/') {
                $sock = $rest;
            } elseif (ctype_digit($rest)) {
                $port = (int) $rest;
            }
        }

        // Suppress mysqli's default exception/warning chatter so we can
        // surface a clean error.
        $mysqli = @new \mysqli($host, $user, $pass, $name, $port, $sock ?? '');
        if ($mysqli->connect_errno) {
            throw new \RuntimeException('DbRestorer: connect failed: ' . $mysqli->connect_error);
        }
        @$mysqli->set_charset('utf8mb4');
        return $mysqli;
    }

    /**
     * Apply the WPvivid-pattern session config that makes legacy dumps
     * replay cleanly on strict-mode MySQL 8.
     */
    private function configureSession(\mysqli $mysqli): void
    {
        // Permissive sql_mode. Strip NO_ENGINE_SUBSTITUTION (it breaks old
        // dumps that named non-default engines no longer present); add
        // ALLOW_INVALID_DATES + NO_AUTO_VALUE_ON_ZERO (the canonical
        // WP-on-MySQL-8 fix). Deep-dive §1.6.
        @$mysqli->query("SET SESSION sql_mode = 'ALLOW_INVALID_DATES,NO_AUTO_VALUE_ON_ZERO'");
        // Don't fight FK constraints during the import — we'll restore the
        // checks after swap.
        @$mysqli->query('SET FOREIGN_KEY_CHECKS=0');
        // Don't fight unique-index violations either: the import may emit
        // INSERTs in dependency-broken order on rare schemas.
        @$mysqli->query('SET UNIQUE_CHECKS=0');
    }

    // ==================================================================
    // SQL parsing
    // ==================================================================

    /**
     * Split a SQL buffer into complete statements + trailing remainder.
     *
     * State machine: tracks whether we're inside `'string'`, `"string"`,
     * `` `identifier` ``, `/* block comment * /`, or `-- line comment` so a
     * `;` inside any of those is NOT a statement terminator. WPvivid's
     * `while(!feof(...)){ fgets+endWith==';' }` parser explodes on these,
     * which is why we ship our own.
     *
     * @param string $buf Raw SQL bytes (a concatenation of previous tail +
     *                    one fresh gzread).
     * @return array{0:list<string>,1:string} List of complete statements
     *                                          (trimmed of leading whitespace,
     *                                          NO trailing `;`), plus the
     *                                          remaining incomplete tail.
     */
    public static function splitStatements(string $buf): array
    {
        $statements = [];
        $len        = strlen($buf);
        $i          = 0;
        $stmtStart  = 0;

        // Parser state.
        $inSingle    = false; // inside '...'
        $inDouble    = false; // inside "..."
        $inBacktick  = false; // inside `...`
        $inBlockCmt  = false; // inside /* ... */
        $inLineCmt   = false; // inside -- ... \n  (also # ... \n)

        while ($i < $len) {
            $ch = $buf[$i];

            if ($inLineCmt) {
                if ($ch === "\n") {
                    $inLineCmt = false;
                }
                $i++;
                continue;
            }
            if ($inBlockCmt) {
                if ($ch === '*' && $i + 1 < $len && $buf[$i + 1] === '/') {
                    $inBlockCmt = false;
                    $i += 2;
                    continue;
                }
                $i++;
                continue;
            }
            if ($inSingle) {
                if ($ch === '\\' && $i + 1 < $len) {
                    $i += 2;
                    continue;
                }
                if ($ch === "'") {
                    $inSingle = false;
                }
                $i++;
                continue;
            }
            if ($inDouble) {
                if ($ch === '\\' && $i + 1 < $len) {
                    $i += 2;
                    continue;
                }
                if ($ch === '"') {
                    $inDouble = false;
                }
                $i++;
                continue;
            }
            if ($inBacktick) {
                if ($ch === '`') {
                    $inBacktick = false;
                }
                $i++;
                continue;
            }

            // Not inside any quoted/comment region — look for delimiters.
            switch ($ch) {
                case "'":
                    $inSingle = true;
                    $i++;
                    break;
                case '"':
                    $inDouble = true;
                    $i++;
                    break;
                case '`':
                    $inBacktick = true;
                    $i++;
                    break;
                case '-':
                    if ($i + 1 < $len && $buf[$i + 1] === '-') {
                        // SQL line comment requires "-- " (dash dash space)
                        // OR newline immediately after — be tolerant; treat
                        // any "--" at column-start-ish as a line comment.
                        $inLineCmt = true;
                        $i += 2;
                        break;
                    }
                    $i++;
                    break;
                case '#':
                    // MySQL-style line comment.
                    $inLineCmt = true;
                    $i++;
                    break;
                case '/':
                    if ($i + 1 < $len && $buf[$i + 1] === '*') {
                        $inBlockCmt = true;
                        $i += 2;
                        break;
                    }
                    $i++;
                    break;
                case ';':
                    // Statement boundary. Slice from $stmtStart to $i.
                    $stmt = trim(substr($buf, $stmtStart, $i - $stmtStart));
                    if ($stmt !== '') {
                        $statements[] = $stmt;
                    }
                    $i++;
                    $stmtStart = $i;
                    break;
                default:
                    $i++;
            }
        }

        // Whatever's left after the last `;` is the tail. We DO NOT emit it
        // as a complete statement — caller will feed more bytes and call us
        // again, or flush it at EOF.
        $tail = substr($buf, $stmtStart);
        return [$statements, $tail];
    }

    /**
     * Rewrite source-prefix identifiers in a statement to the tmp prefix.
     * Updates $currentTable by reference when the statement names a table.
     *
     * Only catches identifiers at canonical positions:
     *   CREATE TABLE [IF NOT EXISTS] `wp_X`
     *   DROP TABLE [IF EXISTS] `wp_X` (...)
     *   INSERT INTO `wp_X` ...
     *   ALTER TABLE `wp_X` ...
     *   LOCK TABLES `wp_X` ...
     *
     * Identifiers may be backtick-quoted or bare. We deliberately do NOT do
     * a global str_replace (deep-dive §1.3 warns against it — string
     * literals could contain the prefix and would get corrupted).
     *
     * @param string $stmt         SQL statement (no trailing `;`).
     * @param string $sourcePrefix Source prefix to match (e.g. `wp_`).
     * @param string $tmpPrefix    Replacement prefix (e.g. `tmpAB12_`).
     * @param string $currentTable In/out — current table name (updated).
     * @return string Rewritten statement. Empty string skips the statement.
     */
    public static function rewritePrefix(string $stmt, string $sourcePrefix, string $tmpPrefix, string &$currentTable): string
    {
        $trimmed = ltrim($stmt);
        if ($trimmed === '') {
            return '';
        }
        // Strip any leading SQL comments (block + line) so the keyword regex
        // sees the real statement head. We keep the comments in the OUTPUT
        // (so dumps' provenance / hints survive) but match against the
        // post-comment body.
        $headOffset = self::leadingCommentLength($trimmed);
        $body       = $headOffset > 0 ? ltrim(substr($trimmed, $headOffset)) : $trimmed;
        $leading    = $headOffset > 0 ? substr($trimmed, 0, strlen($trimmed) - strlen($body)) : '';
        if ($body === '') {
            return '';
        }

        // Match the leading keyword (case-insensitive). Order matters: more
        // specific first.
        $patterns = [
            // CREATE TABLE [IF NOT EXISTS]
            '/^(CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?)`?([A-Za-z0-9_\$]+)`?/i',
            // DROP TABLE [IF EXISTS] (may have comma list — we handle the first identifier only;
            // dumps typically emit one DROP per statement).
            '/^(DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?)`?([A-Za-z0-9_\$]+)`?/i',
            // INSERT INTO
            '/^(INSERT\s+(?:IGNORE\s+|LOW_PRIORITY\s+|DELAYED\s+|HIGH_PRIORITY\s+)?INTO\s+)`?([A-Za-z0-9_\$]+)`?/i',
            // ALTER TABLE
            '/^(ALTER\s+TABLE\s+)`?([A-Za-z0-9_\$]+)`?/i',
            // LOCK TABLES (rare in WP dumps, but we handle for completeness)
            '/^(LOCK\s+TABLES\s+)`?([A-Za-z0-9_\$]+)`?/i',
            // REPLACE INTO
            '/^(REPLACE\s+INTO\s+)`?([A-Za-z0-9_\$]+)`?/i',
            // RENAME TABLE
            '/^(RENAME\s+TABLE\s+)`?([A-Za-z0-9_\$]+)`?/i',
            // TRUNCATE [TABLE]
            '/^(TRUNCATE\s+(?:TABLE\s+)?)`?([A-Za-z0-9_\$]+)`?/i',
        ];

        foreach ($patterns as $p) {
            if (!preg_match($p, $body, $m, PREG_OFFSET_CAPTURE)) {
                continue;
            }
            $tableName = $m[2][0];
            // Only rewrite if it starts with our source prefix.
            if (strpos($tableName, $sourcePrefix) !== 0) {
                // Statement names a table, but not one of OUR tables. Skip
                // it entirely — replaying a CREATE/INSERT against an
                // unrelated table risks corrupting other plugins' data.
                return '';
            }
            $bare         = substr($tableName, strlen($sourcePrefix));
            $newName      = $tmpPrefix . $bare;
            $currentTable = $newName;

            // Reconstruct: leading-comments + keyword + new backticked
            // identifier + rest of body after the original identifier.
            $keyword  = $m[1][0];
            $matchEnd = $m[0][1] + strlen($m[0][0]);
            $rest     = substr($body, $matchEnd);
            return $leading . $keyword . '`' . $newName . '`' . $rest;
        }

        // No table-named DDL/DML — keep the statement as-is. SET, START
        // TRANSACTION, COMMIT, and pragma comments are all fine.
        // BUT: skip statements that try to switch the database / user /
        // host context — those are safety risks in a replayed dump.
        $leadingUpper = strtoupper(substr($body, 0, 16));
        $forbidden    = ['USE ', 'CREATE DATABASE', 'DROP DATABASE', 'GRANT ', 'REVOKE ', 'CREATE USER', 'DROP USER', 'SET PASSWORD', 'FLUSH '];
        foreach ($forbidden as $f) {
            if (strpos($leadingUpper, $f) === 0) {
                return '';
            }
        }

        return $trimmed;
    }

    /**
     * Count the number of leading bytes in $stmt that are SQL comments
     * (block `/star ... star/` or line `-- ...\n` / `# ...\n`) followed by
     * whitespace. Used so rewritePrefix() can skip past comments to match
     * the real keyword.
     */
    private static function leadingCommentLength(string $stmt): int
    {
        $len = strlen($stmt);
        $i   = 0;
        while ($i < $len) {
            // Skip whitespace.
            while ($i < $len && ctype_space($stmt[$i])) {
                $i++;
            }
            if ($i >= $len) {
                break;
            }
            // Block comment?
            if ($i + 1 < $len && $stmt[$i] === '/' && $stmt[$i + 1] === '*') {
                $end = strpos($stmt, '*/', $i + 2);
                if ($end === false) {
                    return $len; // unterminated — treat as all comment
                }
                $i = $end + 2;
                continue;
            }
            // Line comment `-- ` or `--\n`?
            if ($i + 1 < $len && $stmt[$i] === '-' && $stmt[$i + 1] === '-') {
                $nl = strpos($stmt, "\n", $i + 2);
                if ($nl === false) {
                    return $len;
                }
                $i = $nl + 1;
                continue;
            }
            // MySQL-style `#` line comment.
            if ($stmt[$i] === '#') {
                $nl = strpos($stmt, "\n", $i + 1);
                if ($nl === false) {
                    return $len;
                }
                $i = $nl + 1;
                continue;
            }
            // First real (non-comment, non-whitespace) char.
            return $i;
        }
        return $i;
    }

    /**
     * Whether a buffer tail looks like only whitespace and SQL comments — in
     * which case we don't need to flush it as a "statement" at EOF.
     */
    private static function looksLikeCommentOnly(string $tail): bool
    {
        // Strip /* */ block comments and -- line comments / # line comments.
        $stripped = preg_replace('#/\*.*?\*/#s', '', $tail) ?? $tail;
        $lines    = preg_split('/\r?\n/', $stripped) ?? [];
        foreach ($lines as $line) {
            $t = trim($line);
            if ($t === '' || strpos($t, '--') === 0 || strpos($t, '#') === 0) {
                continue;
            }
            return false;
        }
        return true;
    }

    /**
     * Defang an identifier for use inside backticks. MySQL allows backticks
     * inside identifiers if you double them, so we do the same. Belt and
     * suspenders — our prefix generator never produces backticks but the
     * source-prefix could be operator-supplied.
     */
    private function escIdent(string $ident): string
    {
        return str_replace('`', '``', $ident);
    }

    /**
     * Invoke caller progress callback safely; never let a broken hook fail a
     * restore.
     *
     * @param callable            $progress Caller callback.
     * @param string              $phase    Phase label.
     * @param array<string,mixed> $detail   Phase detail payload.
     */
    private static function safeProgress(callable $progress, string $phase, array $detail): void
    {
        try {
            $progress($phase, $detail);
        } catch (\Throwable $_) {
            // Swallow.
        }
    }
}
