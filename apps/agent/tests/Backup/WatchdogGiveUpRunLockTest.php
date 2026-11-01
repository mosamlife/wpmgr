<?php
/**
 * GH #983: every watchdog give-up that spares a live worker deletes the task
 * row only while it holds the run-lock, after re-reading the task under that
 * lock. A worker that takes the lock first keeps its row and finishes its
 * backup; a task whose worker saved progress before the lock was taken is
 * not given up.
 *
 * The worker is a real TaskRunner::run() inside a Fiber: it starts the moment
 * the watchdog's own lock probe releases the lock, takes the real run-lock,
 * and pauses at its first save until the watchdog pass has returned.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Backup\TaskRunner;
use WPMgr\Agent\Backup\Watchdog;
use WPMgr\Agent\Support\AgeCrypto;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

require_once __DIR__ . '/FakeBackupTasksWpdb.php';

/** Task storage that runs a hook after the first lock release and can pause a fiber worker at its first save. */
final class LockRaceTasksWpdb
{
    public string $prefix = 'wp_';

    /** @var (callable(): void)|null Runs once, right after the first RELEASE_LOCK. */
    public $afterFirstRelease = null;

    private bool $released = false;

    private bool $paused = false;

    public function __construct(public FakeBackupTasksWpdb $inner)
    {
    }

    /** @param mixed ...$args */
    public function prepare(string $query, ...$args): string
    {
        return $this->inner->prepare($query, ...$args);
    }

    /** @return string|null */
    public function get_var(string $prepared)
    {
        $result = $this->inner->get_var($prepared);
        if (!$this->released && str_contains($prepared, 'RELEASE_LOCK')) {
            $this->released = true;
            if ($this->afterFirstRelease !== null) { ($this->afterFirstRelease)(); }
        }
        return $result;
    }

    /** @return array<string,mixed>|null */
    public function get_row(string $prepared, $output = null)
    {
        return $this->inner->get_row($prepared, $output);
    }

    /** @return list<string> */
    public function get_col(string $prepared): array
    {
        return $this->inner->get_col($prepared);
    }

    /** @param array<string,mixed> $data @param array<string,mixed> $where */
    public function update(string $table, array $data, array $where, $format = null, $whereFormat = null): int
    {
        if (!$this->paused && isset($data['sub_state'], $data['phase']) && \Fiber::getCurrent() !== null) {
            // The worker holds the run-lock here; it waits for the watchdog pass.
            $this->paused = true;
            \Fiber::suspend();
        }
        return $this->inner->update($table, $data, $where, $format, $whereFormat);
    }

    public function query(string $prepared): int
    {
        return $this->inner->query($prepared);
    }

    /** @param array<string,mixed> $where */
    public function delete(string $table, array $where, $whereFormat = null): int
    {
        return $this->inner->delete($table, $where, $whereFormat);
    }
}

/**
 * @covers \WPMgr\Agent\Backup\Watchdog
 * @covers \WPMgr\Agent\Backup\TaskRunner
 */
final class WatchdogGiveUpRunLockTest extends TestCase
{
    private const ID = '98309833-aaaa-4bbb-8ccc-dddddddddddd';

    private string $root = '';

    private string $recipient = '';

    /** @var list<array{phase:string,detail:array<string,mixed>}> */
    private array $posts = [];

    private int $scheduled = 0;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->root = sys_get_temp_dir() . '/wpmgr-983-giveup-lock-' . bin2hex(random_bytes(6));
        mkdir($this->root . '/runs', 0700, true);
        mkdir($this->root . '/wp-content/uploads', 0700, true);
        $this->root = (string) realpath($this->root);
        file_put_contents($this->root . '/wp-content/index.php', '<?php');
        file_put_contents($this->root . '/wp-content/uploads/photo.jpg', str_repeat('p', 256));
        $this->recipient = (new AgeCrypto())->generateIdentity()['recipient'];

        Functions\when('esc_html')->returnArg();
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
        Functions\when('wp_delete_file')->alias(static fn ($f) => @unlink($f)); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
        $this->scheduled = 0;
        Functions\when('wp_schedule_single_event')->alias(function (): bool {
            $this->scheduled++;
            return true;
        });
        // Capture the control-plane channel without a provisioned site key.
        \Patchwork\redefine('WPMgr\Agent\Signer::signHeaders', fn (): array => ['X-Test-Signed' => '1']); // Instance method: the closure must not be static.
        $this->posts = [];
        Functions\when('wp_remote_post')->alias(function ($url, $args) {
            $url = (string) $url;
            if (str_ends_with($url, '/presign')) {
                return ['response' => ['code' => 200], 'body' => (string) json_encode(['uploads' => []])];
            }
            if (str_ends_with($url, '/manifest')) {
                return ['response' => ['code' => 200], 'body' => (string) json_encode(['ok' => true, 'chunk_count' => 0, 'stored_count' => 0])];
            }
            $body = json_decode((string) $args['body'], true);
            if (str_ends_with($url, '/progress') && is_array($body) && isset($body['phase'])) {
                $this->posts[] = ['phase' => (string) $body['phase'], 'detail' => (array) ($body['phase_detail'] ?? [])];
            }
            return ['response' => ['code' => 200], 'body' => '{}'];
        });
        Functions\when('is_wp_error')->alias(static fn ($v) => $v instanceof \WP_Error);
        Functions\when('wp_remote_retrieve_response_code')->alias(static fn ($r) => is_array($r) ? ($r['response']['code'] ?? 0) : 0);
        Functions\when('wp_remote_retrieve_body')->alias(static fn ($r) => is_array($r) ? ($r['body'] ?? '') : '');
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        $this->removeTree($this->root);
        parent::tear_down();
    }

    /** @return array<string,array{0:string,1:array<string,int>,2:int}> entry point, row columns, progress age */
    public static function giveUpsThatSpareALiveWorker(): array
    {
        return [
            'resume cap'                  => ['resumeIfStalled', ['started_at' => -600, 'last_progress_at' => -200, 'resume_count' => 6], 200],
            'no progress past 2h, resume' => ['resumeIfStalled', ['started_at' => -3 * 3600, 'last_progress_at' => -200], 2 * 3600],
            'no progress past 2h, dispatch' => ['dispatch', ['started_at' => -3 * 3600, 'last_progress_at' => -200], 2 * 3600],
        ];
    }

    /**
     * @dataProvider giveUpsThatSpareALiveWorker
     * @param array<string,int> $columns Offsets from now for the time columns, plain values otherwise.
     */
    public function testWorkerThatTakesTheRunLockBeforeTheGiveUpKeepsItsRowAndFinishes(string $entry, array $columns, int $progressAge): void
    {
        $wpdb = $this->seed($columns, $progressAge);
        $worker = new \Fiber(fn (): string => (new TaskRunner($this->params()))->run());
        // The watchdog's probe found the lock free; a delayed worker takes it now.
        $wpdb->afterFirstRelease = static function () use ($worker): void { $worker->start(); };

        Watchdog::$entry(self::ID);

        self::assertTrue($worker->isSuspended(), 'the worker held the run-lock while the watchdog decided');
        $keptRow = isset($wpdb->inner->rows[self::ID]);
        $failedBeforeTheWorkerResumed = $this->failedMessages();
        $worker->resume();

        self::assertTrue($worker->isTerminated());
        self::assertSame(TaskRunner::PHASE_COMPLETED, $worker->getReturn(), 'the worker holding the run-lock finishes its backup');
        self::assertTrue($keptRow, 'the give-up leaves the row of a worker that holds the run-lock');
        self::assertSame([], $failedBeforeTheWorkerResumed, 'the watchdog reports no give-up');
        self::assertSame([], $this->failedMessages());
        self::assertContains(TaskRunner::PHASE_COMPLETED, array_column($this->posts, 'phase'));
        self::assertGreaterThan(0, $this->scheduled, 'the give-up waits for the next watchdog pass');
        self::assertArrayNotHasKey(self::ID, $wpdb->inner->rows, 'the finished worker removes its own row');
    }

    public function testTaskWhoseWorkerSavedProgressBeforeTheLockWasTakenIsNotGivenUp(): void
    {
        $wpdb = $this->seed(['started_at' => -600, 'last_progress_at' => -200, 'resume_count' => 6], 200);
        // Between the watchdog's probe and its give-up, a worker saved progress and ended.
        $wpdb->afterFirstRelease = static function () use ($wpdb): void {
            $state = TaskRunner::decodeSubStateColumn($wpdb->inner->rows[self::ID]['sub_state']);
            $state['progress'] = ['mark' => [2, 1, 3, 0, 0], 'at' => time(), 'seq' => 4];
            $wpdb->inner->rows[self::ID]['sub_state'] = (string) json_encode($state);
            $wpdb->inner->rows[self::ID]['last_progress_at'] = time();
        };

        Watchdog::resumeIfStalled(self::ID);

        self::assertArrayHasKey(self::ID, $wpdb->inner->rows, 'the task re-read under the run-lock is no longer at its cap');
        self::assertSame(6, $wpdb->inner->rows[self::ID]['resume_count']);
        self::assertSame([], $this->failedMessages());
        self::assertDirectoryExists($this->root . '/runs/' . self::ID, 'its scratch is kept for the next worker');
    }

    // ---- helpers -----------------------------------------------------------

    /** @param array<string,int> $columns */
    private function seed(array $columns, int $progressAge): LockRaceTasksWpdb
    {
        $now = time();
        foreach (['started_at', 'last_progress_at'] as $time) {
            if (isset($columns[$time])) { $columns[$time] += $now; }
        }
        $state = [
            'params'   => $this->params(),
            // The last worker saved nothing: its start is the latest progress.
            'progress' => ['mark' => [2, 0, 0, 0, 0], 'at' => $now - $progressAge, 'seq' => 3],
            'worker'   => ['seq' => 3, 'started_at' => $now - $progressAge],
        ];
        mkdir($this->root . '/runs/' . self::ID, 0700, true);
        file_put_contents($this->root . '/runs/' . self::ID . '/database.sql.gz', 'scratch of the run');
        $inner = new FakeBackupTasksWpdb();
        $inner->seedRow(self::ID, array_merge(['phase' => TaskRunner::PHASE_ARCHIVING_FILES, 'sub_state' => (string) json_encode($state)], $columns));
        $wpdb = new LockRaceTasksWpdb($inner);
        $GLOBALS['wpdb'] = $wpdb;
        return $wpdb;
    }

    /** @return array<string,mixed> */
    private function params(): array
    {
        return [
            'snapshot_id'       => self::ID,
            'kind'              => 'files',
            'age_recipient'     => $this->recipient,
            'presign_endpoint'  => 'https://cp.invalid/agent/v1/backups/' . self::ID . '/presign',
            'manifest_endpoint' => 'https://cp.invalid/agent/v1/backups/' . self::ID . '/manifest',
            'progress_endpoint' => 'https://cp.invalid/agent/v1/backups/' . self::ID . '/progress',
            'chunk_bytes'       => 4 * 1024 * 1024,
            'scratch_dir'       => $this->root . '/runs/' . self::ID,
            'wp_content_path'   => $this->root . '/wp-content',
            'db'                => ['prefix' => 'wp_'],
        ];
    }

    /** @return list<string> Messages of every `failed` report sent to the control plane. */
    private function failedMessages(): array
    {
        $messages = [];
        foreach ($this->posts as $post) {
            if ($post['phase'] === TaskRunner::PHASE_FAILED) {
                $messages[] = (string) ($post['detail']['message'] ?? '');
            }
        }
        return $messages;
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
