<?php
/**
 * Tests for the backup encrypt-and-upload pipeline: chunking at the ~4 MiB
 * boundary, content-addressing chunks by the hash of the UPLOADED bytes,
 * manifest shape matching backup_contract.go, CP-driven dedup upload, and the
 * invariant that the age PRIVATE key never appears in any control-plane-bound
 * payload.
 *
 * Reconciliation note (why this no longer drives BackupCommand directly):
 * Through M5.6 / ADR-033 the backup flow was refactored from an in-process
 * `BackupCommand` orchestrator into a cron-dispatched state machine. The
 * `BackupCommand` class is now a thin REST acceptor (validate → claim dedup →
 * seed task row → spawn cron) and does NO chunking/manifest work itself; that
 * moved into the backup pipeline (`EncryptAndUpload`, driven by `TaskRunner`).
 * The chunk-addressing + manifest-shape + no-secret-leak invariants these tests
 * guard are real and still implemented — they now live in `EncryptAndUpload`,
 * so the tests exercise that class against the same `BackupSource` /
 * `BackupTransport` seams the original suite used.
 *
 * V0 encryption note (ADR-033): `EncryptAndUpload::ENCRYPT_CHUNKS` is false in
 * the self-hosted V0 build — chunks are hashed and uploaded as PLAINTEXT (the
 * operator IS the customer, so encrypting against ourselves buys nothing; this
 * matches the design of leading backup plugins). The chunk id is therefore the blake3 of the
 * uploaded bytes (plaintext in V0), and `chunk.size` is the uploaded byte
 * length. The "private key never leaks" invariant is unchanged either way.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Backup\EncryptAndUpload;
use WPMgr\Agent\Commands\BackupCommand;
use WPMgr\Agent\Support\AgeCrypto;
use WPMgr\Agent\Support\AgeIdentity;
use WPMgr\Agent\Support\Blake3;
use WPMgr\Agent\Support\BackupTransport;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Backup\EncryptAndUpload
 */
final class BackupCommandTest extends TestCase
{
    private string $scratchDir = '';

    /** @var array{secret:string,recipient:string,identity:string} */
    private array $identity = ['secret' => '', 'recipient' => '', 'identity' => ''];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('wp_json_encode')->alias(static fn ($d) => json_encode($d));

        $age            = new AgeCrypto();
        $this->identity = $age->generateIdentity();

        $this->scratchDir = sys_get_temp_dir() . '/wpmgr-backup-test-' . bin2hex(random_bytes(6));
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
     * A transport stub that records presign/manifest payloads and PUTs without
     * doing any real network I/O. Matches the BackupTransport public surface
     * that EncryptAndUpload (via CpDestination) calls.
     */
    private function transport(): BackupTransport
    {
        return new class extends BackupTransport {
            /** @var list<string> */
            public array $presignedHashes = [];
            /** @var array<string,string> hash => uploaded bytes PUT */
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

            public function putChunkWithStatus(string $presignedUrl, string $ciphertext): array
            {
                // Key by the hash embedded in the fake URL.
                $hash = substr($presignedUrl, strlen('https://s3.example/put/'));
                $this->puts[$hash] = $ciphertext;

                return ['ok' => true, 'status' => 200, 'error' => '', 's3_code' => '', 'host' => 's3.example', 'retryable' => false, 'represign' => false];
            }

            public function submitManifest(string $endpoint, string $snapshotId, string $ageRecipient, array $entries): array
            {
                $this->manifest = [
                    'snapshot_id'   => $snapshotId,
                    'age_recipient' => $ageRecipient,
                    'entries'       => $entries,
                ];

                return ['ok' => true, 'chunk_count' => count($entries), 'stored_count' => count($entries)];
            }
        };
    }

    /**
     * Write a set of artifact files into the scratch dir and return the
     * EncryptAndUpload artifact list (`{path, logical}` tuples, in order).
     *
     * @param array<string,string> $files logical-name => content.
     * @return list<array{path:string,logical:string}>
     */
    private function artifacts(array $files): array
    {
        $artifacts = [];
        foreach ($files as $logical => $content) {
            $abs = $this->scratchDir . DIRECTORY_SEPARATOR . $logical;
            file_put_contents($abs, $content);
            $artifacts[] = ['path' => $abs, 'logical' => $logical];
        }

        return $artifacts;
    }

    /**
     * Build an EncryptAndUpload bound to the test transport + identity.
     *
     * @return array{0:EncryptAndUpload,1:object}
     */
    private function makePipeline(int $chunkBytes = EncryptAndUpload::DEFAULT_CHUNK_BYTES): array
    {
        $transport = $this->transport();
        $pipeline  = new EncryptAndUpload(
            new AgeCrypto(),
            $transport,
            'snap-123',
            $this->identity['recipient'],
            'https://cp.example/agent/v1/backups/snap-123/presign',
            'https://cp.example/agent/v1/backups/snap-123/manifest',
            $chunkBytes
        );

        return [$pipeline, $transport];
    }

    /** A progress callback that records nothing (the pipeline calls it freely). */
    private function noopProgress(): callable
    {
        return static function (string $phase, array $detail): void {
        };
    }

    /**
     * Drive the full pipeline (encrypt → upload → manifest) and return the
     * transport so the test can inspect what hit the CP.
     *
     * @return object
     */
    private function drive(EncryptAndUpload $pipeline, array $artifacts, object $transport)
    {
        $progress = $this->noopProgress();
        $cursor   = $pipeline->encryptChunks($this->scratchDir, $artifacts, [], $progress);
        $this->assertTrue($cursor['done'] ?? false, 'encryptChunks did not complete');
        $pipeline->uploadChunks($cursor + ['scratch_dir' => $this->scratchDir], ['scratch_dir' => $this->scratchDir], $progress);
        /** @var list<array<string,mixed>> $entries */
        $entries = $cursor['entries'];
        $pipeline->submitManifest($entries, $progress);

        return $transport;
    }

    /**
     * Find the manifest entry for a given logical path.
     *
     * @param array<string,mixed> $manifest
     * @return array<string,mixed>
     */
    private function entryFor(array $manifest, string $path): array
    {
        foreach (($manifest['entries'] ?? []) as $entry) {
            if (($entry['path'] ?? null) === $path) {
                return $entry;
            }
        }
        self::fail("no manifest entry for path: {$path}");
    }

    public function test_chunking_splits_at_boundary(): void
    {
        // chunk_bytes = 10; a 25-byte file => 3 chunks (10, 10, 5).
        $content = str_repeat('A', 25);
        [$pipeline, $transport] = $this->makePipeline(10);

        $transport = $this->drive($pipeline, $this->artifacts(['big.bin' => $content]), $transport);

        $entry = $this->entryFor($transport->manifest, 'big.bin');
        $this->assertCount(3, $entry['chunks'], 'expected 25 bytes / 10 = 3 chunks');
        $this->assertSame('big.bin', $entry['path']);
        $this->assertSame(25, $entry['size']);
        $this->assertSame(10, $entry['chunks'][0]['size']);
        $this->assertSame(10, $entry['chunks'][1]['size']);
        $this->assertSame(5, $entry['chunks'][2]['size']);
    }

    public function test_chunk_id_is_blake3_of_uploaded_bytes(): void
    {
        $content = 'hello world';
        [$pipeline, $transport] = $this->makePipeline(EncryptAndUpload::DEFAULT_CHUNK_BYTES);

        $transport = $this->drive($pipeline, $this->artifacts(['f.txt' => $content]), $transport);

        $entry = $this->entryFor($transport->manifest, 'f.txt');
        $chunk = $entry['chunks'][0];

        // The uploaded bytes for this hash must hash (blake3/blake2b content key)
        // to the chunk id, and the chunk size must equal the uploaded length.
        $this->assertArrayHasKey($chunk['blake3'], $transport->puts);
        $uploaded = $transport->puts[$chunk['blake3']];
        $this->assertSame($chunk['blake3'], Blake3::hashHex($uploaded));
        $this->assertSame(strlen($uploaded), $chunk['size'], 'chunk size is the uploaded byte length');
    }

    public function test_manifest_shape_matches_contract(): void
    {
        [$pipeline, $transport] = $this->makePipeline(EncryptAndUpload::DEFAULT_CHUNK_BYTES);

        // A "full" backup ships the DB dump first, then file parts. The DB
        // artifact must classify as entry_kind='db' with the contract path.
        $artifacts = $this->artifacts([
            'database.sql' => 'CREATE TABLE t;',
            'wp-content.part001.zip' => 'data',
        ]);
        $transport = $this->drive($pipeline, $artifacts, $transport);

        $m = $transport->manifest;
        $this->assertSame('snap-123', $m['snapshot_id']);
        $this->assertSame($this->identity['recipient'], $m['age_recipient']);

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

        // The DB dump rides the manifest as an entry_kind='db' entry.
        $kinds = array_map(static fn ($e) => $e['entry_kind'], $m['entries']);
        $this->assertContains('db', $kinds);
        $dbEntry = $this->entryFor($m, 'database.sql');
        $this->assertSame('db', $dbEntry['entry_kind']);
    }

    public function test_private_key_never_in_cp_payloads(): void
    {
        [$pipeline, $transport] = $this->makePipeline(8);
        $artifacts = $this->artifacts([
            'database.sql' => 'INSERT 1; secret-data',
            'wp-content.part001.zip' => 'secret-data',
        ]);
        $transport = $this->drive($pipeline, $artifacts, $transport);

        // Serialize EVERYTHING the pipeline handed to the transport for the CP:
        // the manifest, the presigned hash list, AND the uploaded chunk bytes.
        $cpBound = json_encode([
            'manifest'  => $transport->manifest,
            'presigned' => $transport->presignedHashes,
            'puts'      => array_map('base64_encode', $transport->puts),
        ]);

        $secret = $this->identity['secret'];
        // The AGE-SECRET-KEY string and the raw secret scalar (hex + base64)
        // must never appear in any CP-bound payload.
        $this->assertStringNotContainsString($this->identity['identity'], (string) $cpBound);
        $this->assertStringNotContainsString('AGE-SECRET-KEY-', (string) $cpBound);
        $this->assertStringNotContainsString(bin2hex($secret), (string) $cpBound);
        $this->assertStringNotContainsString(base64_encode($secret), (string) $cpBound);
    }

    public function test_dedup_skips_already_stored_chunks(): void
    {
        [$pipeline, $transport] = $this->makePipeline(EncryptAndUpload::DEFAULT_CHUNK_BYTES);
        // CP reports nothing needs upload -> presign returns an empty upload map,
        // the pipeline must NOT PUT anything but must still presign the hashes.
        $transport->allNew = false;
        $transport = $this->drive($pipeline, $this->artifacts(['a.txt' => 'dup', 'b.txt' => 'dup']), $transport);

        $this->assertSame([], $transport->puts, 'no uploads when CP reports all chunks stored');
        $this->assertNotEmpty($transport->presignedHashes);
    }

    // ------------------------------------------------------------------
    // BackupCommand REST-acceptor validation (the thin ADR-033 acceptor).
    //
    // BackupCommand no longer chunks/uploads — but it DOES still gate the
    // request: it refuses a missing or mismatched age recipient before
    // claiming a snapshot slot. Those refusals are pure input validation
    // (no DB / pipeline), so they're exercised directly against the real
    // command with a stub AgeIdentity. A valid UUID snapshot_id is required
    // to get past the earlier `invalid snapshot id` guard and reach the
    // recipient checks.
    // ------------------------------------------------------------------

    /** A stub AgeIdentity whose recipient match is controllable, no Keystore. */
    private function stubIdentity(string $recipient): AgeIdentity
    {
        return new class($recipient) extends AgeIdentity {
            private string $rcpt;

            public function __construct(string $recipient)
            {
                $this->rcpt = $recipient;
            }

            public function recipient(): string
            {
                return $this->rcpt;
            }

            public function recipientMatches(string $candidate): bool
            {
                return hash_equals($this->rcpt, $candidate);
            }
        };
    }

    /**
     * @return array<string,mixed>
     */
    private function acceptorParams(string $recipient): array
    {
        return [
            'snapshot_id'       => '11111111-2222-3333-4444-555555555555',
            'kind'              => 'files',
            'age_recipient'     => $recipient,
            'chunk_bytes'       => 4 << 20,
            'presign_endpoint'  => 'https://cp.example/agent/v1/backups/x/presign',
            'manifest_endpoint' => 'https://cp.example/agent/v1/backups/x/manifest',
        ];
    }

    public function test_acceptor_refuses_when_recipient_missing(): void
    {
        $cmd    = new BackupCommand($this->stubIdentity($this->identity['recipient']));
        $params = $this->acceptorParams($this->identity['recipient']);
        $params['age_recipient'] = '';

        $res = $cmd->execute([], $params);
        $this->assertFalse($res['ok']);
        $this->assertSame('missing age recipient', $res['detail']);
    }

    public function test_acceptor_refuses_when_recipient_mismatch(): void
    {
        $cmd = new BackupCommand($this->stubIdentity($this->identity['recipient']));
        $res = $cmd->execute([], $this->acceptorParams('age1someoneelsesrecipientvaluethatwontmatchxxxxxxxxxxxxxxxxx'));
        $this->assertFalse($res['ok']);
        $this->assertSame('age recipient mismatch', $res['detail']);
    }

    /**
     * GH #753. When the stored backup key no longer opens under the site's
     * encryption key, the backup is refused in plain words with a stable
     * code, before any preflight row, dedup claim or scratch directory, and
     * the unreadable key is left exactly as it is.
     */
    public function test_unreadable_age_identity_refuses_with_keystore_unreadable_code(): void
    {
        $keyPath = $this->scratchDir . '/master.key';
        file_put_contents($keyPath, random_bytes(32));

        $iv     = random_bytes(12);
        $tag    = '';
        $ct     = openssl_encrypt(random_bytes(32), 'aes-256-gcm', random_bytes(32), OPENSSL_RAW_DATA, $iv, $tag, '', 16);
        $this->assertIsString($ct);
        $sealed = base64_encode($iv . $tag . $ct);

        $options = [
            \WPMgr\Agent\Keystore::OPTION_MASTER_KEY_SOURCE => ['source' => 'file', 'path' => $keyPath],
            \WPMgr\Agent\Keystore::OPTION_AGE_IDENTITY      => $sealed,
        ];
        $writes = [];
        Functions\when('get_option')->alias(static function ($name, $default = false) use (&$options) {
            return $options[$name] ?? $default;
        });
        foreach (['update_option', 'add_option', 'delete_option', 'set_transient', 'wp_schedule_single_event'] as $fn) {
            Functions\when($fn)->alias(static function ($name) use (&$writes, $fn) {
                $writes[] = $fn . ':' . (is_string($name) ? $name : (string) json_encode($name));
                return true;
            });
        }

        // Any database access (preflight row, dedup claim) would reach this spy.
        $dbCalls = [];
        $previousWpdb = $GLOBALS['wpdb'] ?? null;
        $GLOBALS['wpdb'] = new class ($dbCalls) {
            public string $prefix = 'wp_';
            /** @var list<string> */
            private array $calls;
            /** @param list<string> $calls */
            public function __construct(array &$calls)
            {
                $this->calls = &$calls;
            }
            /** @param array<int,mixed> $args */
            public function __call(string $method, array $args): bool
            {
                $this->calls[] = $method;
                return false;
            }
        };

        // Where the scratch directory would be created. The base exists, so a
        // scratch directory made too early is observed here, not masked by a
        // failing mkdir of the base.
        if (!defined('WP_CONTENT_DIR')) {
            define('WP_CONTENT_DIR', sys_get_temp_dir() . '/wpmgr-shared-wp-content');
        }
        $runsBase = WP_CONTENT_DIR . '/wpmgr-agent/runs';
        if (!is_dir($runsBase)) {
            mkdir($runsBase, 0700, true);
        }
        $snapshotId = bin2hex(random_bytes(4)) . '-' . bin2hex(random_bytes(2)) . '-' . bin2hex(random_bytes(2))
            . '-' . bin2hex(random_bytes(2)) . '-' . bin2hex(random_bytes(6));
        $scratch = $runsBase . '/' . $snapshotId;
        $params  = $this->acceptorParams($this->identity['recipient']);
        $params['snapshot_id'] = $snapshotId;

        try {
            $keystore = new \WPMgr\Agent\Keystore();
            $cmd      = new BackupCommand(new AgeIdentity($keystore));
            $res      = $cmd->execute([], $params);
        } finally {
            $GLOBALS['wpdb'] = $previousWpdb;
            $scratchExisted = is_dir($scratch);
            if ($scratchExisted) {
                @rmdir($scratch);
            }
        }

        $this->assertFalse($res['ok']);
        $this->assertSame('keystore_unreadable', $res['code'] ?? null);
        $this->assertStringStartsWith('Backup not started: this site cannot read its backup key.', $res['detail']);
        $this->assertStringContainsString('Most often', $res['detail']);
        $this->assertStringNotContainsString('ciphertext authentication failed', $res['detail']);
        if (!EncryptAndUpload::ENCRYPT_CHUNKS) {
            $this->assertStringContainsString('Backups already taken are not affected.', $res['detail']);
        }
        $this->assertSame([], $dbCalls, 'The refusal must come before any database work.');
        $this->assertSame([], $writes, 'The refusal must not write anything.');
        $this->assertSame($sealed, $options[\WPMgr\Agent\Keystore::OPTION_AGE_IDENTITY]);
        $this->assertFalse($scratchExisted, 'The refusal must come before the scratch directory is created.');
    }

    /**
     * No backup key is stored and the site keypair does not open under the
     * current key. The backup command's recipient check must not create a
     * backup key: it refuses with the keystore_unreadable code, names what
     * does not open, and writes nothing.
     */
    public function test_absent_backup_key_is_not_created_when_the_keypair_does_not_open(): void
    {
        $keyPath = $this->scratchDir . '/master.key';
        file_put_contents($keyPath, random_bytes(32));

        $iv      = random_bytes(12);
        $tag     = '';
        $ct      = openssl_encrypt(random_bytes(96), 'aes-256-gcm', random_bytes(32), OPENSSL_RAW_DATA, $iv, $tag, '', 16);
        $this->assertIsString($ct);
        $keypair = base64_encode($iv . $tag . $ct);

        $options = [
            \WPMgr\Agent\Keystore::OPTION_MASTER_KEY_SOURCE => ['source' => 'file', 'path' => $keyPath],
            \WPMgr\Agent\Keystore::OPTION_SITE_KEYPAIR      => $keypair,
        ];
        $writes = [];
        Functions\when('get_option')->alias(static function ($name, $default = false) use (&$options) {
            return $options[$name] ?? $default;
        });
        foreach (['update_option', 'add_option', 'delete_option', 'set_transient', 'wp_schedule_single_event'] as $fn) {
            Functions\when($fn)->alias(static function ($name, $value = null) use (&$writes, &$options, $fn) {
                $writes[] = $fn . ':' . (is_string($name) ? $name : (string) json_encode($name));
                if ($fn === 'update_option' && is_string($name)) {
                    $options[$name] = $value;
                }
                return true;
            });
        }

        $res = (new BackupCommand(new AgeIdentity(new \WPMgr\Agent\Keystore())))
            ->execute([], $this->acceptorParams($this->identity['recipient']));

        $this->assertArrayNotHasKey(
            \WPMgr\Agent\Keystore::OPTION_AGE_IDENTITY,
            $options,
            'No backup key may be created while the site keypair does not open.'
        );
        $this->assertSame([], $writes, 'The refusal must not write anything.');
        $this->assertFalse($res['ok']);
        $this->assertSame('keystore_unreadable', $res['code'] ?? null);
        $this->assertStringStartsWith(
            'Backup not started: this site has no backup key yet, and it does not create one while the keys saved on it do not open.',
            $res['detail']
        );
        $this->assertStringContainsString('The connection keys saved on this site cannot be opened', $res['detail']);
        $this->assertSame($keypair, $options[\WPMgr\Agent\Keystore::OPTION_SITE_KEYPAIR]);
    }

    /**
     * Over-fire guard: no backup key is stored and the site keypair opens, so
     * the recipient check creates the backup key (a first backup can
     * proceed) and it opens under the current key.
     */
    public function test_absent_backup_key_is_created_when_the_keypair_opens(): void
    {
        $keyPath = $this->scratchDir . '/master.key';
        file_put_contents($keyPath, random_bytes(32));
        $options = [
            \WPMgr\Agent\Keystore::OPTION_MASTER_KEY_SOURCE => ['source' => 'file', 'path' => $keyPath],
        ];
        Functions\when('get_option')->alias(static function ($name, $default = false) use (&$options) {
            return $options[$name] ?? $default;
        });
        Functions\when('update_option')->alias(static function ($name, $value) use (&$options) {
            $options[$name] = $value;
            return true;
        });
        (new \WPMgr\Agent\Keystore())->generateSiteKeypair();

        $identity = new AgeIdentity(new \WPMgr\Agent\Keystore());
        $res      = (new BackupCommand($identity))->execute([], $this->acceptorParams($this->identity['recipient']));

        $this->assertArrayHasKey(\WPMgr\Agent\Keystore::OPTION_AGE_IDENTITY, $options, 'The absent backup key must be created.');
        $this->assertSame(\WPMgr\Agent\Keystore::PROBE_OK, (new \WPMgr\Agent\Keystore())->probe()['state']);
        $this->assertStringStartsWith('age1', $identity->recipient());
        // The command's recipient is another site's, so the check itself refuses.
        $this->assertSame('age recipient mismatch', $res['detail']);
    }

    /**
     * A recipient check that fails for a reason the keystore does not
     * explain (every stored key opens) is refused without the
     * keystore_unreadable code, so it is not reported as a keystore problem.
     */
    public function test_recipient_check_failure_on_a_healthy_keystore_is_not_keystore_unreadable(): void
    {
        $identity = new class extends AgeIdentity {
            public function __construct()
            {
            }

            public function recipientMatches(string $candidate): bool
            {
                throw new \RuntimeException('recipient derivation failed');
            }

            public function probeKeystore(): array
            {
                return [
                    'state'      => \WPMgr\Agent\Keystore::PROBE_OK,
                    'key_source' => 'salts',
                    'items'      => [
                        'site_keypair'             => 'ok',
                        'cp_public_key'            => 'ok',
                        'age_identity'             => 'ok',
                        'email_secret'             => 'absent',
                        'email_connection_secrets' => 'absent',
                    ],
                    'unreadable' => [],
                    'detail'     => '',
                ];
            }
        };

        $res = (new BackupCommand($identity))->execute([], $this->acceptorParams($this->identity['recipient']));

        $this->assertFalse($res['ok']);
        $this->assertArrayNotHasKey('code', $res);
        $this->assertSame('Backup not started: this site could not check its backup key.', $res['detail']);
    }

    /**
     * Recursively remove a directory tree (scratch dir cleanup).
     */
    private function rrmdir(string $dir): void
    {
        if ($dir === '' || !is_dir($dir)) {
            return;
        }
        $items = scandir($dir);
        if ($items === false) {
            return;
        }
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $path = $dir . DIRECTORY_SEPARATOR . $item;
            if (is_dir($path)) {
                $this->rrmdir($path);
            } else {
                @unlink($path);
            }
        }
        @rmdir($dir);
    }
}
