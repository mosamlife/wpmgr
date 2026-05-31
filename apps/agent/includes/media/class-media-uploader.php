<?php
/**
 * MediaUploader: the single network seam for the Media Optimizer's
 * presigned-S3 transport + the signed agent->CP /agent/v1/media/* callbacks.
 *
 * Mirrors the backup transport (WPMgr\Agent\Support\BackupTransport): signed
 * agent->CP JSON via the four X-WPMgr-* headers (Signer), plain presigned
 * PUT/GET for bytes (the URL is the auth — NEVER logged).
 *
 * It implements exactly the CP contract from
 * apps/api/internal/media/handler/agent_handler.go +
 * apps/api/internal/agentcmd/media_contract.go:
 *
 *   - POST /agent/v1/media/sync-batch    {attachments:[...]}      -> {upserted_count}
 *   - POST /agent/v1/media/presign       {job_id,variants:[...]}  -> {uploads:{name->putUrl}}
 *   - POST /agent/v1/media/encode-ready  {job_id,variants:[...]}  -> {ok:true}
 *   - POST /agent/v1/media/job-status    {job_id,...}             -> {ok:true}
 *   - POST /agent/v1/media/restore-status{job_id,restored,error?} -> {ok:true}
 *   - PUT  <presigned>  (source variant bytes -> media/.../src/<name>)
 *   - GET  <presigned>  (optimized output bytes <- media/.../out/<name>)
 *
 * Endpoint URLs are CP-SUPPLIED in the command payload (presign_endpoint,
 * ready_endpoint, status_endpoint, batch_endpoint), so this class signs over the
 * URL's PATH component (Signer canonical message), matching the CP verifier.
 *
 * @package WPMgr\Agent\Media
 */

declare(strict_types=1);

namespace WPMgr\Agent\Media;

use WPMgr\Agent\Signer;

/**
 * Signed CP callbacks + presigned byte transfer for the Media Optimizer.
 */
class MediaUploader
{
    /** Default outbound request timeout, in seconds. */
    private const TIMEOUT = 30;

    /** PUT/GET retry attempts on transient (network / 5xx / 408/425/429) failures. */
    private const MAX_RETRIES = 3;

    private Signer $signer;

    public function __construct(Signer $signer)
    {
        $this->signer = $signer;
    }

    // ------------------------------------------------------------------
    // signed agent->CP JSON callbacks
    // ------------------------------------------------------------------

    /**
     * POST /agent/v1/media/sync-batch — push a page of enumerated attachments.
     *
     * @param string                          $endpoint    Absolute batch endpoint URL.
     * @param list<array<string,mixed>>       $attachments syncBatchAttachmentDTO-shaped rows.
     * @return array{ok:bool,upserted_count:int}
     */
    public function syncBatch(string $endpoint, array $attachments): array
    {
        $body = (string) wp_json_encode(['attachments' => array_values($attachments)]);
        $data = $this->signedPostJson($endpoint, $body);

        return [
            'ok'             => $data !== null,
            'upserted_count' => is_array($data) && isset($data['upserted_count']) && is_numeric($data['upserted_count'])
                ? (int) $data['upserted_count']
                : 0,
        ];
    }

    /**
     * POST /agent/v1/media/presign — request presigned PUT URLs for source
     * variants. Returns the {name -> putUrl} map the agent then PUTs to.
     *
     * @param string                                                       $endpoint Absolute presign endpoint URL.
     * @param string                                                       $jobId    CP job id (ULID).
     * @param list<array{name:string,source_size:int,source_mime:string}> $variants presignVariantDTO rows.
     * @return array<string,string> name => presigned PUT URL.
     * @throws \RuntimeException On transport/auth/parse failure.
     */
    public function presign(string $endpoint, string $jobId, array $variants): array
    {
        $body = (string) wp_json_encode([
            'job_id'   => $jobId,
            'variants' => array_values($variants),
        ]);
        $data = $this->signedPostJson($endpoint, $body);
        if ($data === null) {
            throw new \RuntimeException('WPMgr Agent: media presign rejected.');
        }

        $uploads = [];
        if (isset($data['uploads']) && is_array($data['uploads'])) {
            foreach ($data['uploads'] as $name => $url) {
                if (is_string($name) && is_string($url) && $name !== '' && $url !== '') {
                    $uploads[$name] = $url;
                }
            }
        }

        return $uploads;
    }

    /**
     * POST /agent/v1/media/encode-ready — signal sources uploaded so the CP can
     * enqueue the encode jobs.
     *
     * @param string                                                       $endpoint Absolute ready endpoint URL.
     * @param string                                                       $jobId    CP job id.
     * @param list<array{name:string,source_size:int,source_mime:string}> $variants presignVariantDTO rows.
     * @return bool True on a 2xx ack.
     */
    public function encodeReady(string $endpoint, string $jobId, array $variants): bool
    {
        $body = (string) wp_json_encode([
            'job_id'   => $jobId,
            'variants' => array_values($variants),
        ]);

        return $this->signedPostJson($endpoint, $body) !== null;
    }

    /**
     * POST /agent/v1/media/job-status — report an apply (or delete-originals)
     * result. Field names mirror jobStatusBody in agent_handler.go exactly.
     *
     * @param string              $endpoint Absolute status endpoint URL.
     * @param array<string,mixed> $payload  jobStatusBody-shaped (job_id required).
     * @return bool True on a 2xx ack.
     */
    public function jobStatus(string $endpoint, array $payload): array
    {
        $body = (string) wp_json_encode($payload);
        $data = $this->signedPostJson($endpoint, $body);

        return ['ok' => $data !== null];
    }

    /**
     * POST /agent/v1/media/restore-status — report a restore result.
     *
     * @param string $endpoint Absolute status endpoint URL.
     * @param string $jobId    CP job id.
     * @param bool   $restored Whether the restore succeeded.
     * @param string $error    Optional error detail ('' when none).
     * @return bool True on a 2xx ack.
     */
    public function restoreStatus(string $endpoint, string $jobId, bool $restored, string $error = ''): bool
    {
        $body = (string) wp_json_encode([
            'job_id'   => $jobId,
            'restored' => $restored,
            'error'    => $error,
        ]);

        return $this->signedPostJson($endpoint, $body) !== null;
    }

    // ------------------------------------------------------------------
    // presigned byte transfer (the URL is the auth — never logged)
    // ------------------------------------------------------------------

    /**
     * PUT raw source-variant bytes to a presigned URL (media/.../src/<name>).
     * Plain PUT with octet-stream; retried on transient failures. Mirrors
     * BackupTransport::putChunk.
     *
     * @param string $presignedUrl Presigned S3 PUT URL (bearer credential).
     * @param string $bytes        Raw image bytes.
     * @return bool True on a 2xx response.
     */
    public function putBytes(string $presignedUrl, string $bytes): bool
    {
        for ($attempt = 1; $attempt <= self::MAX_RETRIES; $attempt++) {
            $response = wp_remote_request(
                $presignedUrl,
                [
                    'method'  => 'PUT',
                    'timeout' => self::TIMEOUT,
                    'headers' => ['Content-Type' => 'application/octet-stream'],
                    'body'    => $bytes,
                ]
            );

            if ($this->isWpError($response)) {
                if ($attempt < self::MAX_RETRIES) {
                    $this->backoff($attempt);
                    continue;
                }

                return false;
            }
            $status = (int) wp_remote_retrieve_response_code($response);
            if ($status >= 200 && $status < 300) {
                return true;
            }
            if ($this->retryable($status) && $attempt < self::MAX_RETRIES) {
                $this->backoff($attempt);
                continue;
            }

            return false;
        }

        return false;
    }

    /**
     * GET optimized-output bytes from a presigned URL (media/.../out/<name>).
     * Retried on transient failures. Returns null on terminal failure.
     *
     * @param string $presignedUrl Presigned S3 GET URL (bearer credential).
     * @return string|null Raw bytes, or null on failure.
     */
    public function getBytes(string $presignedUrl): ?string
    {
        for ($attempt = 1; $attempt <= self::MAX_RETRIES; $attempt++) {
            $response = wp_remote_get($presignedUrl, ['timeout' => self::TIMEOUT]);

            if ($this->isWpError($response)) {
                if ($attempt < self::MAX_RETRIES) {
                    $this->backoff($attempt);
                    continue;
                }

                return null;
            }
            $status = (int) wp_remote_retrieve_response_code($response);
            if ($status >= 200 && $status < 300) {
                $body = wp_remote_retrieve_body($response);

                return is_string($body) ? $body : null;
            }
            if ($this->retryable($status) && $attempt < self::MAX_RETRIES) {
                $this->backoff($attempt);
                continue;
            }

            return null;
        }

        return null;
    }

    // ------------------------------------------------------------------
    // internals
    // ------------------------------------------------------------------

    /**
     * Sign + POST JSON to an absolute CP endpoint URL, returning the decoded 2xx
     * body or null on any failure. The Signer signs over the URL's PATH only.
     *
     * @param string $url  Absolute endpoint URL (CP-supplied).
     * @param string $body Raw JSON body.
     * @return array<string,mixed>|null
     */
    private function signedPostJson(string $url, string $body): ?array
    {
        $path = $this->pathOf($url);
        if ($path === '') {
            return null;
        }
        try {
            $authHeaders = $this->signer->signHeaders('POST', $path, $body);
        } catch (\Throwable $e) {
            return null;
        }

        $response = wp_remote_post(
            $url,
            [
                'timeout' => self::TIMEOUT,
                'headers' => array_merge(
                    ['Content-Type' => 'application/json', 'Accept' => 'application/json'],
                    $authHeaders
                ),
                'body'    => $body,
            ]
        );

        if ($this->isWpError($response)) {
            return null;
        }
        $status = (int) wp_remote_retrieve_response_code($response);
        if ($status < 200 || $status >= 300) {
            return null;
        }
        $raw  = (string) wp_remote_retrieve_body($response);
        $data = json_decode($raw, true);

        return is_array($data) ? $data : [];
    }

    /**
     * Path component of an absolute URL (for canonical signing).
     *
     * @param string $url
     * @return string
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
     * Whether an HTTP status is worth retrying (5xx + 408/425/429), mirroring
     * BackupTransport's classification.
     *
     * @param int $status
     * @return bool
     */
    private function retryable(int $status): bool
    {
        return ($status >= 500 && $status < 600) || $status === 408 || $status === 425 || $status === 429;
    }

    /**
     * Sleep a short, bounded backoff between retries.
     *
     * @param int $attempt 1-based attempt number.
     * @return void
     */
    private function backoff(int $attempt): void
    {
        // 200ms, 400ms — bounded; never long enough to risk a request timeout.
        usleep(200000 * $attempt);
    }

    /**
     * @param mixed $response
     * @return bool
     */
    private function isWpError($response): bool
    {
        return function_exists('is_wp_error') && is_wp_error($response);
    }
}
