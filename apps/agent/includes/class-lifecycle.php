<?php
/**
 * Lifecycle: connection-lifecycle behaviours (ADR-039/040/041).
 *
 * Owns the three lifecycle transitions that are NOT plain enrollment:
 *
 *   - revoke_self()   — the control plane asked (via a heartbeat instruction)
 *                       for this site to be disconnected: wipe keys, deactivate
 *                       the plugin, and persist a marker so the admin page can
 *                       explain "this site was disconnected from the dashboard."
 *   - heartbeat_now() — fire a single synchronous heartbeat right after a
 *                       successful enroll so the dashboard flips
 *                       pending_enrollment→connected within ~1s instead of
 *                       waiting out the first 60s cron tick.
 *   - on_deactivate() — send a SIGNED best-effort last-will disconnect
 *                       (reason=deactivated, 3s budget) but DO NOT wipe keys
 *                       (a deactivate may be temporary).
 *   - on_uninstall()  — STATIC entry (uninstall hooks cannot use $this): send a
 *                       SIGNED last-will (reason=uninstalled, 3s budget) THEN
 *                       wipe keys + drop the agent's options/transients.
 *
 * Every CP call goes through the existing signed-request path (Enrollment +
 * Signer); no signature is ever hand-rolled here. Last-wills are best-effort:
 * a failure must never block deactivation/uninstall of the plugin.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent;

use WPMgr\Agent\Commands\MetadataCommand;
use WPMgr\Agent\Support\AgeIdentity;

/**
 * Connection-lifecycle orchestration for the agent.
 */
final class Lifecycle
{
    /**
     * Option recording that the control plane revoked this site's connection.
     * Shape: ['reason' => string, 'at' => int]. Read by the admin page to show
     * a "disconnected from the dashboard" notice + a Re-enroll affordance.
     */
    public const OPTION_REVOKED = 'wpmgr_agent_revoked';

    /** Heartbeat instruction token: disconnect + deactivate this site. */
    public const INSTRUCTION_REVOKE = 'revoke';

    /** Reason recorded when a revoke arrives via a dashboard instruction. */
    public const REASON_REVOKED = 'user_initiated_from_dashboard';

    private Keystore $keystore;

    private Settings $settings;

    private Enrollment $enrollment;

    /**
     * @param Keystore   $keystore   Key material store (wiped on revoke/uninstall).
     * @param Settings   $settings   Enrollment/config state.
     * @param Enrollment $enrollment Signed outbound client (heartbeat + last-will).
     */
    public function __construct(Keystore $keystore, Settings $settings, Enrollment $enrollment)
    {
        $this->keystore   = $keystore;
        $this->settings   = $settings;
        $this->enrollment = $enrollment;
    }

    /**
     * Act on the instructions carried by a heartbeat response. Currently the
     * only instruction is "revoke"; unknown tokens are ignored (forward-compat).
     *
     * @param array<int,string> $instructions Tokens from the heartbeat response.
     * @return void
     */
    public function handleInstructions(array $instructions): void
    {
        foreach ($instructions as $instruction) {
            if ($instruction === self::INSTRUCTION_REVOKE) {
                $this->revokeSelf(self::REASON_REVOKED);
                // A revoke is terminal — the plugin is deactivating; stop here.
                return;
            }
        }
    }

    /**
     * Revoke this site's connection on the control plane's instruction (ADR-041
     * adjacent): fire a hook, wipe the binding keys, deactivate the plugin, and
     * persist a marker so the admin UI can explain the disconnect.
     *
     * Does NOT post a last-will: the revoke ORIGINATED at the CP, which already
     * knows the site is disconnected, so a disconnect POST would be redundant
     * (and the keys are about to be wiped anyway).
     *
     * @param string $reason Machine reason recorded in the marker.
     * @return void
     */
    public function revokeSelf(string $reason): void
    {
        if (function_exists('do_action')) {
            do_action('wpmgr_revoking_self', $reason);
        }

        // Persist the marker BEFORE wiping/deactivating so it survives even if a
        // later step throws. clearSiteIdentity() only removes the CP key + site
        // keypair, never our own options, so the marker is safe.
        if (function_exists('update_option')) {
            update_option(self::OPTION_REVOKED, ['reason' => $reason, 'at' => time()], false);
        }

        // Wipe the keys that bind this agent to the CP (ADR-041: re-enroll wipes
        // keys then enrolls fresh; a revoke is the CP-initiated half of that).
        $this->keystore->clearSiteIdentity();
        $this->settings->clearEnrollment();
        $this->settings->clearLastSyncTimestamps();

        $this->deactivateSelf();
    }

    /**
     * Fire exactly one synchronous heartbeat. Called right after a successful
     * enroll so the dashboard flips pending_enrollment→connected immediately.
     * Wrapped so a failed beat never bubbles into the enroll flow — the 60s
     * cron is the backstop. Returns the parsed instructions so the caller can,
     * in the unlikely event the CP revokes on the very first beat, act on them.
     *
     * @return array<int,string> Instructions returned by the heartbeat (usually []).
     */
    public function heartbeatNow(): array
    {
        try {
            $result = $this->enrollment->sendHeartbeat();
            $instructions = isset($result['instructions']) && is_array($result['instructions'])
                ? $result['instructions']
                : [];
            return $instructions;
        } catch (\Throwable $e) {
            return [];
        }
    }

    /**
     * Deactivate-hook behaviour (ADR-040): SIGNED best-effort last-will, then
     * clear scheduled events is handled by the caller. Does NOT wipe keys — a
     * deactivate may be temporary, and re-activation should resume cleanly.
     *
     * @return void
     */
    public function onDeactivate(): void
    {
        if (!$this->settings->isEnrolled()) {
            return;
        }
        try {
            $this->enrollment->disconnect('deactivated');
        } catch (\Throwable $e) {
            // Best-effort: deactivation must complete even if the CP is down.
        }
    }

    /**
     * Uninstall-hook entry point. MUST be static + free of $this — WordPress
     * invokes the uninstall callback in a bare context with no plugin instance.
     * Builds the minimal object graph it needs, posts a SIGNED last-will
     * (reason=uninstalled, 3s budget, best-effort), then wipes keys and drops
     * the agent's options/transients.
     *
     * @return void
     */
    public static function on_uninstall(): void
    {
        $keystore = new Keystore();
        $settings = new Settings();
        $signer   = new Signer($keystore);
        $enrollment = new Enrollment($keystore, $settings, $signer, new MetadataCommand(new AgeIdentity($keystore)));

        $lifecycle = new self($keystore, $settings, $enrollment);

        if ($settings->isEnrolled()) {
            try {
                $enrollment->disconnect('uninstalled');
            } catch (\Throwable $e) {
                // Best-effort — uninstall must complete regardless.
            }
        }

        $lifecycle->wipeAll();
    }

    /**
     * Wipe every key + persisted artefact this plugin owns. Used by uninstall.
     * Intentionally aggressive (uninstall = clean slate), but the age identity
     * is preserved by Keystore::clearSiteIdentity()'s contract elsewhere; on
     * uninstall we remove it too since the install is going away entirely.
     *
     * @return void
     */
    public function wipeAll(): void
    {
        $this->keystore->clearSiteIdentity();
        $this->settings->clearEnrollment();
        $this->settings->clearLastSyncTimestamps();

        if (!function_exists('delete_option')) {
            return;
        }

        foreach ($this->ownedOptions() as $option) {
            delete_option($option);
        }

        // Drop the one-shot admin-notice transient too, if present.
        if (function_exists('delete_transient')) {
            delete_transient('wpmgr_agent_notice');
        }

        // Clear any remaining scheduled events for the agent's hooks.
        if (function_exists('wp_clear_scheduled_hook')) {
            wp_clear_scheduled_hook(Scheduler::HOOK_HEARTBEAT);
            wp_clear_scheduled_hook(Scheduler::HOOK_METADATA);
            wp_clear_scheduled_hook(Scheduler::HOOK_SAFETY);
            wp_clear_scheduled_hook(Scheduler::HOOK_DIAGNOSTICS);
            wp_clear_scheduled_hook(Scheduler::HOOK_SIZES);
            wp_clear_scheduled_hook(Scheduler::HOOK_ACTIVITY_SHIP);
            wp_clear_scheduled_hook(Scheduler::HOOK_ERRORS_SHIP);
        }
    }

    /**
     * Read the revoked marker, if the CP has disconnected this site.
     *
     * @return array{reason:string,at:int}|null
     */
    public static function revokedMarker(): ?array
    {
        if (!function_exists('get_option')) {
            return null;
        }
        $raw = get_option(self::OPTION_REVOKED, null);
        if (!is_array($raw)) {
            return null;
        }
        return [
            'reason' => isset($raw['reason']) && is_scalar($raw['reason']) ? (string) $raw['reason'] : '',
            'at'     => isset($raw['at']) && is_numeric($raw['at']) ? (int) $raw['at'] : 0,
        ];
    }

    /**
     * Clear the revoked marker. Called when the operator re-enrolls so the
     * "you were disconnected" notice does not persist past a fresh pairing.
     *
     * @return void
     */
    public static function clearRevokedMarker(): void
    {
        if (function_exists('delete_option')) {
            delete_option(self::OPTION_REVOKED);
        }
    }

    /**
     * Deactivate this plugin. Mirrors Scheduler::runSafetyCheck so the include
     * + capability are present even when called from a cron worker context.
     *
     * @return void
     */
    private function deactivateSelf(): void
    {
        if (!defined('WPMGR_AGENT_FILE') || !function_exists('plugin_basename')) {
            return;
        }
        if (defined('ABSPATH') && is_readable(ABSPATH . 'wp-admin/includes/plugin.php')) {
            require_once ABSPATH . 'wp-admin/includes/plugin.php';
        }
        if (function_exists('deactivate_plugins')) {
            deactivate_plugins(plugin_basename((string) constant('WPMGR_AGENT_FILE')));
        }
    }

    /**
     * The wp-option keys this plugin owns and removes on uninstall.
     *
     * @return list<string>
     */
    private function ownedOptions(): array
    {
        return [
            Keystore::OPTION_CP_PUBLIC_KEY,
            Keystore::OPTION_SITE_KEYPAIR,
            Keystore::OPTION_AGE_IDENTITY,
            Keystore::OPTION_MASTER_KEY_SOURCE,
            Settings::OPTION_CP_URL,
            Settings::OPTION_SITE_ID,
            Settings::OPTION_TENANT_ID,
            Settings::OPTION_ACTIVATED_AT,
            Settings::OPTION_LAST_HEARTBEAT,
            Settings::OPTION_LAST_METADATA,
            Plugin::OPTION_KEYSTORE_ERROR,
            Plugin::OPTION_LAST_DIAGNOSTICS_AT,
            Admin::OPTION_CONNECTION_KEY,
            Schema::OPTION_DB_VERSION,
            self::OPTION_REVOKED,
        ];
    }
}
