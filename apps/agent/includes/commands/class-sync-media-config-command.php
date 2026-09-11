<?php
/**
 * SyncMediaConfigCommand (ADR-044): receives a per-site media auto-optimize
 * config from the control plane and persists it to typed wp-options so the
 * upload filter can read the enable flag locally on the fast path.
 *
 * Wire contract (CP → agent) — matches media_config_contract.go exactly:
 *   POST /wp-json/wpmgr/v1/command/sync_media_config
 *   Authorization: Bearer <Ed25519 JWT, cmd="sync_media_config", aud=<siteId>>
 *   Content-Type: application/json
 *   Body: {
 *     "enabled":        <bool>,               // auto-optimize on upload toggle
 *     "target_format":  "avif"|"webp"|"original",
 *     "target_quality": "lossy"|"lossless"
 *   }
 *
 * Response (200 OK on success, wrapped by Router in WP_REST_Response):
 *   { "ok": true, "detail": "applied" }
 *
 * Error responses follow the same { "ok": false, "detail": "<reason>" } envelope.
 *
 * Auth: the Router's permission_callback already enforces the Ed25519 + anti-
 * replay JWT contract (Connector::verifyCommand) before execute() is called.
 * This command validates only its own payload shape.
 *
 * The stored options are:
 *   wpmgr_media_auto_optimize  (bool)   — Settings::OPTION_MEDIA_AUTO_OPTIMIZE
 *   wpmgr_media_auto_format    (string) — Settings::OPTION_MEDIA_AUTO_FORMAT
 *   wpmgr_media_auto_quality   (string) — Settings::OPTION_MEDIA_AUTO_QUALITY
 *
 * IMPORTANT: the actual format/quality used for each encode is ALWAYS re-read
 * CP-side in HandleAutoOptimize (ADR-044 §4) — a stale agent option can never
 * select an invalid format. These options are informational fast-path only.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Settings;

/**
 * Persists a CP-pushed media config into typed wp-options.
 */
final class SyncMediaConfigCommand implements CommandInterface
{
    /** Valid target_format values (mirrors domain.go ValidTargetFormat). */
    private const VALID_FORMATS = ['avif', 'webp', 'original'];

    /** Valid target_quality values (mirrors domain.go ValidTargetQuality). */
    private const VALID_QUALITIES = ['lossy', 'lossless'];

    private Settings $settings;

    /**
     * @param Settings $settings Typed accessor over the agent's wp-options.
     */
    public function __construct(Settings $settings)
    {
        $this->settings = $settings;
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'sync_media_config';
    }

    /**
     * Effect: persists the auto-optimize enable flag, target format and target quality.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Write;
    }

    /**
     * Repeatability: writing the same configuration twice converges.
     *
     * @return CommandRepeatability
     */
    public function repeatability(): CommandRepeatability
    {
        return CommandRepeatability::Idempotent;
    }

    /**
     * {@inheritDoc}
     *
     * Accepts (field names match media_config_contract.go exactly):
     *   - enabled        (required, bool)   — auto-optimize toggle.
     *   - target_format  (optional, string) — encode target; validated against
     *                                         VALID_FORMATS; defaults to "webp".
     *   - target_quality (optional, string) — encode quality; validated against
     *                                         VALID_QUALITIES; defaults to "lossy".
     *
     * Invalid optional values are replaced with safe defaults so a malformed
     * push can never brick the agent. `enabled` is required so a bare `{}` body
     * can never accidentally toggle the feature on.
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused; Router
     *   already enforced aud + cmd binding before dispatch).
     * @param array<string,mixed> $params Decoded JSON body from the CP.
     * @return array{ok:bool,detail:string}
     */
    public function execute(array $claims, array $params): array
    {
        // `enabled` is required and must be bool (or int 0/1 for JSON safety).
        if (!array_key_exists('enabled', $params)) {
            return ['ok' => false, 'detail' => 'missing required field: enabled'];
        }

        $enabled = $params['enabled'];
        if (!is_bool($enabled) && !is_int($enabled)) {
            return ['ok' => false, 'detail' => 'enabled must be a boolean'];
        }

        // `target_format` is optional; validate against the allowed set or default.
        $format = '';
        if (array_key_exists('target_format', $params)) {
            if (!is_string($params['target_format'])) {
                return ['ok' => false, 'detail' => 'target_format must be a string'];
            }
            $format = $params['target_format'];
            if ($format !== '' && !in_array($format, self::VALID_FORMATS, true)) {
                return ['ok' => false, 'detail' => 'target_format must be one of: avif, webp, original'];
            }
        }
        if ($format === '') {
            $format = 'webp'; // safe default matches CP server-side default
        }

        // `target_quality` is optional; validate against the allowed set or default.
        $quality = '';
        if (array_key_exists('target_quality', $params)) {
            if (!is_string($params['target_quality'])) {
                return ['ok' => false, 'detail' => 'target_quality must be a string'];
            }
            $quality = $params['target_quality'];
            if ($quality !== '' && !in_array($quality, self::VALID_QUALITIES, true)) {
                return ['ok' => false, 'detail' => 'target_quality must be one of: lossy, lossless'];
            }
        }
        if ($quality === '') {
            $quality = 'lossy'; // safe default
        }

        try {
            $this->settings->setMediaAutoOptimize((bool) $enabled);
            $this->settings->setMediaAutoFormat($format);
            $this->settings->setMediaAutoQuality($quality);
        } catch (\Throwable $e) {
            // Never let a config sync fatal the request.
            return ['ok' => false, 'detail' => 'failed to persist media config'];
        }

        return ['ok' => true, 'detail' => 'applied'];
    }
}
