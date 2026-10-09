<?php
/**
 * BackupTransport: the single seam between the backup/restore command logic and
 * the network. It performs:
 *
 *   - presignChunks():  agent->CP signed POST to the command's presign_endpoint,
 *                       returning the blake3 -> presigned-PUT-URL map for chunks
 *                       NOT already stored (dedup).      [PresignChunksRequest /
 *                                                          PresignChunksResponse]
 *   - putChunk():       direct PUT of ciphertext to a presigned S3 URL
 *                       (putChunkWithStatus() returns the structured outcome).
 *   - submitManifest(): agent->CP signed POST to the command's manifest_endpoint
 *                       with the completed manifest.      [SubmitManifestRequest /
 *                                                          SubmitManifestResponse]
 *   - getChunk():       direct GET of ciphertext from a presigned S3 URL.
 *
 * The presign/manifest callbacks reuse the M2 Ed25519 signed-request scheme
 * (Signer + the four X-WPMgr-* headers); the CP authenticates the agent from the
 * verified key, never a client header (see agent_handler.go).
 *
 * Presigned URLs are bearer credentials: they are NEVER logged or returned. A
 * failed chunk transfer reports its HTTP status, the storage host and a short
 * sanitized cause, never the URL, its query string or the raw response body.
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
    /** Default outbound request timeout, in seconds (CP callbacks, chunk GETs). */
    private const TIMEOUT = 30;

    /**
     * Per-attempt timeout for one chunk PUT, in seconds. A 4 MiB chunk on a
     * slow uplink can run past the 30 s callback timeout; 120 s matches the
     * file-archive upload path. A PUT blocks without emitting progress, so
     * EncryptAndUpload keeps this plus its heartbeat interval below the
     * backup watchdog's stall threshold (pinned by a test).
     */
    private const PUT_TIMEOUT = 120;

    /** Longest S3 error <Code> kept; longer or non-alphanumeric codes are dropped. */
    private const S3_CODE_MAX = 64;

    /** Longest transport-error excerpt returned by putChunkWithStatus(). */
    private const PUT_ERROR_MAX = 160;

    /** Longest host returned for diagnostics. */
    private const HOST_MAX = 100;

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
     * Thin wrapper over putChunkWithStatus() for callers that only need a
     * yes/no answer (CpDestination's single-shot putChunk()).
     *
     * @param string $presignedUrl Presigned S3 PUT URL (bearer credential).
     * @param string $ciphertext   Ciphertext bytes.
     * @return bool True on a 2xx response.
     */
    public function putChunk(string $presignedUrl, string $ciphertext): bool
    {
        return $this->putChunkWithStatus($presignedUrl, $ciphertext)['ok'];
    }

    /**
     * Upload a ciphertext chunk and return a structured result the caller can
     * use to decide whether to retry and to say why an upload failed. The PUT
     * counterpart of getChunkWithStatus().
     *
     *   ok         true iff the response status was 2xx.
     *   status     HTTP status code (0 on WP_Error / connect failure).
     *   error      transport error excerpt (WP_Error message), '' otherwise.
     *              Any URL in it is reduced to its host and any signature
     *              query parameter is redacted, so the presigned URL cannot
     *              leak through a transport that echoes it.
     *   s3_code    the <Code> of an S3 XML error body (e.g.
     *              SignatureDoesNotMatch, AccessDenied, SlowDown), kept only
     *              when it is a single alphanumeric token of at most 64
     *              characters; '' otherwise. The body itself is never returned.
     *   host       host of the presigned URL (never its path or query).
     *   retryable  true for a WP_Error (network, DNS, TLS, timeout), HTTP 408,
     *              425, 429 or any 5xx. False on 2xx and on every other 4xx:
     *              the same URL keeps getting the same answer.
     *   represign  true for an HTTP 403 that a freshly presigned URL may get
     *              past: code AccessDenied or ExpiredToken, or an expired-
     *              request message. Retrying the SAME URL never helps there.
     *
     * The presigned URL itself is NEVER returned or logged.
     *
     * @param string $presignedUrl Presigned S3 PUT URL (bearer credential).
     * @param string $ciphertext   Ciphertext bytes.
     * @return array{ok:bool,status:int,error:string,s3_code:string,host:string,retryable:bool,represign:bool}
     */
    public function putChunkWithStatus(string $presignedUrl, string $ciphertext): array
    {
        $host     = $this->hostOf($presignedUrl);
        $response = wp_remote_request(
            $presignedUrl,
            [
                'method'  => 'PUT',
                'timeout' => self::PUT_TIMEOUT,
                'headers' => ['Content-Type' => 'application/octet-stream'],
                'body'    => $ciphertext,
            ]
        );

        if ($this->isWpError($response)) {
            $msg = '';
            if (is_object($response) && method_exists($response, 'get_error_message')) {
                $msg = (string) $response->get_error_message();
            }
            return [
                'ok'        => false,
                'status'    => 0,
                'error'     => $this->redactTransportError($msg, $presignedUrl),
                's3_code'   => '',
                'host'      => $host,
                'retryable' => true,
                'represign' => false,
            ];
        }

        $status = (int) wp_remote_retrieve_response_code($response);
        if ($status >= 200 && $status < 300) {
            return [
                'ok'        => true,
                'status'    => $status,
                'error'     => '',
                's3_code'   => '',
                'host'      => $host,
                'retryable' => false,
                'represign' => false,
            ];
        }

        $raw = wp_remote_retrieve_body($response);
        // An S3 error document is a few hundred bytes; never scan more.
        $raw  = is_string($raw) ? substr($raw, 0, 4096) : '';
        $code = $this->s3ErrorCode($raw);

        // Same retry classification as getChunkWithStatus().
        $retryable = ($status >= 500 && $status < 600)
            || $status === 408
            || $status === 425
            || $status === 429;

        $represign = $status === 403
            && ($code === 'AccessDenied'
                || $code === 'ExpiredToken'
                || stripos($raw, 'Request has expired') !== false);

        return [
            'ok'        => false,
            'status'    => $status,
            'error'     => '',
            's3_code'   => $code,
            'host'      => $host,
            'retryable' => $retryable,
            'represign' => $represign,
        ];
    }

    /**
     * Pull the <Code> out of an S3 XML error body, e.g.
     * `<Error><Code>SignatureDoesNotMatch</Code>...</Error>`. The value ends up
     * in a backup-failure message, so only a single alphanumeric token that
     * starts with a letter is accepted (S3's documented codes are all of that
     * shape, some with digits, e.g. XAmzContentSHA256Mismatch). Anything else
     * yields ''.
     *
     * @param string $body Response body (already length-capped).
     * @return string
     */
    private function s3ErrorCode(string $body): string
    {
        if ($body === '') {
            return '';
        }
        $pattern = '#<Code>\s*([A-Za-z][A-Za-z0-9]{0,' . (self::S3_CODE_MAX - 1) . '})\s*</Code>#';
        if (preg_match($pattern, $body, $m) !== 1) {
            return '';
        }
        return $m[1];
    }

    /**
     * Make a transport error message safe to return: the presigned URL (and
     * any other absolute URL) is reduced to its host, signature-bearing query
     * parameters are redacted, control characters are flattened and the
     * result is capped.
     *
     * @param string $message      Raw WP_Error message.
     * @param string $presignedUrl The URL the request was made to.
     * @return string
     */
    private function redactTransportError(string $message, string $presignedUrl): string
    {
        if ($message === '') {
            return '';
        }
        if ($presignedUrl !== '') {
            $message = str_replace($presignedUrl, $this->hostOf($presignedUrl), $message);
        }
        $message = (string) preg_replace_callback(
            '#\bhttps?://[^\s"\'<>]+#i',
            function (array $m): string {
                $host = $this->hostOf($m[0]);
                return $host !== '' ? $host : '[url]';
            },
            $message
        );
        // A percent-encoded URL carries its signature encoded too.
        $message = (string) preg_replace('#\bhttps?%3A%2F%2F[^\s"\'<>]*#i', '[url]', $message);
        $message = (string) preg_replace(
            '#\b(X-Amz-[A-Za-z0-9-]+|X-Goog-[A-Za-z0-9-]+|Signature|AWSAccessKeyId|GoogleAccessId|Expires)(?:=|%3D)[^\s&"\'<>]*#i',
            '$1=[redacted]',
            $message
        );
        $message = (string) preg_replace('/[\x00-\x1F\x7F]+/', ' ', $message);

        return self::capUtf8(trim($message), self::PUT_ERROR_MAX);
    }

    /**
     * Cap text bound for a failure report at a byte budget and keep it valid
     * UTF-8. The report travels as JSON (ProgressClient, saveTaskState), and
     * json_encode() refuses invalid UTF-8, which would drop the whole report.
     * Text that is not valid UTF-8 to begin with keeps only its ASCII; a cut
     * that lands inside a multi-byte character drops that partial character.
     * Needs neither mbstring nor iconv.
     *
     * @param string $text     Text to cap.
     * @param int    $maxBytes Byte budget.
     * @return string
     */
    public static function capUtf8(string $text, int $maxBytes): string
    {
        if (preg_match('//u', $text) !== 1) {
            $text = (string) preg_replace('/[\x80-\xFF]+/', '', $text);
        }
        if (strlen($text) <= $maxBytes) {
            return $text;
        }
        $cut = substr($text, 0, max(0, $maxBytes));
        // Valid input cut at a byte boundary leaves at most one partial
        // sequence (up to 3 bytes) at the end.
        for ($i = 0; $i < 3 && $cut !== '' && preg_match('//u', $cut) !== 1; $i++) {
            $cut = substr($cut, 0, -1);
        }
        return $cut;
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
     * Extract the host of a URL (used by getChunkWithStatus and
     * putChunkWithStatus for diagnostics that don't leak the presigned bearer
     * URL). The host reaches failure messages, so it is reduced to hostname
     * characters (letters, digits, dot, hyphen, and the brackets and colons
     * of an IPv6 literal) and capped.
     */
    private function hostOf(string $url): string
    {
        $parts = wp_parse_url($url);
        if (!is_array($parts) || !isset($parts['host']) || !is_string($parts['host'])) {
            return '';
        }
        $host = (string) preg_replace('/[^A-Za-z0-9.\-\[\]:]/', '', $parts['host']);
        return substr($host, 0, self::HOST_MAX);
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
