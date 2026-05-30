<?php
/**
 * Scheduler: wp-cron orchestration for reporting + the auto-deactivate safety.
 *
 * Cron events:
 *   - wpmgr_agent_heartbeat  : every 5 minutes, sends a heartbeat (when enrolled).
 *   - wpmgr_agent_metadata   : daily, pushes full metadata (when enrolled). Also
 *                              fired on enrollment and on plugin/theme changes.
 *   - wpmgr_agent_safety     : one-shot ~30 min after first activation; if the
 *                              site is still NOT enrolled, the plugin
 *                              self-deactivates (MainWP-style safety). Disabled
 *                              by defining WPMGR_AGENT_DISABLE_AUTO_DEACTIVATE.
 *
 * Everything is guarded so nothing runs (or reaches the network) until enrolled.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent;

/**
 * Registers cron schedules + hooks and runs the scheduled jobs.
 */
final class Scheduler
{
    /** Heartbeat cron hook name. */
    public const HOOK_HEARTBEAT = 'wpmgr_agent_heartbeat';

    /** Metadata cron hook name. */
    public const HOOK_METADATA = 'wpmgr_agent_metadata';

    /** Auto-deactivate safety cron hook name. */
    public const HOOK_SAFETY = 'wpmgr_agent_safety';

    /**
     * ADR-037 Sprint 2 — daily site-diagnostics cron hook. The hook callback
     * is bound by Plugin::registerHooks (Plugin owns DiagnosticsCommand + the
     * Enrollment push); Scheduler only owns the SCHEDULE (jittered 24h). Kept
     * additive so Sprint 1's parallel edits to this file don't collide.
     */
    public const HOOK_DIAGNOSTICS = 'wpmgr_agent_diagnostics_daily';

    /**
     * ADR-037 Sprint 3 — dedicated activity-log ship cron hook (every 5 min).
     * The callback is bound by Plugin::registerHooks (Plugin owns ActivityLog +
     * the signed-POST helper); Scheduler only owns the SCHEDULE so Sprint 1's
     * parallel edits to this file stay additive. The heartbeat (runHeartbeat)
     * is the backstop; this dedicated event guarantees batches drain even on a
     * site whose only cron traffic is wp-cron page hits.
     */
    public const HOOK_ACTIVITY_SHIP = 'wpmgr_agent_activity_ship';

    /**
     * Dedicated PHP-error ship cron hook (every 5 min). Plugin::registerHooks
     * binds Plugin::shipErrors() to this AND to the heartbeat backstop. Mirrors
     * HOOK_ACTIVITY_SHIP: previously errors only rode the daily diagnostics
     * cron, so they reached the dashboard hours late; this guarantees a 5-min
     * drain. ErrorMonitor::IMMEDIATE_SHIP_HOOK is the SAME hook string so a
     * fatal can schedule a one-shot near-immediate ship onto it.
     */
    public const HOOK_ERRORS_SHIP = 'wpmgr_agent_errors_ship';

    /** Custom cron schedule key for the 5-minute interval. */
    public const SCHEDULE_5MIN = 'wpmgr_agent_5min';

    /**
     * Custom cron schedule key for the 30-minute interval used by the metadata
     * push. v0.9.0: was `daily`; bumped to 30 min so dashboards refresh
     * available-update counts within an operational SLA without thrashing the
     * WP.org API (Scheduler::runMetadata holds a 5-minute transient lock
     * around the force-refresh).
     */
    public const SCHEDULE_30MIN = 'wpmgr_agent_30min';

    /**
     * Transient key used to throttle force-refreshes of update_plugins /
     * update_themes / update_core to once every 5 minutes regardless of how
     * many metadata events fire (upgrader_process_complete, switch_theme,
     * activated_plugin all hit the same code path).
     */
    public const REFRESH_LOCK_KEY = 'wpmgr_agent_refresh_transients_lock';

    /** Window after activation before the safety check deactivates, in seconds. */
    public const SAFETY_WINDOW = 1800;

    private Settings $settings;

    private Enrollment $enrollment;

    /**
     * @param Settings   $settings   Enrollment/config state.
     * @param Enrollment $enrollment Reporting client.
     */
    public function __construct(Settings $settings, Enrollment $enrollment)
    {
        $this->settings   = $settings;
        $this->enrollment = $enrollment;
    }

    /**
     * Register cron filters/actions. Bind during normal plugin boot.
     *
     * @return void
     */
    public function registerHooks(): void
    {
        add_filter('cron_schedules', [$this, 'addSchedules']);

        add_action(self::HOOK_HEARTBEAT, [$this, 'runHeartbeat']);
        add_action(self::HOOK_METADATA, [$this, 'runMetadata']);
        add_action(self::HOOK_SAFETY, [$this, 'runSafetyCheck']);

        // Push metadata when the plugin/theme inventory changes.
        add_action('upgrader_process_complete', [$this, 'runMetadata']);
        add_action('switch_theme', [$this, 'runMetadata']);
        add_action('activated_plugin', [$this, 'runMetadata']);
        add_action('deactivated_plugin', [$this, 'runMetadata']);
    }

    /**
     * Add the custom 5-minute cron interval.
     *
     * @param mixed $schedules Existing schedules (array, per WP contract).
     * @return array<string,array{interval:int,display:string}>
     */
    public function addSchedules($schedules): array
    {
        if (!is_array($schedules)) {
            $schedules = [];
        }

        $schedules[self::SCHEDULE_5MIN] = [
            'interval' => 300,
            'display'  => 'Every 5 minutes (WPMgr Agent)',
        ];

        $schedules[self::SCHEDULE_30MIN] = [
            'interval' => 1800,
            'display'  => 'Every 30 minutes (WPMgr Agent)',
        ];

        /** @var array<string,array{interval:int,display:string}> $schedules */
        return $schedules;
    }

    /**
     * Schedule the recurring + safety events. Call on activation.
     *
     * @param int $now Current timestamp.
     * @return void
     */
    public function scheduleEvents(int $now): void
    {
        if (function_exists('wp_next_scheduled') && function_exists('wp_schedule_event')) {
            if (wp_next_scheduled(self::HOOK_HEARTBEAT) === false) {
                wp_schedule_event($now + 60, self::SCHEDULE_5MIN, self::HOOK_HEARTBEAT);
            }
            // v0.9.0: metadata cadence dropped from daily to 30 min so the CP
            // sees available-update counts refresh on an operator-friendly
            // SLA. If a pre-v0.9.0 daily cron is already scheduled (re-upload
            // path), clear it and re-register on the new schedule so existing
            // installs migrate without an admin action.
            if (function_exists('wp_get_scheduled_event')) {
                $existing = wp_get_scheduled_event(self::HOOK_METADATA);
                if (is_object($existing) && isset($existing->schedule)
                    && (string) $existing->schedule !== self::SCHEDULE_30MIN
                ) {
                    if (function_exists('wp_clear_scheduled_hook')) {
                        wp_clear_scheduled_hook(self::HOOK_METADATA);
                    }
                }
            }
            if (wp_next_scheduled(self::HOOK_METADATA) === false) {
                wp_schedule_event($now + 120, self::SCHEDULE_30MIN, self::HOOK_METADATA);
            }

            // ADR-037 Sprint 2 — daily diagnostics push. Jittered up to 4h so
            // a fleet of sites doesn't all hit the CP at the same wall-clock
            // minute. Per-site jitter is computed from the site URL so the
            // offset is stable across activations (the operator doesn't see
            // the diagnostics cadence drift on every plugin reload).
            if (wp_next_scheduled(self::HOOK_DIAGNOSTICS) === false) {
                $jitter = $this->diagnosticsJitter();
                wp_schedule_event($now + 600 + $jitter, 'daily', self::HOOK_DIAGNOSTICS);
            }

            // ADR-037 Sprint 3 — activity-log shipper, every 5 min (piggybacks
            // the heartbeat cadence). The handler is bound in Plugin.
            if (wp_next_scheduled(self::HOOK_ACTIVITY_SHIP) === false) {
                wp_schedule_event($now + 90, self::SCHEDULE_5MIN, self::HOOK_ACTIVITY_SHIP);
            }

            // PHP-error shipper, every 5 min (same cadence as activity). Handler
            // bound in Plugin. Offset 100s so it does not collide with the
            // activity tick at +90s.
            if (wp_next_scheduled(self::HOOK_ERRORS_SHIP) === false) {
                wp_schedule_event($now + 100, self::SCHEDULE_5MIN, self::HOOK_ERRORS_SHIP);
            }
        }

        // One-shot safety check ~30 minutes out, unless disabled by constant.
        if (!self::autoDeactivateDisabled()
            && function_exists('wp_next_scheduled')
            && function_exists('wp_schedule_single_event')
            && wp_next_scheduled(self::HOOK_SAFETY) === false
        ) {
            wp_schedule_single_event($now + self::SAFETY_WINDOW, self::HOOK_SAFETY);
        }
    }

    /**
     * Clear all scheduled events. Call on deactivation.
     *
     * @return void
     */
    public function clearEvents(): void
    {
        if (!function_exists('wp_clear_scheduled_hook')) {
            return;
        }

        wp_clear_scheduled_hook(self::HOOK_HEARTBEAT);
        wp_clear_scheduled_hook(self::HOOK_METADATA);
        wp_clear_scheduled_hook(self::HOOK_SAFETY);
        // ADR-037 Sprint 2 — also clear the diagnostics cron on deactivation.
        wp_clear_scheduled_hook(self::HOOK_DIAGNOSTICS);
        // ADR-037 Sprint 3 — clear the activity-ship cron on deactivation.
        wp_clear_scheduled_hook(self::HOOK_ACTIVITY_SHIP);
        // Clear the dedicated PHP-error ship cron on deactivation.
        wp_clear_scheduled_hook(self::HOOK_ERRORS_SHIP);
    }

    /**
     * Compute a per-site jitter (seconds, 0..14400 inclusive) for the daily
     * diagnostics push so a fleet of sites does not all dial the CP at the
     * same wall-clock minute. Deterministic per site URL so the offset is
     * stable across plugin reloads.
     *
     * @return int Jitter seconds.
     */
    private function diagnosticsJitter(): int
    {
        $url = function_exists('get_site_url') ? (string) get_site_url() : (string) (microtime(true) * 1000);
        $h = crc32($url);
        // 14400 = 4h; even spread within a 24h window.
        return (int) ($h % 14400);
    }

    /**
     * Heartbeat job: no-op until enrolled.
     *
     * v0.9.13 backstop: after a successful heartbeat, peek at the last
     * recorded diagnostics-push timestamp (wpmgr_agent_last_diagnostics_at,
     * written by Plugin::runDiagnostics on 2xx). If it has never run, or it
     * was more than 6 hours ago, schedule a one-shot HOOK_DIAGNOSTICS event
     * 10 seconds out. This guarantees the operator sees data in the Health
     * tab within at most one heartbeat cycle (5 min) of pairing even if the
     * activation-time priming single-event was lost (e.g. wp-cron not run
     * yet, or the activation fired pre-enrollment so runDiagnostics no-op'd).
     * Cap is 6h rather than 24h so the system self-heals fast on day-of-install.
     *
     * @return void
     */
    public function runHeartbeat(): void
    {
        if (!$this->settings->isEnrolled()) {
            return;
        }

        $this->enrollment->sendHeartbeat();

        // v0.9.13 diagnostics backstop. Cheap (one get_option + one
        // function_exists). wp_schedule_single_event self-dedupes any
        // duplicate hook+args within 10 minutes, so back-to-back heartbeats
        // do not double-schedule.
        if (function_exists('get_option') && function_exists('wp_schedule_single_event')) {
            $last = (int) get_option(Plugin::OPTION_LAST_DIAGNOSTICS_AT, 0);
            $sixHours = defined('HOUR_IN_SECONDS') ? 6 * HOUR_IN_SECONDS : 6 * 3600;
            if ($last === 0 || (time() - $last) > $sixHours) {
                wp_schedule_single_event(time() + 10, self::HOOK_DIAGNOSTICS);
            }
        }
    }

    /**
     * Metadata job: no-op until enrolled.
     *
     * v0.9.0: before collecting the inventory we force-refresh WordPress's own
     * update transients so the per-plugin / per-theme / core `available_update`
     * fields reflect the freshest state WP.org has offered. We hold a 5-minute
     * transient lock to keep cascading metadata events (upgrader_process_complete,
     * switch_theme, activated_plugin, deactivated_plugin all fire back-to-back
     * during a multi-plugin update) from hammering WP.org. The wp_* calls are
     * @-suppressed: a transient HTTP error must not turn this whole job into
     * a fatal — the stale inventory still ships.
     *
     * @return void
     */
    public function runMetadata(): void
    {
        if (!$this->settings->isEnrolled()) {
            return;
        }

        $this->refreshUpdateTransients();

        $this->enrollment->pushMetadata();
    }

    /**
     * Force WordPress to re-poll its plugin/theme/core update endpoints, but
     * at most once per REFRESH_LOCK_KEY window (default 5 minutes).
     *
     * Public so a dispatcher (e.g. the RefreshInventoryCommand on-demand
     * refresh) can reuse the same throttle without re-implementing the lock.
     *
     * @param bool $force Bypass the rate-limit lock (used by the on-demand
     *                    refresh command, which is human-initiated).
     * @return void
     */
    public function refreshUpdateTransients(bool $force = false): void
    {
        if (!$force && function_exists('get_transient') && get_transient(self::REFRESH_LOCK_KEY) !== false) {
            return;
        }
        if (function_exists('wp_update_plugins')) {
            @wp_update_plugins();
        }
        if (function_exists('wp_update_themes')) {
            @wp_update_themes();
        }
        if (function_exists('wp_version_check')) {
            @wp_version_check();
        }
        if (function_exists('set_transient') && defined('MINUTE_IN_SECONDS')) {
            set_transient(self::REFRESH_LOCK_KEY, 1, 5 * MINUTE_IN_SECONDS);
        } elseif (function_exists('set_transient')) {
            set_transient(self::REFRESH_LOCK_KEY, 1, 5 * 60);
        }
    }

    /**
     * Safety job: if still not enrolled after the activation window, deactivate
     * this plugin. Opt-out via WPMGR_AGENT_DISABLE_AUTO_DEACTIVATE.
     *
     * @return void
     */
    public function runSafetyCheck(): void
    {
        if (self::autoDeactivateDisabled()) {
            return;
        }

        if ($this->settings->isEnrolled()) {
            return;
        }

        $this->clearEvents();

        if (function_exists('deactivate_plugins') && defined('WPMGR_AGENT_FILE')) {
            if (defined('ABSPATH') && file_exists(ABSPATH . 'wp-admin/includes/plugin.php')) {
                require_once ABSPATH . 'wp-admin/includes/plugin.php';
            }
            if (function_exists('plugin_basename')) {
                deactivate_plugins(plugin_basename((string) constant('WPMGR_AGENT_FILE')));
            }
        }
    }

    /**
     * Whether the auto-deactivate safety is disabled by constant.
     *
     * @return bool
     */
    public static function autoDeactivateDisabled(): bool
    {
        return defined('WPMGR_AGENT_DISABLE_AUTO_DEACTIVATE') && (bool) constant('WPMGR_AGENT_DISABLE_AUTO_DEACTIVATE');
    }
}
