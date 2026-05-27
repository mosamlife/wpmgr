<?php
/**
 * Enrollment + reporting client (agent -> control plane).
 *
 * Responsibilities:
 *   - POST /enroll (public, no auth): exchange a pairing code + the agent's own
 *     Ed25519 public key for a site_id, tenant_id, and the control-plane public
 *     key. Persist site_id/tenant_id (Settings) and the CP key (Keystore, so the
 *     inbound Connector verification uses the enrolled key).
 *   - POST /agent/v1/metadata and /agent/v1/heartbeat: agent-authenticated via
 *     the four X-WPMgr-* headers produced by Signer.
 *
 * All responses are treated as untrusted. Secrets (pairing code, keys) are
 * never logged.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent;

use WPMgr\Agent\Commands\MetadataCommand;

/**
 * Outbound HTTP client for enrollment and reporting.
 */
final class Enrollment
{
    /** Public enrollment path. */
    public const PATH_ENROLL = '/enroll';

    /** Agent-authenticated metadata path. */
    public const PATH_METADATA = '/agent/v1/metadata';

    /** Agent-authenticated heartbeat path. */
    public const PATH_HEARTBEAT = '/agent/v1/heartbeat';

    /** Default outbound request timeout, in seconds. */
    private const TIMEOUT = 15;

    private Keystore $keystore;

    private Settings $settings;

    private Signer $signer;

    private MetadataCommand $metadata;

    /**
     * @param Keystore        $keystore Key material store.
     * @param Settings        $settings Enrollment/config state.
     * @param Signer          $signer   Outbound request signer.
     * @param MetadataCommand $metadata Metadata collector.
     */
    public function __construct(Keystore $keystore, Settings $settings, Signer $signer, MetadataCommand $metadata)
    {
        $this->keystore = $keystore;
        $this->settings = $settings;
        $this->signer   = $signer;
        $this->metadata = $metadata;
    }

    /**
     * Build the JSON-serializable /enroll request payload.
     *
     * @param string $pairingCode Plaintext pairing code from the admin.
     * @return array{
     *     pairing_code:string,
     *     site_url:string,
     *     agent_public_key:string,
     *     name:string,
     *     wp_version:string,
     *     php_version:string,
     *     tags:array<int,string>
     * }
     */
    public function buildEnrollPayload(string $pairingCode): array
    {
        return [
            'pairing_code'     => $pairingCode,
            'site_url'         => $this->siteUrl(),
            'agent_public_key' => $this->signer->agentPublicKeyBase64(),
            'name'             => $this->siteName(),
            'wp_version'       => $this->metadata->collect()['wp_version'],
            'php_version'      => PHP_VERSION,
            'tags'             => [],
        ];
    }

    /**
     * Perform the enrollment exchange.
     *
     * @param string $pairingCode Plaintext pairing code from the admin.
     * @return array{ok:bool,status:int,code:string,message:string}
     */
    public function enroll(string $pairingCode): array
    {
        $base = $this->settings->controlPlaneUrl();
        if ($base === '') {
            return $this->result(false, 0, 'no_url', 'Set the control-plane URL before enrolling.');
        }

        $payload = $this->buildEnrollPayload($pairingCode);
        $body    = (string) wp_json_encode($payload);

        $response = wp_remote_post(
            $base . self::PATH_ENROLL,
            [
                'timeout' => self::TIMEOUT,
                'headers' => ['Content-Type' => 'application/json', 'Accept' => 'application/json'],
                'body'    => $body,
            ]
        );

        // Wipe the pairing code reference from the local payload copy.
        unset($payload, $pairingCode);

        if ($this->isWpError($response)) {
            return $this->result(false, 0, 'unreachable', 'Control plane is unreachable.');
        }

        $status = (int) wp_remote_retrieve_response_code($response);
        $rawBody = (string) wp_remote_retrieve_body($response);

        if ($status === 200) {
            return $this->finishEnroll($rawBody);
        }

        return $this->result(false, $status, 'http_' . $status, $this->enrollErrorMessage($status));
    }

    /**
     * Parse a 200 enrollment response and persist the results.
     *
     * @param string $rawBody Response body.
     * @return array{ok:bool,status:int,code:string,message:string}
     */
    private function finishEnroll(string $rawBody): array
    {
        $data = json_decode($rawBody, true);
        if (!is_array($data)) {
            return $this->result(false, 200, 'bad_response', 'Malformed enrollment response.');
        }

        $siteId   = isset($data['site_id']) && is_scalar($data['site_id']) ? (string) $data['site_id'] : '';
        $tenantId = isset($data['tenant_id']) && is_scalar($data['tenant_id']) ? (string) $data['tenant_id'] : '';
        $cpKeyB64 = isset($data['control_plane_public_key']) && is_string($data['control_plane_public_key'])
            ? $data['control_plane_public_key']
            : '';

        if ($siteId === '' || $cpKeyB64 === '') {
            return $this->result(false, 200, 'bad_response', 'Enrollment response missing required fields.');
        }

        $rawKey = base64_decode($cpKeyB64, true);
        if ($rawKey === false || strlen($rawKey) !== SODIUM_CRYPTO_SIGN_PUBLICKEYBYTES) {
            return $this->result(false, 200, 'bad_key', 'Control plane returned an invalid public key.');
        }

        // Persist: CP key into the encrypted keystore (used by inbound Connector
        // verification), site/tenant ids into settings.
        $this->keystore->storeControlPlanePublicKey($rawKey);
        $this->settings->setEnrollment($siteId, $tenantId);

        return $this->result(true, 200, 'enrolled', 'Site enrolled successfully.');
    }

    /**
     * Map an enrollment HTTP status to an admin-facing message.
     *
     * @param int $status HTTP status code.
     * @return string
     */
    private function enrollErrorMessage(int $status): string
    {
        switch ($status) {
            case 401:
                return 'Pairing code is invalid or expired.';
            case 409:
                return 'Pairing code already used, or this site is owned elsewhere.';
            case 422:
                return 'Enrollment was rejected (validation error). Check the site URL.';
            default:
                return 'Enrollment failed (HTTP ' . $status . ').';
        }
    }

    /**
     * Collect and push site metadata (signed).
     *
     * @return array{ok:bool,status:int,code:string,message:string}
     */
    public function pushMetadata(): array
    {
        if (!$this->settings->isEnrolled()) {
            return $this->result(false, 0, 'not_enrolled', 'Not enrolled.');
        }

        $payload = $this->metadata->collect();
        $body    = (string) wp_json_encode($payload);

        $result = $this->signedPost(self::PATH_METADATA, $body);
        if ($result['ok']) {
            $this->settings->setLastMetadata(time());
        }

        return $result;
    }

    /**
     * Send a heartbeat (signed, tiny body).
     *
     * @return array{ok:bool,status:int,code:string,message:string}
     */
    public function sendHeartbeat(): array
    {
        if (!$this->settings->isEnrolled()) {
            return $this->result(false, 0, 'not_enrolled', 'Not enrolled.');
        }

        $body = (string) wp_json_encode([
            'site_id' => $this->settings->siteId(),
            'ts'      => time(),
        ]);

        $result = $this->signedPost(self::PATH_HEARTBEAT, $body);
        if ($result['ok']) {
            $this->settings->setLastHeartbeat(time());
        }

        return $result;
    }

    /**
     * Perform an agent-authenticated POST: sign the canonical message and send
     * the four X-WPMgr-* headers.
     *
     * @param string $path Request path only (no host/query).
     * @param string $body Raw JSON body.
     * @return array{ok:bool,status:int,code:string,message:string}
     */
    private function signedPost(string $path, string $body): array
    {
        $base = $this->settings->controlPlaneUrl();
        if ($base === '') {
            return $this->result(false, 0, 'no_url', 'Control-plane URL not set.');
        }

        try {
            $authHeaders = $this->signer->signHeaders('POST', $path, $body);
        } catch (\Throwable $e) {
            return $this->result(false, 0, 'sign_failed', 'Unable to sign request.');
        }

        $headers = array_merge(
            ['Content-Type' => 'application/json', 'Accept' => 'application/json'],
            $authHeaders
        );

        $response = wp_remote_post(
            $base . $path,
            [
                'timeout' => self::TIMEOUT,
                'headers' => $headers,
                'body'    => $body,
            ]
        );

        if ($this->isWpError($response)) {
            return $this->result(false, 0, 'unreachable', 'Control plane is unreachable.');
        }

        $status = (int) wp_remote_retrieve_response_code($response);
        if ($status >= 200 && $status < 300) {
            return $this->result(true, $status, 'ok', 'OK.');
        }

        return $this->result(false, $status, 'http_' . $status, 'Request failed (HTTP ' . $status . ').');
    }

    /**
     * This site's canonical URL.
     *
     * @return string
     */
    private function siteUrl(): string
    {
        if (function_exists('home_url')) {
            $url = home_url();
            if (is_string($url) && $url !== '') {
                return $url;
            }
        }
        if (function_exists('get_option')) {
            $url = get_option('siteurl');
            if (is_string($url)) {
                return $url;
            }
        }

        return '';
    }

    /**
     * This site's display name.
     *
     * @return string
     */
    private function siteName(): string
    {
        if (function_exists('get_bloginfo')) {
            $name = get_bloginfo('name');
            if (is_string($name) && $name !== '') {
                return $name;
            }
        }

        return $this->siteUrl();
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

    /**
     * Build a uniform result tuple.
     *
     * @param bool   $ok      Success flag.
     * @param int    $status  HTTP status (0 when no response).
     * @param string $code    Machine code.
     * @param string $message Human message (safe to surface to admins).
     * @return array{ok:bool,status:int,code:string,message:string}
     */
    private function result(bool $ok, int $status, string $code, string $message): array
    {
        return ['ok' => $ok, 'status' => $status, 'code' => $code, 'message' => $message];
    }
}
