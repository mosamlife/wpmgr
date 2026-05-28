<?php
/**
 * BackupTransport: the single seam between the backup/restore command logic and
 * the network. It performs:
 *
 *   - presignChunks():  agent->CP signed POST to the command's presign_endpoint,
 *                       returning the blake3 -> presigned-PUT-URL map for chunks
 *                       NOT already stored (dedup).      [PresignChunksRequest /
 *                                                          PresignChunksResponse]
 *   - putChunk():       direct PUT of ciphertext to a presigned S3 URL.
 *   - submitManifest(): agent->CP signed POST to the command's manifest_endpoint
 *                       with the completed manifest.      [SubmitManifestRequest /
 *                                                          SubmitManifestResponse]
 *   - getChunk():       direct GET of ciphertext from a presigned S3 URL.
 *
 * The presign/manifest callbacks reuse the M2 Ed25519 signed-request scheme
 * (Signer + the four X-WPMgr-* headers); the CP authenticates the agent from the
 * verified key, never a client header (see agent_handler.go).
 *
 * Presigned URLs are bearer credentials: they are NEVER logged. On any failure
 * we surface a generic boolean/empty result, not the URL or response body.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

use WPMgr\Agent\Signer;

/**
 * Signed CP callbacks + direct S3 chunk transfer for backup/restore.
 */
class BackupTransport
{
    /** Default outbound request timeout, in seconds. */
    private const TIMEOUT = 30;

    private Signer $signer;

    /**
     * @param Signer $signer Outbound agent-auth request signer.
     */
    public function __construct(Signer $signer)
    {
        $this->signer = $signer;
    }

    /**
     * Ask the CP which ciphertext chunk hashes are not yet stored, receiving a
     * presigned PUT URL for each one to upload.
     *
     * @param string       $endpoint   Absolute presign endpoint URL (CP-supplied).
     * @param string       $snapshotId In-flight snapshot id.
     * @param list<string> $hashes     Candidate ciphertext chunk hashes.
     * @return array<string,string> Map of blake3 => presigned PUT URL (uploads).
     * @throws \RuntimeException On transport/auth/parse failure.
     */
    public function presignChunks(string $endpoint, string $snapshotId, array $hashes): array
    {
        $body = (string) wp_json_encode([
            'snapshot_id' => $snapshotId,
            'hashes'      => array_values($hashes),
        ]);

        $response = $this->signedPost($endpoint, $body);
        $data     = $this->decodeJsonResponse($response);

        $uploads = [];
        if (isset($data['uploads']) && is_array($data['uploads'])) {
            foreach ($data['uploads'] as $hash => $url) {
                if (is_string($hash) && is_string($url) && $hash !== '' && $url !== '') {
                    $uploads[$hash] = $url;
                }
            }
        }

        return $uploads;
    }

    /**
     * Submit the completed manifest to the CP.
     *
     * @param string                                                                                                 $endpoint     Absolute manifest endpoint URL.
     * @param string                                                                                                 $snapshotId   Snapshot id.
     * @param string                                                                                                 $ageRecipient Recipient the chunks were encrypted to.
     * @param list<array{path:string,entry_kind:string,table_name:string,mode:int,size:int,chunks:list<array{blake3:string,size:int}>}> $entries      Manifest entries.
     * @return array{ok:bool,chunk_count:int,stored_count:int}
     * @throws \RuntimeException On transport/auth/parse failure.
     */
    public function submitManifest(string $endpoint, string $snapshotId, string $ageRecipient, array $entries): array
    {
        $body = (string) wp_json_encode([
            'snapshot_id'   => $snapshotId,
            'age_recipient' => $ageRecipient,
            'entries'       => $entries,
        ]);

        $response = $this->signedPost($endpoint, $body);
        $data     = $this->decodeJsonResponse($response);

        return [
            'ok'           => isset($data['ok']) && $data['ok'] === true,
            'chunk_count'  => isset($data['chunk_count']) && is_numeric($data['chunk_count']) ? (int) $data['chunk_count'] : 0,
            'stored_count' => isset($data['stored_count']) && is_numeric($data['stored_count']) ? (int) $data['stored_count'] : 0,
        ];
    }

    /**
     * Upload a ciphertext chunk to a presigned PUT URL.
     *
     * @param string $presignedUrl Presigned S3 PUT URL (bearer credential).
     * @param string $ciphertext   Ciphertext bytes.
     * @return bool True on a 2xx response.
     */
    public function putChunk(string $presignedUrl, string $ciphertext): bool
    {
        $response = wp_remote_request(
            $presignedUrl,
            [
                'method'  => 'PUT',
                'timeout' => self::TIMEOUT,
                'headers' => ['Content-Type' => 'application/octet-stream'],
                'body'    => $ciphertext,
            ]
        );

        if ($this->isWpError($response)) {
            return false;
        }
        $status = (int) wp_remote_retrieve_response_code($response);

        return $status >= 200 && $status < 300;
    }

    /**
     * Download a ciphertext chunk from a presigned GET URL.
     *
     * @param string $presignedUrl Presigned S3 GET URL (bearer credential).
     * @return string|null Ciphertext bytes, or null on failure.
     */
    public function getChunk(string $presignedUrl): ?string
    {
        $response = wp_remote_get(
            $presignedUrl,
            ['timeout' => self::TIMEOUT]
        );

        if ($this->isWpError($response)) {
            return null;
        }
        $status = (int) wp_remote_retrieve_response_code($response);
        if ($status < 200 || $status >= 300) {
            return null;
        }

        $body = wp_remote_retrieve_body($response);

        return is_string($body) ? $body : null;
    }

    /**
     * Perform an agent-authenticated POST to an absolute CP endpoint URL.
     *
     * The Signer signs the canonical message over METHOD\nPATH\n... where PATH
     * is the URL path component only (no host/query), matching the CP verifier.
     *
     * @param string $url  Absolute endpoint URL (CP-supplied).
     * @param string $body Raw JSON body.
     * @return mixed wp_remote_* response or WP_Error.
     * @throws \RuntimeException On signing failure or a malformed URL.
     */
    private function signedPost(string $url, string $body)
    {
        $path = $this->pathOf($url);
        if ($path === '') {
            throw new \RuntimeException('WPMgr Agent: invalid callback URL.');
        }

        $authHeaders = $this->signer->signHeaders('POST', $path, $body);

        $headers = array_merge(
            ['Content-Type' => 'application/json', 'Accept' => 'application/json'],
            $authHeaders
        );

        return wp_remote_post(
            $url,
            [
                'timeout' => self::TIMEOUT,
                'headers' => $headers,
                'body'    => $body,
            ]
        );
    }

    /**
     * Extract the path component of an absolute URL (for canonical signing).
     *
     * @param string $url Absolute URL.
     * @return string Path (e.g. "/agent/v1/backups/<id>/presign"), or '' if bad.
     */
    private function pathOf(string $url): string
    {
        $parts = parse_url($url);
        if (!is_array($parts) || !isset($parts['path']) || !is_string($parts['path']) || $parts['path'] === '') {
            return '';
        }

        return $parts['path'];
    }

    /**
     * Decode a CP JSON response, asserting a 2xx status.
     *
     * @param mixed $response wp_remote_* response or WP_Error.
     * @return array<string,mixed>
     * @throws \RuntimeException On error/non-2xx/invalid JSON.
     */
    private function decodeJsonResponse($response): array
    {
        if ($this->isWpError($response)) {
            throw new \RuntimeException('WPMgr Agent: control plane unreachable.');
        }
        $status = (int) wp_remote_retrieve_response_code($response);
        if ($status < 200 || $status >= 300) {
            throw new \RuntimeException('WPMgr Agent: control plane callback rejected.');
        }
        $raw  = (string) wp_remote_retrieve_body($response);
        $data = json_decode($raw, true);
        if (!is_array($data)) {
            throw new \RuntimeException('WPMgr Agent: malformed control plane response.');
        }

        /** @var array<string,mixed> $data */
        return $data;
    }

    /**
     * Whether a wp_remote_* response is a WP_Error.
     *
     * @param mixed $response Response or WP_Error.
     * @return bool
     */
    private function isWpError($response): bool
    {
        return function_exists('is_wp_error') && is_wp_error($response);
    }
}
