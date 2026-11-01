<?php
/**
 * GH #1008: one restore, one runner. RestoreRunner holds a per-restore run
 * lock (a database lock plus a file lock, as the backup TaskRunner does)
 * for its whole run, and only the holder changes anything.
 *
 *   - A second runner for the same restore, started while the first holds
 *     the lock, does nothing and changes nothing: no row write, no deletion.
 *   - A runner whose lock has been taken over stops: it never swaps, never
 *     removes staging, and never writes the row.
 *   - A row that has ended (failed or completed) stays ended: a phase write
 *     never moves it out, and the runner does not swap after it.
 *   - A host that offers neither lock still restores, but a failing stage
 *     leaves its staging tree for the cleanup job instead of removing it.
 *   - A core part that fails after the wp-content staging tree is finished
 *     removes that finished tree too.
 *
 * The second runner is started from inside the first one's progress tick,
 * so both share one staging tree exactly as two live processes would.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Backup\RestoreGuard;
use WPMgr\Agent\Backup\RestoreRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;
use ZipArchive;

/**
 * Delegates to the fake and calls $onTick at the runner's first progress
 * tick (an update that only moves last_progress_at).
 */
final class RunLockTickWpdb
{
    /** @var (callable():void)|null */
    public $onTick = null;

    private bool $ticked = false;

    public function __construct(public FakeRestoreRunnerWpdb $inner)
    {
    }

    public function __get(string $name)
    {
        return $this->inner->$name;
    }

    public function __set(string $name, $value): void
    {
        $this->inner->$name = $value;
    }

    public function __isset(string $name): bool
    {
        return isset($this->inner->$name);
    }

    /**
     * @param array<mixed> $args
     * @return mixed
     */
    public function __call(string $name, array $args)
    {
        return $this->inner->$name(...$args);
    }

    /**
     * @param array<string,mixed> $data
     * @param array<string,mixed> $where
     */
    public function update(string $table, array $data, array $where, $format = null, $whereFormat = null): int
    {
        if (!$this->ticked && $this->onTick !== null && array_keys($data) === ['last_progress_at']) {
            $this->ticked = true;
            ($this->onTick)();
        }
        return $this->inner->update($table, $data, $where, $format, $whereFormat);
    }
}

/**
 * @covers \WPMgr\Agent\Backup\RestoreRunner
 * @covers \WPMgr\Agent\Backup\FilesRestorer
 */
final class RestoreRunnerRunLockTest extends TestCase
{
    private const SNAPSHOT_ID = '40084008-4008-4008-8008-400840084008';
    private const RESTORE_ID  = 'a1b2c3d4-1008-4008-8008-a1b2c3d41008';
    private const FILES       = 120;

    private string $root       = '';
    private string $liveDir    = '';
    private string $scratchDir = '';
    private string $wpRootDir  = '';

    private FakeRestoreRunnerWpdb $fake;
    private RunLockTickWpdb $wpdb;

    /** @var list<mixed> */
    private array $shutdown = [];

    /** @var resource|null A lock handle the test holds as "another runner". */
    private $otherLock = null;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root       = sys_get_temp_dir() . '/wpmgr-1008-lock-' . bin2hex(random_bytes(6));
        $this->liveDir    = $this->root . '/wp-content';
        $this->scratchDir = $this->root . '/scratch';
        $this->wpRootDir  = $this->root . '/wproot';
        foreach ([$this->liveDir, $this->scratchDir, $this->wpRootDir] as $dir) {
            mkdir($dir, 0755, true);
        }
        $this->writeFile($this->liveDir . '/plugins/live/live.php', '<?php // live plugin');
        $this->writeFile($this->liveDir . '/uploads/2023/old.jpg', 'live upload');
        $this->writeFile($this->wpRootDir . '/wp-includes/version.php', '<?php // live core');

        $this->fake      = new FakeRestoreRunnerWpdb();
        $this->wpdb      = new RunLockTickWpdb($this->fake);
        $GLOBALS['wpdb'] = $this->wpdb;

        $this->shutdown = [];
        Functions\when('register_shutdown_function')->alias(function ($callback): void {
            $this->shutdown[] = $callback;
        });
        Functions\when('wp_mkdir_p')->alias(static function (string $target): bool {
            $target = rtrim($target, '/');
            if (file_exists($target)) {
                return @is_dir($target);
            }
            return @mkdir($target, 0755, true); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir -- test double for core's wp_mkdir_p
        });
    }

    protected function tear_down(): void
    {
        if (is_resource($this->otherLock)) {
            flock($this->otherLock, LOCK_UN);
            fclose($this->otherLock); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- test-held lock handle
        }
        $this->otherLock = null;
        $this->rrmdir($this->root);
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_a_second_runner_cannot_run_while_the_first_holds_the_lock(): void
    {
        $this->seed(['archive.part001.zip' => $this->uploadsPart()]);

        $second = null;
        $this->wpdb->onTick = function () use (&$second): void {
            $staging = $this->stagingDirs();
            $this->assertCount(1, $staging, 'precondition: the first runner is mid-stage');
            $rowBefore     = $this->row();
            $updatesBefore = count($this->fake->updates);
            $filesBefore   = $this->countFiles($staging[0]);

            $second = [
                'phase'   => $this->runner()->run(),
                'row'     => $this->row(),
                'updates' => count($this->fake->updates) - $updatesBefore,
                'files'   => $this->countFiles($staging[0]),
                'before'  => [$rowBefore, $filesBefore],
            ];
        };

        $this->assertSame(RestoreRunner::PHASE_FAILED, $this->runner()->run(), 'the first runner swaps, then fails in restore_db (no SQL artifact)');

        $this->assertIsArray($second, 'the second runner never started');
        $this->assertSame(RestoreRunner::PHASE_STAGE_FILES, $second['phase']);
        $this->assertSame(0, $second['updates'], 'a runner without the lock wrote the row');
        $this->assertSame($second['before'][0], $second['row'], 'a runner without the lock changed the row');
        $this->assertSame($second['before'][1], $second['files'], 'a runner without the lock changed the staging tree');

        $this->assertCount(self::FILES + 1, glob($this->liveDir . '/uploads/2024/*.jpg') ?: [], 'the holder swapped the whole staged tree');
        $this->assertSame(RestoreRunner::PHASE_RESTORE_DB, $this->rowSubState()['failed_in'] ?? null);
    }

    public function test_a_runner_that_lost_the_lock_never_swaps_and_writes_nothing(): void
    {
        $this->seed(['archive.part001.zip' => $this->uploadsPart()]);
        $liveBefore = $this->hashTree($this->liveDir);

        $rowAtTick = null;
        $this->wpdb->onTick = function () use (&$rowAtTick): void {
            $this->takeOverLock();
            $rowAtTick = $this->row();
        };

        $this->runner()->run();

        $this->assertSame($liveBefore, $this->hashTree($this->liveDir), 'a runner that lost the lock swapped');
        $this->assertSame([], glob($this->root . '/.wpmgr-old-*') ?: []);
        $this->assertIsArray($rowAtTick);
        $this->assertSame($rowAtTick['phase'], $this->row()['phase'], 'a runner that lost the lock moved the row');
        $this->assertSame($rowAtTick['sub_state'], $this->row()['sub_state'], 'a runner that lost the lock wrote the row');
        $this->assertSame(0, $this->fireShutdown());
    }

    public function test_a_failing_runner_that_lost_the_lock_removes_nothing(): void
    {
        // The entries after the tick end in a file that cannot be written.
        $this->seed(['archive.part001.zip' => $this->uploadsPart([
            'uploads/blocker'       => 'a file where a folder must go',
            'uploads/blocker/x.jpg' => 'cannot be written',
        ])]);

        $this->wpdb->onTick = function (): void {
            $this->takeOverLock();
        };

        $this->runner()->run();

        $staging = $this->stagingDirs();
        $this->assertCount(1, $staging, 'a runner that lost the lock removed the shared staging tree');
        $this->assertGreaterThan(0, $this->countFiles($staging[0]));
        $this->assertSame(RestoreRunner::PHASE_STAGE_FILES, $this->row()['phase'], 'a runner that lost the lock marked the row failed');
    }

    public function test_an_ended_row_stays_ended_and_the_runner_does_not_swap(): void
    {
        $this->seed(['archive.part001.zip' => $this->uploadsPart()]);
        $liveBefore = $this->hashTree($this->liveDir);

        // Something outside this runner ends the row mid-stage.
        $this->wpdb->onTick = function (): void {
            $this->fake->rows[self::SNAPSHOT_ID . '|' . self::RESTORE_ID]['phase'] = RestoreRunner::PHASE_FAILED;
        };

        $this->runner()->run();

        $this->assertSame(RestoreRunner::PHASE_FAILED, $this->row()['phase'], 'a phase write moved an ended row');
        $this->assertSame($liveBefore, $this->hashTree($this->liveDir), 'the runner swapped after its row had ended');
    }

    public function test_without_any_lock_the_restore_runs_and_a_failing_stage_leaves_staging_for_cleanup(): void
    {
        $this->blockFileLock();
        $this->seed(['archive.part001.zip' => $this->part([
            'plugins/new/new.php'        => '<?php // from the snapshot',
            'plugins/conflict'           => 'a file where a folder must go',
            'plugins/conflict/inner.php' => '<?php // cannot be written',
        ])]);
        $liveBefore = $this->hashTree($this->liveDir);

        $this->assertSame(RestoreRunner::PHASE_FAILED, $this->runner()->run());

        $this->assertSame($liveBefore, $this->hashTree($this->liveDir));
        $this->assertSame(RestoreRunner::PHASE_STAGE_FILES, $this->rowSubState()['failed_in'] ?? null);
        $this->assertStringContainsString('could not create its folder', (string) ($this->rowSubState()['last_error'] ?? ''));
        $this->assertCount(1, $this->stagingDirs(), 'without a lock, staging is left for the cleanup job, never removed');
    }

    public function test_without_any_lock_a_clean_restore_still_swaps(): void
    {
        $this->blockFileLock();
        $this->seed(['archive.part001.zip' => $this->part(['plugins/new/new.php' => '<?php // from the snapshot'])]);

        $this->assertSame(RestoreRunner::PHASE_FAILED, $this->runner()->run(), 'swaps, then fails in restore_db (no SQL artifact)');

        $this->assertFileExists($this->liveDir . '/plugins/new/new.php', 'a host without locks must still be able to restore');
        $this->assertSame(RestoreRunner::PHASE_RESTORE_DB, $this->rowSubState()['failed_in'] ?? null);
    }

    public function test_a_core_failure_after_a_finished_wp_content_stage_removes_both_staging_trees(): void
    {
        $this->seed([
            'archive.part001.zip'   => $this->part(['plugins/new/new.php' => '<?php // from the snapshot']),
            'core.g001.part001.zip' => $this->part([
                'wp-includes/conflict'       => 'a file where a folder must go',
                'wp-includes/conflict/x.php' => '<?php // cannot be written',
            ], 'core.g001.part001.zip'),
        ]);
        $liveBefore = $this->hashTree($this->liveDir);
        $coreBefore = $this->hashTree($this->wpRootDir);

        $this->assertSame(RestoreRunner::PHASE_FAILED, $this->runner()->run());

        $this->assertSame(RestoreRunner::PHASE_STAGE_FILES, $this->rowSubState()['failed_in'] ?? null);
        $this->assertSame($liveBefore, $this->hashTree($this->liveDir));
        $this->assertSame($coreBefore, $this->hashTree($this->wpRootDir));
        $this->assertSame([], $this->stagingDirs(), 'the finished wp-content staging tree must be removed too');
        $this->assertSame([], glob($this->root . '/.wpmgr-core-staging-*') ?: []);
    }

    // ------------------------------------------------------------ helpers

    /**
     * @param array<string,string> $extra Entries appended after the uploads.
     */
    private function uploadsPart(array $extra = []): string
    {
        $files = [];
        for ($i = 0; $i < self::FILES; $i++) {
            $files[sprintf('uploads/2024/%03d.jpg', $i)] = 'media ' . $i;
        }
        $files['uploads/2024/last.jpg'] = 'the last file';

        return $this->part($files + $extra);
    }

    /**
     * @param array<string,string> $files
     */
    private function part(array $files, string $name = 'archive.part001.zip'): string
    {
        $zipPath = $this->scratchDir . '/' . $name;
        $zip     = new ZipArchive();
        $this->assertTrue($zip->open($zipPath, ZipArchive::CREATE | ZipArchive::OVERWRITE));
        foreach ($files as $entry => $contents) {
            $zip->addFromString($entry, $contents);
        }
        $zip->close();

        return $zipPath;
    }

    /**
     * @param array<string,string> $paths Logical artifact name => zip path.
     */
    private function seed(array $paths): void
    {
        $this->fake->rows[self::SNAPSHOT_ID . '|' . self::RESTORE_ID] = [
            'phase'            => RestoreRunner::PHASE_STAGE_FILES,
            'kind'             => 'full',
            'sub_state'        => (string) json_encode([
                'tmp_prefix' => 'tmpffffffff_',
                'download'   => ['artifact_paths' => $paths],
            ]),
            'resume_count'     => 0,
            'max_resumes'      => 6,
            'last_progress_at' => time(),
        ];
    }

    /** The file lock path the runner uses for RESTORE_ID. */
    private function lockPath(): string
    {
        return $this->root . '/' . self::RESTORE_ID . '.lock';
    }

    /** Replace the lock file with one this test holds, as a new owner would. */
    private function takeOverLock(): void
    {
        @unlink($this->lockPath()); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test simulates another owner
        $h = fopen($this->lockPath(), 'c'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- test-held lock handle
        $this->assertNotFalse($h);
        $this->assertTrue(flock($h, LOCK_EX | LOCK_NB));
        $this->otherLock = $h;
    }

    /** A folder at the lock path: the file lock cannot be opened on this "host". */
    private function blockFileLock(): void
    {
        mkdir($this->lockPath(), 0755, true);
    }

    /** @return list<string> */
    private function stagingDirs(): array
    {
        return glob($this->root . '/.wpmgr-staging-*') ?: [];
    }

    private function countFiles(string $dir): int
    {
        if (!is_dir($dir)) {
            return 0;
        }
        $n = 0;
        foreach (new \RecursiveIteratorIterator(new \RecursiveDirectoryIterator($dir, \FilesystemIterator::SKIP_DOTS)) as $info) {
            $n += $info->isFile() ? 1 : 0;
        }
        return $n;
    }

    /**
     * @return array<string,mixed>
     */
    private function row(): array
    {
        $row = $this->fake->rows[self::SNAPSHOT_ID . '|' . self::RESTORE_ID] ?? null;
        $this->assertIsArray($row);
        return $row;
    }

    /**
     * @return array<string,mixed>
     */
    private function rowSubState(): array
    {
        $decoded = json_decode((string) $this->row()['sub_state'], true);
        $this->assertIsArray($decoded);
        return $decoded;
    }

    /**
     * @return array<string,string>
     */
    private function hashTree(string $dir): array
    {
        $out = [];
        $it  = new \RecursiveIteratorIterator(
            new \RecursiveDirectoryIterator($dir, \FilesystemIterator::SKIP_DOTS),
            \RecursiveIteratorIterator::SELF_FIRST
        );
        foreach ($it as $path => $info) {
            $rel       = substr((string) $path, strlen($dir) + 1);
            $out[$rel] = $info->isDir() ? 'dir' : (string) sha1_file((string) $path);
        }
        ksort($out);
        return $out;
    }

    private function fireShutdown(): int
    {
        $fired = 0;
        foreach ($this->shutdown as $callback) {
            if (is_array($callback) && ($callback[0] ?? null) instanceof RestoreGuard) {
                $result = $callback();
                $fired += !empty($result['fired']) ? 1 : 0;
            }
        }
        return $fired;
    }

    private function runner(): RestoreRunner
    {
        return new RestoreRunner([
            'snapshot_id'       => self::SNAPSHOT_ID,
            'restore_id'        => self::RESTORE_ID,
            'kind'              => 'full',
            'progress_endpoint' => '',
            'scratch_dir'       => $this->scratchDir,
            'wp_content_path'   => $this->liveDir,
            'wp_root'           => $this->wpRootDir,
            'db'                => [
                'host'     => '127.0.0.1:1',
                'user'     => 'nouser',
                'password' => 'nopass',
                'name'     => 'no_such_db',
                'prefix'   => 'wp_',
            ],
        ]);
    }

    private function writeFile(string $path, string $contents): void
    {
        if (!is_dir(dirname($path))) {
            mkdir(dirname($path), 0755, true);
        }
        file_put_contents($path, $contents);
    }

    private function rrmdir(string $dir): void
    {
        if ($dir === '' || !is_dir($dir)) {
            return;
        }
        foreach (scandir($dir) ?: [] as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $path = $dir . '/' . $item;
            if (is_dir($path) && !is_link($path)) {
                $this->rrmdir($path);
            } else {
                @unlink($path); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
            }
        }
        @rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- test-only fixture cleanup
    }
}
