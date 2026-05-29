<?php
/**
 * Backup destination contract — ADR-036 P1 storage-adapter foundation.
 *
 * A "destination" abstracts WHERE the agent ships encrypted chunks. WPvivid's
 * decade-old Remote_Storage interface (`Wpvivid_Remote::upload()` / `download()`
 * / `delete()`) is the prior art we're modelling after. The motivation is the
 * same: a single backup pipeline that doesn't care whether bytes land in:
 *
 *   - the WPMgr control-plane bucket (current behaviour — `cp`),
 *   - a folder on the same webserver (V1 self-hosted — `local`),
 *   - a customer-owned S3-compatible bucket (V1 BYO — `s3_compat`).
 *
 * The CP and S3-compat kinds use the same wire path: CP-issued presigned PUT
 * URLs hide the actual endpoint from the agent, so the agent never holds a
 * customer's S3 credentials. The `local` kind side-steps the wire entirely and
 * writes ciphertext to `wp-content/wpmgr-backups/<snapshot>/` with deny-by-
 * default web-server config alongside the chunks.
 *
 * @package WPMgr\Agent\Backup\Destinations
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup\Destinations;

/**
 * Adapter contract every destination implementation satisfies. The methods
 * mirror the three-pass `EncryptAndUpload` pipeline so the upper layer can call
 * into the destination without knowing which storage backend is active.
 *
 * Implementations are constructed by Destination_Resolver from the
 * `destination_kind` + `destination_config` fields on the BackupRequest. They
 * must be stateless aside from per-snapshot scratch state and must NOT log
 * presigned URLs, customer credentials, or local paths beyond what is already
 * surfaced by the existing transport layer.
 */
interface BackupDestination
{
    /**
     * Prepare per-snapshot state on the destination side. Must be idempotent
     * across watchdog re-entries (the runner may call this on every resume).
     * Local destinations create the chunks dir + deny-by-default config files;
     * CP / s3_compat destinations are no-ops (the CP records the snapshot in
     * its own DB when the backup is enqueued).
     *
     * @param string $snapshotId UUID of the in-flight snapshot.
     */
    public function prepare(string $snapshotId): void;

    /**
     * Store one ciphertext chunk. The hash is the BLAKE3 of the bytes the
     * caller already computed (content-addressed). Returns true on success;
     * caller is expected to retry on false via the existing TaskRunner resume
     * loop.
     *
     * @param string $hash       BLAKE3 hex digest of $ciphertext.
     * @param string $ciphertext Chunk bytes (encrypted or plaintext per
     *                           EncryptAndUpload::ENCRYPT_CHUNKS).
     */
    public function putChunk(string $hash, string $ciphertext): bool;

    /**
     * Read one stored chunk back (used by the restore path). Returns null on
     * any failure — the caller's retry loop decides whether to back off.
     */
    public function getChunk(string $hash): ?string;

    /**
     * Persist the completed manifest. CP-bound destinations POST the signed
     * manifest to the CP's manifest endpoint and return the response shape
     * (ok/chunk_count/stored_count). Local destinations write the manifest
     * JSON next to the chunks AND ship the snapshot-metadata-only manifest to
     * the CP so the operator can still see + manage the snapshot from the UI.
     *
     * @param list<array<string,mixed>> $entries Manifest entries from
     *                                            EncryptAndUpload::encryptChunks.
     * @param array<string,mixed>       $meta    Caller metadata
     *                                            (snapshot_id, age_recipient,
     *                                            manifest_endpoint, etc.).
     * @return array{ok:bool,chunk_count:int,stored_count:int}
     */
    public function submitManifest(array $entries, array $meta): array;

    /**
     * Delete the given chunk hashes from the destination — used during GC of
     * snapshots whose retention expired. Best-effort; missing chunks are not
     * an error (idempotent delete semantics).
     *
     * @param list<string> $hashes BLAKE3 hex digests to remove.
     */
    public function deleteChunks(array $hashes): void;

    /**
     * Identifier for telemetry / audit: "cp" | "local" | "s3_compat".
     */
    public function getKind(): string;
}
