<?php
/**
 * SyncSecurityHardeningCommand — receives the hardening config + ban list from
 * the control plane and atomically applies it on the WordPress site.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/sync_security_hardening
 *   Authorization: Bearer <Ed25519 JWT with cmd="sync_security_hardening", aud=<siteId>>
 *   Content-Type: application/json
 *   Body: {
 *     "config": {
 *       "disable_file_editor":        <bool>,
 *       "xmlrpc_mode":                "on"|"off"|"limited",
 *       "restrict_rest_api":          "default"|"restricted",
 *       "restrict_login_identifier":  "username"|"email"|"both",
 *       "force_unique_nickname":      <bool>,
 *       "disable_author_archive_enum": <bool>,
 *       "force_ssl":                  <bool>,
 *       "disable_directory_browsing": <bool>,
 *       "disable_php_in_uploads":     <bool>,
 *       "protect_system_files":       <bool>
 *     },
 *     "bans": [
 *       {"id":"<uuid>","type":"ip","value":"1.2.3.4","comment":"..."},
 *       {"id":"<uuid>","type":"range","value":"10.0.0.0/8","comment":"..."},
 *       {"id":"<uuid>","type":"user_agent","value":"badbot/1.0"}
 *     ]
 *   }
 *
 * Response (200 OK, wrapped by Router):
 *   { "ok": true, "detail": "applied" }
 *
 * Auth: Router's permission_callback enforces the Ed25519 + anti-replay JWT
 * contract (Connector::verifyCommand) before execute() is called.
 *
 * On every push the agent replaces its local hardening config + ban list
 * ATOMICALLY (single update_option write). Missing toggles default to off for
 * forward-compat (HardeningConfig::fromArray() handles this).
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Security\HardeningConfig;
use WPMgr\Agent\Security\HardeningModule;

/**
 * Persists and applies the CP-pushed hardening config + ban list.
 */
final class SyncSecurityHardeningCommand implements CommandInterface
{
    private HardeningModule $module;

    /**
     * @param HardeningModule $module The shared hardening-module instance.
     */
    public function __construct(HardeningModule $module)
    {
        $this->module = $module;
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'sync_security_hardening';
    }

    /**
     * {@inheritDoc}
     *
     * Accepts the full wire contract body described in the file docblock.
     * Top-level validation is minimal: we require `config` to be an object (if
     * present) and `bans` to be an array (if present). All per-field validation
     * and safe-defaulting is delegated to HardeningConfig::fromArray() so this
     * command can never brick the agent on a malformed push.
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused; Router
     *   already enforced aud + cmd binding before dispatch).
     * @param array<string,mixed> $params Decoded JSON body from the CP.
     * @return array{ok:bool,detail:string}
     */
    public function execute(array $claims, array $params): array
    {
        // 'config' key must be an object / associative array if provided.
        if (array_key_exists('config', $params) && !is_array($params['config'])) {
            return ['ok' => false, 'detail' => 'config must be an object'];
        }

        // 'bans' key must be an array if provided.
        if (array_key_exists('bans', $params) && !is_array($params['bans'])) {
            return ['ok' => false, 'detail' => 'bans must be an array'];
        }

        try {
            // Build and validate the full config; safe defaults for any missing/
            // invalid field (never throws, always returns a valid object).
            $config = HardeningConfig::fromArray($params);

            // Persist and apply. applyConfig() writes the wp-option atomically
            // and refreshes the server-config block.
            $persisted = $this->module->applyConfig($config);
            if (!$persisted) {
                return ['ok' => false, 'detail' => 'failed to persist hardening config'];
            }

            // Sync DISALLOW_FILE_EDIT in wp-config.php. Returns false when
            // wp-config is not writable — we proceed anyway (runtime filter in
            // HardeningModule::install() is the defence-in-depth fallback) and
            // note it in the detail so the dashboard can surface the caveat.
            $wpConfigOk = $this->module->syncWpConfigFileEdit($config);

            // Merge IP/range bans into the WAF mu-plugin's deny_cidrs so
            // they take effect at the earliest possible PHP boot point.
            $this->module->syncWafDenyCidrs($config);
        } catch (\Throwable $e) {
            // Never let the config sync fatal the request.
            return ['ok' => false, 'detail' => 'failed to apply hardening config'];
        }

        $detail = 'applied';
        if ($config->disableFileEditor && !$wpConfigOk) {
            $detail = 'applied; wp-config.php not writable — disable_file_editor via runtime filter only';
        } elseif ($config->disableFileEditor) {
            // Informational only (GH #268): e.g. on Roots/Bedrock the constant
            // is already enforced elsewhere and wp-config.php was left
            // untouched. Never surfaced as a failure — $wpConfigOk is already
            // true here.
            $notice = $this->module->lastWpConfigNotice();
            if ($notice !== null) {
                $detail = 'applied; ' . $notice;
            }
        }

        return ['ok' => true, 'detail' => $detail];
    }
}
