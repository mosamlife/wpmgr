<?php
/**
 * Backup command: client-side-encrypted, content-addressed, incremental backup.
 *
 * Contract (CP -> agent), mirroring apps/api/internal/agentcmd/backup_contract.go:
 *   POST /wp-json/wpmgr/v1/command/backup
 *   request:  { "snapshot_id", "kind" in {files|db|full}, "age_recipient",
 *               "chunk_bytes", "presign_endpoint", "manifest_endpoint" }
 *   response: { "ok": bool, "detail": string }   (BackupResponse)
 *
 * Flow (per backup_contract.go and agent_handler.go):
 *   1. Verify age_recipient is non-empty AND equals THIS site's own recipient
 *      (the operator could never decrypt a backup made to a foreign recipient).
 *   2. Build manifest entries by chunking each source (files under wp-content;
 *      db = `wp db export --single-transaction`) into ~chunk_bytes plaintext
 *      pieces.
 *   3. For each plaintext chunk: age-encrypt to the recipient -> blake3(hex) of
 *      the CIPHERTEXT = the chunk id; record {blake3,size(ciphertext)} ordered.
 *   4. POST candidate ciphertext hashes to presign_endpoint; PUT only the
 *      not-yet-stored ciphertext chunks to the returned presigned URLs (dedup).
 *   5. POST the manifest (snapshot_id, age_recipient, entries) to manifest_endpoint.
 *
 * Memory is bounded: chunks are read/encrypted one at a time; ciphertext for a
 * given chunk is held only long enough to hash + (maybe) upload it.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\AgeIdentity;
use WPMgr\Agent\Support\Blake3;
use WPMgr\Agent\Support\BackupSource;
use WPMgr\Agent\Support\BackupTransport;

/**
 * Performs an incremental, client-side-encrypted backup.
 */
final class BackupCommand implements CommandInterface
{
    /** Valid snapshot kinds. */
    private const KINDS = ['files', 'db', 'full'];

    /** Manifest entry kinds (contract: EntryKindFile / EntryKindDB). */
    private const ENTRY_FILE = 'file';
    private const ENTRY_DB   = 'db';

    /** Default plaintext chunk size (contract ChunkBytes = 4 MiB). */
    private const DEFAULT_CHUNK_BYTES = 4 << 20;

    private AgeIdentity $identity;

    private BackupSource $source;

    private BackupTransport $transport;

    /**
     * @param AgeIdentity     $identity  Site age identity manager.
     * @param BackupSource    $source    Filesystem/DB source seam.
     * @param BackupTransport $transport CP-callback + S3 transport seam.
     */
    public function __construct(AgeIdentity $identity, BackupSource $source, BackupTransport $transport)
    {
        $this->identity  = $identity;
        $this->source    = $source;
        $this->transport = $transport;
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'backup';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused).
     * @param array<string,mixed> $params Request parameters (BackupRequest).
     * @return array{ok:bool,detail:string}
     */
    public function execute(array $claims, array $params): array
    {
        $snapshotId = $this->str($params, 'snapshot_id');
        $kind       = $this->str($params, 'kind');
        $recipient  = $this->str($params, 'age_recipient');
        $presign    = $this->str($params, 'presign_endpoint');
        $manifestEp = $this->str($params, 'manifest_endpoint');

        $chunkBytes = isset($params['chunk_bytes']) && is_numeric($params['chunk_bytes']) && (int) $params['chunk_bytes'] > 0
            ? (int) $params['chunk_bytes']
            : self::DEFAULT_CHUNK_BYTES;

        if ($snapshotId === '' || $presign === '' || $manifestEp === '') {
            return $this->refuse('missing snapshot or callback endpoints');
        }
        if (!in_array($kind, self::KINDS, true)) {
            return $this->refuse('invalid kind');
        }
        // The CP guarantees age_recipient is set; refuse otherwise (an
        // undecryptable backup is useless), and verify it is OUR recipient so a
        // substituted recipient cannot be used.
        if ($recipient === '') {
            return $this->refuse('missing age recipient');
        }
        if (!$this->identity->recipientMatches($recipient)) {
            return $this->refuse('age recipient mismatch');
        }

        try {
            $entries = $this->buildEntries($kind, $chunkBytes);
        } catch (\Throwable $e) {
            return $this->refuse('backup preparation failed');
        }

        if ($entries === []) {
            return $this->refuse('nothing to back up');
        }

        try {
            $this->uploadAndSubmit($snapshotId, $recipient, $presign, $manifestEp, $entries);
        } catch (\Throwable $e) {
            return $this->refuse('backup upload failed');
        }

        return ['ok' => true, 'detail' => 'completed'];
    }

    /**
     * Build the ordered manifest entries (encrypting + hashing every chunk) for
     * the requested kind. Each entry carries its ordered ciphertext chunk list;
     * the encrypted bytes are cached transiently per chunk for the upload pass.
     *
     * @param string $kind       files|db|full.
     * @param int    $chunkBytes Plaintext chunk size.
     * @return list<array{path:string,entry_kind:string,table_name:string,mode:int,size:int,chunks:list<array{blake3:string,size:int}>,_cipher:array<string,string>}>
     */
    private function buildEntries(string $kind, int $chunkBytes): array
    {
        $entries = [];

        if ($kind === 'files' || $kind === 'full') {
            foreach ($this->source->files() as $file) {
                $entries[] = $this->fileEntry($file, $chunkBytes);
            }
        }

        if ($kind === 'db' || $kind === 'full') {
            $sql = $this->source->exportDatabase();
            if ($sql !== '') {
                $entries[] = $this->blobEntry(
                    BackupSource::DB_ENTRY_PATH,
                    self::ENTRY_DB,
                    0,
                    $sql,
                    $chunkBytes
                );
            }
        }

        return $entries;
    }

    /**
     * Chunk + encrypt + hash a single file into a manifest entry.
     *
     * @param array{abs:string,rel:string,mode:int,size:int} $file       File tuple.
     * @param int                                             $chunkBytes Chunk size.
     * @return array{path:string,entry_kind:string,table_name:string,mode:int,size:int,chunks:list<array{blake3:string,size:int}>,_cipher:array<string,string>}
     */
    private function fileEntry(array $file, int $chunkBytes): array
    {
        $chunks = [];
        $cipher = [];
        $offset = 0;
        $size   = $file['size'];

        if ($size === 0) {
            // Represent an empty file as a single empty plaintext chunk so it
            // round-trips on restore.
            $this->addChunk('', $chunks, $cipher);
        } else {
            while ($offset < $size) {
                $slice = $this->source->readSlice($file['abs'], $offset, $chunkBytes);
                if ($slice === '') {
                    break;
                }
                $offset += strlen($slice);
                $this->addChunk($slice, $chunks, $cipher);
            }
        }

        return [
            'path'       => $file['rel'],
            'entry_kind' => self::ENTRY_FILE,
            'table_name' => '',
            'mode'       => $file['mode'],
            'size'       => $size,
            'chunks'     => $chunks,
            '_cipher'    => $cipher,
        ];
    }

    /**
     * Chunk + encrypt + hash an in-memory blob (the DB dump) into an entry.
     *
     * @param string $path       Logical entry path.
     * @param string $entryKind  Entry kind.
     * @param int    $mode        File mode (0 for db).
     * @param string $blob        Plaintext bytes.
     * @param int    $chunkBytes  Chunk size.
     * @return array{path:string,entry_kind:string,table_name:string,mode:int,size:int,chunks:list<array{blake3:string,size:int}>,_cipher:array<string,string>}
     */
    private function blobEntry(string $path, string $entryKind, int $mode, string $blob, int $chunkBytes): array
    {
        $chunks = [];
        $cipher = [];
        $len    = strlen($blob);
        $offset = 0;

        do {
            $slice   = substr($blob, $offset, $chunkBytes);
            $offset += strlen($slice);
            $this->addChunk($slice, $chunks, $cipher);
        } while ($offset < $len);

        return [
            'path'       => $path,
            'entry_kind' => $entryKind,
            'table_name' => '',
            'mode'       => $mode,
            'size'       => $len,
            'chunks'     => $chunks,
            '_cipher'    => $cipher,
        ];
    }

    /**
     * Encrypt one plaintext chunk, hash its CIPHERTEXT, and record the ordered
     * chunk ref + the ciphertext (keyed by hash) for the upload pass.
     *
     * @param string                                            $plaintext Chunk plaintext.
     * @param list<array{blake3:string,size:int}>               $chunks    Ordered refs (by-ref).
     * @param array<string,string>                              $cipher    hash => ciphertext (by-ref).
     * @return void
     */
    private function addChunk(string $plaintext, array &$chunks, array &$cipher): void
    {
        $ciphertext = $this->identity->encryptChunk($plaintext);
        $hash       = Blake3::hashHex($ciphertext);

        $chunks[]      = ['blake3' => $hash, 'size' => strlen($ciphertext)];
        $cipher[$hash] = $ciphertext;
    }

    /**
     * Presign the unique ciphertext hashes, upload the not-yet-stored chunks,
     * then submit the manifest. The manifest sent to the CP carries NO age
     * identity and NO ciphertext — only hashes/sizes/paths.
     *
     * @param string                                                                                                                            $snapshotId Snapshot id.
     * @param string                                                                                                                            $recipient  age recipient (echoed in manifest).
     * @param string                                                                                                                            $presign    Presign endpoint URL.
     * @param string                                                                                                                            $manifestEp Manifest endpoint URL.
     * @param list<array{path:string,entry_kind:string,table_name:string,mode:int,size:int,chunks:list<array{blake3:string,size:int}>,_cipher:array<string,string>}> $entries Entries with transient ciphertext.
     * @return void
     * @throws \RuntimeException On transport failure.
     */
    private function uploadAndSubmit(string $snapshotId, string $recipient, string $presign, string $manifestEp, array $entries): void
    {
        // Collect every unique ciphertext hash across all entries.
        $hashes = [];
        foreach ($entries as $entry) {
            foreach ($entry['chunks'] as $chunk) {
                $hashes[$chunk['blake3']] = true;
            }
        }

        $uploads = $this->transport->presignChunks($presign, $snapshotId, array_keys($hashes));

        // Upload only the chunks the CP asked for (dedup: it omits stored ones).
        foreach ($entries as $entry) {
            foreach ($entry['chunks'] as $chunk) {
                $hash = $chunk['blake3'];
                if (!isset($uploads[$hash])) {
                    continue; // already stored for the tenant.
                }
                $ciphertext = $entry['_cipher'][$hash] ?? null;
                if ($ciphertext === null) {
                    continue;
                }
                if (!$this->transport->putChunk($uploads[$hash], $ciphertext)) {
                    throw new \RuntimeException('WPMgr Agent: chunk upload failed.');
                }
                // Avoid re-uploading a duplicate hash present in multiple entries.
                unset($uploads[$hash]);
            }
        }

        // Strip the transient ciphertext before submitting the manifest.
        $manifestEntries = [];
        foreach ($entries as $entry) {
            unset($entry['_cipher']);
            $manifestEntries[] = $entry;
        }

        $result = $this->transport->submitManifest($manifestEp, $snapshotId, $recipient, $manifestEntries);
        if (!$result['ok']) {
            throw new \RuntimeException('WPMgr Agent: manifest submission rejected.');
        }
    }

    /**
     * Build a refusal/early-exit BackupResponse.
     *
     * @param string $detail Short, secret-free reason.
     * @return array{ok:bool,detail:string}
     */
    private function refuse(string $detail): array
    {
        return ['ok' => false, 'detail' => $detail];
    }

    /**
     * Read a string parameter safely.
     *
     * @param array<string,mixed> $params Params.
     * @param string              $key    Key.
     * @return string
     */
    private function str(array $params, string $key): string
    {
        return isset($params[$key]) && is_string($params[$key]) ? $params[$key] : '';
    }
}
