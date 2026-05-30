<?php
/**
 * SyncLoginBrandCommand: receives a per-site login-brand config from the
 * control plane and persists it so LoginBrand applies it on the next
 * wp-login.php request.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/sync_login_brand
 *   Authorization: Bearer <Ed25519 JWT with cmd="sync_login_brand", aud=<siteId>>
 *   Content-Type: application/json
 *   Body: {
 *     "logo_url":  string,  // http(s) URL to the logo image, or "" to clear
 *     "logo_link": string,  // http(s) URL for the logo anchor, or "" to clear
 *     "message":   string   // HTML message to prepend to the login form, or "" to clear
 *   }
 *   All three fields are optional. Absent = treated as "" (no change to that
 *   field's semantics; LoginBrand::applyConfig() handles the empty-string case).
 *
 * Response (200 OK on success, wrapped by Router in WP_REST_Response):
 *   { "ok": true, "detail": "login brand applied" }
 *
 * Error responses follow the same { "ok": false, "detail": "<reason>" } envelope.
 *
 * Auth: the Router's permission_callback already enforces the Ed25519 + anti-
 * replay JWT contract (Connector::verifyCommand) before execute() is called.
 * This command validates only its own payload shape.
 *
 * The written wp-option (LoginBrand::OPTION = 'wpmgr_login_brand') holds JSON:
 *   { "logo_url": string, "logo_link": string, "message": string }
 * URL fields are validated to http/https only; message is wp_kses'd with a
 * narrow allowlist before storage. See LoginBrand::applyConfig() for details.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\LoginBrand;

/**
 * Persists a CP-pushed login-brand config (logo URL, logo link, message) into
 * wp-options and makes it available to LoginBrand's hook callbacks.
 */
final class SyncLoginBrandCommand implements CommandInterface
{
    private LoginBrand $loginBrand;

    /**
     * @param LoginBrand $loginBrand The shared LoginBrand instance. Its
     *   applyConfig() method validates + writes the wp-option and clears the
     *   per-instance cache so the login-page hooks in the same request (if any)
     *   would pick up the new config.
     */
    public function __construct(LoginBrand $loginBrand)
    {
        $this->loginBrand = $loginBrand;
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'sync_login_brand';
    }

    /**
     * {@inheritDoc}
     *
     * Accepts (all optional, default to ""):
     *   - logo_url  (string): http(s) URL to the login-page logo image.
     *   - logo_link (string): http(s) URL the logo should link to.
     *   - message   (string): HTML block to prepend to the login form.
     *
     * Type-checks all three fields before delegating to LoginBrand::applyConfig(),
     * which performs URL scheme validation and message kses sanitization. Invalid
     * non-empty URLs are silently coerced to empty string by applyConfig() rather
     * than returning an error, to avoid leaving the agent in a state where a
     * single malformed push prevents all future brand updates.
     *
     * Returns { "ok": bool, "detail": string } matching the sibling command
     * envelope (sync_error_config, sync_security_config, etc.).
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused here;
     *   Router already enforced aud + cmd binding before dispatch).
     * @param array<string,mixed> $params Decoded JSON body from the CP.
     * @return array{ok:bool,detail:string}
     */
    public function execute(array $claims, array $params): array
    {
        // All three fields are optional strings; absent = "".
        $logoUrl  = '';
        $logoLink = '';
        $message  = '';

        if (array_key_exists('logo_url', $params)) {
            if (!is_string($params['logo_url'])) {
                return ['ok' => false, 'detail' => 'logo_url must be a string'];
            }
            $logoUrl = $params['logo_url'];
        }

        if (array_key_exists('logo_link', $params)) {
            if (!is_string($params['logo_link'])) {
                return ['ok' => false, 'detail' => 'logo_link must be a string'];
            }
            $logoLink = $params['logo_link'];
        }

        if (array_key_exists('message', $params)) {
            if (!is_string($params['message'])) {
                return ['ok' => false, 'detail' => 'message must be a string'];
            }
            $message = $params['message'];
        }

        try {
            $this->loginBrand->applyConfig($logoUrl, $logoLink, $message);
        } catch (\Throwable $e) {
            // Never let the config sync fatal the request.
            return ['ok' => false, 'detail' => 'failed to apply login brand'];
        }

        return ['ok' => true, 'detail' => 'login brand applied'];
    }
}
