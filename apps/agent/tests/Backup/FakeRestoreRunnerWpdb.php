<?php
/**
 * Minimal in-memory $wpdb double for GH #146 RestoreRunner/RestoreHealthCheck
 * tests. Supports exactly the surface those classes touch:
 *
 *   - RestoreHealthCheck::checkDatabase() — RAW interpolated get_var() reads
 *     against `siteurl` / the users table / the posts table (no prepare()).
 *   - RestoreRunner::loadTask()/saveTaskState() — prepare()+get_row() and
 *     update() against an in-memory `wpmgr_restore_tasks` row, keyed by
 *     "snapshot_id|restore_id".
 *   - RestoreRunner::estimateLiveDbBytes() — prepare()+get_var() against
 *     information_schema (returns null/unknown; harmless).
 *   - RestoreRunner::cleanupOnCompleted() — delete().
 *   - RestoreWatchdog::run(): the same get_row() and update() surface. An
 *     update whose other WHERE columns no longer match the row changes
 *     nothing and returns 0, as in SQL.
 *
 * prepare() carries the query + bound args through as a JSON envelope
 * (mirrors the existing tests/FakeWpdb.php trick) so get_row()/get_var() can
 * recover the exact bound values without fragile SQL-text regexing.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

final class FakeRestoreRunnerWpdb
{
    public string $prefix = 'wp_';
    public string $options = 'wp_options';
    public string $users = 'wp_users';
    public string $posts = 'wp_posts';
    public string $last_error = '';

    // --- Probe A canned answers (RestoreHealthCheck::checkDatabase()). ---
    public ?string $siteurlValue = 'https://example.test';
    public bool $siteurlErrors = false;
    public bool $usersErrors = false;
    public bool $postsErrors = false;

    /**
     * In-memory `wpmgr_restore_tasks` rows, keyed "snapshot_id|restore_id".
     *
     * @var array<string,array{phase:string,kind:string,sub_state:string,resume_count:int,max_resumes:int}>
     */
    public array $rows = [];

    /** @var list<array{table:string,data:array<string,mixed>,where:array<string,mixed>}> */
    public array $updates = [];

    /** @var list<array{table:string,where:array<string,mixed>}> */
    public array $deletes = [];

    /**
     * Optional hook run after get_row() has read a row: stands in for a
     * concurrent writer between a caller's read and its write.
     *
     * @var (callable(self):void)|null
     */
    public $afterGetRow = null;

    /**
     * @param string $query SQL with %s/%d placeholders.
     * @param mixed  ...$args Bound arguments.
     */
    public function prepare(string $query, ...$args): string
    {
        $flat = [];
        foreach ($args as $a) {
            if (is_array($a)) {
                foreach ($a as $v) {
                    $flat[] = $v;
                }
            } else {
                $flat[] = $a;
            }
        }
        return (string) json_encode(['sql' => $query, 'args' => $flat]);
    }

    /**
     * @param string $prepared Output of prepare().
     * @param mixed  $mode     Ignored (ARRAY_A in production).
     * @return array<string,mixed>|null
     */
    public function get_row(string $prepared, $mode = null): ?array
    {
        $decoded = json_decode($prepared, true);
        if (!is_array($decoded) || !isset($decoded['args']) || !is_array($decoded['args'])) {
            return null;
        }
        $args = $decoded['args'];
        $key  = ($args[0] ?? '') . '|' . ($args[1] ?? '');
        $row  = $this->rows[$key] ?? null;
        if ($this->afterGetRow !== null) {
            ($this->afterGetRow)($this);
        }
        return $row;
    }

    /**
     * Handles BOTH raw interpolated SELECTs (RestoreHealthCheck's Probe A)
     * and prepared JSON-enveloped SELECTs (RestoreRunner::estimateLiveDbBytes()).
     *
     * @param string $sql
     * @return string|null
     */
    public function get_var(string $sql): ?string
    {
        $decoded = json_decode($sql, true);
        if (is_array($decoded) && isset($decoded['sql'])) {
            // A prepared query — only estimateLiveDbBytes() prepares one in
            // this class's surface. Return null (unknown size); harmless.
            $this->last_error = '';
            return null;
        }

        if (stripos($sql, 'siteurl') !== false) {
            $this->last_error = $this->siteurlErrors ? 'simulated siteurl error' : '';
            return $this->siteurlErrors ? null : $this->siteurlValue;
        }
        if (stripos($sql, $this->users) !== false) {
            $this->last_error = $this->usersErrors ? 'simulated users table error' : '';
            return $this->usersErrors ? null : '1';
        }
        if (stripos($sql, $this->posts) !== false) {
            $this->last_error = $this->postsErrors ? 'simulated posts table error' : '';
            return $this->postsErrors ? null : '1';
        }

        $this->last_error = '';
        return null;
    }

    /**
     * @param array<string,mixed> $data
     * @param array<string,mixed> $where
     */
    public function update(string $table, array $data, array $where, $format = null, $whereFormat = null): int
    {
        $this->updates[] = ['table' => $table, 'data' => $data, 'where' => $where];
        $key = ($where['snapshot_id'] ?? '') . '|' . ($where['restore_id'] ?? '');
        if (!isset($this->rows[$key])) {
            return 0;
        }
        // Every other WHERE column must match the stored row, as in SQL.
        foreach ($where as $column => $value) {
            if ($column === 'snapshot_id' || $column === 'restore_id') {
                continue;
            }
            if (!array_key_exists($column, $this->rows[$key]) || (string) $this->rows[$key][$column] !== (string) $value) {
                return 0;
            }
        }
        $this->rows[$key] = array_merge($this->rows[$key], $data);
        return 1;
    }

    /**
     * @param array<string,mixed> $where
     */
    public function delete(string $table, array $where, $format = null): int
    {
        $this->deletes[] = ['table' => $table, 'where' => $where];
        return 1;
    }

    /**
     * @param array<string,mixed> $data
     */
    public function insert(string $table, array $data, $format = null): int
    {
        return 1;
    }

    public function query(string $prepared)
    {
        return 0;
    }
}
