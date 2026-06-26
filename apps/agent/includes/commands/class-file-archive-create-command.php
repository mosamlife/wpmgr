<?php
// File: class-file-archive-create-command.php — FileArchiveCreateCommand.

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\StoragePaths;
use ZipArchive;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * FileArchiveCreateCommand: zip one or more jailed paths and stage to S3.
 *
 * Wire contract (CP→agent):
 *   POST /wp-json/wpmgr/v1/command/file_archive_create
 *   Authorization: Bearer <Ed25519 JWT cmd="file_archive_create">
 *   Body: {
 *     "paths":              [<site-relative path, forward slashes>, ...],
 *     "presigned_puts":     [ { "index": <int>, "url": <string> }, ... ],
 *     "part_size":          <int — bytes per chunk>,
 *     "confirm_sensitive":  <bool — default false; CP owner-gates this>
 *   }
 *
 * Response (200 OK):
 *   {
 *     "object_key":  <string — object key of the first presigned PUT URL>,
 *     "size":        <int — total zip bytes>,
 *     "chunk_count": <int>,
 *     "parts":       [ { "index": <int>, "etag": <string>, "size": <int> }, ... ]
 *   }
 *
 * Errors:
 *   { "error": { "code": <string>, "message": <string> } }
 *   Codes: invalid_path, outside_root, not_found, not_readable, too_large,
 *          sensitive_denied, write_failed.
 *
 * SENSITIVE-FILE GATE (F1):
 *   For every file added to the archive (including files inside directories),
 *   FileReadCommand::isSensitive() is called on the site-relative path. If ANY
 *   source file is sensitive and confirm_sensitive is false, the entire archive
 *   operation is aborted with sensitive_denied — no archive is written. When
 *   confirm_sensitive=true the CP has already obtained owner-level confirmation.
 *   This mirrors the exact deny-list semantics used by the read/download path.
 *
 * Every path is run through FileListCommand::jailPath(). The zip is written to a
 * temp file under the staging area (never under a web-served dir), then uploaded
 * to S3 via the presigned-PUT chain (one curl PUT per chunk), and cleaned up.
 *
 * Streaming: ZipArchive::addFile() is used so PHP never loads file bodies into
 * memory; the C extension streams source bytes through deflate. fopen/fread are
 * used to read the finished zip in part_size chunks for upload — the headless
 * agent never initialises WP_Filesystem (it would prompt for FTP creds and
 * hard-fail), so direct streaming I/O is the correct posture here.
 *
 * @package WPMgr\Agent\Commands
 */
final class FileArchiveCreateCommand implements CommandInterface {

	/** Maximum total uncompressed bytes across all source paths (512 MiB). */
	private const MAX_TOTAL_BYTES = 536870912;

	/** Default chunk size when part_size is not specified (20 MiB). */
	private const DEFAULT_PART_SIZE = 20971520;

	/** Minimum allowable part_size (1 MiB). */
	private const MIN_PART_SIZE = 1048576;

	/** Maximum allowable part_size (500 MiB). */
	private const MAX_PART_SIZE = 524288000;

	/**
	 * {@inheritDoc}
	 */
	public function name(): string {
		return 'file_archive_create';
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
		if ( ! isset( $params['paths'] ) || ! is_array( $params['paths'] ) || count( $params['paths'] ) === 0 ) {
			return $this->error( 'invalid_path', 'paths is required and must be a non-empty array' );
		}

		if ( ! isset( $params['presigned_puts'] ) || ! is_array( $params['presigned_puts'] ) || count( $params['presigned_puts'] ) === 0 ) {
			return $this->error( 'invalid_path', 'presigned_puts is required and must be a non-empty array' );
		}

		$partSize        = self::DEFAULT_PART_SIZE;
		$confirmSensitive = ! empty( $params['confirm_sensitive'] );

		if ( isset( $params['part_size'] ) && is_int( $params['part_size'] ) ) {
			$partSize = min( max( self::MIN_PART_SIZE, $params['part_size'] ), self::MAX_PART_SIZE );
		}

		// Validate presigned_puts shape.
		$presignedPuts = [];
		foreach ( $params['presigned_puts'] as $put ) {
			if ( ! is_array( $put ) || ! isset( $put['index'], $put['url'] )
				|| ! is_int( $put['index'] ) || ! is_string( $put['url'] )
			) {
				return $this->error( 'invalid_path', 'each presigned_puts entry must have integer index and string url' );
			}
			$presignedPuts[ $put['index'] ] = esc_url_raw( $put['url'] );
		}

		// ------------------------------------------------------------------
		// 2. Resolve jail root (T3: must be non-empty before any write).
		// ------------------------------------------------------------------
		$jailRoot = FileListCommand::resolveJailRoot();
		if ( $jailRoot === '' ) {
			return $this->error( 'write_failed', 'file jail root could not be resolved; no write performed' );
		}

		// ------------------------------------------------------------------
		// 3. Jail and validate every source path.
		// ------------------------------------------------------------------
		/** @var list<array{abs:string,rel:string}> $jailed */
		$jailed     = [];
		$totalBytes = 0;

		foreach ( $params['paths'] as $rawPath ) {
			if ( ! is_string( $rawPath ) ) {
				return $this->error( 'invalid_path', 'each path must be a string' );
			}

			$relPath    = str_replace( '\\', '/', $rawPath );
			$jailResult = FileListCommand::jailPath( $jailRoot, $relPath );
			if ( ! $jailResult['ok'] ) {
				return $this->error( (string) $jailResult['code'], (string) $jailResult['message'] );
			}

			$absPath     = (string) $jailResult['abs'];
			$resolvedRel = (string) $jailResult['rel'];

			if ( ! file_exists( $absPath ) ) {
				return $this->error( 'not_found', 'path not found: ' . $resolvedRel );
			}

			// Count bytes for the cap — recurse into directories.
			$totalBytes += $this->treeSize( $absPath );
			if ( $totalBytes > self::MAX_TOTAL_BYTES ) {
				return $this->error( 'too_large', 'total source size exceeds the 512 MiB archive limit' );
			}

			$jailed[] = [ 'abs' => $absPath, 'rel' => $resolvedRel ];
		}

		// ------------------------------------------------------------------
		// 4. Build the zip into a temp file under the staging area.
		// ------------------------------------------------------------------
		$stagingBase = StoragePaths::ensureHardened( 'file-backups' );
		if ( $stagingBase === '' ) {
			return $this->error( 'write_failed', 'staging directory could not be resolved' );
		}

		$tmpZip = $stagingBase . '/archive-' . bin2hex( random_bytes( 8 ) ) . '.zip';

		$zip = new ZipArchive();
		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- ZipArchive::open creates via native path; WP_Filesystem has no zip-creation streaming API; headless agent never initialises WP_Filesystem
		$opened = $zip->open( $tmpZip, ZipArchive::CREATE | ZipArchive::OVERWRITE );
		if ( $opened !== true ) {
			return $this->error( 'write_failed', 'could not create zip archive' );
		}

		foreach ( $jailed as $entry ) {
			$addErr = $this->addToZip( $zip, $entry['abs'], $entry['rel'], $confirmSensitive );
			if ( $addErr !== null ) {
				$zip->close();
				// phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- headless agent; WP_Filesystem never initialized; cleanup of temp zip on failure
				@unlink( $tmpZip );
				return $this->error( $addErr['code'], $addErr['message'] );
			}
		}

		$zip->close();

		// ------------------------------------------------------------------
		// 5. Upload the zip to S3 via presigned-PUT chain.
		// ------------------------------------------------------------------
		$uploadResult = $this->uploadInChunks( $tmpZip, $presignedPuts, $partSize );

		// phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- headless agent; WP_Filesystem never initialized; cleanup of temp zip after upload
		@unlink( $tmpZip );

		if ( isset( $uploadResult['error'] ) ) {
			return $uploadResult;
		}

		return $uploadResult;
	}

	// ------------------------------------------------------------------
	// Helpers
	// ------------------------------------------------------------------

	/**
	 * Recursively add a path (file or directory tree) to a ZipArchive.
	 * All entries are added with a path relative to their parent inside the archive.
	 *
	 * F1: For every file entry, FileReadCommand::isSensitive() is called on its
	 * site-relative path. If the file is sensitive and $confirmSensitive is false,
	 * the operation is aborted immediately with sensitive_denied. The check mirrors
	 * the exact deny-list used by the read/download path so policy stays in sync.
	 *
	 * @param ZipArchive $zip              The open archive.
	 * @param string     $abs              Absolute source path.
	 * @param string     $rel              Relative entry name to use inside the archive.
	 * @param bool       $confirmSensitive Whether the CP has confirmed owner approval for sensitive files.
	 * @return array{code:string,message:string}|null Null on success, error array on failure.
	 */
	private function addToZip( ZipArchive $zip, string $abs, string $rel, bool $confirmSensitive ): ?array {
		// Never add symlinks (T2 — symlink entries are a zip-slip vector).
		if ( is_link( $abs ) ) {
			return [ 'code' => 'outside_root', 'message' => 'source path is a symbolic link: ' . $rel ];
		}

		if ( is_dir( $abs ) ) {
			// Add directory entry then recurse.
			$zip->addEmptyDir( $rel );

			$handle = @opendir( $abs );
			if ( $handle === false ) {
				return null; // Skip unreadable dirs silently.
			}

			while ( true ) {
				$child = readdir( $handle );
				if ( $child === false ) {
					break;
				}
				if ( $child === '.' || $child === '..' ) {
					continue;
				}
				$childAbs = $abs . '/' . $child;
				$childRel = $rel === '' ? $child : $rel . '/' . $child;
				$err      = $this->addToZip( $zip, $childAbs, $childRel, $confirmSensitive );
				if ( $err !== null ) {
					closedir( $handle );
					return $err;
				}
			}

			closedir( $handle );
			return null;
		}

		if ( ! is_file( $abs ) || ! is_readable( $abs ) ) {
			return null; // Skip special files silently.
		}

		// F1: Sensitive-file gate — checked on every individual file before it is
		// added to the archive. A single sensitive file aborts the whole operation.
		if ( ! $confirmSensitive && FileReadCommand::isSensitive( $rel, basename( $rel ) ) ) {
			return [
				'code'    => 'sensitive_denied',
				'message' => 'archive contains a sensitive file: ' . $rel . '; set confirm_sensitive=true to override (owner permission required)',
			];
		}

		// ZipArchive::addFile() streams the file via the C extension — no full load
		// into PHP memory regardless of file size.
		if ( ! $zip->addFile( $abs, $rel ) ) {
			return [ 'code' => 'write_failed', 'message' => 'could not add file to zip: ' . $rel ];
		}

		return null;
	}

	/**
	 * Upload the zip file to S3 in chunks via presigned PUTs.
	 *
	 * @param string               $zipPath       Absolute path to the temporary zip.
	 * @param array<int,string>    $presignedPuts Map of index => presigned PUT URL.
	 * @param int                  $partSize      Chunk size in bytes.
	 * @return array<string,mixed>
	 */
	private function uploadInChunks( string $zipPath, array $presignedPuts, int $partSize ): array {
		$lstat = @lstat( $zipPath );
		if ( $lstat === false ) {
			return $this->error( 'write_failed', 'zip file disappeared before upload' );
		}
		$totalSize = (int) ( $lstat['size'] ?? 0 );

		// Determine first presigned key from the URL (for CP bookkeeping).
		$sortedKeys = array_keys( $presignedPuts );
		sort( $sortedKeys );
		$objectKey = '';
		if ( count( $sortedKeys ) > 0 ) {
			$firstUrl  = $presignedPuts[ $sortedKeys[0] ];
			$parsed    = wp_parse_url( $firstUrl, PHP_URL_PATH );
			$objectKey = is_string( $parsed ) ? ltrim( $parsed, '/' ) : '';
		}

		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- streaming upload of zip file; WP_Filesystem has no streaming API; headless agent never initialises WP_Filesystem
		$fh = @fopen( $zipPath, 'rb' );
		if ( $fh === false ) {
			return $this->error( 'write_failed', 'could not open zip for upload' );
		}

		$parts     = [];
		$chunkIdx  = 0;
		$bytesSent = 0;

		foreach ( $sortedKeys as $idx ) {
			$url = $presignedPuts[ $idx ];

			// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fread -- streaming read of chunk; WP_Filesystem has no streaming API; headless agent never initialises WP_Filesystem
			$chunk = fread( $fh, $partSize );
			if ( $chunk === false ) {
				// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- cleanup after read error
				fclose( $fh );
				return $this->error( 'write_failed', 'could not read chunk ' . $chunkIdx . ' from zip' );
			}
			if ( $chunk === '' ) {
				break;
			}

			$chunkLen = strlen( $chunk );
			$response = wp_remote_request(
				$url,
				[
					'method'  => 'PUT',
					'body'    => $chunk,
					'headers' => [
						'Content-Length' => (string) $chunkLen,
						'Content-Type'   => 'application/zip',
					],
					'timeout' => 120,
				]
			);

			if ( is_wp_error( $response ) ) {
				// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- cleanup after upload error
				fclose( $fh );
				return $this->error( 'write_failed', 'PUT failed for chunk ' . $chunkIdx . ': ' . $response->get_error_message() );
			}

			$statusCode = (int) wp_remote_retrieve_response_code( $response );
			if ( $statusCode < 200 || $statusCode > 299 ) {
				// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- cleanup after upload error
				fclose( $fh );
				return $this->error( 'write_failed', 'PUT returned HTTP ' . $statusCode . ' for chunk ' . $chunkIdx );
			}

			$etag     = wp_remote_retrieve_header( $response, 'etag' );
			$parts[]  = [
				'index' => $idx,
				'etag'  => is_string( $etag ) ? trim( $etag, '"' ) : '',
				'size'  => $chunkLen,
			];

			$bytesSent += $chunkLen;
			++$chunkIdx;
		}

		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- cleanup after successful upload
		fclose( $fh );

		return [
			'object_key'  => $objectKey,
			'size'        => $totalSize,
			'chunk_count' => count( $parts ),
			'parts'       => $parts,
		];
	}

	/**
	 * Recursively count the total byte size of a file or directory tree.
	 *
	 * @param string $path Absolute path.
	 * @return int Total bytes.
	 */
	private function treeSize( string $path ): int {
		if ( is_link( $path ) ) {
			return 0;
		}

		if ( is_file( $path ) ) {
			$s = @lstat( $path );
			return $s !== false ? (int) ( $s['size'] ?? 0 ) : 0;
		}

		if ( ! is_dir( $path ) ) {
			return 0;
		}

		$total  = 0;
		$handle = @opendir( $path );
		if ( $handle === false ) {
			return 0;
		}

		while ( true ) {
			$child = readdir( $handle );
			if ( $child === false ) {
				break;
			}
			if ( $child === '.' || $child === '..' ) {
				continue;
			}
			$total += $this->treeSize( $path . '/' . $child );
		}

		closedir( $handle );
		return $total;
	}

	/**
	 * @param string $code    Error code.
	 * @param string $message Human-readable message (no absolute host paths).
	 * @return array{error:array{code:string,message:string}}
	 */
	private function error( string $code, string $message ): array {
		return [ 'error' => [ 'code' => $code, 'message' => $message ] ];
	}
}
