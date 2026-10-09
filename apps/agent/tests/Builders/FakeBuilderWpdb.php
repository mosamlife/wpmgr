<?php
/**
 * In-memory $wpdb double for the builder document reads: one posts row by ID
 * and the postmeta rows of a post by key.
 *
 * It answers only the two query shapes BuilderDocumentFingerprint::read()
 * sends and throws on any other, so a changed query is loud. It emulates the
 * parts of MySQL those reads depend on: a posts read returns exactly the
 * columns the query names (placement columns default to 0, as the table's
 * do), postmeta rows come back in storage order unless the query asks for
 * meta_id order, and meta_key matches under the default collation
 * (case-insensitive, trailing spaces ignored).
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

/**
 * The wpdb surface the builder document reads touch.
 */
final class FakeBuilderWpdb
{
    /** The posts columns a read may name. */
    private const POST_COLUMNS = ['post_type', 'post_status', 'post_title', 'post_content', 'post_excerpt', 'post_name', 'post_password', 'post_modified_gmt', 'post_parent', 'menu_order'];

    /** The integer columns among them, which a driver may return as ints. */
    private const INT_COLUMNS = ['post_parent', 'menu_order'];

    public string $prefix   = 'wp_';
    public string $posts    = 'wp_posts';
    public string $postmeta = 'wp_postmeta';
    public string $last_error = '';

    /** @var list<array{sql:string,args:list<mixed>}> Queries run, in order. */
    public array $queries = [];

    /** @var string Table whose next read fails ("posts" or "postmeta"), or "". */
    public string $failOn = '';

    /**
     * @var bool A driver that returns native types: the integer columns of a
     *           posts read come back as ints. Off, every column is text, as
     *           mysqli answers.
     */
    public bool $nativeInts = false;

    /** @var array<int,array<string,string|int|null>> */
    private array $postRows = [];

    /** @var list<array{meta_id:int,post_id:int,meta_key:string,meta_value:string|null}> Storage order. */
    private array $metaRows = [];

    /**
     * Stores a posts row. A column the fields leave out has the table's
     * default: '' for the excerpt, the slug and the password, 0 for the
     * placement. A value may be given as text or as an int; a read answers
     * text unless $nativeInts is set.
     *
     * @param int                          $id     Post ID.
     * @param array<string,string|int|null> $fields Column => stored value.
     */
    public function addPost(int $id, array $fields): void
    {
        $this->postRows[$id] = $fields + ['post_excerpt' => '', 'post_name' => '', 'post_password' => '', 'post_parent' => '0', 'menu_order' => '0'];
    }

    /**
     * Appends a postmeta row in storage order, which need not be meta_id order.
     */
    public function addMeta(int $metaId, int $postId, string $key, ?string $value): void
    {
        $this->metaRows[] = ['meta_id' => $metaId, 'post_id' => $postId, 'meta_key' => $key, 'meta_value' => $value];
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
     * @param string $prepared Output of prepare().
     * @param string $output   ARRAY_A or OBJECT.
     * @param int    $y        Row offset (unused).
     * @return array<string,string|int|null>|object|null
     */
    public function get_row(string $prepared, string $output = 'OBJECT', int $y = 0)
    {
        [$sql, $args] = $this->begin($prepared);
        if (preg_match('/^SELECT ([a-z_]+(?:, [a-z_]+)*) FROM %i WHERE ID = %d$/', $sql, $m) !== 1) {
            throw new \LogicException('get_row: unexpected query: ' . $sql);
        }
        $columns = explode(', ', $m[1]);
        foreach ($columns as $column) {
            if (!in_array($column, self::POST_COLUMNS, true)) {
                throw new \LogicException('get_row: not a posts column: ' . $column);
            }
        }
        if ($args[0] !== $this->posts) {
            throw new \LogicException('get_row: not the posts table');
        }
        if ($this->fails('posts')) {
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
     * @param string $prepared Output of prepare().
     * @param string $output   ARRAY_A or OBJECT.
     * @return list<array<string,string|null>|object>|null
     */
    public function get_results(string $prepared, string $output = 'OBJECT'): ?array
    {
        [$sql, $args] = $this->begin($prepared);
        if (preg_match('/^SELECT meta_key, meta_value FROM %i WHERE post_id = %d AND meta_key IN \((%s(?:, %s)*)\)( ORDER BY meta_id ASC)?$/', $sql, $m) !== 1) {
            throw new \LogicException('get_results: unexpected query: ' . $sql);
        }
        if ($args[0] !== $this->postmeta) {
            throw new \LogicException('get_results: not the postmeta table');
        }
        if ($this->fails('postmeta')) {
            return null;
        }
        $postId = (int) $args[1];
        $keys   = array_map(static fn ($k): string => self::collate((string) $k), array_slice($args, 2));
        $rows   = array_values(array_filter(
            $this->metaRows,
            static fn (array $r): bool => $r['post_id'] === $postId && in_array(self::collate($r['meta_key']), $keys, true)
        ));
        if (($m[2] ?? '') !== '') {
            usort($rows, static fn (array $a, array $b): int => $a['meta_id'] <=> $b['meta_id']);
        }
        $out = [];
        foreach ($rows as $r) {
            $row   = ['meta_key' => $r['meta_key'], 'meta_value' => $r['meta_value']];
            $out[] = $output === ARRAY_A ? $row : (object) $row;
        }

        return $out;
    }

    /**
     * Records the query and resets the error, as every wpdb query does.
     *
     * @return array{0:string,1:list<mixed>}
     */
    private function begin(string $prepared): array
    {
        $this->last_error = '';
        $q                = json_decode($prepared, true, 512, JSON_THROW_ON_ERROR);
        $sql              = (string) preg_replace('/\s+/', ' ', trim((string) $q['sql']));
        $args             = array_values((array) $q['args']);
        $this->queries[]  = ['sql' => $sql, 'args' => $args];

        return [$sql, $args];
    }

    private function fails(string $table): bool
    {
        if ($this->failOn !== $table) {
            return false;
        }
        $this->failOn     = '';
        $this->last_error = 'Lost connection to MySQL server during query';

        return true;
    }

    /** The comparison form of a key under a case-insensitive, pad-space collation. */
    private static function collate(string $key): string
    {
        return strtolower(rtrim($key, ' '));
    }
}
