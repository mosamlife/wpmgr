<?php
/**
 * In-memory $wpdb double over the options table, for the statements that claim
 * the key-scheme purge and nothing else: the INSERT IGNORE that records the
 * scheme when no row exists, the SELECT that reads the row as stored, and the
 * UPDATE that replaces it only from the value that was read.
 *
 * The rows are the test's own option store, shared by reference, so
 * get_option() answers what a claim wrote, as it does against a real table.
 * Like the real table it enforces a unique key on option_name: an INSERT IGNORE
 * of a name that exists changes nothing and reports 0 rows, and an UPDATE
 * conditional on the stored value reports 0 rows once another statement has
 * changed it.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

/**
 * Emulates the wpdb surface the key-scheme claim touches.
 */
final class FakeKeySchemeWpdb
{
    /** Options table name, as core exposes it. */
    public string $options = 'wp_options';

    /**
     * When true, every write fails the way it does on a read-only database:
     * query() returns false and the table is unchanged.
     */
    public bool $writesFail = false;

    /** @var list<string> Every INSERT and UPDATE that was attempted, as "VERB option_name". */
    public array $writes = [];

    /** @var array<string,mixed> Rows of the options table, shared with the test by reference. */
    private array $rows;

    /**
     * @param array<string,mixed> $rows Options table, by reference.
     */
    public function __construct(array &$rows)
    {
        $this->rows = &$rows;
    }

    /**
     * Carries the statement and its bound values through to execution.
     *
     * @param string $query   Statement with %s placeholders.
     * @param mixed  ...$args Bound values.
     * @return string
     */
    public function prepare(string $query, ...$args): string
    {
        if (count($args) === 1 && is_array($args[0])) {
            $args = $args[0];
        }

        return (string) json_encode(['sql' => preg_replace('/\s+/', ' ', $query), 'args' => array_values($args)]);
    }

    /**
     * Run INSERT IGNORE or UPDATE.
     *
     * @param string $prepared Output of prepare().
     * @return int|false Rows changed, or false when the write fails.
     */
    public function query(string $prepared)
    {
        [$sql, $args] = self::decode($prepared);

        if ($sql === "INSERT IGNORE INTO %i (option_name, option_value, autoload) VALUES (%s, %s, %s)") {
            $this->assertOptionsTable($args[0]);
            $this->writes[] = 'INSERT ' . $args[1];
            if ($this->writesFail) {
                return false;
            }
            if (array_key_exists($args[1], $this->rows)) {
                return 0;
            }
            $this->rows[$args[1]] = $args[2];

            return 1;
        }

        if ($sql === 'UPDATE %i SET option_value = %s WHERE option_name = %s AND option_value = %s') {
            $this->assertOptionsTable($args[0]);
            $this->writes[] = 'UPDATE ' . $args[2];
            if ($this->writesFail) {
                return false;
            }
            if (!array_key_exists($args[2], $this->rows) || (string) $this->rows[$args[2]] !== $args[3]) {
                return 0;
            }
            $this->rows[$args[2]] = $args[1];

            return 1;
        }

        throw new \LogicException('statement is not one the key-scheme claim issues: ' . $sql);
    }

    /**
     * Run SELECT option_value FROM %i WHERE option_name = %s
     *
     * @param string $prepared Output of prepare().
     * @return string|null The stored value, or null when there is no row.
     */
    public function get_var(string $prepared): ?string
    {
        [$sql, $args] = self::decode($prepared);
        if ($sql !== 'SELECT option_value FROM %i WHERE option_name = %s') {
            throw new \LogicException('statement is not one the key-scheme claim issues: ' . $sql);
        }
        $this->assertOptionsTable($args[0]);

        return array_key_exists($args[1], $this->rows) ? (string) $this->rows[$args[1]] : null;
    }

    /**
     * The identifier bound to %i must be this options table.
     *
     * @param string $table Identifier as bound.
     * @return void
     */
    private function assertOptionsTable(string $table): void
    {
        if ($table !== $this->options) {
            throw new \LogicException('statement names a table other than the options table: ' . $table);
        }
    }

    /**
     * @param string $prepared Output of prepare().
     * @return array{0:string,1:list<string>}
     */
    private static function decode(string $prepared): array
    {
        $decoded = json_decode($prepared, true);
        if (!is_array($decoded) || !isset($decoded['sql'], $decoded['args']) || !is_array($decoded['args'])) {
            throw new \LogicException('expected a statement built by prepare()');
        }

        return [(string) $decoded['sql'], array_map('strval', array_values($decoded['args']))];
    }
}
