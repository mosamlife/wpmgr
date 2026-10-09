<?php
/**
 * In-memory $wpdb double for the builder document reads and writes: posts
 * rows by ID, the postmeta rows of a post, and an options table.
 *
 * It answers only the statement shapes the builder classes send and throws
 * on any other, so a changed query is loud. It emulates the parts of MySQL
 * those statements depend on:
 *
 * - a posts read returns exactly the columns the query names, as text (the
 *   integer columns as ints under $nativeInts); a column the fields leave out
 *   has the table's default;
 * - postmeta rows come back in storage order unless the query asks for
 *   meta_id order, and storage order need not be meta_id order;
 * - meta_key and option_name match under the default collation
 *   (case-insensitive, trailing spaces ignored), and option_name is unique
 *   under it;
 * - an insert takes the next AUTO_INCREMENT id, which is never reused, not
 *   even after a ROLLBACK;
 * - START TRANSACTION, COMMIT and ROLLBACK cover the posts, postmeta and
 *   options rows, and a FOR UPDATE read outside a transaction throws;
 * - every statement resets last_error, and a failing one sets it.
 *
 * add_option() and delete_option() as core runs them against this table are
 * addOptionLikeCore() and deleteOptionLikeCore(), for Brain Monkey aliases.
 * metaRowsOf(), postRow() and optionRows() dump the stored rows for
 * byte-equality assertions.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use WPMgr\Agent\Tests\FakeOptionsTableWpdb;

/**
 * The wpdb surface the builder document reads and writes touch.
 */
final class FakeBuilderWpdb
{
    /** The posts columns a statement may name. */
    private const POST_COLUMNS = ['post_type', 'post_status', 'post_title', 'post_content', 'post_excerpt', 'post_name', 'post_password', 'post_modified_gmt', 'post_parent', 'menu_order', 'post_modified', 'post_content_filtered', 'post_author', 'post_date', 'post_date_gmt'];

    /** The integer columns among them, which a driver may return as ints. */
    private const INT_COLUMNS = ['post_parent', 'menu_order', 'post_author'];

    /** Defaults of the columns addPost() fills in when the fields leave them out. */
    private const POST_DEFAULTS = [
        'post_excerpt'          => '',
        'post_name'             => '',
        'post_password'         => '',
        'post_parent'           => '0',
        'menu_order'            => '0',
        'post_modified'         => '0000-00-00 00:00:00',
        'post_content_filtered' => '',
        'post_author'           => '0',
        'post_date'             => '0000-00-00 00:00:00',
        'post_date_gmt'         => '0000-00-00 00:00:00',
    ];

    public string $prefix   = 'wp_';
    public string $posts    = 'wp_posts';
    public string $postmeta = 'wp_postmeta';
    public string $options  = 'wp_options';
    public string $last_error = '';

    /** Id given to the last inserted row, as wpdb exposes it. */
    public int $insert_id = 0;

    /** @var list<array{sql:string,args:list<mixed>}> Statements run, in order. */
    public array $queries = [];

    /** @var list<string> Every LIKE pattern bound into a statement. */
    public array $likePatterns = [];

    /** @var string Table whose next statement fails ("posts", "postmeta" or "options"), or "". */
    public string $failOn = '';

    /** @var string The next statement whose SQL starts with this fails, or "". */
    public string $failOnStatement = '';

    /** @var int How many statements matching $failOnStatement succeed before the one that fails. */
    public int $failOnStatementAfter = 0;

    /**
     * @var bool A driver that returns native types: the integer columns of a
     *           posts read, and the ids of a revisions read, come back as
     *           ints. Off, every column is text, as mysqli answers.
     */
    public bool $nativeInts = false;

    /**
     * @var (\Closure(string,string):string)|null What addOptionLikeCore()
     *      stores for (name, value-as-core-serializes-it): a filter or a
     *      connection that changes the bytes on the way in. Null stores them
     *      as they are.
     */
    public ?\Closure $optionWriteFilter = null;

    /** @var array<int,array<string,string|int|null>> Posts rows by ID. */
    private array $postRows = [];

    /** @var list<array{meta_id:int,post_id:int,meta_key:string,meta_value:string|null}> Storage order. */
    private array $metaRows = [];

    /** @var list<array{option_id:int,option_name:string,option_value:string|null,autoload:string}> Storage order. */
    private array $optionRows = [];

    /** Highest meta_id ever used: AUTO_INCREMENT never reuses one. */
    private int $lastMetaId = 0;

    /** Highest option_id ever used. */
    private int $lastOptionId = 0;

    /** @var array{0:array<int,array<string,string|int|null>>,1:list<array{meta_id:int,post_id:int,meta_key:string,meta_value:string|null}>,2:list<array{option_id:int,option_name:string,option_value:string|null,autoload:string}>}|null Rows as START TRANSACTION found them. */
    private ?array $transaction = null;

    /**
     * Stores a posts row. A column the fields leave out has the table's
     * default: '' for the excerpt, slug, password and filtered content, 0 for
     * the placement and the author, the zero date for the dates. A value may
     * be given as text or as an int; a read answers text unless $nativeInts
     * is set.
     *
     * @param int                          $id     Post ID.
     * @param array<string,string|int|null> $fields Column => stored value.
     */
    public function addPost(int $id, array $fields): void
    {
        $this->postRows[$id] = $fields + self::POST_DEFAULTS;
    }

    /**
     * Appends a postmeta row in storage order, which need not be meta_id order.
     */
    public function addMeta(int $metaId, int $postId, string $key, ?string $value): void
    {
        $this->metaRows[] = ['meta_id' => $metaId, 'post_id' => $postId, 'meta_key' => $key, 'meta_value' => $value];
        $this->lastMetaId = max($this->lastMetaId, $metaId);
    }

    /**
     * Appends an options row, as something other than the code under test
     * stored it.
     *
     * @throws \LogicException When the name collides with a stored one under the collation.
     */
    public function addOption(string $name, ?string $value, string $autoload = 'off'): void
    {
        if ($this->optionIndex($name) !== null) {
            throw new \LogicException('addOption: option_name is unique under the collation: ' . $name);
        }
        $this->optionRows[] = ['option_id' => ++$this->lastOptionId, 'option_name' => $name, 'option_value' => $value, 'autoload' => $autoload];
    }

    /**
     * add_option() as core runs it here: a name already stored (under the
     * collation) refuses; an array or object is serialized, and so is a
     * string that already is serialized data; any other string is stored as
     * it is, through $optionWriteFilter.
     *
     * @param string $name     Option name.
     * @param mixed  $value    Value.
     * @param mixed  $unused   Deprecated argument.
     * @param mixed  $autoload Autoload flag.
     * @return bool
     */
    public function addOptionLikeCore(string $name, mixed $value = '', mixed $unused = '', mixed $autoload = null): bool
    {
        $name = trim($name);
        if ($name === '' || $this->optionIndex($name) !== null) {
            return false;
        }
        if (is_array($value) || is_object($value)) {
            $stored = serialize($value);
        } elseif (is_string($value) && self::isSerializedLikeCore($value)) {
            $stored = serialize($value);
        } else {
            $stored = (string) $value;
        }
        if ($this->optionWriteFilter !== null) {
            $stored = ($this->optionWriteFilter)($name, $stored);
        }
        $flag = $autoload === false || $autoload === 'no' || $autoload === 'off' ? 'off' : 'auto';

        return $this->insert($this->options, ['option_name' => $name, 'option_value' => $stored, 'autoload' => $flag]) === 1;
    }

    /**
     * delete_option() as core runs it here: the row whose name matches under
     * the collation.
     */
    public function deleteOptionLikeCore(string $name): bool
    {
        $deleted = $this->delete($this->options, ['option_name' => trim($name)]);

        return is_int($deleted) && $deleted > 0;
    }

    /**
     * The stored postmeta rows of a post in meta_id order, exactly as stored.
     *
     * @return list<array{meta_id:int,meta_key:string,meta_value:string|null}>
     */
    public function metaRowsOf(int $postId): array
    {
        $rows = array_values(array_filter($this->metaRows, static fn (array $r): bool => $r['post_id'] === $postId));
        usort($rows, static fn (array $a, array $b): int => $a['meta_id'] <=> $b['meta_id']);

        return array_map(static fn (array $r): array => ['meta_id' => $r['meta_id'], 'meta_key' => $r['meta_key'], 'meta_value' => $r['meta_value']], $rows);
    }

    /**
     * The stored posts row, or null.
     *
     * @return array<string,string|int|null>|null
     */
    public function postRow(int $id): ?array
    {
        return $this->postRows[$id] ?? null;
    }

    /**
     * The options rows, in storage order.
     *
     * @return list<array{option_id:int,option_name:string,option_value:string|null,autoload:string}>
     */
    public function optionRows(): array
    {
        return $this->optionRows;
    }

    /**
     * Replaces the stored value of an options row, as a direct write by
     * something else would.
     *
     * @throws \LogicException When no row has the name.
     */
    public function setOptionValue(string $name, ?string $value): void
    {
        $i = $this->optionIndex($name);
        if ($i === null) {
            throw new \LogicException('setOptionValue: no such option: ' . $name);
        }
        $this->optionRows[$i]['option_value'] = $value;
    }

    /**
     * Same escaping as core's wpdb::esc_like().
     */
    public function esc_like(string $text): string
    {
        return addcslashes($text, '_%\\');
    }

    /**
     * Carries the query and its arguments through; the readers parse them.
     * Takes the arguments either spread or as one array, as wpdb does, and
     * refuses a placeholder count that differs from the argument count.
     *
     * @param string $query Query with %s, %d and %i placeholders.
     * @param mixed  ...$args Arguments.
     * @return string
     */
    public function prepare(string $query, ...$args): string
    {
        if (count($args) === 1 && is_array($args[0])) {
            $args = array_values($args[0]);
        }
        $placeholders = preg_match_all('/%[sdfi]/', $query);
        if ($placeholders !== count($args)) {
            throw new \LogicException('prepare: ' . $placeholders . ' placeholders for ' . count($args) . ' arguments');
        }

        return json_encode(['sql' => $query, 'args' => $args], JSON_THROW_ON_ERROR);
    }

    /**
     * A posts row by ID, or an options row by name.
     *
     * @param string $prepared Output of prepare().
     * @param string $output   ARRAY_A or OBJECT.
     * @param int    $y        Row offset (unused).
     * @return array<string,string|int|null>|object|null
     */
    public function get_row(string $prepared, string $output = 'OBJECT', int $y = 0)
    {
        [$sql, $args] = $this->begin($prepared);
        if ($sql === 'SELECT option_name, option_value FROM %i WHERE option_name = %s') {
            $this->assertTable($args[0], $this->options, 'get_row');
            if ($this->fails('options', $sql)) {
                return null;
            }
            $i = $this->optionIndex((string) $args[1]);
            if ($i === null) {
                return null;
            }
            $row = ['option_name' => $this->optionRows[$i]['option_name'], 'option_value' => $this->optionRows[$i]['option_value']];

            return $output === ARRAY_A ? $row : (object) $row;
        }
        if (preg_match('/^SELECT ([a-z_]+(?:, [a-z_]+)*) FROM %i WHERE ID = %d( FOR UPDATE)?$/', $sql, $m) !== 1) {
            throw new \LogicException('get_row: unexpected query: ' . $sql);
        }
        $columns = explode(', ', $m[1]);
        foreach ($columns as $column) {
            if (!in_array($column, self::POST_COLUMNS, true)) {
                throw new \LogicException('get_row: not a posts column: ' . $column);
            }
        }
        $this->assertTable($args[0], $this->posts, 'get_row');
        if (($m[2] ?? '') !== '') {
            $this->assertInTransaction('get_row FOR UPDATE');
        }
        if ($this->fails('posts', $sql)) {
            return null;
        }
        $stored = $this->postRows[(int) $args[1]] ?? null;
        if ($stored === null) {
            return null;
        }
        // The columns the query names, in the order it names them, as text
        // (ints for the integer columns under $nativeInts); a column the row
        // never had is NULL.
        $row = [];
        foreach ($columns as $column) {
            $value = $stored[$column] ?? null;
            if ($value !== null) {
                $value = $this->nativeInts && in_array($column, self::INT_COLUMNS, true) ? (int) $value : (string) $value;
            }
            $row[$column] = $value;
        }

        return $output === ARRAY_A ? $row : (object) $row;
    }

    /**
     * Postmeta rows of a post (some keys or every key), or the heads of the
     * options rows whose name is LIKE a pattern.
     *
     * @param string $prepared Output of prepare().
     * @param string $output   ARRAY_A or OBJECT.
     * @return list<array<string,string|null>|object>|null
     */
    public function get_results(string $prepared, string $output = 'OBJECT'): ?array
    {
        [$sql, $args] = $this->begin($prepared);
        if ($sql === 'SELECT option_id, option_name, SUBSTRING(option_value, 1, %d) AS head FROM %i WHERE option_name LIKE %s ORDER BY option_id ASC LIMIT %d') {
            return $this->optionHeads($sql, $args, $output);
        }

        $keys   = null;
        $withId = false;
        if (preg_match('/^SELECT meta_id, meta_key, meta_value FROM %i WHERE post_id = %d ORDER BY meta_id ASC( FOR UPDATE)?$/', $sql, $m) === 1) {
            $withId  = true;
            $ordered = true;
            if (($m[1] ?? '') !== '') {
                $this->assertInTransaction('get_results FOR UPDATE');
            }
        } elseif (preg_match('/^SELECT meta_key, meta_value FROM %i WHERE post_id = %d AND meta_key IN \((%s(?:, %s)*)\)( ORDER BY meta_id ASC)?$/', $sql, $m) === 1) {
            $keys    = array_map(static fn ($k): string => self::collate((string) $k), array_slice($args, 2));
            $ordered = ($m[2] ?? '') !== '';
        } elseif (preg_match('/^SELECT meta_key, meta_value FROM %i WHERE post_id = %d( ORDER BY meta_id ASC)?$/', $sql, $m) === 1) {
            $ordered = ($m[1] ?? '') !== '';
        } else {
            throw new \LogicException('get_results: unexpected query: ' . $sql);
        }
        $this->assertTable($args[0], $this->postmeta, 'get_results');
        if ($this->fails('postmeta', $sql)) {
            return null;
        }
        $postId = (int) $args[1];
        $rows   = array_values(array_filter(
            $this->metaRows,
            static fn (array $r): bool => $r['post_id'] === $postId && ($keys === null || in_array(self::collate($r['meta_key']), $keys, true))
        ));
        if ($ordered) {
            usort($rows, static fn (array $a, array $b): int => $a['meta_id'] <=> $b['meta_id']);
        }
        $out = [];
        foreach ($rows as $r) {
            $row = ['meta_key' => $r['meta_key'], 'meta_value' => $r['meta_value']];
            if ($withId) {
                $row = ['meta_id' => $this->nativeInts ? $r['meta_id'] : (string) $r['meta_id']] + $row;
            }
            $out[] = $output === ARRAY_A ? $row : (object) $row;
        }

        return $out;
    }

    /**
     * The ids of a post's revisions, in id order.
     *
     * @param string $prepared Output of prepare().
     * @return list<string|int>
     */
    public function get_col(string $prepared): array
    {
        [$sql, $args] = $this->begin($prepared);
        if ($sql !== 'SELECT ID FROM %i WHERE post_parent = %d AND post_type = %s ORDER BY ID ASC') {
            throw new \LogicException('get_col: unexpected query: ' . $sql);
        }
        $this->assertTable($args[0], $this->posts, 'get_col');
        if ($this->fails('posts', $sql)) {
            return [];
        }
        $ids = [];
        foreach ($this->postRows as $id => $row) {
            if ((int) ($row['post_parent'] ?? 0) === (int) $args[1] && self::collate((string) ($row['post_type'] ?? '')) === self::collate((string) $args[2])) {
                $ids[] = $id;
            }
        }
        sort($ids);

        return array_map(fn (int $id) => $this->nativeInts ? $id : (string) $id, $ids);
    }

    /**
     * The stored length of a post's meta values, without its edit lock rows.
     *
     * @param string $prepared Output of prepare().
     * @return string|null
     */
    public function get_var(string $prepared): ?string
    {
        [$sql, $args] = $this->begin($prepared);
        if ($sql !== 'SELECT COALESCE(SUM(LENGTH(meta_value)), 0) FROM %i WHERE post_id = %d AND meta_key <> %s') {
            throw new \LogicException('get_var: unexpected query: ' . $sql);
        }
        $this->assertTable($args[0], $this->postmeta, 'get_var');
        if ($this->fails('postmeta', $sql)) {
            return null;
        }
        $sum = 0;
        foreach ($this->metaRows as $r) {
            if ($r['post_id'] === (int) $args[1] && self::collate($r['meta_key']) !== self::collate((string) $args[2]) && $r['meta_value'] !== null) {
                $sum += strlen($r['meta_value']);
            }
        }

        return (string) $sum;
    }

    /**
     * INSERT one postmeta or options row. The id is the next AUTO_INCREMENT
     * value; an option_name already stored under the collation is a
     * duplicate key.
     *
     * @param string              $table  Table name.
     * @param array<string,mixed> $data   Column => value; null is SQL NULL.
     * @param mixed               $format Formats (unused).
     * @return int|false Rows inserted, or false.
     */
    public function insert(string $table, array $data, $format = null)
    {
        $this->record('INSERT INTO ' . $table, [$data]);
        if ($table === $this->postmeta) {
            self::assertColumns($data, ['post_id', 'meta_key', 'meta_value'], 'insert');
            if ($this->fails('postmeta', 'INSERT INTO ' . $table)) {
                return false;
            }
            $id                = ++$this->lastMetaId;
            $this->metaRows[]  = ['meta_id' => $id, 'post_id' => (int) $data['post_id'], 'meta_key' => (string) $data['meta_key'], 'meta_value' => $data['meta_value'] === null ? null : (string) $data['meta_value']];
            $this->insert_id   = $id;

            return 1;
        }
        if ($table === $this->options) {
            self::assertColumns($data, ['option_name', 'option_value', 'autoload'], 'insert');
            if ($this->fails('options', 'INSERT INTO ' . $table)) {
                return false;
            }
            if ($this->optionIndex((string) $data['option_name']) !== null) {
                $this->last_error = "Duplicate entry for key 'option_name'";

                return false;
            }
            $id                 = ++$this->lastOptionId;
            $this->optionRows[] = ['option_id' => $id, 'option_name' => (string) $data['option_name'], 'option_value' => $data['option_value'] === null ? null : (string) $data['option_value'], 'autoload' => (string) $data['autoload']];
            $this->insert_id    = $id;

            return 1;
        }

        throw new \LogicException('insert: unexpected table: ' . $table);
    }

    /**
     * UPDATE posts columns by ID. Answers the rows changed, as MySQL does.
     *
     * @param string              $table       Table name.
     * @param array<string,mixed> $data        Column => value.
     * @param array<string,mixed> $where       Exactly ['ID' => id].
     * @param mixed               $format      Formats (unused).
     * @param mixed               $whereFormat Formats (unused).
     * @return int|false
     */
    public function update(string $table, array $data, array $where, $format = null, $whereFormat = null)
    {
        $this->record('UPDATE ' . $table, [$data, $where]);
        if ($table !== $this->posts || array_keys($where) !== ['ID'] || $data === []) {
            throw new \LogicException('update: posts columns by ID only');
        }
        foreach (array_keys($data) as $column) {
            if (!in_array($column, self::POST_COLUMNS, true)) {
                throw new \LogicException('update: not a posts column: ' . $column);
            }
        }
        if ($this->fails('posts', 'UPDATE ' . $table)) {
            return false;
        }
        $id = (int) $where['ID'];
        if (!isset($this->postRows[$id])) {
            return 0;
        }
        $before = $this->postRows[$id];
        foreach ($data as $column => $value) {
            $this->postRows[$id][$column] = $value === null ? null : (string) $value;
        }

        return $this->postRows[$id] === $before ? 0 : 1;
    }

    /**
     * DELETE postmeta rows by meta_id or by post and key, or an options row
     * by name; keys and names match under the collation.
     *
     * @param string              $table       Table name.
     * @param array<string,mixed> $where       Column => value.
     * @param mixed               $whereFormat Formats (unused).
     * @return int|false Rows deleted, or false.
     */
    public function delete(string $table, array $where, $whereFormat = null)
    {
        $this->record('DELETE FROM ' . $table, [$where]);
        if ($table === $this->postmeta) {
            $columns = array_keys($where);
            sort($columns);
            if ($columns === ['meta_id']) {
                $match = static fn (array $r): bool => $r['meta_id'] === (int) $where['meta_id'];
            } elseif ($columns === ['meta_id', 'post_id']) {
                $match = static fn (array $r): bool => $r['meta_id'] === (int) $where['meta_id'] && $r['post_id'] === (int) $where['post_id'];
            } elseif ($columns === ['meta_key', 'post_id']) {
                $match = static fn (array $r): bool => $r['post_id'] === (int) $where['post_id'] && self::collate($r['meta_key']) === self::collate((string) $where['meta_key']);
            } else {
                throw new \LogicException('delete: postmeta by meta_id, by meta_id and post_id, or by post_id and meta_key, only');
            }
            if ($this->fails('postmeta', 'DELETE FROM ' . $table)) {
                return false;
            }
            $kept           = array_values(array_filter($this->metaRows, static fn (array $r): bool => !$match($r)));
            $deleted        = count($this->metaRows) - count($kept);
            $this->metaRows = $kept;

            return $deleted;
        }
        if ($table === $this->options) {
            if (array_keys($where) !== ['option_name']) {
                throw new \LogicException('delete: options by option_name only');
            }
            if ($this->fails('options', 'DELETE FROM ' . $table)) {
                return false;
            }
            $i = $this->optionIndex((string) $where['option_name']);
            if ($i === null) {
                return 0;
            }
            array_splice($this->optionRows, $i, 1);

            return 1;
        }

        throw new \LogicException('delete: unexpected table: ' . $table);
    }

    /**
     * START TRANSACTION, COMMIT and ROLLBACK, and a prepared
     * DELETE FROM postmeta WHERE post_id = %d AND meta_key = %s.
     *
     * @param string $query Raw SQL, or the output of prepare().
     * @return int|bool
     */
    public function query(string $query)
    {
        $decoded = json_decode($query, true);
        if (is_array($decoded) && isset($decoded['sql'], $decoded['args'])) {
            [$sql, $args] = $this->begin($query);
            if ($sql !== 'DELETE FROM %i WHERE post_id = %d AND meta_key = %s') {
                throw new \LogicException('query: unexpected statement: ' . $sql);
            }
            $this->assertTable($args[0], $this->postmeta, 'query');
            if ($this->fails('postmeta', $sql)) {
                return false;
            }
            $before         = count($this->metaRows);
            $this->metaRows = array_values(array_filter(
                $this->metaRows,
                static fn (array $r): bool => !($r['post_id'] === (int) $args[1] && self::collate($r['meta_key']) === self::collate((string) $args[2]))
            ));

            return $before - count($this->metaRows);
        }

        $sql = strtoupper(trim($query));
        $this->record($sql, []);
        switch ($sql) {
            case 'START TRANSACTION':
                // MySQL commits an open transaction when another starts.
                $this->transaction = [$this->postRows, $this->metaRows, $this->optionRows];

                return true;
            case 'COMMIT':
                $this->transaction = null;

                return true;
            case 'ROLLBACK':
                if ($this->transaction !== null) {
                    [$this->postRows, $this->metaRows, $this->optionRows] = $this->transaction;
                    $this->transaction                                   = null;
                }

                return true;
        }

        throw new \LogicException('query: unexpected statement: ' . $query);
    }

    /**
     * The sweep read: option_id, option_name and the first characters of
     * option_value of the rows whose name is LIKE the pattern, by option_id,
     * at most LIMIT rows.
     *
     * @param string      $sql    Statement.
     * @param list<mixed> $args   Bound values.
     * @param string      $output ARRAY_A or OBJECT.
     * @return list<array<string,string|null>|object>|null
     */
    private function optionHeads(string $sql, array $args, string $output): ?array
    {
        [$length, $table, $pattern, $limit] = $args;
        $this->assertTable($table, $this->options, 'get_results');
        $this->likePatterns[] = (string) $pattern;
        if ($this->fails('options', $sql)) {
            return null;
        }
        $rows = array_values(array_filter($this->optionRows, static fn (array $r): bool => FakeOptionsTableWpdb::likeMatches($r['option_name'], (string) $pattern)));
        usort($rows, static fn (array $a, array $b): int => $a['option_id'] <=> $b['option_id']);
        $out = [];
        foreach (array_slice($rows, 0, (int) $limit) as $r) {
            $row   = ['option_id' => (string) $r['option_id'], 'option_name' => $r['option_name'], 'head' => $r['option_value'] === null ? null : substr($r['option_value'], 0, (int) $length)];
            $out[] = $output === ARRAY_A ? $row : (object) $row;
        }

        return $out;
    }

    /** Whether a transaction is open. */
    public function inTransaction(): bool
    {
        return $this->transaction !== null;
    }

    /**
     * A locking read means nothing outside a transaction.
     *
     * @throws \LogicException When no transaction is open.
     */
    private function assertInTransaction(string $method): void
    {
        if ($this->transaction === null) {
            throw new \LogicException($method . ': a locking read outside a transaction');
        }
    }

    /**
     * Records the query and resets the error, as every wpdb query does.
     *
     * @return array{0:string,1:list<mixed>}
     */
    private function begin(string $prepared): array
    {
        $q    = json_decode($prepared, true, 512, JSON_THROW_ON_ERROR);
        $sql  = (string) preg_replace('/\s+/', ' ', trim((string) $q['sql']));
        $args = array_values((array) $q['args']);
        $this->record($sql, $args);

        return [$sql, $args];
    }

    /**
     * @param list<mixed> $args Bound values.
     */
    private function record(string $sql, array $args): void
    {
        $this->last_error = '';
        $this->queries[]  = ['sql' => $sql, 'args' => $args];
    }

    /** Whether this statement on $table fails, as one set by $failOn or $failOnStatement. */
    private function fails(string $table, string $sql): bool
    {
        if ($this->failOn === $table) {
            $this->failOn = '';
        } elseif ($this->failOnStatement !== '' && str_starts_with($sql, $this->failOnStatement)) {
            if ($this->failOnStatementAfter > 0) {
                --$this->failOnStatementAfter;

                return false;
            }
            $this->failOnStatement = '';
        } else {
            return false;
        }
        $this->last_error = 'Lost connection to MySQL server during query';

        return true;
    }

    private function assertTable(mixed $given, string $table, string $method): void
    {
        if ($given !== $table) {
            throw new \LogicException($method . ': not the ' . $table . ' table');
        }
    }

    /**
     * @param array<string,mixed> $data    Row.
     * @param list<string>        $columns The columns it must have, exactly.
     */
    private static function assertColumns(array $data, array $columns, string $method): void
    {
        $given = array_keys($data);
        sort($given);
        sort($columns);
        if ($given !== $columns) {
            throw new \LogicException($method . ': columns ' . implode(', ', $given) . ' are not ' . implode(', ', $columns));
        }
    }

    /** The storage index of the options row whose name matches under the collation, or null. */
    private function optionIndex(string $name): ?int
    {
        foreach ($this->optionRows as $i => $row) {
            if (self::collate($row['option_name']) === self::collate($name)) {
                return $i;
            }
        }

        return null;
    }

    /**
     * Core's is_serialized() for a string, strict mode: whether add_option()
     * would serialize it again.
     */
    private static function isSerializedLikeCore(string $data): bool
    {
        $data = trim($data);
        if ($data === 'N;') {
            return true;
        }
        if (strlen($data) < 4 || $data[1] !== ':') {
            return false;
        }
        $last = substr($data, -1);
        if ($last !== ';' && $last !== '}') {
            return false;
        }

        return preg_match('/^(?:[aOC]:\d+:|s:\d+:"|[bid]:[0-9.E+-]+;)/', $data) === 1;
    }

    /** The comparison form of a key under a case-insensitive, pad-space collation. */
    private static function collate(string $key): string
    {
        return strtolower(rtrim($key, ' '));
    }
}
