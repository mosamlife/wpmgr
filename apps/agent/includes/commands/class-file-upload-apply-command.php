<?php
// File: class-file-upload-apply-command.php — FileUploadApplyCommand.

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * FileUploadApplyCommand: reassemble a CP-staged upload from presigned S3 GETs
 * and atomically place it within the site jail.
 *
 * Wire contract (CP→agent):
 *   POST /wp-json/wpmgr/v1/command/file_upload_apply
 *   Authorization: Bearer <Ed25519 JWT cmd="file_upload_apply">
 *   Body: {
 *     "path":         <site-relative destination path, forward slashes>,
 *     "presigned_gets": [
 *       { "index": <int>, "url": <string> }, ...
 *     ],
 *     "part_count":  <int — expected number of parts>,
 *     "total_size":  <int — expected total reassembled size in bytes>,
 *     "sha256":      <hex string — optional SHA-256 of the complete reassembled file>
 *   }
 *
 * Response (200 OK):
 *   { "path": <string>, "size": <int>, "mtime": <int> }
 *
 * Errors:
 *   { "error": { "code": <string>, "message": <string> } }
 *   Codes: invalid_path, outside_root, not_found, is_directory, too_large,
 *          sensitive_denied, executable_write_denied, base_unresolved, write_failed.
 *
 * Security (critical — this is the RCE vector):
 *   - T1 Executable-write prevention: SAME guard as file_write.
 *     Uploading a .php file via presigned GET is the primary CVE-2020-25213
 *     class attack. The deny-list, double-extension check, AND full-file content
 *     sniff ALL apply here. The content is streamed into a temp file first,
 *     then inspected before atomic placement. The content sniff covers '<?php',
 *     '<?=', and bare '<?' (short-open-tag, carved out for '<?xml') across the
 *     entire file in 64 KiB chunks with an 8-byte carry-over so no tag spanning
 *     a chunk boundary is missed.
 *   - T2 Path jail: FileListCommand::jailPath() on the destination path.
 *   - T3 Base-unresolved guard: jail root checked before any write.
 *   - Streaming: chunks are fetched via wp_remote_get() and streamed directly
 *     into a temp file using fopen/fwrite — NEVER buffered into memory as a
 *     whole. This respects www-data PHP memory_limit (the exact constraint that
 *     makes WP_Filesystem unusable here too).
 *   - total_size cap: same 256 KiB as file_write by default. The CP may set a
 *     higher cap for explicitly permitted binary uploads, but the agent enforces
 *     a hard cap of UPLOAD_MAX_BYTES.
 *   - sha256 verification: if the CP provides a sha256 hex digest, it is
 *     verified against the reassembled temp file before atomic rename.
 *   - Atomic: temp file is written in the SAME directory as the target, then
 *     rename()'d atomically.
 *   - Sensitive deny: isSensitive() blocks writes to credential/config files.
 *
 * Streaming rationale: fopen/fwrite/fclose are used for the temp file because
 * WP_Filesystem has no streaming API, and the headless agent never initialises
 * WP_Filesystem. The chunks are fetched via wp_remote_get() (WP's HTTP API),
 * not raw curl — no WP_Filesystem or curl_multi needed here since the parts
 * are small (capped) and serial streaming is sufficient.
 *
 * @package WPMgr\Agent\Commands
 */
final class FileUploadApplyCommand implements CommandInterface {

	/**
	 * Hard cap on reassembled upload size (bytes).
	 * The CP may raise this for explicitly permitted binary uploads, but the
	 * agent enforces this ceiling unconditionally.
	 * 512 MiB: generous enough for most operator use cases; above this, the
	 * backup/restore presigned-PUT path should be used instead.
	 */
	private const UPLOAD_MAX_BYTES = 536870912; // 512 MiB

	/**
	 * Read-buffer size for streaming chunk bodies into the temp file.
	 * 64 KiB: small enough to stay well within typical PHP memory_limit.
	 */
	private const READ_BUF = 65536;

	/**
	 * Maximum number of parts (presigned GET URLs).
	 * Prevents memory exhaustion from an absurdly large part list.
	 */
	private const MAX_PARTS = 10000;

	/**
	 * {@inheritDoc}
	 */
	public function name(): string {
		return 'file_upload_apply';
	}

	/**
	 * {@inheritDoc}
	 *
	 * @param array<string,mixed> $claims Validated JWT claims.
	 * @param array<string,mixed> $params Decoded JSON body from the CP.
	 * @return array<string,mixed>
	 */
	public function execute( array $claims, array $params ): array {
		// ------------------------------------------------------------------
		// 1. Extract and validate parameters.
		// ------------------------------------------------------------------
		if ( ! array_key_exists( 'path', $params ) || ! is_string( $params['path'] ) || $params['path'] === '' ) {
			return $this->error( 'invalid_path', 'path is required' );
		}

		$relPath = str_replace( '\\', '/', (string) $params['path'] );

		if ( ! isset( $params['presigned_gets'] ) || ! is_array( $params['presigned_gets'] ) ) {
			return $this->error( 'invalid_path', 'presigned_gets is required and must be an array' );
		}

		$presignedGets = $params['presigned_gets'];

		if ( count( $presignedGets ) === 0 ) {
			return $this->error( 'invalid_path', 'presigned_gets must contain at least one entry' );
		}

		if ( count( $presignedGets ) > self::MAX_PARTS ) {
			return $this->error( 'too_large', 'presigned_gets exceeds maximum part count' );
		}

		$partCount = isset( $params['part_count'] ) && is_int( $params['part_count'] )
			? $params['part_count']
			: count( $presignedGets );

		$totalSize = isset( $params['total_size'] ) && is_int( $params['total_size'] )
			? $params['total_size']
			: -1;

		if ( $totalSize > self::UPLOAD_MAX_BYTES ) {
			return $this->error( 'too_large', 'total_size exceeds the maximum upload size limit' );
		}

		$sha256Expected = isset( $params['sha256'] ) && is_string( $params['sha256'] ) && strlen( $params['sha256'] ) === 64
			? strtolower( $params['sha256'] )
			: null;

		$confirmExecutableWrite = ! empty( $params['confirm_executable_write'] );
		$confirmSensitive       = ! empty( $params['confirm_sensitive'] );

		// ------------------------------------------------------------------
		// 2. Validate each presigned_gets entry.
		// ------------------------------------------------------------------
		$sortedParts = [];
		foreach ( $presignedGets as $part ) {
			if ( ! is_array( $part )
				|| ! isset( $part['index'], $part['url'] )
				|| ! is_int( $part['index'] )
				|| ! is_string( $part['url'] )
				|| $part['url'] === ''
			) {
				return $this->error( 'invalid_path', 'each presigned_gets entry must have integer index and non-empty string url' );
			}
			$sortedParts[ $part['index'] ] = $part['url'];
		}
		ksort( $sortedParts );

		// ------------------------------------------------------------------
		// 3. Resolve jail root (T3 guard).
		// ------------------------------------------------------------------
		$jailRoot = FileListCommand::resolveJailRoot();
		if ( $jailRoot === '' ) {
			return $this->error( 'base_unresolved', 'file jail root could not be resolved; no write performed' );
		}

		// ------------------------------------------------------------------
		// 4. Jail the destination path.
		// ------------------------------------------------------------------
		$jailResult = FileListCommand::jailPath( $jailRoot, $relPath );
		if ( ! $jailResult['ok'] ) {
			return $this->error( $jailResult['code'], $jailResult['message'] );
		}
		$absPath     = $jailResult['abs'];
		$resolvedRel = $jailResult['rel'];

		// Refuse if the destination is a directory.
		if ( is_dir( $absPath ) ) {
			return $this->error( 'is_directory', 'destination path is a directory, not a file' );
		}

		// ------------------------------------------------------------------
		// 5. Executable-write check on the DESTINATION name (T1).
		//    Extension + double-extension check happen here, BEFORE any bytes
		//    are fetched (fast path for obvious denials).
		//    Content sniff (including bare '<?' short-tag detection) happens
		//    after full reassembly (step 11 below) — that is the definitive
		//    RCE control for extension-clean uploads.
		// ------------------------------------------------------------------
		if ( ! $confirmExecutableWrite && FileGuards::hasExecutableExtension( strtolower( basename( $resolvedRel ) ) ) ) {
			return $this->error(
				'executable_write_denied',
				'upload denied: destination file extension is in the executable deny-list; set confirm_executable_write=true to override'
			);
		}

		// ------------------------------------------------------------------
		// 6. Sensitive-file deny (T6).
		// ------------------------------------------------------------------
		$basename = basename( $resolvedRel );
		if ( FileReadCommand::isSensitive( $resolvedRel, $basename ) && ! $confirmSensitive ) {
			return $this->error(
				'sensitive_denied',
				'destination matches the sensitive-file deny-list; set confirm_sensitive=true to override'
			);
		}

		// ------------------------------------------------------------------
		// 7. Ensure the parent directory exists.
		// ------------------------------------------------------------------
		$parentDir = dirname( $absPath );
		if ( ! is_dir( $parentDir ) ) {
			return $this->error( 'not_found', 'parent directory does not exist' );
		}

		// ------------------------------------------------------------------
		// 8. Stream-reassemble all chunks into a temp file in the SAME dir.
		//    NEVER buffer the whole file in memory.
		// ------------------------------------------------------------------
		$tmpPath = $parentDir . '/.wpmgr-ul-' . bin2hex( random_bytes( 8 ) );

		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming chunk reassembly; WP_Filesystem has no streaming API; headless agent never initializes WP_Filesystem
		$fh = @fopen( $tmpPath, 'wb' );
		if ( $fh === false ) {
			return $this->error( 'write_failed', 'could not open temp file for writing' );
		}

		$totalWritten = 0;
		// phpcs:disable WordPress.WP.AlternativeFunctions.file_system_operations_fwrite,WordPress.WP.AlternativeFunctions.file_system_operations_fclose,WordPress.WP.AlternativeFunctions.unlink_unlink -- streaming chunk reassembly; WP_Filesystem has no streaming API and is never initialized in the headless agent (would prompt for FTP creds and hard-fail); fwrite/fclose/unlink are the only correct primitives here
		try {
			foreach ( $sortedParts as $idx => $url ) {
				$result = $this->fetchChunk( $url );
				if ( ! $result['ok'] ) {
					@fclose( $fh );
					@unlink( $tmpPath );
					return $this->error( 'write_failed', 'failed to fetch chunk ' . (int) $idx . ': ' . esc_html( $result['error'] ) );
				}

				$body    = $result['body'];
				$written = @fwrite( $fh, $body );
				if ( $written === false ) {
					@fclose( $fh );
					@unlink( $tmpPath );
					return $this->error( 'write_failed', 'failed to write chunk ' . (int) $idx . ' to temp file' );
				}
				$totalWritten += $written;

				if ( $totalWritten > self::UPLOAD_MAX_BYTES ) {
					@fclose( $fh );
					@unlink( $tmpPath );
					return $this->error( 'too_large', 'reassembled upload exceeds the maximum allowed size' );
				}
			}
			@fclose( $fh );
		} catch ( \Throwable $e ) {
			@fclose( $fh );
			@unlink( $tmpPath );
			return $this->error( 'write_failed', 'unexpected error during chunk reassembly' );
		}
		// phpcs:enable WordPress.WP.AlternativeFunctions.file_system_operations_fwrite,WordPress.WP.AlternativeFunctions.file_system_operations_fclose,WordPress.WP.AlternativeFunctions.unlink_unlink

		// ------------------------------------------------------------------
		// 9. Verify total_size.
		// ------------------------------------------------------------------
		if ( $totalSize >= 0 && $totalWritten !== $totalSize ) {
			// phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- headless agent; cleanup temp file on verification failure; WP_Filesystem never initialized
			@unlink( $tmpPath );
			return $this->error( 'write_failed', 'reassembled size does not match expected total_size' );
		}

		// ------------------------------------------------------------------
		// 10. Verify sha256 (if provided).
		// ------------------------------------------------------------------
		if ( $sha256Expected !== null ) {
			$sha256Actual = hash_file( 'sha256', $tmpPath );
			if ( $sha256Actual === false || ! hash_equals( $sha256Expected, strtolower( $sha256Actual ) ) ) {
				// phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- headless agent; cleanup temp file on sha256 mismatch; WP_Filesystem never initialized
				@unlink( $tmpPath );
				return $this->error( 'write_failed', 'sha256 integrity check failed; upload may be corrupted' );
			}
		}

		// ------------------------------------------------------------------
		// 11. Content sniff (T1 — THE RCE prevention on upload):
		//     Full-file scan of the reassembled temp file for PHP open tags.
		//     Reads in 64 KiB chunks with an 8-byte carry-over so a tag
		//     straddling a chunk boundary is never missed. Covers '<?php',
		//     '<?=', and bare '<?' (short-open-tag) with a '<?xml' carve-out.
		//     The file is on local disk and capped at UPLOAD_MAX_BYTES so a
		//     full scan is cheap and is the whole point of this post-reassembly
		//     gate.
		// ------------------------------------------------------------------
		if ( ! $confirmExecutableWrite ) {
			$phpDetected = $this->fileContainsPhpTag( $tmpPath );
			if ( $phpDetected ) {
				// phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- headless agent; cleanup temp file when PHP content sniff denies the upload
				@unlink( $tmpPath );
				return $this->error(
					'executable_write_denied',
					'upload denied: file content contains a PHP open tag (<?php, <?=, or bare <?); set confirm_executable_write=true to override'
				);
			}
		}

		// ------------------------------------------------------------------
		// 12. TOCTOU symlink guard + atomic rename (F3).
		//     rename() follows symlinks at the destination and would write outside
		//     the jail if one was planted between jailPath() and here.
		// ------------------------------------------------------------------
		if ( is_link( $absPath ) ) {
			// phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- headless agent; cleanup temp file when symlink at destination denies the upload
			@unlink( $tmpPath );
			return $this->error( 'outside_root', 'upload denied: destination is a symbolic link' );
		}

		// Re-verify the destination's parent directory is still jailed.
		$parentReal = realpath( $parentDir );
		if ( $parentReal === false
			|| ( strncmp( str_replace( '\\', '/', $parentReal ), $jailRoot . '/', strlen( $jailRoot ) + 1 ) !== 0
				&& str_replace( '\\', '/', $parentReal ) !== $jailRoot )
		) {
			// phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- headless agent; cleanup temp file when parent dir escapes jail
			@unlink( $tmpPath );
			return $this->error( 'outside_root', 'upload denied: destination parent is outside the jail root' );
		}

		// phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- atomic placement; WP_Filesystem::move() is non-atomic and WP_Filesystem is never initialized in the headless agent
		if ( ! @rename( $tmpPath, $absPath ) ) {
			// phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- headless agent; cleanup temp file on atomic rename failure
			@unlink( $tmpPath );
			return $this->error( 'write_failed', 'atomic rename to destination failed' );
		}

		// ------------------------------------------------------------------
		// 13. Build response.
		// ------------------------------------------------------------------
		$lstat = @lstat( $absPath );
		$size  = $lstat !== false ? (int) ( $lstat['size'] ?? $totalWritten ) : $totalWritten;
		$mtime = $lstat !== false ? (int) ( $lstat['mtime'] ?? 0 ) : 0;

		return [
			'path'  => $resolvedRel,
			'size'  => $size,
			'mtime' => $mtime,
		];
	}

	// ------------------------------------------------------------------
	// Helpers
	// ------------------------------------------------------------------

	/**
	 * Fetch a single presigned-GET chunk via WP HTTP API.
	 * Returns ['ok'=>bool, 'body'=>string, 'error'=>string].
	 *
	 * Presigned URLs are bearer credentials — we do NOT log them on failure.
	 *
	 * @param string $url Presigned S3 GET URL.
	 * @return array{ok:bool,body:string,error:string}
	 */
	private function fetchChunk( string $url ): array {
		$response = wp_remote_get(
			$url,
			[ 'timeout' => 60 ]
		);

		if ( is_wp_error( $response ) ) {
			return [ 'ok' => false, 'body' => '', 'error' => 'HTTP transport error' ];
		}

		$status = (int) wp_remote_retrieve_response_code( $response );
		if ( $status < 200 || $status >= 300 ) {
			return [ 'ok' => false, 'body' => '', 'error' => 'HTTP ' . $status ];
		}

		$body = wp_remote_retrieve_body( $response );
		if ( ! is_string( $body ) ) {
			return [ 'ok' => false, 'body' => '', 'error' => 'empty or non-string response body' ];
		}

		return [ 'ok' => true, 'body' => $body, 'error' => '' ];
	}

	/**
	 * Full-file scan of the temp file for PHP open tags.
	 *
	 * Reads the file in self::READ_BUF (64 KiB) chunks, prepending the last
	 * 8 bytes of the previous chunk to catch any tag that straddles a buffer
	 * boundary (longest sniff pattern is '<?php' = 5 bytes; 8 bytes is safe).
	 * Delegates per-chunk detection to FileGuards::sniffsAsPhp() which covers
	 * '<?php', '<?=', and bare '<?' with the '<?xml' carve-out.
	 *
	 * The file is on local disk, capped at UPLOAD_MAX_BYTES, so scanning the
	 * entire file is inexpensive and is the whole point of the post-reassembly gate.
	 *
	 * @param string $tmpPath Absolute path to the reassembled temp file.
	 * @return bool True if a PHP open tag was found anywhere in the file.
	 */
	private function fileContainsPhpTag( string $tmpPath ): bool {
		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming PHP sniff; WP_Filesystem has no streaming API; headless agent never initializes WP_Filesystem
		$fh = @fopen( $tmpPath, 'rb' );
		if ( $fh === false ) {
			// Cannot open — treat as clean (the rename will fail anyway if the file is gone).
			return false;
		}

		// phpcs:disable WordPress.WP.AlternativeFunctions.file_system_operations_fread,WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- full-file streaming PHP content sniff; WP_Filesystem has no streaming/fread API and is never initialized in the headless agent

		// Carry-over: last N bytes of the previous chunk prepended to the next
		// so a pattern spanning a boundary is not missed.
		// 8 bytes covers the longest possible split of any pattern we detect.
		$carryLen = 8;
		$carry    = '';

		while ( ! feof( $fh ) ) {
			$chunk = fread( $fh, self::READ_BUF );
			if ( ! is_string( $chunk ) || $chunk === '' ) {
				break;
			}
			$window = $carry . $chunk;
			if ( FileGuards::sniffsAsPhp( $window ) ) {
				@fclose( $fh );
				return true;
			}
			// Keep the tail of the current chunk as carry for the next iteration.
			$carry = substr( $chunk, -$carryLen );
		}

		@fclose( $fh );
		// phpcs:enable WordPress.WP.AlternativeFunctions.file_system_operations_fread,WordPress.WP.AlternativeFunctions.file_system_operations_fclose
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
	private function error( string $code, string $message ): array {
		return [ 'error' => [ 'code' => $code, 'message' => $message ] ];
	}
}
