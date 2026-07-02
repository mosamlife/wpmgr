<?php
/**
 * Update command: applies plugin/theme/core updates in response to a verified,
 * signed control-plane request.
 *
 * Contract (CP -> agent):
 *   POST /wp-json/wpmgr/v1/command/update
 *   body: { "dry_run": bool, "snapshot": bool, "items": [ { "type", "slug", "version" } ] }
 *   response: { "ok": bool, "results": [ { type, slug, from_version, to_version,
 *               status, snapshot_id, log } ] }
 *
 * Execution strategy:
 *   - dry_run: never touches the filesystem; reports would_update / up_to_date.
 *   - snapshot=true (non-dry): a PRE-UPDATE snapshot is captured so RollbackCommand
 *     can restore. Plugin/theme directories are copied under
 *     wp-content/uploads/wpmgr-snapshots/<snapshot_id>/; core records its prior
 *     version (downgrade-by-version on rollback).
 *   - apply: prefers WP-CLI when available, else falls back to the WordPress
 *     upgrader APIs under a quiet skin.
 *
 * Every input is treated as untrusted: type is whitelisted, slug is sanitized to
 * reject path traversal, and snapshot paths are bounded to wp-content.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\Maintenance;
use WPMgr\Agent\Support\SnapshotManager;
use WPMgr\Agent\Support\UpdateRunner;

/**
 * Performs core/plugin/theme updates with optional pre-update snapshots.
 */
final class UpdateCommand implements CommandInterface
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
        return 'update';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (unused).
     * @param array<string,mixed> $params Request parameters.
     * @return array{ok:bool,results:array<int,array{type:string,slug:string,from_version:string,to_version:string,status:string,snapshot_id:string,log:string}>}
     */
    public function execute(array $claims, array $params): array
    {
        // Heal a `.maintenance` flag left behind by a prior interrupted
        // update/rollback before starting new work, and arm the shutdown
        // backstop so a fatal error or a timeout mid-update still clears
        // whatever flag THIS run may leave set.
        Maintenance::healStaleIfPresent();
        Maintenance::armShutdownGuard();

        $dryRun   = isset($params['dry_run']) && (bool) $params['dry_run'];
        $snapshot = isset($params['snapshot']) && (bool) $params['snapshot'];

        $items = isset($params['items']) && is_array($params['items']) ? $params['items'] : [];

        $results = [];
        $ok      = true;

        foreach ($items as $item) {
            $result = $this->processItem(is_array($item) ? $item : [], $dryRun, $snapshot);
            if ($result['status'] === 'failed') {
                $ok = false;
            }
            $results[] = $result;
        }

        return ['ok' => $ok, 'results' => $results];
    }

    /**
     * Process a single update item, catching errors so the batch continues.
     *
     * @param array<string,mixed> $item     One request item.
     * @param bool                $dryRun   Whether to avoid mutation.
     * @param bool                $snapshot Whether to capture a pre-update snapshot.
     * @return array{type:string,slug:string,from_version:string,to_version:string,status:string,snapshot_id:string,log:string}
     */
    private function processItem(array $item, bool $dryRun, bool $snapshot): array
    {
        $type    = isset($item['type']) && is_string($item['type']) ? $item['type'] : '';
        $rawSlug = isset($item['slug']) && is_string($item['slug']) ? $item['slug'] : '';
        $version = isset($item['version']) && is_string($item['version']) && $item['version'] !== ''
            ? $item['version']
            : 'latest';

        // --- Validate type. ---
        if (!in_array($type, self::TYPES, true)) {
            return $this->result('', $rawSlug, '', '', 'failed', '', 'Invalid type.');
        }

        // --- Validate / sanitize slug (rejects path traversal). ---
        if ($type === 'core') {
            $slug = 'core';
        } else {
            $slug = self::sanitizeSlug($rawSlug);
            if ($slug === '' || $slug !== $rawSlug) {
                return $this->result($type, $rawSlug, '', '', 'failed', '', 'Invalid or unsafe slug.');
            }
        }

        try {
            // --- Not-applicable target: the plugin/theme isn't present on
            // this site at all (a stale or mistargeted task). Report this as
            // skipped, not failed, so it never pollutes the run's failure
            // count — there is nothing here to have failed.
            if (!$this->runner->isInstalled($type, $slug)) {
                return $this->result($type, $slug, '', '', 'skipped', '', 'Not installed on this site.');
            }

            $fromVersion = $this->runner->currentVersion($type, $slug);

            if ($dryRun) {
                return $this->dryRun($type, $slug, $version, $fromVersion);
            }

            // --- Not-applicable target: installed but nothing to do (already
            // at, or beyond, the requested/available version). Report this
            // up front as up_to_date without running a snapshot+apply — both
            // avoids needless work and keeps a target with no real update to
            // apply from ever being able to surface as a failure.
            $available = $this->runner->availableVersion($type, $slug, $version);
            if (!self::isUpdatable($available, $fromVersion, $version)) {
                return $this->result(
                    $type,
                    $slug,
                    $fromVersion,
                    $fromVersion,
                    'up_to_date',
                    '',
                    'Already up to date; no update applied.'
                );
            }

            // GUARANTEE: whatever this item's snapshot/apply work does below —
            // succeed, fail, or throw — a `finally` clears any `.maintenance`
            // flag before we move on, so a failed item can never leave the
            // whole site dark for the rest of the batch (or indefinitely).
            try {
                // --- Optional pre-update snapshot. ---
                $snapshotId = '';
                $log        = '';
                if ($snapshot) {
                    $snap        = $this->snapshots->capture($type, $slug, $fromVersion);
                    $snapshotId  = $snap['snapshot_id'];
                    $log        .= $snap['log'];
                }

                // --- Apply the update. ---
                $applied = $this->runner->apply($type, $slug, $version);
                $log    .= ($log !== '' ? "\n" : '') . $applied['log'];

                $toVersion = $this->runner->currentVersion($type, $slug);

                $status = $applied['ok']
                    ? ($toVersion !== $fromVersion ? 'succeeded' : 'up_to_date')
                    : 'failed';

                return $this->result($type, $slug, $fromVersion, $toVersion, $status, $snapshotId, $log);
            } finally {
                Maintenance::clear();
            }
        } catch (\Throwable $e) {
            // Never leak internals; keep the per-item failure contained.
            return $this->result($type, $slug, '', '', 'failed', '', 'Update error.');
        }
    }

    /**
     * Compute the dry-run outcome without touching the filesystem.
     *
     * @param string $type        Item type.
     * @param string $slug        Sanitized slug.
     * @param string $requested   Requested version ('latest' or x.y.z).
     * @param string $fromVersion Currently installed version.
     * @return array{type:string,slug:string,from_version:string,to_version:string,status:string,snapshot_id:string,log:string}
     */
    private function dryRun(string $type, string $slug, string $requested, string $fromVersion): array
    {
        $available = $this->runner->availableVersion($type, $slug, $requested);
        $updatable = self::isUpdatable($available, $fromVersion, $requested);

        $status = $updatable ? 'would_update' : 'up_to_date';
        $to     = $updatable ? $available : $fromVersion;

        return $this->result($type, $slug, $fromVersion, $to, $status, '', 'Dry run: no changes applied.');
    }

    /**
     * Is there an actual update to apply?
     *
     * Shared by the dry-run report and the real-apply pre-check so both agree
     * on what "nothing to do" means: no available version, the available
     * version already matches what's installed, or (for a 'latest' request)
     * the available version isn't actually newer.
     *
     * @param string $available   Version an update would move to ('' when none).
     * @param string $fromVersion Currently installed version.
     * @param string $requested   'latest' or an explicit x.y.z.
     * @return bool
     */
    private static function isUpdatable(string $available, string $fromVersion, string $requested): bool
    {
        return $available !== '' && $available !== $fromVersion
            && (version_compare($available, $fromVersion, '>') || $requested !== 'latest');
    }

    /**
     * Build a single normalized result row matching the contract shape exactly.
     *
     * @param string $type        Item type.
     * @param string $slug        Slug.
     * @param string $fromVersion From version.
     * @param string $toVersion   To version.
     * @param string $status      Status enum.
     * @param string $snapshotId  Snapshot id (or empty).
     * @param string $log         Concise log (no secrets).
     * @return array{type:string,slug:string,from_version:string,to_version:string,status:string,snapshot_id:string,log:string}
     */
    private function result(
        string $type,
        string $slug,
        string $fromVersion,
        string $toVersion,
        string $status,
        string $snapshotId,
        string $log
    ): array {
        return [
            'type'         => $type,
            'slug'         => $slug,
            'from_version' => $fromVersion,
            'to_version'   => $toVersion,
            'status'       => $status,
            'snapshot_id'  => $snapshotId,
            'log'          => $log,
        ];
    }

    /**
     * Sanitize an untrusted slug, rejecting anything that could escape its
     * intended directory. Accepts a plugin folder, a "folder/file.php" plugin
     * basename, or a theme stylesheet.
     *
     * @param string $slug Raw slug from the request body.
     * @return string Sanitized slug, or '' when unsafe.
     */
    public static function sanitizeSlug(string $slug): string
    {
        $slug = trim($slug);
        if ($slug === '') {
            return '';
        }

        // Reject path traversal and absolute paths outright.
        if (str_contains($slug, '..') || str_contains($slug, "\0")) {
            return '';
        }
        if ($slug[0] === '/' || $slug[0] === '\\') {
            return '';
        }
        // Reject Windows drive-letter absolute paths (e.g. C:\...).
        if (preg_match('#^[A-Za-z]:#', $slug) === 1) {
            return '';
        }

        // Allow only: alphanumerics, dash, underscore, dot, and a single forward
        // slash separating folder/file (plugin basename form).
        if (preg_match('#^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)?$#', $slug) !== 1) {
            return '';
        }

        return $slug;
    }
}
