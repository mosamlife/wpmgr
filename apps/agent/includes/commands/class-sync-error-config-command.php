<?php
/**
 * SyncErrorConfigCommand (S1.2): receives a per-site error config from the
 * control plane and persists it so ErrorMonitor honours it on the next request.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/sync_error_config
 *   Authorization: Bearer <Ed25519 JWT with cmd="sync_error_config", aud=<siteId>>
 *   Content-Type: application/json
 *   Body: {
 *     "error_level": <int>,       // E_* bitmask — which non-fatal codes to capture
 *     "ignore_md5s": ["<32hex>"]  // fingerprints to drop entirely
 *   }
 *
 * Response (200 OK on success, wrapped by Router in WP_REST_Response):
 *   { "ok": true, "detail": "error config applied" }
 *
 * Error responses follow the same { "ok": false, "detail": "<reason>" } envelope.
 *
 * Auth: the Router's permission_callback already enforces the Ed25519 + anti-
 * replay JWT contract (Connector::verifyCommand) before execute() is called.
 * This command validates only its own payload shape.
 *
 * The written wp-option (OPTION_CONFIG = 'wpmgr_error_config') holds JSON:
 *   { "error_level": <int>, "ignore_md5s": ["<32hex>", ...] }
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\ErrorMonitor;

/**
 * Persists a CP-pushed error config (capture level + ignore fingerprints) into
 * wp-options and immediately applies it to the running ErrorMonitor.
 */
final class SyncErrorConfigCommand implements CommandInterface
{
    private ErrorMonitor $errorMonitor;

    /**
     * @param ErrorMonitor $errorMonitor The shared error monitor instance. Its
     *   applyConfig() method validates + writes the wp-option and clears the
     *   per-request static cache so subsequent record() calls in this request
     *   pick up the new config.
     */
    public function __construct(ErrorMonitor $errorMonitor)
    {
        $this->errorMonitor = $errorMonitor;
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'sync_error_config';
    }

    /**
     * {@inheritDoc}
     *
     * Accepts:
     *   - error_level (required, int): E_* bitmask for non-fatal capture.
     *   - ignore_md5s (optional, string[]): fingerprints to silence globally.
     *
     * Both fields are re-validated inside ErrorMonitor::applyConfig(); invalid
     * values are clamped to safe defaults rather than returning an error, to
     * avoid leaving the agent in a non-functional state on a malformed push.
     *
     * Returns { "ok": bool, "detail": string } to match the sibling command
     * envelope (refresh_inventory, etc.).
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused here;
     *   Router already enforced aud + cmd binding before dispatch).
     * @param array<string,mixed> $params Decoded JSON body from the CP.
     * @return array{ok:bool,detail:string}
     */
    public function execute(array $claims, array $params): array
    {
        // error_level is required and must be an integer.
        if (!array_key_exists('error_level', $params)) {
            return ['ok' => false, 'detail' => 'missing required field: error_level'];
        }

        $rawLevel = $params['error_level'];
        if (!is_int($rawLevel)) {
            return ['ok' => false, 'detail' => 'error_level must be an integer'];
        }

        // ignore_md5s is optional; default to an empty array.
        $rawMd5s = [];
        if (array_key_exists('ignore_md5s', $params)) {
            if (!is_array($params['ignore_md5s'])) {
                return ['ok' => false, 'detail' => 'ignore_md5s must be an array'];
            }
            $rawMd5s = $params['ignore_md5s'];
        }

        try {
            $this->errorMonitor->applyConfig($rawLevel, $rawMd5s);
        } catch (\Throwable $e) {
            // Never let the config sync fatal the request.
            return ['ok' => false, 'detail' => 'failed to apply config'];
        }

        return ['ok' => true, 'detail' => 'error config applied'];
    }
}
