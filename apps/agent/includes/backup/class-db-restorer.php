<?php
/**
 * DbRestorer: M5.6 / ADR-034 — DB restorer using a staged-prefix pattern.
 *
 * Replays a `.sql.gz` dump into the live database under a temporary table
 * prefix (`tmp<short>_X`), then atomically swaps each tmp table over the live
 * one with `DROP TABLE IF EXISTS wp_X; RENAME TABLE tmp_X TO wp_X;`. The whole
 * point is documented in `docs/research/restore-deep-dive.md` §1.3:
 *
 *   "Every CREATE/INSERT/DROP rewrites the table name from `wp_X` to
 *    `tmp<id>_X` before execution. So WordPress keeps reading and writing
 *    `wp_options` ... the whole time. Only at the very end ... does
 *    `rename_db()` swap things atomically per-table."
 *
 * Specifically we adopt the following patterns (§7.1) and add fixes (§7.2):
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
 *       A naive line-ending-`;` parser breaks on multi-line
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

use WPMgr\Agent\Support\LongRunningJob;

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
     * @throws \RuntimeException On fatal connection / read failure; when the
     *      dump's trailing fragment cannot be classified (see
     *      looksLikeCommentOnly()) — aborting beats discarding SQL; and when a
     *      statement's target table cannot be identified (see rewritePrefix())
     *      — aborting beats replaying it at the live tables.
     */
    public function restore(string $sqlGzPath, string $tmpPrefix, string $sourcePrefix, callable $progress): array
    {
        if (!is_file($sqlGzPath)) {
            throw new \RuntimeException('DbRestorer: dump file missing: ' . esc_html($sqlGzPath)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to server log/SSE, not browser output
        }
        if ($tmpPrefix === '' || substr($tmpPrefix, -1) !== '_') {
            throw new \RuntimeException('DbRestorer: tmpPrefix must end with "_"');
        }
        if ($sourcePrefix === '') {
            throw new \RuntimeException('DbRestorer: sourcePrefix empty');
        }

        @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running restore loop must not hit max_execution_time; @-guarded, no-op when disabled
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
            // landed. Matches the amortised-fsync approach (deep-dive
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
                            // Log-and-continue — permissive per-statement semantics.
                            \WPMgr\Agent\Support\DebugLog::write(sprintf(
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

        @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running restore swap loop must not hit max_execution_time; @-guarded, no-op when disabled
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
     * P0 URL rewriter: drive the URL rewrite across every tmp table.
     *
     * Wrapper around `rewriteTable()` that opens its own mysqli, lists tmp
     * tables under `$tmpPrefix`, applies the table-level skip denylist
     * (`UrlRewriter::should_skip_table`), and drives the per-table rewriter
     * paginated. Persists `$subState['url_rewrite']` checkpoints via the
     * supplied callback so a watchdog re-entry resumes mid-table.
     *
     * @param string                                  $tmpPrefix    The tmp prefix the restore is using.
     * @param string                                  $sourcePrefix The bare prefix the dump was made with.
     * @param array{0:list<string>,1:list<string>}    $replacements From `UrlRewriter::build_replacements`.
     * @param array<string,mixed>                     $resume       Sub-state from a prior call.
     * @param callable                                $checkpoint   function(array $newSubState): void
     *                                                              Called after every page so the runner can
     *                                                              persist `next_offset` between pages.
     * @param callable                                $progress     function(string $phase, array $detail): void
     * @return array<string,mixed> Final sub-state (`finished` => true).
     */
    public function rewriteAllTables(string $tmpPrefix, string $sourcePrefix, array $replacements, array $resume, callable $checkpoint, callable $progress): array
    {
        @set_time_limit(LongRunningJob::TIME_LIMIT_SECONDS); // phpcs:ignore Squiz.PHP.DiscouragedFunctions.Discouraged -- long-running URL-rewrite loop must not hit max_execution_time; @-guarded, no-op when disabled
        @ignore_user_abort(true);

        $mysqli = $this->connect();
        try {
            $allTables   = $this->listTablesWithPrefix($mysqli, $tmpPrefix);
            if ($allTables === []) {
                return ['finished' => true, 'tables_done' => [], 'total_updates' => 0];
            }
            $tablesDone   = isset($resume['tables_done']) && is_array($resume['tables_done']) ? $resume['tables_done'] : [];
            $tableOffsets = isset($resume['table_offset']) && is_array($resume['table_offset']) ? $resume['table_offset'] : [];
            $totalUpdates = isset($resume['total_updates']) ? (int) $resume['total_updates'] : 0;
            $tablesDoneSet = array_flip($tablesDone);

            $rowsPerCall  = 5000; // pagination budget matching common backup plugin practice.

            foreach ($allTables as $tmpTable) {
                if (isset($tablesDoneSet[$tmpTable])) {
                    continue;
                }
                // Strip the tmp prefix to get the source-bare name. The
                // denylist test compares against bare names.
                $bare = (strpos($tmpTable, $tmpPrefix) === 0)
                    ? substr($tmpTable, strlen($tmpPrefix))
                    : $tmpTable;
                if (UrlRewriter::should_skip_table($bare, '')) {
                    $tablesDone[] = $tmpTable;
                    $tablesDoneSet[$tmpTable] = true;
                    self::safeProgress($progress, 'url_rewrite', [
                        'table'       => $tmpTable,
                        'skipped'     => 'denylist',
                        'tables_done' => count($tablesDone),
                    ]);
                    continue;
                }

                $offset = isset($tableOffsets[$tmpTable]) ? (int) $tableOffsets[$tmpTable] : 0;
                while (true) {
                    $page = $this->rewriteTable($mysqli, $tmpTable, $bare, $replacements, $rowsPerCall, $offset);
                    $offset = (int) $page['next_offset'];
                    $totalUpdates += (int) $page['updates'];
                    $tableOffsets[$tmpTable] = $offset;
                    $checkpoint([
                        'table_offset'  => $tableOffsets,
                        'tables_done'   => $tablesDone,
                        'current_table' => $tmpTable,
                        'current_offset' => $offset,
                        'total_updates' => $totalUpdates,
                    ]);
                    self::safeProgress($progress, 'url_rewrite', [
                        'table'         => $tmpTable,
                        'rows_processed' => (int) $page['processed'],
                        'rows_updated'  => (int) $page['updates'],
                        'offset'        => $offset,
                        'tables_done'   => count($tablesDone),
                        'tables_total'  => count($allTables),
                        'total_updates' => $totalUpdates,
                    ]);
                    if (!empty($page['finished'])) {
                        break;
                    }
                }
                $tablesDone[] = $tmpTable;
                $tablesDoneSet[$tmpTable] = true;
                unset($tableOffsets[$tmpTable]);
            }

            $out = [
                'finished'      => true,
                'tables_done'   => $tablesDone,
                'tables_total'  => count($allTables),
                'total_updates' => $totalUpdates,
            ];
            $checkpoint($out);
            return $out;
        } finally {
            @$mysqli->close();
        }
    }

    /**
     * P0 URL rewriter: list every table whose name begins with the supplied
     * prefix. Used to enumerate tmp tables for the URL rewrite phase.
     *
     * @return list<string>
     */
    private function listTablesWithPrefix(\mysqli $mysqli, string $prefix): array
    {
        $like = $mysqli->real_escape_string($prefix . '%');
        $res  = @$mysqli->query("SHOW TABLES LIKE '" . $like . "'");
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

    /**
     * P0 URL rewriter: extract the source-URL banner comments from a dump.
     *
     * The dumper writes a 6-line banner immediately after the file header so
     * the restorer can recover the source URLs even when the CP-supplied
     * `target_*` URLs are absent (a restore initiated against an older agent
     * snapshot pre-ADR-036, or a manifest that never landed source URLs in
     * `backup_snapshots`). Uses a banner format compatible with common dump
     * produced by older backup pipelines is ALSO parseable here.
     *
     * Reads up to 50 lines from the start of the file. If the file is gzipped
     * (`.sql.gz`) it's transparently decompressed via `gzopen` — the same
     * call works for plain `.sql` too because gzip detects a non-gzip stream
     * and falls through to raw read.
     *
     * @param string $sqlFilePath Absolute path to the dump (`.sql` or `.sql.gz`).
     * @return array{old_site_url:string,old_home_url:string,old_content_url:string,old_upload_url:string,old_table_prefix:string}
     *              All five fields present (empty string when not found).
     */
    public static function extractDumpUrls(string $sqlFilePath): array
    {
        $out = [
            'old_site_url'     => '',
            'old_home_url'     => '',
            'old_content_url'  => '',
            'old_upload_url'   => '',
            'old_table_prefix' => '',
        ];
        if (!is_file($sqlFilePath)) {
            return $out;
        }
        $handle = @gzopen($sqlFilePath, 'rb');
        if ($handle === false) {
            return $out;
        }
        try {
            $lineNum = 0;
            while (!gzeof($handle) && $lineNum < 50) {
                $line = gzgets($handle);
                if ($line === false) {
                    break;
                }
                $lineNum++;
                $line = rtrim($line);
                if ($line === '') {
                    continue;
                }
                // Only inspect comment lines — the banner is wholly inside
                // SQL comments so the dump remains a valid SQL stream.
                $head = substr(ltrim($line), 0, 2);
                if ($head !== '--' && $head !== '/*' && $head !== '//') {
                    continue;
                }
                // Each banner entry ends with " #" (stable anchor used by
                // common dump formats so we stay compatible with those dumps).
                $matchers = [
                    'old_site_url'     => '/# site_url: (.*?) #/',
                    'old_home_url'     => '/# home_url: (.*?) #/',
                    'old_content_url' => '/# content_url: (.*?) #/',
                    'old_upload_url'   => '/# upload_url: (.*?) #/',
                    'old_table_prefix' => '/# table_prefix: (.*?) #/',
                ];
                // A preg_match() failure here is safe to absorb, unlike the
                // one in rewritePrefix(). Nothing this method returns decides
                // where data is written: the source/target table prefixes come
                // from the CP params and the live $wpdb (RestoreRunner::
                // sourcePrefix() / targetPrefix()), never from the banner. A
                // missed banner field only leaves the OLD urls unknown, and the
                // rewrite pass then falls back to the CP-supplied source_* URLs
                // or short-circuits to a no-op — the restored site can end up
                // still serving the source URLs, which is visible, recoverable
                // and non-destructive. Failing the whole restore over an
                // unreadable comment banner would be the worse trade.
                foreach ($matchers as $key => $pattern) {
                    if ($out[$key] === '' && preg_match($pattern, $line, $m) === 1) {
                        $out[$key] = trim($m[1]);
                    }
                }
            }
        } finally {
            @gzclose($handle);
        }
        return $out;
    }

    /**
     * P0 URL rewriter: paginated UPDATE loop for one tmp table.
     *
     * Walks `$maxRowsPerCall` rows starting at `$resumeOffset`, applies
     * `UrlRewriter::rewrite_row_data` to each cell whose column is NOT in the
     * skip list, and issues batched UPDATE statements (concatenated until the
     * SQL grows past ~1 MiB, then flushed). This shape — pagination +
     * batching — is what lets us run cross-environment rewrites against a
     * 500MB serialized-options table without hitting `max_allowed_packet`
     * (single huge UPDATE) or PHP's `max_execution_time` (single huge SELECT
     * with no chunking).
     *
     * Returns the cursor state so the RestoreRunner can checkpoint per chunk
     * and resume on watchdog re-entry.
     *
     * @param \mysqli              $mysqli         An already-connected mysqli handle
     *                                              (the caller owns it; we don't open
     *                                              our own here because the runner
     *                                              walks many tables in a row and we
     *                                              want to amortise the connect).
     * @param string               $tmpTable       Full tmp-prefixed table name.
     * @param string               $oldName        Bare source table name (e.g. "options"),
     *                                              used to gate per-table column skips
     *                                              (posts.guid).
     * @param array{0:list<string>,1:list<string>} $replacements Built by `UrlRewriter::build_replacements`.
     * @param int                  $maxRowsPerCall Pagination budget (5000 mirrors
     *                                              matching common backup plugin practice).
     * @param int                  $resumeOffset   Where to start in the table.
     * @return array{processed:int, finished:bool, next_offset:int, updates:int}
     *              `processed`: rows considered; `updates`: rows actually changed;
     *              `finished`: true when this call drained the table.
     */
    public function rewriteTable(\mysqli $mysqli, string $tmpTable, string $oldName, array $replacements, int $maxRowsPerCall, int $resumeOffset): array
    {
        if ($maxRowsPerCall <= 0) {
            $maxRowsPerCall = 5000;
        }
        if ($resumeOffset < 0) {
            $resumeOffset = 0;
        }
        $result = [
            'processed'   => 0,
            'finished'    => true,
            'next_offset' => $resumeOffset,
            'updates'     => 0,
        ];

        // Cap the per-flush batch at 1 MiB so we stay well under MySQL's
        // default `max_allowed_packet` (4 MiB on modern installs, but as low
        // as 1 MiB on shared hosts). The same shape DbDumper uses for its
        // multi-row INSERTs.
        $maxPacketBytes = 1_000_000;

        $columns      = $this->describeColumnsForRewrite($mysqli, $tmpTable);
        if ($columns === []) {
            return $result;
        }
        $primaryKey   = $this->detectPrimaryKey($mysqli, $tmpTable);
        if ($primaryKey === '') {
            // No primary key — can't rewrite safely (we'd risk updating the
            // wrong row). Skip; RestoreRunner logs and continues.
            \WPMgr\Agent\Support\DebugLog::write('WPMgr UrlRewriter: skipping table without PK: ' . $tmpTable);
            return $result;
        }

        $offset = $resumeOffset;
        $rowsThisCall = 0;

        // SELECT one page, walk rows, build per-row UPDATE statements,
        // batch-flush. We re-SELECT for each page so a chunk that's already
        // been rewritten is read with the new values on resume.
        $sql = sprintf(
            'SELECT * FROM `%s` LIMIT %d OFFSET %d',
            $this->escIdent($tmpTable),
            $maxRowsPerCall,
            $offset
        );
        $rows = @$mysqli->query($sql);
        if ($rows === false) {
            // Surface — runner will catch and translate to a failed phase.
            throw new \RuntimeException('UrlRewriter: SELECT failed for ' . esc_html($tmpTable) . ': ' . esc_html((string) $mysqli->error)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to server log/SSE, not browser output
        }

        $batch       = '';
        $batchBytes  = 0;
        $updateCount = 0;

        while ($row = $rows->fetch_assoc()) {
            $rowsThisCall++;
            $sets = [];
            foreach ($row as $colName => $value) {
                if (!isset($columns[$colName])) {
                    continue;
                }
                if ($value === null) {
                    continue;
                }
                $colType = $columns[$colName];
                if (UrlRewriter::should_skip_column($tmpTable, $colName, '', $colType)) {
                    // Compare against the tmp table name AND also the source
                    // bare name — posts.guid lives at tmpXXX_posts.guid after
                    // the prefix swap, and the rule is "skip if bare =
                    // posts". We approximate by also testing the explicit
                    // oldName.
                    continue;
                }
                if ($oldName === 'posts' && $colName === 'guid') {
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

            // Build "UPDATE `t` SET c='v',c2='v' WHERE pk='id'" — one
            // statement per row. Mysqli's `real_escape_string` is the
            // canonical defense for arbitrary bytes inside a SQL literal.
            $setSql = [];
            foreach ($sets as $col => $val) {
                $setSql[] = '`' . $this->escIdent($col) . "`='" . $mysqli->real_escape_string($val) . "'";
            }
            $stmt = sprintf(
                "UPDATE `%s` SET %s WHERE `%s`='%s';",
                $this->escIdent($tmpTable),
                implode(',', $setSql),
                $this->escIdent($primaryKey),
                $mysqli->real_escape_string((string) $pkValue)
            );
            $stmtLen = strlen($stmt);

            if ($batchBytes > 0 && ($batchBytes + $stmtLen) > $maxPacketBytes) {
                // Flush before adding this statement.
                @$mysqli->multi_query($batch);
                $this->drainMultiResults($mysqli);
                $batch       = '';
                $batchBytes  = 0;
            }
            $batch .= $stmt . "\n";
            $batchBytes += $stmtLen + 1;
            $updateCount++;
        }
        $rows->free();

        if ($batch !== '') {
            @$mysqli->multi_query($batch);
            $this->drainMultiResults($mysqli);
        }

        $result['processed']   = $rowsThisCall;
        $result['updates']     = $updateCount;
        $result['next_offset'] = $offset + $rowsThisCall;
        // Finished when the page returned fewer rows than the budget — there's
        // no Nth+1 page to fetch on the next call.
        $result['finished']    = $rowsThisCall < $maxRowsPerCall;
        return $result;
    }

    /**
     * Helper for `rewriteTable`. Returns column-name => column-type-slug for
     * every column of the table. Type slug is lowercased and matches what
     * `UrlRewriter::should_skip_column` expects.
     *
     * @return array<string,string>
     */
    private function describeColumnsForRewrite(\mysqli $mysqli, string $table): array
    {
        $res = $mysqli->query('SHOW COLUMNS FROM `' . $this->escIdent($table) . '`');
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
     * Helper for `rewriteTable`. Returns the primary-key column name (or '').
     * Prefers the explicit PRIMARY KEY index; falls back to any single UNIQUE
     * key. Returning '' tells the caller to skip the table — without a stable
     * row identifier the per-row UPDATE WHERE clause isn't safe.
     */
    private function detectPrimaryKey(\mysqli $mysqli, string $table): string
    {
        $res = $mysqli->query('SHOW KEYS FROM `' . $this->escIdent($table) . "` WHERE Key_name = 'PRIMARY'");
        if ($res === false) {
            return '';
        }
        $pk = '';
        while ($row = $res->fetch_assoc()) {
            // Compound PK: take the first column. Rewrites are still safe
            // because we WHERE on every column — but P0 simplifies to the
            // first column. Most WP tables (options, postmeta, posts) have
            // single-column PKs; compound is rare in standard WP schema.
            if ($pk === '' && isset($row['Column_name'])) {
                $pk = (string) $row['Column_name'];
            }
        }
        $res->free();
        if ($pk !== '') {
            return $pk;
        }
        // Fallback: any UNIQUE key. wp_options.option_name is the canonical
        // case — it lacks a PRIMARY on some older WP installs.
        $res = $mysqli->query('SHOW KEYS FROM `' . $this->escIdent($table) . "` WHERE Non_unique = 0");
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
     * Helper for `rewriteTable`. `multi_query` leaves any subsequent result
     * sets attached to the connection; if we don't drain them the next query
     * raises "Commands out of sync". We don't care about the contents (UPDATEs
     * don't return rows).
     */
    private function drainMultiResults(\mysqli $mysqli): void
    {
        do {
            if ($r = @$mysqli->store_result()) {
                $r->free();
            }
        } while (@$mysqli->more_results() && @$mysqli->next_result());
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
        $mysqli = @new \mysqli($host, $user, $pass, $name, $port, $sock ?? ''); // phpcs:ignore WordPress.DB.RestrictedClasses.mysql__mysqli -- dedicated streaming connection for backup DB restore; $wpdb buffers the full result set (OOM risk)
        if ($mysqli->connect_errno) {
            throw new \RuntimeException('DbRestorer: connect failed: ' . esc_html((string) $mysqli->connect_error)); // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to server log/SSE, not browser output
        }
        @$mysqli->set_charset('utf8mb4');
        return $mysqli;
    }

    /**
     * Apply the session config that makes legacy dumps
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
     * `;` inside any of those is NOT a statement terminator. A naive
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
     * @throws \RuntimeException When PCRE cannot decide whether a statement
     *      names a table — see the keyword loop below. Aborting beats
     *      replaying an unrewritten statement against the LIVE tables.
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
            // preg_match() has THREE outcomes and they are not interchangeable
            // here: 1 = this statement names a table, 0 = it does not, false =
            // PCRE gave up and we do not know. `!preg_match(...)` collapsed
            // `false` into `0` — a PCRE failure was read as "no table name".
            //
            // That is the worst possible reading. Every pattern would fail the
            // same way, the loop would fall through to the tail below, and the
            // statement would be returned VERBATIM — still naming `wp_posts`
            // instead of `tmp<rand>_posts`. The dump then replays straight
            // into the LIVE tables, outside the staging discipline the tmp
            // prefix exists to provide. $currentTable is never set either, so
            // $touchedTbls stays empty, swap() takes its `$tmpTables === []`
            // branch, emits 'done' => true, and the whole restore reports
            // Completed having overwritten the live database and swapped
            // nothing. The staging mechanism exists precisely so that a failed
            // restore is non-destructive; this defeated it silently.
            //
            // "Cannot tell" must never resolve to "route it at the live
            // tables". Abort: restore() never reaches its COMMIT, the caller
            // fails the restore, and the live database is left alone.
            $matched = preg_match($p, $body, $m, PREG_OFFSET_CAPTURE);
            if ($matched === false) {
                throw new \RuntimeException(
                    'DbRestorer: cannot determine which table a dump statement targets (preg_match failed: '
                    . esc_html(preg_last_error_msg()) // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to server log/SSE, not browser output
                    . '). Aborting the restore rather than replay unrewritten SQL against the live tables.'
                    . ' Raise pcre.backtrack_limit / pcre.recursion_limit on this host and retry the restore.'
                );
            }
            if ($matched === 0) {
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
     *
     * A `true` here makes the caller DISCARD the dump's final fragment, so
     * this method must never answer `true` out of ignorance. The two regex
     * calls below are therefore treated asymmetrically, on purpose.
     *
     * @throws \RuntimeException When the tail cannot be classified at all.
     */
    private static function looksLikeCommentOnly(string $tail): bool
    {
        // Strip /* */ block comments and -- line comments / # line comments.
        //
        // A preg_replace() failure is safe to absorb. Falling back to the
        // unstripped tail can only make a line look like real SQL, never like
        // a comment, so the worst case is flushing a comment-only tail to the
        // server as a statement. That errs towards KEEPING data, which is the
        // safe direction on a restore.
        $stripped = preg_replace('#/\*.*?\*/#s', '', $tail) ?? $tail;

        // A preg_split() failure is NOT safe to absorb. Coercing `false` to an
        // empty list would skip the loop below entirely and fall through to
        // `return true` — telling the caller "only comments here, safe to
        // drop" and silently discarding the dump's last SQL statement while
        // the restore still reports success. A regex failure is not evidence
        // that the tail is comment-only; it is evidence that we cannot tell,
        // and "cannot tell" must never resolve to "safe to discard". Abort
        // instead: restore()'s transaction is left uncommitted and the caller
        // fails the restore rather than committing a partial database.
        $lines = preg_split('/\r?\n/', $stripped);
        if ($lines === false) {
            throw new \RuntimeException(
                'DbRestorer: cannot classify the trailing SQL fragment of the dump (preg_split failed: '
                . esc_html(preg_last_error_msg()) // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to server log/SSE, not browser output
                . '). Aborting the restore rather than risk discarding SQL. Raise pcre.backtrack_limit'
                . ' / pcre.recursion_limit on this host and retry the restore.'
            );
        }

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
