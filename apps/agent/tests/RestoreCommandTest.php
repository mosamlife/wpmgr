<?php
/**
 * Tests for the M5.6 / ADR-034 RestoreCommand — the cron-
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
use Brain\Monkey\Functions;
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

    // ==========================================================
    // ADR-036 P1 backup-destinations completeness fix (Phase 1): a
    // destination_kind=local snapshot's chunks carry hash + size but NO
    // presigned_url (see class-restore-command.php docblock). Every other
    // destination_kind still requires one — this is the contract-shape
    // parsing test for RestoreCommand::parseChunkDownloads().
    // ==========================================================

    /**
     * A url-less chunk entry under destination_kind=local must NOT be
     * dropped by parseChunkDownloads() — proven by observing that the
     * request gets PAST the "no chunk_downloads / manifest entries
     * supplied" refusal.
     *
     * Depending on ambient WP_CONTENT_DIR / $wpdb state left behind by other
     * test files sharing this PHPUnit process, execute() may sail all the
     * way through preflight/dedup/scratch-dir/scheduling — so the WP cron
     * functions at the very end of the happy path are mocked defensively
     * (this test does not care whether the request ultimately succeeds; it
     * only asserts the "not dropped" property).
     */
    public function test_local_destination_kind_accepts_chunk_without_presigned_url(): void
    {
        Functions\when('wp_schedule_single_event')->justReturn(true);
        Functions\when('spawn_cron')->justReturn(true);

        $snapshotId = '11111111-1111-1111-1111-111111111111';
        $restoreId  = '22222222-2222-2222-2222-222222222222';

        $cmd = new RestoreCommand();
        $res = $cmd->execute([], [
            'snapshot_id'      => $snapshotId,
            'restore_id'       => $restoreId,
            'kind'             => 'full',
            'destination_kind' => 'local',
            'chunk_downloads'  => [
                [
                    'logical_path' => 'database.sql.gz',
                    'chunks'       => [
                        ['hash' => str_repeat('a', 64), 'size' => 1024],
                    ],
                ],
            ],
        ]);
        $this->assertNotSame(
            'no chunk_downloads / manifest entries supplied',
            $res['detail'],
            'a destination_kind=local chunk with no presigned_url must not be dropped'
        );

        // Best-effort: if the request reached prepareScratchDir() (ambient
        // WP_CONTENT_DIR was already defined by another test in this shared
        // process), clean up the exact scratch dir these fixed IDs produce.
        if (defined('WP_CONTENT_DIR') && is_string(WP_CONTENT_DIR) && WP_CONTENT_DIR !== '') {
            $shortRestoreId = substr(preg_replace('/[^a-f0-9]/i', '', $restoreId) ?? '', 0, 12);
            $this->rrmdir(WP_CONTENT_DIR . '/wpmgr-agent/restores/' . $snapshotId . '-' . $shortRestoreId);
        }
    }

    /**
     * Regression: the SAME url-less chunk shape under a cp/absent
     * destination_kind must still be dropped (a cp/s3_compat chunk with no
     * presigned_url is malformed) — the request refuses with the exact
     * "no chunk_downloads" message once every chunk has been filtered out.
     */
    public function test_non_local_destination_kind_still_requires_presigned_url(): void
    {
        $cmd = new RestoreCommand();
        $res = $cmd->execute([], [
            'snapshot_id'     => '11111111-1111-1111-1111-111111111111',
            'restore_id'      => '22222222-2222-2222-2222-222222222222',
            'kind'            => 'full',
            // destination_kind omitted entirely -> defaults to 'cp'.
            'chunk_downloads' => [
                [
                    'logical_path' => 'database.sql.gz',
                    'chunks'       => [
                        ['hash' => str_repeat('a', 64), 'size' => 1024],
                    ],
                ],
            ],
        ]);
        $this->assertFalse($res['ok']);
        $this->assertSame('no chunk_downloads / manifest entries supplied', $res['detail']);
    }

    /** Recursive rmdir for best-effort test cleanup. */
    private function rrmdir(string $dir): void
    {
        if (!is_dir($dir)) {
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
            $path = $dir . DIRECTORY_SEPARATOR . $item;
            if (is_link($path) || is_file($path)) {
                @unlink($path);
            } elseif (is_dir($path)) {
                $this->rrmdir($path);
            }
        }
        @rmdir($dir);
    }
}
