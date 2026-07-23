<?php
/**
 * GH #279: FilesArchiver must emit periodic intra-phase progress heartbeats
 * during the long zip-pack loop so the CP watchdog's soft-stall threshold
 * (default 180s) never trips while the archiver is still making genuine
 * forward progress on a few-but-large-files run (where the file-count
 * trigger, PROGRESS_EVERY_FILES, may not fire for a long time).
 *
 * Three things are covered:
 *   1. the wall-clock gate itself (maybeEmitHeartbeat()) fires once the
 *      configured interval has elapsed, and resets on every emit (whether
 *      heartbeat- or file-count/rotation-triggered) so it never double-fires
 *      right after a real tick;
 *   2. archive() actually wires that gate into the pack loop: with a
 *      near-zero interval and a file count under PROGRESS_EVERY_FILES, at
 *      least one heartbeat-flagged tick must appear;
 *   3. a "finalizing part" heartbeat fires immediately before every
 *      closeActivePart() call (ZipArchive::close() is a single blocking
 *      finalize with no progress hook of its own).
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionClass;
use WPMgr\Agent\Backup\FilesArchiver;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\FilesArchiver
 */
final class FilesArchiverHeartbeatTest extends TestCase
{
    private string $sourceDir = '';

    private string $outDir = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('esc_html')->returnArg();
        $base            = sys_get_temp_dir() . DIRECTORY_SEPARATOR . 'wpmgr-files-archiver-hb-' . bin2hex(random_bytes(4));
        $this->sourceDir = $base . DIRECTORY_SEPARATOR . 'src';
        $this->outDir    = $base . DIRECTORY_SEPARATOR . 'out';
        mkdir($this->sourceDir, 0755, true);
        mkdir($this->outDir, 0755, true);
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        if ($this->sourceDir !== '' && is_dir($this->sourceDir)) {
            $this->rrmdir(dirname($this->sourceDir));
        }
        parent::tear_down();
    }

    /**
     * Direct unit test of the wall-clock gate: seeding a stale
     * `lastProgressEmitAt` (older than the configured interval) must fire a
     * heartbeat-flagged tick; an immediate second call (fresh timestamp,
     * reset by the first emit) must be a no-op.
     */
    public function test_maybe_emit_heartbeat_fires_once_interval_elapsed_and_resets(): void
    {
        $archiver = new FilesArchiver($this->sourceDir, [], ['heartbeat_interval_seconds' => 5.0]);

        $reflection    = new ReflectionClass(FilesArchiver::class);
        $lastEmitProp  = $reflection->getProperty('lastProgressEmitAt');
        $heartbeatMeth = $reflection->getMethod('maybeEmitHeartbeat');

        $calls    = [];
        $progress = function (string $phase, array $detail) use (&$calls): void {
            $calls[] = ['phase' => $phase, 'detail' => $detail];
        };

        // Seed a stale last-emit timestamp, well past the 5s interval.
        $lastEmitProp->setValue($archiver, microtime(true) - 10.0);

        $heartbeatMeth->invoke($archiver, $progress, ['files_done' => 3]);

        self::assertCount(1, $calls, 'heartbeat must fire once the interval has elapsed');
        self::assertSame('archiving_files', $calls[0]['phase']);
        self::assertTrue($calls[0]['detail']['heartbeat'] ?? false, 'the heartbeat flag must be merged into the detail payload');
        self::assertSame(3, $calls[0]['detail']['files_done']);

        // Calling again immediately must be a no-op: emitProgress() already
        // reset the window on the call above.
        $heartbeatMeth->invoke($archiver, $progress, ['files_done' => 4]);
        self::assertCount(1, $calls, 'a second call inside the interval must not fire again');
    }

    /**
     * archive() must wire the wall-clock gate into the pack loop: with a
     * near-zero heartbeat interval and a file count well under
     * PROGRESS_EVERY_FILES (50), any intermediate progress ticks besides the
     * terminal "done" tick can only be heartbeat-driven.
     */
    public function test_archive_emits_wall_clock_heartbeat_when_file_count_trigger_never_fires(): void
    {
        if (!class_exists(\ZipArchive::class)) {
            self::markTestSkipped('ext-zip not available');
        }

        // 10 small files: well under PROGRESS_EVERY_FILES (50) and well
        // under the default max_part_bytes, so neither the file-count nor
        // the rotation trigger fires.
        for ($i = 0; $i < 10; $i++) {
            file_put_contents($this->sourceDir . '/file-' . $i . '.txt', str_repeat('X', 2048));
        }

        $archiver = new FilesArchiver($this->sourceDir, [], [
            // Effectively "always due" without a real sleep in the test.
            'heartbeat_interval_seconds' => 0.0000001,
        ]);

        $calls    = [];
        $progress = function (string $phase, array $detail) use (&$calls): void {
            $calls[] = $detail;
        };

        $result = $archiver->archive($this->outDir, [], $progress);
        self::assertTrue($result['done'] ?? false);
        self::assertSame(10, $result['files_total']);

        $heartbeats = array_values(array_filter(
            $calls,
            static fn (array $d): bool => ($d['heartbeat'] ?? false) === true
        ));
        self::assertNotEmpty(
            $heartbeats,
            'expected at least one wall-clock heartbeat tick with 10 files and a near-zero interval'
        );
        // Every heartbeat tick carries the same detail shape as a normal tick.
        foreach ($heartbeats as $hb) {
            self::assertArrayHasKey('files_done', $hb);
            self::assertArrayHasKey('files_total', $hb);
        }
    }

    /**
     * With a default (30s) heartbeat interval and a small file count, NO
     * heartbeat ticks fire in a fast test run; the wall-clock gate must not
     * be a hair-trigger on every call. Guards against a regression where the
     * interval check is inverted (fires when it should NOT).
     */
    public function test_archive_emits_no_heartbeat_within_default_interval(): void
    {
        if (!class_exists(\ZipArchive::class)) {
            self::markTestSkipped('ext-zip not available');
        }

        for ($i = 0; $i < 5; $i++) {
            file_put_contents($this->sourceDir . '/file-' . $i . '.txt', str_repeat('Y', 512));
        }

        // Default heartbeat_interval_seconds (30s); a fast unit test run
        // never gets remotely close to that.
        $archiver = new FilesArchiver($this->sourceDir);

        $calls    = [];
        $progress = function (string $phase, array $detail) use (&$calls): void {
            $calls[] = $detail;
        };

        $result = $archiver->archive($this->outDir, [], $progress);
        self::assertTrue($result['done'] ?? false);

        $heartbeats = array_filter($calls, static fn (array $d): bool => ($d['heartbeat'] ?? false) === true);
        self::assertEmpty($heartbeats, 'no heartbeat should fire well within a 30s default interval on a fast test run');
    }

    /**
     * A "finalizing part" heartbeat (stage=finalizing_part) must fire
     * immediately before every closeActivePart() call. Forces rotation after
     * every single file (tiny max_part_bytes) so each part-close is
     * individually observable, and asserts the finalizing_part tick count
     * matches the number of parts actually produced.
     */
    public function test_finalizing_part_heartbeat_precedes_each_part_close(): void
    {
        if (!class_exists(\ZipArchive::class)) {
            self::markTestSkipped('ext-zip not available');
        }

        file_put_contents($this->sourceDir . '/alpha.txt', str_repeat('A', 16 * 1024));
        file_put_contents($this->sourceDir . '/beta.txt', str_repeat('B', 16 * 1024));
        file_put_contents($this->sourceDir . '/gamma.txt', str_repeat('C', 16 * 1024));

        $archiver = new FilesArchiver(
            $this->sourceDir,
            [],
            [
                // Far smaller than any test file: forces a rotation (and thus
                // a closeActivePart() call) right after each file is added.
                'max_part_bytes'   => 1024,
                'max_part_entries' => 10000,
            ]
        );

        $calls    = [];
        $progress = function (string $phase, array $detail) use (&$calls): void {
            $calls[] = $detail;
        };

        $result = $archiver->archive($this->outDir, [], $progress);
        self::assertTrue($result['done'] ?? false);
        self::assertGreaterThanOrEqual(2, count($result['parts']), 'small max_part_bytes should force rotation');

        $finalizing = array_values(array_filter(
            $calls,
            static fn (array $d): bool => ($d['stage'] ?? null) === 'finalizing_part'
        ));

        self::assertSame(
            count($result['parts']),
            count($finalizing),
            'one finalizing_part heartbeat must precede each closeActivePart() call that produced a part'
        );
    }

    /**
     * Recursively delete a directory tree (used by tear_down).
     *
     * @param string $path Absolute path.
     * @return void
     */
    private function rrmdir(string $path): void
    {
        if (!is_dir($path)) {
            if (is_file($path) || is_link($path)) {
                @unlink($path);
            }
            return;
        }
        $entries = scandir($path);
        if ($entries === false) {
            return;
        }
        foreach ($entries as $entry) {
            if ($entry === '.' || $entry === '..') {
                continue;
            }
            $this->rrmdir($path . DIRECTORY_SEPARATOR . $entry);
        }
        @rmdir($path);
    }
}
