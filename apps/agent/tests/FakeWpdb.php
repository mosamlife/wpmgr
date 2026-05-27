<?php
/**
 * Minimal in-memory $wpdb double for jti anti-replay tests.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

/**
 * Emulates only the wpdb surface the Connector touches.
 */
final class FakeWpdb
{
    public string $prefix = 'wp_';

    /** @var array<int,array{jti_hash:string,expires_at:int}> */
    private array $rows = [];

    /**
     * Naive sprintf-style prepare; carries the SQL through with placeholders
     * substituted so the matchers below can inspect it.
     *
     * @param string $query Query with %s/%d placeholders.
     * @param mixed  ...$args Bound arguments.
     * @return string
     */
    public function prepare(string $query, ...$args): string
    {
        // Encode the intent + args as a portable token the other methods parse.
        return json_encode(['sql' => $query, 'args' => $args]) ?: '';
    }

    /**
     * @param string $prepared Output of prepare().
     * @return string|null Returns "1" when a matching live jti exists.
     */
    public function get_var(string $prepared): ?string
    {
        $decoded = json_decode($prepared, true);
        if (!is_array($decoded)) {
            return null;
        }
        [$hash, $now] = $decoded['args'];

        foreach ($this->rows as $row) {
            if (hash_equals($row['jti_hash'], (string) $hash) && $row['expires_at'] >= (int) $now) {
                return '1';
            }
        }

        return null;
    }

    /**
     * Handles the prune DELETE.
     *
     * @param string $prepared Output of prepare().
     * @return int
     */
    public function query(string $prepared): int
    {
        $decoded = json_decode($prepared, true);
        if (!is_array($decoded)) {
            return 0;
        }
        $now = (int) ($decoded['args'][0] ?? 0);

        $before     = count($this->rows);
        $this->rows = array_values(array_filter(
            $this->rows,
            static fn (array $r): bool => $r['expires_at'] >= $now
        ));

        return $before - count($this->rows);
    }

    /**
     * @param string                       $table  Table name.
     * @param array<string,int|string>     $data   Row data.
     * @param array<int,string>            $format Column formats.
     * @return int
     */
    public function insert(string $table, array $data, array $format): int
    {
        $this->rows[] = [
            'jti_hash'   => (string) $data['jti_hash'],
            'expires_at' => (int) $data['expires_at'],
        ];

        return 1;
    }
}
