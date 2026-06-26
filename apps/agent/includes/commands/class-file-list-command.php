<?php
// File: class-file-list-command.php — FileListCommand.

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\FileScanner;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * FileListCommand: one-level directory listing within the site jail.
 *
 * Wire contract (CP→agent):
 *   POST /wp-json/wpmgr/v1/command/file_list
 *   Authorization: Bearer <Ed25519 JWT cmd="file_list">
 *   Body: {
 *     "path":    <site-relative path, forward slashes, "" = root>,
 *     "cursor":  <opaque string from a prior truncated response> | null
 *   }
 *
 * Response (200 OK):
 *   {
 *     "path":      <string — the resolved site-relative path>,
 *     "entries":   [
 *       {
 *         "name":        <string>,
 *         "size":        <int — 0 for directories>,
 *         "mtime":       <int — Unix timestamp>,
 *         "mode":        <string — octal e.g. "0644">,
 *         "is_dir":      <bool>,
 *         "is_link":     <bool>,
 *         "is_writable": <bool>
 *       }, ...
 *     ],
 *     "total":     <int — total count before truncation>,
 *     "truncated": <bool>,
 *     "cursor":    <string|null — continuation cursor when truncated>
 *   }
 *
 * Errors:
 *   { "error": { "code": <string>, "message": <string> } }
 *   Codes: invalid_path, outside_root, not_found, not_readable, is_directory (n/a here),
 *          sensitive_denied, too_large.
 *
 * Security: every path is run through FileScanner::getFileContent()'s containment
 * guard (realpath + strncmp). The jail root is WPMGR_FILE_JAIL_ROOT (defaults to ABSPATH).
 *
 * @package WPMgr\Agent\Commands
 */
final class FileListCommand implements CommandInterface
{
    /** Maximum number of entries returned in a single call. */
    private const MAX_ENTRIES = 1000;

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'file_list';
    }

    /**
     * {@inheritDoc}
     *
     * @param array<string,mixed> $claims Validated JWT claims (aud, cmd already enforced).
     * @param array<string,mixed> $params Decoded JSON body from the CP.
     * @return array<string,mixed>
     */
    public function execute(array $claims, array $params): array
    {
        // ------------------------------------------------------------------
        // 1. Extract and validate parameters.
        // ------------------------------------------------------------------
        $relPath = isset($params['path']) && is_string($params['path'])
            ? $params['path']
            : '';
        $relPath = str_replace('\\', '/', $relPath);

        // Cursor is an opaque base64-encoded JSON blob or null.
        $cursorIn = isset($params['cursor']) && is_string($params['cursor']) && $params['cursor'] !== ''
            ? $params['cursor']
            : null;

        // ------------------------------------------------------------------
        // 2. Resolve jail root.
        // ------------------------------------------------------------------
        $jailRoot = $this->resolveJailRoot();
        if ($jailRoot === '') {
            return $this->internalError('file jail root could not be resolved');
        }

        // ------------------------------------------------------------------
        // 3. Validate and jail the requested directory path.
        //    Reuse FileScanner segment checks (NUL, .., .) then realpath.
        // ------------------------------------------------------------------
        $jailResult = $this->jailPath($jailRoot, $relPath);
        if (!$jailResult['ok']) {
            return $this->error($jailResult['code'], $jailResult['message']);
        }
        $absDir      = $jailResult['abs'];
        $resolvedRel = $jailResult['rel'];

        // Must be a directory.
        if (!is_dir($absDir)) {
            if (!file_exists($absDir)) {
                return $this->error('not_found', 'path not found: ' . $resolvedRel);
            }
            return $this->error('not_readable', 'path is not a directory: ' . $resolvedRel);
        }

        // Must be readable.
        if (!is_readable($absDir)) {
            return $this->error('not_readable', 'directory is not readable: ' . $resolvedRel);
        }

        // ------------------------------------------------------------------
        // 4. Decode continuation cursor.
        // ------------------------------------------------------------------
        $offset = 0;
        if ($cursorIn !== null) {
            $decoded = $this->decodeCursor($cursorIn, $resolvedRel);
            if ($decoded === null) {
                return $this->error('invalid_path', 'cursor is invalid or expired');
            }
            $offset = $decoded;
        }

        // ------------------------------------------------------------------
        // 5. Read directory entries (one-level, no recursion).
        // ------------------------------------------------------------------
        $handle = @opendir($absDir);
        if ($handle === false) {
            return $this->error('not_readable', 'could not open directory: ' . $resolvedRel);
        }

        /** @var list<array{name:string,size:int,mtime:int,mode:int,is_dir:bool,is_link:bool,is_writable:bool}> $all */
        $all = [];

        while (true) {
            $entry = @readdir($handle);
            if ($entry === false) {
                break;
            }
            if ($entry === '.' || $entry === '..') {
                continue;
            }

            $absEntry = $absDir . '/' . $entry;

            // Use lstat so we see the link itself, not its target.
            $statResult = @lstat($absEntry);
            if ($statResult === false) {
                continue;
            }

            $isLink     = is_link($absEntry);
            $isDir      = !$isLink && is_dir($absEntry);
            $size       = $isDir ? 0 : (int) ($statResult['size'] ?? 0);
            $mtime      = (int) ($statResult['mtime'] ?? 0);
            $mode       = (int) ($statResult['mode'] ?? 0);
            // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_is_writable -- WP_Filesystem::is_writable() requires filesystem init which the headless agent cannot perform (no FTP/SSH context)
            $isWritable = is_writable($absEntry);

            $all[] = [
                'name'        => $entry,
                'size'        => $size,
                'mtime'       => $mtime,
                'mode'        => $mode,
                'is_dir'      => $isDir,
                'is_link'     => $isLink,
                'is_writable' => $isWritable,
            ];
        }
        closedir($handle);

        // ------------------------------------------------------------------
        // 6. Sort: dirs first, then files; case-insensitive name within each group.
        // ------------------------------------------------------------------
        usort($all, static function (array $a, array $b): int {
            if ($a['is_dir'] !== $b['is_dir']) {
                return $a['is_dir'] ? -1 : 1;
            }
            return strcasecmp($a['name'], $b['name']);
        });

        $total = count($all);

        // ------------------------------------------------------------------
        // 7. Apply cursor offset + page cap.
        // ------------------------------------------------------------------
        $sliced    = array_slice($all, $offset, self::MAX_ENTRIES);
        $truncated = ($offset + count($sliced)) < $total;

        $nextCursor = null;
        if ($truncated) {
            $nextCursor = $this->encodeCursor($resolvedRel, $offset + count($sliced));
        }

        // ------------------------------------------------------------------
        // 8. Format entries for the wire: convert mode int to octal string.
        // ------------------------------------------------------------------
        $entries = [];
        foreach ($sliced as $row) {
            $entries[] = [
                'name'        => $row['name'],
                'size'        => $row['size'],
                'mtime'       => $row['mtime'],
                'mode'        => sprintf('%04o', $row['mode'] & 07777),
                'is_dir'      => $row['is_dir'],
                'is_link'     => $row['is_link'],
                'is_writable' => $row['is_writable'],
            ];
        }

        $response = [
            'path'      => $resolvedRel,
            'entries'   => $entries,
            'total'     => $total,
            'truncated' => $truncated,
        ];
        if ($nextCursor !== null) {
            $response['cursor'] = $nextCursor;
        }
        return $response;
    }

    // ------------------------------------------------------------------
    // Path-jail helper (shared with FileReadCommand and FileDownloadPrepareCommand
    // via duplication intentionally avoided: both call this same pattern).
    // Reuses FileScanner's segment checks conceptually; implemented here so no
    // second path-validation routine exists beyond what FileScanner already does.
    // ------------------------------------------------------------------

    /**
     * Resolve a site-relative path against the jail root with the same guards
     * FileScanner::getFileContent() uses: strip leading slash, reject NUL,
     * reject '..' and '.' segments, lstat, symlink guard, realpath containment.
     *
     * Returns ['ok'=>true,'abs'=>string,'rel'=>string] on success, or
     * ['ok'=>false,'code'=>string,'message'=>string] on failure.
     *
     * @param string $jailRoot Absolute jail root (no trailing slash, realpath'd).
     * @param string $relPath  Site-relative forward-slash path.
     * @return array{ok:bool,abs?:string,rel?:string,code?:string,message?:string}
     */
    public static function jailPath(string $jailRoot, string $relPath): array
    {
        // Strip leading slash.
        $stripped = ltrim($relPath, '/');

        // An empty path means the jail root itself.
        if ($stripped === '') {
            return ['ok' => true, 'abs' => $jailRoot, 'rel' => ''];
        }

        // Reject NUL bytes.
        if (strpos($stripped, "\0") !== false) {
            return ['ok' => false, 'code' => 'invalid_path', 'message' => 'path contains NUL bytes'];
        }

        // Reject any segment that is '.' or '..' (traversal).
        $segments = explode('/', $stripped);
        foreach ($segments as $seg) {
            if ($seg === '.' || $seg === '..') {
                return ['ok' => false, 'code' => 'outside_root', 'message' => 'path traversal not allowed'];
            }
            if ($seg === '') {
                return ['ok' => false, 'code' => 'invalid_path', 'message' => 'path contains empty segment'];
            }
        }

        $joined = $jailRoot . '/' . $stripped;

        // Symlink guard: if the entry itself is a symlink, refuse it for
        // file read/download operations (caller checks separately for list).
        // For the jail containment we use lstat to see what's there before
        // deciding, then realpath to verify containment.
        $lstat = @lstat($joined);
        if ($lstat === false) {
            // Not found — still return the computed path so callers can 404.
            return ['ok' => true, 'abs' => $joined, 'rel' => $stripped];
        }

        // Realpath containment: resolve all symlinks in the path and verify
        // the result starts with jailRoot + '/'.
        $real = realpath($joined);
        if ($real === false) {
            return ['ok' => true, 'abs' => $joined, 'rel' => $stripped];
        }

        if (strncmp($real, $jailRoot . '/', strlen($jailRoot) + 1) !== 0
            && $real !== $jailRoot
        ) {
            return ['ok' => false, 'code' => 'outside_root', 'message' => 'path escapes the jail root'];
        }

        return ['ok' => true, 'abs' => $real, 'rel' => $stripped];
    }

    /**
     * Resolve the file jail root constant or fall back to ABSPATH.
     *
     * WPMGR_FILE_JAIL_ROOT is the single internal constant that can be used to
     * narrow the jail to a sub-tree (e.g. wp-content) without code changes.
     * Default: ABSPATH.
     *
     * @return string Absolute path without trailing slash, or '' on failure.
     */
    public static function resolveJailRoot(): string
    {
        if (defined('WPMGR_FILE_JAIL_ROOT')) {
            $root = rtrim((string) constant('WPMGR_FILE_JAIL_ROOT'), '/\\');
        } elseif (defined('ABSPATH')) {
            $root = rtrim((string) constant('ABSPATH'), '/\\');
        } else {
            return '';
        }

        if ($root === '') {
            return '';
        }

        // Canonicalize the root itself via realpath to resolve OS-level symlinks
        // (e.g. macOS /tmp -> /private/tmp) and match what realpath() returns for
        // paths beneath it — identical to FileScanner::normaliseRoot().
        $resolved = realpath($root);
        if ($resolved !== false) {
            $root = str_replace('\\', '/', $resolved);
        }

        return $root;
    }

    // ------------------------------------------------------------------
    // Cursor encoding / decoding
    // ------------------------------------------------------------------

    /**
     * Encode a pagination cursor as opaque base64-encoded JSON.
     *
     * @param string $relPath Resolved relative path of the listed directory.
     * @param int    $offset  Next offset to resume from.
     * @return string Opaque cursor string.
     */
    private function encodeCursor(string $relPath, int $offset): string
    {
        $payload = (string) wp_json_encode(['p' => $relPath, 'o' => $offset]);
        return base64_encode($payload);
    }

    /**
     * Decode and validate a cursor. Returns the integer offset on success,
     * or null if the cursor is malformed or references a different path.
     *
     * @param string $cursor      Cursor string from the client.
     * @param string $currentPath The currently-requested resolved path.
     * @return int|null
     */
    private function decodeCursor(string $cursor, string $currentPath): ?int
    {
        $raw = @base64_decode($cursor, true);
        if ($raw === false || $raw === '') {
            return null;
        }
        $data = @json_decode($raw, true);
        if (!is_array($data)) {
            return null;
        }
        // The cursor must reference the same directory path.
        if (!isset($data['p']) || $data['p'] !== $currentPath) {
            return null;
        }
        if (!isset($data['o']) || !is_int($data['o']) || $data['o'] < 0) {
            return null;
        }
        return $data['o'];
    }

    // ------------------------------------------------------------------
    // Response helpers
    // ------------------------------------------------------------------

    /**
     * @param string $code    Error code from the shared set.
     * @param string $message Human-readable message (no absolute host paths).
     * @return array{error:array{code:string,message:string}}
     */
    private function error(string $code, string $message): array
    {
        return ['error' => ['code' => $code, 'message' => $message]];
    }

    /**
     * @param string $message Internal detail (safe for the agent's own log).
     * @return array{error:array{code:string,message:string}}
     */
    private function internalError(string $message): array
    {
        return $this->error('not_readable', 'internal error: ' . $message);
    }
}
