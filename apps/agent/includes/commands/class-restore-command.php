<?php
/**
 * Restore command: download, integrity-verify, decrypt, and reassemble a
 * (possibly partial) snapshot.
 *
 * Contract (CP -> agent), mirroring apps/api/internal/agentcmd/backup_contract.go:
 *   POST /wp-json/wpmgr/v1/command/restore
 *   request:  { "snapshot_id", "kind", "entries": [ { "path", "entry_kind",
 *               "table_name", "mode", "size", "chunks": [ { "blake3",
 *               "get_url", "size" } ] } ] }   (RestoreRequest / RestoreEntry /
 *                                              RestoreChunk)
 *   response: { "ok": bool, "restored_entries": int, "verified": bool,
 *               "log": string }   (RestoreResponse)
 *
 * For each entry the agent processes its chunks IN ORDER: download the
 * ciphertext from the presigned GET URL, recompute blake3 over the downloaded
 * CIPHERTEXT and reject on mismatch (integrity), age-decrypt with the site's
 * stored identity, and reassemble. File entries are written atomically inside a
 * containment-checked path under wp-content; the db entry is imported.
 *
 * No decryption key is present in the request — the agent decrypts with the age
 * identity it alone holds. Presigned URLs are bearer credentials and never
 * logged.
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
 * Reassembles files / imports the database from an encrypted snapshot.
 */
final class RestoreCommand implements CommandInterface
{
    private const ENTRY_FILE = 'file';
    private const ENTRY_DB   = 'db';

    private AgeIdentity $identity;

    private BackupSource $source;

    private BackupTransport $transport;

    /**
     * @param AgeIdentity     $identity  Site age identity manager.
     * @param BackupSource    $source    Filesystem/DB sink seam.
     * @param BackupTransport $transport S3 download transport seam.
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
        return 'restore';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused).
     * @param array<string,mixed> $params Request parameters (RestoreRequest).
     * @return array{ok:bool,restored_entries:int,verified:bool,log:string}
     */
    public function execute(array $claims, array $params): array
    {
        $entries = isset($params['entries']) && is_array($params['entries']) ? $params['entries'] : [];
        if ($entries === []) {
            return $this->result(false, 0, true, 'no entries to restore');
        }

        $restored = 0;
        $verified = true;
        $failures = 0;

        foreach ($entries as $entry) {
            if (!is_array($entry)) {
                $failures++;
                continue;
            }
            try {
                $outcome = $this->restoreEntry($entry);
            } catch (\Throwable $e) {
                $failures++;
                continue;
            }
            if (!$outcome['verified']) {
                // A blake3 mismatch on any chunk taints the restore's integrity.
                $verified = false;
                $failures++;
                continue;
            }
            if ($outcome['ok']) {
                $restored++;
            } else {
                $failures++;
            }
        }

        $ok = $failures === 0;

        return $this->result($ok, $restored, $verified, $ok ? 'restore completed' : 'restore completed with errors');
    }

    /**
     * Restore a single entry: gather its ordered, verified, decrypted plaintext
     * then write/import it.
     *
     * @param array<string,mixed> $entry RestoreEntry.
     * @return array{ok:bool,verified:bool}
     * @throws \RuntimeException On a write/import failure.
     */
    private function restoreEntry(array $entry): array
    {
        $entryKind = isset($entry['entry_kind']) && is_string($entry['entry_kind']) ? $entry['entry_kind'] : '';
        $path      = isset($entry['path']) && is_string($entry['path']) ? $entry['path'] : '';
        $mode      = isset($entry['mode']) && is_numeric($entry['mode']) ? (int) $entry['mode'] : 0;
        $chunks    = isset($entry['chunks']) && is_array($entry['chunks']) ? $entry['chunks'] : [];

        // Reassemble plaintext from the ordered chunks (streaming to a temp file
        // for files so a large entry never lives fully in memory).
        if ($entryKind === self::ENTRY_DB) {
            $plain = $this->reassembleToString($chunks);
            if ($plain === null) {
                return ['ok' => false, 'verified' => false];
            }

            return ['ok' => $this->source->importDatabase($plain), 'verified' => true];
        }

        if ($entryKind !== self::ENTRY_FILE) {
            return ['ok' => false, 'verified' => true];
        }

        $target = $this->source->resolveWritePath($path);
        if ($target === '') {
            // Path traversal / outside-wp-content => reject (not an integrity issue).
            return ['ok' => false, 'verified' => true];
        }

        return $this->reassembleToFile($chunks, $target, $mode);
    }

    /**
     * Download + verify + decrypt all chunks of an entry, concatenated into a
     * string (used for the db dump).
     *
     * @param array<int,mixed> $chunks Ordered RestoreChunk list.
     * @return string|null Plaintext, or null on integrity/transport failure.
     */
    private function reassembleToString(array $chunks): ?string
    {
        $out = '';
        foreach ($chunks as $chunk) {
            $plain = $this->fetchChunkPlaintext($chunk);
            if ($plain === null) {
                return null;
            }
            $out .= $plain;
        }

        return $out;
    }

    /**
     * Download + verify + decrypt all chunks, streaming into a temp file, then
     * atomically move into the containment-checked target path.
     *
     * @param array<int,mixed> $chunks Ordered RestoreChunk list.
     * @param string           $target Absolute target path (inside wp-content).
     * @param int              $mode   File mode bits to apply.
     * @return array{ok:bool,verified:bool}
     */
    private function reassembleToFile(array $chunks, string $target, int $mode): array
    {
        $dir = dirname($target);
        if (!is_dir($dir) && !@mkdir($dir, 0755, true) && !is_dir($dir)) {
            return ['ok' => false, 'verified' => true];
        }

        $tmp = @tempnam($dir, '.wpmgr-restore');
        if ($tmp === false) {
            return ['ok' => false, 'verified' => true];
        }

        $handle = @fopen($tmp, 'wb');
        if ($handle === false) {
            @unlink($tmp);

            return ['ok' => false, 'verified' => true];
        }

        try {
            foreach ($chunks as $chunk) {
                $plain = $this->fetchChunkPlaintext($chunk);
                if ($plain === null) {
                    // Distinguish integrity failure from a benign empty chunk:
                    // fetchChunkPlaintext returns null only on download/verify/
                    // decrypt failure, so treat as an integrity failure.
                    fclose($handle);
                    @unlink($tmp);

                    return ['ok' => false, 'verified' => false];
                }
                if ($plain !== '' && fwrite($handle, $plain) === false) {
                    fclose($handle);
                    @unlink($tmp);

                    return ['ok' => false, 'verified' => true];
                }
            }
        } catch (\Throwable $e) {
            fclose($handle);
            @unlink($tmp);

            return ['ok' => false, 'verified' => true];
        }

        fclose($handle);

        if (!@rename($tmp, $target)) {
            @unlink($tmp);

            return ['ok' => false, 'verified' => true];
        }
        if ($mode > 0) {
            @chmod($target, $mode & 0777);
        }

        return ['ok' => true, 'verified' => true];
    }

    /**
     * Download one chunk's ciphertext, verify blake3 over the CIPHERTEXT, and
     * age-decrypt it. Returns null on any download/verify/decrypt failure.
     *
     * @param mixed $chunk RestoreChunk (path, get_url, blake3, size).
     * @return string|null Decrypted plaintext, or null on failure.
     */
    private function fetchChunkPlaintext($chunk): ?string
    {
        if (!is_array($chunk)) {
            return null;
        }
        $expected = isset($chunk['blake3']) && is_string($chunk['blake3']) ? $chunk['blake3'] : '';
        $getUrl   = isset($chunk['get_url']) && is_string($chunk['get_url']) ? $chunk['get_url'] : '';
        if ($expected === '' || $getUrl === '') {
            return null;
        }

        $ciphertext = $this->transport->getChunk($getUrl);
        if ($ciphertext === null) {
            return null;
        }

        // INTEGRITY: recompute blake3 over the downloaded ciphertext and compare
        // in constant time. A tampered/corrupted chunk is rejected here.
        $actual = Blake3::hashHex($ciphertext);
        if (!hash_equals($expected, $actual)) {
            return null;
        }

        try {
            return $this->identity->decryptChunk($ciphertext);
        } catch (\Throwable $e) {
            return null;
        }
    }

    /**
     * Build a RestoreResponse-shaped result.
     *
     * @param bool   $ok       Whether the restore succeeded.
     * @param int    $restored Entries restored.
     * @param bool   $verified Whether every chunk matched its blake3.
     * @param string $log      Short, secret-free detail.
     * @return array{ok:bool,restored_entries:int,verified:bool,log:string}
     */
    private function result(bool $ok, int $restored, bool $verified, string $log): array
    {
        return [
            'ok'               => $ok,
            'restored_entries' => $restored,
            'verified'         => $verified,
            'log'              => $log,
        ];
    }
}
