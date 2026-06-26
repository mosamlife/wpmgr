<?php
// File: class-file-read-command.php — FileReadCommand.

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\FileScanner;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * FileReadCommand: read one file (≤ 256 KiB, base64-encoded) within the site jail.
 *
 * Wire contract (CP→agent):
 *   POST /wp-json/wpmgr/v1/command/file_read
 *   Authorization: Bearer <Ed25519 JWT cmd="file_read">
 *   Body: {
 *     "path":               <site-relative path, forward slashes>,
 *     "max_bytes":          <int — default+hard cap 262144>,
 *     "confirm_sensitive":  <bool — default false>
 *   }
 *
 * Response (200 OK):
 *   {
 *     "path":           <string — site-relative>,
 *     "size":           <int — full file size on disk>,
 *     "mtime":          <int — Unix timestamp>,
 *     "mode":           <string — octal e.g. "0644">,
 *     "encoding":       "base64",
 *     "content_base64": <string>,
 *     "truncated":      <bool — true when file was larger than max_bytes>
 *   }
 *
 * Errors:
 *   { "error": { "code": <string>, "message": <string> } }
 *   Codes: invalid_path, outside_root, not_found, not_readable, is_directory,
 *          too_large, sensitive_denied.
 *
 * Sensitive-file deny (T6 — all comparisons are case-folded):
 *   The following name patterns are denied unless confirm_sensitive=true:
 *   wp-config.php, wp-config-*.php, wp-config.php.* (backup variants),
 *   .env*, *.pem, *.key, *.crt, *.p12, *.pfx, *.ppk,
 *   id_rsa*, id_dsa*, id_ecdsa*, id_ed25519*,
 *   .htpasswd, auth.json, .npmrc, .git-credentials,
 *   .aws/credentials, any file inside a .git/ directory.
 *
 * @package WPMgr\Agent\Commands
 */
final class FileReadCommand implements CommandInterface
{
    /** Hard cap on inline reads. */
    private const MAX_BYTES = 262144;

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'file_read';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims.
     * @param array<string,mixed> $params Decoded JSON body from the CP.
     * @return array<string,mixed>
     */
    public function execute(array $claims, array $params): array
    {
        // ------------------------------------------------------------------
        // 1. Extract and validate parameters.
        // ------------------------------------------------------------------
        if (!array_key_exists('path', $params) || !is_string($params['path']) || $params['path'] === '') {
            return $this->error('invalid_path', 'path is required');
        }

        $relPath = str_replace('\\', '/', (string) $params['path']);

        $maxBytes = self::MAX_BYTES;
        if (isset($params['max_bytes']) && is_int($params['max_bytes'])) {
            $maxBytes = min(max(0, $params['max_bytes']), self::MAX_BYTES);
        }

        $confirmSensitive = isset($params['confirm_sensitive']) && (bool) $params['confirm_sensitive'];

        // ------------------------------------------------------------------
        // 2. Resolve jail root.
        // ------------------------------------------------------------------
        $jailRoot = FileListCommand::resolveJailRoot();
        if ($jailRoot === '') {
            return $this->error('not_readable', 'file jail root could not be resolved');
        }

        // ------------------------------------------------------------------
        // 3. Jail the path (segment checks + realpath containment).
        //    Reuses the guard from FileListCommand (single path-validation routine).
        // ------------------------------------------------------------------
        $jailResult = FileListCommand::jailPath($jailRoot, $relPath);
        if (!$jailResult['ok']) {
            return $this->error($jailResult['code'], $jailResult['message']);
        }
        $absPath     = $jailResult['abs'];
        $resolvedRel = $jailResult['rel'];

        // File must exist.
        $lstat = @lstat($absPath);
        if ($lstat === false) {
            return $this->error('not_found', 'path not found: ' . $resolvedRel);
        }

        // Refuse symlinks (jail guard: realpath already caught symlink escapes,
        // but we also refuse symlinks as files — the caller should list the dir
        // to see it as is_link=true).
        if (is_link($absPath)) {
            return $this->error('not_readable', 'symlinks are not readable via file_read');
        }

        // Refuse directories.
        if (is_dir($absPath)) {
            return $this->error('is_directory', 'path is a directory, use file_list instead');
        }

        // ------------------------------------------------------------------
        // 4. Sensitive-file deny-list (T6).
        // ------------------------------------------------------------------
        $basename  = basename($resolvedRel);
        $sensitive = $this->isSensitive($resolvedRel, $basename);
        if ($sensitive && !$confirmSensitive) {
            return $this->error(
                'sensitive_denied',
                'file matches the sensitive-file deny-list; set confirm_sensitive=true to override'
            );
        }

        // ------------------------------------------------------------------
        // 5. Delegate to FileScanner::getFileContent() — the single read
        //    primitive with containment + size guards.  We pass the already-
        //    resolved absolute path as the relPath because FileScanner strips
        //    the root itself.  Actually, FileScanner takes absRoot + relPath
        //    so we reconstruct: absRoot=jailRoot, relPath=resolvedRel.
        // ------------------------------------------------------------------
        $fullSize = (int) ($lstat['size'] ?? 0);
        $mtime    = (int) ($lstat['mtime'] ?? 0);
        $mode     = (int) ($lstat['mode']  ?? 0);

        // Readable check.
        if (!is_readable($absPath)) {
            return $this->error('not_readable', 'file is not readable: ' . $resolvedRel);
        }

        // ------------------------------------------------------------------
        // 6. Read up to maxBytes.  We use FileScanner::getFileContent() to
        //    get the second containment pass (realpath + strncmp) even though
        //    we already did it above — defence-in-depth.
        // ------------------------------------------------------------------
        try {
            $scanner = new FileScanner();
            // resolvedRel is '' for the jail root; guard that case.
            $scanResult = $scanner->getFileContent($jailRoot, $resolvedRel, $maxBytes);
        } catch (\Throwable $e) {
            return $this->error('not_readable', 'internal read error');
        }

        if (!$scanResult['ok']) {
            $errCode = match ($scanResult['error']) {
                'OUTSIDE_ROOT' => 'outside_root',
                'IS_DIR'       => 'is_directory',
                'IS_LINK'      => 'not_readable',
                'too_large'    => 'too_large',
                default        => 'not_readable',
            };
            return $this->error($errCode, 'read failed: ' . ($scanResult['error'] ?? 'unknown'));
        }

        // Determine whether the returned bytes were truncated (file is larger
        // than maxBytes).
        $truncated = $fullSize > $maxBytes;

        return [
            'path'           => $resolvedRel,
            'size'           => $fullSize,
            'mtime'          => $mtime,
            'mode'           => sprintf('%04o', $mode & 07777),
            'encoding'       => 'base64',
            'content_base64' => (string) ($scanResult['content_base64'] ?? ''),
            'truncated'      => $truncated,
        ];
    }

    // ------------------------------------------------------------------
    // Sensitive-file patterns (T6)
    // ------------------------------------------------------------------

    /**
     * Return true if the site-relative path matches the sensitive-file deny-list.
     *
     * All comparisons are performed on the lowercased basename / segment so that
     * case-insensitive filesystems (macOS HFS+, Windows NTFS) cannot be used to
     * bypass the deny-list with names like WP-CONFIG.PHP or .ENV.
     *
     * Patterns (case-folded):
     *   - basename === 'wp-config.php'
     *   - basename starts with 'wp-config-' and ends with '.php'
     *   - basename starts with 'wp-config.php' and is not exactly 'wp-config.php'
     *     (catches backup suffixes: .bak, .save, .orig, .old, .swp, .swo, ~, etc.)
     *   - basename starts with '.env'
     *   - basename ends with '.pem', '.key', '.crt', '.p12', '.pfx', '.ppk'
     *   - basename starts with 'id_rsa', 'id_dsa', 'id_ecdsa', 'id_ed25519'
     *   - basename === '.htpasswd'
     *   - basename === 'auth.json'
     *   - basename === '.npmrc'
     *   - basename === '.git-credentials'
     *   - path contains a '.aws' segment immediately followed by 'credentials'
     *   - any path segment === '.git' (file inside a .git directory)
     *
     * @param string $resolvedRel Site-relative path (forward slashes, no leading slash).
     * @param string $basename    The basename component of the path.
     * @return bool
     */
    public static function isSensitive(string $resolvedRel, string $basename): bool
    {
        // Case-fold both inputs so uppercase variants cannot bypass the deny-list.
        $lbase     = strtolower($basename);
        $segments  = explode('/', $resolvedRel);
        $lsegments = array_map('strtolower', $segments);

        // Any file under a .git/ directory, or a .aws/credentials path.
        $prevSeg = '';
        foreach ($lsegments as $seg) {
            if ($seg === '.git') {
                return true;
            }
            // .aws/credentials: segment pair check.
            if ($prevSeg === '.aws' && $seg === 'credentials') {
                return true;
            }
            $prevSeg = $seg;
        }

        // wp-config.php (exact).
        if ($lbase === 'wp-config.php') {
            return true;
        }

        // wp-config-*.php (e.g. wp-config-local.php, wp-config-staging.php).
        if (str_starts_with($lbase, 'wp-config-') && str_ends_with($lbase, '.php')) {
            return true;
        }

        // wp-config.php backup variants: starts with 'wp-config.php' but is not
        // exactly 'wp-config.php' — covers .bak, .save, .orig, .old, .swp, .swo, ~ etc.
        if (str_starts_with($lbase, 'wp-config.php') && $lbase !== 'wp-config.php') {
            return true;
        }

        // .env, .env.local, .env.production, etc.
        if (str_starts_with($lbase, '.env')) {
            return true;
        }

        // Certificate / key file extensions.
        foreach (['.pem', '.key', '.crt', '.p12', '.pfx', '.ppk'] as $ext) {
            if (str_ends_with($lbase, $ext)) {
                return true;
            }
        }

        // SSH key prefixes: id_rsa*, id_dsa*, id_ecdsa*, id_ed25519*.
        foreach (['id_rsa', 'id_dsa', 'id_ecdsa', 'id_ed25519'] as $prefix) {
            if (str_starts_with($lbase, $prefix)) {
                return true;
            }
        }

        // Exact-match sensitive files.
        if (in_array($lbase, ['.htpasswd', 'auth.json', '.npmrc', '.git-credentials'], true)) {
            return true;
        }

        return false;
    }

    // ------------------------------------------------------------------
    // Response helpers
    // ------------------------------------------------------------------

    /**
     * @param string $code    Structured error code.
     * @param string $message Human-readable message (no absolute host paths).
     * @return array{error:array{code:string,message:string}}
     */
    private function error(string $code, string $message): array
    {
        return ['error' => ['code' => $code, 'message' => $message]];
    }
}
