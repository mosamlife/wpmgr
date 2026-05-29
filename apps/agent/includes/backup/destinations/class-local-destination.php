<?php
/**
 * Local destination — ADR-036 P1.
 *
 * Chunks land in `WP_CONTENT_DIR/wpmgr-backups/<snapshot_id>/chunks/<hash>.bin`
 * on the same webserver as the WordPress site. This mirrors WPvivid's own
 * "local folder" destination (the most-used option in their 40M install base
 * because customers without S3 credentials still want a backup off the live
 * tree). The manifest is written next to the chunks AND a metadata-only POST
 * still goes to the CP so the snapshot shows up in the operator UI; only the
 * bytes stay local.
 *
 * Deny-by-default is critical: a backup zip is a complete copy of the site,
 * including wp-config.php credentials and the SQL dump. We drop four files
 * into the base dir on first prepare:
 *
 *   - .htaccess          (Apache mod_rewrite deny — covers shared hosts)
 *   - web.config         (IIS — covers Microsoft-host customers)
 *   - nginx.conf.snippet (sample directive — customer must include it)
 *   - README.txt         (explains the directory + nginx instructions)
 *
 * Three of those are picked up automatically by the web server; nginx needs an
 * operator action so we ship a clear README pointing at the snippet.
 *
 * @package WPMgr\Agent\Backup\Destinations
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup\Destinations;

use WPMgr\Agent\Support\BackupTransport;

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
     * @param BackupTransport $transport        For the metadata-only manifest POST
     *                                          (the agent still signs it with the
     *                                          existing Ed25519 transport so the
     *                                          CP authenticates the caller).
     * @param string          $snapshotId       In-flight snapshot UUID.
     * @param string          $manifestEndpoint CP /manifest URL (metadata only).
     */
    public function __construct(
        BackupTransport $transport,
        string $snapshotId,
        string $manifestEndpoint
    ) {
        $this->transport        = $transport;
        $this->snapshotId       = $snapshotId;
        $this->manifestEndpoint = $manifestEndpoint;
    }

    /**
     * Resolve the base dir (WP_CONTENT_DIR preferred; uploads fallback), create
     * the per-snapshot chunk dir, and drop the deny-by-default config files.
     */
    public function prepare(string $snapshotId): void
    {
        $base = $this->resolveBaseDir();
        $this->ensureBaseGuardFiles($base);

        $snapshotDir = $base . DIRECTORY_SEPARATOR . $snapshotId;
        $chunksDir   = $snapshotDir . DIRECTORY_SEPARATOR . 'chunks';
        if (!is_dir($chunksDir) && !mkdir($chunksDir, self::DIR_MODE, true) && !is_dir($chunksDir)) {
            throw new \RuntimeException('WPMgr Local Destination: cannot create chunks dir at ' . $chunksDir);
        }
        @chmod($snapshotDir, self::DIR_MODE);
        @chmod($chunksDir, self::DIR_MODE);

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
        $path = $this->chunkPath($hash);
        if (is_file($path)) {
            return true;
        }
        $bytes = strlen($ciphertext);
        $written = @file_put_contents($path, $ciphertext, LOCK_EX);
        if ($written !== $bytes) {
            return false;
        }
        @chmod($path, self::CHUNK_MODE);
        return true;
    }

    public function getChunk(string $hash): ?string
    {
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
        @chmod($manifestPath, self::CHUNK_MODE);

        // POST the SAME manifest shape to the CP. The CP records the snapshot
        // as completed with `destination_kind='local'` (the destination_id
        // travels in BackupRequest so the CP already knows). This is the
        // metadata-only shipment: we send the manifest, not the chunks.
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
            if (!is_string($hash) || $hash === '') {
                continue;
            }
            $path = $this->chunkPath($hash);
            if (is_file($path)) {
                @unlink($path);
            }
        }
    }

    public function getKind(): string
    {
        return 'local';
    }

    /**
     * Pick the dir under which we'll create wpmgr-backups/. WP_CONTENT_DIR is
     * the preferred target (it's the canonical "writable WordPress dir") but
     * we fall back to the uploads basedir on hosts where wp-content itself is
     * read-only (e.g. some managed-WP providers).
     */
    private function resolveBaseDir(): string
    {
        $candidates = [];
        if (defined('WP_CONTENT_DIR') && is_string(WP_CONTENT_DIR) && WP_CONTENT_DIR !== '') {
            $candidates[] = rtrim((string) WP_CONTENT_DIR, '/\\') . DIRECTORY_SEPARATOR . self::BASE_DIR_NAME;
        }
        if (function_exists('wp_upload_dir')) {
            $upload = wp_upload_dir();
            if (is_array($upload) && isset($upload['basedir']) && is_string($upload['basedir']) && $upload['basedir'] !== '') {
                $candidates[] = rtrim($upload['basedir'], '/\\') . DIRECTORY_SEPARATOR . self::BASE_DIR_NAME;
            }
        }
        foreach ($candidates as $candidate) {
            if (is_dir($candidate)) {
                if (is_writable($candidate)) {
                    return $candidate;
                }
                continue;
            }
            // Try to create + chmod the dir; if we succeed it's writable for us.
            if (@mkdir($candidate, self::DIR_MODE, true) || is_dir($candidate)) {
                @chmod($candidate, self::DIR_MODE);
                if (is_writable($candidate)) {
                    return $candidate;
                }
            }
        }
        throw new \RuntimeException('WPMgr Local Destination: no writable base dir under wp-content or uploads');
    }

    /**
     * Drop deny-by-default web-server config + a README on first prepare. Each
     * file is only written if missing (idempotent across snapshots — the same
     * base dir is shared).
     */
    private function ensureBaseGuardFiles(string $base): void
    {
        $files = [
            '.htaccess' => "<IfModule mod_rewrite.c>\nRewriteEngine On\nRewriteRule .* - [F,L]\n</IfModule>\n",
            'web.config' => '<configuration><system.webServer><security><authorization><remove users="*" roles="" verbs="" /></authorization></security></system.webServer></configuration>',
            'nginx.conf.snippet' => "location ~ ^/wp-content/wpmgr-backups/ { deny all; return 403; }\n",
            'README.txt' => $this->readmeBody(),
        ];
        foreach ($files as $name => $contents) {
            $path = $base . DIRECTORY_SEPARATOR . $name;
            if (is_file($path)) {
                continue;
            }
            @file_put_contents($path, $contents, LOCK_EX);
            @chmod($path, self::CHUNK_MODE);
        }
    }

    private function readmeBody(): string
    {
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
            . "that GET https://your-site/wp-content/wpmgr-backups/ returns a\n"
            . "403 before relying on this destination.\n";
    }

    private function chunkPath(string $hash): string
    {
        return $this->snapshotDir
            . DIRECTORY_SEPARATOR . 'chunks'
            . DIRECTORY_SEPARATOR . $hash . '.bin';
    }
}
