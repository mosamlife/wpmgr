<?php
/**
 * Local destination — ADR-036 P1.
 *
 * Chunks land in `uploads/wpmgr-backups/<snapshot_id>/chunks/<hash>.bin`
 * on the same webserver as the WordPress site, routing through
 * StoragePaths::dataBase('backups') (uploads-first, wp-content fallback).
 * This mirrors the "local folder" destination offered by leading backup plugins
 * (the most-used option because customers without S3 credentials still want a
 * backup off the live tree). The manifest is written next to the chunks AND a
 * metadata-only POST still goes to the CP so the snapshot shows up in the
 * operator UI; only the bytes stay local.
 *
 * Deny-by-default is critical: a backup zip is a complete copy of the site,
 * including wp-config.php credentials and the SQL dump. We drop the following
 * guard files into the base dir on first prepare (via StoragePaths::ensureHardened):
 *
 *   - .htaccess          (Apache/LiteSpeed deny — auto-applied)
 *   - index.php          (PHP directory-listing silence guard — auto-applied)
 *   - web.config         (IIS deny — covers Microsoft-host customers)
 *   - nginx.conf.snippet (sample directive — customer must include it)
 *   - README.txt         (explains the directory + nginx instructions)
 *
 * Apache, LiteSpeed, and PHP-proxied servers are protected automatically;
 * nginx needs an operator action so we ship a clear README pointing at the
 * snippet.
 *
 * @package WPMgr\Agent\Backup\Destinations
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup\Destinations;

use WPMgr\Agent\Support\BackupTransport;
use WPMgr\Agent\Support\StoragePaths;

/**
 * Writes ciphertext chunks to a local directory under wp-content. The CP only
 * sees the manifest metadata (so it can list / retain / delete the snapshot),
 * never the bytes — those stay on the customer's webserver.
 */
final class LocalDestination implements BackupDestination
{
    /** Subdir under WP_CONTENT_DIR / uploads (chosen at prepare time). */
    private const BASE_DIR_NAME = 'wpmgr-backups';

    /** Chmod for chunk files: owner rw, group r, world none. */
    private const CHUNK_MODE = 0640;

    /** Chmod for the per-snapshot directory: owner only. */
    private const DIR_MODE = 0700;

    private BackupTransport $transport;
    private string $snapshotId;
    private string $manifestEndpoint;
    private string $snapshotDir = '';

    /**
     * Sanitized `destination_config.local_path_prefix` (operator-configured
     * on-disk path, relative to WP_CONTENT_DIR). Empty when the CP sent none
     * (or sent a value that sanitized down to nothing) — resolveBaseDir()
     * then falls back to the agent's own uploads-first default.
     */
    private string $localPathPrefix;

    /**
     * @param BackupTransport $transport        For the metadata-only manifest POST
     *                                          (the agent still signs it with the
     *                                          existing Ed25519 transport so the
     *                                          CP authenticates the caller).
     * @param string          $snapshotId       In-flight snapshot UUID.
     * @param string          $manifestEndpoint CP /manifest URL (metadata only).
     * @param string          $localPathPrefix  Optional operator-configured on-disk
     *                                          path (relative to WP_CONTENT_DIR) from
     *                                          `destination_config.local_path_prefix`.
     *                                          Empty string = agent default.
     */
    public function __construct(
        BackupTransport $transport,
        string $snapshotId,
        string $manifestEndpoint,
        string $localPathPrefix = ''
    ) {
        $this->transport        = $transport;
        $this->snapshotId       = $snapshotId;
        $this->manifestEndpoint = $manifestEndpoint;
        $this->localPathPrefix  = self::sanitizePrefix($localPathPrefix);
    }

    /**
     * Resolve the base dir (uploads-first via StoragePaths; wp-content fallback),
     * create the per-snapshot chunk dir, and drop the deny-by-default config files.
     *
     * resolveBaseDir() delegates to StoragePaths for uploads-first resolution and
     * handles the best-effort legacy migration. StoragePaths::ensureHardened() then
     * drops .htaccess + index.php web-access guards (required because uploads/ is
     * web-accessible). ensureBaseGuardFiles() adds IIS web.config, nginx snippet,
     * and README for additional server coverage.
     */
    public function prepare(string $snapshotId): void
    {
        $base = $this->resolveBaseDir();
        // Apply web-access hardening (.htaccess + index.php) on the path that is
        // ACTUALLY written to. resolveBaseDir() can return the legacy wp-content
        // location on read-only-uploads hosts, so harden $base directly rather
        // than the uploads path StoragePaths::dataBase() would recompute. Guards
        // are idempotent (file_exists short-circuits).
        StoragePaths::ensureHardenedPath($base);
        $this->ensureBaseGuardFiles($base);

        $snapshotDir = $base . DIRECTORY_SEPARATOR . $snapshotId;
        $chunksDir   = $snapshotDir . DIRECTORY_SEPARATOR . 'chunks';
        if (!is_dir($chunksDir) && !mkdir($chunksDir, self::DIR_MODE, true) && !is_dir($chunksDir)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir -- explicit 0700 perms on secret/scratch dir; wp_mkdir_p would apply the wider FS_CHMOD_DIR
            throw new \RuntimeException('WPMgr Local Destination: cannot create chunks dir at ' . esc_html($chunksDir));
        }
        @chmod($snapshotDir, self::DIR_MODE); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0700); WP_Filesystem would coerce to wider FS_CHMOD_DIR
        @chmod($chunksDir, self::DIR_MODE); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0700); WP_Filesystem would coerce to wider FS_CHMOD_DIR

        $this->snapshotDir = $snapshotDir;
    }

    /**
     * Write the ciphertext to disk under the per-snapshot chunks dir. Uses
     * LOCK_EX so a watchdog re-entry concurrently writing the same hash
     * cannot tear the file mid-write. The "skip if exists" check is the local
     * counterpart of the CP-side dedup: identical chunks across snapshots
     * already collide on the same hash filename.
     */
    public function putChunk(string $hash, string $ciphertext): bool
    {
        if (!self::isValidHash($hash)) {
            return false;
        }
        $path = $this->chunkPath($hash);
        if (is_file($path)) {
            return true;
        }
        $bytes = strlen($ciphertext);
        $written = @file_put_contents($path, $ciphertext, LOCK_EX);
        if ($written !== $bytes) {
            return false;
        }
        @chmod($path, self::CHUNK_MODE); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0640); WP_Filesystem would coerce to wider FS_CHMOD_FILE
        return true;
    }

    /**
     * Read one stored chunk back by content hash — used by the restore path
     * (RestoreRunner::fetchLocalChunk) for a `destination_kind=local`
     * snapshot, whose manifest chunks carry no presigned URL.
     *
     * Validates the hash shape BEFORE building a filesystem path from it
     * (defense in depth: the hash arrives over the wire in a RestoreRequest,
     * so — unlike the backup-side putChunk, whose hash is always this
     * process's own Blake3::hashHex() output — it must be treated as
     * untrusted input here).
     */
    public function getChunk(string $hash): ?string
    {
        if (!self::isValidHash($hash)) {
            return null;
        }
        $path = $this->chunkPath($hash);
        if (!is_file($path)) {
            return null;
        }
        $bytes = @file_get_contents($path);
        return is_string($bytes) ? $bytes : null;
    }

    /**
     * Write the signed manifest JSON next to the chunks AND POST the metadata-
     * only shape to the CP so the snapshot shows up in the operator UI.
     *
     * The CP's submit handler counts the manifest entries' chunk lists for
     * `chunk_count` / `stored_count` regardless of whether the bytes
     * physically arrived — so a local-destination snapshot looks like any
     * other completed snapshot in the dashboard, just with a
     * `destination_kind='local'` tag on the row.
     */
    public function submitManifest(array $entries, array $meta): array
    {
        if ($this->snapshotDir === '') {
            // submitManifest can be called on a fresh runner re-entry (the
            // prepare step's dir resolution didn't survive a process boundary).
            // Re-resolve so we can still drop the on-disk manifest.
            $this->prepare($this->snapshotId);
        }

        $ageRecipient = isset($meta['age_recipient']) && is_string($meta['age_recipient'])
            ? $meta['age_recipient']
            : '';

        // Persist the manifest alongside the chunks so a restore can run
        // entirely off-CP if the operator later pulls the dir.
        $manifestPath = $this->snapshotDir . DIRECTORY_SEPARATOR . 'manifest.json';
        $manifestJson = (string) wp_json_encode([
            'snapshot_id'   => $this->snapshotId,
            'age_recipient' => $ageRecipient,
            'entries'       => $entries,
            'written_at'    => time(),
        ]);
        @file_put_contents($manifestPath, $manifestJson, LOCK_EX);
        @chmod($manifestPath, self::CHUNK_MODE); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0640); WP_Filesystem would coerce to wider FS_CHMOD_FILE

        // POST the SAME manifest shape to the CP. The CP already knows this
        // snapshot's destination_kind='local' + destination_config — it
        // recorded them on the snapshot row when it enqueued the
        // BackupRequest, and DestinationResolver used those exact same
        // fields (from runner params) to build THIS LocalDestination
        // instance. So this POST doesn't need to repeat the destination
        // choice; it is purely the metadata-only shipment: we send the
        // manifest (paths, chunk hashes/sizes), never the chunk bytes.
        return $this->transport->submitManifest(
            $this->manifestEndpoint,
            $this->snapshotId,
            $ageRecipient,
            // @phpstan-ignore-next-line — manifest entries shape enforced upstream.
            $entries
        );
    }

    public function deleteChunks(array $hashes): void
    {
        if ($this->snapshotDir === '') {
            // Best-effort: re-resolve so GC can still clean up.
            try {
                $this->prepare($this->snapshotId);
            } catch (\Throwable $_) {
                return;
            }
        }
        foreach ($hashes as $hash) {
            if (!is_string($hash) || !self::isValidHash($hash)) {
                continue;
            }
            $path = $this->chunkPath($hash);
            if (is_file($path)) {
                wp_delete_file($path);
            }
        }
    }

    public function getKind(): string
    {
        return 'local';
    }

    /**
     * Pick the base dir for wpmgr-backups/.
     *
     * When the operator configured an explicit `local_path_prefix`
     * (destination_config), that path wins outright — see
     * resolveConfiguredBaseDir(). Otherwise this delegates to
     * StoragePaths::dataBase('backups') for uploads-first resolution
     * (wp.org Guideline compliance). Web-access guard files (.htaccess +
     * index.php) are written by StoragePaths::ensureHardened() because
     * uploads/ is web-accessible.
     *
     * Best-effort migration: if the legacy wp-content/wpmgr-backups directory exists
     * and the new uploads-based location does not yet exist, we atomically rename it
     * so existing local backups are preserved without operator intervention.
     *
     * Read-fallback: if the primary path is not writable but the legacy location is,
     * we use the legacy location so existing self-hosted flows are not broken.
     */
    private function resolveBaseDir(): string
    {
        if ($this->localPathPrefix !== '') {
            return $this->resolveConfiguredBaseDir();
        }

        // Primary candidate: uploads-first via StoragePaths.
        $primary = StoragePaths::dataBase('backups');
        $legacy  = StoragePaths::legacyBase('backups');

        // Best-effort migration: atomically move legacy dir to new location.
        if ($primary !== ''
            && $primary !== $legacy
            && $legacy !== ''
            && is_dir($legacy)
            && !is_dir($primary)
        ) {
            $newParent = dirname($primary);
            if (!is_dir($newParent) && function_exists('wp_mkdir_p')) {
                wp_mkdir_p($newParent);
            }
            // phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic directory rename; WP_Filesystem::move() is non-atomic and would leave partial state on failure
            @rename($legacy, $primary);
        }

        $candidates = array_filter(array_unique([$primary, $legacy]));
        foreach ($candidates as $candidate) {
            if (is_dir($candidate)) {
                if (is_writable($candidate)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_is_writable -- headless agent; WP_Filesystem never initialized; direct writability probe is the only option
                    return $candidate;
                }
                continue;
            }
            // Try to create the dir; if we succeed it's writable for us.
            if (@mkdir($candidate, self::DIR_MODE, true) || is_dir($candidate)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir -- explicit 0700 perms on secret backup dir; wp_mkdir_p would apply the wider FS_CHMOD_DIR
                @chmod($candidate, self::DIR_MODE); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0700); WP_Filesystem would coerce to wider FS_CHMOD_DIR
                if (is_writable($candidate)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_is_writable -- headless agent; WP_Filesystem never initialized; direct writability probe is the only option
                    return $candidate;
                }
            }
        }
        throw new \RuntimeException('WPMgr Local Destination: no writable base dir under uploads or wp-content');
    }

    /**
     * Resolve the operator-configured base dir: `WP_CONTENT_DIR/<localPathPrefix>`.
     *
     * Unlike resolveBaseDir()'s uploads-first default, an explicit
     * local_path_prefix is an operator choice — we honor it outright rather
     * than falling back to a different candidate on a writability failure
     * (a silent fallback would mean "the operator picked path X but their
     * backup landed on path Y", which is exactly the kind of surprise a
     * storage-destination feature must not produce).
     *
     * $this->localPathPrefix was already sanitized in the constructor (no
     * `..`, no absolute-path leader, no traversal-capable characters), so
     * the join below cannot escape WP_CONTENT_DIR.
     */
    private function resolveConfiguredBaseDir(): string
    {
        if (!defined('WP_CONTENT_DIR') || !is_string(WP_CONTENT_DIR) || WP_CONTENT_DIR === '') {
            throw new \RuntimeException('WPMgr Local Destination: WP_CONTENT_DIR is unavailable for the configured local_path_prefix');
        }

        $wpContent = rtrim(WP_CONTENT_DIR, '/\\');
        $relative  = str_replace('/', DIRECTORY_SEPARATOR, $this->localPathPrefix);
        $base      = $wpContent . DIRECTORY_SEPARATOR . $relative;

        if (!is_dir($base)) {
            if (!@mkdir($base, self::DIR_MODE, true) && !is_dir($base)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir -- explicit 0700 perms on operator-configured backup dir; wp_mkdir_p would apply the wider FS_CHMOD_DIR
                throw new \RuntimeException('WPMgr Local Destination: cannot create configured local_path_prefix dir at ' . esc_html($base));
            }
            @chmod($base, self::DIR_MODE); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0700); WP_Filesystem would coerce to wider FS_CHMOD_DIR
        }
        if (!is_writable($base)) { // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_is_writable -- headless agent; WP_Filesystem never initialized; direct writability probe is the only option
            throw new \RuntimeException('WPMgr Local Destination: configured local_path_prefix dir not writable: ' . esc_html($base));
        }

        return $base;
    }

    /**
     * Sanitize `destination_config.local_path_prefix` into a traversal-safe
     * relative path. The CP is expected to validate this operator input
     * before it ever reaches the agent, but the agent treats every wire
     * field as untrusted (VALIDATE-early / SANITIZE-before-use) — especially
     * one that is about to become part of a filesystem path.
     *
     * Splits on both `/` and `\`, drops empty/`.`/`..` segments outright
     * (no traversal is possible even if every other segment were hostile),
     * and strips each remaining segment down to a conservative
     * `[A-Za-z0-9_.-]` charset (defeats NUL-byte injection, drive letters,
     * and stray control characters). Returns '' — meaning "use the agent's
     * own default" — when nothing safe survives.
     */
    private static function sanitizePrefix(string $prefix): string
    {
        $prefix = trim($prefix);
        if ($prefix === '') {
            return '';
        }

        $normalized = str_replace('\\', '/', $prefix);
        $segments   = explode('/', $normalized);
        $safe       = [];
        foreach ($segments as $segment) {
            if ($segment === '' || $segment === '.' || $segment === '..') {
                continue;
            }
            $clean = preg_replace('/[^A-Za-z0-9_.\-]/', '', $segment) ?? '';
            if ($clean === '' || $clean === '.' || $clean === '..') {
                continue;
            }
            $safe[] = $clean;
        }

        return implode('/', $safe);
    }

    /**
     * Validate that $hash is shaped like a BLAKE3 hex digest (32 bytes ->
     * 64 lowercase/uppercase hex chars) BEFORE it is used to build a
     * filesystem path (chunkPath()). Guards putChunk/getChunk/deleteChunks
     * against path traversal via a malformed hash — this matters most for
     * getChunk(), whose $hash comes from a RestoreRequest chunk entry (wire
     * input), not from this process's own Blake3::hashHex() output.
     */
    private static function isValidHash(string $hash): bool
    {
        return preg_match('/^[a-f0-9]{64}$/i', $hash) === 1;
    }

    /**
     * Drop supplemental deny-by-default guard files + a README on first prepare.
     * Each file is only written if missing (idempotent across snapshots — the
     * same base dir is shared). .htaccess and index.php are already written by
     * StoragePaths::ensureHardened(); this method adds IIS and nginx coverage.
     */
    private function ensureBaseGuardFiles(string $base): void
    {
        // The actual on-disk path (may be under uploads/ or wp-content/ depending
        // on the host) is passed to the nginx snippet so operators can copy it.
        $nginxLocation = rtrim(str_replace('\\', '/', $base), '/') . '/';
        $files         = [
            'web.config'         => '<configuration><system.webServer><security><authorization><remove users="*" roles="" verbs="" /></authorization></security></system.webServer></configuration>',
            'nginx.conf.snippet' => "# Add inside the `server { ... }` block of your WordPress vhost.\n"
                                    . "location ~ ^" . preg_quote($nginxLocation, '~') . " { deny all; return 403; }\n",
            'README.txt'         => $this->readmeBody($base),
        ];
        foreach ($files as $name => $contents) {
            $path = $base . DIRECTORY_SEPARATOR . $name;
            if (is_file($path)) {
                continue;
            }
            @file_put_contents($path, $contents, LOCK_EX);
            @chmod($path, self::CHUNK_MODE); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0640) on deny-by-default guard files; WP_Filesystem would coerce to wider FS_CHMOD_FILE
        }
    }

    private function readmeBody(string $base = ''): string
    {
        $dirNote = $base !== '' ? rtrim(str_replace('\\', '/', $base), '/') . '/' : 'this directory';
        return "WPMgr local backups\n"
            . "===================\n\n"
            . "This directory holds backup chunks for sites that selected the\n"
            . "'Local folder' destination in WPMgr. The bytes are encrypted on\n"
            . "the agent before being written, but the layout (chunk filenames,\n"
            . "manifest contents) still reveals information about your site.\n\n"
            . "Apache and IIS users: the bundled .htaccess and web.config block\n"
            . "all public reads of this directory automatically.\n\n"
            . "nginx users: include the snippet in nginx.conf.snippet inside the\n"
            . "matching `server { ... }` block (or the relevant `include` you\n"
            . "already pull into the WordPress vhost). Reload nginx and verify\n"
            . "that a request to " . $dirNote . " returns a\n"
            . "403 before relying on this destination.\n";
    }

    private function chunkPath(string $hash): string
    {
        return $this->snapshotDir
            . DIRECTORY_SEPARATOR . 'chunks'
            . DIRECTORY_SEPARATOR . $hash . '.bin';
    }
}
