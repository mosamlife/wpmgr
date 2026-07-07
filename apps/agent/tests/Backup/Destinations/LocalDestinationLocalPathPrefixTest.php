<?php
/**
 * Tests for the ADR-036 P1 backup-destinations completeness fix (agent half,
 * Phase 1): the `destination_config.local_path_prefix` wiring from
 * DestinationResolver into LocalDestination, LocalDestination's
 * traversal-safe sanitization of that operator-configured path, its
 * default-fallback behaviour when the prefix is absent/empty/unsanitizable,
 * and the hash-format guard that protects putChunk/getChunk/deleteChunks
 * from a malformed (wire-supplied, therefore untrusted) hash.
 *
 * cp/s3_compat regression: DestinationResolver::resolve() must still route
 * those kinds (and an absent/empty destination_kind, matching older CP
 * builds) to CpDestination, completely unaffected by the local_path_prefix
 * wiring — even when a destination_config happens to be present.
 *
 * All filesystem operations happen under a unique, per-test marker directory
 * inside WP_CONTENT_DIR (never the whole content dir is touched or removed),
 * so this file is independent of ambient WP_CONTENT_DIR state other test
 * files in this suite may have already frozen as a global PHP constant
 * (constants cannot be redefined once set) — see BackupJanitorTest /
 * SnapshotManagerTest for the same documented landmine and mitigation.
 *
 * @package WPMgr\Agent\Tests\Backup\Destinations
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Backup\Destinations;

use Brain\Monkey;
use WPMgr\Agent\Backup\Destinations\CpDestination;
use WPMgr\Agent\Backup\Destinations\DestinationResolver;
use WPMgr\Agent\Backup\Destinations\LocalDestination;
use WPMgr\Agent\Support\BackupTransport;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\Destinations\DestinationResolver
 * @covers \WPMgr\Agent\Backup\Destinations\LocalDestination
 */
final class LocalDestinationLocalPathPrefixTest extends TestCase
{
    /** Unique per-test subdir name under WP_CONTENT_DIR (own cleanup only). */
    private string $marker = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();

        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', sys_get_temp_dir() . '/wpmgr-shared-wp-content');
        }
        if (!is_dir(WP_CONTENT_DIR)) {
            mkdir(WP_CONTENT_DIR, 0755, true);
        }

        $this->marker = 'wpmgr-ldpt-' . bin2hex(random_bytes(6));
    }

    protected function tear_down(): void
    {
        // Only ever clean up subtrees THIS test created — never the shared
        // WP_CONTENT_DIR itself, which other tests in this process may
        // still rely on.
        $this->rrmdir(WP_CONTENT_DIR . DIRECTORY_SEPARATOR . $this->marker);
        Monkey\tearDown();
        parent::tear_down();
    }

    // ==========================================================
    // DestinationResolver routing
    // ==========================================================

    public function test_resolver_routes_local_kind_with_prefix_to_local_destination(): void
    {
        $snapshotId  = $this->uuid();
        $destination = DestinationResolver::resolve([
            'snapshot_id'        => $snapshotId,
            'destination_kind'   => 'local',
            'destination_config' => ['local_path_prefix' => $this->marker . '/custom-backups'],
            'manifest_endpoint'  => '',
            'presign_endpoint'   => '',
        ], $this->stubTransport());

        $this->assertInstanceOf(LocalDestination::class, $destination);
        $this->assertSame('local', $destination->getKind());

        $destination->prepare($snapshotId);
        $hash = str_repeat('a', 64);
        $this->assertTrue($destination->putChunk($hash, 'ciphertext-bytes'));

        $expectedDir = WP_CONTENT_DIR . DIRECTORY_SEPARATOR . $this->marker . DIRECTORY_SEPARATOR . 'custom-backups';
        $this->assertDirectoryExists($expectedDir);
        $this->assertFileExists(
            $expectedDir . DIRECTORY_SEPARATOR . $snapshotId . DIRECTORY_SEPARATOR . 'chunks' . DIRECTORY_SEPARATOR . $hash . '.bin'
        );
    }

    /**
     * @dataProvider cpLikeKindProvider
     */
    public function test_resolver_routes_cp_like_kinds_to_cp_destination(string $kindParam, string $expectedKind): void
    {
        $params = [
            'snapshot_id'       => $this->uuid(),
            'manifest_endpoint' => 'https://cp.test/manifest',
            'presign_endpoint'  => 'https://cp.test/presign',
        ];
        if ($kindParam !== '') {
            $params['destination_kind'] = $kindParam;
        }
        // Even when a destination_config happens to be present, cp/s3_compat
        // must ignore it entirely — it is only meaningful for 'local'.
        $params['destination_config'] = ['local_path_prefix' => 'should-be-ignored'];

        $destination = DestinationResolver::resolve($params, $this->stubTransport());

        $this->assertInstanceOf(CpDestination::class, $destination);
        $this->assertSame($expectedKind, $destination->getKind());
    }

    /**
     * @return array<string,array{0:string,1:string}>
     */
    public static function cpLikeKindProvider(): array
    {
        return [
            'absent (older CP builds)' => ['', 'cp'],
            'cp'                       => ['cp', 'cp'],
            's3_compat'                => ['s3_compat', 's3_compat'],
        ];
    }

    // ==========================================================
    // local_path_prefix sanitization (contract shape + traversal defense)
    // ==========================================================

    public function test_prefix_traversal_segments_are_stripped_not_escaped(): void
    {
        $destination = new LocalDestination(
            $this->stubTransport(),
            $this->uuid(),
            '',
            '../../../../' . $this->marker . '/escape-attempt'
        );

        $snapshotId = $this->uuid();
        $destination->prepare($snapshotId);
        $hash = str_repeat('b', 64);
        $this->assertTrue($destination->putChunk($hash, 'x'));

        // The '..' segments are dropped outright (never resolved against the
        // filesystem) — the chunk must land INSIDE
        // WP_CONTENT_DIR/<marker>/escape-attempt, never above WP_CONTENT_DIR.
        $expectedDir = WP_CONTENT_DIR . DIRECTORY_SEPARATOR . $this->marker . DIRECTORY_SEPARATOR . 'escape-attempt'
            . DIRECTORY_SEPARATOR . $snapshotId . DIRECTORY_SEPARATOR . 'chunks';
        $this->assertFileExists($expectedDir . DIRECTORY_SEPARATOR . $hash . '.bin');
    }

    public function test_prefix_of_only_dotdot_segments_falls_back_to_default(): void
    {
        // A prefix that sanitizes down to nothing must not throw or resolve
        // an empty/undefined path — it must fall back to the agent's own
        // default (uploads-first / wp-content), exactly like an absent prefix.
        $destination = new LocalDestination($this->stubTransport(), $this->uuid(), '', '../..');
        $snapshotId  = $this->uuid();

        $destination->prepare($snapshotId);
        $hash = str_repeat('c', 64);
        $this->assertTrue($destination->putChunk($hash, 'y'));

        // wp_upload_dir() stub returns [] in this suite, so
        // StoragePaths::dataBase() falls through to the WP_CONTENT_DIR
        // candidate — the same default a truly-absent prefix would use.
        $expected = WP_CONTENT_DIR . DIRECTORY_SEPARATOR . 'wpmgr-backups'
            . DIRECTORY_SEPARATOR . $snapshotId . DIRECTORY_SEPARATOR . 'chunks'
            . DIRECTORY_SEPARATOR . $hash . '.bin';
        $this->assertFileExists($expected);

        // This one lands under the SHARED default dir (not this test's
        // marker) — clean up only this test's own snapshot subdir.
        $this->rrmdir(WP_CONTENT_DIR . DIRECTORY_SEPARATOR . 'wpmgr-backups' . DIRECTORY_SEPARATOR . $snapshotId);
    }

    public function test_prefix_with_nul_byte_is_stripped(): void
    {
        $destination = new LocalDestination(
            $this->stubTransport(),
            $this->uuid(),
            '',
            $this->marker . "/nul\x00-attempt"
        );
        $snapshotId = $this->uuid();
        $destination->prepare($snapshotId);
        $hash = str_repeat('d', 64);
        $this->assertTrue($destination->putChunk($hash, 'z'));

        $expectedDir = WP_CONTENT_DIR . DIRECTORY_SEPARATOR . $this->marker . DIRECTORY_SEPARATOR . 'nul-attempt';
        $this->assertDirectoryExists($expectedDir);
    }

    public function test_resolver_ignores_non_string_local_path_prefix(): void
    {
        // A malformed destination_config (e.g. local_path_prefix sent as an
        // int) must not crash the resolver — it degrades to the default.
        $snapshotId  = $this->uuid();
        $destination = DestinationResolver::resolve([
            'snapshot_id'        => $snapshotId,
            'destination_kind'   => 'local',
            'destination_config' => ['local_path_prefix' => 12345],
            'manifest_endpoint'  => '',
            'presign_endpoint'   => '',
        ], $this->stubTransport());

        $this->assertInstanceOf(LocalDestination::class, $destination);
        // No exception preparing/using it with the default path.
        $destination->prepare($snapshotId);
        $this->rrmdir(WP_CONTENT_DIR . DIRECTORY_SEPARATOR . 'wpmgr-backups' . DIRECTORY_SEPARATOR . $snapshotId);
        $this->addToAssertionCount(1);
    }

    // ==========================================================
    // Local BACKUP: full prepare -> putChunk -> submitManifest under the
    // configured path (files land + manifest, per the task's test list)
    // ==========================================================

    public function test_local_backup_writes_ciphertext_chunks_and_manifest_under_configured_path(): void
    {
        $snapshotId  = $this->uuid();
        $destination = new LocalDestination(
            $this->stubTransport(),
            $snapshotId,
            'https://cp.test/manifest',
            $this->marker . '/manifest-check'
        );

        $destination->prepare($snapshotId);

        $hashes = [str_repeat('1', 64), str_repeat('2', 64)];
        foreach ($hashes as $i => $hash) {
            $this->assertTrue($destination->putChunk($hash, 'chunk-bytes-' . $i));
        }

        $entries = [[
            'path'       => 'database.sql.gz',
            'entry_kind' => 'db',
            'table_name' => '',
            'mode'       => 0,
            'size'       => 24,
            'chunks'     => [
                ['blake3' => $hashes[0], 'size' => 13],
                ['blake3' => $hashes[1], 'size' => 13],
            ],
        ]];

        $result = $destination->submitManifest($entries, ['age_recipient' => '']);
        $this->assertTrue($result['ok']);

        $baseDir = WP_CONTENT_DIR . DIRECTORY_SEPARATOR . $this->marker . DIRECTORY_SEPARATOR . 'manifest-check'
            . DIRECTORY_SEPARATOR . $snapshotId;

        foreach ($hashes as $hash) {
            $this->assertFileExists($baseDir . DIRECTORY_SEPARATOR . 'chunks' . DIRECTORY_SEPARATOR . $hash . '.bin');
        }
        $manifestPath = $baseDir . DIRECTORY_SEPARATOR . 'manifest.json';
        $this->assertFileExists($manifestPath);
        $decoded = json_decode((string) file_get_contents($manifestPath), true);
        $this->assertSame($snapshotId, $decoded['snapshot_id']);
        $this->assertCount(1, $decoded['entries']);
    }

    // ==========================================================
    // Hash-format guard (path-traversal-via-hash defense)
    // ==========================================================

    public function test_get_chunk_rejects_malformed_hash(): void
    {
        $snapshotId  = $this->uuid();
        $destination = new LocalDestination($this->stubTransport(), $snapshotId, '', $this->marker . '/hashguard');
        $destination->prepare($snapshotId);

        $this->assertNull($destination->getChunk('../../../../etc/passwd'));
        $this->assertNull($destination->getChunk(str_repeat('g', 64))); // 'g' is not hex
        $this->assertNull($destination->getChunk('tooshort'));
    }

    public function test_put_chunk_rejects_malformed_hash(): void
    {
        $snapshotId  = $this->uuid();
        $destination = new LocalDestination($this->stubTransport(), $snapshotId, '', $this->marker . '/hashguard2');
        $destination->prepare($snapshotId);

        $this->assertFalse($destination->putChunk('../../../../etc/passwd', 'x'));
    }

    public function test_delete_chunks_skips_malformed_hash_entries(): void
    {
        $snapshotId  = $this->uuid();
        $destination = new LocalDestination($this->stubTransport(), $snapshotId, '', $this->marker . '/hashguard3');
        $destination->prepare($snapshotId);

        $validHash = str_repeat('e', 64);
        $destination->putChunk($validHash, 'x');

        // Must not throw or attempt to delete a path built from a malformed hash.
        $destination->deleteChunks(['../../../../etc/passwd', $validHash]);

        $chunkPath = WP_CONTENT_DIR . DIRECTORY_SEPARATOR . $this->marker . DIRECTORY_SEPARATOR . 'hashguard3'
            . DIRECTORY_SEPARATOR . $snapshotId . DIRECTORY_SEPARATOR . 'chunks' . DIRECTORY_SEPARATOR . $validHash . '.bin';
        $this->assertFileDoesNotExist($chunkPath, 'the VALID hash must still be deleted');
    }

    // ==========================================================
    // Round-trip: what a backup writes, a restore reads back by hash
    // ==========================================================

    public function test_get_chunk_reads_back_exactly_what_put_chunk_wrote(): void
    {
        $snapshotId  = $this->uuid();
        $destination = new LocalDestination($this->stubTransport(), $snapshotId, '', $this->marker . '/roundtrip');
        $destination->prepare($snapshotId);

        $hash  = hash('sha256', 'irrelevant-for-shape-only-test'); // 64 lowercase hex chars
        $bytes = random_bytes(4096);
        $this->assertTrue($destination->putChunk($hash, $bytes));

        $this->assertSame($bytes, $destination->getChunk($hash));
        $this->assertNull($destination->getChunk(str_repeat('0', 64)), 'a hash that was never written must read back null');
    }

    // ==========================================================
    // Helpers
    // ==========================================================

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

    private function stubTransport(): BackupTransport
    {
        return new class extends BackupTransport {
            public function __construct()
            {
                // Intentionally skip parent::__construct — no real Signer is
                // needed; this stub's submitManifest() never touches it, and
                // getKind()/prepare()/putChunk()/getChunk() on LocalDestination
                // never call into the transport at all.
            }

            public function submitManifest(string $endpoint, string $snapshotId, string $ageRecipient, array $entries): array
            {
                return ['ok' => true, 'chunk_count' => count($entries), 'stored_count' => count($entries)];
            }
        };
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
