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

    /**
     * Agent-authenticated last-will path (ADR-040). The agent posts a SIGNED
     * disconnect here on deactivate/uninstall so the CP can flip the site to
     * `disconnected` immediately instead of waiting out the heartbeat-miss
     * window. Best-effort: a failure here never blocks deactivation/uninstall.
     */
    public const PATH_DISCONNECT = '/agent/v1/disconnect';

    /** Default outbound request timeout, in seconds. */
    private const TIMEOUT = 15;

    /**
     * Last-will request timeout, in seconds (ADR-040). Deliberately tiny: the
     * disconnect runs INSIDE the WP deactivate/uninstall request, which must
     * complete even if the control plane is unreachable. Three seconds bounds
     * the worst-case admin-page stall.
     */
    public const DISCONNECT_TIMEOUT = 3;

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
                return 'Pairing code is invalid or expired. Re-paste the code; ensure there\'s no extra whitespace.';
            case 403:
                // Signature/auth rejection at enroll time almost always means a
                // mangled (whitespace-padded) code. Steer the operator to re-paste.
                return 'Enrollment was rejected. Re-paste the code; ensure there\'s no extra whitespace.';
            case 409:
                return 'Pairing code already used, or this site is owned elsewhere.';
            case 410:
                // Live-enrollment (Phase 3): a consumed or expired site-bound code.
                return 'This code expired or was already used. Request a new code from your dashboard.';
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
     * Build the light heartbeat payload (ADR-039). The CP currently uses only
     * liveness, but we send a forward-compatible status blob so a future CP can
     * surface drift without a separate metadata pull. Kept cheap: no WP.org
     * polling, no directory walks — this fires every 60s.
     *
     * @return array{
     *     site_id:string,
     *     ts:int,
     *     status:string,
     *     wp_version:string,
     *     php_memory:string,
     *     plugin_versions:array<string,string>,
     *     installed_updates_count:int,
     *     multisite:bool
     * }
     */
    public function buildHeartbeatPayload(): array
    {
        return [
            'site_id'                 => $this->settings->siteId(),
            'ts'                      => time(),
            'status'                  => 'ok',
            'wp_version'              => $this->wpVersion(),
            'php_memory'              => (string) ini_get('memory_limit'),
            'plugin_versions'         => $this->pluginVersions(),
            'installed_updates_count' => $this->installedUpdatesCount(),
            'multisite'               => function_exists('is_multisite') ? (bool) is_multisite() : false,
        ];
    }

    /**
     * Send a heartbeat (signed). Reads the response for control-plane
     * instructions (ADR-039) and reports any back to the caller so the
     * scheduler can act on a queued "revoke".
     *
     * The CP response is EITHER:
     *   - 200 with JSON `{"ok":true,"instructions":[...],"revoke_token":"<jwt>"}`
     *     (lifecycle wired), or
     *   - 204 / empty (legacy liveness-only) — handled gracefully, no error.
     *
     * For a revoked site the CP returns BOTH `instructions:["revoke"]` and a
     * signed `revoke_token` (a compact Ed25519 JWT; cmd="revoke", aud=site_id).
     * The token is carried back UNVERIFIED here — the caller (Lifecycle) verifies
     * it through the existing Connector before acting (Phase-6 finding B). We
     * never act on the revoke from this body alone.
     *
     * @return array{
     *     ok:bool,
     *     status:int,
     *     code:string,
     *     message:string,
     *     instructions:array<int,string>,
     *     revoke_token:string
     * }
     */
    public function sendHeartbeat(): array
    {
        if (!$this->settings->isEnrolled()) {
            $notEnrolled = $this->result(false, 0, 'not_enrolled', 'Not enrolled.');
            return [
                'ok'           => $notEnrolled['ok'],
                'status'       => $notEnrolled['status'],
                'code'         => $notEnrolled['code'],
                'message'      => $notEnrolled['message'],
                'instructions' => [],
                'revoke_token' => '',
            ];
        }

        $body = (string) wp_json_encode($this->buildHeartbeatPayload());

        $result = $this->signedPost(self::PATH_HEARTBEAT, $body);
        if ($result['ok']) {
            $this->settings->setLastHeartbeat(time());
        }

        return [
            'ok'           => $result['ok'],
            'status'       => $result['status'],
            'code'         => $result['code'],
            'message'      => $result['message'],
            'instructions' => $this->parseInstructions($result['raw_body']),
            'revoke_token' => $this->parseRevokeToken($result['raw_body']),
        ];
    }

    /**
     * Send a SIGNED best-effort last-will disconnect (ADR-040). Posted by the
     * deactivate/uninstall lifecycle hooks so the CP can flip the site to
     * `disconnected` immediately. Runs through the SAME signed-request path as
     * every other agent→CP call (the CP verifies the Ed25519 signature before
     * acting), with a 3-second timeout so it can never hang the WP request.
     *
     * Any failure (unreachable CP, 503 lifecycle_disabled on an un-wired CP,
     * timeout) is swallowed — the disconnect is advisory, not load-bearing.
     *
     * @param string $reason One of 'deactivated' | 'uninstalled' | 'user_initiated'.
     * @return array{ok:bool,status:int,code:string,message:string}
     */
    public function disconnect(string $reason): array
    {
        if (!$this->settings->isEnrolled()) {
            return $this->result(false, 0, 'not_enrolled', 'Not enrolled.');
        }

        $allowed = ['deactivated', 'uninstalled', 'user_initiated'];
        if (!in_array($reason, $allowed, true)) {
            $reason = 'user_initiated';
        }

        $body = (string) wp_json_encode([
            'site_id' => $this->settings->siteId(),
            'reason'  => $reason,
        ]);

        return $this->signedPost(self::PATH_DISCONNECT, $body, self::DISCONNECT_TIMEOUT);
    }

    /**
     * Parse the heartbeat response body into a list of instruction strings.
     * Tolerates an empty body (legacy 204), a non-JSON body, and a body whose
     * `instructions` is absent or not a list — all yield an empty array.
     *
     * @param string $rawBody Raw HTTP response body.
     * @return array<int,string> Instruction tokens (e.g. ['revoke']).
     */
    private function parseInstructions(string $rawBody): array
    {
        $rawBody = trim($rawBody);
        if ($rawBody === '') {
            return [];
        }
        $decoded = json_decode($rawBody, true);
        if (!is_array($decoded) || !isset($decoded['instructions']) || !is_array($decoded['instructions'])) {
            return [];
        }
        $out = [];
        foreach ($decoded['instructions'] as $instruction) {
            if (is_string($instruction) && $instruction !== '') {
                $out[] = $instruction;
            }
        }
        return $out;
    }

    /**
     * Extract the signed `revoke_token` (a compact Ed25519 JWT) from the
     * heartbeat response body, if present. Returns '' for an empty/non-JSON
     * body or when the field is absent or not a non-empty string. The token is
     * NOT verified here — that is the Lifecycle gate's job (it runs the token
     * through the existing Connector before any teardown). Carrying it back
     * raw keeps verification in one place and this parser dumb + side-effect-free.
     *
     * @param string $rawBody Raw HTTP response body.
     * @return string The compact JWT, or '' when absent.
     */
    private function parseRevokeToken(string $rawBody): string
    {
        $rawBody = trim($rawBody);
        if ($rawBody === '') {
            return '';
        }
        $decoded = json_decode($rawBody, true);
        if (!is_array($decoded) || !isset($decoded['revoke_token']) || !is_string($decoded['revoke_token'])) {
            return '';
        }
        return trim($decoded['revoke_token']);
    }

    /**
     * This site's reported WordPress version (best-effort).
     *
     * @return string
     */
    private function wpVersion(): string
    {
        if (isset($GLOBALS['wp_version']) && is_scalar($GLOBALS['wp_version'])) {
            return (string) $GLOBALS['wp_version'];
        }
        if (function_exists('get_bloginfo')) {
            $v = get_bloginfo('version');
            if (is_string($v)) {
                return $v;
            }
        }
        return '';
    }

    /**
     * A compact slug=>version map of installed plugins for the heartbeat. Cheap
     * (reads the already-loaded plugin headers; no WP.org polling). Returns an
     * empty map when get_plugins() is unavailable (e.g. front-end-only request).
     *
     * @return array<string,string>
     */
    private function pluginVersions(): array
    {
        if (!function_exists('get_plugins')) {
            $adminInc = defined('ABSPATH') ? ABSPATH . 'wp-admin/includes/plugin.php' : '';
            if ($adminInc !== '' && is_readable($adminInc)) {
                require_once $adminInc;
            }
        }
        if (!function_exists('get_plugins')) {
            return [];
        }
        $map = [];
        /** @var array<string,array<string,mixed>> $plugins */
        $plugins = get_plugins();
        foreach ($plugins as $file => $data) {
            $version = isset($data['Version']) && is_scalar($data['Version']) ? (string) $data['Version'] : '';
            $map[(string) $file] = $version;
        }
        return $map;
    }

    /**
     * Count of currently-available updates (plugins + themes + core). Reads the
     * update transients WordPress already maintains; does NOT force a refresh
     * (that is the 30-min metadata cron's job) so the 60s heartbeat stays light.
     *
     * @return int
     */
    private function installedUpdatesCount(): int
    {
        $count = 0;

        if (function_exists('get_site_transient')) {
            $plugins = get_site_transient('update_plugins');
            if (is_object($plugins) && isset($plugins->response) && is_array($plugins->response)) {
                $count += count($plugins->response);
            }
            $themes = get_site_transient('update_themes');
            if (is_object($themes) && isset($themes->response) && is_array($themes->response)) {
                $count += count($themes->response);
            }
            $core = get_site_transient('update_core');
            if (is_object($core) && isset($core->updates) && is_array($core->updates)) {
                foreach ($core->updates as $update) {
                    if (is_object($update) && isset($update->response) && $update->response === 'upgrade') {
                        $count++;
                    }
                }
            }
        }

        return $count;
    }

    /**
     * Perform an agent-authenticated POST: sign the canonical message and send
     * the four X-WPMgr-* headers.
     *
     * @param string   $path    Request path only (no host/query).
     * @param string   $body    Raw JSON body.
     * @param int|null $timeout Per-request timeout override (seconds); defaults
     *                          to self::TIMEOUT. The last-will path passes a
     *                          short 3s budget so it never hangs deactivation.
     * @return array{ok:bool,status:int,code:string,message:string,raw_body:string}
     */
    private function signedPost(string $path, string $body, ?int $timeout = null): array
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
                'timeout' => $timeout ?? self::TIMEOUT,
                'headers' => $headers,
                'body'    => $body,
            ]
        );

        if ($this->isWpError($response)) {
            return $this->result(false, 0, 'unreachable', 'Control plane is unreachable.');
        }

        $status  = (int) wp_remote_retrieve_response_code($response);
        $rawBody = (string) wp_remote_retrieve_body($response);
        if ($status >= 200 && $status < 300) {
            $ok = $this->result(true, $status, 'ok', 'OK.');
            $ok['raw_body'] = $rawBody;
            return $ok;
        }

        $message = 'Request failed (HTTP ' . $status . ').';
        $detail  = $this->summarizeBody($rawBody);
        if ($detail !== '') {
            $message .= ' ' . $detail;
        }

        $err = $this->result(false, $status, 'http_' . $status, $message);
        $err['raw_body'] = $rawBody;
        return $err;
    }

    /**
     * Build a short, safe, single-line summary of a control-plane error
     * response body for surfacing in an admin notice. Prefers the CP's JSON
     * {code,message} shape, falls back to the raw body. Always truncated and
     * collapsed to a single line; the caller still escapes for output.
     *
     * @param string $rawBody Raw HTTP response body.
     * @return string
     */
    private function summarizeBody(string $rawBody): string
    {
        $rawBody = trim($rawBody);
        if ($rawBody === '') {
            return '';
        }

        $decoded = json_decode($rawBody, true);
        if (is_array($decoded)) {
            $code    = isset($decoded['code']) && is_scalar($decoded['code']) ? (string) $decoded['code'] : '';
            $message = isset($decoded['message']) && is_scalar($decoded['message']) ? (string) $decoded['message'] : '';
            if ($message !== '') {
                $summary = $code !== '' ? $code . ': ' . $message : $message;
                return $this->clampLine($summary, 300);
            }
        }

        return $this->clampLine($rawBody, 300);
    }

    /**
     * Collapse whitespace to single spaces and truncate to at most $max
     * characters (multibyte-safe), appending an ellipsis when clipped.
     *
     * @param string $text Input text.
     * @param int    $max  Maximum length in characters.
     * @return string
     */
    private function clampLine(string $text, int $max): string
    {
        $text = (string) preg_replace('/\s+/u', ' ', trim($text));
        if (function_exists('mb_strlen') && mb_strlen($text) > $max) {
            return rtrim(mb_substr($text, 0, $max)) . '…';
        }
        if (strlen($text) > $max) {
            return rtrim(substr($text, 0, $max)) . '…';
        }

        return $text;
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
     * @return array{ok:bool,status:int,code:string,message:string,raw_body:string}
     */
    private function result(bool $ok, int $status, string $code, string $message): array
    {
        return ['ok' => $ok, 'status' => $status, 'code' => $code, 'message' => $message, 'raw_body' => ''];
    }
}
