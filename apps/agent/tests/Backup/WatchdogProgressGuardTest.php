<?php
/**
 * GH #983: the watchdog gives a backup up only for lack of progress, never
 * for age alone below its hard ceiling; it counts only resumes whose previous
 * worker saved no progress; and every give-up fails the backup on the
 * dashboard through the runner's signed `/progress` failure channel.
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
 * @covers \WPMgr\Agent\Backup\Watchdog
 * @covers \WPMgr\Agent\Backup\TaskRunner
 */
final class WatchdogProgressGuardTest extends TestCase
{
    private const ID = '98309830-aaaa-4bbb-8ccc-dddddddddddd';

    private string $root = '';

    /** @var list<array{phase:string,detail:array<string,mixed>}> */
    private array $posts = [];

    /** @var resource|null */
    private $liveLock = null;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->root = sys_get_temp_dir() . '/wpmgr-983-watchdog-' . bin2hex(random_bytes(6));
        mkdir($this->root . '/runs', 0700, true);
        mkdir($this->root . '/wp-content', 0700, true);
        $this->root = (string) realpath($this->root);
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
        if (is_resource($this->liveLock)) {
            flock($this->liveLock, LOCK_UN);
            fclose($this->liveLock);
        }
        unset($GLOBALS['wpdb']);
        Monkey\tearDown();
        $this->removeTree($this->root);
        parent::tear_down();
    }

    // ---- Item 1: age guard -------------------------------------------------

    public function testOldTaskWhoseCheckpointAdvancedRecentlyIsNotGivenUp(): void
    {
        $wpdb = $this->seed(['started_at' => time() - 3 * 3600, 'last_progress_at' => time()], time() - 60);

        Watchdog::resumeIfStalled(self::ID);

        self::assertArrayHasKey(self::ID, $wpdb->rows, 'a 3h-old task that saved a part a minute ago keeps running');
        self::assertSame([], $this->failedPosts());
    }

    public function testDispatchNeverRefusesAnOldTaskWhoseCheckpointAdvancedRecently(): void
    {
        $wpdb = $this->seed(['started_at' => time() - 3 * 3600, 'last_progress_at' => time()], time() - 60);
        $this->holdLiveRunnerLock();

        Watchdog::dispatch(self::ID);

        self::assertArrayHasKey(self::ID, $wpdb->rows);
        self::assertSame([], $this->failedPosts());
    }

    public function testOldTaskWithoutRecentProgressIsGivenUpAndReported(): void
    {
        $wpdb = $this->seed(['started_at' => time() - 3 * 3600, 'last_progress_at' => time()], time() - 2 * 3600);

        Watchdog::resumeIfStalled(self::ID);

        self::assertArrayNotHasKey(self::ID, $wpdb->rows);
        self::assertSame(['Backup stopped: it saved no progress for over an hour. Check the site\'s free disk space and PHP error log, then run the backup again.'], $this->failedPosts());
    }

    public function testTaskPastTheHardCeilingIsGivenUpWhateverItsProgress(): void
    {
        $wpdb = $this->seed(['started_at' => time() - Watchdog::HARD_CEILING_SECONDS - 60, 'last_progress_at' => time()], time());

        Watchdog::resumeIfStalled(self::ID);

        self::assertArrayNotHasKey(self::ID, $wpdb->rows);
        self::assertCount(1, $this->failedPosts());
        self::assertStringContainsString('after 24 hours', $this->failedPosts()[0]);
    }

    // ---- Item 2: only resumes without progress count -----------------------

    public function testResumeAfterAWorkerThatSavedProgressIsNotCounted(): void
    {
        $wpdb = $this->seed(['last_progress_at' => time() - 200, 'resume_count' => 6], time() - 200, 4, 3);

        Watchdog::resumeIfStalled(self::ID);

        self::assertSame([6], $this->resumeCountWrites($wpdb), 'the cap is not reached: the worker before this resume saved progress');
        self::assertSame([], array_values(array_filter($this->failedPosts(), static fn (string $m): bool => str_contains($m, 'host ended'))));
    }

    public function testResumeAfterAWorkerThatSavedNoProgressIsCounted(): void
    {
        $wpdb = $this->seed(['last_progress_at' => time() - 200, 'resume_count' => 2], time() - 200, 3, 3);

        Watchdog::resumeIfStalled(self::ID);

        self::assertSame([3], $this->resumeCountWrites($wpdb));
    }

    public function testCapGivesUpAfterResumesWithoutProgressAndReportsIt(): void
    {
        $wpdb = $this->seed(['last_progress_at' => time() - 200, 'resume_count' => 6], time() - 200, 3, 3);

        Watchdog::resumeIfStalled(self::ID);

        self::assertArrayNotHasKey(self::ID, $wpdb->rows);
        self::assertSame([], $this->resumeCountWrites($wpdb), 'no worker is started after the cap');
        self::assertSame(['Backup stopped: the host ended the backup worker 6 times before it saved any progress. Raise the PHP time or memory limit, or exclude large files, then run the backup again.'], $this->failedPosts());
    }

    // ---- Item 4: every give-up is reported ---------------------------------

    public function testLatePhaseGiveUpIsReportedAndALiveUploaderKeepsItsRow(): void
    {
        $wpdb = $this->seed(['phase' => TaskRunner::PHASE_ENCRYPTING_UPLOADING, 'last_progress_at' => time() - 400], time() - 400);
        $this->holdLiveRunnerLock();
        Watchdog::resumeIfStalled(self::ID);
        self::assertArrayHasKey(self::ID, $wpdb->rows, 'a slow upload whose runner holds the run-lock is left to finish');
        self::assertSame([], $this->failedPosts());

        flock($this->liveLock, LOCK_UN);
        Watchdog::resumeIfStalled(self::ID);
        self::assertArrayNotHasKey(self::ID, $wpdb->rows);
        self::assertSame(['Backup stopped: its upload stalled and the worker running it ended. Check that the site can reach the control plane, then run the backup again.'], $this->failedPosts());
    }

    public function testDispatchStaleGiveUpIsReported(): void
    {
        $wpdb = $this->seed(['started_at' => time() - 3 * 3600], time() - 3 * 3600);

        Watchdog::dispatch(self::ID);

        self::assertArrayNotHasKey(self::ID, $wpdb->rows);
        self::assertCount(1, $this->failedPosts());
    }

    // ---- helpers -----------------------------------------------------------

    /**
     * Seed an archiving task whose durable progress last advanced at
     * $progressAt with sequence $seq, and whose last worker started at $workerSeq.
     *
     * @param array<string,mixed> $row
     */
    private function seed(array $row, int $progressAt, int $seq = 1, int $workerSeq = 0): FakeBackupTasksWpdb
    {
        $wpdb = new FakeBackupTasksWpdb();
        $state = [
            'params'   => $this->params(),
            'progress' => ['mark' => [2, $seq, 0, 0, 0], 'at' => $progressAt, 'seq' => $seq],
            'worker'   => ['seq' => $workerSeq, 'started_at' => $progressAt],
        ];
        $wpdb->seedRow(self::ID, array_merge(['phase' => TaskRunner::PHASE_ARCHIVING_FILES, 'sub_state' => (string) json_encode($state)], $row));
        $GLOBALS['wpdb'] = $wpdb;
        return $wpdb;
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

    private function holdLiveRunnerLock(): void
    {
        $handle = fopen($this->root . '/runs/' . self::ID . '.lock', 'c');
        self::assertNotFalse($handle);
        self::assertTrue(flock($handle, LOCK_EX | LOCK_NB));
        $this->liveLock = $handle;
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
    private function resumeCountWrites(FakeBackupTasksWpdb $wpdb): array
    {
        $writes = [];
        foreach ($wpdb->updates as $update) {
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
