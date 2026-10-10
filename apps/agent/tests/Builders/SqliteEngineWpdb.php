<?php
/**
 * EngineWpdb as the SQLite database integration's driver: an instance of its
 * database class, WP_SQLite_DB (this class under that name, through
 * asDriver()), whose information_schema has no ENGINES table, so the
 * catalogue read of the storage engines fails as it does there: no rows, and
 * last_error set. Every other statement is EngineWpdb's.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

/**
 * The wpdb surface a page-edit call touches, on SQLite.
 */
final class SqliteEngineWpdb extends EngineWpdb
{
    /** The driver's database class. */
    public const DRIVER_CLASS = 'WP_SQLite_DB';

    /** Statements naming information_schema this handle was sent. */
    public int $catalogueReads = 0;

    /**
     * A handle over $rows that is an instance of DRIVER_CLASS: the name is
     * given to this class when no class holds it yet.
     *
     * @param FakeBuilderWpdb $rows Posts, postmeta and options rows.
     */
    public static function asDriver(FakeBuilderWpdb $rows): self
    {
        if (!class_exists(self::DRIVER_CLASS, false)) {
            class_alias(self::class, self::DRIVER_CLASS);
        }

        return new self($rows);
    }

    /**
     * @param string $prepared Output of prepare().
     * @param string $output   ARRAY_A or OBJECT.
     * @return list<array<string,string|null>|object>|null
     */
    public function get_results(string $prepared, string $output = 'OBJECT'): ?array
    {
        if (str_contains($prepared, 'information_schema')) {
            ++$this->catalogueReads;
            $this->rows->last_error = 'no such table: information_schema.ENGINES';

            return null;
        }

        return $this->rows->get_results($prepared, $output);
    }
}
