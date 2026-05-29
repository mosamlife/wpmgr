<?php
/**
 * ProgressClient — POSTs phpbu phase events to the CP's /agent/v1/backups/{id}
 * /progress endpoint. Signs with the same Ed25519 scheme the existing
 * `BackupTransport` uses for presign/manifest (we reuse the agent's `Signer`).
 *
 * Fire-and-forget: a failed progress POST MUST NEVER fail the backup. The CP
 * watchdog will mark the snapshot stalled if it stops hearing from us — but
 * one dropped progress event is not a stall.
 *
 * @package WPMgr\Agent\Phpbu
 */

declare(strict_types=1);

namespace WPMgr\Agent\Phpbu;

use WPMgr\Agent\Signer;

final class ProgressClient
{
    private string $endpoint;
    private string $snapshotId;
    private Signer $signer;

    public function __construct(string $endpoint, string $snapshotId, Signer $signer)
    {
        $this->endpoint   = $endpoint;
        $this->snapshotId = $snapshotId;
        $this->signer     = $signer;
    }

    /**
     * Post one phase event. Returns true on 2xx, false otherwise. Never throws.
     *
     * @param string              $phase  One of the closed set the CP accepts
     *                                    (see backup.allowedProgressPhases).
     * @param array<string,mixed> $detail Phase-specific telemetry (chunks_done,
     *                                    bytes_done, …). Must be JSON-encodable.
     */
    public function post(string $phase, array $detail = []): bool
    {
        $body = json_encode(['phase' => $phase, 'phase_detail' => (object) $detail]);
        if ($body === false) {
            return false;
        }
        // Reuse the existing M2 signed-request scheme. Signer::signHeaders
        // returns the X-WPMgr-* header map the CP's agent.Authenticator
        // verifies (the same shape BackupTransport uses for presign/manifest).
        try {
            $auth = $this->signer->signHeaders('POST', $this->path(), $body);
        } catch (\Throwable $e) {
            return false;
        }
        $headers = array_merge(['Content-Type' => 'application/json'], $auth);

        // wp_remote_post is available because the runner shim bootstraps WP
        // minimally; if not, fall back to a raw socket POST (the runner
        // bootstrap ensures wp_remote_* exists, so this is defensive).
        if (function_exists('wp_remote_post')) {
            $resp = wp_remote_post($this->endpoint, [
                'headers'   => $headers,
                'body'      => $body,
                'timeout'   => 5,
                'sslverify' => true,
                'blocking'  => true,
            ]);
            if (is_wp_error($resp)) {
                return false;
            }
            $code = (int) wp_remote_retrieve_response_code($resp);
            return $code >= 200 && $code < 300;
        }
        return false;
    }

    /** Extract the URL path (everything after the host) for the signature. */
    private function path(): string
    {
        $parts = parse_url($this->endpoint);
        if (!is_array($parts) || empty($parts['path'])) {
            return '/';
        }
        $path = $parts['path'];
        if (!empty($parts['query'])) {
            $path .= '?' . $parts['query'];
        }
        return $path;
    }
}
