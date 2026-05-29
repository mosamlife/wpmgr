<?php
/**
 * CP destination — ADR-036 P1.
 *
 * This is the legacy, unchanged-behaviour path: chunks are shipped to whatever
 * presigned PUT URLs the control plane mints for them. The CP is free to back
 * those URLs with its own bucket (the original `cp` kind) or a customer-
 * managed bucket (the `s3_compat` kind — CP holds the credentials, agent
 * never sees them). Either way, the agent's code path is identical, which is
 * exactly the property we want from the adapter abstraction.
 *
 * This adapter is a thin wrapper over `BackupTransport`: each call delegates
 * straight through so we keep bit-for-bit compatibility with the 0.9.6
 * pipeline. The watchdog, dedup, manifest idempotency, etc. all still live in
 * EncryptAndUpload — the destination is purely a "where do bytes go" decision.
 *
 * @package WPMgr\Agent\Backup\Destinations
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup\Destinations;

use WPMgr\Agent\Support\BackupTransport;

/**
 * Default destination: chunks go to CP-presigned PUT URLs (which in turn point
 * at the CP-global bucket OR a customer-owned S3 bucket, transparently).
 */
final class CpDestination implements BackupDestination
{
    private BackupTransport $transport;
    private string $snapshotId;
    private string $presignEndpoint;
    private string $manifestEndpoint;
    private string $kind;

    /**
     * @param BackupTransport $transport        Signed-transport singleton.
     * @param string          $snapshotId       In-flight snapshot UUID.
     * @param string          $presignEndpoint  CP /presign callback URL.
     * @param string          $manifestEndpoint CP /manifest callback URL.
     * @param string          $kind             "cp" or "s3_compat" — surfaces
     *                                          in audit/telemetry; the wire
     *                                          path is identical either way.
     */
    public function __construct(
        BackupTransport $transport,
        string $snapshotId,
        string $presignEndpoint,
        string $manifestEndpoint,
        string $kind = 'cp'
    ) {
        $this->transport        = $transport;
        $this->snapshotId       = $snapshotId;
        $this->presignEndpoint  = $presignEndpoint;
        $this->manifestEndpoint = $manifestEndpoint;
        // Both "cp" and "s3_compat" share the presigned-URL wire — guard the
        // value so audit doesn't grow surprise destinations.
        $this->kind = $kind === 's3_compat' ? 's3_compat' : 'cp';
    }

    public function prepare(string $snapshotId): void
    {
        // CP-bound destinations need no agent-side prep: the CP recorded the
        // snapshot row when the backup was enqueued.
    }

    /**
     * Ask the CP for a presigned PUT URL for the one hash and upload it. We do
     * a single-hash presign here so the destination interface stays uniform
     * across local / cp / s3_compat. The bulk-presign optimisation
     * EncryptAndUpload uses today is preserved because EncryptAndUpload
     * branches on getKind() === 'cp' and short-circuits to the legacy code
     * path (see class-encrypt-and-upload.php). This method is the fallback
     * single-shot path used only for one-off re-uploads.
     */
    public function putChunk(string $hash, string $ciphertext): bool
    {
        $uploads = $this->transport->presignChunks(
            $this->presignEndpoint,
            $this->snapshotId,
            [$hash]
        );
        if (!isset($uploads[$hash])) {
            // CP says it already has this chunk (dedup hit). Treat as success.
            return true;
        }
        return $this->transport->putChunk($uploads[$hash], $ciphertext);
    }

    public function getChunk(string $hash): ?string
    {
        // For the CP/s3_compat path, the restore engine drives chunk reads via
        // CP-presigned GET URLs from the restore plan, not via this method.
        // Returning null here keeps the interface honest without misleading
        // callers into thinking we can synthesize a GET URL out of band.
        return null;
    }

    public function submitManifest(array $entries, array $meta): array
    {
        $ageRecipient = isset($meta['age_recipient']) && is_string($meta['age_recipient'])
            ? $meta['age_recipient']
            : '';

        return $this->transport->submitManifest(
            $this->manifestEndpoint,
            $this->snapshotId,
            $ageRecipient,
            // @phpstan-ignore-next-line — manifest entries shape is enforced upstream.
            $entries
        );
    }

    public function deleteChunks(array $hashes): void
    {
        // GC for CP-bound destinations runs CP-side: the retention worker
        // refcounts chunk rows in Postgres and the GC worker deletes any chunk
        // whose refcount hit zero. The agent has nothing to do here.
        unset($hashes);
    }

    public function getKind(): string
    {
        return $this->kind;
    }

    /**
     * Bulk-presign helper exposed for EncryptAndUpload's optimised path. Lets
     * the encrypt-and-upload pass keep its single round-trip to the CP for the
     * full hash list instead of one presign call per chunk.
     *
     * @param list<string> $hashes Candidate chunk hashes (full set).
     * @return array<string,string> hash => presigned PUT URL.
     */
    public function presignAll(array $hashes): array
    {
        return $this->transport->presignChunks(
            $this->presignEndpoint,
            $this->snapshotId,
            $hashes
        );
    }

    /**
     * Raw PUT to a presigned URL. Companion to presignAll() so the encrypt
     * pipeline can drive uploads without going through putChunk() (which
     * re-issues a presign for one hash).
     */
    public function putPresigned(string $presignedUrl, string $ciphertext): bool
    {
        return $this->transport->putChunk($presignedUrl, $ciphertext);
    }
}
