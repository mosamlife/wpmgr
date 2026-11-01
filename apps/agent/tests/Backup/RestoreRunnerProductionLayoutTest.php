<?php
/**
 * GH #1008: the restore runner with its scratch where production keeps it,
 * inside wp-content (`wp-content/wpmgr-agent/restores/<snapshot>-<short>/`,
 * see RestoreCommand::prepareScratchDir()), so the whole-wp-content swap
 * moves the tree that holds the scratch dir and the downloaded parts.
 *
 *   - A clean part: the run swaps, carries on to the database phase, and the
 *     row records the swap_files rollback pointer, as on main.
 *   - A part with a file that cannot be written: the run fails in
 *     stage_files, nothing is swapped, and live wp-content (scratch included)
 *     is byte-identical.
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
use WPMgr\Agent\Backup\RestoreRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;
use ZipArchive;

/**
 * @covers \WPMgr\Agent\Backup\RestoreRunner
 * @covers \WPMgr\Agent\Backup\FilesRestorer
 */
final class RestoreRunnerProductionLayoutTest extends TestCase
{
    private const SNAPSHOT_ID = '50085008-5008-4008-8008-500850085008';
    private const RESTORE_ID  = '60086008-6008-4008-8008-600860086008';

    private string $root       = '';
    private string $liveDir    = '';
    private string $scratchDir = '';
    private string $wpRootDir  = '';

    private FakeRestoreRunnerWpdb $wpdb;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root      = sys_get_temp_dir() . '/wpmgr-1008-prod-' . bin2hex(random_bytes(6));
        $this->wpRootDir = $this->root . '/wordpress';
        $this->liveDir   = $this->wpRootDir . '/wp-content';
        // Where RestoreCommand::prepareScratchDir() puts it.
        $this->scratchDir = $this->liveDir . '/wpmgr-agent/restores/' . self::SNAPSHOT_ID . '-600860086008';
        mkdir($this->scratchDir, 0755, true);

        $this->writeFile($this->liveDir . '/plugins/live/live.php', '<?php // live plugin');
        $this->writeFile($this->liveDir . '/uploads/2023/old.jpg', 'live upload');
        $this->writeFile($this->wpRootDir . '/wp-includes/version.php', '<?php // live core');

        $this->wpdb      = new FakeRestoreRunnerWpdb();
        $GLOBALS['wpdb'] = $this->wpdb;

        // The guard armed before the swap is captured, never run at exit.
        Functions\when('register_shutdown_function')->justReturn(null);
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
        $this->rrmdir($this->root);
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_a_clean_restore_reaches_the_database_phase_and_records_its_swap_pointer(): void
    {
        $this->seed($this->part([
            'plugins/new/new.php'  => '<?php // from the snapshot',
            'themes/new/style.css' => '/* from the snapshot */',
        ]));

        // No SQL artifact: after the swap the run fails in restore_db.
        $this->assertSame(RestoreRunner::PHASE_FAILED, $this->runner()->run());

        $sub = $this->rowSubState();
        $this->assertSame(RestoreRunner::PHASE_RESTORE_DB, $sub['failed_in'] ?? null, 'the run must carry on past the swap and record where it failed');
        $this->assertIsArray($sub['swap_files'] ?? null, 'the swap_files rollback pointer must be recorded');
        $oldDir = (string) ($sub['swap_files']['old_files_dir'] ?? '');
        $this->assertNotSame('', $oldDir);
        $this->assertDirectoryExists($oldDir, 'the pointer names the pre-restore tree');
        $this->assertFileExists($oldDir . '/plugins/live/live.php');
        $this->assertFileExists($this->liveDir . '/plugins/new/new.php', 'the snapshot tree is live');
        $this->assertFileExists($this->scratchDir . '/archive.part001.zip', 'the scratch dir is carried across the swap');
    }

    public function test_a_failing_stage_swaps_nothing_and_leaves_live_unchanged(): void
    {
        $this->seed($this->part([
            'plugins/new/new.php'        => '<?php // from the snapshot',
            'plugins/conflict'           => 'a file where a folder must go',
            'plugins/conflict/inner.php' => '<?php // cannot be written',
        ]));
        $before = $this->hashTree($this->liveDir);

        $this->assertSame(RestoreRunner::PHASE_FAILED, $this->runner()->run());

        $this->assertSame($before, $this->hashTree($this->liveDir), 'live wp-content changed although staging failed');
        $sub = $this->rowSubState();
        $this->assertSame(RestoreRunner::PHASE_STAGE_FILES, $sub['failed_in'] ?? null);
        $this->assertStringContainsString('the site was not changed', (string) ($sub['last_error'] ?? ''));
        $this->assertStringContainsString('plugins/conflict/inner.php', (string) ($sub['last_error'] ?? ''));
        $this->assertArrayNotHasKey('swap_files', $sub);
        $this->assertSame([], glob($this->wpRootDir . '/.wpmgr-old-*') ?: [], 'nothing was moved aside');
    }

    /**
     * @param array<string,string> $files
     */
    private function part(array $files): string
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

    private function seed(string $zipPath): void
    {
        $this->wpdb->rows[self::SNAPSHOT_ID . '|' . self::RESTORE_ID] = [
            'phase'        => RestoreRunner::PHASE_STAGE_FILES,
            'kind'         => 'full',
            'sub_state'    => (string) json_encode([
                'tmp_prefix' => 'tmpgggggggg_',
                // Not a component part name: the whole-wp-content swap path.
                'download'   => ['artifact_paths' => ['archive.part001.zip' => $zipPath]],
            ]),
            'resume_count' => 0,
            'max_resumes'  => 6,
        ];
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
