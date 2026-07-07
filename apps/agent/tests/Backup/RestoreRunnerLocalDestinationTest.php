<?php
/**
 * ADR-036 P1 backup-destinations completeness fix (agent half, Phase 1) —
 * the NEW gap this phase closes: a `destination_kind=local` RestoreRequest's
 * chunks live on this same webserver's disk (no presigned_url in the
 * manifest — see class-restore-command.php) and must be read by content
 * hash instead of an HTTP GET.
 *
 * Covers:
 *   - destinationKind() defaults to 'cp' when absent/empty, reads 'local'
 *     when set (mirrors DestinationResolver's own default).
 *   - fetchLocalChunk() reads back exactly the bytes a matching backup
 *     wrote via LocalDestination::putChunk() — no HTTP involved.
 *   - fetchLocalChunk() raises a structured, operator-grep-able error when
 *     the chunk is missing on disk.
 *   - runDownloadArtifacts() end-to-end for a destination_kind=local restore:
 *     reads two chunks straight off local disk (real Blake3 verify, real
 *     fwrite reassembly), never constructs the network transport, and the
 *     reconstructed scratch-dir artifact byte-for-byte matches what a
 *     matching backup would have produced.
 *   - Regression: destination_kind='cp' (or absent) still REQUIRES a
 *     presigned_url per chunk — a local-shaped (URL-less) chunk enter that
 *     path and must fail fast, proving the branch decision itself (not just
 *     the local path) is correct.
 *
 * We reach into RestoreRunner's private methods via reflection — same
 * technique RestoreRunnerRetryTest uses for fetchChunkWithRetries() — so
 * these tests need no live WordPress DB, progress client, or WP_CONTENT_DIR
 * ambient state beyond a per-test scratch dir.
 *
 * @package WPMgr\Agent\Tests\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup;

use Brain\Monkey;
use ReflectionClass;
use WPMgr\Agent\Backup\Destinations\LocalDestination;
use WPMgr\Agent\Backup\RestoreRunner;
use WPMgr\Agent\Support\BackupTransport;
use WPMgr\Agent\Support\Blake3;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\RestoreRunner
 */
final class RestoreRunnerLocalDestinationTest extends TestCase
{
    /** Per-test scratch dir (removed in tear_down). */
    private string $scratchDir = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        // Guard-define: WP_CONTENT_DIR may already be a frozen global PHP
        // constant from another test file in this suite (constants cannot be
        // redefined once set) — see BackupJanitorTest / SnapshotManagerTest
        // for the same documented landmine. Every path this file resolves
        // under WP_CONTENT_DIR uses a fresh, unique per-test prefix, so
        // whichever directory the constant ends up pointing at is fine.
        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', sys_get_temp_dir() . '/wpmgr-shared-wp-content');
        }
        if (!is_dir(WP_CONTENT_DIR)) {
            mkdir(WP_CONTENT_DIR, 0755, true);
        }

        $this->scratchDir = sys_get_temp_dir() . '/wpmgr-rrld-scratch-' . bin2hex(random_bytes(6));
        mkdir($this->scratchDir, 0700, true);
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->scratchDir);
        Monkey\tearDown();
        parent::tear_down();
    }

    // ==========================================================
    // destinationKind()
    // ==========================================================

    public function test_destination_kind_defaults_to_cp_when_absent(): void
    {
        $runner = $this->makeRunner([]);
        $this->assertSame('cp', $this->callPrivate($runner, 'destinationKind'));
    }

    public function test_destination_kind_defaults_to_cp_when_empty_string(): void
    {
        $runner = $this->makeRunner(['destination_kind' => '']);
        $this->assertSame('cp', $this->callPrivate($runner, 'destinationKind'));
    }

    public function test_destination_kind_reads_local(): void
    {
        $runner = $this->makeRunner(['destination_kind' => 'local']);
        $this->assertSame('local', $this->callPrivate($runner, 'destinationKind'));
    }

    public function test_destination_kind_reads_s3_compat(): void
    {
        $runner = $this->makeRunner(['destination_kind' => 's3_compat']);
        $this->assertSame('s3_compat', $this->callPrivate($runner, 'destinationKind'));
    }

    // ==========================================================
    // fetchLocalChunk()
    // ==========================================================

    public function test_fetch_local_chunk_reads_bytes_a_backup_wrote(): void
    {
        $snapshotId = $this->uuid();
        $prefix     = 'wpmgr-rrld-' . bin2hex(random_bytes(6));
        $destination = new LocalDestination($this->stubTransport(), $snapshotId, '', $prefix);
        $destination->prepare($snapshotId);

        $bytes = random_bytes(2048);
        $hash  = Blake3::hashHex($bytes);
        $this->assertTrue($destination->putChunk($hash, $bytes));

        $runner = $this->makeRunner(['destination_kind' => 'local']);
        $result = $this->callPrivate($runner, 'fetchLocalChunk', [$destination, $hash, 'database.sql.gz', 0]);

        $this->assertSame($bytes, $result);

        $this->rrmdir(WP_CONTENT_DIR . DIRECTORY_SEPARATOR . $prefix);
    }

    public function test_fetch_local_chunk_throws_structured_error_when_missing(): void
    {
        $snapshotId  = $this->uuid();
        $prefix      = 'wpmgr-rrld-missing-' . bin2hex(random_bytes(6));
        $destination = new LocalDestination($this->stubTransport(), $snapshotId, '', $prefix);
        $destination->prepare($snapshotId);

        $runner   = $this->makeRunner(['destination_kind' => 'local']);
        $hash     = str_repeat('f', 64);

        try {
            $this->callPrivate($runner, 'fetchLocalChunk', [$destination, $hash, 'wp-content.part001.zip', 3]);
            $this->fail('expected RuntimeException for a missing local chunk');
        } catch (\RuntimeException $e) {
            $this->assertStringContainsString('wp-content.part001.zip', $e->getMessage());
            $this->assertStringContainsString('not found on local disk', $e->getMessage());
        }

        $this->rrmdir(WP_CONTENT_DIR . DIRECTORY_SEPARATOR . $prefix);
    }

    // ==========================================================
    // runDownloadArtifacts() end-to-end for destination_kind=local — the
    // "prove the local-restore path non-vacuous" test.
    // ==========================================================

    public function test_run_download_artifacts_reads_local_chunks_and_reassembles_without_network(): void
    {
        $snapshotId = $this->uuid();
        $prefix     = 'wpmgr-rrld-e2e-' . bin2hex(random_bytes(6));

        // Seed local disk exactly as a matching local-destination BACKUP
        // would have: two content-addressed chunk files under
        // <prefix>/<snapshotId>/chunks/<hash>.bin.
        $seed = new LocalDestination($this->stubTransport(), $snapshotId, '', $prefix);
        $seed->prepare($snapshotId);

        $part1 = random_bytes(1024);
        $part2 = random_bytes(777);
        $hash1 = Blake3::hashHex($part1);
        $hash2 = Blake3::hashHex($part2);
        $this->assertTrue($seed->putChunk($hash1, $part1));
        $this->assertTrue($seed->putChunk($hash2, $part2));

        // Deliberately NO presigned_url on either chunk — a 'local' snapshot's
        // manifest never carries one (see class-restore-command.php). If the
        // runner's branch logic mistakenly fell through to the cp/s3 path,
        // this would throw "chunk missing url" instead of reassembling.
        $runner = $this->makeRunner([
            'destination_kind'   => 'local',
            'destination_config' => ['local_path_prefix' => $prefix],
            'snapshot_id'        => $snapshotId,
            'chunk_downloads'    => [
                [
                    'logical_path' => 'database.sql.gz',
                    'chunks'       => [
                        ['hash' => $hash1, 'size' => strlen($part1)],
                        ['hash' => $hash2, 'size' => strlen($part2)],
                    ],
                ],
            ],
        ]);

        $subState = $this->callPrivate($runner, 'runDownloadArtifacts', [[]]);

        $this->assertTrue($subState['download']['done']);
        $this->assertSame(strlen($part1) + strlen($part2), $subState['download']['bytes_downloaded']);

        $artifactPath = $this->scratchDir . DIRECTORY_SEPARATOR . 'database.sql.gz';
        $this->assertFileExists($artifactPath);
        $this->assertSame($part1 . $part2, (string) file_get_contents($artifactPath));

        $this->rrmdir(WP_CONTENT_DIR . DIRECTORY_SEPARATOR . $prefix);
    }

    // ==========================================================
    // Regression: non-local destination_kind still requires presigned_url
    // (proves the branch decision, not just the local path, is correct)
    // ==========================================================

    public function test_run_download_artifacts_cp_destination_kind_requires_presigned_url(): void
    {
        $runner = $this->makeRunner([
            'destination_kind' => 'cp',
            'chunk_downloads'  => [
                [
                    'logical_path' => 'database.sql.gz',
                    'chunks'       => [
                        ['hash' => str_repeat('a', 64), 'size' => 10],
                    ],
                ],
            ],
        ]);

        $this->expectException(\RuntimeException::class);
        $this->expectExceptionMessageMatches('/chunk missing url/');
        $this->callPrivate($runner, 'runDownloadArtifacts', [[]]);
    }

    public function test_run_download_artifacts_absent_destination_kind_requires_presigned_url(): void
    {
        $runner = $this->makeRunner([
            // destination_kind omitted entirely — must default to 'cp'.
            'chunk_downloads' => [
                [
                    'logical_path' => 'database.sql.gz',
                    'chunks'       => [
                        ['hash' => str_repeat('b', 64), 'size' => 10],
                    ],
                ],
            ],
        ]);

        $this->expectException(\RuntimeException::class);
        $this->expectExceptionMessageMatches('/chunk missing url/');
        $this->callPrivate($runner, 'runDownloadArtifacts', [[]]);
    }

    // ==========================================================
    // Helpers
    // ==========================================================

    /**
     * @param array<string,mixed> $extraParams
     */
    private function makeRunner(array $extraParams): RestoreRunner
    {
        $defaults = [
            'snapshot_id'       => $this->uuid(),
            'restore_id'        => $this->uuid(),
            'kind'              => 'full',
            'progress_endpoint' => '',
            'chunk_downloads'   => [],
            'scratch_dir'       => $this->scratchDir,
            'wp_content_path'   => $this->scratchDir,
            'wp_root'           => sys_get_temp_dir(),
            'db'                => ['host' => '', 'user' => '', 'password' => '', 'name' => '', 'prefix' => 'wp_'],
        ];
        return new RestoreRunner(array_merge($defaults, $extraParams));
    }

    /**
     * Invoke a private RestoreRunner method via reflection. setAccessible()
     * is not called — a no-op since PHP 8.1 and deprecated since PHP 8.5;
     * private methods are directly accessible via ReflectionMethod::invoke()
     * on PHP 8.1+.
     *
     * @param list<mixed> $args
     */
    private function callPrivate(RestoreRunner $runner, string $method, array $args = []): mixed
    {
        $m = (new ReflectionClass($runner))->getMethod($method);
        return $m->invokeArgs($runner, $args);
    }

    private function stubTransport(): BackupTransport
    {
        return new class extends BackupTransport {
            public function __construct()
            {
                // Intentionally skip parent::__construct — LocalDestination
                // never calls into the transport for prepare()/putChunk()/
                // getChunk(), only for the (backup-only) submitManifest().
            }
        };
    }

    private function uuid(): string
    {
        return sprintf(
            '%08x-%04x-%04x-%04x-%012x',
            random_int(0, 0xffffffff),
            random_int(0, 0xffff),
            random_int(0, 0xffff),
            random_int(0, 0xffff),
            random_int(0, 0xffffffffffff)
        );
    }

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
