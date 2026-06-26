<?php
// File: class-file-download-prepare-command.php — FileDownloadPrepareCommand.

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * FileDownloadPrepareCommand: stage a file to S3 for large-file download.
 *
 * Wire contract (CP→agent):
 *   POST /wp-json/wpmgr/v1/command/file_download_prepare
 *   Authorization: Bearer <Ed25519 JWT cmd="file_download_prepare">
 *   Body: {
 *     "path":           <site-relative path, forward slashes>,
 *     "presigned_puts": [
 *       { "index": <int>, "url": <string> }, ...
 *     ],
 *     "part_size":         <int — bytes per chunk>,
 *     "confirm_sensitive": <bool — default false>
 *   }
 *
 * Response (200 OK):
 *   {
 *     "object_key":  <string — the first presigned PUT URL's key, for CP bookkeeping>,
 *     "size":        <int — total file size>,
 *     "chunk_count": <int>,
 *     "parts":       [
 *       { "index": <int>, "etag": <string>, "size": <int> }, ...
 *     ]
 *   }
 *
 * Errors:
 *   { "error": { "code": <string>, "message": <string> } }
 *   Codes: invalid_path, outside_root, not_found, not_readable, is_directory,
 *          too_large, sensitive_denied.
 *
 * The CP mints presigned PUT URLs and passes them in the body (exactly like the
 * backup chunk-upload flow). The agent streams the file in part_size chunks
 * via fopen/fread, PUTs each chunk to the corresponding presigned URL via
 * wp_remote_request, and returns the part manifest.
 *
 * The same containment guard (FileListCommand::jailPath) and sensitive-file
 * deny-list (FileReadCommand::isSensitive) used by file_list and file_read
 * are applied here. This is NOT a second path-validation routine — it is the
 * same shared static method.
 *
 * Streaming rationale: fopen/fread are used instead of file_get_contents to
 * avoid loading a whole large file into PHP memory (www-data runs under a
 * constrained memory_limit). WP_Filesystem has no streaming API, and the
 * headless agent never initialises WP_Filesystem (it would prompt for FTP creds
 * and hard-fail). This is a justified deviation from the WP_Filesystem sniff.
 *
 * @package WPMgr\Agent\Commands
 */
final class FileDownloadPrepareCommand implements CommandInterface
{
    /** Minimum allowed part size (1 MiB). */
    private const MIN_PART_SIZE = 1048576;

    /** Maximum allowed part size (100 MiB). */
    private const MAX_PART_SIZE = 104857600;

    /** PUT timeout per chunk, in seconds. */
    private const CHUNK_TIMEOUT = 120;

    /**
     * {@inheritDoc}
     */
    public function name(): string
    {
        return 'file_download_prepare';
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
            return $this->error('invalid_path', 'path is required');
        }
        $relPath = str_replace('\\', '/', (string) $params['path']);

        if (!isset($params['presigned_puts']) || !is_array($params['presigned_puts'])) {
            return $this->error('invalid_path', 'presigned_puts is required');
        }
        $presignedPuts = $params['presigned_puts'];

        $partSize = self::MIN_PART_SIZE;
        if (isset($params['part_size']) && is_int($params['part_size'])) {
            $partSize = max(self::MIN_PART_SIZE, min(self::MAX_PART_SIZE, $params['part_size']));
        }

        $confirmSensitive = isset($params['confirm_sensitive']) && (bool) $params['confirm_sensitive'];

        // ------------------------------------------------------------------
        // 2. Validate the presigned_puts array structure.
        //    Each element must have integer 'index' and non-empty string 'url'.
        // ------------------------------------------------------------------
        /** @var array<int,string> $urlByIndex Map of chunk index => presigned PUT URL. */
        $urlByIndex = [];
        foreach ($presignedPuts as $put) {
            if (!is_array($put)) {
                continue;
            }
            if (!isset($put['index']) || !is_int($put['index'])) {
                continue;
            }
            if (!isset($put['url']) || !is_string($put['url']) || $put['url'] === '') {
                continue;
            }
            $urlByIndex[(int) $put['index']] = $put['url'];
        }
        if ($urlByIndex === []) {
            return $this->error('invalid_path', 'presigned_puts contains no valid entries');
        }

        // ------------------------------------------------------------------
        // 3. Resolve jail root.
        // ------------------------------------------------------------------
        $jailRoot = FileListCommand::resolveJailRoot();
        if ($jailRoot === '') {
            return $this->error('not_readable', 'file jail root could not be resolved');
        }

        // ------------------------------------------------------------------
        // 4. Jail the path — reuses the single validation routine.
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

        // Refuse symlinks.
        if (is_link($absPath)) {
            return $this->error('not_readable', 'symlinks cannot be downloaded via file_download_prepare');
        }

        // Refuse directories.
        if (is_dir($absPath)) {
            return $this->error('is_directory', 'path is a directory; only files can be downloaded');
        }

        // Readable?
        if (!is_readable($absPath)) {
            return $this->error('not_readable', 'file is not readable: ' . $resolvedRel);
        }

        // ------------------------------------------------------------------
        // 5. Sensitive-file deny-list (T6) — same rules as file_read.
        // ------------------------------------------------------------------
        $basename  = basename($resolvedRel);
        $sensitive = FileReadCommand::isSensitive($resolvedRel, $basename);
        if ($sensitive && !$confirmSensitive) {
            return $this->error(
                'sensitive_denied',
                'file matches the sensitive-file deny-list; set confirm_sensitive=true to override'
            );
        }

        // ------------------------------------------------------------------
        // 6. Compute chunk plan from file size.
        // ------------------------------------------------------------------
        $fileSize = (int) ($lstat['size'] ?? 0);
        if ($fileSize === 0) {
            // Zero-byte file: one chunk of zero bytes.
            $chunkCount = 1;
        } else {
            $chunkCount = (int) ceil($fileSize / $partSize);
        }

        // Verify we have enough presigned PUT URLs.
        if (count($urlByIndex) < $chunkCount) {
            return $this->error(
                'not_readable',
                'not enough presigned_puts: need ' . $chunkCount . ', got ' . count($urlByIndex)
            );
        }

        // ------------------------------------------------------------------
        // 7. Open the file for streaming and upload each chunk.
        //
        //    We use fopen/fread rather than file_get_contents to avoid loading
        //    the whole file into PHP memory.
        //    Justified deviation from WP_Filesystem: the headless agent never
        //    initializes WP_Filesystem (it would prompt for FTP creds), and
        //    WP_Filesystem has no streaming API for multi-GB files.
        // ------------------------------------------------------------------

        // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming large file; WP_Filesystem has no streaming API and the headless agent never initializes it (would prompt for FTP creds and hard-fail)
        $fh = @fopen($absPath, 'rb');
        if ($fh === false) {
            return $this->error('not_readable', 'could not open file for reading: ' . $resolvedRel);
        }

        /** @var list<array{index:int,etag:string,size:int}> $parts */
        $parts = [];

        try {
            for ($i = 0; $i < $chunkCount; $i++) {
                // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fread -- streaming chunk read; see fopen justification above
                $chunk = @fread($fh, $partSize);
                if ($chunk === false) {
                    return $this->error('not_readable', 'read error at chunk ' . $i);
                }

                $chunkLen = strlen($chunk);

                if (!isset($urlByIndex[$i])) {
                    return $this->error('not_readable', 'missing presigned URL for chunk index ' . $i);
                }
                $presignedUrl = $urlByIndex[$i];

                // PUT the chunk to S3 via the presigned URL.
                $putResult = $this->putChunk($presignedUrl, $chunk);
                if (!$putResult['ok']) {
                    return $this->error(
                        'not_readable',
                        'upload failed for chunk ' . $i . ': HTTP ' . $putResult['status']
                    );
                }

                $parts[] = [
                    'index' => $i,
                    'etag'  => $putResult['etag'],
                    'size'  => $chunkLen,
                ];
            }
        } finally {
            // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- streaming large file; see fopen justification above
            @fclose($fh);
        }

        // ------------------------------------------------------------------
        // 8. Derive the object_key from the first presigned PUT URL's path.
        //    This gives the CP a stable key for bookkeeping (it already knows
        //    the full URL; we expose only the key component to avoid logging
        //    bearer credentials in the response body that may be audited).
        // ------------------------------------------------------------------
        $objectKey = $this->extractObjectKey($urlByIndex[0] ?? '');

        return [
            'object_key'  => $objectKey,
            'size'        => $fileSize,
            'chunk_count' => $chunkCount,
            'parts'       => $parts,
        ];
    }

    // ------------------------------------------------------------------
    // Chunk upload
    // ------------------------------------------------------------------

    /**
     * PUT a chunk to a presigned S3 URL via wp_remote_request.
     *
     * @param string $presignedUrl Presigned PUT URL (bearer credential — not logged).
     * @param string $bytes        Chunk bytes.
     * @return array{ok:bool,status:int,etag:string}
     */
    private function putChunk(string $presignedUrl, string $bytes): array
    {
        $response = wp_remote_request(
            $presignedUrl,
            [
                'method'  => 'PUT',
                'timeout' => self::CHUNK_TIMEOUT,
                'headers' => ['Content-Type' => 'application/octet-stream'],
                'body'    => $bytes,
            ]
        );

        if (is_wp_error($response)) {
            return ['ok' => false, 'status' => 0, 'etag' => ''];
        }

        $status = (int) wp_remote_retrieve_response_code($response);
        if ($status < 200 || $status >= 300) {
            return ['ok' => false, 'status' => $status, 'etag' => ''];
        }

        // S3 returns the chunk etag in the ETag response header.
        $etag = wp_remote_retrieve_header($response, 'etag');
        $etag = is_string($etag) ? trim($etag, '"') : '';

        return ['ok' => true, 'status' => $status, 'etag' => $etag];
    }

    // ------------------------------------------------------------------
    // Helpers
    // ------------------------------------------------------------------

    /**
     * Extract the path/key component from a presigned URL for CP bookkeeping.
     * We return only the path (no query string / bearer params).
     *
     * @param string $url Presigned PUT URL.
     * @return string Path component, or empty string on failure.
     */
    private function extractObjectKey(string $url): string
    {
        if ($url === '') {
            return '';
        }
        $parts = wp_parse_url($url);
        if (!is_array($parts) || !isset($parts['path']) || !is_string($parts['path'])) {
            return '';
        }
        // Strip leading slash; the key is the path without the leading '/'.
        return ltrim($parts['path'], '/');
    }

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
