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

use WPMgr\Agent\Backup\Destinations\BackupDestination;
use WPMgr\Agent\Backup\Destinations\CpDestination;
use WPMgr\Agent\Backup\Destinations\DestinationResolver;
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
     * Destination adapter resolved at construction (ADR-036 P1). Routes
     * chunk uploads + manifest submission to the right backend (CP, local
     * folder, or customer-owned S3). Always non-null after the ctor; defaults
     * to a CpDestination so any caller that hasn't yet learned about the
     * destination_kind plumbing keeps the legacy bit-for-bit behaviour.
     */
    private BackupDestination $destination;

    /**
     * @param AgeCrypto             $age               Shared age helper.
     * @param BackupTransport       $transport         Configured M4 transport (CP-callbacks + raw PUT).
     * @param string                $snapshotId        In-flight snapshot id.
     * @param string                $ageRecipient      "age1..." public recipient.
     * @param string                $presignEndpoint   Absolute CP presign URL.
     * @param string                $manifestEndpoint  Absolute CP manifest URL.
     * @param int                   $chunkBytes        Plaintext chunk size in bytes (>=1).
     * @param array<string,mixed>|null $destinationParams Optional destination
     *   selector for ADR-036 P1. Shape: `['destination_kind' => 'cp'|'local'|'s3_compat',
     *   'destination_config' => [...]]` plus the standard runner params (snapshot_id,
     *   presign_endpoint, manifest_endpoint — supplied automatically below). When
     *   null, the destination defaults to `cp`, preserving the 0.9.6 pipeline.
     */
    public function __construct(
        AgeCrypto $age,
        BackupTransport $transport,
        string $snapshotId,
        string $ageRecipient,
        string $presignEndpoint,
        string $manifestEndpoint,
        int $chunkBytes = self::DEFAULT_CHUNK_BYTES,
        ?array $destinationParams = null
    ) {
        $this->age              = $age;
        $this->transport        = $transport;
        $this->snapshotId       = $snapshotId;
        $this->ageRecipient     = $ageRecipient;
        $this->presignEndpoint  = $presignEndpoint;
        $this->manifestEndpoint = $manifestEndpoint;
        $this->chunkBytes       = max(1, $chunkBytes);

        // Resolve the destination adapter. The resolver needs the runner
        // params it would normally read from sub_state; we synthesize the
        // minimal subset here so the legacy 'cp' default still works without
        // any caller plumbing the new fields through.
        $resolverParams = is_array($destinationParams) ? $destinationParams : [];
        $resolverParams['snapshot_id']       = $snapshotId;
        $resolverParams['presign_endpoint']  = $presignEndpoint;
        $resolverParams['manifest_endpoint'] = $manifestEndpoint;
        if (!isset($resolverParams['destination_kind']) || !is_string($resolverParams['destination_kind']) || $resolverParams['destination_kind'] === '') {
            $resolverParams['destination_kind'] = 'cp';
        }
        $this->destination = DestinationResolver::resolve($resolverParams, $transport);
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

        // Inspection pass-0: walk the dump file BEFORE chunking and emit a
        // sql-inspection.json manifest entry that the CP consumes as the
        // restore-safety preflight. See class-sql-inspector.php for the
        // why; in short, this lets the CP refuse to restore a snapshot
        // whose dump doesn't look like a WordPress site (or whose siteurl
        // points at the wrong host) BEFORE it touches the customer DB.
        //
        // Runs once per encryptChunks() invocation. On a watchdog re-entry
        // (when $resume['done'] is unset but partial cursors exist), we
        // re-run the inspector — it's cheap relative to the encrypt pass,
        // and re-running guarantees the report on disk matches the dump
        // we're about to ship even if the dump was rewritten during a
        // retry. file_exists() short-circuits the on-disk write so we
        // don't churn the file unnecessarily.
        $artifacts = $this->maybeInjectInspectionArtifact($scratchDir, $artifacts, $progress);

        // ADR-037 Sprint 1, 1D — environment fingerprint. Mirrors the SQL
        // inspection wiring exactly: a synthetic environment.json artifact
        // appended after the dump+files+inspection, classified entry_kind=
        // 'environment' so the CP can locate it the same way it locates the
        // sql-inspection.json today. Carries PHP/MySQL/WP version, URLs,
        // file + table counts, plugin/theme slugs, table names, total size
        // — everything an operator needs to answer "what environment was
        // this snapshot taken from?" without restoring the dump.
        $artifacts = $this->maybeInjectEnvironmentArtifact($scratchDir, $artifacts, $progress);

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

        // ADR-036 P1 storage adapter: branch on destination kind. CP / s3_compat
        // share the legacy bulk-presign + PUT path (no behavioural change for
        // self-hosted v0 deployments). The "local" destination writes chunks
        // straight to disk via the adapter — no presign round-trip.
        $kind = $this->destination->getKind();

        $putCount     = 0;
        $sinceTick    = 0;
        $chunksDone   = count($uploadedHashes);
        $dedupHits    = 0;

        if ($kind === 'local') {
            // Local destination: every chunk we just encrypted needs to land
            // in the local backups dir. Dedup is filesystem-level (the
            // destination's putChunk no-ops when the file already exists).
            $this->destination->prepare($this->snapshotId);
            $uploads = [];
            foreach ($allHashes as $hash) {
                if (isset($uploadedHashes[$hash])) {
                    continue;
                }
                $chunkPath = $this->chunkPath($scratchDir, $hash);
                if (!is_file($chunkPath)) {
                    throw new \RuntimeException('EncryptAndUpload: missing local chunk for local-destination write: ' . $hash);
                }
                $cipher = @file_get_contents($chunkPath);
                if ($cipher === false) {
                    throw new \RuntimeException('EncryptAndUpload: cannot read local chunk: ' . $chunkPath);
                }
                if (!$this->destination->putChunk($hash, $cipher)) {
                    throw new \RuntimeException('EncryptAndUpload: local-destination write failed for chunk ' . $hash);
                }
                $bytesUploaded += strlen($cipher);
                $cipher = '';
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
        } else {
            // CP / s3_compat: legacy bulk-presign + PUT. Both kinds talk to
            // the CP callback identically — only audit/telemetry differ.
            $uploads = [];
            if ($this->destination instanceof CpDestination) {
                $uploads = $this->destination->presignAll($allHashes);
            } else {
                // Defensive fallback: fall through to the raw transport. Should
                // never hit in practice (CP / s3_compat both build a
                // CpDestination via the resolver).
                $uploads = $this->transport->presignChunks($this->presignEndpoint, $this->snapshotId, $allHashes);
            }

            // First sweep: PUT the missing hashes.
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

                $ok = ($this->destination instanceof CpDestination)
                    ? $this->destination->putPresigned($url, $cipher)
                    : $this->transport->putChunk($url, $cipher);
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
            // ADR-036 P1 storage adapter: route through the destination so
            // 'local' can also drop a manifest.json next to the chunks while
            // still ACKing the CP. 'cp' / 's3_compat' fall straight through to
            // BackupTransport::submitManifest, preserving legacy behaviour.
            /** @phpstan-ignore-next-line — entries shape is enforced by encryptChunks. */
            $result = $this->destination->submitManifest($entries, [
                'snapshot_id'   => $this->snapshotId,
                'age_recipient' => $this->ageRecipient,
            ]);
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
     *
     * Track 5 (0.9.6) — per-component split. The set is now:
     *   'db'         — SQL dump (database.sql.gz).
     *   'inspection' — agent-side restore-safety preflight JSON.
     *   'plugin'     — plugins.partNNN.zip (FilesArchiver `plugins` bucket).
     *   'theme'      — themes.partNNN.zip (FilesArchiver `themes` bucket).
     *   'upload'     — uploads.partNNN.zip (FilesArchiver `uploads` bucket).
     *   'wp-content' — wp-content.partNNN.zip (catch-all bucket: mu-plugins,
     *                  languages, drop-ins, custom dirs — NOT plugins/themes/
     *                  uploads).
     *   'file'       — UNUSED for fresh snapshots; legacy fallback only.
     *                  Pre-0.9.6 snapshots have entry_kind='file' on their
     *                  rotating wp-content.partNNN.zip sequence; the CP and
     *                  restorer both recognize it and route via the old
     *                  whole-wp-content swap path.
     *
     * The classifier maps `<component>.partNNN.zip` filenames produced by
     * FilesArchiver to their components by filename-prefix match. This keeps
     * the contract with the task-runner unchanged: the runner just passes
     * the part filename as the logical path; the kind falls out here.
     *
     * @param string $logical Manifest path (e.g. "plugins.part001.zip",
     *                        "database.sql.gz", "sql-inspection.json").
     * @return string 'db' | 'inspection' | 'plugin' | 'theme' | 'upload' |
     *                'wp-content' | 'file'
     */
    private function entryKind(string $logical): string
    {
        $lower = strtolower($logical);
        // Inspection report — recognised by the literal manifest path.
        // entry_kind is a free-form string on the CP side (no DB-level
        // enum), so we can ship 'inspection' without a migration.
        if ($lower === 'sql-inspection.json') {
            return 'inspection';
        }
        // ADR-037 Sprint 1, 1D — environment fingerprint. Same convention as
        // 'inspection': literal logical-path match maps to a free-form
        // entry_kind the CP recognises by string equality. No CP migration
        // needed; the snapshot detail endpoint surfaces this entry by name.
        if ($lower === 'environment.json') {
            return 'environment';
        }
        // Be lenient on the SQL artifact name: we ship database.sql.gz today
        // but a future change could rename it; the test is "looks like a SQL
        // dump", not an exact string match.
        if (str_ends_with($lower, '.sql') || str_ends_with($lower, '.sql.gz') || str_contains($lower, 'database.sql')) {
            return 'db';
        }
        // Track 5 per-component archives. The FilesArchiver emits
        // `<component>.partNNN.zip`; classify by filename prefix.
        // Order matters: 'wp-content' MUST come last because the others are
        // strict prefixes that also contain a dot (unambiguous against the
        // catch-all 'wp-content.partNNN.zip').
        if (str_starts_with($lower, 'plugins.part') && str_ends_with($lower, '.zip')) {
            return 'plugin';
        }
        if (str_starts_with($lower, 'themes.part') && str_ends_with($lower, '.zip')) {
            return 'theme';
        }
        if (str_starts_with($lower, 'uploads.part') && str_ends_with($lower, '.zip')) {
            return 'upload';
        }
        if (str_starts_with($lower, 'wp-content.part') && str_ends_with($lower, '.zip')) {
            return 'wp-content';
        }
        // Defensive default. Pre-Track-5 callers that hand a non-typed zip
        // here (and any future artifact whose filename we haven't taught
        // about yet) end up as the legacy 'file' kind — which both CP and
        // restorer still recognize.
        return 'file';
    }

    /**
     * Run the SQL inspector over any DB artifact in $artifacts, write the
     * resulting `sql-inspection.json` to $scratchDir, and append a synthetic
     * artifact entry so the inspection report rides the same encrypt + chunk
     * + upload pipeline as everything else (it ends up in the manifest with
     * entry_kind='inspection', logical_path='sql-inspection.json').
     *
     * No-op (returns $artifacts unchanged) if the artifact list contains no
     * DB dump — e.g. a files-only snapshot.
     *
     * Failures are non-fatal: a parser exception is caught, surfaced via
     * progress as a warning, and the backup continues without the
     * inspection entry. The restore-safety preflight is a defense-in-depth
     * check, not a hard gate; a missing report just means the CP applies
     * its standard checks instead.
     *
     * @param string                                  $scratchDir Absolute scratch dir (same one the chunks land in).
     * @param list<array{path:string,logical:string}> $artifacts  Current artifact list (in order).
     * @param callable                                $progress   Progress callback (used to surface warnings).
     * @return list<array{path:string,logical:string}> Artifact list with the inspection entry appended (or unchanged on no-op).
     */
    private function maybeInjectInspectionArtifact(string $scratchDir, array $artifacts, callable $progress): array
    {
        // Find the DB artifact (there's at most one in V0). We scan the
        // whole list rather than assume index 0 — the assembleArtifacts
        // contract documents DB-before-files, but defensive is cheap.
        $dbPath = '';
        foreach ($artifacts as $a) {
            $absPath = (string) ($a['path'] ?? '');
            $logical = (string) ($a['logical'] ?? '');
            if ($absPath === '' || $logical === '') {
                continue;
            }
            if ($this->entryKind($logical) === 'db') {
                $dbPath = $absPath;
                break;
            }
        }
        if ($dbPath === '' || !is_file($dbPath)) {
            return $artifacts;
        }

        $reportPath = $scratchDir . DIRECTORY_SEPARATOR . 'sql-inspection.json';

        // Idempotency: if a prior pass wrote the report and the file is
        // still on disk, reuse it. This matters on watchdog re-entry where
        // we want to avoid re-scanning a 500 MB dump on every retry.
        if (!is_file($reportPath)) {
            try {
                $inspector = new SqlInspector();
                $report    = $inspector->inspect($dbPath);

                $json = json_encode(
                    $report,
                    JSON_UNESCAPED_SLASHES | JSON_PRETTY_PRINT
                );
                if (!is_string($json)) {
                    throw new \RuntimeException('json_encode returned non-string');
                }

                // LOCK_EX so a concurrent watchdog re-entry can't tear the
                // file mid-write — same rationale as the chunk files.
                $written = @file_put_contents($reportPath, $json, LOCK_EX);
                if ($written !== strlen($json)) {
                    throw new \RuntimeException('write failed for ' . $reportPath);
                }
            } catch (\Throwable $e) {
                // Non-fatal — surface a warning and continue without the
                // inspection entry. The CP applies its standard checks
                // when no agent-side report is present.
                $this->safeProgress($progress, 'encrypting_uploading', [
                    'stage'   => 'inspect',
                    'warning' => 'sql inspection failed: ' . substr($e->getMessage(), 0, 200),
                ]);
                // Best-effort cleanup of a partial file.
                if (is_file($reportPath)) {
                    @unlink($reportPath);
                }
                return $artifacts;
            }
        }

        // Append the synthetic artifact. Ordering: AFTER the DB dump and
        // AFTER any file parts — the inspection report is tiny so its
        // position in the chunk stream doesn't matter for memory, but
        // keeping it LAST makes it trivially identifiable as the manifest
        // tail entry in operator audits.
        $artifacts[] = [
            'path'    => $reportPath,
            'logical' => 'sql-inspection.json',
        ];

        return $artifacts;
    }

    /**
     * ADR-037 Sprint 1, 1D — environment fingerprint synthetic artifact.
     *
     * Builds a JSON document describing the WP/PHP/MySQL environment the
     * snapshot was taken from and appends it as an extra artifact so the
     * encrypt-upload pipeline ships it through the same dedup + chunking
     * path as the dump and file zips. The CP fetches it via the same
     * manifest-chunks-then-concat pattern it uses for sql-inspection.json
     * (see backup/manifest_inspection_fetcher.go) and renders it on the
     * snapshot detail page.
     *
     * The shape mirrors the sql-inspection wiring:
     *   - logical_path = "environment.json"
     *   - entry_kind   = "environment" (free-form CP-side string)
     *   - ships through the SAME encrypt + chunk + upload pipeline; no
     *     special-casing required on either side.
     *
     * Idempotent: if a prior pass wrote the file, we reuse it. Failures are
     * non-fatal — a corrupt env probe just leaves the entry off the manifest.
     *
     * @param string                                  $scratchDir Absolute scratch dir.
     * @param list<array{path:string,logical:string}> $artifacts  Current artifact list.
     * @param callable                                $progress   Progress hook.
     * @return list<array{path:string,logical:string}>
     */
    private function maybeInjectEnvironmentArtifact(string $scratchDir, array $artifacts, callable $progress): array
    {
        $reportPath = $scratchDir . DIRECTORY_SEPARATOR . 'environment.json';

        if (!is_file($reportPath)) {
            try {
                $env = $this->collectEnvironmentFingerprint($artifacts);
                $json = json_encode($env, JSON_UNESCAPED_SLASHES | JSON_PRETTY_PRINT);
                if (!is_string($json)) {
                    throw new \RuntimeException('environment.json: encode failed');
                }
                $written = @file_put_contents($reportPath, $json, LOCK_EX);
                if ($written !== strlen($json)) {
                    throw new \RuntimeException('environment.json: write failed');
                }
            } catch (\Throwable $e) {
                // Non-fatal — operator visibility comes via progress warning.
                $this->safeProgress($progress, 'encrypting_uploading', [
                    'stage'   => 'environment',
                    'warning' => 'env fingerprint failed: ' . substr($e->getMessage(), 0, 200),
                ]);
                if (is_file($reportPath)) {
                    @unlink($reportPath);
                }
                return $artifacts;
            }
        }

        $artifacts[] = [
            'path'    => $reportPath,
            'logical' => 'environment.json',
        ];

        return $artifacts;
    }

    /**
     * Collect the environment-fingerprint payload. Best-effort across WP / PHP /
     * MySQL; missing values fall back to empty strings / zero so the JSON shape
     * stays stable for the CP decoder.
     *
     * @param list<array{path:string,logical:string}> $artifacts
     * @return array<string,mixed>
     */
    private function collectEnvironmentFingerprint(array $artifacts): array
    {
        global $wpdb;

        // --- Versions ---
        $wpVersion    = '';
        if (function_exists('get_bloginfo')) {
            $wpVersion = (string) get_bloginfo('version');
        }
        if ($wpVersion === '' && isset($GLOBALS['wp_version']) && is_scalar($GLOBALS['wp_version'])) {
            $wpVersion = (string) $GLOBALS['wp_version'];
        }
        $mysqlVersion = '';
        if (is_object($wpdb)) {
            try {
                // @phpstan-ignore-next-line
                $mysqlVersion = (string) $wpdb->get_var('SELECT VERSION()');
            } catch (\Throwable $e) {
                // ignore.
            }
        }

        // --- URLs ---
        $siteUrl = function_exists('get_site_url') ? (string) get_site_url() : '';
        $homeUrl = function_exists('get_home_url') ? (string) get_home_url() : '';

        // --- File + table counts ---
        // Files: cap walk at 2s — preflight-style sampling so a 200k-file
        // uploads tree doesn't blow the encrypt-pass deadline.
        $fileCount = 0;
        if (defined('WP_CONTENT_DIR') && is_dir(WP_CONTENT_DIR)) {
            $fileCount = $this->countFilesCapped(WP_CONTENT_DIR, 2);
        }
        // Tables + table names + total DB bytes from SHOW TABLE STATUS.
        $tableNames = [];
        $totalDbBytes = 0;
        if (is_object($wpdb)) {
            try {
                // @phpstan-ignore-next-line
                $rows = $wpdb->get_results('SHOW TABLE STATUS', ARRAY_A);
                if (is_array($rows)) {
                    foreach ($rows as $row) {
                        if (!is_array($row)) {
                            continue;
                        }
                        if (isset($row['Name']) && is_scalar($row['Name'])) {
                            $tableNames[] = (string) $row['Name'];
                        }
                        $totalDbBytes += isset($row['Data_length']) ? (int) $row['Data_length'] : 0;
                        $totalDbBytes += isset($row['Index_length']) ? (int) $row['Index_length'] : 0;
                    }
                }
            } catch (\Throwable $e) {
                // ignore.
            }
        }
        sort($tableNames);

        // --- Plugin + theme slugs ---
        $pluginSlugs = [];
        if (!function_exists('get_plugins') && defined('ABSPATH') && file_exists(ABSPATH . 'wp-admin/includes/plugin.php')) {
            require_once ABSPATH . 'wp-admin/includes/plugin.php';
        }
        if (function_exists('get_plugins')) {
            $all = get_plugins();
            if (is_array($all)) {
                foreach (array_keys($all) as $file) {
                    if (is_string($file) && $file !== '') {
                        // Stem the slug at the first slash (matches WP's plugin slug convention).
                        $parts = explode('/', $file, 2);
                        $pluginSlugs[] = $parts[0];
                    }
                }
            }
        }
        $pluginSlugs = array_values(array_unique($pluginSlugs));
        sort($pluginSlugs);

        $themeSlugs = [];
        if (function_exists('wp_get_themes')) {
            $themes = wp_get_themes();
            if (is_array($themes)) {
                foreach (array_keys($themes) as $stylesheet) {
                    if (is_string($stylesheet) && $stylesheet !== '') {
                        $themeSlugs[] = $stylesheet;
                    }
                }
            }
        }
        $themeSlugs = array_values(array_unique($themeSlugs));
        sort($themeSlugs);

        // --- Total size: DB bytes + sum of on-disk artifact sizes ---
        $totalSize = $totalDbBytes;
        foreach ($artifacts as $a) {
            $absPath = (string) ($a['path'] ?? '');
            if ($absPath !== '' && is_file($absPath)) {
                $totalSize += (int) @filesize($absPath);
            }
        }

        return [
            'schema_version'  => 1,
            'php_version'     => PHP_VERSION,
            'mysql_version'   => $mysqlVersion,
            'wp_version'      => $wpVersion,
            'site_url'        => $siteUrl,
            'home_url'        => $homeUrl,
            'is_multisite'    => function_exists('is_multisite') && is_multisite(),
            'file_count'      => $fileCount,
            'db_table_count'  => count($tableNames),
            'plugin_slugs'    => $pluginSlugs,
            'theme_slugs'     => $themeSlugs,
            'table_names'     => $tableNames,
            'total_size_bytes'=> $totalSize,
            'captured_at'     => gmdate('Y-m-d\TH:i:s\Z'),
        ];
    }

    /**
     * Count files under $path with a wall-clock cap. Same pattern as the
     * preflight estimator — representative sample, not exhaustive.
     */
    private function countFilesCapped(string $path, int $maxSeconds): int
    {
        $count = 0;
        $deadline = microtime(true) + $maxSeconds;
        try {
            $it = new \RecursiveIteratorIterator(
                new \RecursiveDirectoryIterator($path, \FilesystemIterator::SKIP_DOTS | \FilesystemIterator::FOLLOW_SYMLINKS),
                \RecursiveIteratorIterator::LEAVES_ONLY,
                \RecursiveIteratorIterator::CATCH_GET_CHILD
            );
            foreach ($it as $file) {
                if (microtime(true) > $deadline) {
                    break;
                }
                if ($file instanceof \SplFileInfo && $file->isFile()) {
                    $count++;
                }
            }
        } catch (\Throwable $e) {
            // ignore.
        }
        return $count;
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
