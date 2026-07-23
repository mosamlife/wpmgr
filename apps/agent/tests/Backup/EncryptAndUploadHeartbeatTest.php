<?php
/**
 * GH #279: EncryptAndUpload must emit periodic intra-phase progress
 * heartbeats during the long encrypt (pass 1) and upload (pass 2) chunk
 * loops so the CP watchdog's soft-stall threshold (default 180s) never trips
 * on a slow chunk (a large fread(), a slow network PUT) while genuine forward
 * progress is being made -- covering the case where the per-N-chunks trigger
 * (PROGRESS_EVERY_ENCRYPT / PROGRESS_EVERY_UPLOAD, both 4) would otherwise
 * stay quiet for a while.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use Brain\Monkey\Functions;
use ReflectionClass;
use WPMgr\Agent\Backup\EncryptAndUpload;
use WPMgr\Agent\Support\AgeCrypto;
use WPMgr\Agent\Support\BackupTransport;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\EncryptAndUpload
 */
final class EncryptAndUploadHeartbeatTest extends TestCase
{
    private string $scratchDir = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));

        $this->scratchDir = sys_get_temp_dir() . '/wpmgr-encrypt-upload-hb-' . bin2hex(random_bytes(6));
        if (!is_dir($this->scratchDir) && !mkdir($this->scratchDir, 0700, true) && !is_dir($this->scratchDir)) {
            self::fail('could not create scratch dir for test');
        }
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->scratchDir);
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * A transport stub that records PUTs without doing real network I/O.
     * Matches the pattern used by BackupCommandTest.
     */
    private function transport(): BackupTransport
    {
        return new class extends BackupTransport {
            /** @var array<string,string> hash => uploaded bytes PUT */
            public array $puts = [];

            public function __construct()
            {
            }

            public function presignChunks(string $endpoint, string $snapshotId, array $hashes): array
            {
                $uploads = [];
                foreach ($hashes as $h) {
                    $uploads[$h] = 'https://s3.example/put/' . $h;
                }
                return $uploads;
            }

            public function putChunk(string $presignedUrl, string $ciphertext): bool
            {
                $hash = substr($presignedUrl, strlen('https://s3.example/put/'));
                $this->puts[$hash] = $ciphertext;
                return true;
            }
        };
    }

    /**
     * Build an EncryptAndUpload pipeline with a near-zero heartbeat interval
     * (the 9th constructor arg) so the wall-clock gate is effectively
     * "always due" without a real sleep in the test.
     */
    private function makePipeline(int $chunkBytes, BackupTransport $transport, float $heartbeatIntervalSeconds = 0.0000001): EncryptAndUpload
    {
        return new EncryptAndUpload(
            new AgeCrypto(),
            $transport,
            'snap-hb-1',
            'age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq',
            'https://cp.example/agent/v1/backups/snap-hb-1/presign',
            'https://cp.example/agent/v1/backups/snap-hb-1/manifest',
            $chunkBytes,
            null,
            $heartbeatIntervalSeconds
        );
    }

    /**
     * Direct unit test of the wall-clock gate: seeding a stale
     * `lastProgressEmitAt` must fire a heartbeat-flagged tick through
     * safeProgress(); an immediate second call must be a no-op.
     */
    public function test_maybe_emit_heartbeat_fires_once_interval_elapsed_and_resets(): void
    {
        $pipeline = $this->makePipeline(EncryptAndUpload::DEFAULT_CHUNK_BYTES, $this->transport(), 5.0);

        $reflection    = new ReflectionClass(EncryptAndUpload::class);
        $lastEmitProp  = $reflection->getProperty('lastProgressEmitAt');
        $heartbeatMeth = $reflection->getMethod('maybeEmitHeartbeat');

        $calls    = [];
        $progress = function (string $phase, array $detail) use (&$calls): void {
            $calls[] = ['phase' => $phase, 'detail' => $detail];
        };

        $lastEmitProp->setValue($pipeline, microtime(true) - 10.0);

        $heartbeatMeth->invoke($pipeline, $progress, 'encrypting_uploading', ['stage' => 'encrypt', 'chunks_done' => 1]);

        self::assertCount(1, $calls, 'heartbeat must fire once the interval has elapsed');
        self::assertSame('encrypting_uploading', $calls[0]['phase']);
        self::assertTrue($calls[0]['detail']['heartbeat'] ?? false);

        $heartbeatMeth->invoke($pipeline, $progress, 'encrypting_uploading', ['stage' => 'encrypt', 'chunks_done' => 2]);
        self::assertCount(1, $calls, 'a second call inside the interval must not fire again');
    }

    /**
     * encryptChunks() must wire the wall-clock gate into its fread loop. A
     * tiny single-artifact run (well under one 4 MiB default chunk, and
     * under PROGRESS_EVERY_ENCRYPT=4 total chunks even counting the
     * synthetic environment.json artifact encryptChunks() always appends)
     * means the per-N-chunks trigger never fires; any intermediate tick
     * besides the terminal "done" beacon must be heartbeat-driven.
     */
    public function test_encrypt_chunks_emits_wall_clock_heartbeat(): void
    {
        $artifactPath = $this->scratchDir . DIRECTORY_SEPARATOR . 'small.bin';
        file_put_contents($artifactPath, str_repeat('A', 100));

        $pipeline = $this->makePipeline(EncryptAndUpload::DEFAULT_CHUNK_BYTES, $this->transport());

        $calls    = [];
        $progress = function (string $phase, array $detail) use (&$calls): void {
            $calls[] = $detail;
        };

        $cursor = $pipeline->encryptChunks(
            $this->scratchDir,
            [['path' => $artifactPath, 'logical' => 'small.bin']],
            [],
            $progress
        );

        self::assertTrue($cursor['done'] ?? false);
        // 1 chunk for the 100-byte artifact + at most 1 for the synthetic
        // environment.json -- both well under the DEFAULT_CHUNK_BYTES (4 MiB)
        // boundary, so this never reaches PROGRESS_EVERY_ENCRYPT (4).
        self::assertLessThan(4, $cursor['chunks_done']);

        $heartbeats = array_values(array_filter(
            $calls,
            static fn (array $d): bool => ($d['heartbeat'] ?? false) === true
        ));
        self::assertNotEmpty(
            $heartbeats,
            'expected at least one wall-clock heartbeat during the encrypt pass with a near-zero interval'
        );
        foreach ($heartbeats as $hb) {
            self::assertSame('encrypt', $hb['stage'] ?? null);
        }
    }

    /**
     * uploadChunks() (CP/s3_compat path) must wire the same wall-clock gate
     * into its PUT loop. The chunk count from the encrypt pass above stays
     * under PROGRESS_EVERY_UPLOAD (4), so the per-N-chunks trigger never
     * fires on its own here either.
     */
    public function test_upload_chunks_emits_wall_clock_heartbeat(): void
    {
        $artifactPath = $this->scratchDir . DIRECTORY_SEPARATOR . 'small.bin';
        file_put_contents($artifactPath, str_repeat('B', 100));

        $transport = $this->transport();
        $pipeline  = $this->makePipeline(EncryptAndUpload::DEFAULT_CHUNK_BYTES, $transport);

        $noop = static function (): void {
        };
        $encCursor = $pipeline->encryptChunks(
            $this->scratchDir,
            [['path' => $artifactPath, 'logical' => 'small.bin']],
            [],
            $noop
        );
        self::assertTrue($encCursor['done'] ?? false);
        self::assertLessThan(4, count($encCursor['all_hashes']));

        $calls    = [];
        $progress = function (string $phase, array $detail) use (&$calls): void {
            $calls[] = $detail;
        };

        $upCursor = $pipeline->uploadChunks(
            $encCursor,
            ['scratch_dir' => $this->scratchDir],
            $progress
        );
        self::assertTrue($upCursor['done'] ?? false);
        self::assertSame(count($encCursor['all_hashes']), $upCursor['chunks_put']);

        $heartbeats = array_values(array_filter(
            $calls,
            static fn (array $d): bool => ($d['heartbeat'] ?? false) === true
        ));
        self::assertNotEmpty(
            $heartbeats,
            'expected at least one wall-clock heartbeat during the upload pass with a near-zero interval'
        );
        foreach ($heartbeats as $hb) {
            self::assertSame('upload', $hb['stage'] ?? null);
        }
    }

    /**
     * With the default (30s) heartbeat interval, a fast test run over a
     * couple of chunks must emit NO heartbeat ticks. Guards against an
     * inverted elapsed-time comparison.
     */
    public function test_encrypt_chunks_emits_no_heartbeat_within_default_interval(): void
    {
        $artifactPath = $this->scratchDir . DIRECTORY_SEPARATOR . 'small.bin';
        file_put_contents($artifactPath, str_repeat('C', 10));

        // Default heartbeat interval (30s, via EncryptAndUpload::DEFAULT_HEARTBEAT_INTERVAL_SECONDS).
        $pipeline = $this->makePipeline(5, $this->transport(), EncryptAndUpload::DEFAULT_HEARTBEAT_INTERVAL_SECONDS);

        $calls    = [];
        $progress = function (string $phase, array $detail) use (&$calls): void {
            $calls[] = $detail;
        };

        $cursor = $pipeline->encryptChunks(
            $this->scratchDir,
            [['path' => $artifactPath, 'logical' => 'small.bin']],
            [],
            $progress
        );
        self::assertTrue($cursor['done'] ?? false);

        $heartbeats = array_filter($calls, static fn (array $d): bool => ($d['heartbeat'] ?? false) === true);
        self::assertEmpty($heartbeats, 'no heartbeat should fire well within a 30s default interval on a fast test run');
    }

    /**
     * Recursively delete a directory tree (used by tear_down).
     */
    private function rrmdir(string $dir): void
    {
        if (!is_dir($dir)) {
            if (is_file($dir) || is_link($dir)) {
                @unlink($dir);
            }
            return;
        }
        $entries = scandir($dir);
        if ($entries === false) {
            return;
        }
        foreach ($entries as $entry) {
            if ($entry === '.' || $entry === '..') {
                continue;
            }
            $this->rrmdir($dir . DIRECTORY_SEPARATOR . $entry);
        }
        @rmdir($dir);
    }
}
