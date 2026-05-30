<?php
/**
 * SyncSecurityConfigCommand (S2): receives a per-site security config from the
 * control plane and persists it so LoginProtection honours it on the next request.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/sync_security_config
 *   Authorization: Bearer <Ed25519 JWT with cmd="sync_security_config", aud=<siteId>>
 *   Content-Type: application/json
 *   Body: {
 *     "mode": "disabled"|"audit"|"protect",
 *     "thresholds": {
 *       "captcha_limit":    <int>,   // failed attempts before captcha gate (default 3)
 *       "temp_block_limit": <int>,   // failed attempts before temp block (default 10)
 *       "block_all_limit":  <int>,   // global failures before all-blocked (default 100)
 *       "failed_login_gap": <int>,   // look-back window for failures, seconds (default 1800)
 *       "success_login_gap":<int>,   // look-back window for successes, seconds (default 1800)
 *       "all_blocked_gap":  <int>    // look-back window for global block, seconds (default 1800)
 *     },
 *     "ip_header":   "<string>",     // $_SERVER key for client IP (default REMOTE_ADDR)
 *     "allow_cidrs": ["<cidr>"],     // always-bypass ranges
 *     "deny_cidrs":  ["<cidr>"]      // always-block ranges (WAF mu-plugin gate)
 *   }
 *
 * Response (200 OK on success, wrapped by Router in WP_REST_Response):
 *   { "ok": true, "detail": "security config applied" }
 *
 * Error responses follow the same { "ok": false, "detail": "<reason>" } envelope.
 *
 * Auth: the Router's permission_callback already enforces the Ed25519 + anti-
 * replay JWT contract (Connector::verifyCommand) before execute() is called.
 * This command validates only its own payload shape.
 *
 * The written wp-option (LoginProtection::OPTION_CONFIG = 'wpmgr_security_config')
 * holds a compact JSON encoding of the validated config object. LoginProtection's
 * buildConfig() is the canonical validator for both read and write paths; we
 * delegate to applyConfig() here so validation is never duplicated.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\LoginProtection;

/**
 * Persists a CP-pushed security config into wp-options and immediately applies
 * it to the running LoginProtection instance.
 */
final class SyncSecurityConfigCommand implements CommandInterface
{
    private LoginProtection $loginProtection;

    /**
     * @param LoginProtection $loginProtection The shared login-protection instance.
     *   Its applyConfig() method validates the full config object, writes the
     *   wp-option, and clears the per-instance config cache so subsequent
     *   decisions in this request see the new values.
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
        return 'sync_security_config';
    }

    /**
     * {@inheritDoc}
     *
     * Accepts:
     *   - mode (required, string): one of "disabled", "audit", "protect".
     *   - thresholds (optional, array): per-threshold overrides; invalid or
     *     missing keys default to the pinned defaults inside LoginProtection.
     *   - ip_header (optional, string): $_SERVER key for client IP resolution.
     *   - allow_cidrs (optional, string[]): CIDR ranges to always bypass.
     *   - deny_cidrs (optional, string[]): CIDR ranges to always block.
     *
     * All fields are re-validated inside LoginProtection::applyConfig() (via
     * buildConfig()). Invalid values are replaced with safe defaults rather
     * than returning an error, so a malformed push can never brick the agent.
     * We pre-validate `mode` here so the caller gets a clear rejection message
     * instead of silently resetting to "protect".
     *
     * Returns { "ok": bool, "detail": string } to match the sibling command
     * envelope (sync_error_config, refresh_inventory, etc.).
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused here;
     *   Router already enforced aud + cmd binding before dispatch).
     * @param array<string,mixed> $params Decoded JSON body from the CP.
     * @return array{ok:bool,detail:string}
     */
    public function execute(array $claims, array $params): array
    {
        // mode is required and must be one of the three valid strings.
        if (!array_key_exists('mode', $params)) {
            return ['ok' => false, 'detail' => 'missing required field: mode'];
        }

        $mode = $params['mode'];
        if (!is_string($mode)) {
            return ['ok' => false, 'detail' => 'mode must be a string'];
        }

        $validModes = [
            LoginProtection::MODE_DISABLED,
            LoginProtection::MODE_AUDIT,
            LoginProtection::MODE_PROTECT,
        ];
        if (!in_array($mode, $validModes, true)) {
            return ['ok' => false, 'detail' => 'mode must be one of: disabled, audit, protect'];
        }

        // thresholds is optional; must be an array if present.
        if (array_key_exists('thresholds', $params) && !is_array($params['thresholds'])) {
            return ['ok' => false, 'detail' => 'thresholds must be an object'];
        }

        // allow_cidrs / deny_cidrs are optional; must be arrays if present.
        if (array_key_exists('allow_cidrs', $params) && !is_array($params['allow_cidrs'])) {
            return ['ok' => false, 'detail' => 'allow_cidrs must be an array'];
        }
        if (array_key_exists('deny_cidrs', $params) && !is_array($params['deny_cidrs'])) {
            return ['ok' => false, 'detail' => 'deny_cidrs must be an array'];
        }

        // ip_header is optional; must be a non-empty string if present.
        if (array_key_exists('ip_header', $params)
            && (!is_string($params['ip_header']) || trim($params['ip_header']) === '')
        ) {
            return ['ok' => false, 'detail' => 'ip_header must be a non-empty string'];
        }

        try {
            // Delegate full validation + persistence to LoginProtection. The raw
            // $params array is passed directly; buildConfig() inside applyConfig()
            // validates and defaults every field so partial payloads are safe.
            $this->loginProtection->applyConfig($params);
        } catch (\Throwable $e) {
            // Never let the config sync fatal the request.
            return ['ok' => false, 'detail' => 'failed to apply security config'];
        }

        return ['ok' => true, 'detail' => 'security config applied'];
    }
}
