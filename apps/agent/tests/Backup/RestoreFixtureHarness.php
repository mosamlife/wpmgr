<?php
/**
 * RestoreFixtureHarness — shared fixture-building trait for the forward
 * restore OUTCOME tests (GH #170 Wave 2 backfill: the restore ROLLBACK/health
 * path is well covered by RestoreRollbackTest/RestoreHealthPassTest/#146, but
 * nothing previously drove the FORWARD path — FilesRestorer::stage()+swap()/
 * swapComponents() — end to end against a real archive and asserted on the
 * actual promoted file set).
 *
 * One rig serves every file-side outcome test: it creates a temp WP-content-
 * like tree (a live `wp-content` dir FilesRestorer::stage()/swap() can
 * operate on) plus a scratch dir for building real backup zip parts from a
 * fixture file set, then hands back plain paths so each test drives the REAL
 * `FilesRestorer::stage()`/`swap()`/`swapComponents()` itself — never a stub.
 *
 * Conventions are lifted from the existing restore tests
 * (RestoreRollbackTest, RestoreHealthPassTest, FilesRestorerExcludeAnchoringTest):
 * plain `mkdir()`/`file_put_contents()` fixture construction, a recursive
 * `rrmdir()`-style teardown helper, and `sys_get_temp_dir()`-rooted per-test
 * directories so parallel test runs never collide.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use ZipArchive;

trait RestoreFixtureHarness
{
    /**
     * Temp roots created by makeRestoreRoot(), swept by
     * tearDownRestoreFixtures() — call that from the consuming test's
     * tear_down().
     *
     * @var list<string>
     */
    private array $restoreFixtureRoots = [];

    /**
     * Create a fresh temp root with a `wp-content` (live target) dir already
     * present — both `FilesRestorer::stage()` and `::swap()` require
     * `is_dir($targetDir)` up front, exactly like a real WordPress install.
     *
     * @return array{root:string,targetDir:string,scratchDir:string}
     */
    private function makeRestoreRoot(string $label): array
    {
        $root       = sys_get_temp_dir() . '/wpmgr-restore-' . $label . '-' . bin2hex(random_bytes(6));
        $targetDir  = $root . '/wp-content';
        $scratchDir = $root . '/scratch';
        mkdir($targetDir, 0755, true);
        mkdir($scratchDir, 0755, true);
        $this->restoreFixtureRoots[] = $root;

        return ['root' => $root, 'targetDir' => $targetDir, 'scratchDir' => $scratchDir];
    }

    /**
     * Build a real zip at $zipPath containing $files (wp-content-relative
     * entry name => raw contents). Mirrors the shape of the part-zips
     * `FilesRestorer::stage()` consumes in production (entry names are
     * wp-content-relative paths like `plugins/foo/foo.php`).
     *
     * @param array<string,string> $files
     */
    private function buildFixtureZip(string $zipPath, array $files): void
    {
        $zip = new ZipArchive();
        $rc  = $zip->open($zipPath, ZipArchive::CREATE | ZipArchive::OVERWRITE);
        if ($rc !== true) {
            throw new \RuntimeException("RestoreFixtureHarness: cannot create fixture zip {$zipPath} (rc={$rc})");
        }
        foreach ($files as $rel => $contents) {
            $zip->addFromString($rel, $contents);
        }
        $zip->close();
    }

    /**
     * Write a file directly into a live tree — used to seed pre-restore
     * state (the "currently running site") BEFORE stage()/swap() runs, so a
     * test can assert what survives a restore whose archive never contained
     * that path at all (the preserve-from-live carry-forward contract).
     */
    private function writeLiveFile(string $baseDir, string $rel, string $contents): void
    {
        $path = $baseDir . '/' . ltrim($rel, '/');
        $dir  = dirname($path);
        if (!is_dir($dir)) {
            mkdir($dir, 0755, true);
        }
        file_put_contents($path, $contents);
    }

    /** No-op progress callback for stage()/swap()/swapComponents(). */
    private function noopRestoreProgress(): callable
    {
        return static function (string $phase, array $detail): void {
        };
    }

    /** Real UUID-shaped restore id — FilesRestorer::shortId() only cares about hex chars. */
    private function makeRestoreId(): string
    {
        return sprintf(
            '%08x-%04x-%04x-%04x-%012x',
            random_int(0, 0xffffffff),
            random_int(0, 0xffff),
            random_int(0, 0xffff),
            random_int(0, 0xffff),
            random_int(0, 0xffffffffffff)
        );
    }

    private function rrmdirFixture(string $dir): void
    {
        if (!is_dir($dir)) {
            return;
        }
        $items = @scandir($dir);
        if ($items === false) {
            return;
        }
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $path = $dir . '/' . $item;
            if (is_link($path) || is_file($path)) {
                @unlink($path);
            } elseif (is_dir($path)) {
                $this->rrmdirFixture($path);
            }
        }
        @rmdir($dir);
    }

    /** Call from the consuming test's tear_down(). */
    private function tearDownRestoreFixtures(): void
    {
        foreach ($this->restoreFixtureRoots as $root) {
            $this->rrmdirFixture($root);
        }
        $this->restoreFixtureRoots = [];
    }
}
