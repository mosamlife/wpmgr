<?php
/**
 * Tests for the M5.6 / ADR-034 RestoreCommand — the WPvivid-pattern cron-
 * dispatched restore entry point. The legacy M4 in-process restore tests
 * (ordered reassembly, blake3 verify, etc.) were retired when the command
 * was refactored to mirror BackupCommand. The new RestoreCommand is a thin
 * shim: validate, dedup, seed task row, schedule cron, return ACK in ms.
 * The real work moved to RestoreRunner + RestoreWatchdog + DbRestorer +
 * FilesRestorer; those have their own focused tests where useful.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use WPMgr\Agent\Commands\RestoreCommand;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\RestoreCommand
 */
final class RestoreCommandTest extends TestCase
{
    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    public function test_name_is_restore(): void
    {
        $this->assertSame('restore', (new RestoreCommand())->name());
    }

    public function test_refuses_missing_ids(): void
    {
        $cmd = new RestoreCommand();
        $res = $cmd->execute([], []);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('missing', $res['detail']);
    }

    public function test_refuses_invalid_snapshot_id(): void
    {
        $cmd = new RestoreCommand();
        $res = $cmd->execute([], [
            'snapshot_id' => 'not-a-uuid',
            'restore_id'  => '11111111-1111-1111-1111-111111111111',
            'kind'        => 'files',
        ]);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('invalid snapshot id', $res['detail']);
    }

    public function test_refuses_invalid_restore_id(): void
    {
        $cmd = new RestoreCommand();
        $res = $cmd->execute([], [
            'snapshot_id' => '11111111-1111-1111-1111-111111111111',
            'restore_id'  => 'bad',
            'kind'        => 'files',
        ]);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('invalid restore id', $res['detail']);
    }

    public function test_refuses_unknown_kind(): void
    {
        $cmd = new RestoreCommand();
        $res = $cmd->execute([], [
            'snapshot_id' => '11111111-1111-1111-1111-111111111111',
            'restore_id'  => '22222222-2222-2222-2222-222222222222',
            'kind'        => 'banana',
        ]);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('invalid kind', $res['detail']);
    }

    public function test_refuses_missing_chunk_downloads(): void
    {
        $cmd = new RestoreCommand();
        $res = $cmd->execute([], [
            'snapshot_id' => '11111111-1111-1111-1111-111111111111',
            'restore_id'  => '22222222-2222-2222-2222-222222222222',
            'kind'        => 'full',
        ]);
        $this->assertFalse($res['ok']);
        $this->assertStringContainsString('chunk_downloads', $res['detail']);
    }
}
