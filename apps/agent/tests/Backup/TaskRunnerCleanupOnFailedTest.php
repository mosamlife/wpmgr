<?php
/**
 * GH #279: on a genuinely terminal failure, TaskRunner must GC the orphaned
 * scratch dir + chunk/artifact files -- mirroring cleanupOnCompleted()'s
 * file-level sweep -- instead of leaving them on disk until the age-gated
 * BackupJanitor::gcRuns() backstop eventually sweeps them.
 *
 * We reach the private cleanupOnFailed() directly via reflection: it only
 * touches the filesystem (no wpdb, no network), so this keeps the test
 * hermetic per the "self-contained fakes" convention.
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
final class TaskRunnerCleanupOnFailedTest extends TestCase
{
    private string $scratchDir = '';

    protected function set_up(): void
    {
        parent::set_up();
        $this->scratchDir = sys_get_temp_dir() . DIRECTORY_SEPARATOR . 'wpmgr-taskrunner-cleanup-' . bin2hex(random_bytes(6));
        mkdir($this->scratchDir, 0700, true);
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->scratchDir);
        parent::tear_down();
    }

    /**
     * A fully-populated scratch dir (chunk files of both encryption modes,
     * DB dump, per-component zip parts of both the namespaced and legacy
     * naming, files.list/tombstones.list, the paths caches, and the sub_state
     * sidecar) must be swept down to nothing -- and the now-empty scratch dir
     * itself removed -- by cleanupOnFailed().
     */
    public function test_cleanup_on_failed_removes_chunk_and_artifact_files_and_scratch_dir(): void
    {
        $dir = $this->scratchDir;

        // Chunk files (both encryption modes; only one is ever live per
        // deployment, but the sweep must catch either).
        file_put_contents($dir . '/chunks-aaaa1111.age', 'ciphertext');
        file_put_contents($dir . '/chunks-bbbb2222.bin', 'plaintext');

        // Artifact files.
        file_put_contents($dir . '/database.sql.gz', 'gz-bytes');
        file_put_contents($dir . '/paths.cache', "wp-content\tsome/file.php\n");
        file_put_contents($dir . '/files.list', "some/file.php\t10\t1700000000\n");
        file_put_contents($dir . '/tombstones.list', "deleted/file.php\n");
        file_put_contents($dir . '/core-paths.cache', "core/wp-load.php\n");

        // Per-component zip parts: generation-namespaced (current) + legacy
        // (pre-namespace) naming, across every component the sweep covers.
        file_put_contents($dir . '/plugins.g000.part001.zip', 'zip-bytes');
        file_put_contents($dir . '/themes.part001.zip', 'zip-bytes');
        file_put_contents($dir . '/uploads.g001.part002.zip', 'zip-bytes');
        file_put_contents($dir . '/wp-content.g000.part001.zip', 'zip-bytes');
        file_put_contents($dir . '/core.g000.part001.zip', 'zip-bytes');

        // ADR-051 prev_files.list + the sub_state sidecar.
        file_put_contents($dir . '/prev_files.list', "old/file.php\t5\t1699999999\n");
        file_put_contents($dir . '/task_substate.json', '{"encrypt":{}}');

        $runner = $this->buildRunner(['scratch_dir' => $dir]);

        $reflection = new ReflectionClass(TaskRunner::class);
        $method     = $reflection->getMethod('cleanupOnFailed');
        $method->invoke($runner);

        self::assertDirectoryDoesNotExist($dir, 'the now-empty scratch dir must be removed');
    }

    /**
     * A scratch dir with an unrecognised leftover file (something the sweep
     * doesn't know about) is NOT fully emptied, so rmdir() correctly refuses
     * -- the dir survives as a non-fatal partial GC, backstopped by
     * BackupJanitor::gcRuns(). Regression guard: cleanupOnFailed() must never
     * throw when rmdir() fails on a non-empty directory.
     */
    public function test_cleanup_on_failed_is_best_effort_when_dir_not_fully_empty(): void
    {
        $dir = $this->scratchDir;
        file_put_contents($dir . '/chunks-cccc3333.age', 'ciphertext');
        file_put_contents($dir . '/some-unknown-artifact.dat', 'unswept bytes');

        $runner = $this->buildRunner(['scratch_dir' => $dir]);

        $reflection = new ReflectionClass(TaskRunner::class);
        $method     = $reflection->getMethod('cleanupOnFailed');
        // Must not throw.
        $method->invoke($runner);

        self::assertDirectoryExists($dir, 'a non-empty dir must survive rmdir()s refusal, not error out');
        self::assertFileDoesNotExist($dir . '/chunks-cccc3333.age', 'known chunk file must still be swept');
        self::assertFileExists($dir . '/some-unknown-artifact.dat', 'unrecognised artifact is left for the age-gated backstop');
    }

    /**
     * An empty/unset scratch_dir (or one that doesn't exist) must be a
     * silent no-op -- cleanupOnFailed() runs inside the top-level catch,
     * where a second exception must never mask the original failure.
     */
    public function test_cleanup_on_failed_no_ops_on_missing_scratch_dir(): void
    {
        $runner = $this->buildRunner(['scratch_dir' => $this->scratchDir . '/does-not-exist']);

        $reflection = new ReflectionClass(TaskRunner::class);
        $method     = $reflection->getMethod('cleanupOnFailed');
        $method->invoke($runner);

        // No exception -- that's the whole assertion. Sanity-confirm the
        // real scratch dir (untouched by this call) is still there.
        self::assertDirectoryExists($this->scratchDir);
    }

    /**
     * GH #256: reclaimScratch() is the public wrapper Watchdog's terminal
     * give-up paths use to reach cleanupOnFailed() from OUTSIDE run()'s own
     * try/catch. Proves it delegates to the exact same sweep (no second,
     * independently-duplicated implementation) without going through
     * reflection.
     */
    public function test_reclaim_scratch_delegates_to_the_same_sweep_as_cleanup_on_failed(): void
    {
        $dir = $this->scratchDir;
        file_put_contents($dir . '/database.sql.gz', 'gz-bytes');
        file_put_contents($dir . '/chunks-aaaa1111.age', 'ciphertext');

        $runner = $this->buildRunner(['scratch_dir' => $dir]);
        $runner->reclaimScratch();

        self::assertDirectoryDoesNotExist($dir, 'reclaimScratch() must remove the now-empty scratch dir via the same sweep cleanupOnFailed() runs');
    }

    /**
     * Build a TaskRunner with a minimal, syntactically-valid params payload.
     * Mirrors TaskRunnerTest::buildRunner().
     *
     * @param array<string,mixed> $extraParams Additional/overriding params.
     */
    private function buildRunner(array $extraParams = []): TaskRunner
    {
        return new TaskRunner(array_merge([
            'snapshot_id'       => '00000000-0000-0000-0000-000000000000',
            'kind'              => TaskRunner::KIND_FULL,
            'age_recipient'     => 'age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq',
            'presign_endpoint'  => 'https://cp.invalid/agent/v1/backups/x/presign',
            'manifest_endpoint' => 'https://cp.invalid/agent/v1/backups/x/manifest',
            'progress_endpoint' => '',
            'chunk_bytes'       => 4 * 1024 * 1024,
            'scratch_dir'       => $this->scratchDir,
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

    /**
     * Recursively delete a directory tree (used by tear_down).
     */
    private function rrmdir(string $dir): void
    {
        if (!is_dir($dir)) {
            if (is_file($dir) || is_link($dir)) {
                @unlink($dir);
            }
            return;
        }
        $entries = scandir($dir);
        if ($entries === false) {
            return;
        }
        foreach ($entries as $entry) {
            if ($entry === '.' || $entry === '..') {
                continue;
            }
            $this->rrmdir($dir . DIRECTORY_SEPARATOR . $entry);
        }
        @rmdir($dir);
    }
}
