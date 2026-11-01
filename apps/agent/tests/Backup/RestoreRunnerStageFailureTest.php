<?php
/**
 * GH #1008: a restore whose staging cannot write a file it means to restore
 * ends failed before any swap, and the live wp-content is byte-identical
 * afterwards.
 *
 * Each case drives the real run() dispatch loop from stage_files with a real
 * archive part:
 *
 *   - The part holds an entry whose folder cannot be created. The run ends
 *     failed in stage_files with an operator message that says the site was
 *     not changed, names the file and says what to do; the live tree hashes
 *     the same before and after; no swap pointer, old-files tree or
 *     shutdown rollback exists, and staging is left for the regular cleanup.
 *   - Control: the same rig with a part that stages cleanly does reach the
 *     swap and changes the live tree, so the first case's "unchanged" is a
 *     result of the stop, not of a rig that never swaps.
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
final class RestoreRunnerStageFailureTest extends TestCase
{
    private const SNAPSHOT_ID = '10081008-1008-4008-8008-100810081008';
    private const RESTORE_ID  = '20082008-2008-4008-8008-200820082008';

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

        $this->root       = sys_get_temp_dir() . '/wpmgr-1008-' . bin2hex(random_bytes(6));
        $this->liveDir    = $this->root . '/wp-content';
        $this->scratchDir = $this->root . '/scratch';
        $this->wpRootDir  = $this->root . '/wproot';
        foreach ([$this->liveDir, $this->scratchDir, $this->wpRootDir] as $dir) {
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

        $this->writeFile($this->liveDir . '/plugins/live/live.php', '<?php // live plugin');
        $this->writeFile($this->liveDir . '/themes/live/style.css', '/* live theme */');
        $this->writeFile($this->liveDir . '/uploads/2024/01/photo.txt', 'live upload');
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->root);
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_a_stage_that_cannot_write_a_file_fails_the_restore_and_leaves_the_live_tree_unchanged(): void
    {
        // The part's own `plugins/conflict` file takes the path the next
        // entry needs as its folder.
        $zipPath = $this->buildPart([
            'plugins/new/new.php'        => '<?php // from the snapshot',
            'plugins/conflict'           => 'a file where a folder must go',
            'plugins/conflict/inner.php' => '<?php // cannot be written',
            'themes/new/style.css'       => '/* from the snapshot */',
        ]);
        $this->seedAtStageFiles($zipPath);
        $before = $this->hashTree($this->liveDir);

        $this->assertSame(RestoreRunner::PHASE_FAILED, $this->runner()->run());

        $this->assertSame($before, $this->hashTree($this->liveDir), 'the live wp-content changed although staging failed');

        $sub = $this->rowSubState();
        $this->assertSame(RestoreRunner::PHASE_STAGE_FILES, $sub['failed_in'] ?? null);
        $message = (string) ($sub['last_error'] ?? '');
        $this->assertStringContainsString('the site was not changed', $message);
        $this->assertStringContainsString('plugins/conflict/inner.php', $message);
        $this->assertStringContainsString('could not create its folder (a file is in the way)', $message);
        $this->assertStringContainsString('Free disk space or fix folder permissions, then retry', $message);
        $this->assertArrayNotHasKey('swap_files', $sub, 'no swap may run after a failed stage');
        $this->assertArrayNotHasKey('stage', $sub, 'a failed stage must not record a staging dir for a later swap');

        $this->assertCount(1, $this->siblings('.wpmgr-staging-'), 'staging is left in place for the regular cleanup');
        $this->assertSame([], $this->siblings('.wpmgr-old-'), 'no live tree may be moved aside');
        $this->assertSame(0, $this->fireShutdown(), 'nothing was swapped, so no shutdown rollback may be armed');
    }

    public function test_control_a_part_that_stages_cleanly_reaches_the_swap_and_changes_the_live_tree(): void
    {
        $zipPath = $this->buildPart([
            'plugins/new/new.php'  => '<?php // from the snapshot',
            'themes/new/style.css' => '/* from the snapshot */',
        ]);
        $this->seedAtStageFiles($zipPath);
        $before = $this->hashTree($this->liveDir);

        // No SQL artifact: the run swaps files, then fails in restore_db.
        $this->assertSame(RestoreRunner::PHASE_FAILED, $this->runner()->run());

        $this->assertNotSame($before, $this->hashTree($this->liveDir), 'the rig never reached the swap, so it cannot show a failed stage stops one');
        $this->assertFileExists($this->liveDir . '/plugins/new/new.php');
        $sub = $this->rowSubState();
        $this->assertSame(RestoreRunner::PHASE_RESTORE_DB, $sub['failed_in'] ?? null);
        $this->assertIsArray($sub['swap_files'] ?? null);
    }

    /**
     * @param array<string,string> $files
     */
    private function buildPart(array $files): string
    {
        $zipPath = $this->scratchDir . '/archive.part001.zip';
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
                'tmp_prefix' => 'tmpdddddddd_',
                // `archive.part001.zip` is not a component part name, so the
                // run takes the whole-wp-content swap path.
                'download'   => ['artifact_paths' => ['archive.part001.zip' => $zipPath]],
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

    /**
     * @return list<string> Names in the live tree's parent starting with $prefix.
     */
    private function siblings(string $prefix): array
    {
        $hits = [];
        foreach (scandir($this->root) ?: [] as $name) {
            if (strncmp($name, $prefix, strlen($prefix)) === 0) {
                $hits[] = $name;
            }
        }

        return $hits;
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
