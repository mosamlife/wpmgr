<?php
/**
 * GH #954 / #983: a fresh archive pass restarts the progress marks. Starting
 * over is not progress, and the pass's first closed part is.
 *
 * When a saved checkpoint no longer validates (an incremental backup whose
 * previous file list cannot be fetched on resume flips its mode, say), the
 * archive starts one fresh pass under a new discovery id, and its first closed
 * parts have lower marks than the pass it replaced. The durable progress
 * record takes the fresh pass's mark as its baseline without advancing, so a
 * backup that restarts on every worker cannot look busy; the pass's first
 * closed part then advances it, so a working backup is not frozen against the
 * old pass's mark. The record's sequence number and time feed the hand-over
 * decision, the watchdog's resume cap and its age guard. A fresh pass discards
 * the parts of the one it replaces, so a backup that keeps restarting never
 * finishes: the restart after MAX_ARCHIVE_RESTARTS stops it.
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

    // ---- A fresh pass is a new baseline; its first closed part advances ----

    public function testFreshPassTakesANewBaselineAndItsFirstClosedPartAdvancesTheRecord(): void
    {
        $db = $this->seed(600, 600, 5);
        $t0 = time();

        $records = $this->runAWorkerThroughAFreshPass($db);

        self::assertSame(
            [7, 7, 7, 8, 9, 9],
            array_column($records, 'seq'),
            'saves in order: worker start and archive start change nothing; the fresh pass\'s discovery checkpoint is a new baseline, not an advance; its first closed part advances; so does its next; a repeat of that state does not'
        );
        $baseline = $records[2];
        self::assertSame($records[0]['at'], $baseline['at'], 'starting over does not refresh the record\'s time');
        self::assertSame([2, 0, 0, 0, 0], $baseline['mark'], 'the record\'s baseline is the fresh pass\'s mark, lower than the pass it replaced');
        self::assertSame(self::NEW_PASS, $baseline['pass'] ?? null, 'the baseline names the fresh pass');
        self::assertSame(1, $baseline['restarts'] ?? null, 'the restart is counted');
        self::assertGreaterThanOrEqual($t0, $records[3]['at'], 'the first closed part of the fresh pass moves the record\'s time');
        self::assertSame(1, end($records)['restarts'] ?? null, 'closing parts is not another restart');
    }

    public function testRecordThatNamesNoPassLearnsItWithoutCountingAnAdvance(): void
    {
        // A record saved before passes were recorded, or before the archive
        // had a pass: finding the pass it is already in is not progress, or a
        // worker would hand over before it closed anything. Once learned, a
        // fresh pass is told from it as usual.
        $db = $this->seed(600, 600, 5, false);

        $records = $this->runAWorkerThroughAFreshPass($db);

        self::assertSame([7, 7, 7, 8, 9, 9], array_column($records, 'seq'), 'only closed parts advance the record');
        self::assertSame(self::OLD_PASS, $records[0]['pass'] ?? null, 'the first save names the pass the worker resumed in');
        self::assertSame(0, $records[0]['restarts'] ?? 0, 'learning the pass is not a restart');
        self::assertSame(self::NEW_PASS, $records[2]['pass'] ?? null);
        self::assertSame(1, $records[2]['restarts'] ?? null);
    }

    // ---- A flapping mode cannot churn workers or look busy -----------------

    public function testFlippingModeWithTheBudgetSpentDoesNotHandOverWithoutClosingAPart(): void
    {
        // Every worker's saved checkpoint is rejected (the mode flips) and its
        // CPU budget is spent: only a real advance may hand the task over, and
        // a fresh pass's discovery checkpoint is none.
        $this->spendEveryWorkersBudget();
        $log = $this->flippingArchiver(false);
        $this->letTheHostEndWorkers();
        $this->seedNewBackup();

        (new TaskRunner($this->params()))->run();

        self::assertSame([['parts' => 0, 'handed_over' => false]], $log->getArrayCopy(), 'one worker ran and nothing was handed over: the loop ended');
    }

    public function testFlippingModeThatClosesAPartPerWorkerIsStoppedAfterTheRestartBound(): void
    {
        // Each worker restarts the pass, closes one part and hands over, and the
        // next worker discards that part by restarting again: every hand-over
        // follows a real advance, yet the backup can never finish.
        $this->spendEveryWorkersBudget();
        $log = $this->flippingArchiver(true);
        $this->letTheHostEndWorkers();
        $db  = $this->seedNewBackup();
        $max = $this->maxRestarts();

        (new TaskRunner($this->params()))->run();

        $workers = $log->getArrayCopy();
        self::assertCount($max + 2, $workers, 'the first pass, one worker per allowed restart, and the one the bound stops');
        foreach ($workers as $i => $worker) {
            if ($worker['handed_over']) {
                self::assertSame(1, $worker['parts'], 'worker ' . ($i + 1) . ' handed over without closing a part');
            }
        }
        self::assertSame(array_fill(0, $max + 1, true), array_slice(array_column($workers, 'handed_over'), 0, $max + 1), 'every worker before the last handed over');
        self::assertArrayNotHasKey(self::ID, $db->rows, 'the stopped backup\'s row is removed');
        self::assertCount(1, $this->failedPosts());
        self::assertStringContainsString('start over ' . ($max + 1) . ' times', $this->failedPosts()[0], 'the reason names the restarts');
    }

    public function testRestartsThatCloseNothingCountAgainstTheResumeCapAndStopTheBackup(): void
    {
        // Each round the watchdog resumes a worker that starts a fresh pass and
        // is ended by the host right after its discovery checkpoint. Such a
        // round saved nothing, so it is counted; and the backup is not left
        // restarting until the 24 hour ceiling.
        $log = $this->flippingArchiver(false);
        $this->letTheHostEndWorkers();
        $db     = $this->seed(600, 600, 0);
        $max    = $this->maxRestarts();
        $rounds = 0;

        while ($rounds < $max + 6 && isset($db->rows[self::ID])) {
            ++$rounds;
            $db->rows[self::ID]['last_progress_at'] = time() - 400; // The host ended the worker: the task is stalled.
            Watchdog::resumeIfStalled(self::ID);
        }

        self::assertArrayNotHasKey(self::ID, $db->rows, 'a backup that restarts its pass and closes nothing is given up');
        self::assertSame($max + 1, $rounds, 'by the restart after the bound');
        self::assertSame(range(1, $max + 1), $this->resumeCountWrites($db), 'every round was a resume without progress, so each was counted');
        self::assertCount($max + 1, $log, 'each round started one worker');
        self::assertCount(1, $this->failedPosts());
        self::assertStringContainsString('start over', $this->failedPosts()[0]);
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
        self::assertSame($base + ['pass' => self::NEW_PASS, 'restarts' => 2], TaskRunner::progressRecord($base + ['pass' => self::NEW_PASS, 'restarts' => 2]), 'the restart count is kept');
        foreach ([-1, 1001, '2', 1.5, null, ['x']] as $invalid) {
            self::assertSame($base, TaskRunner::progressRecord($base + ['restarts' => $invalid]), 'an invalid restart count is dropped and the record stays valid');
        }
    }

    // ---- helpers -----------------------------------------------------------

    /**
     * Run a worker on the row $seed left, with an archive pass that starts
     * fresh: the saved checkpoint is not used, a new discovery id is, and the
     * pass saves what the archiver saves, through the runner's own checkpoint
     * callback: first the discovery checkpoint with no closed part, then each
     * closed part (the last state twice). The host then ends the worker, so
     * nothing runs after the last save: no failure report, no cleanup. Later
     * workers' archive passes only record the cursor they were handed and end
     * the same way.
     *
     * @return list<array<string,mixed>> The progress record of every state the worker saved, in order.
     */
    private function runAWorkerThroughAFreshPass(FakeBackupTasksWpdb $db): array
    {
        $calls  = $this->archiveCalls;
        $zero   = $this->cursor(self::NEW_PASS, [], 0);
        $first  = $this->cursor(self::NEW_PASS, ['uploads.g000.part001.zip'], 10);
        $second = $this->cursor(self::NEW_PASS, ['uploads.g000.part001.zip', 'uploads.g000.part002.zip'], 20);
        \Patchwork\redefine('WPMgr\Agent\Backup\FilesArchiver::archive', function (string $outDir, array $resume, callable $progress, ?array $prevMap = null, ?callable $checkpoint = null) use ($calls, $zero, $first, $second): array { // Instance method: the closure must not be static.
            $calls[] = (string) ($resume['discovery_id'] ?? '');
            if (count($calls) === 1) {
                ($checkpoint)($zero);
                ($checkpoint)($first);
                ($checkpoint)($second);
                ($checkpoint)($second);
            }
            throw new \RuntimeException('the host ends this worker');
        });
        $this->letTheHostEndWorkers();

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
     * Archive passes that start fresh on every worker, as an intermittent
     * fetch of the parent's file list makes the mode flip: the saved
     * checkpoint is not used, a new discovery id is, and the pass saves the
     * discovery checkpoint the archiver saves first. With $closePart it then
     * closes one part and saves it. The host then ends the worker; an
     * exception from a save (a hand-over, a stop) passes through. The chain is
     * ended after $limit workers so that a run which never stops still ends.
     *
     * @return \ArrayObject<int,array{parts:int,handed_over:bool}> One entry per worker, in start order.
     */
    private function flippingArchiver(bool $closePart, int $limit = 12): \ArrayObject
    {
        $log      = new \ArrayObject();
        $cursor   = fn (string $pass, array $parts, int $line): array => $this->cursor($pass, $parts, $line);
        $handOver = TaskRunner::CODE_HANDOVER;
        \Patchwork\redefine('WPMgr\Agent\Backup\FilesArchiver::archive', function (string $outDir, array $resume, callable $progress, ?array $prevMap = null, ?callable $checkpoint = null) use ($log, $closePart, $limit, $cursor, $handOver): array { // Instance method: the closure must not be static.
            $worker = count($log);
            $log[]  = ['parts' => 0, 'handed_over' => false];
            if ($worker >= $limit) {
                throw new \RuntimeException('the test ends a chain that did not stop');
            }
            $pass = sprintf('%032x', 0xa000 + $worker);
            try {
                ($checkpoint)($cursor($pass, [], 0));
                if ($closePart) {
                    $log[$worker] = ['parts' => 1, 'handed_over' => false];
                    ($checkpoint)($cursor($pass, ['uploads.g000.part001.zip'], 10));
                }
            } catch (\RuntimeException $e) {
                if ($e->getCode() === $handOver) {
                    $log[$worker] = ['parts' => $log[$worker]['parts'], 'handed_over' => true];
                }
                throw $e;
            }
            throw new \RuntimeException('the host ends this worker');
        });
        return $log;
    }

    /**
     * Every worker's CPU budget is spent, so only the real progress guard
     * stands between a save and a hand-over.
     */
    private function spendEveryWorkersBudget(): void
    {
        \Patchwork\redefine('WPMgr\Agent\Backup\TaskRunner::cpuSeconds', static function (): float {
            static $seconds = 0.0;
            $seconds += 1.0e6;
            return $seconds;
        });
    }

    /**
     * A worker the host ends runs nothing after its last save: no failure
     * report, no cleanup. Only a worker that stops its own backup reports it.
     */
    private function letTheHostEndWorkers(): void
    {
        \Patchwork\redefine('WPMgr\Agent\Backup\TaskRunner::failRun', function (string $currentPhase, \Throwable $e) { // Instance method: the closure must not be static.
            return str_contains($e->getMessage(), 'start over') ? \Patchwork\relay() : TaskRunner::PHASE_FAILED;
        });
    }

    /** The restarts one backup may make: the runner's bound, or the number this test was written for where the runner has none. */
    private function maxRestarts(): int
    {
        $name = TaskRunner::class . '::MAX_ARCHIVE_RESTARTS';
        return defined($name) ? (int) constant($name) : 3;
    }

    /** A new backup: its archive phase has no checkpoint and its record is not yet written. */
    private function seedNewBackup(): FakeBackupTasksWpdb
    {
        $db = new FakeBackupTasksWpdb();
        $db->seedRow(self::ID, ['phase' => TaskRunner::PHASE_ARCHIVING_FILES, 'sub_state' => (string) json_encode(['params' => $this->params()])]);
        $GLOBALS['wpdb'] = $db;
        return $db;
    }

    /**
     * Seed an archiving task the host ended after five closed parts of pass
     * OLD_PASS: its record is at sequence 7, its last worker started at
     * sequence 7 and saved nothing since, and its row is stalled.
     */
    private function seed(int $startedAgo, int $progressAgo, int $resumeCount, bool $recordNamesItsPass = true): FakeBackupTasksWpdb
    {
        $now    = time();
        $parts  = ['uploads.g000.part001.zip', 'uploads.g000.part002.zip', 'uploads.g000.part003.zip', 'uploads.g000.part004.zip', 'uploads.g000.part005.zip'];
        $record = ['mark' => [2, 5, 100, 0, 0], 'at' => $now - $progressAgo, 'seq' => 7];
        $state  = [
            'params'   => $this->params(),
            'files'    => $this->cursor(self::OLD_PASS, $parts, 100),
            'progress' => $recordNamesItsPass ? $record + ['pass' => self::OLD_PASS] : $record,
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
