<?php
/**
 * Shared in-memory $wpdb double for the GitHub issue #232 backup-stall
 * regression tests (TaskRunner's advisory run-lock + Watchdog's reaper).
 *
 * Emulates exactly the `wpmgr_backup_tasks` surface TaskRunner/Watchdog
 * touch: prepare() returns a `{sql,args}` JSON envelope — the same
 * convention already used across this suite (see tests/FakeWpdb.php and
 * PacketLimitedWpdb in TaskRunnerSubStatePacketTest.php) — and get_var() /
 * get_row() / get_col() / update() / insert() / query() / delete() all
 * inspect that envelope to decide how to answer.
 *
 * GET_LOCK()/RELEASE_LOCK() emulation is scriptable per-instance via
 * `$lockResponses` (an ordered list of '1'/'0'/null values, popped off in
 * call order; the last entry repeats once the list is exhausted) so a test
 * can express "first caller wins, second caller loses" or "GET_LOCK is
 * unsupported".
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

/**
 * In-memory wpmgr_backup_tasks double.
 */
final class FakeBackupTasksWpdb
{
    public string $prefix = 'wp_';

    /** @var array<string,array<string,mixed>> snapshot_id => row */
    public array $rows = [];

    /**
     * Scripted GET_LOCK() return values, consumed in call order. The final
     * entry repeats for any call beyond the list's length. Defaults to
     * "always wins" so tests that don't care about locking still complete.
     *
     * @var list<string|null>
     */
    public array $lockResponses = ['1'];

    public int $lockCallCount = 0;

    public int $releaseCallCount = 0;

    /**
     * Every update() call recorded, in order, for assertions.
     *
     * @var list<array{table:string,data:array<string,mixed>,where:array<string,mixed>}>
     */
    public array $updates = [];

    /** Every prepare() call ever made, incremented regardless of query shape. */
    public int $prepareCallCount = 0;

    /**
     * @param mixed ...$args
     */
    public function prepare(string $query, ...$args): string
    {
        $this->prepareCallCount++;
        return json_encode(['sql' => $query, 'args' => $args]) ?: '';
    }

    /** @return string|null */
    public function get_var(string $prepared)
    {
        [$sql, $args] = $this->decode($prepared);

        if (strpos($sql, 'GET_LOCK') !== false) {
            $idx = $this->lockCallCount;
            $this->lockCallCount++;
            if ($this->lockResponses === []) {
                return null;
            }
            return $idx < count($this->lockResponses)
                ? $this->lockResponses[$idx]
                : $this->lockResponses[count($this->lockResponses) - 1];
        }

        if (strpos($sql, 'RELEASE_LOCK') !== false) {
            $this->releaseCallCount++;
            return '1';
        }

        if (strpos($sql, 'SELECT sub_state FROM') !== false) {
            $id = (string) ($args[0] ?? '');
            return isset($this->rows[$id]) ? (string) $this->rows[$id]['sub_state'] : null;
        }

        return null;
    }

    /**
     * @param mixed $output
     * @return array<string,mixed>|null
     */
    public function get_row(string $prepared, $output = null)
    {
        [, $args] = $this->decode($prepared);
        $id = $this->idFromArgs($args);
        if ($id === '' || !isset($this->rows[$id])) {
            return null;
        }
        return $this->rows[$id];
    }

    /** @return list<string> */
    public function get_col(string $prepared): array
    {
        [, $args] = $this->decode($prepared);

        // sweepStalled(): args = [PHASE_COMPLETED, PHASE_FAILED, $cutoff, $limit].
        $excludedPhases = [(string) ($args[0] ?? ''), (string) ($args[1] ?? '')];
        $cutoff         = (int) ($args[2] ?? 0);
        $limit          = (int) ($args[3] ?? 20);

        $candidates = $this->rows;
        uasort($candidates, static fn (array $a, array $b): int => ((int) ($a['started_at'] ?? 0)) <=> ((int) ($b['started_at'] ?? 0)));

        $matched = [];
        foreach ($candidates as $id => $row) {
            if (in_array((string) ($row['phase'] ?? ''), $excludedPhases, true)) {
                continue;
            }
            if ((int) ($row['last_progress_at'] ?? 0) >= $cutoff) {
                continue;
            }
            $matched[] = (string) $id;
            if (count($matched) >= $limit) {
                break;
            }
        }

        return $matched;
    }

    /**
     * @param array<string,mixed>       $data
     * @param array<string,mixed>       $where
     * @param list<string>|string|null  $format
     * @param list<string>|string|null  $whereFormat
     */
    public function update(string $table, array $data, array $where, $format = null, $whereFormat = null): int
    {
        $id = (string) ($where['snapshot_id'] ?? '');
        if ($id === '' || !isset($this->rows[$id])) {
            return 0;
        }
        $this->updates[] = ['table' => $table, 'data' => $data, 'where' => $where];
        foreach ($data as $k => $v) {
            $this->rows[$id][$k] = $v;
        }
        return 1;
    }

    /**
     * @param array<string,mixed>      $data
     * @param list<string>|string|null $format
     */
    public function insert(string $table, array $data, $format = null): int
    {
        $id = (string) ($data['snapshot_id'] ?? '');
        if ($id === '') {
            return 0;
        }
        $this->rows[$id] = $data;
        return 1;
    }

    /**
     * @param array<string,mixed>      $where
     * @param list<string>|string|null $whereFormat
     */
    public function delete(string $table, array $where, $whereFormat = null): int
    {
        $id = (string) ($where['snapshot_id'] ?? '');
        if ($id !== '' && isset($this->rows[$id])) {
            unset($this->rows[$id]);
            return 1;
        }
        return 0;
    }

    /** Handles seedTask()'s / seedTaskRow()'s raw `INSERT IGNORE`. */
    public function query(string $prepared): int
    {
        [$sql, $args] = $this->decode($prepared);

        if (stripos($sql, 'INSERT IGNORE') !== false) {
            $id = (string) ($args[0] ?? '');
            if ($id === '' || isset($this->rows[$id])) {
                return 0; // IGNORE — missing id, or already exists.
            }
            $this->rows[$id] = [
                'snapshot_id'      => $id,
                'kind'             => (string) ($args[1] ?? ''),
                'phase'            => (string) ($args[2] ?? ''),
                'sub_state'        => (string) ($args[3] ?? '{}'),
                'started_at'       => (int) ($args[4] ?? time()),
                'last_progress_at' => (int) ($args[5] ?? time()),
                'resume_count'     => (int) ($args[6] ?? 0),
                'max_resumes'      => (int) ($args[7] ?? 6),
            ];
            return 1;
        }

        return 0;
    }

    /**
     * Test helper: seed a row directly, mirroring
     * BackupCommand::seedTaskRow()'s / TaskRunner::seedTask()'s shape.
     *
     * @param array<string,mixed> $overrides
     */
    public function seedRow(string $snapshotId, array $overrides = []): void
    {
        $now = time();
        $this->rows[$snapshotId] = array_merge([
            'snapshot_id'      => $snapshotId,
            'kind'             => 'files',
            'phase'            => 'queued',
            'sub_state'        => '{}',
            'started_at'       => $now,
            'last_progress_at' => $now,
            'resume_count'     => 0,
            'max_resumes'      => 6,
        ], $overrides);
    }

    /**
     * @param string $prepared
     * @return array{0:string,1:array<int,mixed>}
     */
    private function decode(string $prepared): array
    {
        $decoded = json_decode($prepared, true);
        if (!is_array($decoded)) {
            return ['', []];
        }
        $sql  = is_string($decoded['sql'] ?? null) ? $decoded['sql'] : '';
        $args = is_array($decoded['args'] ?? null) ? $decoded['args'] : [];
        return [$sql, $args];
    }

    /**
     * @param array<int,mixed> $args
     */
    private function idFromArgs(array $args): string
    {
        foreach ($args as $a) {
            if (is_string($a) && isset($this->rows[$a])) {
                return $a;
            }
        }
        return '';
    }
}
