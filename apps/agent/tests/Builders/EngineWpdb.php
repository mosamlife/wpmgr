<?php
/**
 * FakeBuilderWpdb plus the statements the engine's own bookkeeping sends:
 * the ledger's claim rows (INSERT IGNORE, the stale takeover UPDATE, the
 * owner's DELETE and the raw read), held in $claims, and the signed-token
 * replay record and its prune, accepted as PageCreateWriteTest has them.
 * Everything else reaches FakeBuilderWpdb, which throws on a statement it
 * does not know.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

/**
 * The wpdb surface a page-edit call touches, through the command or not.
 */
final class EngineWpdb
{
    /** @var array<string,string> Claim rows by option name. */
    public array $claims = [];

    /**
     * @param FakeBuilderWpdb $rows Posts, postmeta and options rows.
     */
    public function __construct(public readonly FakeBuilderWpdb $rows)
    {
    }

    /**
     * @param string $name Property.
     * @return mixed
     */
    public function __get(string $name)
    {
        return $this->rows->$name;
    }

    /**
     * @param string $name Property.
     */
    public function __isset(string $name): bool
    {
        return isset($this->rows->$name);
    }

    /**
     * @param string       $name Method.
     * @param list<mixed>  $args Arguments.
     * @return mixed
     */
    public function __call(string $name, array $args)
    {
        return $this->rows->$name(...$args);
    }

    /**
     * @param mixed ...$args Arguments.
     */
    public function prepare(string $query, ...$args): string
    {
        return $this->rows->prepare($query, ...$args);
    }

    /**
     * @return string|null
     */
    public function get_var(string $prepared): ?string
    {
        [$sql, $args] = self::parse($prepared);
        if (str_starts_with($sql, 'SELECT option_value FROM wp_options WHERE option_name = %s')) {
            return $this->claims[(string) $args[0]] ?? null;
        }
        if (str_starts_with($sql, 'SELECT 1 FROM wp_wpmgr_')) {
            return null;
        }

        return $this->rows->get_var($prepared);
    }

    /**
     * @return int|bool
     */
    public function query(string $query)
    {
        [$sql, $args] = self::parse($query);
        if (str_starts_with($sql, 'INSERT IGNORE INTO wp_options')) {
            if (array_key_exists((string) $args[0], $this->claims)) {
                return 0;
            }
            $this->claims[(string) $args[0]] = (string) $args[1];

            return 1;
        }
        if (str_starts_with($sql, 'UPDATE wp_options SET option_value = %s WHERE option_name = %s AND option_value = %s')) {
            return $this->swap((string) $args[1], (string) $args[2], (string) $args[0]);
        }
        if (str_starts_with($sql, 'DELETE FROM wp_options WHERE option_name = %s AND option_value = %s')) {
            return $this->swap((string) $args[0], (string) $args[1], null);
        }
        if (str_starts_with($sql, 'DELETE FROM wp_wpmgr_')) {
            return 0;
        }

        return $this->rows->query($query);
    }

    /**
     * @param array<string,mixed> $data   Row.
     * @param mixed               $format Formats.
     * @return int|false
     */
    public function insert(string $table, array $data, $format = null)
    {
        if (!in_array($table, [$this->rows->posts, $this->rows->postmeta, $this->rows->options], true)) {
            return 1;
        }

        return $this->rows->insert($table, $data, $format);
    }

    /**
     * Replace (or delete, when $new is null) a claim row holding $old.
     */
    private function swap(string $name, string $old, ?string $new): int
    {
        if (($this->claims[$name] ?? null) !== $old) {
            return 0;
        }
        if ($new === null) {
            unset($this->claims[$name]);
        } else {
            $this->claims[$name] = $new;
        }

        return 1;
    }

    /**
     * The statement and its arguments, from prepare()'s output or raw SQL.
     *
     * @return array{0:string,1:list<mixed>}
     */
    private static function parse(string $q): array
    {
        $d = json_decode($q, true);
        if (!is_array($d) || !isset($d['sql'])) {
            return [trim($q), []];
        }

        return [(string) preg_replace('/\s+/', ' ', trim((string) $d['sql'])), array_values((array) ($d['args'] ?? []))];
    }
}
