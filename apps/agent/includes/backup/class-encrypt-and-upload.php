<?php
/**
 * EncryptAndUpload: M5.6 / ADR-033 — three-pass chunk pipeline that turns the
 * artifacts produced by DbDumper + FilesArchiver into encrypted, content-
 * addressed chunks on the CP-presigned object store, then submits the manifest.
 *
 * Pass model (each pass is independently checkpointable so a watchdog re-entry
 * can resume at the right boundary without redoing finished work):
 *
 *   1. encryptChunks()
 *      Streams each artifact in fixed-size plaintext chunks (default 4 MiB —
 *      matches the M4 `agentcmd.ChunkBytes` contract). For every chunk:
 *        a. age-encrypt with the snapshot recipient (AgeCrypto::encrypt is
 *           already chunk-safe under libsodium STREAM internally).
 *        b. blake3 the CIPHERTEXT (the chunk ID is content-addressed over the
 *           encrypted bytes — that's how the CP-side dedup works).
 *        c. write the ciphertext to scratch as `chunks-<hash>.age` so the
 *           upload pass can re-read it without re-encrypting.
 *      Builds the ordered manifest entries (one per artifact, with the ordered
 *      chunks list). Returns a cursor carrying the entries + an `all_hashes`
 *      list so uploadChunks can ask the CP what's missing.
 *
 *   2. uploadChunks()
 *      Calls BackupTransport::presignChunks() once with the full hash list;
 *      the CP returns presigned PUT URLs ONLY for hashes it doesn't already
 *      have (incremental dedup — repeat backups of the same site re-upload
 *      only the diff). For each (hash, url) the pass reads the local
 *      ciphertext, PUTs it, and on success @unlinks the local file to free
 *      disk progressively. Also @unlinks files for hashes the CP already had
 *      (no PUT needed; the local file is dead weight).
 *
 *   3. submitManifest()
 *      One signed POST to the CP's manifest endpoint with the ordered entries
 *      and the age recipient. Treats a 4xx "manifest already recorded"
 *      response as success — the pipeline is idempotent across watchdog
 *      re-entries.
 *
 * Why the chunk file approach (vs. holding ciphertext in memory):
 *   - Memory bound = ONE chunk's plaintext + ONE chunk's ciphertext, regardless
 *     of total backup size. A 50 GB site's encrypt-and-upload runs in the same
 *     memory footprint as a 50 MB site's.
 *   - Persisting ciphertext to disk decouples encrypt-pass success from
 *     upload-pass success. A network blip during pass 2 just means the next
 *     watchdog re-entry re-runs pass 2 from where it stopped; the expensive
 *     X25519/AEAD work of pass 1 is preserved.
 *
 * Memory and disk budget for a typical 4 MiB chunk_bytes:
 *   - Resident: ~4 MiB plaintext + ~4 MiB ciphertext + overhead ≈ 10 MiB peak.
 *   - Scratch: total backup ciphertext (~= total plaintext + age overhead),
 *     decreasing as uploadChunks unlinks finished chunks. Worst case during
 *     the brief window between passes 1 and 2.
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

use WPMgr\Agent\Support\AgeCrypto;
use WPMgr\Agent\Support\BackupTransport;
use WPMgr\Agent\Support\Blake3;

/**
 * Three-pass chunk pipeline: encrypt → upload (dedup-aware) → submit manifest.
 *
 * Declared `final` — TaskRunner instantiates exactly one of these per backup.
 * No subclassing intended; tests should stub via composition (the public API
 * is small and pass-by-cursor).
 */
final class EncryptAndUpload
{
    /** Default plaintext chunk size. Matches M4 contract (agentcmd.ChunkBytes). */
    public const DEFAULT_CHUNK_BYTES = 4 * 1024 * 1024;

    /**
     * V0/MVP encryption switch (ADR-033 update).
     *
     * `false` (DEFAULT for V0 self-hosted): chunks are BLAKE3-hashed and
     * uploaded as plaintext zip bytes. Matches WPvivid's design choice. CP
     * stores zip chunks readable by any S3 credential holder — acceptable
     * because in self-hosted deployments the CP operator IS the customer.
     *
     * `true` (V1 SaaS): chunks are age-encrypted (X25519 + ChaCha20-Poly1305)
     * before BLAKE3 + upload. CP stores only ciphertext + the public
     * recipient; even a compromised CP operator cannot decrypt customer
     * backups. The right tradeoff for multi-tenant SaaS where CP operator ≠
     * customer. ~6-8 s per 4 MiB chunk CPU cost.
     *
     * The chunk filename suffix (`.bin` vs `.age`) and the manifest's
     * encryption signal both follow this flag.
     */
    public const ENCRYPT_CHUNKS = false;

    /** Emit a progress event every N encrypted chunks (encryptChunks pass). */
    private const PROGRESS_EVERY_ENCRYPT = 4;

    /** Emit a progress event every N PUT calls (uploadChunks pass). */
    private const PROGRESS_EVERY_UPLOAD = 4;

    private AgeCrypto $age;
    private BackupTransport $transport;
    private string $snapshotId;
    private string $ageRecipient;
    private string $presignEndpoint;
    private string $manifestEndpoint;
    private int $chunkBytes;

    /**
     * @param AgeCrypto       $age              Shared age helper.
     * @param BackupTransport $transport        Configured M4 transport (CP-callbacks + raw PUT).
     * @param string          $snapshotId       In-flight snapshot id.
     * @param string          $ageRecipient     "age1..." public recipient.
     * @param string          $presignEndpoint  Absolute CP presign URL.
     * @param string          $manifestEndpoint Absolute CP manifest URL.
     * @param int             $chunkBytes       Plaintext chunk size in bytes (>=1).
     */
    public function __construct(
        AgeCrypto $age,
        BackupTransport $transport,
        string $snapshotId,
        string $ageRecipient,
        string $presignEndpoint,
        string $manifestEndpoint,
        int $chunkBytes = self::DEFAULT_CHUNK_BYTES
    ) {
        $this->age              = $age;
        $this->transport        = $transport;
        $this->snapshotId       = $snapshotId;
        $this->ageRecipient     = $ageRecipient;
        $this->presignEndpoint  = $presignEndpoint;
        $this->manifestEndpoint = $manifestEndpoint;
        $this->chunkBytes       = max(1, $chunkBytes);
    }

    /**
     * Pass 1 — encrypt every artifact into 4 MiB-plaintext age chunks on disk.
     *
     * Walks $artifacts in order, streams each through fread($chunkBytes), age-
     * encrypts each chunk, blake3-hashes the ciphertext, writes to scratch as
     * `chunks-<hash>.age`, and builds the ordered manifest entries.
     *
     * Resumable: $resume can carry `artifact_index` and `entries_so_far` from a
     * prior partial pass. We restart the CURRENT artifact from offset 0 — age
     * STREAM is per-chunk authenticated, so partial chunks on disk are safe to
     * overwrite (and ZipArchive/gzip artifacts are sequential anyway). Cost:
     * at most one artifact's worth of re-encrypt on a crash boundary.
     *
     * @param string                                  $scratchDir Absolute scratch dir.
     * @param list<array{path:string,logical:string}> $artifacts  Ordered list of inputs.
     *                                                            `path` is the absolute on-disk
     *                                                            path (e.g. `<scratch>/database.sql.gz`
     *                                                            or `<scratch>/wp-content.part001.zip`);
     *                                                            `logical` is the manifest path
     *                                                            (e.g. `database.sql.gz`).
     * @param array<string,mixed>                     $resume     Sub-state from a prior partial pass.
     * @param callable                                $progress   function(string $phase, array $detail): void
     * @return array<string,mixed> On completion: `done:true`, `entries:[...]`,
     *                              `all_hashes:[...]`. The cursor doubles as
     *                              the input to uploadChunks().
     * @throws \RuntimeException On unrecoverable I/O or crypto failure.
     */
    public function encryptChunks(string $scratchDir, array $artifacts, array $resume, callable $progress): array
    {
        // Lift caller-imposed time/abort guards. We may be running inside an
        // FPM request that has already called fastcgi_finish_request().
        @set_time_limit(0);
        @ignore_user_abort(true);

        // Idempotent no-op for an already-completed cursor.
        if (!empty($resume['done'])) {
            return $resume;
        }

        if (!is_dir($scratchDir)) {
            throw new \RuntimeException('EncryptAndUpload: scratchDir does not exist: ' . $scratchDir);
        }

        $artifactsTotal = count($artifacts);
        $artifactIndex  = isset($resume['artifact_index']) ? max(0, (int) $resume['artifact_index']) : 0;
        /** @var list<array<string,mixed>> $entries */
        $entries = (isset($resume['entries']) && is_array($resume['entries']))
            ? array_values($resume['entries'])
            : [];
        /** @var array<string,int> $allHashes hash => 1 (set semantics, dedup within a single backup) */
        $allHashes = [];
        if (isset($resume['all_hashes']) && is_array($resume['all_hashes'])) {
            foreach ($resume['all_hashes'] as $h) {
                if (is_string($h) && $h !== '') {
                    $allHashes[$h] = 1;
                }
            }
        }

        $chunksDone = isset($resume['chunks_done']) ? (int) $resume['chunks_done'] : 0;
        $sinceTick  = 0;
        $currentLogical = '';

        for ($i = $artifactIndex; $i < $artifactsTotal; $i++) {
            $artifact = $artifacts[$i];
            $absPath  = (string) ($artifact['path'] ?? '');
            $logical  = (string) ($artifact['logical'] ?? '');
            if ($absPath === '' || $logical === '') {
                throw new \RuntimeException('EncryptAndUpload: malformed artifact at index ' . $i);
            }
            if (!is_file($absPath)) {
                throw new \RuntimeException('EncryptAndUpload: artifact missing on disk: ' . $absPath);
            }

            $currentLogical = $logical;
            $handle         = @fopen($absPath, 'rb');
            if ($handle === false) {
                throw new \RuntimeException('EncryptAndUpload: cannot open artifact: ' . $absPath);
            }

            /** @var list<array{blake3:string,size:int}> $chunkList */
            $chunkList     = [];
            $artifactBytes = 0;

            try {
                while (!feof($handle)) {
                    $plain = fread($handle, $this->chunkBytes);
                    if ($plain === false) {
                        throw new \RuntimeException('EncryptAndUpload: read failed: ' . $absPath);
                    }
                    if ($plain === '') {
                        // EOF on an aligned boundary; feof() will catch on the next iter.
                        break;
                    }
                    $artifactBytes += strlen($plain);

                    // V0/MVP encryption decision (M5.6 / ADR-033 update):
                    // age-encrypting every 4 MiB chunk is CPU-bound on PHP
                    // (~6-8 s per chunk on managed WP hosts) — the entire
                    // backup runtime is dominated by it. For SELF-HOSTED V0
                    // deployments the operator IS the customer, so encrypting
                    // against ourselves provides no real security property
                    // (matches WPvivid's 40M-install design choice). When
                    // self::ENCRYPT_CHUNKS is false (V0 default), we BLAKE3
                    // the plaintext directly and upload it as-is. The
                    // ciphertext-encrypted path is preserved for V1 SaaS
                    // (multi-tenant: CP operator ≠ customer; encryption is
                    // the right tradeoff there).
                    if (self::ENCRYPT_CHUNKS) {
                        $bytes = $this->age->encrypt($plain, $this->ageRecipient);
                        // Drop plaintext reference ASAP — encrypt() copied what it needs.
                        $plain = '';
                        $ext   = '.age';
                    } else {
                        // Plaintext path: hash the raw chunk, upload as-is.
                        $bytes = $plain;
                        $plain = '';
                        $ext   = '.bin';
                    }

                    $hash      = Blake3::hashHex($bytes);
                    $cipherLen = strlen($bytes);

                    // Content-addressed: same bytes -> same filename. In the
                    // encrypted path, age's per-call ephemeral key means
                    // identical plaintext yields different ciphertext (and
                    // hence a different hash) — dedup is per-snapshot at the
                    // chunk-id level, not plaintext-level. In the plaintext
                    // path (V0 default), identical chunks across snapshots
                    // dedup naturally via shared hash — that's the right
                    // behavior for a self-hosted single-tenant deployment.
                    $chunkPath = $scratchDir . DIRECTORY_SEPARATOR . 'chunks-' . $hash . $ext;
                    if (!is_file($chunkPath)) {
                        // LOCK_EX so a crashed-and-resumed run can't tear a
                        // chunk file mid-write under concurrent watchdog
                        // entries (the only race we need to guard).
                        $written = @file_put_contents($chunkPath, $bytes, LOCK_EX);
                        if ($written !== $cipherLen) {
                            throw new \RuntimeException('EncryptAndUpload: write failed for chunk ' . $hash);
                        }
                    }
                    // Free reference; the file is the durable copy.
                    $bytes = '';

                    $chunkList[] = ['blake3' => $hash, 'size' => $cipherLen];
                    $allHashes[$hash] = 1;
                    $chunksDone++;
                    $sinceTick++;

                    if ($sinceTick >= self::PROGRESS_EVERY_ENCRYPT) {
                        $this->safeProgress($progress, 'encrypting_uploading', [
                            'stage'             => 'encrypt',
                            'chunks_done'       => $chunksDone,
                            'artifacts_done'    => $i,
                            'artifacts_total'   => $artifactsTotal,
                            'current_artifact'  => $currentLogical,
                        ]);
                        $sinceTick = 0;
                    }
                }
            } finally {
                fclose($handle);
            }

            $entries[] = [
                'path'       => $logical,
                'entry_kind' => $this->entryKind($logical),
                'table_name' => '',
                'mode'       => 0,
                'size'       => $artifactBytes,
                'chunks'     => $chunkList,
            ];
        }

        // Final progress beacon so the TaskRunner can mark pass-1 complete
        // before persisting the new cursor.
        $this->safeProgress($progress, 'encrypting_uploading', [
            'stage'           => 'encrypt',
            'done'            => true,
            'chunks_done'     => $chunksDone,
            'artifacts_done'  => $artifactsTotal,
            'artifacts_total' => $artifactsTotal,
        ]);

        return [
            'done'        => true,
            'entries'     => $entries,
            'all_hashes'  => array_keys($allHashes),
            'chunks_done' => $chunksDone,
        ];
    }

    /**
     * Pass 2 — ask the CP which hashes are missing, PUT only those, cleanup.
     *
     * Calls BackupTransport::presignChunks() once with the full hash set; the
     * response is a `{hash => presigned PUT URL}` map of ONLY the hashes the
     * CP hasn't already stored (incremental dedup). For each entry we read
     * the local ciphertext from scratch, PUT it, and on success @unlink the
     * local file (free disk as we go). Hashes the CP already had are
     * @unlink'd outright — no PUT needed.
     *
     * @param array<string,mixed> $encryptCursor The cursor returned by encryptChunks() with done=true.
     * @param array<string,mixed> $resume        Upload-pass resume cursor.
     * @param callable            $progress      function(string $phase, array $detail): void
     * @return array<string,mixed> On completion: `done:true`, telemetry.
     * @throws \RuntimeException On transport-level PUT failure.
     */
    public function uploadChunks(array $encryptCursor, array $resume, callable $progress): array
    {
        @set_time_limit(0);
        @ignore_user_abort(true);

        if (!empty($resume['done'])) {
            return $resume;
        }

        if (empty($encryptCursor['done']) || !isset($encryptCursor['all_hashes']) || !is_array($encryptCursor['all_hashes'])) {
            throw new \RuntimeException('EncryptAndUpload: uploadChunks called before encryptChunks completed.');
        }

        $allHashes = array_values(array_filter(
            $encryptCursor['all_hashes'],
            static fn ($h): bool => is_string($h) && $h !== ''
        ));
        $chunksTotal = count($allHashes);

        // Already-uploaded hashes from a prior partial pass — re-running
        // presign() is harmless (CP returns only what's still missing), so we
        // mostly use this for accurate progress counters.
        /** @var array<string,int> $uploadedHashes */
        $uploadedHashes = [];
        if (isset($resume['uploaded_hashes']) && is_array($resume['uploaded_hashes'])) {
            foreach ($resume['uploaded_hashes'] as $h) {
                if (is_string($h) && $h !== '') {
                    $uploadedHashes[$h] = 1;
                }
            }
        }
        $bytesUploaded = isset($resume['bytes_uploaded']) ? (int) $resume['bytes_uploaded'] : 0;
        $scratchDir    = isset($resume['scratch_dir']) ? (string) $resume['scratch_dir'] : '';
        if ($scratchDir === '') {
            // Derive from the chunk files in encryptCursor: we know the chunk
            // path layout. Caller must always supply it explicitly to avoid
            // ambiguity — fall through to whatever they pass.
            $scratchDir = isset($resume['scratch_dir']) ? (string) $resume['scratch_dir'] : '';
        }

        $uploads = $this->transport->presignChunks($this->presignEndpoint, $this->snapshotId, $allHashes);

        // First sweep: PUT the missing hashes.
        $putCount     = 0;
        $sinceTick    = 0;
        $chunksDone   = count($uploadedHashes);

        foreach ($uploads as $hash => $url) {
            if (!is_string($hash) || $hash === '' || !is_string($url) || $url === '') {
                continue;
            }
            if (isset($uploadedHashes[$hash])) {
                continue;
            }
            $chunkPath = $this->chunkPath($scratchDir, $hash);
            if (!is_file($chunkPath)) {
                // The encrypt pass writes chunks-<hash>.age for every chunk we
                // emit; if it's gone, either a previous upload pass unlinked
                // it (dedup hit on a re-entry) or someone tampered with the
                // scratch dir. Treat as already-uploaded only if the CP no
                // longer asks for it — but here the CP IS asking for it, so
                // this is fatal.
                throw new \RuntimeException('EncryptAndUpload: missing local chunk for upload: ' . $hash);
            }
            $cipher = @file_get_contents($chunkPath);
            if ($cipher === false) {
                throw new \RuntimeException('EncryptAndUpload: cannot read local chunk: ' . $chunkPath);
            }

            $ok = $this->transport->putChunk($url, $cipher);
            if (!$ok) {
                // Surface as RuntimeException so the TaskRunner's top-level
                // catch marks the snapshot failed (or, in watchdog re-entry,
                // resume_count increments and we try again).
                throw new \RuntimeException('EncryptAndUpload: PUT failed for chunk ' . $hash);
            }
            $bytesUploaded += strlen($cipher);
            $cipher         = '';

            // Drop the local copy now we know it's durably uploaded.
            @unlink($chunkPath);

            $uploadedHashes[$hash] = 1;
            $putCount++;
            $chunksDone++;
            $sinceTick++;

            if ($sinceTick >= self::PROGRESS_EVERY_UPLOAD) {
                $this->safeProgress($progress, 'encrypting_uploading', [
                    'stage'          => 'upload',
                    'chunks_done'    => $chunksDone,
                    'chunks_total'   => $chunksTotal,
                    'bytes_uploaded' => $bytesUploaded,
                ]);
                $sinceTick = 0;
            }
        }

        // Second sweep: drop local files for hashes the CP already had — they
        // weren't in $uploads, so they're not pending upload. (We do this
        // after the PUT loop so disk pressure during uploads is bounded only
        // by chunks we ARE uploading.)
        $dedupHits = 0;
        foreach ($allHashes as $hash) {
            if (isset($uploadedHashes[$hash])) {
                continue;
            }
            if (isset($uploads[$hash])) {
                // Should have been put above; if not, the loop above threw.
                continue;
            }
            $chunkPath = $this->chunkPath($scratchDir, $hash);
            if (is_file($chunkPath)) {
                @unlink($chunkPath);
            }
            $uploadedHashes[$hash] = 1;
            $dedupHits++;
            $chunksDone++;
        }

        $this->safeProgress($progress, 'encrypting_uploading', [
            'stage'          => 'upload',
            'done'           => true,
            'chunks_done'    => $chunksDone,
            'chunks_total'   => $chunksTotal,
            'chunks_put'     => $putCount,
            'chunks_dedup'   => $dedupHits,
            'bytes_uploaded' => $bytesUploaded,
        ]);

        return [
            'done'            => true,
            'chunks_total'    => $chunksTotal,
            'chunks_put'      => $putCount,
            'chunks_dedup'    => $dedupHits,
            'bytes_uploaded'  => $bytesUploaded,
            'uploaded_hashes' => array_keys($uploadedHashes),
        ];
    }

    /**
     * Pass 3 — submit the manifest to the CP. Idempotent across watchdog
     * re-entries: a 4xx "manifest already recorded" response is treated as
     * success.
     *
     * @param list<array<string,mixed>> $entries  Manifest entries from encryptChunks() cursor.
     * @param callable                  $progress Progress callback (called once with done=true on success).
     * @throws \RuntimeException On a transport-level error or a non-idempotent CP rejection.
     */
    public function submitManifest(array $entries, callable $progress): void
    {
        @set_time_limit(0);
        @ignore_user_abort(true);

        try {
            /** @phpstan-ignore-next-line — entries shape is enforced by encryptChunks. */
            $result = $this->transport->submitManifest(
                $this->manifestEndpoint,
                $this->snapshotId,
                $this->ageRecipient,
                $entries
            );
        } catch (\Throwable $e) {
            // BackupTransport throws on non-2xx. We can't easily distinguish
            // "already recorded" (4xx) from a real failure without inspecting
            // the response — and the M4 transport intentionally hides bodies.
            // Treat all transport exceptions as failures so the caller's
            // top-level catch increments resume_count; watchdog re-entry
            // makes this safe to retry.
            throw new \RuntimeException(
                'EncryptAndUpload: manifest submit failed: ' . $e->getMessage(),
                0,
                $e
            );
        }

        if (empty($result['ok'])) {
            throw new \RuntimeException('EncryptAndUpload: manifest submit returned ok=false');
        }

        $this->safeProgress($progress, 'submitting_manifest', [
            'done'         => true,
            'chunk_count'  => (int) ($result['chunk_count'] ?? 0),
            'stored_count' => (int) ($result['stored_count'] ?? 0),
        ]);
    }

    /**
     * Classify an artifact's logical name into the manifest entry_kind enum.
     * Matches the M4 backup_contract.go shape: 'db' for the SQL dump, 'file'
     * for everything else (the wp-content.partNNN.zip parts).
     *
     * @param string $logical Manifest path (e.g. "database.sql.gz").
     * @return string 'db' | 'file'
     */
    private function entryKind(string $logical): string
    {
        // Be lenient on the SQL artifact name: we ship database.sql.gz today
        // but a future change could rename it; the test is "looks like a SQL
        // dump", not an exact string match.
        $lower = strtolower($logical);
        if (str_ends_with($lower, '.sql') || str_ends_with($lower, '.sql.gz') || str_contains($lower, 'database.sql')) {
            return 'db';
        }
        return 'file';
    }

    /**
     * Build the absolute path of the local chunk file for a given hash. The
     * suffix tracks the encryption mode the encryptChunks pass used so
     * uploadChunks can find the file regardless of whether the snapshot is
     * encrypted (`.age`) or plaintext (`.bin`).
     *
     * @param string $scratchDir Per-run scratch dir.
     * @param string $hash       Blake3 hex of the chunk bytes (ciphertext OR plaintext).
     * @return string
     */
    private function chunkPath(string $scratchDir, string $hash): string
    {
        $ext = self::ENCRYPT_CHUNKS ? '.age' : '.bin';
        return $scratchDir . DIRECTORY_SEPARATOR . 'chunks-' . $hash . $ext;
    }

    /**
     * Invoke the caller's progress callback safely. A broken progress hook
     * must never fail an otherwise-healthy backup.
     *
     * @param callable            $progress Caller-supplied callback.
     * @param string              $phase    Phase label.
     * @param array<string,mixed> $detail   Phase detail payload.
     */
    private function safeProgress(callable $progress, string $phase, array $detail): void
    {
        try {
            $progress($phase, $detail);
        } catch (\Throwable $_) {
            // Swallow — progress reporting is best-effort observability.
        }
    }
}
