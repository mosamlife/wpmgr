<?php
/**
 * GH #954 / #983: a fresh archive pass restarts the progress marks, and its
 * first checkpoint still counts as progress.
 *
 * When a saved checkpoint no longer validates (an incremental backup whose
 * previous file list cannot be fetched on resume flips its mode, say), the
 * archive starts one fresh pass under a new discovery id, and its first closed
 * parts have lower marks than the pass it replaced. The durable progress
 * record must still advance at that first checkpoint: its sequence number and
 * time feed the hand-over decision, the watchdog's resume cap and its age
 * guard, so a record frozen across a fresh pass makes a working backup look
 * dead.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Backup\TaskRunner;
use WPMgr\Agent\Backup\Watchdog;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

require_once __DIR__ . '/FakeBackupTasksWpdb.php';

/**
 * @covers \WPMgr\Agent\Backup\TaskRunner
 * @covers \WPMgr\Agent\Backup\Watchdog
 */
final class FreshArchivePassProgressTest extends TestCase
{
    private const ID = '95409541-aaaa-4bbb-8ccc-dddddddddddd';

    /** The discovery id of the pass the host ended after five closed parts. */
    private const OLD_PASS = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';

    /** The discovery id of the fresh pass that replaces it. */
    private const NEW_PASS = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb';

    private string $root = '';

    /** @var list<array{phase:string,detail:array<string,mixed>}> */
    private array $posts = [];

    /** The discovery id of the cursor every archive pass was handed, in call order. */
    private \ArrayObject $archiveCalls;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->root = sys_get_temp_dir() . '/wpmgr-954-pass-' . bin2hex(random_bytes(6));
        mkdir($this->root . '/runs', 0700, true);
        mkdir($this->root . '/wp-content', 0700, true);
        $this->root = (string) realpath($this->root);
        $this->archiveCalls = new \ArrayObject();
        Functions\when('esc_html')->returnArg();
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
        Functions\when('wp_schedule_single_event')->justReturn(true);
        Functions\when('wp_delete_file')->alias(static fn ($f) => @unlink($f)); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
        // Capture the control-plane channel without a provisioned site key.
        \Patchwork\redefine('WPMgr\Agent\Signer::signHeaders', fn (): array => ['X-Test-Signed' => '1']); // Instance method: the closure must not be static.
        $this->posts = [];
        Functions\when('wp_remote_post')->alias(function ($url, $args) {
            $body = json_decode((string) $args['body'], true);
            if (str_ends_with((string) $url, '/progress') && is_array($body) && isset($body['phase'])) {
                $this->posts[] = ['phase' => (string) $body['phase'], 'detail' => (array) ($body['phase_detail'] ?? [])];
            }
            return ['response' => ['code' => 200], 'body' => '{}'];
        });
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        $this->removeTree($this->root);
        parent::tear_down();
    }

    // ---- The record advances at the fresh pass's first checkpoint ---------

    public function testFirstCheckpointOfAFreshPassAdvancesTheProgressRecord(): void
    {
        $db = $this->seed(600, 600, 5);
        $t0 = time();

        $records = $this->runAWorkerThroughAFreshPass($db);

        self::assertSame(
            [7, 7, 8, 9, 9],
            array_column($records, 'seq'),
            'saves in order: worker start and archive start change nothing; the fresh pass\'s first checkpoint advances although its marks are lower; its next part advances; a repeat of that state does not'
        );
        $afterFirst = $records[2];
        self::assertGreaterThanOrEqual($t0, $afterFirst['at'], 'the record\'s time moves with the first checkpoint of the fresh pass');
        self::assertSame(self::NEW_PASS, $afterFirst['pass'] ?? null, 'the record names the pass it advanced in');
        self::assertSame(self::NEW_PASS, end($records)['pass'] ?? null);
    }

    // ---- The watchdog neither counts that resume nor gives the task up ----

    public function testWatchdogDoesNotCountTheResumeAfterAFreshPassSavedItsFirstCheckpoint(): void
    {
        // Every resume the cap allows is used: a task whose last worker saved
        // progress must still be resumed, and that resume is not counted.
        $db = $this->seed(600, 600, 6);
        $this->runAWorkerThroughAFreshPass($db);
        $db->rows[self::ID]['last_progress_at'] = time() - 400; // The host ended that worker: the task is stalled.

        Watchdog::resumeIfStalled(self::ID);

        self::assertArrayHasKey(self::ID, $db->rows, 'a task whose last worker saved a part is not given up at the resume cap');
        self::assertSame([], $this->failedPosts());
        self::assertSame([6], $this->resumeCountWrites($db), 'the resume is not counted');
        self::assertSame([self::OLD_PASS, self::NEW_PASS], $this->archiveCalls->getArrayCopy(), 'the next worker continued from the fresh pass');
    }

    public function testWatchdogDoesNotGiveUpForLackOfProgressAfterAFreshPassSavedItsFirstCheckpoint(): void
    {
        // Three hours old, and the record last advanced two hours ago: the age
        // guard gives it up unless the fresh pass's first checkpoint moved it.
        $db = $this->seed(3 * 3600, 2 * 3600, 0);
        $this->runAWorkerThroughAFreshPass($db);
        $db->rows[self::ID]['last_progress_at'] = time() - 400;

        Watchdog::resumeIfStalled(self::ID);

        self::assertArrayHasKey(self::ID, $db->rows, 'a backup that just saved a part is not given up for lack of progress');
        self::assertSame([], $this->failedPosts());
        self::assertSame([0], $this->resumeCountWrites($db));
        self::assertSame([self::OLD_PASS, self::NEW_PASS], $this->archiveCalls->getArrayCopy(), 'the next worker continued from the fresh pass');
    }

    // ---- The record's optional pass ---------------------------------------

    public function testProgressRecordKeepsAValidPassAndStillLoadsARecordWithoutOne(): void
    {
        $base = ['mark' => [2, 1, 10, 0, 0], 'at' => 1700000000, 'seq' => 4];

        self::assertSame($base + ['pass' => self::NEW_PASS], TaskRunner::progressRecord($base + ['pass' => self::NEW_PASS]));
        self::assertSame($base, TaskRunner::progressRecord($base), 'a record saved without a pass still loads');
        foreach ([123, '', str_repeat('a', 65), "two\nlines", ['x'], null] as $invalid) {
            self::assertSame($base, TaskRunner::progressRecord($base + ['pass' => $invalid]), 'an invalid pass is dropped and the record stays valid');
        }
    }

    // ---- helpers -----------------------------------------------------------

    /**
     * Run a worker on the row $seed left, with an archive pass that starts
     * fresh: the saved checkpoint is not used, a new discovery id is, and the
     * pass closes two parts and saves each through the runner's own checkpoint
     * callback (the second state twice). The host then ends the worker, so
     * nothing runs after the last save: no failure report, no cleanup. Later
     * workers' archive passes only record the cursor they were handed and end
     * the same way.
     *
     * @return list<array<string,mixed>> The progress record of every state the worker saved, in order.
     */
    private function runAWorkerThroughAFreshPass(FakeBackupTasksWpdb $db): array
    {
        $calls  = $this->archiveCalls;
        $first  = $this->cursor(self::NEW_PASS, ['uploads.g000.part001.zip'], 10);
        $second = $this->cursor(self::NEW_PASS, ['uploads.g000.part001.zip', 'uploads.g000.part002.zip'], 20);
        \Patchwork\redefine('WPMgr\Agent\Backup\FilesArchiver::archive', function (string $outDir, array $resume, callable $progress, ?array $prevMap = null, ?callable $checkpoint = null) use ($calls, $first, $second): array { // Instance method: the closure must not be static.
            $calls[] = (string) ($resume['discovery_id'] ?? '');
            if (count($calls) === 1) {
                ($checkpoint)($first);
                ($checkpoint)($second);
                ($checkpoint)($second);
            }
            throw new \RuntimeException('the host ends this worker');
        });
        \Patchwork\redefine('WPMgr\Agent\Backup\TaskRunner::failRun', function (string $currentPhase, \Throwable $e): string { return TaskRunner::PHASE_FAILED; }); // Instance method: the closure must not be static.

        (new TaskRunner($this->params()))->run();

        $records = [];
        foreach ($db->updates as $update) {
            if (isset($update['data']['sub_state'])) {
                $state     = TaskRunner::decodeSubStateColumn($update['data']['sub_state']);
                $records[] = (array) ($state['progress'] ?? []);
            }
        }
        return $records;
    }

    /**
     * Seed an archiving task the host ended after five closed parts of pass
     * OLD_PASS: its record is at sequence 7, its last worker started at
     * sequence 7 and saved nothing since, and its row is stalled.
     */
    private function seed(int $startedAgo, int $progressAgo, int $resumeCount): FakeBackupTasksWpdb
    {
        $now   = time();
        $parts = ['uploads.g000.part001.zip', 'uploads.g000.part002.zip', 'uploads.g000.part003.zip', 'uploads.g000.part004.zip', 'uploads.g000.part005.zip'];
        $state = [
            'params'   => $this->params(),
            'files'    => $this->cursor(self::OLD_PASS, $parts, 100),
            'progress' => ['mark' => [2, 5, 100, 0, 0], 'at' => $now - $progressAgo, 'seq' => 7, 'pass' => self::OLD_PASS],
            'worker'   => ['seq' => 7, 'started_at' => $now - $progressAgo],
        ];
        $db = new FakeBackupTasksWpdb();
        $db->seedRow(self::ID, [
            'phase'            => TaskRunner::PHASE_ARCHIVING_FILES,
            'sub_state'        => (string) json_encode($state),
            'started_at'       => $now - $startedAgo,
            'last_progress_at' => $now - 400,
            'resume_count'     => $resumeCount,
            'max_resumes'      => 6,
        ]);
        $GLOBALS['wpdb'] = $db;
        return $db;
    }

    /**
     * A cursor shaped like the archiver's own checkpoint.
     *
     * @param list<string> $parts
     * @return array<string,mixed>
     */
    private function cursor(string $pass, array $parts, int $fileIndex): array
    {
        return [
            'checkpoint'            => 1,
            'complete'              => false,
            'discovery_id'          => $pass,
            'file_index'            => $fileIndex,
            'parts_completed'       => $parts,
            'part_kinds'            => array_fill(0, count($parts), 'upload'),
            'part_sizes'            => array_fill(0, count($parts), 1024),
            'skipped'               => [],
            'skipped_sources_count' => 0,
            'skipped_sources'       => [],
        ];
    }

    /** @return array<string,mixed> */
    private function params(): array
    {
        return [
            'snapshot_id'       => self::ID,
            'kind'              => 'files',
            'age_recipient'     => 'age1' . str_repeat('q', 58),
            'presign_endpoint'  => 'https://cp.invalid/presign',
            'manifest_endpoint' => 'https://cp.invalid/manifest',
            'progress_endpoint' => 'https://cp.invalid/agent/v1/backups/' . self::ID . '/progress',
            'chunk_bytes'       => 4 * 1024 * 1024,
            'scratch_dir'       => $this->root . '/runs/' . self::ID,
            'wp_content_path'   => $this->root . '/wp-content',
            'db'                => ['prefix' => 'wp_'],
        ];
    }

    /** @return list<string> Messages of every `failed` report sent to the control plane. */
    private function failedPosts(): array
    {
        $messages = [];
        foreach ($this->posts as $post) {
            if ($post['phase'] === TaskRunner::PHASE_FAILED && isset($post['detail']['message'])) {
                $messages[] = (string) $post['detail']['message'];
            }
        }
        return array_values(array_filter($messages, static fn (string $m): bool => str_starts_with($m, 'Backup stopped')));
    }

    /** @return list<int> resume_count values the watchdog wrote. */
    private function resumeCountWrites(FakeBackupTasksWpdb $db): array
    {
        $writes = [];
        foreach ($db->updates as $update) {
            if (array_key_exists('resume_count', $update['data'])) {
                $writes[] = (int) $update['data']['resume_count'];
            }
        }
        return $writes;
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
