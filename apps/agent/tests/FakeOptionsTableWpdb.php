<?php
/**
 * In-memory $wpdb double over the two tables WordPress keeps options in: the
 * per-site options table and, on multisite, the network table (sitemeta).
 *
 * It answers only the shape a namespace sweep issues: a prepared SELECT of one
 * name column filtered by LIKE, scoped to one network for the network table.
 * LIKE is evaluated the way MySQL evaluates it under WordPress's default
 * collations: '%' is any run of characters, '_' is any single character, '\'
 * escapes the next character, and the comparison ignores case. A pattern that
 * forgets to escape '_', or is not anchored, over-matches here exactly as it
 * would against a real database.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

/**
 * Emulates the wpdb surface Lifecycle's namespace sweep touches.
 */
final class FakeOptionsTableWpdb
{
    /** Per-site options table name, as core exposes it. */
    public string $options = 'wp_options';

    /** Network table name, as core exposes it on multisite. */
    public string $sitemeta = 'wp_sitemeta';

    /** @var list<string> Every name a SELECT returned, across all calls. */
    public array $selected = [];

    /** @var list<string> Every LIKE pattern bound into a query, across all calls. */
    public array $likePatterns = [];

    /** @var array<string,mixed> Rows of the per-site options table, shared with the test by reference. */
    private array $optionRows;

    /** @var array<string,mixed> Rows of the network table for $networkId, shared with the test by reference. */
    private array $networkRows;

    private int $networkId;

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
     * Carries the query and its bound values through to get_col().
     *
     * @param string $query   Query with %s / %d placeholders.
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
     * Run a prepared SELECT of one name column filtered by LIKE.
     *
     * @param string $prepared Output of prepare().
     * @return list<string>
     */
    public function get_col(string $prepared): array
    {
        $decoded = json_decode($prepared, true);
        if (!is_array($decoded) || !isset($decoded['sql'], $decoded['args']) || !is_array($decoded['args'])) {
            throw new \LogicException('get_col() expects a query built by prepare()');
        }
        $sql = (string) $decoded['sql'];
        if (stripos(ltrim($sql), 'SELECT') !== 0) {
            throw new \LogicException('FakeOptionsTableWpdb answers SELECT only: ' . $sql);
        }

        preg_match_all('/%[sd]/', $sql, $matches);
        if (count($matches[0]) !== count($decoded['args'])) {
            throw new \LogicException('placeholder count does not match bound values: ' . $sql);
        }
        $likes    = [];
        $integers = [];
        foreach ($matches[0] as $i => $placeholder) {
            if ($placeholder === '%s') {
                $likes[] = (string) $decoded['args'][$i];
            } else {
                $integers[] = (int) $decoded['args'][$i];
            }
        }
        $this->likePatterns = array_merge($this->likePatterns, $likes);

        if (strpos($sql, $this->sitemeta) !== false) {
            // The network table holds rows for every network; a query that is
            // not scoped to exactly this network must not see these rows.
            $rows = $integers === [$this->networkId] ? $this->networkRows : [];
        } elseif (strpos($sql, $this->options) !== false) {
            $rows = $this->optionRows;
        } else {
            throw new \LogicException('query names neither table: ' . $sql);
        }

        $out = [];
        foreach (array_keys($rows) as $name) {
            foreach ($likes as $like) {
                if (self::likeMatches((string) $name, $like)) {
                    $out[] = (string) $name;
                    break;
                }
            }
        }
        $this->selected = array_merge($this->selected, $out);

        return $out;
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
}
