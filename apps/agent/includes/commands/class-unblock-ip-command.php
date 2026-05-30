<?php
/**
 * UnblockIpCommand (S2): removes the brute-force block state for a specific IP.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/unblock_ip
 *   Authorization: Bearer <Ed25519 JWT with cmd="unblock_ip", aud=<siteId>>
 *   Content-Type: application/json
 *   Body: {
 *     "ip": "<IPv4 or IPv6 address string>"
 *   }
 *
 * Response (200 OK on success, wrapped by Router in WP_REST_Response):
 *   { "ok": true, "detail": "IP unblocked: <ip>" }
 *
 * Error responses follow the same { "ok": false, "detail": "<reason>" } envelope.
 *
 * Auth: the Router's permission_callback already enforces the Ed25519 + anti-
 * replay JWT contract (Connector::verifyCommand) before execute() is called.
 * This command validates only its own payload shape.
 *
 * Effect:
 *   1. Deletes the per-IP unblock transient (if set by a prior CAPTCHA-solve path).
 *   2. Deletes all failure rows for the IP from wpmgr_login_events, resetting the
 *      failure counter to zero so the next login attempt is no longer blocked.
 *
 * The operation is best-effort: a DB failure results in { "ok": false } but never
 * fatals the request.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\LoginProtection;

/**
 * Clears the brute-force block state for a specific IP address.
 */
final class UnblockIpCommand implements CommandInterface
{
    private LoginProtection $loginProtection;

    /**
     * @param LoginProtection $loginProtection The shared login-protection instance.
     *   Its unblockIp() method deletes the failure rows and the per-IP transient.
     */
    public function __construct(LoginProtection $loginProtection)
    {
        $this->loginProtection = $loginProtection;
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'unblock_ip';
    }

    /**
     * {@inheritDoc}
     *
     * Accepts:
     *   - ip (required, string): the IPv4 or IPv6 address to unblock.
     *
     * The address is passed through to LoginProtection::unblockIp() which trims
     * and validates it. An empty string after trimming is a no-op in unblockIp(),
     * so we reject it here to give the caller a clear rejection message.
     *
     * Returns { "ok": bool, "detail": string } to match the sibling command
     * envelope.
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused here;
     *   Router already enforced aud + cmd binding before dispatch).
     * @param array<string,mixed> $params Decoded JSON body from the CP.
     * @return array{ok:bool,detail:string}
     */
    public function execute(array $claims, array $params): array
    {
        // ip is required and must be a non-empty string.
        if (!array_key_exists('ip', $params)) {
            return ['ok' => false, 'detail' => 'missing required field: ip'];
        }

        $ip = $params['ip'];
        if (!is_string($ip)) {
            return ['ok' => false, 'detail' => 'ip must be a string'];
        }

        $ip = trim($ip);
        if ($ip === '') {
            return ['ok' => false, 'detail' => 'ip must not be empty'];
        }

        // Validate that the value parses as an IP address to reject obvious junk
        // (e.g. SQL fragments) before it reaches the DB layer.
        if (filter_var($ip, FILTER_VALIDATE_IP) === false) {
            return ['ok' => false, 'detail' => 'ip is not a valid IPv4 or IPv6 address'];
        }

        try {
            $this->loginProtection->unblockIp($ip);
        } catch (\Throwable $e) {
            return ['ok' => false, 'detail' => 'failed to unblock IP'];
        }

        return ['ok' => true, 'detail' => 'IP unblocked: ' . $ip];
    }
}
