<?php
/**
 * Sub_state persistence + sidecar-spill tests for TaskRunner.
 *
 * Background
 * ----------
 * The original "0-files base" bug (ADR-048) was caused by the incremental scan
 * cursor (scan.changed[] with thousands of entries) serializing to >1 MB and
 * silently failing $wpdb->update() when @@max_allowed_packet was < 1 MiB. Two
 * safety nets were added in 0.21.2:
 *
 *   (a) Sidecar-spill: sub_state encoded > ~48 KiB is written to
 *       `<scratch>/task_substate.json` (atomic temp+rename); the DB column
 *       holds only a small `{"_sidecar":true,"file":"..."}` pointer.
 *       loadTask() rehydrates from the sidecar on watchdog re-entry.
 *       cleanupOnCompleted() unlinks it.
 *
 *   (b) Throw-on-false: $wpdb->update() returning false (genuine DB error,
 *       e.g. packet overflow) throws \RuntimeException so the watchdog retries
 *       rather than silently completing with a stale cursor.
 *
 * ADR-051 (archive-delta pivot) retired the sidecar-spill and throw-on-false
 * with the comment "the cursor is now tiny". That was correct for the parts
 * cursor (~25 part-name strings) BUT the tombstone list in sub_state.files
 * was left as an unbounded array — a deletion-heavy increment removing
 * thousands of files re-introduced the exact same silent-write bug.
 *
 * 0.21.5 restores both safety nets and moves the tombstone list to an on-disk
 * flat file (tombstones.list written by FilesArchiver) so sub_state stays
 * small even before the sidecar triggers. Both behaviors are tested here.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use ReflectionClass;
use WPMgr\Agent\Backup\TaskRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\TaskRunner
 */
final class TaskRunnerSubStatePacketTest extends TestCase
{
    private string $scratch = '';

    protected function set_up(): void
    {
        parent::set_up();
        $this->scratch = sys_get_temp_dir() . '/wpmgr-substate-' . bin2hex(random_bytes(6));
        mkdir($this->scratch, 0777, true);
    }

    protected function tear_down(): void
    {
        foreach (glob($this->scratch . '/*') ?: [] as $f) {
            @unlink($f);
        }
        @rmdir($this->scratch);
        unset($GLOBALS['wpdb']);
        parent::tear_down();
    }

    /**
     * ADR-051/0.21.5: the archive-delta sub_state only carries ~25 part-basename
     * strings (files.parts[]) — always a few KB — so a typical cursor fits inline
     * even under a small packet limit. The sidecar is NOT triggered here.
     */
    public function test_archive_delta_cursor_survives_reload_under_small_packet_limit(): void
    {
        // 256 KiB packet — smaller than the old per-file scan cursor but large
        // enough for the new ~25-part-name cursor (a few KB at most).
        $wpdb = new PacketLimitedWpdb(256 * 1024);
        $GLOBALS['wpdb'] = $wpdb;

        $snapshotId = '11111111-2222-3333-4444-555555555555';
        $params     = $this->params($snapshotId);

        // Seed the row like BackupCommand::seedTaskRow.
        $wpdb->seedRow($snapshotId, 'full', $params);

        $runner = new TaskRunner($params);
        $ref    = new ReflectionClass($runner);
        $save   = $ref->getMethod('saveTaskState');

        // Build a typical ADR-051 sub_state: files.parts = ~25 part names,
        // tombstones as on-disk file reference (not inline array).
        $parts = [];
        for ($i = 1; $i <= 25; $i++) {
            $parts[] = 'plugins.part' . str_pad((string) $i, 3, '0', STR_PAD_LEFT) . '.zip';
        }
        $subState = [
            'params' => $params,
            'files'  => [
                'done'              => true,
                'parts'             => $parts,
                'part_kinds'        => array_fill(0, 25, 'plugin'),
                'parts_total'       => 25,
                'files_total'       => 75000,
                'bytes_written'     => 512 * 1024 * 1024,
                'tombstones_file'   => $this->scratch . '/tombstones.list',
                'tombstones_count'  => 1,
            ],
        ];

        $beforeRejections = $wpdb->packetRejections;
        $save->invoke($runner, TaskRunner::PHASE_ENCRYPTING_UPLOADING, $subState);

        // The DB write must have SUCCEEDED with NO packet rejection because the
        // cursor is tiny (a few KB, not hundreds of KB).
        $this->assertSame(
            $beforeRejections,
            $wpdb->packetRejections,
            'archive-delta cursor must never exceed @@max_allowed_packet'
        );

        // Reload from the DB and verify the cursor is intact.
        $runner2 = new TaskRunner($params);
        $ref2    = new ReflectionClass($runner2);
        $load    = $ref2->getMethod('loadTask');
        $task    = $load->invoke($runner2);

        $this->assertIsArray($task);
        $sub = $task['sub_state'];
        $this->assertArrayHasKey('files', $sub, 'files cursor was lost across the DB round-trip');
        $this->assertCount(25, $sub['files']['parts'], 'all 25 part names must reload intact');
        $this->assertSame(75000, $sub['files']['files_total']);
        $this->assertSame($this->scratch . '/tombstones.list', $sub['files']['tombstones_file']);
    }

    /**
     * 0.21.5: saveTaskState throws when $wpdb->update() returns false (genuine
     * DB failure). This ensures the watchdog retries on a write error instead
     * of silently completing with a stale cursor.
     */
    public function test_save_throws_when_db_update_fails(): void
    {
        $wpdb                   = new PacketLimitedWpdb(8 * 1024 * 1024);
        $wpdb->forceUpdateFailure = true;
        $GLOBALS['wpdb']        = $wpdb;

        $snapshotId = '22222222-3333-4444-5555-666666666666';
        $params     = $this->params($snapshotId);
        $wpdb->seedRow($snapshotId, 'full', $params);

        $runner = new TaskRunner($params);
        $ref    = new ReflectionClass($runner);
        $save   = $ref->getMethod('saveTaskState');

        $threw = false;
        try {
            $save->invoke($runner, TaskRunner::PHASE_DUMPING_DB, ['params' => $params]);
        } catch (\RuntimeException $e) {
            $threw = true;
        }

        $this->assertTrue($threw, 'saveTaskState must throw on update() failure so the watchdog retries');
    }

    /**
     * 0.21.5: when sub_state exceeds the sidecar threshold, saveTaskState writes
     * the full cursor to <scratch>/task_substate.json and stores only a pointer
     * in the DB. loadTask() must rehydrate the full cursor from disk.
     */
    public function test_sidecar_spill_and_rehydration(): void
    {
        // 1 MiB packet limit — matches the MySQL default.
        $wpdb = new PacketLimitedWpdb(1024 * 1024);
        $GLOBALS['wpdb'] = $wpdb;

        $snapshotId = '33333333-4444-5555-6666-777777777777';
        $params     = $this->params($snapshotId);
        $wpdb->seedRow($snapshotId, 'full', $params);

        $runner = new TaskRunner($params);
        $ref    = new ReflectionClass($runner);
        $save   = $ref->getMethod('saveTaskState');

        // Build a sub_state that is just over the 48 KiB sidecar threshold.
        // We simulate what the old (pre-0.21.5) code produced: a large inline
        // tombstones array.
        $bigTombstones = [];
        for ($i = 0; $i < 3000; $i++) {
            $bigTombstones[] = 'plugins/some-plugin/file-' . $i . '.php';
        }
        $subState = [
            'files' => [
                'done'       => true,
                'parts'      => ['plugins.g001.part001.zip'],
                'tombstones' => $bigTombstones, // deliberately inline — large
            ],
        ];

        // Confirm it would indeed exceed 48 KiB inline.
        $this->assertGreaterThan(48 * 1024, strlen((string) json_encode($subState)));

        $save->invoke($runner, TaskRunner::PHASE_ENCRYPTING_UPLOADING, $subState);

        // The DB column must hold the sidecar pointer, NOT the full payload.
        $row = $wpdb->rows[$snapshotId] ?? null;
        $this->assertNotNull($row);
        $dbPayload = json_decode((string) ($row['sub_state'] ?? ''), true);
        $this->assertIsArray($dbPayload);
        $this->assertTrue(
            isset($dbPayload['_sidecar']) && $dbPayload['_sidecar'] === true,
            'DB column must hold a _sidecar pointer when sub_state exceeds the threshold'
        );
        $this->assertArrayHasKey('file', $dbPayload, 'sidecar pointer must carry the file path');

        // The sidecar file must exist on disk.
        $sidecarPath = (string) $dbPayload['file'];
        $this->assertFileExists($sidecarPath, 'sidecar file must be written to scratch dir');

        // loadTask() must rehydrate the full cursor from the sidecar.
        $runner2 = new TaskRunner($params);
        $ref2    = new ReflectionClass($runner2);
        $load    = $ref2->getMethod('loadTask');
        $task    = $load->invoke($runner2);

        $this->assertIsArray($task);
        $sub = $task['sub_state'];
        $this->assertArrayHasKey('files', $sub, 'files key missing after sidecar rehydration');
        $this->assertCount(3000, $sub['files']['tombstones'], 'all 3000 tombstone entries must reload intact from sidecar');
    }

    /**
     * GH #256 finding 5: TaskRunner::decodeSubStateColumn() is the shared,
     * public helper loadTask() itself uses, and the ONLY correct way to
     * decode a raw sub_state column value once it may have spilled to the
     * sidecar. A bare json_decode() of the column (the pre-fix behavior in
     * Watchdog::decodeSubState()) returns only the small
     * `{"_sidecar":true,"file":"..."}` pointer for a spilled row, never the
     * real payload underneath, including `params`. Asserted directly here,
     * independent of loadTask()/Watchdog, so a future caller of this shared
     * helper gets the same guarantee.
     */
    public function test_decode_sub_state_column_follows_the_sidecar_pointer(): void
    {
        $wpdb = new PacketLimitedWpdb(1024 * 1024);
        $GLOBALS['wpdb'] = $wpdb;

        $snapshotId = '99999999-aaaa-4bbb-8ccc-dddddddddddd';
        $params     = $this->params($snapshotId);
        $wpdb->seedRow($snapshotId, 'full', $params);

        $runner = new TaskRunner($params);
        $ref    = new ReflectionClass($runner);
        $save   = $ref->getMethod('saveTaskState');

        $bigTombstones = [];
        for ($i = 0; $i < 3000; $i++) {
            $bigTombstones[] = 'plugins/some-plugin/file-' . $i . '.php';
        }
        $subState = [
            'params' => $params,
            'files'  => [
                'done'       => true,
                'tombstones' => $bigTombstones,
            ],
        ];
        $save->invoke($runner, TaskRunner::PHASE_ENCRYPTING_UPLOADING, $subState);

        $rawColumn = (string) ($wpdb->rows[$snapshotId]['sub_state'] ?? '');
        $this->assertStringContainsString('_sidecar', $rawColumn, 'sanity: the fixture must actually have spilled to the sidecar');

        // A bare json_decode() (the pre-fix behavior) would stop here.
        $bareDecode = json_decode($rawColumn, true);
        $this->assertIsArray($bareDecode);
        $this->assertArrayNotHasKey('params', $bareDecode, 'sanity: the raw column alone never carries params once spilled');

        // The shared helper must follow the pointer and recover the full payload.
        $decoded = TaskRunner::decodeSubStateColumn($rawColumn);
        $this->assertArrayHasKey('params', $decoded, 'decodeSubStateColumn() must rehydrate params from the sidecar');
        $this->assertSame($snapshotId, $decoded['params']['snapshot_id'] ?? null);
        $this->assertArrayHasKey('files', $decoded);
        $this->assertCount(3000, $decoded['files']['tombstones'] ?? []);
    }

    /** @return array<string,mixed> */
    private function params(string $snapshotId): array
    {
        return [
            'snapshot_id'            => $snapshotId,
            'kind'                   => 'full',
            'age_recipient'          => 'age1' . str_repeat('q', 58),
            'presign_endpoint'       => 'https://cp.invalid/agent/v1/backups/x/presign',
            'manifest_endpoint'      => 'https://cp.invalid/agent/v1/backups/x/manifest',
            'progress_endpoint'      => '',
            'chunk_bytes'            => 4 * 1024 * 1024,
            'scratch_dir'            => $this->scratch,
            'wp_content_path'        => $this->scratch,
            'db'                     => ['host' => 'localhost', 'user' => 'u', 'password' => 'p', 'name' => 'n', 'prefix' => 'wp_'],
            'is_incremental'         => true,
            'prev_files_list_chunks' => [],
        ];
    }
}

/**
 * A $wpdb double that enforces @@max_allowed_packet on UPDATE. Used to verify
 * the sidecar-spill prevents packet-overflow silent failures.
 */
final class PacketLimitedWpdb
{
    public string $prefix = 'wp_';
    public int $packetRejections = 0;
    public bool $forceUpdateFailure = false;

    /** @var array<string,array<string,mixed>> */
    public array $rows = [];

    public function __construct(private int $maxAllowedPacket)
    {
    }

    public function prepare(string $query, ...$args): string
    {
        return json_encode(['sql' => $query, 'args' => $args]) ?: '';
    }

    /** @return int|false */
    public function update($table, $data, $where, $format = null, $whereFormat = null)
    {
        if ($this->forceUpdateFailure) {
            return false;
        }
        $id = (string) ($where['snapshot_id'] ?? '');
        if ($id === '' || !isset($this->rows[$id])) {
            return false;
        }
        $bytes = 256;
        foreach ($data as $v) {
            $bytes += strlen((string) $v);
        }
        if ($bytes > $this->maxAllowedPacket) {
            $this->packetRejections++;
            return false; // row unchanged — mirrors real mysqli over-packet failure
        }
        foreach ($data as $k => $v) {
            $this->rows[$id][$k] = $v;
        }
        return 1;
    }

    /** @return array<string,mixed>|null */
    public function get_row($prepared, $output = ARRAY_A)
    {
        $id = $this->idFrom($prepared);
        if ($id === '' || !isset($this->rows[$id])) {
            return null;
        }
        $row = $this->rows[$id];
        return $output === ARRAY_A ? $row : (object) $row;
    }

    public function get_var($q) { return '1'; }
    public function query($q) { return 1; }
    public function insert($t, $d, $f = null) { return 1; }
    public function delete($t, $w, $wf = null) { return 1; }

    /** @param array<string,mixed> $params */
    public function seedRow(string $snapshotId, string $kind, array $params): void
    {
        $now = time();
        $this->rows[$snapshotId] = [
            'snapshot_id'      => $snapshotId,
            'kind'             => $kind,
            'phase'            => 'queued',
            'sub_state'        => (string) json_encode(['params' => $params]),
            'started_at'       => $now,
            'last_progress_at' => $now,
            'resume_count'     => 0,
            'max_resumes'      => 6,
        ];
    }

    private function idFrom($prepared): string
    {
        if (!is_string($prepared)) {
            return '';
        }
        $d = json_decode($prepared, true);
        if (is_array($d) && isset($d['args'])) {
            foreach ($d['args'] as $a) {
                if (is_string($a) && preg_match('/^[a-f0-9-]{36}$/i', $a)) {
                    return $a;
                }
            }
        }
        return '';
    }
}
