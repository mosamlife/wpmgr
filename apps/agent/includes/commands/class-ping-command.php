<?php
/**
 * Ping command: cheap liveness probe for the control-plane active-verify dial.
 *
 * Returns agent version, PHP wall-clock time, whether WP-Cron is disabled, and
 * how many seconds the last heartbeat is overdue. Designed to be answered in
 * milliseconds — no inventory build, no directory walks.
 *
 * A side-effect of handling this command is draining overdue WP-Cron events via
 * spawn_cron(), so every CP verify dial also drains the heartbeat and other
 * periodic jobs on page-cached sites where WP never boots on its own.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

/**
 * Responds to control-plane liveness pings.
 */
final class PingCommand implements CommandInterface
{
    /**
     * Option key holding the Unix timestamp of the last successful heartbeat.
     * Written by Enrollment::sendHeartbeat() on every 2xx response.
     * Non-autoloaded; read here only on the CP verify path (cheap).
     */
    public const OPTION_LAST_HEARTBEAT_AT = 'wpmgr_last_heartbeat_at';

    /**
     * Heartbeat interval in seconds. A heartbeat is considered overdue when the
     * elapsed time since the last recorded send exceeds this value.
     */
    private const HEARTBEAT_INTERVAL = 60;

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'ping';
    }

    /**
     * Effect: returns liveness, agent version and heartbeat lag. It calls spawn_cron(), which drains
     * already-due cron events -- a scheduling nudge, not a change of site state.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Read;
    }

    /**
     * Repeatability: a read, though the cron nudge means a second call may drain more due events.
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
     * @param array<string,mixed> $claims Validated JWT claims (unused for ping).
     * @param array<string,mixed> $params Request parameters (unused for ping).
     * @return array<string,mixed>
     */
    public function execute(array $claims, array $params): array
    {
        $payload = [
            'ok'                   => true,
            'agent_version'        => defined('WPMGR_AGENT_VERSION') ? (string) constant('WPMGR_AGENT_VERSION') : '',
            'php_time'             => time(),
            'wp_cron_disabled'     => defined('DISABLE_WP_CRON') && (bool) constant('DISABLE_WP_CRON'),
            'heartbeat_overdue_sec' => $this->heartbeatOverdueSec(),
        ];

        // Drain overdue WP-Cron events so every CP verify dial also ticks the
        // heartbeat, metadata, and perf jobs on page-cached idle sites.
        if (function_exists('spawn_cron')) {
            @spawn_cron(); // phpcs:ignore WordPress.PHP.NoSilencedErrors.Discouraged -- spawn_cron is best-effort; a failed spawn must not break the ping response
        }

        return $payload;
    }

    /**
     * Compute how many seconds the last heartbeat is overdue.
     *
     * Returns null when the option has never been written (agent never sent a
     * heartbeat — e.g. immediately after activation). Returns 0 when the last
     * heartbeat was within the interval window. Returns the positive elapsed
     * overage otherwise.
     *
     * @return int|null Seconds overdue, or null when no record exists.
     */
    private function heartbeatOverdueSec(): ?int
    {
        if (!function_exists('get_option')) {
            return null;
        }

        $stored = get_option(self::OPTION_LAST_HEARTBEAT_AT, false);
        if ($stored === false || !is_numeric($stored)) {
            return null;
        }

        $elapsed = time() - (int) $stored;
        $overdue = $elapsed - self::HEARTBEAT_INTERVAL;

        return max(0, $overdue);
    }
}
