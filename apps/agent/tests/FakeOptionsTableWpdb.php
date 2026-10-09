<?php
/**
 * In-memory $wpdb double over the two tables WordPress keeps options in: the
 * per-site options table and, on multisite, the network table (sitemeta).
 *
 * It answers the statements uninstall issues and nothing else: a prepared
 * SELECT of rows by name, either the name column alone (get_col) or the row id
 * and name (get_results with ARRAY_A), and a prepared DELETE. A WHERE clause
 * is predicates joined by AND, each comparing one column with one placeholder.
 * A statement on the network table must be scoped to one network.
 *
 * Names compare the way MySQL compares them under WordPress's default
 * collations, which ignore case: '=' and LIKE alike, so a lookup or a delete
 * by name also reaches a row that differs from it only in case. In LIKE, '%'
 * is any run of characters, '_' is any single character, and '\' escapes the
 * next character, so a pattern that forgets to escape '_', or is not anchored,
 * over-matches here exactly as it would against a real database. Trailing-space
 * padding and accent folding are not modelled.
 *
 * HEX(<name column>) = %s compares bytes: it holds only when the bound value
 * is the upper-case hex of the stored name, exactly, which is the form HEX()
 * returns on MySQL, MariaDB and SQLite, and the only form SQLite's default
 * binary comparison accepts.
 *
 * The options table keeps its unique key on option_name by default, as core
 * creates it: under a case-insensitive collation that admits one row per name
 * whatever its case, so a statement over a table seeded with two such rows
 * throws. {@see $optionsUniqueKey} turns the key off, to model a table that
 * has lost it. The network table has no such key.
 *
 * Each row has an id, assigned the first time a statement sees it, as a
 * primary key would be. {@see renameRow()} changes a row's name and keeps its
 * id, and {@see $beforeDelete} runs before each DELETE, so a test can change
 * the table between uninstall's read and its delete.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

/**
 * Emulates the wpdb surface Lifecycle's uninstall touches.
 */
final class FakeOptionsTableWpdb
{
    /** Per-site options table name, as core exposes it. */
    public string $options = 'wp_options';

    /** Network table name, as core exposes it on multisite. */
    public string $sitemeta = 'wp_sitemeta';

    /** @var list<string> Every name a SELECT returned, across all calls. */
    public array $selected = [];

    /** @var list<string> Every LIKE pattern bound into a statement, across all calls. */
    public array $likePatterns = [];

    /** @var list<string> Every row a DELETE removed, as "table:name", across all calls. */
    public array $deleted = [];

    /**
     * When true, LIKE matches no row: a namespace sweep that finds nothing,
     * leaving a lookup by exact name as the only way to reach a row.
     */
    public bool $likeMatchesNothing = false;

    /**
     * Whether the options table has its unique key on option_name. False
     * models a table without it, where a row and a copy differing only in
     * case are both stored.
     */
    public bool $optionsUniqueKey = true;

    /**
     * Runs before each DELETE with this double and the statement's table name.
     *
     * @var (\Closure(self, string): void)|null
     */
    public ?\Closure $beforeDelete = null;

    /** @var array<string,mixed> Rows of the per-site options table, shared with the test by reference. */
    private array $optionRows;

    /** @var array<string,mixed> Rows of the network table for $networkId, shared with the test by reference. */
    private array $networkRows;

    private int $networkId;

    /** @var array<string,int> Row id by table and name, assigned on first sight. */
    private array $ids = [];

    private int $lastId = 0;

    /**
     * @param array<string,mixed> $optionRows  Per-site options table, by reference.
     * @param array<string,mixed> $networkRows Network table rows of $networkId, by reference.
     * @param int                 $networkId   The only network id whose rows exist.
     */
    public function __construct(array &$optionRows, array &$networkRows, int $networkId = 1)
    {
        $this->optionRows  = &$optionRows;
        $this->networkRows = &$networkRows;
        $this->networkId   = $networkId;
    }

    /**
     * Whether MySQL, under WordPress's default collations, holds two names
     * equal: case is ignored.
     *
     * @param string $a One name.
     * @param string $b The other.
     * @return bool
     */
    public static function collationEquals(string $a, string $b): bool
    {
        return strcasecmp($a, $b) === 0;
    }

    /**
     * Same escaping as core's wpdb::esc_like().
     *
     * @param string $text Raw text.
     * @return string
     */
    public function esc_like(string $text): string
    {
        return addcslashes($text, '_%\\');
    }

    /**
     * Carries the statement and its bound values through to execution.
     *
     * @param string $query   Statement with %s / %d placeholders.
     * @param mixed  ...$args Bound values (or one array of them, as core accepts).
     * @return string
     */
    public function prepare(string $query, ...$args): string
    {
        if (count($args) === 1 && is_array($args[0])) {
            $args = $args[0];
        }

        return (string) json_encode(['sql' => $query, 'args' => array_values($args)]);
    }

    /**
     * Run SELECT <name column> FROM <table> WHERE ...
     *
     * @param string $prepared Output of prepare().
     * @return list<string>
     */
    public function get_col(string $prepared): array
    {
        [$sql, $args] = self::decode($prepared);
        if (preg_match('/^SELECT (\w+) FROM (\S+) WHERE (.+)$/s', $sql, $m) !== 1) {
            throw new \LogicException('get_col() answers SELECT <name column> FROM <table> WHERE ... only: ' . $sql);
        }
        $table = $this->table($m[2]);
        if ($m[1] !== $table['name']) {
            throw new \LogicException('get_col() selects the name column only: ' . $sql);
        }

        $names = array_column($this->matching($table, $m[3], $args), 'name');
        $this->selected = array_merge($this->selected, $names);

        return $names;
    }

    /**
     * Run SELECT <id column> AS id, <name column> AS name FROM <table> WHERE ...
     * Values come back as strings, as mysqli returns them.
     *
     * @param string $prepared Output of prepare().
     * @param string $output   Must be ARRAY_A.
     * @return list<array{id:string,name:string}>
     */
    public function get_results(string $prepared, $output = 'OBJECT'): array
    {
        if ($output !== ARRAY_A) {
            throw new \LogicException('get_results() answers ARRAY_A only');
        }
        [$sql, $args] = self::decode($prepared);
        if (preg_match('/^SELECT (\w+) AS id, (\w+) AS name FROM (\S+) WHERE (.+)$/s', $sql, $m) !== 1) {
            throw new \LogicException('get_results() answers SELECT <id> AS id, <name> AS name FROM <table> WHERE ... only: ' . $sql);
        }
        $table = $this->table($m[3]);
        if ($m[1] !== $table['id'] || $m[2] !== $table['name']) {
            throw new \LogicException('get_results() selects the id and name columns only: ' . $sql);
        }

        $out = [];
        foreach ($this->matching($table, $m[4], $args) as $row) {
            $out[]            = ['id' => (string) $row['id'], 'name' => $row['name']];
            $this->selected[] = $row['name'];
        }

        return $out;
    }

    /**
     * Run DELETE FROM <table> WHERE ...
     *
     * @param string $prepared Output of prepare().
     * @return int Rows deleted.
     */
    public function query(string $prepared): int
    {
        [$sql, $args] = self::decode($prepared);
        if (preg_match('/^DELETE FROM (\S+) WHERE (.+)$/s', $sql, $m) !== 1) {
            throw new \LogicException('query() answers DELETE FROM <table> WHERE ... only: ' . $sql);
        }
        $table = $this->table($m[1]);
        if ($this->beforeDelete !== null) {
            ($this->beforeDelete)($this, $table['table']);
        }

        $rows = $this->matching($table, $m[2], $args);
        foreach ($rows as $row) {
            if ($table['scope'] === null) {
                unset($this->optionRows[$row['name']]);
            } else {
                unset($this->networkRows[$row['name']]);
            }
            $this->deleted[] = $table['table'] . ':' . $row['name'];
        }

        return count($rows);
    }

    /**
     * Give the row stored as $from the name $to and keep its id, as an UPDATE
     * of the name column by primary key would.
     *
     * @param string $table Table name, as a statement names it.
     * @param string $from  The row's name, exactly as stored.
     * @param string $to    Its new name.
     * @return void
     */
    public function renameRow(string $table, string $from, string $to): void
    {
        $columns = $this->table($table);
        if ($columns['scope'] === null) {
            $rows = &$this->optionRows;
        } else {
            $rows = &$this->networkRows;
        }
        if (!array_key_exists($from, $rows) || array_key_exists($to, $rows)) {
            throw new \LogicException('renameRow() needs a stored row and a free name: ' . $from . ' to ' . $to);
        }

        $id = $this->idFor($columns['table'], $from);
        unset($this->ids[$columns['table'] . "\0" . $from]);
        $this->ids[$columns['table'] . "\0" . $to] = $id;

        $rows[$to] = $rows[$from];
        unset($rows[$from]);
    }

    /**
     * Whether $subject matches the SQL LIKE $pattern, case-insensitively.
     *
     * @param string $subject Candidate name.
     * @param string $pattern LIKE pattern with backslash escapes.
     * @return bool
     */
    public static function likeMatches(string $subject, string $pattern): bool
    {
        $regex  = '';
        $length = strlen($pattern);
        for ($i = 0; $i < $length; $i++) {
            $char = $pattern[$i];
            if ($char === '\\' && $i + 1 < $length) {
                $i++;
                $regex .= preg_quote($pattern[$i], '/');
                continue;
            }
            if ($char === '%') {
                $regex .= '.*';
                continue;
            }
            if ($char === '_') {
                $regex .= '.';
                continue;
            }
            $regex .= preg_quote($char, '/');
        }

        return preg_match('/^' . $regex . '$/is', $subject) === 1;
    }

    /**
     * @param string $prepared Output of prepare().
     * @return array{0:string,1:list<mixed>}
     */
    private static function decode(string $prepared): array
    {
        $decoded = json_decode($prepared, true);
        if (!is_array($decoded) || !isset($decoded['sql'], $decoded['args']) || !is_array($decoded['args'])) {
            throw new \LogicException('expected a statement built by prepare()');
        }

        return [(string) $decoded['sql'], array_values($decoded['args'])];
    }

    /**
     * The columns of a table this double knows.
     *
     * @param string $name Table name from the statement.
     * @return array{table:string,id:string,name:string,scope:?string}
     */
    private function table(string $name): array
    {
        if ($name === $this->options) {
            return ['table' => $name, 'id' => 'option_id', 'name' => 'option_name', 'scope' => null];
        }
        if ($name === $this->sitemeta) {
            return ['table' => $name, 'id' => 'meta_id', 'name' => 'meta_key', 'scope' => 'site_id'];
        }

        throw new \LogicException('statement names neither options table: ' . $name);
    }

    /**
     * Rows of $table that satisfy every predicate of $where.
     *
     * @param array{table:string,id:string,name:string,scope:?string} $table Table columns.
     * @param string                                                  $where Predicates joined by AND.
     * @param list<mixed>                                             $args  Bound values, in order.
     * @return list<array{id:int,name:string}>
     */
    private function matching(array $table, string $where, array $args): array
    {
        $predicates = explode(' AND ', $where);
        if (count($predicates) !== count($args)) {
            throw new \LogicException('placeholder count does not match bound values: ' . $where);
        }

        if ($table['scope'] === null && $this->optionsUniqueKey) {
            $this->assertUniqueOptionNames();
        }

        $tests  = [];
        $scoped = false;
        foreach ($predicates as $i => $predicate) {
            if (preg_match('/^HEX\((\w+)\) = %s$/', trim($predicate), $h) === 1) {
                if ($h[1] !== $table['name']) {
                    throw new \LogicException('HEX() compares the name column only: ' . $predicate);
                }
                $hex     = (string) $args[$i];
                $tests[] = static fn (array $row): bool => strtoupper(bin2hex($row['name'])) === $hex;
                continue;
            }
            if (preg_match('/^(\w+) (=|LIKE) (%s|%d)$/', trim($predicate), $p) !== 1) {
                throw new \LogicException('unsupported predicate: ' . $predicate);
            }
            [, $column, $operator, $placeholder] = $p;
            $value = $args[$i];

            if ($column === $table['name'] && $placeholder === '%s') {
                $bound = (string) $value;
                if ($operator === 'LIKE') {
                    $this->likePatterns[] = $bound;
                    $tests[] = fn (array $row): bool => !$this->likeMatchesNothing && self::likeMatches($row['name'], $bound);
                } else {
                    $tests[] = static fn (array $row): bool => self::collationEquals($row['name'], $bound);
                }
            } elseif ($column === $table['id'] && $operator === '=' && $placeholder === '%d') {
                $id      = (int) $value;
                $tests[] = static fn (array $row): bool => $row['id'] === $id;
            } elseif ($table['scope'] !== null && $column === $table['scope'] && $operator === '=' && $placeholder === '%d') {
                $scoped  = true;
                $network = (int) $value;
                $tests[] = fn (array $row): bool => $network === $this->networkId;
            } else {
                throw new \LogicException('unsupported predicate: ' . $predicate);
            }
        }
        if ($table['scope'] !== null && !$scoped) {
            // The network table holds rows for every network.
            throw new \LogicException('a network-table statement must be scoped to one network: ' . $where);
        }

        $rows = $table['scope'] === null ? $this->optionRows : $this->networkRows;
        $hits = [];
        foreach (array_keys($rows) as $name) {
            $row = ['id' => $this->idFor($table['table'], (string) $name), 'name' => (string) $name];
            foreach ($tests as $test) {
                if (!$test($row)) {
                    continue 2;
                }
            }
            $hits[] = $row;
        }

        return $hits;
    }

    /**
     * Refuse an options table holding two names its unique key admits only
     * one of: under a case-insensitive collation, two names equal but for case.
     *
     * @return void
     */
    private function assertUniqueOptionNames(): void
    {
        $seen = [];
        foreach (array_keys($this->optionRows) as $name) {
            $folded = strtolower((string) $name);
            if (isset($seen[$folded])) {
                throw new \LogicException(
                    'the options table holds ' . $seen[$folded] . ' and ' . $name . ', which its unique key admits only one of; set $optionsUniqueKey = false to model a table without that key'
                );
            }
            $seen[$folded] = (string) $name;
        }
    }

    /**
     * The id of a row, assigned the first time a statement sees it.
     *
     * @param string $table Table name.
     * @param string $name  Row name, exactly as stored.
     * @return int
     */
    private function idFor(string $table, string $name): int
    {
        $key = $table . "\0" . $name;
        if (!isset($this->ids[$key])) {
            $this->ids[$key] = ++$this->lastId;
        }

        return $this->ids[$key];
    }
}
