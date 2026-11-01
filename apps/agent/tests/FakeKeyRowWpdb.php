<?php
/**
 * In-memory $wpdb double over the options table, for the keystore's own
 * reads and writes of the backup-key row.
 *
 * It answers exactly the three statements Keystore issues for that row and
 * throws on anything else: the aggregate SELECT that reads it, the plain
 * INSERT that creates it, and the UPDATE that fills it only while it is empty.
 * An INSERT of a row that exists fails as a duplicate key does: query()
 * returns false and last_error is set.
 * Rows are the test's own options array, shared by reference, so a value the
 * test's get_option() / update_option() stubs see is the value the table
 * holds.
 *
 * {@see $failSelects} makes the next SELECTs fail the way a MySQL error does:
 * get_row() returns null and last_error is set. {@see $beforeInsert} runs
 * before each INSERT, so a test can store a row from "another request"
 * between the keystore's read and its write.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

/**
 * Emulates the wpdb surface Keystore touches for the backup-key row.
 */
final class FakeKeyRowWpdb
{
    /** Options table name, as core exposes it. */
    public string $options = 'wp_options';

    /** Table prefix, as core exposes it. */
    public string $prefix = 'wp_';

    /** Error text of the last statement, '' when it succeeded, as core keeps it. */
    public string $last_error = '';

    /** How many of the next SELECTs fail. */
    public int $failSelects = 0;

    /** SELECTs that failed so far. */
    public int $failedSelects = 0;

    /** Rows an INSERT created so far. */
    public int $inserted = 0;

    /** @var array<string,int> Rows an INSERT created so far, by option name. */
    public array $insertedByName = [];

    /** INSERTs that failed as a duplicate key so far. */
    public int $duplicateInserts = 0;

    /** Whether errors are suppressed, as suppress_errors() sets it. */
    public bool $suppressErrors = false;

    /** @var list<bool> Whether errors were suppressed while each INSERT ran. */
    public array $insertSuppressed = [];

    /** @var list<string> Every statement run, in order. */
    public array $statements = [];

    /**
     * Runs before each INSERT.
     *
     * @var (\Closure(): void)|null
     */
    public ?\Closure $beforeInsert = null;

    /** @var array<string,mixed> Options table rows, shared with the test by reference. */
    private array $rows;

    /**
     * @param array<string,mixed> $rows Options table rows, by reference.
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

        return (string) json_encode(['sql' => $query, 'args' => array_values($args)]);
    }

    /**
     * Run the aggregate SELECT of one option row.
     *
     * @param string $prepared Output of prepare().
     * @param string $output   Must be ARRAY_A.
     * @return array<string,string|null>|null Null when the statement failed.
     */
    public function get_row(string $prepared, $output = 'OBJECT')
    {
        [$sql, $args] = self::decode($prepared);
        $this->statements[] = $sql;
        $this->last_error   = '';
        if ($output !== ARRAY_A
            || $sql !== 'SELECT COUNT(*) AS stored_rows, MAX(option_value) AS stored_value FROM wp_options WHERE option_name = %s'
        ) {
            throw new \LogicException('FakeKeyRowWpdb does not answer: ' . $sql);
        }
        if ($this->failSelects > 0) {
            --$this->failSelects;
            ++$this->failedSelects;
            $this->last_error = 'Lock wait timeout exceeded; try restarting transaction';

            return null;
        }

        $name = (string) $args[0];
        if (!array_key_exists($name, $this->rows)) {
            return ['stored_rows' => '0', 'stored_value' => null];
        }

        return ['stored_rows' => '1', 'stored_value' => (string) $this->rows[$name]];
    }

    /**
     * Rows an INSERT created so far for one option.
     *
     * @param string $name Option name.
     * @return int
     */
    public function insertedFor(string $name): int
    {
        return $this->insertedByName[$name] ?? 0;
    }

    /**
     * Set whether errors are suppressed, as core does.
     *
     * @param bool $suppress New state.
     * @return bool The previous state.
     */
    public function suppress_errors($suppress = true): bool
    {
        $previous             = $this->suppressErrors;
        $this->suppressErrors = (bool) $suppress;

        return $previous;
    }

    /**
     * Run the INSERT that creates a row, or the UPDATE that fills an empty one.
     *
     * @param string $prepared Output of prepare().
     * @return int|false Rows written, or false when the statement failed.
     */
    public function query(string $prepared)
    {
        [$sql, $args] = self::decode($prepared);
        $this->statements[] = $sql;
        $this->last_error   = '';

        if ($sql === "INSERT INTO wp_options (option_name, option_value, autoload) VALUES (%s, %s, 'no')") {
            $this->insertSuppressed[] = $this->suppressErrors;
            if ($this->beforeInsert !== null) {
                ($this->beforeInsert)();
            }
            $name = (string) $args[0];
            if (array_key_exists($name, $this->rows)) {
                ++$this->duplicateInserts;
                $this->last_error = "Duplicate entry '" . $name . "' for key 'option_name'";

                return false;
            }
            $this->rows[$name] = (string) $args[1];
            ++$this->inserted;
            $this->insertedByName[$name] = ($this->insertedByName[$name] ?? 0) + 1;

            return 1;
        }

        if ($sql === "UPDATE wp_options SET option_value = %s WHERE option_name = %s AND option_value = ''") {
            $name = (string) $args[1];
            if (!array_key_exists($name, $this->rows) || $this->rows[$name] !== '') {
                return 0;
            }
            $this->rows[$name] = (string) $args[0];

            return 1;
        }

        throw new \LogicException('FakeKeyRowWpdb does not answer: ' . $sql);
    }

    /**
     * @param string $prepared Output of prepare().
     * @return array{0:string,1:list<mixed>}
     */
    private static function decode(string $prepared): array
    {
        $decoded = json_decode($prepared, true);
        if (!is_array($decoded) || !is_string($decoded['sql'] ?? null) || !is_array($decoded['args'] ?? null)) {
            throw new \LogicException('Statement was not prepared: ' . $prepared);
        }

        return [$decoded['sql'], array_values($decoded['args'])];
    }
}
