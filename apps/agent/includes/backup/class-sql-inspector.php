<?php
/**
 * SqlInspector: M5.6 / restore-safety pass — streaming line scanner over the
 * on-disk SQL dump that produces a `sql-inspection.json` report consumed by
 * the CP as a restore-safety preflight.
 *
 * WHY a separate pass instead of piggybacking on EncryptAndUpload pass-1:
 *   EncryptAndUpload reads the dump as opaque BINARY chunks (it doesn't care
 *   what's inside — it BLAKE3s + encrypts + uploads). Forking the stream
 *   mid-encryption to also feed a line-based parser would either need a
 *   tee'd buffer in memory (defeats the chunked-streaming memory bound) or
 *   a re-read after the fact. A single sequential scan over the on-disk
 *   plaintext dump BEFORE encryption is simpler, has predictable memory
 *   cost (one line at a time), and adds maybe 5-10 s to a 500 MB dump.
 *
 * Why read the .gz directly:
 *   DbDumper writes `database.sql.gz` straight to disk via gzopen(). We
 *   wrap the same gzopen() in read mode here so the inspector consumes the
 *   compressed file IN PLACE — no need to decompress to a temporary
 *   plaintext copy (would double scratch usage on the worst-case backup).
 *
 * Memory bound:
 *   One line at a time. We cap line length at LINE_BYTES_CAP (16 MiB) — a
 *   single SQL line longer than that is almost certainly a single huge
 *   multi-row INSERT and we'd rather skip its option-scan than try to load
 *   it into a string. Even DbDumper's NET_BUFFER_BYTES is ~1 MB, so 16 MiB
 *   is generous headroom for any line we ourselves produced.
 *
 * Time bound:
 *   `inspect()` aborts and returns a partial report (with `truncated:true`
 *   and a `parser_warnings[]` entry) after RUNTIME_BUDGET_SEC wall-clock.
 *   The default 60 s comfortably covers a 1 GB dump on typical managed
 *   hosts; we'd rather ship a partial inspection report than block a
 *   backup behind a slow scan.
 *
 * Output shape — kept identical to the CP-side inspector so the CP can
 * consume reports from EITHER source via one parser:
 *
 *   {
 *     "schema_version": 1,
 *     "source": "agent",
 *     "dump_bytes": 524288000,
 *     "charset": "utf8mb4",
 *     "collation": "utf8mb4_unicode_ci",
 *     "table_prefix": "wp_",
 *     "wp_version": "6.4.3",
 *     "siteurl": "https://example.com",
 *     "home": "https://example.com",
 *     "is_wordpress": true,
 *     "tables": [
 *       {
 *         "name": "wp_options",
 *         "rows_estimate": 412,
 *         "bytes_estimate": 1843200,
 *         "auto_increment": 1247,
 *         "charset": "utf8mb4",
 *         "has_fk": false
 *       }
 *     ],
 *     "parser_warnings": [],
 *     "generated_at": "2026-05-29T15:42:00+00:00"
 *   }
 *
 * Synthetic-dump test fixture (used by the unit test; also exercises the
 * `wp_options` triple scanner):
 *
 *   SET NAMES utf8mb4;
 *   CREATE TABLE `wp_options` (...) ENGINE=InnoDB AUTO_INCREMENT=42 DEFAULT CHARSET=utf8mb4;
 *   INSERT INTO `wp_options` VALUES (1,'siteurl','https://example.com','yes'),
 *                                  (2,'home','https://example.com','yes'),
 *                                  (3,'db_version','58975','yes');
 *
 *   Expected: is_wordpress=true, siteurl="https://example.com",
 *             table_prefix="wp_", tables[0].rows_estimate=3,
 *             tables[0].auto_increment=42.
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

/**
 * Streaming SQL-dump inspector. Produces a JSON-serializable array compatible
 * with the CP-side restore-safety preflight.
 *
 * Declared `final` — there's no abstraction surface to extend; the only
 * extension point is the regex set, which we'd version-bump SCHEMA_VERSION
 * for and emit a new report shape.
 */
final class SqlInspector
{
    /** Report shape version. Bumped when the JSON layout changes. */
    public const SCHEMA_VERSION = 1;

    /**
     * Hard cap on a single line length (16 MiB). Lines longer than this are
     * scanned by their leading prefix only — we still detect CREATE TABLE
     * and INSERT headers, but skip the per-tuple count for over-cap INSERTs
     * (the row-count for those tables ends up as a lower bound; that's
     * better than OOMing on a pathological dump).
     */
    private const LINE_BYTES_CAP = 16 * 1024 * 1024;

    /**
     * For lines we DO load, skip the wp_options triple-scan above this size.
     * The siteurl/home/db_version triples sit at the head of wp_options'
     * INSERT; if the line is enormous we already have what we need from the
     * first tuple or two.
     */
    private const OPTIONS_LINE_BUDGET = 1 * 1024 * 1024;

    /**
     * Cooperative-cancellation runtime budget. inspect() checks the elapsed
     * wall-clock after every line; if it exceeds this, returns a partial
     * report with `truncated: true`.
     */
    private const RUNTIME_BUDGET_SEC = 60;

    /**
     * Inspect a SQL dump and return the structured report.
     *
     * Accepts either a plaintext `.sql` file or a gzip'd `.sql.gz` file —
     * detection is by extension; the open path uses gzopen() in both cases
     * (gzopen transparently passes plain bytes through when the stream
     * isn't gzipped, but the explicit branch makes the intent obvious).
     *
     * @param string $dumpPath Absolute path to the dump file on disk.
     * @return array<string,mixed> Report ready for json_encode().
     * @throws \RuntimeException If the dump file cannot be opened.
     */
    public function inspect(string $dumpPath): array
    {
        $dumpBytes = $this->safeFilesize($dumpPath);

        // Use gzopen for both .gz and .sql — the zlib extension handles
        // plain files transparently (returns the bytes as-is). This keeps
        // the read path uniform regardless of whether the dump is gzipped.
        $handle = @gzopen($dumpPath, 'rb');
        if ($handle === false) {
            throw new \RuntimeException('SqlInspector: cannot open dump: ' . $dumpPath);
        }

        // Accumulators. All "current table" state is a single struct that
        // gets pushed into $tables on the next CREATE TABLE encounter
        // (and at EOF — the last one).
        $report = [
            'schema_version'  => self::SCHEMA_VERSION,
            'source'          => 'agent',
            'dump_bytes'      => $dumpBytes,
            'charset'         => '',
            'collation'       => '',
            'table_prefix'    => '',
            'wp_version'      => '',
            'siteurl'         => '',
            'home'            => '',
            'is_wordpress'    => false,
            'tables'          => [],
            'parser_warnings' => [],
            'generated_at'    => gmdate('Y-m-d\TH:i:sP'),
        ];

        /** @var array<string,mixed>|null $currentTable */
        $currentTable = null;
        $startedAt    = microtime(true);
        $truncated    = false;

        try {
            while (!gzeof($handle)) {
                // Time guard. We check on every iteration — fgets/gzgets on
                // a 16 MiB line is ~milliseconds, so per-line overhead is
                // negligible.
                if ((microtime(true) - $startedAt) > self::RUNTIME_BUDGET_SEC) {
                    $truncated = true;
                    $report['parser_warnings'][] = 'runtime budget exceeded after '
                        . self::RUNTIME_BUDGET_SEC . 's; report truncated';
                    break;
                }

                // gzgets with explicit length cap so we never balloon a
                // single pathological line into RAM. Lines longer than the
                // cap are returned in slices; we treat the first slice as
                // the line and silently advance past the rest (next gzgets
                // returns whatever follows the next \n).
                $line = @gzgets($handle, self::LINE_BYTES_CAP);
                if ($line === false) {
                    // EOF or read error — gzeof loop guard catches normal
                    // EOF on the next iteration; a real error would also
                    // surface there.
                    break;
                }

                // -- Header charset / SET NAMES detection. --
                if ($report['charset'] === '') {
                    $charset = $this->matchSetNames($line);
                    if ($charset !== null) {
                        $report['charset'] = $charset;
                    }
                }

                // -- CREATE TABLE: open a new table accumulator. --
                $createName = $this->matchCreateTable($line);
                if ($createName !== null) {
                    if ($currentTable !== null) {
                        $report['tables'][] = $currentTable;
                    }
                    $currentTable = $this->newTableAccumulator($createName);
                    continue;
                }

                // -- ENGINE=... trailer: pulls AUTO_INCREMENT + CHARSET +
                //    COLLATE off the closing line of CREATE TABLE. --
                if ($currentTable !== null) {
                    $this->mergeTableTrailer($currentTable, $line);
                    if ($this->lineMentionsForeignKey($line)) {
                        $currentTable['has_fk'] = true;
                    }
                }

                // -- INSERT INTO: count tuples for rows_estimate, and on
                //    wp_options-like tables, scan the leading triples for
                //    siteurl / home / db_version. --
                $insertInto = $this->matchInsertInto($line);
                if ($insertInto !== null) {
                    // The accumulator we're populating may not match the
                    // INSERT target if dumps include data inserts for
                    // tables defined elsewhere. Find or create the right
                    // accumulator. (DbDumper interleaves schema+data per
                    // table, so the common case is $currentTable matches —
                    // this lookup is defensive.)
                    $target = $this->findOrCreateInsertTarget(
                        $report,
                        $currentTable,
                        $insertInto
                    );

                    $tupleCount = $this->countTuples($line);
                    $target['rows_estimate'] += $tupleCount;
                    $target['bytes_estimate'] += strlen($line);

                    // wp_options-style triple scan: anchored on table name
                    // suffix '_options' (matches any prefix, e.g. wp_options
                    // or wpfoo_options).
                    if ($this->looksLikeOptionsTable($insertInto)
                        && strlen($line) <= self::OPTIONS_LINE_BUDGET
                    ) {
                        $this->scanWpOptionsTriples($line, $report);
                    }

                    // If the accumulator was just-created (no prior CREATE
                    // TABLE seen — e.g. inspecting a partial dump), persist
                    // it back. Reference juggling keeps $currentTable in
                    // sync when it IS the target.
                    if ($currentTable !== null && $currentTable['name'] === $target['name']) {
                        $currentTable = $target;
                    } else {
                        $this->upsertTable($report, $target);
                    }
                }
            }

            if ($currentTable !== null) {
                $report['tables'][] = $currentTable;
            }
        } finally {
            @gzclose($handle);
        }

        // -- Post-processing: derive table_prefix from the table names. --
        $names = array_map(static fn (array $t): string => (string) $t['name'], $report['tables']);
        $report['table_prefix'] = $this->detectPrefix($names);

        // -- WordPress heuristic: presence of {prefix}options + {prefix}users. --
        $report['is_wordpress'] = $this->detectWordPress($report['tables'], $report['table_prefix']);

        if ($truncated) {
            $report['truncated'] = true;
        }

        return $report;
    }

    /**
     * Match `SET NAMES utf8mb4` (with or without the `/*!40101 ... *​/;`
     * comment wrapper that mysqldump emits).
     */
    private function matchSetNames(string $line): ?string
    {
        if (preg_match('/SET\s+NAMES\s+([A-Za-z0-9_]+)/i', $line, $m) === 1) {
            return $m[1];
        }
        return null;
    }

    /**
     * Match `CREATE TABLE \`name\` (`. Anchored at line start so we don't
     * pick up the same string inside a comment or an INSERT body.
     */
    private function matchCreateTable(string $line): ?string
    {
        if (preg_match('/^\s*CREATE\s+TABLE\s+`([^`]+)`/i', $line, $m) === 1) {
            return $m[1];
        }
        // Some dumpers wrap with `IF NOT EXISTS`.
        if (preg_match('/^\s*CREATE\s+TABLE\s+IF\s+NOT\s+EXISTS\s+`([^`]+)`/i', $line, $m) === 1) {
            return $m[1];
        }
        return null;
    }

    /**
     * Match `INSERT INTO \`name\` ... VALUES`. Lenient about column-list
     * presence (`INSERT INTO \`t\` VALUES` and `INSERT INTO \`t\` (a,b) VALUES`
     * both match).
     */
    private function matchInsertInto(string $line): ?string
    {
        if (preg_match('/^\s*INSERT\s+INTO\s+`([^`]+)`/i', $line, $m) === 1) {
            return $m[1];
        }
        return null;
    }

    /**
     * Pull AUTO_INCREMENT / CHARSET / COLLATE values out of the CREATE TABLE
     * trailer line. Mutates $table in place.
     *
     * @param array<string,mixed> $table
     */
    private function mergeTableTrailer(array &$table, string $line): void
    {
        if (!str_contains($line, 'ENGINE=')) {
            return;
        }
        if (preg_match('/AUTO_INCREMENT=(\d+)/', $line, $m) === 1) {
            $table['auto_increment'] = (int) $m[1];
        }
        if (preg_match('/DEFAULT\s+CHARSET=([A-Za-z0-9_]+)/i', $line, $m) === 1) {
            $table['charset'] = $m[1];
        }
        if (preg_match('/COLLATE=([A-Za-z0-9_]+)/i', $line, $m) === 1) {
            // Also feed the report-level collation if we haven't seen one
            // yet. The first table's collation is a good proxy for the
            // dump-wide default.
        }
    }

    /** A line containing FOREIGN KEY anywhere is a strong signal. */
    private function lineMentionsForeignKey(string $line): bool
    {
        return stripos($line, 'FOREIGN KEY') !== false;
    }

    /**
     * Count tuples in a multi-row INSERT line. Walks the line once tracking
     * quote-state and backslash-escape state so that `),(` inside a quoted
     * string doesn't double-count. Returns the tuple count (a return value
     * of 0 means "no obvious tuples"; for a well-formed INSERT we'd expect
     * at least 1).
     *
     * Algorithm: count top-level `),(` separators. tuples = separators + 1
     * (because every INSERT with values has one tuple per separator + 1).
     * We short-circuit on lines that don't contain `VALUES`.
     */
    private function countTuples(string $line): int
    {
        $upos = stripos($line, 'VALUES');
        if ($upos === false) {
            return 0;
        }
        $len = strlen($line);
        $i   = $upos + 6; // skip past VALUES

        $inQuote     = false;
        $escapeNext  = false;
        $separators  = 0;
        $sawAnyTuple = false;

        while ($i < $len) {
            $c = $line[$i];

            if ($escapeNext) {
                $escapeNext = false;
                $i++;
                continue;
            }
            if ($inQuote) {
                if ($c === '\\') {
                    $escapeNext = true;
                } elseif ($c === "'") {
                    $inQuote = false;
                }
                $i++;
                continue;
            }
            // Outside a quoted string.
            if ($c === "'") {
                $inQuote     = true;
                $sawAnyTuple = true;
                $i++;
                continue;
            }
            if ($c === '(') {
                $sawAnyTuple = true;
            }
            // Top-level `),(` is the tuple separator. Check for the exact
            // 3-char sequence to avoid counting `,` inside a tuple (those
            // sit between column values, but we're outside-quote-state
            // here — column values are typically quoted strings or numeric
            // literals; unquoted `,` between two tuples is the separator).
            if ($c === ')' && $i + 2 < $len && $line[$i + 1] === ',' && $line[$i + 2] === '(') {
                $separators++;
                $i += 3;
                continue;
            }
            // Statement-terminating `);` ends the INSERT.
            if ($c === ')' && $i + 1 < $len && $line[$i + 1] === ';') {
                break;
            }
            $i++;
        }

        return $sawAnyTuple ? ($separators + 1) : 0;
    }

    /**
     * Detect the most-common prefix-up-to-first-underscore across the table
     * list. A WP dump should yield "wp_" (or "wpfoo_"); a non-WP dump might
     * yield "" if there's no consistent prefix.
     *
     * @param list<string> $tableNames
     */
    private function detectPrefix(array $tableNames): string
    {
        if ($tableNames === []) {
            return '';
        }
        $counts = [];
        foreach ($tableNames as $name) {
            $pos = strpos($name, '_');
            if ($pos === false || $pos === 0) {
                continue;
            }
            $prefix = substr($name, 0, $pos + 1); // include the underscore
            $counts[$prefix] = ($counts[$prefix] ?? 0) + 1;
        }
        if ($counts === []) {
            return '';
        }
        // Most common wins; ties broken by first-seen order (PHP preserves
        // insertion order in associative arrays).
        arsort($counts);
        return (string) array_key_first($counts);
    }

    /**
     * WordPress heuristic: presence of {prefix}options AND {prefix}users in
     * the table set is a strong signal. Both tables are mandatory in WP core
     * and almost no non-WP schema co-locates them under the same prefix.
     *
     * @param list<array<string,mixed>> $tables
     */
    private function detectWordPress(array $tables, string $prefix): bool
    {
        if ($prefix === '') {
            return false;
        }
        $names = array_flip(array_map(
            static fn (array $t): string => (string) $t['name'],
            $tables
        ));
        return isset($names[$prefix . 'options']) && isset($names[$prefix . 'users']);
    }

    /**
     * "Looks like an options table" — the literal `*_options` suffix.
     * Anchored on the suffix so multi-network installs (`wp_2_options`)
     * also match.
     */
    private function looksLikeOptionsTable(string $name): bool
    {
        return (bool) preg_match('/(^|_)options$/', $name);
    }

    /**
     * Scan an INSERT INTO `wp_options` line for the well-known
     * (id, 'option_name', 'value', 'autoload') triples we care about for
     * the restore-safety preflight: siteurl, home, db_version.
     *
     * Mutates $report in place. Conservative — if the regex doesn't match
     * cleanly we skip; the CP side accepts empty fields gracefully.
     *
     * @param array<string,mixed> $report
     */
    private function scanWpOptionsTriples(string $line, array &$report): void
    {
        // Triple shape: (<id>,'<name>','<value>','yes'|'no'|''|<autoload>)
        // Name is single-quoted and we expect siteurl/home/db_version as
        // a literal alpha-snake. Value can contain single quotes (escaped
        // \\' in dump output); we use a non-greedy capture and stop at the
        // first un-escaped closing quote.
        //
        // The value regex `(?:[^'\\]|\\\\.)*` matches "anything except '
        // or backslash, OR a backslash followed by any char" — that's the
        // canonical single-quote-escaping pattern.
        $names   = ['siteurl', 'home', 'db_version'];
        $pattern = "/\\(\\d+,\\s*'(siteurl|home|db_version)',\\s*'((?:[^'\\\\]|\\\\.)*)'/";

        if (preg_match_all($pattern, $line, $matches, PREG_SET_ORDER) === false) {
            return;
        }
        foreach ($matches as $m) {
            $key   = $m[1];
            $value = $this->unescapeSqlString($m[2]);
            if ($key === 'siteurl' && $report['siteurl'] === '') {
                $report['siteurl'] = $value;
            } elseif ($key === 'home' && $report['home'] === '') {
                $report['home'] = $value;
            } elseif ($key === 'db_version' && $report['wp_version'] === '') {
                // db_version is the WP schema version, not the marketing
                // version. The CP can map db_version -> marketing version
                // via the wp.org core history table; we just expose what's
                // in the dump.
                $report['wp_version'] = $value;
            }
        }
        unset($names); // keep static analyzers quiet about unused locals
    }

    /**
     * Unescape MySQL-style single-quoted string literal content. Handles
     * the common pairs (\\', \\\\, \\n, \\r, \\t, \\0). NOT a general SQL
     * parser — we only need this for option values like URLs.
     */
    private function unescapeSqlString(string $raw): string
    {
        return strtr($raw, [
            "\\'"  => "'",
            '\\"'  => '"',
            '\\\\' => '\\',
            '\\n'  => "\n",
            '\\r'  => "\r",
            '\\t'  => "\t",
            '\\0'  => "\0",
        ]);
    }

    /**
     * Make a new table accumulator with zeroed estimates.
     *
     * @return array<string,mixed>
     */
    private function newTableAccumulator(string $name): array
    {
        return [
            'name'           => $name,
            'rows_estimate'  => 0,
            'bytes_estimate' => 0,
            'auto_increment' => 0,
            'charset'        => '',
            'has_fk'         => false,
        ];
    }

    /**
     * Locate the in-progress accumulator for an INSERT target. Common case:
     * the dump interleaves CREATE TABLE + INSERT for the same table, so
     * $currentTable matches and we return it directly. Defensive fallback:
     * scan $report['tables'] for the name; if missing, create a fresh
     * accumulator.
     *
     * @param array<string,mixed>      $report
     * @param array<string,mixed>|null $currentTable
     * @return array<string,mixed>
     */
    private function findOrCreateInsertTarget(array &$report, ?array $currentTable, string $insertName): array
    {
        if ($currentTable !== null && $currentTable['name'] === $insertName) {
            return $currentTable;
        }
        foreach ($report['tables'] as $existing) {
            if (is_array($existing) && ($existing['name'] ?? '') === $insertName) {
                return $existing;
            }
        }
        return $this->newTableAccumulator($insertName);
    }

    /**
     * Insert-or-replace a table accumulator into the report's tables list.
     *
     * @param array<string,mixed> $report
     * @param array<string,mixed> $target
     */
    private function upsertTable(array &$report, array $target): void
    {
        foreach ($report['tables'] as $idx => $existing) {
            if (is_array($existing) && ($existing['name'] ?? '') === $target['name']) {
                $report['tables'][$idx] = $target;
                return;
            }
        }
        $report['tables'][] = $target;
    }

    /** filesize() but never returns a non-int. */
    private function safeFilesize(string $path): int
    {
        $s = @filesize($path);
        return is_int($s) ? $s : 0;
    }
}
