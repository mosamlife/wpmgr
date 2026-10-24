<?php

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Backup\BackupJanitor;
use WPMgr\Agent\Backup\FilesArchiver;
use WPMgr\Agent\Backup\TaskRunner;
use WPMgr\Agent\Backup\Watchdog;
use WPMgr\Agent\Keystore;
use WPMgr\Agent\Plugin;
use WPMgr\Agent\Settings;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

require_once __DIR__ . '/FakeBackupTasksWpdb.php';
require_once __DIR__ . '/FileLockStub.php';
require_once __DIR__ . '/ScriptedCloseZipArchive.php';

#[\PHPUnit\Framework\Attributes\RunTestsInSeparateProcesses]
#[\PHPUnit\Framework\Attributes\PreserveGlobalState(false)]
final class BackupArchiveBehaviorTest extends TestCase
{
    private const SNAPSHOT = 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee';
    private string $root = '';
    private array $options = [];
    private bool $createdKey = false;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->root = __DIR__ . '/issue954-fixture-' . bin2hex(random_bytes(6));
        mkdir($this->root . '/src', 0700, true);
        mkdir($this->root . '/out', 0700, true);
        Functions\when('esc_html')->returnArg();
        Functions\when('wp_json_encode')->alias(static fn ($data) => json_encode($data));
        Functions\when('wp_schedule_single_event')->justReturn(true);
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        $this->removeTree($this->root);
        if ($this->createdKey && is_file(__DIR__ . '/issue954-signing.key')) { unlink(__DIR__ . '/issue954-signing.key'); }
        parent::tear_down();
    }

    public function testFreshExclusionsApplyToEveryComponent(): void
    {
        $expected = $this->selectionTree();
        $result = (new FilesArchiver($this->root . '/src', ['2024', '2025', '2026'], ['exclude_extensions' => ['log']]))
            ->archive($this->root . '/out', [], static function (): void {});
        self::assertSame($expected, $this->entries($result));
        self::assertSame(count($expected), $result['files_total']);
        fwrite(STDOUT, "\nfresh entries=" . json_encode($this->entries($result)) . "\n");
    }

    public function testTaskRunnerForwardsExclusionsToEveryComponent(): void
    {
        $expected = $this->selectionTree();
        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->seedRow(self::SNAPSHOT);
        $GLOBALS['wpdb'] = $wpdb;
        $runner = new TaskRunner($this->params() + ['exclude_paths' => ['2024'], 'exclude_extensions' => ['log']]);
        $method = new \ReflectionMethod($runner, 'runArchivingFiles');
        $subState = $method->invoke($runner, []);
        self::assertSame($expected, $this->entries($subState['files']));
    }

    public function testTextSegmentAndExtensionsAreExcludedInEveryComponent(): void
    {
        $expected = $this->selectionTree('skip');
        $result = (new FilesArchiver($this->root . '/src', ['skip'], ['exclude_extensions' => ['log']]))
            ->archive($this->root . '/out', [], static function (): void {});
        self::assertSame($expected, $this->entries($result));
        fwrite(STDOUT, "\ntext segment and extension exclusions=" . json_encode($this->entries($result)) . "\n");
    }

    public function testNumericExtensionsRemainStringsAfterDeduplication(): void
    {
        file_put_contents($this->root . '/src/excluded.123', 'excluded');
        file_put_contents($this->root . '/src/keep.jpg', 'keep');
        $result = (new FilesArchiver($this->root . '/src', [], ['exclude_extensions' => ['123', '.123']]))
            ->archive($this->root . '/out', [], static function (): void {});
        self::assertSame(['keep.jpg'], $this->entries($result));
    }

    public function testNativeFailedCloseRetainsNumericStatusAndReason(): void
    {
        $source = $this->root . '/src/native.txt';
        file_put_contents($source, 'native');
        $zip = new \ZipArchive();
        self::assertTrue($zip->open($this->root . '/out/native.zip', \ZipArchive::CREATE | \ZipArchive::OVERWRITE));
        self::assertTrue($zip->addFile($source, 'native.txt'));
        unlink($source);
        set_error_handler(static fn () => true);
        try { self::assertFalse($zip->close()); }
        finally { restore_error_handler(); }
        self::assertSame(\ZipArchive::ER_OPEN, $zip->status);
        self::assertStringContainsString("Can't open file", $zip->getStatusString());
    }

    public function testPartRotationUsesSourceBytesAndChecksAfterAdding(): void
    {
        foreach (['a', 'b', 'c'] as $name) { file_put_contents($this->root . '/src/' . $name, str_repeat($name, 10)); }
        $result = (new FilesArchiver($this->root . '/src', [], ['max_part_bytes' => 15]))
            ->archive($this->root . '/out', [], static function (): void {});
        $parts = [];
        foreach ($result['parts'] as $part) {
            $zip = new \ZipArchive();
            self::assertTrue($zip->open($this->root . '/out/' . $part));
            $size = 0;
            for ($i = 0; $i < $zip->numFiles; $i++) { $size += $zip->statIndex($i)['size']; }
            $parts[] = ['entries' => $zip->numFiles, 'source_bytes' => $size, 'zip_bytes' => filesize($this->root . '/out/' . $part)];
            $zip->close();
        }
        self::assertSame([2, 1], array_column($parts, 'entries'));
        self::assertSame([20, 10], array_column($parts, 'source_bytes'));
        fwrite(STDOUT, "\nrotation cap=15 source_bytes parts=" . json_encode($parts) . ' default_bytes=' . FilesArchiver::DEFAULT_MAX_PART_BYTES . ' default_entries=' . FilesArchiver::DEFAULT_MAX_PART_ENTRIES . "\n");
    }

    public function testCachedDiscoveryRetainsOriginalSelectionOnResume(): void
    {
        $this->selectionTree('skip');
        $original = (new FilesArchiver($this->root . '/src'))->archive($this->root . '/out', [], static function (): void {});
        $cache = $this->root . '/out/paths.cache';
        $before = file_get_contents($cache);
        foreach ($original['parts'] as $part) { unlink($this->root . '/out/' . $part); }
        $resume = ['cache_file' => $cache, 'total_files' => $original['files_total'], 'file_index' => 0];
        $again = (new FilesArchiver($this->root . '/src', ['skip'], ['exclude_extensions' => ['log']]))
            ->archive($this->root . '/out', $resume, static function (): void {});
        self::assertSame($before, file_get_contents($cache));
        self::assertContains('uploads/skip/a.jpg', $this->entries($again));
        self::assertContains('uploads/x.log', $this->entries($again));
        self::assertSame($original['files_total'], $again['files_total']);
        fwrite(STDOUT, "\ncached entries=" . json_encode($this->entries($again)) . "\n");
    }

    public function testWatchdogResumeRebuildsDiscoveryUsingSavedOptions(): void
    {
        $this->signing();
        $expected = $this->selectionTree('skip');
        $cache = $this->root . '/out/paths.cache';
        file_put_contents($cache, "uploads\tuploads/skip/a.jpg\n");
        $params = $this->params(true) + ['exclude_paths' => ['skip'], 'exclude_extensions' => ['log']];
        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->seedRow(self::SNAPSHOT, ['phase' => 'archiving_files', 'last_progress_at' => time() - Watchdog::STALL_THRESHOLD_SECONDS - 1,
            'sub_state' => json_encode(['params' => $params])]);
        $GLOBALS['wpdb'] = $wpdb;
        $packed = null;
        $savedCursor = null;
        Functions\when('wp_remote_post')->alias(function ($url, $args) use (&$packed, &$savedCursor, $wpdb) {
            $event = json_decode($args['body'], true);
            if ($event['phase'] === 'archiving_files' && ($event['phase_detail']['done'] ?? false)) {
                $packed = $this->entries($event['phase_detail']);
                $state = json_decode($wpdb->rows[self::SNAPSHOT]['sub_state'], true);
                $savedCursor = $state['files'] ?? null;
            }
            return ['response' => ['code' => 200], 'body' => '{}'];
        });
        Watchdog::resumeIfStalled(self::SNAPSHOT);
        self::assertSame($expected, $packed);
        self::assertNull($savedCursor);
        fwrite(STDOUT, "\nwatchdog resume entries=" . json_encode($packed) . " durable_files_cursor=null\n");
    }

    public function testDeletedSourceIsReportedAsSkipped(): void
    {
        file_put_contents($this->root . '/src/victim.txt', 'victim');
        $result = (new FilesArchiver($this->root . '/src'))->archive($this->root . '/out', [], function ($phase, $detail): void {
            if (($detail['stage'] ?? '') === 'finalizing_part') { unlink($this->root . '/src/victim.txt'); }
        });
        self::assertTrue($result['done']);
        self::assertSame(['victim.txt'], $result['skipped_sources']);
        self::assertSame(1, $result['skipped_sources_count']);
        self::assertSame(0, $result['files_total']);
        self::assertSame(0, $result['bytes_written']);
        self::assertSame([], $result['parts']);
        self::assertSame('', file_get_contents($this->root . '/out/files.list'));
    }

    public function testCloseFailureMustIncludeLibzipReason(): void
    {
        [$message] = $this->destinationCloseFailure();
        self::assertStringContainsString('Failure to create temporary file', $message, 'The close failure must preserve libzip diagnostics.');
        self::assertStringContainsString('libzip status ' . \ZipArchive::ER_TMPOPEN, $message);
        self::assertStringEndsWith('for uploads.g000.part001.zip', $message);
        self::assertStringNotContainsString($this->root, $message);
    }

    public function testSourceDisappearanceMustPreserveReadableSiblings(): void
    {
        $source = $this->root . '/src/victim.txt';
        file_put_contents($source, 'victim');
        file_put_contents($this->root . '/src/keep.txt', 'keep');
        $result = null;
        $failure = '';
        set_error_handler(static fn () => true);
        try {
            $result = (new FilesArchiver($this->root . '/src'))->archive($this->root . '/out', [], static function ($phase, $detail) use ($source): void {
                if (($detail['stage'] ?? '') === 'finalizing_part' && is_file($source)) { unlink($source); }
            });
        } catch (\RuntimeException $e) {
            $failure = $e->getMessage();
        } finally {
            restore_error_handler();
        }
        self::assertNotNull($result, 'Readable siblings must survive a disappearing source: ' . $failure);
        self::assertTrue($result['done']);
        self::assertContains('keep.txt', $this->entries($result));
    }

    public function testTruncatedSourceIsRebuiltWithCurrentContentsAndMetadata(): void
    {
        file_put_contents($this->root . '/src/victim.txt', 'abcdefgh');
        file_put_contents($this->root . '/src/keep.txt', 'keep');
        $closes = 0;
        $factory = static function () use (&$closes): \ZipArchive { return new ScriptedCloseZipArchive([1], $closes); };
        $result = (new FilesArchiver($this->root . '/src', [], ['zip_factory' => $factory]))->archive($this->root . '/out', [], function ($phase, $detail): void {
            if (($detail['stage'] ?? '') === 'finalizing_part') { file_put_contents($this->root . '/src/victim.txt', 'abc'); }
        });
        self::assertSame(2, $closes);
        $zip = new \ZipArchive();
        self::assertTrue($zip->open($this->root . '/out/' . $result['parts'][0]));
        self::assertSame('abc', $zip->getFromName('victim.txt'));
        $zip->close();
        $baseline = FilesArchiver::loadPrevMap($this->root . '/out/files.list');
        self::assertSame(3, $baseline['victim.txt']['size']);
        self::assertSame(0, $result['skipped_sources_count']);
        self::assertSame(2, $result['files_total']);
        self::assertSame(array_sum(array_map(fn ($part) => filesize($this->root . '/out/' . $part), $result['parts'])), $result['bytes_written']);
    }

    public function testSuccessfulCloseAfterTruncationRearchivesSourceOnNextIncremental(): void
    {
        $source = $this->root . '/src/victim.txt';
        file_put_contents($source, 'abcdefgh');
        touch($source, time() - 60);
        clearstatcache(true, $source);
        $mtime = filemtime($source);
        file_put_contents($this->root . '/src/keep.txt', 'keep');
        $closes = 0;
        $factory = static function () use (&$closes): \ZipArchive {
            return new class ($closes) extends \ZipArchive {
                private array $sources = [];
                public function __construct(private int &$closes) {}
                public function addFile(string $filepath, string $entryname = '', int $start = 0, int $length = 0, int $flags = \ZipArchive::FL_OVERWRITE): bool
                {
                    $this->sources[$entryname] = $filepath;
                    return parent::addFile($filepath, $entryname, $start, $length, $flags);
                }
                public function close(): bool
                {
                    $this->closes++;
                    // Materialize current bytes to model a successful close after truncation.
                    foreach ($this->sources as $name => $path) {
                        \PHPUnit\Framework\Assert::assertTrue($this->addFromString($name, file_get_contents($path)));
                    }
                    return parent::close();
                }
            };
        };
        $reports = [];
        $discovered = [];
        $result = (new FilesArchiver($this->root . '/src', [], ['zip_factory' => $factory]))
            ->archive($this->root . '/out', [], static function ($phase, $detail) use ($source, $mtime, &$reports, &$discovered): void {
                $reports[] = $detail;
                if (($detail['stage'] ?? '') === 'finalizing_part') {
                    $discovered = FilesArchiver::loadPrevMap(dirname($source, 2) . '/out/files.list');
                    file_put_contents($source, 'abc');
                    touch($source, $mtime);
                }
            });
        self::assertTrue($result['done']);
        self::assertSame(1, $closes);
        self::assertNotContains('rebuilding_part', array_column($reports, 'stage'));
        self::assertSame(['keep.txt', 'victim.txt'], $this->entries($result));
        $zip = new \ZipArchive();
        self::assertTrue($zip->open($this->root . '/out/' . $result['parts'][0]));
        self::assertSame('abc', $zip->getFromName('victim.txt'));
        self::assertSame(3, $zip->statName('victim.txt')['size']);
        $zip->close();
        $baseline = FilesArchiver::loadPrevMap($this->root . '/out/files.list');
        self::assertSame(8, $baseline['victim.txt']['size']);
        self::assertSame($discovered['victim.txt'], $baseline['victim.txt']);
        self::assertCount(2, $baseline);
        $cached = file($this->root . '/out/paths.cache', FILE_IGNORE_NEW_LINES);
        sort($cached);
        self::assertSame(["wp-content\tkeep.txt", "wp-content\tvictim.txt"], $cached);
        self::assertSame(2, $result['files_total']);
        self::assertSame(0, $result['skipped_sources_count']);
        self::assertSame(0, $result['tombstones_count']);
        self::assertSame(filesize($this->root . '/out/' . $result['parts'][0]), $result['bytes_written']);
        touch($source, $baseline['victim.txt']['mtime'] - 60);
        clearstatcache(true, $source);
        self::assertLessThanOrEqual($baseline['victim.txt']['mtime'], filemtime($source));
        mkdir($this->root . '/next');
        $next = (new FilesArchiver($this->root . '/src'))->archive($this->root . '/next', [], static function (): void {}, $baseline);
        self::assertSame(1, $next['files_changed']);
        self::assertSame(1, $next['files_carried']);
        self::assertTrue($zip->open($this->root . '/next/' . $next['parts'][0]));
        self::assertSame(1, $zip->numFiles);
        self::assertSame('victim.txt', $zip->getNameIndex(0));
        self::assertSame('abc', $zip->getFromName('victim.txt'));
        $zip->close();
        $nextBaseline = FilesArchiver::loadPrevMap($this->root . '/next/files.list');
        self::assertSame(3, $nextBaseline['victim.txt']['size']);
        self::assertLessThanOrEqual($baseline['victim.txt']['mtime'], $nextBaseline['victim.txt']['mtime']);
    }

    public function testReadableSourceRejectedByAddFileIsSkipped(): void
    {
        $this->assertRejectedSourceSkipped(false);
    }

    public function testReadableSourceRejectedDuringRebuildIsSkipped(): void
    {
        $this->assertRejectedSourceSkipped(true);
    }

    private function assertRejectedSourceSkipped(bool $rebuild): void
    {
        file_put_contents($this->root . '/src/victim.txt', 'abcdefgh');
        file_put_contents($this->root . '/src/keep.txt', 'keep');
        $opened = 0;
        $rejections = 0;
        $closes = 0;
        $factory = static function () use (&$opened, &$rejections, &$closes, $rebuild): \ZipArchive {
            $opened++;
            return new class (!$rebuild || $opened > 1, $rejections, $rebuild, $closes) extends ScriptedCloseZipArchive {
                public function __construct(private bool $reject, private int &$rejections, bool $rebuild, int &$closes)
                {
                    parent::__construct($rebuild ? [1] : [], $closes);
                }
                public function addFile(string $filepath, string $entryname = '', int $start = 0, int $length = 0, int $flags = \ZipArchive::FL_OVERWRITE): bool
                {
                    if ($this->reject && $entryname === 'victim.txt') {
                        self::assertReadableSource($filepath);
                        $this->rejections++;
                        return false;
                    }
                    return parent::addFile($filepath, $entryname, $start, $length, $flags);
                }
                private static function assertReadableSource(string $path): void
                {
                    \PHPUnit\Framework\Assert::assertFileExists($path);
                    \PHPUnit\Framework\Assert::assertTrue(is_readable($path));
                }
            };
        };
        $reports = [];
        $result = (new FilesArchiver($this->root . '/src', [], ['zip_factory' => $factory]))
            ->archive($this->root . '/out', [], function ($phase, $detail) use ($rebuild, &$reports): void {
                if ($rebuild && ($detail['stage'] ?? '') === 'finalizing_part') {
                    file_put_contents($this->root . '/src/victim.txt', 'abc');
                }
                $reports[] = $detail;
            });
        self::assertSame(1, $result['skipped_sources_count']);
        self::assertSame(1, $rejections);
        self::assertTrue($result['done']);
        self::assertSame(['victim.txt'], $result['skipped_sources']);
        self::assertSame(['keep.txt'], $this->entries($result));
        self::assertSame(1, $result['files_total']);
        self::assertArrayNotHasKey('victim.txt', FilesArchiver::loadPrevMap($this->root . '/out/files.list'));
        self::assertStringNotContainsString('victim.txt', file_get_contents($this->root . '/out/paths.cache'));
        self::assertSame(1, $reports[array_key_last($reports)]['skipped_sources_count']);
        self::assertSame(['victim.txt'], $reports[array_key_last($reports)]['skipped_sources']);
    }

    public function testEmptyPartWithFailedCloseIsDiscarded(): void
    {
        file_put_contents($this->root . '/src/victim.txt', 'victim');
        $path = $this->root . '/out/wp-content.g000.part001.zip';
        $zip = new class ($path) extends \ZipArchive {
            public bool $closed = false;
            public function __construct(private string $path) {}
            public function addFile(string $filepath, string $entryname = '', int $start = 0, int $length = 0, int $flags = \ZipArchive::FL_OVERWRITE): bool
            {
                return false;
            }
            public function close(): bool
            {
                $this->closed = true;
                parent::close();
                file_put_contents($this->path, 'empty placeholder');
                return false;
            }
        };
        $result = (new FilesArchiver($this->root . '/src', [], ['zip_factory' => static fn (): \ZipArchive => $zip]))
            ->archive($this->root . '/out', [], static function (): void {});
        self::assertTrue($zip->closed);
        self::assertTrue($result['done']);
        self::assertSame([], $result['parts']);
        self::assertSame(0, $result['bytes_written']);
        self::assertSame(0, $result['files_total']);
        self::assertSame(1, $result['skipped_sources_count']);
        self::assertSame('', file_get_contents($this->root . '/out/paths.cache'));
        self::assertSame('', file_get_contents($this->root . '/out/files.list'));
        self::assertFileDoesNotExist($path);
    }

    public function testRebuildPreservesEntriesAlreadyInAResumedPart(): void
    {
        mkdir($this->root . '/src/uploads');
        file_put_contents($this->root . '/src/uploads/victim.txt', 'victim');
        file_put_contents($this->root . '/src/uploads/keep.txt', 'keep');
        $zip = new \ZipArchive();
        self::assertTrue($zip->open($this->root . '/out/uploads.g000.part001.zip', \ZipArchive::CREATE));
        $zip->addFromString('uploads/earlier.txt', 'EARLIER');
        $zip->addFromString('uploads/victim.txt', 'OLD');
        $zip->close();
        file_put_contents($this->root . '/out/paths.cache', "uploads\tuploads/earlier.txt\nuploads\tuploads/victim.txt\nuploads\tuploads/keep.txt\n");
        file_put_contents($this->root . '/out/files.list', "uploads/earlier.txt\t7\t1\nuploads/victim.txt\t6\t1\nuploads/keep.txt\t4\t1\n");
        $resume = ['total_files' => 3, 'file_index' => 1, 'components' => ['uploads' => ['current_part' => 1]]];
        $result = (new FilesArchiver($this->root . '/src'))->archive($this->root . '/out', $resume, function ($phase, $detail): void {
            if (($detail['stage'] ?? '') === 'finalizing_part') { unlink($this->root . '/src/uploads/victim.txt'); }
        });
        self::assertSame(['uploads/earlier.txt', 'uploads/keep.txt'], $this->entries($result));
        self::assertTrue($zip->open($this->root . '/out/uploads.g000.part001.zip'));
        self::assertSame('EARLIER', $zip->getFromName('uploads/earlier.txt'));
        $zip->close();
        self::assertSame(2, $result['files_total']);
    }

    public function testUnreadableQueuedSourceIsSkipped(): void
    {
        file_put_contents($this->root . '/src/victim.txt', 'victim');
        file_put_contents($this->root . '/src/keep.txt', 'keep');
        try {
            $result = (new FilesArchiver($this->root . '/src'))->archive($this->root . '/out', [], function ($phase, $detail): void {
                if (($detail['stage'] ?? '') === 'finalizing_part') { chmod($this->root . '/src/victim.txt', 0000); }
            });
            self::assertSame(['keep.txt'], $this->entries($result));
            self::assertSame(1, $result['skipped_sources_count']);
        } finally { chmod($this->root . '/src/victim.txt', 0600); }
    }

    public function testResumedRebuildReplaysDeletionsMadeBeforeClose(): void
    {
        $result = $this->resumedPartWithAnEarlyDeletion(false);
        self::assertTrue($result['done']);
        self::assertSame(['uploads/earlier.txt', 'uploads/keep.txt', 'uploads/victim.txt'], $this->entries($result));
        $baseline = FilesArchiver::loadPrevMap($this->root . '/out/files.list');
        self::assertArrayNotHasKey('uploads/missing.txt', $baseline);
        $paths = array_keys($baseline);
        sort($paths);
        self::assertSame(['uploads/earlier.txt', 'uploads/keep.txt', 'uploads/victim.txt'], $paths);
        self::assertSame(3, $baseline['uploads/victim.txt']['size']);
        self::assertSame("uploads\tuploads/earlier.txt\nuploads\tuploads/victim.txt\nuploads\tuploads/keep.txt\n", file_get_contents($this->root . '/out/paths.cache'));
        self::assertSame(['uploads/missing.txt'], file($result['tombstones_file'], FILE_IGNORE_NEW_LINES));
        self::assertSame(1, $result['tombstones_count']);
        self::assertSame(1, $result['skipped_sources_count']);
        self::assertSame(3, $result['files_total']);
    }

    public function testResumedRebuildDeletionFailureIsFatal(): void
    {
        $this->expectException(\WPMgr\Agent\Backup\ZipCloseException::class);
        $this->resumedPartWithAnEarlyDeletion(true);
    }

    private function resumedPartWithAnEarlyDeletion(bool $rejectReplay): array
    {
        mkdir($this->root . '/src/uploads');
        file_put_contents($this->root . '/src/uploads/victim.txt', 'abcdefgh');
        file_put_contents($this->root . '/src/uploads/keep.txt', 'keep');
        $zip = new \ZipArchive();
        self::assertTrue($zip->open($this->root . '/out/uploads.g000.part001.zip', \ZipArchive::CREATE));
        $zip->addFromString('uploads/earlier.txt', 'EARLIER');
        $zip->addFromString('uploads/missing.txt', 'OLD');
        $zip->close();
        file_put_contents($this->root . '/out/paths.cache', "uploads\tuploads/earlier.txt\nuploads\tuploads/missing.txt\nuploads\tuploads/victim.txt\nuploads\tuploads/keep.txt\n");
        file_put_contents($this->root . '/out/files.list', "uploads/earlier.txt\t7\t1\nuploads/missing.txt\t3\t1\nuploads/victim.txt\t8\t1\nuploads/keep.txt\t4\t1\n");
        $opens = 0;
        $closes = 0;
        $deletions = [];
        $factory = static function () use (&$opens, &$closes, &$deletions, $rejectReplay): \ZipArchive {
            $opens++;
            return new class ($opens > 1 && $rejectReplay, $deletions, $closes) extends ScriptedCloseZipArchive {
                public function __construct(private bool $reject, private array &$deletions, int &$closes)
                {
                    parent::__construct([1, 2], $closes);
                }
                public function deleteName(string $name): bool
                {
                    $this->deletions[] = $name;
                    return $this->reject ? false : parent::deleteName($name);
                }
            };
        };
        $resume = ['total_files' => 4, 'file_index' => 1, 'components' => ['uploads' => ['current_part' => 1]]];
        $deletedBeforeClose = null;
        $result = (new FilesArchiver($this->root . '/src', [], ['zip_factory' => $factory]))->archive($this->root . '/out', $resume,
            function ($phase, $detail) use (&$deletions, &$deletedBeforeClose): void {
                if (($detail['stage'] ?? '') === 'finalizing_part') {
                    $deletedBeforeClose = $deletions;
                    file_put_contents($this->root . '/src/uploads/victim.txt', 'abcdef');
                } elseif (($detail['stage'] ?? '') === 'rebuilding_part' && $detail['rebuild_attempt'] === 1) {
                    file_put_contents($this->root . '/src/uploads/victim.txt', 'abc');
                }
            }, ['uploads/missing.txt' => ['size' => 3, 'mtime' => 1]]);
        self::assertSame(['uploads/missing.txt'], $deletedBeforeClose);
        self::assertSame(3, $opens);
        return $result;
    }

    public function testUnwritableDestinationRemainsFatalEvenWithAChangedSource(): void
    {
        foreach ([false, true] as $changed) {
            file_put_contents($this->root . '/src/victim.txt', 'abcdefgh');
            $failure = null;
            try {
                (new FilesArchiver($this->root . '/src'))->archive($this->root . '/out', [], function ($phase, $detail) use ($changed): void {
                    if (($detail['stage'] ?? '') === 'finalizing_part') {
                        if ($changed) { file_put_contents($this->root . '/src/victim.txt', 'x'); }
                        chmod($this->root . '/out', 0500);
                    }
                });
            } catch (\WPMgr\Agent\Backup\ZipCloseException $e) { $failure = $e; }
            finally { chmod($this->root . '/out', 0700); }
            self::assertInstanceOf(\WPMgr\Agent\Backup\ZipCloseException::class, $failure);
            self::assertSame(\ZipArchive::ER_TMPOPEN, $failure->libzipStatus());
            self::assertStringContainsString('Failure to create temporary file', $failure->getMessage());
        }
    }

    public function testPersistentlyChangingSourceStopsAfterTwoRebuilds(): void
    {
        $source = $this->root . '/src/victim.txt';
        file_put_contents($source, str_repeat('x', 64));
        $length = 64;
        $rebuilds = 0;
        $failure = null;
        $closes = 0;
        $factory = static function () use (&$closes): \ZipArchive { return new ScriptedCloseZipArchive([1, 2, 3], $closes); };
        try {
            (new FilesArchiver($this->root . '/src', [], ['zip_factory' => $factory]))->archive($this->root . '/out', [], static function ($phase, $detail) use ($source, &$length, &$rebuilds): void {
                if (in_array($detail['stage'] ?? '', ['finalizing_part', 'rebuilding_part'], true)) {
                    if ($detail['stage'] === 'rebuilding_part') { $rebuilds++; }
                    file_put_contents($source, str_repeat('x', --$length));
                }
            });
        } catch (\WPMgr\Agent\Backup\ZipCloseException $e) { $failure = $e; }
        self::assertInstanceOf(\WPMgr\Agent\Backup\ZipCloseException::class, $failure);
        self::assertSame(2, $rebuilds);
        self::assertSame(3, $closes);
        self::assertGreaterThan(0, $failure->libzipStatus());
    }

    public function testSkippedSourcesAreAbsentFromEveryIncrementalInventory(): void
    {
        $source = $this->root . '/src/victim.txt';
        file_put_contents($source, 'victim');
        file_put_contents($this->root . '/src/keep.txt', 'keep');
        $mtime = filemtime($source);
        $result = (new FilesArchiver($this->root . '/src'))->archive($this->root . '/out', [], static function ($phase, $detail) use ($source): void {
            if (($detail['stage'] ?? '') === 'finalizing_part') { unlink($source); }
        }, ['victim.txt' => ['size' => 0, 'mtime' => 0]]);
        $baseline = FilesArchiver::loadPrevMap($this->root . '/out/files.list');
        self::assertArrayNotHasKey('victim.txt', $baseline);
        self::assertStringNotContainsString('victim.txt', file_get_contents($this->root . '/out/paths.cache'));
        self::assertSame(['keep.txt'], $this->entries($result));
        self::assertSame(1, $result['files_total']);
        self::assertSame(1, $result['files_changed']);
        self::assertSame(['victim.txt'], file($result['tombstones_file'], FILE_IGNORE_NEW_LINES));
        self::assertSame(1, $result['tombstones_count']);
        file_put_contents($source, 'victim');
        touch($source, $mtime);
        mkdir($this->root . '/next');
        $next = (new FilesArchiver($this->root . '/src'))->archive($this->root . '/next', [], static function (): void {}, $baseline);
        $zip = new \ZipArchive();
        self::assertTrue($zip->open($this->root . '/next/' . $next['parts'][0]));
        self::assertSame('victim', $zip->getFromName('victim.txt'));
        $zip->close();
        self::assertSame(1, $next['files_changed']);
        self::assertSame(1, $next['files_carried']);
    }

    public function testSourceAlreadyMissingFromCachedDiscoveryIsExcluded(): void
    {
        file_put_contents($this->root . '/src/keep.txt', 'keep');
        file_put_contents($this->root . '/out/paths.cache', "wp-content\tvictim.txt\nwp-content\tkeep.txt\n");
        file_put_contents($this->root . '/out/files.list', "victim.txt\t6\t1\nkeep.txt\t4\t1\n");
        $result = (new FilesArchiver($this->root . '/src'))->archive($this->root . '/out', ['total_files' => 2], static function (): void {});
        self::assertSame(['keep.txt'], $this->entries($result));
        self::assertSame(['victim.txt'], $result['skipped_sources']);
        self::assertSame(1, $result['files_total']);
        self::assertArrayNotHasKey('victim.txt', FilesArchiver::loadPrevMap($this->root . '/out/files.list'));
        self::assertStringNotContainsString('victim.txt', file_get_contents($this->root . '/out/paths.cache'));
    }

    public function testRecoveryUsesOnlyTheCurrentTombstoneInventory(): void
    {
        file_put_contents($this->root . '/src/keep.txt', 'keep');
        foreach ([false, true] as $currentDeletion) {
            $out = $this->root . '/tombstones-' . (int) $currentDeletion;
            mkdir($out);
            file_put_contents($out . '/tombstones.list', "keep.txt\n");
            $source = $this->root . '/src/victim.txt';
            file_put_contents($source, 'victim');
            $parent = ['victim.txt' => ['size' => 0, 'mtime' => 0]];
            if ($currentDeletion) { $parent['already-gone.txt'] = ['size' => 1, 'mtime' => 0]; }
            $result = (new FilesArchiver($this->root . '/src'))->archive($out, [], static function ($phase, $detail) use ($source): void {
                if (($detail['stage'] ?? '') === 'finalizing_part') { unlink($source); }
            }, $parent);
            $expected = $currentDeletion ? ['already-gone.txt', 'victim.txt'] : ['victim.txt'];
            self::assertSame($expected, file($result['tombstones_file'], FILE_IGNORE_NEW_LINES));
            self::assertSame(count($expected), $result['tombstones_count']);
        }
    }

    public function testSkippedSourcesReachProgressCompletionAndUploadedBaseline(): void
    {
        $this->signing();
        file_put_contents($this->root . '/src/keep.txt', 'keep');
        for ($i = 0; $i < 25; $i++) { file_put_contents($this->root . '/src/skip-' . $i . '.txt', 'skip'); }
        $GLOBALS['wpdb'] = new FakeBackupTasksWpdb();
        $params = array_replace($this->params(true), ['presign_endpoint' => 'https://cp.invalid/presign', 'manifest_endpoint' => 'https://cp.invalid/manifest']);
        $events = [];
        $uploadedBaseline = null;
        $manifest = null;
        $savedState = null;
        $puts = [];
        Functions\when('wp_remote_request')->alias(static function ($url, $args) use (&$puts) {
            $puts[basename($url)] = $args['body'];
            return ['response' => ['code' => 200], 'body' => ''];
        });
        Functions\when('wp_remote_retrieve_body')->alias(static fn ($response) => $response['body']);
        Functions\when('wp_remote_post')->alias(function ($url, $args) use (&$events, &$uploadedBaseline, &$manifest, &$savedState, $params) {
            $body = json_decode($args['body'], true);
            if ($url === $params['presign_endpoint']) {
                $uploadedBaseline = file_get_contents($this->root . '/out/files.list');
                $uploads = [];
                foreach ($body['hashes'] as $hash) { $uploads[$hash] = 'https://storage.invalid/' . $hash; }
                return ['response' => ['code' => 200], 'body' => json_encode(['uploads' => $uploads])];
            }
            if ($url === $params['manifest_endpoint']) {
                $manifest = $body;
                $savedState = $GLOBALS['wpdb']->rows[self::SNAPSHOT]['sub_state'];
                return ['response' => ['code' => 200], 'body' => json_encode(['ok' => true, 'chunk_count' => 0, 'stored_count' => 0])];
            }
            $events[] = $body;
            if (($body['phase_detail']['stage'] ?? '') === 'finalizing_part') {
                foreach (glob($this->root . '/src/skip-*.txt') as $source) { unlink($source); }
            }
            return ['response' => ['code' => 200], 'body' => '{}'];
        });
        self::assertSame('completed', (new TaskRunner($params))->run());
        $done = end($events);
        self::assertSame('completed', $done['phase']);
        self::assertSame(25, $done['phase_detail']['skipped_sources_count']);
        self::assertCount(20, $done['phase_detail']['skipped_sources']);
        $archiveDone = array_values(array_filter($events, static fn ($event) => $event['phase'] === 'archiving_files' && ($event['phase_detail']['done'] ?? false)))[0];
        self::assertSame(25, $archiveDone['phase_detail']['skipped_sources_count']);
        self::assertSame(1, $archiveDone['phase_detail']['files_total']);
        self::assertStringNotContainsString('skip-', $uploadedBaseline);
        self::assertStringContainsString('keep.txt', $uploadedBaseline);
        self::assertNotNull($manifest);
        $baselineEntry = array_values(array_filter($manifest['entries'], static fn ($entry) => $entry['entry_kind'] === 'files-list'))[0];
        $shipped = '';
        foreach ($baselineEntry['chunks'] as $chunk) { $shipped .= $puts[$chunk['blake3']]; }
        self::assertSame($uploadedBaseline, $shipped);
        self::assertSame(strlen($shipped), $baselineEntry['size']);
        $events = [];
        $GLOBALS['wpdb']->seedRow(self::SNAPSHOT, ['phase' => 'submitting_manifest', 'sub_state' => $savedState]);
        self::assertSame('completed', (new TaskRunner($params))->run());
        self::assertSame(25, end($events)['phase_detail']['skipped_sources_count']);
        self::assertCount(20, end($events)['phase_detail']['skipped_sources']);
    }

    public function testNonUtf8SkippedSourceCompletesAndReportsReplacementCharacter(): void
    {
        $events = [];
        $manifest = null;
        $params = $this->successfulBackupNetwork($events, $manifest);
        file_put_contents($this->root . '/src/keep.txt', 'keep');
        $bad = "uploads/caf\xe9.jpg";
        file_put_contents($this->root . '/out/paths.cache', "uploads\t" . $bad . "\nwp-content\tkeep.txt\n");
        file_put_contents($this->root . '/out/files.list', $bad . "\t1\t1\nkeep.txt\t4\t1\n");
        $state = ['files' => ['cache_file' => $this->root . '/out/paths.cache', 'total_files' => 2, 'file_index' => 0]];
        $GLOBALS['wpdb']->seedRow(self::SNAPSHOT, ['phase' => 'archiving_files', 'sub_state' => json_encode($state)]);
        self::assertSame('completed', (new TaskRunner($params))->run());
        self::assertNotNull($manifest);
        self::assertNotEmpty($events);
        self::assertSame('completed', end($events)['phase']);
        self::assertSame(["uploads/caf\u{FFFD}.jpg"], end($events)['phase_detail']['skipped_sources']);
        foreach ($GLOBALS['wpdb']->updates as $update) {
            if (!isset($update['data']['sub_state'])) { continue; }
            $saved = $update['data']['sub_state'];
            self::assertStringNotContainsString($bad, $saved);
            self::assertIsArray(json_decode($saved, true));
        }
    }

    public function testProgressSubstitutesInvalidUtf8CurrentFileAndMessage(): void
    {
        $events = [];
        $manifest = null;
        $params = $this->successfulBackupNetwork($events, $manifest);
        $client = new \WPMgr\Agent\Phpbu\ProgressClient($params['progress_endpoint'], self::SNAPSHOT, new \WPMgr\Agent\Signer(new Keystore()));
        self::assertTrue($client->post('archiving_files', ['current_file' => "caf\xe9.jpg", 'message' => "source \xff"]));
        self::assertSame("caf\u{FFFD}.jpg", $events[0]['phase_detail']['current_file']);
        self::assertSame("source \u{FFFD}", $events[0]['phase_detail']['message']);
    }

    public function testArchiverSamplesAreUtf8BeforeProgressAndResultLeaveIt(): void
    {
        $bad = "caf\xe9.jpg";
        file_put_contents($this->root . '/out/paths.cache', "wp-content\t" . $bad . "\n");
        file_put_contents($this->root . '/out/files.list', $bad . "\t1\t1\n");
        $details = [];
        $result = (new FilesArchiver($this->root . '/src'))->archive($this->root . '/out', ['total_files' => 1], static function ($phase, $detail) use (&$details): void { $details[] = $detail; });
        self::assertSame(["caf\u{FFFD}.jpg"], $result['skipped_sources']);
        self::assertSame(["caf\u{FFFD}.jpg"], end($details)['skipped_sources']);
    }

    public function testStateRoundTripsExactFilenameBytesInlineAndThroughSidecar(): void
    {
        $wpdb = new FakeBackupTasksWpdb();
        $GLOBALS['wpdb'] = $wpdb;
        $wpdb->seedRow(self::SNAPSHOT);
        $runner = new TaskRunner($this->params());
        $save = new \ReflectionMethod($runner, 'saveTaskState');
        foreach (['', str_repeat('padding', 12000)] as $padding) {
            $state = ['params' => ['wp_content_path' => "/site/caf\xe9", 'exclude_paths' => ["caf\xe9"]],
                'db' => ['output_path' => "/run/\xff/database.sql.gz"], 'files' => ['cache_file' => "/run/\xff/paths.cache", 'tombstones_file' => "/run/\xff/tombstones.list", 'tombstones' => ["bad\xff.txt"]],
                'upload' => ['scratch_dir' => "/run/\xff"], 'encrypt' => ['entries' => [['path' => "bad\xff.txt"]]], 'keyed' => ["bad\xfe" => "value\xff"], 'padding' => $padding];
            self::assertTrue($save->invoke($runner, 'archiving_files', $state));
            $raw = $wpdb->rows[self::SNAPSHOT]['sub_state'];
            self::assertNotNull(json_decode($raw, true));
            self::assertSame($state, TaskRunner::decodeSubStateColumn($raw));
        }
    }

    public function testCommandSeedPreservesExactResumePathBytes(): void
    {
        $GLOBALS['wpdb'] = new FakeBackupTasksWpdb();
        $params = array_replace($this->params(), ['wp_content_path' => "/site/caf\xe9", 'scratch_dir' => "/site/caf\xe9/run", 'exclude_paths' => ["caf\xe9"]]);
        $command = new \WPMgr\Agent\Commands\BackupCommand(new \WPMgr\Agent\Support\AgeIdentity(new Keystore()));
        (new \ReflectionMethod($command, 'seedTaskRow'))->invoke($command, self::SNAPSHOT, 'files', $params);
        self::assertSame(['params' => $params], TaskRunner::decodeSubStateColumn($GLOBALS['wpdb']->rows[self::SNAPSHOT]['sub_state']));
    }

    public function testWatchdogBreadcrumbPreservesByteEncodedState(): void
    {
        $GLOBALS['wpdb'] = new FakeBackupTasksWpdb();
        $GLOBALS['wpdb']->seedRow(self::SNAPSHOT);
        $runner = new TaskRunner($this->params());
        $state = ['params' => ['wp_content_path' => "/site/caf\xe9"]];
        self::assertTrue((new \ReflectionMethod($runner, 'saveTaskState'))->invoke($runner, 'archiving_files', $state));
        (new \ReflectionMethod(Watchdog::class, 'stampStage'))->invoke(null, self::SNAPSHOT, 'dispatch_entered');
        self::assertSame($state + ['stage' => 'dispatch_entered'], TaskRunner::decodeSubStateColumn($GLOBALS['wpdb']->rows[self::SNAPSHOT]['sub_state']));
    }

    public function testExistingUnreadableIncrementalSourceCarriesParentMetadata(): void
    {
        $this->assertExistingSkippedSourceCarriesParent(true);
    }

    public function testExistingRejectedIncrementalSourceCarriesParentMetadata(): void
    {
        $this->assertExistingSkippedSourceCarriesParent(false);
    }

    private function assertExistingSkippedSourceCarriesParent(bool $unreadableDuringClose): void
    {
        $source = $this->root . '/src/victim.txt';
        file_put_contents($source, 'changed content');
        file_put_contents($this->root . '/src/keep.txt', 'keep');
        $parent = ['victim.txt' => ['size' => 3, 'mtime' => 1]];
        $opts = $unreadableDuringClose ? [] : ['zip_factory' => static fn (): \ZipArchive => new class extends \ZipArchive {
            public function addFile(string $filepath, string $entryname = '', int $start = 0, int $length = 0, int $flags = \ZipArchive::FL_OVERWRITE): bool
            {
                return $entryname === 'victim.txt' ? false : parent::addFile($filepath, $entryname, $start, $length, $flags);
            }
        }];
        try {
            $result = (new FilesArchiver($this->root . '/src', [], $opts))->archive($this->root . '/out', [], static function ($phase, $detail) use ($source, $unreadableDuringClose): void {
                if ($unreadableDuringClose && ($detail['stage'] ?? '') === 'finalizing_part') { chmod($source, 0000); }
            }, $parent);
        } finally { chmod($source, 0600); }
        self::assertSame(1, $result['files_carried']);
        self::assertSame(0, $result['tombstones_count']);
        self::assertSame('', $result['tombstones_file']);
        self::assertSame(1, $result['skipped_sources_count']);
        self::assertSame(['keep.txt'], $this->entries($result));
        $baseline = FilesArchiver::loadPrevMap($this->root . '/out/files.list');
        self::assertSame($parent['victim.txt'], $baseline['victim.txt']);
        mkdir($this->root . '/next');
        $next = (new FilesArchiver($this->root . '/src'))->archive($this->root . '/next', [], static function (): void {}, $baseline);
        $zip = new \ZipArchive();
        self::assertTrue($zip->open($this->root . '/next/' . $next['parts'][0]));
        self::assertSame('changed content', $zip->getFromName('victim.txt'));
        $zip->close();
    }

    public function testUnreliableIdentityUsesDatabaseGuardAndRetainsCoordinationPath(): void
    {
        $events = [];
        $manifest = null;
        $params = $this->successfulBackupNetwork($events, $manifest);
        file_put_contents($this->root . '/src/keep.txt', 'keep');
        $checks = 0;
        \Patchwork\redefine(TaskRunner::class . '::fileLockMatchesPath', static function () use (&$checks): bool { return ++$checks > 3; });
        self::assertSame('completed', (new TaskRunner($params))->run());
        self::assertSame(2, $checks);
        $path = $this->root . '/' . self::SNAPSHOT . '.lock';
        self::assertFileExists($path);
        self::assertArrayNotHasKey('lock_identity_unverified', $events[0]['phase_detail']);
        $first = json_decode($GLOBALS['wpdb']->updates[0]['data']['sub_state'], true);
        self::assertSame('runner_started', $first['stage']);
        touch($path, time() - BackupJanitor::RUNS_GC_AGE_SECONDS - 1);
        $checks = 0;
        BackupArchiveBehaviorJanitor::$base = $this->root;
        BackupArchiveBehaviorJanitor::gcRuns();
        self::assertSame(2, $checks);
        self::assertFileExists($path);
    }

    public function testUnreliableIdentityCannotAuthorizeReclamation(): void
    {
        $GLOBALS['wpdb'] = new FakeBackupTasksWpdb();
        $GLOBALS['wpdb']->lockResponses = [null];
        \Patchwork\redefine(TaskRunner::class . '::fileLockMatchesPath', static fn (): bool => false);
        $invoked = false;
        self::assertFalse((new TaskRunner($this->params()))->withRunLock(static function () use (&$invoked): void { $invoked = true; }));
        self::assertFalse($invoked);
        self::assertFileExists($this->root . '/' . self::SNAPSHOT . '.lock');
    }

    private function successfulBackupNetwork(array &$events, ?array &$manifest): array
    {
        $this->signing();
        $GLOBALS['wpdb'] = new FakeBackupTasksWpdb();
        $params = array_replace($this->params(true), ['presign_endpoint' => 'https://cp.invalid/presign', 'manifest_endpoint' => 'https://cp.invalid/manifest']);
        Functions\when('wp_remote_request')->justReturn(['response' => ['code' => 200], 'body' => '']);
        Functions\when('wp_remote_retrieve_body')->alias(static fn ($response) => $response['body']);
        Functions\when('wp_remote_post')->alias(static function ($url, $args) use (&$events, &$manifest, $params) {
            $body = json_decode($args['body'], true);
            if ($url === $params['presign_endpoint']) {
                $uploads = [];
                foreach ($body['hashes'] as $hash) { $uploads[$hash] = 'https://storage.invalid/' . $hash; }
                return ['response' => ['code' => 200], 'body' => json_encode(['uploads' => $uploads])];
            }
            if ($url === $params['manifest_endpoint']) {
                $manifest = $body;
                return ['response' => ['code' => 200], 'body' => '{"ok":true,"chunk_count":0,"stored_count":0}'];
            }
            $events[] = $body;
            return ['response' => ['code' => 200], 'body' => '{}'];
        });
        return $params;
    }

    public function testHealthyFlockRejectsOverlappingRunner(): void
    {
        $trace = $this->overlap(false);
        self::assertSame(0, $trace['nested_finalizing']);
        self::assertSame(2, $trace['lock_probes']);
    }

    public function testUnavailableGuardsStopBeforeArchiving(): void
    {
        $trace = $this->overlap(true);
        self::assertSame(0, $trace['nested_finalizing']);
        self::assertSame(1, $trace['lock_probes']);
        self::assertSame([], $trace['events']);
        fwrite(STDOUT, "\noverlap=" . json_encode($trace) . "\n");
    }

    public function testUnavailableGuardsMustRejectOverlappingRunner(): void
    {
        $trace = $this->overlap(true);
        self::assertSame(0, $trace['nested_finalizing'], 'A second runner must not reach the same part while the first is finalizing.');
    }

    public function testGuardlessRunnerOnlyRecordsBreadcrumbOnStaleTask(): void
    {
        $this->assertGuardlessOwnerUnchanged(true);
    }

    public function testAcquireRetriesWhenOpenedLockPathDisappears(): void
    {
        $this->assertAcquireRetries(false, 1);
    }

    public function testGuardlessRunnerNeverUsesProgressAgeToFailTask(): void
    {
        $this->signing();
        mkdir($this->root . '/' . self::SNAPSHOT . '.lock');
        $now = time();
        \Patchwork\redefine('time', static fn (): int => $now);
        foreach ([Watchdog::STALL_THRESHOLD_SECONDS - 1, Watchdog::STALL_THRESHOLD_SECONDS, Watchdog::STALL_THRESHOLD_SECONDS + 1, null] as $age) {
            $wpdb = new FakeBackupTasksWpdb();
            $wpdb->lockResponses = [null];
            if ($age !== null) { $wpdb->seedRow(self::SNAPSHOT, ['phase' => 'archiving_files', 'last_progress_at' => $now - $age]); }
            $GLOBALS['wpdb'] = $wpdb;
            $events = [];
            Functions\when('wp_remote_post')->alias(static function ($url, $args) use (&$events) {
                $events[] = json_decode($args['body'], true);
                return ['response' => ['code' => 200], 'body' => '{}'];
            });
            self::assertSame($age === null ? 'queued' : 'archiving_files', (new TaskRunner($this->params(true)))->run());
            self::assertSame([], $events);
            if ($age === null) {
                self::assertSame([], $wpdb->rows);
            } else {
                self::assertSame('archiving_files', $wpdb->rows[self::SNAPSHOT]['phase']);
                self::assertSame('lock_unavailable', json_decode($wpdb->rows[self::SNAPSHOT]['sub_state'], true)['stage']);
                self::assertSame($now - $age, $wpdb->rows[self::SNAPSHOT]['last_progress_at']);
            }
        }
    }

    public function testContendedBreadcrumbPreservesNewOwnerCheckpoint(): void
    {
        $this->assertBreadcrumbPreservesNewOwnerCheckpoint('lock_contended');
    }

    public function testUnavailableBreadcrumbPreservesNewOwnerCheckpoint(): void
    {
        $this->assertBreadcrumbPreservesNewOwnerCheckpoint('lock_unavailable');
    }

    public function testContendedBreadcrumbPreservesCaseOnlyOwnerCheckpoint(): void
    {
        $this->assertBreadcrumbPreservesNewOwnerCheckpoint('lock_contended', 'OLD');
    }

    public function testUnavailableBreadcrumbPreservesCaseOnlyOwnerCheckpoint(): void
    {
        $this->assertBreadcrumbPreservesNewOwnerCheckpoint('lock_unavailable', 'OLD');
    }

    private function assertBreadcrumbPreservesNewOwnerCheckpoint(string $stage, string $owner = 'new'): void
    {
        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->lockResponses = [$stage === 'lock_contended' ? '0' : null];
        $wpdb->seedRow(self::SNAPSHOT, ['phase' => 'archiving_files', 'sub_state' => '{"owner":"old"}']);
        $GLOBALS['wpdb'] = $wpdb;
        if ($stage === 'lock_unavailable') { mkdir($this->root . '/' . self::SNAPSHOT . '.lock'); }
        $id = self::SNAPSHOT;
        $raced = false;
        $newTime = time() + 1;
        $newState = json_encode(['owner' => $owner]);
        \Patchwork\redefine(FakeBackupTasksWpdb::class . '::get_var', function ($prepared) use ($wpdb, $id, $newState, $newTime, &$raced) {
            $raw = \Patchwork\relay();
            $query = json_decode($prepared, true);
            if (str_contains($query['sql'] ?? '', 'SELECT sub_state FROM')) {
                $raced = true;
                $wpdb->rows[$id]['sub_state'] = $newState;
                $wpdb->rows[$id]['last_progress_at'] = $newTime;
            }
            return $raw;
        });
        (new TaskRunner($this->params()))->run();
        self::assertTrue($raced);
        self::assertSame($newState, $wpdb->rows[$id]['sub_state']);
        self::assertSame($newTime, $wpdb->rows[$id]['last_progress_at']);
        self::assertSame('archiving_files', $wpdb->rows[$id]['phase']);
    }

    public function testUnguardedContenderPreservesFreshFileLockOwner(): void
    {
        $this->assertGuardlessOwnerUnchanged(false);
    }

    public function testGuardlessContenderPreservesOwnerBlockedPastStallThreshold(): void
    {
        $this->assertGuardlessOwnerUnchanged(true);
    }

    public function testGuardlessContenderPreservesOwnerBeforeTaskSeed(): void
    {
        $this->signing();
        $path = $this->root . '/' . self::SNAPSHOT . '.lock';
        $holder = fopen($path, 'c');
        self::assertTrue(flock($holder, LOCK_EX | LOCK_NB));
        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->lockResponses = [null];
        $GLOBALS['wpdb'] = $wpdb;
        $events = [];
        Functions\when('wp_remote_post')->alias(static function ($url, $args) use (&$events) {
            $events[] = json_decode($args['body'], true);
            return ['response' => ['code' => 200], 'body' => '{}'];
        });
        \Patchwork\redefine('fopen', static function ($filename, ...$args) use ($path) { return $filename === $path ? false : \Patchwork\relay(); });
        file_put_contents($this->root . '/out/owner-data', 'retain');
        try {
            self::assertSame('queued', (new TaskRunner($this->params(true)))->run());
            self::assertSame([], $wpdb->rows);
            self::assertSame([], $events);
            self::assertFileExists($this->root . '/out/owner-data');
        } finally { flock($holder, LOCK_UN); fclose($holder); }
    }

    public function testDiagnosticPathRemovalRetriesAndAcquiresCurrentInode(): void
    {
        $this->assertDiagnosticRemovalRetries(1);
    }

    public function testDiagnosticPathRemovalExhaustionIsContention(): void
    {
        $this->assertDiagnosticRemovalRetries(3);
    }

    private function assertDiagnosticRemovalRetries(int $disruptions): void
    {
        $path = $this->root . '/' . self::SNAPSHOT . '.lock';
        $attempts = 0;
        $opened = [];
        \Patchwork\redefine('fopen', static function ($filename, ...$args) use ($path, $disruptions, &$attempts, &$opened) {
            $handle = \Patchwork\relay();
            if ($filename === $path && is_resource($handle)) {
                $opened[] = $handle;
                if ($args[0] === 'c') {
                    $attempts++;
                    if ($attempts <= $disruptions) {
                        unlink($path);
                        file_put_contents($path, 'replacement');
                    }
                } elseif ($args[0] === 'r+') {
                    unlink($path);
                }
            }
            return $handle;
        });
        $lock = TaskRunner::acquireFileLockAtPath($path);
        try {
            if ($disruptions === 3) {
                self::assertNull($lock['handle']);
                self::assertTrue($lock['contended'], 'Replacement during diagnosis must stay contention.');
            } else {
                self::assertIsResource($lock['handle']);
                self::assertTrue(TaskRunner::fileLockMatchesPath($lock['handle'], $path));
            }
            self::assertSame(min(3, $disruptions + 1), $attempts);
            foreach ($opened as $handle) {
                if ($handle !== $lock['handle']) { self::assertFalse(is_resource($handle)); }
            }
        } finally {
            if (is_resource($lock['handle'])) { flock($lock['handle'], LOCK_UN); fclose($lock['handle']); }
        }
    }

    public function testUnsupportedFlockIsUnavailableAndClosesHandle(): void
    {
        $this->assertFlockFailure(false);
    }

    public function testWouldBlockFlockIsContentionAndClosesHandle(): void
    {
        $this->assertFlockFailure(true);
    }

    private function assertFlockFailure(bool $blocked): void
    {
        $path = $this->root . '/' . self::SNAPSHOT . '.lock';
        $opened = null;
        \Patchwork\redefine('WPMgr\\Agent\\Backup\\flock', static function ($handle, $operation, &$wouldBlock = null) use ($path, $blocked, &$opened) {
            if (($operation & LOCK_EX) && stream_get_meta_data($handle)['uri'] === $path) {
                $opened = $handle;
                $wouldBlock = (int) $blocked;
                return false;
            }
            return \Patchwork\relay();
        });
        $lock = TaskRunner::acquireFileLockAtPath($path);
        self::assertNull($lock['handle']);
        self::assertSame($blocked, $lock['contended']);
        self::assertNotNull($opened);
        self::assertFalse(is_resource($opened));
    }

    public function testFreshOpenIdentityMismatchDegradesAndReleasesHandles(): void
    {
        $path = $this->root . '/' . self::SNAPSHOT . '.lock';
        $opened = [];
        \Patchwork\redefine('fopen', static function ($filename, ...$args) use ($path, &$opened) {
            $handle = \Patchwork\relay();
            if ($filename === $path) { $opened[] = $handle; }
            return $handle;
        });
        \Patchwork\redefine(TaskRunner::class . '::fileLockMatchesPath', static fn (): bool => false);
        $lock = TaskRunner::acquireFileLockAtPath($path);
        try {
            self::assertNull($lock['handle']);
            self::assertFalse($lock['contended']);
            self::assertCount(2, $opened);
            foreach ($opened as $handle) { self::assertFalse(is_resource($handle)); }
            $probe = fopen($path, 'r+');
            try { self::assertTrue(flock($probe, LOCK_EX | LOCK_NB)); }
            finally { fclose($probe); }
        } finally {
            if (is_resource($lock['handle'])) { flock($lock['handle'], LOCK_UN); fclose($lock['handle']); }
        }
    }

    private function assertGuardlessOwnerUnchanged(bool $stale): void
    {
        $this->signing();
        $path = $this->root . '/' . self::SNAPSHOT . '.lock';
        $holder = fopen($path, 'c');
        self::assertTrue(flock($holder, LOCK_EX | LOCK_NB));
        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->lockResponses = [null];
        $wpdb->seedRow(self::SNAPSHOT, ['phase' => 'archiving_files', 'sub_state' => '{"owner_cursor":"retain"}', 'last_progress_at' => time() - ($stale ? Watchdog::STALL_THRESHOLD_SECONDS + 1 : 0)]);
        $before = $wpdb->rows[self::SNAPSHOT];
        $GLOBALS['wpdb'] = $wpdb;
        $events = [];
        Functions\when('wp_remote_post')->alias(static function ($url, $args) use (&$events) {
            $events[] = json_decode($args['body'], true);
            return ['response' => ['code' => 200], 'body' => '{}'];
        });
        \Patchwork\redefine('fopen', static function ($filename, ...$args) use ($path) {
            return $filename === $path ? false : \Patchwork\relay();
        });
        file_put_contents($this->root . '/out/owner-data', 'retain');
        try {
            self::assertSame('archiving_files', (new TaskRunner($this->params(true)))->run());
            $after = $wpdb->rows[self::SNAPSHOT];
            self::assertSame($before['phase'], $after['phase']);
            self::assertSame($before['last_progress_at'], $after['last_progress_at']);
            self::assertSame($before['started_at'], $after['started_at']);
            self::assertSame(['owner_cursor' => 'retain', 'stage' => 'lock_unavailable'], json_decode($after['sub_state'], true));
            self::assertSame([], $events);
            self::assertFileExists($this->root . '/out/owner-data');
            foreach ($wpdb->updates as $update) { self::assertSame(['sub_state'], array_keys($update['data'])); }
        } finally { flock($holder, LOCK_UN); fclose($holder); }
    }

    public function testUnavailableGuardsDoNotPublishFailureWithoutTaskStorage(): void
    {
        $this->signing();
        unset($GLOBALS['wpdb']);
        mkdir($this->root . '/' . self::SNAPSHOT . '.lock');
        $events = [];
        Functions\when('wp_remote_post')->alias(static function ($url, $args) use (&$events) {
            $events[] = json_decode($args['body'], true);
            return ['response' => ['code' => 200], 'body' => '{}'];
        });
        self::assertSame(TaskRunner::PHASE_QUEUED, (new TaskRunner($this->params(true)))->run());
        self::assertSame([], $events);
    }

    public function testAcquireRetriesWhenOpenedLockPathNamesAnotherInode(): void
    {
        $this->assertAcquireRetries(true, 1);
    }

    public function testAcquireRetriesTwiceThenKeepsVerifiedIdentity(): void
    {
        $this->assertAcquireRetries(true, 2);
    }

    public function testAcquireStopsAfterThreeReplacedInodes(): void
    {
        $this->assertAcquireRetries(true, 3);
    }

    private function assertAcquireRetries(bool $replace, int $disruptions): void
    {
        $runner = new TaskRunner($this->params());
        $path = $this->root . '/' . self::SNAPSHOT . '.lock';
        $opened = [];
        \Patchwork\redefine('fopen', static function ($filename, ...$args) use ($path, $replace, $disruptions, &$opened) {
            $handle = \Patchwork\relay();
            if ($filename === $path && ($args[0] ?? '') === 'c' && is_resource($handle)) {
                $opened[] = $handle;
                if (count($opened) <= $disruptions) {
                    unlink($path);
                    if ($replace) { file_put_contents($path, 'replacement'); }
                }
            }
            return $handle;
        });
        $lock = (new \ReflectionMethod($runner, 'acquireFileLock'))->invoke($runner);
        try {
            if ($disruptions === 3) {
                self::assertNull($lock['handle']);
                self::assertTrue($lock['contended']);
                self::assertCount(3, $opened);
            } else {
                self::assertIsResource($lock['handle']);
                self::assertFalse($lock['contended']);
                self::assertCount($disruptions + 1, $opened);
                clearstatcache(true, $path);
                self::assertSame(stat($path)['ino'], fstat($lock['handle'])['ino']);
                self::assertSame(stat($path)['dev'], fstat($lock['handle'])['dev']);
            }
            foreach (array_slice($opened, 0, $disruptions) as $stale) { self::assertFalse(is_resource($stale)); }
        } finally {
            if (is_resource($lock['handle'])) { flock($lock['handle'], LOCK_UN); fclose($lock['handle']); }
        }
    }

    public function testTerminalCleanupUnlinksWhileHoldingTheLock(): void
    {
        $wpdb = new FakeBackupTasksWpdb();
        $GLOBALS['wpdb'] = $wpdb;
        $path = $this->root . '/' . self::SNAPSHOT . '.lock';
        $checked = false;
        Functions\when('wp_delete_file')->alias(static function ($file) use ($path, &$checked): void {
            if ($file === $path) {
                $probe = fopen($file, 'r+');
                try { self::assertFalse(flock($probe, LOCK_EX | LOCK_NB)); }
                finally { fclose($probe); }
                $checked = true;
            }
            unlink($file);
        });
        self::assertTrue((new TaskRunner($this->params()))->withRunLock(static function (): void {}));
        self::assertTrue($checked);
        self::assertFileDoesNotExist($path);
    }

    public function testCleanupPreservesAReplacementLockInode(): void
    {
        $wpdb = new FakeBackupTasksWpdb();
        $GLOBALS['wpdb'] = $wpdb;
        $path = $this->root . '/' . self::SNAPSHOT . '.lock';
        $replacement = null;
        try {
            self::assertTrue((new TaskRunner($this->params()))->withRunLock(static function () use ($path, &$replacement): void {
                unlink($path);
                $replacement = fopen($path, 'c');
                self::assertTrue(flock($replacement, LOCK_EX | LOCK_NB));
            }));
            self::assertFileExists($path);
        } finally {
            if (is_resource($replacement)) { flock($replacement, LOCK_UN); fclose($replacement); }
        }
    }

    public function testJanitorRetainsHeldOldLockAndRemovesUnheldOldLock(): void
    {
        $GLOBALS['wpdb'] = new FakeBackupTasksWpdb();
        $held = $this->root . '/' . self::SNAPSHOT . '.lock';
        $free = $this->root . '/bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee.lock';
        $holder = fopen($held, 'c');
        self::assertTrue(flock($holder, LOCK_EX | LOCK_NB));
        file_put_contents($free, '');
        foreach ([$held, $free] as $path) { touch($path, time() - BackupJanitor::RUNS_GC_AGE_SECONDS - 1); }
        $deletedWhileHeld = false;
        Functions\when('wp_delete_file')->alias(static function ($path) use ($free, &$deletedWhileHeld): void {
            self::assertSame($free, $path);
            $probe = fopen($path, 'r+');
            try { self::assertFalse(flock($probe, LOCK_EX | LOCK_NB)); }
            finally { fclose($probe); }
            $deletedWhileHeld = true;
            unlink($path);
        });
        BackupArchiveBehaviorJanitor::$base = $this->root;
        try {
            BackupArchiveBehaviorJanitor::gcRuns();
            self::assertFileExists($held);
            self::assertFileDoesNotExist($free);
            self::assertTrue($deletedWhileHeld);
        } finally { flock($holder, LOCK_UN); fclose($holder); }
    }

    public function testJanitorRetainsFreshReplacementAfterInodeRetry(): void
    {
        $GLOBALS['wpdb'] = new FakeBackupTasksWpdb();
        $path = $this->root . '/' . self::SNAPSHOT . '.lock';
        file_put_contents($path, 'old');
        touch($path, time() - BackupJanitor::RUNS_GC_AGE_SECONDS - 1);
        $opens = 0;
        \Patchwork\redefine('fopen', static function ($filename, ...$args) use ($path, &$opens) {
            $handle = \Patchwork\relay();
            if ($filename === $path && ++$opens === 1) {
                unlink($path);
                file_put_contents($path, 'fresh');
            }
            return $handle;
        });
        BackupArchiveBehaviorJanitor::$base = $this->root;
        BackupArchiveBehaviorJanitor::gcRuns();
        self::assertSame(3, $opens);
        self::assertSame('fresh', file_get_contents($path));
    }

    public function testLockCleanupDoesNotAdmitOverlappingRunners(): void
    {
        $trace = $this->lockCleanupOverlap();
        self::assertSame(0, $trace['overlapping_finalizing']);
        fwrite(STDOUT, "\nlock cleanup overlap=" . json_encode($trace) . "\n");
    }

    public function testReleasedLockCleanupMustPreserveNextOwner(): void
    {
        $trace = $this->lockCleanupOverlap();
        self::assertSame(0, $trace['overlapping_finalizing'], 'The lock pathname must continue to coordinate runners after ownership changes.');
    }

    public function testJanitorRetainsHeldOldLockAndRejectsAnotherOwner(): void
    {
        $trace = $this->oldLockJanitor();
        self::assertTrue($trace['lock_path_exists']);
        self::assertFalse($trace['another_owner_entered']);
        fwrite(STDOUT, "\nold lock janitor=" . json_encode($trace) . "\n");
    }

    public function testJanitorMustRetainHeldOldLock(): void
    {
        $trace = $this->oldLockJanitor();
        self::assertTrue($trace['lock_path_exists'], 'A held lock must retain a stable pathname for every contender.');
    }

    public function testEveryWatchdogEntryPointHonorsHeldFlock(): void
    {
        file_put_contents($this->root . '/src/marker.txt', 'marker');
        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->lockResponses = ['1'];
        $params = $this->params();
        $GLOBALS['wpdb'] = $wpdb;
        $holder = fopen($this->root . '/' . self::SNAPSHOT . '.lock', 'c');
        self::assertTrue(flock($holder, LOCK_EX | LOCK_NB));
        try {
            foreach (['dispatch', 'run', 'resumeIfStalled', 'sweepStalled'] as $entry) {
                $wpdb->seedRow(self::SNAPSHOT, ['last_progress_at' => time() - Watchdog::STALL_THRESHOLD_SECONDS - 1, 'sub_state' => json_encode(['params' => $params])]);
                if ($entry === 'sweepStalled') { Watchdog::sweepStalled(); }
                else { Watchdog::$entry(self::SNAPSHOT); }
                self::assertSame('queued', $wpdb->rows[self::SNAPSHOT]['phase'], $entry);
                self::assertFileDoesNotExist($this->root . '/out/paths.cache', $entry);
            }
        } finally {
            flock($holder, LOCK_UN);
            fclose($holder);
        }
        self::assertSame(4, $wpdb->lockCallCount);
        fwrite(STDOUT, "\nwatchdog dispatch/run/resumeIfStalled/sweepStalled: all rejected by held flock\n");
    }

    public function testSilentDumpStallResumerCannotPassHeldFlock(): void
    {
        $wpdb = new FakeBackupTasksWpdb();
        $params = $this->params();
        $wpdb->seedRow(self::SNAPSHOT, ['phase' => 'dumping_db', 'last_progress_at' => time() - Watchdog::STALL_THRESHOLD_SECONDS - 1,
            'sub_state' => json_encode(['params' => $params])]);
        $GLOBALS['wpdb'] = $wpdb;
        $holder = fopen($this->root . '/' . self::SNAPSHOT . '.lock', 'c');
        self::assertTrue(flock($holder, LOCK_EX | LOCK_NB));
        try { Watchdog::resumeIfStalled(self::SNAPSHOT); }
        finally { flock($holder, LOCK_UN); fclose($holder); }
        self::assertSame('dumping_db', $wpdb->rows[self::SNAPSHOT]['phase']);
        self::assertSame(1, $wpdb->rows[self::SNAPSHOT]['resume_count']);
        self::assertFileDoesNotExist($this->root . '/out/database.sql.gz');
        fwrite(STDOUT, "\nsilent dump stall: resume_count=1 phase=dumping_db second_worker_blocked=true\n");
    }

    public function testRequestSweepRunsBackupBeforeReturningToRequest(): void
    {
        $this->signing();
        $this->options[Settings::OPTION_SITE_ID] = 'test-site';
        $this->options[Settings::OPTION_CP_URL] = 'https://cp.invalid';
        file_put_contents($this->root . '/src/marker.txt', 'marker');
        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->seedRow(self::SNAPSHOT, ['last_progress_at' => time() - Watchdog::STALL_THRESHOLD_SECONDS - 1,
            'sub_state' => json_encode(['params' => $this->params(true)])]);
        $GLOBALS['wpdb'] = $wpdb;
        $events = ['request_enter'];
        Functions\when('wp_remote_post')->alias(static function ($url, $args) use (&$events) {
            $event = json_decode($args['body'], true);
            $events[] = $event['phase'];
            return ['response' => ['code' => 200], 'body' => '{}'];
        });
        $reflection = new \ReflectionClass(Plugin::class);
        $plugin = $reflection->newInstanceWithoutConstructor();
        $reflection->getProperty('settings')->setValue($plugin, new Settings());
        $plugin->maybeSweepStalledBackups();
        $events[] = 'request_return';
        self::assertContains('archiving_files', $events);
        self::assertContains('failed', $events);
        self::assertSame('request_return', end($events));
        fwrite(STDOUT, "\nrequest sweep sequence=" . json_encode($events) . "\n");
    }

    public function testFailureWirePayloadAndCleanup(): void
    {
        $this->signing();
        file_put_contents($this->root . '/src/victim.txt', 'victim');
        $wpdb = new FakeBackupTasksWpdb();
        $GLOBALS['wpdb'] = $wpdb;
        $events = [];
        Functions\when('wp_remote_post')->alias(function ($url, $args) use (&$events) {
            $event = json_decode($args['body'], true);
            $events[] = $event;
            if (($event['phase_detail']['stage'] ?? '') === 'finalizing_part') {
                rename($this->root . '/out', $this->root . '/out-held');
            }
            return ['response' => ['code' => 200], 'body' => '{}'];
        });
        set_error_handler(static fn () => true);
        try { $phase = (new TaskRunner($this->params(true)))->run(); }
        finally { restore_error_handler(); }
        self::assertSame(TaskRunner::PHASE_FAILED, $phase);
        $failure = end($events);
        self::assertSame('failed', $failure['phase']);
        self::assertSame('archiving_files', $failure['phase_detail']['stage']);
        self::assertStringContainsString('zip close failed (Failure to create temporary file', $failure['phase_detail']['message']);
        self::assertSame('wp-content.g000.part001.zip', $failure['phase_detail']['part']);
        self::assertSame(\ZipArchive::ER_TMPOPEN, $failure['phase_detail']['libzip_status']);
        self::assertStringNotContainsString($this->root, $failure['phase_detail']['message']);
        self::assertDirectoryDoesNotExist($this->root . '/out');
        self::assertArrayNotHasKey(self::SNAPSHOT, $wpdb->rows);
        fwrite(STDOUT, "\nfailure payload=" . json_encode($failure) . " scratch_removed=true task_removed=true\n");
    }

    public function testTimingAppearsOnlyInFirstFreshOrResumedWorkerReport(): void
    {
        $this->signing();
        file_put_contents($this->root . '/src/keep.txt', 'keep');
        $timing = ['received_at' => time() - 30, 'preflight_done_at' => time() - 20, 'claimed_at' => time() - 10];
        foreach (['queued', 'archiving_files'] as $phase) {
            $params = $this->params(true) + ['timing' => $timing];
            $wpdb = new FakeBackupTasksWpdb();
            $wpdb->seedRow(self::SNAPSHOT, ['phase' => $phase, 'sub_state' => json_encode(['params' => $params])]);
            $GLOBALS['wpdb'] = $wpdb;
            $events = [];
            Functions\when('wp_remote_post')->alias(static function ($url, $args) use (&$events) {
                $events[] = json_decode($args['body'], true);
                return ['response' => ['code' => 200], 'body' => '{}'];
            });
            $before = time();
            (new TaskRunner($params))->run();
            self::assertGreaterThan(1, count($events));
            $firstTiming = $events[0]['phase_detail']['timing'] ?? null;
            self::assertIsArray($firstTiming);
            self::assertSame($timing, array_intersect_key($firstTiming, $timing));
            self::assertIsInt($firstTiming['worker_started_at']);
            self::assertGreaterThanOrEqual($before, $firstTiming['worker_started_at']);
            self::assertLessThanOrEqual(time(), $firstTiming['worker_started_at']);
            foreach (array_slice($events, 1) as $event) { self::assertArrayNotHasKey('timing', $event['phase_detail']); }
        }
    }

    public function testEarlyWorkerFailureIncludesTiming(): void
    {
        $this->signing();
        unset($GLOBALS['wpdb']);
        $params = $this->params(true) + ['timing' => ['received_at' => 100, 'preflight_done_at' => 101, 'claimed_at' => 102]];
        $events = [];
        Functions\when('wp_remote_post')->alias(static function ($url, $args) use (&$events) {
            $events[] = json_decode($args['body'], true);
            return ['response' => ['code' => 200], 'body' => '{}'];
        });
        self::assertSame('failed', (new TaskRunner($params))->run());
        self::assertCount(1, $events);
        self::assertSame($params['timing'] + ['worker_started_at' => $events[0]['phase_detail']['timing']['worker_started_at']], $events[0]['phase_detail']['timing']);
    }

    private function destinationCloseFailure(): array
    {
        mkdir($this->root . '/src/uploads');
        $source = $this->root . '/src/uploads/victim.txt';
        file_put_contents($source, 'abcdefgh');
        $warnings = [];
        set_error_handler(static function ($errno, $message) use (&$warnings): bool { $warnings[] = $message; return true; });
        try {
            (new FilesArchiver($this->root . '/src'))->archive($this->root . '/out', [], static function ($phase, $detail) use ($source): void {
                if (($detail['stage'] ?? '') === 'finalizing_part') { chmod(dirname($source, 3) . '/out', 0500); }
            });
        } catch (\RuntimeException $e) {
            return [$e->getMessage(), $warnings];
        } finally {
            chmod($this->root . '/out', 0700);
            restore_error_handler();
        }
        self::fail('Expected close failure for an unwritable destination.');
    }

    private function overlap(bool $degraded): array
    {
        $this->signing();
        file_put_contents($this->root . '/src/marker.txt', 'marker');
        if ($degraded) { mkdir($this->root . '/' . self::SNAPSHOT . '.lock'); }
        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->lockResponses = [null];
        $GLOBALS['wpdb'] = $wpdb;
        $params = $this->params(true);
        $active = false;
        $triggered = false;
        $nestedFinalizing = 0;
        $events = [];
        Functions\when('wp_remote_post')->alias(function ($url, $args) use ($params, &$active, &$triggered, &$nestedFinalizing, &$events) {
            $event = json_decode($args['body'], true);
            $events[] = ['worker' => $active ? 'B' : 'A', 'phase' => $event['phase'], 'stage' => $event['phase_detail']['stage'] ?? null];
            if (($event['phase_detail']['stage'] ?? '') === 'finalizing_part') {
                if ($active) { $nestedFinalizing++; }
                elseif (!$triggered) {
                    $triggered = true;
                    $active = true;
                    (new TaskRunner($params))->run();
                    $active = false;
                    if (is_file($this->root . '/src/marker.txt')) { unlink($this->root . '/src/marker.txt'); }
                }
            }
            return ['response' => ['code' => 200], 'body' => '{}'];
        });
        set_error_handler(static fn () => true);
        try { (new TaskRunner($params))->run(); }
        finally { restore_error_handler(); }
        return ['nested_finalizing' => $nestedFinalizing, 'lock_probes' => $wpdb->lockCallCount, 'events' => $events];
    }

    private function signing(): void
    {
        $this->options = [];
        Functions\when('get_option')->alias(fn ($name, $default = false) => $this->options[$name] ?? $default);
        Functions\when('update_option')->alias(function ($name, $value) { $this->options[$name] = $value; return true; });
        if (!defined('WPMGR_AGENT_KEY_FILE')) { define('WPMGR_AGENT_KEY_FILE', __DIR__ . '/issue954-signing.key'); }
        self::assertSame(__DIR__ . '/issue954-signing.key', WPMGR_AGENT_KEY_FILE, 'Run this issue-specific test in its own PHP process.');
        $this->createdKey = !is_file(WPMGR_AGENT_KEY_FILE);
        (new Keystore())->generateSiteKeypair();
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
    }

    private function lockCleanupOverlap(): array
    {
        $this->signing();
        file_put_contents($this->root . '/src/marker.txt', 'marker');
        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->lockResponses = [null];
        $GLOBALS['wpdb'] = $wpdb;
        $params = $this->params(true);
        $lockPath = $this->root . '/' . self::SNAPSHOT . '.lock';
        $worker = 'A';
        $triggered = false;
        $trace = ['overlapping_finalizing' => 0, 'owner_inode' => null, 'new_inode' => null];
        $owner = new \Fiber(static function () use ($params): void { (new TaskRunner($params))->run(); });
        Functions\when('wp_remote_post')->alias(function ($url, $args) use (&$worker, &$trace, $lockPath, $owner) {
            $event = json_decode($args['body'], true);
            if (($event['phase_detail']['stage'] ?? '') === 'finalizing_part') {
                clearstatcache(true, $lockPath);
                if ($worker === 'B') {
                    $trace['owner_inode'] = fileinode($lockPath);
                    \Fiber::suspend();
                } elseif ($worker === 'C' && $owner->isSuspended()) {
                    $trace['overlapping_finalizing']++;
                    $trace['new_inode'] = fileinode($lockPath);
                }
            }
            return ['response' => ['code' => 200], 'body' => '{}'];
        });
        Functions\when('wp_delete_file')->alias(function ($path) use (&$worker, &$triggered, $lockPath, $owner, $params) {
            if ($path === $lockPath && !$triggered) {
                $triggered = true;
                $worker = 'B';
                $owner->start();
                if (!$owner->isSuspended()) {
                    $worker = 'A';
                    return unlink($path);
                }
                unlink($path);
                $worker = 'C';
                (new TaskRunner($params))->run();
                $worker = 'B';
                $owner->resume();
                $worker = 'A';
                return true;
            }
            return @unlink($path);
        });
        set_error_handler(static fn () => true);
        try { self::assertSame(TaskRunner::PHASE_FAILED, (new TaskRunner($params))->run()); }
        finally { restore_error_handler(); }
        $trace['lock_probes'] = $wpdb->lockCallCount;
        return $trace;
    }

    private function oldLockJanitor(): array
    {
        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->lockResponses = [null];
        $GLOBALS['wpdb'] = $wpdb;
        $lockPath = $this->root . '/' . self::SNAPSHOT . '.lock';
        $holder = fopen($lockPath, 'c');
        self::assertTrue(flock($holder, LOCK_EX | LOCK_NB));
        touch($lockPath, time() - BackupJanitor::RUNS_GC_AGE_SECONDS - 1);
        BackupArchiveBehaviorJanitor::$base = $this->root;
        try {
            BackupArchiveBehaviorJanitor::gcRuns();
            clearstatcache(true, $lockPath);
            $pathExists = is_file($lockPath);
            $entered = false;
            (new TaskRunner($this->params()))->withRunLock(static function () use (&$entered): void { $entered = true; });
            return ['age_threshold_seconds' => BackupJanitor::RUNS_GC_AGE_SECONDS, 'lock_path_exists' => $pathExists, 'another_owner_entered' => $entered];
        } finally {
            flock($holder, LOCK_UN);
            fclose($holder);
        }
    }

    private function params(bool $progress = false): array
    {
        return ['snapshot_id' => self::SNAPSHOT, 'kind' => 'files', 'scratch_dir' => $this->root . '/out',
            'wp_content_path' => $this->root . '/src', 'progress_endpoint' => $progress ? 'https://cp.invalid/progress' : '',
            'presign_endpoint' => '', 'manifest_endpoint' => '', 'age_recipient' => '', 'db' => ['prefix' => 'wp_']];
    }

    private function selectionTree(string $segment = '2024'): array
    {
        $expected = [];
        foreach (['uploads', 'plugins/acme', 'themes/acme', 'other'] as $group) {
            mkdir($this->root . '/src/' . $group . '/' . $segment, 0700, true);
            foreach ([$segment . '/a.jpg', 'keep.jpg', 'x.log'] as $file) {
                file_put_contents($this->root . '/src/' . $group . '/' . $file, $file);
            }
            $expected[] = $group . '/keep.jpg';
        }
        sort($expected);
        return $expected;
    }

    private function entries(array $result): array
    {
        $entries = [];
        foreach ($result['parts'] as $part) {
            $zip = new \ZipArchive();
            self::assertTrue($zip->open($this->root . '/out/' . $part));
            for ($i = 0; $i < $zip->numFiles; $i++) { $entries[] = $zip->getNameIndex($i); }
            $zip->close();
        }
        sort($entries);
        return $entries;
    }

    private function removeTree(string $dir): void
    {
        if (!is_dir($dir)) { return; }
        foreach (new \FilesystemIterator($dir) as $file) {
            if ($file->isDir() && !$file->isLink()) { $this->removeTree($file->getPathname()); }
            else { unlink($file->getPathname()); }
        }
        rmdir($dir);
    }
}

final class BackupArchiveBehaviorJanitor extends BackupJanitor
{
    public static string $base = '';
    protected function runsBaseDir(): string { return self::$base; }
}
