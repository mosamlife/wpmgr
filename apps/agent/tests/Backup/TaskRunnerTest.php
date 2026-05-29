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
     *   kind=db   -> dumping_db
     *   kind=full -> dumping_db
     *   kind=files -> archiving_files
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
     * Build a TaskRunner with a minimal, syntactically-valid params payload.
     * We never actually touch the network/disk in these tests — the
     * reflection-driven asserts target pure transition helpers.
     *
     * @param string $kind One of {files, db, full}.
     */
    private function buildRunner(string $kind): TaskRunner
    {
        return new TaskRunner([
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
        ]);
    }
}
