<?php
/**
 * Minimal $wpdb double for DbCleanup tests: records prepared statements + writes
 * and returns canned id sets so the cleanup tasks can be asserted precisely.
 *
 * Updated for M38: get_results() now returns the data-free map rows for the
 * information_schema DATA_FREE query so the new dataFreeMap() path in
 * DbCleanup::runOptimizeTables() can detect eligible tables.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

/**
 * Emulates the wpdb surface DbCleanup touches (prepare/get_col/get_results/query).
 */
final class FakeCleanupWpdb
{
    public string $prefix = 'wp_';

    /** @var list<int> Canned ids returned by get_col() for most SELECT queries. */
    public array $idResults = [];

    /**
     * Canned ids returned by get_col() when the SQL is the term_taxonomy
     * SELECT (the first ids() call in runDeleteOrphanedTermRelationships).
     * When null, $idResults is used instead.
     *
     * @var list<int>|null
     */
    public ?array $termTaxonomyIds = null;

    /**
     * Canned ids returned by get_col() for the orphan object_id SELECT in
     * runDeleteOrphanedTermRelationships (the loop SELECT). When null,
     * $idResults is used instead.
     *
     * @var list<int>|null
     */
    public ?array $orphanObjectIds = null;

    /**
     * Canned table names that are eligible for OPTIMIZE (non-InnoDB + DATA_FREE > 0).
     * Used by get_results() for the information_schema DATA_FREE query AND by
     * get_col() for the legacy tableExists guard.
     *
     * @var list<string>
     */
    public array $optimizableTables = [];

    /** @var list<string> Prepared statement strings (post-substitution). */
    public array $prepared = [];

    /** @var list<string> Raw (pre-substitution) query templates. */
    public array $rawQueries = [];

    /** @var list<string> Write statements executed via query(). */
    public array $writes = [];

    /**
     * @param string $query SQL with placeholders.
     * @param mixed  ...$args Bound args.
     * @return string
     */
    public function prepare(string $query, ...$args): string
    {
        $this->rawQueries[] = $query;

        // Flatten a possible single array arg (DbCleanup spreads array chunks).
        $flat = [];
        foreach ($args as $a) {
            if (is_array($a)) {
                foreach ($a as $v) {
                    $flat[] = $v;
                }
            } else {
                $flat[] = $a;
            }
        }
        $i = 0;
        $sql = preg_replace_callback('/%[sdi]/', static function ($m) use (&$i, $flat) {
            $v = $flat[$i] ?? '';
            $i++;
            if ($m[0] === '%d') {
                return (string) (int) $v;
            }
            if ($m[0] === '%i') {
                // Mirrors core wpdb::prepare()'s %i identifier placeholder: backtick-quote,
                // doubling any embedded backticks.
                return '`' . str_replace('`', '``', (string) $v) . '`';
            }
            return "'" . $v . "'";
        }, $query);
        $this->prepared[] = (string) $sql;
        return (string) $sql;
    }

    /**
     * @param string $sql Prepared SELECT.
     * @return list<int|string>
     */
    public function get_col(string $sql): array
    {
        // tableExists check: returns a list of table names present.
        if (stripos($sql, 'information_schema') !== false && stripos($sql, 'DATA_FREE') === false) {
            // Return the table list for existence checks — always "found" for
            // known tables so action_scheduler guards pass cleanly.
            return $this->optimizableTables;
        }

        // Fix 1/2: discriminate the two ids() calls in runDeleteOrphanedTermRelationships.
        // First call: collects post-taxonomy term_taxonomy_ids (contains 'term_taxonomy'
        // and 'taxonomy NOT IN').
        if (stripos($sql, 'term_taxonomy') !== false && stripos($sql, 'taxonomy NOT IN') !== false) {
            return $this->termTaxonomyIds ?? $this->idResults;
        }
        // Loop call: collects orphan object_ids (contains 'term_relationships' and
        // 'NOT IN (SELECT ID FROM').
        if (stripos($sql, 'term_relationships') !== false && stripos($sql, 'NOT IN') !== false) {
            return $this->orphanObjectIds ?? $this->idResults;
        }

        return $this->idResults;
    }

    /**
     * Returns canned associative rows for the DATA_FREE information_schema query
     * (used by DbCleanup::dataFreeMap). All other get_results calls return [].
     *
     * @param string $sql  Prepared SELECT.
     * @param mixed  $mode Output mode (ignored).
     * @return list<array<string,mixed>>
     */
    public function get_results(string $sql, $mode = null): array
    {
        if (stripos($sql, 'DATA_FREE') !== false && stripos($sql, 'information_schema') !== false) {
            // Return one row per eligible table with a non-zero DATA_FREE so the
            // dataFreeMap() BEFORE-scan sees them; the AFTER-scan also returns them
            // (same fake), so bytes_freed = 0 (before == after). That's fine —
            // rows_deleted (table count) is what the tests assert.
            $rows = [];
            foreach ($this->optimizableTables as $tbl) {
                $rows[] = ['TABLE_NAME' => $tbl, 'DATA_FREE' => 1024];
            }
            return $rows;
        }
        return [];
    }

    /**
     * @param string $sql Statement.
     * @return int Affected rows.
     */
    public function query(string $sql): int
    {
        $this->writes[] = $sql;
        if (stripos($sql, 'DELETE FROM') === 0 && preg_match('/IN \(([^)]*)\)/', $sql, $m)) {
            return count(array_filter(explode(',', $m[1]), static fn ($x) => trim($x) !== ''));
        }
        // OPTIMIZE TABLE returns 1 (success) per table.
        if (stripos($sql, 'OPTIMIZE TABLE') !== false) {
            return 1;
        }
        return 1;
    }

    /**
     * Did any prepared statement bind $value (post-substitution) AND carry the
     * column fragment (matched against the RAW template, which still has %s/%d)?
     *
     * @param string $rawFragment SQL fragment with the placeholder (e.g. "post_type = %s").
     * @param string $value       Expected bound value.
     * @return bool
     */
    public function preparedWith(string $rawFragment, string $value): bool
    {
        $sawFragment = false;
        foreach ($this->rawQueries as $raw) {
            if (strpos($raw, $rawFragment) !== false) {
                $sawFragment = true;
                break;
            }
        }
        if (!$sawFragment) {
            return false;
        }
        foreach ($this->prepared as $p) {
            if (strpos($p, "'" . $value . "'") !== false) {
                return true;
            }
        }
        return false;
    }

    /**
     * Did any executed write contain $needle?
     *
     * @param string $needle SQL fragment.
     * @return bool
     */
    public function wroteLike(string $needle): bool
    {
        foreach ($this->writes as $w) {
            if (strpos($w, $needle) !== false) {
                return true;
            }
        }
        return false;
    }
}
