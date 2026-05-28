<?php
/**
 * Tests for the restore command: ordered reassembly, blake3-of-ciphertext
 * verification, tampered-chunk rejection, path-traversal containment, db import,
 * and partial restore.
 *
 * @package WPMgr\Agent\Tests
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests;

use Brain\Monkey;
use WPMgr\Agent\Commands\RestoreCommand;
use WPMgr\Agent\Support\AgeCrypto;
use WPMgr\Agent\Support\AgeIdentity;
use WPMgr\Agent\Support\Blake3;
use WPMgr\Agent\Support\BackupSource;
use WPMgr\Agent\Support\BackupTransport;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Commands\RestoreCommand
 */
final class RestoreCommandTest extends TestCase
{
    private string $root = '';

    private AgeCrypto $age;

    /** @var array{recipient:string,identity:string,secret:string} */
    private array $pair;

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        $this->age  = new AgeCrypto();
        $this->pair = $this->age->generateIdentity();
        $this->root = sys_get_temp_dir() . '/wpmgr-restore-' . bin2hex(random_bytes(6));
        mkdir($this->root, 0755, true);
    }

    protected function tear_down(): void
    {
        $this->rrmdir($this->root);
        Monkey\tearDown();
        parent::tear_down();
    }

    private function rrmdir(string $dir): void
    {
        if (!is_dir($dir)) {
            return;
        }
        $items = scandir($dir);
        foreach ($items === false ? [] : $items as $i) {
            if ($i === '.' || $i === '..') {
                continue;
            }
            $p = $dir . '/' . $i;
            is_dir($p) ? $this->rrmdir($p) : @unlink($p);
        }
        @rmdir($dir);
    }

    private function identity(): AgeIdentity
    {
        $secret = $this->pair['secret'];
        $crypto = $this->age;

        return new class($secret, $crypto) extends AgeIdentity {
            private string $secret;
            private AgeCrypto $crypto;

            public function __construct(string $secret, AgeCrypto $crypto)
            {
                $this->secret = $secret;
                $this->crypto = $crypto;
            }

            public function decryptChunk(string $ciphertext): string
            {
                return $this->crypto->decrypt($ciphertext, $this->secret);
            }
        };
    }

    private function source(): BackupSource
    {
        $root = $this->root;

        return new class($root) extends BackupSource {
            public string $importedSql = '';
            private string $root;

            public function __construct(string $root)
            {
                $this->root = $root;
            }

            public function contentRoot(): string
            {
                return $this->root;
            }

            public function importDatabase(string $sql): bool
            {
                $this->importedSql = $sql;

                return true;
            }
        };
    }

    /**
     * A transport whose getChunk serves a fixed map of ciphertext by URL.
     *
     * @param array<string,string> $byUrl URL => ciphertext.
     */
    private function transport(array $byUrl): BackupTransport
    {
        return new class($byUrl) extends BackupTransport {
            /** @var array<string,string> */
            private array $byUrl;

            /** @param array<string,string> $byUrl Map. */
            public function __construct(array $byUrl)
            {
                $this->byUrl = $byUrl;
            }

            public function getChunk(string $presignedUrl): ?string
            {
                return $this->byUrl[$presignedUrl] ?? null;
            }
        };
    }

    /**
     * Encrypt plaintext into a chunk + its presigned-GET descriptor.
     *
     * @param string $plain Plaintext.
     * @param string $url   Fake GET URL.
     * @return array{chunk:array{blake3:string,get_url:string,size:int},url:string,ct:string}
     */
    private function chunk(string $plain, string $url): array
    {
        $ct   = $this->age->encrypt($plain, $this->pair['recipient']);
        $hash = Blake3::hashHex($ct);

        return [
            'chunk' => ['blake3' => $hash, 'get_url' => $url, 'size' => strlen($ct)],
            'url'   => $url,
            'ct'    => $ct,
        ];
    }

    public function test_reassembles_file_in_order(): void
    {
        $c1 = $this->chunk('Hello, ', 'u1');
        $c2 = $this->chunk('World!',  'u2');
        $transport = $this->transport([$c1['url'] => $c1['ct'], $c2['url'] => $c2['ct']]);
        $cmd = new RestoreCommand($this->identity(), $this->source(), $transport);

        $res = $cmd->execute([], ['entries' => [[
            'path'       => 'uploads/note.txt',
            'entry_kind' => 'file',
            'mode'       => 0644,
            'size'       => 13,
            'chunks'     => [$c1['chunk'], $c2['chunk']],
        ]]]);

        $this->assertTrue($res['ok']);
        $this->assertTrue($res['verified']);
        $this->assertSame(1, $res['restored_entries']);
        $this->assertSame('Hello, World!', file_get_contents($this->root . '/uploads/note.txt'));
    }

    public function test_rejects_tampered_chunk(): void
    {
        $c1 = $this->chunk('integrity matters', 'u1');
        // Tamper with the served ciphertext so blake3 no longer matches.
        $tampered = $c1['ct'];
        $tampered[strlen($tampered) - 1] = $tampered[strlen($tampered) - 1] === "\x00" ? "\x01" : "\x00";

        $transport = $this->transport([$c1['url'] => $tampered]);
        $cmd = new RestoreCommand($this->identity(), $this->source(), $transport);

        $res = $cmd->execute([], ['entries' => [[
            'path'       => 'uploads/x.txt',
            'entry_kind' => 'file',
            'mode'       => 0644,
            'size'       => 17,
            'chunks'     => [$c1['chunk']],
        ]]]);

        $this->assertFalse($res['ok']);
        $this->assertFalse($res['verified'], 'tampered ciphertext must fail blake3 verification');
        $this->assertFileDoesNotExist($this->root . '/uploads/x.txt');
    }

    public function test_rejects_path_traversal(): void
    {
        $c1 = $this->chunk('evil', 'u1');
        $transport = $this->transport([$c1['url'] => $c1['ct']]);
        $cmd = new RestoreCommand($this->identity(), $this->source(), $transport);

        $res = $cmd->execute([], ['entries' => [[
            'path'       => '../../wp-config.php',
            'entry_kind' => 'file',
            'mode'       => 0644,
            'size'       => 4,
            'chunks'     => [$c1['chunk']],
        ]]]);

        $this->assertFalse($res['ok']);
        // It is a containment rejection, not an integrity failure.
        $this->assertTrue($res['verified']);
        $this->assertFileDoesNotExist(dirname($this->root) . '/wp-config.php');
    }

    public function test_imports_database_entry(): void
    {
        $sql = "DROP TABLE t;\nCREATE TABLE t (id INT);\n";
        $c1  = $this->chunk($sql, 'u1');
        $transport = $this->transport([$c1['url'] => $c1['ct']]);
        $source = $this->source();
        $cmd = new RestoreCommand($this->identity(), $source, $transport);

        $res = $cmd->execute([], ['entries' => [[
            'path'       => 'database.sql',
            'entry_kind' => 'db',
            'mode'       => 0,
            'size'       => strlen($sql),
            'chunks'     => [$c1['chunk']],
        ]]]);

        $this->assertTrue($res['ok']);
        $this->assertTrue($res['verified']);
        /** @var object{importedSql:string} $source */
        $this->assertSame($sql, $source->importedSql);
    }

    public function test_partial_restore_subset_of_entries(): void
    {
        $c1 = $this->chunk('only this one', 'u1');
        $transport = $this->transport([$c1['url'] => $c1['ct']]);
        $cmd = new RestoreCommand($this->identity(), $this->source(), $transport);

        $res = $cmd->execute([], ['entries' => [[
            'path'       => 'plugins/foo/readme.txt',
            'entry_kind' => 'file',
            'mode'       => 0644,
            'size'       => 13,
            'chunks'     => [$c1['chunk']],
        ]]]);

        $this->assertTrue($res['ok']);
        $this->assertSame(1, $res['restored_entries']);
        $this->assertSame('only this one', file_get_contents($this->root . '/plugins/foo/readme.txt'));
    }

    public function test_no_entries_is_ok(): void
    {
        $cmd = new RestoreCommand($this->identity(), $this->source(), $this->transport([]));
        $res = $cmd->execute([], ['entries' => []]);
        $this->assertFalse($res['ok']);
        $this->assertSame(0, $res['restored_entries']);
    }
}
