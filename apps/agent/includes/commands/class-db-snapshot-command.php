<?php
/**
 * DbSnapshotCommand: local database snapshot tool (#189).
 *
 * A db_snapshot is a FAST LOCAL safety-net for the WP server filesystem, distinct
 * from the durable encrypted backups produced by BackupCommand. The dump SQL lives
 * under wp-content/wpmgr-snapshots/db/ (not encrypted, not uploaded to object
 * storage). It is designed for "capture before a risky change, revert in one click".
 *
 * Actions dispatched via the `action` field:
 *   create — dump the database using DbDumper (same engine as backups), write the
 *             snapshot .sql.gz + manifest, enforce the retention cap.
 *   list   — return the manifest index (ids, labels, sizes, timestamps).
 *   revert — import a snapshot SQL back into the live database (DESTRUCTIVE).
 *             Uses DbRestorer's tmp-prefix + atomic-swap strategy so WP stays
 *             readable while the import runs.
 *   delete — remove one snapshot from the local store.
 *
 * Security model:
 *   - The REST route is JWT-signed (Connector::verifyCommand) like every other
 *     command: cmd=db_snapshot bound into the token's `cmd` claim.
 *   - Defense-in-depth: current_user_can('manage_options') is checked by the
 *     Router's authorize() callback before execute() is ever called.
 *   - The snapshots directory is web-hardened on first use (index.php + .htaccess
 *     deny), mirroring the backup scratch dir pattern.
 *   - Revert requires the `confirm` field = 'REVERT' (exact match via hash_equals)
 *     as a destructive-action gate. Optionally, an auto-safety snapshot is taken
 *     before the import so the pre-revert state is not lost.
 *   - Retention cap: create enforces MAX_SNAPSHOTS (default 5); oldest is pruned
 *     first. Bound is configurable via `retention` param (capped to hard max 20).
 *   - Snapshot IDs and filenames are hex-generated; no user-supplied string is
 *     ever used as a filesystem path segment without validation.
 *   - SQL is piped through DbRestorer (the same parser the restore pipeline uses),
 *     not through an `exec(mysql ...)` shell call.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Backup\DbDumper;
use WPMgr\Agent\Backup\DbRestorer;
use WPMgr\Agent\Support\StoragePaths;

/**
 * Handles the `db_snapshot` command: create / list / revert / delete.
 */
final class DbSnapshotCommand implements CommandInterface
{
    /** Snapshot sub-directory under wpmgr-snapshots/. */
    private const STORE_SUBDIR = 'db';

    /** Default retention cap (snapshots kept). */
    private const DEFAULT_RETENTION = 5;

    /** Hard upper bound the caller cannot exceed. */
    private const MAX_RETENTION = 20;

    /** Exact confirmation token required for the destructive revert action. */
    private const REVERT_CONFIRM_TOKEN = 'REVERT';

    /** Tmp-table prefix used during the revert import. Ends with `_`. */
    private const TMP_PREFIX_CHARS = 8;

    public function name(): string
    {
        return 'db_snapshot';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims.
     * @param array<string,mixed> $params Request parameters.
     * @return array<string,mixed>
     */
    public function execute(array $claims, array $params): array
    {
        $action = isset($params['action']) && is_string($params['action'])
            ? $params['action']
            : '';

        switch ($action) {
            case 'create':
                return $this->actionCreate($params);
            case 'list':
                return $this->actionList();
            case 'revert':
                return $this->actionRevert($params);
            case 'delete':
                return $this->actionDelete($params);
            default:
                return ['ok' => false, 'detail' => 'unknown action; valid: create, list, revert, delete'];
        }
    }

    // =========================================================================
    // Action: create
    // =========================================================================

    /**
     * Create a new local database snapshot.
     *
     * @param array<string,mixed> $params
     * @return array<string,mixed>
     */
    private function actionCreate(array $params): array
    {
        $label     = isset($params['label']) && is_string($params['label'])
            ? substr(trim($params['label']), 0, 120)
            : '';
        $retention = isset($params['retention']) && is_numeric($params['retention'])
            ? min((int) $params['retention'], self::MAX_RETENTION)
            : self::DEFAULT_RETENTION;
        if ($retention < 1) {
            $retention = self::DEFAULT_RETENTION;
        }

        try {
            $storeDir = $this->resolveStoreDir();
        } catch (\Throwable $e) {
            return ['ok' => false, 'detail' => 'snapshot store unavailable: ' . $e->getMessage()];
        }

        // Enforce retention cap BEFORE writing a new snapshot so we never exceed the
        // limit even if a prior deletion was incomplete.
        $this->pruneOldest($storeDir, $retention - 1);

        // Generate a unique snapshot ID (snap_<16 hex chars>).
        $snapId   = $this->newId();
        $snapDir  = $storeDir . '/' . $snapId;
        $dumpPath = $snapDir . '/db.sql.gz';

        if (!@mkdir($snapDir, 0700, true) && !is_dir($snapDir)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir -- explicit 0700 perms on snapshot dir; wp_mkdir_p would apply the wider FS_CHMOD_DIR
            return ['ok' => false, 'detail' => 'cannot create snapshot directory'];
        }
        @chmod($snapDir, 0700); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0700); WP_Filesystem would coerce to wider FS_CHMOD_DIR

        // Count tables before dumping so the manifest has an accurate table_count.
        $tableCount = $this->countTables();

        try {
            $creds   = $this->dbCreds();
            $dumper  = new DbDumper($creds);
            $result  = $dumper->dump($dumpPath, [], static function (string $phase, array $detail): void {
                // Progress is a local fast-path — we do not have a CP progress
                // endpoint here. Silently discard.
            });
        } catch (\Throwable $e) {
            // Clean up the half-written directory on failure.
            $this->deleteDir($snapDir);
            return ['ok' => false, 'detail' => 'dump failed: ' . substr($e->getMessage(), 0, 300)];
        }

        $bytes = isset($result['bytes']) && is_int($result['bytes']) ? $result['bytes'] : 0;
        if ($bytes <= 0) {
            $this->deleteDir($snapDir);
            return ['ok' => false, 'detail' => 'dump produced empty output'];
        }

        // Write per-snapshot metadata.
        $meta = [
            'id'          => $snapId,
            'label'       => $label,
            'created_at'  => time(),
            'size'        => $bytes,
            'table_count' => $tableCount,
        ];
        @file_put_contents($snapDir . '/meta.json', (string) wp_json_encode($meta), LOCK_EX);
        @chmod($snapDir . '/meta.json', 0600); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0600); WP_Filesystem would coerce to wider FS_CHMOD_FILE

        return [
            'ok'     => true,
            'detail' => 'snapshot created',
            'snapshot' => $meta,
        ];
    }

    // =========================================================================
    // Action: list
    // =========================================================================

    /**
     * Return the manifest of all stored snapshots, newest first.
     *
     * @return array<string,mixed>
     */
    private function actionList(): array
    {
        try {
            $storeDir = $this->resolveStoreDir();
        } catch (\Throwable $e) {
            // Not an error if the directory simply doesn't exist yet.
            return ['ok' => true, 'snapshots' => []];
        }

        $snapshots = $this->loadManifest($storeDir);

        // Sort newest first for the UI.
        usort($snapshots, static function (array $a, array $b): int {
            return (int) ($b['created_at'] ?? 0) - (int) ($a['created_at'] ?? 0);
        });

        return ['ok' => true, 'snapshots' => $snapshots];
    }

    // =========================================================================
    // Action: revert
    // =========================================================================

    /**
     * Import a previously-captured snapshot into the live database.
     *
     * This is a DESTRUCTIVE whole-database overwrite. The operator MUST pass
     * `confirm = "REVERT"` (exact match). An auto-safety snapshot is taken
     * before the import so the pre-revert state is preserved locally.
     *
     * @param array<string,mixed> $params
     * @return array<string,mixed>
     */
    private function actionRevert(array $params): array
    {
        // --- Destructive confirm gate ---
        $confirm = isset($params['confirm']) && is_string($params['confirm'])
            ? $params['confirm']
            : '';
        if (!hash_equals(self::REVERT_CONFIRM_TOKEN, $confirm)) {
            return [
                'ok'     => false,
                'detail' => 'revert requires confirm="REVERT"; this operation replaces the entire database',
            ];
        }

        $snapId = isset($params['snapshot_id']) && is_string($params['snapshot_id'])
            ? $params['snapshot_id']
            : '';
        if (!$this->validId($snapId)) {
            return ['ok' => false, 'detail' => 'invalid snapshot_id'];
        }

        try {
            $storeDir = $this->resolveStoreDir();
        } catch (\Throwable $e) {
            return ['ok' => false, 'detail' => 'snapshot store unavailable'];
        }

        $snapDir  = $storeDir . '/' . $snapId;
        $dumpPath = $snapDir . '/db.sql.gz';

        // Containment: resolved path must sit under storeDir.
        if ($this->containedRealpath($snapDir, $storeDir) === '') {
            return ['ok' => false, 'detail' => 'invalid snapshot_id'];
        }
        if (!is_file($dumpPath)) {
            return ['ok' => false, 'detail' => 'snapshot file not found'];
        }

        // --- Optional auto-safety snapshot before the destructive import ---
        // Take a quick snapshot of the current DB state so the operator can
        // recover if the revert turns out to be wrong.
        $safetyId = '';
        $autoSafety = isset($params['skip_safety_snapshot']) && $params['skip_safety_snapshot'] === true
            ? false
            : true;
        if ($autoSafety) {
            try {
                $safetyResult = $this->actionCreate([
                    'label'     => 'Auto: pre-revert to ' . $snapId,
                    'retention' => self::MAX_RETENTION, // Don't prune the safety snapshot immediately.
                ]);
                if (!empty($safetyResult['ok']) && isset($safetyResult['snapshot']['id'])) {
                    $safetyId = (string) $safetyResult['snapshot']['id'];
                }
            } catch (\Throwable $e) {
                // Best-effort — a failed safety snapshot must not block the revert.
            }
        }

        // --- Run the DB import using DbRestorer ---
        // DbRestorer's restore() + swap() does: parse gzip SQL → rewrite prefix
        // (source prefix = dest prefix here, so the rewrite is a no-op) → replay
        // into tmp tables → atomically rename each tmp table over the live table.
        // This keeps WP readable throughout the import.
        try {
            $creds      = $this->dbCreds();
            $restorer   = new DbRestorer($creds);
            $tmpPrefix  = 'wpmsnap' . strtolower(bin2hex(random_bytes(self::TMP_PREFIX_CHARS / 2))) . '_';
            $srcPrefix  = $creds['prefix'];
            $noop       = static function (string $phase, array $detail): void {};

            $tmpTables = $restorer->restore($dumpPath, $tmpPrefix, $srcPrefix, $noop);
            $restorer->swap($tmpPrefix, $srcPrefix, $tmpTables, $noop);
        } catch (\Throwable $e) {
            // Best-effort cleanup of stray tmp tables.
            try {
                if (isset($restorer, $tmpPrefix)) {
                    $restorer->dropTmpTables($tmpPrefix);
                }
            } catch (\Throwable $ignored) {
            }
            return [
                'ok'        => false,
                'detail'    => 'revert failed: ' . substr($e->getMessage(), 0, 300),
                'safety_id' => $safetyId,
            ];
        }

        return [
            'ok'        => true,
            'detail'    => 'database reverted to snapshot',
            'safety_id' => $safetyId,
        ];
    }

    // =========================================================================
    // Action: delete
    // =========================================================================

    /**
     * Remove a snapshot from the local store.
     *
     * @param array<string,mixed> $params
     * @return array<string,mixed>
     */
    private function actionDelete(array $params): array
    {
        $snapId = isset($params['snapshot_id']) && is_string($params['snapshot_id'])
            ? $params['snapshot_id']
            : '';
        if (!$this->validId($snapId)) {
            return ['ok' => false, 'detail' => 'invalid snapshot_id'];
        }

        try {
            $storeDir = $this->resolveStoreDir();
        } catch (\Throwable $e) {
            return ['ok' => false, 'detail' => 'snapshot store unavailable'];
        }

        $snapDir = $storeDir . '/' . $snapId;

        // Containment check before any filesystem operation.
        if ($this->containedRealpath($snapDir, $storeDir) === '') {
            return ['ok' => false, 'detail' => 'invalid snapshot_id'];
        }
        if (!is_dir($snapDir)) {
            return ['ok' => false, 'detail' => 'snapshot not found'];
        }

        $deleted = $this->deleteDir($snapDir);

        return $deleted
            ? ['ok' => true, 'detail' => 'snapshot deleted']
            : ['ok' => false, 'detail' => 'could not fully remove snapshot directory'];
    }

    // =========================================================================
    // Store management helpers
    // =========================================================================

    /**
     * Resolve (and create if needed) the snapshots store directory.
     * Drops web-protection files on first use.
     *
     * @throws \RuntimeException When no writable location can be found.
     */
    private function resolveStoreDir(): string
    {
        // Uploads-first (wp.org Guideline compliance): user-generated DB snapshots
        // are stored under uploads/wpmgr-snapshots/db rather than wp-content/.
        // Falls back to the legacy wp-content location for read-only-uploads hosts.
        $base = StoragePaths::dataBase('snapshots');
        if ($base === '') {
            throw new \RuntimeException('WPMgr DB Snapshot: cannot resolve a writable base directory (uploads and WP_CONTENT_DIR both unavailable)');
        }
        $base = $base . '/' . self::STORE_SUBDIR;

        // Create the parent dir tree if needed.
        if (!is_dir($base)) {
            if (function_exists('wp_mkdir_p')) {
                if (!wp_mkdir_p($base) && !is_dir($base)) {
                    throw new \RuntimeException('cannot create snapshots directory: ' . esc_html($base));
                }
            } elseif (!@mkdir($base, 0700, true) && !is_dir($base)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir -- explicit 0700 perms on snapshot dir; wp_mkdir_p would apply the wider FS_CHMOD_DIR
                throw new \RuntimeException('cannot create snapshots directory: ' . esc_html($base));
            }
        }

        if (!is_writable($base)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_is_writable -- headless agent; WP_Filesystem never initialized; direct writability probe is the only option
            throw new \RuntimeException('snapshots directory is not writable: ' . esc_html($base));
        }

        @chmod($base, 0700); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0700); WP_Filesystem would coerce to wider FS_CHMOD_DIR

        // Harden against web access on first setup. Files are written once;
        // missing-on-web-serve → the .htaccess/index.php barrier is the guard.
        $this->ensureWebGuard($base);

        return $base;
    }

    /**
     * Drop an index.php and .htaccess in the base dir so web servers cannot
     * serve snapshot files directly.
     *
     * @param string $base Absolute snapshots store directory.
     */
    private function ensureWebGuard(string $base): void
    {
        $index = $base . '/index.php';
        if (!file_exists($index)) {
            @file_put_contents($index, "<?php\n// Silence is golden.\n");
            @chmod($index, 0644); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0644); WP_Filesystem would coerce to wider FS_CHMOD_FILE
        }

        $htaccess = $base . '/.htaccess';
        if (!file_exists($htaccess)) {
            @file_put_contents(
                $htaccess,
                "# Block all direct web access to local database snapshots.\n"
                . "<IfModule mod_authz_core.c>\n"
                . "    Require all denied\n"
                . "</IfModule>\n"
                . "<IfModule !mod_authz_core.c>\n"
                . "    Deny from all\n"
                . "</IfModule>\n"
            );
            @chmod($htaccess, 0644); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0644); WP_Filesystem would coerce to wider FS_CHMOD_FILE
        }
    }

    /**
     * Read all snapshot metadata entries from the store directory.
     *
     * @param string $storeDir Absolute store directory.
     * @return list<array<string,mixed>>
     */
    private function loadManifest(string $storeDir): array
    {
        $entries = [];
        $items   = @scandir($storeDir);
        if (!is_array($items)) {
            return $entries;
        }
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            if (!$this->validId($item)) {
                continue;
            }
            $metaFile = $storeDir . '/' . $item . '/meta.json';
            if (!is_file($metaFile)) {
                continue;
            }
            $raw = @file_get_contents($metaFile);
            if (!is_string($raw) || $raw === '') {
                continue;
            }
            $meta = @json_decode($raw, true);
            if (!is_array($meta)) {
                continue;
            }
            // Ensure required fields exist so the CP can rely on the shape.
            $entries[] = [
                'id'          => (string) ($meta['id'] ?? $item),
                'label'       => (string) ($meta['label'] ?? ''),
                'created_at'  => (int) ($meta['created_at'] ?? 0),
                'size'        => (int) ($meta['size'] ?? 0),
                'table_count' => (int) ($meta['table_count'] ?? 0),
            ];
        }
        return $entries;
    }

    /**
     * Prune old snapshots so at most $keepCount remain after pruning.
     * Sorted by created_at ascending so the OLDEST are removed first.
     *
     * @param string $storeDir  Absolute store directory.
     * @param int    $keepCount Maximum number of snapshots to retain.
     */
    private function pruneOldest(string $storeDir, int $keepCount): void
    {
        if ($keepCount < 0) {
            $keepCount = 0;
        }
        $snapshots = $this->loadManifest($storeDir);
        if (count($snapshots) <= $keepCount) {
            return;
        }

        // Sort ascending by created_at so oldest come first.
        usort($snapshots, static function (array $a, array $b): int {
            return (int) ($a['created_at'] ?? 0) - (int) ($b['created_at'] ?? 0);
        });

        $toRemove = count($snapshots) - $keepCount;
        for ($i = 0; $i < $toRemove && $i < count($snapshots); $i++) {
            $id  = (string) ($snapshots[$i]['id'] ?? '');
            if (!$this->validId($id)) {
                continue;
            }
            $dir = $storeDir . '/' . $id;
            if ($this->containedRealpath($dir, $storeDir) !== '') {
                $this->deleteDir($dir);
            }
        }
    }

    // =========================================================================
    // DB helpers
    // =========================================================================

    /**
     * Build DB credentials from WP runtime constants.
     *
     * @return array{host:string,user:string,password:string,name:string,prefix:string}
     */
    private function dbCreds(): array
    {
        global $wpdb;
        return [
            'host'     => defined('DB_HOST')     ? (string) DB_HOST     : 'localhost',
            'user'     => defined('DB_USER')     ? (string) DB_USER     : '',
            'password' => defined('DB_PASSWORD') ? (string) DB_PASSWORD : '',
            'name'     => defined('DB_NAME')     ? (string) DB_NAME     : '',
            'prefix'   => is_object($wpdb) && isset($wpdb->prefix) ? (string) $wpdb->prefix : 'wp_',
        ];
    }

    /**
     * Count the number of base tables in the database.
     * Returns 0 when the count cannot be determined (non-fatal).
     */
    private function countTables(): int
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            return 0;
        }
        /** @var \wpdb $wpdb */
        try {
            $count = $wpdb->get_var("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE = 'BASE TABLE'"); // phpcs:ignore WordPress.DB.PreparedSQL.NotPrepared,WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- static literal query on information_schema; direct query; no caching appropriate for a live count
            return is_numeric($count) ? (int) $count : 0;
        } catch (\Throwable $e) {
            return 0;
        }
    }

    // =========================================================================
    // Filesystem primitives
    // =========================================================================

    /**
     * Generate a unique snapshot identifier: snap_<24 hex chars>.
     *
     * @return string
     */
    private function newId(): string
    {
        try {
            return 'snap_' . bin2hex(random_bytes(12));
        } catch (\Throwable $e) {
            return 'snap_' . sprintf('%016x', time()) . sprintf('%08x', random_int(0, 0xFFFFFFFF));
        }
    }

    /**
     * Validate a snapshot ID against the allowed pattern.
     * Only accepts ids generated by newId(): `snap_` + exactly 24 lowercase hex chars.
     *
     * @param string $id Candidate ID.
     * @return bool
     */
    private function validId(string $id): bool
    {
        return preg_match('/^snap_[0-9a-f]{24}$/', $id) === 1;
    }

    /**
     * Verify that a path is contained within a trusted base directory.
     * Returns the canonicalized path if contained, '' otherwise.
     *
     * @param string $path Candidate path.
     * @param string $base Trusted base directory.
     * @return string
     */
    private function containedRealpath(string $path, string $base): string
    {
        $realBase = realpath($base);
        if ($realBase === false) {
            return '';
        }
        // The path may not yet exist (a snapshot dir being created). Walk up to
        // the nearest existing ancestor and check containment there.
        $p = $path;
        while ($p !== '' && $p !== dirname($p)) {
            $real = @realpath($p);
            if ($real !== false) {
                $realBase = rtrim($realBase, '/\\');
                if ($real !== $realBase && !str_starts_with($real, $realBase . DIRECTORY_SEPARATOR)) {
                    return '';
                }
                return $real;
            }
            $p = dirname($p);
        }
        return '';
    }

    /**
     * Recursively delete a directory. Skips symlinks.
     *
     * @param string $dir Absolute path.
     * @return bool True when all items and the dir itself were removed.
     */
    private function deleteDir(string $dir): bool
    {
        if (!is_dir($dir)) {
            return false;
        }
        $items = @scandir($dir);
        if ($items === false) {
            return false;
        }
        $ok = true;
        foreach ($items as $item) {
            if ($item === '.' || $item === '..') {
                continue;
            }
            $path = $dir . '/' . $item;
            if (is_dir($path) && !is_link($path)) {
                $ok = $this->deleteDir($path) && $ok;
            } else {
                wp_delete_file($path);
                $ok = (!file_exists($path)) && $ok;
            }
        }
        $ok = (@rmdir($dir) !== false) && $ok; // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- removes an empty server-derived snapshot dir; WP_Filesystem not initialized
        return $ok;
    }
}
