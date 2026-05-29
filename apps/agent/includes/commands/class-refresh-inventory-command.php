<?php
/**
 * Refresh-inventory command: on-demand variant of the metadata cron.
 *
 * Wire contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/refresh_inventory
 *   body: {}
 *   response: { "ok": bool, "detail": string }
 *
 * The Router's permission_callback already enforces the signed-JWT contract
 * (Ed25519 signature, aud=siteId, cmd="refresh_inventory", jti anti-replay)
 * via Connector::verifyCommand. By the time execute() runs the request is
 * authenticated; the command itself just (1) refuses anything other than an
 * empty body, (2) forces WP to re-poll its plugin/theme/core update endpoints
 * (bypassing Scheduler's 5-minute lock — this is human-initiated), (3) pushes
 * the resulting inventory to the control plane.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

/**
 * On-demand refresh of the agent's inventory + push to the control plane.
 *
 * The two collaborators are injected as Closures rather than concrete types so
 * the command is unit-testable without doubling the `final` Enrollment /
 * Scheduler classes. Plugin::commands() binds them to:
 *   - $refresh:  fn() => $this->scheduler->refreshUpdateTransients(true)
 *   - $push:     fn() => $this->enrollment->pushMetadata()
 */
final class RefreshInventoryCommand implements CommandInterface
{
    /** @var \Closure(): void */
    private \Closure $refresh;

    /** @var \Closure(): array<string,mixed> */
    private \Closure $push;

    /**
     * @param \Closure(): void                $refresh Forces a re-poll of WP's
     *                                                  update_plugins / update_themes /
     *                                                  update_core transients. May throw;
     *                                                  the command swallows so the push
     *                                                  still happens.
     * @param \Closure(): array<string,mixed> $push    Performs the signed metadata push
     *                                                  to the control plane; returns the
     *                                                  Enrollment result tuple
     *                                                  (`['ok' => bool, ...]`).
     */
    public function __construct(\Closure $refresh, \Closure $push)
    {
        $this->refresh = $refresh;
        $this->push    = $push;
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'refresh_inventory';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused — Router
     *                                    already enforced aud + cmd binding).
     * @param array<string,mixed> $params Request parameters; MUST be empty.
     * @return array{ok:bool,detail:string}
     */
    public function execute(array $claims, array $params): array
    {
        // Body shape: refuse anything beyond an empty object. This keeps the
        // command surface unambiguous; future extensions go behind explicit
        // named params, not "we'll silently accept whatever".
        if ($params !== []) {
            return ['ok' => false, 'detail' => 'body must be an empty object'];
        }

        // Force a fresh poll of update_plugins / update_themes / update_core.
        // bypass=true (human-initiated): a CP-issued refresh should not be
        // rate-limited by the cron's 5-minute lock.
        try {
            ($this->refresh)();
        } catch (\Throwable $e) {
            // Don't fail the whole command if a single wp_* helper errors —
            // we'll still push whatever inventory we have.
        }

        $result = ($this->push)();
        if (!is_array($result) || !($result['ok'] ?? false)) {
            $detail = is_array($result) && isset($result['message']) && is_string($result['message'])
                ? $result['message']
                : 'metadata push failed';
            return ['ok' => false, 'detail' => $detail];
        }

        return ['ok' => true, 'detail' => 'inventory refreshed and pushed'];
    }
}
