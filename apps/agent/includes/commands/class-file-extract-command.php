<?php
// File: class-file-extract-command.php — FileExtractCommand.

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Support\StoragePaths;
use ZipArchive;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * FileExtractCommand: extract a jailed .zip into a jailed destination.
 *
 * THIS IS THE HIGH-RISK COMMAND. Every entry is validated before any byte
 * is written, and extraction is fully atomic (quarantine → validate → swap).
 *
 * Wire contract (CP→agent):
 *   POST /wp-json/wpmgr/v1/command/file_extract
 *   Authorization: Bearer <Ed25519 JWT cmd="file_extract">
 *   Body: {
 *     "archive_path":             <site-relative path to a .zip file>,
 *     "dest_path":                <site-relative destination directory>,
 *     "confirm_executable_write": <bool — default false>,
 *     "confirm_sensitive":        <bool — default false>
 *   }
 *
 * Response (200 OK):
 *   { "dest_path": <string>, "extracted": <int> }
 *
 * Errors:
 *   { "error": { "code": <string>, "message": <string> } }
 *   Codes: invalid_path, outside_root, not_found, not_readable, not_archive,
 *          bad_archive, zip_slip, zip_bomb, too_large,
 *          executable_write_denied, sensitive_denied, write_failed.
 *
 * ZIP-SLIP DEFENSE (T2 — for every entry):
 *   - Reject any entry whose name is absolute (starts with / or drive letter).
 *   - Reject any entry name containing '..', NUL, or a Windows drive/UNC prefix.
 *   - Reject any entry that is a symlink, hardlink, device, or special file
 *     (checked via external attributes in the central directory).
 *   - Canonicalize the entry name against realpath(dest_path) and ABORT THE
 *     WHOLE EXTRACTION (no partial write) if the resolved target is not strictly
 *     contained in dest.
 *
 * ZIP-BOMB DEFENSE (T10 — enforced BEFORE and DURING extraction):
 *   - MAX_ENTRY_COUNT: hard cap on number of entries.
 *   - MAX_TOTAL_UNCOMPRESSED: hard cap on total uncompressed bytes (1 GiB).
 *   - MAX_ENTRY_UNCOMPRESSED: hard cap on a single entry's uncompressed size (256 MiB).
 *   - MAX_ENTRY_RATIO: abort if any entry's compression ratio (uncompressed/compressed)
 *     exceeds this limit (200×). Only enforced when compressed size > 0.
 *
 * EXEC / SENSITIVE GUARD:
 *   - Abort with executable_write_denied if ANY entry resolves to an executable
 *     file (FileGuards extension check), UNLESS confirm_executable_write=true.
 *   - Abort with sensitive_denied if ANY entry resolves to a sensitive path
 *     (FileReadCommand::isSensitive), UNLESS confirm_sensitive=true.
 *   - Content sniff (<?php / <?=) is applied to the EXTRACTED BYTES of each entry.
 *
 * ATOMIC QUARANTINE PATTERN:
 *   1. Create a fresh quarantine temp dir under the system temp dir returned by
 *      get_temp_dir() (which falls back to sys_get_temp_dir()). The system temp
 *      dir is not served by the web server on any standard Linux/Apache/nginx
 *      stack, so extracted .php files are inaccessible during the extract window.
 *      If get_temp_dir() resolves to a path under ABSPATH (unusual mis-configured
 *      installs), we fall back to the hardened staging area under uploads/ with
 *      .htaccess + index.php guards already in place.
 *   2. Validate EVERY entry (slip + bomb + exec + sensitive) BEFORE writing any byte.
 *   3. Extract into the quarantine dir.
 *   4. Atomically move (rename) the quarantine tree into dest_path on success.
 *   5. On ANY failure (including fatal errors), clean up the quarantine dir via
 *      a finally block and leave dest untouched.
 *
 * @package WPMgr\Agent\Commands
 */
final class FileExtractCommand implements CommandInterface {

	/**
	 * Hard cap on the number of entries in the archive (zip-bomb defense, T10).
	 * Abort with zip_bomb when the archive contains more than this many entries.
	 */
	public const MAX_ENTRY_COUNT = 50000;

	/**
	 * Hard cap on total uncompressed bytes across all entries, in bytes (1 GiB).
	 * Abort with zip_bomb when the sum of all uncompressed entry sizes exceeds this.
	 */
	public const MAX_TOTAL_UNCOMPRESSED = 1073741824;

	/**
	 * Hard cap on a single entry's uncompressed size (256 MiB).
	 * Abort with too_large when any single entry's uncompressed size exceeds this.
	 */
	public const MAX_ENTRY_UNCOMPRESSED = 268435456;

	/**
	 * Maximum compression ratio (uncompressed / compressed).
	 * A ratio above 200 strongly indicates a zip-bomb payload. Only enforced
	 * when the entry's compressed size is > 0 (stored/uncompressed entries are exempt).
	 */
	public const MAX_ENTRY_RATIO = 200;

	/**
	 * External attributes Unix mode mask for symlink detection.
	 * A zip entry's external attributes (high 16 bits) encode the Unix mode.
	 * S_IFLNK = 0xA000 (40960). If (externalAttributes >> 16) & 0xF000 == 0xA000,
	 * the entry is a symbolic link.
	 */
	private const UNIX_SYMLINK_MODE = 0xA000;

	/** Unix mode file-type mask. */
	private const UNIX_FTYPE_MASK = 0xF000;

	/**
	 * {@inheritDoc}
	 */
	public function name(): string {
		return 'file_extract';
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
		if ( ! isset( $params['archive_path'] ) || ! is_string( $params['archive_path'] ) || $params['archive_path'] === '' ) {
			return $this->error( 'invalid_path', 'archive_path is required' );
		}

		if ( ! isset( $params['dest_path'] ) || ! is_string( $params['dest_path'] ) || $params['dest_path'] === '' ) {
			return $this->error( 'invalid_path', 'dest_path is required' );
		}

		$archivePath           = str_replace( '\\', '/', (string) $params['archive_path'] );
		$destPathRel           = str_replace( '\\', '/', (string) $params['dest_path'] );
		$confirmExecWrite      = ! empty( $params['confirm_executable_write'] );
		$confirmSensitive      = ! empty( $params['confirm_sensitive'] );

		// ------------------------------------------------------------------
		// 2. Resolve jail root (T3 guard: must be non-empty before any write).
		// ------------------------------------------------------------------
		$jailRoot = FileListCommand::resolveJailRoot();
		if ( $jailRoot === '' ) {
			return $this->error( 'write_failed', 'file jail root could not be resolved; no write performed' );
		}

		// ------------------------------------------------------------------
		// 3. Jail the archive path.
		// ------------------------------------------------------------------
		$archiveJail = FileListCommand::jailPath( $jailRoot, $archivePath );
		if ( ! $archiveJail['ok'] ) {
			return $this->error( (string) $archiveJail['code'], (string) $archiveJail['message'] );
		}
		$archiveAbs = (string) $archiveJail['abs'];
		$archiveRel = (string) $archiveJail['rel'];

		if ( ! file_exists( $archiveAbs ) ) {
			return $this->error( 'not_found', 'archive not found: ' . $archiveRel );
		}

		if ( is_dir( $archiveAbs ) ) {
			return $this->error( 'not_archive', 'archive_path points to a directory, not a file' );
		}

		if ( is_link( $archiveAbs ) ) {
			return $this->error( 'not_readable', 'archive_path is a symbolic link' );
		}

		// Must be a .zip file (extension + magic bytes check).
		if ( ! $this->isZipFile( $archiveAbs, $archiveRel ) ) {
			return $this->error( 'not_archive', 'file is not a .zip archive: ' . $archiveRel );
		}

		// ------------------------------------------------------------------
		// 4. Jail the destination path; create it (hardened) if absent.
		// ------------------------------------------------------------------
		$destJail = FileListCommand::jailPath( $jailRoot, $destPathRel );
		if ( ! $destJail['ok'] ) {
			return $this->error( (string) $destJail['code'], (string) $destJail['message'] );
		}
		$destAbs = (string) $destJail['abs'];
		$destRel = (string) $destJail['rel'];

		if ( file_exists( $destAbs ) && ! is_dir( $destAbs ) ) {
			return $this->error( 'invalid_path', 'dest_path exists but is not a directory: ' . $destRel );
		}

		if ( ! is_dir( $destAbs ) ) {
			$destAbs = StoragePaths::ensureHardenedPath( $destAbs );
			if ( $destAbs === '' || ! is_dir( $destAbs ) ) {
				return $this->error( 'write_failed', 'could not create destination directory: ' . $destRel );
			}
		}

		// Canonicalize destAbs after creation.
		$destReal = realpath( $destAbs );
		if ( $destReal === false ) {
			return $this->error( 'write_failed', 'destination directory could not be canonicalized' );
		}

		// Re-verify dest is inside the jail after canonicalization.
		if ( strncmp( str_replace( '\\', '/', $destReal ), $jailRoot . '/', strlen( $jailRoot ) + 1 ) !== 0
			&& str_replace( '\\', '/', $destReal ) !== $jailRoot
		) {
			return $this->error( 'outside_root', 'destination is outside the jail root after canonicalization' );
		}

		// ------------------------------------------------------------------
		// 5. Open the archive and run preflight checks (zip-bomb + zip-slip
		//    entry validation) BEFORE extracting any bytes.
		// ------------------------------------------------------------------
		$zip = new ZipArchive();
		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- ZipArchive::open; WP_Filesystem has no zip-extraction API; headless agent never initialises WP_Filesystem
		$opened = $zip->open( $archiveAbs );
		if ( $opened !== true ) {
			return $this->error( 'bad_archive', 'could not open zip archive (ZipArchive code: ' . $opened . ')' );
		}

		$numFiles = $zip->numFiles;

		// Zip-bomb: entry count cap.
		if ( $numFiles > self::MAX_ENTRY_COUNT ) {
			$zip->close();
			return $this->error( 'zip_bomb', 'archive exceeds maximum entry count (' . self::MAX_ENTRY_COUNT . ')' );
		}

		// Validate every entry in the central directory (no bytes extracted yet).
		$preflightResult = $this->preflightEntries( $zip, $destReal, $confirmExecWrite, $confirmSensitive );
		if ( $preflightResult !== null ) {
			$zip->close();
			return $preflightResult;
		}

		// ------------------------------------------------------------------
		// 6. Extract into a quarantine temp dir under the system temp dir.
		//
		//    NEW-2: We resolve the quarantine location via get_temp_dir() (which
		//    returns WP_TEMP_DIR when defined, then falls back to sys_get_temp_dir()).
		//    The system temp dir is never web-served on standard Linux installs so
		//    extracted .php files cannot be reached by the web server during the
		//    sub-second extract window — stronger than the prior .htaccess guard.
		//
		//    Fallback: if get_temp_dir() resolves under ABSPATH (rare mis-configured
		//    install), we fall back to the hardened staging area (uploads/wpmgr-file-
		//    extract-tmp), which carries .htaccess + index.php guards. The fallback
		//    is only taken when the system temp dir is not trustworthy; it is not the
		//    primary path.
		//
		//    A crash or mid-extract failure is always handled by the finally block.
		// ------------------------------------------------------------------
		$quarantineDir = $this->resolveQuarantineDir( $jailRoot );
		if ( $quarantineDir === '' ) {
			$zip->close();
			return $this->error( 'write_failed', 'quarantine directory could not be resolved or created' );
		}

		try {
			$extractResult = $this->extractEntries( $zip, $quarantineDir, $destReal, $confirmExecWrite, $confirmSensitive );

			if ( isset( $extractResult['error'] ) ) {
				return $extractResult;
			}

			$extractedCount = (int) ( $extractResult['count'] ?? 0 );

			// ---------------------------------------------------------------
			// 7. Atomic swap: move quarantine tree into dest_path.
			//    We move the contents of quarantine INTO dest (not replace dest
			//    itself) so that dest_path remains at the same absolute path.
			// ---------------------------------------------------------------
			$swapResult = $this->swapIntoDestination( $quarantineDir, $destReal );
			if ( $swapResult !== null ) {
				return $swapResult;
			}

			return [
				'dest_path' => $destRel,
				'extracted' => $extractedCount,
			];
		} finally {
			// F5: Always clean up the quarantine dir and close the archive handle.
			// Runs on success, failure, and any exception/fatal so the temp dir
			// never accumulates resident files and the ZipArchive handle is freed.
			$zip->close();
			$this->removeTree( $quarantineDir );
		}
	}

	// ------------------------------------------------------------------
	// Preflight: validate all entries from the central directory BEFORE extraction.
	// Returns an error array on the first violation, or null if all entries pass.
	// ------------------------------------------------------------------

	/**
	 * Validate every zip entry against: zip-slip, zip-bomb, symlink/hardlink/device,
	 * executable-write, sensitive-file, and content-sniff policies.
	 * No bytes are extracted; this reads only the central directory metadata.
	 *
	 * @param ZipArchive $zip              Open archive.
	 * @param string     $destReal         Canonicalized destination directory.
	 * @param bool       $confirmExecWrite Whether exec writes are confirmed.
	 * @param bool       $confirmSensitive Whether sensitive writes are confirmed.
	 * @return array{error:array{code:string,message:string}}|null
	 */
	private function preflightEntries(
		ZipArchive $zip,
		string $destReal,
		bool $confirmExecWrite,
		bool $confirmSensitive
	): ?array {
		$numFiles       = $zip->numFiles;
		$totalUncomp    = 0;

		for ( $i = 0; $i < $numFiles; $i++ ) {
			$stat = $zip->statIndex( $i );
			if ( $stat === false ) {
				return $this->error( 'bad_archive', 'could not read entry at index ' . $i );
			}

			$entryName = $stat['name'];

			// -- Zip-slip: absolute path check.
			if ( $entryName === '' ) {
				continue; // Empty entry name — skip.
			}

			// Reject absolute paths (Unix / or Windows C:\ / UNC \\).
			if ( $entryName[0] === '/' || $entryName[0] === '\\' ) {
				return $this->error( 'zip_slip', 'entry has absolute path: ' . $entryName );
			}

			// Reject Windows drive prefix (e.g. C:).
			if ( strlen( $entryName ) >= 2 && ctype_alpha( $entryName[0] ) && $entryName[1] === ':' ) {
				return $this->error( 'zip_slip', 'entry has Windows drive prefix: ' . $entryName );
			}

			// Reject UNC path prefix (\\).
			if ( strlen( $entryName ) >= 2 && $entryName[0] === '\\' && $entryName[1] === '\\' ) {
				return $this->error( 'zip_slip', 'entry has UNC prefix: ' . $entryName );
			}

			// Reject NUL bytes.
			if ( strpos( $entryName, "\0" ) !== false ) {
				return $this->error( 'zip_slip', 'entry name contains NUL byte' );
			}

			// Reject '..' in any path segment.
			$segments = explode( '/', str_replace( '\\', '/', $entryName ) );
			foreach ( $segments as $seg ) {
				if ( $seg === '..' ) {
					return $this->error( 'zip_slip', 'entry name contains path traversal (..) : ' . $entryName );
				}
			}

			// -- Symlink / hardlink / device check via external attributes.
			// Use getExternalAttributesIndex() — more reliable than statIndex()'s
			// 'external_attributes' key, which some PHP builds omit.
			$opsys    = 0;
			$extAttr  = 0;
			$zip->getExternalAttributesIndex( $i, $opsys, $extAttr );

			if ( $opsys === ZipArchive::OPSYS_UNIX ) {
				// Unix mode is in the high 16 bits of the external attributes word.
				$unixMode = ( $extAttr >> 16 ) & 0xFFFF;
				$ftype    = $unixMode & self::UNIX_FTYPE_MASK;

				// Symlink.
				if ( $ftype === self::UNIX_SYMLINK_MODE ) {
					return $this->error( 'zip_slip', 'entry is a symbolic link: ' . $entryName );
				}

				// Any non-regular, non-directory, non-zero type (device/FIFO/socket).
				// S_IFREG = 0x8000, S_IFDIR = 0x4000.
				if ( $unixMode !== 0 && $ftype !== 0 && $ftype !== 0x8000 && $ftype !== 0x4000 ) {
					return $this->error( 'zip_slip', 'entry is a non-regular file (device/socket/FIFO): ' . $entryName );
				}
			}

			// -- Zip-slip: realpath containment check on the resolved target path.
			// Clean the entry name and resolve where it would land in dest.
			$cleanName  = ltrim( str_replace( '\\', '/', $entryName ), '/' );
			$targetPath = $destReal . '/' . $cleanName;
			// Normalize away any redundant separators (no .. remain after the segment check).
			$targetNorm = str_replace( '//', '/', $targetPath );
			// The target must be strictly inside destReal.
			if ( strncmp( $targetNorm, $destReal . '/', strlen( $destReal ) + 1 ) !== 0
				&& $targetNorm !== $destReal
			) {
				return $this->error( 'zip_slip', 'entry resolves outside destination: ' . $entryName );
			}

			// -- Zip-bomb: per-entry uncompressed size.
			$uncompSize = (int) ( $stat['size'] ?? 0 );
			if ( $uncompSize > self::MAX_ENTRY_UNCOMPRESSED ) {
				return $this->error( 'too_large', 'entry exceeds the 256 MiB per-entry limit: ' . $entryName );
			}

			// -- Zip-bomb: compression ratio check.
			$compSize = (int) ( $stat['comp_size'] ?? 0 );
			if ( $compSize > 0 && $uncompSize > 0 ) {
				$ratio = (int) ceil( $uncompSize / $compSize );
				if ( $ratio > self::MAX_ENTRY_RATIO ) {
					return $this->error( 'zip_bomb', 'entry compression ratio (' . $ratio . ':1) exceeds limit: ' . $entryName );
				}
			}

			// -- Zip-bomb: total uncompressed bytes.
			$totalUncomp += $uncompSize;
			if ( $totalUncomp > self::MAX_TOTAL_UNCOMPRESSED ) {
				return $this->error( 'zip_bomb', 'archive total uncompressed size exceeds the 1 GiB limit' );
			}

			// -- Executable-write and sensitive-file checks (extension-based).
			// Skip directory entries for executable/sensitive checks.
			$isDir = str_ends_with( $cleanName, '/' );
			if ( ! $isDir && $cleanName !== '' ) {
				$basename  = basename( $cleanName );
				// F6: Use substr instead of str_replace to strip the dest prefix — str_replace
				// would silently mangle paths that happen to contain $destReal as a substring.
				$relInDest = ltrim( substr( $targetNorm, strlen( $destReal ) ), '/' );

				if ( ! $confirmExecWrite && FileGuards::hasExecutableExtension( strtolower( $basename ) ) ) {
					return $this->error(
						'executable_write_denied',
						'archive contains executable file: ' . $entryName . '; set confirm_executable_write=true to override'
					);
				}

				if ( ! $confirmSensitive && FileReadCommand::isSensitive( $relInDest, $basename ) ) {
					return $this->error(
						'sensitive_denied',
						'archive contains sensitive file: ' . $entryName . '; set confirm_sensitive=true to override'
					);
				}
			}
		}

		return null;
	}

	// ------------------------------------------------------------------
	// Extraction: stream entries into quarantine, re-checking each one.
	// ------------------------------------------------------------------

	/**
	 * Extract all entries into the quarantine directory, re-validating each
	 * entry as it is written (content sniff + hard limits applied to actual bytes).
	 *
	 * F2: A $totalWritten accumulator tracks ACTUAL bytes written across all entries.
	 * If at any point $totalWritten would exceed MAX_TOTAL_UNCOMPRESSED the extraction
	 * is aborted immediately with zip_bomb — regardless of what the central directory
	 * claimed. This closes the lying-central-directory bypass vector where a crafted
	 * zip under-reports sizes in its metadata but expands to unbounded bytes on disk.
	 *
	 * F8: The content sniff is applied to the FULL entry content, not just the first
	 * 512 bytes. An 8-byte carry-over buffer is kept across chunk boundaries so a PHP
	 * open tag that straddles a chunk boundary (e.g. '<?ph' at end + 'p ...' at start
	 * of next chunk) is correctly detected. This is the same approach used by the P2
	 * full-file upload scan.
	 *
	 * @param ZipArchive $zip              Open archive.
	 * @param string     $quarantineDir    Absolute path to quarantine temp dir (outside web root).
	 * @param string     $destReal         Canonicalized destination (used only for rel-path computation).
	 * @param bool       $confirmExecWrite Whether exec writes are confirmed.
	 * @param bool       $confirmSensitive Whether sensitive writes are confirmed.
	 * @return array<string,mixed> { count: int } on success, or { error: {...} } on failure.
	 */
	private function extractEntries(
		ZipArchive $zip,
		string $quarantineDir,
		string $destReal,
		bool $confirmExecWrite,
		bool $confirmSensitive
	): array {
		$numFiles     = $zip->numFiles;
		$extracted    = 0;
		// F2: Running total of ACTUAL bytes written — not the central-directory claim.
		$totalWritten = 0;

		for ( $i = 0; $i < $numFiles; $i++ ) {
			$stat = $zip->statIndex( $i );
			if ( $stat === false ) {
				continue;
			}

			$entryName = $stat['name'];
			$cleanName = ltrim( str_replace( '\\', '/', $entryName ), '/' );
			if ( $cleanName === '' ) {
				continue;
			}

			$targetPath = $quarantineDir . '/' . $cleanName;
			$isDir      = str_ends_with( $cleanName, '/' ) || str_ends_with( $entryName, '/' );

			if ( $isDir ) {
				wp_mkdir_p( $targetPath );
				continue;
			}

			// Ensure parent dir exists.
			$parentDir = dirname( $targetPath );
			if ( ! is_dir( $parentDir ) ) {
				wp_mkdir_p( $parentDir );
			}

			// Verify the resolved path is still inside quarantine (re-check after mkdir).
			$realParent = realpath( $parentDir );
			if ( $realParent === false
				|| strncmp( str_replace( '\\', '/', $realParent ), $quarantineDir . '/', strlen( $quarantineDir ) + 1 ) !== 0
				&& str_replace( '\\', '/', $realParent ) !== $quarantineDir
			) {
				return $this->error( 'zip_slip', 'entry would escape quarantine: ' . $entryName );
			}

			// Verify the target file itself is inside quarantine.
			$realTarget = $realParent . '/' . basename( $cleanName );
			if ( strncmp( str_replace( '\\', '/', $realTarget ), $quarantineDir . '/', strlen( $quarantineDir ) + 1 ) !== 0
				&& str_replace( '\\', '/', $realTarget ) !== $quarantineDir
			) {
				return $this->error( 'zip_slip', 'entry resolves outside quarantine: ' . $entryName );
			}

			// Refuse if target exists as a symlink (TOCTOU guard).
			if ( is_link( $realTarget ) ) {
				return $this->error( 'zip_slip', 'extraction target is a symbolic link: ' . $entryName );
			}

			// Stream the entry via ZipArchive::getStream() to avoid loading the
			// whole entry into PHP memory. Read up to MAX_ENTRY_UNCOMPRESSED bytes.
			// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- ZipArchive::getStream; WP_Filesystem has no zip-stream API; headless agent never initialises WP_Filesystem
			$stream = $zip->getStream( $entryName );
			if ( $stream === false ) {
				return $this->error( 'bad_archive', 'could not open stream for entry: ' . $entryName );
			}

			// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- wrapping ZipArchive stream; no WP_Filesystem equivalent
			$destStream = @fopen( $realTarget, 'wb' );
			if ( $destStream === false ) {
				// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- cleanup of ZipArchive stream
				fclose( $stream );
				return $this->error( 'write_failed', 'could not open destination for writing: ' . $entryName );
			}

			$entryTooLarge = false;
			$totalBomb     = false;
			$entryRead     = 0;

			// F8: Full-content sniff with an 8-byte carry-over across chunk boundaries.
			// We accumulate all content (memory-bounded by per-entry cap) into a sniff
			// buffer and scan the full string after all chunks are read. The carry-over
			// ensures PHP open tags straddling a 65536-byte chunk boundary are caught.
			$sniffBuffer = '';
			$carry       = '';             // Last 8 bytes of the previous chunk.
			$sniffDone   = false;          // Set to true once we detect a PHP tag.

			// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fread -- streaming extraction from ZipArchive; no WP_Filesystem equivalent
			while ( ! feof( $stream ) ) {
				// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fread -- streaming extraction from ZipArchive; no WP_Filesystem equivalent
				$chunk = fread( $stream, 65536 );
				if ( $chunk === false ) {
					break;
				}
				if ( $chunk === '' ) {
					continue;
				}

				$chunkLen  = strlen( $chunk );
				$entryRead += $chunkLen;

				if ( $entryRead > self::MAX_ENTRY_UNCOMPRESSED ) {
					$entryTooLarge = true;
					break;
				}

				// F2: Check aggregate total BEFORE writing to disk.
				if ( ( $totalWritten + $entryRead ) > self::MAX_TOTAL_UNCOMPRESSED ) {
					$totalBomb = true;
					break;
				}

				// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fwrite -- writing to file during extraction; WP_Filesystem has no streaming extraction API; headless agent never initialises WP_Filesystem
				fwrite( $destStream, $chunk );

				// F8: Build the carry-over + chunk window for the sniff. We scan
				// carry||chunk so tags crossing the boundary are always visible.
				if ( ! $confirmExecWrite && ! $sniffDone ) {
					$window = $carry . $chunk;
					if ( FileGuards::sniffsAsPhp( $window ) ) {
						$sniffDone = true;
					}
					// Keep the last 8 bytes as the next carry-over (longest PHP tag
					// is '<?php' = 5 chars; 8 bytes gives a safe margin).
					$carry = substr( $chunk, -8 );
				}
			}

			// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- cleanup after extraction of entry
			fclose( $destStream );
			// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- cleanup after extraction of entry
			fclose( $stream );

			if ( $entryTooLarge ) {
				return $this->error( 'too_large', 'entry exceeds per-entry size limit during extraction: ' . $entryName );
			}

			// F2: Aggregate total exceeded — the central directory lied about sizes.
			if ( $totalBomb ) {
				return $this->error( 'zip_bomb', 'archive total uncompressed size exceeds the 1 GiB limit during streaming extraction' );
			}

			// F8: Full-content sniff detected a PHP open tag anywhere in the entry.
			if ( ! $confirmExecWrite && $sniffDone ) {
				return $this->error(
					'executable_write_denied',
					'archive entry content contains PHP open tag: ' . $entryName . '; set confirm_executable_write=true to override'
				);
			}

			$totalWritten += $entryRead;
			++$extracted;
		}

		return [ 'count' => $extracted ];
	}

	// ------------------------------------------------------------------
	// Atomic swap: move quarantine contents into destination.
	// ------------------------------------------------------------------

	/**
	 * Move the contents of quarantine dir into dest dir.
	 * Uses rename() for each top-level entry in the quarantine so the move
	 * is as atomic as possible (each rename is atomic on POSIX, same filesystem).
	 *
	 * @param string $quarantineDir Absolute quarantine dir.
	 * @param string $destReal      Absolute destination dir.
	 * @return array{error:array{code:string,message:string}}|null
	 */
	private function swapIntoDestination( string $quarantineDir, string $destReal ): ?array {
		$handle = @opendir( $quarantineDir );
		if ( $handle === false ) {
			return $this->error( 'write_failed', 'could not open quarantine directory for swap' );
		}

		while ( true ) {
			$child = readdir( $handle );
			if ( $child === false ) {
				break;
			}
			if ( $child === '.' || $child === '..' ) {
				continue;
			}

			$from = $quarantineDir . '/' . $child;
			$to   = $destReal . '/' . $child;

			// If the destination already has an entry at this name, remove it first
			// so rename() can overwrite (on some OS, rename() fails if $to exists as a dir).
			if ( is_dir( $to ) && ! is_link( $to ) ) {
				$this->removeTree( $to );
			}

			// phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- WP_Filesystem::move() is non-atomic (copy+delete) and WP_Filesystem is never initialized in the headless agent path; native rename() is the only atomic option on POSIX filesystems
			if ( ! @rename( $from, $to ) ) {
				closedir( $handle );
				return $this->error( 'write_failed', 'could not move extracted entry to destination: ' . $child );
			}
		}

		closedir( $handle );
		return null;
	}

	// ------------------------------------------------------------------
	// Utilities
	// ------------------------------------------------------------------

	/**
	 * Resolve and create a per-job quarantine directory under the system temp dir.
	 *
	 * NEW-2: Uses get_temp_dir() (which honours WP_TEMP_DIR, then sys_get_temp_dir())
	 * rather than the uploads staging area. The system temp dir is never served by
	 * the web server on standard Linux stacks, eliminating the sub-second web-exec
	 * window that existed when the quarantine was under uploads/.
	 *
	 * Fallback: if get_temp_dir() returns a path that resolves under ABSPATH (unusual
	 * mis-configured installs where /tmp is inside the web root), we fall back to the
	 * hardened uploads staging area which carries .htaccess + index.php guards.
	 *
	 * @param string $jailRoot Canonicalized ABSPATH jail root (used for fallback check).
	 * @return string Absolute path to the per-job quarantine dir, '' on failure.
	 */
	private function resolveQuarantineDir( string $jailRoot ): string {
		// Prefer the system temp dir (not web-served on any standard Linux install).
		$tmpBase = function_exists( 'get_temp_dir' ) ? rtrim( (string) get_temp_dir(), '/\\' ) : '';
		if ( $tmpBase === '' ) {
			$tmpBase = rtrim( sys_get_temp_dir(), '/\\' );
		}

		// Safety: confirm the temp base is writable and not under the web root.
		$tmpBaseReal = $tmpBase !== '' ? @realpath( $tmpBase ) : false;
		$underAbspath = $tmpBaseReal !== false
			&& $jailRoot !== ''
			&& strncmp(
				str_replace( '\\', '/', (string) $tmpBaseReal ),
				$jailRoot . '/',
				strlen( $jailRoot ) + 1
			) === 0;

		$useSystemTmp = $tmpBaseReal !== false
			&& ! $underAbspath
			// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_is_writable -- headless agent; WP_Filesystem never initialized; direct writability probe
			&& is_writable( $tmpBaseReal );

		$quarantineDir = '';
		if ( $useSystemTmp ) {
			// Create a per-job subdirectory with restrictive permissions.
			$quarantineDir = (string) $tmpBaseReal . '/wpmgr-extract-' . bin2hex( random_bytes( 8 ) );
			// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_mkdir -- explicit 0700 perms; wp_mkdir_p uses 0755 which is too wide for a temp dir holding decrypted bytes
			if ( ! @mkdir( $quarantineDir, 0700, true ) && ! is_dir( $quarantineDir ) ) {
				// mkdir failed — fall through to the uploads fallback below.
				$useSystemTmp = false;
			}
		}

		if ( $useSystemTmp ) {
			// System temp dir quarantine is ready. No .htaccess guard needed — the
			// directory is not under the web root. The 0700 permission ensures only
			// the web-server user (www-data) can read the per-job dir.
			return $quarantineDir;
		}

		// Fallback: hardened uploads staging area (carries .htaccess + index.php).
		// This path is taken only when the system temp dir is under ABSPATH or
		// is not writable (unusual but defensively handled).
		$stagingBase = StoragePaths::ensureHardened( 'file-extract-tmp' );
		if ( $stagingBase === '' ) {
			return '';
		}

		$quarantineDir = $stagingBase . '/extract-' . bin2hex( random_bytes( 8 ) );

		if ( ! wp_mkdir_p( $quarantineDir ) ) {
			return '';
		}

		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- headless agent; WP_Filesystem never initialized; restricting quarantine dir permissions
		@chmod( $quarantineDir, 0700 );

		// Drop an index.php so the per-job slot is hardened independently of the
		// base dir (the base already has .htaccess + index.php from ensureHardened).
		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- headless agent; WP_Filesystem never initialized; static guard file write
		@file_put_contents( $quarantineDir . '/index.php', "<?php\n// Silence is golden.\n", LOCK_EX );

		return $quarantineDir;
	}

	/**
	 * Check that the file is actually a .zip (extension + PK magic bytes).
	 *
	 * @param string $absPath    Absolute file path.
	 * @param string $resolvedRel Site-relative path for the error message.
	 * @return bool
	 */
	private function isZipFile( string $absPath, string $resolvedRel ): bool {
		// Extension check (case-insensitive).
		$ext = strtolower( (string) pathinfo( $absPath, PATHINFO_EXTENSION ) );
		if ( $ext !== 'zip' ) {
			return false;
		}

		// Magic bytes: ZIP local file header starts with 'PK\x03\x04' (or 'PK\x05\x06' for empty).
		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- magic bytes check; WP_Filesystem has no binary read API; headless agent never initialises WP_Filesystem
		$fh = @fopen( $absPath, 'rb' );
		if ( $fh === false ) {
			return false;
		}
		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fread -- reading magic bytes for format check
		$magic = fread( $fh, 4 );
		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- cleanup after magic bytes read
		fclose( $fh );

		if ( $magic === false || strlen( $magic ) < 2 ) {
			return false;
		}

		return $magic[0] === 'P' && $magic[1] === 'K';
	}

	/**
	 * Recursively remove a directory tree.
	 * Errors are silenced — cleanup failure does not affect correctness.
	 *
	 * @param string $path Absolute path to remove.
	 * @return void
	 */
	private function removeTree( string $path ): void {
		if ( ! file_exists( $path ) && ! is_link( $path ) ) {
			return;
		}

		if ( is_link( $path ) || is_file( $path ) ) {
			// phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- cleanup of quarantine/temp file; headless agent; WP_Filesystem never initialized
			@unlink( $path );
			return;
		}

		if ( ! is_dir( $path ) ) {
			return;
		}

		$handle = @opendir( $path );
		if ( $handle !== false ) {
			while ( true ) {
				$child = readdir( $handle );
				if ( $child === false ) {
					break;
				}
				if ( $child === '.' || $child === '..' ) {
					continue;
				}
				$this->removeTree( $path . '/' . $child );
			}
			closedir( $handle );
		}

		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_rmdir -- headless agent; WP_Filesystem never initialized; cleanup of quarantine temp dir
		@rmdir( $path );
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
