<?php
/**
 * P0 outcome test (GH #170 Wave 2 / GH #147, the OTHER half): drives the REAL
 * `FilesRestorer::swap()` end to end and asserts its preserve-from-live
 * carry-forward actually keeps the running agent plugin + host-state
 * drop-ins on disk when the restored archive never contained them at all —
 * the ordinary case for a backup taken before the agent was installed, or
 * from a host running a different object-cache backend.
 *
 * Pre-#147, these paths were excluded from staging (so a snapshot couldn't
 * clobber them with stale content) but were NOT pulled forward from live —
 * so a restore silently DELETED the running drop-in/plugin with no
 * replacement the moment the live target dir was renamed aside and the
 * (incomplete) staging tree was promoted in its place. This test proves the
 * fix end to end: build staging via the real `stage()` from an archive that
 * OMITS the preserved paths, then run the real `swap()` and assert the live
 * copies are still present, byte-identical, in the promoted tree.
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
final class FilesRestorePreserveFromLiveOutcomeTest extends TestCase
{
    use RestoreFixtureHarness;

    protected function tear_down(): void
    {
        $this->tearDownRestoreFixtures();
        parent::tear_down();
    }

    public function test_swap_preserves_live_dropins_and_running_agent_when_archive_omits_them(): void
    {
        $fx        = $this->makeRestoreRoot('preserve');
        $targetDir = $fx['targetDir'];

        // Seed the LIVE tree: host-state drop-ins + the running agent plugin
        // (a multi-file tree, not just its main file, to prove the whole
        // plugin directory is carried forward, not merely its entry file).
        $this->writeLiveFile($targetDir, 'db.php', 'LIVE_DB_PHP');
        $this->writeLiveFile($targetDir, 'object-cache.php', 'LIVE_OBJECT_CACHE');
        $this->writeLiveFile($targetDir, 'advanced-cache.php', 'LIVE_ADVANCED_CACHE');
        $this->writeLiveFile($targetDir, 'plugins/wpmgr-agent/wpmgr-agent.php', 'LIVE_AGENT_MAIN_PHP');
        $this->writeLiveFile($targetDir, 'plugins/wpmgr-agent/includes/class-foo.php', 'LIVE_AGENT_INCLUDE');

        // Archive OMITS all of the above entirely (a genuinely older snapshot
        // predating the agent install / a different host's cache config) —
        // only unrelated plugin content is present.
        $zipPath = $fx['scratchDir'] . '/wp-content.part001.zip';
        $this->buildFixtureZip($zipPath, [
            'plugins/other-plugin/main.php' => '<?php // OTHER_PLUGIN',
        ]);

        $restorer  = new FilesRestorer();
        $restoreId = $this->makeRestoreId();

        $stagingDir = $restorer->stage([$zipPath], $targetDir, $restoreId, $this->noopRestoreProgress());

        // Sanity: none of the preserved paths were staged FROM THE ARCHIVE
        // (it never contained them) — proves what we assert after swap()
        // really is the live carry-forward, not an artifact of the archive.
        $this->assertFileDoesNotExist($stagingDir . '/db.php');
        $this->assertFileDoesNotExist($stagingDir . '/object-cache.php');
        $this->assertFileDoesNotExist($stagingDir . '/advanced-cache.php');
        $this->assertFileDoesNotExist($stagingDir . '/plugins/wpmgr-agent/wpmgr-agent.php');

        $restorer->swap($stagingDir, $targetDir, $restoreId, $this->noopRestoreProgress());

        $this->assertSame('LIVE_DB_PHP', file_get_contents($targetDir . '/db.php'));
        $this->assertSame('LIVE_OBJECT_CACHE', file_get_contents($targetDir . '/object-cache.php'));
        $this->assertSame('LIVE_ADVANCED_CACHE', file_get_contents($targetDir . '/advanced-cache.php'));
        $this->assertSame(
            'LIVE_AGENT_MAIN_PHP',
            file_get_contents($targetDir . '/plugins/wpmgr-agent/wpmgr-agent.php'),
            'the running agent plugin main file must survive a restore whose archive never contained it'
        );
        $this->assertSame(
            'LIVE_AGENT_INCLUDE',
            file_get_contents($targetDir . '/plugins/wpmgr-agent/includes/class-foo.php'),
            'the whole agent plugin directory must be carried forward, not just its main file'
        );

        // And the archive's own content also landed — the carry-forward is
        // additive, not a wholesale live-tree passthrough that would mask a
        // real files restore.
        $this->assertSame(
            '<?php // OTHER_PLUGIN',
            file_get_contents($targetDir . '/plugins/other-plugin/main.php')
        );
    }
}
