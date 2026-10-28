<?php
/**
 * GH #954 (site B): a backup whose worker the host ends must continue its
 * archive after the last closed part, never hold the parts of two passes in
 * its scratch dir, and never archive the agent's own scratch, lock or temp
 * files.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Backup\FilesArchiver;
use WPMgr\Agent\Backup\TaskRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

require_once __DIR__ . '/FakeBackupTasksWpdb.php';

/** Stands in for a worker the host ends right after a checkpoint was saved. */
final class ArchiveWorkerEnded extends \RuntimeException
{
}

/**
 * @covers \WPMgr\Agent\Backup\FilesArchiver
 * @covers \WPMgr\Agent\Backup\TaskRunner
 */
final class ArchiveResumeTest extends TestCase
{
    private const SNAPSHOT = 'bbbbbbbb-cccc-4ddd-8eee-ffffffffffff';

    /** Part files and libzip temporary outputs of the four file components. */
    private const PART_FILE = '/^(?:plugins|themes|uploads|wp-content)\.(?:g\d+\.)?part\d+\.zip(?:\..+)?$/';

    private string $root = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('esc_html')->returnArg();
        $this->root = sys_get_temp_dir() . DIRECTORY_SEPARATOR . 'wpmgr-archive-resume-' . bin2hex(random_bytes(6));
        mkdir($this->root . '/src', 0700, true);
        mkdir($this->root . '/out', 0700, true);
        $this->root = (string) realpath($this->root);
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        $this->removeTree($this->root);
        parent::tear_down();
    }

    public function testInterruptedArchiveResumesAfterItsLastClosedPartWithOnePassOfParts(): void
    {
        $order = $this->interleavedTree();
        $cacheBefore = (string) file_get_contents($this->root . '/out/paths.cache');
        $listBefore  = (string) file_get_contents($this->root . '/out/files.list');
        $saved = null;
        $savedHashes = [];
        try {
            $this->archiver()->archive($this->root . '/out', ['total_files' => count($order)], static function (): void {}, null,
                function (array $cursor) use (&$saved, &$savedHashes): void {
                    $this->assertDiskHoldsOnly($cursor['parts_completed']);
                    if (count($cursor['parts_completed']) === 2) {
                        $saved = $cursor;
                        foreach ($cursor['parts_completed'] as $part) { $savedHashes[$part] = sha1_file($this->root . '/out/' . $part); }
                        throw new ArchiveWorkerEnded('host ended the worker');
                    }
                });
            self::fail('the first worker must be ended after its second closed part');
        } catch (ArchiveWorkerEnded $e) {
            // The host ended the worker.
        }
        self::assertIsArray($saved);
        // Three components still had open parts when the worker ended.
        self::assertSame(0, $saved['file_index']);
        self::assertSame(['plugins' => 2, 'themes' => 5, 'uploads' => 10, 'wp-content' => 0], array_map(static fn (array $c): int => $c['resume_from'], $saved['components']));

        $checkpoints = [];
        $result = $this->archiver()->archive($this->root . '/out', $saved, static function (): void {}, null,
            function (array $cursor) use (&$checkpoints): void {
                $this->assertDiskHoldsOnly($cursor['parts_completed']);
                $checkpoints[] = $cursor;
            });

        self::assertTrue($result['done']);
        self::assertSame(array_keys($savedHashes), array_slice($result['parts'], 0, 2), 'the saved parts stay first and are never rebuilt');
        foreach ($savedHashes as $part => $hash) { self::assertSame($hash, sha1_file($this->root . '/out/' . $part)); }
        $entries = $this->entries($result['parts'], $this->root . '/out');
        $expected = $order;
        sort($expected);
        self::assertSame($expected, $entries, 'every file is archived exactly once across one pass of parts');
        $this->assertDiskHoldsOnly($result['parts']);
        self::assertSame(count($order), $result['files_total']);
        self::assertSame($cacheBefore, file_get_contents($this->root . '/out/paths.cache'));
        self::assertSame($listBefore, file_get_contents($this->root . '/out/files.list'));
        self::assertSame(array_map([FilesArchiver::class, 'componentKindFromPartName'], $result['parts']), $result['part_kinds']);
        self::assertSame(array_sum(array_map(fn (string $p): int => (int) filesize($this->root . '/out/' . $p), $result['parts'])), $result['bytes_written']);
        self::assertTrue(end($checkpoints)['complete']);
        fwrite(STDOUT, "\nresumed parts=" . json_encode($result['parts']) . ' checkpoints=' . count($checkpoints) . "\n");
    }

    public function testPartialAndAbandonedPartsAreRemovedBeforeTheResumedWorkerArchives(): void
    {
        $order = $this->interleavedTree();
        $saved = $this->interruptAfter(1, ['total_files' => count($order)]);
        // What an ended worker leaves: the parts libzip wrote for its open
        // components, a libzip temporary output, a truncated part at the next
        // part's name, and a part from an earlier abandoned pass.
        $leftovers = ['uploads.g000.part002.zip', 'uploads.g000.part002.zip.k3Xy9Q', 'uploads.g000.part009.zip', 'plugins.g000.part001.zip'];
        file_put_contents($this->root . '/out/uploads.g000.part002.zip', 'PK partial');
        file_put_contents($this->root . '/out/uploads.g000.part002.zip.k3Xy9Q', str_repeat('t', 64));
        $zip = new \ZipArchive();
        self::assertTrue($zip->open($this->root . '/out/uploads.g000.part009.zip', \ZipArchive::CREATE | \ZipArchive::OVERWRITE));
        $zip->addFromString('uploads/u00.jpg', 'abandoned pass');
        $zip->close();
        foreach ($leftovers as $leftover) { self::assertFileExists($this->root . '/out/' . $leftover); }

        $presentAtFirstTick = null;
        $result = $this->archiver()->archive($this->root . '/out', $saved, function () use (&$presentAtFirstTick): void {
            if ($presentAtFirstTick === null) { $presentAtFirstTick = $this->partFilesOnDisk(); }
        });

        self::assertSame($saved['parts_completed'], $presentAtFirstTick, 'stale parts are removed before the resumed worker archives anything');
        $this->assertDiskHoldsOnly($result['parts']);
        $expected = $order;
        sort($expected);
        self::assertSame($expected, $this->entries($result['parts'], $this->root . '/out'));
        fwrite(STDOUT, "\nleftovers removed=" . json_encode($leftovers) . ' kept=' . json_encode($saved['parts_completed']) . "\n");
    }

    public function testCheckpointFromAnEarlierDiscoveryIsNeverApplied(): void
    {
        $order = $this->interleavedTree();
        $stale = $this->interruptAfter(1, ['total_files' => count($order)]);
        // The next worker cannot use that checkpoint (this time the parent
        // file list arrived), so it discovers again in the walk's own order.
        // Every checkpoint it saves is lost, and the host ends it after it has
        // rebuilt part names of the same sizes.
        try {
            $this->archiver()->archive($this->root . '/out', $stale, static function (): void {}, [],
                static function (array $cursor): void {
                    if (count($cursor['parts_completed']) === 2) { throw new ArchiveWorkerEnded('host ended the worker'); }
                });
            self::fail('the second worker must be ended');
        } catch (ArchiveWorkerEnded $e) {
            // The host ended the worker.
        }
        self::assertFileExists($this->root . '/out/' . $stale['parts_completed'][0]);

        // The task row still holds the first worker's checkpoint.
        $result = $this->archiver()->archive($this->root . '/out', $stale, static function (): void {});

        $expected = $order;
        sort($expected);
        self::assertSame($expected, $this->entries($result['parts'], $this->root . '/out'), 'every file exactly once');
        $this->assertDiskHoldsOnly($result['parts']);
        $listed = array_keys(FilesArchiver::loadPrevMap($this->root . '/out/files.list'));
        sort($listed);
        self::assertSame($expected, $listed);
        self::assertSame(count($order), $result['files_total']);
    }

    public function testSavedPartOfAnotherSizeIsNotTrusted(): void
    {
        $order = $this->interleavedTree();
        $saved = $this->interruptAfter(2, ['total_files' => count($order)]);
        // The first saved part changed after the checkpoint: same name, other bytes.
        $first = $this->root . '/out/' . $saved['parts_completed'][0];
        $zip = new \ZipArchive();
        self::assertTrue($zip->open($first, \ZipArchive::CREATE | \ZipArchive::OVERWRITE));
        $zip->addFromString('uploads/u00.jpg', 'not what the checkpoint saved');
        $zip->close();
        clearstatcache(true, $first);
        self::assertNotSame($saved['part_sizes'][0], filesize($first));

        $result = $this->archiver()->archive($this->root . '/out', $saved, static function (): void {});

        $expected = $order;
        sort($expected);
        self::assertSame($expected, $this->entries($result['parts'], $this->root . '/out'), 'every file exactly once');
        $this->assertDiskHoldsOnly($result['parts']);
    }

    public function testSettledSkipSurvivesResumeAndFilesListNeverClaimsIt(): void
    {
        $order = $this->interleavedTree();
        $victim = $this->root . '/src/themes/acme/style.css';
        chmod($victim, 0000);
        try {
            $saved = $this->interruptAfter(2, ['total_files' => count($order)]);
            // No themes part was open when the source was skipped, so the skip is settled.
            self::assertContains('themes/acme/style.css', $saved['skipped']);
            $result = $this->archiver()->archive($this->root . '/out', $saved, static function (): void {});
        } finally {
            chmod($victim, 0600);
        }
        $entries = $this->entries($result['parts'], $this->root . '/out');
        self::assertNotContains('themes/acme/style.css', $entries);
        $listed = array_keys(FilesArchiver::loadPrevMap($this->root . '/out/files.list'));
        sort($listed);
        self::assertSame($entries, $listed, 'files.list lists exactly the archived files');
        self::assertSame(1, $result['skipped_sources_count']);
        self::assertSame(count($order) - 1, $result['files_total']);
    }

    public function testSkipSettledByAClosedPartSurvivesResume(): void
    {
        $order = $this->interleavedTree();
        $victim = $this->root . '/src/plugins/acme/readme.txt';
        chmod($victim, 0000);
        try {
            // Four uploads parts close in the loop; the fifth is the plugins
            // part that held the line of the skipped source.
            $saved = $this->interruptAfter(5, ['total_files' => count($order)]);
            self::assertContains('plugins.g000.part001.zip', $saved['parts_completed']);
            self::assertContains('plugins/acme/readme.txt', $saved['skipped'], 'closing the part settles the skip inside it');
            $result = $this->archiver()->archive($this->root . '/out', $saved, static function (): void {});
        } finally {
            chmod($victim, 0600);
        }
        $entries = $this->entries($result['parts'], $this->root . '/out');
        self::assertNotContains('plugins/acme/readme.txt', $entries);
        $listed = array_keys(FilesArchiver::loadPrevMap($this->root . '/out/files.list'));
        sort($listed);
        self::assertSame($entries, $listed, 'files.list lists exactly the archived files');
        self::assertSame(1, $result['skipped_sources_count']);
    }

    public function testPendingSkipIsDecidedAgainOnResume(): void
    {
        $order = $this->interleavedTree();
        $victim = $this->root . '/src/plugins/acme/readme.txt';
        chmod($victim, 0000);
        try {
            // The skip happens while the plugins part is open, so it is not settled.
            $saved = $this->interruptAfter(2, ['total_files' => count($order)]);
        } finally {
            chmod($victim, 0600);
        }
        self::assertNotContains('plugins/acme/readme.txt', $saved['skipped']);

        $result = $this->archiver()->archive($this->root . '/out', $saved, static function (): void {});

        $expected = $order;
        sort($expected);
        self::assertSame($expected, $this->entries($result['parts'], $this->root . '/out'), 'the source readable again is archived');
        $listed = array_keys(FilesArchiver::loadPrevMap($this->root . '/out/files.list'));
        sort($listed);
        self::assertSame($expected, $listed, 'files.list lists exactly the archived files');
        self::assertSame(0, $result['skipped_sources_count']);
    }

    public function testFileDeletedDuringTheRunIsTombstonedAfterResume(): void
    {
        $order = $this->interleavedTree();
        $deleted = 'themes/acme/style.css';
        $parent = [$deleted => ['size' => 120, 'mtime' => 1]];
        unlink($this->root . '/src/' . $deleted);
        $saved = null;
        try {
            $this->archiver()->archive($this->root . '/out', ['total_files' => count($order)], static function (): void {}, $parent,
                static function (array $cursor) use (&$saved): void {
                    if (count($cursor['parts_completed']) === 2) { $saved = $cursor; throw new ArchiveWorkerEnded('host ended the worker'); }
                });
            self::fail('the first worker must be ended');
        } catch (ArchiveWorkerEnded $e) {
            // The host ended the worker.
        }
        self::assertContains($deleted, $saved['skipped']);

        $result = $this->archiver()->archive($this->root . '/out', $saved, static function (): void {}, $parent);

        self::assertSame([$deleted], file($result['tombstones_file'], FILE_IGNORE_NEW_LINES), 'a restore of the chain must not bring the file back');
        self::assertSame(1, $result['tombstones_count']);
        self::assertArrayNotHasKey($deleted, FilesArchiver::loadPrevMap($this->root . '/out/files.list'));
    }

    public function testOlderCheckpointAgainstThePrunedFileListStartsOver(): void
    {
        $order = $this->interleavedTree();
        $victim = $this->root . '/src/index.php';
        chmod($victim, 0000);
        try {
            $older = null;
            $done = $this->archiver()->archive($this->root . '/out', ['total_files' => count($order)], static function (): void {}, null,
                static function (array $cursor) use (&$older): void {
                    if ($older === null && count($cursor['parts_completed']) === 1) { $older = $cursor; }
                });
            // The run finished and pruned the skipped line from paths.cache,
            // but every save after the first part was lost before it ended.
            self::assertTrue($done['done']);
            self::assertSame(count($order) - 1, $done['files_total']);
            $result = $this->archiver()->archive($this->root . '/out', $older, static function (): void {});
        } finally {
            chmod($victim, 0600);
        }
        $expected = array_values(array_diff($order, ['index.php']));
        sort($expected);
        self::assertSame($expected, $this->entries($result['parts'], $this->root . '/out'), 'every file exactly once');
        $this->assertDiskHoldsOnly($result['parts']);
    }

    public function testFailedCheckpointSaveNeverFailsTheBackup(): void
    {
        foreach (['index.php', 'plugins/acme/acme.php', 'uploads/a.jpg'] as $rel) {
            $path = $this->root . '/src/' . $rel;
            if (!is_dir(dirname($path))) { mkdir(dirname($path), 0700, true); }
            file_put_contents($path, $rel);
        }
        // Task storage accepts the phase saves but refuses every checkpoint.
        $GLOBALS['wpdb'] = new class {
            public string $prefix = 'wp_';
            public int $refused = 0;
            /** @param array<string,mixed> $data */
            public function update(string $table, array $data, array $where, $format = null, $whereFormat = null)
            {
                if (str_contains((string) ($data['sub_state'] ?? ''), '"checkpoint"')) { $this->refused++; return false; }
                return 1;
            }
        };
        $runner = new TaskRunner($this->params());
        $subState = (new \ReflectionMethod($runner, 'runArchivingFiles'))->invoke($runner, ['params' => $this->params()]);
        self::assertTrue($subState['files']['done']);
        self::assertCount(3, $subState['files']['parts']);
        self::assertGreaterThan(0, $GLOBALS['wpdb']->refused, 'every checkpoint save was refused');
    }

    public function testResumeKeepsItsDiscoveryWhileTheSiteAddsFiles(): void
    {
        mkdir($this->root . '/src/uploads');
        for ($i = 0; $i < 9; $i++) { file_put_contents($this->root . '/src/uploads/f' . $i . '.jpg', str_repeat((string) $i, 120)); }
        $saved = $this->interruptAfter(1, []);
        $cacheBefore = (string) file_get_contents($this->root . '/out/paths.cache');
        file_put_contents($this->root . '/src/uploads/added-between-workers.jpg', 'new');

        $result = $this->archiver()->archive($this->root . '/out', $saved, static function (): void {});

        self::assertSame(9, $result['files_total']);
        self::assertSame($cacheBefore, file_get_contents($this->root . '/out/paths.cache'));
        self::assertNotContains('uploads/added-between-workers.jpg', $this->entries($result['parts'], $this->root . '/out'));
        self::assertCount(9, $this->entries($result['parts'], $this->root . '/out'));
    }

    public function testAgentScratchLockAndTempFilesAreNeverArchived(): void
    {
        $runs = $this->root . '/src/wpmgr-agent/runs';
        $own = [
            'wpmgr-agent/runs/' . self::SNAPSHOT . '.lock' => '',
            'wpmgr-agent/runs/' . self::SNAPSHOT . '/uploads.g000.part004.zip.k3Xy9Q' => 'libzip temporary output',
            'wpmgr-agent/runs/' . self::SNAPSHOT . '/task_substate.json.tmp.4242' => '{}',
            'wpmgr-agent/runs/earlier-run/uploads.g000.part001.zip' => 'an earlier run',
            '.wpmgr-staging-1a2b3c4d/uploads/photo.jpg' => 'restore staging copy',
            '.wpmgr-old-files-1a2b3c4d/uploads/photo.jpg' => 'pre-restore copy',
            'uploads/.wpmgr-tmp-00aa11bb22cc33dd' => 'atomic write in progress',
            'uploads/2026/.wpmgr-ul-0011223344556677' => 'upload in progress',
            '.wpmgr-oc-state.json.tmp.1234567' => '{}',
            'wpmgr-object-cache-config.php.tmp.123456' => 'redis credentials being rewritten',
            'uploads/wpmgr-update-inflight/plugin-acme.lock' => '',
            'wpmgr-update-watchdog/plugin-acme.json' => '{}',
            'uploads/wpmgr-file-extract-tmp/job/a.txt' => 'extraction in progress',
        ];
        $site = [
            'index.php' => '<?php',
            'plugins/acme/acme.php' => '<?php',
            'uploads/photo.jpg' => 'photo',
            'uploads/banner.wpmgr-original.jpg' => 'original kept by the media optimizer',
            'uploads/notes.wpmgr-tmp.txt' => 'a user file',
            'uploads/wpmgr-agent-docs/readme.txt' => 'a user directory with a similar name',
        ];
        foreach ($own + $site as $rel => $bytes) {
            $path = $this->root . '/src/' . $rel;
            if (!is_dir(dirname($path))) { mkdir(dirname($path), 0700, true); }
            file_put_contents($path, $bytes);
        }
        $expected = array_keys($site);
        sort($expected);

        foreach ([self::SNAPSHOT, 'later-run'] as $run) {
            $scratch = $runs . '/' . $run;
            $result = (new FilesArchiver($this->root . '/src'))->archive($scratch, [], static function (): void {});
            self::assertSame($expected, $this->entries($result['parts'], $scratch));
            $listed = array_keys(FilesArchiver::loadPrevMap($scratch . '/files.list'));
            sort($listed);
            self::assertSame($expected, $listed);
            self::assertSame(count($expected), $result['files_total']);
        }
        fwrite(STDOUT, "\nown files excluded=" . count($own) . ' site files kept=' . json_encode($expected) . "\n");
    }

    public function testTaskRunnerPersistsTheArchiveCheckpointAfterEveryClosedPart(): void
    {
        foreach (['index.php', 'plugins/acme/acme.php', 'themes/acme/style.css', 'uploads/a.jpg'] as $rel) {
            $path = $this->root . '/src/' . $rel;
            if (!is_dir(dirname($path))) { mkdir(dirname($path), 0700, true); }
            file_put_contents($path, $rel);
        }
        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->seedRow(self::SNAPSHOT, ['phase' => 'archiving_files', 'sub_state' => json_encode(['params' => $this->params()])]);
        $GLOBALS['wpdb'] = $wpdb;
        $runner = new TaskRunner($this->params());
        $subState = (new \ReflectionMethod($runner, 'runArchivingFiles'))->invoke($runner, ['params' => $this->params()]);
        $parts = $subState['files']['parts'];
        self::assertCount(4, $parts);

        $persisted = [];
        foreach ($wpdb->updates as $update) {
            if (!isset($update['data']['sub_state'])) { continue; }
            $state = TaskRunner::decodeSubStateColumn($update['data']['sub_state']);
            if (isset($state['files']['parts_completed'])) { $persisted[] = $state['files']['parts_completed']; }
        }
        for ($closed = 0; $closed <= count($parts); $closed++) {
            self::assertContains(array_slice($parts, 0, $closed), $persisted, 'a checkpoint is saved to the task row after closed part ' . $closed);
        }
        fwrite(STDOUT, "\npersisted checkpoints=" . json_encode($persisted) . "\n");
    }

    public function testTaskRunnerResumesTheArchiveFromThePersistedCheckpoint(): void
    {
        $order = $this->interleavedTree();
        $saved = $this->interruptAfter(2, ['total_files' => count($order)], 250);
        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->seedRow(self::SNAPSHOT, ['phase' => 'archiving_files', 'sub_state' => json_encode(['params' => $this->params(), 'files' => $saved])]);
        $GLOBALS['wpdb'] = $wpdb;
        $row = TaskRunner::decodeSubStateColumn($wpdb->rows[self::SNAPSHOT]['sub_state']);

        $runner = new TaskRunner($this->params());
        $subState = (new \ReflectionMethod($runner, 'runArchivingFiles'))->invoke($runner, $row);

        $parts = $subState['files']['parts'];
        self::assertSame($saved['parts_completed'], array_slice($parts, 0, 2));
        $expected = $order;
        sort($expected);
        self::assertSame($expected, $this->entries($parts, $this->root . '/out'));
        $this->assertDiskHoldsOnly($parts);
    }

    /**
     * Run a first worker until it has closed $closed parts, then end it.
     *
     * @param array<string,mixed> $resume
     * @return array<string,mixed> The checkpoint saved before the worker ended.
     */
    private function interruptAfter(int $closed, array $resume, int $maxPartBytes = 250): array
    {
        $saved = null;
        try {
            $this->archiver($maxPartBytes)->archive($this->root . '/out', $resume, static function (): void {}, null,
                static function (array $cursor) use (&$saved, $closed): void {
                    if (count($cursor['parts_completed']) === $closed) {
                        $saved = $cursor;
                        throw new ArchiveWorkerEnded('host ended the worker');
                    }
                });
            self::fail('the first worker must be ended');
        } catch (ArchiveWorkerEnded $e) {
            // The host ended the worker.
        }
        self::assertIsArray($saved);
        return $saved;
    }

    private function archiver(int $maxPartBytes = 250): FilesArchiver
    {
        return new FilesArchiver($this->root . '/src', [], ['max_part_bytes' => $maxPartBytes]);
    }

    /**
     * Seventeen 120-byte files whose discovery interleaves all four
     * components, so uploads closes parts while the other components still
     * hold open ones. Writes the matching paths.cache and files.list.
     *
     * @return list<string>
     */
    private function interleavedTree(): array
    {
        $order = [
            'index.php', 'uploads/u00.jpg', 'plugins/acme/acme.php', 'uploads/u01.jpg', 'uploads/u02.jpg',
            'themes/acme/style.css', 'uploads/u03.jpg', 'plugins/acme/readme.txt', 'uploads/u04.jpg', 'uploads/u05.jpg',
            'uploads/u06.jpg', 'uploads/u07.jpg', 'uploads/u08.jpg', 'languages/de.mo', 'uploads/u09.jpg',
            'uploads/u10.jpg', 'uploads/u11.jpg',
        ];
        $cache = '';
        $list = '';
        foreach ($order as $i => $rel) {
            $path = $this->root . '/src/' . $rel;
            if (!is_dir(dirname($path))) { mkdir(dirname($path), 0700, true); }
            file_put_contents($path, str_repeat(chr(65 + $i), 120));
            $top = explode('/', $rel)[0];
            $cache .= (in_array($top, ['plugins', 'themes', 'uploads'], true) ? $top : 'wp-content') . "\t" . $rel . "\n";
            $list .= $rel . "\t" . filesize($path) . "\t" . filemtime($path) . "\n";
        }
        file_put_contents($this->root . '/out/paths.cache', $cache);
        file_put_contents($this->root . '/out/files.list', $list);
        return $order;
    }

    /** @param list<string> $parts */
    private function assertDiskHoldsOnly(array $parts): void
    {
        sort($parts);
        self::assertSame($parts, $this->partFilesOnDisk(), 'the scratch dir holds exactly the saved parts');
    }

    /** @return list<string> */
    private function partFilesOnDisk(): array
    {
        $names = array_values(array_filter((array) scandir($this->root . '/out'), static fn ($n): bool => preg_match(self::PART_FILE, (string) $n) === 1));
        sort($names);
        return $names;
    }

    /**
     * @param list<string> $parts
     * @return list<string> Every entry across the parts, duplicates kept.
     */
    private function entries(array $parts, string $dir): array
    {
        $entries = [];
        foreach ($parts as $part) {
            $zip = new \ZipArchive();
            self::assertTrue($zip->open($dir . '/' . $part) === true, 'unreadable part ' . $part);
            for ($i = 0; $i < $zip->numFiles; $i++) { $entries[] = (string) $zip->getNameIndex($i); }
            $zip->close();
        }
        sort($entries);
        return $entries;
    }

    /** @return array<string,mixed> */
    private function params(): array
    {
        return ['snapshot_id' => self::SNAPSHOT, 'kind' => 'files', 'scratch_dir' => $this->root . '/out',
            'wp_content_path' => $this->root . '/src', 'progress_endpoint' => '', 'presign_endpoint' => '',
            'manifest_endpoint' => '', 'age_recipient' => '', 'db' => ['prefix' => 'wp_']];
    }

    private function removeTree(string $dir): void
    {
        if ($dir === '' || !is_dir($dir)) { return; }
        foreach (new \FilesystemIterator($dir) as $file) {
            if ($file->isDir() && !$file->isLink()) { $this->removeTree($file->getPathname()); }
            else { unlink($file->getPathname()); }
        }
        rmdir($dir);
    }
}
