<?php
/**
 * A $wpdb double for tests that keep their options in get_option() /
 * update_option() stubs: it answers the keystore's statements for a key row
 * through those stubs, so a key the test stores through the keystore lands
 * where the test's other option reads find it.
 *
 * It answers exactly the statements Keystore issues for a key row and throws
 * on anything else: the aggregate SELECT that reads it, the plain INSERT that
 * creates it (which fails as a duplicate key does when the option has a
 * value), and the UPDATE that fills it only while it is empty.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

/**
 * Emulates the wpdb surface Keystore touches for a key row, over the option stubs.
 */
final class OptionsBackedKeyRowWpdb
{
    /** Options table name, as core exposes it. */
    public string $options = 'wp_options';

    /** Error text of the last statement, '' when it succeeded, as core keeps it. */
    public string $last_error = '';

    /** Whether errors are suppressed, as suppress_errors() sets it. */
    public bool $suppressErrors = false;

    /**
     * Run $fn with this double as $wpdb, then put the previous $wpdb back.
     *
     * @template T
     * @param callable(): T $fn Work that stores or reads a key row.
     * @return T
     */
    public static function around(callable $fn)
    {
        $previous        = $GLOBALS['wpdb'] ?? null;
        $GLOBALS['wpdb'] = new self();
        try {
            return $fn();
        } finally {
            $GLOBALS['wpdb'] = $previous;
        }
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
     * Run the aggregate SELECT of one key row.
     *
     * @param string $prepared Output of prepare().
     * @param string $output   Must be ARRAY_A.
     * @return array<string,string|null>
     */
    public function get_row(string $prepared, $output = 'OBJECT'): array
    {
        [$sql, $args] = self::decode($prepared);
        $this->last_error = '';
        if ($output !== ARRAY_A
            || $sql !== 'SELECT COUNT(*) AS stored_rows, MAX(option_value) AS stored_value FROM wp_options WHERE option_name = %s'
        ) {
            throw new \LogicException('OptionsBackedKeyRowWpdb does not answer: ' . $sql);
        }
        $value = get_option((string) $args[0], null);
        if ($value === null || $value === false) {
            return ['stored_rows' => '0', 'stored_value' => null];
        }

        return ['stored_rows' => '1', 'stored_value' => is_string($value) ? $value : ''];
    }

    /**
     * Run the INSERT that creates a key row, or the UPDATE that fills an empty one.
     *
     * @param string $prepared Output of prepare().
     * @return int|false Rows written, or false when the statement failed.
     */
    public function query(string $prepared)
    {
        [$sql, $args] = self::decode($prepared);
        $this->last_error = '';

        if ($sql === "INSERT INTO wp_options (option_name, option_value, autoload) VALUES (%s, %s, 'no')") {
            $name    = (string) $args[0];
            $current = get_option($name, null);
            if ($current !== null && $current !== false) {
                $this->last_error = "Duplicate entry '" . $name . "' for key 'option_name'";

                return false;
            }
            update_option($name, (string) $args[1], false);

            return 1;
        }

        if ($sql === "UPDATE wp_options SET option_value = %s WHERE option_name = %s AND option_value = ''") {
            $name = (string) $args[1];
            if (get_option($name, null) !== '') {
                return 0;
            }
            update_option($name, (string) $args[0], false);

            return 1;
        }

        throw new \LogicException('OptionsBackedKeyRowWpdb does not answer: ' . $sql);
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
