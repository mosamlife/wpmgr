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
 * reject path traversal, and the snapshot id is validated by the manager.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\SnapshotManager;
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
        $type       = isset($params['type']) && is_string($params['type']) ? $params['type'] : '';
        $rawSlug    = isset($params['slug']) && is_string($params['slug']) ? $params['slug'] : '';
        $snapshotId = isset($params['snapshot_id']) && is_string($params['snapshot_id']) ? $params['snapshot_id'] : '';
        $toVersion  = isset($params['to_version']) && is_string($params['to_version']) ? $params['to_version'] : '';

        if (!in_array($type, self::TYPES, true)) {
            return $this->fail('Invalid type.');
        }

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
