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

use WPMgr\Agent\Commands\AgentSelfUpdateCommand;
use WPMgr\Agent\Commands\MetadataCommand;
use WPMgr\Agent\Commands\ObjectcacheDisableCommand;
use WPMgr\Agent\Diagnostics\SizeProbe;
use WPMgr\Agent\Media\HtaccessInstaller;
use WPMgr\Agent\ObjectCache\ObjectCacheConfig;
use WPMgr\Agent\ObjectCache\ObjectCacheDropinInstaller;
use WPMgr\Agent\ObjectCache\ObjectCacheHeartbeat;
use WPMgr\Agent\Support\ActivityLog;
use WPMgr\Agent\Support\AgeIdentity;
use WPMgr\Agent\Support\ErrorMonitor;
use WPMgr\Agent\Support\LoginProtection;
use WPMgr\Agent\Support\MuPluginInstaller;

/**
 * Connection-lifecycle orchestration for the agent.
 */
final class Lifecycle
{
    /**
     * Prefix of the agent's own namespace in the options tables.
     *
     * A published, versioned contract ("Option namespace contract" in
     * apps/agent/README.md): the agent keeps its identity and connection state
     * in options whose names start with this prefix, a transient in the
     * namespace carries the prefix in its own name, and uninstall removes every
     * row in the namespace whether or not it is listed anywhere. Store nothing
     * under it that should outlive the plugin, and never change it without
     * bumping the contract version.
     */
    public const NAME_PREFIX = 'wpmgr_agent_';

    /**
     * The row names an entry in the namespace can take in the per-site options
     * table: the option itself, a transient's value and timeout rows, and a site
     * transient's value and timeout rows (a single-site install keeps site
     * transients in the options table).
     *
     * @var list<string>
     */
    private const OPTIONS_TABLE_FORMS = [
        self::NAME_PREFIX,
        '_transient_' . self::NAME_PREFIX,
        '_transient_timeout_' . self::NAME_PREFIX,
        '_site_transient_' . self::NAME_PREFIX,
        '_site_transient_timeout_' . self::NAME_PREFIX,
    ];

    /**
     * The row names an entry in the namespace can take in the network table on
     * multisite: a network-wide option, and a site transient's value and
     * timeout rows.
     *
     * @var list<string>
     */
    private const NETWORK_TABLE_FORMS = [
        self::NAME_PREFIX,
        '_site_transient_' . self::NAME_PREFIX,
        '_site_transient_timeout_' . self::NAME_PREFIX,
    ];

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

    /** Command name carried by the signed revoke proof (`cmd` claim). */
    public const REVOKE_CMD = 'revoke';

    private Keystore $keystore;

    private Settings $settings;

    private Enrollment $enrollment;

    /**
     * Verifier seam for the signed revoke token. Signature:
     *   fn(string $jwt): array<string,mixed>  — returns the validated claims,
     *   or THROWS on any failure (bad signature / aud / cmd / exp / replay).
     *
     * Null in production: {@see verifyRevokeToken()} lazily builds a real
     * {@see Connector} and calls Connector::verifyCommand($jwt, 'revoke'),
     * which runs the full Ed25519 signature + exp + jti-replay gate AND binds
     * `aud` to this site's enrolled UUID + `cmd` to "revoke" (hash_equals).
     * Tests inject a stub here so they can exercise the gate without minting a
     * full JWT — but a real signed token also flows through unchanged.
     *
     * @var (\Closure(string):array<string,mixed>)|null
     */
    private ?\Closure $revokeVerifier;

    /**
     * @param Keystore   $keystore       Key material store (wiped on revoke/uninstall).
     * @param Settings   $settings       Enrollment/config state.
     * @param Enrollment $enrollment     Signed outbound client (heartbeat + last-will).
     * @param (\Closure(string):array<string,mixed>)|null $revokeVerifier
     *        Optional test seam over the signed-revoke-token verifier. When null
     *        (production) a real Connector::verifyCommand is used — see the
     *        $revokeVerifier property docblock. MUST throw on ANY failure.
     */
    public function __construct(
        Keystore $keystore,
        Settings $settings,
        Enrollment $enrollment,
        ?\Closure $revokeVerifier = null
    ) {
        $this->keystore       = $keystore;
        $this->settings       = $settings;
        $this->enrollment     = $enrollment;
        $this->revokeVerifier = $revokeVerifier;
    }

    /**
     * Act on the instructions carried by a heartbeat response. Currently the
     * only instruction is "revoke"; unknown tokens are ignored (forward-compat).
     *
     * SECURITY (Phase-6 finding B / ADR-040 addendum): a "revoke" is destructive
     * (wipes keys + self-deactivates), so it is acted on ONLY when accompanied by
     * a valid signed proof. The CP returns a short-lived Ed25519 JWT
     * (`revoke_token`) minted by the SAME signer the agent already verifies for
     * inbound CP→agent commands. We verify it (signature + exp + jti-replay +
     * aud==own-site + cmd=="revoke") via the existing Connector BEFORE tearing
     * down. The token is REQUIRED: a missing, forged, stale, replayed, or
     * wrong-aud/cmd token is a NO-OP — never destructive (fail closed).
     *
     * @param array<int,string> $instructions Tokens from the heartbeat response.
     * @param string            $revokeToken  Signed revoke proof from the same
     *                                         heartbeat response ('' when absent).
     * @return void
     */
    public function handleInstructions(array $instructions, string $revokeToken = ''): void
    {
        foreach ($instructions as $instruction) {
            if ($instruction === self::INSTRUCTION_REVOKE) {
                // Fail closed: only proceed when the signed proof verifies.
                if (!$this->revokeTokenIsValid($revokeToken)) {
                    // Forged / missing / invalid / replayed token → ignore the
                    // instruction entirely. No teardown, ever.
                    return;
                }
                $this->revokeSelf(self::REASON_REVOKED);
                // A revoke is terminal — the plugin is deactivating; stop here.
                return;
            }
        }
    }

    /**
     * Verify the signed revoke proof, fail-closed. Returns true ONLY when the
     * token is non-empty AND the verifier accepts it (valid Ed25519 signature
     * against the stored control-plane public key, unexpired, unreplayed,
     * aud == this site's enrolled UUID, cmd == "revoke"). ANY failure — absent
     * token, bad signature, wrong aud/cmd, expired, replayed, verifier throwing
     * for any reason — returns false so the caller does NOT tear down.
     *
     * The actual cryptographic check is delegated to the existing
     * Connector::verifyCommand (no hand-rolled JWT/Ed25519 here).
     *
     * @param string $revokeToken Compact JWT from the heartbeat response.
     * @return bool True iff the proof fully verifies.
     */
    private function revokeTokenIsValid(string $revokeToken): bool
    {
        $revokeToken = trim($revokeToken);
        if ($revokeToken === '') {
            return false;
        }

        try {
            $claims = $this->verifyRevokeToken($revokeToken);
        } catch (\Throwable $e) {
            // Swallow: a verification failure must be silent + non-destructive.
            return false;
        }

        // Defense-in-depth. Connector::verifyCommand already enforces both
        // aud == Settings::siteId() and cmd == "revoke" (hash_equals) and would
        // have thrown above otherwise; re-assert here so the gate is correct even
        // if an injected test verifier skips those binds. Both must hold.
        $cmd = isset($claims['cmd']) && is_string($claims['cmd']) ? $claims['cmd'] : '';
        if (!hash_equals(self::REVOKE_CMD, $cmd)) {
            return false;
        }

        $siteId = $this->settings->siteId();
        $aud     = isset($claims['aud']) && is_string($claims['aud']) ? $claims['aud'] : '';
        if ($siteId === '' || !hash_equals($siteId, $aud)) {
            return false;
        }

        return true;
    }

    /**
     * Run the signed revoke token through the verifier. In production this builds
     * a real Connector (over the same Keystore/Settings the agent already uses for
     * inbound command verification) and calls verifyCommand($jwt, 'revoke'); tests
     * may inject a closure seam via the constructor.
     *
     * @param string $revokeToken Compact JWT.
     * @return array<string,mixed> Validated claims.
     * @throws \Throwable On ANY verification failure.
     */
    private function verifyRevokeToken(string $revokeToken): array
    {
        if ($this->revokeVerifier !== null) {
            return ($this->revokeVerifier)($revokeToken);
        }

        $connector = new Connector($this->keystore, $this->settings);

        return $connector->verifyCommand($revokeToken, self::REVOKE_CMD);
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
     * cron is the backstop. Returns the parsed instructions AND the signed
     * revoke proof so the caller can, in the unlikely event the CP revokes on
     * the very first beat, act on them through the verified gate.
     *
     * @return array{instructions:array<int,string>,revoke_token:string}
     *         The heartbeat's instruction list and signed revoke proof (both
     *         empty on failure or when nothing is queued).
     */
    public function heartbeatNow(): array
    {
        try {
            $result = $this->enrollment->sendHeartbeat();
            $instructions = isset($result['instructions']) && is_array($result['instructions'])
                ? $result['instructions']
                : [];
            $revokeToken = isset($result['revoke_token']) && is_string($result['revoke_token'])
                ? $result['revoke_token']
                : '';
            return ['instructions' => $instructions, 'revoke_token' => $revokeToken];
        } catch (\Throwable $e) {
            return ['instructions' => [], 'revoke_token' => ''];
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
            // Still run OC drop-in teardown even when not enrolled.
        } else {
            try {
                $this->enrollment->disconnect('deactivated');
            } catch (\Throwable $e) {
                // Best-effort: deactivation must complete even if the CP is down.
            }
        }

        // H8: deactivate removes the drop-in but KEEPS the config file so that
        // re-activation can re-enable without re-configuring.
        try {
            $installer = new ObjectCacheDropinInstaller();
            $installer->uninstall();
        } catch (\Throwable $e) {
            // Best-effort.
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
     * Leaves no row in the agent's namespace ({@see self::NAME_PREFIX}): named
     * options and transients go through the options API first, then every
     * remaining row in the namespace is swept from the database.
     *
     * @return void
     */
    public function wipeAll(): void
    {
        $this->keystore->clearSiteIdentity();
        $this->settings->clearEnrollment();
        $this->settings->clearLastSyncTimestamps();

        // H8: uninstall = drop-in removed + config file deleted + options cleared.
        try {
            $disableCmd = new ObjectcacheDisableCommand();
            $disableCmd->execute([], ['flush' => true]);
        } catch (\Throwable $e) {
            // Best-effort: uninstall must complete regardless.
        }

        try {
            $configLoader = new ObjectCacheConfig();
            $configLoader->delete();
        } catch (\Throwable $e) {
            // Best-effort.
        }

        if (!function_exists('delete_option')) {
            return;
        }

        $multisite = function_exists('is_multisite') && is_multisite();

        foreach ($this->ownedOptions() as $option) {
            delete_option($option);
            // Settings keeps its rows network-wide on multisite.
            if ($multisite && function_exists('delete_site_option')) {
                delete_site_option($option);
            }
        }

        // Through the transient API, so a persistent object cache drops them too.
        foreach ($this->ownedTransients() as $transient) {
            if (function_exists('delete_transient')) {
                delete_transient($transient);
            }
            if (function_exists('delete_site_transient')) {
                delete_site_transient($transient);
            }
        }

        $this->sweepNamespace($multisite);

        // Clear any remaining scheduled events for the agent's hooks.
        if (function_exists('wp_clear_scheduled_hook')) {
            wp_clear_scheduled_hook(Scheduler::HOOK_HEARTBEAT);
            wp_clear_scheduled_hook(Scheduler::HOOK_METADATA);
            wp_clear_scheduled_hook(Scheduler::HOOK_SAFETY);
            wp_clear_scheduled_hook(Scheduler::HOOK_DIAGNOSTICS);
            wp_clear_scheduled_hook(Scheduler::HOOK_SIZES);
            wp_clear_scheduled_hook(Scheduler::HOOK_ACTIVITY_SHIP);
            wp_clear_scheduled_hook(Scheduler::HOOK_ERRORS_SHIP);
            // Phase 3 — page-cache cron events.
            wp_clear_scheduled_hook(\WPMgr\Agent\Cache\Preload::HOOK);
            wp_clear_scheduled_hook(\WPMgr\Agent\Cache\CacheRefreshCron::HOOK);
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
     * Names in the namespace are also swept by prefix ({@see sweepNamespace()}),
     * but naming them here removes them through the options API even where the
     * sweep cannot see them. LifecycleOwnedOptionsTest fails when an OPTION_*
     * constant in the namespace is missing from this list.
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
            // GH #257 last-resort tier: the raw (base64) master key itself,
            // when this install's key lives in the database rather than a
            // file or salts. Must not outlive the plugin.
            Keystore::OPTION_DB_MASTER_KEY,
            // GH #577: the encrypted email-delivery secrets the dashboard sent.
            Keystore::OPTION_EMAIL_SECRET,
            Keystore::OPTION_EMAIL_CONN_SECRETS,
            Settings::OPTION_CP_URL,
            Settings::OPTION_SITE_ID,
            Settings::OPTION_TENANT_ID,
            Settings::OPTION_ACTIVATED_AT,
            Settings::OPTION_LAST_HEARTBEAT,
            Settings::OPTION_LAST_METADATA,
            Plugin::OPTION_KEYSTORE_ERROR,
            Plugin::OPTION_KEYSTORE_ERROR_KIND,
            Plugin::OPTION_LAST_DIAGNOSTICS_AT,
            Plugin::OPTION_REPORTED_VERSION,
            'wpmgr_agent_engine_opcache_version', // Plugin writes it by literal name.
            Admin::OPTION_CONNECTION_KEY,
            Schema::OPTION_DB_VERSION,
            self::OPTION_REVOKED,
            AgentSelfUpdateCommand::OPTION_RESULT,
            // Self-update bookkeeping, named by value: the class that writes
            // them is not part of every build of this plugin.
            'wpmgr_agent_update_last_iat',
            'wpmgr_agent_self_update_apply_id',
            SizeProbe::OPTION_SIZES,
            HtaccessInstaller::OPTION_NGINX_NOTICE,
            ActivityLog::OPTION_SEQ,
            ErrorMonitor::OPTION_SHIP_CURSOR,
            ErrorMonitor::OPTION_SHIP_TS,
            LoginProtection::OPTION_SHIP_CURSOR,
            MuPluginInstaller::OPTION_INSTALLED,
            MuPluginInstaller::OPTION_WAF_INSTALLED,
            MuPluginInstaller::OPTION_WATCHDOG_INSTALLED,
            // Phase 3 — page-cache options.
            \WPMgr\Agent\Cache\CacheManager::OPTION_CONFIG,
            \WPMgr\Agent\Cache\CacheManager::OPTION_LAST_ERROR,
            \WPMgr\Agent\Cache\CacheManager::OPTION_STATS,
            \WPMgr\Agent\Cache\NginxHelper::OPTION_NGINX_NOTICE,
            'wpmgr_cache_preload_queue',
            // H8: object-cache options.
            ObjectCacheConfig::OPTION_CONFIG_HASH,
            ObjectCacheHeartbeat::OPTION_STATS,
            'wpmgr_oc_outage_marker', // WPMgr_Object_Cache::FAILBACK_MARKER_OPTION (H5).
            'wpmgr_snapshot_gc_last', // SnapshotManager::OPTION_GC_LAST (GH #226 GC throttle stamp).
            Plugin::OPTION_BACKUP_SWEEP_LAST, // GH #232 stalled-backup reaper throttle stamp.
            Plugin::OPTION_BACKUP_JANITOR_LAST, // GH #256 backup-scratch GC (BackupJanitor::gcRuns()) throttle stamp.
            Plugin::OPTION_RESTORE_GC_LAST, // GH #256 restore-rollback-material GC throttle stamp.
        ];
    }

    /**
     * Transients the agent sets under a fixed name in its namespace. Each is
     * removed as both a transient and a site transient.
     *
     * @return list<string>
     */
    private function ownedTransients(): array
    {
        return [
            'wpmgr_agent_notice', // Admin's one-shot notice (private constant there).
            Plugin::TRANSIENT_KEYSTORE_PROBE,
            Scheduler::REFRESH_LOCK_KEY,
            'wpmgr_agent_exec_probe', // SizeProbe's probe result (private constant there).
            'wpmgr_agent_update_manifest', // Named by value: its class is not part of every build.
        ];
    }

    /**
     * Delete every row left in the agent's namespace: names written by another
     * release, names built at runtime, and the value and timeout rows of
     * transients and site transients. On multisite the current network's
     * network table is swept too.
     *
     * The database narrows with an anchored, LIKE-escaped prefix; the exact,
     * case-sensitive prefix check decides. Nothing outside the namespace is
     * removed: not a name that merely contains the prefix, not one that differs
     * only by case, and not one that matched only because '_' is a LIKE
     * wildcard. Each delete goes through the options API, which keeps object
     * caches coherent.
     *
     * @param bool $multisite Whether this is a multisite install.
     * @return void
     */
    private function sweepNamespace(bool $multisite): void
    {
        if (!isset($GLOBALS['wpdb']) || !is_object($GLOBALS['wpdb'])) {
            return;
        }

        /** @var \wpdb $wpdb */
        $wpdb = $GLOBALS['wpdb'];

        foreach (self::OPTIONS_TABLE_FORMS as $form) {
            // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- uninstall must find every row in the namespace; no API lists options by prefix, and a cached answer could miss rows.
            $names = $wpdb->get_col(
                $wpdb->prepare(
                    "SELECT option_name FROM {$wpdb->options} WHERE option_name LIKE %s", // @phpstan-ignore argument.type (the only interpolation is core's own table name)
                    $wpdb->esc_like($form) . '%'
                )
            );
            foreach (self::namesStartingWith($names, $form) as $name) {
                delete_option($name);
            }
        }

        if (!$multisite || empty($wpdb->sitemeta) || !function_exists('delete_site_option') || !function_exists('get_current_network_id')) {
            return;
        }

        $networkId = (int) get_current_network_id();
        foreach (self::NETWORK_TABLE_FORMS as $form) {
            // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- uninstall must find every row in the namespace; no API lists network options by prefix, and a cached answer could miss rows.
            $names = $wpdb->get_col(
                $wpdb->prepare(
                    "SELECT meta_key FROM {$wpdb->sitemeta} WHERE site_id = %d AND meta_key LIKE %s", // @phpstan-ignore argument.type (the only interpolation is core's own table name)
                    $networkId,
                    $wpdb->esc_like($form) . '%'
                )
            );
            foreach (self::namesStartingWith($names, $form) as $name) {
                delete_site_option($name);
            }
        }
    }

    /**
     * The names in a database column that start, exactly and case-sensitively,
     * with $prefix. LIKE under the usual collations ignores case, so this is
     * the check that decides what is deleted.
     *
     * @param mixed  $names  Column returned by the database.
     * @param string $prefix Required prefix.
     * @return list<string>
     */
    private static function namesStartingWith($names, string $prefix): array
    {
        if (!is_array($names)) {
            return [];
        }

        $kept = [];
        foreach ($names as $name) {
            if (is_string($name) && str_starts_with($name, $prefix)) {
                $kept[] = $name;
            }
        }

        return $kept;
    }
}
