<?php
/**
 * P0 outcome test (GH #170 Wave 2, Track 5 selective restore): drives the
 * REAL `FilesRestorer::stage()` + `swapComponents(['plugin'])` end to end and
 * asserts ONLY the selected component's live subdir is replaced — the other
 * components must be left byte-identical to their pre-restore state, even
 * though the SAME archive/staging tree carries payload for all of them (the
 * CP's `selectEntries` filtering is what normally narrows the archive to just
 * the requested component; here we stage every component on purpose to prove
 * `swapComponents()` itself — not the upstream filtering — is what enforces
 * the selection boundary).
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use WPMgr\Agent\Backup\FilesRestorer;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\FilesRestorer
 */
final class FilesRestoreSwapComponentsOutcomeTest extends TestCase
{
    use RestoreFixtureHarness;

    protected function tear_down(): void
    {
        $this->tearDownRestoreFixtures();
        parent::tear_down();
    }

    public function test_swap_components_plugin_only_leaves_themes_and_uploads_untouched(): void
    {
        $fx        = $this->makeRestoreRoot('components');
        $targetDir = $fx['targetDir'];

        // Pre-existing LIVE state across all three components.
        $this->writeLiveFile($targetDir, 'plugins/old-plugin/main.php', 'OLD_PLUGIN_LIVE');
        $this->writeLiveFile($targetDir, 'themes/live-theme/style.css', 'LIVE_THEME_STYLE');
        $this->writeLiveFile($targetDir, 'uploads/2026/07/photo.jpg', 'LIVE_UPLOAD_BYTES');

        // The archive carries payload for ALL THREE components — proving the
        // selection boundary is enforced by swapComponents() itself, not by
        // the archive happening to only contain the selected component.
        $zipPath = $fx['scratchDir'] . '/wp-content.part001.zip';
        $this->buildFixtureZip($zipPath, [
            'plugins/new-plugin/main.php'  => '<?php // NEW_PLUGIN_FROM_ARCHIVE',
            'themes/new-theme/style.css'   => '/* NEW_THEME_FROM_ARCHIVE */',
            'uploads/2026/08/new.jpg'      => 'NEW_UPLOAD_FROM_ARCHIVE',
        ]);

        $restorer  = new FilesRestorer();
        $restoreId = $this->makeRestoreId();

        $stagingDir = $restorer->stage([$zipPath], $targetDir, $restoreId, $this->noopRestoreProgress());

        // Sanity: staging really does carry all three components — otherwise
        // "themes/uploads untouched" would be a vacuous pass (nothing to swap).
        $this->assertFileExists($stagingDir . '/plugins/new-plugin/main.php');
        $this->assertFileExists($stagingDir . '/themes/new-theme/style.css');
        $this->assertFileExists($stagingDir . '/uploads/2026/08/new.jpg');

        $restorer->swapComponents($stagingDir, $targetDir, ['plugin'], $restoreId, $this->noopRestoreProgress());

        // --- plugins WAS swapped: the archive's plugin content is live, the
        //     old plugin (not present in staging's plugins/ subdir) is gone —
        //     swapComponents replaces the whole subdir, it does not merge. ---
        $this->assertFileExists($targetDir . '/plugins/new-plugin/main.php');
        $this->assertSame(
            '<?php // NEW_PLUGIN_FROM_ARCHIVE',
            file_get_contents($targetDir . '/plugins/new-plugin/main.php')
        );
        $this->assertFileDoesNotExist(
            $targetDir . '/plugins/old-plugin/main.php',
            'the plugins subdir is REPLACED wholesale by the selected component swap'
        );

        // --- themes was NOT selected: byte-identical to pre-swap, and the
        //     archive's new-theme must NOT have been promoted. ---
        $this->assertSame(
            'LIVE_THEME_STYLE',
            file_get_contents($targetDir . '/themes/live-theme/style.css'),
            'themes must be untouched when only "plugin" is selected'
        );
        $this->assertFileDoesNotExist(
            $targetDir . '/themes/new-theme/style.css',
            'the archive theme payload must NOT be promoted when "theme" was not a selected component'
        );

        // --- uploads was NOT selected: byte-identical to pre-swap. ---
        $this->assertSame(
            'LIVE_UPLOAD_BYTES',
            file_get_contents($targetDir . '/uploads/2026/07/photo.jpg'),
            'uploads must be untouched when only "plugin" is selected'
        );
        $this->assertFileDoesNotExist(
            $targetDir . '/uploads/2026/08/new.jpg',
            'the archive upload payload must NOT be promoted when "upload" was not a selected component'
        );
    }
}
