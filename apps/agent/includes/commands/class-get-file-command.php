<?php
/**
 * GetFileCommand (S3): on-demand single-file fetch for scan findings inspection.
 *
 * Wire contract (CP→agent):
 *   POST /wp-json/wpmgr/v1/command/get_file
 *   Authorization: Bearer <Ed25519 JWT cmd="get_file">
 *   Body: {
 *     "path":      <site-relative path, forward slashes>,
 *     "max_bytes": 262144
 *   }
 *
 * Response (200 OK):
 *   {
 *     "ok":             true|false,
 *     "path":           <string>,
 *     "size":           <int>,
 *     "is_dir":         <bool>,
 *     "is_link":        <bool>,
 *     "content_base64": <string>|null,
 *     "error":          <string>|null
 *   }
 *
 * Guards:
 *   - Refuses directories  → is_dir:true, content_base64:null, error:"IS_DIR"
 *   - Refuses symlinks     → is_link:true, content_base64:null, error:"IS_LINK"
 *   - Refuses over max_bytes → error:"too_large"
 *   - Refuses paths outside ABSPATH → error:"OUTSIDE_ROOT"
 *   - Refuses unreadable paths → error:"NOT_READABLE"
 *
 * Auth: the Router's permission_callback already enforces Ed25519 + anti-replay
 * verification (Connector::verifyCommand) before execute() is ever called.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\FileScanner;

/**
 * Returns the base64-encoded content of a single file within ABSPATH.
 * Refuses directories, symlinks, files exceeding max_bytes, and path traversal.
 */
final class GetFileCommand implements CommandInterface
{
    /** Default cap matching the wire contract (256 KiB). */
    private const DEFAULT_MAX_BYTES = 262144;

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'get_file';
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
        // 1. Validate and extract parameters.
        // ------------------------------------------------------------------

        if (!array_key_exists('path', $params) || !is_string($params['path']) || $params['path'] === '') {
            return [
                'ok'             => false,
                'path'           => '',
                'size'           => 0,
                'is_dir'         => false,
                'is_link'        => false,
                'content_base64' => null,
                'error'          => 'missing required field: path',
            ];
        }

        $relPath  = str_replace('\\', '/', (string) $params['path']);
        $maxBytes = array_key_exists('max_bytes', $params) && is_int($params['max_bytes'])
            ? $params['max_bytes']
            : self::DEFAULT_MAX_BYTES;

        if ($maxBytes < 0) {
            $maxBytes = 0;
        }

        // ------------------------------------------------------------------
        // 2. Resolve ABSPATH.
        // ------------------------------------------------------------------

        $absPath = defined('ABSPATH') ? rtrim((string) constant('ABSPATH'), '/\\') : '';
        if ($absPath === '') {
            return [
                'ok'             => false,
                'path'           => $relPath,
                'size'           => 0,
                'is_dir'         => false,
                'is_link'        => false,
                'content_base64' => null,
                'error'          => 'ABSPATH not defined',
            ];
        }

        // ------------------------------------------------------------------
        // 3. Delegate to FileScanner (containment + dir/symlink/size guards).
        // ------------------------------------------------------------------

        try {
            $scanner = new FileScanner();
            return $scanner->getFileContent($absPath, $relPath, $maxBytes);
        } catch (\Throwable $e) {
            return [
                'ok'             => false,
                'path'           => $relPath,
                'size'           => 0,
                'is_dir'         => false,
                'is_link'        => false,
                'content_base64' => null,
                'error'          => 'internal_error',
            ];
        }
    }
}
