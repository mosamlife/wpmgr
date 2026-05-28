<?php
/**
 * Tests for the backup command: chunking at the ~4 MiB boundary, blake3-of-
 * CIPHERTEXT chunk addressing, manifest shape matching backup_contract.go,
 * recipient verification, dedup upload, and the invariant that the age PRIVATE
 * key never appears in any control-plane-bound payload.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Commands\BackupCommand;
use WPMgr\Agent\Support\AgeCrypto;
use WPMgr\Agent\Support\AgeIdentity;
use WPMgr\Agent\Support\Blake3;
use WPMgr\Agent\Support\BackupSource;
use WPMgr\Agent\Support\BackupTransport;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\BackupCommand
 */
final class BackupCommandTest extends TestCase
{
    private string $secretSeen = '';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * An AgeIdentity over an in-memory, fixed identity (no WP keystore).
     */
    private function identity(): AgeIdentity
    {
        $age  = new AgeCrypto();
        $pair = $age->generateIdentity();
        $this->secretSeen = $pair['secret'];
        $identityStr      = $pair['identity'];
        $recipient        = $pair['recipient'];
        $secret           = $pair['secret'];

        return new class($recipient, $secret, $identityStr) extends AgeIdentity {
            private string $recipient;
            private string $secret;
            public string $identityStr;
            private AgeCrypto $crypto;

            public function __construct(string $recipient, string $secret, string $identityStr)
            {
                $this->recipient   = $recipient;
                $this->secret      = $secret;
                $this->identityStr = $identityStr;
                $this->crypto      = new AgeCrypto();
            }

            public function ensureRecipient(): string
            {
                return $this->recipient;
            }

            public function recipient(): string
            {
                return $this->recipient;
            }

            public function recipientMatches(string $candidate): bool
            {
                return hash_equals($this->recipient, $candidate);
            }

            public function encryptChunk(string $plaintext): string
            {
                return $this->crypto->encrypt($plaintext, $this->recipient);
            }

            public function decryptChunk(string $ciphertext): string
            {
                return $this->crypto->decrypt($ciphertext, $this->secret);
            }
        };
    }

    /**
     * A BackupSource that serves a fixed set of in-memory files.
     *
     * @param array<string,string> $files rel-path => content.
     */
    private function source(array $files, string $db = ''): BackupSource
    {
        return new class($files, $db) extends BackupSource {
            /** @var array<string,string> */
            private array $files;
            private string $db;

            /** @param array<string,string> $files Files. */
            public function __construct(array $files, string $db)
            {
                $this->files = $files;
                $this->db    = $db;
            }

            public function files(): \Generator
            {
                foreach ($this->files as $rel => $content) {
                    yield [
                        'abs'  => '/virtual/' . $rel,
                        'rel'  => $rel,
                        'mode' => 0644,
                        'size' => strlen($content),
                    ];
                }
            }

            public function readSlice(string $absPath, int $offset, int $length): string
            {
                $rel = substr($absPath, strlen('/virtual/'));

                return substr($this->files[$rel] ?? '', $offset, $length);
            }

            public function exportDatabase(): string
            {
                return $this->db;
            }
        };
    }

    /**
     * A transport that records presign/manifest payloads and PUTs without I/O.
     */
    private function transport(): BackupTransport
    {
        return new class extends BackupTransport {
            /** @var list<string> */
            public array $presignedHashes = [];
            /** @var array<string,string> hash => ciphertext PUT */
            public array $puts = [];
            /** @var array<string,mixed>|null */
            public ?array $manifest = null;
            /** @var bool Whether to claim every hash needs upload. */
            public bool $allNew = true;

            public function __construct()
            {
            }

            public function presignChunks(string $endpoint, string $snapshotId, array $hashes): array
            {
                $this->presignedHashes = array_values($hashes);
                $uploads = [];
                foreach ($hashes as $h) {
                    if ($this->allNew) {
                        $uploads[$h] = 'https://s3.example/put/' . $h;
                    }
                }

                return $uploads;
            }

            public function putChunk(string $presignedUrl, string $ciphertext): bool
            {
                // Key by the hash embedded in the fake URL.
                $hash = substr($presignedUrl, strlen('https://s3.example/put/'));
                $this->puts[$hash] = $ciphertext;

                return true;
            }

            public function submitManifest(string $endpoint, string $snapshotId, string $ageRecipient, array $entries): array
            {
                $this->manifest = [
                    'snapshot_id'   => $snapshotId,
                    'age_recipient' => $ageRecipient,
                    'entries'       => $entries,
                ];

                return ['ok' => true, 'chunk_count' => 1, 'stored_count' => 1];
            }
        };
    }

    /**
     * @param array<string,string> $files Files.
     * @param string                $db    DB dump.
     * @return array{0:BackupCommand,1:object,2:AgeIdentity}
     */
    private function makeCommand(array $files, string $db = ''): array
    {
        $identity  = $this->identity();
        $source    = $this->source($files, $db);
        $transport = $this->transport();
        $cmd       = new BackupCommand($identity, $source, $transport);

        return [$cmd, $transport, $identity];
    }

    /**
     * @return array<string,mixed>
     */
    private function params(string $recipient, string $kind = 'files', int $chunkBytes = 4 << 20): array
    {
        return [
            'snapshot_id'       => 'snap-123',
            'kind'              => $kind,
            'age_recipient'     => $recipient,
            'chunk_bytes'       => $chunkBytes,
            'presign_endpoint'  => 'https://cp.example/agent/v1/backups/snap-123/presign',
            'manifest_endpoint' => 'https://cp.example/agent/v1/backups/snap-123/manifest',
        ];
    }

    public function test_refuses_when_recipient_missing(): void
    {
        [$cmd, , $identity] = $this->makeCommand(['a.txt' => 'x']);
        $params = $this->params($identity->recipient());
        $params['age_recipient'] = '';
        $res = $cmd->execute([], $params);
        $this->assertFalse($res['ok']);
        $this->assertSame('missing age recipient', $res['detail']);
    }

    public function test_refuses_when_recipient_mismatch(): void
    {
        [$cmd] = $this->makeCommand(['a.txt' => 'x']);
        $res = $cmd->execute([], $this->params('age1someoneelsesrecipientvaluethatwontmatchxxxxxxxxxxxxxxxxx'));
        $this->assertFalse($res['ok']);
        $this->assertSame('age recipient mismatch', $res['detail']);
    }

    public function test_chunking_splits_at_boundary(): void
    {
        // chunk_bytes = 10; a 25-byte file => 3 plaintext chunks (10,10,5).
        $content = str_repeat('A', 25);
        [$cmd, $transport, $identity] = $this->makeCommand(['big.bin' => $content]);

        $res = $cmd->execute([], $this->params($identity->recipient(), 'files', 10));
        $this->assertTrue($res['ok']);

        $entries = $transport->manifest['entries'];
        $this->assertCount(1, $entries);
        $entry = $entries[0];
        $this->assertCount(3, $entry['chunks'], 'expected 25 bytes / 10 = 3 chunks');
        $this->assertSame('big.bin', $entry['path']);
        $this->assertSame('file', $entry['entry_kind']);
        $this->assertSame(25, $entry['size']);
    }

    public function test_chunk_id_is_blake3_of_ciphertext(): void
    {
        $content = 'hello world';
        [$cmd, $transport, $identity] = $this->makeCommand(['f.txt' => $content]);

        $res = $cmd->execute([], $this->params($identity->recipient(), 'files', 4 << 20));
        $this->assertTrue($res['ok']);

        $entry = $transport->manifest['entries'][0];
        $chunk = $entry['chunks'][0];

        // The uploaded ciphertext for this hash must hash (blake3) to the chunk id.
        $this->assertArrayHasKey($chunk['blake3'], $transport->puts);
        $ciphertext = $transport->puts[$chunk['blake3']];
        $this->assertSame($chunk['blake3'], Blake3::hashHex($ciphertext));
        $this->assertSame(strlen($ciphertext), $chunk['size'], 'chunk size is the CIPHERTEXT length');
    }

    public function test_manifest_shape_matches_contract(): void
    {
        [$cmd, $transport, $identity] = $this->makeCommand(['x.txt' => 'data'], 'CREATE TABLE t;');

        $res = $cmd->execute([], $this->params($identity->recipient(), 'full', 4 << 20));
        $this->assertTrue($res['ok']);

        $m = $transport->manifest;
        $this->assertSame('snap-123', $m['snapshot_id']);
        $this->assertSame($identity->recipient(), $m['age_recipient']);

        // Entry keys exactly match ManifestEntry JSON fields (no extra _cipher).
        foreach ($m['entries'] as $entry) {
            $this->assertSame(
                ['path', 'entry_kind', 'table_name', 'mode', 'size', 'chunks'],
                array_keys($entry)
            );
            $this->assertArrayNotHasKey('_cipher', $entry);
            foreach ($entry['chunks'] as $chunk) {
                $this->assertSame(['blake3', 'size'], array_keys($chunk));
            }
        }

        // A "full" backup includes a db entry with the contract's logical path.
        $kinds = array_map(static fn ($e) => $e['entry_kind'], $m['entries']);
        $this->assertContains('db', $kinds);
        $dbEntry = array_values(array_filter($m['entries'], static fn ($e) => $e['entry_kind'] === 'db'))[0];
        $this->assertSame('database.sql', $dbEntry['path']);
    }

    public function test_private_key_never_in_cp_payloads(): void
    {
        /** @var object{manifest:array<string,mixed>|null,presignedHashes:list<string>,puts:array<string,string>} $transport */
        [$cmd, $transport, $identity] = $this->makeCommand(['x.txt' => 'secret-data'], 'INSERT 1;');
        $res = $cmd->execute([], $this->params($identity->recipient(), 'full', 8));
        $this->assertTrue($res['ok']);

        // Serialize EVERYTHING the command handed to the transport for the CP.
        $cpBound = json_encode([
            'manifest'  => $transport->manifest,
            'presigned' => $transport->presignedHashes,
        ]);

        // The raw secret scalar (hex + base64) and the AGE-SECRET-KEY string must
        // never appear in any CP-bound payload.
        /** @var AgeIdentity&object{identityStr:string} $identity */
        $this->assertStringNotContainsString($identity->identityStr, (string) $cpBound);
        $this->assertStringNotContainsString(bin2hex($this->secretSeen), (string) $cpBound);
        $this->assertStringNotContainsString(base64_encode($this->secretSeen), (string) $cpBound);
        $this->assertStringNotContainsString('AGE-SECRET-KEY-', (string) $cpBound);
    }

    public function test_dedup_skips_already_stored_chunks(): void
    {
        [$cmd, $transport, $identity] = $this->makeCommand(['a.txt' => 'dup', 'b.txt' => 'dup']);
        // Identical content -> identical plaintext, but age uses a random file
        // key per call, so ciphertext (and hash) differ; assert presign got the
        // union of hashes and the CP-driven dedup path is exercised by allNew.
        $transport->allNew = false; // CP says nothing needs upload.
        $res = $cmd->execute([], $this->params($identity->recipient(), 'files', 4 << 20));
        $this->assertTrue($res['ok']);
        $this->assertSame([], $transport->puts, 'no uploads when CP reports all chunks stored');
        $this->assertNotEmpty($transport->presignedHashes);
    }
}
