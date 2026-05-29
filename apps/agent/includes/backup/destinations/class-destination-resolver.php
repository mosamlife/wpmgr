<?php
/**
 * Destination resolver — ADR-036 P1.
 *
 * Builds the right BackupDestination implementation from the
 * `destination_kind` + `destination_config` fields the CP threads through the
 * BackupRequest. All construction lives behind this static factory so the
 * upper layer (EncryptAndUpload / BackupCommand) stays oblivious to the
 * concrete storage backend.
 *
 * Why `s3_compat` maps to the CP wire: customer-owned S3 destinations are
 * configured CP-side (the operator stores the credentials there, age-
 * encrypted at rest), and the CP mints presigned PUT URLs against the
 * customer's bucket on demand. The agent never holds the customer's S3
 * credentials — same security property as the CP-global bucket today. From
 * the agent's POV, "the URL the CP told me to PUT to" is the only contract,
 * which is exactly the CpDestination path.
 *
 * @package WPMgr\Agent\Backup\Destinations
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup\Destinations;

use WPMgr\Agent\Support\BackupTransport;

/**
 * Static factory: kind + config -> BackupDestination instance.
 */
final class DestinationResolver
{
    /**
     * @param array<string,mixed> $params Runner params (must carry at least
     *                                     `snapshot_id`, `presign_endpoint`,
     *                                     `manifest_endpoint`; may carry
     *                                     `destination_kind` and
     *                                     `destination_config`).
     * @param BackupTransport     $transport Shared signed transport.
     * @return BackupDestination
     */
    public static function resolve(array $params, BackupTransport $transport): BackupDestination
    {
        $snapshotId = isset($params['snapshot_id']) && is_string($params['snapshot_id'])
            ? $params['snapshot_id']
            : '';
        $manifestEp = isset($params['manifest_endpoint']) && is_string($params['manifest_endpoint'])
            ? $params['manifest_endpoint']
            : '';
        $presignEp = isset($params['presign_endpoint']) && is_string($params['presign_endpoint'])
            ? $params['presign_endpoint']
            : '';

        $kind = isset($params['destination_kind']) && is_string($params['destination_kind'])
            ? $params['destination_kind']
            : 'cp';

        switch ($kind) {
            case 'local':
                return new LocalDestination($transport, $snapshotId, $manifestEp);
            case 'cp':
            case 's3_compat':
                // s3_compat reuses the CP transport because CP-issued presigned
                // URLs hide the actual endpoint from the agent — same code path
                // as the legacy `cp` destination.
                return new CpDestination($transport, $snapshotId, $presignEp, $manifestEp, $kind);
            default:
                throw new \InvalidArgumentException(
                    'WPMgr Destination Resolver: unknown destination kind: ' . $kind
                );
        }
    }
}
