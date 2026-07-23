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
        $res = $this->getChunkWithStatus($presignedUrl);
        return $res['ok'] ? $res['body'] : null;
    }

    /**
     * Download a ciphertext chunk and return a structured result the caller can
     * inspect to make a retry/no-retry decision and emit a useful error message.
     *
     * The returned shape is intentionally rich enough to drive the v0.8.6
     * restore-side retry loop without leaking the presigned URL itself:
     *
     *   ok            true iff status was 2xx and body was a string.
     *   status        HTTP status code (0 on WP_Error / connect failure).
     *   body          response body bytes on success, '' otherwise.
     *   error         WP_Error message excerpt (truncated), or '' if none.
     *   body_excerpt  first 200 chars of the (non-2xx) body, for diagnostics.
     *   host          host of the presigned URL (NOT the path/query/sig), for
     *                 operator-side grep without leaking the bearer URL.
     *   retryable     true if the caller should back off and retry: WP_Error
     *                 (network/DNS/timeout), HTTP 408/425/429, or any 5xx.
     *                 False on 2xx (no retry needed) AND on terminal 4xx
     *                 (404 missing, 403/400 malformed/expired — retrying won't
     *                 help; the URL is the same on each attempt).
     *
     * The presigned URL itself is NEVER returned or logged. Bearer credentials
     * stay inside this method.
     *
     * @param string $presignedUrl Presigned S3 GET URL (bearer credential).
     * @return array{ok:bool,status:int,body:string,error:string,body_excerpt:string,host:string,retryable:bool}
     */
    public function getChunkWithStatus(string $presignedUrl): array
    {
        $host = $this->hostOf($presignedUrl);
        $response = wp_remote_get(
            $presignedUrl,
            ['timeout' => self::TIMEOUT]
        );

        if ($this->isWpError($response)) {
            $msg = '';
            if (is_object($response) && method_exists($response, 'get_error_message')) {
                $msg = (string) $response->get_error_message();
            }
            return [
                'ok'           => false,
                'status'       => 0,
                'body'         => '',
                'error'        => substr($msg, 0, 240),
                'body_excerpt' => '',
                'host'         => $host,
                // Network/transport-level errors are always worth retrying:
                // DNS hiccup, TLS handshake reset, CF tunnel blip, socket
                // timeout. WP_Error means we never saw an HTTP status.
                'retryable'    => true,
            ];
        }

        $status = (int) wp_remote_retrieve_response_code($response);
        if ($status >= 200 && $status < 300) {
            $body = wp_remote_retrieve_body($response);
            $body = is_string($body) ? $body : '';
            return [
                'ok'           => true,
                'status'       => $status,
                'body'         => $body,
                'error'        => '',
                'body_excerpt' => '',
                'host'         => $host,
                'retryable'    => false,
            ];
        }

        $raw = wp_remote_retrieve_body($response);
        $raw = is_string($raw) ? $raw : '';

        // Retry classification — standard retry-with-backoff semantics (see
        // `docs/research/async-progress-restore.md` §7) plus the
        // standard "5xx + 408/425/429 retryable, terminal 4xx is not":
        //   - 5xx: gateway/origin transient (e.g. SeaweedFS restart, CF 502).
        //   - 408 Request Timeout / 425 Too Early / 429 Too Many Requests.
        //   - everything else 4xx is terminal — the same presigned URL will
        //     keep producing the same answer.
        $retryable = ($status >= 500 && $status < 600)
            || $status === 408
            || $status === 425
            || $status === 429;

        return [
            'ok'           => false,
            'status'       => $status,
            'body'         => '',
            'error'        => '',
            'body_excerpt' => substr($raw, 0, 200),
            'host'         => $host,
            'retryable'    => $retryable,
        ];
    }

    /**
     * Extract the host of a URL (used by getChunkWithStatus for diagnostics
     * that don't leak the presigned bearer URL).
     */
    private function hostOf(string $url): string
    {
        $parts = wp_parse_url($url);
        if (!is_array($parts) || !isset($parts['host']) || !is_string($parts['host'])) {
            return '';
        }
        return $parts['host'];
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
        $parts = wp_parse_url($url);
        if (!is_array($parts) || !isset($parts['path']) || !is_string($parts['path']) || $parts['path'] === '') {
            return '';
        }

        return $parts['path'];
    }

    /**
     * Decode a CP JSON response, asserting a 2xx status.
     *
     * GH #279: on a non-2xx status, the thrown message includes the HTTP
     * status and a truncated (200-char) body excerpt, mirroring
     * getChunkWithStatus()'s body_excerpt pattern. Covers BOTH
     * presignChunks() and submitManifest() since both route through here.
     * The CP error body is a JSON `{error:{code,message}}` object, not a
     * credential, so surfacing it is safe (contrast with putChunk()/
     * getChunk(), which never log the presigned URL itself).
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
        $raw    = (string) wp_remote_retrieve_body($response);
        if ($status < 200 || $status >= 300) {
            throw new \RuntimeException(sprintf( // phpcs:ignore WordPress.Security.EscapeOutput.ExceptionNotEscaped -- thrown exception; message goes to server log/SSE, not browser output
                'WPMgr Agent: control plane callback rejected (HTTP %s): %s',
                esc_html((string) $status),
                esc_html(substr($raw, 0, 200))
            ));
        }
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
