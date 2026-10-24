<?php

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Backup\TaskRunner;
use WPMgr\Agent\Commands\BackupCommand;
use WPMgr\Agent\Schema;
use WPMgr\Agent\Support\AgeIdentity;
use WPMgr\Agent\Support\ConnectionFinisher;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

require_once __DIR__ . '/FakeBackupTasksWpdb.php';
require_once __DIR__ . '/FileLockStub.php';

#[\PHPUnit\Framework\Attributes\RunTestsInSeparateProcesses]
#[\PHPUnit\Framework\Attributes\PreserveGlobalState(false)]
final class BackupCommandLifecycleTest extends TestCase
{
    private const SNAPSHOT = 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee';
    private BackupCommandLifecycleWpdb $database;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        if (!defined('WP_CONTENT_DIR')) { define('WP_CONTENT_DIR', __DIR__ . '/issue954-command-content'); }
        self::assertSame(__DIR__ . '/issue954-command-content', WP_CONTENT_DIR, 'Run this issue-specific command test in its own PHP process.');
        if (!is_dir(WP_CONTENT_DIR)) { mkdir(WP_CONTENT_DIR, 0700, true); }
        Functions\when('wp_json_encode')->alias(static fn ($data) => json_encode($data));
        Functions\when('get_option')->justReturn(Schema::CURRENT_VERSION);
        Functions\when('wp_schedule_single_event')->justReturn(true);
        Functions\when('spawn_cron')->justReturn(true);
        Functions\when('site_url')->justReturn('https://site.invalid/wp-cron.php');
        Functions\when('wp_remote_get')->justReturn(['response' => ['code' => 200], 'body' => '']);
        Functions\when('is_wp_error')->justReturn(false);
        Functions\when('wp_remote_retrieve_response_code')->justReturn(200);
        $this->database = new BackupCommandLifecycleWpdb();
        $GLOBALS['wpdb'] = $this->database;
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        $this->removeTree(WP_CONTENT_DIR);
        parent::tear_down();
    }

    public function testCommandPreservesExclusionsAndClaimsStartBeforeShutdownWork(): void
    {
        $shutdown = null;
        Functions\expect('register_shutdown_function')->once()->andReturnUsing(static function ($callback) use (&$shutdown): void { $shutdown = $callback; });
        $before = time();
        $result = $this->command()->execute([], $this->params());
        $after = time();
        self::assertTrue($result['ok'], json_encode($result));
        self::assertIsCallable($shutdown);
        $task = $this->database->tasks->rows[self::SNAPSHOT];
        $params = json_decode($task['sub_state'], true)['params'];
        self::assertSame(['2024', '2025', '2026'], $params['exclude_paths']);
        self::assertSame(['log'], $params['exclude_extensions']);
        self::assertSame('queued', $task['phase']);
        self::assertGreaterThanOrEqual($before, $this->database->runs[self::SNAPSHOT]['started_at']);
        self::assertLessThanOrEqual($after, $this->database->runs[self::SNAPSHOT]['started_at']);
        self::assertSame($task['started_at'], $task['last_progress_at']);
        fwrite(STDOUT, "\naccepted command=" . json_encode([
            'runs_started_at' => $this->database->runs[self::SNAPSHOT]['started_at'],
            'tasks_started_at' => $task['started_at'], 'phase' => $task['phase'],
            'exclude_paths' => $params['exclude_paths'], 'exclude_extensions' => $params['exclude_extensions'],
            'shutdown_invoked' => false,
        ]) . "\n");
    }

    public function testExpiredDuplicateClaimPreservesActiveTask(): void
    {
        $old = time() - 367;
        $this->database->runs[self::SNAPSHOT] = ['snapshot_id' => self::SNAPSHOT, 'pid' => 123, 'started_at' => $old];
        $this->database->tasks->seedRow(self::SNAPSHOT, ['phase' => 'dumping_db', 'started_at' => $old, 'last_progress_at' => time()]);
        $command = $this->command();
        $method = new \ReflectionMethod($command, 'tryClaimDedup');
        self::assertFalse($method->invoke($command, self::SNAPSHOT));
        self::assertSame($old, $this->database->runs[self::SNAPSHOT]['started_at']);
        self::assertSame('dumping_db', $this->database->tasks->rows[self::SNAPSHOT]['phase']);
        self::assertSame($old, $this->database->tasks->rows[self::SNAPSHOT]['started_at']);
        fwrite(STDOUT, "\nduplicate claim old=" . $old . ' replacement=' . $this->database->runs[self::SNAPSHOT]['started_at'] . " task_phase=dumping_db task_started_at_unchanged=true worker_not_invoked=true\n");
    }

    public function testShutdownRunnerHonorsHeldFlock(): void
    {
        $shutdown = null;
        Functions\expect('register_shutdown_function')->once()->andReturnUsing(static function ($callback) use (&$shutdown): void { $shutdown = $callback; });
        self::assertTrue($this->command()->execute([], $this->params())['ok']);
        $lock = fopen(WP_CONTENT_DIR . '/wpmgr-agent/runs/' . self::SNAPSHOT . '.lock', 'c');
        self::assertTrue(flock($lock, LOCK_EX | LOCK_NB));
        try { $shutdown(); }
        finally { flock($lock, LOCK_UN); fclose($lock); }
        self::assertSame('queued', $this->database->tasks->rows[self::SNAPSHOT]['phase']);
        self::assertSame(2, $this->database->tasks->lockCallCount);
        self::assertFileDoesNotExist(WP_CONTENT_DIR . '/wpmgr-agent/runs/' . self::SNAPSHOT . '/paths.cache');
        fwrite(STDOUT, "\naccepted shutdown runner rejected by held flock\n");
    }

    public function testRecentDuplicateCommandDoesNotRegisterAnotherRunner(): void
    {
        $this->database->runs[self::SNAPSHOT] = ['snapshot_id' => self::SNAPSHOT, 'pid' => 123, 'started_at' => time()];
        Functions\expect('register_shutdown_function')->never();
        $result = $this->command()->execute([], $this->params());
        self::assertFalse($result['ok']);
        self::assertSame('runner_in_flight', $result['code']);
        fwrite(STDOUT, "\nrecent duplicate=" . json_encode($result) . "\n");
    }

    public function testBothLockProbesUnavailableRefusesBeforeClaimAndTaskSeed(): void
    {
        $this->database->tasks->lockResponses = [null];
        $path = WP_CONTENT_DIR . '/wpmgr-agent/runs/' . self::SNAPSHOT . '.lock';
        \Patchwork\redefine('fopen', static function ($filename, ...$args) use ($path) { return $filename === $path ? false : \Patchwork\relay(); });
        $registered = 0;
        Functions\when('register_shutdown_function')->alias(static function () use (&$registered): void { $registered++; });
        $result = $this->command()->execute([], $this->params());
        self::assertFalse($result['ok']);
        self::assertSame('backup_lock_unavailable', $result['code']);
        self::assertStringContainsString('neither a database lock nor a working file lock', $result['detail']);
        self::assertStringContainsString('wp-content/wpmgr-agent', $result['detail']);
        self::assertStringContainsString('Ask your host', $result['detail']);
        self::assertSame([], $this->database->runs);
        self::assertSame([], $this->database->tasks->rows);
        self::assertSame(0, $registered);
        self::assertDirectoryDoesNotExist(WP_CONTENT_DIR . '/wpmgr-agent/runs/' . self::SNAPSHOT);
    }

    public function testUnsupportedFlockWithoutDatabaseSupportRefusesBeforeTaskSeed(): void
    {
        $this->database->tasks->lockResponses = [null];
        $path = WP_CONTENT_DIR . '/wpmgr-agent/runs/' . self::SNAPSHOT . '.lock';
        \Patchwork\redefine('WPMgr\\Agent\\Backup\\flock', static function ($handle, $operation, &$wouldBlock = null) use ($path) {
            if (($operation & LOCK_EX) && stream_get_meta_data($handle)['uri'] === $path) {
                $wouldBlock = 0;
                return false;
            }
            return \Patchwork\relay();
        });
        $registered = 0;
        Functions\when('register_shutdown_function')->alias(static function () use (&$registered): void { $registered++; });
        $result = $this->command()->execute([], $this->params());
        self::assertFalse($result['ok']);
        self::assertSame('backup_lock_unavailable', $result['code']);
        self::assertSame([], $this->database->runs);
        self::assertSame([], $this->database->tasks->rows);
        self::assertSame(0, $registered);
    }

    public function testDatabaseProbeAloneAcceptsAndReleasesProbeName(): void
    {
        $this->database->tasks->lockResponses = ['1'];
        $path = WP_CONTENT_DIR . '/wpmgr-agent/runs/' . self::SNAPSHOT . '.lock';
        \Patchwork\redefine('fopen', static function ($filename, ...$args) use ($path) { return $filename === $path ? false : \Patchwork\relay(); });
        $names = [];
        \Patchwork\redefine(FakeBackupTasksWpdb::class . '::get_var', function ($prepared) use (&$names) {
            $query = json_decode($prepared, true);
            if (str_contains($query['sql'] ?? '', 'GET_LOCK') || str_contains($query['sql'] ?? '', 'RELEASE_LOCK')) { $names[] = $query['args'][0]; }
            return \Patchwork\relay();
        });
        Functions\expect('register_shutdown_function')->once();
        self::assertTrue($this->command()->execute([], $this->params())['ok']);
        self::assertCount(2, $names);
        self::assertSame($names[0], $names[1]);
        self::assertStringStartsWith('wpmgr_bk_probe_', $names[0]);
        self::assertNotSame('wpmgr_bk_' . self::SNAPSHOT, $names[0]);
        self::assertSame(1, $this->database->tasks->releaseCallCount);
        self::assertArrayHasKey(self::SNAPSHOT, $this->database->tasks->rows);
    }

    public function testFileProbeAloneAcceptsAndReleasesHandle(): void
    {
        $this->database->tasks->lockResponses = [null];
        Functions\expect('register_shutdown_function')->once();
        self::assertTrue($this->command()->execute([], $this->params())['ok']);
        $path = WP_CONTENT_DIR . '/wpmgr-agent/runs/' . self::SNAPSHOT . '.lock';
        $probe = fopen($path, 'r+');
        self::assertIsResource($probe);
        try { self::assertTrue(flock($probe, LOCK_EX | LOCK_NB)); }
        finally { fclose($probe); }
        self::assertSame(0, $this->database->tasks->releaseCallCount);
    }

    public function testUnreliableFileIdentityWithoutDatabaseSupportRefusesCommand(): void
    {
        $this->database->tasks->lockResponses = [null];
        \Patchwork\redefine(TaskRunner::class . '::fileLockMatchesPath', static fn (): bool => false);
        Functions\expect('register_shutdown_function')->never();
        $result = $this->command()->execute([], $this->params());
        self::assertFalse($result['ok']);
        self::assertSame('backup_lock_unavailable', $result['code']);
        self::assertSame([], $this->database->runs);
        self::assertSame([], $this->database->tasks->rows);
    }

    public function testContendedDatabaseProbeIsUsableAndNeverReleasedByContender(): void
    {
        $this->database->tasks->lockResponses = ['0'];
        $path = WP_CONTENT_DIR . '/wpmgr-agent/runs/' . self::SNAPSHOT . '.lock';
        \Patchwork\redefine('fopen', static function ($filename, ...$args) use ($path) { return $filename === $path ? false : \Patchwork\relay(); });
        Functions\expect('register_shutdown_function')->once();
        self::assertTrue($this->command()->execute([], $this->params())['ok']);
        self::assertSame(0, $this->database->tasks->releaseCallCount);
    }

    public function testContendedFileProbePreservesRunningOwner(): void
    {
        $this->database->tasks->lockResponses = [null];
        $base = WP_CONTENT_DIR . '/wpmgr-agent/runs';
        mkdir($base, 0700, true);
        $path = $base . '/' . self::SNAPSHOT . '.lock';
        $holder = fopen($path, 'c');
        self::assertTrue(flock($holder, LOCK_EX | LOCK_NB));
        $this->database->runs[self::SNAPSHOT] = ['snapshot_id' => self::SNAPSHOT, 'pid' => 123, 'started_at' => time()];
        $this->database->tasks->seedRow(self::SNAPSHOT, ['phase' => 'archiving_files']);
        $before = $this->database->tasks->rows;
        Functions\expect('register_shutdown_function')->never();
        try {
            $result = $this->command()->execute([], $this->params());
            self::assertSame('runner_in_flight', $result['code']);
            self::assertSame($before, $this->database->tasks->rows);
            self::assertFileExists($path);
            $probe = fopen($path, 'r+');
            try { self::assertFalse(flock($probe, LOCK_EX | LOCK_NB)); }
            finally { fclose($probe); }
        } finally { flock($holder, LOCK_UN); fclose($holder); }
    }

    public function testUnavailableProbesDoNotRefuseExistingInFlightWorkAsFailed(): void
    {
        $this->database->tasks->lockResponses = [null];
        $path = WP_CONTENT_DIR . '/wpmgr-agent/runs/' . self::SNAPSHOT . '.lock';
        \Patchwork\redefine('fopen', static function ($filename, ...$args) use ($path) { return $filename === $path ? false : \Patchwork\relay(); });
        $this->database->runs[self::SNAPSHOT] = ['snapshot_id' => self::SNAPSHOT, 'pid' => 123, 'started_at' => time() - 1000];
        Functions\expect('register_shutdown_function')->never();
        foreach ([null, 'archiving_files'] as $phase) {
            if ($phase !== null) { $this->database->tasks->seedRow(self::SNAPSHOT, ['phase' => $phase, 'last_progress_at' => time() - 1000]); }
            $claim = $this->database->runs[self::SNAPSHOT];
            $tasks = $this->database->tasks->rows;
            $result = $this->command()->execute([], $this->params());
            self::assertFalse($result['ok']);
            self::assertSame('runner_in_flight', $result['code']);
            self::assertSame($claim, $this->database->runs[self::SNAPSHOT]);
            self::assertSame($tasks, $this->database->tasks->rows);
        }
    }

    public function testExpiredDuplicateCommandLeavesEveryActivePhaseUntouched(): void
    {
        Functions\expect('register_shutdown_function')->never();
        foreach (['queued', 'dumping_db', 'archiving_files', 'encrypting_uploading', 'submitting_manifest'] as $phase) {
            $this->database->runs[self::SNAPSHOT] = ['snapshot_id' => self::SNAPSHOT, 'pid' => 123, 'started_at' => time() - 367];
            $this->database->tasks->seedRow(self::SNAPSHOT, ['phase' => $phase]);
            $claim = $this->database->runs[self::SNAPSHOT];
            $task = $this->database->tasks->rows[self::SNAPSHOT];
            $result = $this->command()->execute([], $this->params());
            self::assertFalse($result['ok'], $phase);
            self::assertSame('runner_in_flight', $result['code'], $phase);
            self::assertSame($claim, $this->database->runs[self::SNAPSHOT]);
            self::assertSame($task, $this->database->tasks->rows[self::SNAPSHOT]);
        }
    }

    public function testExpiredClaimCanBeReplacedForTerminalOrAbsentTask(): void
    {
        foreach (['completed', 'failed', null] as $phase) {
            $old = time() - 367;
            $this->database->runs[self::SNAPSHOT] = ['snapshot_id' => self::SNAPSHOT, 'pid' => 123, 'started_at' => $old];
            unset($this->database->tasks->rows[self::SNAPSHOT]);
            if ($phase !== null) { $this->database->tasks->seedRow(self::SNAPSHOT, ['phase' => $phase]); }
            self::assertTrue((new \ReflectionMethod(BackupCommand::class, 'tryClaimDedup'))->invoke($this->command(), self::SNAPSHOT));
            self::assertGreaterThan($old, $this->database->runs[self::SNAPSHOT]['started_at']);
        }
    }

    public function testCommandPersistsTimingBoundariesForWorker(): void
    {
        Functions\expect('register_shutdown_function')->once();
        $before = time();
        $_SERVER['REQUEST_TIME'] = $before;
        self::assertTrue($this->command()->execute([], $this->params())['ok']);
        $params = json_decode($this->database->tasks->rows[self::SNAPSHOT]['sub_state'], true)['params'];
        self::assertArrayHasKey('timing', $params);
        self::assertSame(['received_at', 'preflight_done_at', 'claimed_at'], array_keys($params['timing']));
        self::assertGreaterThanOrEqual($before, $params['timing']['received_at']);
        self::assertGreaterThanOrEqual($params['timing']['received_at'], $params['timing']['preflight_done_at']);
        self::assertGreaterThanOrEqual($params['timing']['preflight_done_at'], $params['timing']['claimed_at']);
        self::assertLessThanOrEqual(time(), $params['timing']['claimed_at']);
    }

    public function testTimingIncludesRequestWorkBeforeCommandExecution(): void
    {
        Functions\expect('register_shutdown_function')->once();
        $receivedAt = time() - 30;
        $_SERVER['REQUEST_TIME'] = $receivedAt;
        self::assertTrue($this->command()->execute([], $this->params())['ok']);
        $params = json_decode($this->database->tasks->rows[self::SNAPSHOT]['sub_state'], true)['params'];
        self::assertSame($receivedAt, $params['timing']['received_at']);
        self::assertGreaterThan($receivedAt, $params['timing']['preflight_done_at']);
    }

    private function command(): BackupCommand
    {
        $identity = new class extends AgeIdentity {
            public function __construct() {}
            public function recipientMatches(string $candidate): bool { return true; }
        };
        return new BackupCommand($identity, new ConnectionFinisher(static fn () => false, null, static function (): void {}));
    }

    private function params(): array
    {
        return ['snapshot_id' => self::SNAPSHOT, 'kind' => 'full', 'age_recipient' => 'test-recipient',
            'presign_endpoint' => 'https://cp.invalid/presign', 'manifest_endpoint' => 'https://cp.invalid/manifest',
            'progress_endpoint' => '', 'exclude_paths' => ['2024', '2025', '2026'], 'exclude_extensions' => ['log']];
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

final class BackupCommandLifecycleWpdb
{
    public string $prefix = 'wp_';
    public array $runs = [];
    public FakeBackupTasksWpdb $tasks;

    public function __construct() { $this->tasks = new FakeBackupTasksWpdb(); }
    public function prepare(string $sql, ...$args): string { return $this->tasks->prepare($sql, ...$args); }
    public function get_results(string $sql, $mode = null): array { return []; }
    public function query(string $sql): int { return $this->tasks->query($sql); }
    public function get_var(string $sql)
    {
        if ($sql === 'SELECT 1') { return '1'; }
        if (str_contains($sql, 'max_allowed_packet')) { return '67108864'; }
        return $this->tasks->get_var($sql);
    }
    public function get_row(string $prepared, $mode = null)
    {
        $data = json_decode($prepared, true);
        if (str_contains($data['sql'], 'wpmgr_backup_runs')) {
            return isset($this->runs[$data['args'][0]]) ? (object) $this->runs[$data['args'][0]] : null;
        }
        return $this->tasks->get_row($prepared, $mode);
    }
    public function insert(string $table, array $data, $format = null): int
    {
        if (str_contains($table, 'wpmgr_backup_runs')) { $this->runs[$data['snapshot_id']] = $data; return 1; }
        return $this->tasks->insert($table, $data, $format);
    }
    public function update(string $table, array $data, array $where, $format = null, $whereFormat = null): int
    {
        if (str_contains($table, 'wpmgr_backup_runs')) {
            $this->runs[$where['snapshot_id']] = array_merge($this->runs[$where['snapshot_id']], $data);
            return 1;
        }
        return $this->tasks->update($table, $data, $where, $format, $whereFormat);
    }
    public function delete(string $table, array $where, $format = null): int
    {
        if (str_contains($table, 'wpmgr_backup_runs')) { unset($this->runs[$where['snapshot_id']]); return 1; }
        return $this->tasks->delete($table, $where, $format);
    }
}
