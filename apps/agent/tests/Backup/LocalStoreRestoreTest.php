<?php
/**
 * GH #982: a restore keeps the Local destination's live store. An older
 * snapshot's copy of that store is never extracted, and the live store is
 * carried across the swap by hard link, so no restore deletes or rolls back
 * the local backups, including the one it is restoring from.
 *
 * The store is wherever the agent resolves it: the uploads folder WordPress
 * reports (a custom UPLOADS constant or upload_path moves it), or wp-content.
 * A backup excludes exactly that place and a restore keeps exactly that
 * place, and a restore that cannot keep all of it stops before the swap.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Backup\FilesArchiver;
use WPMgr\Agent\Backup\FilesRestorer;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

final class LocalStoreRestoreTest extends TestCase
{
    use RestoreFixtureHarness;

    /** The tests that only check what a restore keeps, so a child PHP can run them with link() disabled. */
    private const CONTENT_ONLY_TESTS = 'testWholeRestoreKeepsTheLiveLocalStore|testUploadsRestoreKeepsTheLiveLocalStore|testWholeRestoreKeepsAStoreInACustomUploadsFolder|testWpContentRestoreKeepsAStoreInACustomUploadsFolder';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', sys_get_temp_dir() . '/wpmgr-shared-wp-content');
        }
    }

    protected function tear_down(): void
    {
        $this->tearDownRestoreFixtures();
        Monkey\tearDown();
        parent::tear_down();
    }

    public function testWholeRestoreKeepsTheLiveLocalStore(): void
    {
        [$restorer, $targetDir, $stagingDir, $restoreId] = $this->stagedOverALiveStore('whole');

        $restorer->swap($stagingDir, $targetDir, $restoreId, $this->noopRestoreProgress());

        $this->assertLiveStoreKept($targetDir);
    }

    public function testTheLiveStoreIsCarriedByHardLink(): void
    {
        [$restorer, $targetDir, $stagingDir, $restoreId] = $this->stagedOverALiveStore('link');

        $oldFiles = $restorer->swap($stagingDir, $targetDir, $restoreId, $this->noopRestoreProgress());

        $this->assertSame(fileinode($oldFiles . '/uploads/wpmgr-backups/live-snap/chunks/a.bin'), fileinode($targetDir . '/uploads/wpmgr-backups/live-snap/chunks/a.bin'), 'the store is linked, not copied');
    }

    public function testUploadsRestoreKeepsTheLiveLocalStore(): void
    {
        [$restorer, $targetDir, $stagingDir, $restoreId] = $this->stagedOverALiveStore('uploads');

        $restorer->swapComponents($stagingDir, $targetDir, ['upload', 'wp-content'], $restoreId, $this->noopRestoreProgress());

        $this->assertLiveStoreKept($targetDir);
    }

    // ---- A store in a custom uploads folder (UPLOADS / upload_path) --------

    public function testWholeRestoreKeepsAStoreInACustomUploadsFolder(): void
    {
        [$restorer, $targetDir, $stagingDir, $restoreId] = $this->stagedOverACustomUploadsStore('custom-whole');

        $restorer->swap($stagingDir, $targetDir, $restoreId, $this->noopRestoreProgress());

        $this->assertCustomUploadsStoreKept($targetDir);
    }

    public function testWpContentRestoreKeepsAStoreInACustomUploadsFolder(): void
    {
        [$restorer, $targetDir, $stagingDir, $restoreId] = $this->stagedOverACustomUploadsStore('custom-component');

        // A custom uploads folder is not `uploads/`, so it is part of the wp-content component.
        $restorer->swapComponents($stagingDir, $targetDir, ['wp-content'], $restoreId, $this->noopRestoreProgress());

        $this->assertCustomUploadsStoreKept($targetDir);
    }

    public function testBackupExcludesExactlyTheStoreInACustomUploadsFolder(): void
    {
        $this->useCustomUploadsFolder();
        $fx  = $this->makeRestoreRoot('custom-archive');
        $src = $fx['targetDir'];
        foreach ([
            'media/wpmgr-backups/live-snap/chunks/' . str_repeat('a', 64) . '.bin' => 'ciphertext',
            'media/wpmgr-backups/.htaccess' => 'Require all denied',
            'media/photo.jpg' => 'photo',
            'plugins/acme/wpmgr-backups/readme.txt' => 'a plugin folder that only shares the name',
            'media/2026/wpmgr-backups' => 'a user file that only shares the name',
        ] as $rel => $bytes) {
            $this->writeLiveFile($src, $rel, $bytes);
        }

        $result  = (new FilesArchiver($src))->archive($fx['scratchDir'], [], static function (): void {});
        $entries = $this->zipEntries($result['parts'], $fx['scratchDir']);

        $this->assertSame(['media/2026/wpmgr-backups', 'media/photo.jpg', 'plugins/acme/wpmgr-backups/readme.txt'], $entries, 'only the store itself is left out');
    }

    // ---- Hosts that disable link() -----------------------------------------

    public function testRestoreKeepsTheStoreOnAHostThatDisablesLink(): void
    {
        $agentDir = dirname(__DIR__, 2);
        $probe    = $this->runChild([PHP_BINARY, '-d', 'disable_functions=link', '-r', 'echo function_exists("link") ? "link on" : "link off";'], $agentDir);
        $this->assertSame([0, 'link off'], [$probe['exit'], trim($probe['out'])], 'the child PHP must run with link() disabled');

        $child = $this->runChild([
            PHP_BINARY, '-d', 'disable_functions=link', '-d', 'memory_limit=-1',
            $agentDir . '/vendor/bin/phpunit', '--do-not-cache-result',
            '--filter', '/::(' . self::CONTENT_ONLY_TESTS . ')$/',
            $agentDir . '/tests/Backup/LocalStoreRestoreTest.php',
        ], $agentDir);

        $this->assertSame(0, $child['exit'], "the restore tests fail on a host without link():\n" . $child['out']);
        $this->assertStringContainsString('OK (4 tests', $child['out'], $child['out']);
    }

    // ---- A store that cannot be kept in full ------------------------------

    public function testRestoreStopsBeforeTheSwapWhenAStoreFileCannotBeKept(): void
    {
        [$restorer, $targetDir, $stagingDir, $restoreId] = $this->stagedOverALiveStore('partial');
        // Something already sits where one store file must go, so neither a
        // link nor a copy can put it there.
        mkdir($stagingDir . '/uploads/wpmgr-backups/live-snap/chunks/a.bin', 0700, true);

        $thrown = null;
        try {
            $restorer->swap($stagingDir, $targetDir, $restoreId, $this->noopRestoreProgress());
        } catch (\RuntimeException $e) {
            $thrown = $e->getMessage();
        }

        $this->assertIsString($thrown, 'a restore that cannot keep the whole store must stop');
        $this->assertStringContainsString('uploads/wpmgr-backups/live-snap/chunks/a.bin', $thrown);
        $this->assertSame('LIVE_PHOTO', file_get_contents($targetDir . '/uploads/photo.jpg'), 'it stops before the swap: the live site is untouched');
        $this->assertSame('LIVE_CHUNK', file_get_contents($targetDir . '/uploads/wpmgr-backups/live-snap/chunks/a.bin'));
        $this->assertSame([], glob(dirname($targetDir) . '/.wpmgr-old-files-*'), 'the live tree was never moved aside');
    }

    // ---- helpers -----------------------------------------------------------

    /** @return array{0:FilesRestorer,1:string,2:string,3:string} */
    private function stagedOverALiveStore(string $label): array
    {
        $fx        = $this->makeRestoreRoot('local-store-' . $label);
        $targetDir = $fx['targetDir'];
        // The live store: the uploads-first location and the wp-content one.
        $this->writeLiveFile($targetDir, 'uploads/wpmgr-backups/live-snap/chunks/a.bin', 'LIVE_CHUNK');
        $this->writeLiveFile($targetDir, 'uploads/wpmgr-backups/.htaccess', 'Require all denied');
        $this->writeLiveFile($targetDir, 'wpmgr-backups/legacy-snap/chunks/b.bin', 'LIVE_LEGACY_CHUNK');
        $this->writeLiveFile($targetDir, 'uploads/photo.jpg', 'LIVE_PHOTO');
        // An older snapshot that still archived a copy of the store.
        $zipPath = $fx['scratchDir'] . '/uploads.part001.zip';
        $this->buildFixtureZip($zipPath, [
            'uploads/photo.jpg' => 'RESTORED_PHOTO',
            'uploads/wpmgr-backups/old-snap/chunks/old.bin' => 'OLD_COPY',
            'wpmgr-backups/old-snap/chunks/old.bin' => 'OLD_COPY',
        ]);

        $restorer   = new FilesRestorer();
        $restoreId  = $this->makeRestoreId();
        $stagingDir = $restorer->stage([$zipPath], $targetDir, $restoreId, $this->noopRestoreProgress());
        $this->assertFileDoesNotExist($stagingDir . '/uploads/wpmgr-backups/old-snap/chunks/old.bin', 'an archived copy of the store is never extracted');
        $this->assertFileDoesNotExist($stagingDir . '/wpmgr-backups/old-snap/chunks/old.bin');
        return [$restorer, $targetDir, $stagingDir, $restoreId];
    }

    private function assertLiveStoreKept(string $targetDir): void
    {
        $this->assertSame('RESTORED_PHOTO', file_get_contents($targetDir . '/uploads/photo.jpg'), 'the restore itself happened');
        $this->assertSame('LIVE_CHUNK', file_get_contents($targetDir . '/uploads/wpmgr-backups/live-snap/chunks/a.bin'));
        $this->assertSame('Require all denied', file_get_contents($targetDir . '/uploads/wpmgr-backups/.htaccess'));
        $this->assertSame('LIVE_LEGACY_CHUNK', file_get_contents($targetDir . '/wpmgr-backups/legacy-snap/chunks/b.bin'));
        $this->assertFileDoesNotExist($targetDir . '/uploads/wpmgr-backups/old-snap/chunks/old.bin');
        $this->assertFileDoesNotExist($targetDir . '/wpmgr-backups/old-snap/chunks/old.bin');
    }

    /** The site keeps its uploads in wp-content/media, as `define('UPLOADS', 'wp-content/media')` does. */
    private function useCustomUploadsFolder(): void
    {
        Functions\when('wp_upload_dir')->justReturn(['basedir' => rtrim((string) WP_CONTENT_DIR, '/') . '/media']);
    }

    /** @return array{0:FilesRestorer,1:string,2:string,3:string} */
    private function stagedOverACustomUploadsStore(string $label): array
    {
        $this->useCustomUploadsFolder();
        $fx        = $this->makeRestoreRoot('local-store-' . $label);
        $targetDir = $fx['targetDir'];
        $this->writeLiveFile($targetDir, 'media/wpmgr-backups/live-snap/chunks/a.bin', 'LIVE_CHUNK');
        $this->writeLiveFile($targetDir, 'media/wpmgr-backups/live-snap/manifest.json', '{"snapshot_id":"live-snap"}');
        $this->writeLiveFile($targetDir, 'media/photo.jpg', 'LIVE_PHOTO');
        $zipPath = $fx['scratchDir'] . '/wp-content.part001.zip';
        $this->buildFixtureZip($zipPath, [
            'media/photo.jpg' => 'RESTORED_PHOTO',
            'media/wpmgr-backups/old-snap/chunks/old.bin' => 'OLD_COPY',
            'plugins/acme/wpmgr-backups/readme.txt' => 'RESTORED_PLUGIN_FILE',
        ]);

        $restorer   = new FilesRestorer();
        $restoreId  = $this->makeRestoreId();
        $stagingDir = $restorer->stage([$zipPath], $targetDir, $restoreId, $this->noopRestoreProgress());
        $this->assertFileDoesNotExist($stagingDir . '/media/wpmgr-backups/old-snap/chunks/old.bin', 'an archived copy of the store is never extracted');
        return [$restorer, $targetDir, $stagingDir, $restoreId];
    }

    private function assertCustomUploadsStoreKept(string $targetDir): void
    {
        $this->assertSame('RESTORED_PHOTO', file_get_contents($targetDir . '/media/photo.jpg'), 'the restore itself happened');
        $this->assertSame('LIVE_CHUNK', file_get_contents($targetDir . '/media/wpmgr-backups/live-snap/chunks/a.bin'), 'the live store survives the swap');
        $this->assertSame('{"snapshot_id":"live-snap"}', file_get_contents($targetDir . '/media/wpmgr-backups/live-snap/manifest.json'));
        $this->assertFileDoesNotExist($targetDir . '/media/wpmgr-backups/old-snap/chunks/old.bin');
    }

    /**
     * @param list<string> $parts
     * @return list<string>
     */
    private function zipEntries(array $parts, string $dir): array
    {
        $entries = [];
        foreach ($parts as $part) {
            $zip = new \ZipArchive();
            $this->assertTrue($zip->open($dir . '/' . $part) === true, 'unreadable part ' . $part);
            for ($i = 0; $i < $zip->numFiles; $i++) {
                $entries[] = (string) $zip->getNameIndex($i);
            }
            $zip->close();
        }
        sort($entries);
        return $entries;
    }

    /**
     * Run a child process without a shell. A child that cannot start fails
     * the test: it is never a skip.
     *
     * @param list<string> $cmd
     * @return array{exit:int,out:string}
     */
    private function runChild(array $cmd, string $cwd): array
    {
        $this->assertTrue(function_exists('proc_open'), 'proc_open() is needed to run the child PHP');
        $process = proc_open($cmd, [1 => ['pipe', 'w'], 2 => ['redirect', 1]], $pipes, $cwd);
        $this->assertIsResource($process, 'the child PHP did not start: ' . implode(' ', $cmd));
        $out = (string) stream_get_contents($pipes[1]);
        fclose($pipes[1]);
        return ['exit' => proc_close($process), 'out' => $out];
    }
}
