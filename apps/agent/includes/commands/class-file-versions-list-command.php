<?php
// File: class-file-versions-list-command.php — FileVersionsListCommand.

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\StoragePaths;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * FileVersionsListCommand: list the pre-write staged backups for a jailed file.
 *
 * Wire contract (CP→agent):
 *   POST /wp-json/wpmgr/v1/command/file_versions_list
 *   Authorization: Bearer <Ed25519 JWT cmd="file_versions_list">
 *   Body: {
 *     "path":              <site-relative path, forward slashes>,
 *     "confirm_sensitive": <bool — default false; CP owner-gates this>
 *   }
 *
 * Response (200 OK):
 *   {
 *     "path":     <string — site-relative>,
 *     "versions": [
 *       {
 *         "version_id": <string — opaque identifier for this version>,
 *         "size":       <int — bytes>,
 *         "mtime":      <int — Unix timestamp of the backup file>,
 *         "created_at": <int — Unix timestamp encoded in the version_id>
 *       }, ...
 *     ]
 *   }
 *
 * Errors:
 *   { "error": { "code": <string>, "message": <string> } }
 *   Codes: invalid_path, outside_root, not_found, sensitive_denied.
 *
 * F3: If the requested path is sensitive (FileReadCommand::isSensitive), listing
 * version history requires confirm_sensitive=true (CP owner-gates it). This
 * prevents a read-role operator from enumerating credential-file backups.
 *
 * VERSION NAMING SCHEME (formalized from FileWriteCommand::stageBackup):
 *
 *   staging root : StoragePaths::dataBase('file-backups')   (e.g. uploads/wpmgr-file-backups)
 *   per-file dir : <staging_root>/<safe_rel>/
 *                  where safe_rel = sanitize_file_name(str_replace(['/', '\\', ':'], '_', $resolvedRel))
 *   backup file  : <per-file dir>/<timestamp>-<hex8>.bak
 *
 *   version_id encodes: "<safe_rel>/<basename>" where basename is the .bak filename.
 *   It is HMAC-free (no secret) — it is an opaque reference, and version_id is
 *   validated by checking that it maps to a file that lives inside the per-file
 *   staging dir for the requested path. Cross-path restore is therefore impossible:
 *   the version_id is only valid for the exact path it was created for.
 *
 * The scheme is deterministic: given a path, we derive safe_rel, scan that dir,
 * and return all .bak files sorted newest-first. The version_id is the .bak
 * basename (just the filename, not the full path), which is enough to reconstruct
 * the absolute path later without leaking host paths in the response.
 *
 * @package WPMgr\Agent\Commands
 */
final class FileVersionsListCommand implements CommandInterface {

	/** Maximum number of versions returned per call. */
	private const MAX_VERSIONS = 50;

	/**
	 * {@inheritDoc}
	 */
	public function name(): string {
		return 'file_versions_list';
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
		// 1. Validate parameters.
		// ------------------------------------------------------------------
		if ( ! array_key_exists( 'path', $params ) || ! is_string( $params['path'] ) || $params['path'] === '' ) {
			return $this->error( 'invalid_path', 'path is required' );
		}

		$relPath          = str_replace( '\\', '/', (string) $params['path'] );
		$confirmSensitive = ! empty( $params['confirm_sensitive'] );

		// ------------------------------------------------------------------
		// 2. Resolve jail root.
		// ------------------------------------------------------------------
		$jailRoot = FileListCommand::resolveJailRoot();
		if ( $jailRoot === '' ) {
			return $this->error( 'not_readable', 'file jail root could not be resolved' );
		}

		// ------------------------------------------------------------------
		// 3. Jail the path.
		// ------------------------------------------------------------------
		$jailResult = FileListCommand::jailPath( $jailRoot, $relPath );
		if ( ! $jailResult['ok'] ) {
			return $this->error( (string) $jailResult['code'], (string) $jailResult['message'] );
		}

		$resolvedRel = (string) $jailResult['rel'];

		// ------------------------------------------------------------------
		// F3: Sensitive-file gate. Listing version history for a sensitive file
		// (wp-config.php, .env, key files, etc.) requires explicit owner confirmation.
		// Without this gate a read-role operator could enumerate backup filenames
		// and learn when sensitive credentials were last changed.
		// ------------------------------------------------------------------
		if ( ! $confirmSensitive && FileReadCommand::isSensitive( $resolvedRel, basename( $resolvedRel ) ) ) {
			return $this->error(
				'sensitive_denied',
				'listing versions for a sensitive file requires confirm_sensitive=true (owner permission required)'
			);
		}

		// ------------------------------------------------------------------
		// 4. Compute the per-file staging directory (same scheme as stageBackup).
		// ------------------------------------------------------------------
		$stagingBase = StoragePaths::dataBase( 'file-backups' );
		if ( $stagingBase === '' ) {
			return [ 'path' => $resolvedRel, 'versions' => [] ];
		}

		$backupDir = self::backupDirForPath( $stagingBase, $resolvedRel );

		if ( ! is_dir( $backupDir ) ) {
			return [ 'path' => $resolvedRel, 'versions' => [] ];
		}

		// ------------------------------------------------------------------
		// 5. Scan for .bak files in the per-file dir.
		// ------------------------------------------------------------------
		$handle = @opendir( $backupDir );
		if ( $handle === false ) {
			return [ 'path' => $resolvedRel, 'versions' => [] ];
		}

		/** @var list<array{version_id:string,size:int,mtime:int,created_at:int}> $versions */
		$versions = [];

		while ( true ) {
			$entry = readdir( $handle );
			if ( $entry === false ) {
				break;
			}

			if ( $entry === '.' || $entry === '..' ) {
				continue;
			}

			// Only .bak files belong to us.
			if ( ! str_ends_with( $entry, '.bak' ) ) {
				continue;
			}

			// Skip non-files and symlinks.
			$absEntry = $backupDir . '/' . $entry;
			if ( is_link( $absEntry ) || ! is_file( $absEntry ) ) {
				continue;
			}

			$lstat = @lstat( $absEntry );
			if ( $lstat === false ) {
				continue;
			}

			$size  = (int) ( $lstat['size'] ?? 0 );
			$mtime = (int) ( $lstat['mtime'] ?? 0 );

			// Extract the Unix timestamp from the filename (<timestamp>-<hex8>.bak).
			// The timestamp is the number before the first '-'.
			$createdAt = 0;
			$dashPos   = strpos( $entry, '-' );
			if ( $dashPos !== false ) {
				$tsPart    = substr( $entry, 0, $dashPos );
				$createdAt = ctype_digit( $tsPart ) ? (int) $tsPart : 0;
			}

			// version_id is the .bak filename (no path) — unambiguous per-path identifier.
			$versions[] = [
				'version_id' => $entry,
				'size'       => $size,
				'mtime'      => $mtime,
				'created_at' => $createdAt,
			];
		}

		closedir( $handle );

		// Sort newest-first (by created_at, then mtime as tie-breaker).
		usort(
			$versions,
			static function ( array $a, array $b ): int {
				$cmp = $b['created_at'] - $a['created_at'];
				if ( $cmp !== 0 ) {
					return $cmp;
				}
				return $b['mtime'] - $a['mtime'];
			}
		);

		// Cap the result.
		$versions = array_slice( $versions, 0, self::MAX_VERSIONS );

		return [
			'path'     => $resolvedRel,
			'versions' => $versions,
		];
	}

	// ------------------------------------------------------------------
	// Shared staging-path helper (used by FileVersionRestoreCommand too).
	// ------------------------------------------------------------------

	/**
	 * Compute the absolute per-file backup directory for a given site-relative path.
	 *
	 * This formalizes the scheme used by FileWriteCommand::stageBackup() and
	 * must stay in sync with it.
	 *
	 * Scheme:
	 *   <staging_base>/<sanitize_file_name(str_replace(['/', '\\', ':'], '_', $resolvedRel))>/
	 *
	 * @param string $stagingBase  Absolute staging base (from StoragePaths::dataBase).
	 * @param string $resolvedRel  Site-relative path (forward slashes, no leading slash).
	 * @return string Absolute path of the per-file backup directory.
	 */
	public static function backupDirForPath( string $stagingBase, string $resolvedRel ): string {
		$safeRel = str_replace( [ '/', '\\', ':' ], '_', $resolvedRel );
		$safeRel = ltrim( $safeRel, '.' );
		return $stagingBase . '/' . sanitize_file_name( $safeRel );
	}

	// ------------------------------------------------------------------
	// Response helpers
	// ------------------------------------------------------------------

	/**
	 * @param string $code    Error code.
	 * @param string $message Human-readable message (no absolute host paths).
	 * @return array{error:array{code:string,message:string}}
	 */
	private function error( string $code, string $message ): array {
		return [ 'error' => [ 'code' => $code, 'message' => $message ] ];
	}
}
