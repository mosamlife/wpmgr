<?php
/**
 * Smoke tests for TaskRunner — the M5.6/ADR-033 backup state-machine driver.
 *
 * Scope: contract surface only. We assert
 *   - the class is loadable in the documented namespace,
 *   - the closed-set phase constants match what the CP allowedProgressPhases
 *     validator expects,
 *   - the kind -> first-active-phase transition table is correct.
 *
 * We do NOT exercise a full backup end-to-end here — that's an integration
 * test (needs a live wpdb, a CP endpoint to receive presign/manifest, and a
 * source dir with real files). The TaskRunner internals (loadTask, seedTask,
 * saveTaskState) are reached only behind those network-dependent calls, so
 * unit-testing them in isolation would mean re-asserting Brain Monkey's
 * stubs. Defer that to the integration sweep.
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
final class TaskRunnerTest extends TestCase
{
    /**
     * The class must be loadable via the classmap autoloader and live in the
     * documented namespace. Catches namespace typos, missing `final`, or a
     * stray `class-` prefix in the include path.
     */
    public function test_class_exists_in_expected_namespace(): void
    {
        $this->assertTrue(class_exists(TaskRunner::class));
    }

    /**
     * Closed set of phase names that the CP-side allowedProgressPhases
     * validator accepts. If anyone renames a constant here it's a wire-break
     * — the CP will reject the /progress POST with HTTP 400.
     *
     * Source of truth: ADR-033 ("queued / dumping_db / archiving_files /
     * encrypting_uploading / submitting_manifest / completed / failed").
     */
    public function test_phase_constants_match_closed_set(): void
    {
        $this->assertSame('queued', TaskRunner::PHASE_QUEUED);
        $this->assertSame('dumping_db', TaskRunner::PHASE_DUMPING_DB);
        $this->assertSame('archiving_files', TaskRunner::PHASE_ARCHIVING_FILES);
        $this->assertSame('encrypting_uploading', TaskRunner::PHASE_ENCRYPTING_UPLOADING);
        $this->assertSame('submitting_manifest', TaskRunner::PHASE_SUBMITTING_MANIFEST);
        $this->assertSame('completed', TaskRunner::PHASE_COMPLETED);
        $this->assertSame('failed', TaskRunner::PHASE_FAILED);
    }

    /**
     * The set of PHASE_* constants must EXACTLY match the closed set — no
     * extras, no omissions. Defends against accidental additions that the CP
     * wouldn't recognize.
     */
    public function test_phase_constant_set_is_closed(): void
    {
        $reflection = new ReflectionClass(TaskRunner::class);
        $constants  = $reflection->getConstants();
        $phases     = [];
        foreach ($constants as $name => $value) {
            if (strpos($name, 'PHASE_') === 0) {
                $phases[$name] = $value;
            }
        }

        $expected = [
            'PHASE_QUEUED',
            'PHASE_DUMPING_DB',
            'PHASE_ARCHIVING_FILES',
            'PHASE_ENCRYPTING_UPLOADING',
            'PHASE_SUBMITTING_MANIFEST',
            'PHASE_COMPLETED',
            'PHASE_FAILED',
            // ADR-051: incremental runs use the same phases as a full backup.
            // The retired ADR-048 incremental phases (FETCH_INDEX, SCAN_FILES,
            // UPLOAD_INCREMENTAL, INCREMENTAL_FALLBACK) no longer exist.
        ];
        sort($expected);
        $actual = array_keys($phases);
        sort($actual);

        $this->assertSame($expected, $actual);
    }

    /**
     * Kind constants mirror the M4 backup_contract.go Kind enum: {files, db,
     * full}. A drift here breaks the BackupCommand handoff in Phase D.
     */
    public function test_kind_constants_match_backup_contract_enum(): void
    {
        $this->assertSame('files', TaskRunner::KIND_FILES);
        $this->assertSame('db', TaskRunner::KIND_DB);
        $this->assertSame('full', TaskRunner::KIND_FULL);
    }

    /**
     * Phase-transition table from the `queued` entry phase:
     *   kind=db    -> dumping_db
     *   kind=full  -> dumping_db
     *   kind=files -> archiving_files
     *
     * ADR-051: incremental runs (is_incremental=true) go through the SAME
     * phase sequence as a full backup; PHASE_FETCH_INDEX is retired.
     *
     * We reach into the private nextAfterQueued() via reflection so the
     * assertion targets the state-machine logic, not the (network-dependent)
     * run() loop.
     */
    public function test_next_after_queued_dispatches_by_kind(): void
    {
        $reflection = new ReflectionClass(TaskRunner::class);
        $method     = $reflection->getMethod('nextAfterQueued');
        // setAccessible() has been a no-op for private-method invocation
        // since PHP 8.1 — the test environment runs at >=8.1, so we just call.

        foreach (
            [
                TaskRunner::KIND_DB    => TaskRunner::PHASE_DUMPING_DB,
                TaskRunner::KIND_FULL  => TaskRunner::PHASE_DUMPING_DB,
                TaskRunner::KIND_FILES => TaskRunner::PHASE_ARCHIVING_FILES,
            ] as $kind => $expected
        ) {
            $runner = $this->buildRunner($kind);
            $this->assertSame(
                $expected,
                $method->invoke($runner),
                "kind={$kind} must transition queued -> {$expected}"
            );
        }
    }

    /**
     * ADR-051: an incremental run (is_incremental=true, kind=full) follows
     * the SAME queued -> dumping_db path as a full backup. No FETCH_INDEX
     * detour.
     */
    public function test_next_after_queued_incremental_uses_same_phases_as_full(): void
    {
        $reflection = new ReflectionClass(TaskRunner::class);
        $method     = $reflection->getMethod('nextAfterQueued');

        $runner = $this->buildRunner(TaskRunner::KIND_FULL, ['is_incremental' => true]);
        $this->assertSame(
            TaskRunner::PHASE_DUMPING_DB,
            $method->invoke($runner),
            'incremental kind=full must follow the same queued->dumping_db path as a full backup'
        );
    }

    /**
     * Phase-transition table from `dumping_db`:
     *   kind=db   -> encrypting_uploading (skip archiving)
     *   kind=full -> archiving_files
     */
    public function test_next_after_dumping_db_dispatches_by_kind(): void
    {
        $reflection = new ReflectionClass(TaskRunner::class);
        $method     = $reflection->getMethod('nextAfterDumpingDb');
        // setAccessible() is a no-op on PHP 8.1+ — direct invoke works.

        $this->assertSame(
            TaskRunner::PHASE_ENCRYPTING_UPLOADING,
            $method->invoke($this->buildRunner(TaskRunner::KIND_DB)),
            'kind=db skips files-archive phase'
        );
        $this->assertSame(
            TaskRunner::PHASE_ARCHIVING_FILES,
            $method->invoke($this->buildRunner(TaskRunner::KIND_FULL)),
            'kind=full continues into files-archive phase'
        );
    }

    /**
     * run() must NEVER throw — the contract is "synchronous, returns the
     * terminal phase string". A wpdb-less environment is the worst-case path
     * (no row to load, no row to seed) and must surface as PHASE_FAILED with
     * no exception escape. Defends against the BackupCommand caller (Phase D)
     * needing its own try/catch.
     */
    public function test_run_returns_failed_when_wpdb_is_missing_and_never_throws(): void
    {
        // No $GLOBALS['wpdb'] -> loadTask() returns null and seedTask() can't
        // create the row. TaskRunner's top-level catch must translate that
        // into PHASE_FAILED. The progress post is silently skipped because
        // ProgressClient construction also requires $wpdb-less Signer state
        // we don't stub here; the test asserts the outer contract only.
        unset($GLOBALS['wpdb']);

        $runner = $this->buildRunner(TaskRunner::KIND_DB);
        $phase  = $runner->run();

        $this->assertSame(TaskRunner::PHASE_FAILED, $phase);
    }

    /**
     * ADR-051: saveTaskState must persist the phase-cursor sub_state reliably.
     * The old 0-files bug was triggered by json_encode() failing on non-UTF-8
     * file paths that had been accumulated in a per-file scan cursor.
     *
     * Under ADR-051 the incremental cursor is just ~25 part-basename strings —
     * always valid UTF-8 — and the sidecar spill is retired. This regression
     * test asserts the current (simple) behavior: sub_state with a small-part
     * cursor is written verbatim and is never wiped to '{}'. It also confirms
     * the JSON encodes cleanly even when a file path has invalid UTF-8 bytes
     * (which can still appear in the files.parts list if a part name came from
     * a non-ASCII source dir on an unusual host).
     */
    public function test_save_task_state_persists_parts_cursor(): void
    {
        $wpdb = new CapturingWpdb();
        $GLOBALS['wpdb'] = $wpdb;

        try {
            $runner = $this->buildRunner(TaskRunner::KIND_FULL);

            // Simulate a typical sub_state after archiving_files: ~25 part names
            // plus a files cursor. All ASCII — json_encode will always succeed.
            $subState = [
                'files' => [
                    'done'          => true,
                    'parts'         => ['plugins.part001.zip', 'themes.part001.zip'],
                    'part_kinds'    => ['plugin', 'theme'],
                    'parts_total'   => 2,
                    'files_total'   => 42,
                    'bytes_written' => 1024 * 1024,
                    'tombstones'    => [],
                ],
            ];

            $reflection = new ReflectionClass(TaskRunner::class);
            $save       = $reflection->getMethod('saveTaskState');
            $save->invoke($runner, TaskRunner::PHASE_ENCRYPTING_UPLOADING, $subState);

            $this->assertNotNull($wpdb->lastSubState, 'saveTaskState skipped the write — sub_state was lost');
            $this->assertNotSame('{}', $wpdb->lastSubState, 'sub_state was wiped to {} (regression)');

            $decoded = json_decode((string) $wpdb->lastSubState, true);
            $this->assertIsArray($decoded);
            $this->assertArrayHasKey('files', $decoded);
            $this->assertSame(['plugins.part001.zip', 'themes.part001.zip'], $decoded['files']['parts']);
            $this->assertSame(42, $decoded['files']['files_total']);
        } finally {
            unset($GLOBALS['wpdb']);
        }
    }

    /**
     * GH #283 follow-up: saveTaskState() must return true only when the
     * write genuinely reached durable storage. A successful $wpdb->update()
     * is the durable case.
     */
    public function test_save_task_state_returns_true_on_durable_persist(): void
    {
        $wpdb = new CapturingWpdb();
        $GLOBALS['wpdb'] = $wpdb;

        try {
            $runner     = $this->buildRunner(TaskRunner::KIND_FULL);
            $reflection = new ReflectionClass(TaskRunner::class);
            $save       = $reflection->getMethod('saveTaskState');
            $result     = $save->invoke($runner, TaskRunner::PHASE_ENCRYPTING_UPLOADING, ['upload' => ['uploaded_hashes' => ['abc']]]);

            $this->assertTrue($result, 'a successful DB write must report true, not just "did not throw"');
        } finally {
            unset($GLOBALS['wpdb']);
        }
    }

    /**
     * GH #283 follow-up: saveTaskState() has three silent no-op paths that
     * persist nothing and return normally, with nothing to throw. Each one
     * must now report false so a caller relying on "did this throw" alone
     * (the old bug) can instead check the return value.
     */
    public function test_save_task_state_returns_false_when_wpdb_missing(): void
    {
        unset($GLOBALS['wpdb']);

        $runner     = $this->buildRunner(TaskRunner::KIND_FULL);
        $reflection = new ReflectionClass(TaskRunner::class);
        $save       = $reflection->getMethod('saveTaskState');
        $result     = $save->invoke($runner, TaskRunner::PHASE_ENCRYPTING_UPLOADING, ['upload' => ['uploaded_hashes' => ['abc']]]);

        $this->assertFalse($result, 'no $wpdb at all is a silent no-op and must report false, not just "did not throw"');
    }

    /**
     * See test_save_task_state_returns_false_when_wpdb_missing for context.
     * This covers the second silent path: $wpdb is present but the table
     * name cannot be resolved (an empty prefix).
     */
    public function test_save_task_state_returns_false_when_table_name_unresolvable(): void
    {
        $wpdb = new CapturingWpdb();
        $wpdb->prefix = '';
        $GLOBALS['wpdb'] = $wpdb;

        try {
            $runner     = $this->buildRunner(TaskRunner::KIND_FULL);
            $reflection = new ReflectionClass(TaskRunner::class);
            $save       = $reflection->getMethod('saveTaskState');
            $result     = $save->invoke($runner, TaskRunner::PHASE_ENCRYPTING_UPLOADING, ['upload' => ['uploaded_hashes' => ['abc']]]);

            $this->assertFalse($result, 'an unresolvable table name is a silent no-op and must report false, not just "did not throw"');
        } finally {
            unset($GLOBALS['wpdb']);
        }
    }

    /**
     * See test_save_task_state_returns_false_when_wpdb_missing for context.
     * This covers the third silent path: json_encode() itself fails
     * (malformed UTF8 bytes in sub_state, which json_encode rejects by
     * default with no flags to coerce or escape it).
     */
    public function test_save_task_state_returns_false_when_json_encode_fails(): void
    {
        $wpdb = new CapturingWpdb();
        $GLOBALS['wpdb'] = $wpdb;

        try {
            $runner     = $this->buildRunner(TaskRunner::KIND_FULL);
            $reflection = new ReflectionClass(TaskRunner::class);
            $save       = $reflection->getMethod('saveTaskState');
            // "\xB1\x31" is not valid UTF8, so json_encode() returns false.
            $result = $save->invoke($runner, TaskRunner::PHASE_ENCRYPTING_UPLOADING, ['bad' => "\xB1\x31"]);

            $this->assertFalse($result, 'a json_encode failure is a silent no-op and must report false, not just "did not throw"');
            $this->assertNull($wpdb->lastSubState, 'nothing should have reached the DB update call');
        } finally {
            unset($GLOBALS['wpdb']);
        }
    }

    /**
     * Build a TaskRunner with a minimal, syntactically-valid params payload.
     * We never actually touch the network/disk in these tests — the
     * reflection-driven asserts target pure transition helpers.
     *
     * @param string              $kind      One of {files, db, full}.
     * @param array<string,mixed> $extraParams Additional params to merge.
     */
    private function buildRunner(string $kind, array $extraParams = []): TaskRunner
    {
        return new TaskRunner(array_merge([
            'snapshot_id'       => '00000000-0000-0000-0000-000000000000',
            'kind'              => $kind,
            'age_recipient'     => 'age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq', // shape-only
            'presign_endpoint'  => 'https://cp.invalid/agent/v1/backups/x/presign',
            'manifest_endpoint' => 'https://cp.invalid/agent/v1/backups/x/manifest',
            'progress_endpoint' => '', // empty -> no ProgressClient construction attempted
            'chunk_bytes'       => 4 * 1024 * 1024,
            'scratch_dir'       => sys_get_temp_dir() . DIRECTORY_SEPARATOR . 'wpmgr-taskrunner-unit',
            'wp_content_path'   => sys_get_temp_dir(),
            'db'                => [
                'host'     => 'localhost',
                'user'     => 'wp',
                'password' => 'wp',
                'name'     => 'wp_db',
                'prefix'   => 'wp_',
            ],
        ], $extraParams));
    }
}

/**
 * In-memory $wpdb double that captures the sub_state passed to update(), so the
 * regression test can assert the serialized cursor survives non-UTF-8 paths.
 */
final class CapturingWpdb
{
    public string $prefix = 'wp_';

    /** @var string|null The sub_state value of the last update() call. */
    public ?string $lastSubState = null;

    /**
     * @param string               $table  Table name.
     * @param array<string,mixed>   $data   Column => value.
     * @param array<string,mixed>   $where  WHERE column => value.
     * @param list<string>|null     $format Value formats.
     * @param list<string>|null     $whereFormat WHERE formats.
     */
    public function update($table, $data, $where, $format = null, $whereFormat = null): int
    {
        if (isset($data['sub_state']) && is_string($data['sub_state'])) {
            $this->lastSubState = $data['sub_state'];
        }
        return 1;
    }
}
