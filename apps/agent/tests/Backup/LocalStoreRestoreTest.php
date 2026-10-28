<?php
/**
 * GH #982: a restore keeps the Local destination's live store. An older
 * snapshot's copy of that store is never extracted, and the live store is
 * carried across the swap by hard link, so no restore deletes or rolls back
 * the local backups, including the one it is restoring from.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use WPMgr\Agent\Backup\FilesRestorer;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

final class LocalStoreRestoreTest extends TestCase
{
    use RestoreFixtureHarness;

    protected function tear_down(): void
    {
        $this->tearDownRestoreFixtures();
        parent::tear_down();
    }

    public function testWholeRestoreKeepsTheLiveLocalStore(): void
    {
        [$restorer, $targetDir, $stagingDir, $restoreId] = $this->stagedOverALiveStore('whole');

        $oldFiles = $restorer->swap($stagingDir, $targetDir, $restoreId, $this->noopRestoreProgress());

        $this->assertLiveStoreKept($targetDir);
        $this->assertSame(fileinode($oldFiles . '/uploads/wpmgr-backups/live-snap/chunks/a.bin'), fileinode($targetDir . '/uploads/wpmgr-backups/live-snap/chunks/a.bin'), 'the store is linked, not copied');
    }

    public function testUploadsRestoreKeepsTheLiveLocalStore(): void
    {
        [$restorer, $targetDir, $stagingDir, $restoreId] = $this->stagedOverALiveStore('uploads');

        $restorer->swapComponents($stagingDir, $targetDir, ['upload', 'wp-content'], $restoreId, $this->noopRestoreProgress());

        $this->assertLiveStoreKept($targetDir);
    }

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
}
