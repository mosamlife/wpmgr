<?php
/**
 * GH #1008: FilesRestorer::stage() stops at the first entry it means to
 * restore but cannot write into staging, and leaves nothing behind for a
 * swap to promote.
 *
 *   - An entry whose folder cannot be created (a regular file occupies the
 *     parent path in staging): stage() throws, naming the entry and "could
 *     not create its folder".
 *   - An entry that cannot be unpacked (its folder is read-only, or a
 *     folder sits where the file must be written): stage() throws, naming
 *     the entry and "could not unpack it".
 *   - Entries stage() deliberately leaves out (the exclude lists, unsafe
 *     entry paths, a parent that resolves outside staging) are still skipped
 *     without a throw, and the rest of the archive is staged.
 *
 * Every case drives the real stage() against a real zip.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionMethod;
use WPMgr\Agent\Backup\FilesRestorer;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\FilesRestorer
 */
final class FilesRestorerStageFailureTest extends TestCase
{
    use RestoreFixtureHarness;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        // WordPress core's wp_mkdir_p() answers false for a path a file
        // already occupies and silences its own mkdir() call, leaving the
        // reason in error_get_last(). The shared stub calls mkdir() bare.
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
        $this->tearDownRestoreFixtures();
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_stage_stops_when_an_entry_folder_cannot_be_created(): void
    {
        $fx        = $this->makeRestoreRoot('nofolder');
        $restoreId = $this->makeRestoreId();
        $staging   = $this->stagingDirFor($fx['targetDir'], $restoreId);
        $this->writeLiveFile($fx['targetDir'], 'plugins/live/live.php', 'live');

        // A regular file occupies the parent path the entry needs.
        mkdir($staging, 0700, true);
        file_put_contents($staging . '/plugins', 'not a folder');

        $zipPath = $fx['scratchDir'] . '/part001.zip';
        $this->buildFixtureZip($zipPath, [
            'themes/ok/style.css'         => '/* ok */',
            'plugins/blocked/blocked.php' => '<?php // blocked',
            'themes/after/style.css'      => '/* never reached */',
        ]);

        $message = $this->stageExpectingFailure($zipPath, $fx['targetDir'], $restoreId);

        $this->assertStringContainsString('plugins/blocked/blocked.php', $message);
        $this->assertStringContainsString('could not create its folder', $message);
        $this->assertStringContainsString('the site was not changed', $message);
        $this->assertStringContainsString('Free disk space or fix folder permissions, then retry', $message);
        $this->assertDirectoryDoesNotExist($staging, 'a failed stage must not leave its partial tree for a swap to promote');
        $this->assertSame('live', file_get_contents($fx['targetDir'] . '/plugins/live/live.php'));
    }

    public function test_stage_stops_when_an_entry_cannot_be_unpacked_into_a_read_only_folder(): void
    {
        $fx        = $this->makeRestoreRoot('readonly');
        $restoreId = $this->makeRestoreId();
        $staging   = $this->stagingDirFor($fx['targetDir'], $restoreId);

        // The folder exists (as on a resumed run) but cannot be written to.
        $locked = $staging . '/plugins/locked';
        mkdir($locked, 0755, true);

        $zipPath = $fx['scratchDir'] . '/part001.zip';
        $this->buildFixtureZip($zipPath, [
            'themes/ok/style.css'       => '/* ok */',
            'plugins/locked/locked.php' => '<?php // locked',
        ]);

        chmod($locked, 0500);
        try {
            clearstatcache();
            if (is_writable($locked)) {
                $this->markTestSkipped('a read-only folder does not stop writes for this user (running as root)');
            }
            $message = $this->stageExpectingFailure($zipPath, $fx['targetDir'], $restoreId);
        } finally {
            if (is_dir($locked)) {
                chmod($locked, 0755);
            }
        }

        // The cause in parentheses is not asserted: under the test harness
        // every file open goes through Patchwork's stream wrapper, which
        // replaces the operating system's "Permission denied".
        $this->assertStringContainsString('plugins/locked/locked.php', $message);
        $this->assertStringContainsString('could not unpack it', $message);
        $this->assertStringContainsString('the site was not changed', $message);
        $this->assertDirectoryDoesNotExist($staging);
    }

    public function test_stage_stops_when_a_folder_sits_where_the_file_must_be_written(): void
    {
        $fx        = $this->makeRestoreRoot('isdir');
        $restoreId = $this->makeRestoreId();
        $staging   = $this->stagingDirFor($fx['targetDir'], $restoreId);

        // Fails for every user, root included: a file cannot be written over
        // a folder.
        mkdir($staging . '/plugins/foo/foo.php', 0755, true);

        $zipPath = $fx['scratchDir'] . '/part001.zip';
        $this->buildFixtureZip($zipPath, [
            'plugins/foo/foo.php' => '<?php // foo',
        ]);

        $message = $this->stageExpectingFailure($zipPath, $fx['targetDir'], $restoreId);

        $this->assertStringContainsString('plugins/foo/foo.php', $message);
        $this->assertStringContainsString('could not unpack it', $message);
        $this->assertDirectoryDoesNotExist($staging);
    }

    public function test_a_long_entry_name_keeps_the_message_within_what_a_failed_restore_records(): void
    {
        $fx        = $this->makeRestoreRoot('longname');
        $restoreId = $this->makeRestoreId();

        $deep    = 'plugins/conflict/' . str_repeat('nested-folder/', 20) . 'the-file-that-failed.php';
        $zipPath = $fx['scratchDir'] . '/part001.zip';
        $this->buildFixtureZip($zipPath, [
            'plugins/conflict' => 'a file where a folder must go',
            $deep              => '<?php // deep',
        ]);

        $message = $this->stageExpectingFailure($zipPath, $fx['targetDir'], $restoreId);

        $this->assertLessThanOrEqual(240, strlen($message), $message);
        $this->assertStringContainsString('the-file-that-failed.php', $message);
        $this->assertStringContainsString('could not create its folder', $message);
        $this->assertStringContainsString('Free disk space or fix folder permissions, then retry.', $message);
    }

    public function test_entries_stage_leaves_out_are_still_skipped_and_the_rest_is_staged(): void
    {
        $fx        = $this->makeRestoreRoot('skips');
        $restoreId = $this->makeRestoreId();
        $staging   = $this->stagingDirFor($fx['targetDir'], $restoreId);

        // A folder outside staging, reached through a link planted where an
        // entry's parent would be: the containment check refuses it.
        $outside = $fx['root'] . '/outside';
        mkdir($outside, 0755, true);
        mkdir($staging . '/plugins', 0700, true);
        symlink($outside, $staging . '/plugins/linked');

        $zipPath = $fx['scratchDir'] . '/part001.zip';
        $this->buildFixtureZip($zipPath, [
            'plugins/ok/ok.php'            => '<?php // ok',
            'db.php'                       => '<?php // archive drop-in',
            'wpmgr-agent/secret.key'       => 'secret',
            'plugins/wpmgr-agent/agent.php' => '<?php // agent',
            '../escape.php'                => '<?php // escape',
            'plugins/../../escape2.php'    => '<?php // escape',
            'plugins/linked/planted.php'   => '<?php // planted',
            'themes/t/style.css'           => '/* theme */',
            'uploads/2024/01/a.txt'        => 'upload',
        ]);

        $returned = (new FilesRestorer())->stage([$zipPath], $fx['targetDir'], $restoreId, $this->noopRestoreProgress());

        $this->assertSame($staging, $returned);
        $this->assertSame('<?php // ok', file_get_contents($staging . '/plugins/ok/ok.php'));
        $this->assertSame('/* theme */', file_get_contents($staging . '/themes/t/style.css'));
        $this->assertSame('upload', file_get_contents($staging . '/uploads/2024/01/a.txt'));

        $this->assertFileDoesNotExist($staging . '/db.php');
        $this->assertDirectoryDoesNotExist($staging . '/wpmgr-agent');
        $this->assertDirectoryDoesNotExist($staging . '/plugins/wpmgr-agent');
        $this->assertFileDoesNotExist($fx['root'] . '/escape.php');
        $this->assertFileDoesNotExist($fx['root'] . '/escape2.php');
        $this->assertFileDoesNotExist($fx['targetDir'] . '/escape.php');
        $this->assertFileDoesNotExist($outside . '/planted.php', 'an entry whose parent resolves outside staging must be skipped, never written');
    }

    /**
     * Run stage() and return the message of the RuntimeException it must
     * throw; fail when it returns instead.
     */
    private function stageExpectingFailure(string $zipPath, string $targetDir, string $restoreId): string
    {
        try {
            $dir = (new FilesRestorer())->stage([$zipPath], $targetDir, $restoreId, $this->noopRestoreProgress());
        } catch (\RuntimeException $e) {
            return $e->getMessage();
        }
        $this->fail('stage() returned ' . $dir . ' with an entry it could not write; it must throw before any swap');
    }

    /** The staging dir stage() uses for this restore id. */
    private function stagingDirFor(string $targetDir, string $restoreId): string
    {
        $short = (string) (new ReflectionMethod(FilesRestorer::class, 'shortId'))->invoke(null, $restoreId);
        return dirname($targetDir) . '/.wpmgr-staging-' . $short;
    }
}
