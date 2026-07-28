<?php
/**
 * Rollback command: restores a previously captured snapshot in response to a
 * verified, signed control-plane request.
 *
 * Contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/rollback
 *   body: { "type", "slug", "snapshot_id", "to_version" }
 *   response: { "ok": bool, "restored_version": "...", "log": "..." }
 *
 * For plugin/theme the snapshot directory is restored over the live directory.
 * For core a downgrade-by-version is performed (WP-CLI `core update
 * --version=<to_version> --force`, or the Core_Upgrader equivalent). On success
 * the snapshot directory is removed.
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
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused).
     * @param array<string,mixed> $params Request parameters.
     * @return array{ok:bool,restored_version:string,log:string}
     */
    public function execute(array $claims, array $params): array
    {
        // Heal a `.maintenance` flag left behind by a prior interrupted
        // update/rollback before starting new work, and arm the shutdown
        // backstop so a fatal error or a timeout mid-rollback still clears
        // whatever flag THIS run may leave set.
        Maintenance::healStaleIfPresent();
        Maintenance::armShutdownGuard();

        $type       = isset($params['type']) && is_string($params['type']) ? $params['type'] : '';
        $rawSlug    = isset($params['slug']) && is_string($params['slug']) ? $params['slug'] : '';
        $snapshotId = isset($params['snapshot_id']) && is_string($params['snapshot_id']) ? $params['snapshot_id'] : '';
        $toVersion  = isset($params['to_version']) && is_string($params['to_version']) ? $params['to_version'] : '';

        if (!in_array($type, self::TYPES, true)) {
            return $this->fail('Invalid type.');
        }

        // GUARANTEE: this is precisely the reported incident — a rollback
        // that itself fails (the new version is already active, the restore
        // errors, etc.) must still clear maintenance mode. Everything from
        // here on is wrapped so success, failure, or a thrown exception all
        // reach the `finally`.
        try {
            if ($type === 'core') {
                return $this->rollbackCore($snapshotId, $toVersion);
            }

            // plugin / theme
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
            Maintenance::clear();
        }
    }

    /**
     * Roll core back to a prior version via a forced downgrade.
     *
     * @param string $snapshotId Optional snapshot id holding the prior version.
     * @param string $toVersion  Target version (overrides snapshot when set).
     * @return array{ok:bool,restored_version:string,log:string}
     */
    private function rollbackCore(string $snapshotId, string $toVersion): array
    {
        $target = $toVersion !== ''
            ? $toVersion
            : ($snapshotId !== '' ? $this->snapshots->recordedVersion($snapshotId) : '');

        if ($target === '' || preg_match('#^[0-9][0-9A-Za-z.\-]*$#', $target) !== 1) {
            return $this->fail('No valid target core version.');
        }

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
