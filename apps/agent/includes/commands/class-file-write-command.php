<?php
// File: class-file-write-command.php — FileWriteCommand.

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

use WPMgr\Agent\Keystore;
use WPMgr\Agent\Support\StoragePaths;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * FileWriteCommand: create or overwrite a small text file (≤ 256 KiB) within
 * the site jail via an atomic temp-write → rename swap.
 *
 * Wire contract (CP→agent):
 *   POST /wp-json/wpmgr/v1/command/file_write
 *   Authorization: Bearer <Ed25519 JWT cmd="file_write">
 *   Body: {
 *     "path":                    <site-relative path, forward slashes>,
 *     "content_base64":          <base64-encoded file content>,
 *     "confirm_executable_write": <bool — default false>,
 *     "confirm_sensitive":        <bool — default false>
 *   }
 *
 * Response (200 OK):
 *   { "path": <string>, "size": <int>, "mtime": <int>, "mode": <string octal> }
 *
 * Errors:
 *   { "error": { "code": <string>, "message": <string> } }
 *   Codes: invalid_path, outside_root, not_found, is_directory, too_large,
 *          sensitive_denied, executable_write_denied, base_unresolved, write_failed.
 *
 * Security controls (T1, T3, T11):
 *   - T1 Executable-write prevention:
 *     Rejects writes when ANY of: (a) basename or any secondary extension in the
 *     executable-extension deny-list; (b) double-extension or trailing-dot tricks
 *     (x.php.jpg, x.php., .PhP); (c) decoded content contains '<?php', '<?=',
 *     or a bare '<?' not immediately followed by 'xml' (short-open-tag detection).
 *     Only bypassed by confirm_executable_write=true.
 *   - T3 Empty-base guard: resolves and validates the jail root BEFORE any FS
 *     mutation — throws base_unresolved rather than writing at an empty path.
 *   - T11 Pre-write backup: if the target file already exists, it is copied to a
 *     per-op staging area before any mutation so restore-previous-version is possible.
 *   - Atomic write: content is written to a temp file in the SAME directory, then
 *     rename() moves it atomically over the target. No partial writes are visible.
 *   - Sensitive deny: isSensitive() blocks writes to credential/config files unless
 *     confirm_sensitive=true.
 *   - The 256 KiB cap matches file_read (same inline limit; larger files go via
 *     the presigned-upload path, file_upload_apply).
 *
 * @package WPMgr\Agent\Commands
 */
final class FileWriteCommand implements CommandInterface {

	/** Hard cap: inline content limit. Larger files use file_upload_apply. */
	private const MAX_BYTES = 262144;

	/**
	 * {@inheritDoc}
	 */
	public function name(): string {
		return 'file_write';
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

		if ( ! array_key_exists( 'content_base64', $params ) || ! is_string( $params['content_base64'] ) ) {
			return $this->error( 'invalid_path', 'content_base64 is required' );
		}

		// Decode content before any validation — reject immediately if encoding is wrong.
		$content = base64_decode( $params['content_base64'], true );
		if ( $content === false ) {
			return $this->error( 'invalid_path', 'content_base64 is not valid base64' );
		}

		if ( strlen( $content ) > self::MAX_BYTES ) {
			return $this->error( 'too_large', 'content exceeds the 256 KiB inline limit; use file_upload_apply for larger files' );
		}

		$confirmExecutableWrite = ! empty( $params['confirm_executable_write'] );
		$confirmSensitive       = ! empty( $params['confirm_sensitive'] );

		// ------------------------------------------------------------------
		// 2. Resolve jail root (T3: base_unresolved guard before any write).
		// ------------------------------------------------------------------
		$jailRoot = FileListCommand::resolveJailRoot();
		if ( $jailRoot === '' ) {
			return $this->error( 'base_unresolved', 'file jail root could not be resolved; no write performed' );
		}

		// ------------------------------------------------------------------
		// 3. Jail the path (segment checks + realpath containment).
		// ------------------------------------------------------------------
		$jailResult = FileListCommand::jailPath( $jailRoot, $relPath );
		if ( ! $jailResult['ok'] ) {
			return $this->error( $jailResult['code'], $jailResult['message'] );
		}
		$absPath     = $jailResult['abs'];
		$resolvedRel = $jailResult['rel'];

		// Refuse if the resolved path is a directory.
		if ( is_dir( $absPath ) ) {
			return $this->error( 'is_directory', 'path is a directory, not a file' );
		}

		// ------------------------------------------------------------------
		// 4. Sensitive-file deny (T6). Checked on BOTH existing and new files.
		// ------------------------------------------------------------------
		$basename = basename( $resolvedRel );
		if ( FileReadCommand::isSensitive( $resolvedRel, $basename ) && ! $confirmSensitive ) {
			return $this->error(
				'sensitive_denied',
				'file matches the sensitive-file deny-list; set confirm_sensitive=true to override'
			);
		}

		// ------------------------------------------------------------------
		// 5. Executable-write prevention (T1 — THE core control).
		//    Deny the write unless confirm_executable_write=true.
		// ------------------------------------------------------------------
		if ( ! $confirmExecutableWrite && FileGuards::isExecutableWrite( $absPath, $resolvedRel, $content ) ) {
			return $this->error(
				'executable_write_denied',
				'write denied: file extension, double-extension, content sniff, or target directory indicates an executable file; set confirm_executable_write=true to override (owner permission required)'
			);
		}

		// ------------------------------------------------------------------
		// 6. Ensure the parent directory exists.
		// ------------------------------------------------------------------
		$parentDir = dirname( $absPath );
		if ( ! is_dir( $parentDir ) ) {
			return $this->error( 'not_found', 'parent directory does not exist' );
		}

		// ------------------------------------------------------------------
		// 7. T11: Pre-write backup — if target exists, copy to staging area.
		// ------------------------------------------------------------------
		if ( file_exists( $absPath ) && ! is_dir( $absPath ) ) {
			$this->stageBackup( $absPath, $resolvedRel );
		}

		// ------------------------------------------------------------------
		// 8. Atomic write: temp file in the SAME directory → rename.
		//    T3: parent/tempfile already within the jail (dirname is the same dir).
		//    TOCTOU symlink guard: verify the final target is not a symlink and
		//    the parent dir is still jailed immediately before the rename.
		// ------------------------------------------------------------------

		// F3: Reject if the target is (or has become) a symlink — rename() follows
		// symlinks at the destination and would write outside the jail.
		if ( is_link( $absPath ) ) {
			return $this->error( 'outside_root', 'write denied: destination is a symbolic link' );
		}

		// F3: Re-verify the parent directory is still inside the jail immediately
		// before we open the temp file (re-jail the parent, which always exists).
		$parentReal = realpath( $parentDir );
		if ( $parentReal === false
			|| ( strncmp( str_replace( '\\', '/', $parentReal ), $jailRoot . '/', strlen( $jailRoot ) + 1 ) !== 0
				&& str_replace( '\\', '/', $parentReal ) !== $jailRoot )
		) {
			return $this->error( 'outside_root', 'write denied: parent directory is outside the jail root' );
		}

		$tmpPath = $parentReal . '/.wpmgr-tmp-' . bin2hex( random_bytes( 6 ) );

		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- headless agent; WP_Filesystem never initialized (prompts for FTP creds and hard-fails in this context); atomic temp+rename pattern required
		$written = @file_put_contents( $tmpPath, $content, LOCK_EX );
		if ( $written === false ) {
			return $this->error( 'write_failed', 'could not write temporary file' );
		}

		// ------------------------------------------------------------------
		// Final jail assertion — redundant, self-contained, and visible at the
		// write call site so a reviewer can verify containment without tracing
		// the full call stack.
		//
		// Full gate the destination already passed:
		//   (1) resolveJailRoot() validated the jail root via realpath (T3)
		//   (2) jailPath() verified realpath containment + traversal rejection (§3)
		//   (3) FileReadCommand::isSensitive() blocked credential/config paths (T6)
		//   (4) FileGuards::isExecutableWrite() blocked .php and other executables (T1)
		//   (5) F3 symlink guard above rejected symlink destinations
		//   (6) F3 parent re-jail above re-confirmed the parent is inside the jail
		//   (7) The JWT authorising this write carries cmd="file_write" and was
		//       verified by Ed25519 + anti-replay jti + 60 s expiry window
		//
		// This assertion re-resolves $absPath's real parent and confirms it is still
		// within $jailRoot, catching any race that slipped through the checks above.
		$finalParentReal = realpath( dirname( $absPath ) );
		if ( $finalParentReal === false
			|| ( strncmp( str_replace( '\\', '/', $finalParentReal ), $jailRoot . '/', strlen( $jailRoot ) + 1 ) !== 0
				&& str_replace( '\\', '/', $finalParentReal ) !== $jailRoot )
		) {
			// phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- headless agent; WP_Filesystem never initialized; cleanup of temp file on final-assertion failure
			@unlink( $tmpPath );
			return $this->error( 'outside_root', 'write denied: final jail assertion failed — destination escaped the jail root' );
		}

		// phpcs:ignore WordPress.WP.AlternativeFunctions.rename_rename -- WP_Filesystem::move() is non-atomic (copy+delete) and WP_Filesystem is never initialized in the headless agent path; native rename() is the only atomic option on POSIX filesystems
		if ( ! @rename( $tmpPath, $absPath ) ) {
			// phpcs:ignore WordPress.WP.AlternativeFunctions.unlink_unlink -- headless agent; WP_Filesystem never initialized; cleanup of temp file on rename failure
			@unlink( $tmpPath );
			return $this->error( 'write_failed', 'atomic rename failed' );
		}

		// ------------------------------------------------------------------
		// 9. Build response.
		// ------------------------------------------------------------------
		$lstat = @lstat( $absPath );
		$size  = $lstat !== false ? (int) ( $lstat['size'] ?? strlen( $content ) ) : strlen( $content );
		$mtime = $lstat !== false ? (int) ( $lstat['mtime'] ?? 0 ) : 0;
		$mode  = $lstat !== false ? (int) ( $lstat['mode'] ?? 0 ) : 0;

		return [
			'path'  => $resolvedRel,
			'size'  => $size,
			'mtime' => $mtime,
			'mode'  => sprintf( '%04o', $mode & 07777 ),
		];
	}

	// ------------------------------------------------------------------
	// T11: pre-write backup into a hardened staging directory.
	// ------------------------------------------------------------------

	/**
	 * Copy the existing target file to the staging area before overwriting it,
	 * AES-256-GCM-encrypting the backup content at rest.
	 *
	 * F4: Backups are stored as ciphertext (Keystore AES-256-GCM envelope) so that
	 * even if the .htaccess deny rule is removed, the raw file bytes on disk are
	 * unreadable without the agent's master key. The key is derived from wp-config
	 * salts (or a site-specific file), same as all other Keystore-protected data.
	 *
	 * Errors are silenced — failure here does NOT block the write; the backup is
	 * best-effort so that a later "restore previous version" is possible.
	 *
	 * @param string $absPath     Absolute path to the current target file.
	 * @param string $resolvedRel Site-relative path of the target.
	 * @return void
	 */
	private function stageBackup( string $absPath, string $resolvedRel ): void {
		$stagingBase = StoragePaths::ensureHardened( 'file-backups' );
		if ( $stagingBase === '' ) {
			return;
		}

		// Per-op directory: <staging_base>/<sanitized_rel>/<timestamp>-<rand>.bak
		$safeRel   = str_replace( [ '/', '\\', ':' ], '_', $resolvedRel );
		$safeRel   = ltrim( $safeRel, '.' );
		$backupDir = $stagingBase . '/' . sanitize_file_name( $safeRel );

		if ( ! is_dir( $backupDir ) ) {
			wp_mkdir_p( $backupDir );
		}

		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_get_contents -- headless agent; WP_Filesystem never initialized; reading source file for encrypted backup
		$plaintext = @file_get_contents( $absPath );
		if ( $plaintext === false ) {
			return; // Unreadable source — skip silently.
		}

		// F4: Encrypt the backup bytes with the existing Keystore AES-256-GCM cipher.
		// No new dependency — the Keystore is already vendored for the key/credential store.
		try {
			$keystore   = new Keystore();
			$ciphertext = $keystore->encrypt( $plaintext );
		} catch ( \Throwable $e ) {
			return; // Encryption failed — skip backup rather than writing plaintext.
		}

		$backupFile = $backupDir . '/' . time() . '-' . bin2hex( random_bytes( 4 ) ) . '.bak';
		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- headless agent; WP_Filesystem never initialized; writing AES-256-GCM-encrypted backup file
		@file_put_contents( $backupFile, $ciphertext, LOCK_EX );
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
