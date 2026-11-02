<?php
/**
 * In-memory $wpdb double over the options table, for the keystore's own
 * reads and writes of the keystore option rows.
 *
 * For a keystore row it answers the statements Keystore issues and throws
 * on anything else: the aggregate SELECT, the plain INSERT, and conditional
 * UPDATEs that fill empty rows or an exact malformed source descriptor.
 * An INSERT of a row that exists fails as a duplicate key does: query()
 * returns false and last_error is set. Any other statement that names a key
 * row throws, so a change to the keystore's SQL fails loudly.
 *
 * A statement on any other option, such as another component's claim row,
 * gets a neutral answer and changes nothing: an INSERT returns false with no
 * rows affected, any other write affects no rows, and a read finds nothing.
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
 * Emulates the wpdb surface Keystore touches for its option rows.
 */
final class FakeKeyRowWpdb
{
    /** Option names of the keystore's key rows. */
    private const KEY_ROWS = [
        \WPMgr\Agent\Keystore::OPTION_AGE_IDENTITY,
        \WPMgr\Agent\Keystore::OPTION_SITE_KEYPAIR,
        \WPMgr\Agent\Keystore::OPTION_MASTER_KEY_SOURCE,
        \WPMgr\Agent\Keystore::OPTION_DB_MASTER_KEY,
    ];

    /** Options table name, as core exposes it. */
    public string $options = 'wp_options';

    /** Table prefix, as core exposes it. */
    public string $prefix = 'wp_';

    /** Error text of the last statement, '' when it succeeded, as core keeps it. */
    public string $last_error = '';

    /** How many of the next SELECTs fail. */
    public int $failSelects = 0;

    /** @var array<string,int> Pending read failures by option name. */
    public array $failSelectsByName = [];

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

    /** @var list<string> Statements on other options, answered neutrally, in order. */
    public array $neutralStatements = [];

    /** Rows the last write affected, as core keeps it. */
    public int $rows_affected = 0;

    /**
     * Runs before each INSERT.
     *
     * @var (\Closure(string): void)|null
     */
    public ?\Closure $beforeInsert = null;

    /** @var (\Closure(string): void)|null Runs before an exact-value UPDATE. */
    public ?\Closure $beforeConditionalUpdate = null;

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
        if ($sql !== 'SELECT COUNT(*) AS stored_rows, MAX(option_value) AS stored_value FROM wp_options WHERE option_name = %s') {
            $this->answerNeutrally($sql, $args);

            return null;
        }
        if ($output !== ARRAY_A) {
            throw new \LogicException('FakeKeyRowWpdb answers the key-row read with ARRAY_A only: ' . $sql);
        }
        $name = (string) $args[0];
        if (($this->failSelectsByName[$name] ?? 0) > 0) {
            --$this->failSelectsByName[$name];
            ++$this->failedSelects;
            $this->last_error = 'Lock wait timeout exceeded; try restarting transaction';

            return null;
        }
        if ($this->failSelects > 0) {
            --$this->failSelects;
            ++$this->failedSelects;
            $this->last_error = 'Lock wait timeout exceeded; try restarting transaction';

            return null;
        }

        if (!array_key_exists($name, $this->rows)) {
            return ['stored_rows' => '0', 'stored_value' => null];
        }

        $value = $this->rows[$name];

        return ['stored_rows' => '1', 'stored_value' => is_array($value) ? serialize($value) : (string) $value];
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
            $name = (string) $args[0];
            if ($this->beforeInsert !== null) {
                ($this->beforeInsert)($name);
            }
            if (array_key_exists($name, $this->rows)) {
                ++$this->duplicateInserts;
                $this->last_error = "Duplicate entry '" . $name . "' for key 'option_name'";

                return false;
            }
            $this->rows[$name] = self::optionValue($name, (string) $args[1]);
            ++$this->inserted;
            $this->insertedByName[$name] = ($this->insertedByName[$name] ?? 0) + 1;

            return 1;
        }

        if ($sql === "UPDATE wp_options SET option_value = %s WHERE option_name = %s AND option_value = ''") {
            $name = (string) $args[1];
            if (!array_key_exists($name, $this->rows) || $this->rows[$name] !== '') {
                return 0;
            }
            $this->rows[$name] = self::optionValue($name, (string) $args[0]);

            return 1;
        }

        if ($sql === 'UPDATE wp_options SET option_value = %s WHERE option_name = %s AND BINARY option_value = %s') {
            $name = (string) $args[1];
            if ($this->beforeConditionalUpdate !== null) {
                ($this->beforeConditionalUpdate)($name);
            }
            $current = $this->rows[$name] ?? null;
            $current = is_array($current) ? serialize($current) : $current;
            if ($current !== $args[2]) {
                return 0;
            }
            $this->rows[$name] = self::optionValue($name, (string) $args[0]);

            return 1;
        }

        $this->answerNeutrally($sql, $args);
        $this->rows_affected = 0;

        return preg_match('/^\s*INSERT\b/i', $sql) === 1 ? false : 0;
    }

    /**
     * Read one value: only statements on other options are answered, and they
     * find nothing.
     *
     * @param string $prepared Output of prepare().
     * @return null
     */
    public function get_var(string $prepared)
    {
        [$sql, $args] = self::decode($prepared);
        $this->statements[] = $sql;
        $this->last_error   = '';
        $this->answerNeutrally($sql, $args);

        return null;
    }

    /**
     * Record a statement on another option, or throw when it names a key row:
     * the keystore's own statements for key rows are answered above, and any
     * other statement on them is a change to fail loudly on.
     *
     * @param string      $sql  Statement with placeholders.
     * @param list<mixed> $args Bound values.
     * @return void
     */
    private function answerNeutrally(string $sql, array $args): void
    {
        foreach (self::KEY_ROWS as $keyRow) {
            if (str_contains($sql, $keyRow) || in_array($keyRow, $args, true)) {
                throw new \LogicException('FakeKeyRowWpdb does not answer this statement on a key row: ' . $sql);
            }
        }
        $this->neutralStatements[] = $sql;
    }

    /**
     * @param string $name Option name.
     * @param string $value Stored value.
     * @return mixed
     */
    private static function optionValue(string $name, string $value)
    {
        return $name === \WPMgr\Agent\Keystore::OPTION_MASTER_KEY_SOURCE
            ? unserialize($value, ['allowed_classes' => false]) : $value;
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
