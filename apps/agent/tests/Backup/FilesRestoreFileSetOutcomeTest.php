<?php
/**
 * P0 outcome test (GH #170 Wave 2 / GH #147 at the OUTCOME level): drives the
 * REAL `FilesRestorer::stage()` + `swap()` end to end against a real backup
 * archive and asserts the PROMOTED file set is correct — not just that the
 * `isExcluded()` classifier answers correctly in isolation (that's already
 * covered by `FilesRestorerExcludeAnchoringTest`, driven via reflection).
 *
 * The gap this test closes: nothing before it ever built a real zip
 * containing a nested `*db.php` plugin file, ran it through extraction AND
 * the atomic directory swap, and asserted the file actually landed byte-for-
 * byte in the promoted tree. A stub restorer — or a regression back to the
 * pre-#147 unanchored substring exclude — would pass every existing test in
 * this suite while silently dropping the nested file at the one point that
 * actually matters: what's on disk after `swap()` returns.
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
final class FilesRestoreFileSetOutcomeTest extends TestCase
{
    use RestoreFixtureHarness;

    protected function tear_down(): void
    {
        $this->tearDownRestoreFixtures();
        parent::tear_down();
    }

    /**
     * GH #147 outcome-level regression: stage a real archive containing a
     * nested `*db.php` plugin file (Rank Math's exact reported shape), a
     * couple of ordinary plugin/theme files, and an archive-side wp-content-
     * ROOT `db.php` drop-in. Drive the REAL `stage()` + `swap()` to
     * completion and assert the promoted tree:
     *
     *   - the nested class-db.php survives — this is exactly the file the
     *     pre-#147 unanchored substring exclude silently dropped at extract
     *     time, before a single byte reached staging.
     *   - ordinary plugin/theme files survive.
     *   - the wp-content-root db.php in the FINAL tree is the LIVE copy
     *     (seeded before the restore), never the archive's — proving the
     *     anchored-exclude rule actually kept the archive's root db.php out
     *     of staging (asserted directly) and that the live drop-in survives
     *     the swap (the preserve-from-live carry-forward).
     */
    public function test_stage_and_swap_reconstructs_correct_file_set(): void
    {
        $fx        = $this->makeRestoreRoot('fileset');
        $targetDir = $fx['targetDir'];

        // Seed the LIVE tree with its own root db.php drop-in — the archive
        // must never clobber this with a different root db.php of its own.
        $this->writeLiveFile($targetDir, 'db.php', 'LIVE_DB_PHP_DROPIN_CONTENT');

        $zipPath = $fx['scratchDir'] . '/wp-content.part001.zip';
        $this->buildFixtureZip($zipPath, [
            // GH #147: nested *db.php plugin file — MUST survive intact.
            'plugins/seo-by-rank-math/includes/modules/redirections/class-db.php'
                => '<?php // RANK_MATH_REDIRECTIONS_DB_CLASS',
            // Ordinary plugin/theme files — the everyday happy path.
            'plugins/my-plugin/main.php' => '<?php // MY_PLUGIN_MAIN',
            'themes/my-theme/style.css'  => '/* MY_THEME_STYLE */',
            // A root db.php IN THE ARCHIVE — must never land; the live copy
            // seeded above must win instead.
            'db.php' => 'ARCHIVE_DB_PHP_SHOULD_NEVER_LAND',
        ]);

        $restorer  = new FilesRestorer();
        $restoreId = $this->makeRestoreId();

        $stagingDir = $restorer->stage([$zipPath], $targetDir, $restoreId, $this->noopRestoreProgress());

        // Non-vacuous mid-point check: the archive's root db.php must never
        // even reach staging — isExcluded() drops it before extraction, not
        // merely at swap time.
        $this->assertFileDoesNotExist(
            $stagingDir . '/db.php',
            'the archive root db.php must be excluded at STAGE time'
        );
        $this->assertFileExists(
            $stagingDir . '/plugins/seo-by-rank-math/includes/modules/redirections/class-db.php',
            'the NESTED class-db.php must reach staging — this is the exact GH #147 file'
        );

        $restorer->swap($stagingDir, $targetDir, $restoreId, $this->noopRestoreProgress());

        // --- The nested db.php-named plugin file survived the FULL pipeline. ---
        $nestedPath = $targetDir . '/plugins/seo-by-rank-math/includes/modules/redirections/class-db.php';
        $this->assertFileExists($nestedPath);
        $this->assertSame('<?php // RANK_MATH_REDIRECTIONS_DB_CLASS', file_get_contents($nestedPath));

        // --- Ordinary plugin/theme files survived. ---
        $this->assertSame(
            '<?php // MY_PLUGIN_MAIN',
            file_get_contents($targetDir . '/plugins/my-plugin/main.php')
        );
        $this->assertSame(
            '/* MY_THEME_STYLE */',
            file_get_contents($targetDir . '/themes/my-theme/style.css')
        );

        // --- The root db.php in the final tree is the LIVE copy, never the
        //     archive's — anchored-exclude kept the archive's copy out of
        //     staging, and preserve-from-live carried the live copy forward.
        $this->assertSame(
            'LIVE_DB_PHP_DROPIN_CONTENT',
            file_get_contents($targetDir . '/db.php'),
            'the wp-content-root db.php drop-in must be the preserved LIVE copy, never overwritten by an archived snapshot'
        );
    }
}
