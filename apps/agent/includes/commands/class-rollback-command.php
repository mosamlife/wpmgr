<?php
/**
 * Rollback command: restores a previously captured snapshot in response to a
 * verified, signed control-plane request.
 *
 * Contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/rollback
 *   body: { "type", "slug", "snapshot_id", "to_version", "allow_core_downgrade": bool }
 *   response: { "ok": bool, "restored_version": "...", "log": "..." }
 *
 * For plugin/theme the snapshot directory is restored over the live directory.
 * For core a downgrade-by-version is performed (WP-CLI `core update
 * --version=<to_version> --force`, or the Core_Upgrader equivalent). On success
 * the snapshot directory is removed.
 *
 * A core rollback is a forced downgrade of WordPress itself, so it runs only
 * when the request carries `allow_core_downgrade` as the JSON boolean `true`.
 * Any other value, including a missing key, the string "true" or the number 1,
 * is a refusal: ok=false, a plain log line, and no rollback work. Core is not
 * downgraded, the snapshot is kept, the update transient is left alone, and a
 * fresh maintenance flag stays in place. Like every rollback request, a
 * refused one still clears a stale maintenance flag left behind by an
 * interrupted run, on the way in. `allow_core_downgrade` is ignored for plugin
 * and theme rollbacks (GitHub issue #415).
 *
 * All input is untrusted: the type is whitelisted, the slug is sanitized to
 * reject path traversal, and the snapshot id is validated by the manager. A
 * target that resolves to the agent's own plugin is refused outright, sharing
 * UpdateCommand::isSelfTarget()'s definition; see the refusal's own comment in
 * execute() for why a rollback aimed at the agent is the same hazard as an
 * update aimed at it.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\Maintenance;
use WPMgr\Agent\Support\SiteUpdateLock;
use WPMgr\Agent\Support\SnapshotManager;
use WPMgr\Agent\Support\UpdateInFlight;
use WPMgr\Agent\Support\UpdateRunner;

/**
 * Restores a snapshot or downgrades core to a prior version.
 */
final class RollbackCommand implements CommandInterface
{
    /** Valid item types. */
    private const TYPES = ['plugin', 'theme', 'core'];

    private SnapshotManager $snapshots;

    private UpdateRunner $runner;

    /**
     * @param SnapshotManager|null $snapshots Snapshot store (defaults to real one).
     * @param UpdateRunner|null    $runner    Update executor (defaults to real one).
     */
    public function __construct(?SnapshotManager $snapshots = null, ?UpdateRunner $runner = null)
    {
        $this->snapshots = $snapshots ?? new SnapshotManager();
        $this->runner    = $runner ?? new UpdateRunner();
    }

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'rollback';
    }

    /**
     * Effect: reinstates a pre-update snapshot over the currently installed plugin or theme, or
     * downgrades WordPress core to the requested version when the request sets allow_core_downgrade
     * to true. A core rollback without that flag is refused and changes nothing. Whatever version is
     * replaced is not retained.
     *
     * @return CommandEffect
     */
    public function effect(): CommandEffect
    {
        return CommandEffect::Destructive;
    }

    /**
     * Repeatability: a blind retry re-applies the same snapshot and discards changes made since the first
     * rollback landed.
     *
     * @return CommandRepeatability
     */
    public function repeatability(): CommandRepeatability
    {
        return CommandRepeatability::Unsafe;
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused).
     * @param array<string,mixed> $params Request parameters.
     * @return array{ok:bool,restored_version:string,log:string}
     */
    public function execute(array $claims, array $params): array
    {
        // --- Per-site serialisation (GitHub issue #328) -------------------
        // A rollback replaces the live directory wholesale, so it is the same
        // class of writer as an update and must take the SAME per-site lock:
        // restoring a plugin directory while another request is running
        // WordPress's upgrader against wp-content/upgrade/ is exactly the
        // concurrency this lock exists to remove. Taken FIRST, above the
        // maintenance heal and the shutdown guard, so a refused rollback
        // writes nothing at all.
        //
        // A REFUSED ROLLBACK DOES NOT WAIT. RollbackCommand answers on the
        // control plane's own HTTP connection (it does not hand the response
        // back early the way the self-update channel does), so polling here
        // would pin a PHP worker for the whole of somebody else's apply. It
        // refuses honestly instead, with ok=false and a log line the operator
        // can act on, using the response's existing fields.
        $lock = SiteUpdateLock::acquire();
        if ($lock === SiteUpdateLock::HELD_BY_OTHER) {
            return $this->fail(
                'Another update, rollback or agent upgrade is already running on this site. '
                . 'Nothing was attempted and nothing on this site was changed. Try again once it has finished.'
            );
        }

        try {
            // Heal a `.maintenance` flag left behind by a prior interrupted
            // update/rollback before starting new work. Only a stale flag is
            // removed; a fresh one is left to whoever owns it. The shutdown
            // backstop for THIS run's own flag is armed later, in run(), once
            // the request has passed the type and core-permission checks.
            //
            // This sits INSIDE the try, not above it, so that a throw from it
            // still reaches the finally and releases the site lock. Above the
            // try, a throw here would strand the lock for its whole 900s TTL
            // and wedge every update and rollback on this site for fifteen
            // minutes. It is effectively non-throwing today, so this is latent
            // rather than live, but the lock's release must not depend on that
            // staying true. UpdateCommand::execute() already orders it this
            // way; this removes the asymmetry.
            Maintenance::healStaleIfPresent();

            return $this->run($params);
        } finally {
            SiteUpdateLock::release();
        }
    }

    /**
     * The rollback proper, with the site lock already held by execute().
     *
     * @param array<string,mixed> $params Request parameters.
     * @return array{ok:bool,restored_version:string,log:string}
     */
    private function run(array $params): array
    {
        $type       = isset($params['type']) && is_string($params['type']) ? $params['type'] : '';
        $rawSlug    = isset($params['slug']) && is_string($params['slug']) ? $params['slug'] : '';
        $snapshotId = isset($params['snapshot_id']) && is_string($params['snapshot_id']) ? $params['snapshot_id'] : '';
        $toVersion  = isset($params['to_version']) && is_string($params['to_version']) ? $params['to_version'] : '';

        if (!in_array($type, self::TYPES, true)) {
            return $this->fail('Invalid type.');
        }

        // A core rollback is a forced downgrade of WordPress itself, so it
        // needs an explicit request (GitHub issue #415). Only the JSON boolean
        // `true` counts: a string such as "false" is truthy in PHP, and an
        // irreversible operation must not turn on through a loose cast.
        // Refused here, above the shutdown backstop and the try/finally below,
        // for the same reason an invalid type is: a refused request does no
        // rollback work, and that includes leaving a fresh maintenance flag
        // exactly as it was, during the request and after it ends.
        $allowCoreDowngrade = ($params['allow_core_downgrade'] ?? null) === true;
        if ($type === 'core' && !$allowCoreDowngrade) {
            return $this->fail(
                'Refused: rolling back WordPress core is a forced downgrade, and this request did not allow one '
                . '(allow_core_downgrade was not set to true). Nothing was attempted, and WordPress core stays at '
                . 'its current version.'
            );
        }

        // Every remaining refusal is made here too, before the shutdown
        // backstop is armed and before the try/finally below that clears
        // maintenance mode, so a refused request leaves a `.maintenance` flag
        // exactly as it found it, during the request and after it ends.
        $slug       = 'core';
        $coreTarget = '';
        if ($type === 'core') {
            $coreTarget = $this->coreTarget($snapshotId, $toVersion);
            if ($coreTarget === '') {
                return $this->fail('No valid target core version.');
            }
        } else {
            $slug = UpdateCommand::sanitizeSlug($rawSlug);
            if ($slug === '' || $slug !== $rawSlug) {
                return $this->fail('Invalid or unsafe slug.');
            }
            if ($snapshotId === '') {
                return $this->fail('Missing snapshot_id.');
            }

            // Self-target refusal (agent-only hardening): restore() replaces
            // the live directory wholesale, so a rollback aimed at the agent
            // is the same self-destruction as an update aimed at it, from the
            // same control-plane-driven direction. Refused with the identical
            // definition UpdateCommand uses, before the snapshot store is
            // touched. Nothing legitimate is lost: UpdateCommand refuses to
            // snapshot or update the agent in the first place, so no
            // control-plane snapshot of the agent can exist to restore, and
            // the agent's own update channel does not use this command.
            if (UpdateCommand::isSelfTarget($type, $slug)) {
                return $this->fail(
                    'Refused: this target is the management agent itself. The agent updates through its own update channel, not through a plugin update or rollback task.'
                );
            }
        }

        // Arm the shutdown backstop only now, once the request has passed
        // every check, so a fatal error or a timeout mid-rollback still clears
        // whatever flag THIS run leaves set. A fresh flag already in place
        // belongs to another updater: the backstop and the finally below leave
        // it alone while it is unchanged (see Maintenance's class doc). It sits
        // above the try/finally below on purpose: run() is called inside
        // execute()'s try, so a throw from here still releases the site lock,
        // and no rollback work has begun that would need maintenance cleared.
        $foreignFlag = Maintenance::armShutdownGuard();

        // GUARANTEE: this is precisely the reported incident — a rollback
        // that itself fails (the new version is already active, the restore
        // errors, etc.) must still clear maintenance mode. Everything from
        // here on is wrapped so success, failure, or a thrown exception all
        // reach the `finally`.
        try {
            if ($type === 'core') {
                return $this->rollbackCore($snapshotId, $coreTarget);
            }

            try {
                $restore = $this->snapshots->restore($type, $slug, $snapshotId);
            } catch (\Throwable $e) {
                return $this->fail('Restore error.');
            }

            if (!$restore['ok']) {
                return [
                    'ok'               => false,
                    'restored_version' => '',
                    'log'              => $restore['log'],
                ];
            }

            // Completeness sweep (GH #211/#212 cluster) — a successful
            // restore just put an OLDER version back on disk, but WordPress's
            // own `update_plugins`/`update_themes` transient still remembers
            // whatever it last saw as current, so the rolled-back-FROM
            // version stays cached as "available" and re-offers itself
            // (feeding the #211 phantom-update display, or a re-apply loop
            // if the operator/CP acts on it). Invalidate NOW, synchronously,
            // BEFORE this response reaches the CP and before any subsequent
            // metadata pull re-reads the transient — the next check (cron or
            // on-demand refresh) then re-polls wp.org against the version
            // actually on disk.
            if (function_exists('delete_site_transient')) {
                delete_site_transient($type === 'plugin' ? 'update_plugins' : 'update_themes');
            }

            // S4 (issue #131 adversarial review) — defensive cleanup: the CP
            // may call RollbackCommand directly against a snapshot whose
            // original `update` request was hard-killed severely enough that
            // it never reached its own cleanup (so an UpdateInFlight marker
            // for this type/slug could still be sitting around). This
            // restore already handled recovery, so clear it now rather than
            // leaving a stale marker for the reconcile sweep to needlessly
            // re-restore later. Unconditionally safe — a no-op when no
            // marker exists for this type/slug.
            UpdateInFlight::clear($type, $slug);

            // Determine the version after restore; prefer the recorded prior version.
            $restoredVersion = $this->runner->currentVersion($type, $slug);
            if ($restoredVersion === '') {
                $restoredVersion = $this->snapshots->recordedVersion($snapshotId);
            }
            if ($restoredVersion === '' && $toVersion !== '') {
                $restoredVersion = $toVersion;
            }

            // Snapshot consumed; remove it.
            $this->snapshots->cleanup($snapshotId);

            return [
                'ok'               => true,
                'restored_version' => $restoredVersion,
                'log'              => $restore['log'],
            ];
        } finally {
            Maintenance::clear(null, $foreignFlag);
        }
    }

    /**
     * The core version a core rollback targets: to_version when set, else the
     * version the snapshot recorded.
     *
     * @param string $snapshotId Optional snapshot id holding the prior version.
     * @param string $toVersion  Target version (overrides snapshot when set).
     * @return string The target, or '' when there is no valid one.
     */
    private function coreTarget(string $snapshotId, string $toVersion): string
    {
        $target = $toVersion !== ''
            ? $toVersion
            : ($snapshotId !== '' ? $this->snapshots->recordedVersion($snapshotId) : '');

        if ($target === '' || preg_match('#^[0-9][0-9A-Za-z.\-]*$#', $target) !== 1) {
            return '';
        }

        return $target;
    }

    /**
     * Roll core back to a prior version via a forced downgrade. Reached only
     * when the request set allow_core_downgrade to true and named a valid
     * target (see run()).
     *
     * @param string $snapshotId Optional snapshot id holding the prior version.
     * @param string $target     Validated target version, from coreTarget().
     * @return array{ok:bool,restored_version:string,log:string}
     */
    private function rollbackCore(string $snapshotId, string $target): array
    {
        try {
            $result = $this->runner->forceCore($target);
        } catch (\Throwable $e) {
            return $this->fail('Core rollback error.');
        }

        if (!$result['ok']) {
            return [
                'ok'               => false,
                'restored_version' => '',
                'log'              => $result['log'],
            ];
        }

        // Completeness sweep (GH #211/#212 cluster) — same rationale as the
        // plugin/theme path above: a successful core downgrade just put an
        // older core on disk, but the `update_core` transient still
        // remembers the pre-rollback "current" version, so it would keep
        // offering the rolled-back-FROM version as an "update". Invalidate
        // synchronously, before the CP's next metadata pull re-reads it.
        if (function_exists('delete_site_transient')) {
            delete_site_transient('update_core');
        }

        if ($snapshotId !== '') {
            $this->snapshots->cleanup($snapshotId);
        }

        $restored = $this->runner->currentVersion('core', 'core');

        return [
            'ok'               => true,
            'restored_version' => $restored !== '' ? $restored : $target,
            'log'              => $result['log'],
        ];
    }

    /**
     * Build a uniform failed response.
     *
     * @param string $log Concise log (no secrets).
     * @return array{ok:bool,restored_version:string,log:string}
     */
    private function fail(string $log): array
    {
        return ['ok' => false, 'restored_version' => '', 'log' => $log];
    }
}
