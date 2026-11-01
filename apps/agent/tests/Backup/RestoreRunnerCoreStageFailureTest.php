<?php
/**
 * GH #1008: core parts follow the same rule as wp-content parts. A core
 * entry the restore means to write into its core staging dir has to land
 * whole; otherwise the run ends failed in stage_files, before the core
 * overlay touches the live WordPress root.
 *
 * Each case drives the real run() dispatch loop from stage_files with a real
 * `core.g001.part001.zip`:
 *
 *   - A core entry that cannot be written (a folder sits where the file must
 *     go), one that is cut short on disk, and one whose folder cannot be
 *     created: the run fails in stage_files with the operator message, the
 *     live core files hash the same before and after, no swap pointer or
 *     shutdown rollback exists, and core staging is left for the regular
 *     cleanup.
 *   - Control: a clean core part (plus an unsafe entry, still skipped) does
 *     reach the overlay and changes the live core files, so "unchanged"
 *     above is a result of the stop.
 *
 * No live MySQL is needed: the DB credentials point at a closed loopback
 * port, so every DB connection fails fast.
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
 * @covers \WPMgr\Agent\Backup\RestoreRunner
 * @covers \WPMgr\Agent\Backup\FilesRestorer
 */
final class RestoreRunnerCoreStageFailureTest extends TestCase
{
    private const SNAPSHOT_ID = '10081008-1008-4008-8008-300830083008';
    private const RESTORE_ID  = '30083008-3008-4008-8008-300830083008';

    private string $root       = '';
    private string $liveDir    = '';
    private string $scratchDir = '';
    private string $wpRootDir  = '';

    private FakeRestoreRunnerWpdb $wpdb;

    /** @var list<mixed> Every callback the run registered for shutdown. */
    private array $shutdown = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root       = sys_get_temp_dir() . '/wpmgr-1008-core-' . bin2hex(random_bytes(6));
        $this->liveDir    = $this->root . '/wordpress/wp-content';
        $this->scratchDir = $this->root . '/scratch';
        $this->wpRootDir  = $this->root . '/wordpress';
        foreach ([$this->liveDir, $this->scratchDir] as $dir) {
            mkdir($dir, 0755, true);
        }

        $this->wpdb      = new FakeRestoreRunnerWpdb();
        $GLOBALS['wpdb'] = $this->wpdb;

        $this->shutdown = [];
        Functions\when('register_shutdown_function')->alias(function ($callback): void {
            $this->shutdown[] = $callback;
        });
        // WordPress core's wp_mkdir_p() answers false for a path a file
        // already occupies and silences its own mkdir() call. The shared stub
        // calls mkdir() bare.
        Functions\when('wp_mkdir_p')->alias(static function (string $target): bool {
            $target = rtrim($target, '/');
            if (file_exists($target)) {
                return @is_dir($target);
            }
            return @mkdir($target, 0755, true); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir -- test double for core's wp_mkdir_p
        });

        $this->writeFile($this->wpRootDir . '/index.php', '<?php // live index');
        $this->writeFile($this->wpRootDir . '/wp-admin/admin.php', '<?php // live admin');
        $this->writeFile($this->wpRootDir . '/wp-includes/version.php', '<?php // live version');
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->root);
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_a_core_entry_that_cannot_be_written_fails_the_restore_before_the_overlay(): void
    {
        // A folder sits where the staged file must be written (as on a
        // resumed run); writing a file over a folder fails for every user.
        mkdir($this->coreStagingDir() . '/wp-admin/admin.php', 0755, true);

        $message = $this->runExpectingStageFailure([
            'index.php'              => '<?php // snapshot index',
            'wp-admin/admin.php'     => '<?php // snapshot admin',
            'wp-includes/version.php' => '<?php // snapshot version',
        ]);

        $this->assertStringContainsString('wp-admin/admin.php', $message);
        $this->assertStringContainsString('could not unpack it', $message);
    }

    public function test_a_core_entry_cut_short_on_disk_fails_the_restore_before_the_overlay(): void
    {
        // What a full disk does: only part of the file reaches the disk. The
        // write reports the full length, so only the size on disk shows it.
        $cutShort = $this->coreStagingDir() . '/wp-includes/version.php';
        Functions\when('file_put_contents')->alias(static function (string $path, $data, int $flags = 0) use ($cutShort) {
            $data = is_array($data) ? implode('', $data) : (string) $data;
            $out  = $path === $cutShort ? substr($data, 0, intdiv(strlen($data), 2)) : $data;
            $h    = fopen($path, ($flags & FILE_APPEND) !== 0 ? 'ab' : 'wb'); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- test double for file_put_contents
            if ($h === false) {
                return false;
            }
            fwrite($h, $out); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fwrite -- test double for file_put_contents
            fclose($h); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- test double for file_put_contents
            return strlen($data);
        });

        $message = $this->runExpectingStageFailure([
            'index.php'               => '<?php // snapshot index',
            'wp-includes/version.php' => '<?php // snapshot version, long enough to cut',
        ]);

        $this->assertStringContainsString('wp-includes/version.php', $message);
        $this->assertStringContainsString('could not unpack it', $message);
        $this->assertStringContainsString('bytes written', $message);
    }

    public function test_a_core_entry_whose_folder_cannot_be_created_fails_the_restore_before_the_overlay(): void
    {
        // The part's own `wp-includes/blocker` file takes the path the next
        // entry needs as its folder.
        $message = $this->runExpectingStageFailure([
            'wp-includes/blocker'         => 'a file where a folder must go',
            'wp-includes/blocker/new.php' => '<?php // cannot be written',
            'index.php'                   => '<?php // snapshot index',
        ]);

        $this->assertStringContainsString('wp-includes/blocker/new.php', $message);
        $this->assertStringContainsString('could not create its folder (a file is in the way)', $message);
    }

    public function test_control_a_clean_core_part_reaches_the_overlay_and_changes_the_live_core(): void
    {
        $zipPath = $this->buildCorePart([
            'index.php'               => '<?php // snapshot index',
            'wp-includes/version.php' => '<?php // snapshot version',
            '../outside.php'          => '<?php // unsafe entry, still skipped',
        ]);
        $this->seedAtStageFiles($zipPath);
        $before = $this->hashTree($this->wpRootDir);

        // No SQL artifact: the run overlays core, then fails in restore_db.
        $this->assertSame(RestoreRunner::PHASE_FAILED, $this->runner()->run());

        $this->assertNotSame($before, $this->hashTree($this->wpRootDir), 'the rig never reached the overlay, so it cannot show a failed stage stops one');
        $this->assertSame('<?php // snapshot version', file_get_contents($this->wpRootDir . '/wp-includes/version.php'));
        $this->assertFileDoesNotExist($this->root . '/outside.php', 'an unsafe core entry must stay skipped');
        $sub = $this->rowSubState();
        $this->assertSame(RestoreRunner::PHASE_RESTORE_DB, $sub['failed_in'] ?? null);
        $this->assertSame('core_only_overlay', $sub['swap_files']['mode'] ?? null);
    }

    /**
     * Run a core-only restore of $files from stage_files, assert it failed
     * there with the live core untouched and nothing swapped, and return
     * the recorded message.
     *
     * @param array<string,string> $files
     */
    private function runExpectingStageFailure(array $files): string
    {
        $this->seedAtStageFiles($this->buildCorePart($files));
        $before = $this->hashTree($this->wpRootDir);

        $this->assertSame(RestoreRunner::PHASE_FAILED, $this->runner()->run());

        $this->assertSame($before, $this->hashTree($this->wpRootDir), 'the live core files changed although core staging failed');
        $sub = $this->rowSubState();
        $this->assertSame(RestoreRunner::PHASE_STAGE_FILES, $sub['failed_in'] ?? null);
        $this->assertArrayNotHasKey('swap_files', $sub, 'no overlay may run after a failed core stage');
        $this->assertArrayNotHasKey('stage', $sub);
        $this->assertDirectoryExists($this->coreStagingDir(), 'core staging is left in place for the regular cleanup');
        $this->assertSame(0, $this->fireShutdown(), 'nothing was overlaid, so no shutdown rollback may be armed');

        $message = (string) ($sub['last_error'] ?? '');
        $this->assertStringContainsString('the site was not changed', $message);
        $this->assertStringContainsString('Free disk space or fix folder permissions, then retry', $message);

        return $message;
    }

    /** The core staging dir the run uses for RESTORE_ID. */
    private function coreStagingDir(): string
    {
        return $this->root . '/.wpmgr-core-staging-30083008';
    }

    /**
     * @param array<string,string> $files
     */
    private function buildCorePart(array $files): string
    {
        $zipPath = $this->scratchDir . '/core.g001.part001.zip';
        $zip     = new ZipArchive();
        $this->assertTrue($zip->open($zipPath, ZipArchive::CREATE | ZipArchive::OVERWRITE));
        foreach ($files as $name => $contents) {
            $zip->addFromString($name, $contents);
        }
        $zip->close();

        return $zipPath;
    }

    private function seedAtStageFiles(string $zipPath): void
    {
        $this->wpdb->rows[self::SNAPSHOT_ID . '|' . self::RESTORE_ID] = [
            'phase'        => RestoreRunner::PHASE_STAGE_FILES,
            'kind'         => 'full',
            'sub_state'    => (string) json_encode([
                'tmp_prefix' => 'tmpeeeeeeee_',
                'download'   => ['artifact_paths' => ['core.g001.part001.zip' => $zipPath]],
            ]),
            'resume_count' => 0,
            'max_resumes'  => 6,
        ];
    }

    /**
     * @return array<string,string> Relative path => sha1 of contents (folders map to 'dir').
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

    /**
     * @return array<string,mixed>
     */
    private function rowSubState(): array
    {
        $row = $this->wpdb->rows[self::SNAPSHOT_ID . '|' . self::RESTORE_ID] ?? null;
        $this->assertIsArray($row);
        $this->assertSame(RestoreRunner::PHASE_FAILED, $row['phase']);
        $decoded = json_decode((string) $row['sub_state'], true);
        $this->assertIsArray($decoded);

        return $decoded;
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
                // A closed loopback port: every connection fails fast.
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
