<?php
/**
 * GitHub issue #256: RestoreRunner::runCleanup()'s self-poisoning one-shot
 * `wpmgr_restore_oldfiles_gc` scheduling bug.
 *
 * Root cause: an event scheduled but OVERDUE (WP-Cron never ticked past its
 * timestamp, on a quiet site or one with DISABLE_WP_CRON set) stays in the
 * cron array forever, so `wp_next_scheduled()` keeps returning it as truthy
 * and the pre-#256 `if (!wp_next_scheduled(...))` guard permanently declined
 * to schedule a replacement for EVERY later restore: one missed tick
 * poisons the trigger for good. The fix treats an overdue timestamp as
 * "nothing usefully scheduled": it clears the stale entry via
 * `wp_unschedule_event()` and schedules a fresh one.
 *
 * Reaches the private runCleanup() directly via reflection, mirroring
 * TaskRunnerCleanupOnFailedTest's convention: it only touches the
 * filesystem + the WP-Cron scheduling functions (no $wpdb), so this stays
 * hermetic per the "self-contained fakes" test convention.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionClass;
use WPMgr\Agent\Backup\RestoreRunner;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\RestoreRunner
 */
final class RestoreOldFilesGcRescheduleTest extends TestCase
{
    private const GC_HOOK = 'wpmgr_restore_oldfiles_gc';

    private string $root        = '';
    private string $scratchDir  = '';
    private string $oldFilesDir = '';

    /** @var list<array{ts:int,hook:string}> */
    private array $scheduled = [];

    /** @var list<array{ts:int,hook:string}> */
    private array $unscheduled = [];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root        = sys_get_temp_dir() . '/wpmgr-oldfiles-resched-' . bin2hex(random_bytes(6));
        $this->scratchDir  = $this->root . '/scratch';
        $this->oldFilesDir = $this->root . '/.wpmgr-old-files-aaaaaaaaaaaa';
        mkdir($this->scratchDir, 0755, true);
        mkdir($this->oldFilesDir, 0755, true);
        file_put_contents($this->oldFilesDir . '/good.txt', 'pre-restore-tree');

        $this->scheduled   = [];
        $this->unscheduled = [];
        Functions\when('wp_delete_file')->alias(static function ($f) {
            return @unlink($f); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
        });
        Functions\when('wp_schedule_single_event')->alias(function (int $ts, string $hook, array $args = []): bool {
            $this->scheduled[] = ['ts' => $ts, 'hook' => $hook];
            return true;
        });
        Functions\when('wp_unschedule_event')->alias(function (int $ts, string $hook, array $args = []): bool {
            $this->unscheduled[] = ['ts' => $ts, 'hook' => $hook];
            return true;
        });
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->root);
        Monkey\tearDown();
        parent::tear_down();
    }

    private function buildRunner(): RestoreRunner
    {
        return new RestoreRunner([
            'snapshot_id'       => '55555555-5555-4555-8555-555555555555',
            'restore_id'        => '66666666-6666-4666-8666-666666666666',
            'kind'              => 'full',
            'progress_endpoint' => '',
            'scratch_dir'       => $this->scratchDir,
            'wp_content_path'   => $this->root . '/wp-content',
            'wp_root'           => $this->root . '/wproot',
            'db'                => ['host' => '', 'user' => '', 'password' => '', 'name' => '', 'prefix' => 'wp_'],
        ]);
    }

    /**
     * @param array<string,mixed> $subState
     * @return array<string,mixed>
     */
    private function runCleanup(RestoreRunner $runner, array $subState): array
    {
        $reflection = new ReflectionClass(RestoreRunner::class);
        $method     = $reflection->getMethod('runCleanup');

        return $method->invoke($runner, $subState);
    }

    /** @return array<string,mixed> */
    private function swapFilesSubState(): array
    {
        return [
            'swap_files' => [
                'done'          => true,
                'old_files_dir' => $this->oldFilesDir,
                'mode'          => 'legacy_whole',
            ],
        ];
    }

    public function test_no_existing_schedule_schedules_a_fresh_event(): void
    {
        Functions\when('wp_next_scheduled')->justReturn(false);

        $this->runCleanup($this->buildRunner(), $this->swapFilesSubState());

        $this->assertNotEmpty($this->scheduled, 'a fresh event must be scheduled when nothing is currently scheduled');
        $this->assertSame(self::GC_HOOK, $this->scheduled[0]['hook']);
        $this->assertEmpty($this->unscheduled, 'nothing to unschedule when no event exists');
    }

    public function test_future_scheduled_event_is_left_alone_and_no_duplicate_is_scheduled(): void
    {
        $future = time() + 1800;
        Functions\when('wp_next_scheduled')->justReturn($future);

        $this->runCleanup($this->buildRunner(), $this->swapFilesSubState());

        $this->assertEmpty($this->scheduled, 'an event already scheduled in the future must not be duplicated');
        $this->assertEmpty($this->unscheduled, 'a future (not overdue) event must never be unscheduled');
    }

    public function test_overdue_scheduled_event_is_cleared_and_a_fresh_one_is_scheduled(): void
    {
        // GH #256: an event scheduled for the past (WP-Cron never ticked
        // it) previously stayed in the cron array forever, so
        // wp_next_scheduled() kept returning it as truthy and permanently
        // suppressed every later restore's attempt to schedule a
        // replacement.
        $overdue = time() - 3600;
        Functions\when('wp_next_scheduled')->justReturn($overdue);

        $this->runCleanup($this->buildRunner(), $this->swapFilesSubState());

        $this->assertNotEmpty($this->unscheduled, 'the overdue, never-fired event must be cleared');
        $this->assertSame($overdue, $this->unscheduled[0]['ts']);
        $this->assertSame(self::GC_HOOK, $this->unscheduled[0]['hook']);

        $this->assertNotEmpty($this->scheduled, 'a fresh replacement event must be scheduled once the overdue one is cleared');
        $this->assertSame(self::GC_HOOK, $this->scheduled[0]['hook']);
        $this->assertGreaterThan(time(), $this->scheduled[0]['ts'], 'the replacement must be scheduled for the future, not re-using the overdue timestamp');
    }

    /** Recursive delete used only for test fixture cleanup. */
    private function rrmdir(string $dir): void
    {
        if ($dir === '' || !is_dir($dir)) {
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
            if (is_dir($path) && !is_link($path)) {
                $this->rrmdir($path);
            } else {
                @unlink($path); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
            }
        }
        @rmdir($dir); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- test-only fixture cleanup
    }
}
