<?php
/**
 * DbDumper: M5.6 / ADR-033 — pure-PHP DB dumper using mysqli (via WordPress's
 * underlying connection) directly, NOT PDO.
 *
 * Why mysqli, not PDO:
 *   Original ifsnop/mysqldump-php uses PDO under the hood — needs the
 *   `pdo_mysql` extension. Many managed WP hosts (including 1panel-hosted WP
 *   containers like curvabykerline.in) ship `mysqli` only — that's what
 *   WordPress core uses. WPvivid (40M+ installs, the gold standard for "works
 *   on every WP host") forked ifsnop and runs it through `$wpdb`/mysqli for
 *   exactly this reason. We mirror that decision: this dumper depends ONLY on
 *   what WordPress itself depends on (mysqli) and works on every host that
 *   runs WP.
 *
 * Streaming model — memory bound is independent of total DB size:
 *   1. Connect via mysqli, set utf8mb4, REPEATABLE READ snapshot
 *      (`START TRANSACTION WITH CONSISTENT SNAPSHOT`).
 *   2. Open the output path with `gzopen('wb')` — write straight to a
 *      compressed stream on disk, never accumulate in memory.
 *   3. For each base table: emit `DROP TABLE IF EXISTS` + the schema from
 *      `SHOW CREATE TABLE`. Then SELECT all rows with `MYSQLI_USE_RESULT`
 *      (server-side cursor — fetches one row at a time, no client-side
 *      buffering of the whole result set). Format each row using column
 *      types from `SHOW COLUMNS` (BLOB → `0x{hex}`, numeric → raw, else
 *      `'escaped'`), batch into multi-row INSERTs capped at ~1 MiB
 *      (`net_buffer_length`, matches MySQL's `max_allowed_packet` default
 *      AND WPvivid's choice), `gzwrite` the batch.
 *   4. Wrap the whole dump in `SET FOREIGN_KEY_CHECKS=0/1` so restore can
 *      apply the SQL in arbitrary table order without FK violations.
 *
 * V0 limitation — no table-level mid-dump resume:
 *   `dump()` is a single atomic call: opens the gzip stream, walks all
 *   tables inside one transaction, closes the stream. A partial run leaves
 *   a half-written `.sql.gz` that cannot safely be resumed (the snapshot
 *   transaction is gone, the gzip stream is unfinalized). On watchdog
 *   re-entry, an incomplete dump is discarded and restarted from scratch.
 *   For real WP sites this is fine — typical DBs finish in seconds; even
 *   multi-GB serialized-postmeta tails finish in a couple of minutes, well
 *   under the 180 s stall threshold. Table-level resume is tracked as a
 *   follow-up if customer telemetry shows it's needed.
 *
 * Out of scope for V0 (acceptable for the WP use case):
 *   - Stored procedures, triggers, views, events. WP core uses none of
 *     these; restore-from-snapshot doesn't need them. Re-add if a customer
 *     surveys against a stored-proc-heavy install.
 *   - LOCK TABLES. We rely on the transaction snapshot for consistency.
 *     For MyISAM tables (deprecated, almost no modern WP install uses
 *     them) the snapshot doesn't apply — they're dumped at whatever state
 *     they're in. WPvivid accepts the same trade-off.
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

/**
 * Streaming DB dumper. Constructs its OWN mysqli connection from the
 * supplied credentials — does NOT reuse WordPress's global $wpdb so it
 * remains usable from tests / CLI contexts that haven't booted WP, AND so
 * the dump's REPEATABLE READ transaction doesn't bleed across $wpdb's
 * concurrent reads from the rest of the request.
 */
final class DbDumper
{
    /**
     * Multi-row INSERT batch cap in bytes. Approximates MySQL's
     * `max_allowed_packet` default (~1 MB) and matches WPvivid's `net_buffer_length`.
     * Smaller = more INSERT headers; larger = risk single-statement rejection
     * on restore against a default-configured server.
     */
    private const NET_BUFFER_BYTES = 1_000_000;

    /**
     * @var array{host:string,user:string,password:string,name:string,prefix:string}
     */
    private array $db;

    /** @var array<string,mixed> Future-proofing knobs. */
    private array $opts;

    /**
     * @param array{host:string,user:string,password:string,name:string,prefix:string} $db
     *      WordPress DB credentials. `host` may include a `:port` suffix
     *      (we split it off — mysqli's constructor takes port as a separate arg).
     * @param array<string,mixed> $opts Reserved.
     */
    public function __construct(array $db, array $opts = [])
    {
        $this->db   = $db;
        $this->opts = $opts;
    }

    /**
     * Stream-dump the database into a single `.sql.gz` file on disk.
     *
     * @param string              $outPath  Absolute path of the output `.sql.gz`.
     * @param array<string,mixed> $resume   Sub-state from a prior partial run.
     *                                      `done: true` → no-op; non-empty + non-done
     *                                      → discard partial file, restart fresh.
     * @param callable            $progress function(string $phase, array $detail): void
     *                                      Called at each table boundary with
     *                                      $phase === 'dumping_db'. Never throws
     *                                      (we wrap in try/catch).
     * @return array<string,mixed> On full completion: `[done:true, output_path,
     *                              bytes:int]`. The TaskRunner persists this.
     * @throws \RuntimeException On fatal error. Message NEVER contains DB credentials.
     */
    public function dump(string $outPath, array $resume, callable $progress): array
    {
        @set_time_limit(0);
        @ignore_user_abort(true);

        if (!empty($resume['done'])) {
            return $resume; // Idempotent re-entry — watchdog replays.
        }
        if ($resume !== []) {
            // V0: no mid-dump resume (see file header). A partial run is
            // unrecoverable; discard the half-written gzip and restart.
            @unlink($outPath);
        }
        if (!function_exists('gzopen')) {
            throw new \RuntimeException('WPMgr Agent: ext-zlib required for DB dump (gzopen missing).');
        }
        if (!class_exists('mysqli')) {
            throw new \RuntimeException('WPMgr Agent: ext-mysqli required for DB dump.');
        }

        $mysqli = $this->connect();
        $gz     = $this->openOutput($outPath, $mysqli);

        try {
            $this->writeHeader($gz);
            $tables = $this->listBaseTables($mysqli);
            if ($tables === []) {
                throw new \RuntimeException('WPMgr Agent: no base tables in database (DB empty?).');
            }
            $tablesTotal = count($tables);
            $tablesDone  = 0;
            $rowsDone    = 0;

            foreach ($tables as $table) {
                $this->dumpTableSchema($mysqli, $gz, $table);
                $rowsDone += $this->dumpTableRows($mysqli, $gz, $table);
                $tablesDone++;
                $this->safeProgress($progress, 'dumping_db', [
                    'table'         => $table,
                    'tables_done'   => $tablesDone,
                    'tables_total'  => $tablesTotal,
                    'rows_done'     => $rowsDone,
                    'bytes_written' => $this->safeFilesize($outPath),
                ]);
            }

            $this->writeFooter($gz);
            // Commit closes the consistent-snapshot transaction. Most failure
            // modes would have thrown earlier; if we got here without an
            // error the snapshot is no longer needed.
            @$mysqli->commit();
        } catch (\Throwable $e) {
            // Abort the transaction (best-effort) and clean up.
            @$mysqli->rollback();
            // Ensure no half-written file lingers — TaskRunner's retry logic
            // assumes a missing file means "fresh start".
            @gzclose($gz);
            @unlink($outPath);
            throw new \RuntimeException(
                'WPMgr Agent: DB dump failed (' . $this->scrubMessage($e->getMessage()) . ').',
                0,
                $e
            );
        }

        // gzclose is critical — without it the gzip footer is missing and
        // the file is corrupt. Do NOT put this in the catch above.
        if (!@gzclose($gz)) {
            throw new \RuntimeException('WPMgr Agent: failed to finalize gzip stream.');
        }
        @$mysqli->close();

        // Bust PHP's stat cache for this path — filesize() can return a stale
        // 0 immediately after gzclose on some filesystems. Without this we
        // saw a false-negative empty-file error in live QA (25 tables dumped
        // successfully, file was actually fine on disk, but filesize cached
        // pre-gzclose returned 0).
        clearstatcache(true, $outPath);

        $bytes = $this->safeFilesize($outPath);
        if ($bytes <= 0) {
            // Include diagnostic detail so the next failure is actionable
            // rather than mysterious. "exists" vs "empty" vs "stat-cache"
            // — all three look identical without this.
            $exists  = @file_exists($outPath) ? 'yes' : 'no';
            $readable= @is_readable($outPath) ? 'yes' : 'no';
            $diskSpace = @disk_free_space(dirname($outPath));
            throw new \RuntimeException(sprintf(
                'WPMgr Agent: DB dump produced empty output (path=%s exists=%s readable=%s free_disk=%s)',
                $outPath,
                $exists,
                $readable,
                is_numeric($diskSpace) ? (string) (int) $diskSpace : 'unknown'
            ));
        }

        $this->safeProgress($progress, 'dumping_db', [
            'done'          => true,
            'bytes_written' => $bytes,
        ]);

        return [
            'done'        => true,
            'output_path' => $outPath,
            'bytes'       => $bytes,
        ];
    }

    /**
     * Connect to MySQL via mysqli, set utf8mb4, open the REPEATABLE READ
     * consistent-snapshot transaction. Throws on any connect/setup failure
     * with a credential-safe message.
     */
    private function connect(): \mysqli
    {
        [$host, $port] = $this->splitHostPort($this->db['host']);

        // mysqli_report controls how mysqli surfaces errors. Be explicit:
        // throw exceptions (introduced default in PHP 8.1+ but worth pinning)
        // so we can catch and translate.
        mysqli_report(MYSQLI_REPORT_ERROR | MYSQLI_REPORT_STRICT);

        try {
            $mysqli = new \mysqli(
                $host,
                $this->db['user'],
                $this->db['password'],
                $this->db['name'],
                $port ?? 3306
            );
        } catch (\Throwable $e) {
            throw new \RuntimeException('connect failed');
        }
        if ($mysqli->connect_errno) {
            throw new \RuntimeException('connect failed (' . $mysqli->connect_errno . ')');
        }
        if (!$mysqli->set_charset('utf8mb4')) {
            throw new \RuntimeException('set_charset utf8mb4 failed');
        }

        // Consistent snapshot — same property as ifsnop/WPvivid's --single-transaction.
        // Works only on InnoDB tables; MyISAM tables are dumped at whatever
        // state they're in (acceptable — MyISAM is functionally deprecated).
        $mysqli->query('SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ');
        $mysqli->query('START TRANSACTION WITH CONSISTENT SNAPSHOT');

        return $mysqli;
    }

    /**
     * Open the gzip output stream. Throws (with mysqli rollback) on failure.
     * @return resource
     */
    private function openOutput(string $outPath, \mysqli $mysqli)
    {
        $gz = @gzopen($outPath, 'wb');
        if ($gz === false) {
            @$mysqli->rollback();
            @$mysqli->close();
            throw new \RuntimeException('cannot open output path for write');
        }
        return $gz;
    }

    /** @param resource $gz */
    private function writeHeader($gz): void
    {
        gzwrite($gz, sprintf(
            "-- WPMgr Agent database backup\n-- Generated %s UTC\n-- DB: %s\n\n",
            gmdate('Y-m-d H:i:s'),
            // DB name is non-secret (the customer can see it themselves);
            // included for support-side debugging.
            $this->db['name']
        ));
        gzwrite($gz, "SET NAMES utf8mb4;\n");
        gzwrite($gz, "SET FOREIGN_KEY_CHECKS=0;\n");
        gzwrite($gz, "SET SQL_MODE='NO_AUTO_VALUE_ON_ZERO';\n\n");
    }

    /** @param resource $gz */
    private function writeFooter($gz): void
    {
        gzwrite($gz, "SET FOREIGN_KEY_CHECKS=1;\n");
        gzwrite($gz, "-- end of dump\n");
    }

    /** @return list<string> */
    private function listBaseTables(\mysqli $mysqli): array
    {
        // BASE TABLE filter excludes VIEWs (which would need separate handling
        // — out of scope for V0; vanilla WP has none).
        $result = $mysqli->query("SHOW FULL TABLES WHERE Table_type='BASE TABLE'");
        if (!$result) {
            throw new \RuntimeException('SHOW TABLES failed');
        }
        $tables = [];
        while ($row = $result->fetch_array(MYSQLI_NUM)) {
            $tables[] = (string) $row[0];
        }
        $result->free();
        return $tables;
    }

    /** @param resource $gz */
    private function dumpTableSchema(\mysqli $mysqli, $gz, string $table): void
    {
        $safe = $this->backtick($table);
        gzwrite($gz, sprintf("--\n-- Table %s\n--\n\n", $safe));
        gzwrite($gz, "DROP TABLE IF EXISTS {$safe};\n");

        $result = $mysqli->query("SHOW CREATE TABLE {$safe}");
        if (!$result) {
            throw new \RuntimeException("SHOW CREATE TABLE {$table} failed");
        }
        $row = $result->fetch_array(MYSQLI_NUM);
        $result->free();
        if (!is_array($row) || !isset($row[1])) {
            throw new \RuntimeException("SHOW CREATE TABLE {$table} returned no row");
        }
        gzwrite($gz, $row[1] . ";\n\n");
    }

    /**
     * Dump rows from one table as multi-row INSERTs, batched to ~NET_BUFFER_BYTES.
     * Uses MYSQLI_USE_RESULT so the server streams rows (no client-side full-result
     * buffering — memory bound is one row at a time).
     *
     * @param resource $gz
     * @return int Total rows emitted.
     */
    private function dumpTableRows(\mysqli $mysqli, $gz, string $table): int
    {
        $columns = $this->describeColumns($mysqli, $table);
        if ($columns === []) {
            return 0; // No columns? Empty / odd table — skip data.
        }
        $colList = implode(',', array_map([$this, 'backtick'], array_keys($columns)));
        $insertHeader = 'INSERT INTO ' . $this->backtick($table) . " ({$colList}) VALUES\n";

        // Use a SEPARATE connection-bound result-set so we can keep
        // streaming rows from this query while we issue gzwrites. mysqli
        // restricts a connection to ONE in-flight USE_RESULT at a time —
        // we have no other queries running on this connection during the
        // SELECT, so this is fine.
        $result = $mysqli->query("SELECT * FROM " . $this->backtick($table), MYSQLI_USE_RESULT);
        if (!$result) {
            throw new \RuntimeException("SELECT from {$table} failed");
        }

        $totalRows  = 0;
        $batch      = '';
        $batchBytes = 0;
        $first      = true;

        try {
            while ($row = $result->fetch_assoc()) {
                $rowSql = '(' . $this->formatRowValues($mysqli, $row, $columns) . ')';
                $rowLen = strlen($rowSql);
                // If adding this row would push the batch past the net buffer
                // cap, flush first. The +2 accounts for the trailing ";\n".
                if (!$first && ($batchBytes + 1 + $rowLen + 2) > self::NET_BUFFER_BYTES) {
                    gzwrite($gz, $insertHeader . $batch . ";\n");
                    $batch      = '';
                    $batchBytes = 0;
                    $first      = true;
                }
                $batch      .= ($first ? '' : ",\n") . $rowSql;
                $batchBytes += ($first ? 0 : 2) + $rowLen;
                $first       = false;
                $totalRows++;
            }
            if ($batch !== '') {
                gzwrite($gz, $insertHeader . $batch . ";\n");
            }
            gzwrite($gz, "\n");
        } finally {
            $result->free();
        }

        return $totalRows;
    }

    /**
     * Per-column SQL type slug, lowercased, used to drive value formatting.
     * @return array<string,string> Map of column name → type string ("int(11)", "blob", "varchar(255)", …).
     */
    private function describeColumns(\mysqli $mysqli, string $table): array
    {
        $result = $mysqli->query("SHOW COLUMNS FROM " . $this->backtick($table));
        if (!$result) {
            throw new \RuntimeException("SHOW COLUMNS for {$table} failed");
        }
        $out = [];
        while ($row = $result->fetch_assoc()) {
            $out[(string) $row['Field']] = strtolower((string) $row['Type']);
        }
        $result->free();
        return $out;
    }

    /**
     * Format an associative row as SQL values, using column types to decide
     * NULL vs raw-numeric vs hex-blob vs quoted-string. Uses
     * mysqli::real_escape_string for the quoted path — that's the canonical
     * defense for dumping arbitrary bytes into a SQL literal.
     *
     * @param array<string,string|null> $row
     * @param array<string,string>      $columns
     */
    private function formatRowValues(\mysqli $mysqli, array $row, array $columns): string
    {
        $vals = [];
        foreach ($columns as $name => $type) {
            $v = $row[$name] ?? null;
            if ($v === null) {
                $vals[] = 'NULL';
                continue;
            }
            // BLOB/binary columns → 0x{hex}. Avoids escape issues with
            // arbitrary bytes (notably 0x00 NUL which can't appear safely in
            // a single-quoted SQL string).
            if (str_contains($type, 'blob') || str_contains($type, 'binary')) {
                $vals[] = '0x' . bin2hex((string) $v);
                continue;
            }
            // Bit columns — emit as b'01010101'.
            if (str_starts_with($type, 'bit')) {
                $bits = '';
                $bytes = (string) $v;
                for ($i = 0; $i < strlen($bytes); $i++) {
                    $bits .= sprintf('%08b', ord($bytes[$i]));
                }
                $vals[] = "b'" . ltrim($bits, '0') . "'";
                // The empty case (all zeros) → b'0'.
                if (end($vals) === "b''") {
                    $vals[count($vals) - 1] = "b'0'";
                }
                continue;
            }
            // Numeric types — emit raw unquoted. INT/DECIMAL/FLOAT/etc.
            if (preg_match('/^(tinyint|smallint|mediumint|int|bigint|decimal|float|double|real|numeric)/', $type) === 1) {
                $vals[] = is_numeric($v) ? (string) $v : "'" . $mysqli->real_escape_string((string) $v) . "'";
                continue;
            }
            // Default: string-quoted, mysqli-escaped.
            $vals[] = "'" . $mysqli->real_escape_string((string) $v) . "'";
        }
        return implode(',', $vals);
    }

    /**
     * Backtick-quote an identifier safely. mysqli has no parameterized
     * identifiers; backticks + doubling embedded backticks is the standard
     * MySQL escape.
     */
    private function backtick(string $ident): string
    {
        return '`' . str_replace('`', '``', $ident) . '`';
    }

    /**
     * @return array{0:string,1:?int} [host, port]. Splits only on the LAST
     * colon and only if the suffix is all digits — defensive against IPv6 in
     * brackets, though no WP host known to do that.
     */
    private function splitHostPort(string $host): array
    {
        $port = null;
        if (str_contains($host, ':')) {
            $pos = strrpos($host, ':');
            $portPart = substr($host, $pos + 1);
            if ($portPart !== '' && ctype_digit($portPart)) {
                $port = (int) $portPart;
                $host = substr($host, 0, $pos);
            }
        }
        return [$host, $port];
    }

    /**
     * Scrub credentials from a thrown error message before re-surfacing.
     * mysqli error messages CAN include "Access denied for user 'foo'@'1.2.3.4'"
     * style strings — we strip them to a generic "auth failed" before letting
     * the message propagate. Adjusts WPvivid's same defense.
     */
    private function scrubMessage(string $msg): string
    {
        // Common patterns mysqli/PDO emit; replace user/host pairs.
        $msg = preg_replace("/user '[^']*'@'[^']*'/", "user '***'@'***'", $msg) ?? $msg;
        $msg = preg_replace('/password=\S+/i', 'password=***', $msg) ?? $msg;
        // Truncate to a sane length — caller's progress payload has a hard
        // ~4KB cap on the CP side.
        return substr($msg, 0, 200);
    }

    /** Wrap progress() in try/catch — a buggy CP poster MUST NOT abort the dump. */
    private function safeProgress(callable $progress, string $phase, array $detail): void
    {
        try {
            $progress($phase, $detail);
        } catch (\Throwable $ignored) {
            // Intentionally swallow — fire-and-forget.
        }
    }

    private function safeFilesize(string $path): int
    {
        $s = @filesize($path);
        return is_int($s) ? $s : 0;
    }
}
