<?php
/**
 * GitHub issue #256 post-ship review, finding 5: reclaimRunScratch() must
 * follow the sidecar-spill pointer the same way TaskRunner::loadTask()
 * does, or it silently no-ops on exactly the biggest runs: the ones whose
 * encoded sub_state exceeded TaskRunner's SUBSTATE_SIDECAR_THRESHOLD
 * (48 KiB) and got spilled to a `<scratch>/task_substate.json` sidecar
 * file, leaving only a small `{"_sidecar":true,"file":"..."}` pointer
 * inline in the DB column (see TaskRunner::saveTaskState()).
 *
 * A bare json_decode() of that column (the pre-fix Watchdog::decodeSubState()
 * behavior) never sees `sub_state.params` for a spilled row, so
 * reclaimRunScratch() could not resolve scratch_dir and returned without
 * deleting anything, on precisely the runs with the most scratch to
 * reclaim (the ones large enough to have spilled in the first place).
 *
 * This fixture builds a spilled sub_state the same shape TaskRunner's own
 * saveTaskState() produces (see TaskRunnerSubStatePacketTest for the
 * production-code round trip), then drives it through a Watchdog give-up
 * path and asserts the scratch directory IS reclaimed.
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

/**
 * @covers \WPMgr\Agent\Backup\Watchdog
 * @covers \WPMgr\Agent\Backup\TaskRunner
 */
final class WatchdogReclaimSubstateSidecarTest extends TestCase
{
    private const SNAPSHOT_ID = '77777777-8888-4999-8aaa-bbbbbbbbbbbb';

    /** Mirrors TaskRunner::SUBSTATE_SIDECAR_NAME, a private class constant. */
    private const SIDECAR_FILENAME = 'task_substate.json';

    private string $root       = '';
    private string $scratchDir = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        $this->root       = sys_get_temp_dir() . '/wpmgr-reclaim-sidecar-test-' . bin2hex(random_bytes(6));
        $this->scratchDir = $this->root . '/scratch/' . self::SNAPSHOT_ID;
        mkdir($this->scratchDir, 0700, true);

        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
        Functions\when('wp_schedule_single_event')->justReturn(true);
        Functions\when('wp_delete_file')->alias(static function ($f) {
            return @unlink($f); // phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- test-only fixture cleanup
        });
    }

    protected function tear_down(): void
    {
        unset($GLOBALS['wpdb']);
        $this->rrmdir($this->root);
        Monkey\tearDown();
        parent::tear_down();
    }

    /** @return array<string,mixed> Minimal-but-valid TaskRunner params. */
    private function runnerParams(): array
    {
        return [
            'snapshot_id'       => self::SNAPSHOT_ID,
            'kind'              => 'files',
            'age_recipient'     => 'age1' . str_repeat('q', 58),
            'presign_endpoint'  => 'https://cp.invalid/agent/v1/backups/sidecar-test/presign',
            'manifest_endpoint' => 'https://cp.invalid/agent/v1/backups/sidecar-test/manifest',
            'progress_endpoint' => '',
            'chunk_bytes'       => 4 * 1024 * 1024,
            'scratch_dir'       => $this->scratchDir,
            'wp_content_path'   => $this->root . '/wp-content',
            'db'                => ['host' => 'localhost', 'user' => 'u', 'password' => 'p', 'name' => 'n', 'prefix' => 'wp_'],
        ];
    }

    /**
     * Build a spilled sub_state fixture the same shape saveTaskState()
     * produces once the encoded cursor exceeds SUBSTATE_SIDECAR_THRESHOLD
     * (48 KiB): the full cursor, including sub_state.params (the very thing
     * reclaimRunScratch() needs to find scratch_dir), written to
     * `<scratch>/task_substate.json`, with only a small pointer left
     * inline for the DB column.
     *
     * @return string The pointer JSON that belongs in the row's sub_state column.
     */
    private function seedSpilledSubState(): string
    {
        // A large inline tombstones array is exactly what pushed a real
        // sub_state over the sidecar threshold before ADR-051 moved
        // tombstones to an on-disk flat file (see
        // TaskRunnerSubStatePacketTest); reused here purely to build a
        // realistically oversized fixture.
        $bigTombstones = [];
        for ($i = 0; $i < 3000; $i++) {
            $bigTombstones[] = 'plugins/some-plugin/file-' . $i . '.php';
        }

        $fullSubState = [
            'params' => $this->runnerParams(),
            'files'  => [
                'done'       => true,
                'parts'      => ['wp-content.g000.part001.zip'],
                'tombstones' => $bigTombstones,
            ],
        ];
        $encoded = (string) json_encode($fullSubState);
        $this->assertGreaterThan(48 * 1024, strlen($encoded), 'fixture must actually exceed the sidecar threshold to be a faithful test');

        $sidecarPath = $this->scratchDir . DIRECTORY_SEPARATOR . self::SIDECAR_FILENAME;
        file_put_contents($sidecarPath, $encoded);

        return (string) json_encode(['_sidecar' => true, 'file' => $sidecarPath]);
    }

    /**
     * FAILS against the pre-fix code: Watchdog::decodeSubState() used a
     * bare json_decode() of the raw column, which for a spilled row
     * returns only the `{"_sidecar":true,...}` pointer object and never
     * sees sub_state.params, so reclaimRunScratch() could not resolve
     * scratch_dir and silently did nothing, leaking exactly the largest
     * runs' scratch directories (short of the age-gated
     * BackupJanitor::gcRuns() backstop).
     */
    public function test_hard_ceiling_guard_reclaims_scratch_when_substate_has_spilled_to_the_sidecar(): void
    {
        file_put_contents($this->scratchDir . '/database.sql.gz', 'fixture-bytes');
        file_put_contents($this->scratchDir . '/wp-content.g000.part001.zip', 'fixture-zip-bytes');

        $pointerJson = $this->seedSpilledSubState();

        $wpdb = new FakeBackupTasksWpdb();
        $wpdb->seedRow(self::SNAPSHOT_ID, [
            'phase'            => TaskRunner::PHASE_ARCHIVING_FILES,
            'started_at'       => time() - 7300, // past the 7200s hard ceiling.
            'last_progress_at' => time() - 7300,
            'sub_state'        => $pointerJson,
        ]);
        $GLOBALS['wpdb'] = $wpdb;

        Watchdog::resumeIfStalled(self::SNAPSHOT_ID);

        $this->assertDirectoryDoesNotExist(
            $this->scratchDir,
            'a spilled sub_state must still resolve scratch_dir (by following the sidecar pointer) and reclaim the scratch directory'
        );
        $this->assertArrayNotHasKey(self::SNAPSHOT_ID, $wpdb->rows);
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
